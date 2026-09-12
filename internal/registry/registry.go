// Package registry implements the durable, append-only agent lifecycle
// store for bunkerd (GAP-070).
//
// The registry is a line-oriented JSONL event log at
// /var/lib/bunkerd/agents.jsonl by default (mode 0600). Every lifecycle
// change — spawn, heartbeat, destroy — is appended as one JSON object per
// line and fsync'd. The daemon replays the log at startup to rebuild the
// set of live agents before it serves traffic or starts TTL reaping.
//
// Design notes:
//
//   - Rotation is size-capped at MaxBytes (default 5 MiB) with MaxBackups
//     (default 3) rotated files (agents.jsonl.1 … agents.jsonl.3). Replay
//     reads the rotated files oldest-first, then the active file.
//   - Parsing is tolerant: a malformed complete line is logged and skipped,
//     and a partial FINAL line (a torn append) is ignored, so one bad byte
//     never hides the rest of the log.
//   - Writes hold an advisory cross-process flock on agents.jsonl.lock, which
//     `bunker registry compact` also takes — compaction is therefore
//     serialized against daemon spawn/destroy writes.
//   - Compaction rewrites the active file to exactly one current-state
//     record per live agent plus a single bounded "known" index line that
//     preserves the previously-known-but-destroyed agent IDs needed to make
//     a repeated destroy idempotent.
package registry

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// Event kinds written to the log.
const (
	// KindSpawn records a new live agent together with the persisted
	// metadata needed to adopt it after a relaunch (port range, limits,
	// key paths, expiry). A spawn record for an agent that is later
	// destroyed is a stale lifecycle event and is dropped by compaction.
	KindSpawn = "spawn"
	// KindHeartbeat extends an agent's expiry (never shrinks it).
	KindHeartbeat = "heartbeat"
	// KindDestroy removes an agent from the live set and adds it to the
	// known-ID set so a repeated destroy stays idempotent.
	KindDestroy = "destroy"
	// KindKnown is a compaction-only index carrying the bounded list of
	// previously-known agent IDs (most recent last).
	KindKnown = "known"
)

// Defaults for the durable registry.
const (
	// DefaultPath is the production registry location.
	DefaultPath = "/var/lib/bunkerd/agents.jsonl"
	// DefaultMaxBytes caps the active file at 5 MiB before rotation.
	DefaultMaxBytes int64 = 5 << 20
	// DefaultMaxBackups is the number of rotated files kept (.1 … .3).
	DefaultMaxBackups = 3
	// DefaultKnownIDCap bounds the known-ID index written by compaction.
	// 10000 IDs of 63 bytes worst case is ~640 KiB, comfortably inside the
	// 5 MiB active-file cap.
	DefaultKnownIDCap = 10000
)

// Event is one line of the registry log.
type Event struct {
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	AgentID string `json:"agent_id,omitempty"`

	// Current-state fields (spawn / heartbeat).
	Status    string             `json:"status,omitempty"`
	CreatedAt string             `json:"created_at,omitempty"`
	ExpiresAt string             `json:"expires_at,omitempty"`
	PortStart uint32             `json:"port_start,omitempty"`
	PortEnd   uint32             `json:"port_end,omitempty"`
	Limits    *v1.ResourceLimits `json:"limits,omitempty"`

	// Connection metadata persisted so an adopted agent is byte-compatible
	// with a freshly spawned one.
	SSHKeyPath       string `json:"ssh_key_path,omitempty"`
	SSHFSMount       string `json:"sshfs_mount,omitempty"`
	DockerHostTunnel string `json:"docker_host_tunnel,omitempty"`
	PublicURL        string `json:"public_url,omitempty"`
	TailnetIP        string `json:"tailnet_ip,omitempty"`

	// KnownIDs is set only on KindKnown index records.
	KnownIDs []string `json:"known_ids,omitempty"`
}

// Record is the folded current state of one live agent.
type Record struct {
	AgentID          string
	Status           string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	PortStart        uint32
	PortEnd          uint32
	Limits           *v1.ResourceLimits
	SSHKeyPath       string
	SSHFSMount       string
	DockerHostTunnel string
	PublicURL        string
	TailnetIP        string
}

// Report summarises one replay pass.
type Report struct {
	// Files is the number of log files read (rotated + active).
	Files int
	// Events is the number of well-formed event lines folded.
	Events int
	// Malformed counts complete lines that could not be parsed (skipped).
	Malformed int
	// PartialTail is true when the active file's final line was torn and
	// therefore ignored.
	PartialTail bool
	// Live and Known are the folded set sizes after replay.
	Live  int
	Known int
}

