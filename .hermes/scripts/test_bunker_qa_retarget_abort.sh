#!/usr/bin/env bash
# QA-BUNKER-45 — regression test: collect must abort after one failed host
# probe, attribute the harness failure to that host, and record timing. Uses
# only local fixtures; it never launches or collects a live battery.
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bunker-qa.sh}"
pass=0
fail=0
check() {
  if [ "$2" = "$3" ]; then
    echo "  ok   $1 -> $3"
    pass=$((pass + 1))
  else
    echo "  FAIL $1: expected [$2] got [$3]"
    fail=$((fail + 1))
  fi
}
contains() {
  case "$3" in
    *"$2"*) echo "  ok   $1"; pass=$((pass + 1)) ;;
    *) echo "  FAIL $1: missing [$2]"; fail=$((fail + 1)) ;;
  esac
}

TMPD="${TMPDIR:-/tmp}/qa_bunker_45.$$"
mkdir -p "$TMPD/bin" "$TMPD/home/.bunker" "$TMPD/home/.ssh"
trap 'rm -rf "$TMPD"' EXIT

# The ssh fixture records invocations and fails every probe. The harness's
# bounded timeout still invokes it exactly once for this collect attempt.
cat > "$TMPD/bin/ssh" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${SSH_LOG:?SSH_LOG unset}"
exit 255
STUB
chmod +x "$TMPD/bin/ssh"
: > "$TMPD/key"
printf 'agent=fixture-agent\nserver=fixture-host\nevidence=%s\nlaunched=2026-10-30T00:00:00Z\nkeep=1\n' "$TMPD/evidence.jsonl" > "$TMPD/evidence.jsonl.meta"
: > "$TMPD/evidence.jsonl"
: > "$TMPD/ssh.log"

echo "QA-BUNKER-45: one failed collect aborts the harness with timed host evidence"
echo

set +e
out=$(HOME="$TMPD/home" PATH="$TMPD/bin:$PATH" SSH_LOG="$TMPD/ssh.log" \
  BUNKER_QA_HOST_KEY="$TMPD/key" BUNKER_QA_SSH_PORT=22 \
  BUNKER_QA_COLLECT_SSH_TIMEOUT=1 \
  bash "$HARNESS" collect --evidence "$TMPD/evidence.jsonl" 2>&1)
rc=$?
set -e
rows=$(cat "$TMPD/evidence.jsonl")

check "collect exits with explicit abort status" "2" "$rc"
contains "abort output names the host" "COLLECT-ABORT host=fixture-host" "$out"
check "one failed collect makes one ssh probe" "1" "$(wc -l < "$TMPD/ssh.log" | tr -d ' ')"
check "collect failure row exists" "1" "$(grep -c '"cell":"collect","status":"FAIL"' "$TMPD/evidence.jsonl")"
check "harness failure row exists" "1" "$(grep -c '"cell":"harness-failure","status":"INFO"' "$TMPD/evidence.jsonl")"
contains "harness failure is attributed to host" '"attributed_host":"fixture-host"' "$rows"
contains "collect row records attempt start" '"attempt_started":"' "$rows"
contains "collect row records attempt finish" '"attempt_finished":"' "$rows"
contains "collect row records elapsed milliseconds" '"duration_ms":' "$rows"
contains "harness failure says abort after one attempt" "harness abort after one failed collect" "$rows"

# Source-level acceptance: the real path contains several explicit abort/
# retarget mentions and retains the preflight bypass guard.
count=$(grep -cE 'retarget|abort' "$HARNESS")
check "acceptance abort/retarget count" "1" "$([ "$count" -ge 6 ] && echo 1 || echo 0)"
if grep -q 'BUNKER_QA_SKIP_PREFLIGHT' "$HARNESS"; then
  echo "  ok   acceptance preflight bypass guard"
  pass=$((pass + 1))
else
  echo "  FAIL acceptance preflight bypass guard"
  fail=$((fail + 1))
fi

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
