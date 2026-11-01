#!/usr/bin/env bash
# Regression for QA-BUNKER-PERF-1: a stalled spawn or agent probe must not
# consume the caller's 180s launch budget without a named diagnosis.
set -uo pipefail
HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bunker-qa.sh}"
TMPD="${TMPDIR:-/tmp}/qa_bunker_perf1.$$"
mkdir -p "$TMPD/bin" "$TMPD/home/.bunker/keys" "$TMPD/repo"
trap 'rm -rf "$TMPD"' EXIT
sed -n '/^qa_slice_cpus() {/,/^}$/p' "$HARNESS" > "$TMPD/fns.sh"
sed -n '/^qa_slice_args() {/,/^}$/p' "$HARNESS" >> "$TMPD/fns.sh"
sed -n '/^spawn_agent() {/,/^}$/p' "$HARNESS" >> "$TMPD/fns.sh"

cat > "$TMPD/bin/bunker" <<'STUB'
#!/usr/bin/env bash
sleep 3
STUB
chmod +x "$TMPD/bin/bunker"

# A stalled bunker spawn is bounded and names each retry.
out=$(HOME="$TMPD/home" PATH="$TMPD/bin:$PATH" BUNKER_QA_SPAWN_TIMEOUT=1 \
  BUNKER_QA_RETRY_SLEEP=0 EVIDENCE="$TMPD/evidence.jsonl" \
  bash -c "source '$TMPD/fns.sh'; repo='$TMPD/repo'; SERVER=fixture; TTL=4h; BUNKER_QA_HOST_KEY=unused; spawn_agent" 2>&1)
rc=$?
if [ "$rc" -eq 0 ] || ! printf '%s\n' "$out" | grep -q 'spawn attempt 1 timed out after 1s on fixture' \
  || ! printf '%s\n' "$out" | grep -q 'spawn attempt 3 timed out after 1s on fixture'; then
  echo "FAIL: spawn timeout was not bounded and diagnosed: $out"
  exit 1
fi

# Return a known agent id, then stall the SSH reachability probe.
cat > "$TMPD/bin/bunker" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  spawn) echo 'Agent created: abc12345' ;;
esac
exit 0
STUB
cat > "$TMPD/bin/ssh" <<'STUB'
#!/usr/bin/env bash
sleep 3
STUB
chmod +x "$TMPD/bin/bunker" "$TMPD/bin/ssh"

# A stalled agent reachability probe is bounded, named, and cleaned up.
out=$(HOME="$TMPD/home" PATH="$TMPD/bin:$PATH" BUNKER_QA_PROBE_TIMEOUT=1 \
  BUNKER_QA_RETRY_SLEEP=0 EVIDENCE="$TMPD/evidence.jsonl" \
  bash -c "source '$TMPD/fns.sh'; qa_write_agent_lock() { return 0; }; qa_remove_agent_lock() { return 0; }; agent_ssh_port() { echo 22; }; repo='$TMPD/repo'; SERVER=fixture; TTL=4h; BUNKER_QA_HOST_KEY=unused; spawn_agent" 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then
  echo "FAIL: stalled agent probe unexpectedly succeeded"
  exit 1
fi
if ! printf '%s\n' "$out" | grep -q 'spawn attempt 1 produced agent abc12345 but ssh reachability probe failed (rc=124; timeout=1s)'; then
  echo "FAIL: probe timeout diagnosis missing: $out"
  exit 1
fi
if ! printf '%s\n' "$out" | grep -q 'spawn attempt 3 produced agent abc12345 but ssh reachability probe failed (rc=124; timeout=1s)'; then
  echo "FAIL: probe timeout retries were not bounded and diagnosed: $out"
  exit 1
fi
# The production default must stay well below the 180s outer call ceiling.
if ! grep -q 'BUNKER_QA_PROBE_TIMEOUT:-30' "$HARNESS"; then
  echo "FAIL: agent SSH probe default is not below the outer 180s ceiling"
  exit 1
fi
echo "PASS: stalled agent SSH probes are bounded and named; default probe budget is 30s"
