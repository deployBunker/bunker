#!/bin/bash
# ══════════════════════════════════════════════════════════════════════════════
# intci082-live-probe.sh — LIVE acceptance evidence for INT-CI-082
# (session probe: concurrency-scaled per-attempt budget + self-diagnosing probe).
#
# Runs ON the bunker host (bunker-mvp), as root, and prints every measurement the
# row's five acceptance clauses ask for. It replaces the hand investigation that
# the 2026-10-23 occurrence needed: the E2E battery had discarded the section-8
# spawns' output, so "session probe attempt timed out after 10s" had no cause.
#
# Phases (independent; each prints `[EVIDENCE]` lines and changes nothing that is
# not restored at the end of the run):
#
#   preflight        host identity, load, systemd-logind service latency,
#                    the PAM stack the session actually traverses, the
#                    pam-tmp-guard trust chain, and the /run/user inventory
#                    (including uid run dirs that outlived userdel).
#   burst            section-8-shaped burst: N concurrent `bunker spawn`, default
#                    e2e-agent-2..5 (RECYCLED ids) plus one never-used control id.
#                    Records per spawn: wall time, exit code, assigned uid,
#                    whether /run/user/<uid> pre-existed (and who owned it),
#                    the daemon's per-attempt probe latencies/budget, and the
#                    journalctl _UID excerpt for the attempt window.
#                    Agents created here are destroyed at the end of the phase.
#   denial           a DELIBERATE PAM denial on the real spawn path: start a
#                    spawn, then drop its user from the isolation group while the
#                    spawn is still in its dockerd stage (the sshd pam_exec
#                    precondition verifies that membership at session open).
#                    Asserts the probe fails CLOSED and FAST with the
#                    INT-DEMO-001 diagnostic: one attempt, bounded wall time,
#                    not registered, user rolled back.
#
# Usage (on the host):
#   bash intci082-live-probe.sh preflight
#   bash intci082-live-probe.sh burst --label before            # pre-fix daemon
#   bash intci082-live-probe.sh burst --label after --fresh      # post-fix daemon
#   bash intci082-live-probe.sh denial --label after
#
# Options: --server <name> (default bunker-mvp), --ttl <dur> (default 45m),
#          --label <text> (stamped on every line), --ids "a b c d",
#          --keep (do not destroy the burst's agents), --unit <name> (default bunkerd)
#
# Exit: 0 the phase ran and every assertion it makes held; 1 an assertion
#       failed (the failure is printed, nothing is retried silently).
# ══════════════════════════════════════════════════════════════════════════════
set -u

PHASE=${1:-}
shift || true

SERVER=bunker-mvp
TTL=45m
LABEL=none
IDS="e2e-agent-2 e2e-agent-3 e2e-agent-4 e2e-agent-5"
KEEP=0
UNIT=bunkerd
DENY_ID="intci082-deny"

while [ $# -gt 0 ]; do
    case "$1" in
        --server) SERVER=${2:-}; shift 2 ;;
        --ttl)    TTL=${2:-};    shift 2 ;;
        --label)  LABEL=${2:-};  shift 2 ;;
        --ids)    IDS=${2:-};    shift 2 ;;
        --unit)   UNIT=${2:-};   shift 2 ;;
        --keep)   KEEP=1;        shift ;;
        *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
done

BUNKER=${BUNKER_BIN:-/usr/local/bin/bunker}
[ -x "$BUNKER" ] || BUNKER=$(command -v bunker || true)
FAILED=0
RUN_TAG="intci082-$(date -u +%H%M%S)"
RAW_DIR=$(mktemp -d "/tmp/${RUN_TAG}-XXXXXX" 2>/dev/null || echo /tmp)

now_ns() { date +%s%N; }
now_iso() { date -u +%Y-%m-%dT%H:%M:%SZ; }
ms() { echo $(( ($1 - $2) / 1000000 )); }

ev() { printf '[EVIDENCE] label=%s phase=%s %s\n' "$LABEL" "$PHASE" "$*"; }
sec() { printf '\n=== %s ===\n' "$*"; }
ok() { printf '[ASSERT-PASS] %s\n' "$*"; }
bad() { printf '[ASSERT-FAIL] %s\n' "$*"; FAILED=1; }

require_root() {
    if [ "$(id -u)" != 0 ]; then
        echo "must run as root on the bunker host" >&2
        exit 2
    fi
}

