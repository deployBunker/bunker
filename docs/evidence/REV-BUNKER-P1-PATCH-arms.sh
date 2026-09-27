#!/bin/sh
# REV-BUNKER-P1-PATCH — the arms, the RED, the negative control, and the live
# daemon reproduction. One script, so every claim in the evidence bundle comes
# from the same files.
#
#   green    the FIXED tree: the sweep-guard cells (agent arms + GREEN cells +
#            registry provenance + config surface) must all PASS.
#   unfixed  the tree AS FILED: the three product files this row changed are
#            replaced by the blobs of the base commit (default 4879343, the
#            commit the row was filed on) and the row's GREEN-only test files are
#            set aside, so the DEFECT cells run against the filed code with the
#            FINAL test text. They must FAIL there, and the three non-vacuity
#            controls must still PASS — which is what attributes the failures to
#            this row's defect rather than to the harness. Every swap and every
#            restore is sha256-asserted.
#   noguard  negative control: the guard is NEUTERED by patch on the FIXED tree
#            (the call site in runOrphanWalk is forced to "no refusal"), which is
#            the filed defect and nothing else. The DEFECT cells must go FAIL and
#            the controls must stay PASS; the mutation is reverted from a byte
#            copy whose sha256 is re-checked after the copy back.
#   suites   the whole tree's gates on the fixed tree: gofmt -l, go build, go vet,
#            go test ./... — the row's acceptance cannot rest on the three
#            packages it happens to touch.
#   live-red    a SCRATCH, NON-ROOT bunkerd built from the FILED blobs, with its
#               own config and an EMPTY registry path in /tmp, booted on this host
#               (which runs real bunker-* users): the orphan walk must reach them
#               and enter the destroy path. This is the row's reproduction.
#   live-green  the same daemon built from the FIXED tree: it must REFUSE, destroy
#               nothing, count the refusal, and leave /etc/passwd byte-identical.
#               THIS HOST CARRIES ONLY TWO unknown bunker-* users, which is BELOW
#               the shipped default limit of five — so this arm sets the limit to
#               1 to exercise the guard's refusal at the host's real scale, and
#               both the limit line and the reason are printed in the transcript.
#               The shipped boundary (five swept / six refused) is proven
#               deterministically by the cells, not by this two-user host.
#   live    both live arms in order.
#
# usage: sh docs/evidence/REV-BUNKER-P1-PATCH-arms.sh <green|unfixed|noguard|suites|live-red|live-green|live> [base-ref]

set -u

mode="${1:-}"
base="${2:-4879343}"

case "$mode" in
green | unfixed | noguard | suites | live-red | live-green | live) ;;
*)
	printf 'usage: %s <green|unfixed|noguard|suites|live-red|live-green|live> [base-ref]\n' "$0" >&2
	exit 2
	;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root" || exit 2

# The product files this row CHANGED. In `unfixed` mode each is replaced by the
# base blob, and the replacement is asserted to differ from the fixed tree.
prod_changed="internal/agent/reconcile.go internal/agent/manager.go internal/config/config.go internal/registry/registry.go"

# GREEN-only test files: they assert surface this row ADDS (ReconcileReport.Refused,
# CreatedThisBoot, the reconciliation knobs), so they cannot compile against the
# filed tree and are set aside in `unfixed` mode. The behaviour RED lives in
# reconcile_sweep_guard_arms_test.go, which uses only pre-existing surface.
green_only="internal/agent/reconcile_sweep_guard_test.go internal/config/reconciliation_sweep_guard_test.go internal/registry/registry_provenance_test.go"

control_patch="$here/REV-BUNKER-P1-PATCH-negative-control.patch"
control_file="internal/agent/reconcile.go"

