#!/usr/bin/env bash
# REV-BUNKER-005 / 006 / 007 — the RED and NEGATIVE-CONTROL battery.
#
# Every phase: record the blob hash -> mutate the COMMITTED revision -> run the
# cell -> restore with `git checkout HEAD --` -> prove byte-identical by BOTH
# `git diff --quiet HEAD` and the sha256. A cell that cannot go red proves
# nothing, so each fix is neutered at its own layer, and the two layers of each
# fix are neutered separately.
#
# Run from the repo root of the worktree:  bash docs/evidence/REV-BUNKER-005-006-007-controls.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT" || exit 1
OUT="$ROOT/docs/evidence/REV-BUNKER-005-006-007-controls.txt"
: > "$OUT"

sha() { sha256sum "$1" | awk '{print $1}'; }

say() { echo "$*" | tee -a "$OUT"; }

restore() { # files...
	git checkout HEAD -- "$@"
	local ok=1
	for f in "$@"; do
		if ! git diff --quiet HEAD -- "$f"; then ok=0; fi
	done
	if [ "$ok" = "1" ]; then
		say "RESTORE: git diff --quiet HEAD is CLEAN for $* -> byte-identical to the committed revision"
	else
		say "RESTORE: *** FAILED *** -- $* differs from HEAD after restore"
	fi
}

CELLS=(internal/server/server.go internal/auth/interceptor.go internal/auth/jwt.go \
	internal/auth/httpware.go internal/auth/throttle.go internal/apikey/manager.go)

say "# REV-BUNKER-005/006/007 — RED + NEGATIVE CONTROL battery"
say "# graded revision: HEAD = $(git rev-parse HEAD)  ($(git log -1 --format=%s | cut -c1-70))"
say "# date: $(date --iso-8601=seconds)"
say "#"
say "# committed blob sha256 (the revision every phase below mutates and restores):"
for f in "${CELLS[@]}"; do say "#   $(sha "$f")  $f"; done
say "#"

# ---------------------------------------------------------------- 005 RED ---
say ""
say "==================== 005-RED: the credential gate is never applied to the registration ===================="
say "The pre-fix tree registered these routes on the raw router with no credential check at all; the mutation"
say "below leaves the gate object handed to registerGraphRoutes unapplied, i.e. behaviourally the filed defect."
say "before:  $(sha internal/server/server.go)"
sed -i 's@gr\.Use(gate)@gr.Use(func(next http.Handler) http.Handler { return next })@' internal/server/server.go
say "mutated: $(sha internal/server/server.go)"
go test ./internal/server/ -run TestREVBUNKER005 -count=1 2>&1 | grep -vE '^time=|\{"time"' | tee -a "$OUT" | grep -E '^(--- FAIL|--- PASS|ok |FAIL|    [a-z_]+\.go)' | head -20
restore internal/server/server.go
say "after:   $(sha internal/server/server.go)"

# ------------------------------------------------------- 005 NEG CONTROL ---
say ""
say "==================== 005-CONTROL: the gate IS applied, but the validator admits anything ===================="
say "A different layer from the RED above: the registration is untouched, and the middleware's own"
say "enabled/validator check is bypassed -- so the surface is gated by name and open in fact."
say "before:  $(sha internal/auth/httpware.go)"
sed -i 's@if !enabled || jwa == nil {@if true {@' internal/auth/httpware.go
say "mutated: $(sha internal/auth/httpware.go)"
go test ./internal/server/ -run TestREVBUNKER005 -count=1 2>&1 | grep -vE '^time=|\{"time"' | tee -a "$OUT" | grep -E '^(--- FAIL|--- PASS|ok |FAIL|    [a-z_]+\.go)' | head -20
restore internal/auth/httpware.go
say "after:   $(sha internal/auth/httpware.go)"

# ---------------------------------------------------------------- 006 RED ---
say ""
say "==================== 006-RED: the throttle is not armed by the interceptors ===================="
say "Reverts BOTH arming sites to their pre-fix form (the FromAuth helpers returning the raw instance), which"
say "is exactly the template the daemon used when the audit sink was absent: no sink -> AttachDenySink never"
say "called -> throttle nil. Driven with audit.enabled:false through the daemon's own buildAuthInterceptors."
say "before:  $(sha internal/auth/interceptor.go)"
sed -i 's@^\treturn ArmThrottle(jwtAuth)$@\treturn jwtAuth@' internal/auth/interceptor.go
sed -i 's@^\treturn ArmThrottle(NewMasterOnlyJWTAuthFromAuth(jwtAuth))$@\treturn NewMasterOnlyJWTAuthFromAuth(jwtAuth)@' internal/auth/interceptor.go
say "mutated: $(sha internal/auth/interceptor.go)"
go test ./internal/server/ -run TestREVBUNKER006 -count=1 2>&1 | grep -vE '^time=|\{"time"' | tee -a "$OUT" | grep -E '^(--- FAIL|--- PASS|ok |FAIL|    [a-z_]+\.go)' | head -20
restore internal/auth/interceptor.go
say "after:   $(sha internal/auth/interceptor.go)"

