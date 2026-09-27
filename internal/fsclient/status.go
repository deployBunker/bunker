package fsclient

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// The reported state: BFS-005 §3.2's status document, the refusal log, and the
// per-mount cache directory. A bound the owner cannot see is not a bound, and a
// refusal that only exists as a returned errno is invisible to the next person.
// ---------------------------------------------------------------------------

// Conflict is one refused write, recorded so `bunker fs conflicts` can show it
// after the fact. `current` is present ONLY when the server named one
// (`hash_mismatch`); `code` always carries the server's machine code, so the two
// refusal classes stay distinguishable (§5.2 step 4).
type Conflict struct {
	Path     string    `json:"path"`
	Expected string    `json:"expected,omitempty"`
	Current  string    `json:"current,omitempty"`
	Code     string    `json:"code"`
	Detail   string    `json:"detail,omitempty"`
	TS       time.Time `json:"ts"`
}

// ConflictState is the `conflicts` block of the status document.
type ConflictState struct {
	RefusalsTotal int64     `json:"refusals_total"`
	Last          *Conflict `json:"last"`
}

// TransportState is the `transport` block: three verdicts, never two, because
// `unreachable` (transport) and `stale_identity` (wrong tree) demand different
// recovery (§7.3).
type TransportState struct {
	Verdict     string `json:"verdict"`
	LastOKAgeMS *int64 `json:"last_ok_age_ms"`
	Cause       string `json:"cause"`
	// InFlight/InFlightMax are the concurrency figures acceptance criterion 5
	// asks to be stated rather than asserted.
	InFlight    int64  `json:"in_flight"`
	InFlightMax int64  `json:"in_flight_max"`
	Requests    int64  `json:"requests_total"`
	Proto       string `json:"proto,omitempty"`
	Tree        string `json:"tree,omitempty"`
	Rev         string `json:"rev,omitempty"`
}

// SnapshotState is the `snapshot` block: where the node tree came from and what
// it cost, which is the honest way to report the one-call win (SourceSnapshotOp,
// Calls=1) versus the standard-protocol fallback (SourcePropfindWalk, Calls=N).
type SnapshotState struct {
	Source    string `json:"source"`
	Nodes     int    `json:"nodes"`
	Calls     int    `json:"calls"`
	Truncated bool   `json:"truncated"`
	AgeMS     int64  `json:"age_ms"`
}

// Status is the whole reported state. Field names are the spec's: a consumer
// (a test, a dashboard, a human) reads one document.
type Status struct {
	Mount        string            `json:"mount"`
	Mode         string            `json:"mode"`
	Endpoint     string            `json:"endpoint"`
	Mountpoint   string            `json:"mountpoint"`
	Concurrency  int               `json:"concurrency"`
	OnConflict   string            `json:"on_conflict"`
	Cache        CacheStats        `json:"cache"`
	Invalidation InvalidationState `json:"invalidation"`
	Conflicts    ConflictState     `json:"conflicts"`
	Transport    TransportState    `json:"transport"`
	Snapshot     SnapshotState     `json:"snapshot"`
	// WriteHandles reports how many open write handles are buffering rather
	// than streaming — the one local-capacity figure the write path can grow.
	WriteHandlesBuffered int   `json:"write_handles_buffered"`
	WriteBufferBytes     int64 `json:"write_buffer_bytes"`
	// ReadBound is the BFS-025 figure set: how often a read found the content it
	// held inconsistent with the size this mount had published to the kernel.
	// Refusals are reads that were refused (ESTALE) rather than served short;
	// corrections are the times the mount repaired metadata it could not trust
	// from the content the server served. Both are reported because a rule that
	// fires silently is not a rule anyone can audit.
	ReadBound ReadBoundState `json:"read_bound"`
	// WriteShape is the BFS-030 figure set: how often a write shape this surface
	// cannot complete was refused BEFORE it published anything — the destructive
	// half of an in-place rewrite (a resize arriving while a handle opened for
	// writing is live on the path). Reported for the same reason ReadBound is:
	// the rule is only auditable if the owner can see it fire.
	WriteShape WriteShapeState `json:"write_shape"`
	UpdatedMS  int64           `json:"updated_ms"`
}

