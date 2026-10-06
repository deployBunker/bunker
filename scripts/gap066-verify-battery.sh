#!/bin/bash
# GAP-066 live verification battery — docker-as-installer program aliases.
#
# Runs ON the bunker host (bunker-mvp) as root, driving the INSTALLED CLI
# against the RUNNING daemon. Every cell below is a real observation, not a
# restatement of the design:
#
#   A  preflight   — daemon reachable, alias RPC served, agent present
#   B  CRUD        — register / list / delete round-trip, unknown-name refusal
#   C  AC4 mounts  — a host path outside the agent-home root is refused at
#                    REGISTRATION; another agent's home is refused AT EXEC
#   D  AC2/AC3     — `bunker run <agent> -- yq --version` == the native
#                    invocation (stdout + exit code), the first run pulls with
#                    the latency logged, the second is served from the cache
#   E  AC5 parity  — a script INSIDE the agent that calls `yq` produces the same
#                    stdout and exit code as the native invocation, on both the
#                    success and the failure exit code
#   F  entrypoint  — `--entrypoint` is real: an explicit command vector produces
#                    the same stdout/exit code as the image's own entrypoint
#   G  socket      — the alias container sees no docker socket and no agent
#                    runtime directory
#
# Exit status: 0 with a final "VERIFY-PASS" line, 1 with "VERIFY-FAIL".
#
# The "native invocation" reference throughout is the program executed directly
# in its own image (`docker run --rm <image> [--version]`) on the agent host —
# i.e. without any of the alias machinery. The alias must be indistinguishable
# from it.
#
# Inputs (optional, explicit values win):
#   BUNKER_BIN      CLI under test                  (default /usr/local/bin/bunker)
#   BUNKER_SERVER   server alias in the CLI config  (default mvp-live)
#   BUNKER_AGENT    agent under test                (default gap066-live)
#   BUNKER_ALIAS_IMAGE / BUNKER_ALIAS_PROGRAM
#   BUNKER_PROBE_IMAGE                              (default alpine:3.20)
set -uo pipefail

BUNKER_BIN=${BUNKER_BIN:-/usr/local/bin/bunker}
BUNKER_SERVER=${BUNKER_SERVER:-mvp-live}
BUNKER_AGENT=${BUNKER_AGENT:-gap066-live}
ALIAS_IMAGE=${BUNKER_ALIAS_IMAGE:-mikefarah/yq:4}
ALIAS_PROGRAM=${BUNKER_ALIAS_PROGRAM:-yq}
PROBE_IMAGE=${BUNKER_PROBE_IMAGE:-alpine:3.20}

export BUNKER_SESSION_TARGET="$BUNKER_SERVER"

PASS=0
FAIL=0
SCRATCH=$(mktemp -d /tmp/gap066-battery.XXXXXX)

say()  { printf '%s\n' "$*"; }
cell() { printf '\n── %s\n' "$*"; }
ok()   { PASS=$((PASS + 1)); printf '   PASS  %s\n' "$*"; }
bad()  { FAIL=$((FAIL + 1)); printf '   FAIL  %s\n' "$*"; }
note() { printf '   NOTE  %s\n' "$*"; }

# check <description> <expected> <actual>
check() {
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want [$2], got [$3])"; fi
}

alias_delete() { "$BUNKER_BIN" alias delete "$1" >/dev/null 2>&1; }

cleanup() {
  alias_delete yq
  alias_delete badmnt
  alias_delete otraspace
  alias_delete socker
  alias_delete yqsh
}

# daemon_log_since_start prints every bunkerd journal line since the CURRENT
# daemon process started (the last "REST listening" banner), which is the window
# over which the in-process image cache is meaningful.
daemon_log_since_start() {
  journalctl -u bunkerd --no-pager 2>/dev/null | awk '
    /bunkerd REST listening/ { buf = "" }
    { buf = buf "\n" $0 }
    END { print buf }'
}

alias_lines_for() { # <alias name and image> <stream>
  grep '"msg":"program alias exec"' "$1" | grep -F "\"alias\":\"$2\"" | grep -F "\"image\":\"$3\""
}

