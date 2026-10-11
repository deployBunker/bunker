#!/usr/bin/env bash
# INTEG-002 safety-semantics probe over the bunker exec path.
# Extends the GAP-092 verb-contract proof (tools/gap092-verbs.sh) with the
# safety machinery: strict-patch refusal, atomic apply rollback, ambiguous
# replace refusal, single-grant lease, and a stdin edit-set carrying quotes,
# newlines and a NUL byte.
#
# Run IDENTICALLY on the control host and inside a bunker agent, with ROOT
# pointing at a fresh probe directory and TOOLSD at the binary under test.
# Every mutating verb runs inside ROOT (the script cd's there first); the
# only other writes are the transcript redirect itself. Nothing outside
# ROOT is touched; nothing is deleted.
#
# Prints raw command/stdout/stderr/exit-code triples. Nothing is paraphrased.
# Exit status is 0 when the probe ran to completion: refusals are EXPECTED
# outcomes and do not fail the run; the verdict lives in the transcript.
set -u
ROOT=${ROOT:?set ROOT to the probe directory}
TOOLSD=${TOOLSD:-toolsd}
cd "$ROOT" || exit 9

sec() { printf '\n===== %s =====\n' "$1"; }
run() { # run <label> <cmd...> -> prints the command, stdout+stderr, rc
  local label=$1; shift
  printf -- '--- %s\n' "$label"
  printf '$ %s\n' "$*"
  local out rc
  out=$("$@" 2>&1); rc=$?
  printf '%s\n' "$out"
  printf 'rc=%d\n' "$rc"
}
run2() { # run2 <label> <cmd...> -> split stdout/stderr, for refusal evidence
  local label=$1; shift
  printf -- '--- %s\n' "$label"
  printf '$ %s\n' "$*"
  local so se rc
  so=$("$@" 2>/tmp/integ002-stderr.$$.out); rc=$?
  se=$(cat /tmp/integ002-stderr.$$.out)
  printf -- '--- stdout ---\n%s\n--- stderr ---\n%s\nrc=%d\n' "$so" "$se" "$rc"
}
sha() { printf '$ sha256sum %s\n' "$1"; sha256sum "$1" 2>&1; }

sec "ENVIRONMENT"
printf 'whoami=%s\n' "$(id -un 2>&1)"
printf 'uname=%s\n' "$(uname -m 2>&1)"
printf 'toolsd_path=%s\n' "$TOOLSD"
sha "$TOOLSD"
run "toolsd version" "$TOOLSD" version
for b in git rg jq python3; do
  if command -v "$b" >/dev/null 2>&1; then printf 'tool %s: PRESENT (%s)\n' "$b" "$(command -v "$b")"
  else printf 'tool %s: ABSENT\n' "$b"; fi
done

sec "FIXTURE (deterministic; seeded in place, no transfer needed)"
# lines.txt: 120 numbered lines, needles at 37 and 88
i=1; while [ "$i" -le 120 ]; do
  if [ "$i" -eq 37 ] || [ "$i" -eq 88 ]; then printf 'needle line %d\n' "$i" >> lines.txt
  else printf 'filler line %d\n' "$i" >> lines.txt; fi
  i=$((i+1))
done
printf 'one\ntwo\nthree\n' > patchme.txt
printf 'alpha\nbeta\ngamma\n' > editme.txt
printf 'AAAA\n' > a1.txt
mkdir -p d2
: > big5.txt
i=0; while [ "$i" -lt 100 ]; do printf '0123456789ABCDEF' >> big5.txt; i=$((i+1)); done
printf '\n' >> big5.txt
printf '$ wc -c big5.txt lines.txt\n'; wc -c big5.txt lines.txt

sec "PRE-HASHES (sha256 before any mutation)"
for f in patchme.txt editme.txt a1.txt big5.txt lines.txt; do sha "$f"; done

sec "S1 PATCH: inexact diff must REFUSE with a named reason, target byte-identical"
cat > bad.diff <<'EOF'
--- a/patchme.txt
+++ b/patchme.txt
@@ -1,3 +1,3 @@
 NOT-PRESENT
-two
+TWO
 three
EOF
run2 "toolsd patch bad.diff (expect refusal + rollback, rc!=0)" "$TOOLSD" patch bad.diff
sha patchme.txt

