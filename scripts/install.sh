#!/bin/sh
# ══════════════════════════════════════════════════════════════════════════════
# install.sh — install `bunker` and `bunkerd` with one command (DF-BUNKER-25).
#
#   curl -fsSL https://github.com/deployBunker/bunker/releases/latest/download/install.sh | sh
#
# Three sources, one "verify first, then install" code path:
#
#   (default)       download the prebuilt release assets for this platform from
#                   the GitHub release and verify both binaries against the
#                   release's SHA256SUMS before anything is written
#   --from-dir DIR  install the binaries sitting in a local directory (offline /
#                   air-gapped users, and the repo's own tests); verifies
#                   DIR/SHA256SUMS when that file is present
#   --build         build from this checkout with `go build` (no `make` needed);
#                   requires a Go toolchain, and refuses with instructions when
#                   one is missing
#
# Exit codes: 0 on success, 42 for a refusal (unsupported platform, checksum
# mismatch, missing checksum entry, no Go toolchain for --build — the same
# "refusal" convention e2e-full-battery.sh uses), 1 for any other error.
#
# Privileges: this script never escalates. It installs into --dir when that
# directory is writable, falls back to $HOME/.local/bin when the default
# /usr/local/bin is not writable, and otherwise refuses and tells you to re-run
# under sudo yourself.
#
# No secrets, no telemetry. The only writes are the installed binaries in the
# target directory plus a temporary directory that is always cleaned up.
#
# Environment:
#   BUNKER_RELEASES_URL  override the release base URL (air-gapped mirrors and
#                        the test suite; default https://github.com/deployBunker/bunker/releases)
#   INSTALL_SH_TEST_OS   override the detected OS, e.g. "darwin" (tests only)
#   INSTALL_SH_TEST_ARCH override the detected architecture (tests only)
#
# sshfs (MOUNT-009): by default this installer NEVER touches the host's sshfs.
# Pass --sshfs[=MODE] to opt in (see usage). sshfs <= 3.7.5 is affected by
# CVE-2026-47187 / CVE-2026-48711; the pinned fix is 3.7.6
# (docs/sshfs-3.7.6-deployment.md).
# ══════════════════════════════════════════════════════════════════════════════
set -eu