say "GAP-066 live battery — docker-as-installer program aliases"
say "  cli     : $BUNKER_BIN"
say "  server  : $BUNKER_SERVER"
say "  agent   : $BUNKER_AGENT"
say "  program : $ALIAS_PROGRAM from $ALIAS_IMAGE"
say "  scratch : $SCRATCH"

# ── A. preflight ────────────────────────────────────────────────
cell "A  preflight"
cleanup
if "$BUNKER_BIN" alias list >"$SCRATCH/list0" 2>&1; then
  ok "alias RPC served (bunker alias list rc=0)"
else
  bad "alias RPC unreachable: $(cat "$SCRATCH/list0")"
fi
if "$BUNKER_BIN" exec "$BUNKER_AGENT" -- true >/dev/null 2>&1; then
  ok "agent $BUNKER_AGENT reachable (bunker exec rc=0)"
else
  bad "agent $BUNKER_AGENT not reachable"
fi
# Precondition for the pull arm: is the image already on the agent, and has this
# daemon process already confirmed it?  Both are recorded, not assumed. The
# local image is dropped first so a cold-cache daemon always exercises the
# first-run pull; when the daemon has already confirmed the image, the cached
# arm is asserted instead and the transcript says so.
"$BUNKER_BIN" exec "$BUNKER_AGENT" -- docker rmi -f "$ALIAS_IMAGE" >/dev/null 2>&1 || true
if "$BUNKER_BIN" exec "$BUNKER_AGENT" -- docker image inspect "$ALIAS_IMAGE" >/dev/null 2>&1; then
  IMAGE_PRESENT=1
else
  IMAGE_PRESENT=0
fi
daemon_log_since_start >"$SCRATCH/daemonlog"
if grep -q "\"image\":\"$ALIAS_IMAGE\".*\"pull\":true" "$SCRATCH/daemonlog"; then
  PRIOR_PULL=1
else
  PRIOR_PULL=0
fi
say "  image present on agent : $IMAGE_PRESENT"
say "  daemon already pulled  : $PRIOR_PULL"
if [ "$IMAGE_PRESENT" = 0 ] && [ "$PRIOR_PULL" = 0 ]; then
  note "clean image state — the strict first-run-pull arm will be asserted"
else
  note "image already available — the cached-path arm will be asserted instead"
fi

# ── B. CRUD ─────────────────────────────────────────────────────
cell "B  alias CRUD (AC1 surface)"
if "$BUNKER_BIN" alias set yq --image "$ALIAS_IMAGE" >"$SCRATCH/set" 2>&1; then
  ok "alias set: $(cat "$SCRATCH/set")"
else
  bad "alias set failed: $(cat "$SCRATCH/set")"
fi
"$BUNKER_BIN" alias list >"$SCRATCH/list1" 2>&1
if grep -q "^yq	" "$SCRATCH/list1"; then
  ok "alias list shows yq: $(grep '^yq	' "$SCRATCH/list1")"
else
  bad "alias list does not show yq: $(cat "$SCRATCH/list1")"
fi
if "$BUNKER_BIN" alias delete yq >"$SCRATCH/del" 2>&1 && ! "$BUNKER_BIN" alias list | grep -q "^yq	"; then
  ok "alias delete removes the alias: $(cat "$SCRATCH/del")"
else
  bad "alias delete did not remove yq"
fi
if "$BUNKER_BIN" alias delete yq >/dev/null 2>&1; then
  bad "deleting an unknown alias succeeded (want a not-found refusal)"
else
  ok "deleting an unknown alias is refused (not a silent success)"
fi
# Malformed name / image are refused before any RPC.
if "$BUNKER_BIN" alias set 'bad name' --image "$ALIAS_IMAGE" >/dev/null 2>&1; then
  bad "a malformed alias name was accepted"
else
  ok "a malformed alias name is refused"
fi
if "$BUNKER_BIN" alias set evil --image 'yq; rm -rf /' >/dev/null 2>&1; then
  bad "a shell-metacharacter image reference was accepted"
else
  ok "a shell-metacharacter image reference is refused"
fi

# ── C. AC4: the HOME-ONLY mount rule, both arms ─────────────────
cell "C  AC4 home-only mounts"
"$BUNKER_BIN" alias set yq --image "$ALIAS_IMAGE" >/dev/null 2>&1
"$BUNKER_BIN" alias set badmnt --image "$ALIAS_IMAGE" --mount /etc >"$SCRATCH/mnt1" 2>&1
check "a host path outside the agent-home root is refused AT REGISTRATION" "1" "$?"
if grep -qi "must be inside an agent home" "$SCRATCH/mnt1"; then
  ok "the refusal names the rule: $(tr -d '\n' < "$SCRATCH/mnt1" | tail -c 130)"
