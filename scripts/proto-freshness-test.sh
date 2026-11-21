#!/usr/bin/env bash
# Exercise the real generation and local-guard paths in a disposable repository.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/proto-freshness.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/repo/.github/scripts" "$tmp/repo/scripts" "$tmp/repo/proto/bunker/v1" "$tmp/bin"
cp "$root/.github/scripts/ensure-proto.sh" "$tmp/repo/.github/scripts/"
cp "$root/scripts/gitreins-guard.sh" "$tmp/repo/scripts/"
cp "$root/Makefile" "$tmp/repo/"
cp "$root/proto/bunker/v1/bunker.proto" "$tmp/repo/proto/bunker/v1/"
cp "$root/buf.gen.yaml" "$tmp/repo/"
# The variable is deliberately expanded by the stub at invocation time.
# shellcheck disable=SC2016
printf '#!/usr/bin/env bash\nprintf "called\\n" >> "$BUF_CALLS"\n' > "$tmp/bin/buf"
chmod +x "$tmp/bin/buf"
export PATH="$tmp/bin:$PATH" BUF_CALLS="$tmp/calls"
cd "$tmp/repo"
git init -q
git config user.name 'Proto test'
git config user.email 'proto-test@example.invalid'
git add .
PROTO_STAMP_ENFORCE=0 bash .github/scripts/ensure-proto.sh > /dev/null
git add proto/.codegen-fresh
git -c core.hooksPath=/dev/null commit -qm baseline
: > "$BUF_CALLS"
bash .github/scripts/ensure-proto.sh --check > /dev/null
make proto > /dev/null
[ ! -s "$BUF_CALLS" ] || { echo 'fresh path called buf' >&2; exit 1; }
printf '\n// drift test\n' >> proto/bunker/v1/bunker.proto
# PROTO_CACHE_HIT=true is deliberate: the local gate must NOT trust CI's
# cache-hit signal, so a stale stamp is still refused when it is set.
if PROTO_CACHE_HIT=true bash scripts/gitreins-guard.sh > "$tmp/guard.log" 2>&1; then
  echo 'stale guard passed' >&2; exit 1
fi
grep -F 'TEST-FAILURE (proto freshness)' "$tmp/guard.log" > /dev/null
grep -F 'bash .github/scripts/ensure-proto.sh && git add proto/.codegen-fresh' "$tmp/guard.log" > /dev/null
[ ! -s "$BUF_CALLS" ] || { echo 'check path called buf' >&2; exit 1; }
make proto > /dev/null
[ "$(wc -l < "$BUF_CALLS")" -eq 1 ]
git diff --quiet -- proto/.codegen-fresh && { echo 'stamp was not refreshed' >&2; exit 1; }
PROTO_STAMP_ENFORCE=1 bash .github/scripts/ensure-proto.sh > /dev/null
git add proto/bunker/v1/bunker.proto
if bash .github/scripts/ensure-proto.sh --check > "$tmp/index.log" 2>&1; then
  echo 'partial staging accepted' >&2; exit 1
fi
grep -F 'staged inputs and stamp disagree' "$tmp/index.log" > /dev/null
git add proto/.codegen-fresh
bash .github/scripts/ensure-proto.sh --check > /dev/null
# A stale stamp regenerated in a dirty checkout must refuse CI, and name the fix.
printf 'stale\n' > proto/.codegen-fresh
if PROTO_STAMP_ENFORCE=1 bash .github/scripts/ensure-proto.sh > "$tmp/ci.log" 2>&1; then
  echo 'CI enforcement accepted regeneration drift' >&2; exit 1
fi
grep -F 'bash .github/scripts/ensure-proto.sh && git add proto/.codegen-fresh' "$tmp/ci.log" > /dev/null
printf 'proto freshness tests: PASS\n'