PROG=install.sh
REPO=deployBunker/bunker
RELEASES_URL=${BUNKER_RELEASES_URL:-https://github.com/deployBunker/bunker/releases}
DEFAULT_DEST=/usr/local/bin
# The repo's convention for "declined to proceed" (e2e-full-battery.sh).
EXIT_REFUSE=42
# Go version the source tree requires when go.mod cannot be read.
GO_FALLBACK=1.26.5

# ── output ──────────────────────────────────────────────────────────────────
say() { printf '%s: %s\n' "$PROG" "$*"; }
warn() { printf '%s: warning: %s\n' "$PROG" "$*" >&2; }
die() {
	printf '%s: error: %s\n' "$PROG" "$*" >&2
	exit 1
}
refuse() {
	printf '%s: refusing to continue: %s\n' "$PROG" "$*" >&2
	exit "$EXIT_REFUSE"
}

usage() {
	cat <<'EOF'
install.sh — install the bunker CLI and the bunkerd daemon.

Usage:
  install.sh [--version vX.Y.Z] [--dir PREFIX] [--dry-run]
  install.sh --from-dir DIR [--dir PREFIX] [--dry-run]
  install.sh --build [--dir PREFIX] [--dry-run]

Default: download the prebuilt release assets for this platform (linux/amd64 or
linux/arm64), verify them against the release's SHA256SUMS, then install.

sshfs is NOT installed or upgraded unless --sshfs is passed; the default run
leaves whatever sshfs the host already has exactly as it is.

Options:
  --version vX.Y.Z  install that release tag instead of the newest release
  --from-dir DIR    install the binaries in DIR (offline/air-gapped); the
                    checksums in DIR/SHA256SUMS are verified when present
  --build           build bunker and bunkerd from this checkout with go build
                    (no make, no prebuilt asset; needs a Go toolchain)
  --dir PREFIX      install prefix (default /usr/local/bin; falls back to
                    $HOME/.local/bin when the default is not writable)
  --dry-run         print every action without downloading, writing or building
  --sshfs[=MODE]    after the bunker install, also ensure the host's sshfs.
                    MODE is one of:
                      min (default)  install the newest sshfs the distro offers
                                     when none or < 3.7.6; WARN when the result
                                     is still in the affected range (< 3.7.6)
                      package        like min, but REFUSE (42) when the distro
                                     cannot provide >= 3.7.6
                      source         build sshfs 3.7.6 from the pinned upstream
                                     tag (checksum-verified) and install it to
                                     <prefix>/sshfs; needs meson, ninja and git
                    Without =MODE, --sshfs means --sshfs=min.
  -h, --help        print this help

Exit codes: 0 success, 42 refusal (unsupported platform, checksum mismatch,
missing checksum entry, no Go for --build, sshfs mode refusals), 1 any other
error.

Examples:
  curl -fsSL https://github.com/deployBunker/bunker/releases/latest/download/install.sh | sh
  sh install.sh --version v0.1.4 --dir "$HOME/.local/bin"
  sh install.sh --from-dir ./dist
  sh install.sh --build
  sh install.sh --sshfs=source --dir "$HOME/.local/bin"   # also build+install patched sshfs
EOF
}

# ── argument parsing ────────────────────────────────────────────────────────
MODE=release
FROM_DIR=
VERSION=
DEST=$DEFAULT_DEST
DEST_EXPLICIT=0
DRY_RUN=0
# SSHFS_MODE empty = the flag was not passed = sshfs is NOT touched.
SSHFS_MODE=

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--from-dir)
			[ $# -ge 2 ] || refuse "--from-dir needs a directory argument"
			FROM_DIR=$2
			MODE=dir
			shift 2
			;;
		--from-dir=*)
			FROM_DIR=${1#--from-dir=}
			MODE=dir
			shift
			;;
		--version)
			[ $# -ge 2 ] || refuse "--version needs a release tag argument (e.g. --version v0.1.4)"
			VERSION=$2
			shift 2
			;;
		--version=*)
			VERSION=${1#--version=}
			shift
			;;
		--build)
			MODE=build
			shift
			;;
		--sshfs)
			SSHFS_MODE=min
			shift
			;;
		--sshfs=*)
			SSHFS_MODE=${1#--sshfs=}
			case "$SSHFS_MODE" in
			min | package | source) ;;
			*)
				refuse "--sshfs: unknown mode \"$SSHFS_MODE\" (need min, package or source; try --help)"
				;;
			esac
			shift
			;;
		--dir)
			[ $# -ge 2 ] || refuse "--dir needs a prefix argument"
			DEST=$2
			DEST_EXPLICIT=1
			shift 2
			;;
		--dir=*)
			DEST=${1#--dir=}
			DEST_EXPLICIT=1
			shift
			;;
		--dry-run)
			DRY_RUN=1
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			refuse "unknown argument \"$1\" (try --help)"
			;;
		esac
	done

	if [ "$MODE" = dir ] && [ -z "$FROM_DIR" ]; then
		refuse "--from-dir needs a non-empty directory"
	fi
	if [ -n "$VERSION" ]; then
		case "$VERSION" in
		*..* | */*) refuse "--version must be a plain release tag, got \"$VERSION\"" ;;
		esac
	fi
}

# ── platform detection ──────────────────────────────────────────────────────
# INSTALL_SH_TEST_OS/ARCH exist so the platform gate is testable without faking
# `uname`; they are documented in the header and used by scripts/install_test.sh.
detect_os() {
	if [ -n "${INSTALL_SH_TEST_OS:-}" ]; then
		printf '%s' "$INSTALL_SH_TEST_OS"
		return 0
	fi
	uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]'
}

detect_arch() {
	if [ -n "${INSTALL_SH_TEST_ARCH:-}" ]; then
		printf '%s' "$INSTALL_SH_TEST_ARCH"
		return 0
	fi
	case "$(uname -m 2>/dev/null)" in
	x86_64 | amd64) printf 'amd64' ;;
	aarch64 | arm64) printf 'arm64' ;;
	*) uname -m 2>/dev/null ;;
	esac
}

# require_supported_platform refuses (42) on anything but linux/amd64,linux/arm64.
require_supported_platform() {
	case "$OS" in
	linux) ;;
	*)
		refuse "unsupported operating system \"$OS\": this installer supports linux/amd64 and linux/arm64 only (the release assets are cross-compiled for those two targets). Build from source on this host instead: see the \"Install\" section of README.md."
		;;
	esac
	case "$ARCH" in
	amd64 | arm64) ;;
	*)
		refuse "unsupported architecture \"$ARCH\": this installer supports linux/amd64 and linux/arm64 only. Build from source on this host instead: see the \"Install\" section of README.md."
		;;
	esac
}

# ── target directory ────────────────────────────────────────────────────────
# dir_usable DIR: 0 when DIR exists (or can be created) and is writable.
# It creates the directory unless DRY_RUN=1, so a dry run changes nothing.
dir_usable() {
	du_dir=$1
	if [ -e "$du_dir" ] && [ ! -d "$du_dir" ]; then
		return 1
	fi
	if [ ! -d "$du_dir" ]; then
		if [ "$DRY_RUN" = 1 ]; then
			return 0
		fi
		mkdir -p "$du_dir" 2>/dev/null || return 1
	fi
	[ -w "$du_dir" ]
}

# choose_dest resolves DEST to a writable directory, falling back to
# $HOME/.local/bin when the default is not writable. An explicit --dir is never
# silently redirected: it either works or the script refuses.
choose_dest() {
	case "$DEST" in
	/*) ;;
	*) refuse "--dir must be an absolute path, got \"$DEST\"" ;;
	esac

	if dir_usable "$DEST"; then
		return 0
	fi

	if [ "$DEST_EXPLICIT" = 1 ]; then
		refuse "--dir $DEST is not a writable directory (running as $(id -un 2>/dev/null || printf 'this user')). Choose a writable prefix such as --dir \"\$HOME/.local/bin\", or run this installer under sudo yourself — it never escalates on its own."
	fi

	[ -n "${HOME:-}" ] || refuse "the default install directory $DEST is not writable and HOME is unset — pass --dir <writable directory>"

	fallback=$HOME/.local/bin
	say "note: $DEST is not writable by $(id -un 2>/dev/null || printf 'this user') — falling back to $fallback (override with --dir)"
	DEST=$fallback

	if ! dir_usable "$DEST"; then
		refuse "$DEST is not writable either — pass --dir <writable directory>"
	fi
}

# ── temp dir ────────────────────────────────────────────────────────────────
TMP_DIR=

ensure_tmp() {
	if [ -n "$TMP_DIR" ]; then
		return 0
	fi
	TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/bunker-install.XXXXXX") || die "cannot create a temporary directory"
}

cleanup() {
	if [ -n "${TMP_DIR:-}" ] && [ -d "$TMP_DIR" ]; then
		rm -rf "$TMP_DIR"
	fi
}

# ── checksums ───────────────────────────────────────────────────────────────
require_sha256_tool() {
	if command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || command -v openssl >/dev/null 2>&1; then
		return 0
	fi
	refuse "no sha256 tool found on PATH (need one of sha256sum, shasum, openssl) — cannot verify the binaries"
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		openssl dgst -sha256 "$1" | awk '{print $NF}'
	fi
}

# checksum_entry NAME SUMSFILE: print the recorded hash for NAME, or nothing.
checksum_entry() {
	awk -v want="$1" '
		{ name = $2; sub(/^\*/, "", name); if (name == want) { print $1; exit } }
	' "$2"
}

