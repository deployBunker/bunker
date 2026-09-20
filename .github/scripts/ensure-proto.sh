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

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

STAMP_FILE="proto/.codegen-fresh"
MAX_ATTEMPTS="${PROTO_SKIP_MAX_ATTEMPTS:-5}"
BACKOFF_BASE="${PROTO_SKIP_BACKOFF_BASE:-3}"

# ── 1. Freshness check: is `buf generate` needed at all? ──
# Root-anchored input set: tracked *.proto plus the buf config at any level.
# Cache workflows restore proto/bunker/v1 generated files keyed on the same
# input hash (hashFiles in ci.yml); this stamp is the offline twin of that.
RAW_INPUTS="$(git -c safe.directory='*' ls-files -- '*.proto' '*.yaml' || true)"

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
