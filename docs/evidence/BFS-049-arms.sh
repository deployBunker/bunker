#!/bin/sh
# BFS-049 — the arms, the negative controls, and the numbers.
#
# Two observers of one tree disagreed about a same-size, mtime-restored edit:
# the hash cache (tree.go, keyed on (size, mtime)) answered "unchanged" while
# the event ledger (events.go, keyed on (size, mtime, ctime)) reported the path
# as moved. This row gives them ONE identity definition. The modes below are
# what proves it, and each one is named for what it must show:
#
#   green     the fixed tree: every BFS-049 arm must PASS. The six arms are
#             split by what they may assert: identity_bfs049_test.go is written
#             against surface that existed BEFORE this row, and
#             identity_report_bfs049_test.go asserts the surface this row ADDS.
#   unfixed   the tree as FILED: both production blobs are replaced by the base
#             commit's — sha256-checked against the hashes recorded below, so
#             "the filed tree" is a named artifact and not a memory — and the
#             report file is set aside because the filed tree has no
#             IdentityDivergenceCounters to compile against (BFS-062's split).
#             The RED cell and the isolating arm MUST FAIL, and the attribution
#             arm (an unchanged read is still a cache hit) MUST PASS, so a red
#             arm here cannot be "the cache was disabled".
#   neutered  control: the fix neutered by ONE substitution — the shared
#             projection drops ctime again, exactly as filed, while the ledger,
#             the never-quiet rule and the frame bound are untouched. The RED
#             cell and the isolating arm MUST FAIL (the divergence is back) and
#             the attribution arm MUST still PASS (the cache still hits when
#             nothing moves), which is what attributes the failure to the shared
#             identity rather than to a dead cache. The report arms stay GREEN:
#             the ledger's classification did not change, so the backstop still
#             counts.
#   cost      the blast radius. The cost arms are run on the fixed tree and then
#             on the filed tree, same fixtures, min-of-three passes; and the
#             stat-family syscall count is taken with strace on two test BINARIES
#             that differ only in tree.go, over 500 cache hits. A per-path ctime
#             stat would show as +500 there; SPEC-watcher-capability R-V2 is the
#             reason that number had to be zero.
#   race      the whole webdav package under -race on the fixed tree.
#
# Every mutation is a substitution on text this row ADDED, verified after the
# fact by grep in BOTH directions (mutant text present, pre-image gone), and
# built before the arms run, so a mutation that does not compile is reported as
# such rather than silently measuring the fixed tree. Every restore is a byte
# copy whose sha256 is re-checked after the copy back; a restore that does not
# match aborts the run.
#
# usage: sh docs/evidence/BFS-049-arms.sh <green|unfixed|neutered|cost|race> [base-ref]

set -u

mode="${1:-}"
base="${2:-4879343}"

case "$mode" in
green | unfixed | neutered | cost | race) ;;
*)
	printf 'usage: %s <green|unfixed|neutered|cost|race> [base-ref]\n' "$0" >&2
	exit 2
	;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root" || exit 2

prod_tree=internal/server/webdav/tree.go
prod_events=internal/server/webdav/events.go
arms_file=internal/server/webdav/identity_bfs049_test.go
report_file=internal/server/webdav/identity_report_bfs049_test.go
pkg=./internal/server/webdav

# The filed blobs of the base commit — the content the RED is measured on. A
# swap whose blob does not match these aborts: measuring against moving main is
# how a lane's evidence becomes somebody else's diff.
filed_tree=3c4e95611e51757bab079df93f3d23abfdd1e1466c8edb3a36438a7ec5c09f39
filed_events=61e4bdf123cf98fb51d3f439106f6afa1986f46069730284496ac45c0e2855f2

say() { printf '%s\n' "$*"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }

bak_tree=""
bak_events=""
before_tree=""
before_events=""
stash_dir=""
report_was_moved=0