# verify_file FILE SUMSFILE: refuse (42) unless FILE's sha256 matches its
# SHA256SUMS entry. A missing or malformed entry is a refusal, never a warning.
verify_file() {
	vf_file=$1
	vf_sums=$2
	vf_name=${vf_file##*/}
	vf_want=$(checksum_entry "$vf_name" "$vf_sums")

	if [ -z "$vf_want" ]; then
		refuse "no SHA256SUMS entry for $vf_name in $vf_sums — refusing to install an unverified binary"
	fi
	case "$vf_want" in
	*[!0-9a-fA-F]*)
		refuse "malformed SHA256SUMS entry for $vf_name in $vf_sums (\"$vf_want\" is not a hex digest)"
		;;
	esac
	if [ ${#vf_want} -ne 64 ]; then
		refuse "malformed SHA256SUMS entry for $vf_name in $vf_sums (expected 64 hex characters, got ${#vf_want})"
	fi

	vf_got=$(sha256_of "$vf_file")
	vf_want_lc=$(printf '%s' "$vf_want" | tr 'A-F' 'a-f')
	vf_got_lc=$(printf '%s' "$vf_got" | tr 'A-F' 'a-f')

	if [ "$vf_got_lc" != "$vf_want_lc" ]; then
		refuse "checksum mismatch for $vf_name: SHA256SUMS says $vf_want_lc, the file hashes to $vf_got_lc — the download is corrupt or tampered with. Nothing was installed."
	fi

	say "verified $vf_name (sha256 $vf_got_lc)"
}

# ── release assets ──────────────────────────────────────────────────────────
asset_name() { # asset_name BINARY
	printf '%s-%s-%s' "$1" "$OS" "$ARCH"
}

asset_url() { # asset_url ASSET
	if [ -z "$VERSION" ]; then
		printf '%s/latest/download/%s' "$RELEASES_URL" "$1"
	else
		printf '%s/download/%s/%s' "$RELEASES_URL" "$VERSION" "$1"
	fi
}

fetch() { # fetch URL OUTPATH
	fe_url=$1
	fe_out=$2
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$fe_url" -o "$fe_out" || die "download failed: $fe_url (check the tag and your network, or use --from-dir/--build)"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$fe_out" "$fe_url" || die "download failed: $fe_url (check the tag and your network, or use --from-dir/--build)"
	else
		refuse "neither curl nor wget is on PATH — install one, or use --from-dir/--build"
	fi
	say "downloaded $fe_url"
}

# ── source resolution ───────────────────────────────────────────────────────
STAGE_DIR=
STAGED_BUNKER=
STAGED_BUNKERD=

stage_one() { # stage_one NAME SOURCE
	so_name=$1
	so_src=$2
	ensure_tmp
	[ -n "$STAGE_DIR" ] || STAGE_DIR=$TMP_DIR/stage
	mkdir -p "$STAGE_DIR"
	cp "$so_src" "$STAGE_DIR/$so_name" || die "cannot copy $so_src"
	chmod 0755 "$STAGE_DIR/$so_name"
	case "$so_name" in
	bunker) STAGED_BUNKER=$STAGE_DIR/$so_name ;;
	bunkerd) STAGED_BUNKERD=$STAGE_DIR/$so_name ;;
	esac
}

