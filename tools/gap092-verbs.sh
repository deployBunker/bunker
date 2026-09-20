#!/usr/bin/env bash
# GAP-092 verb driver. Run identically on the host and inside the agent, with
# ROOT pointing at a byte-identical copy of the fixture. Prints raw
# command/output/exit-code triples; nothing is paraphrased.
set -u
ROOT=${ROOT:-$PWD}
cd "$ROOT" || exit 9
TOOLSD=${TOOLSD:-toolsd}

sec() { printf '\n===== %s =====\n' "$1"; }
run() { # run <label> <cmd...>  -> prints the command, stdout+stderr, and rc
  local label=$1; shift
  printf -- '--- %s\n' "$label"
  printf '$ %s\n' "$*"
  local out rc
  out=$("$@" 2>&1); rc=$?
  printf '%s\n' "$out"
  printf 'rc=%d\n' "$rc"
}

sec "ENVIRONMENT"
printf 'whoami=%s\n' "$(id -un)"
printf 'uname=%s\n' "$(uname -m)"
run "toolsd version" $TOOLSD version
for b in git rg jq gopls; do
  if command -v "$b" >/dev/null 2>&1; then printf 'tool %s: PRESENT (%s)\n' "$b" "$(command -v "$b")"
  else printf 'tool %s: ABSENT\n' "$b"; fi
done

sec "SURFACE (the CLI contract we would depend on)"
run "describe --json verb names" sh -c "$TOOLSD describe --json 2>/dev/null | tr ',' '\\n' | grep -o '\"name\": *\"[a-z0-9]*\"' | sed 's/.*\"name\": *\"//; s/\"//' | sort"

sec "FSOPS REACHABILITY (basic read/write/list as CLI verbs?)"
for v in read write list mkdir; do
  run "toolsd $v (expected: not a CLI verb)" $TOOLSD "$v"
done

# scratch working copy so mutating verbs start pristine
mkdir -p scratch
cp -f lines.txt editme.txt patchme.txt apply_a.txt apply_b.txt binary.bin scratch/ 2>/dev/null
cd scratch || exit 9

sec "1 READ (bounded line window)"
run "sed -n 30,40p lines.txt" sed -n '30,40p' lines.txt
run "wc -l lines.txt" wc -l lines.txt
run "sed window beyond EOF (45,200p)" sed -n '45,200p' lines.txt

sec "2 SEARCH (three output modes)"
run "content mode" rg -n 'needle' .
run "files-only mode" rg -l 'needle' .
run "count mode" rg -c 'needle' .

sec "3 WRITE (whole-file, via the transport)"
printf 'written remotely\n' > written.txt
run "cat written.txt" cat written.txt
run "sha256 written.txt" sha256sum written.txt

sec "4 EDIT (unique-match replace)"
run "replace beta -> BETA" $TOOLSD replace editme.txt beta BETA
run "cat editme.txt after" cat editme.txt
run "REFUSAL: ambiguous needle (alpha appears once, use 'a')" $TOOLSD replace editme.txt a X
run "REFUSAL: no match" $TOOLSD replace editme.txt zzzz X
run "editme.txt unchanged after refusals (sha256)" sha256sum editme.txt

sec "5 PATCH (strict unified diff)"
cat > ok.diff <<'EOF'
--- a/patchme.txt
+++ b/patchme.txt
@@ -1,3 +1,3 @@
 one
-two
+TWO
 three
EOF
run "patch ok.diff" $TOOLSD patch ok.diff
run "cat patchme.txt after" cat patchme.txt
cat > bad.diff <<'EOF'
--- a/patchme.txt
+++ b/patchme.txt
@@ -1,3 +1,3 @@
 one
-NOSUCHLINE
+whatever
 three
EOF
run "REFUSAL: inexact diff" $TOOLSD patch bad.diff
run "patchme.txt after refusal (sha256)" sha256sum patchme.txt

sec "6 APPLY (atomic multi-file)"
printf '[{"path":"apply_a.txt","content":"set-a changed\\n"},{"path":"apply_b.txt","content":"set-b changed\\n"}]' > good.json
run "apply good.json" $TOOLSD apply good.json
run "cat apply_a.txt" cat apply_a.txt
run "cat apply_b.txt" cat apply_b.txt
printf '[{"path":"apply_a.txt","content":"SHOULD-NOT-LAND\\n"},{"path":"apply_b.txt","content":"also\\n"},{"path":"apply_a.txt","content":"dup-key-dup-path\\n","bogus":"x"}]' > rollback.json
run "ROLLBACK: edit-set with an unknown key" $TOOLSD apply rollback.json
run "apply_a.txt after rollback (sha256)" sha256sum apply_a.txt

sec "7 LEASE (cross-session registry, git worktree required)"
run "git init for the lease root" git init -q .
run "lease acquire holder-A" $TOOLSD lease acquire --holder holder-A --ttl 10m --root . lines.txt
run "lease acquire holder-B on the SAME path (expect refusal naming holder-A)" $TOOLSD lease acquire --holder holder-B --ttl 10m --root . lines.txt
run "lease status" $TOOLSD lease status --root .

sec "8 BINARY round-trip (NUL + non-UTF8 must survive)"
run "sha256 binary.bin" sha256sum binary.bin
run "base64 round-trip through the transport" sh -c 'base64 binary.bin > b64.txt; base64 -d b64.txt > binary.bin.rt; sha256sum binary.bin.rt'
run "cmp binary.bin binary.bin.rt" cmp binary.bin binary.bin.rt

sec "9 LSP (needs a language server on the agent)"
run "lsp check with an explicit server (expect a named failure if absent)" $TOOLSD lsp check --root . --server gopls

sec "10 EXEC (the transport itself)"
run "sh -c echo" sh -c 'echo exec-ok; echo "stderr-line" >&2'
run "exit code propagation" sh -c 'exit 42'

sec "11 DIFF3 / NARRATE / PROBE (surface presence)"
run "diff3 with no args (usage)" $TOOLSD diff3
run "narrate with no args (usage)" $TOOLSD narrate
run "probe with no args (usage)" $TOOLSD probe

printf '\n===== END =====\n'
