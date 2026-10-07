#!/bin/bash
# QA-BUNKER-24 — regression test: pytest.ini/conftest.py-only repos get the
# venv-pytest fallback arm. Extracts detect_cmds() from bunker-qa.sh and
# exercises the pytest.ini/conftest.py arm's runner selection against fixture
# repos (pytest.ini-only) with/without a fake $HOME/tools/venv/bin/pytest.
# Mirrors the pass/fail check pattern of test_bunker_qa_classifier.sh.
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$HOME/worktrees/bunker-QA-BUNKER-24/.hermes/scripts/bunker-qa.sh}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1 -> $3"; pass=$((pass+1))
  else echo "  FAIL $1: expected $2 got $3"; fail=$((fail+1)); fi
}

TMPD=${TMPDIR:-/tmp}/qa_bunker_24.$$
mkdir -p "$TMPD"
trap 'rm -rf "$TMPD"' EXIT

# Extract the detect_cmds function (awk from 'detect_cmds() {' to the closing
# brace of the function — brace-closing `}` at column 0 AFTER detect_upgrade_inputs's
# call site, i.e. line of the standalone `}` that follows the function body).
# Simpler and robust: print from 'detect_cmds() {' until the line '}' that
# immediately follows 'detect_upgrade_inputs "$repo"'.
awk '
  /^detect_cmds\(\) \{/ {f=1}
  f {print}
  f && /detect_upgrade_inputs "\$repo"/ {grab=1}
  grab && /^\}$/ {exit}
' "$HARNESS" > "$TMPD/dc_raw.sh"
if ! grep -q '^detect_cmds() {' "$TMPD/dc_raw.sh" || ! grep -q '^}$' "$TMPD/dc_raw.sh"; then
  echo "FAIL: could not extract detect_cmds from $HARNESS"; exit 1
fi

# Guard drift check: the pytest.ini/conftest.py arm must carry the same
# venv guard as the pyproject arm (grep-verifiable acceptance criterion 2).
if grep -A4 'pytest.ini' "$TMPD/dc_raw.sh" | grep -q 'command -v pytest'; then
  check "venv guard present between elif and python3 assignment" ok ok
else
  check "venv guard present between elif and python3 assignment" ok MISSING
fi

# --- fixture: pytest.ini-only repo (no pyproject.toml, no conftest.py ambiguity) ---
REPO="$TMPD/repo"
mkdir -p "$REPO" "$TMPD/home/tools/venv/bin" "$TMPD/pathbin" "$TMPD/nohome"
printf '[pytest]\n' > "$REPO/pytest.ini"
printf '#!/bin/sh\n' > "$TMPD/home/tools/venv/bin/pytest"
chmod +x "$TMPD/home/tools/venv/bin/pytest"
printf '#!/bin/sh\n' > "$TMPD/pathbin/pytest"
chmod +x "$TMPD/pathbin/pytest"

# Stub out the tail helper detect_cmds calls so the extracted slice is
# self-contained: detect_upgrade_inputs sets upgrade-cell variables only.
printf 'detect_upgrade_inputs() { :; }\n' > "$TMPD/dc.sh"
cat "$TMPD/dc_raw.sh" >> "$TMPD/dc.sh"

# Probe wrapper: sources the extracted function, runs it with a pinned
# HOME/PATH, and prints the resulting native_cmd (written to a file, no
# inline bash -c).
# detect_cmds is a function in the host driver; its visible contract is the
# globals it sets (DETECT_INSTALL / DETECT_NATIVE / DETECT_CI / DETECT_BIN) —
# native_cmd itself is `local` and does not survive the call.
cat > "$TMPD/probe.sh" <<'EOF'
#!/bin/bash
source "$1"
HOME="$2" PATH="$3" detect_cmds "$4" >/dev/null 2>&1
echo "${DETECT_NATIVE:-UNSET}"
EOF
chmod +x "$TMPD/probe.sh"

# This host ships /usr/bin/pytest, so probe 1 pins PATH to an empty dir to
# simulate a PATH without pytest (the harness runs agent-side, where pytest
# may be absent from PATH entirely).
mkdir -p "$TMPD/emptypath"

run_probe() { # <home> <path>
  "$TMPD/probe.sh" "$TMPD/dc.sh" "$1" "$2" "$REPO"
}

# --- probe 1: PATH lacks pytest, venv has it -> venv pytest selected ---
got=$(run_probe "$TMPD/home" "$TMPD/emptypath")
check "pytest.ini repo + venv pytest, PATH w/o pytest -> venv cmd" \
  "$TMPD/home/tools/venv/bin/pytest -x -q" "$got"

# --- probe 2: no venv dir at all -> stays python3 -m pytest -q ---
got=$(run_probe "$TMPD/nohome" "$TMPD/emptypath")
check "pytest.ini repo + no venv -> python3 -m pytest" \
  "python3 -m pytest -q" "$got"

# --- probe 3: PATH HAS pytest -> plain python3 -m pytest (venv unused) ---
got=$(run_probe "$TMPD/home" "$TMPD/pathbin:/usr/bin:/bin")
check "PATH has pytest -> python3 -m pytest (venv guard skipped)" \
  "python3 -m pytest -q" "$got"

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