# from_release: download the three assets, verify both binaries, then stage.
from_release() {
	req_sums=SHA256SUMS
	req_bunker=$(asset_name bunker)
	req_bunkerd=$(asset_name bunkerd)

	if [ "$DRY_RUN" = 1 ]; then
		say "dry-run: release source $RELEASES_URL (version ${VERSION:-latest})"
		say "dry-run: would download $(asset_url "$req_sums")"
		say "dry-run: would download $(asset_url "$req_bunker")"
		say "dry-run: would download $(asset_url "$req_bunkerd")"
		say "dry-run: would verify both binaries against SHA256SUMS, then install them into $DEST"
		return 0
	fi

	ensure_tmp
	assets=$TMP_DIR/assets
	mkdir -p "$assets"

	fetch "$(asset_url "$req_sums")" "$assets/$req_sums"
	fetch "$(asset_url "$req_bunker")" "$assets/$req_bunker"
	fetch "$(asset_url "$req_bunkerd")" "$assets/$req_bunkerd"

	require_sha256_tool
	verify_file "$assets/$req_bunker" "$assets/$req_sums"
	verify_file "$assets/$req_bunkerd" "$assets/$req_sums"

	stage_one bunker "$assets/$req_bunker"
	stage_one bunkerd "$assets/$req_bunkerd"
}

# from_dir: locate the two binaries in a local directory (dist/ layout first,
# plain names second), verify them against DIR/SHA256SUMS when it exists.
from_dir() {
	fd_dir=$FROM_DIR
	[ -d "$fd_dir" ] || refuse "--from-dir $fd_dir is not a directory"

	fd_bunker=
	fd_bunkerd=
	for fd_candidate in "$fd_dir/$(asset_name bunker)" "$fd_dir/bunker"; do
		if [ -f "$fd_candidate" ]; then
			fd_bunker=$fd_candidate
			break
		fi
	done
	for fd_candidate in "$fd_dir/$(asset_name bunkerd)" "$fd_dir/bunkerd"; do
		if [ -f "$fd_candidate" ]; then
			fd_bunkerd=$fd_candidate
			break
		fi
	done

	if [ -z "$fd_bunker" ] || [ -z "$fd_bunkerd" ]; then
		refuse "--from-dir $fd_dir does not contain both binaries (looked for $(asset_name bunker), bunker, $(asset_name bunkerd), bunkerd)"
	fi

	if [ "$DRY_RUN" = 1 ]; then
		say "dry-run: local source $fd_dir"
		say "dry-run: would install $fd_bunker and $fd_bunkerd into $DEST"
		if [ -f "$fd_dir/SHA256SUMS" ]; then
			say "dry-run: would verify both against $fd_dir/SHA256SUMS"
		else
			say "dry-run: no $fd_dir/SHA256SUMS — would install without checksum verification"
		fi
		return 0
	fi

	if [ -f "$fd_dir/SHA256SUMS" ]; then
		require_sha256_tool
		verify_file "$fd_bunker" "$fd_dir/SHA256SUMS"
		verify_file "$fd_bunkerd" "$fd_dir/SHA256SUMS"
	else
		warn "no SHA256SUMS in $fd_dir — installing $fd_bunker and $fd_bunkerd without checksum verification"
	fi

	stage_one bunker "$fd_bunker"
	stage_one bunkerd "$fd_bunkerd"
}

# ── build from source ───────────────────────────────────────────────────────
# resolve_repo_root finds the checkout --build should compile: INSTALL_SH_REPO_DIR
# wins, then the script's own directory when it is a real file, then $PWD.
resolve_repo_root() {
	for rr_candidate in "${INSTALL_SH_REPO_DIR:-}" "$(script_dir)" "$PWD"; do
		if [ -n "$rr_candidate" ] && [ -f "$rr_candidate/go.mod" ]; then
			printf '%s' "$rr_candidate"
			return 0
		fi
	done
	refuse "--build needs a bunker checkout (a directory with go.mod): run it from the repo root, or set INSTALL_SH_REPO_DIR=<checkout>"
}