else
  bad "the refusal does not name the home rule: $(cat "$SCRATCH/mnt1")"
fi

# Second arm: a mount inside ANOTHER agent's home is a valid agent-home path, so
# it registers — and must then be refused for THIS agent when the alias runs.
OTHER_HOME="/home/bunker-somebodyelse/data"
if "$BUNKER_BIN" alias set otraspace --image "$ALIAS_IMAGE" --mount "$OTHER_HOME" >"$SCRATCH/mnt2" 2>&1; then
  ok "a mount inside another agent's home is storable (shape-valid)"
  "$BUNKER_BIN" exec "$BUNKER_AGENT" -- otraspace --version >"$SCRATCH/mnt2run" 2>&1
  rc=$?
  if [ "$rc" -ne 0 ] && grep -qi "must be inside the agent's home" "$SCRATCH/mnt2run"; then
    ok "the same mount is refused when the alias runs for this agent (rc=$rc)"
  else
    bad "a mount for another agent was not refused at exec (rc=$rc): $(cat "$SCRATCH/mnt2run")"
  fi
else
  bad "storing a shape-valid agent-home mount failed: $(cat "$SCRATCH/mnt2")"
fi

# ── D. AC2 + AC3: run through the alias, pull once, then cache ──
cell "D  AC2/AC3 bunker run <agent> -- $ALIAS_PROGRAM --version"

# First alias run in this battery. It must come BEFORE the native reference,
# because a `docker run` of a missing image auto-pulls it — running the
# reference first would populate the local image store behind the aliases's
# back and hide the first-run pull.
"$BUNKER_BIN" run "$BUNKER_AGENT" -- "$ALIAS_PROGRAM" --version >"$SCRATCH/a1.out" 2>"$SCRATCH/a1.err"
A1_RC=$?
check "alias run (first) exits 0" "0" "$A1_RC"
if grep -Eq "^${ALIAS_PROGRAM}[ :].*version" "$SCRATCH/a1.out"; then
  ok "alias run stdout: $(tr -d '\n' < "$SCRATCH/a1.out")"
else
  bad "alias run stdout is not a version line: [$(cat "$SCRATCH/a1.out")] (err: $(cat "$SCRATCH/a1.err"))"
fi

sleep 1
daemon_log_since_start >"$SCRATCH/after1"
alias_lines_for "$SCRATCH/after1" yq "$ALIAS_IMAGE" >"$SCRATCH/yqlines"
say "   daemon alias-run lines for yq: $(grep -c . "$SCRATCH/yqlines")"
say "   first line      : $(head -1 "$SCRATCH/yqlines" | cut -c1-260)"
if [ "$IMAGE_PRESENT" = 0 ] && [ "$PRIOR_PULL" = 0 ]; then
  if grep -q '"pull":true' "$SCRATCH/yqlines"; then
    ok "first run PULLED the image"
  else
    bad "first run did not report a pull"
  fi
  if grep -q '"first_run_pull_ms"' "$SCRATCH/yqlines"; then
    ok "the first-run pull LATENCY is logged: $(grep -o '"first_run_pull_ms":[0-9]*' "$SCRATCH/yqlines" | head -1)"
  else
    bad "the first-run pull latency is not logged"
  fi
else
  if grep -q '"pull":false' "$SCRATCH/yqlines"; then
    ok "the image was already available to this daemon, so the first run correctly reported pull=false (cached path)"
  else
    bad "expected a cached (pull=false) first run, got: $(cat "$SCRATCH/yqlines")"
  fi
fi

