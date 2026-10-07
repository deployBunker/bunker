#!/usr/bin/env bash
# bunker-qa.sh — QA FOREMAN pipeline: FULL fresh-system CI pass on a JIT bunker agent.
# The stand-in PM's sibling: where the PM finds work/gaps on boards, this finds
# BRITTLENESS by running the project on a CLEAN machine (Bane doctrine 08-24:
# "not just we have valid code but does that code give us things that are brittle").
#
# Cells: fresh-install · ci-pass (act --bind) · docker-deploy · ui-probe · upgrade
#        chaos-disconnect · chaos-shutdown · chaos-corruption · chaos-resource · chaos-errorpath
#
# Architecture: ALL remote commands run from a generated script (qa-run.sh) shipped
# base64 to the agent — never nested shell strings through ssh layers (quoting hell
# observed 2026-08-27: PIPESTATUS eaten by inner pipes, $(..) expanded locally,
# syntax errors misread as "HANGS").
#
# Usage:
#   bunker-qa.sh run     <repo-dir> [--server X] [--ttl 4h] [--keep]
#                                             — SYNCHRONOUS compat mode: waits for
#                                               the whole battery, then pulls
#   bunker-qa.sh launch  <repo-dir> [--keep]  — PHASE A: preflights + spawn + sync +
#                                               START the battery DETACHED on the
#                                               agent (~60-120s). Writes $EVIDENCE.meta
#                                               and one launch cell. Waits on NO cells.
#   bunker-qa.sh collect --evidence <path>    — PHASE B: poll → pull → destroy.
#                                               Idempotent; prints RUNNING (rc=0, no
#                                               destroy) while the battery is alive.
#   bunker-qa.sh status
#
# WHY THE SPLIT (QA-HERMES-CANOPY-8, 2026-09-15): a full fresh-system battery
# (spawn, sync, toolchain bootstrap, act CI, native suite, compose, 4 chaos
# cells) takes 7-15 min, but a single Hermes terminal tool call is killed at
# ~420s. The synchronous form therefore killed the battery mid-run, the
# cleanup traps died with the call, and the resulting 0-byte evidence file
# reads as "no cells" = UNVERIFIED (143 of 751 /tmp/bunker-qa-evidence-*.jsonl
# on this box are 0 bytes — a systemic class, not a one-off). Every launch/
# collect tool call is designed to finish far under that cap.
# NOTE: NO `set -e` on purpose — every cell records its own status via RC echoes.
set -uo pipefail

# Default server: bunker-las-03 was REMOVED from ~/.bunker/config.yaml
# (stale default produced 4 consecutive days of P1 "spawn failed" findings,
# 27+ re-files — QA-DAGGER-GUARD 2026-09-10), then re-registered 2026-09-10
# and live-probe-verified 2026-09-22 (spawn/exec/destroy green, tick 547
# QA-HERMES-CANOPY-19). bunker-las-02 remains the verified default.
# Override with BUNKER_QA_SERVER.
SERVER="${BUNKER_QA_SERVER:-bunker-mvp}"
# QA-keyfix 2026-09-30: ~/.ssh/id_ed25519_bunker was clobbered by an ELF binary
# (2026-09-28), so any ssh carrying it as the ONLY IdentitiesOnly identity fails
# auth and every preflight graded its server "unreachable". BUNKER_QA_HOST_KEY
# (full path) injects a working host key ahead of every ssh the script makes;
# a replacement key for bunker-mvp lives at ~/.ssh/id_ed25519_bunker_mvp.
: "${BUNKER_QA_HOST_KEY:=$HOME/.ssh/id_ed25519_bunker_mvp}"
TTL="${BUNKER_QA_TTL:-4h}"
ACT_URL="https://github.com/nektos/act/releases/latest/download/act_Linux_x86_64.tar.gz"
# QA-H3-14 (2026-09-21): userspace GNU make bootstrap for make-less JIT agents
# (agentuid install into ~/tools/make; build.sh needs no preexisting make).
MAKE_URL="https://ftp.gnu.org/gnu/make/make-4.4.1.tar.gz"
EVIDENCE="${BUNKER_QA_EVIDENCE:-/tmp/bunker-qa-evidence.jsonl}"
# TR-085: the shared default evidence path is how one run's meta gets read by the
# next run's collect (dogfood 2026-09-20 probed a 2-day-old agent). Callers that
# accept the default get a per-run suffix unless they explicitly opt out —
# set BUNKER_QA_ALLOW_SHARED_EVIDENCE=1 to keep the historical path.
if [ -z "${BUNKER_QA_EVIDENCE:-}" ] && [ "${BUNKER_QA_ALLOW_SHARED_EVIDENCE:-0}" != "1" ]; then
  EVIDENCE="/tmp/bunker-qa-evidence-$(date -u +%Y%m%dT%H%M%SZ)-$$.jsonl"
fi

log()  { echo "→ $*"; }

# ─── qa_destroy_agent <id> <evidence-file|''> (QA-BUNKER-19 b+c, 2026-09-21) ───
# Destroy with SURFACING + post-destroy absence verify. Replaced the bare
# `bunker destroy … || true` swallow in the cleanup/collect paths, which
# defeated the destroy-failure escalation contract (QA-BUNKER-19: a failed
# cleanup destroy left agent 230abb5b running with no trace in any log).
# QA-BUNKER-39 (2026-09-30) hardened the whole path: on a loaded server the
# CLI destroy's short internal deadline expires (deadline_exceeded x3 on
# bunker-las-02, 2026-09-29 11:53-12:02Z), the temp agent survives, and its
# slot stays consumed until the ~4h TTL reaps it — one leaked slot is one
# dead lane launch ("capacity exhausted (8/8)"). Contract now:
#   * RETRY: up to 3 destroy attempts with backoff (10s after attempt 1,
#     30s after attempt 2; BUNKER_QA_DESTROY_BACKOFF overrides for tests).
#     Every failed attempt is logged on stderr.
#   * RE-CHECK: after the final attempt, one bounded `bunker list` probe is
#     the HONEST verdict — not the destroy rc. An id still listed after a
#     failed run of attempts (or with an unverifiable list) is a REAL leak:
#     stderr + one FAIL evidence row (cell destroy-verify) naming the agent
#     id, server, the ~4h TTL reap hint and the manual recovery command.
#     The destroy landing LATE (failed attempts, id gone from the list) is
#     teardown RECOVERED — logged, no failure row.
#   * STDOUT CONTRACT: exactly one final stdout line — "TEARDOWN-GONE" (clean
#     destroy), "TEARDOWN-RECOVERED" (late-landing destroy) or
#     "TEARDOWN-LEAK" (still listed / unverifiable after failed attempts) —
#     so callers (collect) can state a summary that matches reality.
# The list probe stays best-effort on the clean path: a failed/unparseable
# list after a clean destroy is silence, never a failure.
# Never fails the caller (returns 0 unconditionally) — a cleanup/verify probe
# must not become the run's verdict; the destroy rc is surfaced, not propagated.
# spawn_agent()'s pre-retry destroy stays inline (stderr only): no evidence file
# is open there yet, and the retry loop needs no extra list round-trip.
qa_destroy_agent() {
  local id="$1" evfile="$2" drc=0 lst="" row="" attempt list_rc=0 destroy_failed=0
  for attempt in 1 2 3; do
    drc=0
    bunker destroy "$id" --server "$SERVER" >/dev/null 2>&1 || drc=$?
    if [ "$drc" -eq 0 ]; then break; fi
    destroy_failed=1
    echo "destroy FAILED: agent $id rc=$drc server=$SERVER (attempt $attempt/3)" >&2
    if [ "$attempt" -lt 3 ]; then
      local backoff=$(( attempt == 1 ? 10 : 30 ))
      sleep "${BUNKER_QA_DESTROY_BACKOFF:-$backoff}"
    fi
  done
  # Honest verdict: re-check the server list whatever the destroy rc claimed —
  # a destroy may land late, after its client already reported failure.
  lst=$(timeout 10 bunker list --server "$SERVER" 2>/dev/null)
  list_rc=$?
  if [ "$list_rc" -eq 0 ] && printf '%s' "$lst" | grep -qF -- "$id"; then
    echo "STILL PRESENT AFTER DESTROY: agent $id on $SERVER" >&2
    row="STILL PRESENT AFTER DESTROY: agent=$id server=$SERVER — destroy still failing after 3 attempts (last rc=$drc) — temp agent holds a slot until the ~4h TTL reaps it; manual recovery: bunker destroy $id --server $SERVER"
  elif [ "$destroy_failed" -eq 1 ] && [ "$list_rc" -eq 0 ]; then
    echo "destroy reported failure (last rc=$drc) but agent=$id is GONE from the $SERVER list — teardown recovered (destroy landed late)" >&2
  elif [ "$destroy_failed" -eq 1 ]; then
    echo "destroy FAILED after 3 attempts (last rc=$drc) and the server list is unverifiable (list rc=$list_rc) — treating agent $id on $SERVER as LEAKED" >&2
    row="bunker destroy FAILED after 3 attempts (last rc=$drc); server list unverifiable (rc=$list_rc) — agent=$id server=$SERVER may hold a slot until the ~4h TTL reaps it; manual recovery: bunker destroy $id --server $SERVER"
  fi
  if [ -n "$row" ] && [ -n "$evfile" ]; then
    printf '{"cell":"destroy-verify","status":"FAIL","detail":"%s","ts":"%s"}\n' \
      "$(printf '%s' "$row" | tr '\n' ' ' | cut -c1-300)" "$(date -u +%FT%TZ)" >> "$evfile"
  fi
  if [ -n "$row" ]; then echo "TEARDOWN-LEAK"
  elif [ "$destroy_failed" -eq 1 ]; then echo "TEARDOWN-RECOVERED"
  else echo "TEARDOWN-GONE"; fi
  return 0
}

spawn_agent() {
  # QA-HERMES-CANOPY-16 (2026-09-21): an agent that spawns server-side but is
  # unreachable for the launch is an ORPHAN (holds a slot until TTL, no
  # evidence points at it; field evidence agent cefd6920, bunker-las-02,
  # 2026-09-17). Contract: capture the id at spawn time; if the attempt cannot
  # be handed off (spawn produced no usable id, or the ssh reachability probe
  # fails), DESTROY that id before retrying or failing.
  # stdout contract unchanged: success prints ONLY the agent id, rc 0;
  # total failure prints nothing on stdout, rc 1.
  local out id drc
  for attempt in 1 2 3; do
    out=$(bunker spawn --server "$SERVER" --ttl "$TTL" 2>&1)
    id=$(echo "$out" | grep -oP 'keys/\K[a-f0-9]+' | head -1)
    if [ -z "$id" ]; then
      [ $attempt -lt 3 ] && { echo "  spawn attempt $attempt failed: $(echo "$out" | tail -1)" >&2; sleep "${BUNKER_QA_RETRY_SLEEP:-5}"; }
      continue
    fi
    # Reachability gate: the agent must answer ssh BEFORE it is handed off,
    # so an unreachable spawn never leaks to the caller as a "success".
    if timeout "${BUNKER_QA_PROBE_TIMEOUT:-180}" ssh -p "${BUNKER_QA_SSH_PORT:-2223}" -i "$BUNKER_QA_HOST_KEY" -i "$HOME/.bunker/keys/$id" -o IdentitiesOnly=yes -o ConnectTimeout=20 -o StrictHostKeyChecking=no -o BatchMode=yes "bunker-$id@$SERVER" true 2>/dev/null; then
      echo "$id"
      return 0
    fi
    echo "spawn attempt $attempt produced agent $id but was unusable — destroyed before retry" >&2
    # QA-BUNKER-19 b+c: surfaced, not swallowed — stderr only (no evidence file
    # is open inside spawn_agent, and the retry loop needs no extra list
    # round-trip); rc captured so `set -o pipefail` alone can never see it as
    # handled silence.
    drc=0
    bunker destroy "$id" --server "$SERVER" >/dev/null 2>&1 || drc=$?
    [ "$drc" -ne 0 ] && echo "destroy FAILED: agent $id rc=$drc server=$SERVER (pre-retry cleanup)" >&2
    [ $attempt -lt 3 ] && sleep "${BUNKER_QA_RETRY_SLEEP:-5}"
  done
  echo "$out" | tail -3 >&2
  return 1
}
agent_ssh() { timeout "${BUNKER_QA_SSH_TIMEOUT:-1800}" ssh -p "${BUNKER_QA_SSH_PORT:-2223}" -i "$BUNKER_QA_HOST_KEY" -i "$key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o BatchMode=yes "bunker-$agent@$SERVER" "$@"; }

status() {
  echo "BUNKER QA — $(date -u +%Y-%m-%dT%H:%M:%SZ) — server $SERVER"
  timeout 30 ssh -i "$BUNKER_QA_HOST_KEY" ${BUNKER_QA_HOST_KEY:+-o IdentitiesOnly=yes} -o ConnectTimeout=20 -o StrictHostKeyChecking=no -o BatchMode=yes "$SERVER" \
    'echo "bunkerd=$(systemctl is-active bunkerd) docker=$(command -v docker >/dev/null && echo yes || echo no) act=$(command -v act >/dev/null && echo yes || echo no) uidmap=$(command -v newuidmap >/dev/null && echo yes || echo no)"' 2>/dev/null \
    || echo "  $SERVER: UNREACHABLE"
}

# ─── bunker capacity probe (QA-OFF-BY-ONE-9, 2026-09-15) ───
# Once the pool is full, `bunker spawn` fails DETERMINISTICALLY, not
# transiently: either
#   "port range allocation: no free port ranges available"  (port-pool class)
#   "capacity full: C/C agents"                             (agent-ceiling class)
# so spawn_agent()'s 3x retry loop buys nothing but latency and turns one
# actionable finding into a retry ladder (triage reference:
# skills/verification/qa-foreman-interpretation/references/bunker-server-portpool-exhaustion.md).
#
# Source of truth is the CLI — bunkerd speaks connect/gRPC, raw HTTP probes
# against /api/v1/* 404 without proving anything:
#   bunker status --server <srv>            →  "  Agents:   N/MAX"
#   bunker list --server <srv> --status all →  "Total: N agents"
#
# parse_bunker_capacity <srv> <status-output> → "USED MAX" | "" (unparseable).
# Pure (text in, text out) so the parse is provable without a live host.
parse_bunker_capacity() {
  local srv="$1" out="$2" line used max
  [ -n "$out" ] || return 0
  # `status --all` prints one "── <name> ──" section per host — scope to ours
  # first, then fall back to a whole-output scan (status --server = one section).
  line=$(printf '%s\n' "$out" | awk -v s="$srv" '
    index($0, "──") && index($0, s) { inside=1; next }
    inside && index($0, "──") { inside=0 }
    inside && /Agents:[[:space:]]*[0-9]+[[:space:]]*\/[[:space:]]*[0-9]+/ { print; exit }')
  if [ -z "$line" ]; then
    # No section matched. Only trust a whole-output scan when there is exactly
    # ONE Agents line (status --server = a single section); with several
    # sections and none naming $srv, another host's numbers are not ours.
    if [ "$(printf '%s\n' "$out" | grep -cE 'Agents:[[:space:]]*[0-9]+[[:space:]]*\/[[:space:]]*[0-9]+')" = "1" ]; then
      line=$(printf '%s\n' "$out" | grep -m1 -E 'Agents:[[:space:]]*[0-9]+[[:space:]]*\/[[:space:]]*[0-9]+' || true)
    fi
  fi
  [ -n "$line" ] || return 0
  used=$(printf '%s' "$line" | sed -E 's/^[^0-9]*([0-9]+).*/\1/')
  max=$(printf '%s' "$line" | sed -E 's#.*/[[:space:]]*([0-9]+).*#\1#')
  case "$used" in ''|*[!0-9]*) return 0 ;; esac
  case "$max"  in ''|*[!0-9]*) return 0 ;; esac
  printf '%s %s\n' "$used" "$max"
}

# bunker_capacity <srv> → "USED MAX" | "USED -" (ceiling unreadable) | "" (unknown)
# At most TWO cheap bunkerd probes: `bunker status` first, `bunker list` only
# when status yielded no usable "Agents: N/MAX" pair. Callers must treat a
# missing ceiling as UNKNOWN, never as full — blocking the battery on a probe
# hiccup would wedge the QA lane for every project on that host.
bunker_capacity() {
  local srv="$1" out cap used max
  out=$(timeout "${BUNKER_QA_CAPACITY_TIMEOUT:-20}" bunker status --server "$srv" 2>/dev/null) || out=""
  cap=$(parse_bunker_capacity "$srv" "$out")
  if [ -n "$cap" ]; then printf '%s\n' "$cap"; return 0; fi
  # Fallback (used count only): `Total: N agents`, with the ceiling taken from
  # any "N/M" pair still present in the status output.
  used=$(printf '%s\n' "$(timeout "${BUNKER_QA_CAPACITY_TIMEOUT:-20}" bunker list --server "$srv" --status all 2>/dev/null)" \
           | sed -nE 's/^Total:[[:space:]]*([0-9]+)[[:space:]]*agents.*/\1/p' | head -1)
  max=$(printf '%s\n' "$out" | sed -nE 's#.*[^0-9]([0-9]+)[[:space:]]*/[[:space:]]*([0-9]+).*#\2#p' | head -1)
  case "$used" in ''|*[!0-9]*) return 0 ;; esac
  case "$max" in ''|*[!0-9]*) max="-" ;; esac
  printf '%s %s\n' "$used" "$max"
}

# ─── write_fail_if_empty — never leave a 0-byte evidence file ───
# Now top-level (2026-09-15) because run() AND launch() share it. Rationale
# unchanged from 2026-09-07 (QA-DEXDAT-CORE-1): a tool-level SIGTERM (Hermes
# terminal cap) or a sync-fail exit kills the battery mid-run leaving a 0-byte
# evidence file — parse_cells then sees cells:[] and the DAG reports
# UNVERIFIED silently, indistinguishable from "nothing to check".
# Idempotent: TERM + EXIT traps firing together produce exactly one line.
write_fail_if_empty() {
  local reason="${1:-battery terminated before any cell recorded}"
  if ! grep -q '^{' "$EVIDENCE" 2>/dev/null; then
    printf '{"cell":"run_battery","status":"FAIL","detail":"%s","ts":"%s"}\n' \
      "$reason" "$(date -u +%FT%TZ)" >> "$EVIDENCE"
  fi
}

# ─── shared preflights (extracted verbatim from run(), 2026-09-15) ───
# launch() needs the SAME deterministic-failure gates as run(), so they live
# here and both call preflights(). Contract (unchanged): record exactly ONE
# actionable FAIL row in $EVIDENCE and exit rc=2 — a removed/stale server, an
# unreachable host, or a full pool is a STATIC error, not transient congestion,
# so spawn_agent's 3x retry loop buys nothing but latency and turns one
# actionable finding into a retry ladder.
#   config  : BUNKER_QA_SERVER must exist in ~/.bunker/config.yaml (QA-DAGGER-GUARD 09-10)
#   ssh     : the host must answer over tailscale before any spawn
#   capacity: a FULL pool fails spawn deterministically (QA-OFF-BY-ONE-9 09-15);
#             BUNKER_QA_SKIP_PREFLIGHT=1 bypasses THIS probe only.
preflights() {
  if ! grep -qE "^ +$SERVER:" "$HOME/.bunker/config.yaml" 2>/dev/null; then
    printf '{"cell":"run_battery","status":"FAIL","detail":"server %s not present in ~/.bunker/config.yaml — stale BUNKER_QA_SERVER default; run bunker config / pick a live server (no spawn attempted)","ts":"%s"}\n' \
      "$SERVER" "$(date -u +%FT%TZ)" >> "$EVIDENCE"
    echo "ERROR: server '$SERVER' not in ~/.bunker/config.yaml — refusing to spawn (deterministic config error)" >&2
    exit 2
  fi
  if ! timeout 30 ssh -i "$BUNKER_QA_HOST_KEY" -o IdentitiesOnly=yes -o ConnectTimeout=20 -o StrictHostKeyChecking=no -o BatchMode=yes "$SERVER" 'true' 2>/dev/null; then
    printf '{"cell":"run_battery","status":"FAIL","detail":"server %s unreachable via ssh (tailscale) preflight — no spawn attempted","ts":"%s"}\n' \
      "$SERVER" "$(date -u +%FT%TZ)" >> "$EVIDENCE"
    echo "ERROR: server '$SERVER' unreachable — refusing to spawn (deterministic reachability error)" >&2
    exit 2
  fi
  # Capacity preflight: a FULL pool is a deterministic spawn failure, so the
  # retry loop in spawn_agent() would burn 3 attempts and report the
  # exhaustion as if it were transient congestion. Probe cost: at most 2 cheap
  # bunkerd CLI calls, no spawn, no polling.
  if [ "${BUNKER_QA_SKIP_PREFLIGHT:-0}" = "1" ]; then
    log "capacity preflight skipped (BUNKER_QA_SKIP_PREFLIGHT=1)"
  else
    local cap_used cap_max cap
    cap=$(bunker_capacity "$SERVER")
    if [ -z "$cap" ]; then
      # Probe failed / unparseable ≠ full. Warn and continue: refusing here
      # would wedge QA on any bunkerd hiccup.
      echo "WARN: bunker capacity unknown on $SERVER (capacity probe failed) — proceeding without capacity preflight" >&2
    else
      cap_used="${cap%% *}"; cap_max="${cap##* }"
      if [ "$cap_max" = "-" ]; then
        echo "WARN: bunker capacity ceiling unreadable on $SERVER ($cap_used agents live) — proceeding without capacity preflight" >&2
      elif [ "$cap_used" -ge "$cap_max" ]; then
        printf '{"cell":"run_battery","status":"FAIL","detail":"bunker capacity exhausted on %s (%s/%s agents) — pool full, no spawn attempted (capacity preflight). Hint (QA-BUNKER-39): a stale QA temp agent may still hold a slot; `bunker destroy <id> --server %s` frees it (temp agents reap after the ~4h TTL)","ts":"%s"}\n' \
          "$SERVER" "$cap_used" "$cap_max" "$SERVER" "$(date -u +%FT%TZ)" >> "$EVIDENCE"
        echo "PREFLIGHT FAIL: bunker capacity exhausted on $SERVER ($cap_used/$cap_max agents) — refusing to spawn (deterministic capacity error)" >&2
        echo "HINT (QA-BUNKER-39): a stale QA temp agent may still hold a slot — list with: bunker list --server $SERVER; free it with: bunker destroy <id> --server $SERVER (temp agents reap after the ~4h TTL)" >&2
        exit 2
      fi
    fi
  fi
}