sec "S2 APPLY: multi-file edit-set, failure AFTER the first write, must roll back"
# positive control: same shape, all-regular-file targets -> applies
printf '[{"path":"a1.txt","content":"replaced content\\n"},{"path":"big5.txt","content":"' > es-control.json
tr -d '\n' < big5.txt >> es-control.json
printf '\\n"}]' >> es-control.json
run "toolsd apply es-control.json (positive control, expect rc=0)" "$TOOLSD" apply es-control.json
sha a1.txt; sha big5.txt
# the subject: 2nd entry targets a DIRECTORY -> commit fails after entry 1 is staged
printf '[{"path":"a1.txt","content":"rolled back content\\n"},{"path":"d2","content":"this target is a directory\\n"}]' > es-refuse.json
run2 "toolsd apply es-refuse.json (expect commit failure naming d2 + rollback, rc!=0)" "$TOOLSD" apply es-refuse.json
sha a1.txt; sha big5.txt
run "ls d2 (still an empty directory, nothing written into it)" ls d2

sec "S3 REPLACE: ambiguous needle must refuse naming the match count"
run2 "toolsd replace editme.txt beta BETA (positive control)" "$TOOLSD" replace editme.txt beta BETA
run "cat editme.txt" cat editme.txt
# after beta->BETA the letter 'a' occurs in BOTH alpha and gamma -> ambiguous
run2 "toolsd replace editme.txt a X (expect named ambiguous refusal, rc!=0)" "$TOOLSD" replace editme.txt a X
run2 "toolsd replace editme.txt zzzz X (expect no-match refusal, rc!=0)" "$TOOLSD" replace editme.txt zzzz X
sha editme.txt

sec "S4 LEASE: two acquires on one path -> exactly one grant, one refusal naming the holder"
git init -q .
run "git rev-parse --is-inside-work-tree" git rev-parse --is-inside-work-tree
run2 "lease acquire holder-A" "$TOOLSD" lease acquire --holder holder-A --ttl 45m --note INTEG-002 lines.txt
run2 "lease acquire holder-B (expect refusal naming holder-A, rc!=0)" "$TOOLSD" lease acquire --holder holder-B --ttl 45m lines.txt
run "lease status" "$TOOLSD" lease status
run "registry cat .git/agent-leases.json" cat .git/agent-leases.json
sha .git/agent-leases.json
run "lease release holder-A" "$TOOLSD" lease release --holder holder-A
run "registry after release" cat .git/agent-leases.json

sec "S5 STDIN edit-set: quotes, newlines and a NUL byte must apply or refuse BY NAME"
# arm A: NUL carried as the JSON escape \u0000 -> must APPLY with a real NUL byte
printf '%s' '[{"path":"nul.txt","content":"line one with \u0000 NUL inside\nline two has \"quotes\" and \\ backslash\n"}]' > editset-nul.json
printf '$ wc -c editset-nul.json\n'; wc -c editset-nul.json
run2 "toolsd apply - < editset-nul.json (expect rc=0 and a real NUL byte)" sh -c "$TOOLSD apply - < $ROOT/editset-nul.json"
run "od -c nul.txt (expect a literal \\0 in the first line)" sh -c "od -c nul.txt | head -6"
sha nul.txt
run "expected-content hash (printf emits a real NUL)" sh -c "printf 'line one with \\0 NUL inside\nline two has \"quotes\" and \\\\ backslash\n' | sha256sum"
# arm B: RAW NUL byte in the JSON source -> parse refusal, nothing written
{
printf '[{"path":"nul_bad.txt","content":"prefix "'
printf '\0'
printf '" suffix and text after the NUL\\n"}]'
} > editset-rawnul.json
printf '$ od -An -tx1 editset-rawnul.json (raw NUL = byte 0x00 mid-string)\n'
od -An -tx1 editset-rawnul.json | head -4
run2 "toolsd apply editset-rawnul.json (expect parse refusal naming NUL, rc!=0)" "$TOOLSD" apply editset-rawnul.json
run "ls nul_bad.txt (expect: not found - nothing written)" sh -c "ls nul_bad.txt 2>&1"

sec "POST-HASHES (all mutation targets, final state)"
for f in patchme.txt editme.txt a1.txt big5.txt lines.txt nul.txt; do sha "$f"; done

sec "PROBE COMPLETE"
exit 0
