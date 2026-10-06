package programalias

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// DefaultFileName is the alias store's file name inside the daemon's state
// directory (alongside agents.jsonl).
const DefaultFileName = "program-aliases.json"

// EnvPathOverride is the environment variable an operator can use to point
// the alias store somewhere other than the daemon state directory.  It exists
// for the same reason BUNKERD_SAFETY_PRESET does: the store path is a
// deployment detail that tests and one-off runs must be able to relocate
// without a config-file edit.
const EnvPathOverride = "BUNKERD_PROGRAM_ALIASES_PATH"

// storeVersion is the on-disk schema version.  A file carrying any other
// version is refused rather than guessed at.
const storeVersion = 1

// storeFile is the JSON document persisted by Registry.Save.
type storeFile struct {
	Version int     `json:"version"`
	Aliases []Alias `json:"aliases"`
}

// ErrNotFound is returned by Registry.Get / Registry.Delete for an unknown
// alias name.
var ErrNotFound = errors.New("program alias not found")

// ErrSchemaVersion refuses a store file written by an incompatible version.
var ErrSchemaVersion = errors.New("unsupported program-alias store version")

// Registry is the durable alias store.  It is safe for concurrent use: the
// daemon serves execs from many goroutines and the CRUD RPCs from others.
//
// Durability shape (mirrors internal/registry but for a whole-document
// store): a mutation writes the complete document to a temporary file in the
// same directory, fsyncs it, renames it over the target, then fsyncs the
// directory.  A crash therefore leaves either the old document or the new
// one, never a torn one.  Only the daemon process writes, so a process-local
// mutex is sufficient serialization; there is no cross-process writer to
// lock against.
type Registry struct {
	path string

	mu      sync.RWMutex
	aliases map[string]Alias
	loaded  bool
}

// NewRegistry returns a registry backed by path.  Nothing is read until the
// first operation, so constructing a registry is side-effect free.
func NewRegistry(path string) *Registry {
	return &Registry{path: path}
}

// Path returns the backing file path.
func (r *Registry) Path() string { return r.path }

// DefaultPath derives the alias store path for a daemon whose agent registry
// lives at agentsPath (the production value is
// /var/lib/bunkerd/agents.jsonl, so the aliases land next to it).  The
// environment override, when set, wins.
func DefaultPath(agentsPath string) string {
	if v := strings.TrimSpace(os.Getenv(EnvPathOverride)); v != "" {
		return v
	}
	dir := filepath.Dir(agentsPath)
	if dir == "" || dir == "." {
		dir = "."
	}
	return filepath.Join(dir, DefaultFileName)
}

// List returns every alias sorted by name.  A missing store file is an empty
// registry, not an error: a daemon that has never registered an alias must
// serve execs normally.
func (r *Registry) List() ([]Alias, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Alias, 0, len(r.aliases))
	for _, a := range r.aliases {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns the alias registered under name.
func (r *Registry) Get(name string) (Alias, bool, error) {
	if err := r.load(); err != nil {
		return Alias{}, false, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.aliases[name]
	return a, ok, nil
}

// Put inserts or replaces an alias.  The alias is validated (name/image
// grammar and mount SHAPE) before anything is written; the HOME-ONLY mount
// rule is applied at exec time by ValidateMount, because registration does not
// know which agent will run it.
func (r *Registry) Put(a Alias) error {
	if err := ValidateAlias(a); err != nil {
		return err
	}
	if err := r.load(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]Alias, len(r.aliases)+1)
	for k, v := range r.aliases {
		next[k] = v
	}
	next[a.Name] = a
	return r.persistLocked(next)
}

// Delete removes an alias.  An unknown name is ErrNotFound so the CLI can
// report a real miss instead of a silent success.
func (r *Registry) Delete(name string) error {
	if err := r.load(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.aliases[name]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	next := make(map[string]Alias, len(r.aliases))
	for k, v := range r.aliases {
		if k == name {
			continue
		}
		next[k] = v
	}
	return r.persistLocked(next)
}

// load reads the store once.  A subsequent mutation keeps the in-memory view
// authoritative, so a concurrent Put cannot be lost by re-reading.
func (r *Registry) load() error {
	r.mu.RLock()
	if r.loaded {
		r.mu.RUnlock()
		return nil
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded {
		return nil
	}
	aliases := map[string]Alias{}
	data, err := os.ReadFile(r.path)
	switch {
	case err == nil:
		var doc storeFile
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parse program-alias store %s: %w", r.path, err)
		}
		if doc.Version != storeVersion {
			return fmt.Errorf("%w: %d (want %d)", ErrSchemaVersion, doc.Version, storeVersion)
		}
		for _, a := range doc.Aliases {
			aliases[a.Name] = a
		}
	case errors.Is(err, os.ErrNotExist):
		// Empty registry.
	default:
		return fmt.Errorf("read program-alias store %s: %w", r.path, err)
	}
	r.aliases = aliases
	r.loaded = true
	return nil
}

// persistLocked writes the whole document atomically.  Callers hold r.mu.
func (r *Registry) persistLocked(next map[string]Alias) error {
	names := make([]string, 0, len(next))
	for k := range next {
		names = append(names, k)
	}
	sort.Strings(names)
	doc := storeFile{Version: storeVersion, Aliases: make([]Alias, 0, len(names))}
	for _, n := range names {
		doc.Aliases = append(doc.Aliases, next[n])
	}
	payload, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode program-alias store: %w", err)
	}
	payload = append(payload, '\n')

	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create program-alias dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".program-aliases-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp program-alias file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp program-alias file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp program-alias file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync temp program-alias file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp program-alias file: %w", err)
	}
	if err := os.Rename(tmpName, r.path); err != nil {
		return fmt.Errorf("install program-alias store: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	r.aliases = next
	r.loaded = true
	return nil
}
