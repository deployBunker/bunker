#!/bin/bash
# ══════════════════════════════════════════════════════════════════════════════
# runner-stranded-reap.sh — job-level pre-flight reap of STRANDED GitHub Actions
# runner workers (INT-CI-081 acceptance 3).
#
# Why this exists. bunker-mvp carries the repo's only self-hosted runner, so a
# cancelled or superseded run does not necessarily stop executing on the host:
# measured 2026-10-23T03:48Z, a `Runner.Worker spawnclient 157 160` started at
# 03:31:50 was still running `bash scripts/root-suite.sh` with `gh run list`
# showing NO active run — its TestSpawn loop kept spawning agents every ~24 s
# and each spawn re-applied /run/bunker mode 0700, silently reverting the
# INT-CI-080 repair twice inside 25 s. GitHub's cancellation is a request to the
# runner, not a guarantee about the host's process tree (the unit is even
# KillMode=process, so systemd never sweeps children either).
#
# What it does. At the start of a self-hosted job, kill every Runner.Worker
# process tree that is NOT an ancestor of this step. On a single runner there is
# exactly one legitimate worker — the one executing this job — so anything else
# is residue from an earlier job that outlived its cancellation. Descendants are
# signalled leaf-first (TERM, then KILL after a bounded wait), which also takes
# the `bunker` / `bunkerd` / `go test` children a stranded battery had running.
#
# Safety rails, in order:
#   * self-ancestry is never a candidate (walked from this PID up through ppid),
#   * a worker younger than --min-age (default 30 s) is left alone — it may be a
#     job that is still being set up,
#   * PID 1 and the Runner.Listener are never matched (the Listener is the
#     service itself; a worker's args are what is matched),
#   * the script never fails the job: every failure prints WARN and exits 0
#     unless --strict is passed (manual runs want a real exit code).
#
# --dry-run prints what WOULD be reaped and kills nothing; that is the mode to
# use when diagnosing a runner by hand.
#
# Seams for tests (scripts/test-runner-stranded-reap.sh): RUNNER_REAP_PS_SOURCE
# points at a fixture file in `ps -eo pid=,ppid=,etimes=,args=` format and
# RUNNER_REAP_KILL_CMD replaces the `kill` binary; production never sets either.
#
# Exit codes: 0 always (except --strict: 1 on a fatal error).
# ══════════════════════════════════════════════════════════════════════════════
set -uo pipefail

PROG=runner-stranded-reap.sh
MIN_AGE=${RUNNER_REAP_MIN_AGE:-30}
SELF_PID=${RUNNER_REAP_SELF_PID:-$$}
PS_SOURCE=${RUNNER_REAP_PS_SOURCE:-}
KILL_CMD=${RUNNER_REAP_KILL_CMD:-kill}
TERM_WAIT=${RUNNER_REAP_TERM_WAIT:-10}
KILL_WAIT=${RUNNER_REAP_KILL_WAIT:-5}

DRY_RUN=0
STRICT=0

say()  { printf '%s: %s\n' "$PROG" "$*"; }
warn() { printf '%s: warning: %s\n' "$PROG" "$*" >&2; }

usage() {
	cat <<'EOF'
runner-stranded-reap.sh — reap stranded GitHub Actions runner workers.

Usage:
  scripts/runner-stranded-reap.sh [--dry-run] [--strict] [--min-age SECONDS]

Intended as the FIRST step of a self-hosted job: any Runner.Worker that is not
an ancestor of this step is residue from a cancelled/superseded job and is
reaped (its whole process tree, leaf-first).

  --dry-run           report what would be reaped; kill nothing
  --strict            exit 1 on a fatal error (default: warn and exit 0, so the
                      mitigation can never fail a job)
  --min-age SECONDS   skip workers younger than this (default 30)

Environment: RUNNER_REAP_MIN_AGE, RUNNER_REAP_TERM_WAIT, RUNNER_REAP_KILL_WAIT.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--dry-run) DRY_RUN=1; shift ;;
	--strict) STRICT=1; shift ;;
	--min-age) MIN_AGE=${2:-}; shift 2 ;;
	-h|--help) usage; exit 0 ;;
	*) usage >&2; warn "unknown argument: $1"; [ "$STRICT" -eq 1 ] && exit 1; exit 0 ;;
	esac
done

fatal() {
	warn "$*"
	if [ "$STRICT" -eq 1 ]; then exit 1; fi
	exit 0
}

case "$MIN_AGE" in
''|*[!0-9]*) fatal "invalid --min-age '$MIN_AGE' (expected an integer number of seconds)" ;;
esac

# ── snapshot: pid ppid etimes args ───────────────────────────────────────────
snapshot() {
	if [ -n "$PS_SOURCE" ]; then
		cat "$PS_SOURCE"
		return
	fi
	ps -eo pid=,ppid=,etimes=,args= 2>/dev/null
}

SNAP=$(snapshot) || fatal "cannot read the process table (ps failed)"
[ -n "$SNAP" ] || fatal "empty process table snapshot"

declare -A PPID_OF=() AGE_OF=() ARGS_OF=() CHILDREN=()
while read -r pid ppid etimes args; do
	[ -n "${pid:-}" ] || continue
	case "$pid" in *[!0-9]*) continue ;; esac
	PPID_OF[$pid]=${ppid:-0}
	AGE_OF[$pid]=${etimes:-0}
	ARGS_OF[$pid]=${args:-}
	CHILDREN[${ppid:-0}]="${CHILDREN[${ppid:-0}]:-} $pid"
