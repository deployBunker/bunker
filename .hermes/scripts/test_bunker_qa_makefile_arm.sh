#!/bin/bash
# QA-BUNKER-82 — regression test: the makefile-repo native suite arm must
# prefer the repo's REAL test target over the checks-only `make verify`
# fallback. Extracts detect_cmds() from bunker-qa.sh (same extraction pattern
# as test_bunker_qa_pytestini_arm.sh) and probes fixture Makefiles:
#   test:           -> make test            (conventional)
#   check:          -> make check           (automake convention)
#   verify-roundtrip: -> make verify-roundtrip (h3's real cross-language suite)
#   verify: only    -> make verify          (QA-H3-14 backward compat)
# and pins the INSTALL arm invariance (build: -> make build, else make verify)
# so the fix cannot silently re-contract the fresh-install leg.
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "$0")" && pwd)/bunker-qa.sh}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1 -> $3"; pass=$((pass+1))
  else echo "  FAIL $1: expected $2 got $3"; fail=$((fail+1)); fi
}

TMPD=${TMPDIR:-/tmp}/qa_bunker_82.$$
mkdir -p "$TMPD"
trap 'rm -rf "$TMPD"' EXIT

# Extract the detect_cmds function (awk from 'detect_cmds() {' to the closing
# brace of the function — the standalone `}` that follows the
# 'detect_upgrade_inputs "$repo"' call site).
awk '
  /^detect_cmds\(\) \{/ {f=1}
  f {print}
  f && /detect_upgrade_inputs "\$repo"/ {grab=1}
  grab && /^\}$/ {exit}
' "$HARNESS" > "$TMPD/dc_raw.sh"
if ! grep -q '^detect_cmds() {' "$TMPD/dc_raw.sh" || ! grep -q '^}$' "$TMPD/dc_raw.sh"; then
  echo "FAIL: could not extract detect_cmds from $HARNESS"; exit 1
fi

# Guard drift checks (grep-verifiable acceptance):
# - the native arm must carry the make test/check/verify-roundtrip ladder
# - the checks-only `make verify` fallback must still exist (backward compat)
# - the install arm's build/verify ordering must be untouched
if grep -q 'elif grep -qE .\^check:.' "$TMPD/dc_raw.sh" \
   && grep -q 'make check' "$TMPD/dc_raw.sh"; then
  check "native arm probes check: target" ok ok
else
  check "native arm probes check: target" ok MISSING
fi
if grep -q 'verify-roundtrip' "$TMPD/dc_raw.sh"; then
  check "native arm probes verify-roundtrip: target" ok ok
else
  check "native arm probes verify-roundtrip: target" ok MISSING
fi
if grep -qE 'native_cmd="make verify"' "$TMPD/dc_raw.sh" \
   && grep -qE 'DETECT_INSTALL="make (build|verify)"' "$TMPD/dc_raw.sh"; then
  check "checks-only fallbacks preserved (native + install arms)" ok ok
else
  check "checks-only fallbacks preserved (native + install arms)" ok MISSING
fi

# Stub the tail helper detect_cmds calls so the extracted slice is
# self-contained (detect_upgrade_inputs sets upgrade-cell variables only).
printf 'detect_upgrade_inputs() { :; }\n' > "$TMPD/dc.sh"
cat "$TMPD/dc_raw.sh" >> "$TMPD/dc.sh"

# Probe wrapper: sources the extracted function, runs it on a fixture repo,
# and prints "INSTALL=<DETECT_INSTALL> NATIVE=<DETECT_NATIVE>" (written to a
# file, no inline bash -c).
cat > "$TMPD/probe.sh" <<'EOF'
#!/bin/bash
source "$1"
detect_cmds "$2" >/dev/null 2>&1
echo "INSTALL=${DETECT_INSTALL:-UNSET} NATIVE=${DETECT_NATIVE:-UNSET}"
EOF
chmod +x "$TMPD/probe.sh"

# mkrepo <name> <makefile-content>
mkrepo() {
  mkdir -p "$TMPD/$1"
  printf '%s' "$2" > "$TMPD/$1/Makefile"
}

# --- fixture 1: conventional test: target (also carries verify:) ---
mkrepo test_repo '.PHONY: test verify
test:
	@echo running tests
verify:
	@echo checks only
'
# --- fixture 2: automake-convention check: only ---
mkrepo check_repo '.PHONY: check
check:
	@echo running check suite
'
# --- fixture 3: h3 shape — verify + verify-roundtrip, NO build/test/check ---
mkrepo h3_repo '.PHONY: verify verify-roundtrip verify-all
verify:
	@echo docs and repo-consistency guards, zero tests
verify-roundtrip:
	@echo cross-language round-trip suite
verify-all: verify verify-roundtrip
'
# --- fixture 4: verify-only Makefile (QA-H3-14 backward compat) ---
mkrepo verify_only_repo '.PHONY: verify
verify:
	@echo checks only
'
# --- fixture 5: Makefile with no recognized suite target at all ---
mkrepo unrelated_repo '.PHONY: all docs
all:
	@echo build
docs:
	@echo docs
'
# --- fixture 6: install-arm invariance — build: present alongside test: ---
mkrepo build_repo '.PHONY: build test
build:
	@echo build artifact
test:
	@echo running tests
'

probe() { "$TMPD/probe.sh" "$TMPD/dc.sh" "$TMPD/$1"; }

# --- native arm: prefer the real suite target ---
check "test: wins over checks-only verify:" \
  "INSTALL=make verify NATIVE=make test" "$(probe test_repo)"
check "check: only -> make check" \
  "INSTALL=make verify NATIVE=make check" "$(probe check_repo)"
check "h3 shape (verify+verify-roundtrip) -> the real suite" \
  "INSTALL=make verify NATIVE=make verify-roundtrip" "$(probe h3_repo)"
check "verify-only Makefile keeps make verify (backward compat)" \
  "INSTALL=make verify NATIVE=make verify" "$(probe verify_only_repo)"
check "no recognized target keeps make verify default" \
  "INSTALL=make verify NATIVE=make verify" "$(probe unrelated_repo)"
check "install arm unchanged: build: -> make build even with test:" \
  "INSTALL=make build NATIVE=make test" "$(probe build_repo)"

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