restore() {
	status=0
	if [ -n "$bak_tree" ]; then
		cp "$bak_tree" "$prod_tree"
		now=$(hash "$prod_tree")
		if [ "$now" != "$before_tree" ]; then
			say "RESTORE FAILED: $prod_tree sha256=$now, expected $before_tree"
			status=1
		else
			say "restore: $prod_tree sha256=$now (verified byte-identical against the pre-mutation hash)"
		fi
	fi
	if [ -n "$bak_events" ]; then
		cp "$bak_events" "$prod_events"
		now=$(hash "$prod_events")
		if [ "$now" != "$before_events" ]; then
			say "RESTORE FAILED: $prod_events sha256=$now, expected $before_events"
			status=1
		else
			say "restore: $prod_events sha256=$now (verified byte-identical against the pre-mutation hash)"
		fi
	fi
	if [ "$report_was_moved" = 1 ] && [ -n "$stash_dir" ]; then
		mv "$stash_dir/$(basename "$report_file")" "$report_file" || status=1
		if [ -f "$report_file" ]; then
			say "restore: $report_file sha256=$(hash "$report_file") (set aside for the filed-tree run, moved back)"
		else
			say "RESTORE FAILED: $report_file was not moved back"
			status=1
		fi
	fi
	return $status
}

trap 'restore || exit 3' EXIT

stash() {
	before_tree=$(hash "$prod_tree")
	before_events=$(hash "$prod_events")
	bak_tree=$(mktemp)
	bak_events=$(mktemp)
	cp "$prod_tree" "$bak_tree"
	cp "$prod_events" "$bak_events"
	say "pre-swap: $prod_tree sha256=$before_tree"
	say "          $prod_events sha256=$before_events"
}

set_aside_report() {
	stash_dir=$(mktemp -d)
	mv "$report_file" "$stash_dir/" || exit 5
	report_was_moved=1
	say "set aside for this run: $report_file (the filed tree has no IdentityDivergenceCounters)"
}

run_arm() {
	label="$1"
	filter="$2"
	say "--- $label: go test $pkg -run $filter -count=1 -v -timeout 300s"
	go test "$pkg" -run "$filter" -count=1 -v -timeout 300s
	rc=$?
	say "--- $label exit=$rc"
	return $rc
}

# must_grep <file> <fixed-string> <present|absent> <what it means>
must_grep() {
	if [ "$3" = present ]; then
		if ! grep -qF -- "$2" "$1"; then
			say "MUTATION AUDIT FAILED: $1 does not contain the mutant text [$4]"
			exit 5
		fi
	elif grep -qF -- "$2" "$1"; then
		say "MUTATION AUDIT FAILED: $1 still contains the pre-image text [$4]"
		exit 5
	fi
}

# mutate <label> <perl-expression> <post-string> <pre-string>
mutate() {
	label="$1"
	expr="$2"
	post="$3"
	pre="$4"
	perl -0777 -pi -e "$expr" "$prod_events" || exit 5
	say "mutated ($label): $prod_events sha256=$(hash "$prod_events")"
	must_grep "$prod_events" "$post" present "$label mutant text"
	must_grep "$prod_events" "$pre" absent "$label pre-image text"
	if gofmt -l "$prod_events" | grep -q .; then
		say "MUTATION AUDIT FAILED: $prod_events is not gofmt-clean after $label"
		exit 5
	fi
	if ! go build "$pkg"; then
		say "MUTATION AUDIT FAILED: $prod_events does not build after $label"
		exit 5
	fi
	say "mutation audit ($label): mutant text present, pre-image gone, gofmt clean, package builds"
}

say "BFS-049 arms — mode=$mode base=$base"
say "go $(go version | cut -d' ' -f3) · $(git rev-parse --short HEAD 2>/dev/null || echo 'no-git') · $(date -u '+%Y-%m-%dT%H:%M:%SZ')"

case "$mode" in
green)
	say "tree under test: $prod_tree sha256=$(hash "$prod_tree")"
	say "                $prod_events sha256=$(hash "$prod_events")"
	say "                $arms_file sha256=$(hash "$arms_file")"
	say "                $report_file sha256=$(hash "$report_file")"
	run_arm GREEN 'TestBFS049'
	;;

