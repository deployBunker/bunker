#!/bin/bash
# QA-BUNKER-REVIEW-3 — regression test: the QA harness must REFUSE a repo-dir
# that is a SUBDIRECTORY of a parent git repo (fleet parity with the
# QA-HARNESS-SCOPE-20261004 guard), yet still launch against STAND-IN dirs: a
# role lane whose .coding-hermes/board SYMLINKS into the real product repo
# identifies the product it stands in for, so qa_repo_sanity resolves that
# repo's root (with a loud NOTE) instead of refusing. Extracts the REAL
# qa_repo_sanity from bunker-qa.sh (same pattern as
# test_bunker_qa_preflight.sh) and drives it against git fixtures — no live
# agent, no network, no battery spawn:
#
#   (c0) real product git repo                  -> returned unchanged (fast path)
#   (c1) stand-in, board symlink -> product repo -> resolved to the PRODUCT root
#                                                   + NOTE naming dir, root, target
#   (c2) subdir of a foreign parent, NO product symlink -> REFUSED (named error,
#                                                   one FAIL evidence row)
#   (c3) BUNKER_QA_SUBDIR set                   -> repo-dir/$BUNKER_QA_SUBDIR wins
#                                                   over auto-resolution (no NOTE)
#   (c4) board symlink back into the SAME parent repo -> still REFUSED (c4 rule:
#                                                   no bogus resolution)
#   (c5) dir in no git repo at all (GIT_CEILING_DIRECTORIES fences off the home
#        meta-repo)                             -> refused "not a git repo"
#   (c6) board symlink into a NON-repo target   -> falls through to the refusal
#                                                   (no resolution without a
#                                                   verified product repo)
#   (w1/w2) run()/launch() wiring               -> both call qa_repo_sanity with
#                                                   their phase (grep on the
#                                                   extracted function bodies)
#   (p1/p2) `bunker-qa.sh resolve` probe entry  -> the REAL dispatch prints the
#                                                   product root (rc=0) or the
#                                                   named refusal (rc=1)
set -uo pipefail

HARNESS="${BUNKER_QA_SH:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bunker-qa.sh}"
pass=0; fail=0
check() { # <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "  ok   $1 -> $3"; pass=$((pass+1))
  else echo "  FAIL $1: expected [$2] got [$3]"; fail=$((fail+1)); fi
}
contains() { # <desc> <needle> <haystack>
  case "$3" in *"$2"*) echo "  ok   $1"; pass=$((pass+1)) ;;
  *) echo "  FAIL $1: missing [$2] in [$(printf '%s' "$3" | cut -c1-160)]"; fail=$((fail+1)); esac
}
absent() { # <desc> <needle> <haystack>
  case "$3" in *"$2"*) echo "  FAIL $1: found unwanted [$2]"; fail=$((fail+1)) ;;
  *) echo "  ok   $1"; pass=$((pass+1)) ;; esac
}

TMPD="${TMPDIR:-/tmp}/qa_bunker_review3.$$"
mkdir -p "$TMPD"
trap 'rm -rf "$TMPD"' EXIT

# Hermetic git: the box sets a global core.hooksPath that poisons scratch-repo
# git operations — never read user/system config in these fixtures.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

# ── fixtures: two REAL git repos, mirroring the live shape ──
# product/ — the "real product" repo a stand-in stands in for
# parent/  — the "foreign parent" meta-repo; the stand-ins are SUBDIRECTORIES
#            of it (live shape: stand-in/review/bunker ⊂ /home/kara home repo)
PRODUCT="$TMPD/product"; PARENT="$TMPD/parent"
mkdir -p "$PRODUCT/.coding-hermes" "$PARENT"
git -C "$PRODUCT" -c init.defaultBranch=main init -q
git -C "$PARENT"  -c init.defaultBranch=main init -q
: > "$PRODUCT/.coding-hermes/board"
PRODUCT_TOP=$(git -C "$PRODUCT" rev-parse --show-toplevel)
PARENT_TOP=$(git -C "$PARENT"  rev-parse --show-toplevel)

