#!/usr/bin/env bash
# install-git-hooks.sh — put the repo's Tier-1 gate in front of `git commit`.
#
# WHY: the incident BFS-017 is about was a LOCAL commit refused by the
# pre-commit hook, which calls a bare `gitreins guard`. The bare guard has two
# verdicts, so the refusal read as "the tests failed" when the run had actually
# run out of time. This installer points the hook at
# scripts/gitreins-guard.sh, which runs the guard ONCE and reports
# PASS (0) / TEST-FAILURE (1) / NOT-FINISHED (3) / GUARD-ERROR (4) — all four
# still refusing the commit except PASS, so the guard is not weakened; the
# difference is that the operator is told which of the two things happened.
#
# SAFETY, in this order:
#   * it refuses to overwrite a pre-commit hook that does not look like this
#     repo's (a foreign hook is somebody's work; pass --force to replace it, and
#     even then the old hook is saved next to the new one as pre-commit.bak-<ts>)
#   * it writes through `git rev-parse --git-path hooks`, so a repo that has
#     already moved its hooks (core.hooksPath) keeps them where they are
#   * it VERIFIES the result (syntax + the gate path is actually referenced) and
#     reports failure rather than leaving a half-installed hook behind
#
# usage: bash scripts/install-git-hooks.sh [--force]
# undo:  rm "$(git rev-parse --git-path hooks)/pre-commit"

set -u

force=""
[ "${1:-}" = "--force" ] && force=1

root="$(git rev-parse --show-toplevel 2>/dev/null)" || {
	echo "install-git-hooks: not inside a git repository" >&2
	exit 2
}
cd "$root" || exit 2

gate="scripts/gitreins-guard.sh"
[ -f "$gate" ] || {
	echo "install-git-hooks: $root/$gate is missing" >&2
	exit 2
}

hooks_dir="$(git rev-parse --git-path hooks)"
mkdir -p "$hooks_dir" 2>/dev/null || true
[ -d "$hooks_dir" ] || {
	echo "install-git-hooks: $hooks_dir is not writable" >&2
	exit 2
}
target="$hooks_dir/pre-commit"

if [ -e "$target" ] && ! grep -q 'gitreins-guard.sh' "$target" 2>/dev/null; then
	if [ -z "$force" ]; then
		echo "install-git-hooks: $target already exists and is not this repo's gate." >&2
		echo "  It was NOT touched. Re-run with --force to replace it (the old hook is saved as pre-commit.bak-<ts>)." >&2
		exit 2
	fi
	cp "$target" "$target.bak-$(date -u +%Y%m%dT%H%M%SZ)"
	echo "install-git-hooks: saved the previous hook next to the new one"
fi

cat >"$target" <<'HOOK'
#!/usr/bin/env bash
# Installed by scripts/install-git-hooks.sh (BFS-017).
#
# The Tier-1 gate is scripts/gitreins-guard.sh: PASS (0) / TEST-FAILURE (1) /
# NOT-FINISHED (3) / GUARD-ERROR (4). Exit 3 means the run did not finish (a
# budget was exhausted, or a gate never ran) — the commit is still refused,
# because an unverified tree is not green, but the message says so instead of
# blaming the code. Nothing is retried here.
set -e
cd "$(git rev-parse --show-toplevel)"
STAGED=$(git diff --cached --name-only --diff-filter=ACM)
[ -z "$STAGED" ] && exit 0
exec bash scripts/gitreins-guard.sh
HOOK
chmod 0755 "$target"

# Verify, and say so only if it is true: a half-written hook that silently
# exits 0 would disable the gate for every commit (the failure mode the
# hookspath reference warns about).
ok=1
bash -n "$target" 2>/dev/null || ok=0
grep -q 'gitreins-guard.sh' "$target" || ok=0
[ -x "$target" ] || ok=0
if [ "$ok" != "1" ]; then
	echo "install-git-hooks: VERIFICATION FAILED — $target is not a working gate" >&2
	exit 3
fi

echo "install-git-hooks: installed $target"
echo "  it runs: bash $gate  (PASS 0 / TEST-FAILURE 1 / NOT-FINISHED 3 / GUARD-ERROR 4)"
echo "  check:   bash -n '$target' && grep -n gitreins-guard.sh '$target'"
echo "  undo:    rm '$target'"