# ── live-arm scratch deployment ─────────────────────────────────────────────
# Every path under /tmp, high ports nothing else is using, no host state, and a
# registry path that DOES NOT EXIST (the incident's precondition: Open fabricates
# an empty registry, so the daemon believes it has no agents). The script REFUSES
# to run the live arms as root — the reproduction is only survivable unprivileged,
# and this script will not prove that the hard way.
live_root="${TMPDIR:-/tmp}/rev-bunker-p1-live"
live_pool_start=42000
live_pool_end=42999

say() { printf '%s\n' "$*"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }
bak() { printf '/tmp/revp1-bak-%s\n' "$(echo "$1" | tr '/' '-')"; }

keep=""
aside=""
pins=""
before_control=""
restore() {
	status=0
	for f in $keep; do
		orig=${f%%|*}
		copy=${f#*|}
		cp "$copy" "$orig"
		now=$(hash "$orig")
		want=$(printf '%s' "$pins" | grep " $orig\$" | cut -d' ' -f1)
		if [ -n "$want" ] && [ "$now" != "$want" ]; then
			say "RESTORE FAILED: $orig sha256=$now, expected $want"
			status=1
		else
			say "restore: $orig sha256=$now (verified against its pre-mode hash)"
		fi
	done
	for f in $aside; do
		if [ -f "$f.setaside" ]; then
			mv "$f.setaside" "$f"
			say "restore: $f (moved back from .setaside)"
		fi
	done
	if [ -f "$control_file.mutant" ]; then
		cp "$control_file.mutant" "$control_file"
		now=$(hash "$control_file")
		if [ "$now" != "$before_control" ]; then
			say "RESTORE FAILED: $control_file sha256=$now, expected $before_control"
			status=1
		else
			say "restore: $control_file sha256=$now (verified against the pre-mutation hash)"
		fi
		rm -f "$control_file.mutant"
	fi
	return $status
}

stash() {
	for f in $1; do
		now=$(hash "$f")
		pins="$pins$now $f
"
		cp "$f" "$(bak "$f")"
		keep="$keep$f|$(bak "$f")
"
		say "pre-swap: $f sha256=$now"
	done
}

# swap_in_base — replace every changed product file with its base blob, asserting
# that the swap actually changed the file (a byte-identical swap would not be the
# filed code at all).
swap_in_base() {
	for f in $prod_changed; do
		git show "$base:$f" > "$f"
		now=$(hash "$f")
		want=$(printf '%s' "$pins" | grep " $f\$" | cut -d' ' -f1)
		if [ "$now" = "$want" ]; then
			say "SWAP FAILED: $f came back byte-identical to the fixed tree — that is not the filed code"
			exit 3
		fi
		say "swapped in base blob: $f sha256=$now (was $want)"
	done
}

# ── test runners ────────────────────────────────────────────────────────────

run_cells() {
	label="$1"
	say "--- $label: go test ./internal/agent ./internal/config ./internal/registry -run <sweep guard cells> -count=1 -v -timeout 300s"
	go test ./internal/agent ./internal/config ./internal/registry \
		-run 'TestReconcileSweepGuard|TestStoreCreatedThisBoot|TestReconciliationSweepGuard' \
		-count=1 -v -timeout 300s
	rc=$?
	say "--- $label exit=$rc"
	return $rc
}

run_arms() {
	label="$1"
	say "--- $label: go test ./internal/agent -run TestReconcileSweepGuardArms -count=1 -v -timeout 300s"
	go test ./internal/agent -run 'TestReconcileSweepGuardArms' -count=1 -v -timeout 300s
	rc=$?
	say "--- $label exit=$rc"
	return $rc
}

# ── live daemon arms ────────────────────────────────────────────────────────

# live_config <limit-line> — write the scratch config. The limit line is passed in
# so the transcript records exactly which threshold each arm ran with.
live_config() {
	limit_line="$1"
	mkdir -p "$live_root/data" "$live_root/ssh" "$live_root/archives" "$live_root/agent-tmp"
	# The incident's precondition is an ABSENT registry file, so every live arm
	# starts by removing it (the previous arm may have left one behind).
	rm -f "$live_root/data/agents.jsonl"
	{
		cat <<EOF
# REV-BUNKER-P1-PATCH live arm: scratch NON-ROOT bunkerd. Every path is under
# $live_root. The registry path DOES NOT EXIST on purpose: that is the incident's
# precondition (Open fabricates an empty registry; the daemon believes it has no
# agents; every bunker-* user on this host is an orphan it claims as its own).
server:
  grpc_addr: "127.0.0.1:$((live_pool_start + 1))"
  rest_addr: "127.0.0.1:$live_pool_start"
  request_timeout: 60s
tls:
  enabled: false
  # Loopback anyway; declared explicitly so the plaintext-listener guard is
  # satisfied without a scratch certificate.
  insecure_dev: true
auth:
  enabled: true
  token: "rev-bunker-p1-scratch-token"
agent:
  base_data_dir: $live_root/data
  ssh_dir: $live_root/ssh
  max_agents: 5
  port_range_start: $live_pool_start
  port_range_end: $live_pool_end
  port_range_per_agent: 100
  rootless_installer_cache_dir: ""
  destroy_archive_dir: $live_root/archives
  registry:
    enabled: true
    path: $live_root/data/agents.jsonl
    max_bytes: 5242880
    max_backups: 3
  isolation:
    shared_scratch_enabled: false
    private_tmp_root: $live_root/agent-tmp
  reconciliation:
    mode: destroy
$limit_line
audit:
  enabled: false
containment:
  disclosure: false
safety:
  preset: ""
EOF
	} > "$live_root/config.yaml"
	say "--- live config:"
	sed 's/^/    /' "$live_root/config.yaml" | grep -E 'reconciliation|unproven_orphan_limit|registry|path|port_range|mode:' | sed 's/^/  /'
	say "--- scratch registry path $live_root/data/agents.jsonl exists before boot: $(test -e "$live_root/data/agents.jsonl" && echo YES || echo no)"
}

run_live() {
	label="$1"
	bin="$2"
	log="$3"
	rm -f "$live_root/data/agents.jsonl"
	passwd_before=$(sha256sum /etc/passwd | cut -d' ' -f1)
	: > "$log"
	"$bin" --config "$live_root/config.yaml" >>"$log" 2>&1 &
	daemon_pid=$!
	say "--- $label: daemon pid $daemon_pid, registry $live_root/data/agents.jsonl (absent at boot)"
	sleep 12
	kill "$daemon_pid" 2>/dev/null
	sleep 1
	kill -9 "$daemon_pid" 2>/dev/null
	wait "$daemon_pid" 2>/dev/null
	passwd_after=$(sha256sum /etc/passwd | cut -d' ' -f1)
	say "--- $label: /etc/passwd sha256 before=$passwd_before after=$passwd_after"
	if [ "$passwd_before" = "$passwd_after" ]; then
		say "--- $label: /etc/passwd UNCHANGED (every host user still present)"
	else
		say "--- $label: /etc/passwd CHANGED"
	fi
	say "--- $label: walk evidence"
	say "destroying_agent_lines=$(grep -c 'destroying agent' "$log" 2>/dev/null || true)"
	say "refusal_lines=$(grep -c 'REFUSING to destroy unproven orphans' "$log" 2>/dev/null || true)"
	say "userdel_permission_denied_lines=$(grep -c 'userdel: Permission denied' "$log" 2>/dev/null || true)"
	grep -n 'reconciliation complete\|REFUSING to destroy unproven orphans\|destroying agent\|refused_orphans' "$log" 2>/dev/null | sed 's/^/    /'
	say "--- $label exit=0"
}

live_arms_ready() {
	if [ "$(id -u)" = "0" ]; then
		say "REFUSING the live arms as root: the reproduction is only survivable unprivileged."
		return 1
	fi
	return 0
}

say "REV-BUNKER-P1-PATCH arms — mode=$mode base=$base"
say "go $(go version | cut -d' ' -f3) · $(git rev-parse --short HEAD 2>/dev/null || echo 'no-git') · $(date -u '+%Y-%m-%dT%H:%M:%SZ')"

case "$mode" in
green)
	for f in $prod_changed; do
		say "tree under test: $f sha256=$(hash "$f")"
	done
	run_cells GREEN
	;;

