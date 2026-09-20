#!/usr/bin/env bash
# Hermetic regression test for regression-tests.sh's operator-config helpers
# (INT-CI-025).
#
# Why: the live suite's cleanup diffs the OPERATOR's CLI config server names
# before/after the run and removes ONLY its own entry if the throwaway
# BUNKER_HOME pin ever breaks. That removal is a surgical YAML edit — it must
# never touch a pre-existing entry and must never rewrite the file. This test
# exercises those two helpers directly, with no root, no daemon, no Linux
# users and no host state: everything happens under one mktemp dir.
#
# Run: bash scripts/test-regression-cleanup-helpers.sh
# Prints server NAMES only — never token values.

set -uo pipefail

SUITE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/regression-tests.sh"
[ -f "$SUITE" ] || { echo "FAIL: suite not found at $SUITE"; exit 1; }

HELPERS="$(mktemp /tmp/regression-helpers-XXXXXX.sh)"
# shellcheck disable=SC2064
trap "rm -f '$HELPERS'" EXIT
sed -n '/^config_server_names() {/,/^}/p;/^remove_server_entry() {/,/^}/p' "$SUITE" > "$HELPERS"
if ! grep -q '^config_server_names() {' "$HELPERS" || ! grep -q '^remove_server_entry() {' "$HELPERS"; then
    echo "FAIL: could not extract both helpers from $SUITE"
    exit 1
fi
# shellcheck source=/dev/null
source "$HELPERS"

T="$(mktemp -d /tmp/regression-helpers-cfg-XXXXXX)"
RC=0
check() { if [ "$1" = "$2" ]; then echo "  PASS $3"; else echo "  FAIL $3 (got: $1 / want: $2)"; RC=1; fi; }
check_sorted() {
    if [ "$(echo "$1" | tr ',' '\n' | sed '/^$/d' | sort)" = "$(echo "$2" | tr ',' '\n' | sed '/^$/d' | sort)" ]; then
        echo "  PASS $3"
    else
        echo "  FAIL $3 (got: $1 / want: $2)"; RC=1
    fi
}

# A realistic config in the shape SaveCLIConfig writes (yaml.v3, 4-space
# nesting) — the exact layout on this fleet's hosts.
cat > "$T/cfg.yaml" <<'YAML'
servers:
    bunker-las-01:
        name: bunker-las-01
        url: http://100.78.165.52:19090
        token: TOKENA
        tls_insecure: false
    cube-las-00:
        name: cube-las-00
        url: http://100.116.99.35:10001
        token: TOKENB
    karaHermes-mde-7840hs:
        name: karaHermes-mde-7840hs
        url: http://192.168.123.102:19090
        token: TOKENC
active_server: bunker-las-01
YAML
cp "$T/cfg.yaml" "$T/cfg.orig.yaml"

echo "1. server names from a yaml.v3 (4-space) config; entry FIELDS are not names"
check_sorted "$(config_server_names "$T/cfg.yaml" | tr '\n' ',')" \
             "bunker-las-01,cube-las-00,karaHermes-mde-7840hs," \
             "3 names, no url/token/name fields"

echo "2. same names from a 2-space human-written config"
sed 's/^    /  /' "$T/cfg.orig.yaml" > "$T/two-space.yaml"
check_sorted "$(config_server_names "$T/two-space.yaml" | tr '\n' ',')" \
             "bunker-las-01,cube-las-00,karaHermes-mde-7840hs," \
             "same 3 names at 2-space indent"

echo "3. remove_server_entry removes ONLY its entry — file restored byte-for-byte"
awk 'NR==1{print; print "    regression-local:"; print "        name: regression-local"; print "        url: http://127.0.0.1:29090"; print "        token: TESTTOKEN"; print "        tls_insecure: false"; next} {print}' \
    "$T/cfg.orig.yaml" > "$T/cfg.yaml"
check_sorted "$(config_server_names "$T/cfg.yaml" | tr '\n' ',')" \
             "bunker-las-01,cube-las-00,karaHermes-mde-7840hs,regression-local," \
             "test entry added (order-independent: the injected entry sorts first)"
remove_server_entry "$T/cfg.yaml" regression-local
if cmp -s "$T/cfg.yaml" "$T/cfg.orig.yaml"; then
    echo "  PASS byte-for-byte identical to the pre-existing file"
else
    echo "  FAIL file drifted:"; diff "$T/cfg.yaml" "$T/cfg.orig.yaml" | sed 's/TOKEN[A-C]/<redacted>/g'; RC=1
fi

echo "4. removing a name that is NOT present is a no-op"
cp "$T/cfg.orig.yaml" "$T/cfg.yaml"
rm -f "$T/absent.yaml"; cp "$T/cfg.orig.yaml" "$T/absent.yaml"
remove_server_entry "$T/absent.yaml" not-a-server
if cmp -s "$T/absent.yaml" "$T/cfg.orig.yaml"; then
    echo "  PASS no-op (pre-existing entries untouched)"
else
    echo "  FAIL absent-name removal edited the file"; RC=1
fi

echo "5. entry removed from the MIDDLE keeps both neighbours"
{
  echo "servers:"
  echo "    aaa-first:"
  echo "        name: aaa-first"
  echo "        url: http://127.0.0.1:1"
  echo "    regression-local:"
  echo "        name: regression-local"
  echo "        url: http://127.0.0.1:29090"
  echo "        token: TESTTOKEN"
  echo "    zzz-last:"
  echo "        name: zzz-last"
  echo "        url: http://127.0.0.1:2"
  echo "active_server: aaa-first"
} > "$T/mid.yaml"
remove_server_entry "$T/mid.yaml" regression-local
check "$(config_server_names "$T/mid.yaml" | tr '\n' ',')" \
      "aaa-first,zzz-last," \
      "neighbours on both sides preserved"

echo
echo "token lines still present (count only, values never printed): $(grep -c 'token:' "$T/cfg.orig.yaml")"
if [ "$RC" -eq 0 ]; then
    echo "RESULT: PASS"
else
    echo "RESULT: FAIL"
fi
exit "$RC"