# ─── detect_cmds <repo> <agent> → DETECT_INSTALL / DETECT_NATIVE / DETECT_CI / DETECT_BIN ───
# Extracted verbatim from run() (2026-09-15) so run() and launch() can never
# drift on the command set they ship to the agent. Globals, because
# build_remote_script() bakes them (and $agent) into the generated script.
# DETECT_BIN (QA-9ROUTER-22, 2026-09-21) is the HOST-side mirror of the start
# command the generated script re-detects on the agent AFTER the install (the
# agent-side ladder in build_remote_script is authoritative at runtime because
# start scripts may materialize console shims only once deps exist); DETECT_BIN
# exists so tests and callers can assert the ladder without spawning an agent.
# KEEP THE TWO LADDERS IN SYNC.
detect_cmds() {
  local repo="$1" agent="$2"
  local tpkg="" start_script="" gofirst=""
  # escape hatches when the auto-detect guesses wrong (e.g. suite needs extras)
  DETECT_INSTALL="${BUNKER_QA_INSTALL_CMD:-}"
  if [ -z "$DETECT_INSTALL" ]; then
  if [ -f "$repo/pnpm-lock.yaml" ]; then DETECT_INSTALL="corepack enable 2>/dev/null; corepack prepare --activate 2>/dev/null; pnpm install --frozen-lockfile"
  elif [ -f "$repo/package.json" ]; then
    # QA-9ROUTER-26 (2026-09-21): the old branch installed ONLY the root
    # package, so a repo with an INDEPENDENT test package (tests/ or test/
    # with its own package.json declaring dependencies/devDependencies —
    # 9router pins vitest ^4.0.0 in tests/ and documents the two-step
    # `npm install && cd tests && npm install`) never got its runner
    # installed: the suite died with "tests/node_modules is missing — vitest
    # cannot be resolved" and chaos-disconnect graded that rc as OK "fails
    # fast on disconnect" (a false PASS on a suite that never ran). Detect an
    # independent test package and install it too, keeping the repo's own npm
    # flags for both steps. The generated script runs the install with cwd at
    # the repo root (fresh-install/upgrade cells wrap it in a subshell), so
    # the relative `cd` is correct.
    DETECT_INSTALL="npm ci --ignore-scripts --no-audit --no-fund"
    for tpkg in tests test; do
      if [ -f "$repo/$tpkg/package.json" ] && jq -e '(.dependencies // {}) + (.devDependencies // {}) | length > 0' "$repo/$tpkg/package.json" >/dev/null 2>&1; then
        DETECT_INSTALL="npm ci --ignore-scripts --no-audit --no-fund && cd $tpkg && npm ci --ignore-scripts --no-audit --no-fund"
        break
      fi
    done
  elif [ -f "$repo/go.mod" ]; then DETECT_INSTALL="go build ./..."
  elif [ -f "$repo/pyproject.toml" ]; then DETECT_INSTALL="pip install -e .[dev] 2>/dev/null || pip install -e ."
  elif [ -f "$repo/Cargo.toml" ]; then DETECT_INSTALL="cargo build --workspace"
  # QA-H3-14 (2026-09-21): a Makefile-only repo (no package manifest) gets a
  # make-based default only when make actually exists on the agent — JIT agents
  # ship WITHOUT make (measured: h3 battery 24dcbb04, all make-dependent cells
  # rc=127 'make: command not found'), and h3's Makefile itself has no build
  # target (verify-* only). `make verify` is the repo's one entry point that is
  # both real and dep-free (POSIX sh + coreutils + git; tick-chain degrades to
  # UNVERIFIED without jq/DuckBrain, qa-target self-validates on a re-inited
  # sync tree). Targets exist ⇒ call them; no targets and no make ⇒ the
  # toolchain-bootstrap leg below bootstraps GNU make into ~/tools first and the
  # verify cells still run; repo with make targets but agent lacking make AND
  # bootstrap failed ⇒ graded ENV-BLOCKED, never a repo defect.
  elif [ -f "$repo/Makefile" ]; then
    if grep -qE '^build:' "$repo/Makefile"; then DETECT_INSTALL="make build"
    else DETECT_INSTALL="make verify"; fi
  else DETECT_INSTALL="echo no-standard-install-path"
  fi
  fi
  local ci_cmd="" native_cmd="${BUNKER_QA_NATIVE_CMD:-}"
  DETECT_CI_FILES=""; DETECT_ACT_NOTE=""
  # The branch the agent's checkout will sit on: the CI cell simulates a push to
  # THE CHECKED-OUT BRANCH, so a workflow gated on other branches cannot fire.
  # sync_repo re-inits the synced tree (`git init -q`, no -b) so the branch name
  # there is whatever the AGENT's git defaults to — read it from the AGENT, not
  # from the host tree (they can differ: this host's git defaults to `master`,
  # but the doctrine is that the event the cell simulates is the one the agent
  # push would produce). Falls back to the host repo's branch, then to
  # BUNKER_QA_BRANCH, and an UNKNOWN branch keeps every workflow (fail-closed:
  # filtering on a guess could drop the workflow that regresses).
  branch_name="${BUNKER_QA_BRANCH:-$(git -C "$repo" rev-parse --abbrev-ref HEAD 2>/dev/null || true)}"
  [ "$branch_name" = "HEAD" ] && branch_name="master"
  [ -n "$branch_name" ] || branch_name="master"
  if [ -z "$native_cmd" ]; then
  if [ -f "$repo/pnpm-lock.yaml" ]; then native_cmd="pnpm test"
  elif [ -f "$repo/package.json" ]; then
  # --runInBand is a Jest flag; vitest dies with CACError on it (proven 9router
  # 2026-09-15: `cd tests && npx vitest run --runInBand` rc=1, zero tests ran).
  # Detect vitest by the test script's own text or a root vitest config and use
  # vitest's serial flag instead. Note: this only fixes HARNESS-induced crashes;
  # if the repo's root `npm test` npx-resolves an unpinned vitest, that hazard
  # belongs to the project (file a board row, do not mask it here).
  if jq -r '.scripts.test // ""' "$repo/package.json" 2>/dev/null | grep -q vitest || ls "$repo"/vitest.config.* >/dev/null 2>&1; then
    native_cmd="npm test -- --no-file-parallelism"
  else
    native_cmd="npm test -- --runInBand"
  fi
  elif [ -f "$repo/go.mod" ]; then native_cmd="go test ./... -count=1"
  elif [ -f "$repo/pyproject.toml" ]; then
    # QA-DIGEST-022 (2026-10-03, digest chaos-resource): a pyproject repo whose
    # pytest lives in a PEP-735 dependency-group gets rc=127 'pytest: command
    # not found' from the agent's broken venv on EVERY leg. The toolchain-
    # bootstrap cell tops pytest into ~/tools/venv earlier in the same agent
    # run — use it when PATH has no pytest. If it is absent too, keep the bare
    # name: native_runner_missing grades UNVERIFIED honestly, and the bootstrap
    # cell already warned which leg will fail and why.
    if ! command -v pytest >/dev/null 2>&1 && [ -x "$HOME/tools/venv/bin/pytest" ]; then
      native_cmd="$HOME/tools/venv/bin/pytest -x -q"
    else
      native_cmd="pytest -x -q"
    fi
  # Mirror the pyproject arm (QA-BUNKER-24, 2026-10-07): the venv-pytest guard
  # must apply here too — a toolchain-bootstrap'd agent whose PATH lacks pytest
  # but has ~/tools/venv/bin/pytest otherwise gets no pytest at all, and the
  # cell grades FAIL/native-runner-missing for an environment gap the harness
  # already solved for pyproject repos.
  # QA-FOREMAN-2026-09-26 (auger): pytest.ini/conftest.py-only repos matched NO
  # shape and got the vacuous `echo no-test-path` — the battery graded
  # ci-pass OK in 57ms with zero tests run while the repo carries 210 tests.
  # If pytest itself is absent (both PATH and venv) the suite NEVER RAN
  # and native_runner_missing grades UNVERIFIED (honest), never OK.
  elif [ -f "$repo/pytest.ini" ] || [ -f "$repo/conftest.py" ]; then
    if ! command -v pytest >/dev/null 2>&1 && [ -x "$HOME/tools/venv/bin/pytest" ]; then
      native_cmd="$HOME/tools/venv/bin/pytest -x -q"
    else
      native_cmd="python3 -m pytest -q"
    fi
  elif [ -f "$repo/Cargo.toml" ]; then native_cmd="cargo test --workspace"
  elif [ -f "$repo/Makefile" ]; then
    # QA-H3-14: native leg mirrors the install-leg target probe — a Makefile
    # with no `test:` target (h3: verify-* only) gets `make verify` instead of
    # a guaranteed-fail `make test` on agents where make was bootstrapped.
    if grep -qE '^test:' "$repo/Makefile"; then native_cmd="make test"
    else native_cmd="make verify"; fi
  else native_cmd="echo no-test-path"
  fi
  fi
  # QA-CRIER-20 (2026-09-18): unfiltered act runs EVERY workflow in the repo
  # under test, so infra-gated workflows (ghcr publish, pages deploys) that can
  # never pass inside act (no live bunker server, no ghcr credentials, no real
  # push event) give the ci-pass cell a PERMANENT red leg — crier's bunker-e2e
  # was 5/5 green on hosted CI at the same HEAD while act stayed red. The CI
  # workflow is the simulated-runner-relevant one; with no ci.yml keep the
  # unfiltered form, and with no workflows at all native suite remains the
  # fallback grade.
  #
  # QA-9ROUTER-19 (2026-09-23): the unfiltered fallback above was itself the
  # defect for a repo with NO ci.yml. 9router's `.github/workflows/` holds
  #   docker-publish.yml  on: push TAGS v* + workflow_dispatch
  #   gitbook-pages.yml   on: push branches [main, master] paths gitbook/**
  #   tests.yml           on: push branches [federation, master]
  # so a branch push to `federation` can NEVER trigger the first two, yet act
  # ran them and the cell graded FAIL — QA-9ROUTER-19 records the false board
  # row ("Test Suite/test SKIP: tests/node_modules missing", "-  Install test
  # dependencies  Success  up to date, audited 1 package", plus red
  # build-and-push and build-deploy legs). Hosted CI was GREEN at the same HEAD.
  # The `-W .github/workflows/ci.yml` arm above (QA-CRIER-20) exists for exactly
  # this class; this arm supplies the same effect for repos without a ci.yml by
  # staging ONLY the workflows that can fire on a branch push into a temp
  # directory and pointing -W at it.
  #
  # MECHANISM (measured 2026-09-23 on this host, act = the same static tarball
  # ACT_URL pins): act MERGES several -W targets in argv order, a DIRECTORY
  # target recurses, and an external directory of symlinks to the repo's
  # workflows resolves correctly — so `-W <staging dir OUTSIDE the synced tree>`
  # runs exactly the selected workflows, in the repo's real checkout (cwd), with
  # the synced tree left untouched (no in-tree write ⇒ nothing can contaminate
  # the install/e2e cells or the audit payload). The staging dir MUST be both
  # symlink-resolvable AND outside the repo: the cell runs with the repo as cwd,
  # so a repo-relative -W target resolves fine too, but an EXTERNAL dir keeps
  # the synced tree byte-clean, which the "accept the hostile event" doctrine
  # (itertools, 2026-09-22 — the synthetic tree is the payload under audit)
  # requires.
  #
  # STAGING IS DONE IN A SUBSHELL, NOT IN THE GENERATED SCRIPT, because the
  # cell's workdir IS the synced tree: every directory and file this arm creates
  # is created by a subshell the cell `cd`s into first, and the staging dir is
  # an EXTERNAL absolute path, so the synced tree gains no new top-level entry.
  # When nothing is triggerable the arm keeps native as the grade — a repo whose
  # only workflows are dispatch/tag-gated has no branch-push CI, and "act could
  # not find any stages" is not a product result.
  #
  # Triggerability is decided by QA_CI_TRIGGER_RE (overridable), matched
  # line-wise against each workflow, and the base-branch name comes from the
  # agent's own checkout (QA_BRANCH): a push push to the checked-out branch is
  # the event the cell simulates, so a workflow gated on other branches is not
  # a product failure.
  if ls "$repo"/.github/workflows/*.yml >/dev/null 2>&1; then
    if [ -f "$repo/.github/workflows/ci.yml" ]; then
      # FIX (QA-9ROUTER-28 follow-up, 2026-09-22): the -W path must be the AGENT's
      # checkout (~/qa-$proj), not the host repo path — $repo does not exist on the
      # agent, so act died rc=1 "stat /home/kara/<repo>/.github/workflows/ci.yml:
      # permission denied" (the same harness artifact recorded in the 09-20/09-21
      # dagger QA ledgers). The remote script cd's to ~/qa-$proj before the ci-pass
      # cell, so a RELATIVE workflow path resolves inside the agent's checkout.
      # FIX (QA foreman 2026-09-24, live-proven on pulse): the ci.yml fast-path
      # previously baked the -W path INTO ci_cmd and left DETECT_CI_FILES empty —
      # but the generated remote script stages workflows ONLY from QA_CI_SELECT
      # (ACT_WF_COUNT gate), so act was NEVER RUN for ci.yml repos: the cell took
      # the skip branch and reported "no triggerable workflow" (or failed over to
      # native as the grade) while a triggerable ci.yml sat in the synced tree.
      # Measured twice on pulse 2026-09-24 (agents 882dc470 / 2a8a8dbd): cell said
      # "no triggerable workflow" while `act -W .github/workflows/ci.yml` run by
      # hand on the same agent, same tree, completed the job. The -W now comes
      # from the shared staging block like every other workflow: select ci.yml,
      # keep ci_cmd bare, let ACT_WF_DIR staging supply the single -W target.
      ci_cmd="DOCKER_HOST=unix:///run/bunker/$agent/docker.sock ~/bin/act -q --pull"
      DETECT_CI_FILES=" .github/workflows/ci.yml"
    else
      # Select the workflows whose `on:` can fire for a push to the branch the
      # agent will have checked out. Parsing happens HOST-side (the agent needs
      # no YAML parser) but the SELECTION is baked as repo-relative paths and
      # the staging itself runs on the agent (see QA_CI_SELECT in
      # build_remote_script).
      #
      # MEASURED on this host 2026-09-23 (act = the tarball ACT_URL pins), which
      # is why the shape is a STAGED DIRECTORY and not a list of -W files:
      #   `-W a.yml -W b.yml`      → act runs ONLY b.yml (the last -W wins; the
      #                              targets do NOT merge — a partial selection
      #                              would silently DROP the workflow that can
      #                              regress, strictly worse than today)
      #   `-W dirA -W dirB`        → both directories merge
      #   `-W <external dir of symlinks to the repo's workflows>` → resolved and
      #     run, and act still executes the repo's OWN checkout as the cwd
      #     (`docker cp src=<repo>/.`), so a staged -W costs the cell nothing.
      # ⇒ stage every selected workflow into ONE directory and pass that single
      #   -W target.
      #
      # PREDICATE — scan the top-level `on:` block and decide triggerability
      # from the KEYS, never from prose: `branches:` gates a push (any entry may
      # be a literal branch, a wildcard, or an expression), `tags:` without
      # `branches:` makes it TAG-ONLY (a push to the branch under test can never
      # fire it), and a push with NEITHER key fires on every branch. `paths:`
      # filters are deliberately ignored — they describe which CHANGES fire the
      # workflow, not which branch can, and reading them as branch patterns was
      # the first shape of this loop: it matched gitbook-pages.yml's
      # `- "gitbook/**"` entry and kept exactly the workflow QA-9ROUTER-19 says
      # must not be run. Entries containing `*` or `$` (a wildcard or a GitHub
      # expression) are kept — they cannot be proven non-matching (fail-closed);
      # everything else is compared literally against the branch under test.
      # A workflow with NO `on:` at all is kept (odd/unparseable file — act must
      # see it and report, never be silently dropped).
      local wf rel keep on_seen push_seen branch_key tags_key in_branches lhs rhs ci_select=""
      for wf in "$repo"/.github/workflows/*.yml; do
        [ -f "$wf" ] || continue
        rel="${wf#$repo/}"
        keep=0; on_seen=0; push_seen=0; branch_key=0; tags_key=0; in_branches=0
        while IFS= read -r line; do
          case "$line" in *$'\t'*) continue ;; esac            # block-scalar body
          case "$line" in
            ""|"#"*) continue ;;
            "on:"|'"on":') on_seen=1; continue ;;
          esac
          case "$line" in [!\ ]*) [ "$on_seen" = 1 ] && break ;; esac   # next top-level key
          [ "$on_seen" = 1 ] || continue
          lhs="${line#"${line%%[![:space:]]*}"}"                # left-trim
          case "$lhs" in
            "- "*)
              [ "$in_branches" = 1 ] || continue
              rhs="${lhs#- }"
              case "$rhs" in "'"*"'") rhs="${rhs#\'}"; rhs="${rhs%\'}" ;; '"'*'"') rhs="${rhs#\"}"; rhs="${rhs%\"}" ;; esac
              case "$rhs" in *"*"*|*'$'*) keep=1; break ;; esac          # wildcard / expression
              case "$rhs" in *"#"*) rhs="${rhs%%#*}"; : ;; esac
              # shellcheck disable=SC2254
              case "$rhs" in "$branch_name") keep=1; break ;; esac
              ;;
            "push:"|"push: "*|'"push":'*) push_seen=1; in_branches=0 ;;
            "branches:"|"branches: "*|"branches-ignore:"|"branches-ignore: "*)
              branch_key=1; in_branches=1
              rhs="${lhs#*:}"
              case "$rhs" in *"$branch_name"*) keep=1; break ;; esac
              case "$rhs" in *"*"*|*'$'*) keep=1; break ;; esac
              ;;
            "tags:"|"tags: "*|"tags-ignore:"|"tags-ignore: "*)
              tags_key=1; in_branches=0 ;;
            *":") in_branches=0 ;;                                # any other key
            *) continue ;;
          esac
        done < "$wf"
        # verdict
        if [ "$on_seen" = 0 ]; then keep=1
        elif [ "$keep" = 1 ]; then :
        elif [ "$push_seen" = 1 ] && [ "$tags_key" = 0 ] && [ "$branch_key" = 0 ]; then keep=1
        fi
        [ "$keep" = 1 ] || continue
        ci_select="$ci_select $rel"
      done
      if [ -n "$ci_select" ]; then
        DETECT_CI_FILES="$ci_select"           # baked, leading space, repo-relative
        ci_cmd="DOCKER_HOST=unix:///run/bunker/$agent/docker.sock ~/bin/act -q --pull"
      else
        # No workflow can fire on a push to this branch ⇒ act has nothing that
        # says anything about the repo, and its unfiltered run grades the
        # non-triggerable legs red (the QA-9ROUTER-19 false FAIL). The native
        # suite is already the documented authoritative fallback; make it the
        # grade here and SAY so, so the cell reports a real suite result rather
        # than an act artifact. QA_ACT_NOTE rides into every ci-pass cell text.
        ci_cmd="$native_cmd"
        DETECT_ACT_NOTE=" no workflow in .github/workflows can fire on a push to '$branch_name' (all are tag-only / other-branch / workflow_dispatch-only) — act skipped, the native suite is the authoritative grade"
      fi
    fi
  else
    ci_cmd="$native_cmd"  # no workflow → native IS the CI pass
  fi
  DETECT_NATIVE="$native_cmd"
  DETECT_CI="$ci_cmd"
  # QA-9ROUTER-22 (2026-09-21): host-side mirror of the agent-side start
  # command ladder (build_remote_script). Same precedence: custom-server.js >
  # server.js > start.sh > package.json .scripts.start > src/index.js (only if
  # the file exists) > empty. Never a bare guess — "node src/index.js" for a
  # package.json repo whose src/index.js does not exist is what made
  # chaos-corruption/chaos-errorpath grade vacuous rc=1 verdicts on 9router.
  DETECT_BIN=""
  if [ -f "$repo/custom-server.js" ]; then
    DETECT_BIN="node custom-server.js"
  elif [ -f "$repo/server.js" ]; then
    DETECT_BIN="node server.js"
  elif [ -f "$repo/start.sh" ] && [ -x "$repo/start.sh" ]; then
    DETECT_BIN="sh start.sh"
  elif [ -f "$repo/package.json" ]; then
    start_script="$(jq -r '.scripts.start // ""' "$repo/package.json" 2>/dev/null)"
    case "$start_script" in
      # npm lifecycle no-ops are not a start command; and a start script that
      # is literally "node server.js" is exactly what the server.js arm above
      # already covers (keeps one canonical form instead of duplicating it).
      ""|"echo "*|"exit "*|":") : ;;
      "node server.js") DETECT_BIN="node server.js" ;;
      *) DETECT_BIN="npm start" ;;
    esac
    # No real start script? Fall through the ladder (it is not the end of it):
    # src/index.js is the LAST resort and only when the file actually exists —
    # the old code guessed it for EVERY package.json repo (9router has none,
    # so both chaos cells started "Cannot find module" and graded the refusal).
    if [ -z "$DETECT_BIN" ] && [ -f "$repo/src/index.js" ]; then
      DETECT_BIN="node src/index.js"
    fi
  elif [ -f "$repo/src/index.js" ]; then
    DETECT_BIN="node src/index.js"
  fi
  # Mirror the agent-side interpreter-resolvability guard: an interpreter that
  # cannot be resolved here means "not observable", same as empty.
  if [ -n "$DETECT_BIN" ] && ! command -v "${DETECT_BIN%% *}" >/dev/null 2>&1; then
    DETECT_BIN=""
  fi
  detect_upgrade_inputs "$repo"
}

# ─── npm package coordinates: publishable beats private (QA-9ROUTER-20) ───
# QA-9ROUTER-20 (2026-09-23): the upgrade cell installed the ROOT package.json
# name, but a repo can be a private workspace whose only PUBLISHED artifact is a
# nested package. 9router's root is `"name": "9router-app", "private": true`
# (upstream identical — never published) while `cli/package.json` is
# `"name": "9router"` (npm dist-tag latest 0.5.75). The cell therefore FAILed
# with a FALSE product verdict on the previous release:
#   cell upgrade FAIL "previous release 9router-app@0.5.69 would not install
#   (tag v0.5.69): rc=1 … E404 Not Found - GET https://registry.npmjs.org/9router-app"
# while the clean-agent probe `npm install -g 9router@0.5.69` returned rc=0.
# The upgrade path stayed UNVERIFIED because the harness named the wrong package.
# Rule: the ROOT name wins whenever the root is publishable (every repo that
# works today must keep byte-identical behaviour); otherwise take the first
# publishable NESTED candidate, and fall back to the root name when no
# publishable candidate exists at all — so a root-only repo is unchanged and a
# genuinely unpublished package still reports the real registry error.
# "Publishable" = non-empty `name` and not `"private": true` (npm's own rule).
npm_pkg_name() { # <package.json> — print the "name" field, or nothing
  [ -f "$1" ] || return 0
  sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$1" | head -1
}

npm_pkg_publishable() { # <package.json> — 0 when it has a name and is not private
  local npf="$1" npn
  [ -f "$npf" ] || return 1
  npn=$(npm_pkg_name "$npf"); [ -n "$npn" ] || return 1
  if command -v jq >/dev/null 2>&1; then
    jq -e '(.private // false) != true' "$npf" >/dev/null 2>&1
  else
    ! grep -qE '"private"[[:space:]]*:[[:space:]]*true' "$npf"
  fi
}

detect_publish_pkg_name() { # <repo> — publishable package name, root name as the fallback
  local dpr="$1" dpf="" dpname="" rel
  if npm_pkg_publishable "$dpr/package.json"; then npm_pkg_name "$dpr/package.json"; return 0; fi
  # Explicit candidates first (deterministic priority for the common layouts),
  # then a bounded scan of any other nested package.json. Build/VCS dirs are
  # pruned so vendored copies cannot win; AMBIGUITY IS NOT GUESSED — two
  # distinct publishable nested candidates and no explicit candidate means the
  # function returns NOTHING, and the upgrade cell reports the honest
  # "no installable previous release detected" instead of picking one.
  for rel in cli packages/cli app packages/app packages/npm server desktop pkg npm; do
    if npm_pkg_publishable "$dpr/$rel/package.json"; then npm_pkg_name "$dpr/$rel/package.json"; return 0; fi
  done
  local -a scan_names=()
  while IFS= read -r dpf; do
    npm_pkg_publishable "$dpf" || continue
    dpname=$(npm_pkg_name "$dpf"); [ -n "$dpname" ] || continue
    scan_names+=("$dpname")
  done < <(find "$dpr" -maxdepth 4 -name package.json \
             -not -path '*/node_modules/*' -not -path '*/.git/*' \
             -not -path '*/.next*' -not -path '*/dist/*' -not -path '*/build/*' \
             -not -path '*/vendor/*' -not -path '*/coverage/*' 2>/dev/null | sort)
  # Ambiguity is NOT guesswork: 2+ distinct publishable nested candidates with no
  # explicit candidate => return NOTHING, and the caller reports "no installable
  # previous release detected" (the honest UNVERIFIED grade) instead of picking
  # one at random.
  local first_name="" seen_other=0 sn
  for sn in "${scan_names[@]+"${scan_names[@]}"}"; do
    if [ -z "$first_name" ]; then first_name="$sn"; continue; fi
    if [ "$sn" != "$first_name" ]; then seen_other=1; fi
  done
  if [ -n "$first_name" ] && [ "$seen_other" = 0 ]; then printf '%s\n' "$first_name"; return 0; fi
  if [ "$seen_other" = 1 ]; then return 0; fi
  # No publishable candidate at all → the ROOT name (legacy behaviour): the
  # registry error that follows is the honest verdict, never a silent skip.
  npm_pkg_name "$dpr/package.json"
}

# ─── detect_upgrade_inputs <repo> — inputs the upgrade cell cannot detect itself ───
# QA-GITREINS-POC-004 (2026-09-17): the upgrade cell used to run `git tag` INSIDE
# the battery, but sync_repo ships the tree with `.git` excluded and then re-inits
# a synthetic single-commit repo on the agent, so `git tag` there is ALWAYS empty.
# The cell therefore reported `upgrade N/A "no release tags in repo"` for repos
# that do have tags — gitreins-poc (40 tags, latest v0.13.0) and
# coding-hermes-boardctl (3) on the 2026-09-12 cycle — and the upgrade path was
# silently recorded as not-applicable instead of untested. Detect the tag list
# HERE, where `.git` really exists, and bake the values into the generated script
# along with the previous release's package coordinates so the cell can install
# that release and re-install HEAD over it on the agent.
# Sets global: DETECT_TAG_COUNT / DETECT_PREV_TAG / DETECT_PREV_VERSION /
# DETECT_PKG_NAME / DETECT_PKG_ECOSYSTEM / DETECT_GO_URL
# ("" when unknown — never guessed).
detect_upgrade_inputs() {
  local repo="$1"
  DETECT_TAG_COUNT=0; DETECT_PREV_TAG=""; DETECT_PREV_VERSION=""
  DETECT_PKG_NAME=""; DETECT_PKG_ECOSYSTEM=""; DETECT_GO_URL=""
  # QA-WARPFS-QA (2026-09-25): initialized here, not only in the npm branch —
  # under set -u an unset DETECT_UP_PREV_DIR aborts the build_remote_script
  # heredoc for non-npm repos (Rust/Go/pip with tags) and ships a 0-byte
  # qa-run.sh, so the whole battery exits instantly with zero cells.
  DETECT_UP_PREV_DIR=""
  [ -d "$repo/.git" ] || return 0
  DETECT_TAG_COUNT=$(git -C "$repo" tag 2>/dev/null | grep -c . || true)
  [ "${DETECT_TAG_COUNT:-0}" = "0" ] && return 0
  DETECT_PREV_TAG=$(git -C "$repo" tag 2>/dev/null | sort -V | tail -2 | head -1)
  DETECT_PREV_VERSION="${DETECT_PREV_TAG#v}"
  # Package coordinates for the index-install half of the upgrade test. Only the
  # two ecosystems whose install command the battery already runs are wired;
  # anything else stays "" and the cell reports the gap instead of guessing.
  if [ -f "$repo/pyproject.toml" ]; then
    DETECT_PKG_ECOSYSTEM="pip"
    DETECT_PKG_NAME=$(sed -n 's/^[[:space:]]*name[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$repo/pyproject.toml" | head -1)
    [ -z "$DETECT_PKG_NAME" ] && DETECT_PKG_NAME=$(sed -n "s/^[[:space:]]*name[[:space:]]*=[[:space:]]*'\([^']*\)'.*/\1/p" "$repo/pyproject.toml" | head -1)
  elif [ -f "$repo/package.json" ]; then
    # QA-9ROUTER-20: publishable name, not the private root name.
    DETECT_PKG_ECOSYSTEM="npm"
    DETECT_PKG_NAME=$(detect_publish_pkg_name "$repo")
    # QA-HEADING-009 (2026-09-25): a NON-publishable npm repo (private:true root,
    # no publishable nested candidate) has NO registry artifact — the registry
    # arm can only ever die ETARGET/E404, a harness premise failure graded as a
    # product verdict. Mark the repo for the tree-upgrade arm: the host streams
    # the previous tag's tree (ship_prev_tag_tree) and the cell upgrades the
    # checked-out tree, same shape as the Go-binary arm (QA-OFF-BY-ONE-22).
    DETECT_UP_PREV_DIR=""
    if [ -n "$DETECT_PREV_TAG" ] && ! npm_pkg_publishable "$repo/package.json"; then
      DETECT_UP_PREV_DIR="~/qa-up-prev-npm"
    fi
  elif [ -f "$repo/go.mod" ]; then
    # QA-OFF-BY-ONE-22 (2026-09-22): Go-binary repos have no index package.
    # The upgrade cell upgrades the checked-out TREE from the previous
    # release tag instead; ship_prev_tag_tree streams that tree to the agent
    # (git archive over the sync channel) and DETECT_GO_URL is the agent-side
    # clone fallback for public URLs.
    DETECT_PKG_ECOSYSTEM="go"
    DETECT_GO_URL=$(git -C "$repo" remote get-url origin 2>/dev/null || true)
  elif [ -f "$repo/Cargo.toml" ]; then
    # QA-WARPFS-QA-2 (2026-09-25): Rust repos were unwired — the host detected
    # tags but the cell had no ecosystem for cargo, so a tagged Rust repo graded
    # upgrade FAIL "no installable previous release" while the tree-stream arm
    # (cargo build of the previous tag's tree) is already ecosystem-agnostic
    # (QA-OFF-BY-ONE-22 shape). Wire the ecosystem; ship_prev_tag_tree streams
    # the previous tag's tree; the cell's UP_PREV arm builds it. Cargo has no
    # index package — PKG_NAME stays "".
    DETECT_PKG_ECOSYSTEM="rust"
  fi
  # The value is interpolated into the generated script inside single quotes.
  DETECT_PKG_NAME=${DETECT_PKG_NAME//\'/}
  DETECT_PREV_TAG=${DETECT_PREV_TAG//\'/}
  return 0
}

# ─── ship_prev_tag_tree <repo> <proj> — stream the previous tag's tree ───
# QA-OFF-BY-ONE-22 (2026-09-22): the upgrade cell for Go-binary repos needs
# the PREVIOUS release's tree, but the synced tree carries no history
# (sync_repo excludes .git). Stream the previous tag's full tree to
# ~/qa-up-prev via git archive over the SAME channel sync_repo uses — no
# clone, no auth on the agent (bunker agents carry no GitHub keys). No-op
# when the repo has no go-ecosystem previous tag. On failure, mark
# ~/qa-up-prev.absent so the cell FAILs with the real reason instead of
# silently skipping the upgrade leg. QA-HEADING-009 (2026-09-25): also streams
# for NON-publishable npm repos (DETECT_UP_PREV_DIR set by detect_upgrade_inputs)
# — same tree-stream, agent dir ~/qa-up-prev-npm, verified by package.json.
ship_prev_tag_tree() {
  local repo="$1" proj="$2" agent_dir verify_path
  if [ "$DETECT_PKG_ECOSYSTEM" = "go" ] && [ -n "$DETECT_PREV_TAG" ]; then
    agent_dir="~/qa-up-prev"; verify_path="go.mod"
  elif [ "$DETECT_PKG_ECOSYSTEM" = "rust" ] && [ -n "$DETECT_PREV_TAG" ]; then
    # QA-WARPFS-QA-2: previous tag's tree for the cargo build arm — verified by
    # Cargo.toml. Cargo has no installable index package, so the tree-stream is
    # the only real previous->HEAD upgrade path for Rust repos.
    agent_ssh "rm -rf ~/qa-up-prev ~/qa-up-prev.absent" >/dev/null 2>&1 || true
    agent_dir="~/qa-up-prev"; verify_path="Cargo.toml"
  elif [ "$DETECT_PKG_ECOSYSTEM" = "npm" ] && [ -n "$DETECT_UP_PREV_DIR" ] && [ -n "$DETECT_PREV_TAG" ]; then
    # QA-HEADING-009: agents are SHARED across projects — clear any stale tree
    # (or an absent-marker from a previous failed stream) before shipping, so
    # the generated script's package.json check cannot false-positive on a
    # foreign repo's leftovers.
    agent_ssh "rm -rf ~/qa-up-prev-npm ~/qa-up-prev-npm.absent" >/dev/null 2>&1 || true
    agent_dir="$DETECT_UP_PREV_DIR"; verify_path="package.json"
  else
    return 0
  fi
  if git -C "$repo" archive --format=tar "$DETECT_PREV_TAG" 2>/dev/null \
      | agent_ssh "mkdir -p $agent_dir && tar -C $agent_dir -xf -" >/dev/null 2>&1 \
      && agent_ssh "test -f $agent_dir/$verify_path" >/dev/null 2>&1; then
    log "CELL upgrade-prep: previous release tree $DETECT_PREV_TAG -> $agent_dir"
  else
    agent_ssh "rm -rf $agent_dir; touch ${agent_dir}.absent" >/dev/null 2>&1 || true
  fi
}

# sync_repo <repo> <proj> — sync the repo to ~/qa-<proj> over SSH
# TWO payload paths (2026-09-23, QA-TERMINAL-JAIL-9):
#   DEFAULT — a FROZEN `git archive HEAD` stream: the audited tree is exactly
#     the pushed HEAD commit, immune to workdir-mid-edit drift (staged worker
#     files, generated caches, untracked dagger.db-style artifacts never ship).
#   LEGACY (BUNKER_QA_SYNC_EXCLUDES set) — the old tracked+untracked file list
#     tar, kept for projects that must exclude tracked-heavy DATA dirs from the
#     payload (git archive cannot exclude tracked paths). This path tars the
#     LIVE workdir non-atomically and carries the documented drift risk.
# Verdict comes from the REMOTE's SYNC-OK echo, not the local stream's exit
# code. GNU tar exits non-zero on "file changed as we read it" even with
# --warning=no-file-changed, and with `set -o pipefail` that would abort the
# whole battery although the remote extraction succeeded. Live repos (actively
# written during tar) trip this constantly.
# BUNKER_QA_SYNC_EXCLUDES: extra --exclude args for monster repos whose
# tracked-but-heavy DATA dirs (generated images, fixtures) would stall the SSH
# pipe past the 1800s agent_ssh timeout (proven wojons-mythos 2026-08-31:
# 6.4G repo / 845M projects/ + 482M packages/backend/projects at ~600KB/s).
# BUNKER_QA_SYNC_MAX_BYTES: payload byte guard, default 300 MB.
# Returns 1 on failure with the remote's last 2 lines in SYNC_ERR.
sync_repo() {
  local repo="$1" proj="$2"
  local sync_excludes="${BUNKER_QA_SYNC_EXCLUDES:-}"
  local sync_max="${BUNKER_QA_SYNC_MAX_BYTES:-314572800}"
  local sync_list sync_bytes sync_out sync_ok sync_oversize
  local -a sync_excl=()
  if [ -n "$sync_excludes" ]; then read -ra sync_excl <<< "$sync_excludes"; fi

  if [ -z "$sync_excludes" ]; then
    # FROZEN path: payload = exactly HEAD. Measure the real stream for the
    # byte guard, then re-stream it through the SSH pipe.
    if ! sync_bytes=$(git -C "$repo" archive HEAD | wc -c); then
      SYNC_ERR="SYNC-ARCHIVE-ERROR: git archive HEAD failed for $repo (empty repo?)"
      return 1
    fi
    if [ "${sync_bytes:-0}" -le 0 ]; then
      SYNC_ERR="SYNC-ARCHIVE-ERROR: git archive HEAD produced no bytes for $repo"
      return 1
    fi
    if [ "$sync_bytes" -gt "$sync_max" ]; then
      sync_oversize="SYNC-OVERSIZE: $sync_bytes > $sync_max (set BUNKER_QA_SYNC_MAX_BYTES to override)"
      printf '%s\n' "$sync_oversize" >&2
      SYNC_ERR="$sync_oversize"
      return 1
    fi
    sync_out=$(git -C "$repo" archive HEAD |
      agent_ssh "mkdir -p ~/qa-$proj && tar -C ~/qa-$proj -xf - && cd ~/qa-$proj && git init -q && git -c user.name='qa-bot' -c user.email='qa@bunker' add -A && git -c user.name='qa-bot' -c user.email='qa@bunker' commit -q -m 'qa-sync' && echo SYNC-OK" 2>&1)
    sync_ok=$(printf '%s' "$sync_out" | grep -c 'SYNC-OK')
    if [ "$sync_ok" -eq 0 ]; then
      SYNC_ERR=$(printf '%s' "$sync_out" | tail -2)
      return 1
    fi
    return 0
  fi

  sync_list=$(mktemp) || { SYNC_ERR='SYNC-LIST-ERROR: could not create file list'; return 1; }
  if ! git -C "$repo" ls-files -z --cached --others --exclude-standard > "$sync_list"; then
    SYNC_ERR="SYNC-LIST-ERROR: git ls-files failed for $repo"
    rm -f "$sync_list"
    return 1
  fi
  # Index entries whose worktree copy was deleted (git status 'D' — e.g. rotated
  # gitreins guard logs) break `du --files0-from` and `tar -T`; keep only paths
  # that actually exist on disk. (2026-09-22, off-by-one SYNC-SIZE-ERROR.)
  local sync_list2
  sync_list2=$(mktemp) || { SYNC_ERR='SYNC-LIST-ERROR: could not create filtered list'; return 1; }
  while IFS= read -r -d '' f; do
    [ -e "$repo/$f" ] && printf '%s\0' "$f"
  done < "$sync_list" > "$sync_list2"
  mv -f "$sync_list2" "$sync_list"
  case "$sync_max" in
    ''|*[!0-9]*)
      SYNC_ERR="SYNC-LIMIT-ERROR: BUNKER_QA_SYNC_MAX_BYTES is not an integer: $sync_max"
      rm -f "$sync_list"
      return 1
      ;;
  esac
  if ! sync_bytes=$(cd "$repo" && du -cb --files0-from="$sync_list" | awk 'END {print $1}'); then
    SYNC_ERR="SYNC-SIZE-ERROR: could not measure payload for $repo"
    rm -f "$sync_list"
    return 1
  fi
  sync_bytes=${sync_bytes:-0}
  if [ "$sync_bytes" -gt "$sync_max" ]; then
    sync_oversize="SYNC-OVERSIZE: $sync_bytes > $sync_max (set BUNKER_QA_SYNC_MAX_BYTES to override)"
    printf '%s\n' "$sync_oversize" >&2
    SYNC_ERR="$sync_oversize"
    rm -f "$sync_list"
    return 1
  fi

  sync_out=$(tar --warning=no-file-changed --null --verbatim-files-from -C "$repo" \
      "${sync_excl[@]}" -T "$sync_list" -cf - |
    agent_ssh "mkdir -p ~/qa-$proj && tar -C ~/qa-$proj -xf - && cd ~/qa-$proj && git init -q && git -c user.name='qa-bot' -c user.email='qa@bunker' add -A && git -c user.name='qa-bot' -c user.email='qa@bunker' commit -q -m 'qa-sync' && echo SYNC-OK" 2>&1)
  rm -f "$sync_list"
  sync_ok=$(printf '%s' "$sync_out" | grep -c 'SYNC-OK')
  if [ "$sync_ok" -eq 0 ]; then
    SYNC_ERR=$(printf '%s' "$sync_out" | tail -2)
    return 1
  fi
  return 0
}

# ─── build the remote cell script (all quoting handled HERE, once) ───
build_remote_script() { # args: proj install_cmd ci_cmd native_cmd
  local proj="$1" install_cmd="$2" ci_cmd="$3" native_cmd="$4"
  cat <<EOF
#!/usr/bin/env bash
# qa-run.sh — generated by bunker-qa.sh for $proj (do not edit)
PROJ='$proj'
EVID=~/qa-evidence.jsonl
# Per-run log dir in HOME — /tmp/*.log is shared across agents and the
# sticky bit makes a fresh agent uid unable to overwrite a previous
# agent's files (Permission denied + stale content read-back). A
# run-unique dir sidesteps both.
LOGD=~/qa-logs-\$\$
mkdir -p \$LOGD
: > \$EVID
# Completion marker (QA-HERMES-CANOPY-8): collect()'s cheap probe — present ⇒
# the battery is DONE (whichever exit path it took), absent + no live process
# ⇒ the agent/process is gone. The EXIT trap guarantees the marker even on the
# early "no repo dir" bail; the explicit touch after CELLS-DONE is the normal path.
trap 'touch ~/qa-run.finished' EXIT
cell() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import json,sys,datetime; print(json.dumps({"project":"'\$PROJ'","cell":sys.argv[1],"status":sys.argv[2],"detail":sys.argv[3],"ts":datetime.datetime.now(datetime.timezone.utc).isoformat()}))' "\$1" "\$2" "\$3" >> \$EVID
  else
    local d; d=\$(printf '%s' "\$3" | tr '\n\t\r' '   ' | sed 's/"/'"'"'/g'); echo "{\"project\":\"\$PROJ\",\"cell\":\"\$1\",\"status\":\"\$2\",\"detail\":\"\$d\",\"ts\":\"\$(date -u +%FT%TZ)\"}" >> \$EVID
  fi
}
fail_detail() { # <rc> <logfile> <cmd...> — ACTIONABLE-FAILURE CONTRACT
  local frc="\$1" flog="\$2"; shift 2
  printf 'rc=%s cmd=[%s] — %s' "\$frc" "\$*" "\$(grep -v '^[[:space:]]*\$' "\$flog" 2>/dev/null | head -40 | tr '\n' '|' | cut -c1-1500)"
}
# qa_have_pip — QA-BUNKER-57 (2026-10-04): a fresh JIT agent's ensurepip can
# fail (no sudo, broken venv), leaving NO pip anywhere; the old harness then
# cascaded rc=127 'pip: command not found' through fresh-install, upgrade and
# chaos-resource as FAIL/ENV-BLOCKED with zero repo signal. Every leg that
# invokes pip must call this first and grade UNVERIFIED instead.
qa_have_pip() { # 0 when pip is usable (PATH pip, python3 -m pip, or the venv interpreter)
  command -v pip >/dev/null 2>&1 && return 0
  python3 -m pip --version >/dev/null 2>&1 && return 0
  [ -x "\$HOME/tools/venv/bin/python3" ] && "\$HOME/tools/venv/bin/python3" -m pip --version >/dev/null 2>&1 && return 0
  return 1
}
# act_failure_context — act's '❌ Failure - Main <step> [Xs]' line names the step
# but never the cause; the step's own output is printed ABOVE the marker, so
# return the 18 lines preceding the first ❌ plus the marker itself.
act_failure_context() { # <act log>
  local lf="\$1" n
  n=\$(grep -n '❌' "\$lf" 2>/dev/null | head -1 | cut -d: -f1)
  if [ -n "\$n" ]; then sed -n "\$(( n > 18 ? n - 18 : 1 )),\${n}p" "\$lf" | tr '\n' '|' | cut -c1-1400
  else tail -8 "\$lf" 2>/dev/null | tr '\n' '|' | cut -c1-1400; fi
}
# build_env_failure — is this failure the AGENT's missing C toolchain instead of a
# repo defect? (QA-WARPFS-9, 2026-09-19.) rust links EVERY crate build script
# through the \`cc\` linker, so on a toolchain-bare agent (no cc/gcc/clang, no sudo)
# the first crate with a build script dies with
#   error: linker \`cc\` not found
#   error: could not compile \`libc\` (build script) due to 1 previous error   (rc=101)
# That is an ENVIRONMENT gap, not a finding — the pre-QA-WARPFS-9 harness graded it
# fresh-install FAIL ("repo defect"), chaos-resource FAIL ("proves nothing about the
# 3G cap") and chaos-disconnect OK "fails fast on disconnect" (a FALSE PASS: the
# network cut was never exercised, the build died first). A log that ALSO carries a
# genuine rustc diagnostic (error[E####]) is a real compile error and must keep its
# FAIL grade, so this returns 0 only for the bare toolchain shape.
build_env_failure() { # <logfile> — 0 when the log carries the toolchain signature only
  local bf="\$1"
  [ -f "\$bf" ] || return 1
  grep -qE 'linker \`cc\` not found|could not compile .*\(build script\)' "\$bf" 2>/dev/null || return 1
  ! grep -qE '^error\[E[0-9]{4}\]' "\$bf" 2>/dev/null
}
# build_sys_dep_failure — did a *-sys crate's build script fail to find a system
# library? (QA-WARPFS-9 rework 3, 2026-09-19.) The live round-2 shape, on the same
# bare agent once the zig leg had fixed the linker:
#   error: failed to run custom build command for \`openssl-sys v0.9.117\`
#   Could not find directory of OpenSSL installation ...
#   Make sure you also have the development packages of openssl installed.
# A *-sys crate is the binding layer for a C library, so its build script dying on
# a missing header / pkg-config file means a MISSING SYSTEM PACKAGE (libssl-dev,
# libfuse3-dev, pkg-config, make) — not a repo defect — and a bare agent has no
# sudo to install one. This predicate is the LOG half ONLY: the caller must ALSO
# require RUST_MISSING_SYS_DEPS (the probe above, which knows what this agent
# lacks) to be non-empty, so a *-sys failure on an agent that HAS the deps is
# still graded FAIL. And exactly as in build_env_failure(), a genuine rustc
# diagnostic (error[E####]) outranks it: code errors keep their FAIL grade.
build_sys_dep_failure() { # <logfile> — 0 when the log carries the missing-system-library shape
  local sf="\$1"
  [ -f "\$sf" ] || return 1
  grep -qE 'failed to run custom build command for [^ ]*-sys([ \`]|\$)|Make sure you also have the development packages' "\$sf" 2>/dev/null || return 1
  ! grep -qE '^error\[E[0-9]{4}\]' "\$sf" 2>/dev/null
}
# build_incomplete — did the build even FINISH? (QA-WARPFS-9 rework 2, 2026-09-19.)
# build_env_failure() above is deliberately NARROW (the bare-toolchain shape only,
# so a genuine rustc diagnostic keeps its FAIL). A build can also die BEFORE the
# chaos condition is reached, for a reason that IS the chaos condition's subject —
# observed live on the same agent: the \`ring\` crate's build script hands the RUST
# target triple straight to the C compiler as
#   --target=x86_64-unknown-linux-gnu
# and zig's target syntax carries no vendor segment, so cc dies with
#   error: unable to parse target query 'x86_64-unknown-linux-gnu': UnknownOperatingSystem
#   error: failed to run custom build command for \`ring v0.17.14\`
#   warning: build failed, waiting for other jobs to finish...          (rc=101)
# Neither shape is a network or memory finding, and neither reached the chaos
# condition, so chaos-disconnect must not grade OK "fails fast on disconnect" and
# chaos-resource must not grade FAIL "rc!=0 under cap" on either of them. This
# predicate is therefore BROAD by design: ANY incomplete build → the two chaos
# cells grade INFO UNVERIFIED / ENV-BLOCKED, whatever the cause (environment OR
# code). It is used ONLY by those two chaos cells; fresh-install keeps
# build_env_failure() so a real compile error stays a repo defect.
# build_incomplete_cause() is the detail-text companion — it prints the first line
# that proves the build never finished, so a cell never has to guess the cause
# from a noisy tail (and the pattern list lives in exactly ONE place). It also
# matches an explicit rustc diagnostic (^error[E####]) because that is the FIRST
# line a compile error prints: the detail then names the real defect instead of
# cargo's summary line further down.
build_incomplete_cause() { # <logfile> — print the line proving the build never finished
  grep -m1 -E 'linker \`cc\` not found|could not compile|failed to run custom build command|warning: build failed, waiting for other jobs|^error\[E[0-9]{4}\]' "\$1" 2>/dev/null | cut -c1-220
}
build_incomplete() { # <logfile> — 0 when the log shows the build never completed
  [ -n "\$(build_incomplete_cause "\$1")" ]
}
# go_suite_env_failure — is this go-suite FAIL the test HOST's missing build
# tool instead of a repo defect? (QA-CRIER-23/QA-CRIER-7, 2026-09-20; measured
# 2026-09-19 on bunker-las-02: cmd/server TestServerVersionCLIFlags "Makefile
# stamps the symbols the binary reads" shells to \`make -n build\`, the JIT
# agent's PATH has no make, and the suite dies with
#   --- FAIL: TestServerVersionCLIFlags ...
#   main_test.go:1358: make -n build: exec: "make": executable file not found in $PATH
# while hosted CI is GREEN at that exact HEAD and the same test PASSES on any
# host with make. A tool-less host says NOTHING about the code, so the two
# chaos cells must not grade this shape and ci-pass must NAME what failed.)
go_suite_env_failure() { # <logfile> — 0 when a go-suite FAIL is explained by exec: "<tool>": executable file not found
  local gf="\$1"
  [ -f "\$gf" ] || return 1
  grep -qE 'exec: "[a-z][a-z-]*": executable file not found in [$]PATH' "\$gf" 2>/dev/null || return 1
  grep -qE -- '--- FAIL|^FAIL' "\$gf" 2>/dev/null || return 1
  true
}
# go_toolchain_oom — did the go TOOLCHAIN itself die building under the memory
# cap? (QA-HERMES-CANOPY-38, 2026-10-04.) On a 3G address-space cap the toolchain
# can fail before any test runs: \`runtime/cgo: SystemResources\` during tool
# bootstrap, or \`cannot allocate memory\` from the compiler. That is the CAP vs
# TOOLCHAIN interaction, not a repo memory finding — grading it FAIL rc!=0
# blamed the code for an environment ceiling.
go_toolchain_oom_cause() { # <logfile> - print the line proving the toolchain failed to build
  grep -m1 -E 'SystemResources|cannot allocate memory|out of memory \(Go toolchain\)|failed to initialize build cache.*address space' "\$1" 2>/dev/null | cut -c1-220
}
go_toolchain_oom() { [ -n "\$(go_toolchain_oom_cause "\$1")" ]; }
# go_panic_timeout — did go test hit its own 10m timeout and PANIC under the cap?
# (QA-HERMES-CANOPY-38.) internal/session on a 3G cap panics 'test timed out
# after 10m0s' instead of failing a package: the suite has NO clean failure mode
# at its worst-case memory ceiling. A real finding, but about the ceiling, not a
# package regression — grade INFO with the panic line as attribution.
go_panic_timeout_cause() { # <logfile> - print the panic-timeout line
  grep -m1 -E 'panic: test timed out' "\$1" 2>/dev/null | cut -c1-220
}
go_panic_timeout() { [ -n "\$(go_panic_timeout_cause "\$1")" ]; }
# capped_fail_summary — a capped-suite FAIL must NAME the failing package.
# (QA-HERMES-CANOPY-37, 2026-10-04.) The old verdict embedded blind tail -3 of
# the raw go-test log; a panic goroutine dump floods the tail and the verdict
# carried no '--- FAIL' / 'panic:' line, making post-destroy attribution
# impossible (the QA-36 lesson, now structural). Extract the FAIL/panic lines
# first; fall back to tail -3 only when the log has neither.
capped_fail_summary() { # <logfile> - FAIL/panic lines, else tail -3
  local s
  s=\$(grep -E -- '--- FAIL|^FAIL|panic: test timed out' "\$1" 2>/dev/null | head -5 | tr '\n' ' ' | cut -c1-500)
  if [ -n "\$s" ]; then printf '%s' "\$s"; else tail -3 "\$1" | tr '\n' ' ' | cut -c1-300; fi
}
# native_runner_missing_cause - did the suite fail to start because the TEST
# RUNNER ITSELF is absent? (QA-TERMINAL-JAIL-9, 2026-09-25.) The harness picks
# native_cmd by repo shape (pytest for pyproject.toml) but cannot guarantee the
# binary exists on the agent: a pyproject repo whose pytest lives in a PEP-735
# dependency-group gets rc=127 'pytest: command not found' from EVERY leg.
# That is the harness's environment gap, not a repo result - the same class as
# build_env_failure/suite_deps_missing: an infra refusal upstream of the suite
# is never a product verdict (terminal-jail 09-23 battery: ci-pass graded FAIL
# on it and chaos cells burned UNVERIFIED rows across 5 ledger cycles).
native_runner_missing_cause() { # <logfile> - print the line proving the runner was absent
  # QA-FOREMAN-2026-09-26: python module-missing form added — a pytest.ini-only
  # repo on a bare agent dies \`/usr/bin/python3: No module named pytest\`, which
  # the command-not-found regex never matched (suite NEVER RAN, must grade
  # UNVERIFIED, not FAIL).
  grep -m1 -E "^(bash: )?(line [0-9]+: )?(pytest|python3?|npm|pnpm|yarn|node|go|cargo|make): (command )?not found|command not found: (pytest|python3?|npm|pnpm|yarn|node|go|cargo|make)|No module named (pytest|nose|unittest2)" "\$1" 2>/dev/null | cut -c1-220
}
native_runner_missing() { # <logfile> - 0 when the log shows the runner never started
  [ -n "\$(native_runner_missing_cause "\$1")" ]
}
# suite_deps_missing — is this suite rc a MISSING-TEST-DEPS refusal instead of a
# product result? (QA-9ROUTER-26, 2026-09-21.) 9router's own \`npm test\` entry
# runs a preflight (scripts/check-test-deps.mjs) that exits 1 with
#   9router: tests/node_modules is missing — vitest cannot be resolved.
# when the independent tests/ package was never installed (tests/package.json
# pins vitest and has its OWN dependency tree). Before the two-step install
# detection (detect_cmds, QA-9ROUTER-26) every repo of that shape hit exactly
# this on a fresh agent, and chaos-disconnect graded the resulting rc=1 as
# OK "fails fast on disconnect" — a FALSE PASS on a suite that NEVER RAN, the
# same class build_incomplete/go_suite_env_failure already handle: an infra
# refusal upstream of the chaos condition is never a product verdict.
suite_deps_missing_cause() { # <logfile> — print the line proving deps were missing
  # FIX (QA-9ROUTER-28 follow-up, 2026-09-22): the old pattern also matched npm's
# own COMMAND-ECHO line ("> node scripts/check-test-deps.mjs && cd tests && ...")
# that npm prints before ANY failure - so EVERY rc=1 npm-suite run graded
# UNVERIFIED "missing test dependencies" and the real failure text never
# reached the verdict. Require a genuine refusal line and exclude the echo.
grep -m1 -E '(tests?/node_modules is missing|vitest cannot be resolved|Cannot find module|npm error)' "\$1" 2>/dev/null | grep -v '^> ' | cut -c1-220
}
suite_deps_missing() { # <logfile> — 0 when the log carries the missing-test-deps signature
  [ -n "\$(suite_deps_missing_cause "\$1")" ]
}
# vacuous_native_suite — did the native leg run ANY suite at all?
# (QA-BUNKER-29, 2026-09-30.) Repos matching no shape fall back to the
# placeholder \`native_cmd="echo no-test-path"\` (detect_cmds): the log holds
# ONLY the echo output, the command exits 0, and BOTH consumers of that
# success — ci-pass ("native suite PASS") and chaos-resource ("suite survives
# 3G memory cap") — graded a suite that NEVER RAN as a product result, six
# ledger cycles in a row. Same class as build_incomplete/suite_deps_missing:
# an infra refusal upstream of the suite is never a product verdict. The log
# is vacuous when it carries the marker itself, or has zero non-whitespace
# content besides the cap-probe lines (PARENT_CAP=/SUBSHELL_CAP=/CAP_EFFECTIVE=)
# that the chaos-resource wrapper adds around the command.
vacuous_native_suite() { # <logfile> -> 0 when the log proves no suite ran
  local f="\$1"
  [ -f "\$f" ] || return 0
  if grep -q 'no-test-path' "\$f"; then return 0; fi
  local rest
  rest=\$(grep -vE '^[[:space:]]*\$|^(PARENT_CAP|SUBSHELL_CAP|CAP_EFFECTIVE)=' "\$f")
  [ -z "\$rest" ]
}
# corr_grade — pure chaos-corruption decision. The caller owns truncation,
# restart, and restoration; this function only grades the observed evidence.
corr_grade() { # <rc> <logfile> <state_file> <ref_note> <tracked_yes_no>
  local rc="\$1" logfile="\$2" state_file="\$3" ref_note="\$4" tracked_yes_no="\$5" crash refusal
  if [ "\$tracked_yes_no" != yes ] && printf '%s' "\$ref_note" | grep -qF 'not referenced by any source file'; then
    printf '%s\t%s\n' INFO "candidate \$state_file is untracked and referenced by no source file — not identifiable as a state store; state-restart behavior UNVERIFIED"
  elif grep -qiE 'panic|traceback \\(most recent|segmentation fault|core dumped|fatal error' "\$logfile"; then
    printf '%s\t%s\n' FAIL "crash on the next start after truncating \$state_file (\$ref_note): \$(grep -m1 -iE 'panic|traceback|segmentation fault|core dumped|fatal error' "\$logfile")"
  elif [ "\$rc" -eq 124 ]; then
    printf '%s\t%s\n' FAIL "next start HANGS (>20s) after truncating \$state_file (\$ref_note): \$(head -2 "\$logfile" | tr '\\n' ' ')"
  elif [ "\$rc" -eq 0 ]; then
    printf '%s\t%s\n' OK "next start unaffected by a truncated \$state_file (rc=0, \$ref_note) — state restored"
  elif grep -qiE 'corrupt|truncat|checksum|malformed|not a valid|unexpected (EOF|end of)' "\$logfile"; then
    printf '%s\t%s\n' OK "clean error on the next start after truncating \$state_file (rc=\$rc, \$ref_note): \$(head -2 "\$logfile" | tr '\\n' ' ')"
  else
    printf '%s\t%s\n' INFO "truncated \$state_file (\$ref_note): next start exited rc=\$rc but the log never mentions corruption — restart behavior UNVERIFIED, likely an unrelated failure: \$(head -2 "\$logfile" | tr '\\n' ' ')"
  fi
}
# ep_grade — pure chaos-errorpath decision. probe_started=yes is required for
# a non-zero exit to be a product result; build/toolchain refusals stay INFO.
ep_grade() { # <rc> <logfile> <probe_started_yes_no>
  local rc="\$1" logfile="\$2" probe_started_yes_no="\$3" refusal
  if [ "\$probe_started_yes_no" = na ]; then
    printf '%s\t%s\n' N/A 'no start command was detected'
  elif grep -qiE '^.*panic|traceback \\(most recent|segmentation fault|core dumped' "\$logfile"; then
    printf '%s\t%s\n' FAIL "crash on the missing-config probe: \$(grep -m1 -iE 'panic|traceback|segmentation fault|core dumped' "\$logfile")"
  elif refusal=\$(grep -m1 -iE 'go\\.mod file not found|cannot find main module|no Go files|not a valid module|build constraints exclude|cannot find package|build failed|fatal error' "\$logfile"); [ -n "\$refusal" ]; then
    printf '%s\t%s\n' INFO "start probe did not run the application (refusal: \$refusal) — missing-config behavior UNVERIFIED"
  elif [ "\$rc" -eq 124 ]; then
    printf '%s\t%s\n' INFO "missing-config start probe timed out (rc=124) — error-path behavior UNVERIFIED: \$(head -2 "\$logfile" | tr '\\n' ' ')"
  elif [ "\$probe_started_yes_no" != yes ]; then
    printf '%s\t%s\n' INFO "start probe did not run the application (probe_started=\$probe_started_yes_no) — missing-config behavior UNVERIFIED: \$(head -2 "\$logfile" | tr '\\n' ' ')"
  elif [ "\$rc" -gt 0 ]; then
    printf '%s\t%s\n' OK "clean error on missing config (rc=\$rc)"
  else
    printf '%s\t%s\n' INFO "unexpected: application started but exited cleanly on missing config (rc=\$rc)"
  fi
}
# ep_probe — run the errorpath probe and report whether the application itself ran.
ep_probe() { # <bin> <ep_dir> <logdir> -> <started>\t<rc>
  local bin="\$1" ep_dir="\$2" logdir="\$3" ep_rc ep_started=no ep_pkg
  mkdir -p "\$ep_dir"
  case "\$bin" in
    "go run "*)
      # The package path is module-relative, so build while cwd is still the repo.
      ep_pkg="\${bin#go run }"
      ( go build -o "\$ep_dir/app" "\$ep_pkg" ) >"\$logdir/ep-build.log" 2>&1; ep_rc=\$?
      if [ \$ep_rc -eq 0 ] && [ -x "\$ep_dir/app" ]; then
        ( cd "\$ep_dir" && timeout 15 "\$ep_dir/app" ) >"\$logdir/ep.log" 2>&1
        ep_rc=\$?
        ep_started=yes
      else
        cp "\$logdir/ep-build.log" "\$logdir/ep.log" 2>/dev/null || :
      fi
      ;;
    *)
      ( cd "\$ep_dir" && timeout 15 \$bin ) >"\$logdir/ep.log" 2>&1
      ep_rc=\$?
      if [ \$ep_rc -ne 127 ] && ! grep -qiE 'go\\.mod file not found|cannot find main module|no Go files|not a valid module|build constraints exclude|cannot find package|build failed|fatal error|cannot find module|module not found|no such file or directory' "\$logdir/ep.log"; then
        ep_started=yes
      fi
      ;;
  esac
  printf '%s\\t%s\\n' "\$ep_started" "\$ep_rc"
}
cd ~/qa-$proj || { echo "no repo dir"; exit 1; }