unfixed)
	trap 'restore || exit 3' EXIT
	stash "$prod_changed"
	swap_in_base
	for f in $green_only; do
		mv "$f" "$f.setaside"
		aside="$aside$f
"
		say "set aside (GREEN-only, cannot compile against the filed tree): $f"
	done
	run_arms "UNFIXED (tree as filed @ $base)"
	;;

noguard)
	trap 'restore || exit 3' EXIT
	before_control=$(hash "$control_file")
	cp "$control_file" "$control_file.mutant"
	say "pre-mutation: $control_file sha256=$before_control"
	if ! git apply "$control_patch"; then
		say "NEGATIVE CONTROL PATCH DID NOT APPLY — the control is not measuring anything"
		exit 3
	fi
	say "mutation applied: $control_patch -> $control_file sha256=$(hash "$control_file")"
	run_arms "NEGATIVE CONTROL (guard neutered on the fixed tree)"
	;;

suites)
	say "--- gofmt -l internal cmd"
	fmt_out=$(gofmt -l internal cmd 2>&1)
	if [ -n "$fmt_out" ]; then
		say "$fmt_out"
		say "gofmt rc=1"
	else
		say "gofmt rc=0 (clean)"
	fi
	say "--- go build ./..."
	if go build ./...; then say "go build rc=0"; else say "go build rc=1"; fi
	say "--- go vet ./..."
	if go vet ./...; then say "go vet rc=0"; else say "go vet rc=1"; fi
	say "--- go test ./... -count=1"
	go test ./... -count=1 -timeout 900s
	say "go test rc=$?"
	;;

