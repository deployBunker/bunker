// store.go — GAP-132 durable JSONL persistence for API keys.
//
// The apikey manager keeps its live key set in memory; before GAP-132 a
// daemon restart silently forgot every issued sub-key (spawn responses kept
// handing out tokens that no longer validated). The store follows the same
// file-backed pattern as the GAP-070 agent registry (internal/registry):
// one JSONL file, mode 0600, in a 0700 directory, appended on every
// lifecycle change and replayed at construction.
//
// Revocation is a MARK, not a delete: a revoked key's record stays in the
// file with revoked=true, so the revocation itself survives a restart (a
// delete would resurrect the key on replay of the older generate line —
// the same last-event-wins reasoning the registry uses for destroy).
package apikey

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StoreFileName is the key store file created inside the store directory.
const StoreFileName = "apikeys.jsonl"

// DirMode is the mode of the directory holding the key store (owner only).
const DirMode os.FileMode = 0o700

// FileMode is the mode of the key store file itself (owner read/write).
const FileMode os.FileMode = 0o600

// keyRecord is the on-disk shape of one key. Field names are the JSONL keys;
// keep them stable — the file is a durable store read back by later daemon
// versions.
type keyRecord struct {
	KeyID     string    `json:"key_id"`
	TokenHash string    `json:"token_hash"`
	AgentID   string    `json:"agent_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Revoked keys stay on disk with revoked=true so the revocation
	// survives restarts and replays. Revoked keys never validate.
	Revoked bool `json:"revoked,omitempty"`
}

// store persists and replays the key set. Writes are serialized under mu;
// every mutation rewrites the file atomically (temp file + rename, the same
// crash-safety shape as the GAP-129 secret writer) so a crash can never
// leave a truncated store that would silently drop keys.
type store struct {
	mu   sync.Mutex
	path string
}

// newStore opens (creating if needed) the key store at dir/apikeys.jsonl.
// The directory is created 0700 and tightened if it pre-exists with a wider
// mode; the file is created 0600.
func newStore(dir string) (*store, error) {
	if dir == "" {
		return nil, fmt.Errorf("apikey store: directory is required")
	}
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return nil, fmt.Errorf("apikey store: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, DirMode); err != nil {
		return nil, fmt.Errorf("apikey store: set %s mode %#o: %w", dir, DirMode, err)
	}
	return &store{path: filepath.Join(dir, StoreFileName)}, nil
}

// load replays the store into a fresh key map (last event wins per keyID,
// matching the registry's replay semantics). A missing file is an empty
// key set, not an error. A malformed line is a hard error: silently
// dropping a line could resurrect or lose live credentials.
func (s *store) load() (map[string]*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *store) loadLocked() (map[string]*Key, error) {
	keys := make(map[string]*Key)
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return keys, nil
		}
		return nil, fmt.Errorf("apikey store: open %s: %w", s.path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec keyRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("apikey store: %s line %d: %w", s.path, lineNo, err)
		}
		keys[rec.KeyID] = &Key{
			KeyID:     rec.KeyID,
			TokenHash: rec.TokenHash,
			AgentID:   rec.AgentID,
			CreatedAt: rec.CreatedAt,
			ExpiresAt: rec.ExpiresAt,
			Revoked:   rec.Revoked,
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("apikey store: read %s: %w", s.path, err)
	}
	return keys, nil
}

// saveAll atomically rewrites the store with the full key set (including
// revoked markers), keeping the file 0600.
func (s *store) saveAll(keys map[string]*Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveAllLocked(keys)
}

func (s *store) saveAllLocked(keys map[string]*Key) error {
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, FileMode)
	if err != nil {
		return fmt.Errorf("apikey store: create %s: %w", tmp, err)
	}
	w := bufio.NewWriter(f)
	for _, key := range keys {
		rec := keyRecord{
			KeyID:     key.KeyID,
			TokenHash: key.TokenHash,
			AgentID:   key.AgentID,
			CreatedAt: key.CreatedAt,
			ExpiresAt: key.ExpiresAt,
			Revoked:   key.Revoked,
		}
		b, err := json.Marshal(rec)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("apikey store: encode key %s: %w", key.KeyID, err)
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("apikey store: write %s: %w", tmp, err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("apikey store: flush %s: %w", tmp, err)
	}
	// The file is created 0600, but tighten explicitly so a store that
	// replaced a pre-existing wider-mode file cannot inherit its mode.
	if err := f.Chmod(FileMode); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("apikey store: chmod %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("apikey store: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("apikey store: rename %s -> %s: %w", tmp, s.path, err)
	}
	return nil
}
