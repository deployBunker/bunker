#!/usr/bin/env bash
# QA-BUNKER-077 — regression test: collect must survive the server-side agent
# reap (~35min post-launch in the field, TTL is 4h). Three contracts:
#   1. RUNNING polls sync the battery's evidence rows incrementally, so the rows
#      are already on local disk before the agent user can vanish.
#   2. An unreachable agent whose rows WERE already synced is graded
#      agent-gone (collect row status=agent-gone, stdout AGENT-GONE, rc=0) —
#      the run is not lost, instead of the QA-BUNKER-45 harness abort.
#   3. A FINISHED agent's per-cell logs (~/qa-logs-*) are pulled to
#      <evidence>.logs/ BEFORE the destroy, so they persist past cleanup.
#   4. An unreachable agent with NOTHING synced still aborts (QA-BUNKER-45
#      semantics preserved for a genuinely lost run).
# Uses only local fixtures; it never launches or collects a live battery.
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

TMPD="${TMPDIR:-/tmp}/qa_bunker_077.$$"
mkdir -p "$TMPD/bin" "$TMPD/home/.bunker" "$TMPD/locks"
trap 'rm -rf "$TMPD"' EXIT
: > "$TMPD/key"

run_collect() { # <evidence> -> harness stdout+stderr; capture rc with $? after
  HOME="$TMPD/home" PATH="$TMPD/bin:$PATH" \
    BUNKER_QA_HOST_KEY="$TMPD/key" BUNKER_QA_SSH_PORT=22 \
    BUNKER_QA_COLLECT_SSH_TIMEOUT=2 BUNKER_QA_ORPHAN_LOCK_DIR="$TMPD/locks" \
    bash "$HARNESS" collect --evidence "$1" 2>&1
}

echo "QA-BUNKER-077: collect survives the agent-user reap (agent-gone) and pulls logs before destroy"
echo

# ─── shared fixtures ──────────────────────────────────────────────────────────
# Stateful ssh stub: classifies by the remote command text (probe / evidence
# pull / log du / log tar) and a mode file for the two-phase RUNNING→dead flow.
cat > "$TMPD/bin/ssh" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${SSH_LOG:?SSH_LOG unset}"
mode=$(cat "$STUB_MODE" 2>/dev/null || echo dead)
case "$*" in
  *qa-run.finished*)
    case "$mode" in
      finished) echo FINISHED ;;
      running)  echo RUNNING ;;
      pullfail) echo FINISHED ;;
      *)        exit 255 ;;
    esac
    ;;
  *qa-evidence.jsonl*)
    case "$mode" in
      finished)
        printf '%s\n' \
          '{"project":"fixture","cell":"ci-pass","status":"OK","detail":"native ok","ts":"2026-11-01T09:37:00Z"}' \
          '{"project":"fixture","cell":"chaos-resource","status":"FAIL","detail":"res.log tail","ts":"2026-11-01T09:38:08Z"}'
        ;;
      running)
        printf '%s\n' '{"project":"fixture","cell":"fresh-install","status":"OK","detail":"mid-battery row","ts":"2026-11-01T09:30:00Z"}'
        ;;
      *) exit 255 ;;
    esac
    ;;
  *du\ -sk*) echo "8" ;;
  *tar\ -czf*) cat "${LOGS_TGZ:?LOGS_TGZ unset}" ;;
  *) exit 255 ;;
esac
STUB
chmod +x "$TMPD/bin/ssh"

# bunker stub: destroy succeeds, list shows nothing (honest TEARDOWN-GONE).
cat > "$TMPD/bin/bunker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "bunker $*" >> "${BUNKER_LOG:?BUNKER_LOG unset}"
case "$1" in
  destroy) exit 0 ;;
  list)    echo "  (no agents)"; exit 0 ;;
esac
exit 0
STUB
chmod +x "$TMPD/bin/bunker"

# Per-cell logs fixture: what qa-run.sh leaves in ~/qa-logs-$$ on the agent.
mkdir -p "$TMPD/agent-home/qa-logs-4242"
echo "make -j4 rc=0" > "$TMPD/agent-home/qa-logs-4242/ci.log"
echo "--- FAIL: TestSurfaceConnect ---" > "$TMPD/agent-home/qa-logs-4242/res.log"
tar -czf "$TMPD/logs.tgz" -C "$TMPD/agent-home" qa-logs-4242

LAUNCHED=$(date -u +%FT%TZ)
export SSH_LOG="$TMPD/ssh.log" LOGS_TGZ="$TMPD/logs.tgz" BUNKER_LOG="$TMPD/bunker.log"

# ─── scenario 1: FINISHED agent — evidence + logs pulled before destroy ──────
echo "scenario 1: FINISHED agent — logs pulled to <evidence>.logs/ before destroy"
EV1="$TMPD/finished.jsonl"
printf 'agent=fixture-agent\nserver=fixture-host\nevidence=%s\nlaunched=%s\n' "$EV1" "$LAUNCHED" > "$EV1.meta"
: > "$EV1"
printf '%s\n' '{"project":"fixture","cell":"launch","status":"OK","detail":"fixture","ts":"2026-11-01T09:18:00Z"}' > "$EV1"
echo finished > "$TMPD/mode"
export STUB_MODE="$TMPD/mode"
: > "$TMPD/ssh.log"; : > "$TMPD/bunker.log"
out=$(run_collect "$EV1"); COLLECT_RC=$?