# 0 · toolchain bootstrap (2026-08-31): temp agents are BARE — no go/node,
#    no sudo (password required), but the home is writable and the agent
#    has internet. Install the toolchain the repo declares into ~/tools
#    (user-space tarballs) BEFORE the cells, so fresh-install tests the
#    project's own install step on a genuinely fresh system.
mkdir -p ~/tools
if [ -f go.mod ] && ! command -v go >/dev/null 2>&1 && [ ! -x ~/tools/go/bin/go ]; then
  ( curl -sL -o ~/tools/go.tgz https://go.dev/dl/go1.26.5.linux-amd64.tar.gz && tar -C ~/tools -xzf ~/tools/go.tgz && rm ~/tools/go.tgz ) >\$LOGD/toolchain.log 2>&1 \
    && cell toolchain-bootstrap OK "go 1.26.5 installed (~/tools/go)" \
    || cell toolchain-bootstrap FAIL "\$(tail -2 \$LOGD/toolchain.log)"
fi
if [ -f package.json ] && ! command -v node >/dev/null 2>&1 && [ ! -x ~/tools/node/bin/node ]; then
  ( curl -sL -o ~/tools/node.tar.xz https://nodejs.org/dist/v22.22.3/node-v22.22.3-linux-x64.tar.xz && tar -C ~/tools -xf ~/tools/node.tar.xz && mv ~/tools/node-v22.22.3-linux-x64 ~/tools/node && rm ~/tools/node.tar.xz ) >\$LOGD/toolchain.log 2>&1 \
    && cell toolchain-bootstrap OK "node v22.22.3 installed (~/tools/node)" \
    || cell toolchain-bootstrap FAIL "\$(tail -2 \$LOGD/toolchain.log)"
fi
if [ -f pyproject.toml ] && [ ! -x ~/tools/venv/bin/python ]; then
  # QA-BUNKER-57 (2026-10-04): a fresh JIT agent's ensurepip can fail (broken
  # venv, no sudo to fix it), leaving NO pip in the venv — the old chain then
  # died at the first `pip install` and four battery cells cascaded rc=127
  # 'pip: command not found' as FAIL/ENV-BLOCKED with zero signal. Verify pip
  # after venv creation and recover (ensurepip retry, then get-pip.py), logging
  # which path was used to venv.log. If pip is still absent the venv itself is
  # still graded honestly (toolchain-bootstrap FAIL names the ensurepip failure)
  # and every pip-dependent cell grades UNVERIFIED via qa_have_pip below.
  if ( python3 -m venv ~/tools/venv ) >\$LOGD/venv.log 2>&1; then
    if "\$HOME/tools/venv/bin/python3" -m pip --version >>\$LOGD/venv.log 2>&1; then
      echo "pip already present in venv (ensurepip path OK)" >>\$LOGD/venv.log
    elif "\$HOME/tools/venv/bin/python3" -m ensurepip --upgrade >>\$LOGD/venv.log 2>&1; then
      echo "pip recovered via python3 -m ensurepip" >>\$LOGD/venv.log
    elif curl -sS https://bootstrap.pypa.io/get-pip.py | "\$HOME/tools/venv/bin/python3" >>\$LOGD/venv.log 2>&1; then
      echo "pip recovered via bootstrap.pypa.io/get-pip.py" >>\$LOGD/venv.log
    else
      echo "pip recovery FAILED (ensurepip + get-pip.py both failed) — pip-dependent cells will grade UNVERIFIED" >>\$LOGD/venv.log
    fi
    if ( "\$HOME/tools/venv/bin/python3" -m pip install --upgrade pip setuptools wheel && "\$HOME/tools/venv/bin/python3" -m pip install pytest ) >>\$LOGD/venv.log 2>&1; then
      cell toolchain-bootstrap OK "python venv ready + pytest (~/tools/venv)"
    else
      cell toolchain-bootstrap FAIL "\$(tail -2 \$LOGD/venv.log)"
    fi
  else
    cell toolchain-bootstrap FAIL "\$(tail -2 \$LOGD/venv.log)"
  fi
fi
# QA-TERMINAL-JAIL-9 (2026-09-25): the harness's own runner detection picks
# pytest -x -q for every pyproject repo, but a pip install -e . install
# never provisions it when the repo declares pytest in a PEP-735
# dependency-groups dev block (pip ignores those) - the native and chaos
# legs then die rc=127 'pytest: command not found' and carry NO repo signal
# (terminal-jail 09-19 + 09-23 batteries). Best-effort top-up for venvs
# created before this fix or on agents where the fresh install could not
# reach the network: offline agents log a WARN and the native-runner guard
# in the ci-pass cell grades the affected legs UNVERIFIED.
if [ -f pyproject.toml ] && [ -x ~/tools/venv/bin/python ] && [ ! -x ~/tools/venv/bin/pytest ]; then
  ( ~/tools/venv/bin/pip install pytest ) >\$LOGD/venv-pytest.log 2>&1 \
    && cell toolchain-bootstrap OK "pytest topped up into ~/tools/venv (QA-TERMINAL-JAIL-9)" \
    || echo "WARN: pytest top-up failed (offline agent?) - native/chaos legs will grade UNVERIFIED 'runner missing': \$(tail -1 \$LOGD/venv-pytest.log 2>/dev/null)" >&2
fi
if [ -f Cargo.toml ] && ! command -v cargo >/dev/null 2>&1 && [ ! -x ~/tools/cargo/bin/cargo ]; then
  ( export CARGO_HOME=~/tools/cargo RUSTUP_HOME=~/tools/rustup; curl -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain stable --profile minimal --no-modify-path ) >\$LOGD/rustup.log 2>&1 \
    && cell toolchain-bootstrap OK "rust stable installed (~/tools/cargo + ~/tools/rustup)" \
    || cell toolchain-bootstrap FAIL "\$(tail -2 \$LOGD/rustup.log)"
fi
[ -x ~/tools/go/bin/go ] && export PATH="\$HOME/tools/go/bin:\$PATH"
[ -x ~/tools/node/bin/node ] && export PATH="\$HOME/tools/node/bin:\$PATH"
[ -x ~/tools/venv/bin/python ] && export PATH="\$HOME/tools/venv/bin:\$PATH"
[ -x ~/tools/cargo/bin/cargo ] && export CARGO_HOME="\$HOME/tools/cargo" RUSTUP_HOME="\$HOME/tools/rustup" && export PATH="\$HOME/tools/cargo/bin:\$PATH"
# C toolchain (QA-WARPFS-9, 2026-09-19; UNCONDITIONAL since QA-H3-14 v2): a user-space C toolchain serves
# BOTH consumers — rust crate build scripts AND the GNU make bootstrap below — so it now runs for every repo,
# not just Cargo.toml ones. Zig ships a self-contained clang + linker and unpacks into ~/tools with no root;
# if the install cannot be done, cells that need a C compiler grade ENV-BLOCKED via build_env_failure()
# instead of blaming the repo.
  if [ ! -x ~/tools/zig/zig ]; then
    ( cd ~/tools && curl -sL -o zig.tar.xz https://ziglang.org/download/0.15.2/zig-x86_64-linux-0.15.2.tar.xz && tar -xJf zig.tar.xz && rm -f zig.tar.xz && { [ -e zig ] || mv zig-x86_64-linux-0.15.2 zig; } ) >\$LOGD/zig.log 2>&1
  fi
  ZIGB=\$(ls -d ~/tools/zig/zig ~/tools/zig*/zig 2>/dev/null | head -1)
  if [ -n "\$ZIGB" ] && [ -x "\$ZIGB" ]; then
    # MCP-008 (2026-10-03): prefer a real gcc/g++ when present — zig-cc cannot
    # link C++ cgo deps (go-duckdb libduckdb.a needs GNU libstdc++; zig ships
    # libc++ and ld.lld rejects it with undefined std::__cxx11/vtable symbols).
    if command -v g++ >/dev/null 2>&1 && command -v gcc >/dev/null 2>&1; then
      mkdir -p ~/tools/bin
      for zt in cc c++ ld ar ranlib; do
        case "\$zt" in
          cc) printf '#!/bin/sh\nexec gcc "\$@"\n' > ~/tools/bin/cc ;;
          c++) printf '#!/bin/sh\nexec g++ "\$@"\n' > ~/tools/bin/c++ ;;
          *) printf '#!/bin/sh\nexec %s "\$@"\n' "\$(command -v \$zt)" > ~/tools/bin/"\$zt" ;;
        esac
        chmod +x ~/tools/bin/"\$zt"
      done
      export PATH="\$HOME/tools/bin:\$PATH"
      export CC=cc CXX=c++
      cell toolchain-bootstrap OK "system gcc/g++ toolchain (~/tools/bin wrappers -> gcc/g++) — preferred over zig-cc for C++ cgo deps (MCP-008)"
    else
    mkdir -p ~/tools/bin
    for zt in cc c++ ld ar ranlib; do
      case "\$zt" in
        cc|c++)
          # QA-WARPFS-9 rework 2 (2026-09-19): cargo/cc-rs hands the RUST target
          # triple to the C compiler as --target=x86_64-unknown-linux-gnu, and
          # zig's target syntax has NO "-unknown-" vendor segment — it answers
          #   error: unable to parse target query 'x86_64-unknown-linux-gnu':
          #          UnknownOperatingSystem
          # so the ring crate's build script dies before the repo builds at all.
          # Translate the --target VALUE only (drop the vendor segment; gnu and
          # musl both), leaving every other argument byte-identical: rotate the
          # positional params one at a time (shift + append) so no argument is
          # ever re-quoted or word-split. Only the joined "--target=<triple>"
          # spelling is handled, because that is what cc-rs emits for a
          # clang-family compiler (the live argv quoted above is verbatim
          # cc-rs output) — and it is the only spelling that CAN be fixed here:
          # measured against real zig 0.15.2, the split form is rejected at the
          # FLAG level ("error: Unknown Clang option: '--target'"), so rewriting
          # the value alone could not rescue it. A stray "--target=..." with a
          # non-gnu/non-musl target is passed through untouched (the sed only
          # matches the "-unknown-linux-" prefix).
          printf '#!/bin/sh\nn=\$#\ni=0\nwhile [ \$i -lt \$n ]; do\n  a=\$1; shift\n  case "\$a" in\n    --target=*) a=\$(printf "%%s" "\$a" | sed "s/-unknown-linux-/-linux-/") ;;\n  esac\n  set -- "\$@" "\$a"\n  i=\$((i+1))\ndone\nexec "%s" %s "\$@"\n' "\$ZIGB" "\$zt" > ~/tools/bin/"\$zt" ;;
        ld)
          # QA-H3-14 v3 (2026-09-21, battery 23ae94ab): bare agents lack binutils
          # entirely and GNU make's ./configure aborts with "no acceptable ld
          # found in $PATH" — ld is NOT a zig subcommand, so it cannot ride the
          # plain-forward *) branch. zig serves lld as \`zig ld.lld\`. Live-proven
          # on the kept battery agent: with this wrapper configure passes,
          # build.sh produces GNU Make 4.4.1, and a toy target runs (TOY_OK).
          printf '#!/bin/sh\nexec "%s" ld.lld "\$@"\n' "\$ZIGB" > ~/tools/bin/"\$zt" ;;
        *)
          # ar/ranlib take no target triple — forwarded verbatim as before.
          printf '#!/bin/sh\nexec "%s" %s "\$@"\n' "\$ZIGB" "\$zt" > ~/tools/bin/"\$zt" ;;
      esac
      chmod +x ~/tools/bin/"\$zt"
    done
    export PATH="\$HOME/tools/bin:\$PATH"
    export CC=cc CXX=c++
    cell toolchain-bootstrap OK "zig 0.15.2 C toolchain installed (~/tools/bin: cc/c++/ld/ar/ranlib wrappers -> ~/tools/zig/zig) — C toolchain for make bootstrap + rust build scripts (EXPERIMENTAL for C++ cgo deps — see MCP-008)"
    fi
  else
    export RUST_NO_CC=1
    cell toolchain-bootstrap INFO "no C toolchain on agent (no sudo) and zig install failed - C-toolchain-dependent cells (make bootstrap, rust build scripts) grade ENV-BLOCKED (\$(tail -2 \$LOGD/zig.log))"
  fi