// WriteShapeState is the `write_shape` block of the status document (BFS-030).
type WriteShapeState struct {
	// RefusalsTotal counts resizes refused because a write handle was live on
	// the path: publishing one would have been the destructive half of a write
	// this surface cannot complete (BFS-030 — `>` emptied the file, then failed).
	RefusalsTotal int64 `json:"refusals_total"`
	// Last names the most recent refusal, formatted `<path>: size=<n>`; empty
	// when none has happened.
	Last string `json:"last,omitempty"`
}

// ReadBoundState is the `read_bound` block of the status document (BFS-025).
type ReadBoundState struct {
	// RefusalsTotal counts reads refused because the content the client held was
	// LONGER than the size the kernel was holding for the path: serving it would
	// have been a silent truncation.
	RefusalsTotal int64 `json:"refusals_total"`
	// CorrectionsTotal counts the times the client corrected state it could not
	// trust — the published size of a path, or a cache entry whose length
	// contradicted that size — from the content the server served.
	CorrectionsTotal int64 `json:"corrections_total"`
	// Last names the most recent divergence, formatted
	// `<path>: published=<n> content=<m> refused=<bool>`; empty when none has
	// happened. It is a string, not a struct, so it survives across a version
	// without a schema change.
	Last string `json:"last,omitempty"`
}

// MountRoot returns the per-user root of this client's cache directories:
// $XDG_CACHE_HOME/bunker/fs (default ~/.cache/bunker/fs), mode 0700.
func MountRoot() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("fsclient: resolve home: %w", err)
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "bunker", "fs"), nil
}

// MountID derives a stable id for one endpoint: the same endpoint is the same
// mount across runs, so its cache and its conflict log are found again.
func MountID(baseURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(baseURL, "/")))
	return hex.EncodeToString(sum[:])[:12]
}

// MountDir is the cache directory of one mount: <root>/<mount-id>, 0700.
func MountDir(baseURL string) (string, error) {
	root, err := MountRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, MountID(baseURL)), nil
}

// StatusFile is the status document's name inside a mount directory.
const StatusFile = "status.json"

// ConflictsFile is the append-only refusal log inside a mount directory.
const ConflictsFile = "conflicts.jsonl"

// FindMounts lists the existing mount directories, newest first.
func FindMounts() ([]string, error) {
	root, err := MountRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		ai, _ := os.Stat(dirs[i])
		aj, _ := os.Stat(dirs[j])
		if ai == nil || aj == nil {
			return dirs[i] < dirs[j]
		}
		return ai.ModTime().After(aj.ModTime())
	})
	return dirs, nil
}

// WriteStatus persists the status document atomically. The mount writes it on a
// cadence and on demand, so `bunker fs status` reads a file rather than talking
// to the mount — no new server-side requirement, no IPC daemon, and it works
// from a different terminal than the one holding the mount.
func WriteStatus(dir string, st Status) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st.UpdatedMS = timeNow().UnixMilli()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := filepath.Join(dir, StatusFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, StatusFile))
}

// ReadStatus reads one mount's status document.
func ReadStatus(dir string) (*Status, error) {
	raw, err := os.ReadFile(filepath.Join(dir, StatusFile))
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("fsclient: decode status %s: %w", dir, err)
	}
	return &st, nil
}

// conflictLog serialises appends to the refusal log.
type conflictLog struct {
	mu sync.Mutex
}

var theConflictLog conflictLog

// AppendConflict records one refusal. It is a log, not a counter: the count in
// the status document can be wrong after a crash, the log is the record.
func AppendConflict(dir string, c Conflict) error {
	if c.TS.IsZero() {
		c.TS = timeNow()
	}
	theConflictLog.mu.Lock()
	defer theConflictLog.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, ConflictsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ReadConflicts reads the refusal log, most recent first, capped at limit
// (0 = all).
func ReadConflicts(dir string, limit int) ([]Conflict, error) {
	f, err := os.Open(filepath.Join(dir, ConflictsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Conflict
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c Conflict
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			continue
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