unfixed)
	stash
	git show "$base:$prod_tree" >"$prod_tree"
	git show "$base:$prod_events" >"$prod_events"
	filed_t=$(hash "$prod_tree")
	filed_e=$(hash "$prod_events")
	say "swapped in $base:$prod_tree sha256=$filed_t"
	say "swapped in $base:$prod_events sha256=$filed_e"
	if [ "$filed_t" != "$filed_tree" ] || [ "$filed_e" != "$filed_events" ]; then
		say "MISMATCH: expected the filed blobs $filed_tree / $filed_events — this is not the tree the row was filed on, so nothing here would be measured against a named artifact"
		exit 4
	fi
	say "the filed tree, sha256-verified: the RED below is measured on a named content, not on a memory"
	set_aside_report
	run_arm "RED (tree as filed): the divergence MUST be constructed" 'TestBFS049TheTwoObserversCannotDisagreeOnASameSizeMtimePreservedEdit'
	run_arm "RED (tree as filed): the isolating arm MUST fail" 'TestBFS049AMetadataOnlyMoveIsNotACacheHit'
	run_arm "RED (tree as filed) attribution: an unchanged read MUST still be a cache hit" 'TestBFS049AnUnchangedReadIsStillACacheHit'
	run_arm "RED (tree as filed) attribution: the cost ceilings MUST still hold" 'TestBFS049TheSharperIdentityCostsNoExtraSyscall'
	;;

neutered)
	stash
	# One substitution, on text this row added: the shared projection drops
	# ctime again — the filed key, with everything else this row did in place.
	mutate neutered \
		's/\treturn id\.Size == other\.Size && id\.Mtime == other\.Mtime && id\.Ctime == other\.Ctime/\treturn id.Size == other.Size \&\& id.Mtime == other.Mtime \/\/ CONTROL (BFS-049 neutered)/' \
		'// CONTROL (BFS-049 neutered)' \
		'return id.Size == other.Size && id.Mtime == other.Mtime && id.Ctime == other.Ctime'
	run_arm "CONTROL neutered: the divergence MUST be back" 'TestBFS049TheTwoObserversCannotDisagreeOnASameSizeMtimePreservedEdit'
	run_arm "CONTROL neutered: the isolating arm MUST fail" 'TestBFS049AMetadataOnlyMoveIsNotACacheHit'
	run_arm "CONTROL neutered attribution: the cache still MISSES nothing that moved — the arm that must stay green" 'TestBFS049AnUnchangedReadIsStillACacheHit'
	run_arm "CONTROL neutered attribution: the ledger's report is unaffected, so the backstop still counts" 'TestBFS049TheFormerDivergenceClassIsReportedNotSilent|TestBFS049TheReportNamesTheClassAndNotEveryChange'
	;;