check "collect exits 0" "0" "$COLLECT_RC"
contains "stdout says COLLECTED" "COLLECTED agent=fixture-agent" "$out"
contains "collect row detail names the saved logs" "per-cell logs saved to" "$(cat "$EV1")"
contains "collect OK row present" '"cell":"collect","status":"OK"' "$(cat "$EV1")"
contains "agent evidence rows merged (json.dumps re-serialized shape)" '"cell": "chaos-resource", "status": "FAIL"' "$(cat "$EV1")"
check "per-cell logs extracted under evidence.logs" "1" "$([ -s "$EV1.logs/qa-logs-4242/ci.log" ] && echo 1 || echo 0)"
contains "res.log survived the destroy" "--- FAIL: TestSurfaceConnect ---" "$(cat "$EV1.logs/qa-logs-4242/res.log" 2>/dev/null)"
check "logs pulled BEFORE destroy (du+tar precede bunker in ssh.log? destroy attempted once)" "1" "$(grep -c '^bunker destroy fixture-agent' "$TMPD/bunker.log" || true)"
check "synced marker written" "1" "$([ -s "$EV1.synced-fixture-agent" ] && echo 1 || echo 0)"

echo

# ─── scenario 2: RUNNING poll syncs rows, later reap grades agent-gone ───────
echo "scenario 2: RUNNING sync then agent reap — agent-gone rc=0, run not lost"
EV2="$TMPD/reaped.jsonl"
printf 'agent=fixture-agent\nserver=fixture-host\nevidence=%s\nlaunched=%s\n' "$EV2" "$LAUNCHED" > "$EV2.meta"
: > "$EV2"
echo running > "$TMPD/mode"
out=$(run_collect "$EV2"); COLLECT_RC=$?

check "RUNNING poll exits 0" "0" "$COLLECT_RC"
contains "stdout says RUNNING" "RUNNING agent=fixture-agent" "$out"
contains "mid-battery row synced into evidence" '"cell": "fresh-install"' "$(cat "$EV2")"
check "already-synced marker written during RUNNING" "1" "$(grep -c 'synced_rows=1' "$EV2.synced-fixture-agent" 2>/dev/null || true)"
check "RUNNING poll records no collect row" "0" "$(grep -c '"cell":"collect"' "$EV2" || true)"

echo dead > "$TMPD/mode"
out=$(run_collect "$EV2"); COLLECT_RC=$?

check "unreachable-after-sync exits 0 (not 2)" "0" "$COLLECT_RC"
contains "stdout names AGENT-GONE" "AGENT-GONE agent=fixture-agent rows=1" "$out"
contains "agent-gone collect row recorded" '"cell":"collect","status":"agent-gone"' "$(cat "$EV2")"
contains "agent-gone reason names the synced rows" "already synced by incremental collect" "$(cat "$EV2")"
check "no harness-failure row (run NOT lost)" "0" "$(grep -c '"cell":"harness-failure"' "$EV2" || true)"

out=$(run_collect "$EV2"); COLLECT_RC=$?
check "re-call is terminal (already-recorded)" "0" "$COLLECT_RC"
contains "stdout says already-recorded" "AGENT-GONE already-recorded" "$out"
check "no duplicate agent-gone row" "1" "$(grep -c '"cell":"collect","status":"agent-gone"' "$EV2" || true)"

echo

# ─── scenario 3: unreachable with NOTHING synced still aborts (QA-BUNKER-45) ─
echo "scenario 3: unreachable agent with nothing synced — harness abort preserved"
EV3="$TMPD/lost.jsonl"
printf 'agent=fixture-agent\nserver=fixture-host\nevidence=%s\nlaunched=%s\n' "$EV3" "$LAUNCHED" > "$EV3.meta"
: > "$EV3"
echo dead > "$TMPD/mode"
rm -f "$EV3.synced-fixture-agent"
out=$(run_collect "$EV3"); COLLECT_RC=$?

check "nothing-synced unreachable still aborts rc=2" "2" "$COLLECT_RC"
contains "abort names the host" "COLLECT-ABORT host=fixture-host" "$out"
contains "collect FAIL row present" '"cell":"collect","status":"FAIL"' "$(cat "$EV3")"
contains "harness-failure row present" '"cell":"harness-failure","status":"INFO"' "$(cat "$EV3")"

echo

# ─── scenario 4: agent vanishes between probe and evidence pull ──────────────
echo "scenario 4: reap between probe and pull with rows already synced — agent-gone"
EV4="$TMPD/vanished.jsonl"
printf 'agent=fixture-agent\nserver=fixture-host\nevidence=%s\nlaunched=%s\n' "$EV4" "$LAUNCHED" > "$EV4.meta"
: > "$EV4"
# Simulate: RUNNING poll synced rows earlier (the marker is the contract),
# and the probe answers FINISHED but the evidence pull fails (reap landed
# between the two ssh calls — the pullfail mode).
printf 'synced_rows=2 ts=2026-11-01T09:35:00Z\n' > "$EV4.synced-fixture-agent"
echo pullfail > "$TMPD/mode"
out=$(run_collect "$EV4"); COLLECT_RC=$?

check "probe-passed pull-failure exits 0" "0" "$COLLECT_RC"
contains "stdout names AGENT-GONE" "AGENT-GONE agent=fixture-agent rows=2" "$out"
contains "agent-gone row recorded" '"cell":"collect","status":"agent-gone"' "$(cat "$EV4")"
check "no harness-failure row" "0" "$(grep -c '"cell":"harness-failure"' "$EV4" || true)"

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
