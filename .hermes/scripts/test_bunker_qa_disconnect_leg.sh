#!/bin/bash
# QA-BUNKER-56-R2 — regression test: the chaos-disconnect leg must SHIP a real
# suite command to the agent. History: the generator escaped the ref on that
# line (`bash -c "\$native_cmd"` agent-side) while the rendered script never
# assigns native_cmd, so every battery rendered `bash -c ""`-equivalent:
# instant rc=0, 0-byte disc.log, cell graded UNVERIFIED forever. The res leg
# (chaos-resource) always interpolated the detected suite generator-side —
# that is the required style.
# Arms (ALL red on the pre-fix harness):
#   1. the harness RENDERS at all under its own set -u (36dbac06 un-escaped
#      suite_dur/LOGD/disc_rc in the same leg and killed every render —
#      0-byte output, rc=1 "suite_dur: unbound variable");
#   2. the rendered disc leg carries the interpolated, non-empty default guard
#      `bash -c "${native_cmd:-go test ./... -count=1}"`;
#   3. no empty `bash -c ""` payload anywhere in the render;
#   4. the dead literal agent-side ref `bash -c "$native_cmd"` is gone;
#   5. contrast: the res leg still carries the interpolated suite.
# Mirrors the env-coupling discipline of test_bunker_qa_gofirst.sh.
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bunker-qa.sh}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1"; pass=$((pass+1))
  else echo "  FAIL $1: expected [$2] got [$3]"; fail=$((fail+1)); fi
}

TMPD=${TMPDIR:-/tmp}/qa_bunker_56r2.$$
mkdir -p "$TMPD/repos/goonly"
trap 'rm -rf "$TMPD"' EXIT

# Go-shaped fixture: the suite ladder must resolve go test (go.mod wins).
printf 'module example.com/fixture\n\ngo 1.21\n' > "$TMPD/repos/goonly/go.mod"

# Render through the harness's OWN test verb (__gen-remote-detected): the full
# generator runs under its real set -uo pipefail, so a render-killing
# un-escaped generator-side ref (the 36dbac06 class) fails arm 1 loudly.
if ! bash "$HARNESS" __gen-remote-detected "$TMPD/repos/goonly" > "$TMPD/qa-run.sh" 2>/dev/null; then
  echo "  FAIL harness render exited nonzero (generator-side unbound ref or other abort)"
  echo "pass=0 fail=1"
  exit 1
fi
check "harness renders under its own set -u" yes "$([ -s "$TMPD/qa-run.sh" ] && echo yes || echo no)"

# Arm 2: the disconnect leg ships the interpolated suite in a default guard.
grep -qF 'bash -c "${native_cmd:-go test ./... -count=1}"' "$TMPD/qa-run.sh" \
  && check "disc leg: interpolated non-empty default guard" yes yes \
  || check "disc leg: interpolated non-empty default guard" yes no

# Arm 3: no empty bash -c payload anywhere in the render.
grep -qF 'bash -c ""' "$TMPD/qa-run.sh" \
  && check "no empty bash -c payload in render" yes no \
  || check "no empty bash -c payload in render" yes yes

# Arm 4: the dead literal agent-side ref is gone from the render.
grep -qF 'bash -c "$native_cmd"' "$TMPD/qa-run.sh" \
  && check "no dead agent-side native_cmd ref in render" yes no \
  || check "no dead agent-side native_cmd ref in render" yes yes

# Arm 5: contrast — the res leg keeps its interpolated suite.
grep -qF 'bash -c "$cap_probe; go test ./... -count=1"' "$TMPD/qa-run.sh" \
  && check "res leg contrast: suite still interpolated" yes yes \
  || check "res leg contrast: suite still interpolated" yes no

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