# QA-H3-14 v2 (2026-09-21, tick h3-2026-09-21-11-07-15): battery 8ff078b2 re-run FALSIFIED v1 — the make
# bootstrap ran BEFORE the zig C-toolchain leg and defaulted CC=${CC:-cc}; on an agent with NEITHER make NOR cc
# configure died instantly and make.log stayed EMPTY (silent). v2: the zig leg moves ABOVE the make bootstrap
# and becomes UNCONDITIONAL (a user-space C toolchain serves make configure AND rust build scripts; re-verified
# locally with cc masked: zig 0.15.2 bootstraps, make 4.4.1 builds a toy target). The make subshell drops its
# CC=${CC:-cc} default — CC arrives from the zig export or the cell grades ENV-BLOCKED with the real cause.
# v3 (same tick, battery 23ae94ab kept-agent autopsy): configure failed on bare agents with "no acceptable ld
# found in \$PATH" (no binutils) — the zig loop now also emits an ld wrapper (zig ld.lld), and configure/build.sh
# output is captured into make.log (the inner >/dev/null redirects made every failure an EMPTY, undiagnosable log).
# v4 (same tick, battery 7561ba71): configure then died on "Something went wrong bootstrapping makefile fragments
# for automatic dependency tracking" — configure wants an already-working make; the make-4.4.1 bootstrap DOES NOT
# need one (build.sh is self-contained, proven locally: make built + toy target OK). So: --disable-dependency-tracking
# plus an exit-code-tolerant configure so build.sh always runs, with configure's rc echoed into make.log.
MAKE_BIN=""
if ! command -v make >/dev/null 2>&1; then
  if [ ! -x ~/tools/make/bin/make ]; then
    mkdir -p ~/tools/make/build
    ( cd ~/tools/make/build && curl -sL -o make.tar.gz '$MAKE_URL' && tar -xzf make.tar.gz && cd make-4.4.1 && { bash ./configure --disable-nls --disable-dependency-tracking || echo "configure rc=\$? — GNU make has no make yet to bootstrap its dependency-tracking fragments; build.sh below does the real bootstrap"; } && bash ./build.sh && mkdir -p ~/tools/make/bin && cp make ~/tools/make/bin/make ) >\$LOGD/make.log 2>&1
  fi
  if [ -x ~/tools/make/bin/make ]; then
    export PATH="\$HOME/tools/make/bin:\$PATH"
    MAKE_BIN=~/tools/make/bin/make
  fi
fi
if command -v make >/dev/null 2>&1; then
  cell toolchain-bootstrap OK "make available (\${MAKE_BIN:+bootstrapped GNU make}\${MAKE_BIN:-preinstalled}) — \$(make --version 2>/dev/null | head -1)"
elif [ -f Makefile ]; then
  cell toolchain-bootstrap INFO "no make on agent and user-space bootstrap failed — make-based cells will grade ENV-BLOCKED (\$(tail -2 \$LOGD/make.log 2>/dev/null | tr '\n' ' '))"
fi
# FIX 1 · system-dep probe (QA-WARPFS-9 rework 3, 2026-09-19): the zig leg above
# supplies a C toolchain, but a rust repo ALSO links against system libraries
# whose DEVELOPMENT files ship in separate packages — and nothing in user space
# substitutes for a missing system package. Observed live on the round-2 agent
# (bare Debian 13, no sudo) once the ring/zig failure was fixed, the build died
# at the next crate that needs headers:
#   error: failed to run custom build command for \`openssl-sys v0.9.117\`
#   Could not find directory of OpenSSL installation ...
#   Make sure you also have the development packages of openssl installed.
# (openssl reaches this repo through git2; hilo-fuse needs libfuse3-dev; both are
# README-documented.) Measured on that agent: make=NO perl=yes pkg-config=NO
# cc=NO gcc=NO. A fully bare agent therefore CANNOT build this class of project,
# so the harness STOPS CHASING and REPORTS: probe the known-required deps, export
# the missing ones, and grade the cells that die on them ENV-BLOCKED with the
# precise list (build_sys_dep_failure() + the fresh-install branch below). This
# list is CONTEXT, never a verdict on its own — the ENV-BLOCKED grade still needs
# an actual *-sys build failure in the log (fuser probes fuse3 first and happily
# falls back to libfuse2, so a missing fuse3 alone proves nothing broke).
RUST_MISSING_SYS_DEPS=""
if [ -f Cargo.toml ]; then
  for sd in make pkg-config; do
    command -v "\$sd" >/dev/null 2>&1 || RUST_MISSING_SYS_DEPS="\${RUST_MISSING_SYS_DEPS:+\$RUST_MISSING_SYS_DEPS }\$sd"
  done
  # openssl/fuse3 are only observable THROUGH pkg-config, so probe them only when
  # the tool itself is present — otherwise the list already says pkg-config, and a
  # missing probe tool must not be reported as a missing library.
  if command -v pkg-config >/dev/null 2>&1; then
    pkg-config --exists openssl >/dev/null 2>&1 || RUST_MISSING_SYS_DEPS="\${RUST_MISSING_SYS_DEPS:+\$RUST_MISSING_SYS_DEPS }openssl(libssl-dev)"
    pkg-config --exists fuse3 >/dev/null 2>&1 || RUST_MISSING_SYS_DEPS="\${RUST_MISSING_SYS_DEPS:+\$RUST_MISSING_SYS_DEPS }fuse3(libfuse3-dev)"
  fi
  if [ -n "\$RUST_MISSING_SYS_DEPS" ]; then
    cell toolchain-bootstrap INFO "bare agent missing system deps: \$RUST_MISSING_SYS_DEPS — rust build cells graded ENV-BLOCKED when they fail on these (no sudo to apt install)"
  fi
fi
export RUST_MISSING_SYS_DEPS
command -v go >/dev/null 2>&1 && command -v node >/dev/null 2>&1 && cell toolchain-bootstrap OK "go+node ready"

# 1 · fresh-install — REAL rc through a log file
# QA-BUNKER-57 (2026-10-04): a pyproject repo's DETECT_INSTALL is pip; on an
# agent where ensurepip failed there is no pip at all and the leg would die
# rc=127 as a false repo FAIL. Grade UNVERIFIED before running anything.
# (QA_ECOSYSTEM is only assigned later, in the upgrade block — test the
# ecosystem here the same way the installer is picked, from $install_cmd.)
if printf '%s' "$install_cmd" | grep -q pip && ! qa_have_pip; then
  cell fresh-install UNVERIFIED "pip absent on agent (ensurepip failed; no sudo) — the install step was never run, so the repo is untested by this cell"
else
( $install_cmd ) >\$LOGD/inst.log 2>&1; inst_rc=\$?
# FIX 2 / QA-WARPFS-9 rework 3 (2026-09-19): the condition above catches the bare
# C toolchain. The OTHER environment failure the live agent produced is a rust
# repo whose crates link a system library (openssl via git2, fuse3 via hilo-fuse):
# a bare agent has no sudo to apt-install libssl-dev/libfuse3-dev/pkg-config, so
# the build dies inside a *-sys build script long after the toolchain is fine.
# ENV-BLOCKED fires only when BOTH halves hold — the probe found missing system
# deps (RUST_MISSING_SYS_DEPS) AND the log carries the *-sys build-script shape —
# and the INFO detail names the missing list so the row is actionable.
if [ \$inst_rc -eq 0 ]; then cell fresh-install OK "\$(tail -1 \$LOGD/inst.log)"
elif [ \$inst_rc -eq 127 ] || grep -qi 'command not found' \$LOGD/inst.log; then cell fresh-install INFO "ENV-BLOCKED: the agent lacks a build tool the install step calls (\$(grep -m1 -oE '[a-zA-Z0-9_.-]+: command not found' \$LOGD/inst.log || echo rc=\$inst_rc)) and has no sudo to install it — harness env, not a repo defect: \$(tail -1 \$LOGD/inst.log)"
elif build_env_failure \$LOGD/inst.log; then cell fresh-install INFO "ENV-BLOCKED: agent has no C toolchain (build-script link failure) — not a repo defect; README documents build-essential"
elif [ -n "\$RUST_MISSING_SYS_DEPS" ] && build_sys_dep_failure \$LOGD/inst.log; then cell fresh-install INFO "ENV-BLOCKED: agent is missing system deps [\$RUST_MISSING_SYS_DEPS] and the build died in a *-sys build script — no sudo to apt install, not a repo defect; README documents the full dependency set: \$( { grep -m1 -E 'failed to run custom build command' \$LOGD/inst.log 2>/dev/null || grep -m1 'development packages' \$LOGD/inst.log 2>/dev/null; } | cut -c1-200 )"
else cell fresh-install FAIL "rc=\$inst_rc: \$(tail -2 \$LOGD/inst.log)"; fi
fi # QA-BUNKER-57 pip-absent gate

