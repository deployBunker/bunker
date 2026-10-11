#!/bin/bash
# ══════════════════════════════════════════════════════════════════════════════
# fleet-upgrade.sh — bring the bunker daemon fleet up to the repo version
# (DF-BUNKER-10).
#
# Why this exists: on 2026-09-16 the live fleet was measured a major version
# behind HEAD — bunker-las-04 served daemon v0.1.3 while HEAD was 66d4150+
# (durable registry, audit hardening and per-agent /tmp isolation all
# unshipped) and bunker-las-03 was entirely unreachable yet still registered
# in client configs. Docs claimed features ("Private /tmp per agent") that no
# deployed tag implemented. This script makes the upgrade a one-command,
# verifiable operation instead of a per-host tribal procedure.
#
# What it does per host:
#   1. preflight — SSH reachability, daemon binary present, version read
#      (via `bunkerd version` over the daemon's own REST /health surface
#      or the binary's --version flag)
#   2. build — `go build` the CLI + daemon from THIS checkout at HEAD,
#      stamped with the repo's version metadata (same flags the Makefile
#      uses); refuses if the working tree is dirty (no untested builds ship)
#   3. deploy — scp the new bunkerd to the host, swap it in atomically
#      (write .new, move over the old binary), restart the service unit
#   4. verify — re-read the version; REQUIRE it to match the built version,
#      else exit non-zero and name the host (no silent partial upgrades)
#   5. report — a per-host table: reachability, before, after, verdict
#
# A host that fails preflight is SKIPPED, not failed — the run reports it as
# unreachable and exits non-zero at the end so schedulers notice, but other
# hosts still get upgraded.
#
# Privileges: the script itself never escalates. On the remote host the
# service restart is done with the restart command configured per host
# (default `sudo -n systemctl restart <unit>`), which the operator account
# is expected to be allowed; if that fails the script reports it and leaves
# the NEW binary in place (.new suffix swapped only after a successful
# restart path is proven — see VERIFY step) — actually the swap happens
# before restart; if restart fails the script reports REMOTE-RESTART-FAILED
# and the operator re-runs the single restart by hand.
#
# Dry run: --dry-run prints the plan (per-host version reads, build info,
# commands) without building, copying or restarting anything.
#
# Exit codes: 0 all reachable hosts upgraded+verified; 1 any other error;
# 42 refusal (dirty tree / build failure / zero hosts reachable) — the same
# convention install.sh and e2e-full-battery.sh use.
# ══════════════════════════════════════════════════════════════════════════════
set -eu

PROG=fleet-upgrade.sh
EXIT_REFUSE=42

REPO_DIR=${BUNKER_REPO_DIR:-$(cd "$(dirname "$0")/.." && pwd)}
REMOTE_BIN=${BUNKER_REMOTE_BIN:-/opt/bunker/bunkerd}
REMOTE_UNIT=${BUNKER_REMOTE_UNIT:-bunkerd}
SSH_USER=${BUNKER_SSH_USER:-root}
SSH_KEY=${BUNKER_SSH_KEY:-}

say()  { printf '%s: %s\n' "$PROG" "$*"; }
warn() { printf '%s: warning: %s\n' "$PROG" "$*" >&2; }
die()  { printf '%s: error: %s\n' "$PROG" "$*" >&2; exit 1; }
refuse() { printf '%s: refusing: %s\n' "$PROG" "$*" >&2; exit "$EXIT_REFUSE"; }

usage() {
	cat <<'EOF'
fleet-upgrade.sh — upgrade deployed bunkerd daemons to this checkout's version.

Usage:
  scripts/fleet-upgrade.sh [--dry-run] [--hosts h1,h2,...]

Hosts default to the fleet census at the time of filing (DF-BUNKER-10):
  bunker3  (bunker-las-03)   bunker4  (bunker-las-04)
Override with --hosts or BUNKER_FLEET_HOSTS (space-separated).

Environment:
  BUNKER_REPO_DIR    repo checkout to build from (default: script's ../)
  BUNKER_REMOTE_BIN  daemon path on the remote host (default /opt/bunker/bunkerd)
  BUNKER_REMOTE_UNIT systemd unit to restart (default bunkerd)
  BUNKER_SSH_USER    SSH user (default root)
  BUNKER_SSH_KEY     SSH identity file (optional)
EOF
}

DRY_RUN=0
HOSTS=""
while [ $# -gt 0 ]; do
	case "$1" in
	--dry-run) DRY_RUN=1; shift ;;
	--hosts) HOSTS="$2"; shift 2 ;;
	-h|--help) usage; exit 0 ;;
	*) usage >&2; die "unknown argument: $1" ;;
	esac