# Second run: exactly one more alias-run line, and it must be the cached path.
BEFORE=$(grep -c . "$SCRATCH/yqlines")
"$BUNKER_BIN" run "$BUNKER_AGENT" -- "$ALIAS_PROGRAM" --version >"$SCRATCH/a2.out" 2>&1
A2_RC=$?
check "alias run (second) exits 0" "0" "$A2_RC"
sleep 1
daemon_log_since_start >"$SCRATCH/after2"
alias_lines_for "$SCRATCH/after2" yq "$ALIAS_IMAGE" >"$SCRATCH/yqlines2"
AFTER=$(grep -c . "$SCRATCH/yqlines2")
check "the second run added exactly one alias-run line" "$((BEFORE + 1))" "$AFTER"
if tail -1 "$SCRATCH/yqlines2" | grep -q '"pull":false'; then
  ok "the second run is served from the cache (pull=false): $(tail -1 "$SCRATCH/yqlines2" | cut -c1-260)"
else
  bad "the second run did not report pull=false: $(tail -1 "$SCRATCH/yqlines2")"
fi

# Native reference: the same program invoked directly in its own image (the
# image's own ENTRYPOINT is the program, so no extra command vector is needed).
"$BUNKER_BIN" exec "$BUNKER_AGENT" -- docker run --rm "$ALIAS_IMAGE" --version \
  >"$SCRATCH/ref.out" 2>"$SCRATCH/ref.err"
REF_RC=$?
check "native reference exits 0" "0" "$REF_RC"
if [ "$(cat "$SCRATCH/a1.out")" = "$(cat "$SCRATCH/ref.out")" ]; then
  ok "alias stdout is byte-identical to the native invocation"
else
  bad "alias stdout differs: [$(cat "$SCRATCH/a1.out")] vs native [$(cat "$SCRATCH/ref.out")] (err: $(cat "$SCRATCH/ref.err"))"
fi
if [ "$(cat "$SCRATCH/a2.out")" = "$(cat "$SCRATCH/ref.out")" ]; then
  ok "the cached run's stdout is also byte-identical to native"
else
  bad "the cached run's stdout differs: [$(cat "$SCRATCH/a2.out")]"
fi

# ── E. AC5: script parity (stdout AND exit code) ────────────────
cell "E  AC5 exec parity: a script inside the agent"
cat >"$SCRATCH/ok.sh" <<EOF
#!/bin/sh
$ALIAS_PROGRAM --version
EOF
"$BUNKER_BIN" exec --script "$SCRATCH/ok.sh" "$BUNKER_AGENT" >"$SCRATCH/script.out" 2>"$SCRATCH/script.err"
SCRIPT_RC=$?
say "   script rc=$SCRIPT_RC out=[$(tr -d '\n' < "$SCRATCH/script.out")]"
check "script exit code == native exit code" "$REF_RC" "$SCRIPT_RC"
if [ "$(cat "$SCRATCH/script.out")" = "$(cat "$SCRATCH/ref.out")" ]; then
  ok "script stdout is byte-identical to the NATIVE invocation"
else
  bad "script stdout differs from native: [$(cat "$SCRATCH/script.out")] vs [$(cat "$SCRATCH/ref.out")] (err: $(cat "$SCRATCH/script.err"))"
fi

# Failure arm: a bad flag must produce the same non-zero exit code both ways.
cat >"$SCRATCH/bad.sh" <<EOF
#!/bin/sh
$ALIAS_PROGRAM --this-flag-does-not-exist
EOF
"$BUNKER_BIN" exec "$BUNKER_AGENT" -- docker run --rm "$ALIAS_IMAGE" --this-flag-does-not-exist >/dev/null 2>&1
REF_BAD_RC=$?
"$BUNKER_BIN" exec --script "$SCRATCH/bad.sh" "$BUNKER_AGENT" >/dev/null 2>>"$SCRATCH/bad.err"
BAD_RC=$?
say "   bad-flag rc: native=$REF_BAD_RC script=$BAD_RC"
if [ "$REF_BAD_RC" -ne 0 ] && [ "$BAD_RC" -eq "$REF_BAD_RC" ]; then
  ok "a failing program propagates the SAME non-zero exit code through the alias ($BAD_RC)"
else
  bad "exit-code parity failed on the failing path (native=$REF_BAD_RC script=$BAD_RC)"
fi

# The shim is what makes the script work; prove it exists and is the daemon's.
"$BUNKER_BIN" exec "$BUNKER_AGENT" -- sh -c 'stat -c "%a %n" "$HOME/bin/'"$ALIAS_PROGRAM"'" ; head -2 "$HOME/bin/'"$ALIAS_PROGRAM"'"' \
  >"$SCRATCH/shim" 2>&1
