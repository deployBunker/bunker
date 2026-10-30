#!/usr/bin/env bash
# QA-BUNKER-078 — regression test: the fresh-install artifact assertion must be
# DERIVED from the install leg, never a literal tool name.
#
# Row (verbatim): "bunker-qa.sh fresh-install no longer requires a literal
# toolsd binary; a green install on a Go-doc-target repo (cmd/ = bunker,
# bunkerd, docs-drift) grades OK/INFO, not FAIL 'no artifact found (toolsd
# ...)'; harness regression test covers the go-build shape."
#
# Arms (2-3 red on the pre-QA-BUNKER-078 harness):
#   0. the harness parses, and so does the battery it RENDERS for the AC repo;
#   1. the generator bakes the install command and its derived expectation
#      (ARTIFACT_CMD / ARTIFACT_EXPECT) — the pre-fix call read a shell
#      variable the generated script never defines, so the check was inert;
#   2. THE AC: a green `go build ./...` leg (no toolsd anywhere, nothing
#      installed) grades OK with the install tail AND a named
#      "no artifact expected for [go build ./...]" reason — never a FAIL and
#      never a toolsd mention;
#   3. a PRE-EXISTING toolsd in PATH cannot satisfy a packaging leg that
#      installed nothing (the old carve-out passed without doing any work);
#   4. a packaging leg that really installs an executable grades OK and names it;
#   5. GOPATH/bin and the repo-local ./bin are scanned (a `go install`/
#      `make build` that writes there is not a false FAIL);
#   6. QA_EXPECTED_ARTIFACT opts into a NAMED assertion (present -> OK,
#      absent -> FAIL naming the tool);
#   7. source guards: no literal-toolsd coupling survives, and the
#      QA-BUNKER-81 strings other suites pin are still present;
#   8. pre-fix control: the row's cited toolsd-only check, verbatim, still
#      FAILs the go-build fixture — a revert re-fails here.
#
# Extraction discipline mirrors test_bunker_qa_dockerdeploy_ports.sh: render the
# battery through the harness' own __gen-remote-detected hook and drive the
# RENDERED helper (no reimplementation, no template-escape guessing).
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bunker-qa.sh}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1 -> $3"; pass=$((pass+1))
  else echo "  FAIL $1: expected [$2] got [$3]"; fail=$((fail+1)); fi
}
contains() { # <desc> <needle> <haystack>
  case "$3" in
    *"$2"*) echo "  ok   $1"; pass=$((pass+1)) ;;
    *) echo "  FAIL $1: missing [$2]"; fail=$((fail+1)) ;;
  esac
}
absent() { # <desc> <needle> <haystack>
  case "$3" in
    *"$2"*) echo "  FAIL $1: unexpected [$2]"; fail=$((fail+1)) ;;
    *) echo "  ok   $1"; pass=$((pass+1)) ;;
  esac
}

TMPD="${TMPDIR:-/tmp}/qa_bunker_078.$$"
mkdir -p "$TMPD"
if [ "${QA078_KEEP:-0}" = 1 ]; then
  echo "  (QA078_KEEP=1: artifacts kept at $TMPD)"
else
  trap 'rm -rf "$TMPD"' EXIT
fi

# ── Arm 0: harness parses ────────────────────────────────────────────────────
if bash -n "$HARNESS" 2>"$TMPD/syn.err"; then
  check "harness parses under bash -n" yes yes
else
  check "harness parses under bash -n" yes "no: $(cat "$TMPD/syn.err")"
  echo; echo "pass=$pass fail=$fail"; exit 1
fi

# Fixture: the AC repo shape — go.mod with cmd/{bunker,bunkerd,docs-drift} and
# NO binary named toolsd (the detection must land on 'go build ./...').
FIX="$TMPD/repo"
mkdir -p "$FIX/cmd/bunker" "$FIX/cmd/bunkerd" "$FIX/cmd/docs-drift"
printf 'module example.com/qa078fixture\n\ngo 1.22\n' > "$FIX/go.mod"
for c in bunker bunkerd docs-drift; do
  printf 'package main\n\nfunc main() {}\n' > "$FIX/cmd/$c/main.go"
done

REND="$TMPD/qa-run.sh"
if bash "$HARNESS" __gen-remote-detected "$FIX" test-agent-0000 > "$REND" 2>"$TMPD/render.err"; then
  check "battery renders for the go-doc fixture" 0 0
else
  check "battery renders for the go-doc fixture" 0 "$(tail -2 "$TMPD/render.err")"