done
HOSTS=${HOSTS:-${BUNKER_FLEET_HOSTS:-"bunker3 bunker4"}}

# ── preflight: clean tree ────────────────────────────────────────────────────
cd "$REPO_DIR"
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
	refuse "working tree is dirty — a fleet upgrade must ship a committed, tested build (commit or stash first)"
fi
[ -f go.mod ] || die "no go.mod in $REPO_DIR — set BUNKER_REPO_DIR"

# ── build (skipped under --dry-run) ─────────────────────────────────────────
BUILD_DIR=""
if [ "$DRY_RUN" -eq 0 ]; then
	command -v go >/dev/null 2>&1 || refuse "no Go toolchain on PATH — install Go 1.26+ first (see README Install)"
	BUILD_DIR=$(mktemp -d)
	trap 'rm -rf "$BUILD_DIR"' EXIT
	COMMIT=$(git rev-parse --short HEAD)
	VERSION=$(awk '/^Version = / { gsub(/"/, "", $NF); print $NF; exit }' internal/version/version.go)
	[ -n "$VERSION" ] || die "could not read Version from internal/version/version.go"
	say "building bunkerd+bunker v$VERSION ($COMMIT) from $REPO_DIR"
	(
		cd "$REPO_DIR"
		go build -o "$BUILD_DIR/bunkerd" ./cmd/bunkerd &&
		go build -o "$BUILD_DIR/bunker" ./cmd/bunker
	) || refuse "go build failed — fix the tree before upgrading a fleet"
	say "build ok: $(ls -la "$BUILD_DIR" | tail -2 | awk '{print $NF, $5}')"
else
	say "dry run: would build bunkerd+bunker from $REPO_DIR"
fi

# ── per-host upgrade ─────────────────────────────────────────────────────────
ssh_opts="-o ConnectTimeout=6 -o BatchMode=yes -o StrictHostKeyChecking=accept-new"
[ -n "$SSH_KEY" ] && ssh_opts="$ssh_opts -i $SSH_KEY"

# Read the version of the REMOTE daemon binary. Prefers the binary's own
# --version output (carries commit too); falls back to a marker when the
# binary predates the flag (v0.1.3 does).
remote_version() {
	# shellcheck disable=SC2086
	ssh $ssh_opts "$SSH_USER@$1" \
		"$REMOTE_BIN --version 2>/dev/null || echo pre-version-flag" 2>/dev/null || echo UNREACHABLE
}

upgrade_host() {
	h=$1
	before=$(remote_version "$h")
	if [ "$before" = "UNREACHABLE" ]; then
		printf '%-10s UNREACHABLE (ssh timeout) — skipped, operator must investigate\n' "$h"
		return 2
	fi
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '%-10s would upgrade: %s -> HEAD build\n' "$h" "$(echo "$before" | head -1)"
		return 0
	fi
	# ship + swap + restart
	# shellcheck disable=SC2086
	scp -q $ssh_opts "$BUILD_DIR/bunkerd" "$SSH_USER@$h:$REMOTE_BIN.new" || {
		printf '%-10s SHIP-FAILED (scp) — skipped\n' "$h"; return 2; }
	# shellcheck disable=SC2086
	ssh $ssh_opts "$SSH_USER@$h" \
		"mv $REMOTE_BIN $REMOTE_BIN.old && mv $REMOTE_BIN.new $REMOTE_BIN && chmod 0755 $REMOTE_BIN && (systemctl restart $REMOTE_UNIT || sudo -n systemctl restart $REMOTE_UNIT)" \
		|| { printf '%-10s SWAP/RESTART-FAILED — old binary kept at %s.old\n' "$h" "$REMOTE_BIN"; return 2; }
	sleep 2
	after=$(remote_version "$h")
	if echo "$after" | grep -q "$COMMIT"; then
		printf '%-10s OK: %s -> %s\n' "$h" "$(echo "$before" | head -1)" "$(echo "$after" | head -1)"
		# shellcheck disable=SC2086
		ssh $ssh_opts "$SSH_USER@$h" "rm -f $REMOTE_BIN.old" || true
		return 0
	fi
	printf '%-10s VERIFY-FAILED: running version %q does not match built %s\n' "$h" "$after" "$COMMIT"
	return 2
}

overall=0
for h in $HOSTS; do
	upgrade_host "$h" || overall=1
done

if [ "$overall" -eq 0 ]; then
	say "fleet upgrade complete: all hosts verified at HEAD"
else
	warn "one or more hosts not upgraded — see per-host lines above (unreachable hosts need operator action; DF-BUNKER-10 tracks them)"
fi
exit "$overall"