# ------------------------------------------------------- 006 NEG CONTROL ---
say ""
say "==================== 006-CONTROL: the throttle is armed but never CONSULTED ===================="
say "A different layer from the RED above: the arming is left intact (the interceptors still carry throttle"
say "state) and the backoff decision itself is neutered, so a counter that moves yet never refuses the"
say "request is shown to be insufficient -- both 'arm it' and 'consult it' are load-bearing."
say "before:  $(sha internal/auth/throttle.go)"
sed -i 's@if now.Before(st.backoffUntil) {@if false \&\& now.Before(st.backoffUntil) {@' internal/auth/throttle.go
say "mutated: $(sha internal/auth/throttle.go)"
go test ./internal/server/ -run TestREVBUNKER006 -count=1 2>&1 | grep -vE '^time=|\{"time"' | tee -a "$OUT" | grep -E '^(--- FAIL|--- PASS|ok |FAIL|    [a-z_]+\.go)' | head -20
restore internal/auth/throttle.go
say "after:   $(sha internal/auth/throttle.go)"

# ---------------------------------------------------------------- 007 RED ---
say ""
say "==================== 007-RED: both compares return to their pre-fix byte-wise form ===================="
say "interceptor.go: ConstantTimeCompare(token, a.token) -> token != a.token."
say "manager.go: the subtle compare -> key.TokenHash == tokenHash, with the now-unused crypto/subtle import"
say "dropped, which is the base blob's exact form. The cell asserts the SOURCE, because the two forms are"
say "behaviourally identical by design (proved separately by the admission-equivalence tables)."
say "before interceptor.go: $(sha internal/auth/interceptor.go)"
say "before manager.go:     $(sha internal/apikey/manager.go)"
sed -i 's@if !ConstantTimeCompare(token, a.token) {@if token != a.token {@' internal/auth/interceptor.go
sed -i 's@if subtle.ConstantTimeCompare(\[\]byte(key.TokenHash), \[\]byte(tokenHash)) == 1 {@if key.TokenHash == tokenHash {@' internal/apikey/manager.go
sed -i '/^\t"crypto\/subtle"$/d' internal/apikey/manager.go
say "mutated interceptor.go: $(sha internal/auth/interceptor.go)"
say "mutated manager.go:     $(sha internal/apikey/manager.go)"
go test ./internal/auth/ ./internal/apikey/ -run TestREVBUNKER007 -count=1 2>&1 | grep -vE '^time=|\{"time"' | tee -a "$OUT" | grep -E '^(--- FAIL|--- PASS|ok |FAIL|    [a-z_]+)' | head -20
restore internal/auth/interceptor.go internal/apikey/manager.go
say "after interceptor.go: $(sha internal/auth/interceptor.go)"
say "after manager.go:     $(sha internal/apikey/manager.go)"

# ------------------------------------------------------- 007 NEG CONTROL ---
say ""
say "==================== 007-CONTROL: the wiring is constant-time by NAME, the helper is not ===================="
say "A different layer from the RED above: the call sites still say ConstantTimeCompare (so a name-level check"
say "would pass) while the shared helper itself returns a == comparison -- the control proves the cell reaches"
say "THROUGH the helper to crypto/subtle instead of trusting the identifier."
say "before:  $(sha internal/auth/jwt.go)"
sed -i 's@^\treturn subtle.ConstantTimeCompare(\[\]byte(a), \[\]byte(b)) == 1$@\treturn a == b@' internal/auth/jwt.go
say "mutated: $(sha internal/auth/jwt.go)"
go test ./internal/auth/ -run TestREVBUNKER007 -count=1 2>&1 | grep -vE '^time=|\{"time"' | tee -a "$OUT" | grep -E '^(--- FAIL|--- PASS|ok |FAIL|    [a-z_]+)' | head -20
restore internal/auth/jwt.go
say "after:   $(sha internal/auth/jwt.go)"

# --------------------------------------------------------------- FINAL STATE --
say ""
say "==================== final state ===================="
git checkout HEAD -- "${CELLS[@]}"
for f in "${CELLS[@]}"; do
	if git diff --quiet HEAD -- "$f"; then say "CLEAN  $(sha "$f")  $f"; else say "DIRTY  $f"; fi
done
say "worktree clean vs HEAD: $(git status --porcelain -- "${CELLS[@]}" | wc -l) modified tracked file(s) under test"

echo
echo "=========== SUMMARY (PASS/FAIL lines only) ==========="
grep -E '^(--- FAIL|--- PASS|ok |FAIL|\s+--- FAIL)' "$OUT" | head -40
