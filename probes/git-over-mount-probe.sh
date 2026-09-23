#!/usr/bin/env bash
# git-over-mount-probe.sh — measure git operations run by the CONTROL HOST
# through an sshfs mount of a remote agent's repo.
#
# WHY: the remote-bunker loop commits and pushes from the control side through
# the mount (the agent holds no credentials). Anecdotally, whole-tree git
# operations over sshfs can block in uninterruptible D state, and a killed git
# leaves a stale zero-byte index.lock that then blocks the next commit. This
# probe turns that anecdote into numbers: per-operation wall clock, exit class,
# and whether a stall was hit.
#
# SAFETY: every mutating operation happens on a scratch branch created here and
# deleted at the end. The base branch is never touched and nothing is pushed to
# it. Read the summary before believing any of it.
#
# USAGE:
#   probes/git-over-mount-probe.sh <mount-repo-path> [--timeout SECONDS] [--csv OUT]
#
# EXIT: 0 if the probe completed (even if individual ops stalled — read the
#       summary), 2 on usage error, 3 if the path is not a git repo.

set -uo pipefail

REPO="${1:-}"
[ -n "$REPO" ] || { echo "usage: $0 <mount-repo-path> [--timeout S] [--csv OUT]" >&2; exit 2; }
shift || true

OP_TIMEOUT=120
CSV=""
while [ $# -gt 0 ]; do
  case "$1" in
    --timeout) OP_TIMEOUT="$2"; shift 2 ;;
    --csv)     CSV="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

REPO="$(cd "$REPO" 2>/dev/null && pwd)" || { echo "cannot enter $REPO" >&2; exit 2; }
git -C "$REPO" rev-parse --git-dir >/dev/null 2>&1 || { echo "not a git repo: $REPO" >&2; exit 3; }

BRANCH="bunker/git-mount-probe-$(date +%Y%m%d%H%M%S)"
BASE="$(git -C "$REPO" rev-parse --abbrev-ref HEAD 2>/dev/null)"
TRACKED="$(git -C "$REPO" ls-files 2>/dev/null | wc -l | tr -d ' ')"

TMP="$(mktemp -d)"
CSV="${CSV:-$TMP/git-over-mount.csv}"
: > "$CSV"
echo "op,elapsed_s,rc,class,tracked_files,note" >> "$CSV"

say() { printf '%s\n' "$*"; }
hr() { printf '%s\n' "────────────────────────────────────────────────────────────"; }

# classify: 0 ok, 124 timed out (D-state stall symptom), other = error
classify() {
  case "$1" in
    0)   echo "ok" ;;
    124) echo "STALL(timeout)" ;;
    128) echo "error(git)" ;;
    *)   echo "error(rc=$1)" ;;
  esac
}

# run_one NAME -- argv... : times one git operation. Mutating ops use the
# scratch branch; nothing here ever targets the base branch.
run_one() {
  local name="$1"; shift
  [ "$1" = "--" ] && shift
  local start end rc elapsed
  start=$(date +%s.%N)
  timeout "$OP_TIMEOUT" "$@" >"$TMP/$name.out" 2>&1
  rc=$?
  end=$(date +%s.%N)
  elapsed=$(python3 -c "print(f'{$end-$start:.2f}')" 2>/dev/null || echo "?")
  local cls; cls="$(classify "$rc")"
  printf '%-34s %8ss  rc=%-4s %s\n' "$name" "$elapsed" "$rc" "$cls"
  echo "$name,$elapsed,$rc,$cls,$TRACKED,\"$(head -c 120 "$TMP/$name.out" | tr '\n' ' ' | tr ',' ';')\"" >> "$CSV"
}

say "git-over-mount probe"
hr
say "repo           : $REPO"
say "tracked files  : $TRACKED"
say "base branch    : $BASE   (NEVER written to)"
say "scratch branch : $BRANCH"
say "op timeout     : ${OP_TIMEOUT}s"
say "csv            : $CSV"
hr