live-red)
	live_arms_ready || exit 3
	trap 'restore || exit 3' EXIT
	stash "$prod_changed"
	swap_in_base
	mkdir -p "$live_root/bin"
	# No limit line: the FILED behaviour at the shipped default. This is the
	# reproduction — nothing bounds the walk.
	live_config ""
	go build -o "$live_root/bin/bunkerd-red" ./cmd/bunkerd || exit 3
	say "built: $live_root/bin/bunkerd-red sha256=$(hash "$live_root/bin/bunkerd-red")"
	run_live "LIVE-RED (filed blobs, scratch non-root daemon, empty registry, default limit)" \
		"$live_root/bin/bunkerd-red" "$live_root/live-red.log"
	;;

live-green)
	live_arms_ready || exit 3
	mkdir -p "$live_root/bin"
	# This host carries TWO unknown bunker-* users, below the shipped default of
	# five, so the limit is set to 1 to exercise the refusal at the host's real
	# scale. The reason is printed with the config, in the same transcript.
	live_config "    unproven_orphan_limit: 1"
	go build -o "$live_root/bin/bunkerd-fixed" ./cmd/bunkerd || exit 3
	say "built: $live_root/bin/bunkerd-fixed sha256=$(hash "$live_root/bin/bunkerd-fixed")"
	run_live "LIVE-GREEN (fixed tree, scratch non-root daemon, empty registry, limit 1)" \
		"$live_root/bin/bunkerd-fixed" "$live_root/live-green.log"
	;;

live)
	say "=== live-red first (it swaps in the filed blobs), then live-green on the fixed tree ==="
	sh "$0" live-red "$base"
	red_rc=$?
	sh "$0" live-green "$base"
	green_rc=$?
	say "--- live summary: live-red exit=$red_rc, live-green exit=$green_rc"
	;;
esac

exit 0