fi
if bash -n "$REND" 2>"$TMPD/rendersyn.err"; then
  check "rendered battery parses (shipped script is runnable)" yes yes
else
  check "rendered battery parses (shipped script is runnable)" yes "no: $(head -3 "$TMPD/rendersyn.err")"
fi
REND_TXT="$(cat "$REND")"

# ── Arm 1: the derived expectation is baked by the generator ─────────────────
contains "rendered battery carries ARTIFACT_CMD" "ARTIFACT_CMD='go build ./...'" "$REND_TXT"
contains "go build leg derives 'none'" "ARTIFACT_EXPECT='none'" "$REND_TXT"
contains "generator derives the expectation from the install shape" \
  "artifact_expectation" "$(cat "$HARNESS")"

# Extract the RENDERED artifact helpers (real $, no template escapes) and stop
# at the pre-install snapshot line that follows the block.
awk '/^install_artifact_dirs\(\) \{/{f=1}
     f{print}
     f && /^install_artifact_snapshot \|/{exit}' \
  "$REND" > "$TMPD/artifact.sh"
grep -q '^artifact_check() {' "$TMPD/artifact.sh" || {
  echo "FAIL: could not extract artifact_check from the rendered battery"; exit 1; }
bash -n "$TMPD/artifact.sh" || { echo "FAIL: extracted artifact block does not parse"; exit 1; }

EXPECT="$(sed -n "s/^ARTIFACT_EXPECT='\(.*\)'\$/\1/p" "$REND" | head -1)"
CMD="$(sed -n "s/^ARTIFACT_CMD='\(.*\)'\$/\1/p" "$REND" | head -1)"
check "baked expectation for the go-doc fixture" none "$EXPECT"
check "baked install command for the go-doc fixture" "go build ./..." "$CMD"

# Rendered-block probe: snapshot BEFORE, run the simulated install, then let the
# real artifact_check classify and hand the grade to a recording cell().
cat > "$TMPD/probe.sh" <<'PROBE'
#!/usr/bin/env bash
# args: <sandbox-home> <probe-cwd> <expect> <before> <after> <log> <out> <install-cmd> [artifact-cmd-label]
set -u
SB="$1"; CWDDIR="$2"; EXPECT="$3"; BEFORE="$4"; AFTER="$5"; LOG="$6"; OUT="$7"; INST="$8"
ARTIFACT_CMD="${9:-fixture install leg}"
export HOME="$SB/home"
export PATH="$SB/bin:$SB/home/.local/bin:/usr/bin:/bin${QA078_GO_DIR:+:$QA078_GO_DIR}"
cd "$CWDDIR" || exit 9
# The block ends with the real pre-install snapshot lines, which reference the
# battery's LOGD; give it one so sourcing the block is faithful.
LOGD="$SB/logs"; mkdir -p "$LOGD"
. "$QA078_BLOCK"
cell() { printf '%s\t%s\n' "$2" "$3" >> "$OUT"; }
ARTIFACT_EXPECT="$EXPECT"
install_artifact_snapshot | LC_ALL=C sort -u > "$BEFORE"
[ -n "$INST" ] && bash -c "$INST" >"$SB/probe.out" 2>"$SB/probe.err"
printf '%s\n' "install finished: ${QA078_TAIL:-ok}" > "$LOG"
artifact_check "$BEFORE" "$AFTER" "$LOG"
PROBE
chmod +x "$TMPD/probe.sh"
export QA078_BLOCK="$TMPD/artifact.sh"
GO_DIR="$(dirname "$(command -v go 2>/dev/null || echo /usr/bin/go)")"
# QA078_GO_DIR is set per arm (the GOPATH/bin arm) — pinning the host's user bin
# into every probe would scan this box's real toolchain instead of the fixture.

status_of() { cut -f1 "$1" 2>/dev/null | head -1; }
detail_of() { cut -f2- "$1" 2>/dev/null | head -1; }

new_sandbox() { # <name> -> echoes the sandbox root, HOME/bin + cwd prepared
  local s="$TMPD/$1"
  mkdir -p "$s/bin" "$s/home/bin" "$s/home/.local/bin" "$s/cwd"
  printf '%s\n' "$s"
}

# ── Arm 2 (THE AC): green `go build ./...`, no toolsd anywhere, no artifact ──
SB="$(new_sandbox ac)"; OUT="$SB/cell.tsv"
QA078_TAIL="$( printf 'go build ./... ok' )" "$TMPD/probe.sh" "$SB" "$SB/cwd" "$EXPECT" \
  "$SB/before.txt" "$SB/after.txt" "$SB/inst.log" "$OUT" "" "$CMD" >"$SB/probe.out" 2>"$SB/probe.err"
