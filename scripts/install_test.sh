#!/bin/sh
# ══════════════════════════════════════════════════════════════════════════════
# install_test.sh — self-contained, network-free tests for scripts/install.sh
# (DF-BUNKER-25). Runs locally and in CI (`make test-sh`), and never reaches
# github.com or any other host:
#
#   * the release path is driven against a file:// mirror of the release layout
#   * every other case uses --from-dir (local fixtures) or --build (refusal)
#
# Every assertion prints PASS or FAIL; the script exits 0 only when all pass.
#
# Usage: sh scripts/install_test.sh    (or: bash scripts/install_test.sh)
# ══════════════════════════════════════════════════════════════════════════════
set -u

ROOT=$(cd "$(dirname -- "$0")/.." && pwd)
INSTALL="$ROOT/scripts/install.sh"
SELF="$ROOT/scripts/install_test.sh"

PASS_COUNT=0
FAIL_COUNT=0

pass() {
	PASS_COUNT=$((PASS_COUNT + 1))
	printf 'PASS  %s\n' "$1"
}

fail() {
	FAIL_COUNT=$((FAIL_COUNT + 1))
	printf 'FAIL  %s\n' "$1"
	if [ $# -ge 2 ] && [ -n "$2" ]; then
		printf '      %s\n' "$2"
	fi
}

# check EXPECTED_RC LABEL RC [DETAIL]
check_rc() {
	if [ "$3" = "$1" ]; then
		pass "$2"
	else
		fail "$2" "expected exit $1, got $3${4:+ — $4}"
	fi
}

# contains LABEL HAYSTACK NEEDLE
contains() {
	case "$2" in
	*"$3"*) pass "$1" ;;
	*) fail "$1" "output does not contain \"$3\"" ;;
	esac
}

# lacks LABEL HAYSTACK NEEDLE
lacks() {
	case "$2" in
	*"$3"*) fail "$1" "output unexpectedly contains \"$3\"" ;;
	*) pass "$1" ;;
	esac
}

# ── fixtures ────────────────────────────────────────────────────────────────
[ -f "$INSTALL" ] || {
	printf 'FAIL  scripts/install.sh not found at %s\n' "$INSTALL"
	exit 1
}

SH_BIN=${INSTALL_TEST_SH:-sh}
SH_ABS=$(command -v "$SH_BIN" 2>/dev/null || printf '')
BASH_ABS=$(command -v bash 2>/dev/null || printf '')

TMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/install-test.XXXXXX") || {
	printf 'FAIL  cannot create a temp directory\n'
	exit 1
}
cleanup() { rm -rf "$TMP_ROOT"; }
trap cleanup EXIT INT TERM HUP

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# stub FILE NAME VERSION — a runnable stand-in for a bunker binary.
stub() {
	{
		printf '#!/bin/sh\n'
		printf 'printf "%%s\\n" "%s %s"\n' "$2" "$3"
		printf 'printf "%%s\\n" "  commit:     0123456789"\n'
	} >"$1"
	chmod 0755 "$1"
}

# run INTERPRETER ARGS... — sets OUT (stdout+stderr) and RC.
run() {
	OUT=$("$@" 2>&1)
	RC=$?
}

# make_pair DIR LAYOUT — two stub binaries. LAYOUT is "plain" (bunker, bunkerd)
# or "dist" (bunker-linux-amd64, bunkerd-linux-amd64).
make_pair() {
	mk_pair_dir=$1
	mk_pair_layout=$2
	mkdir -p "$mk_pair_dir"
	if [ "$mk_pair_layout" = dist ]; then
		stub "$mk_pair_dir/bunker-linux-amd64" bunker 0.1.4
		stub "$mk_pair_dir/bunkerd-linux-amd64" bunkerd 0.1.4
	elif [ "$mk_pair_layout" = dist-arm64 ]; then
		stub "$mk_pair_dir/bunker-linux-arm64" bunker 0.1.4
		stub "$mk_pair_dir/bunkerd-linux-arm64" bunkerd 0.1.4
	else
		stub "$mk_pair_dir/bunker" bunker 0.1.4
		stub "$mk_pair_dir/bunkerd" bunkerd 0.1.4
	fi
}

