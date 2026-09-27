package apikey

// rev_bunker_007_test.go — REV-BUNKER-007, second site: Manager.Validate
// compared the SHA-256 hex digest of a presented token with ==.
//
// Honest severity: the digest is already a one-way function of the presented
// secret, so observing the comparison's timing does not reveal the secret — a
// caller cannot steer the compared value toward a target digest it does not
// already know. This is defence in depth and consistency with internal/auth
// (which compares every credential in constant time), not a remotely
// exploitable read. It is fixed because a codebase that applies the discipline
// in three places and forgets a fourth trains the next reader to copy the
// wrong one.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func reviewCallerDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the package sources")
	}
	return filepath.Dir(file)
}

func reviewFuncBody(t *testing.T, path, sig string) string {
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

// TestREVBUNKER007_ValidateComparesInConstantTime pins the digest comparison
// in Manager.Validate to crypto/subtle.
func TestREVBUNKER007_ValidateComparesInConstantTime(t *testing.T) {
	path := filepath.Join(reviewCallerDir(t), "manager.go")
	body := reviewFuncBody(t, path, "func (m *Manager) Validate(")

	if !strings.Contains(body, "subtle.ConstantTimeCompare(") {
		t.Errorf("Manager.Validate must compare token digests in constant time (want crypto/subtle); body:\n%s", body)
	}
	if strings.Contains(body, "key.TokenHash == tokenHash") {
		t.Error("Manager.Validate still compares the token digest with == — a byte-wise compare returns at the " +
			"first differing byte, and this surface is the one every opaque sub-key passes through")
	}
	if !strings.Contains(body, "tokenHash") {
		t.Error("Manager.Validate no longer mentions the computed digest; the pin above may be matching the wrong body")
	}
}

// TestREVBUNKER007_ValidateAdmissionUnchanged proves the swap changed no
// admission decision for an issued, a wrong, a truncated and a revoked key.
func TestREVBUNKER007_ValidateAdmissionUnchanged(t *testing.T) {
	m := NewManager("rev-bunker-007-master-key")
	token, key, err := m.Generate("agent-rev-007", time.Hour)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if got, err := m.Validate(token); err != nil {
		t.Fatalf("the issued token must validate: %v", err)
	} else if got.KeyID != key.KeyID {
		t.Errorf("Validate returned key %q, want %q", got.KeyID, key.KeyID)
	}

	// Same-length wrong credential: the case that exercises the comparison's
	// whole length rather than an early return.
	sameLengthWrong := strings.Repeat("a", len(token))
	if sameLengthWrong == token {
		t.Fatal("fixture is degenerate: the wrong token equals the issued token")
	}
	if _, err := m.Validate(sameLengthWrong); err == nil {
		t.Error("a same-length wrong token must be rejected")
	}

	// A prefix (one byte short) must be rejected too.
	if _, err := m.Validate(token[:len(token)-1]); err == nil {
		t.Error("a truncated token must be rejected")
	}

	// Revocation still wins over a digest match (GAP-132), i.e. the constant
	// time comparison did not reorder any admission rule.
	if err := m.Revoke(key.KeyID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := m.Validate(token); err == nil {
		t.Error("a revoked key must not validate")
	}
}