// Options configures Open.
type Options struct {
	Path       string
	MaxBytes   int64
	MaxBackups int
	KnownIDCap int
	Logger     *slog.Logger
}

// Store is the durable agent registry.
type Store struct {
	path       string
	maxBytes   int64
	maxBackups int
	knownCap   int
	logger     *slog.Logger

	mu         sync.Mutex // serializes writers within this process
	live       map[string]*Record
	known      map[string]bool
	knownOrder []string
	last       Report
}

// Open opens (creating if needed) the registry at opts.Path and replays it.
func Open(opts Options) (*Store, error) {
	path := opts.Path
	if path == "" {
		path = DefaultPath
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxBackups <= 0 {
		opts.MaxBackups = DefaultMaxBackups
	}
	if opts.KnownIDCap <= 0 {
		opts.KnownIDCap = DefaultKnownIDCap
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("registry: create %s: %w", filepath.Dir(path), err)
	}

	s := &Store{
		path:       path,
		maxBytes:   opts.MaxBytes,
		maxBackups: opts.MaxBackups,
		knownCap:   opts.KnownIDCap,
		logger:     logger,
		live:       make(map[string]*Record),
		known:      make(map[string]bool),
	}
	// Ensure the active file exists with 0600 so replay and rotation have a
	// defined starting state.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("registry: open %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("registry: close %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("registry: chmod %s: %w", path, err)
	}

	if _, err := s.Replay(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the active registry file path.
func (s *Store) Path() string { return s.path }

// Report returns the summary of the last replay.
func (s *Store) Report() Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Replay resets in-memory state and rebuilds it from every log file
// (rotated oldest-first, then the active file).
func (s *Store) Replay() (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replayLocked()
}

// files returns the log files to replay, oldest first.
func (s *Store) files() []string {
	var out []string
	for i := s.maxBackups; i >= 1; i-- {
		out = append(out, fmt.Sprintf("%s.%d", s.path, i))
	}
	return append(out, s.path)
}

func (s *Store) replayLocked() (Report, error) {
	rep := Report{}
	live := make(map[string]*Record)
	known := make(map[string]bool)
	var knownOrder []string

	for _, path := range s.files() {
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return rep, fmt.Errorf("registry: read %s: %w", path, err)
		}
		rep.Files++
		br := bufio.NewReaderSize(f, 64*1024)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				// Torn final line (no newline) or EOF: a partial line is
				// never a complete event, so it is ignored, not parsed.
				if line != "" {
					rep.PartialTail = true
				}
				break
			}
			text := strings.TrimSpace(line)
			if text == "" {
				continue
			}
			var ev Event
			if uerr := json.Unmarshal([]byte(text), &ev); uerr != nil {
				rep.Malformed++
				s.logger.Warn("registry: skipping malformed line", "file", path, "error", uerr)
				continue
			}
			rep.Events++
			// Later events win: replay is a fold in file order.
			switch ev.Kind {
			case KindSpawn:
				if ev.AgentID == "" {
					rep.Malformed++
					s.logger.Warn("registry: skipping spawn without agent_id", "file", path)
					continue
				}
				live[ev.AgentID] = eventToRecord(&ev)
				delete(known, ev.AgentID)
				knownOrder = removeString(knownOrder, ev.AgentID)
			case KindHeartbeat:
				if rec, ok := live[ev.AgentID]; ok {
					if rec.Status == "" {
						rec.Status = ev.Status
					}
					if t := ev.ExpiresAt; t != "" {
						if ts, perr := time.Parse(time.RFC3339, t); perr == nil {
							rec.ExpiresAt = ts
						}
					}
					if ev.Status != "" {
						rec.Status = ev.Status
					}
					// A heartbeat for a live record keeps connection metadata.
					if ev.PublicURL != "" {
						rec.PublicURL = ev.PublicURL
					}
					if ev.TailnetIP != "" {
						rec.TailnetIP = ev.TailnetIP
					}
				}
			case KindDestroy:
				delete(live, ev.AgentID)
				if ev.AgentID != "" && !known[ev.AgentID] {
					known[ev.AgentID] = true
					knownOrder = append(knownOrder, ev.AgentID)
				}
			case KindKnown:
				for _, id := range ev.KnownIDs {
					if id == "" || known[id] {
						continue
					}
					known[id] = true
					knownOrder = append(knownOrder, id)
				}
			default:
				rep.Malformed++
				s.logger.Warn("registry: skipping unknown event kind", "file", path, "kind", ev.Kind)
			}
		}
		if cerr := f.Close(); cerr != nil {
			return rep, fmt.Errorf("registry: close %s: %w", path, cerr)
		}
	}

	// Drop IDs that came back to life so the known index stays disjoint
	// from the live set, then trim the index to its documented bound.
	knownOrder = trimKnown(knownOrder, known, live, s.knownCap)
	rep.Live = len(live)
	rep.Known = len(knownOrder)

	s.live = live
	s.known = known
	s.knownOrder = knownOrder
	s.last = rep
	if rep.Malformed > 0 || rep.PartialTail {
		s.logger.Warn("registry: replay tolerated damaged input",
			"malformed", rep.Malformed, "partial_tail", rep.PartialTail, "live", rep.Live)
	}
	return rep, nil
}