# write_sums DIR [NAME...] — SHA256SUMS over the named files in DIR (all of
# them when no name is given).
write_sums() {
	ws_dir=$1
	shift
	(
		cd "$ws_dir" || exit 1
		if [ $# -gt 0 ]; then
			for ws_name in "$@"; do
				sha256 "$ws_name"
			done >SHA256SUMS
		else
			for ws_name in *; do
				[ "$ws_name" = SHA256SUMS ] && continue
				[ -f "$ws_name" ] || continue
				printf '%s  %s\n' "$(sha256 "$ws_name")" "$ws_name"
			done >SHA256SUMS
		fi
	)
}

# fake_sshfs DIR VERSION — a stand-in sshfs reporting VERSION. PATH-inject
# DIR ahead of the real PATH so tests never depend on the host's sshfs.
fake_sshfs() {
	fs_dir=$1
	fs_ver=$2
	mkdir -p "$fs_dir"
	{
		printf '#!/bin/sh\n'
		printf 'printf "%%s\\n" "SSHFS version %s"\n' "$fs_ver"
	} >"$fs_dir/sshfs"
	chmod 0755 "$fs_dir/sshfs"
}

# fake_apt_get DIR — a stand-in apt-get that fails loudly if ever invoked:
# no --sshfs test may reach a real package manager, and a suite run that
# installs anything would be a defect, not a pass.
fake_apt_get() {
	mkdir -p "$1"
	{
		printf '#!/bin/sh\n'
		printf 'echo "FAKE-APT-GET-WAS-INVOKED $*" >&2\n'
		printf 'exit 1\n'
	} >"$1/apt-get"
	chmod 0755 "$1/apt-get"
}

# fake_apt_cache DIR — a stand-in apt-cache whose `policy sshfs` candidate
# comes from $SSHFS_TEST_CANDIDATE.
fake_apt_cache() {
	mkdir -p "$1"
	{
		printf '#!/bin/sh\n'
		printf 'if [ "$1" = policy ] && [ "$2" = sshfs ]; then\n'
		printf '\tprintf '"'"'sshfs:\\n  Candidate: %%s\\n'"'"' "$SSHFS_TEST_CANDIDATE"\n'
		printf 'else\n'
		printf '\texit 1\n'
		printf 'fi\n'
	} >"$1/apt-cache"
	chmod 0755 "$1/apt-cache"
}

printf 'install_test.sh: testing %s\n' "$INSTALL"
printf 'install_test.sh: interpreter %s (%s)\n\n' "$SH_BIN" "${SH_ABS:-unknown}"

# ── 1. syntax ───────────────────────────────────────────────────────────────
if sh -n "$INSTALL" 2>/dev/null; then
	pass "install.sh parses under sh ($SH_BIN)"
else
	fail "install.sh parses under sh ($SH_BIN)" "$(sh -n "$INSTALL" 2>&1)"
fi

if [ -n "$BASH_ABS" ]; then
	BASH_OUT=$(bash -n "$INSTALL" 2>&1) && BASH_RC=0 || BASH_RC=$?
	check_rc 0 "install.sh parses under bash" "$BASH_RC" "$BASH_OUT"

	BASH_OUT=$(bash -n "$SELF" 2>&1) && BASH_RC=0 || BASH_RC=$?
	check_rc 0 "install_test.sh parses under bash" "$BASH_RC" "$BASH_OUT"
fi

# ── 2. --help ───────────────────────────────────────────────────────────────
run "$SH_BIN" "$INSTALL" --help
check_rc 0 "--help exits 0" "$RC"
contains "--help prints the usage" "$OUT" "Usage:"

# ── 3. unsupported platform (exit 42, before touching anything) ─────────────
GOOD_SRC="$TMP_ROOT/good-plain"
make_pair "$GOOD_SRC" plain
write_sums "$GOOD_SRC"

PFX="$TMP_ROOT/prefix-unsupported"
mkdir -p "$PFX"

OUT=$(INSTALL_SH_TEST_OS=darwin INSTALL_SH_TEST_ARCH=amd64 "$SH_BIN" "$INSTALL" \
	--from-dir "$GOOD_SRC" --dir "$PFX" 2>&1) && RC=0 || RC=$?
check_rc 42 "unsupported OS (darwin) refuses with exit 42" "$RC"
contains "unsupported OS names the supported targets" "$OUT" "linux/amd64 and linux/arm64"

OUT=$(INSTALL_SH_TEST_OS=linux INSTALL_SH_TEST_ARCH=armv7l "$SH_BIN" "$INSTALL" \
	--from-dir "$GOOD_SRC" --dir "$PFX" 2>&1) && RC=0 || RC=$?
check_rc 42 "unsupported architecture (armv7l) refuses with exit 42" "$RC"

if [ -z "$(ls -A "$PFX")" ]; then
	pass "unsupported platform installed nothing"
else
	fail "unsupported platform installed nothing" "found: $(ls -A "$PFX")"
fi

# ── 4. checksum mismatch in --from-dir ──────────────────────────────────────
BAD_SRC="$TMP_ROOT/bad-sum"
make_pair "$BAD_SRC" plain
{
	printf '%s  bunker\n' "0000000000000000000000000000000000000000000000000000000000000000"
	printf '%s  bunkerd\n' "$(sha256 "$BAD_SRC/bunkerd")"
} >"$BAD_SRC/SHA256SUMS"

BAD_PFX="$TMP_ROOT/prefix-bad-sum"
mkdir -p "$BAD_PFX"
OUT=$("$SH_BIN" "$INSTALL" --from-dir "$BAD_SRC" --dir "$BAD_PFX" 2>&1) && RC=0 || RC=$?
check_rc 42 "checksum mismatch refuses with exit 42" "$RC"
contains "checksum mismatch says 'checksum mismatch'" "$OUT" "checksum mismatch"

if [ -z "$(ls -A "$BAD_PFX")" ]; then
	pass "checksum mismatch installed nothing"
else
	fail "checksum mismatch installed nothing" "found: $(ls -A "$BAD_PFX")"
fi

# ── 5. missing SHA256SUMS entry ─────────────────────────────────────────────
MISS_SRC="$TMP_ROOT/missing-entry"
make_pair "$MISS_SRC" plain
{
	printf '%s  bunkerd\n' "$(sha256 "$MISS_SRC/bunkerd")"
} >"$MISS_SRC/SHA256SUMS"

MISS_PFX="$TMP_ROOT/prefix-missing-entry"
mkdir -p "$MISS_PFX"
OUT=$("$SH_BIN" "$INSTALL" --from-dir "$MISS_SRC" --dir "$MISS_PFX" 2>&1) && RC=0 || RC=$?
check_rc 42 "missing SHA256SUMS entry refuses with exit 42" "$RC"
contains "missing entry names the absent file" "$OUT" "no SHA256SUMS entry for bunker"

if [ -z "$(ls -A "$MISS_PFX")" ]; then
	pass "missing entry installed nothing"
else
	fail "missing entry installed nothing" "found: $(ls -A "$MISS_PFX")"
fi

# ── 6. --from-dir happy path (plain layout, both interpreters) ──────────────
for iface in sh bash; do
	if [ "$iface" = bash ] && [ -z "$BASH_ABS" ]; then
		continue
	fi
	HAPPY_PFX="$TMP_ROOT/prefix-happy-$iface"
	mkdir -p "$HAPPY_PFX"
	run "$iface" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$HAPPY_PFX"
	check_rc 0 "--from-dir happy path exits 0 ($iface)" "$RC" "$OUT"

	if [ -x "$HAPPY_PFX/bunker" ] && [ -x "$HAPPY_PFX/bunkerd" ]; then
		pass "--from-dir installed both binaries, executable ($iface)"
	else
		fail "--from-dir installed both binaries, executable ($iface)" "found: $(ls -la "$HAPPY_PFX")"
	fi

	VER_OUT=$("$HAPPY_PFX/bunker" --version 2>&1) || VER_OUT=
	contains "--from-dir installed a runnable bunker ($iface)" "$VER_OUT" "bunker 0.1.4"
	contains "--from-dir smoke-checks the installed version ($iface)" "$OUT" "smoke check OK"

	# idempotence: a second install over the same prefix succeeds
	run "$iface" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$HAPPY_PFX"
	check_rc 0 "--from-dir is idempotent (second run) ($iface)" "$RC" "$OUT"
done

# ── 7. --from-dir with the dist/ (platform-suffixed) layout ─────────────────
DIST_SRC="$TMP_ROOT/dist-layout"
make_pair "$DIST_SRC" dist
write_sums "$DIST_SRC"
DIST_PFX="$TMP_ROOT/prefix-dist"
mkdir -p "$DIST_PFX"
run "$SH_BIN" "$INSTALL" --from-dir "$DIST_SRC" --dir "$DIST_PFX"
check_rc 0 "--from-dir accepts the dist layout (bunker-<os>-<arch>)" "$RC" "$OUT"
if [ -x "$DIST_PFX/bunker" ] && [ -x "$DIST_PFX/bunkerd" ]; then
	pass "dist layout installed both binaries under their plain names"
else
	fail "dist layout installed both binaries under their plain names" "found: $(ls -A "$DIST_PFX")"
fi

# arm64 assets resolve for an arm64 platform probe (cross-platform naming)
DIST_ARM="$TMP_ROOT/dist-arm64"
make_pair "$DIST_ARM" dist-arm64
write_sums "$DIST_ARM"
DIST_ARM_PFX="$TMP_ROOT/prefix-dist-arm64"
mkdir -p "$DIST_ARM_PFX"
OUT=$(INSTALL_SH_TEST_OS=linux INSTALL_SH_TEST_ARCH=arm64 "$SH_BIN" "$INSTALL" \
	--from-dir "$DIST_ARM" --dir "$DIST_ARM_PFX" 2>&1) && RC=0 || RC=$?
check_rc 0 "dist layout resolves the linux/arm64 assets" "$RC" "$OUT"

# ── 8. release path against a file:// mirror (no network) ───────────────────
MIRROR="$TMP_ROOT/mirror/releases"
mkdir -p "$MIRROR/latest/download" "$MIRROR/download/v0.1.4"
cp "$DIST_SRC/bunker-linux-amd64" "$DIST_SRC/bunkerd-linux-amd64" "$MIRROR/latest/download/"
write_sums "$MIRROR/latest/download"
cp "$MIRROR/latest/download/SHA256SUMS" "$MIRROR/download/v0.1.4/"
cp "$DIST_SRC/bunker-linux-amd64" "$DIST_SRC/bunkerd-linux-amd64" "$MIRROR/download/v0.1.4/"

if command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1; then
	REL_PFX="$TMP_ROOT/prefix-release"
	mkdir -p "$REL_PFX"
	OUT=$(BUNKER_RELEASES_URL="file://$MIRROR" "$SH_BIN" "$INSTALL" --dir "$REL_PFX" 2>&1) && RC=0 || RC=$?
	check_rc 0 "default mode installs from the release mirror (latest)" "$RC" "$OUT"
	contains "default mode uses the /latest/download/ asset URLs" "$OUT" "/latest/download/bunker-linux-amd64"
	if [ -x "$REL_PFX/bunker" ] && [ -x "$REL_PFX/bunkerd" ]; then
		pass "default mode installed both binaries"
	else
		fail "default mode installed both binaries" "found: $(ls -A "$REL_PFX")"
	fi

	TAG_PFX="$TMP_ROOT/prefix-release-tag"
	mkdir -p "$TAG_PFX"
	OUT=$(BUNKER_RELEASES_URL="file://$MIRROR" "$SH_BIN" "$INSTALL" --version v0.1.4 --dir "$TAG_PFX" 2>&1) && RC=0 || RC=$?
	check_rc 0 "--version vX.Y.Z installs a pinned tag" "$RC" "$OUT"
	contains "--version pins the /download/<tag>/ asset URLs" "$OUT" "/download/v0.1.4/bunker-linux-amd64"

	# a checksum mismatch on the release path refuses too, installing nothing
	BAD_MIRROR="$TMP_ROOT/mirror-bad/releases"
	mkdir -p "$BAD_MIRROR/latest/download"
	cp "$DIST_SRC/bunker-linux-amd64" "$DIST_SRC/bunkerd-linux-amd64" "$BAD_MIRROR/latest/download/"
	{
		printf '%s  bunker-linux-amd64\n' "0000000000000000000000000000000000000000000000000000000000000000"
		printf '%s  bunkerd-linux-amd64\n' "$(sha256 "$DIST_SRC/bunkerd-linux-amd64")"
	} >"$BAD_MIRROR/latest/download/SHA256SUMS"
	BAD_REL_PFX="$TMP_ROOT/prefix-release-bad"
	mkdir -p "$BAD_REL_PFX"
	OUT=$(BUNKER_RELEASES_URL="file://$BAD_MIRROR" "$SH_BIN" "$INSTALL" --dir "$BAD_REL_PFX" 2>&1) && RC=0 || RC=$?
	check_rc 42 "release checksum mismatch refuses with exit 42" "$RC"
	if [ -z "$(ls -A "$BAD_REL_PFX")" ]; then
		pass "release checksum mismatch installed nothing"
	else
		fail "release checksum mismatch installed nothing" "found: $(ls -A "$BAD_REL_PFX")"
	fi
else
	fail "release path (needs curl or wget)" "neither curl nor wget is available"
fi

# ── 9. --dry-run changes nothing ────────────────────────────────────────────
DRY_PFX="$TMP_ROOT/prefix-dry-run"
mkdir -p "$DRY_PFX"
run "$SH_BIN" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$DRY_PFX" --dry-run
check_rc 0 "--dry-run exits 0" "$RC" "$OUT"
contains "--dry-run says it changed nothing" "$OUT" "dry-run"
if [ -z "$(ls -A "$DRY_PFX")" ]; then
	pass "--dry-run wrote nothing into the prefix"
else
	fail "--dry-run wrote nothing into the prefix" "found: $(ls -A "$DRY_PFX")"
fi

# ── 10. --build with no Go toolchain: refusal, never Error 127 ──────────────
BUILD_PFX="$TMP_ROOT/prefix-build-nogo"
mkdir -p "$BUILD_PFX"
OUT=$(env PATH=/nonexistent INSTALL_SH_TEST_OS=linux INSTALL_SH_TEST_ARCH=amd64 \
	"${SH_ABS:-sh}" "$INSTALL" --build --dir "$BUILD_PFX" 2>&1) && RC=0 || RC=$?
check_rc 42 "--build without Go refuses with exit 42" "$RC"
contains "--build refusal names the README Install section" "$OUT" "README.md"
contains "--build refusal names the Go tarball to fetch" "$OUT" "go.dev/dl/go"
contains "--build refusal names the PATH export" "$OUT" "export PATH=/usr/local/go/bin"
contains "--build refusal explains the GOPATH==GOROOT trap" "$OUT" "GOPATH == GOROOT"
lacks "--build refusal never leaks 'go: not found' (Error 127)" "$OUT" "go: not found"
lacks "--build refusal never leaks an exit-127 error" "$OUT" "Error 127"

if [ -z "$(ls -A "$BUILD_PFX")" ]; then
	pass "--build without Go installed nothing"
else
	fail "--build without Go installed nothing" "found: $(ls -A "$BUILD_PFX")"
fi

# ── 11. --build with Go present really builds (skipped when Go is absent) ───
if command -v go >/dev/null 2>&1 && [ -f "$ROOT/go.mod" ]; then
	BUILD_OK_PFX="$TMP_ROOT/prefix-build-ok"
	mkdir -p "$BUILD_OK_PFX"
	OUT=$(cd "$ROOT" && "$SH_BIN" "$INSTALL" --build --dir "$BUILD_OK_PFX" 2>&1) && RC=0 || RC=$?
	check_rc 0 "--build with Go present exits 0" "$RC" "$OUT"
	if [ -x "$BUILD_OK_PFX/bunker" ] && [ -x "$BUILD_OK_PFX/bunkerd" ]; then
		pass "--build installed both binaries"
	else
		fail "--build installed both binaries" "found: $(ls -A "$BUILD_OK_PFX")"
	fi
	contains "--build stamps a version" "$("$BUILD_OK_PFX/bunker" --version 2>&1 || true)" "bunker "
else
	printf 'SKIP  --build with Go present (no Go toolchain on this host)\n'
fi

# ── 11b. --sshfs (MOUNT-009): parser, default-off, and mode behaviour ────────

# bare --sshfs parses (accepted, exit 0 on --help-free dry run later; here we
# only pin that the parser does not refuse it). The version-unresolvable sshfs
# ahead on PATH forces the "needs install" path, so the announce is assertable
# regardless of the (patched) real sshfs a dev host may carry in /usr/local/bin.
SSHFS_DIR="$TMP_ROOT/sshfs-bin"
mkdir -p "$SSHFS_DIR"
BROKEN_SSHFS_DIR="$TMP_ROOT/sshfs-broken"
mkdir -p "$BROKEN_SSHFS_DIR"
printf '#!/bin/sh\nprintf "sshfs (no version info)\\n"\n' >"$BROKEN_SSHFS_DIR/sshfs"
chmod 0755 "$BROKEN_SSHFS_DIR/sshfs"
OUT=$(PATH="$BROKEN_SSHFS_DIR:$SSHFS_DIR:$PATH" "$SH_BIN" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$DRY_PFX" --dry-run --sshfs 2>&1) && RC=0 || RC=$?
check_rc 0 "bare --sshfs is accepted" "$RC" "$OUT"
contains "bare --sshfs dry-run announces the package-manager step" "$OUT" "dry-run: would run: apt-get install -y sshfs"

OUT=$("$SH_BIN" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$DRY_PFX" --dry-run --sshfs=bogus 2>&1) && RC=0 || RC=$?
check_rc 42 "--sshfs=bogus refuses with exit 42" "$RC"
contains "--sshfs=bogus refusal names the valid modes" "$OUT" "need min, package or source"

# Default (no --sshfs): NO sshfs action whatsoever, even on a host with an
# affected sshfs and no package-manager fixtures on PATH.
FAKE_SSHFS_DIR="$TMP_ROOT/sshfs-old"
fake_sshfs "$FAKE_SSHFS_DIR" 3.7.3
fake_apt_get "$SSHFS_DIR"
fake_apt_cache "$SSHFS_DIR"
OUT=$(PATH="$FAKE_SSHFS_DIR:$SSHFS_DIR:$PATH" "$SH_BIN" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$DRY_PFX" --dry-run 2>&1) && RC=0 || RC=$?
check_rc 0 "default run (no --sshfs) exits 0 with an affected sshfs on PATH" "$RC"
lacks "default run performs no sshfs actions" "$OUT" "sshfs"
if [ -z "$(ls -A "$FAKE_SSHFS_DIR" | grep -v '^sshfs$')" ]; then
	pass "default run left the host's sshfs alone"
else
	fail "default run left the host's sshfs alone" "found: $(ls -A "$FAKE_SSHFS_DIR")"
fi

# --sshfs=min on a host whose sshfs already satisfies >= 3.7.6: a no-op with
# an informational line, and the package manager is never invoked.
MIN_OK_DIR="$TMP_ROOT/sshfs-min-ok"
fake_sshfs "$MIN_OK_DIR" 3.7.6
OUT=$(PATH="$MIN_OK_DIR:$SSHFS_DIR:$PATH" "$SH_BIN" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$DRY_PFX" --dry-run --sshfs=min 2>&1) && RC=0 || RC=$?
check_rc 0 "--sshfs=min with patched sshfs exits 0" "$RC"
contains "--sshfs=min with patched sshfs says so" "$OUT" "already satisfies"
lacks "--sshfs=min with patched sshfs never invokes the package manager" "$OUT" "apt-get install"

# --sshfs=min with an affected sshfs: asks the package manager (dry-run
# announces it), never runs a real apt-get (the fake one fails loudly).
OUT=$(PATH="$FAKE_SSHFS_DIR:$SSHFS_DIR:$PATH" "$SH_BIN" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$DRY_PFX" --dry-run --sshfs=min 2>&1) && RC=0 || RC=$?
check_rc 0 "--sshfs=min with affected sshfs exits 0" "$RC"
contains "--sshfs=min with affected sshfs announces the install" "$OUT" "apt-get install -y sshfs"
lacks "--sshfs=min never reaches a real package manager in tests" "$OUT" "FAKE-APT-GET-WAS-INVOKED"

# --sshfs=min on a host with NO USABLE sshfs (the broken fake's output parses
# to nothing): same announce, no real call.
OUT=$(PATH="$BROKEN_SSHFS_DIR:$SSHFS_DIR:$PATH" "$SH_BIN" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$DRY_PFX" --dry-run --sshfs 2>&1) && RC=0 || RC=$?
check_rc 0 "bare --sshfs with no usable host sshfs exits 0" "$RC"
contains "bare --sshfs with no usable host sshfs announces the install" "$OUT" "apt-get install -y sshfs"

# --sshfs=package with an old candidate refuses (42) BEFORE any install; the
# candidate comes from the fake apt-cache, the install would hit the fake
# (failing, loud) apt-get.
PKG_PFX="$TMP_ROOT/prefix-sshfs-package"
mkdir -p "$PKG_PFX"
OUT=$(SSHFS_TEST_CANDIDATE="3.7.3-1.1build5" PATH="$FAKE_SSHFS_DIR:$SSHFS_DIR:$PATH" "$SH_BIN" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$PKG_PFX" --sshfs=package 2>&1) && RC=0 || RC=$?
check_rc 42 "--sshfs=package refuses when the candidate is affected" "$RC"
contains "--sshfs=package refusal names the candidate" "$OUT" "3.7.3-1.1build5"
contains "--sshfs=package refusal names the affected range" "$OUT" "CVE-2026-47187"
lacks "--sshfs=package refusal never attempts the install" "$OUT" "FAKE-APT-GET-WAS-INVOKED"
if [ ! -e "$PKG_PFX/sshfs" ]; then
	pass "--sshfs=package refusal installed no sshfs"
else
	fail "--sshfs=package refusal installed no sshfs" "$PKG_PFX/sshfs exists"
fi

# --sshfs=package with a satisfying candidate installs through the normal
# path (here: --from-dir content) and never refuses.
OUT=$(SSHFS_TEST_CANDIDATE="3.7.6-1" PATH="$MIN_OK_DIR:$SSHFS_DIR:$PATH" "$SH_BIN" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$PKG_PFX" --sshfs=package 2>&1) && RC=0 || RC=$?
check_rc 0 "--sshfs=package with a patched candidate exits 0" "$RC" "$OUT"
contains "--sshfs=package with a patched candidate reports no action needed" "$OUT" "already satisfies"

# --sshfs=source dry-run: announces the pinned clone/build/install, writes
# nothing, and refuses nothing on hosts without meson/ninja (no tool probing
# happens before the dry-run gate).
OUT=$("$SH_BIN" "$INSTALL" --from-dir "$GOOD_SRC" --dir "$DRY_PFX" --dry-run --sshfs=source 2>&1) && RC=0 || RC=$?
check_rc 0 "--sshfs=source dry-run exits 0" "$RC"
contains "--sshfs=source dry-run names the pinned tag" "$OUT" "sshfs-3.7.6"
contains "--sshfs=source dry-run names the pinned commit" "$OUT" "7a2d988775446ebe7af9b01c99b3b8e86bddb05a"
contains "--sshfs=source dry-run names meson" "$OUT" "meson setup"
contains "--sshfs=source dry-run targets DEST/sshfs" "$OUT" "then install it to $DRY_PFX/sshfs"
lacks "--sshfs=source dry-run downloads nothing" "$OUT" "cloning sshfs"

# ── summary ─────────────────────────────────────────────────────────────────
printf '\ninstall_test.sh: %d passed, %d failed\n' "$PASS_COUNT" "$FAIL_COUNT"
if [ "$FAIL_COUNT" -ne 0 ]; then
	exit 1
fi
exit 0
