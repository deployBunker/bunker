package registry

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CompactStats reports the before/after shape of a compaction.
type CompactStats struct {
	// BeforeEvents is the number of complete log lines found across the
	// active file and its rotated backups before compaction.
	BeforeEvents int
	// AfterEvents is the number of lines written to the compacted file:
	// one current-state record per live agent, plus at most one bounded
	// known-ID index record.
	AfterEvents int
	// Live/Known are the folded set sizes on each side of the rewrite.
	BeforeLive  int
	AfterLive   int
	BeforeKnown int
	AfterKnown  int
	// Files is the number of log files that were read.
	Files int
}

// Compact rewrites the registry to exactly one current-state record per live
// agent, dropping stale lifecycle events, plus at most one bounded known-ID
// index record. It is safe offline and, when the daemon is running, is
// serialized against spawn/destroy writes by the cross-process lock.
//
// The rewrite is atomic: events are written to a temp file in the same
// directory, fsync'd, and renamed over the active path (the parent directory
// is fsync'd too), so a crash mid-compaction can never leave a truncated or
// half-written registry.
func (s *Store) Compact() (CompactStats, error) {
	var stats CompactStats

	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := s.lockCrossProcess()
	if err != nil {
		return stats, err
	}
	defer unlock()

	stats.BeforeEvents, stats.Files, err = s.countEvents()
	if err != nil {
		return stats, err
	}

	// Re-fold from disk under the lock rather than trusting possibly stale
	// in-memory state (another process may have written since our last read).
	// Keep this process's view first: rotation is a RETENTION window, not a
	// state transition, so a live agent whose spawn event was rotated away
	// still exists here and must not be dropped by the rewrite.
	memoryLive := make(map[string]*Record, len(s.live))
	for id, rec := range s.live {
		memoryLive[id] = rec
	}
	memoryKnown := make([]string, len(s.knownOrder))
	copy(memoryKnown, s.knownOrder)

	if _, err := s.replayLocked(); err != nil {
		return stats, err
	}
	// Compaction is a repair point: the rewritten file is the UNION of the
	// on-disk fold and this process's live/known sets.
	for id, rec := range memoryLive {
		if _, ok := s.live[id]; !ok {
			s.live[id] = rec
		}
	}
	for _, id := range memoryKnown {
		if s.known[id] {
			continue
		}
		if _, isLive := s.live[id]; isLive {
			continue
		}
		s.known[id] = true
		s.knownOrder = append(s.knownOrder, id)
	}
	s.knownOrder = trimKnown(s.knownOrder, s.known, s.live, s.knownCap)

	stats.BeforeLive = len(s.live)
	stats.BeforeKnown = len(s.knownOrder)

	events := make([]Event, 0, len(s.live)+1)
	now := time.Now().UTC().Format(time.RFC3339)
	if len(s.knownOrder) > 0 {
		ids := make([]string, len(s.knownOrder))
		copy(ids, s.knownOrder)
		events = append(events, Event{TS: now, Kind: KindKnown, KnownIDs: ids})
	}
	// Deterministic order keeps compaction idempotent: compacting twice
	// produces a byte-identical file.
	ids := make([]string, 0, len(s.live))
	for id := range s.live {
		ids = append(ids, id)
	}
	sortStrings(ids)
	for _, id := range ids {
		rec := s.live[id]
		events = append(events, Event{
			TS:               now,
			Kind:             KindSpawn,
			AgentID:          rec.AgentID,
			Status:           rec.Status,
			CreatedAt:        formatTime(rec.CreatedAt),
			ExpiresAt:        formatTime(rec.ExpiresAt),
			PortStart:        rec.PortStart,
			PortEnd:          rec.PortEnd,
			Limits:           rec.Limits,
			SSHKeyPath:       rec.SSHKeyPath,
			SSHFSMount:       rec.SSHFSMount,
			DockerHostTunnel: rec.DockerHostTunnel,
			PublicURL:        rec.PublicURL,
			TailnetIP:        rec.TailnetIP,
		})
	}

	if err := s.writeAtomic(events); err != nil {
		return stats, err
	}
	// Rotated backups are superseded by the rewrite; leaving them would make
	// replay re-fold stale lifecycle events and silently undo the compact.
	if err := s.removeBackups(); err != nil {
		return stats, err
	}

	stats.AfterEvents = len(events)
	stats.AfterLive = len(s.live)
	stats.AfterKnown = len(s.knownOrder)
	s.last = Report{Files: 1, Events: stats.AfterEvents, Live: stats.AfterLive, Known: stats.AfterKnown}

	s.logger.Info("registry compacted",
		"path", s.path,
		"events_before", stats.BeforeEvents,
		"events_after", stats.AfterEvents,
		"live", stats.AfterLive,
		"known", stats.AfterKnown,
	)
	return stats, nil
}

// countEvents counts complete (newline-terminated, non-empty) lines across
// the active file and its backups.
func (s *Store) countEvents() (events, files int, err error) {
	for _, path := range s.files() {
		f, oerr := os.Open(path)
		if oerr != nil {
			if errors.Is(oerr, os.ErrNotExist) {
				continue
			}
			return 0, files, fmt.Errorf("registry: count %s: %w", path, oerr)
		}
		files++
		br := bufio.NewReaderSize(f, 64*1024)
		for {
			line, rerr := br.ReadString('\n')
			if rerr != nil {
				break // trailing partial line is not a complete event
			}
			if strings.TrimSpace(line) != "" {
				events++
			}
		}
		if cerr := f.Close(); cerr != nil {
			return events, files, fmt.Errorf("registry: close %s: %w", path, cerr)
		}
	}
	return events, files, nil
}

// writeAtomic writes every event to a temp file and renames it over the
// active registry path, fsyncing file and parent directory.
func (s *Store) writeAtomic(events []Event) error {
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".compact-*")
	if err != nil {
		return fmt.Errorf("registry: create temp for compaction: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("registry: chmod temp: %w", err)
	}
	bw := bufio.NewWriterSize(tmp, 64*1024)
	for i := range events {
		line, merr := json.Marshal(events[i])
		if merr != nil {
			cleanup()
			return fmt.Errorf("registry: marshal compacted event: %w", merr)
		}
		if _, werr := bw.Write(append(line, '\n')); werr != nil {
			cleanup()
			return fmt.Errorf("registry: write compacted registry: %w", werr)
		}
	}
	if err := bw.Flush(); err != nil {
		cleanup()
		return fmt.Errorf("registry: flush compacted registry: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("registry: sync compacted registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("registry: close compacted registry: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("registry: install compacted registry: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

// removeBackups deletes rotated log files after a compaction rewrite.
func (s *Store) removeBackups() error {
	for i := 1; i <= s.maxBackups; i++ {
		path := fmt.Sprintf("%s.%d", s.path, i)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("registry: remove backup %s: %w", path, err)
		}
	}
	return syncDir(filepath.Dir(s.path))
}

// sortStrings is a tiny insertion sort to avoid pulling sort into the hot
// path of compaction (the live set is small: tens to hundreds of agents).
func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
