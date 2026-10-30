#!/usr/bin/env bash
# ensure-proto.sh — INT-CI-024: make the CI protobuf step BSR-rate-limit-proof.
#
# Two halves:
#   1. SKIP when not needed. The generated-code inputs are hashed
#      (every tracked *.proto + the buf config *.yaml at the repo root).
#      proto/.codegen-fresh stores the key of the inputs the COMMITTED
#      generated code was generated from; the workflow additionally restores
#      an actions/cache entry keyed on the same input set (PROTO_CACHE_HIT).
#      When either signal says the committed generated files are current,
#      `buf generate` would be a no-op — skip it and never talk to the BSR.
#   2. RETRY with backoff + jitter when regeneration IS needed. BSR
#      `resource_exhausted` (rate limit) failures back off exponentially and
#      retry (5 attempts). Any OTHER buf failure, and retry exhaustion,
#      fail the job loudly.
#
# BUF_TOKEN: this repo has none configured (verified via `gh secret list`);
# buf reads BUF_TOKEN from the environment natively, so if one is ever added
# as a secret the workflow can pass it without touching this script. This
# step must not depend on it.
#
# NOTE on self-hosted runners (regression/root-suite): they run as root in an
# actions-runner-owned checkout, so every git call here carries
# `-c safe.directory='*'` (command line = protected config, never written).
#
# Env seams (TEST-ONLY overrides; CI uses the defaults):
#   PROTO_SKIP_MAX_ATTEMPTS   retry ceiling       (default 5)
#   PROTO_SKIP_BACKOFF_BASE   backoff base secs   (default 3)
#   PROTO_STAMP_ENFORCE       0 = do not fail the job when a generation it had to
#                             run leaves proto/ (code or stamp) different from the
#                             committed tree (default 1 — see step 2b)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

STAMP_FILE="proto/.codegen-fresh"
MAX_ATTEMPTS="${PROTO_SKIP_MAX_ATTEMPTS:-5}"
BACKOFF_BASE="${PROTO_SKIP_BACKOFF_BASE:-3}"

# ── 1. Freshness check: is `buf generate` needed at all? ──
# Root-anchored input set: tracked *.proto plus the buf configuration files.
#
# QA-BUNKER-25: this set MUST NOT be a bare '*.yaml' glob. That swept every
# tracked YAML path into the hash — .gitreins/config.yaml, .gitreins/tasks.yaml,
# .vfs/manifest.yaml, config.example.yaml, examples/*.yaml,
# internal/fleet/inventory.yaml, even a vendored project's
# .pre-commit-config.yaml — so ADDING OR REMOVING any YAML anywhere invalidated
# the stamp and re-armed `buf generate` for a proto tree that had not changed.
# On a JIT agent that is a BSR network dependency for a step that is a no-op:
# `buf generate` against the committed generated code produces ZERO diffs
# (verified 2026-10-22 with buf 1.72.0 + the remote plugins pinned in
# buf.gen.yaml). The stamp itself had drifted (last written 608f175b, before
# internal/fleet/inventory.yaml landed and bunker.proto changed), so every CI
# run and every fresh-system pass was regenerating; the stamp now covers only
# proto + buf config (2 files) and ENFORCE_STAMP below stops that drift.
# Cache workflows restore proto/bunker/v1 generated files keyed on the same
# input hash (hashFiles in ci.yml); this stamp is the offline twin of that.
RAW_INPUTS="$(git -c safe.directory='*' ls-files -- '*.proto' 'buf.yaml' 'buf.gen.yaml' '**/buf.yaml' '**/buf.gen.yaml' || true)"

if [ -z "${RAW_INPUTS}" ]; then
  echo "::error::no tracked proto/buf inputs found — proto step cannot verify freshness"
  exit 1
fi

INPUTS_HASH="$(printf '%s\n' "${RAW_INPUTS}" | sha256sum | awk '{print $1}')"
CACHE_KEY="proto-gen-v1-${INPUTS_HASH}"

if [ "${PROTO_CACHE_HIT:-}" = "true" ]; then
  echo "proto: actions/cache hit for inputs ${INPUTS_HASH} — generated files are current, skipping buf generate (no BSR call)"
  echo "proto: SKIPPED"
  exit 0
fi

if [ -f "${STAMP_FILE}" ] && [ "$(cat "${STAMP_FILE}")" = "${CACHE_KEY}" ]; then
  echo "proto: generated code is current for inputs ${INPUTS_HASH} — skipping buf generate (no BSR call)"
  echo "proto: SKIPPED"
  exit 0
fi

if [ -f "${STAMP_FILE}" ]; then
  echo "proto: inputs changed (stamp $(cat "${STAMP_FILE}") != ${CACHE_KEY}) — regeneration needed"
else
  echo "proto: no freshness stamp — regeneration needed"
fi

# ── 2. buf generate with retry/backoff on BSR rate limiting ──
attempt=1
last_output=""
while [ "${attempt}" -le "${MAX_ATTEMPTS}" ]; do
  echo "proto: buf generate attempt ${attempt}/${MAX_ATTEMPTS}"
  if out="$(buf generate 2>&1)"; then
    printf '%s\n' "${CACHE_KEY}" > "${STAMP_FILE}"
    echo "proto: buf generate OK — stamped ${STAMP_FILE} = ${CACHE_KEY}"
    # ── 2b. Stamp enforcement (QA-BUNKER-25) ──
    # Reaching here means the checked-out tree was STALE: either the generated
    # code or the stamp was not refreshed with the proto change. Committed
    # generated code is what the build compiles, so the drift must be fixed in
    # the commit, not papered over here — a stale stamp re-arms `buf generate`
    # (a BSR network dependency) on EVERY later run, which is exactly how a
    # fresh JIT agent came to depend on the network for a no-op step. Fail
    # loudly and name the one command that fixes it. Escape hatch:
    # PROTO_STAMP_ENFORCE=0 for a deliberate cold-cache experiment.
    if [ "${PROTO_STAMP_ENFORCE:-1}" = "1" ]; then
      drift="$(git -c safe.directory='*' status --porcelain -- proto/ 2>/dev/null || true)"
      if [ -n "${drift}" ]; then
        echo "::error::proto generation drifted from the committed tree — run 'bash .github/scripts/ensure-proto.sh' locally and commit the refreshed ${STAMP_FILE} with your proto change (PROTO_STAMP_ENFORCE=0 skips this check)"
        printf '%s\n' "${drift}"
        exit 1
      fi
    fi
    echo "proto: NEEDED-and-DONE"
    exit 0
  fi
  last_output="${out}"
  # Show the failure in the log either way.
  printf '%s\n' "${out}"
  if printf '%s' "${out}" | grep -qE 'resource_exhausted|too many requests|rate limit'; then
    if [ "${attempt}" -lt "${MAX_ATTEMPTS}" ]; then
      delay=$(( BACKOFF_BASE * (2 ** (attempt - 1)) + (RANDOM % 5) ))
      echo "::warning::BSR rate limited (resource_exhausted) — retrying in ${delay}s (attempt ${attempt}/${MAX_ATTEMPTS})"
      sleep "${delay}"
    fi
  else
    echo "::error::buf generate failed with a non-rate-limit error — failing the job (not a BSR rate limit)"
    exit 1
  fi
  attempt=$((attempt + 1))
done

echo "::error::buf generate still BSR rate-limited after ${MAX_ATTEMPTS} attempts — failing the job loudly"
echo "proto: RATE-LIMIT-RETRY-EXHAUSTED"
printf '%s\n' "${last_output}"
exit 1
