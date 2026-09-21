#!/bin/sh
# pre-release-check.sh — fail-fast local gate for bunker releases.
#
# Local counterpart of the CI version checks ("Version authority check" and
# "Tag build check" in .github/workflows/ci.yml). Run it BEFORE pushing so a
# version mismatch fails on your machine instead of on the tag (see
# docs/release-checklist.md for the full procedure).
#
# Usage:
#   scripts/pre-release-check.sh              # tree check (pre-push)
#   scripts/pre-release-check.sh --tag v0.2.0 # tag-pointing check
#   scripts/pre-release-check.sh --tag v0.2.0 <rev>   # ...must point at <rev>
#
# Tree mode verifies:
#   1. internal/version/version.go `Version` == newest `## N.N.N` heading in
#      CHANGELOG.md == `VERSION ?=` in Makefile (or the env override of
#      VERSION, when one is set — that is what `make VERSION=...` would use).
#   2. `git describe --tags --abbrev=0` == `v<version>` — WARNING only (not a
#      failure) when the checkout has no tags (act/shallow/fork/tarball),
#      same guard as ci.yml.
#   3. The FIRST `##` heading in CHANGELOG.md is `## Unreleased` (an empty
#      Unreleased section is fine — the rename happens in the bump commit).
#
# --tag mode (design decision): verifies the TAG'S CONTENT and, separately,
# where the tag points. Content: the tag's own internal/version/version.go
# must contain `Version = "<tag-without-v>"` — exactly the grep CI's tag
# build check runs, so passing here means the tag will pass CI. The tag's
# CHANGELOG heading is deliberately NOT checked: historical tags predate the
# rename-in-same-commit discipline (v0.1.4's own CHANGELOG still reads
# 0.1.3), and CI does not check it either. Pointing: with a third argument
# (hard failure otherwise); without it, a tag behind HEAD is reported as an
# INFO with the offset — NOT a failure — because --tag is legitimately used
# to re-verify historical tags. The bump-commit workflow asserts pointing by
# passing HEAD (or nothing at all: on a fresh bump commit, tag == HEAD and
# the INFO line confirms it).
#
# Exit 0 + one-line summary on success; exit 1 with a named FAIL reason
# otherwise. POSIX sh, no bashisms.

set -u

fail() {
	printf 'FAIL: %s\n' "$1" >&2
	exit 1
}

warn() {
	printf 'WARN: %s\n' "$1" >&2
}

info() {
	printf 'INFO: %s\n' "$1"
}

REPO_ROOT=$(git rev-parse --show-toplevel 2>/dev/null) ||
	fail "not inside a git checkout (git rev-parse --show-toplevel failed)"
cd "$REPO_ROOT" || exit 1

[ -f internal/version/version.go ] ||
	fail "internal/version/version.go not found under $REPO_ROOT"
[ -f CHANGELOG.md ] || fail "CHANGELOG.md not found under $REPO_ROOT"
[ -f Makefile ] || fail "Makefile not found under $REPO_ROOT"

# --- tag mode -------------------------------------------------------------

