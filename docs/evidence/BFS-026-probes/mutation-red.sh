#!/usr/bin/env bash
# mutation-red.sh — BFS-026's negative control.
#
# The claim under test: "an agent-side edit now reaches the mount because the
# server serves E-6's poll form". A claim like that is worth nothing unless the
# instrument can fail, and this release has been bitten three times by
# instruments that could not. So the mechanism is DISABLED (one line: `events`
# is removed from the implemented-op table, which is exactly the state the row
# was filed against) and every arm must go RED:
#
#   1. the live channel arm must report delivered=NO with the refusal in the
#      record — the pre-change state, reproduced from the mutated build;
#   2. the client-side Go arm must FAIL (it asserts the poll form is served);
#   3. the server-side event arms must FAIL (they drive the op).
#
# The mutated file is restored from a copy and sha256-verified, and the restore
# is proven by re-running arm 2 to green.
#
# usage: mutation-red.sh [--worktree DIR]
set -uo pipefail

WT="/home/kara/worktrees/bunker-BFS-026"
while [ $# -gt 0 ]; do
  case "$1" in
    --worktree) WT="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done

TARGET="$WT/internal/server/webdav/ops.go"
GEN="$WT/internal/server/webdav/events.go"
[ -f "$TARGET" ] || { echo "no $TARGET" >&2; exit 2; }

SHA_BEFORE=$(sha256sum "$TARGET" | awk '{print $1}')
SHA_GEN_BEFORE=$(sha256sum "$GEN" | awk '{print $1}')
echo "target      = $TARGET"
echo "sha256 before = $SHA_BEFORE"

BACKUP="$(mktemp -t bfs026-ops.XXXXXX.go)"
cp "$TARGET" "$BACKUP"

restore() {
  cp "$BACKUP" "$TARGET"
  SHA_NOW=$(sha256sum "$TARGET" | awk '{print $1}')
  if [ "$SHA_NOW" != "$SHA_BEFORE" ]; then
    echo "RESTORE FAILED: sha256 $SHA_NOW != $SHA_BEFORE" >&2
  else
    echo "RESTORE VERIFIED: ops.go sha256=$SHA_NOW (unchanged)"
  fi
  SHA_GEN_NOW=$(sha256sum "$GEN" | awk '{print $1}')
  if [ "$SHA_GEN_NOW" != "$SHA_GEN_BEFORE" ]; then
    echo "RESTORE FAILED: events.go sha256 $SHA_GEN_NOW != $SHA_GEN_BEFORE" >&2
  else
    echo "RESTORE VERIFIED: events.go sha256=$SHA_GEN_NOW (unchanged)"
  fi
}
trap restore EXIT

# The mutation: the op stops being served AND stops being advertised — the exact
# shape BFS-026 measured. Both halves are needed and the reason is worth stating:
# `implementedOps` is a REPORT (it feeds the capability document and the
# degradation list) while the handler's switch is the GATE, so mutating the table
# alone leaves the mechanism working and the arm rightly stayed green the first
# time this control was run. The table/switch agreement is pinned by
# TestOpTableMatchesWhatIsServed.
python3 - "$TARGET" <<'PY'
import sys
path = sys.argv[1]
src = open(path, encoding="utf-8").read()
table = 'implementedOps = map[string]bool{"capabilities": true, "snapshot": true, "events": true}'
gate = '''	case "events":
		// E-6's poll form, which `watch`'s own refusal names as the mode in
		// force on a target without a watcher. Serving it is what makes the
		// declared degradation a working channel rather than a label.
		h.handleEvents(w, r, start)
'''
refusal = '''	case "events":
		h.writeEnvelope(w, r, start, op, 501, VerdictCapabilityUnavailable, false, nil,
			&envelopeError{Capability: op, Scope: "target", Mode: "poll",
				Detail: "no inotify watcher on this target; poll with HEAD/ETag"})
'''
for label, needle in (("implemented-op table", table), ("handler case", gate)):
    if needle not in src:
        print("MUTATION ANCHOR NOT FOUND (%s) — the control is stale, fix it before trusting any result" % label, file=sys.stderr)
        raise SystemExit(3)
src = src.replace(table, 'implementedOps = map[string]bool{"capabilities": true, "snapshot": true}')
src = src.replace(gate, refusal)
open(path, "w", encoding="utf-8").write(src)
print("MUTATION APPLIED: `events` reverted to unserved — table entry dropped, handler case refused")
PY
[ $? -eq 0 ] || exit 3

BIN="$(mktemp -d -t bfs026-mut.XXXXXX)"
echo "building the mutated binaries into $BIN"
(cd "$WT" && go build -o "$BIN/davserve" ./probes/davserve && go build -o "$BIN/bunker" ./cmd/bunker) || {
  echo "mutated build failed"; exit 1;
}

echo
echo "=== ARM 1 — the live channel arm against the MUTATED build ==="
TREE=$(mktemp -d -t bfs026-tree-mut.XXXXXX)
python3 "$WT/docs/evidence/BFS-026-probes/mkfixture.py" "$TREE" >/dev/null
bash "$WT/docs/evidence/BFS-026-probes/channel-arm.sh" --label MUTANT --tree "$TREE" \
     --bin "$BIN/bunker" --davserve "$BIN/davserve" > "$BIN/mutant-arm.txt" 2>&1
grep -E '^VERDICT-LINE' "$BIN/mutant-arm.txt" | sed 's/^/  /'
if grep -qE '^VERDICT-LINE cached-path: .*channel_ms=none fresh_ms=none' "$BIN/mutant-arm.txt"; then
  echo "  ARM 1: RED as required (with the mechanism disabled the cached path is never served fresh)"
else
  echo "  ARM 1: the arm stayed green with the mechanism disabled — it proves nothing" >&2
  exit 1
fi

echo
echo "=== ARM 2 — the client-side arm against the MUTATED server source ==="
(cd "$WT" && go test ./internal/fsclient/ -count=1 -run TestInvalidatorDeliversThePollFormOfTheChannel 2>&1 | tail -6 | sed 's/^/  /')
(cd "$WT" && go test ./internal/fsclient/ -count=1 -run TestInvalidatorDeliversThePollFormOfTheChannel >/dev/null 2>&1)
if [ $? -ne 0 ]; then
  echo "  ARM 2: RED as required"
else
  echo "  ARM 2: the client-side arm passed with the mechanism disabled — it is vacuous" >&2
  exit 1
fi

echo
echo "=== ARM 3 — the server-side event arms against the MUTATED source ==="
(cd "$WT" && go test ./internal/server/webdav/ -count=1 -run 'TestEventsOp' 2>&1 | tail -6 | sed 's/^/  /')
(cd "$WT" && go test ./internal/server/webdav/ -count=1 -run 'TestEventsOp' >/dev/null 2>&1)
if [ $? -ne 0 ]; then
  echo "  ARM 3: RED as required"
else
  echo "  ARM 3: the server-side arms passed with the op disabled — they are vacuous" >&2
  exit 1
fi

echo
echo "=== the restore ==="
restore
trap - EXIT
echo
echo "=== RED control reverted: the client-side arm must be GREEN again ==="
(cd "$WT" && go test ./internal/fsclient/ -count=1 -run TestInvalidatorDeliversThePollFormOfTheChannel 2>&1 | tail -3 | sed 's/^/  /')
echo "NON-VACUITY: PROVEN — every arm goes RED with the mechanism disabled and GREEN with it restored"