check "(ac) go-build leg grades OK" OK "$(status_of "$OUT")"
contains "(ac) detail keeps the install tail" "install finished: go build ./... ok" "$(detail_of "$OUT")"
contains "(ac) detail names the absent artifact" "no artifact expected for [go build ./...]" "$(detail_of "$OUT")"
contains "(ac) detail offers the opt-in" "QA_EXPECTED_ARTIFACT" "$(detail_of "$OUT")"
absent "(ac) go-build leg is never graded FAIL" "install rc=0 but no artifact found" "$(detail_of "$OUT")"
absent "(ac) no literal toolsd demand" "toolsd" "$(detail_of "$OUT")"

# ── Arm 3: a PRE-EXISTING toolsd must not satisfy a packaging leg ────────────
SB="$(new_sandbox stale-toolsd)"; OUT="$SB/cell.tsv"
printf '#!/bin/sh\nexit 0\n' > "$SB/bin/toolsd"; chmod +x "$SB/bin/toolsd"
"$TMPD/probe.sh" "$SB" "$SB/cwd" binary "$SB/before.txt" "$SB/after.txt" \
  "$SB/inst.log" "$OUT" "" >"$SB/probe.out" 2>"$SB/probe.err"
check "(stale-toolsd) packaging leg that installed nothing FAILs" FAIL "$(status_of "$OUT")"
contains "(stale-toolsd) FAIL names the derived expectation" \
  "is a packaging build/install and must leave a NEW executable" "$(detail_of "$OUT")"

# ── Arm 4: a packaging leg that really installs an executable grades OK ──────
SB="$(new_sandbox installs)"; OUT="$SB/cell.tsv"
"$TMPD/probe.sh" "$SB" "$SB/cwd" binary "$SB/before.txt" "$SB/after.txt" "$SB/inst.log" "$OUT" \
  "printf '#!/bin/sh\\nexit 0\\n' > \"\$HOME/bin/fixturetool\" && chmod +x \"\$HOME/bin/fixturetool\"" \
  >"$SB/probe.out" 2>"$SB/probe.err"
check "(installs) new executable grades OK" OK "$(status_of "$OUT")"
contains "(installs) cell names the artifact" "artifact(s): " "$(detail_of "$OUT")"
contains "(installs) cell names fixturetool" "fixturetool" "$(detail_of "$OUT")"

# ── Arm 5: GOPATH/bin and repo-local ./bin are scanned ───────────────────────
SB="$(new_sandbox gopath)"; OUT="$SB/cell.tsv"
mkdir -p "$SB/gopath/bin"
GOPATH="$SB/gopath" QA078_GO_DIR="$GO_DIR" "$TMPD/probe.sh" "$SB" "$SB/cwd" binary "$SB/before.txt" "$SB/after.txt" \
  "$SB/inst.log" "$OUT" \
  "printf '#!/bin/sh\\nexit 0\\n' > \"\$GOPATH/bin/bunkerd\" && chmod +x \"\$GOPATH/bin/bunkerd\"" \
  >"$SB/probe.out" 2>"$SB/probe.err"
check "(gopath) go install into GOPATH/bin grades OK" OK "$(status_of "$OUT")"
contains "(gopath) cell names the installed binary" "bunkerd" "$(detail_of "$OUT")"

SB="$(new_sandbox repobin)"; OUT="$SB/cell.tsv"; mkdir -p "$SB/cwd/bin"
"$TMPD/probe.sh" "$SB" "$SB/cwd" binary "$SB/before.txt" "$SB/after.txt" "$SB/inst.log" "$OUT" \
  "printf '#!/bin/sh\\nexit 0\\n' > bin/bunker && chmod +x bin/bunker" >"$SB/probe.out" 2>"$SB/probe.err"
check "(repobin) Makefile-style ./bin artifact grades OK" OK "$(status_of "$OUT")"
contains "(repobin) cell names the installed binary" "bunker" "$(detail_of "$OUT")"

# ── Arm 6: QA_EXPECTED_ARTIFACT opts into a NAMED assertion ──────────────────
SB="$(new_sandbox optin-hit)"; OUT="$SB/cell.tsv"
QA_EXPECTED_ARTIFACT=bunker "$TMPD/probe.sh" "$SB" "$SB/cwd" none "$SB/before.txt" \
  "$SB/after.txt" "$SB/inst.log" "$OUT" \
  "printf '#!/bin/sh\\nexit 0\\n' > \"\$HOME/bin/bunker\" && chmod +x \"\$HOME/bin/bunker\"" \
  >"$SB/probe.out" 2>"$SB/probe.err"