# c1/c3 fixture: stand-in INSIDE parent, board symlink -> product (live shape)
STANDIN="$PARENT/stand-in"
mkdir -p "$STANDIN/.coding-hermes/sub"
ln -s "$PRODUCT/.coding-hermes/board" "$STANDIN/.coding-hermes/board"
# c4 fixture: board symlink pointing back INSIDE the same parent repo
STANDIN_SAME="$PARENT/stand-in-same"
mkdir -p "$STANDIN_SAME/.coding-hermes"
: > "$PARENT/inside-board"
ln -s "$PARENT/inside-board" "$STANDIN_SAME/.coding-hermes/board"
# c2 fixture: plain subdirectory of parent, NO .coding-hermes at all
SUBDIR="$PARENT/plain-sub"
mkdir -p "$SUBDIR"
# c6 fixture: board symlink into a dir that is NOT inside any git repo
# (c5/c6 use GIT_CEILING_DIRECTORIES to fence off the home meta-repo above
# TMPD — the only real-git way to have "no repo" on a box whose $HOME is one)
NOREPO_TARGET="$TMPD/norepo-target/board"
mkdir -p "$(dirname "$NOREPO_TARGET")"
: > "$NOREPO_TARGET"
STANDIN_BAD="$PARENT/stand-in-bad"
mkdir -p "$STANDIN_BAD/.coding-hermes"
ln -s "$NOREPO_TARGET" "$STANDIN_BAD/.coding-hermes/board"

# ── extract the REAL function from the harness ──
FNS="$TMPD/fns.sh"
sed -n '/^qa_repo_sanity() {/,/^}$/p' "$HARNESS" > "$FNS"
grep -q '^qa_repo_sanity() {' "$FNS" || { echo "FAIL: could not extract qa_repo_sanity from $HARNESS"; exit 1; }
grep -q '^}$' "$FNS" || { echo "FAIL: extraction unbalanced"; exit 1; }

# drive() runs the real function against a fixture dir; stdout (the resolved
# root) and stderr (NOTE/ERROR) are captured separately, EV collects refusal rows.
EV="$TMPD/ev.jsonl"; ERRF="$TMPD/err.txt"
drive() { # <repo-dir> [VAR=val ...]
  local dir="$1"; shift
  : > "$EV"
  RESOLVED=$(env "$@" EVIDENCE="$EV" bash -c "source '$FNS'; qa_repo_sanity '$dir' probe" 2>"$ERRF"); RC=$?
  ERR_OUT=$(cat "$ERRF")
}

echo "QA-BUNKER-REVIEW-3: subdirectory guard + stand-in product-root resolution"
echo

# ── (c0) real product repo: fast path returns it unchanged ──
drive "$PRODUCT"
check "(c0) real repo rc" "0" "$RC"
check "(c0) real repo returned unchanged" "$PRODUCT_TOP" "$RESOLVED"
absent  "(c0) no NOTE on a real repo" "resolving product root" "$ERR_OUT"
check   "(c0) no evidence rows on success" "0" "$(grep -c '^{' "$EV")"

# ── (c1) stand-in with product board symlink -> resolves to the PRODUCT root ──
drive "$STANDIN"
check   "(c1) stand-in resolve rc" "0" "$RC"
check   "(c1) resolved to the product root" "$PRODUCT_TOP" "$RESOLVED"
contains "(c1) NOTE names the launch dir" "$STANDIN is a stand-in; resolving product root" "$ERR_OUT"
contains "(c1) NOTE names the resolved root" "product root $PRODUCT_TOP" "$ERR_OUT"
contains "(c1) NOTE names the symlink target" "(board symlink -> $PRODUCT/.coding-hermes/board)" "$ERR_OUT"
check   "(c1) no evidence rows on success" "0" "$(grep -c '^{' "$EV")"

