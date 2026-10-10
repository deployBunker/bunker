# internal/hilo — graph state files and corruption behavior

QA-BUNKER-B12 + QA-BUNKER-9: this documents (and `hilo_corruption_test.go`
pins) what happens to the dependency graph when the state under
`.vfs/graph/` is missing, empty, truncated, or corrupt.

## Which files exist under `.vfs/graph/`

| File | Who uses it |
| --- | --- |
| `edges.jsonl` | The ONLY graph file **bunkerd** reads (`Graph.load`). One JSON object per line: `{"from":"...","to":"...","rel":"imports"}`. Written by the Hilo CLI. |
| `graph.db` | The **Hilo CLI's own store**. No code in this repository reads or writes it. Truncating or corrupting `graph.db` cannot affect bunkerd — the QA chaos cell did exactly that and the daemon was unaffected for this reason. |

## Defined outcomes for damaged `edges.jsonl`

`NewGraph` / `Reload` never panic on bad input. The outcomes are:

| Input | Outcome |
| --- | --- |
| File missing | Warning logged (`hilo edges file not found, graph empty`), empty graph, nil error. |
| File empty (0 bytes) | Empty graph, nil error, no warning. |
| Truncated mid-line (partial final JSON object) | The partial line is skipped with a warning (`skipping malformed edge line`); all complete lines load; nil error. |
| Malformed or blank line between valid lines | Same warn-and-skip: only the bad line is lost. (Blank lines fail `json.Unmarshal` on empty bytes, so they warn too — pinned, deliberately not special-cased.) |
| JSON that parses but is not an edge (`{}`) | Accepted as a zero-value edge — the loader validates syntax, not fields. |
| Unreadable file (permissions / IO error, but present) | `NewGraph` returns an `open edges.jsonl: ...` error and a nil graph. |
| A single line > 64KiB (bufio.Scanner token limit) | `NewGraph` returns a `scan edges.jsonl: ...` error and a nil graph. |

## Who handles the failure

Callers own the crash posture:

- `internal/server/server.go` (`Run`, server.go:168-173): a `NewGraph`
  error is logged (`hilo graph init failed`) and the daemon **keeps
  serving** with a nil graph; the `/graph` routes are registered only when
  the graph loaded (server.go:224-225).
- `Graph.Reload` resets all in-memory state before re-reading, so after any
  corruption it converges on the fresh file state (an empty graph after
  garbage or deletion) without error; a Reload that fails on a
  scanner-level error leaves the graph empty — never stale.

## Tests

`hilo_corruption_test.go` pins every row of the tables above, including
Reload-after-corruption recovery and the daemon tolerance documented with
its `server.go` line references.
