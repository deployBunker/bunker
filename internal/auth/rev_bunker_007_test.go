package auth

// rev_bunker_007_test.go — REV-BUNKER-007: credential material must be
// compared in constant time everywhere this package compares it.
//
// The defect is not a remotely exploitable read — it is an inconsistency with
// the package's OWN discipline: the JWT static-token fallback (jwt.go) and the
// retired-secret check already go through subtle.ConstantTimeCompare, while
// TokenAuth's static-token path used a byte-wise !=. A byte-wise comparison
// returns at the first differing byte, so the length of the matching prefix is
// observable in the response timing.
//
// The assertion is therefore necessarily structural (a timing property is not
// reliably measurable in a unit test) — plus a behavioural equivalence table
// proving the swap did not change a single admission decision, including the
// length-mismatch case where subtle.ConstantTimeCompare's implementation
// differs from ==.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// callerDir returns the directory holding this test file, so the source
// assertions below read the REAL module files from disk rather than a path
// relative to whatever directory the test binary happens to run in.
func callerDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the package sources")
	}
	return filepath.Dir(file)
}

// funcBody returns the full source text of the top-level function whose
// definition contains sig, from its declaration to its closing brace.
func funcBody(t *testing.T, path, sig string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(src)
	i := strings.Index(text, sig)
	if i < 0 {
		t.Fatalf("%s does not define %q — this pin must name a real definition", path, sig)
	}
	j := strings.Index(text[i:], "{")
	if j < 0 {
		t.Fatalf("%s: no body after %q", path, sig)
	}
	depth := 0
	for k := i + j; k < len(text); k++ {
		switch text[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[i : k+1]
			}
		}
	}
	t.Fatalf("%s: unbalanced braces after %q", path, sig)
	return ""
}

// TestREVBUNKER007_TokenAuthComparesInConstantTime pins the static-token
// comparison to the package's constant-time helper, and pins the helper itself
// to crypto/subtle — a helper that quietly used == would satisfy the first
// assertion while fixing nothing.
func TestREVBUNKER007_TokenAuthComparesInConstantTime(t *testing.T) {
	dir := callerDir(t)

	body := funcBody(t, filepath.Join(dir, "interceptor.go"), "func (a *TokenAuth) authenticate(")
	if !strings.Contains(body, "ConstantTimeCompare(token, a.token)") {
		t.Errorf("TokenAuth.authenticate must compare the presented token with the constant-time helper "+
			"(want a call to ConstantTimeCompare(token, a.token)); body:\n%s", body)
	}
	for _, forbidden := range []string{"token != a.token", "token == a.token"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("TokenAuth.authenticate still compares credential material with %q — a byte-wise compare "+
				"returns at the first differing byte, leaking the matching prefix length in the response timing", forbidden)
		}
	}

	helper := funcBody(t, filepath.Join(dir, "jwt.go"), "func ConstantTimeCompare(")
	if !strings.Contains(helper, "subtle.ConstantTimeCompare(") {
		t.Errorf("ConstantTimeCompare must be backed by crypto/subtle; body:\n%s", helper)
	}
}

// TestREVBUNKER007_TokenAuthAdmissionUnchanged proves the constant-time swap
// did not change one admission decision: every case below yields the same
// answer a byte-wise comparison yields, including the length-mismatch case
// where constant-time comparison is implemented differently.
func TestREVBUNKER007_TokenAuthAdmissionUnchanged(t *testing.T) {
	const good = "rev-bunker-007-correct-token-value"

	cases := []struct {
		name      string
		presented string
		wantOK    bool
	}{
		{name: "the correct token is admitted", presented: good, wantOK: true},
		{name: "same-length wrong token is denied", presented: strings.Repeat("x", len(good)), wantOK: false},
		{name: "one byte shorter is denied", presented: good[:len(good)-1], wantOK: false},
		{name: "one byte longer is denied", presented: good + "x", wantOK: false},
		{name: "a matching prefix is denied", presented: good[:len(good)/2], wantOK: false},
		{name: "an empty token is denied", presented: "", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh interceptor per case: throttle state (REV-BUNKER-006)
			// is per-source and must not leak between rows.
			a := NewTokenAuth(good)
			err := a.authenticate(hdr("Bearer "+tc.presented), "198.51.100.9:4242", "/bunker.v1.Bunkerd/ServerInfo")
			if tc.wantOK {
				if err != nil {
					t.Fatalf("token %q must be admitted: %v", tc.presented, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("token %q must be denied", tc.presented)
			}
			if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
				t.Errorf("token %q: code = %v, want Unauthenticated", tc.presented, got)
			}
		})
	}
}