if [ "${1:-}" = "--tag" ]; then
	[ $# -ge 2 ] || fail "--tag requires an argument: --tag v0.2.0 [<rev>]"
	tag=$2
	case $tag in
	v[0-9]*.[0-9]*.[0-9]*) ;;
	*)
		fail "tag '$tag' is not v-prefixed semver (expected vN.N.N)"
		;;
	esac
	git rev-parse -q --verify "refs/tags/$tag" >/dev/null 2>&1 ||
		fail "tag '$tag' does not exist in this checkout (fetch first: git fetch --tags)"

	expected=${tag#v}

	# Content check 1: the exact grep CI's tag build check runs (ci.yml).
	if ! git show "$tag:internal/version/version.go" 2>/dev/null |
		grep -q "Version = \"$expected\""; then
		fail "tag '$tag' tree's internal/version/version.go does not contain Version = \"$expected\" — the tag points at a pre-bump commit and CI's tag build check WILL go red (docs/release-checklist.md)"
	fi

	# Pointing check: hard-fail only when a rev was passed explicitly.
	tag_rev=$(git rev-parse -q --verify "$tag^{commit}") || exit 1
	if [ $# -ge 3 ]; then
		rev_rev=$(git rev-parse -q --verify "$3^{commit}") ||
			fail "rev '$3' does not exist"
		[ "$tag_rev" = "$rev_rev" ] ||
			fail "tag '$tag' points at $(git rev-parse --short "$tag_rev"), not at '$3' ($(git rev-parse --short "$rev_rev"))"
		printf 'OK: tag %s exists, content matches version %s, points at requested rev %s\n' \
			"$tag" "$expected" "$(git rev-parse --short "$tag_rev")"
		exit 0
	fi

	if [ "$tag_rev" = "$(git rev-parse HEAD)" ]; then
		printf 'OK: tag %s exists, content matches version %s, points at HEAD (%s)\n' \
			"$tag" "$expected" "$(git rev-parse --short HEAD)"
	else
		behind=$(git rev-list --count "$tag..HEAD")
		info "tag '$tag' points at $(git rev-parse --short "$tag_rev"), $behind commit(s) behind HEAD ($(git rev-parse --short HEAD)) — content check PASSED; offset reported only (pass '<rev>' to assert pointing)"
		printf 'OK: tag %s exists and content matches version %s\n' "$tag" "$expected"
	fi
	exit 0
fi

# --- tree mode (no args) ---------------------------------------------------

[ $# -eq 0 ] || fail "unknown argument '$1' (usage: $0 [--tag vN.N.N [<rev>]])"

# 1. Three version sources.
go_version=$(sed -n 's/.*Version = "\([0-9][0-9.]*\)".*/\1/p' internal/version/version.go | head -1)
[ -n "$go_version" ] ||
	fail "could not extract Version from internal/version/version.go (no 'Version = \"N.N.N\"' line)"

changelog_version=$(grep -m1 -E '^## [0-9]' CHANGELOG.md | awk '{print $2}')
[ -n "$changelog_version" ] ||
	fail "no '## N.N.N' release heading found in CHANGELOG.md"

if [ -n "${VERSION:-}" ]; then
	makefile_version=$VERSION
	info "VERSION env override detected — comparing against it (make uses \${VERSION})"
else
	makefile_version=$(sed -n 's/^VERSION[[:space:]]*?[[:space:]]*=[[:space:]]*//p' Makefile | head -1)
fi
[ -n "$makefile_version" ] ||
	fail "could not extract 'VERSION ?=' from Makefile"

if [ "$go_version" != "$changelog_version" ] ||
	[ "$go_version" != "$makefile_version" ] ||
	[ "$changelog_version" != "$makefile_version" ]; then
	fail "version sources disagree: internal/version/version.go=$go_version CHANGELOG.md=$changelog_version Makefile=$makefile_version (bump all three in ONE commit — docs/release-checklist.md)"
fi

# 2. Newest tag == v<version> (warning-only when the checkout has no tags).
if ! latest_tag=$(git describe --tags --abbrev=0 2>/dev/null); then
	warn "no tags in checkout (act/shallow/fork/tarball) — tag parity skipped, same guard as ci.yml"
	tag_status="no tags in checkout"
else
	[ "$latest_tag" = "v$go_version" ] ||
		fail "newest tag $latest_tag != v$go_version (cut order: bump commit FIRST, then tag it, push together — docs/release-checklist.md)"
	tag_status="latest tag $latest_tag"
fi

# 3. First '##' heading in CHANGELOG.md must be '## Unreleased'.
first_heading=$(grep -m1 -E '^## ' CHANGELOG.md)
[ -n "$first_heading" ] ||
	fail "no '##' headings found in CHANGELOG.md"
[ "$first_heading" = "## Unreleased" ] ||
	fail "first CHANGELOG.md heading is '$first_heading', want '## Unreleased' (after a cut, the bump commit must open a fresh empty Unreleased section)"

printf 'OK: version parity — internal/version/version.go=%s CHANGELOG.md=%s Makefile=%s; %s; first CHANGELOG heading: %s\n' \
	"$go_version" "$changelog_version" "$makefile_version" "$tag_status" "$first_heading"
exit 0