// Live returns the live records sorted by agent ID.
func (s *Store) Live() []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Record, 0, len(s.live))
	for _, rec := range s.live {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// Get returns the live record for agentID, or nil.
func (s *Store) Get(agentID string) *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[agentID]
}

// Known reports whether agentID is (or was) known to the registry. It is
// true for live agents and for destroyed agents still inside the bounded
// known-ID index — this is what makes a repeated destroy idempotent while a
// never-seen ID still reports not_found.
func (s *Store) Known(agentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.live[agentID]; ok {
		return true
	}
	return s.known[agentID]
}

// LiveCount returns the number of live agents.
func (s *Store) LiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// KnownCount returns the number of remembered (destroyed) agent IDs.
func (s *Store) KnownCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.knownOrder)
}

// AppendSpawn persists a live agent's full current state.
func (s *Store) AppendSpawn(rec *Record) error {
	if rec == nil || rec.AgentID == "" {
		return fmt.Errorf("registry: spawn event requires an agent_id")
	}
	ev := Event{
		TS:               time.Now().UTC().Format(time.RFC3339),
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
	}
	return s.append(ev, func() {
		clone := *rec
		s.live[rec.AgentID] = &clone
		if s.known[rec.AgentID] {
			delete(s.known, rec.AgentID)
			s.knownOrder = removeString(s.knownOrder, rec.AgentID)
		}
	})
}

// AppendHeartbeat persists a TTL extension for a live agent. A heartbeat for
// an agent the registry does not know as live is a no-op (the in-memory
// tracker remains the source of truth for what is running right now).
func (s *Store) AppendHeartbeat(agentID string, expiresAt time.Time, status string) error {
	if agentID == "" {
		return fmt.Errorf("registry: heartbeat event requires an agent_id")
	}
	ev := Event{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Kind:      KindHeartbeat,
		AgentID:   agentID,
		Status:    status,
		ExpiresAt: formatTime(expiresAt),
	}
	return s.append(ev, func() {
		if rec, ok := s.live[agentID]; ok {
			rec.ExpiresAt = expiresAt
			if status != "" {
				rec.Status = status
			}
		}
	})
}

// AppendDestroy records that agentID is no longer live. It is idempotent:
// destroying an already-destroyed agent appends nothing new and only
// refreshes the known-ID index order.
func (s *Store) AppendDestroy(agentID string) error {
	if agentID == "" {
		return fmt.Errorf("registry: destroy event requires an agent_id")
	}
	wasLive := false
	s.mu.Lock()
	_, wasLive = s.live[agentID]
	alreadyKnown := s.known[agentID]
	s.mu.Unlock()
	if !wasLive && alreadyKnown {
		return nil // repeated destroy of a known-destroyed agent: nothing to write
	}
	ev := Event{
		TS:      time.Now().UTC().Format(time.RFC3339),
		Kind:    KindDestroy,
		AgentID: agentID,
	}
	return s.append(ev, func() {
		delete(s.live, agentID)
		if !s.known[agentID] {
			s.known[agentID] = true
			s.knownOrder = append(s.knownOrder, agentID)
		}
		s.knownOrder = trimKnown(s.knownOrder, s.known, s.live, s.knownCap)
	})
}

// Forget drops agentID from both the live set and the known-ID index, used
// by reconciliation for records that must not be resurrected (never observed
// as a system user and explicitly discarded). Replay after a Forget no
// longer reports the ID as known.
func (s *Store) Forget(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, agentID)
	delete(s.known, agentID)
	s.knownOrder = removeString(s.knownOrder, agentID)
}