script_dir() {
	case "${0:-}" in
	*/*) (CDPATH= cd -- "$(dirname -- "$0")/.." && pwd) ;;
	*) printf '' ;;
	esac
}

# require_go refuses (42) with the documented recovery steps when no Go
# toolchain is on PATH — never let the user see `sh: 1: go: not found` (127).
require_go() {
	if command -v go >/dev/null 2>&1; then
		return 0
	fi
	go_ver=$GO_FALLBACK
	if [ -n "${REPO_DIR:-}" ] && [ -f "$REPO_DIR/go.mod" ]; then
		go_parsed=$(awk '/^go[[:space:]]/ { print $2; exit }' "$REPO_DIR/go.mod" 2>/dev/null || printf '')
		if [ -n "$go_parsed" ]; then
			go_ver=$go_parsed
		fi
	fi
	refuse "no Go toolchain on PATH — --build needs one.
  Install Go first; the \"Install\" section of README.md documents this
  (\"Option 2 — build from source\" → \"Installing Go on a stock Debian/Ubuntu host\"):

    curl -fsSL https://go.dev/dl/go$go_ver.linux-$ARCH.tar.gz -o /tmp/go.tar.gz
    sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tar.gz
    export PATH=/usr/local/go/bin:\$PATH

  Extract into /usr/local/go, NOT into \$HOME: a tarball unpacked in \$HOME makes
  GOPATH == GOROOT and every go command warns about it.
  No toolchain wanted? Install the prebuilt release binaries instead:
    curl -fsSL https://github.com/deployBunker/bunker/releases/latest/download/install.sh | sh"
}

build_from_checkout() {
	if [ "$DRY_RUN" = 1 ]; then
		say "dry-run: build source $REPO_DIR (go build -ldflags ...)"
		say "dry-run: would build ./cmd/bunker and ./cmd/bunkerd into $DEST"
		return 0
	fi

	ensure_tmp
	mkdir -p "$TMP_DIR/build"

	bd_version=$VERSION
	if [ -z "$bd_version" ]; then
		if command -v git >/dev/null 2>&1 && git -C "$REPO_DIR" describe --tags --abbrev=0 >/dev/null 2>&1; then
			bd_version=$(git -C "$REPO_DIR" describe --tags --abbrev=0)
			bd_version=${bd_version#v}
		else
			bd_version=$GO_FALLBACK
		fi
	fi
	case "$bd_version" in
	v*) bd_version=${bd_version#v} ;;
	esac

	bd_commit=unknown
	bd_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	if command -v git >/dev/null 2>&1; then
		bd_commit=$(git -C "$REPO_DIR" rev-parse --short HEAD 2>/dev/null || printf 'unknown')
	fi

	bd_ldflags="-X github.com/deployBunker/bunker/internal/version.Version=$bd_version -X github.com/deployBunker/bunker/internal/version.Commit=$bd_commit -X github.com/deployBunker/bunker/internal/version.BuildDate=$bd_date"

	say "building $REPO_DIR with go build (version $bd_version, commit $bd_commit)"
	for bd_name in bunker bunkerd; do
		bd_out=$TMP_DIR/build/$bd_name
		(cd "$REPO_DIR" && go build -ldflags "$bd_ldflags" -o "$bd_out" "./cmd/$bd_name") ||
			die "go build ./cmd/$bd_name failed in $REPO_DIR"
		stage_one "$bd_name" "$bd_out"
	done
}

# ── install ─────────────────────────────────────────────────────────────────
install_one() { # install_one NAME
	io_name=$1
	io_src=$STAGE_DIR/$io_name
	io_target=$DEST/$io_name
	io_tmp=$DEST/.$io_name.$$.tmp

	[ -f "$io_src" ] || die "internal error: $io_src is missing after staging"
	cp "$io_src" "$io_tmp" || die "cannot write $io_tmp"
	chmod 0755 "$io_tmp"
	mv -f "$io_tmp" "$io_target" || die "cannot install $io_target"
	say "installed $io_target"
}

install_binaries() {
	if [ "$DRY_RUN" = 1 ]; then
		say "dry-run: would install bunker -> $DEST/bunker (mode 0755)"
		say "dry-run: would install bunkerd -> $DEST/bunkerd (mode 0755)"
		return 0
	fi
	install_one bunker
	install_one bunkerd
}

# smoke_check runs the installed CLI's --version, so a binary that cannot start
# or that carries no version stamp is visible immediately.
smoke_check() {
	sc_bin=$DEST/bunker
	if [ ! -x "$sc_bin" ]; then
		warn "smoke check skipped: $sc_bin is not executable"
		return 0
	fi
	if ! sc_out=$("$sc_bin" --version 2>&1); then
		warn "smoke check FAILED: $sc_bin --version exited non-zero"
		printf '%s\n' "$sc_out" >&2
		return 1
	fi
	printf '%s\n' "$sc_out"
	sc_first=$(printf '%s\n' "$sc_out" | head -n 1)
	case "$sc_first" in
	"bunker "*)
		sc_ver=${sc_first#bunker }
		case "$sc_ver" in
		"" | dev | unknown)
			warn "smoke check: $sc_bin reports no version stamp (\"$sc_first\") — the build was not stamped with ldflags"
			;;
		*)
			say "smoke check OK: $sc_bin --version -> $sc_ver"
			;;
		esac
		;;
	*)
		warn "smoke check: unexpected --version output from $sc_bin: \"$sc_first\""
		;;
	esac
	return 0
}

# ── sshfs (MOUNT-009, opt-in via --sshfs[=MODE]) ────────────────────────────
# sshfsPatchedVersion is the fixed release: CVE-2026-47187 (symlink escape —
# a rogue SFTP server gains local file read/write on the client) and
# CVE-2026-48711 (argument injection — local command execution) are fixed in
# 3.7.6; everything below is in the affected range. Deployment record:
# docs/sshfs-3.7.6-deployment.md.
SSHFS_PINNED_VERSION=3.7.6
SSHFS_PINNED_TAG=sshfs-3.7.6
SSHFS_PINNED_COMMIT=7a2d988775446ebe7af9b01c99b3b8e86bddb05a

# sshfs_version_of BINARY: print the X.Y[.Z] version `BINARY --version`
# reports, or nothing when the binary is missing or its output is unparsable.
sshfs_version_of() {
	sv_bin=$1
	command -v "$sv_bin" >/dev/null 2>&1 || return 0
	sv_out=$("$sv_bin" --version 2>/dev/null || true)
	printf '%s\n' "$sv_out" | sed -n 's/.*[Vv]ersion \([0-9]\+\.[0-9]\+\(\.[0-9]\+\)\?\).*/\1/p' | head -n 1
}

