#!/bin/bash
# QA-BUNKER-32 — regression test: go.mod must win over package.json when BOTH
# exist (Go is the primary language; the npm manifest is incidental, e.g. docs
# tooling). Extracts detect_cmds() from bunker-qa.sh and probes fixture repos:
#   (1) go.mod + package.json  -> Go install + Go native (THE FIX; pre-fix this
#       graded 'npm ci' + 'npm test' and the Go suite never ran — dexdat-memory
#       carried 479 _test.go files yet detected 'npm test', native.log
#       'npm error Missing script: "test"')
#   (2) package.json only      -> npm detection intact (regression guard for
#       the 9router/vitest families; QA-9ROUTER-26 independent-tests install
#       must survive the reorder untouched)
#   (3) go.mod only            -> Go detection intact
#   (4) pnpm-lock.yaml only    -> pnpm arm still reachable behind go.mod
# Mirrors the extraction pattern of test_bunker_qa_pytestini_arm.sh.
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bunker-qa.sh}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1 -> $3"; pass=$((pass+1))
  else echo "  FAIL $1: expected [$2] got [$3]"; fail=$((fail+1)); fi
}

TMPD=${TMPDIR:-/tmp}/qa_bunker_32.$$
mkdir -p "$TMPD"
trap 'rm -rf "$TMPD"' EXIT

# Extract the detect_cmds function (awk from 'detect_cmds() {' to the closing
# brace after 'detect_upgrade_inputs "$repo"' — same pattern as the QA-BUNKER-24
# extraction test).
awk '
  /^detect_cmds\(\) \{/ {f=1}
  f {print}
  f && /detect_upgrade_inputs "\$repo"/ {grab=1}
  grab && /^\}$/ {exit}
' "$HARNESS" > "$TMPD/dc_raw.sh"
if ! grep -q '^detect_cmds() {' "$TMPD/dc_raw.sh" || ! grep -q '^}$' "$TMPD/dc_raw.sh"; then
  echo "FAIL: could not extract detect_cmds from $HARNESS"; exit 1
fi

# Stub the tail helper so the extracted slice is self-contained.
printf 'detect_upgrade_inputs() { :; }\n' > "$TMPD/dc.sh"
cat "$TMPD/dc_raw.sh" >> "$TMPD/dc.sh"

# Fixture dirs: HOME pinned to a nonexistent dir (the pyproject/pytest.ini venv
# guard reads $HOME/tools/venv/bin/pytest — no fixture reaches that arm, but the
# pin makes the probe hermetic). PATH stays AMBIENT: the npm arms parse
# package.json with jq, so an empty-path pin (as in the QA-24 test) would hide
# jq and break npm-arm detection for reasons that have nothing to do with
# precedence.
mkdir -p "$TMPD/repos/goplusnpm" "$TMPD/repos/npmonly" "$TMPD/repos/goonly" "$TMPD/repos/pnponly"
command -v jq >/dev/null 2>&1 || { echo "FAIL: jq required by the npm arms of detect_cmds"; exit 1; }

# (1) go.mod + package.json: minimal-but-valid manifests (jq parses the JSON).
printf 'module example.com/fixture\n\ngo 1.21\n' > "$TMPD/repos/goplusnpm/go.mod"
printf '{"name":"fixture","scripts":{"test":"echo npm-test"},"dependencies":{}}\n' > "$TMPD/repos/goplusnpm/package.json"

# (2) npm-only repo with a vitest test script + an independent tests/ package
# (guards the QA-9ROUTER-26 two-step install against drift from the reorder).
printf '{"name":"npmfix","scripts":{"test":"vitest run"},"devDependencies":{"vitest":"^4.0.0"}}\n' > "$TMPD/repos/npmonly/package.json"
mkdir -p "$TMPD/repos/npmonly/tests"
printf '{"name":"npmfix-tests","devDependencies":{"vitest":"^4.0.0"}}\n' > "$TMPD/repos/npmonly/tests/package.json"

# (3) go-only repo.
printf 'module example.com/gofix\n\ngo 1.21\n' > "$TMPD/repos/goonly/go.mod"

# (4) pnpm-only repo (pnpm arm still reachable when no go.mod exists).
printf 'lockfileVersion: 9.0\n' > "$TMPD/repos/pnponly/pnpm-lock.yaml"

# Probe wrapper: sources the extracted function, runs it, prints the globals.
cat > "$TMPD/probe.sh" <<'EOF'
#!/bin/bash
source "$1"
HOME="$2" PATH="$3" detect_cmds "$4" >/dev/null 2>&1
printf '%s\n%s\n' "${DETECT_INSTALL:-UNSET}" "${DETECT_NATIVE:-UNSET}"
EOF
chmod +x "$TMPD/probe.sh"

run_probe() { # <repo> -> "DETECT_INSTALL\nDETECT_NATIVE"
  "$TMPD/probe.sh" "$TMPD/dc.sh" "$TMPD/nonexistent-home" "$PATH" "$1"
}

echo "QA-BUNKER-32: go.mod precedence over package.json in suite detection"
echo

# --- probe 1: go.mod + package.json -> Go wins on BOTH ladders ---
out=$(run_probe "$TMPD/repos/goplusnpm")
inst=$(head -1 <<<"$out"); nat=$(tail -1 <<<"$out")
check "go.mod+package.json -> DETECT_INSTALL go build" "go build ./..." "$inst"
check "go.mod+package.json -> DETECT_NATIVE go test" "go test ./... -count=1" "$nat"

# --- probe 2: package.json only -> npm detection intact (regression guard) ---
out=$(run_probe "$TMPD/repos/npmonly")
inst=$(head -1 <<<"$out"); nat=$(tail -1 <<<"$out")
check "npm-only -> install = root + tests/ two-step" \
  "npm ci --ignore-scripts --no-audit --no-fund && cd tests && npm ci --ignore-scripts --no-audit --no-fund" "$inst"
check "npm-only (vitest script) -> native = vitest serial form" \
  "npm test -- --no-file-parallelism" "$nat"

# --- probe 3: go.mod only -> Go (unchanged arm) ---
out=$(run_probe "$TMPD/repos/goonly")
inst=$(head -1 <<<"$out"); nat=$(tail -1 <<<"$out")
check "go.mod only -> DETECT_INSTALL go build" "go build ./..." "$inst"
check "go.mod only -> DETECT_NATIVE go test" "go test ./... -count=1" "$nat"

# --- probe 4: pnpm-lock only -> pnpm arm still reachable behind go.mod ---
out=$(run_probe "$TMPD/repos/pnponly")
inst=$(head -1 <<<"$out"); nat=$(tail -1 <<<"$out")
check "pnpm-only -> install pnpm" \
  "corepack enable 2>/dev/null; corepack prepare --activate 2>/dev/null; pnpm install --frozen-lockfile" "$inst"
check "pnpm-only -> native pnpm test" "pnpm test" "$nat"

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