# Shared "start the project" command — used by chaos-corruption and
# chaos-errorpath. QA-GITREINS-POC-6/004 (2026-09-17): the old inline detection
# knew only node + go, so a Python CLI — the shape both of those cells care
# about — reported N/A "no single-binary entrypoint" and the corruption probe
# had nothing to start. A console script declared in [project.scripts] IS that
# entrypoint. ⚠️ This block must stay AFTER cell 1: on a fresh agent the console
# script does not exist until the project's own install ran, so detecting
# earlier dropped the value and chaos-corruption reported "no start command was
# detected" on the first live run (bunker-las-02, 2026-09-17). The interpreter
# must also be resolvable, so callers say "not observable" instead of failing
# on a 127.
BIN=""
# QA-9ROUTER-22 (2026-09-21): the old first arm guessed BIN="node src/index.js"
# for EVERY repo with a root package.json, unconditionally — 9router has no
# src/index.js, so chaos-corruption and chaos-errorpath both started
# "Error: Cannot find module .../src/index.js" and graded the refusal as the
# app's verdict (vacuous: it asserts nothing about the app). Detection is now
# OBSERVABLE: a start command is claimed only when the file it names exists in
# the tree (the project's own install has already run at this point), with
# custom-server.js > server.js > start.sh > package.json .scripts.start >
# src/index.js (last resort, only if present) > empty. Empty = "not
# observable", the state the chaos cells already grade INFO/N/A instead of a
# product verdict.
if [ -f package.json ]; then
  if [ -f custom-server.js ]; then BIN="node custom-server.js"
  elif [ -f server.js ]; then BIN="node server.js"
  elif [ -f start.sh ] && [ -x start.sh ]; then BIN="sh start.sh"
  else
    # package.json .scripts.start is a real start command only when it is not
    # an npm-lifecycle no-op (echo/exit/:). A script that is literally
    # "node server.js" is kept in the canonical "node server.js" form the
    # server.js arm above already produces (one form, one interpreter guard).
    BIN=\$(node -e 'const s=(require("./package.json").scripts||{}).start||""; process.exit(s && !/^(echo|exit)\\b|^:\\s*\$/.test(s.trim()) ? 0 : 1)' 2>/dev/null && printf npm_start || true)
    if [ "\$BIN" = "npm_start" ]; then
      if [ "\$(node -e 'process.stdout.write(((require("./package.json").scripts||{}).start||"").trim())' 2>/dev/null)" = "node server.js" ]; then
        BIN="node server.js"
      else
        BIN="npm start"
      fi
    else
      BIN=""
    fi
    # LAST resort: src/index.js only when the file actually exists.
    if [ -z "\$BIN" ] && [ -f src/index.js ]; then BIN="node src/index.js"; fi
  fi