# sshfs_version_lt A B: 0 when version A < B numerically (component-wise, so
# 3.10 > 3.7 — a plain string compare would get that wrong).
sshfs_version_lt() {
	svl_a=$1
	svl_b=$2
	svl_cmp=$(awk -v a="$svl_a" -v b="$svl_b" 'BEGIN {
		split(a, aa, "."); split(b, bb, ".")
		for (i = 1; i <= 3; i++) {
			x = (i in aa) ? aa[i]+0 : 0; y = (i in bb) ? bb[i]+0 : 0
			if (x < y) { print "lt"; exit }
			if (x > y) { print "gt"; exit }
		}
		print "eq"
	}')
	[ "$svl_cmp" = lt ]
}

# sshfs_report_version BINARY: print the resulting sshfs --version output and
# WARN loudly when the binary is still in the affected range.
sshfs_report_version() {
	sr_bin=$1
	if command -v "$sr_bin" >/dev/null 2>&1; then
		"$sr_bin" --version 2>&1 || true
		sr_ver=$(sshfs_version_of "$sr_bin")
		if [ -n "$sr_ver" ] && sshfs_version_lt "$sr_ver" "$SSHFS_PINNED_VERSION"; then
			warn "sshfs $sr_ver is STILL in the affected range (< $SSHFS_PINNED_VERSION) for CVE-2026-47187 / CVE-2026-48711 — re-run with --sshfs=source to build the patched release"
		fi
	else
		warn "$sr_bin is not on PATH after the sshfs install attempt"
	fi
}

# ensure_sshfs_min: bare --sshfs / --sshfs=min. After the bunker install,
# check the host's sshfs; when it is missing or < 3.7.6, install the newest
# version the host's package manager offers and print the resulting
# sshfs --version (with a loud warning when the result is still affected).
ensure_sshfs_min() {
	em_ver=$(sshfs_version_of sshfs)
	if [ -n "$em_ver" ] && ! sshfs_version_lt "$em_ver" "$SSHFS_PINNED_VERSION"; then
		say "sshfs $em_ver already satisfies >= $SSHFS_PINNED_VERSION — no action needed"
		return 0
	fi
	if [ -n "$em_ver" ]; then
		say "sshfs $em_ver is below $SSHFS_PINNED_VERSION (CVE-2026-47187 / CVE-2026-48711) — asking the package manager for its newest sshfs"
	else
		say "no sshfs found on PATH — asking the package manager to install one"
	fi
	em_rc=0
	if command -v apt-get >/dev/null 2>&1; then
		if [ "$DRY_RUN" = 1 ]; then
			say "dry-run: would run: apt-get install -y sshfs"
			return 0
		fi
		DEBIAN_FRONTEND=noninteractive apt-get install -y sshfs >/dev/null 2>&1 || em_rc=$?
	elif command -v dnf >/dev/null 2>&1; then
		if [ "$DRY_RUN" = 1 ]; then
			say "dry-run: would run: dnf install -y sshfs"
			return 0
		fi
		dnf install -y sshfs >/dev/null 2>&1 || em_rc=$?
	else
		if [ "$SSHFS_MODE" = min ]; then
			warn "no supported package manager found (need apt-get or dnf) — sshfs was NOT installed"
			return 0
		fi
		refuse "--sshfs=$SSHFS_MODE: no supported package manager found (need apt-get or dnf)"
	fi
	if [ "$em_rc" -ne 0 ]; then
		if [ "$SSHFS_MODE" = min ]; then
			warn "the package manager failed to install sshfs (exit $em_rc) — sshfs may be missing or outdated"
			return 0
		fi
		refuse "--sshfs=$SSHFS_MODE: the package manager failed to install sshfs (exit $em_rc)"
	fi
	sshfs_report_version sshfs
}