cost)
	bindir=$(mktemp -d)
	tmpmins=$(mktemp -d)
	say "host load at measurement time: $(cat /proc/loadavg)"
	say "=== fixed tree: $prod_tree sha256=$(hash "$prod_tree")"

	# --- the syscall counts first: load-independent, and the number that
	# decides whether a per-path ctime stat sneaked into a hot path.
	say "building the fixed-tree test binary (before any swap, so the two binaries differ ONLY in tree.go)"
	go test -c -o "$bindir/fixed.test" "$pkg" || exit 5
	strace_count() {
		label="$1"
		bin="$2"
		out="$tmpmins/strace.$label"
		# `trace=` names must all exist for the platform: an unknown name makes
		# strace refuse to run at all, and piping that refusal through a grep is
		# how an empty syscall table gets published as a measurement.
		strace -f -c -e trace=newfstatat,statx,fstat,stat,lstat "$bin" \
			-test.run TestBFS049OneStatPerHashFileLookup -test.count=1 -test.v >"$out" 2>&1
		cat "$out"
		if ! grep -q 'syscall' "$out" || ! grep -q 'total' "$out"; then
			say "STRACE MEASUREMENT FAILED for $label: no syscall summary was produced (nothing here may be quoted as a count)"
			exit 5
		fi
	}
	say "--- fixed tree: strace -f -c -e trace=<stat family> … -test.run TestBFS049OneStatPerHashFileLookup"
	strace_count fixed "$bindir/fixed.test"

	stash
	git show "$base:$prod_tree" >"$prod_tree"
	filed_t=$(hash "$prod_tree")
	say "=== filed tree: $prod_tree sha256=$filed_t"
	if [ "$filed_t" != "$filed_tree" ]; then
		say "MISMATCH: expected the filed blob $filed_tree"
		exit 4
	fi
	say "building the filed-tree test binary"
	go test -c -o "$bindir/filed.test" "$pkg" || exit 5
	say "--- filed tree: strace -f -c -e trace=<stat family> … -test.run TestBFS049OneStatPerHashFileLookup"
	strace_count filed "$bindir/filed.test"

	# --- the timings, interleaved: each round runs BOTH trees back to back so
	# host load lands on both, and the direction alternates so a slow window
	# cannot favour one tree.
	say "=== interleaved timing rounds (the same fixture in both trees, both directions)"
	: >"$tmpmins/fixed.min"
	: >"$tmpmins/filed.min"
	round=1
	while [ "$round" -le 3 ]; do
		if [ $((round % 2)) -eq 1 ]; then
			order="fixed filed"
		else
			order="filed fixed"
		fi
		for which in $order; do
			if [ "$which" = fixed ]; then
				cp "$bak_tree" "$prod_tree"
			else
				git show "$base:$prod_tree" >"$prod_tree"
			fi
			# The blob actually under test, recorded per round: a round that ran
			# against the wrong tree is then visible rather than averaged in.
			say "--- round $round ($which) $prod_tree sha256=$(hash "$prod_tree")"
			go test "$pkg" -run TestBFS049TheSharperIdentityCostsNoExtraSyscall -count=1 -v -timeout 300s \
				| tee "$tmpmins/$which.round$round" \
				| grep -E 'BFS-049 cost'
			grep -F 'BFS-049 cost MIN:' "$tmpmins/$which.round$round" >>"$tmpmins/$which.min" || true
		done
		round=$((round + 1))
	done

	# Minimum per (metric, spec) per tree, then the two trees side by side.
	# The aggregation is written to a table file as well as the transcript: the
	# join below compares AGGREGATED lines, and reading the raw log files here
	# is how this table came out empty the first time.
	minimums() {
		awk '
			/BFS-049 cost MIN:/ {
				line = $0
				sub(/^.*BFS-049 cost MIN: /, "", line)
				metric = line; sub(/ .*/, "", metric)
				sub(/^[^ ]+ /, "", line)
				spec = line; sub(/ ns=.*/, "", spec)
				sub(/^.* ns=/, "", line)
				if (line ~ /^[0-9]+$/) { v = line }
				else {
					v = spec; sub(/^.* value=/, "", v); spec = "-"
					if (v !~ /^[0-9]+$/) v = ""
				}
				if (v == "") next
				k = metric "|" spec
				if (!(k in m) || v + 0 < m[k] + 0) m[k] = v
			}
			END { for (k in m) print k "|" m[k] }' "$1" | sort | tee "$2"
	}
	say "=== minimums per tree (min over 3 interleaved rounds; ns unless the metric is entry_bytes)"
	minimums "$tmpmins/fixed.min" "$tmpmins/fixed.table"
	minimums "$tmpmins/filed.min" "$tmpmins/filed.table"
	say "=== fixed vs filed, MINIMUM over the interleaved rounds"
	join_tables() {
		awk -F'|' '
			NR == FNR { f[$1 "|" $2] = $3; next }
			{
				k = $1 "|" $2
				if (k in f) {
					d = $3 + 0 - f[k] + 0
					printf "%-26s %-26s fixed=%-14s filed=%-14s delta=%s\n", $1, $2, f[k], $3, d
				}
			}' "$1" "$2"
	}
	join_tables "$tmpmins/fixed.table" "$tmpmins/filed.table"
	;;

race)
	say "tree under test: $prod_tree sha256=$(hash "$prod_tree")"
	say "                $prod_events sha256=$(hash "$prod_events")"
	say "--- race: go test $pkg -count=1 -race -timeout 900s"
	go test "$pkg" -count=1 -race -timeout 900s
	say "--- race exit=$?"
	;;
esac

restore
trap - EXIT
say "done: mode=$mode"
