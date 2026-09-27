# Criterion 6 — independent QA: the PUSHED state, not anyone's self-report

Every row in this release was verified by its worker, and most were re-verified by the release driver.
Both of those have the same blind spot: **the commit guard verifies the WORKING TREE, not the stored
commit.** That is not theory — it already bit this release once, when `f37c2a8` recorded the client's
23 new files but left `go.mod`/`go.sum`/`cmd/bunker/main.go` unstaged, so the guard passed on a dirty
tree while the stored commit did not compile. It was only caught because the merge was rebuilt by hand.

So this check ignores every working tree and clones **what is actually pushed**.

## Method

    Q=/tmp/qa-clone-$(date +%H%M%S)
    git clone --no-local /home/kara/bunker "$Q"
    cd "$Q" && go build ./... && go vet ./... && go test -count=1 ./...

`--no-local` forces a real clone over the transport rather than a hardlinked copy, so nothing can be
inherited from the source tree's working state. Reproduce with `docs/evidence/QA-fresh-clone.sh`.

## Result

| check | result |
|---|---|
| cloned commit | the pushed `main` head |
| `go build ./...` | rc=0, no output |
| `go vet ./...` | rc=0, no output |
| `go test -count=1 ./...` | **27 packages ok, 0 FAIL**, 7 with no test files |
| wall time | **62.49 s** |
| `docs/evidence/` present in the clone | 20 files |

## What this establishes

1. **The pushed state builds and passes on a machine with no history of it.** No stale build cache, no
   dirty working tree, no local-only file can be propping it up.
2. **The `d1c0659` lesson is closed for real.** `git show --stat d1c0659` on the clone carries
   `cmd/bunker/main.go`, `go.mod` and `go.sum` — so the wiring the broken commit missed is genuinely in
   the pushed history, not just in a working tree.
3. **The evidence is in the release, not only on the machine that produced it.** All 20
   `docs/evidence/` files are present in the clone, so the findings are re-findable by someone who was
   never here — which is the standard this release is being audited by.

## What this does NOT establish

- Not a chaos cell. It is a clean-room green; failure injection is a separate check.
- Not a substitute for per-row verification. It proves the tree is sound, not that every row's claim is
  true.
- It does not run the live-server E2E battery on a real host, and it cannot: the isolation rules for
  this release forbid touching the deployed host.