// append serializes, locks, rotates if needed, writes and fsyncs one event.
func (s *Store) append(ev Event, apply func()) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("registry: marshal %s event: %w", ev.Kind, err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := s.lockCrossProcess()
	if err != nil {
		return err
	}
	defer unlock()

	if err := s.terminatePartialLineLocked(); err != nil {
		return err
	}
	if err := s.rotateIfNeededLocked(int64(len(line))); err != nil {
		return err
	}

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("registry: open %s: %w", s.path, err)
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return fmt.Errorf("registry: append %s event: %w", ev.Kind, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("registry: sync %s: %w", s.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("registry: close %s: %w", s.path, err)
	}
	if apply != nil {
		apply()
	}
	return nil
}

// terminatePartialLineLocked repairs a torn tail before a new event is
// appended. A previous append can be interrupted (kill -9, power loss) after
// writing some bytes but before the trailing newline, and replay correctly
// ignores that partial line — but a plain append would then GLUE the next
// event onto it, producing one malformed line and silently dropping the new
// event. Caller holds s.mu (and the cross-process lock).
func (s *Store) terminatePartialLineLocked() error {
	f, err := os.OpenFile(s.path, os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("registry: open %s for tail repair: %w", s.path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("registry: stat %s: %w", s.path, err)
	}
	if info.Size() == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		return fmt.Errorf("registry: read tail of %s: %w", s.path, err)
	}
	if last[0] == '\n' {
		return nil
	}
	if _, err := f.WriteAt([]byte{'\n'}, info.Size()); err != nil {
		return fmt.Errorf("registry: terminate partial line in %s: %w", s.path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("registry: sync tail repair for %s: %w", s.path, err)
	}
	s.logger.Warn("registry: terminated torn final line before append", "path", s.path)
	return nil
}

// rotateIfNeededLocked rotates the active file when adding incoming bytes
// would push it past the size cap. Caller holds s.mu.
func (s *Store) rotateIfNeededLocked(incoming int64) error {
	info, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("registry: stat %s: %w", s.path, err)
	}
	if info.Size()+incoming <= s.maxBytes {
		return nil
	}
	// Shift .N-1 → .N, dropping the oldest backup.
	oldest := fmt.Sprintf("%s.%d", s.path, s.maxBackups)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("registry: remove %s: %w", oldest, err)
	}
	for i := s.maxBackups - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", s.path, i)
		to := fmt.Sprintf("%s.%d", s.path, i+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("registry: rotate %s -> %s: %w", from, to, err)
		}
	}
	if err := os.Rename(s.path, s.path+".1"); err != nil {
		return fmt.Errorf("registry: rotate %s -> %s.1: %w", s.path, s.path, err)
	}
	if err := syncDir(filepath.Dir(s.path)); err != nil {
		return err
	}
	s.logger.Info("registry rotated", "path", s.path, "max_bytes", s.maxBytes, "backups", s.maxBackups)
	return nil
}

// Close releases the store. The store keeps no persistent handles, so this
// only drops in-memory state; it exists for symmetry and future use.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = make(map[string]*Record)
	s.known = make(map[string]bool)
	s.knownOrder = nil
	return nil
}

func eventToRecord(ev *Event) *Record {
	rec := &Record{
		AgentID:          ev.AgentID,
		Status:           ev.Status,
		CreatedAt:        parseTime(ev.CreatedAt),
		ExpiresAt:        parseTime(ev.ExpiresAt),
		PortStart:        ev.PortStart,
		PortEnd:          ev.PortEnd,
		Limits:           ev.Limits,
		SSHKeyPath:       ev.SSHKeyPath,
		SSHFSMount:       ev.SSHFSMount,
		DockerHostTunnel: ev.DockerHostTunnel,
		PublicURL:        ev.PublicURL,
		TailnetIP:        ev.TailnetIP,
	}
	if rec.Status == "" {
		rec.Status = "running"
	}
	return rec
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// trimKnown bounds the known-ID index to cap entries, dropping the oldest
// entries first (the index is ordered oldest→newest), and removes IDs that
// are currently live.
func trimKnown(order []string, known map[string]bool, live map[string]*Record, cap int) []string {
	out := make([]string, 0, len(order))
	for _, id := range order {
		if _, isLive := live[id]; isLive {
			delete(known, id)
			continue
		}
		if !known[id] {
			continue
		}
		out = append(out, id)
	}
	if cap > 0 && len(out) > cap {
		for _, id := range out[:len(out)-cap] {
			delete(known, id)
		}
		out = out[len(out)-cap:]
	}
	return out
}

func removeString(in []string, val string) []string {
	out := in[:0]
	for _, v := range in {
		if v != val {
			out = append(out, v)
		}
	}
	return out
}

// syncDir fsyncs a directory so a rename/rotation is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("registry: open dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("registry: sync dir %s: %w", dir, err)
	}
	return nil
}
