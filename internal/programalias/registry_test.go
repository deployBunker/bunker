package programalias

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultFileName)
	return NewRegistry(path), path
}

func TestRegistryCRUD(t *testing.T) {
	reg, path := newTestRegistry(t)

	// Empty store: no error, empty list (a daemon that never registered an
	// alias must still serve execs).
	list, err := reg.List()
	if err != nil {
		t.Fatalf("List on missing store: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List on missing store = %d aliases, want 0", len(list))
	}

	yq := Alias{Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"}, Description: "YAML filter"}
	if err := reg.Put(yq); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("store file not written: %v", err)
	}

	got, ok, err := reg.Get("yq")
	if err != nil || !ok {
		t.Fatalf("Get(yq) = %v, %v; want found", ok, err)
	}
	if got.Image != yq.Image || len(got.Entrypoint) != 1 || got.Entrypoint[0] != "yq" {
		t.Fatalf("Get(yq) = %+v, want %+v", got, yq)
	}

	// Update is an upsert, not a duplicate.
	if err := reg.Put(Alias{Name: "yq", Image: "mikefarah/yq:4.44.3", Entrypoint: []string{"yq"}}); err != nil {
		t.Fatalf("Put (update): %v", err)
	}
	list, err = reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List after update = %d aliases, want 1", len(list))
	}
	if list[0].Image != "mikefarah/yq:4.44.3" {
		t.Fatalf("update did not land: %+v", list[0])
	}

	// A second alias sorts by name.
	if err := reg.Put(Alias{Name: "jq", Image: "ghcr.io/jqlang/jq:1.7.1", Entrypoint: []string{"jq"}}); err != nil {
		t.Fatalf("Put(jq): %v", err)
	}
	list, err = reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Name != "jq" || list[1].Name != "yq" {
		t.Fatalf("List not sorted: %+v", list)
	}

	// Delete.
	if err := reg.Delete("jq"); err != nil {
		t.Fatalf("Delete(jq): %v", err)
	}
	if _, ok, _ := reg.Get("jq"); ok {
		t.Fatal("jq still present after Delete")
	}
	if err := reg.Delete("jq"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete(jq) = %v, want ErrNotFound", err)
	}
}

// TestRegistryPersistsAcrossInstances proves durability: the document survives
// a fresh Registry over the same path (the daemon-restart path).
func TestRegistryPersistsAcrossInstances(t *testing.T) {
	reg, path := newTestRegistry(t)
	want := Alias{
		Name:        "toolchain",
		Image:       "golang:1.26-alpine",
		Entrypoint:  []string{"go"},
		Network:     true,
		Description: "go toolchain",
		Mounts: []Mount{
			{Host: "/home/bunker-abc123/src", Container: "/home/bunker-abc123/src", ReadOnly: true},
		},
	}
	if err := reg.Put(want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	fresh := NewRegistry(path)
	got, ok, err := fresh.Get("toolchain")
	if err != nil || !ok {
		t.Fatalf("fresh Get = %v, %v; want found", ok, err)
	}
	if got.Image != want.Image || !got.Network || got.Description != want.Description {
		t.Fatalf("fresh Get = %+v, want %+v", got, want)
	}
	if len(got.Mounts) != 1 || !got.Mounts[0].ReadOnly || got.Mounts[0].Host != want.Mounts[0].Host {
		t.Fatalf("fresh Get mounts = %+v, want %+v", got.Mounts, want.Mounts)
	}
}

func TestRegistryPutRejectsInvalid(t *testing.T) {
	reg, _ := newTestRegistry(t)
	if err := reg.Put(Alias{Name: "bad name", Image: "yq"}); !errors.Is(err, ErrNameInvalid) {
		t.Fatalf("Put(bad name) = %v, want ErrNameInvalid", err)
	}
	if err := reg.Put(Alias{Name: "yq", Image: "yq; rm -rf /"}); !errors.Is(err, ErrImageInvalid) {
		t.Fatalf("Put(bad image) = %v, want ErrImageInvalid", err)
	}
	if err := reg.Put(Alias{Name: "yq", Image: ""}); !errors.Is(err, ErrImageRequired) {
		t.Fatalf("Put(no image) = %v, want ErrImageRequired", err)
	}
	list, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("invalid Put mutated the store: %+v", list)
	}
}

func TestRegistryRejectsUnknownSchemaVersion(t *testing.T) {
	reg, path := newTestRegistry(t)
	doc := map[string]any{"version": 99, "aliases": []any{}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := reg.List(); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("List with version 99 = %v, want ErrSchemaVersion", err)
	}
}

func TestRegistryRejectsMalformedStore(t *testing.T) {
	reg, path := newTestRegistry(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := reg.List(); err == nil {
		t.Fatal("List on malformed store = nil, want error")
	}
}

// TestRegistryStoreFileShape pins the on-disk document: versioned, 0600, and
// pretty-printed so an operator can read and hand-edit it.
func TestRegistryStoreFileShape(t *testing.T) {
	reg, path := newTestRegistry(t)
	if err := reg.Put(Alias{Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("store mode = %o, want 600", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc storeFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Version != storeVersion {
		t.Fatalf("version = %d, want %d", doc.Version, storeVersion)
	}
	if len(doc.Aliases) != 1 || doc.Aliases[0].Name != "yq" {
		t.Fatalf("aliases = %+v", doc.Aliases)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv(EnvPathOverride, "")
	got := DefaultPath("/var/lib/bunkerd/agents.jsonl")
	if want := "/var/lib/bunkerd/" + DefaultFileName; got != want {
		t.Fatalf("DefaultPath = %q, want %q", got, want)
	}
	t.Setenv(EnvPathOverride, "/tmp/custom-aliases.json")
	if got := DefaultPath("/var/lib/bunkerd/agents.jsonl"); got != "/tmp/custom-aliases.json" {
		t.Fatalf("DefaultPath with override = %q", got)
	}
}

func TestImageCache(t *testing.T) {
	var c ImageCache
	if c.Has("yq:4") {
		t.Fatal("zero-value cache reported a hit")
	}
	c.Add("yq:4")
	if !c.Has("yq:4") {
		t.Fatal("Add did not register the image")
	}
	c.Forget("yq:4")
	if c.Has("yq:4") {
		t.Fatal("Forget did not drop the image")
	}
	// Nil receiver is safe (an unwired service).
	var nilCache *ImageCache
	if nilCache.Has("x") {
		t.Fatal("nil cache reported a hit")
	}
	nilCache.Add("x")
	nilCache.Forget("x")
}