cleanup() {
  # A failed/interrupted rebase or merge would leave the agent's repo wedged,
  # so abort both before touching branches. This is the whole reason the probe
  # is safe to run against a live agent's tree.
  git -C "$REPO" rebase --abort   >/dev/null 2>&1 || true
  git -C "$REPO" merge  --abort   >/dev/null 2>&1 || true
  git -C "$REPO" cherry-pick --abort >/dev/null 2>&1 || true
  # Unstage the probe file BEFORE deleting it. Measured: when `git add`
  # succeeds but the following `commit` stalls (which is the common case over
  # sshfs), the index keeps a staged entry for a file that no longer exists
  # ("AD .git-mount-probe.txt"), leaving the agent's repo dirty. Unstaging
  # first makes the probe leave the tree exactly as it found it.
  git -C "$REPO" reset -q -- .git-mount-probe.txt >/dev/null 2>&1 || true
  rm -f "$REPO/.git-mount-probe.txt" 2>/dev/null || true
  git -C "$REPO" checkout -q "$BASE" 2>/dev/null || true
  git -C "$REPO" branch -D "$BRANCH" 2>/dev/null >/dev/null || true
  rm -rf "$TMP" 2>/dev/null || true
}
trap cleanup EXIT

# ── READ-ONLY operations ─────────────────────────────────────────────────────
say "READ-ONLY"
run_one "rev-parse HEAD"          -- git -C "$REPO" rev-parse HEAD
run_one "log -1"                  -- git -C "$REPO" log -1 --format=%h
run_one "log --oneline -20"       -- git -C "$REPO" log --oneline -20
run_one "ls-files (count)"        -- git -C "$REPO" ls-files
run_one "diff --stat HEAD"        -- git -C "$REPO" diff --stat HEAD
run_one "status --short (WHOLE TREE)" -- git -C "$REPO" status --short
run_one "status --porcelain -uno" -- git -C "$REPO" status --porcelain -uno
run_one "fetch origin (read-only)" -- git -C "$REPO" fetch origin
hr

# ── WRITE operations on a scratch branch ─────────────────────────────────────
say "WRITE (scratch branch only)"
run_one "checkout -b scratch"     -- git -C "$REPO" checkout -q -b "$BRANCH"
printf 'probe %s\n' "$(date -Is)" > "$REPO/.git-mount-probe.txt"
run_one "add one file"            -- git -C "$REPO" add .git-mount-probe.txt
run_one "commit"                  -- git -C "$REPO" -c user.name=probe -c user.email=probe@invalid commit -q -m "probe: git over mount"
run_one "commit --amend (no-op)"  -- git -C "$REPO" -c user.name=probe -c user.email=probe@invalid commit -q --amend --no-edit
hr

# ── REBASE + PULL over the mount ─────────────────────────────────────────────
say "REBASE / PULL"
run_one "rebase onto HEAD~1"      -- git -C "$REPO" rebase HEAD~1
run_one "pull --rebase (scratch)" -- git -C "$REPO" pull --rebase origin "$BASE"
hr

cleanup_txt() { rm -f "$REPO/.git-mount-probe.txt"; }
cleanup_txt

say "SUMMARY"
hr
python3 - "$CSV" <<'PY'
import csv, sys, collections
rows = list(csv.DictReader(open(sys.argv[1])))
classes = collections.Counter(r["class"] for r in rows)
for r in rows:
    print(f'  {r["op"]:<36} {r["elapsed_s"]:>8}s  {r["class"]}')
print()
print("  class totals:", dict(classes))
stalls = [r for r in rows if r["class"].startswith("STALL")]
if stalls:
    print(f"  STALLS ({len(stalls)}): " + ", ".join(r["op"] for r in stalls))
    print("  -> a stall means the sshfs write path blocked past the timeout;")
    print("     prefer in-agent git for read-only state and keep writes small.")
else:
    print("  no stalls at this timeout")
PY
say ""
say "nothing was pushed to the base branch; the probe file and branch are cleaned up"
cp -f "$CSV" "${BH_PROBE_CSV:-/tmp/git-over-mount-last.csv}" 2>/dev/null || true
say "csv saved to ${BH_PROBE_CSV:-/tmp/git-over-mount-last.csv}"
