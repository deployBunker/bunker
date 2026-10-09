#!/bin/bash
# QA-BUNKER-26 — regression test: the agent-side venv pytest TOP-UP in
# build_remote_script arms for pytest.ini/conftest.py-only repos too (it
# previously required pyproject.toml, so every suite leg on such a repo died
# 'No module named pytest' — auger chaos-resource UNVERIFIED 2026-09-27).
# Extracts the guarded top-up snippet from the REAL bunker-qa.sh text (drift
# guard included), unescapes the generated-script quoting, and exercises it
# against fixture repos with a stubbed ~/tools/venv/bin/pip.
# Mirrors the extraction pattern of test_bunker_qa_pytestini_arm.sh.
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "$0")" && pwd)/bunker-qa.sh}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1 -> $3"; pass=$((pass+1))
  else echo "  FAIL $1: expected $2 got $3"; fail=$((fail+1)); fi
}

TMPD=${TMPDIR:-/tmp}/qa_bunker_26.$$
mkdir -p "$TMPD"
trap 'rm -rf "$TMPD"' EXIT

[ -f "$HARNESS" ] || { echo "FAIL: harness not found at $HARNESS"; exit 1; }

# Extract the agent-side top-up block: from the QA-TERMINAL-JAIL-9 comment
# (start of the block) to its closing `fi` at column 0.
awk '
  /^# QA-TERMINAL-JAIL-9/ {f=1}
  f {print}
  f && /^fi$/ {exit}
' "$HARNESS" > "$TMPD/raw.sh"
if ! grep -q 'pip install pytest' "$TMPD/raw.sh" || [ "$(tail -1 "$TMPD/raw.sh")" != "fi" ]; then
  echo "FAIL: could not extract pytest top-up block from $HARNESS"; exit 1
fi

# The generated script escapes $ for its single-quoted transport; unescape so
# the fragment is directly executable in a test shell.
sed 's/\\\$/$/g' "$TMPD/raw.sh" > "$TMPD/frag.sh"

# Drift guard: the CONDITION line of the real script must name all three repo
# markers (pyproject.toml legacy + pytest.ini + conftest.py, QA-BUNKER-26).
COND_LINE=$(grep -n 'if {' "$TMPD/raw.sh" | grep 'pyproject.toml' | head -1)
if [ -n "$COND_LINE" ] \
   && printf '%s' "$COND_LINE" | grep -q 'pytest.ini' \
   && printf '%s' "$COND_LINE" | grep -q 'conftest.py'; then
  check "top-up condition arms for pyproject.toml+pytest.ini+conftest.py" ok ok
else
  check "top-up condition arms for pyproject.toml+pytest.ini+conftest.py" ok "MISSING (${COND_LINE:-none})"
fi
check "QA-TERMINAL-JAIL-9 comment preserved in block" \
  ok "$([ "$(grep -c 'QA-TERMINAL-JAIL-9' "$TMPD/raw.sh")" -ge 1 ] && echo ok || echo missing)"

# --- fixture factory -------------------------------------------------------
# make_case <name> <marker-file|none> <preinstall-pytest:0|1>
# Prints the case repo dir; fake HOME at $TMPD/<name>/home.
make_case() {
  local name=$1 marker=$2 pre=$3
  local fh="$TMPD/$name/home" repo="$TMPD/$name/repo"
  mkdir -p "$fh/tools/venv/bin" "$repo"
  : > "$fh/tools/venv/bin/python"; chmod +x "$fh/tools/venv/bin/python"
  # Stub pip: logs every invocation, "installs" pytest on success.
  printf '#!/bin/sh\necho "$*" >> "$HOME/pip-calls.log"\nif [ "$1" = install ]; then : > "$HOME/tools/venv/bin/pytest"; chmod +x "$HOME/tools/venv/bin/pytest"; fi\nexit 0\n' \
    > "$fh/tools/venv/bin/pip"
  chmod +x "$fh/tools/venv/bin/pip"
  [ "$pre" = "1" ] && { : > "$fh/tools/venv/bin/pytest"; chmod +x "$fh/tools/venv/bin/pytest"; }
  case "$marker" in
    pyproject) : > "$repo/pyproject.toml" ;;
    pytestini) : > "$repo/pytest.ini" ;;
    conftest)  : > "$repo/conftest.py" ;;
    none)      : ;;
  esac
  # Runner: stubs the cell helper, pins LOGD (unset in a bare test shell),
  # sources the unescaped fragment.
  printf 'cell() { :; }\nLOGD=%q\nsource %q\n' "$fh" "$TMPD/frag.sh" > "$TMPD/runner.sh"
  printf '%s' "$repo"
}

# pip_calls <home> -> number of pip invocations recorded
pip_calls() {
  local log="$1/pip-calls.log"
  [ -f "$log" ] && wc -l < "$log" | tr -d ' ' || echo 0
}

run_case() { # <name>: source fragment with cwd=case repo, HOME=fake home
  local name=$1
  ( cd "$TMPD/$name/repo" && env HOME="$TMPD/$name/home" bash "$TMPD/runner.sh" ) >/dev/null 2>&1
}

# --- case A: pytest.ini-only repo, venv lacks pytest -> top-up MUST run -----
repo=$(make_case a_pytestini pytestini 0)
run_case a_pytestini
check "pytest.ini-only repo: pip install pytest runs (exactly once)" 1 "$(pip_calls "$TMPD/a_pytestini/home")"
check "pytest.ini-only repo: pytest present in venv after top-up" \
  ok "$([ -x "$TMPD/a_pytestini/home/tools/venv/bin/pytest" ] && echo ok || echo missing)"

# --- case B: pytest ALREADY in venv -> NO double-run ------------------------
repo=$(make_case b_already pytestini 1)
run_case b_already
check "pytest already in venv: top-up does NOT double-run" 0 "$(pip_calls "$TMPD/b_already/home")"

# --- case C: pyproject.toml repo (legacy behavior preserved) ---------------
repo=$(make_case c_pyproject pyproject 0)
run_case c_pyproject
check "pyproject.toml repo: top-up still arms" 1 "$(pip_calls "$TMPD/c_pyproject/home")"

# --- case D: conftest.py-only repo -> top-up MUST run -----------------------
repo=$(make_case d_conftest conftest 0)
run_case d_conftest
check "conftest.py-only repo: pip install pytest runs" 1 "$(pip_calls "$TMPD/d_conftest/home")"

# --- case E: repo with no python marker -> top-up stays off -----------------
repo=$(make_case e_none none 0)
run_case e_none
check "repo without any pytest marker: top-up stays off" 0 "$(pip_calls "$TMPD/e_none/home")"

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