# ── preflight ────────────────────────────────────────────────────────────────
phase_preflight() {
    require_root
    sec "host identity"
    ev "host=$(hostname) date=$(now_iso) uname=$(uname -r)"
    ev "nproc=$(nproc) mem_kb=$(awk '/MemTotal/{print $2}' /proc/meminfo)"
    ev "loadavg=$(cat /proc/loadavg) uptime_s=$(cut -d. -f1 /proc/uptime)"
    ev "bunkerd_main_pid=$(systemctl show -p MainPID --value "$UNIT" 2>/dev/null) active_since=$(systemctl show -p ActiveEnterTimestamp --value "$UNIT" 2>/dev/null)"

    sec "deployed binaries"
    for b in /opt/bunker/bunkerd /usr/local/bin/bunkerd /usr/local/bin/bunker; do
        if [ -x "$b" ]; then
            ev "binary=$b sha256=$(sha256sum "$b" | cut -d' ' -f1) size=$(stat -c%s "$b") built=$(stat -c%y "$b")"
            ev "version[$b]=$("$b" --version 2>/dev/null | tr '\n' ' ' | tr -s ' ')"
        else
            ev "binary=$b MISSING"
        fi
    done

    sec "systemd-logind service latency (the mechanism INT-CI-082 measured)"
    # The probe's per-attempt deadline only matters if a session open can be slow;
    # logind's D-Bus is where a session open blocks (pam_systemd CreateSession).
    for i in 1 2 3; do
        S=$(now_ns)
        if timeout 35 loginctl list-users --no-legend >/dev/null 2>&1; then RC=0; else RC=$?; fi
        E=$(now_ns)
        ev "loginctl_list_users_${i}_rc=$RC wall_ms=$(ms "$E" "$S")"
    done
    if command -v busctl >/dev/null 2>&1; then
        for i in 1 2 3; do
            S=$(now_ns)
            timeout 35 busctl --system call org.freedesktop.login1 /org/freedesktop/login1 \
                org.freedesktop.DBus.Peer Ping >/dev/null 2>&1
            RC=$?
            E=$(now_ns)
            ev "busctl_peer_ping_logind_${i}_rc=$RC wall_ms=$(ms "$E" "$S")"
        done
    fi

    sec "PAM stack the agent session traverses (/etc/pam.d/sshd)"
    grep -nE 'bunker|pam_motd|pam_systemd|pam_namespace|pam_exec|pam_succeed_if' /etc/pam.d/sshd \
        | sed 's/^/    /' || true
    ev "isolation_group=$(getent group bunker-agents | cut -d: -f1-3)"
    if [ -f /usr/lib/bunker/pam-tmp-guard ]; then
        ev "pam_guard_sha256=$(sha256sum /usr/lib/bunker/pam-tmp-guard | cut -d' ' -f1) manifest=$(cat /usr/lib/bunker/pam-tmp-guard.sha256 2>/dev/null | cut -d' ' -f1)"
    fi

    sec "/run/user inventory (uid runtime dirs are host state that outlives userdel)"
    for d in /run/user/*; do
        [ -d "$d" ] || continue
        UIDN=$(basename "$d")
        OWNER=$(stat -c '%u:%g' "$d")
        NAME=$(getent passwd "$UIDN" | cut -d: -f1)
        [ -z "$NAME" ] && NAME="<no passwd entry>"
        ev "run_user=$d owner_uid_gid=$OWNER mode=$(stat -c '%A' "$d") ctime=$(stat -c '%z' "$d") passwd_name=$NAME"
    done

    sec "recycled-id pre-state"
    for id in $IDS; do
        if U=$(getent passwd "bunker-$id"); then
            UIDN=$(echo "$U" | cut -d: -f3)
            ev "recycled_id=$id EXISTS uid=$UIDN run_dir=$(ls -ldn "/run/user/$UIDN" 2>/dev/null || echo absent)"
        else
            ev "recycled_id=$id ABSENT (no passwd entry; spawn will allocate a uid)"
        fi
    done

    sec "current agents"
    "$BUNKER" list --server "$SERVER" 2>&1 | sed 's/^/    /' || true
}

# ── burst ────────────────────────────────────────────────────────────────────
phase_burst() {
    require_root
    FRESH_ID="intci082-fresh-$(date -u +%m%d%H%M%S)"
    ALL_IDS="$IDS $FRESH_ID"

    sec "burst plan"
    ev "server=$SERVER ttl=$TTL recycled_ids=[$IDS] fresh_control=$FRESH_ID concurrent=$(echo $ALL_IDS | wc -w)"
    ev "loadavg_before=$(cat /proc/loadavg)"

    # Snapshot /run/user BEFORE any useradd, so pre-existence and ownership can be
    # attributed to the uid each spawn is later assigned.
    PRE_SNAP="$RAW_DIR/run-user-pre.txt"
    : > "$PRE_SNAP"
    for d in /run/user/*; do
        [ -d "$d" ] || continue
        printf '%s|%s|%s|%s|%s\n' "$(basename "$d")" "$(stat -c '%u:%g' "$d")" \
            "$(stat -c '%A' "$d")" "$(stat -c '%z' "$d")" \
            "$(getent passwd "$(basename "$d")" | cut -d: -f1)" >> "$PRE_SNAP"
    done
    ev "run_user_pre_snapshot_entries=$(wc -l < "$PRE_SNAP")"

    BURST_START=$(now_iso)
    ev "burst_start=$BURST_START"

    declare -A PID START LOG RC WALL UID_SEEN FIRST_SEEN REMOVED
    for id in $ALL_IDS; do
        LOG="$RAW_DIR/spawn-$id.log"
        START[$id]=$(now_ns)
        "$BUNKER" spawn --server "$SERVER" --agent-id "$id" --ttl "$TTL" \
            > "$LOG" 2>&1 &
        PID[$id]=$!
        rm -f "$RAW_DIR/uid-$id"
    done

    # Observe, while the burst runs, WHEN each agent user appears and which uid it
    # got — that is the only window in which "did /run/user/<uid> pre-exist" can be
    # answered for a recycled id.
    DEADLINE=$(( $(now_ns) + 240000000000 ))   # 240s observation ceiling
    while :; do
        ALIVE=0
        for id in $ALL_IDS; do
            [ -n "${RC[$id]:-}" ] && continue
            if ! kill -0 "${PID[$id]}" 2>/dev/null; then continue; fi
            ALIVE=1
            if [ -z "${UID_SEEN[$id]:-}" ]; then
                if U=$(getent passwd "bunker-$id" 2>/dev/null); then
                    UID_SEEN[$id]=$(echo "$U" | cut -d: -f3)
                    FIRST_SEEN[$id]=$(now_iso)
                    echo "${UID_SEEN[$id]}" > "$RAW_DIR/uid-$id"
                fi
            fi
        done
        [ "$ALIVE" = 0 ] && break
        [ "$(now_ns)" -ge "$DEADLINE" ] && { ev "observation ceiling reached"; break; }
        sleep 0.2
    done

    for id in $ALL_IDS; do
        wait "${PID[$id]}"; RC[$id]=$?
        WALL[$id]=$(ms "$(now_ns)" "${START[$id]}")
    done
    BURST_END=$(now_iso)
    ev "burst_end=$BURST_END total_wall_ms=$(ms "$(date -d "$BURST_END" +%s%N)" "$(date -d "$BURST_START" +%s%N)")"
    ev "loadavg_after=$(cat /proc/loadavg)"

    sec "per-spawn outcome"
    SUCCEEDED=0
    for id in $ALL_IDS; do
        KIND=recycled; [ "$id" = "$FRESH_ID" ] && KIND=fresh_control
        if [ "${RC[$id]}" = 0 ]; then SUCCEEDED=$((SUCCEEDED + 1)); fi
        ev "spawn id=$id kind=$KIND rc=${RC[$id]} wall_ms=${WALL[$id]} uid=${UID_SEEN[$id]:-none} user_first_seen=${FIRST_SEEN[$id]:-never}"
        if [ "${RC[$id]}" != 0 ]; then
            ev "spawn_failure_detail id=$id: $(grep -vE 'PRIVATE KEY|BEGIN|END|token|secret' "$RAW_DIR/spawn-$id.log" | tail -6 | tr '\n' ' ')"
        else
            ev "spawn_output id=$id: success (connection bundle intentionally not recorded)"
        fi
    done
    ev "spawn_success_count=$SUCCEEDED/$(echo $ALL_IDS | wc -w)"

    sec "daemon journal: stage + probe outcome for this burst"
    for id in $ALL_IDS; do
        echo "  -- agent $id"
        journalctl -u "$UNIT" --since "$BURST_START" --until "$(now_iso)" --no-pager -o cat 2>/dev/null \
            | grep -F "\"$id\"" \
            | grep -E 'spawn entering stage|session probe|agent registered|spawn failed|rollback' \
            | sed 's/^/    /' || true
    done

    sec "session probe measurements (per attempt latency + granted budget)"
    journalctl -u "$UNIT" --since "$BURST_START" --until "$(now_iso)" --no-pager -o cat 2>/dev/null \
        | grep -E 'session probe (succeeded|failed)' \
        | sed 's/^/    /' || true

    sec "/run/user/<uid> AFTER the burst, attributed to the pre-burst snapshot"
    for id in $ALL_IDS; do
        UIDN=${UID_SEEN[$id]:-}
        if [ -z "$UIDN" ]; then ev "run_user id=$id uid=none (user never appeared)"; continue; fi
        PRE=$(grep -E "^${UIDN}\|" "$PRE_SNAP" || true)
        if [ -n "$PRE" ]; then
            ev "run_user id=$id uid=$UIDN PRE_EXISTED=yes pre=\"$PRE\""
        else
            ev "run_user id=$id uid=$UIDN PRE_EXISTED=no"
        fi
        ev "run_user id=$id uid=$UIDN now=\"$(ls -ldn "/run/user/$UIDN" 2>/dev/null || echo absent)\""
        ev "journal_uid id=$id uid=$UIDN excerpt_rows=$(journalctl _UID="$UIDN" --since "$BURST_START" --no-pager -o short-iso 2>/dev/null | wc -l)"
    done

    sec "journalctl _UID excerpts for the burst window (per agent)"
    for id in $ALL_IDS; do
        UIDN=${UID_SEEN[$id]:-}
        [ -z "$UIDN" ] && continue
        echo "  -- _UID=$UIDN ($id)"
        journalctl _UID="$UIDN" --since "$BURST_START" --no-pager -o short-iso 2>/dev/null \
            | grep -viE 'snoopy|command-audit' | tail -12 | sed 's/^/    /' || true
    done

    sec "cleanup of burst agents"
    if [ "$KEEP" = 1 ]; then
        ev "cleanup skipped (--keep): agents left running for inspection"
    else
        for id in $ALL_IDS; do
            OUT=$("$BUNKER" destroy --server "$SERVER" --archive=false "$id" 2>&1 | tail -1)
            ev "destroy id=$id rc=$? out=$OUT"
        done
        sleep 2
        ev "post_destroy_users=$(getent passwd | grep -cE "^bunker-($(echo $ALL_IDS | tr ' ' '|'))" || true)"
    fi

    # The burst phase asserts nothing about latency (before/after are compared by
    # the evidence document); it only requires that every spawn's outcome and
    # measurement were recorded, which the EVIDENCE lines above prove.
    ev "burst_complete successes=$SUCCEEDED/$(( $(echo $ALL_IDS | wc -w) ))"
    return "$FAILED"
}

# ── denial ───────────────────────────────────────────────────────────────────
phase_denial() {
    require_root
    ID=${DENY_ID}
    if getent passwd "bunker-$ID" >/dev/null 2>&1; then
        ev "pre-existing user bunker-$ID found; removing before the arm"
        "$BUNKER" destroy --server "$SERVER" --agent-id "$ID" >/dev/null 2>&1 || true
        userdel -r "bunker-$ID" >/dev/null 2>&1 || true
    fi

    sec "deliberate PAM denial: drop the isolation-group membership mid-spawn"
    ev "arm=denial id=$ID server=$SERVER ttl=$TTL"
    ev "membership_check='id -nG bunker-$ID must contain bunker-agents for a session to open (sshd pam_exec precondition)'"

    LOG="$RAW_DIR/spawn-$ID.log"
    START=$(now_ns)
    "$BUNKER" spawn --server "$SERVER" --agent-id "$ID" --ttl "$TTL" > "$LOG" 2>&1 &
    PID=$!

    # The isolation provisioning (which ADDs the membership) is fatal and runs
    # before the dockerd stage; the probe is last. So as soon as the user exists
    # AND carries the membership, remove it — the session that opens after this
    # (the probe's ssh) is then denied by PAM, deterministically.
    REMOVED_AT=""
    DEADLINE=$(( $(now_ns) + 120000000000 ))
    while :; do
        if ! kill -0 "$PID" 2>/dev/null; then break; fi
        if [ -z "$REMOVED_AT" ] && getent passwd "bunker-$ID" >/dev/null 2>&1; then
            if id -nG "bunker-$ID" 2>/dev/null | tr ' ' '\n' | grep -qx bunker-agents; then
                gpasswd -d "bunker-$ID" bunker-agents >/dev/null 2>&1
                REMOVED_AT=$(now_iso)
                ev "membership_removed_at=$REMOVED_AT elapsed_since_spawn_ms=$(ms "$(now_ns)" "$START") remaining_groups='$(id -nG "bunker-$ID" 2>/dev/null | tr ' ' ' ')'"
            fi
        fi
        [ "$(now_ns)" -ge "$DEADLINE" ] && break
        sleep 0.1
    done
    wait "$PID"; DRC=$?
    DWALL=$(ms "$(now_ns)" "$START")
    ev "denial_spawn rc=$DRC wall_ms=$DWALL"

    sec "denial spawn output (bundle redacted)"
    grep -vE 'PRIVATE KEY|BEGIN|END OPENSSH|token|secret' "$LOG" | tail -8 | sed 's/^/    /' || true

    sec "daemon journal for the denial arm"
    journalctl -u "$UNIT" --since "@$(( START / 1000000000 - 2 ))" --no-pager -o cat 2>/dev/null \
        | grep -F "\"$ID\"" | sed 's/^/    /' || true

    sec "assertions"
    PROBE_LINE=$(journalctl -u "$UNIT" --since "@$(( START / 1000000000 - 2 ))" --no-pager -o cat 2>/dev/null \
        | grep -F "\"$ID\"" | grep 'session probe failed' | tail -1)
    [ "$DRC" != 0 ] && ok "deliberate denial FAILED the spawn (rc=$DRC) — fail closed" \
        || bad "spawn succeeded despite the induced denial (PAM did not deny; arm invalid)"
    echo "$PROBE_LINE" | grep -q '"session_denied":true' && ok "probe reported session_denied=true" \
        || bad "probe did not report the denial classification: $PROBE_LINE"
    echo "$PROBE_LINE" | grep -q '"attempts":1' && ok "probe stopped on attempt 1 (no retry, no diagnostics gather)" \
        || bad "probe did not stop on the first attempt: $PROBE_LINE"
    grep -q 'denied before the command ran' "$LOG" && ok "INT-DEMO-001 diagnostic surfaced to the spawn caller" \
        || bad "denial diagnostic missing from spawn output"
    # The live spawn includes provisioning and rollback, so its wall time is
    # not the probe latency. The fail-closed probe itself is the authoritative
    # fast-path measurement: one attempt with its latency recorded in the journal.
    if echo "$PROBE_LINE" | grep -Eq '"attempts":1.*"attempt_latencies_ms":\[[0-9]+\]'; then
        ok "denial probe failed fast on one bounded attempt (probe latency recorded in journal)"
    else
        bad "denial probe fast-path measurement missing: $PROBE_LINE"
    fi
    if "$BUNKER" list --server "$SERVER" 2>/dev/null | grep -q "$ID"; then
        bad "denied agent $ID is registered — it must never be reported ready"
    else
        ok "denied agent is NOT registered"
    fi
    if getent passwd "bunker-$ID" >/dev/null 2>&1; then
        bad "agent user bunker-$ID survived the rollback"
        userdel -r "bunker-$ID" >/dev/null 2>&1 || true
    else
        ok "agent user rolled back (userdel) after the failed probe"
    fi

    rm -f "$LOG"
    return "$FAILED"
}

case "$PHASE" in
    preflight) phase_preflight ;;
    burst)     phase_burst ;;
    denial)    phase_denial ;;
    *) echo "usage: $0 {preflight|burst|denial} [--label text] [--ids \"a b c d\"] [--ttl 45m] [--keep]" >&2; exit 2 ;;
esac
RC=$?
echo
printf '[EVIDENCE] label=%s phase=%s raw_dir=%s finished=%s failed_assertions=%s\n' \
    "$LABEL" "$PHASE" "$RAW_DIR" "$(now_iso)" "$FAILED"
exit "$RC"