# ── (c2) subdir of a foreign parent, NO product symlink -> refused ──
drive "$SUBDIR"
check "(c2) refusal rc" "1" "$RC"
contains "(c2) named subdirectory refusal" "$SUBDIR is a SUBDIRECTORY of git repo $PARENT_TOP" "$ERR_OUT"
contains "(c2) refusal names the opt-in lever" "Set BUNKER_QA_SUBDIR=<subdir-relative-to-repo>" "$ERR_OUT"
contains "(c2) refusal names the alternative" "dispatch against the real product git root" "$ERR_OUT"
check "(c2) exactly one FAIL evidence row" "1" "$(grep -c '^{' "$EV")"
contains "(c2) evidence row grades FAIL" '"status":"FAIL"' "$(cat "$EV")"
contains "(c2) evidence row carries the cause" "is a subdirectory of git repo" "$(cat "$EV")"

# ── (c3) BUNKER_QA_SUBDIR set -> explicit override wins over auto-resolution ──
drive "$STANDIN" BUNKER_QA_SUBDIR=sub
check "(c3) override rc" "0" "$RC"
check "(c3) override resolves to repo-dir/\$BUNKER_QA_SUBDIR" "$STANDIN/sub" "$RESOLVED"
absent "(c3) override suppresses stand-in auto-resolution" "resolving product root" "$ERR_OUT"
check  "(c3) no evidence rows on override" "0" "$(grep -c '^{' "$EV")"

# ── (c4) board symlink back into the SAME parent repo -> still refused ──
drive "$STANDIN_SAME"
check "(c4) same-parent symlink refusal rc" "1" "$RC"
contains "(c4) refusal is the subdirectory error" "is a SUBDIRECTORY of git repo $PARENT_TOP" "$ERR_OUT"
absent "(c4) no bogus resolution of a same-parent target" "resolving product root" "$ERR_OUT"

# ── (c5) dir in no git repo at all -> refused "not a git repo" ──
drive "$TMPD/norepo-target" GIT_CEILING_DIRECTORIES="$TMPD"
check "(c5) no-repo refusal rc" "1" "$RC"
contains "(c5) named not-a-repo refusal" "$TMPD/norepo-target is not a git repo (git resolves toplevel: none)" "$ERR_OUT"

# ── (c6) board symlink into a NON-repo -> falls through to the refusal ──
drive "$STANDIN_BAD" GIT_CEILING_DIRECTORIES="$TMPD"
check "(c6) non-repo symlink target refusal rc" "1" "$RC"
contains "(c6) falls through to the subdirectory refusal" "is a SUBDIRECTORY of git repo $PARENT_TOP" "$ERR_OUT"
absent "(c6) no resolution without a verified product repo" "resolving product root" "$ERR_OUT"

# ── (w1/w2) run()/launch() wiring: both call qa_repo_sanity with their phase ──
sed -n '/^run() {/,/^}$/p'    "$HARNESS" > "$TMPD/run-body.sh"
sed -n '/^launch() {/,/^}$/p' "$HARNESS" > "$TMPD/launch-body.sh"
grep -q '^run() {'    "$TMPD/run-body.sh"    || { echo "FAIL: could not extract run() from $HARNESS"; exit 1; }
grep -q '^launch() {' "$TMPD/launch-body.sh" || { echo "FAIL: could not extract launch() from $HARNESS"; exit 1; }
contains "(w1) run() wires qa_repo_sanity"    'repo=$(qa_repo_sanity "$repo" run)'    "$(cat "$TMPD/run-body.sh")"
contains "(w2) launch() wires qa_repo_sanity" 'repo=$(qa_repo_sanity "$repo" launch)' "$(cat "$TMPD/launch-body.sh")"

# ── (p1/p2) the REAL `resolve` probe entry through the script dispatch ──
out=$(bash "$HARNESS" resolve "$STANDIN" 2>"$TMPD/p1err.txt"); rc=$?
check   "(p1) resolve probe rc" "0" "$rc"
check   "(p1) resolve prints the product root" "$PRODUCT_TOP" "$out"
contains "(p1) resolve NOTE on stderr" "is a stand-in; resolving product root $PRODUCT_TOP" "$(cat "$TMPD/p1err.txt")"
out=$(bash "$HARNESS" resolve "$SUBDIR" 2>"$TMPD/p2err.txt"); rc=$?
check   "(p2) resolve refusal rc" "1" "$rc"
contains "(p2) resolve named refusal" "is a SUBDIRECTORY of git repo $PARENT_TOP" "$(cat "$TMPD/p2err.txt")"

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
