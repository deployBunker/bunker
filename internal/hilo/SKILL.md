# Package: `internal/hilo`

## Public API

- `Graph` — in-memory dependency graph loaded from Hilo's `.vfs/graph/edges.jsonl`.
- `Edge` — single dependency edge: From, To, Rel.
- `NewGraph(projectDir, logger)` — loads the edges file from `<projectDir>/.vfs/graph/edges.jsonl`.
- `(*Graph) Related(path)` — forward edges (what this file imports).
- `(*Graph) Impact(path)` — reverse edges (what depends on this file).
- `(*Graph) Stats()` — aggregate statistics: total edges, unique files, unique deps, files with edges.
- `(*Graph) BlastRadius(path, maxDepth)` — BFS-based transitive reverse dependency closure.
- `(*Graph) Reload()` — re-read edges.jsonl for live updates.
- `GraphStats` — struct: TotalEdges, UniqueFiles, UniqueDeps, FilesWithEdges.
- `IsExternal(to)` / `IsInternal(to)` — classify edge targets by `std:` / `pkg:` prefix.

## Conventions

- Edges format follows Hilo's JSONL: `{"from":"src/main.go","to":"std:fmt","rel":"imports"}`.
- External deps are prefixed `std:` (standard library) or `pkg:` (third-party).
- Internal deps are bare relative paths from the project root.
- Thread-safe via `sync.RWMutex` — `Related`, `Impact`, `Stats` acquire read locks; `Reload` acquires write lock.
- Missing edges file is NOT an error — returns an empty graph with a warning log.

## Dependencies

- Standard library: `bufio`, `encoding/json`, `fmt`, `log/slog`, `os`, `path/filepath`, `strings`, `sync`.

## Test Patterns

- `hilo_test.go` validates: empty graph, missing edges file, graph loading, blast radius with max depth, external vs internal classification.
- Tests use a temp directory with a test edges.jsonl fixture.
- `TestGraph_ProjectDirResolution` verifies working-directory-relative path resolution.

## Pitfalls

1. **Edge direction confusion.** `Related` = forward (imports), `Impact` = reverse (imported by). Getting these backwards leads to incorrect blast radius calculations.
2. **`BlastRadius` is unbounded without maxDepth.** Large graphs can produce huge transitive closures. Always set a reasonable maxDepth (3-5 is typical).
3. **Graph is loaded once at startup.** If edges.jsonl is regenerated (e.g., after `hilo graph warm`), call `Reload()` to pick up changes.
4. **JSONL parsing is lenient.** Malformed lines are skipped with a warning — the graph may be incomplete without an error.