check "(optin-hit) asserted artifact present grades OK" OK "$(status_of "$OUT")"
contains "(optin-hit) cell names the asserted artifact" "artifact: bunker" "$(detail_of "$OUT")"

SB="$(new_sandbox optin-miss)"; OUT="$SB/cell.tsv"
QA_EXPECTED_ARTIFACT=bunker "$TMPD/probe.sh" "$SB" "$SB/cwd" none "$SB/before.txt" \
  "$SB/after.txt" "$SB/inst.log" "$OUT" "" >"$SB/probe.out" 2>"$SB/probe.err"
check "(optin-miss) asserted artifact absent FAILs" FAIL "$(status_of "$OUT")"
contains "(optin-miss) FAIL names the asserted artifact" "QA_EXPECTED_ARTIFACT=bunker" "$(detail_of "$OUT")"

# ── Arm 7: source guards on the harness ─────────────────────────────────────
HARNESS_TXT="$(cat "$HARNESS")"
absent "(guard) no 'command -v toolsd' assertion" "command -v toolsd" "$HARNESS_TXT"
absent "(guard) no hard-coded PREFIX/bin/toolsd" "/bin/toolsd" "$HARNESS_TXT"
absent "(guard) no toolsd carve-out in the diff" "toolsd_artifact" "$HARNESS_TXT"
absent "(guard) no *toolsd* requirement arm" '*toolsd*' "$HARNESS_TXT"
absent "(guard) the toolsd-allowance wording is gone" "toolsd is allowed when present" "$HARNESS_TXT"
contains "(guard) QA-BUNKER-81 strings other suites pin survive" "no artifact found" "$HARNESS_TXT"
contains "(guard) QA-BUNKER-81 rc=1 wording survives" \
  "rc=1: install rc=0 but no artifact found" "$HARNESS_TXT"
contains "(guard) pre-install snapshot helper survives" "install_artifact_snapshot" "$HARNESS_TXT"
contains "(guard) opt-in variable is honoured" "QA_EXPECTED_ARTIFACT" "$HARNESS_TXT"

# ── Arm 8: pre-fix control — the row's cited toolsd-only check, verbatim ─────
cat > "$TMPD/prefix-check.sh" <<'PREFIX'
#!/usr/bin/env bash
# VERBATIM shape of the pre-QA-BUNKER-078 check the row cites; kept as a
# control so a revert to a literal-toolsd assertion re-fails this suite.
PREFIX_BIN="$1"; HOME_DIR="$2"
PREFIX=""
cell() { printf '%s\t%s\n' "$2" "$3" >> "$4"; }
if command -v toolsd >/dev/null 2>&1; then PREFIX="$(command -v toolsd)"
elif [ -x "${PREFIX_BIN}/bin/toolsd" ]; then PREFIX="${PREFIX_BIN}/bin/toolsd"
elif [ -x "$HOME_DIR/.local/bin/toolsd" ]; then PREFIX="$HOME_DIR/.local/bin/toolsd"
fi
if [ -z "$PREFIX" ]; then
  cell fresh-install FAIL "rc=1: install rc=0 but no artifact found (toolsd not in PATH, PREFIX/bin/toolsd missing, or zero-size)" "$3"
else
  cell fresh-install OK "install ok" "$3"
fi
PREFIX
chmod +x "$TMPD/prefix-check.sh"
SB="$(new_sandbox prefix)"; OUT="$SB/cell.tsv"
# PATH pinned to the sandbox: the host's own GOPATH/bin really does carry a
# toolsd, and the control must judge the check, not this box's toolchain.
PATH="$SB/bin:/usr/bin:/bin" HOME="$SB/home" \
  "$TMPD/prefix-check.sh" "$SB" "$SB/home" "$OUT" >"$SB/probe.out" 2>"$SB/probe.err"
check "(pre-fix control) the cited toolsd-only check FAILs the go-build shape" \
  FAIL "$(status_of "$OUT")"
contains "(pre-fix control) it fails for exactly the cited reason" \
  "no artifact found (toolsd not in PATH" "$(detail_of "$OUT")"

echo
# Any probe that died before grading leaves its stderr here — surface it rather
# than reporting a silent empty grade.
for e in "$TMPD"/*/probe.err; do
  [ -s "$e" ] && { echo "  [probe stderr] $e:"; sed -n '1,6p' "$e"; }
done
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