say "   shim: $(tr '\n' '|' < "$SCRATCH/shim")"
if grep -q "7[0-9][0-9] .*bin/$ALIAS_PROGRAM" "$SCRATCH/shim" && grep -q '# rev=' "$SCRATCH/shim"; then
  ok "the alias is installed as an executable ~/bin shim with a revision marker"
else
  bad "the ~/bin shim is missing or malformed: $(cat "$SCRATCH/shim")"
fi

# ── F. --entrypoint is real ─────────────────────────────────────
cell "F  --entrypoint produces the same result as the image's own entrypoint"
"$BUNKER_BIN" alias set yqsh --image "$ALIAS_IMAGE" --entrypoint sh >"$SCRATCH/entset" 2>&1
if "$BUNKER_BIN" run "$BUNKER_AGENT" -- yqsh -c "$ALIAS_PROGRAM --version" >"$SCRATCH/ent.out" 2>"$SCRATCH/ent.err"; then
  if [ "$(cat "$SCRATCH/ent.out")" = "$(cat "$SCRATCH/ref.out")" ]; then
    ok "an explicit --entrypoint sh -c '<program> --version' matches the native stdout"
  else
    bad "explicit-entrypoint stdout differs: [$(cat "$SCRATCH/ent.out")] vs [$(cat "$SCRATCH/ref.out")]"
  fi
else
  bad "explicit-entrypoint run failed: $(cat "$SCRATCH/ent.err")"
fi

# ── G. no docker socket inside the alias container ──────────────
cell "G  the alias container gets no docker socket"
if "$BUNKER_BIN" alias set socker --image "$PROBE_IMAGE" --entrypoint sh >"$SCRATCH/probeset" 2>&1; then
  "$BUNKER_BIN" exec "$BUNKER_AGENT" -- socker -c \
    'for p in /var/run/docker.sock /run/bunker /run/bunker/'"$BUNKER_AGENT"'/docker.sock; do if [ -e "$p" ]; then echo "ABSENT-FAIL $p"; else echo "absent $p"; fi; done; \
     if [ -d /home/bunker-'"$BUNKER_AGENT"' ]; then echo "home-visible"; else echo "HOME-MISSING-FAIL"; fi' \
    >"$SCRATCH/sock" 2>&1
  sed 's/^/   /' "$SCRATCH/sock"
  if grep -q -- "-FAIL" "$SCRATCH/sock"; then
    bad "the alias container can see a docker socket / runtime dir, or lost the home: $(cat "$SCRATCH/sock")"
  else
    ok "no docker socket and no agent runtime directory inside the alias container"
  fi
  if grep -q '^home-visible' "$SCRATCH/sock"; then
    ok "the agent home IS mounted (that is the design: the program works in the agent's own tree)"
  else
    bad "the agent home is not visible in the alias container: $(cat "$SCRATCH/sock")"
  fi
  # Control: the same probe run natively can see the runtime dir, so the
  # absences above are the alias configuration and not a broken probe.
  "$BUNKER_BIN" exec "$BUNKER_AGENT" -- sh -c 'test -d /run/bunker/'"$BUNKER_AGENT"' && echo RUNTIME-VISIBLE-NATIVELY' >"$SCRATCH/ctrl" 2>&1
  if grep -q 'RUNTIME-VISIBLE-NATIVELY' "$SCRATCH/ctrl"; then
    ok "control: the runtime dir IS visible natively (so its absence in the container is real)"
  else
    bad "control probe failed — the absences above are not evidence: $(cat "$SCRATCH/ctrl")"
  fi
else
  bad "could not register the probe alias: $(cat "$SCRATCH/probeset")"
fi

# ── cleanup ─────────────────────────────────────────────────────
cell "cleanup"
cleanup
LEFT=$("$BUNKER_BIN" alias list 2>&1 | grep -c "^yq	\|^badmnt	\|^otraspace	\|^socker	\|^yqsh	")
check "test aliases removed" "0" "$LEFT"

say ""
say "════════════════════════════════════════"
say "  cells passed: $PASS"
say "  cells failed: $FAIL"
if [ "$FAIL" -eq 0 ]; then
  say "  VERIFY-PASS"
  exit 0
fi
say "  VERIFY-FAIL"
exit 1