elif [ -f go.mod ]; then
  # QA-OFF-BY-ONE-15 (2026-09-17): the old detection hardcoded BIN="go run ." for
  # any go.mod repo — off-by-one has NO root main package (entrypoint cmd/…), so
  # every start-probe died with "no Go files" and the chaos cells scored the
  # toolchain refusal as the app's verdict. cmd/ wins over a root main because
  # root .go files can exist yet be build-tag-excluded (release-engineer
  # 2026-09-20: "build constraints exclude all Go files" for a root go run).
  gofirst=""
  if [ -d cmd ]; then
    gofirst=\$(grep -rlsE '^package main' cmd --include='*.go' 2>/dev/null | sort | head -1)
  fi
  if [ -n "\$gofirst" ]; then BIN="go run ./\$(dirname "\$gofirst")"
  elif ls ./*.go >/dev/null 2>&1 && grep -qsE '^package main' ./*.go; then BIN="go run ."
  fi
elif [ -f pyproject.toml ]; then
  BIN=\$(grep -A6 '^\[project\.scripts\]' pyproject.toml 2>/dev/null | grep -oE '^[A-Za-z0-9_.-]+[[:space:]]*=' | head -1 | tr -d ' =')
fi
if [ -n "\$BIN" ] && ! command -v \${BIN%% *} >/dev/null 2>&1; then BIN=""; fi

# 2 · ci-pass — act first (simulated GH runner); NATIVE suite is the authoritative
#    fallback (act + rootless dockerd = fatal EOF observed 2026-08-27 on las-03)
#
# QA-9ROUTER-19 (2026-09-23) — TWO false-verdict classes in this cell, both
# recorded as board rows in the graded repo:
#   (1) act ran workflows that CANNOT trigger on the event being simulated. The
#       host now selects only triggerable ones (QA_CI_SELECT) and this cell
#       stages them into an EXTERNAL directory (~/qa-act-wf, outside the synced
#       tree — the tree is the audit payload, so it gains no entry), because act
#       does NOT merge repeated \`-W <file>\` targets: the last one wins and a
#       partial selection would silently drop the workflow that can regress
#       (measured 2026-09-23; a single DIRECTORY target recurses and merges).
#       Nothing triggerable ⇒ act is skipped for the native suite, and
#       QA_ACT_NOTE says why.
#   (2) a step failure inside act caused by the WORKFLOW's own registry/docker
#       coordinates (docker/login-action with no docker in the container, a
#       \`docker pull\` of a private image, a registry 401/403) is not a
#       branch-push regression. Such steps are CLASSIFIED from the log alone
#       (ci_only_failure): the cell leaves the red FAIL band. A workflow that
#       fails for any other reason — a real test failure, a parse error, a
#       shell variable act's own parser rejects — keeps the FAIL band.
command -v ~/bin/act >/dev/null 2>&1 || (mkdir -p ~/bin && cd ~ && curl -sL -o act.tar.gz '$ACT_URL' && tar -xzf act.tar.gz -C ~/bin && rm act.tar.gz)
# Pre-configure act to avoid interactive image selection prompt on first run
mkdir -p ~/.config/act && [ ! -f ~/.config/act/actrc ] && echo "-P ubuntu-latest=catthehacker/ubuntu:act-latest" > ~/.config/act/actrc
# FIX 2 · QA-WARPFS-2-3 (2026-09-19): this pull used to be a single curl —
# no retry, no alternate source — so one external-DNS blip hard-failed the
# bootstrap and the battery graded a transient network failure as if the
# repo were broken (same class as hermes-canopy 2026-09-05: exit 6 "Could
# not resolve host" on the rootless-installer download). Now a logged
# ladder: 3 attempts on the pinned static tarball (backoff 5s/15s), then
# ONE fallback to the docker-rootless-extras tarball on download.docker.com
# — a different artifact that vendors the rootless runtime (rootlesskit,
# vpnkit, dockerd-rootless-setuptool.sh) the docker cells exercise. No
# --resolve / --dns-servers IP pinning by design: the ladder must outlive a
# resolver blip, not depend on a hand-pinned address.
if ! [ -x ~/bin/docker ]; then
  mkdir -p ~/bin
  : > \$LOGD/docker-cli.log
  DOCKER_DL_OK=0
  for att in 1 2 3; do
    echo "== attempt \$att/3: docker-28.5.2.tgz (download.docker.com)" >> \$LOGD/docker-cli.log
    if curl -sSL -o ~/bin/docker.tgz https://download.docker.com/linux/static/stable/x86_64/docker-28.5.2.tgz >> \$LOGD/docker-cli.log 2>&1 && tar -xzf ~/bin/docker.tgz -C ~/bin --strip-components=1 docker/docker >> \$LOGD/docker-cli.log 2>&1; then
      rm -f ~/bin/docker.tgz
      DOCKER_DL_OK=1
      break
    fi
    echo "attempt \$att failed" >> \$LOGD/docker-cli.log
    case \$att in 1) sleep 5 ;; 2) sleep 15 ;; esac
  done
  if [ \$DOCKER_DL_OK -eq 0 ]; then
    echo "== fallback: docker-rootless-extras-28.5.2.tgz (alternate artifact, download.docker.com)" >> \$LOGD/docker-cli.log
    if curl -sSL -o ~/bin/rootless-extras.tgz https://download.docker.com/linux/static/stable/x86_64/docker-rootless-extras-28.5.2.tgz >> \$LOGD/docker-cli.log 2>&1 && tar -xzf ~/bin/rootless-extras.tgz -C ~/bin --strip-components=1 >> \$LOGD/docker-cli.log 2>&1; then
      rm -f ~/bin/rootless-extras.tgz
      chmod +x ~/bin/rootlesskit ~/bin/rootlesskit-docker-proxy ~/bin/vpnkit ~/bin/dockerd-rootless-setuptool.sh 2>/dev/null || true
      echo "fallback extras tarball extracted (rootless runtime vendored; docker CLI itself still absent)" >> \$LOGD/docker-cli.log
    else
      echo "fallback also failed" >> \$LOGD/docker-cli.log
    fi
  fi
  if [ -x ~/bin/docker ]; then
    cell toolchain-bootstrap OK "docker cli installed (~/bin/docker)"
  else
    cell toolchain-bootstrap FAIL "docker toolchain unavailable after 3 attempts + rootless-extras fallback — external download unreachable (DNS/network environment), not a repo defect: \$(tail -2 \$LOGD/docker-cli.log | tr '\n' ' ')"
  fi
fi
# FIX 1 · compose plugin (QA-WARPFS-11, 2026-09-19): the static tarball above
# ships ONLY the docker CLI — the compose plugin is a separate binary — so
# 'docker compose up -d' died rc=125 "unknown shorthand flag: 'd'" on every
# JIT agent and docker-deploy + chaos-shutdown graded FAIL for an environment
# gap instead of a repo defect. Install the plugin user-space into the CLI's
# default discovery dir (~/.docker/cli-plugins), best-effort: an offline
# agent logs a WARNING and continues, it must not hard-fail the bootstrap.
mkdir -p ~/.docker/cli-plugins
if ! [ -x ~/.docker/cli-plugins/docker-compose ]; then
  if curl -fsSL -o ~/.docker/cli-plugins/docker-compose https://github.com/docker/compose/releases/latest/download/docker-compose-linux-x86_64 >\$LOGD/compose.log 2>&1 && head -c 4 ~/.docker/cli-plugins/docker-compose | grep -q ELF && chmod +x ~/.docker/cli-plugins/docker-compose >> \$LOGD/compose.log 2>&1; then
    cell toolchain-bootstrap OK "compose plugin installed (~/.docker/cli-plugins/docker-compose)"
  else
    echo "WARN: compose plugin install failed (offline agent?) — docker compose cells will fail rc=125 on this environment gap: \$(tail -1 \$LOGD/compose.log 2>/dev/null)" >&2
  fi
fi
# FIX 2 · buildx plugin (QA-CHIMERA-V2-32, 2026-09-21): compose v5.5.1's build
# path REQUIRES buildx >= 0.17 — 'docker compose up -d --build' died rc=1
# "compose build requires buildx 0.17.0 or later" on every JIT agent because the
# static docker-28.5.2.tgz ships NEITHER compose NOR buildx (FIX 1 above covers
# compose only). Same best-effort user-space pattern as FIX 1: the plugin goes
# into ~/.docker/cli-plugins (the CLI's default discovery dir); an offline
# agent logs a WARN and continues — the build cells then grade the environment
# gap, not a repo defect.
if ! [ -x ~/.docker/cli-plugins/docker-buildx ]; then
  # Asset name is VERSION-PREFIXED (buildx-vX.Y.Z.linux-amd64) — unlike compose,
  # buildx publishes NO versionless latest/download artifact: that URL 404s with
  # the body "Not Found", and a bare curl -sSL silently saved that body as the
  # plugin (graded OK, compose build then died rc=1 "exec format error" —
  # QA-CHIMERA-V2-32 relaunch, 2026-09-21). Resolve the tag via the GitHub API,
  # curl -f so an HTTP error never lands in the file, and verify the ELF magic
  # before grading the cell OK.
  BX_TAG=\$(curl -fsSL --max-time 20 https://api.github.com/repos/docker/buildx/releases/latest | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4)
  if [ -n "\$BX_TAG" ] && curl -fsSL -o ~/.docker/cli-plugins/docker-buildx "https://github.com/docker/buildx/releases/download/\$BX_TAG/buildx-\$BX_TAG.linux-amd64" >\$LOGD/buildx.log 2>&1 && head -c 4 ~/.docker/cli-plugins/docker-buildx | grep -q ELF && chmod +x ~/.docker/cli-plugins/docker-buildx >> \$LOGD/buildx.log 2>&1; then
    cell toolchain-bootstrap OK "buildx plugin installed (\$(~/.docker/cli-plugins/docker-buildx version 2>/dev/null | head -1 || echo 'version n/a'))"
  else
    echo "WARN: buildx plugin install failed (offline agent?) — compose build cells will fail rc=1 'requires buildx 0.17.0 or later' on this environment gap: \$(tail -1 \$LOGD/buildx.log 2>/dev/null)" >&2
  fi
fi
# FIX 3 · node+npm toolchain bootstrap (QA-HERMES-CANOPY-32, 2026-09-24):
# the JIT agent image ships NO node/npm, so the ui-probe frontend/ arm
# (setsid npm run dev) dies "No such file or directory" on every repo that
# only serves a frontend/ Vite/npm surface. Install user-space Node LTS
# v22.15.0 (proven resolving 2026-09-24) into ~/tools/node, same best-effort
# pattern as FIX 1/FIX 2: offline agents log a WARN and continue.
if ! command -v node npm >/dev/null 2>&1; then
  mkdir -p ~/tools
  if curl -fsSL -o /tmp/node-v22.15.0-linux-x64.tar.xz https://nodejs.org/dist/v22.15.0/node-v22.15.0-linux-x64.tar.xz >\$LOGD/node.log 2>&1 && tar -xJf /tmp/node-v22.15.0-linux-x64.tar.xz -C ~/tools >\$LOGD/node.log 2>&1 && mv ~/tools/node-v22.15.0-linux-x64 ~/tools/node && head -c 4 ~/tools/node/bin/node | grep -q ELF; then
    export PATH="\$HOME/tools/node/bin:\$PATH"
    cell toolchain-bootstrap OK "node+npm installed (\$(node --version 2>/dev/null || echo version n/a))"
  else
    echo "WARN: node+npm install failed (offline agent?) — ui-probe frontend/ arm will grade ENV-BLOCKED: \$(tail -1 \$LOGD/node.log 2>/dev/null)" >&2
  fi
fi
# Bootstrap-end verification (QA-WARPFS-11): a client-side 'docker compose
# version' proves CLI + plugin are discovered together. HARNESS-COMPOSE=ok/missing
# is printed for the run log and recorded as a cell so the evidence shows
# whether a docker gap belongs to the harness or the agent.
HARNESS_COMPOSE=missing
if [ -x ~/bin/docker ] && ~/bin/docker compose version >/dev/null 2>&1; then HARNESS_COMPOSE=ok; fi
echo "HARNESS-COMPOSE=\$HARNESS_COMPOSE"
if [ "\$HARNESS_COMPOSE" = ok ]; then
  cell toolchain-bootstrap OK "HARNESS-COMPOSE=ok (docker compose version passes)"
else
  cell toolchain-bootstrap INFO "HARNESS-COMPOSE=missing — docker compose unavailable (CLI or plugin install failed); docker-deploy/chaos-shutdown will fail on this environment gap, not a repo defect"
fi
# Node+npm bootstrap verification (QA-HERMES-CANOPY-32): confirms the ui-probe
# frontend/ arm can execute. HARNESS_NODE=ok/missing is printed for the run log.
HARNESS_NODE=missing
if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then HARNESS_NODE=ok; fi
echo "HARNESS_NODE=\$HARNESS_NODE"
if [ "\$HARNESS_NODE" = ok ]; then
  cell toolchain-bootstrap OK "HARNESS_NODE=ok (node \$(node --version 2>/dev/null || echo 'n/a'), npm \$(npm --version 2>/dev/null || echo 'n/a'))"
else
  cell toolchain-bootstrap INFO "HARNESS_NODE=missing — node/npm unavailable (install failed or offline); ui-probe frontend/ arm will grade ENV-BLOCKED on this environment gap, not a repo defect"
fi
# QA-MAFIA-AI-BENCHMARK-14 (2026-09-29): pnpm-lock repos rely on \`corepack enable\`
# inside the repo's own install step, but corepack has failed silently on several
# agents (4 consecutive mafia-ai-benchmark cycles: fresh-install/ci-pass native/
# chaos-resource all rc=127 'pnpm: command not found' while HARNESS_NODE=ok).
# Best-effort fallback: npm-install pnpm into the same toolchain prefix npm lives in.
if [ -f pnpm-lock.yaml ] && ! command -v pnpm >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
  if ( npm install -g pnpm --no-audit --no-fund ) >\$LOGD/pnpm.log 2>&1 && command -v pnpm >/dev/null 2>&1; then
    cell toolchain-bootstrap OK "pnpm bootstrapped via npm -g (corepack enable absent/failed)"
  else
    echo "WARN: pnpm bootstrap failed (offline agent?) — pnpm repos will grade ENV-BLOCKED rc=127: \$(tail -1 \$LOGD/pnpm.log 2>/dev/null)" >&2
  fi
fi
# Point docker at the agent's own rootless daemon — bare 'docker' defaults to
# /var/run/docker.sock (rootful, permission-denied for the agent uid) or the
# user-socket guess /run/user/\$UID/docker.sock (proven chimera-v2 run 09-01).
[ -x ~/bin/docker ] && export DOCKER_HOST="unix:///run/bunker/$agent/docker.sock"
for i in \$(seq 1 20); do [ -S /run/bunker/$agent/docker.sock ] && break; sleep 2; done
# CI-only failure classification (QA-9ROUTER-19 class 2). Pure log predicate:
# 0 ⇒ every failure act reported is the workflow's own registry/package
# coordinates (a private image login/pull outside a real runner), i.e. an
# artifact of running the workflow where it cannot really run — never a
# branch-push regression. A plain \`exit 7\`, a parse error, a missing file, a real
# test failure or ANY ❌ block the shape does not cover keeps the red grade.
# The shape pattern lives in ONE place (CI_ARTIFACT_RE); a ❌ line is qualified
# by its own LINE BLOCK (the 8 lines before and 6 after it) because act/docker
# print the reason on either side of the marker: the GHCR login failure puts
# "Unable to locate executable file: docker" BEFORE the ❌, a registry denial
# prints "denied: ..." AFTER it. Both windows are anchored on the ❌ itself, so
# an unrelated artifact-shaped string elsewhere in the log cannot qualify a
# genuine failure.
CI_ARTIFACT_RE="docker/login-action|denied: requested access to the resource is denied|no basic auth credentials|pull access denied|authentication required|not found: manifest|manifest (unknown|not found)|toomanyrequests|Unable to locate executable file: docker|Cannot connect to the Docker daemon|docker daemon is not running|(401 Unauthorized|403 Forbidden)"
ci_only_failure() { # <act log>
  local cf="\$1" n blk from
  [ -f "\$cf" ] || return 1
  n=\$(grep -c '❌' "\$cf" 2>/dev/null || true)
  [ "\${n:-0}" -gt 0 ] || return 1
  while IFS=: read -r lineno _rest; do
    [ -n "\$lineno" ] || continue
    from=\$(( lineno > 8 ? lineno - 8 : 1 ))
    blk=\$(sed -n "\${from},\$((lineno + 6))p" "\$cf" 2>/dev/null)
    printf '%s' "\$blk" | grep -qE "\$CI_ARTIFACT_RE" || return 1
  done < <(grep -n '❌' "\$cf" 2>/dev/null)
  true
}
# QA-FOREMAN-2026-09-26 FIX (use-before-assignment): QA_CI_SELECT/QA_ACT_NOTE
# MUST be assigned BEFORE the staging block below. The generated script
# previously assigned them at the END of the config section, so this loop read
# an UNSET variable, ACT_WF_COUNT was always 0, ci_rc was always "skip", and
# act NEVER RAN for any ci.yml repo — the ci-pass cell graded
# "no triggerable workflow — native suite PASS" vacuously (measured on
# logsey + auger batteries, agents a334d717/c9a0e122, 2026-09-26; corroborated
# by heading + dexdat-memory runs the same day).
QA_CI_SELECT='$DETECT_CI_FILES'
QA_ACT_NOTE='$DETECT_ACT_NOTE'
# Stage the host-selected triggerable workflows into an EXTERNAL staging dir.
# The dir must live OUTSIDE the synced tree (the tree is the payload under audit
# and gains no entries) and hold real FILES, not a dir symlink: \`-W <dir>\` does
# recurse a directory, but act resolves a workflow only when the entry is a file
# it can open (measured 2026-09-23 — a symlinked .github/workflows DIRECTORY
# yielded zero jobs). Symlinked files are fine and act runs the repo's own
# checkout as cwd regardless of where -W points.
ACT_WF_DIR=~/qa-act-wf
rm -rf \$ACT_WF_DIR; mkdir -p \$ACT_WF_DIR
for wf in \$QA_CI_SELECT; do
  [ -f "\$wf" ] && ln -sf "\$PWD/\$wf" "\$ACT_WF_DIR/\$(basename "\$wf")"
done
ACT_WF_COUNT=\$(ls -1 \$ACT_WF_DIR 2>/dev/null | wc -l)
QA_CI_CMD="$ci_cmd"
if [ "\$ACT_WF_COUNT" -gt 0 ]; then QA_CI_CMD="\$QA_CI_CMD -W \$ACT_WF_DIR"; fi
if [ "\$ACT_WF_COUNT" -gt 0 ]; then
( timeout \${BUNKER_QA_ACT_TIMEOUT:-2400} bash -c "\$QA_CI_CMD" ) >\$LOGD/ci.log 2>&1; ci_rc=\$?
else
: > \$LOGD/ci.log
ci_rc=skip   # no triggerable workflow: act never ran, so it has no verdict
fi
#QA-CELL:ci-pass:start
# NOTE: the first test is a STRING compare on purpose — ci_rc is the literal
# "skip" when there was no triggerable workflow, and \`[ skip -eq 0 ]\` makes bash
# print "integer expression expected" and take the wrong branch.
if [ "\$ci_rc" = "0" ]; then
  cell ci-pass OK "act: \$(grep -cE 'Job succeeded|✅ Success' \$LOGD/ci.log || true) jobs green — rc=0 (\$ACT_WF_COUNT triggerable workflow(s))"
else
  suite_start=\$(date +%s)
  ( $native_cmd ) >\$LOGD/native.log 2>&1; nat_rc=\$?
  suite_end=\$(date +%s)
  suite_dur=\$(( suite_end - suite_start ))
  if [ \$nat_rc -eq 0 ]; then
    ACTCTXOK=""; grep -q 'Input required and not supplied: token' \$LOGD/ci.log 2>/dev/null && ACTCTXOK=" (act sandbox has no GITHUB_TOKEN — checkout/setup steps cannot run in act; act leg not repo evidence, hosted CI is authoritative)"
    # QA-BUNKER-29 (2026-09-30): rc=0 from the \`echo no-test-path\` placeholder
    # (or any other empty suite output) is NOT a suite result — nothing ran.
    # Grade UNVERIFIED before the OK, never a false green on a suite that
    # never ran. Non-vacuous rc=0 path is unchanged.
    if vacuous_native_suite \$LOGD/native.log; then
      cell ci-pass UNVERIFIED "native suite NEVER RAN (vacuous no-test-path placeholder) — no repo signal"
    elif [ "\$ci_rc" = "skip" ]; then
      cell ci-pass OK "act: no triggerable workflow\$QA_ACT_NOTE — native suite PASS"
    else
      # QA-CHIMERA-V2-39 (2026-10-04) + QA-BUNKER-B1 (2026-10-06): an act leg
      # that FAILED must NOT be folded into a single OK cell. The 10-04 change
      # split the leg out as ci-act, but still graded ci-pass OK "native suite
      # PASS (see ci-act ...)" — so a CI simulation that died rc=1
      # (level=fatal msg=EOF, las-03 2026-08-31; recurred on batteries since)
      # read OK against its own ci.log. act is this cell's PRIMARY check: a
      # non-zero act rc is a FAIL, and the native suite (which DID run) is
      # carried as detail only — never a status flipper. act SKIPPED entirely
      # (no triggerable workflow) is not a failed act and keeps the
      # native-suite OK. The ci-act row below keeps the act leg's own
      # UNVERIFIED detail; it is never graded OK either.
      if [ "\$ci_rc" = "skip" ]; then
        cell ci-pass OK "act: no triggerable workflow\$QA_ACT_NOTE — native suite PASS"
      else
        cell ci-pass FAIL "act rc=\$ci_rc: \$(act_failure_context \$LOGD/ci.log)\$ACTCTXOK — act CI simulation FAILED (native suite PASS is detail only)"
        cell ci-act UNVERIFIED "act failed rc=\$ci_rc: \$(act_failure_context \$LOGD/ci.log)\$ACTCTXOK — a failed act leg is never graded OK; hosted CI is authoritative for the workflow verdict"
      fi
    fi
  else
    NCTX=""; build_env_failure \$LOGD/native.log && NCTX=" (environment: no C toolchain)"
    NFAIL="\$(grep -v '^[[:space:]]*\$' \$LOGD/native.log 2>/dev/null | grep -E -- '^(--- FAIL|FAIL|ok  )' | tail -8 | tr '\n' '|')"
    [ -n "\$NFAIL" ] || NFAIL="\$(grep -v '^[[:space:]]*\$' \$LOGD/native.log 2>/dev/null | tail -4 | tr '\n' '|')"
    GOCTX=""; go_suite_env_failure \$LOGD/native.log && GOCTX=" (environment: missing build tool on the test host — not a code finding; FAIL lines below name the real failures)"
    # QA-H3-15 (2026-09-21): act inside the sandbox has NO GITHUB_TOKEN, so
    # actions/checkout@v4 (and setup-*) die with "Input required and not
    # supplied: token" BEFORE the suite runs — the act leg then says nothing
    # about the repo (hosted CI on the same HEAD is green; h3 battery 24dcbb04:
    # act rc=1 checkout-token + native rc=127 make-missing, both harness env).
    # Name that context in the FAIL detail so nobody files it as a repo defect.
    ACTCTX=""; grep -q 'Input required and not supplied: token' \$LOGD/ci.log 2>/dev/null && ACTCTX=" (act sandbox has no GITHUB_TOKEN — checkout/setup steps cannot run in act; act leg not repo evidence, hosted CI is authoritative)"
    if native_runner_missing \$LOGD/native.log; then
      # QA-TERMINAL-JAIL-9 (2026-09-25): rc=127 '<runner>: command not found' -
      # the suite NEVER RAN, so this leg carries no repo signal. The harness
      # picked the runner by repo shape but could not provision it (PEP-735
      # dependency-group repos); grade the environment gap, not a repo defect.
      cell ci-pass UNVERIFIED "native runner missing (rc=\$nat_rc) - the suite NEVER RAN, no repo signal; INFRA-FAIL, not a result: \$(native_runner_missing_cause \$LOGD/native.log)"
    elif [ "\$ci_rc" = "skip" ]; then
      cell ci-pass FAIL "no triggerable workflow\$QA_ACT_NOTE — native rc=\$nat_rc: \$NFAIL\$NCTX\$GOCTX"
    elif ci_only_failure \$LOGD/ci.log; then
      # QA-9ROUTER-19 class 2: the act leg's ONLY failures are the workflow's own
      # registry/package coordinates (a private image login/pull outside a real
      # runner) — never a branch-push regression. Grade it INFO with the reason
      # named, so the row is informational instead of a false product FAIL.
      cell ci-pass INFO "act's failures are the workflow's own registry/package coordinates, not a branch-push regression (\$(act_failure_context \$LOGD/ci.log)) — native rc=\$nat_rc: \$NFAIL\$NCTX\$GOCTX\$ACTCTX"
    else
      cell ci-pass FAIL "native rc=\$nat_rc: \$NFAIL\$NCTX\$GOCTX\$ACTCTX (act leg rc=\$ci_rc: \$(act_failure_context \$LOGD/ci.log))"
    fi
  fi
fi
#QA-CELL:ci-pass:end

# QA-DEXDAT-CORE-15 (2026-10-04): act compose legs leak services that bind
# PUBLISHED host ports (e.g. a repo's CI stack binding 0.0.0.0:5435->5432).
# Every later compose leg (docker-deploy, ui-probe, chaos-shutdown) then dies
# 'Bind for 127.0.0.1:5435 failed: port is already allocated' — an inherited
# harness root cause graded as product FAILs (proven dexdat-core agent
# f960e4c7 @ 51a376b: act container held 5435 while dexdat-postgres tried to
# bind). Teardown AFTER the ci-pass leg, BEFORE upgrade/docker-deploy/chaos:
# stop+rm the act-stamped containers and any network it created. Scoped to the
# act runner's labels (com.docker.compose.project=act-*, and any container
# whose name starts with 'act-') so a repo's own freshly-upped stack is never
# touched; best-effort — a teardown failure logs and continues.
if [ -x \$HOME/bin/docker ] && [ -n "\${DOCKER_HOST:-}" ]; then
  # QA-DEXDAT-CORE-16 (2026-10-04): act CI legs leak containers that have NO
  # compose label (act names them act-<Job>-<id>) but still PUBLISH host ports
  # (e.g. 127.0.0.1:6379 from an act redis) - the label-only sweep missed them
  # and the docker-deploy leg died 'Bind for 127.0.0.1:6379 failed: port is
  # already allocated' (agent 5e0f55e0 @ 854726d, dc.log:3020). Sweep BOTH
  # classes: label-bearing compose leftovers AND anything named act-*,
  # running or exited, before the deploy legs start.
  ACT_LEFT=\$( { \$HOME/bin/docker ps -aq --filter 'label=com.docker.compose.project' 2>/dev/null;
                  \$HOME/bin/docker ps -aq --filter 'name=^act-' 2>/dev/null; } \
    | sort -u \
    | while IFS= read -r cid; do
        [ -n "\$cid" ] || continue
        if \$HOME/bin/docker rm -f "\$cid" >/dev/null 2>&1; then echo "teardown: rm battery container \$cid"; fi
      done)
  if [ -n "\$ACT_LEFT" ]; then
    echo "QA-DEXDAT-CORE-15 teardown: removed compose leftovers before the docker legs:"
    echo "\$ACT_LEFT"
  fi
  ACT_NETS=\$(\$HOME/bin/docker network ls --format '{{.Name}}' 2>/dev/null | grep -vE '^(bridge|host|none)$' || true)
  for net in \$ACT_NETS; do \$HOME/bin/docker network rm "\$net" >/dev/null 2>&1 || true; done
fi

# 3 · upgrade — previous release → HEAD reinstall
# QA-GITREINS-POC-004: the synced tree carries no .git history (sync_repo
# excludes .git and re-inits one synthetic commit), so an in-cell 'git tag' is
# ALWAYS empty and the old cell reported N/A "no release tags in repo" for repos
# with 40 tags. Tags are detected on the HOST (detect_upgrade_inputs) and baked
# in below; the cell then installs the previous release and re-installs HEAD over
# it. If no previous release can be installed the cell FAILs with the reason —
# never a silent N/A.
QA_TAG_COUNT='$DETECT_TAG_COUNT'
QA_PREV_TAG='$DETECT_PREV_TAG'
QA_PREV_VERSION='$DETECT_PREV_VERSION'
QA_PKG_NAME='$DETECT_PKG_NAME'
QA_ECOSYSTEM='$DETECT_PKG_ECOSYSTEM'
# QA-HEADING-009: non-empty for non-publishable npm repos -> the upgrade cell
# uses the streamed previous-tag tree instead of the npm registry arm.
QA_UP_PREV_DIR='$DETECT_UP_PREV_DIR'
QA_GO_URL='$DETECT_GO_URL'
# QA-9ROUTER-19: workflows that CAN fire on a push to the branch this checkout
# sits on (host-selected by on:-block triggerability, repo-relative, may be
# empty = "no branch-push CI here"). QA_ACT_NOTE carries the reason when the
# selection is empty and act is therefore skipped; the ci-pass cell classifies
# act failures from the LOG alone (no network call, no baked credentials).
# (QA_CI_SELECT/QA_ACT_NOTE now assigned ABOVE the staging block — use-before-
# assignment fix QA-FOREMAN-2026-09-26; the duplicate assignment that used to
# sit here landed AFTER the loop consumed them.)
TREE_TAG=\$(git tag 2>/dev/null | sort -V | tail -2 | head -1)
# QA-OFF-BY-ONE-22: materialize the previous release's tree for Go-binary
# repos — the HOST streams it (ship_prev_tag_tree -> ~/qa-up-prev); if that
# is missing, try a clone of the baked origin URL (works only for public
# URLs — bunker agents carry no clone auth).
UP_PREV=""
if [ "\$QA_TAG_COUNT" != "0" ] && [ "\$QA_ECOSYSTEM" = "go" ] && [ -n "\$QA_PREV_TAG" ]; then
  if [ -f ~/qa-up-prev/go.mod ]; then UP_PREV=~/qa-up-prev
  elif [ -n "\$QA_GO_URL" ] && git clone -q --no-checkout "\$QA_GO_URL" ~/qa-up-prev-clone 2>/dev/null && git -C ~/qa-up-prev-clone checkout -q --detach "\$QA_PREV_TAG" 2>/dev/null; then
    mv ~/qa-up-prev-clone ~/qa-up-prev && UP_PREV=~/qa-up-prev
  else
    rm -rf ~/qa-up-prev-clone 2>/dev/null
  fi
elif [ "\$QA_TAG_COUNT" != "0" ] && [ "\$QA_ECOSYSTEM" = "rust" ] && [ -n "\$QA_PREV_TAG" ]; then
  # QA-WARPFS-13 (2026-09-26): ship_prev_tag_tree streams the rust arm
  # (~/qa-up-prev, verified by Cargo.toml) but this cell only consumed it for
  # go/npm — every tagged rust repo fell through to the generic "no
  # installable previous release" FAIL. Consume the streamed tree (cargo has
  # no installable index package; the tree-stream is the only real
  # previous->HEAD upgrade path for rust).
  if [ -f "\$HOME/qa-up-prev/Cargo.toml" ]; then UP_PREV="\$HOME/qa-up-prev"; fi
elif [ "\$QA_TAG_COUNT" != "0" ] && [ "\$QA_ECOSYSTEM" = "npm" ] && [ -n "\$QA_UP_PREV_DIR" ] && [ -n "\$QA_PREV_TAG" ]; then
  # QA-HEADING-009: host-streamed previous-tag tree (~/qa-up-prev-npm) for
  # non-publishable npm repos — verified by package.json, no clone fallback
  # (bunker agents carry no forge auth; the host stream is the only source).
  if [ -f "\$HOME/qa-up-prev-npm/package.json" ]; then UP_PREV="\$HOME/qa-up-prev-npm"; fi
fi
if [ -n "\$TREE_TAG" ]; then
  git checkout -q --detach "\$TREE_TAG" 2>/dev/null && ( $install_cmd ) >\$LOGD/up1.log 2>&1 && git checkout -q --detach HEAD 2>/dev/null && ( $install_cmd ) >\$LOGD/up2.log 2>&1 && cell upgrade OK "tag \$TREE_TAG → HEAD reinstall clean (in-tree history)" || cell upgrade FAIL "\$(tail -2 \$LOGD/up1.log \$LOGD/up2.log | head -3)"
elif [ "\$QA_TAG_COUNT" = "0" ]; then cell upgrade N/A "no release tags in repo"
elif [ -n "\$UP_PREV" ]; then
  # Go-binary upgrade: install the previous release's tree, then reinstall
  # HEAD over it — a real previous->HEAD upgrade of the working tree.
  cp -a "\$UP_PREV" ~/qa-up-prev.bak 2>/dev/null || true
  ( cd "\$UP_PREV" && $install_cmd ) >\$LOGD/up1.log 2>&1; up1_rc=\$?
  ( $install_cmd ) >\$LOGD/up2.log 2>&1; up2_rc=\$?
  rm -rf ~/qa-up-prev ~/qa-up-prev.bak ~/qa-up-prev-clone ~/qa-up-prev-npm ~/qa-up-prev-npm.bak 2>/dev/null
  if [ \$up1_rc -eq 0 ] && [ \$up2_rc -eq 0 ]; then cell upgrade OK "previous release \$QA_PREV_TAG (tree via git archive) → HEAD reinstall clean"
  elif [ \$up1_rc -ne 0 ]; then cell upgrade FAIL "previous release \$QA_PREV_TAG would not build (streamed tree): \$(fail_detail \$up1_rc \$LOGD/up1.log $install_cmd)"
  else cell upgrade FAIL "upgrade from \$QA_PREV_TAG (streamed tree) to HEAD failed: \$(fail_detail \$up2_rc \$LOGD/up2.log $install_cmd)"; fi
elif [ "\$QA_ECOSYSTEM" = "go" ]; then
  cell upgrade FAIL "upgrade path UNVERIFIED: repo has \$QA_TAG_COUNT release tag(s), previous \$QA_PREV_TAG, but the host's streamed previous-release tree is missing (~/qa-up-prev) and \$QA_GO_URL is not cloneable from the agent (bunker agents carry no clone auth) — re-run the battery so ship_prev_tag_tree can stream it"
elif [ "\$QA_ECOSYSTEM" = "pip" ] && [ -n "\$QA_PKG_NAME" ] && [ -n "\$QA_PREV_VERSION" ]; then
  # QA-BUNKER-57 (2026-10-04): no pip on the agent ⇒ the upgrade leg would die
  # rc=127 'pip: command not found' and grade FAIL against the repo. UNVERIFIED.
  if ! qa_have_pip; then
    cell upgrade UNVERIFIED "pip absent on agent (ensurepip failed; no sudo) — previous-release install \$QA_PKG_NAME==\$QA_PREV_VERSION never ran, so the upgrade path is untested by this cell"
  else
  ( pip install "\$QA_PKG_NAME==\$QA_PREV_VERSION" ) >\$LOGD/up1.log 2>&1; up1_rc=\$?
  ( $install_cmd ) >\$LOGD/up2.log 2>&1; up2_rc=\$?
  if [ \$up1_rc -eq 0 ] && [ \$up2_rc -eq 0 ]; then cell upgrade OK "pip \$QA_PKG_NAME==\$QA_PREV_VERSION (tag \$QA_PREV_TAG, the previous release) → HEAD install clean"
  elif [ \$up1_rc -ne 0 ]; then cell upgrade FAIL "previous release \$QA_PKG_NAME==\$QA_PREV_VERSION would not install (tag \$QA_PREV_TAG): \$(fail_detail \$up1_rc \$LOGD/up1.log "pip install \$QA_PKG_NAME==\$QA_PREV_VERSION")"
  else cell upgrade FAIL "upgrade install from \$QA_PKG_NAME==\$QA_PREV_VERSION to HEAD failed: \$(fail_detail \$up2_rc \$LOGD/up2.log "$install_cmd")"; fi
  fi
elif [ "\$QA_ECOSYSTEM" = "npm" ] && [ -n "\$QA_PKG_NAME" ] && [ -n "\$QA_PREV_VERSION" ]; then
  ( npm install -g "\$QA_PKG_NAME@\$QA_PREV_VERSION" ) >\$LOGD/up1.log 2>&1; up1_rc=\$?
  ( $install_cmd ) >\$LOGD/up2.log 2>&1; up2_rc=\$?
  if [ \$up1_rc -eq 0 ] && [ \$up2_rc -eq 0 ]; then cell upgrade OK "npm -g \$QA_PKG_NAME@\$QA_PREV_VERSION (tag \$QA_PREV_TAG, the previous release) → HEAD install clean"
  elif [ \$up1_rc -ne 0 ]; then cell upgrade FAIL "previous release \$QA_PKG_NAME@\$QA_PREV_VERSION would not install (tag \$QA_PREV_TAG): \$(fail_detail \$up1_rc \$LOGD/up1.log "npm install -g \$QA_PKG_NAME@\$QA_PREV_VERSION")"
  else cell upgrade FAIL "upgrade install from \$QA_PKG_NAME@\$QA_PREV_VERSION to HEAD failed: \$(fail_detail \$up2_rc \$LOGD/up2.log "$install_cmd")"; fi
else cell upgrade FAIL "upgrade path UNVERIFIED: the repo has \$QA_TAG_COUNT release tag(s) (previous \$QA_PREV_TAG) but the synced tree carries no history to check out and no installable previous release was detected (ecosystem='\$QA_ECOSYSTEM' package='\$QA_PKG_NAME') — treat the cell as untested, not as not-applicable"; fi

# 4 · docker-deploy — compose up + probe + down
# ACTIONABLE-FAILURE (QA-CHIMERA-V2-8, 2026-09-14): the FAIL branch used to emit
# 'tail -3 dc.log', which for a docker CLI failure is its generic footer
# ("For more help on how to use Docker, head to https://docs.docker.com/go/guides/")
# — docker prints the REAL error FIRST and the footer LAST, so the cell read FAIL
# with no diagnosis and the deployment stayed unverified. FAIL now names the
# command, its rc, and the log's leading lines via fail_detail().
if ls docker-compose*.yml compose*.yml >/dev/null 2>&1 || ls docker-compose*.yml 2>/dev/null | grep -q . || ls compose*.yml 2>/dev/null | grep -q .; then
  DC_CMD="docker compose up -d --build"
  ( \$DC_CMD ) >\$LOGD/dc.log 2>&1; dc_rc=\$?
  if [ \$dc_rc -eq 0 ]; then
    # QA-HERMES-CANOPY-31 (2026-09-24 evidence: single-shot probe read 000
    # while the server was booting; deploy was actually GOOD). Poll each mapped
    # host port for up to \${BUNKER_QA_DEPLOY_PROBE_TIMEOUT:-60}s, first
    # non-000 answer wins.
    HP=""
    # QA-HEADING-011 part 2 (2026-09-25, measured on this repo): the deadline
    # was computed ONCE for all ports, and docker-compose*.yml globs in the
    # PROD file too (prod's :3000 leads alphabetically) — a dead first port
    # burned the whole 60s before the live api :4000 was ever probed, and the
    # healthy deploy still graded 'no HTTP response'. Allocate the FULL window
    # PER PORT (reset inside the loop); the OK path finds a live port in
    # seconds, the worst case is bounded at 3x the window.
    for p in \$( { grep -hoE '"[0-9]+:[0-9]+"' docker-compose*.yml compose*.yml 2>/dev/null; grep -hoE '[[:space:]][0-9]+:[0-9]+' docker-compose*.yml compose*.yml 2>/dev/null; } | grep -oE '[0-9]+:[0-9]+' | awk '!seen[$0]++' | head -3 | cut -d: -f1 ); do
      DEADLINE=\$((\$(date +%s) + \${BUNKER_QA_DEPLOY_PROBE_TIMEOUT:-60}))
      while [ \$(date +%s) -lt \$DEADLINE ]; do
        HP=\$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:\$p/ 2>/dev/null)
        [ "\$HP" != "000" ] && break
        sleep 3
      done
      [ -n "\$HP" ] && [ "\$HP" != "000" ] && break
    done
    case "\$HP" in 200|301|302) cell docker-deploy OK "compose up + probe \$HP" ;; [0-9]*) cell docker-deploy INFO "compose up OK, probe \$HP" ;; *) cell docker-deploy INFO "compose up OK, no HTTP response within \${BUNKER_QA_DEPLOY_PROBE_TIMEOUT:-60}s (probe deadline exhausted)" ;; esac
    docker compose down >/dev/null 2>&1
  else cell docker-deploy FAIL "\$(fail_detail \$dc_rc \$LOGD/dc.log \$DC_CMD)"; fi
else cell docker-deploy N/A "no compose file"; fi

# 5 · ui-probe
# FIX · ui-probe Go-served HTML (QA-CRIER-19, 2026-09-21): the gate keyed only on a
# package.json dev/start script, so a Go repo serving an HTML docs surface (crier:
# /docs on :8767) was graded N/A "no web UI detected" on every cycle — a false
# negative: the artifact exists, the gate could not see it. Now: no npm UI detected
# -> start the repo's own \$BIN (detected above) and probe a port list for 200
# text/html before N/A is accepted (\${BUNKER_QA_UI_PORTS:-3000 5173 8000 8080 8081
# 8767 9000}). Measured on crier 2026-09-21: the upstream cmd/ scan resolves BIN to
# go run ./cmd/crier-mcp (alphabetically first; a stdio server that exits on stdin
# EOF), NOT ./cmd/server — so when the detected BIN exits before any port answers
# and further cmd/ main packages exist, the arm retries each of them once before it
# accepts not-a-server; and it polls /docs before / because crier's / is 404 JSON.
# Teardown kills the whole process GROUP (setsid): go run compiles and runs a
# child binary, and killing the wrapper alone orphans the real listener.
# SHARED-BOX GUARD (measured 2026-09-21): the fleet box already serves HTML on
# Vite's default :5173 (canopy-vite), so ports occupied BEFORE the start are
# excluded from attribution AND from fuser teardown — a first-HTML-wins poll
# credited the foreign server to the repo under test, and a bare fuser -k on
# an answering port kills unrelated socket-holders (measured: it killed a
# process that merely HELD A CONNECTION to :5173).
if [ -f package.json ] && grep -qE '"dev"|"start"' package.json; then
  # QA-9ROUTER-21 (2026-09-21): the old probe curled / with NO -L and accepted
  # only 200, so any redirect-first UI graded a guaranteed false FAIL —
  # 9router serves / -> 307 /dashboard -> 307 /login -> 200 (auth-guard
  # redirect-first routing is the app's normal shape) and the cell recorded
  # ui-probe FAIL "UI not serving (http=307)" while the UI was up the whole
  # time. Now: follow the redirect chain (-L), accept ANY 2xx (a served page
  # is a served page; the code is recorded), and keep the real negative —
  # 4xx/5xx/000 still FAIL, naming the code AND the landing path.
  # Teardown fix in the same cell: the old bare \`pkill -f 'npm run dev'\`
  # matched by command line (it once killed an UNRELATED process whose argv
  # merely contained the pattern — the frontend/ arm below documents the live
  # incident) and still LEAKED the node listener. Replaced with the arm's own
  # PID + process-GROUP kill (custom-server style wrappers fork a child, so
  # the group is what dies together), with the fuser sweep as the catch-all.
  ( npm run dev -- --port 3111 >\$LOGD/ui.log 2>&1 & ); sleep 5
  UI_NPM_PID=\$(pgrep -f 'npm run dev' | head -1)
  # QA-HEADING-015 (2026-10-04): the old single-shot curl after sleep 15 lost
  # the race against next dev's first compile (~6-9s warm-up) — the probe
  # fired before the listener was bound and graded http=000 while ui.log
  # proved the server answered GET / 200 shortly after. Now: poll up to 30s
  # (matching the go-native arm's 20-iteration pattern) before accepting 000.
  UP="000"; LAND=""
  for _ui_i in \$(seq 1 30); do
    kill -0 "\$UI_NPM_PID" 2>/dev/null || break
    UP=\$(curl -sL -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1:3111/ 2>/dev/null)
    [ "\$UP" != "000" ] && break
    sleep 1
  done
  LAND=\$(curl -sL -o /dev/null -w '%{url_effective}' --max-time 5 http://127.0.0.1:3111/ 2>/dev/null)
  UI_NPM_PGID=\$(ps -o pgid= -p \$UI_NPM_PID 2>/dev/null | tr -d ' ')
  if [ -n "\$UI_NPM_PGID" ] && [ "\$UI_NPM_PGID" = "\$UI_NPM_PID" ]; then
    kill -- -\$UI_NPM_PID 2>/dev/null || true; sleep 1; kill -9 -- -\$UI_NPM_PID 2>/dev/null || true
  fi
  kill \$UI_NPM_PID 2>/dev/null || true; sleep 1; kill -9 \$UI_NPM_PID 2>/dev/null || true
  fuser -k 3111/tcp >/dev/null 2>&1 || true
  case "\$UP" in
    2??) cell ui-probe OK "UI serves \$UP on :3111 (landed: \${LAND:-/})" ;;
    *) cell ui-probe FAIL "UI not serving (http=\$UP\${LAND:+, landed: \$LAND}): \$(tail -2 \$LOGD/ui.log)" ;;
  esac
elif [ -f frontend/package.json ] && grep -qE '"dev"|"start"' frontend/package.json; then
  # FIX · ui-probe frontend/ (QA-HERMES-CANOPY-9, 2026-09-21): a repo whose UI
  # is a Vite/npm app under frontend/ (NO root package.json) was graded N/A
  # "no web UI detected" on every cycle — the shipped user surface never probed
  # (hermes-canopy: 4 consecutive cycles, frontend/package.json + vite present,
  # canopy-vite serving 200 on :5173 the whole time). Mirrors the root-npm arm
  # but rooted at frontend/. Teardown is setsid + PROCESS-GROUP kill, measured
  # on this box 2026-09-21: the root arm's bare pkill -f 'npm run dev' LEAKS
  # the node listener (probe still 200 after teardown), and a blanket
  # a pkill -f 'node .*vite' safety net is worse — it killed the UNRELATED
  # canopy-vite dev server on :5173 (argv merely contains "vite"). Group kill
  # guard: only kill -PGID when pgid == pid (the setsid leader case); the
  # fuser sweep on the probe port is the catch-all.
  # QA-HERMES-CANOPY-32 (2026-09-24 evidence: JIT agent image ships NO node/npm;
  # frontend arm died "No such file or directory" while the repo was fine).
  # Availability gate: when npm is absent the arm grades ENV-BLOCKED and skips
  # execution — when npm IS present but the UI does not serve, FAIL stays as-is.
  if ! command -v npm >/dev/null 2>&1; then
    cell ui-probe INFO "ENV-BLOCKED: node/npm unavailable on agent — frontend arm skipped (install failed or offline)"
  else
    if [ ! -d frontend/node_modules ]; then
      npm ci --prefix frontend --ignore-scripts --no-audit --no-fund >>\$LOGD/ui.log 2>&1 || npm install --prefix frontend --ignore-scripts --no-audit --no-fund >>\$LOGD/ui.log 2>&1 || true
    fi
    ( cd frontend && setsid npm run dev -- --port 3111 >\$LOGD/ui.log 2>&1 & )
    sleep 5
    UI_NPM_PID=\$(pgrep -f 'npm run dev' | head -1)
    # QA-HEADING-015 (2026-10-04): same race as the root-npm arm — poll up to 30s
    UP="000"; LAND=""
    for _ui_i in \$(seq 1 30); do
      kill -0 "\$UI_NPM_PID" 2>/dev/null || break
      UP=\$(curl -sL -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1:3111/ 2>/dev/null)
      [ "\$UP" != "000" ] && break
      sleep 1
    done
    LAND=\$(curl -sL -o /dev/null -w '%{url_effective}' --max-time 5 http://127.0.0.1:3111/ 2>/dev/null)
    UI_NPM_PGID=\$(ps -o pgid= -p \$UI_NPM_PID 2>/dev/null | tr -d ' ')
    if [ -n "\$UI_NPM_PGID" ] && [ "\$UI_NPM_PGID" = "\$UI_NPM_PID" ]; then
      kill -- -\$UI_NPM_PID 2>/dev/null || true; sleep 1; kill -9 -- -\$UI_NPM_PID 2>/dev/null || true
    fi
    kill \$UI_NPM_PID 2>/dev/null || true; sleep 1; kill -9 \$UI_NPM_PID 2>/dev/null || true
    fuser -k 3111/tcp >/dev/null 2>&1 || true
    case "\$UP" in
      2??) cell ui-probe OK "UI serves \$UP on :3111 (frontend/, landed: \${LAND:-/})" ;;
      *) cell ui-probe FAIL "frontend/ UI not serving (http=\$UP\${LAND:+, landed: \$LAND}): \$(tail -2 \$LOGD/ui.log)" ;;
    esac
  fi
elif [ -f go.mod ] && grep -qE 'http\.ListenAndServe|http\.Server|chi\.NewRouter|mux\.NewRouter|gin\.Default|echo\.New|gorilla/mux' *.go 2>/dev/null; then
  UI_PORTS=${BUNKER_QA_UI_PORTS:-3000 5173 8000 8080 8081 8766 8767 9000}
  UI_BINS="${BIN:-go run .}"
  case "${BIN:-}" in
    "go run ./cmd/"*)
      # start-cmd fallback queue: when the detected cmd/ binary is not the HTTP
      # server (see comment above), the remaining cmd/ main packages get one
      # bounded try each before the cell accepts not-a-server.
      for c in \$(grep -rlsE '^package main' cmd --include='*.go' 2>/dev/null | sort | sed 's|/[^/]*\$||' | sort -u); do
        [ "go run ./\$c" = "\$BIN" ] || UI_BINS="\$UI_BINS|go run ./\$c"
      done
      ;;
  esac
  IFS='|' read -r -a UI_LIST <<< "\$UI_BINS"
  # Baseline: which probed ports answer BEFORE we start anything — those are
  # somebody else's and are skipped for both attribution and teardown below.
  UI_OCCUPIED=" "
  for p in \$UI_PORTS; do
    UI_C=\$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 "http://127.0.0.1:\$p/" 2>/dev/null)
    [ "\$UI_C" = "000" ] && UI_C=\$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 "http://127.0.0.1:\$p/docs" 2>/dev/null)
    if [ -n "\$UI_C" ] && [ "\$UI_C" != "000" ]; then UI_OCCUPIED="\$UI_OCCUPIED\$p "; fi
  done
  UI_HIT=""; UI_OTHER=""; UI_PROBED=" "
  for UI_B in "\${UI_LIST[@]}"; do
    ( setsid timeout 45 \$UI_B ) </dev/null >\$LOGD/ui-probe.log 2>&1 &
    UIPID=\$!
    UI_HIT=""; UI_OTHER=""
    for i in \$(seq 1 20); do
      kill -0 \$UIPID 2>/dev/null || break
      for p in \$UI_PORTS; do
        case "\$UI_OCCUPIED" in *" \$p "*) continue ;; esac
        UI_R=\$(curl -s -o /dev/null -w '%{http_code} %{content_type}' --max-time 2 "http://127.0.0.1:\$p/docs" 2>/dev/null)
        case "\$UI_R" in "200 text/html"*) UI_HIT=\$p; break 2 ;; 000*|"") : ;; *) case "\$UI_PROBED" in *" \$p "*) : ;; *) UI_PROBED="\$UI_PROBED\$p "; UI_OTHER="\$UI_OTHER \${p}=[\$UI_R]" ;; esac ;; esac
        UI_R=\$(curl -s -o /dev/null -w '%{http_code} %{content_type}' --max-time 2 "http://127.0.0.1:\$p/" 2>/dev/null)
        case "\$UI_R" in "200 text/html"*) UI_HIT=\$p; break 2 ;; 000*|"") : ;; *) case "\$UI_PROBED" in *" \$p "*) : ;; *) UI_PROBED="\$UI_PROBED\$p "; UI_OTHER="\$UI_OTHER \${p}=[\$UI_R]" ;; esac ;; esac
      done
      sleep 1
    done
    if [ -n "\$UI_HIT" ] || [ -n "\$UI_OTHER" ] || kill -0 \$UIPID 2>/dev/null; then break; fi
  done
  if [ -n "\$UI_HIT" ]; then
    cell ui-probe OK "go-native http server responded (pattern: \$UI_B, port \$UI_HIT)"
  elif [ -n "\$UI_OTHER" ]; then
    cell ui-probe INFO "go-native server serves but no 200 text/html surface found on probed ports [\$UI_PORTS]:\$UI_OTHER"
  elif ! kill -0 \$UIPID 2>/dev/null; then
    cell ui-probe INFO "go-native start command is not a long-running server (rc/exit in log) — no HTML surface to probe: \$(tail -2 \$LOGD/ui-probe.log)"
  else
    cell ui-probe INFO "go-native server ran 20s, no probed port answered [\$UI_PORTS] — surface may be on an unlisted port: \$(tail -2 \$LOGD/ui-probe.log)"
  fi
  kill -- -\$UIPID 2>/dev/null; sleep 1; kill -9 -- -\$UIPID 2>/dev/null
  kill \$UIPID 2>/dev/null; sleep 1; kill -9 \$UIPID 2>/dev/null
  for p in \$UI_PROBED; do fuser -k \$p/tcp >/dev/null 2>&1; done
  true
else cell ui-probe N/A "no web UI detected"; fi

# 6 · chaos-disconnect — network cut via dead proxy: clean fail or HANG?
# Window is sized from the NATIVE SUITE's observed duration (suite_dur, timed
# around the ci-pass native run above): 2x + 30s, with
# BUNKER_QA_DISCONNECT_TIMEOUT as the minimum floor. A fixed 240s window misread
# a 100s suite as a hang on a slower agent (QA-HERMES-CANOPY-21). The computed
# window is APPLIED to the timeout invocation below — not merely named.
disc_window=\$(( \${suite_dur:-0} * 2 + 30 ))
[ "\$disc_window" -lt \${BUNKER_QA_DISCONNECT_TIMEOUT:-240} ] && disc_window=\${BUNKER_QA_DISCONNECT_TIMEOUT:-240}
# QA-HERMES-DAGGER-13: GOPROXY=off makes module fetch fail fast instead of
# blocking on the dead proxy; with the warm cache from the native run the
# build succeeds offline, so the disconnect cut is exercised against a real
# build. GOTOOLCHAIN=local prevents a toolchain download attempt. These env
# vars are harmless for non-Go native_cmd values (npm/pytest/cargo ignore them).
( HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 ALL_PROXY=http://127.0.0.1:9 GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local timeout \$disc_window bash -c "\$native_cmd" ) >\$LOGD/disc.log 2>&1; disc_rc=\$?
native_dur=\$suite_dur
if [ \$disc_rc -eq 0 ]; then cell chaos-disconnect INFO "passes with network cut (no network dep)"
elif [ \$disc_rc -eq 124 ]; then cell chaos-disconnect FAIL "HANGS when network is cut (timeout \${disc_window}s, sized from the \${native_dur}s suite: 2x+30s; override floor BUNKER_QA_DISCONNECT_TIMEOUT=\${BUNKER_QA_DISCONNECT_TIMEOUT:-240}s) — \$(head -3 \$LOGD/disc.log)"
elif [ \$disc_rc -eq 127 ]; then cell chaos-disconnect UNVERIFIED "command not found (rc=127) — the suite NEVER RAN, so the network cut was never exercised; INFRA-FAIL, not a disconnect result: \$(tail -2 \$LOGD/disc.log)"
elif build_incomplete \$LOGD/disc.log; then cell chaos-disconnect INFO "disconnect UNVERIFIED: the build never completed, so the network cut was NEVER exercised — no disconnect finding either way (rc=\$disc_rc; environment or code, never network): \$(build_incomplete_cause \$LOGD/disc.log)"
elif go_suite_env_failure \$LOGD/disc.log; then cell chaos-disconnect INFO "ENV-BLOCKED: the go suite failed because the test host lacks a build tool (\`make\` on JIT agents, measured 2026-09-19) — the disconnect cut was exercised against a broken build, no disconnect finding (rc=\$disc_rc): \$(grep -m1 -E 'exec: .* executable file not found' \$LOGD/disc.log)"
elif suite_deps_missing \$LOGD/disc.log; then cell chaos-disconnect UNVERIFIED "missing test dependencies — the suite NEVER RAN, so the network cut was never exercised; INFRA-FAIL, not a disconnect result (rc=\$disc_rc): \$(suite_deps_missing_cause \$LOGD/disc.log)"
elif native_runner_missing \$LOGD/disc.log; then cell chaos-disconnect UNVERIFIED "native runner missing (rc=\$disc_rc) - the suite NEVER RAN, so the network cut was never exercised; INFRA-FAIL, not a disconnect result: \$(native_runner_missing_cause \$LOGD/disc.log)"
else cell chaos-disconnect OK "fails fast on disconnect (rc=\$disc_rc): \$(head -2 \$LOGD/disc.log)"; fi

# 7 · chaos-shutdown — SIGTERM stop/restart + SIGKILL recovery (docker path)
if ls docker-compose*.yml compose*.yml >/dev/null 2>&1 || ls docker-compose*.yml 2>/dev/null | grep -q . || ls compose*.yml 2>/dev/null | grep -q .; then
  # QA-HEADING-010 (2026-09-25): the && chain short-circuited BEFORE its echo
  # markers, so a first-verb failure left sd.log EMPTY and the cell graded
  # FAIL "rc=1" with no attribution. Run EVERY verb unconditionally, echo each
  # step's rc, and derive sd_rc as the FIRST failing step — the trace names
  # the failing verb; grading below still keys on the STOP=/KILLRECOVER= markers.
  up_rc=0; stop_rc=0; start1_rc=0; kill_rc=0; start2_rc=0; down_rc=0
  { docker compose up -d >/dev/null 2>&1; up_rc=\$?; echo UP=\$up_rc
    docker compose stop >/dev/null 2>&1; stop_rc=\$?; echo STOP=\$stop_rc
    [ \$stop_rc -eq 0 ] && sleep 4
    docker compose start >/dev/null 2>&1; start1_rc=\$?; echo RESTART=\$start1_rc
    docker compose kill >/dev/null 2>&1; kill_rc=\$?; echo KILL=\$kill_rc
    [ \$kill_rc -eq 0 ] && sleep 2
    docker compose start >/dev/null 2>&1; start2_rc=\$?; echo KILLRECOVER=\$start2_rc
    docker compose down >/dev/null 2>&1; down_rc=\$?; echo DOWN=\$down_rc
  } >\$LOGD/sd.log 2>&1
  sd_rc=\$up_rc
  [ "\$sd_rc" -eq 0 ] && sd_rc=\$stop_rc
  [ "\$sd_rc" -eq 0 ] && sd_rc=\$start1_rc
  [ "\$sd_rc" -eq 0 ] && sd_rc=\$kill_rc
  [ "\$sd_rc" -eq 0 ] && sd_rc=\$start2_rc
  [ "\$sd_rc" -eq 0 ] && sd_rc=\$down_rc
  if [ "\$up_rc" -ne 0 ]; then cell chaos-shutdown FAIL "compose up failed before the stop/kill chain — \$(fail_detail \$up_rc \$LOGD/sd.log 'docker compose up -d')"
  elif [ "\$stop_rc" -ne 0 ]; then cell chaos-shutdown FAIL "compose stop failed rc=\$stop_rc (chain rc=\$sd_rc) — \$(cat \$LOGD/sd.log)"
  elif [ "\$start1_rc" -ne 0 ]; then cell chaos-shutdown FAIL "compose start (post-stop) failed rc=\$start1_rc (chain rc=\$sd_rc) — \$(cat \$LOGD/sd.log)"
  elif [ "\$kill_rc" -ne 0 ]; then cell chaos-shutdown INFO "graceful ok, kill step failed rc=\$kill_rc (chain rc=\$sd_rc) — \$(cat \$LOGD/sd.log)"
  elif [ "\$start2_rc" -ne 0 ]; then cell chaos-shutdown FAIL "kill-recovery start failed rc=\$start2_rc (chain rc=\$sd_rc) — \$(cat \$LOGD/sd.log)"
  elif [ "\$down_rc" -ne 0 ]; then cell chaos-shutdown OK "SIGTERM + SIGKILL recovery clean (down rc=\$down_rc ignored)"
  elif grep -qE 'STOP=0' \$LOGD/sd.log && grep -qE 'KILLRECOVER=0' \$LOGD/sd.log; then cell chaos-shutdown OK "SIGTERM + SIGKILL recovery clean"
  elif grep -qE 'STOP=0' \$LOGD/sd.log; then cell chaos-shutdown INFO "graceful ok, kill-recovery issue: \$(cat \$LOGD/sd.log)"
  else cell chaos-shutdown FAIL "\$(fail_detail \$sd_rc \$LOGD/sd.log 'docker compose up -d; stop; start; kill; start; down')"; fi
else cell chaos-shutdown N/A "no compose file"; fi

# 8 · chaos-corruption — truncate a state file, OBSERVE the next start, restore
# QA-GITREINS-POC-6 (2026-09-17): the old cell truncated the first *.db that
# find returned and recorded INFO "state file truncated: \$SF — watch crash
# behavior on next start". No start was ever observed (that WAS the finding), the
# truncated file was left in place for the cells that run after it, and the cell
# could not tell a real state store from a stray — for gitreins-poc it picked
# ./dev.db, an untracked leftover that no source file in the tree references.
# Now: candidates exclude tool caches, a candidate whose basename appears in the
# project's own source wins, the next start IS run under a timeout, the outcome
# is classified, and ~/state.bak is restored either way.
# Project-probe hook (SDKTS-QA-H3-SDK-TYPESCRIPT-FOREMAN-7): a repo may own a
# REAL failure-mode probe at .qa/chaos-probes/corruption.sh (opt-in, generic —
# the harness never learns any project name; the probe exercises the project's
# own SDK behavior and self-grades). When present + executable it REPLACES the
# generic truncate/restart logic for this run: 0=OK (first stdout line in the
# detail), 1=FAIL (SDK misbehavior), 2=INFO (environment gap, probe skipped),
# 124=FAIL (probe HANGS), anything else=INFO (rc + output). When absent this
# block changes nothing — the generic cell logic below runs byte-identically.
if [ -x .qa/chaos-probes/corruption.sh ]; then
  pp_out=\$(timeout 120 .qa/chaos-probes/corruption.sh 2>&1); pp_rc=\$?
  pp_first=\$(printf '%s' "\$pp_out" | head -1 | cut -c1-300)
  pp_more=\$(printf '%s' "\$pp_out" | tail -n +2 | tr '\n' ' ' | cut -c1-500)
  case "\$pp_rc" in
    0)   cell chaos-corruption OK "project probe: \$pp_first \$pp_more" ;;
    1)   cell chaos-corruption FAIL "project probe reports SDK misbehavior: \$pp_first \$pp_more" ;;
    2)   cell chaos-corruption INFO "project probe: environment gap, probe skipped (\$pp_first \$pp_more)" ;;
    124) cell chaos-corruption FAIL "project probe HANGS (>120s): \$pp_first \$pp_more" ;;
    *)   cell chaos-corruption INFO "project probe rc=\$pp_rc: \$pp_first \$pp_more" ;;
  esac
else
CAND=\$(find . -maxdepth 3 \( -name '*.db' -o -name '*.sqlite' -o -name '*.sqlite3' \) \
  -not -path './.git/*' -not -path './node_modules/*' -not -path './.mypy_cache/*' \
  -not -path './.pytest_cache/*' -not -path './.ruff_cache/*' -not -path './.vfs/*' \
  -not -path './.venv/*' -not -path './venv/*' 2>/dev/null | sort)
state_ref() { # <candidate> → first OTHER source file that mentions its basename
  local b; b=\$(basename "\$1")
  grep -rIl --exclude-dir=.git --exclude-dir=.venv --exclude-dir=node_modules \
    --exclude-dir=.mypy_cache --exclude-dir=.coding-hermes --exclude-dir=.gitreins \
    --include='*.py' --include='*.js' --include='*.ts' --include='*.go' --include='*.rs' \
    --include='*.rb' --include='*.java' --include='*.sh' --include='*.toml' \
    --include='*.yaml' --include='*.yml' -F "\$b" . 2>/dev/null | grep -vF "\$1" | head -1
}
SF=""; SF_REF=""
for c in \$CAND; do r=\$(state_ref "\$c"); if [ -n "\$r" ]; then SF="\$c"; SF_REF="\$r"; break; fi; done
[ -z "\$SF" ] && [ -n "\$CAND" ] && SF=\$(printf '%s\n' "\$CAND" | head -1)
if [ -n "\$SF" ]; then
  REF="not referenced by any source file"
  [ -n "\$SF_REF" ] && REF="referenced by \$SF_REF"
  if [ -n "\$BIN" ]; then
    if cp "\$SF" ~/state.bak 2>/dev/null && head -c 64 "\$SF" > ~/trunc 2>/dev/null && cp ~/trunc "\$SF" 2>/dev/null; then
      ( timeout 20 \$BIN ) >\$LOGD/corr.log 2>&1; corr_rc=\$?
      cp ~/state.bak "\$SF" 2>/dev/null
      corr_tracked=no
      git ls-files --error-unmatch -- "\$SF" >/dev/null 2>&1 && corr_tracked=yes
      corr_result=\$(corr_grade "\$corr_rc" "\$LOGD/corr.log" "\$SF" "\$REF" "\$corr_tracked")
      corr_status=\$(printf '%s' "\$corr_result" | cut -f1)
      corr_detail=\$(printf '%s' "\$corr_result" | cut -f2-)
      cell chaos-corruption "\$corr_status" "\$corr_detail"
    else
      cell chaos-corruption FAIL "could not truncate \$SF: \$(tail -1 ~/trunc 2>/dev/null || echo cp-error)"
    fi
  else
    cell chaos-corruption UNVERIFIED "no start command detected — state-restart behavior UNVERIFIED (\$REF)"
  fi
else cell chaos-corruption N/A "no db/state files in repo"; fi
fi  # end project-probe hook (corruption)

# 9 · chaos-resource — suite under 3G memory cap
# QA-TASK-ROUTER-3: a green/red result is VACUOUS unless the log PROVES the cap
# applied — the finding was that with the wrong PATH the wrapper was dropped and
# the suite ran uncapped while the cell still reported a result. Record the cap
# AND prove it inside the capped child (ulimit -v read back from the child, not
# from the parent), then require that evidence in the verdict.
CAP_KB=3145728
export CAP_KB   # the single-quoted probe expands CAP_KB inside \`bash -c\`, so it must be in the child env
# Single-quoted on purpose: the $(ulimit -v) must expand INSIDE the capped
# child, not at assignment time in the uncapped parent. The old double-quoted
# form froze "CAP_EFFECTIVE=unlimited" into the probe text before the cap was
# set, so every run reported "cap NOT applied" regardless of the real limit
# (warpfs 09-19, off-by-one 09-20 x2 — probe artifact, not a real defect;
# the subshell probe in res.log proves the ulimit itself applies).
cap_probe='echo CAP_EFFECTIVE=\$(ulimit -v) CAP_REQUESTED=\$CAP_KB'
# QA-OFF-BY-ONE-11 / warpfs 09-19 / off-by-one 09-20 (3 runs): the child kept
# reporting CAP_EFFECTIVE=unlimited even though the subshell sets ulimit -v
# first. Probe EVERY layer so the next run names where the cap is lost:
# parent shell -> subshell right after ulimit -> the capped child itself.
echo PARENT_CAP=\$(ulimit -v) >\$LOGD/res.log
# QA-BUNKER-57 (2026-10-04): the pytest leg invokes pip-installed tooling; on an
# agent where ensurepip failed there is no pip and the leg would die rc=127
# 'pip: command not found' under the cap — vacuous noise graded FAIL. Grade
# UNVERIFIED before running anything. (Go/npm legs are unaffected: pip absent
# is irrelevant to them and qa_have_pip only gates the pip-native runner.)
if echo "$native_cmd" | grep -q pytest && ! qa_have_pip; then
  cell chaos-resource UNVERIFIED "pip absent on agent (ensurepip failed; no sudo) — the pytest suite never ran under the cap, so no memory finding either way (\$cap_note)"
else
( ulimit -v \$CAP_KB; echo SUBSHELL_CAP=\$(ulimit -v); bash -c "\$cap_probe; $native_cmd" ) >>\$LOGD/res.log 2>&1; res_rc=\$?
# QA-H3-16 (2026-09-21): the child's cap_probe echoes CAP_EFFECTIVE and
# CAP_REQUESTED on ONE line, so the bare 's/^CAP_EFFECTIVE=//p' capture pulled
# the whole "3145728 3145728" string into cap_seen, which then never equaled
# CAP_KB and forced the UNVERIFIED "cap NOT applied" branch on every run with a
# WORKING cap (warpfs 09-19 + off-by-one 09-20 + h3 09-21 misread the same way).
# Extract only the FIRST field — the value the child actually reported for
# ulimit -v.
cap_seen=\$(sed -n 's/^CAP_EFFECTIVE=//p' \$LOGD/res.log | head -1 | cut -d' ' -f1)
sub_seen=\$(sed -n 's/^SUBSHELL_CAP=//p' \$LOGD/res.log | head -1 | cut -d' ' -f1)
par_seen=\$(sed -n 's/^PARENT_CAP=//p' \$LOGD/res.log | head -1 | cut -d' ' -f1)
cap_note="cap=\${CAP_KB}KB child_saw=\${cap_seen:-NONE} subshell_saw=\${sub_seen:-NONE} parent_saw=\${par_seen:-NONE}"
if [ "\$cap_seen" != "\$CAP_KB" ]; then
  # the child did not see the cap: the wrapper was dropped (PATH/shape problem).
  # A result from this run says NOTHING about memory behaviour.
  cell chaos-resource UNVERIFIED "cap NOT applied in the child (\$cap_note) — the suite ran UNCAPPED, so no memory finding either way (harness defect, not a repo result): rc=\$res_rc"
elif [ \$res_rc -eq 0 ]; then
  # QA-BUNKER-29 (2026-09-30): rc=0 from the vacuous \`echo no-test-path\`
  # placeholder is NOT a cap-survival result — no test ever exercised the cap.
  # Grade UNVERIFIED, never PASS on a suite that never ran. Non-vacuous rc=0
  # path unchanged.
  if vacuous_native_suite \$LOGD/res.log; then
    cell chaos-resource UNVERIFIED "suite NEVER RAN under the cap (vacuous no-test-path) — the cap was never exercised by any test (\$cap_note)"
  else
    cell chaos-resource PASS "suite survives 3G memory cap (\$cap_note)"
  fi
elif [ \$res_rc -eq 127 ]; then cell chaos-resource UNVERIFIED "command not found under cap (rc=127, \$cap_note) — the suite NEVER RAN; INFRA-FAIL, not a result: \$(tail -2 \$LOGD/res.log)"
elif grep -qiE 'killed|out of memory' \$LOGD/res.log; then cell chaos-resource INFO "OOM under cap (expected for heavy suites, \$cap_note): \$(tail -2 \$LOGD/res.log)"
elif build_incomplete \$LOGD/res.log; then cell chaos-resource INFO "ENV-BLOCKED: the build never completed under the 3G cap, so the cap was NEVER exercised (rc=\$res_rc, \$cap_note; environment or code, never memory) — not a memory finding: \$(build_incomplete_cause \$LOGD/res.log)"
elif go_suite_env_failure \$LOGD/res.log; then cell chaos-resource INFO "ENV-BLOCKED: the suite died because the test host lacks a build tool — cap WAS applied, but the failure is the missing tool, not memory (\$cap_note, rc=\$res_rc): \$(grep -m1 -E 'exec: .* executable file not found' \$LOGD/res.log)"
elif suite_deps_missing \$LOGD/res.log; then cell chaos-resource UNVERIFIED "missing test dependencies — the suite NEVER RAN under the cap, so no memory finding either way (rc=\$res_rc, \$cap_note): \$(suite_deps_missing_cause \$LOGD/res.log)"
elif native_runner_missing \$LOGD/res.log; then cell chaos-resource UNVERIFIED "native runner missing under cap (rc=\$res_rc, \$cap_note) - the suite NEVER RAN, so the cap was never exercised by any test; INFRA-FAIL, not a result: \$(native_runner_missing_cause \$LOGD/res.log)"
elif go_toolchain_oom \$LOGD/res.log; then cell chaos-resource INFO "ENV-BLOCKED: the go toolchain itself failed to build under the 3G cap (SystemResources/address-space) — no suite result either way, the cap-vs-toolchain interaction is not a memory verdict about the code (\$cap_note, rc=\$res_rc): \$(go_toolchain_oom_cause \$LOGD/res.log)"
elif go_panic_timeout \$LOGD/res.log; then cell chaos-resource INFO "SUITE-PANIC: go test hit its own 10m timeout and panicked under the cap — the suite has NO clean failure mode at its worst-case memory ceiling (QA-HERMES-CANOPY-38): \$(go_panic_timeout_cause \$LOGD/res.log) | failing: \$(capped_fail_summary \$LOGD/res.log)"
else cell chaos-resource FAIL "rc=\$res_rc under cap (\$cap_note): \$(capped_fail_summary \$LOGD/res.log)"; fi
fi # QA-BUNKER-57 pip-absent gate (pytest legs only)

# 10 · chaos-errorpath — missing config: clean usage or panic?
# Project-probe hook (SDKTS-QA-H3-SDK-TYPESCRIPT-FOREMAN-7): same opt-in, fully
# generic contract as the chaos-corruption cell — a repo-owned
# .qa/chaos-probes/errorpath.sh REPLACES the generic missing-config BIN probe
# for this run (0=OK, 1=FAIL SDK misbehavior, 2=INFO environment gap, 124=FAIL
# HANGS, other=INFO rc). Absent probe → byte-identical generic behavior.
if [ -x .qa/chaos-probes/errorpath.sh ]; then
  pp_out=\$(timeout 120 .qa/chaos-probes/errorpath.sh 2>&1); pp_rc=\$?
  pp_first=\$(printf '%s' "\$pp_out" | head -1 | cut -c1-300)
  pp_more=\$(printf '%s' "\$pp_out" | tail -n +2 | tr '\n' ' ' | cut -c1-500)
  case "\$pp_rc" in
    0)   cell chaos-errorpath OK "project probe: \$pp_first \$pp_more" ;;
    1)   cell chaos-errorpath FAIL "project probe reports SDK misbehavior: \$pp_first \$pp_more" ;;
    2)   cell chaos-errorpath INFO "project probe: environment gap, probe skipped (\$pp_first \$pp_more)" ;;
    124) cell chaos-errorpath FAIL "project probe HANGS (>120s): \$pp_first \$pp_more" ;;
    *)   cell chaos-errorpath INFO "project probe rc=\$pp_rc: \$pp_first \$pp_more" ;;
  esac
elif [ -n "\$BIN" ]; then
  EP_DIR=~/qa-noconfig-\$\$
  ep_out=\$(ep_probe "\$BIN" "\$EP_DIR" "\$LOGD")
  EP_STARTED=\$(printf '%s' "\$ep_out" | cut -f1)
  ep_rc=\$(printf '%s' "\$ep_out" | cut -f2)
  ep_result=\$(ep_grade "\$ep_rc" "\$LOGD/ep.log" "\$EP_STARTED")
  ep_status=\$(printf '%s' "\$ep_result" | cut -f1)
  ep_detail=\$(printf '%s' "\$ep_result" | cut -f2-)
  cell chaos-errorpath "\$ep_status" "\$ep_detail"
else
  # QA-9ROUTER-22: empty BIN = no start command detected / entrypoint not
  # observable. Never a product verdict — the missing-config probe never ran.
  cell chaos-errorpath INFO "no start command detected / entrypoint not observable — missing-config behavior UNVERIFIED"
fi

echo CELLS-DONE
touch ~/qa-run.finished
EOF
}

run() {
  local repo="${1:?usage: bunker-qa.sh run <repo-dir> [--keep]}"
  shift
  local keep=0
  for a in "$@"; do [ "$a" = "--keep" ] && keep=1; done

  local PROJ; PROJ=$(basename "$repo")
  : > "$EVIDENCE"

  # Timeout/empty-evidence traps (2026-09-07, QA-DEXDAT-CORE-1): a tool-level
  # SIGTERM (Hermes terminal cap) or a sync-fail exit kills the battery mid-run
  # leaving a 0-byte evidence file — parse_cells then sees cells:[] and the DAG
  # reports UNVERIFIED silently, indistinguishable from "nothing to check".
  # write_fail_if_empty() (top-level, shared with launch()) appends exactly one
  # FAIL cell — idempotent: TERM + EXIT traps firing together produce one line.
  # Repo sanity runs AFTER truncate+traps (QA-GITREINS-POC-QA-1): a stand-in
  # workdir (no .git) must yield an actionable FAIL row, not a silent exit.
  trap 'write_fail_if_empty "battery terminated by SIGTERM (tool timeout) before any cell recorded"; exit 124' TERM
  trap 'write_fail_if_empty "battery interrupted by SIGINT before any cell recorded"; exit 130' INT
  trap 'write_fail_if_empty "battery exited rc=$? before any cell recorded"' EXIT

  if [ ! -d "$repo/.git" ]; then
    local git_top
    git_top=$(git -C "$repo" rev-parse --show-toplevel 2>/dev/null || true)
    echo "ERROR: $repo is not a git repo (git resolves toplevel: ${git_top:-none}) — dispatch against the primary repo workdir" >&2
    printf '{"cell":"run_battery","status":"FAIL","detail":"run refused: %s is not a git repo (git resolves toplevel: %s) — a role-lane workdir was dispatched instead of the primary repo; re-dispatch against the real workdir","ts":"%s"}\n' \
      "$repo" "${git_top:-none}" "$(date -u +%FT%TZ)" >> "$EVIDENCE"
    exit 1
  fi

  log "QA project: $PROJ — fresh-system pass on $SERVER (ttl $TTL)"

  # Deterministic-failure preflights (config / ssh / capacity) — SHARED with
  # launch(); see preflights() for the record-one-FAIL-row + rc=2 contract.
  preflights

  local agent key spawn_err
  spawn_err=$(mktemp)
  agent=$(spawn_agent 2>"$spawn_err") || {
    # QA-2 (2026-09-03): hard-fail loudly — record a FAIL cell so the DAG's
    # parse_cells never interprets an empty evidence file as "nothing to
    # check". UNVERIFIED must be distinguishable from a clean battery.
    # The REAL spawn stderr (e.g. "port range allocation: no free port
    # ranges available") is captured into the cell detail so interpreters
    # never have to re-diagnose the failure manually.
    local err_detail
    err_detail=$(tail -3 "$spawn_err" | tr '\n' ' ' | cut -c1-300)
    rm -f "$spawn_err"
    echo "ERROR: spawn failed on $SERVER: $err_detail" >&2
    printf '{"cell":"run_battery","status":"FAIL","detail":"spawn failed on %s: %s","ts":"%s"}\n' \
      "$SERVER" "$err_detail" "$(date -u +%FT%TZ)" >> "$EVIDENCE"
    exit 1
  }
  rm -f "$spawn_err"
  key="$HOME/.bunker/keys/$agent"
  BUNKER_QA_AGENT="$agent"
  # QA-BUNKER-19 b+c (2026-09-21): cleanup destroy failures are SURFACED, not
  # swallowed — the old `|| true` defeated the destroy-failure escalation
  # contract (a failed destroy left agent 230abb5b running, invisible to the
  # run log). qa_destroy_agent() emits "destroy FAILED"/"STILL PRESENT AFTER
  # DESTROY" on stderr + a FAIL evidence row; the evidence path is the $EVIDENCE
  # global so the EXIT trap needs nothing captured at definition time.
  cleanup() { log "destroying temp agent $BUNKER_QA_AGENT"; qa_destroy_agent "$BUNKER_QA_AGENT" "$EVIDENCE"; }
  [ "$keep" = 1 ] || trap cleanup EXIT
  log "agent=$agent @ $SERVER (tailscale)"

  # Pre-flight hygiene (2026-08-31): temp-agent homes PERSIST across runs
  # (spawn is idempotent, no userdel), so a previous project's
  # ~/qa-evidence.jsonl, ~/qa-run.sh and /tmp/*.log (owned by the old
  # agent's uid) survive into this run. That caused (a) stale evidence
  # cells from the prior project to be appended to this run's evidence
  # file, and (b) "Permission denied" on /tmp/inst.log etc. because the
  # fresh agent uid can't overwrite the old agent's files. Clear them
  # before shipping anything.
  agent_ssh "rm -f ~/qa-run.sh ~/qa-up-prev ~/qa-up-prev.absent ~/qa-evidence.jsonl /tmp/inst.log /tmp/ci.log /tmp/native.log /tmp/disc.log /tmp/ep.log /tmp/sd.log /tmp/res.log" >/dev/null 2>&1 || true

  # detect tooling LOCALLY (one place, no quoting layers) — SHARED with launch()
  # (BUNKER_QA_INSTALL_CMD / BUNKER_QA_NATIVE_CMD escape hatches live in detect_cmds)
  detect_cmds "$repo" "$agent"
  local install_cmd="$DETECT_INSTALL" ci_cmd="$DETECT_CI" native_cmd="$DETECT_NATIVE"

  log "CELL fresh-install: sync repo → ~/qa-$PROJ"
  # Sync verdict/logic lives in sync_repo() (shared with launch(); see its
  # comment for why the REMOTE's SYNC-OK echo is the truth, not tar's rc).
  if ! sync_repo "$repo" "$PROJ"; then
    echo "ERROR: repo sync failed — $SYNC_ERR"
    exit 1
  fi

  # QA-OFF-BY-ONE-22: stream the previous release's tree for the upgrade cell
  ship_prev_tag_tree "$repo" "$PROJ"

  # ship the remote script via ssh STDIN (QA-BUNKER-40, 2026-10-05): the old
  # `echo '$b64' | base64 -d` embedded the whole script in one ssh argv word —
  # past ~131KB base64 that exceeds the kernel MAX_ARG_STRLEN (E2BIG:
  # "Argument list too long"). stdin is quoting-safe AND argv-unbounded.
  build_remote_script "$PROJ" "$install_cmd" "$ci_cmd" "$native_cmd" \
    | agent_ssh "cat > ~/qa-run.sh && bash ~/qa-run.sh" | tail -1

  # pull evidence back
  agent_ssh "cat ~/qa-evidence.jsonl 2>/dev/null" >> "$EVIDENCE" || true

  log "QA pass complete — evidence: $EVIDENCE"
  echo "EVIDENCE=$EVIDENCE"
}

# ─── launch — PHASE A: preflights + spawn + sync + START the battery DETACHED ───
# Returns as soon as `qa-run.sh` is running on the agent — it NEVER waits on a
# cell. This is what keeps every Hermes tool call far under the ~420s terminal
# cap that killed the synchronous battery (QA-HERMES-CANOPY-8).
# Side effects: spawns the temp agent (it OUTLIVES this call by design — collect
# or its TTL tears it down), syncs the repo, writes $EVIDENCE.meta (agent=,
# server=, evidence=, launched=, keep=) and the ONE launch cell that makes a
# 0-byte evidence file impossible.
launch() {
  local repo="${1:?usage: bunker-qa.sh launch <repo-dir> [--keep]}"
  shift
  local keep=0
  for a in "$@"; do [ "$a" = "--keep" ] && keep=1; done

  local PROJ; PROJ=$(basename "$repo")
  : > "$EVIDENCE"

  # TR-085: a launch killed before it finished (tool cap, SIGTERM, crash) used to
  # leave NO record at all — the meta file was written only on the success path,
  # so `collect` fell back to the shared default evidence path and probed whatever
  # agent id that STALE meta named (dogfood 2026-09-20: collect probed a 2-day-old
  # agent while the real one ran on unrecorded). Write an EARLY record now, and
  # keep it up to date as the id becomes known. collect() refuses a mismatch.
  local meta="$EVIDENCE.meta"
  write_meta() { # agent server keep project
    printf 'agent=%s\nserver=%s\nevidence=%s\nlaunched=%s\nkeep=%s\nproject=%s\n' \
      "${1:-}" "${2:-}" "$EVIDENCE" "${3:-$(date -u +%FT%TZ)}" "${4:-0}" "${5:-$PROJ}" > "$meta"
  }
  write_meta "" "$SERVER" "$(date -u +%FT%TZ)" 0 "$PROJ"

  # Traps + evidence truncate MUST precede the repo sanity check: a
  # stand-in-workdir dispatch (no .git — QA-GITREINS-POC-QA-1, 2026-09-17)
  # used to exit here silently, leaving a 0-byte evidence file and no FAIL
  # row, which collect then read as "launch phase never executed".
  trap 'write_fail_if_empty "launch terminated by SIGTERM (tool timeout) before the launch cell was recorded"; exit 124' TERM
  trap 'write_fail_if_empty "launch interrupted by SIGINT before the launch cell was recorded"; exit 130' INT
  trap 'write_fail_if_empty "launch exited rc=$? before the launch cell was recorded"' EXIT

  if [ ! -d "$repo/.git" ]; then
    local git_top
    git_top=$(git -C "$repo" rev-parse --show-toplevel 2>/dev/null || true)
    echo "ERROR: $repo is not a git repo (git resolves toplevel: ${git_top:-none}) — dispatch against the primary repo workdir" >&2
    printf '{"cell":"run_battery","status":"FAIL","detail":"launch refused: %s is not a git repo (git resolves toplevel: %s) — a role-lane workdir was dispatched instead of the primary repo; re-dispatch against the real workdir","ts":"%s"}\n' \
      "$repo" "${git_top:-none}" "$(date -u +%FT%TZ)" >> "$EVIDENCE"
    exit 1
  fi

  log "QA LAUNCH project: $PROJ — detached battery on $SERVER (ttl $TTL)"
  preflights

  local spawn_err
  spawn_err=$(mktemp)
  agent=$(spawn_agent 2>"$spawn_err") || {
    # Same contract as run(): hard-fail loudly with ONE actionable FAIL cell
    # (the real spawn stderr) so parse_cells can never read an empty evidence
    # file as "nothing to check".
    local err_detail
    err_detail=$(tail -3 "$spawn_err" | tr '\n' ' ' | cut -c1-300)
    rm -f "$spawn_err"
    echo "ERROR: spawn failed on $SERVER: $err_detail" >&2
    printf '{"cell":"run_battery","status":"FAIL","detail":"spawn failed on %s: %s","ts":"%s"}\n' \
      "$SERVER" "$err_detail" "$(date -u +%FT%TZ)" >> "$EVIDENCE"
    exit 1
  }
  rm -f "$spawn_err"
  key="$HOME/.bunker/keys/$agent"
  BUNKER_QA_AGENT="$agent"
  # TR-085: record the real agent id the moment it is known, so a kill anywhere
  # after this point leaves collect() a CORRECT id to poll (or destroy).
  write_meta "$agent" "$SERVER" "$(date -u +%FT%TZ)" "$keep" "$PROJ"
  # NO destroy trap here ON PURPOSE: the agent must outlive this call — that is
  # the entire point of the split. collect() destroys it once the battery has
  # finished; otherwise its TTL does.
  log "agent=$agent @ $SERVER (tailscale)"

  # Pre-flight hygiene (2026-08-31): temp-agent homes PERSIST across runs, so a
  # previous project's script/evidence/marker/logs survive into this run.
  # Clearing the finish marker matters here: a stale one would make collect()
  # read "finished" while this run's battery is barely started.
  agent_ssh "rm -f ~/qa-run.sh ~/qa-run.finished ~/qa-run.log ~/qa-up-prev ~/qa-up-prev.absent ~/qa-evidence.jsonl /tmp/inst.log /tmp/ci.log /tmp/native.log /tmp/disc.log /tmp/ep.log /tmp/sd.log /tmp/res.log" >/dev/null 2>&1 || true

  detect_cmds "$repo" "$agent"

  log "CELL fresh-install: sync repo → ~/qa-$PROJ"
  if ! sync_repo "$repo" "$PROJ"; then
    echo "ERROR: repo sync failed — $SYNC_ERR" >&2
    printf '{"cell":"run_battery","status":"FAIL","detail":"repo sync failed in launch phase (server=%s agent=%s): %s","ts":"%s"}\n' \
      "$SERVER" "$agent" "$(printf '%s' "$SYNC_ERR" | tr '\n' ' ' | cut -c1-300)" "$(date -u +%FT%TZ)" >> "$EVIDENCE"
    exit 1
  fi

  # QA-OFF-BY-ONE-22: stream the previous release's tree for the upgrade cell
  ship_prev_tag_tree "$repo" "$PROJ"

  # Ship the remote script base64 (the ONLY quoting-safe path through ssh) and
  # START it detached. Deviations from the naive one-liner, both deliberate:
  #  * the decode runs in the FOREGROUND (so a mangled b64 cannot print
  #    LAUNCHED over an empty ~/qa-run.sh), and `wc -c` records the shipped size;
  #  * the start is grouped `( ... & )` with </dev/null so the battery is fully
  #    orphaned (setsid + nohup + no stdin) and the ssh channel closes at once.
  #  * 2026-10-05 (QA-BUNKER-40): the script is piped over ssh via STDIN, not
  #    embedded in the ssh COMMAND LINE — the generated script has grown past
  #    ~97KB plain (~131KB base64), and a b64 that large as a single argv word
  #    exceeds the kernel per-argument limit MAX_ARG_STRLEN (131072, ~128KB),
  #    dying `/usr/bin/timeout: Argument list too long` (E2BIG) on the agent
  #    side for every repo whose DETECT_* block pushed the script over the
  #    line. stdin carries no argv-size limit and is still quoting-safe.
  local b64 launch_out launch_rc script_bytes
  script_bytes=$(build_remote_script "$PROJ" "$DETECT_INSTALL" "$DETECT_CI" "$DETECT_NATIVE" | wc -c)
  launch_out=$(build_remote_script "$PROJ" "$DETECT_INSTALL" "$DETECT_CI" "$DETECT_NATIVE" \
    | agent_ssh "cat > ~/qa-run.sh && echo SCRIPT_BYTES=\$(wc -c < ~/qa-run.sh) && ( nohup setsid bash ~/qa-run.sh </dev/null >~/qa-run.log 2>&1 & ) && echo LAUNCHED" 2>&1)
  launch_rc=$?
  if ! printf '%s' "$launch_out" | grep -q 'LAUNCHED'; then
    echo "ERROR: could not start the battery on agent=$agent ($SERVER): $(printf '%s' "$launch_out" | tail -1)" >&2
    printf '{"cell":"run_battery","status":"FAIL","detail":"qa-run.sh could not be started on agent=%s (%s) rc=%s: %s","ts":"%s"}\n' \
      "$agent" "$SERVER" "$launch_rc" "$(printf '%s' "$launch_out" | tr '\n' ' ' | cut -c1-300)" "$(date -u +%FT%TZ)" >> "$EVIDENCE"
    exit 1
  fi

  local launched meta script_bytes
  launched=$(date -u +%FT%TZ)
  script_bytes=$(printf '%s' "$launch_out" | sed -n 's/^SCRIPT_BYTES=//p' | tail -1)
  write_meta "$agent" "$SERVER" "$launched" "$keep" "$PROJ"
  printf '{"project":"%s","cell":"launch","status":"OK","detail":"agent=%s server=%s ttl=%s script_bytes=%s — battery started detached (qa-run.sh); phase B: bunker-qa.sh collect --evidence %s","ts":"%s"}\n' \
    "$PROJ" "$agent" "$SERVER" "$TTL" "${script_bytes:-unknown}" "$EVIDENCE" "$launched" >> "$EVIDENCE"
  log "QA launch complete — no cells waited on locally (phase B: collect --evidence $EVIDENCE)"
  echo "LAUNCHED agent=$agent evidence=$EVIDENCE"
}

# ─── collect — PHASE B: poll → pull → destroy (idempotent) ───
# While the battery is still alive on the agent this prints RUNNING, exits 0 and
# DESTROYS NOTHING, so a short-poll loop can call it repeatedly until the
# 7-15 min battery finishes. Once it is done: merge-dedupe the agent's
# ~/qa-evidence.jsonl into the local evidence file (idempotent on the row "ts"
# field), append ONE collect cell, destroy the temp agent, print COLLECTED.
# Failures (no launch record / unreachable agent / pull failed) record a collect
# FAIL cell and exit 0 — collect is a poll, not the battery's verdict.
collect() {
  local evidence=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --evidence|-e) evidence="${2:-}"; shift 2 ;;
      --evidence=*) evidence="${1#--evidence=}"; shift ;;
      -*) echo "ERROR: unknown collect flag: $1" >&2; exit 2 ;;
      *) evidence="$1"; shift ;;
    esac
  done
  [ -n "$evidence" ] || { echo "usage: bunker-qa.sh collect --evidence <path>" >&2; exit 2; }
  [ -f "$evidence" ] || : > "$evidence"
  local meta="$evidence.meta"

  append_collect_row() { # <status> <detail>
    printf '{"cell":"collect","status":"%s","detail":"%s","ts":"%s"}\n' \
      "$1" "$(printf '%s' "$2" | tr '\n' ' ' | cut -c1-300)" "$(date -u +%FT%TZ)" >> "$evidence"
  }

  if [ ! -f "$meta" ]; then
    append_collect_row FAIL "no launch record at $meta — nothing to collect (launch phase never ran, or a different evidence path)"
    echo "COLLECT-FAIL no-launch-record"
    exit 0
  fi
  # Idempotency: a successful collect already recorded its cell — re-calling is a
  # no-op instead of a spurious "unreachable agent" FAIL (the agent is destroyed
  # by then, so the probe below would be empty).
  if grep -q '"cell":"collect","status":"OK"' "$evidence" 2>/dev/null; then
    echo "COLLECTED already-collected"
    exit 0
  fi

  local agent="" server="" ev="" keep=0
  agent=$(sed -n 's/^agent=//p' "$meta" | head -1)
  server=$(sed -n 's/^server=//p' "$meta" | head -1)
  ev=$(sed -n 's/^evidence=//p' "$meta" | head -1)
  [ "$(sed -n 's/^keep=//p' "$meta" | head -1)" = "1" ] && keep=1
  [ -n "$server" ] && SERVER="$server"
  [ -n "$ev" ] && [ "$ev" != "$evidence" ] && echo "WARN: launch record names evidence=$ev but collect was asked for $evidence (using $evidence)" >&2
  if [ -z "$agent" ]; then
    append_collect_row FAIL "launch record $meta has no agent= line — cannot poll the battery (the launch was killed before the agent id was recorded; check \`bunker list --server $server\` for an orphan and destroy it)"
    echo "COLLECT-FAIL no-agent"
    exit 0
  fi
  # TR-085: the meta names an agent that is not OURS. A stale meta from an earlier
  # run (or a different evidence path) makes collect probe/destroy the wrong
  # machine while the real agent runs unrecorded. Refuse loudly instead.
  if [ -n "$ev" ] && [ "$ev" != "$evidence" ]; then
    append_collect_row FAIL "launch record $meta belongs to a different run: it names evidence=$ev but collect was asked for $evidence (agent=$agent NOT probed or destroyed — verify with \`bunker list --server $server\`)"
    echo "COLLECT-FAIL evidence-mismatch"
    exit 0
  fi
  # An agent id only exists for runs that reached the spawn step. If the record
  # predates this collect by more than the TTL the id cannot be live any more.
  local rec_launched rec_age
  rec_launched=$(sed -n 's/^launched=//p' "$meta" | head -1)
  if [ -n "$rec_launched" ]; then
    rec_age=$(python3 - "$rec_launched" <<'PY' 2>/dev/null || echo ""
import sys, datetime
try:
    t = datetime.datetime.strptime(sys.argv[1], '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=datetime.timezone.utc)
    print(int((datetime.datetime.now(datetime.timezone.utc) - t).total_seconds()))
except Exception:
    print("")
PY
)
    if [ -n "$rec_age" ] && [ "$rec_age" -gt 86400 ]; then
      append_collect_row FAIL "launch record $meta is ${rec_age}s old (>24h, TTL=$TTL) — agent=$agent cannot still be live; refusing to probe a stale id. Check \`bunker list --server $server\` and re-launch if the battery never ran"
      echo "COLLECT-FAIL stale-record"
      exit 0
    fi
  fi
  key="$HOME/.bunker/keys/$agent"
  # collect must stay SHORT: bound the agent ssh at BUNKER_QA_COLLECT_SSH_TIMEOUT
  # (120s) instead of agent_ssh's 1800s battery default, so a wedged host cannot
  # blow the tool cap again.
  local ssh_to="${BUNKER_QA_COLLECT_SSH_TIMEOUT:-120}"

  # ONE cheap probe classifies the remote state. The pgrep pattern is
  # bracket-escaped ([q]a-run\.sh) so this PROBE's own shell argv — which
  # contains the pattern text — can never match itself and report RUNNING forever.
  local probe
  probe=$(BUNKER_QA_SSH_TIMEOUT="$ssh_to" agent_ssh "if [ -f ~/qa-run.finished ]; then echo FINISHED; elif pgrep -f '[q]a-run\.sh' >/dev/null 2>&1; then echo RUNNING; else echo GONE; fi" 2>/dev/null | tail -1)

  case "$probe" in
    RUNNING)
      log "battery still running on agent=$agent ($SERVER) — agent left intact"
      echo "RUNNING agent=$agent evidence=$evidence"
      exit 0
      ;;
    FINISHED|GONE)
      : ;;  # marker present, or the process is gone → pull what it produced
    *)
      append_collect_row FAIL "cannot reach agent=$agent on $SERVER (ssh probe returned no state) — agent left intact"
      echo "COLLECT-FAIL unreachable-agent"
      exit 0
      ;;
  esac

  local tmp errf pull_rc
  tmp=$(mktemp); errf=$(mktemp)
  BUNKER_QA_SSH_TIMEOUT="$ssh_to" agent_ssh "cat ~/qa-evidence.jsonl 2>/dev/null" > "$tmp" 2>"$errf"
  pull_rc=$?
  if [ "$pull_rc" -ne 0 ] && [ ! -s "$tmp" ]; then
    append_collect_row FAIL "could not pull ~/qa-evidence.jsonl from agent=$agent ($SERVER) rc=$pull_rc: $(head -1 "$errf")"
    rm -f "$tmp" "$errf"
    echo "COLLECT-FAIL pull-failed"
    exit 0
  fi
  rm -f "$errf"

  # Merge-dedupe on the row "ts" field: re-calling collect after a successful
  # pull adds nothing. The merge is atomic (temp file + os.replace) so a crash
  # mid-merge cannot truncate the local evidence file.
  if [ -s "$tmp" ] && command -v python3 >/dev/null 2>&1; then
    python3 - "$evidence" "$tmp" <<'PY' || cat "$tmp" >> "$evidence"
import json, os, sys

dest, src = sys.argv[1], sys.argv[2]
seen, out = set(), []
for path in (dest, src):
    try:
        fh = open(path)
    except OSError:
        continue
    with fh:
        for line in fh:
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                row = json.loads(line)
            except Exception:
                continue
            key = row.get("ts")
            if key in seen:
                continue
            seen.add(key)
            out.append(json.dumps(row))
tmp_dest = dest + ".merge.tmp"
with open(tmp_dest, "w") as fh:
    fh.write("\n".join(out) + ("\n" if out else ""))
os.replace(tmp_dest, dest)
PY
  elif [ -s "$tmp" ]; then
    cat "$tmp" >> "$evidence"   # no python3: a duplicate row beats an unpulled battery
  fi
  local pulled
  pulled=$(grep -c '^{' "$tmp" 2>/dev/null || true)
  rm -f "$tmp"

  local detail="agent=$agent ($SERVER) — pulled ${pulled:-0} evidence row(s), merge-deduped by ts"
  if [ "$keep" = 1 ]; then
    detail="$detail; agent KEPT (launched with --keep) — destroy it manually"
  else
    # QA-BUNKER-19 b+c (2026-09-21): surfaced destroy + post-destroy absence
    # verify (qa_destroy_agent appends its own FAIL row to $evidence when the
    # destroy fails or the id still lists). Replaces the bare `|| true` swallow.
    # QA-BUNKER-39 (2026-09-30): qa_destroy_agent retries (3 attempts, backoff),
    # re-checks `bunker list` and reports TEARDOWN-GONE / TEARDOWN-RECOVERED
    # (destroy landed late) / TEARDOWN-LEAK. The collect summary line must
    # match reality — the old unconditional "temp agent destroyed" lied on
    # exactly the loaded-server path that leaked slot 7fb908b5 on las-02.
    local teardown
    teardown=$(qa_destroy_agent "$agent" "$evidence")
    case "$teardown" in
      TEARDOWN-GONE)        detail="$detail; temp agent destroyed" ;;
      TEARDOWN-RECOVERED)   detail="$detail; temp agent destroy reported failure but the agent is GONE from the server list — teardown recovered" ;;
      TEARDOWN-LEAK|*)      detail="$detail; TEMP AGENT LEAKED: destroy failed and agent=$agent still lists on $SERVER — slot holds until the ~4h TTL reap; manual recovery: bunker destroy $agent --server $SERVER" ;;
    esac
  fi
  append_collect_row OK "$detail"
  log "QA collect complete — evidence: $evidence"
  echo "COLLECTED agent=$agent rows=${pulled:-0} evidence=$evidence"
}

case "${1:-}" in
  status) status ;;
  run) run "$2" "${@:3}" ;;
  launch) launch "$2" "${@:3}" ;;
  collect) collect "${@:2}" ;;
  # test hook: print the generated script. `agent` is a placeholder here — the
  # real caller (run/launch) sets it before build_remote_script, and without a
  # default `set -u` aborts the heredoc expansion entirely (no output at all).
  # DETECT_BIN is defaulted to the same set even though the generated script
  # re-detects the start command on the agent (the agent-side ladder is the
  # authoritative runtime detection); the hook's default keeps the variable
  # contract complete for tests.
  __gen-remote) agent="${agent:-test-agent-0000}" DETECT_INSTALL=":" DETECT_CI=":" DETECT_NATIVE=":" DETECT_BIN="" DETECT_TAG_COUNT=0 DETECT_PREV_TAG="" DETECT_PREV_VERSION="" DETECT_PKG_NAME="" DETECT_PKG_ECOSYSTEM="" DETECT_GO_URL="" DETECT_CI_FILES="" DETECT_ACT_NOTE="" DETECT_UP_PREV_DIR="" build_remote_script "$2" "${3:-true}" "${4:-true}" "${5:-true}" ;;
  # QA-CRIER-20 test hook: detect_cmds is a pure function of the repo dir
  # (reads files, sets DETECT_* globals), so this is safe to run against any
  # directory. "$3" defaults to the same placeholder agent __gen-remote uses.
  # DETECT_BIN (QA-9ROUTER-22) is the host-side mirror of the generated
  # script's start-command ladder; empty = "not observable".
  #
  # BUNKER_QA_GEN_DETECT=1 (QA-9ROUTER-19/20, 2026-09-23): run the REAL
  # detection on the target dir before rendering, so the render carries the
  # actual QA_CI_SELECT / QA_ACT_NOTE / QA_PKG_NAME a battery run would ship.
  # Opt-in on purpose — the default hook stays a fixed-placeholder render so
  # existing callers keep byte-identical output.
  __detect-cmds) detect_cmds "$2" "${3:-test-agent-0000}"; printf 'DETECT_INSTALL=%s\nDETECT_NATIVE=%s\nDETECT_CI=%s\nDETECT_BIN=%s\nDETECT_PKG_NAME=%s\nDETECT_PKG_ECOSYSTEM=%s\nDETECT_CI_SELECT=%s\nDETECT_ACT_NOTE=%s\n' "$DETECT_INSTALL" "$DETECT_NATIVE" "$DETECT_CI" "$DETECT_BIN" "$DETECT_PKG_NAME" "$DETECT_PKG_ECOSYSTEM" "$DETECT_CI_FILES" "$DETECT_ACT_NOTE" ;;
  # QA-9ROUTER-19/20: render with the REAL detection for $2, so a test can
  # assert what a battery run would actually ship (see the hook's comment).
  __gen-remote-detected) agent="${agent:-test-agent-0000}"; detect_cmds "$2" "${3:-test-agent-0000}"; build_remote_script "$2" "$DETECT_INSTALL" "$DETECT_CI" "$DETECT_NATIVE" ;;
  *) echo "usage: bunker-qa.sh status | run <repo-dir> [--keep] | launch <repo-dir> [--keep] | collect --evidence <path>" ;;
esac