# ensure_sshfs_package: --sshfs=package. Same discovery as min, but a distro
# that cannot provide >= 3.7.6 is a refusal (42), not a warning.
ensure_sshfs_package() {
	ep_ver=$(sshfs_version_of sshfs)
	if [ -n "$ep_ver" ] && ! sshfs_version_lt "$ep_ver" "$SSHFS_PINNED_VERSION"; then
		say "sshfs $ep_ver already satisfies >= $SSHFS_PINNED_VERSION — no action needed"
		return 0
	fi
	command -v apt-get >/dev/null 2>&1 || command -v dnf >/dev/null 2>&1 ||
		refuse "--sshfs=package: no supported package manager found (need apt-get or dnf)"
	if [ "$DRY_RUN" = 1 ]; then
		if command -v apt-get >/dev/null 2>&1; then
			say "dry-run: would run: apt-get install -y sshfs and REFUSE if the candidate is < $SSHFS_PINNED_VERSION"
		else
			say "dry-run: would run: dnf install -y sshfs and REFUSE if the candidate is < $SSHFS_PINNED_VERSION"
		fi
		return 0
	fi
	ep_candidate=$("${SSHFS_TEST_APT_CACHE:-apt-cache}" policy sshfs 2>/dev/null | awk '/Candidate:/ {print $2; exit}')
	[ -n "$ep_candidate" ] || ep_candidate=$(dnf --showduplicates list sshfs 2>/dev/null | awk '$1 == "sshfs" && $2 !~ /^Available/ {print $2; exit}' | sed 's/-[0-9][0-9]*\..*//')
	if [ -z "$ep_candidate" ] || [ "$ep_candidate" = "(none)" ]; then
		refuse "--sshfs=package: the package manager offers no sshfs candidate at all — cannot satisfy >= $SSHFS_PINNED_VERSION"
	fi
	ep_norm=$(printf '%s' "$ep_candidate" | sed 's/^[0-9]*://; s/-[0-9][0-9]*$//; s/[^0-9.].*$//')
	if [ -z "$ep_norm" ] || sshfs_version_lt "$ep_norm" "$SSHFS_PINNED_VERSION"; then
		refuse "--sshfs=package: the best candidate is $ep_candidate, which is in the affected range (< $SSHFS_PINNED_VERSION) for CVE-2026-47187 / CVE-2026-48711 — re-run with --sshfs=source to build the patched release"
	fi
	ensure_sshfs_min
}

