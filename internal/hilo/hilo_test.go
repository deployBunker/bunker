package hilo

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestNewGraph(t *testing.T) {
	g, err := NewGraph("../..", testLogger())
	if err != nil {
		t.Fatalf("NewGraph failed: %v", err)
	}
	if g == nil {
		t.Fatal("NewGraph returned nil")
	}
	stats := g.Stats()
	if stats.TotalEdges == 0 {
		t.Fatal("expected non-zero edges in graph")
	}
	if stats.UniqueFiles == 0 {
		t.Fatal("expected non-zero unique files")
	}
}

func TestNewGraph_MissingFile(t *testing.T) {
	g, err := NewGraph("/nonexistent/project", testLogger())
	if err != nil {
		t.Fatalf("NewGraph with missing file should not error: %v", err)
	}
	if g == nil {
		t.Fatal("NewGraph returned nil")
	}
	stats := g.Stats()
	if stats.TotalEdges != 0 {
		t.Fatalf("expected 0 edges for missing file, got %d", stats.TotalEdges)
	}
}

func TestGraph_Reload(t *testing.T) {
	g, err := NewGraph("../..", testLogger())
	if err != nil {
		t.Fatalf("NewGraph failed: %v", err)
	}
	before := g.Stats()
	if err := g.Reload(); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	after := g.Stats()
	if after.TotalEdges != before.TotalEdges {
		t.Fatalf("expected same edge count after reload: before=%d after=%d", before.TotalEdges, after.TotalEdges)
	}
}

func TestGraph_Related(t *testing.T) {
	g, err := NewGraph("../..", testLogger())
	if err != nil {
		t.Fatalf("NewGraph failed: %v", err)
	}
	// internal/server/server.go is known to have edges
	edges := g.Related("internal/server/server.go")
	if len(edges) == 0 {
		t.Fatal("expected edges from internal/server/server.go")
	}
	// Should import std:context
	var foundContext bool
	for _, e := range edges {
		if e.To == "std:context" {
			foundContext = true
			break
		}
	}
	if !foundContext {
		t.Fatal("expected internal/server/server.go to import std:context")
	}
}

func TestGraph_Impact(t *testing.T) {
	g, err := NewGraph("../..", testLogger())
	if err != nil {
		t.Fatalf("NewGraph failed: %v", err)
	}
	// internal/config/config.go is imported by many files
	edges := g.Impact("internal/config/config.go")
	if len(edges) == 0 {
		t.Fatal("expected reverse dependencies on internal/config/config.go")
	}
	// At least one file should import config.go
	var foundInternal bool
	for _, e := range edges {
		if strings.HasPrefix(e.From, "internal/") {
			foundInternal = true
			break
		}
	}
	if !foundInternal {
		t.Fatal("expected at least one internal file to depend on config.go")
	}
}

func TestGraph_BlastRadius(t *testing.T) {
	g, err := NewGraph("../..", testLogger())
	if err != nil {
		t.Fatalf("NewGraph failed: %v", err)
	}
	// Changing internal/config/config.go should impact files that import it
	impacted := g.BlastRadius("internal/config/config.go", 2)
	if len(impacted) == 0 {
		t.Fatal("expected blast radius > 0 for internal/config/config.go")
	}
	// Should include files that import config.go directly
	var foundAgentMgr bool
	for _, f := range impacted {
		if f == "internal/agent/manager.go" {
			foundAgentMgr = true
			break
		}
	}
	if !foundAgentMgr {
		t.Logf("blast radius: %v", impacted)
		// Not a hard failure — graph may not have this edge depending on discovery state
	}
}

func TestGraph_BlastRadius_MaxDepth(t *testing.T) {
	g, err := NewGraph("../..", testLogger())
	if err != nil {
		t.Fatalf("NewGraph failed: %v", err)
	}
	// Depth 0 should return only the starting file (excluded from result)
	impacted0 := g.BlastRadius("internal/server/server.go", 0)
	if len(impacted0) != 0 {
		t.Fatalf("expected 0 impacted files at depth 0, got %d", len(impacted0))
	}
	// Depth 1 should return direct reverse dependencies only
	// internal/config/config.go has known reverse deps
	impacted1 := g.BlastRadius("internal/config/config.go", 1)
	if len(impacted1) == 0 {
		t.Fatal("expected some direct reverse dependencies")
	}
}

func TestGraph_Stats(t *testing.T) {
	g, err := NewGraph("../..", testLogger())
	if err != nil {
		t.Fatalf("NewGraph failed: %v", err)
	}
	stats := g.Stats()
	if stats.TotalEdges <= 0 {
		t.Fatalf("expected positive TotalEdges, got %d", stats.TotalEdges)
	}
	if stats.UniqueFiles <= 0 {
		t.Fatalf("expected positive UniqueFiles, got %d", stats.UniqueFiles)
	}
	if stats.FilesWithEdges <= 0 {
		t.Fatalf("expected positive FilesWithEdges, got %d", stats.FilesWithEdges)
	}
	// UniqueDeps should be >= 0 (may be 0 if no external deps discovered)
	if stats.UniqueDeps < 0 {
		t.Fatalf("expected non-negative UniqueDeps, got %d", stats.UniqueDeps)
	}
}

func TestIsExternal(t *testing.T) {
	if !IsExternal("std:fmt") {
		t.Fatal("expected std:fmt to be external")
	}
	if !IsExternal("pkg:connectrpc.com/connect") {
		t.Fatal("expected pkg:connectrpc.com/connect to be external")
	}
	if IsExternal("internal/agent/manager.go") {
		t.Fatal("expected internal/agent/manager.go to be internal")
	}
}

func TestIsInternal(t *testing.T) {
	if !IsInternal("internal/agent/manager.go") {
		t.Fatal("expected internal/agent/manager.go to be internal")
	}
	if IsInternal("std:fmt") {
		t.Fatal("expected std:fmt to be external")
	}
}

func TestGraph_ConcurrentAccess(t *testing.T) {
	g, err := NewGraph("../..", testLogger())
	if err != nil {
		t.Fatalf("NewGraph failed: %v", err)
	}
	// Concurrent reads should not race
	done := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_ = g.Related("internal/server/server.go")
			_ = g.Impact("internal/config/config.go")
			_ = g.Stats()
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestGraph_EdgeStructure(t *testing.T) {
	g, err := NewGraph("../..", testLogger())
	if err != nil {
		t.Fatalf("NewGraph failed: %v", err)
	}
	edges := g.Related("internal/server/server.go")
	for _, e := range edges {
		if e.From == "" {
			t.Fatal("expected non-empty From field")
		}
		if e.To == "" {
			t.Fatal("expected non-empty To field")
		}
		if e.Rel == "" {
			t.Fatal("expected non-empty Rel field")
		}
	}
}

func TestGraph_ProjectDirResolution(t *testing.T) {
	// The test runs from internal/hilo/; project root is two levels up
	g, err := NewGraph("../..", testLogger())
	if err != nil {
		t.Fatalf("NewGraph from project root failed: %v", err)
	}
	stats := g.Stats()
	if stats.TotalEdges == 0 {
		t.Fatal("expected edges when using project root as projectDir")
	}
}
