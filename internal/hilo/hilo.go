// Package hilo provides integration with the Hilo codebase knowledge graph.
// It reads pre-computed graph data from .vfs/ and exposes dependency analysis
// APIs for the bunkerd server and CLI.
package hilo

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Edge represents a single dependency edge in the Hilo graph.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Rel  string `json:"rel"`
}

// Graph holds the parsed dependency graph for the project.
type Graph struct {
	edges      []Edge
	byFrom     map[string][]Edge
	byTo       map[string][]Edge
	mu         sync.RWMutex
	logger     *slog.Logger
	projectDir string
}

// NewGraph creates a new Graph and loads edges from .vfs/graph/edges.jsonl.
func NewGraph(projectDir string, logger *slog.Logger) (*Graph, error) {
	g := &Graph{
		byFrom:     make(map[string][]Edge),
		byTo:       make(map[string][]Edge),
		logger:     logger,
		projectDir: projectDir,
	}
	if err := g.load(); err != nil {
		return nil, err
	}
	return g, nil
}

// load reads edges.jsonl from the .vfs/graph directory.
func (g *Graph) load() error {
	edgesPath := filepath.Join(g.projectDir, ".vfs", "graph", "edges.jsonl")
	f, err := os.Open(edgesPath)
	if err != nil {
		if os.IsNotExist(err) {
			g.logger.Warn("hilo edges file not found, graph empty", "path", edgesPath)
			return nil
		}
		return fmt.Errorf("open edges.jsonl: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var count int
	for scanner.Scan() {
		var e Edge
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			g.logger.Warn("skipping malformed edge line", "error", err)
			continue
		}
		g.edges = append(g.edges, e)
		g.byFrom[e.From] = append(g.byFrom[e.From], e)
		g.byTo[e.To] = append(g.byTo[e.To], e)
		count++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan edges.jsonl: %w", err)
	}
	g.logger.Info("hilo graph loaded", "edges", count)
	return nil
}

// Reload forces a re-read of the edges file.
func (g *Graph) Reload() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.edges = nil
	g.byFrom = make(map[string][]Edge)
	g.byTo = make(map[string][]Edge)
	return g.load()
}

// Related returns all edges from a given file (forward dependencies).
func (g *Graph) Related(path string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.byFrom[path]
}

// Impact returns all files that depend on the given file (reverse dependencies).
func (g *Graph) Impact(path string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.byTo[path]
}

// Stats returns aggregate statistics about the graph.
func (g *Graph) Stats() GraphStats {
	g.mu.RLock()
	defer g.mu.RUnlock()

	uniqueDeps := make(map[string]struct{})
	uniqueFiles := make(map[string]struct{})
	for _, e := range g.edges {
		uniqueFiles[e.From] = struct{}{}
		uniqueFiles[e.To] = struct{}{}
		if strings.HasPrefix(e.To, "std:") || strings.HasPrefix(e.To, "pkg:") {
			uniqueDeps[e.To] = struct{}{}
		}
	}

	return GraphStats{
		TotalEdges:     len(g.edges),
		UniqueFiles:    len(uniqueFiles),
		UniqueDeps:     len(uniqueDeps),
		FilesWithEdges: len(g.byFrom),
	}
}

// GraphStats holds aggregate statistics.
type GraphStats struct {
	TotalEdges     int `json:"total_edges"`
	UniqueFiles    int `json:"unique_files"`
	UniqueDeps     int `json:"unique_deps"`
	FilesWithEdges int `json:"files_with_edges"`
}

// IsExternal returns true if the dependency is an external package.
func IsExternal(to string) bool {
	return strings.HasPrefix(to, "std:") || strings.HasPrefix(to, "pkg:")
}

// IsInternal returns true if the dependency is another project file.
func IsInternal(to string) bool {
	return !IsExternal(to)
}

// BlastRadius returns the transitive closure of files impacted by a change to path.
// It walks up to maxDepth levels of reverse dependencies.
func (g *Graph) BlastRadius(path string, maxDepth int) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	seen := make(map[string]struct{})
	queue := []string{path}
	depth := map[string]int{path: 0}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if depth[current] >= maxDepth {
			continue
		}

		for _, e := range g.byTo[current] {
			if _, ok := seen[e.From]; !ok {
				seen[e.From] = struct{}{}
				queue = append(queue, e.From)
				depth[e.From] = depth[current] + 1
			}
		}
	}

	result := make([]string, 0, len(seen))
	for f := range seen {
		if f != path {
			result = append(result, f)
		}
	}
	return result
}
