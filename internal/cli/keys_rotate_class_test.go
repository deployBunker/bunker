// keys_rotate_class_test.go — DF-BUNKER-46 output contract: `bunker key
// rotate` must name the credential class it returns (the HS256 JWT signing
// secret, NOT a bearer token), point at the static auth.token as the real
// bearer credential, and state that the in-memory switch is IMMEDIATE —
// persisting + restarting is only crash-persistence. The help text must
// stop implying that a restart is required.
package cli

import (
	"strings"
	"testing"
)

func TestKeyRotate_OutputNamesClassAndImmediacy(t *testing.T) {
	mock := &keysMockServer{
		newSecret:    "rotated-secret-class-test-0123456789abcdef",
		rotatePrevFP: "sha256:cafe460046",
	}
	newKeysTestSetup(t, mock)

	out, execErr := runKeys(t, "rotate")
	if execErr != nil {
		t.Fatalf("execute key rotate: %v", execErr)
	}
	for _, want := range []string{
		"NOT a bearer token",
		"static auth.token",
		"IMMEDIATE",
		"crash-persistence",
		"shown ONCE",
		"unauthenticated: invalid token",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rotate output missing %q:\n%s", want, out)
		}
	}
	// The new secret still appears EXACTLY ONCE (the load-bearing contract
	// from keys_test.go, re-checked here because the comment block grew).
	if n := strings.Count(out, mock.newSecret); n != 1 {
		t.Fatalf("new secret printed %d times, want 1:\n%s", n, out)
	}
}

func TestKeyRotate_HelpNamesClassWithoutRestartRequirement(t *testing.T) {
	// The Long help lives on the parent `key` command; the rotate verb adds
	// its own Short. Assert over both.
	parent := NewKeysCommand()
	help := parent.Short + "\n" + parent.Long + "\n" + newKeyRotateCommand().Short

	for _, want := range []string{"NOT a bearer token", "IMMEDIATE"} {
		if !strings.Contains(help, want) {
			t.Fatalf("key help missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(help), "restart bunkerd to load") {
		t.Fatal("help must not imply a restart is required to load the new secret")
	}
}