done <<<"$SNAP"

# ── self ancestry: never a candidate ────────────────────────────────────────
declare -A SELF_ANC=()
hop=0
p=$SELF_PID
while [ -n "${p:-}" ] && [ "$p" != "0" ] && [ -z "${SELF_ANC[$p]:-}" ] && [ "$hop" -lt 64 ]; do
	SELF_ANC[$p]=1
	p=${PPID_OF[$p]:-}
	hop=$((hop + 1))
done

# ── candidates: Runner.Worker processes that are not our own ancestor ───────
is_runner_worker() {
	local args=$1 first
	[ -n "$args" ] || return 1
	first=${args%% *}
	case "$first" in
	Runner.Worker|*/Runner.Worker|Runner.Worker.exe|*/Runner.Worker.exe) return 0 ;;
	esac
	# A wrapper (`sh -c ...`, `sudo ... Runner.Worker ...`) still names it.
	case " $args " in
	*" Runner.Worker "*) return 0 ;;
	esac
	return 1
}

# descendants_of emits a post-order list (children before parents) so a tree is
# signalled leaf-first.
descendants_of() {
	local root=$1 child
	for child in ${CHILDREN[$root]:-}; do
		descendants_of "$child"
		printf '%s\n' "$child"
	done
}

STRANDED=()
KEPT_SELF=0
KEPT_YOUNG=0
inspected=0
for pid in "${!ARGS_OF[@]}"; do
	is_runner_worker "${ARGS_OF[$pid]}" || continue
	inspected=$((inspected + 1))
	if [ -n "${SELF_ANC[$pid]:-}" ]; then
		KEPT_SELF=$((KEPT_SELF + 1))
		continue
	fi
	age=${AGE_OF[$pid]:-0}
	case "$age" in *[!0-9]*) age=0 ;; esac
	if [ "$age" -lt "$MIN_AGE" ]; then
		KEPT_YOUNG=$((KEPT_YOUNG + 1))
		continue
	fi
	STRANDED+=("$pid")
done

if [ "${#STRANDED[@]}" -eq 0 ]; then
	say "no stranded Runner.Worker (inspected ${inspected}: self=${KEPT_SELF}, younger-than-${MIN_AGE}s=${KEPT_YOUNG})"
	exit 0
fi

# ── reap: TERM the trees leaf-first, then KILL survivors ────────────────────
pids_alive() {
	local alive=() pid
	for pid in "$@"; do
		if [ -n "${ARGS_OF[$pid]:-}" ] && kill -0 "$pid" 2>/dev/null; then alive+=("$pid"); fi
	done
	printf '%s\n' "${alive[@]:-}"
}

reap_tree() {
	local root=$1 pid
	local tree
	tree=$(descendants_of "$root")
	for pid in $tree; do
		if [ "$DRY_RUN" -eq 1 ]; then
			say "would terminate child $pid (${ARGS_OF[$pid]:-})"
		else
			$KILL_CMD -TERM "$pid" 2>/dev/null || warn "TERM $pid failed"
		fi
	done
	if [ "$DRY_RUN" -eq 1 ]; then
		say "would terminate stranded worker $root (${ARGS_OF[$root]})"
		return
	fi
	$KILL_CMD -TERM "$root" 2>/dev/null || warn "TERM $root failed"
	say "TERM sent to stranded worker $root and ${tree//[[:space:]]/ } descendant(s)"
}

for pid in "${STRANDED[@]}"; do
	reap_tree "$pid"
done

if [ "$DRY_RUN" -eq 1 ]; then
	say "dry run: ${#STRANDED[@]} stranded worker tree(s) would be reaped: ${STRANDED[*]}"
	exit 0
fi

# With the real kill binary, prove the trees are gone: poll up to TERM_WAIT for
# TERM, then KILL survivors and poll again up to KILL_WAIT. With an injected
# KILL_CMD (tests) the fixture is static, so the signalling itself is the proof.
if [ -z "${RUNNER_REAP_KILL_CMD:-}" ] && [ -z "$PS_SOURCE" ]; then
	deadline=$((SECONDS + TERM_WAIT))
	while [ "$SECONDS" -lt "$deadline" ]; do
		sleep 1
		remaining=$(pids_alive "${STRANDED[@]}")
		[ -z "$remaining" ] && break
	done
	if [ -n "${remaining:-}" ]; then
		for pid in $remaining; do
			warn "worker $pid survived TERM after ${TERM_WAIT}s; sending KILL"
			kill -KILL "$pid" 2>/dev/null || true
		done
		deadline=$((SECONDS + KILL_WAIT))
		while [ "$SECONDS" -lt "$deadline" ]; do
			sleep 1
			remaining=$(pids_alive "${STRANDED[@]}")
			[ -z "$remaining" ] && break
		done
	fi
	if [ -n "${remaining:-}" ]; then
		warn "still alive after KILL: ${remaining//[[:space:]]/ } — the runner needs operator attention"
		if [ "$STRICT" -eq 1 ]; then exit 1; fi
		exit 0
	fi
fi

say "reaped ${#STRANDED[@]} stranded runner worker tree(s): ${STRANDED[*]}"
exit 0