# ensure_sshfs_source: --sshfs=source. Build the pinned upstream release and
# install it to $DEST/sshfs. The pin is verified TWICE: the cloned HEAD must
# equal SSHFS_PINNED_COMMIT, and the built binary must keep the sha256
# recorded immediately after the build (before the install moves it).
ensure_sshfs_source() {
	command -v meson >/dev/null 2>&1 || refuse "--sshfs=source: meson is not installed — it is required to build sshfs from source (e.g. apt-get install meson ninja-build git)"
	command -v ninja >/dev/null 2>&1 || refuse "--sshfs=source: ninja is not installed — it is required to build sshfs from source (e.g. apt-get install meson ninja-build git)"
	command -v git >/dev/null 2>&1 || refuse "--sshfs=source: git is not installed — it is required to clone the pinned sshfs tag"

	es_src=$TMP_DIR/sshfs-src
	es_build=$TMP_DIR/sshfs-build
	if [ "$DRY_RUN" = 1 ]; then
		say "dry-run: would git clone --depth 1 --branch $SSHFS_PINNED_TAG https://github.com/libfuse/sshfs.git"
		say "dry-run: would verify HEAD == $SSHFS_PINNED_COMMIT"
		say "dry-run: would run: meson setup $es_build --buildtype release && ninja -C $es_build"
		say "dry-run: would record the binary's sha256, then install it to $DEST/sshfs"
		return 0
	fi

	ensure_tmp
	say "cloning sshfs $SSHFS_PINNED_TAG (depth 1)"
	git clone --depth 1 --branch "$SSHFS_PINNED_TAG" https://github.com/libfuse/sshfs.git "$es_src" ||
		die "git clone of sshfs $SSHFS_PINNED_TAG failed (check the network, or use --sshfs=min)"
	es_commit=$(git -C "$es_src" rev-parse HEAD)
	if [ "$es_commit" != "$SSHFS_PINNED_COMMIT" ]; then
		refuse "sshfs source verification failed: $SSHFS_PINNED_TAG HEAD is $es_commit, expected $SSHFS_PINNED_COMMIT — the pin moved upstream, refusing to build"
	fi
	say "verified clone at the pinned commit $es_commit"

	say "building sshfs with meson/ninja (release)"
	(meson setup "$es_build" "$es_src" --buildtype release >/dev/null 2>&1) ||
		die "meson setup failed — the sshfs build requirements (libfuse3-dev, glib2.0-dev) may be missing"
	(ninja -C "$es_build" >/dev/null 2>&1) ||
		die "ninja -C build failed — the sshfs build failed to compile"
	[ -f "$es_build/sshfs" ] || die "ninja did not produce $es_build/sshfs"

	es_sha=$(sha256_of "$es_build/sshfs")
	say "built sshfs (sha256 $es_sha)"
	install_one_sshfs "$es_build/sshfs"
	es_installed=$DEST/sshfs
	es_installed_sha=$(sha256_of "$es_installed")
	if [ "$es_installed_sha" != "$es_sha" ]; then
		refuse "sshfs install verification failed: the installed $es_installed hashes to $es_installed_sha but $es_sha was recorded before the install"
	fi
	say "verified installed sshfs (sha256 $es_installed_sha) matches the build"
	sshfs_report_version "$es_installed"
}

# install_one_sshfs SRC: install one staged binary to $DEST/sshfs, honouring
# --dry-run like install_binaries does.
install_one_sshfs() { # install_one_sshfs SRC
	if [ "$DRY_RUN" = 1 ]; then
		say "dry-run: would install $1 -> $DEST/sshfs (mode 0755)"
		return 0
	fi
	is_tmp=$DEST/.sshfs.$$.tmp
	cp "$1" "$is_tmp" || die "cannot write $is_tmp"
	chmod 0755 "$is_tmp"
	mv -f "$is_tmp" "$DEST/sshfs" || die "cannot install $DEST/sshfs"
	say "installed $DEST/sshfs"
}

# ensure_sshfs dispatches on SSHFS_MODE (empty = no-op: the default install
# never touches the host's sshfs).
ensure_sshfs() {
	case "$SSHFS_MODE" in
	'') return 0 ;;
	min) ensure_sshfs_min ;;
	package) ensure_sshfs_package ;;
	source) ensure_sshfs_source ;;
	*)
		# parse_args already refuses unknown modes; this is belt and braces.
		refuse "unknown --sshfs mode \"$SSHFS_MODE\""
		;;
	esac
}

# ── main ────────────────────────────────────────────────────────────────────
parse_args "$@"

OS=$(detect_os)
ARCH=$(detect_arch)
require_supported_platform

trap 'cleanup' EXIT INT TERM HUP

case "$MODE" in
release)
	say "installing bunker $([ -n "$VERSION" ] && printf '%s' "$VERSION" || printf 'latest') for $OS/$ARCH"
	;;
dir)
	say "installing bunker from $FROM_DIR for $OS/$ARCH"
	;;
build)
	say "installing bunker built from source for $OS/$ARCH"
	;;
esac

REPO_DIR=
if [ "$MODE" = build ]; then
	require_go
	REPO_DIR=$(resolve_repo_root)
fi

choose_dest
say "target directory: $DEST"

case "$MODE" in
release) from_release ;;
dir) from_dir ;;
build) build_from_checkout ;;
esac

install_binaries

if [ "$DRY_RUN" = 0 ]; then
	smoke_check

	case ":${PATH:-}:" in
	*":$DEST:"*) ;;
	*) warn "$DEST is not on your PATH — add it, e.g. export PATH=\"$DEST:\$PATH\"" ;;
	esac

	say "done: bunker and bunkerd are installed in $DEST"
else
	say "dry-run complete: nothing was downloaded, built or written"
fi

# ── sshfs (MOUNT-009) ───────────────────────────────────────────────────────
# Runs AFTER the bunker install and its reporting. Empty SSHFS_MODE (the
# default, no --sshfs flag) is a no-op: the default install NEVER touches the
# host's sshfs.
ensure_sshfs
