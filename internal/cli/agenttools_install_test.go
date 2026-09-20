package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommandNameParsesVersionToken: the drift check compares BUILDS, so it has
// to pull the token out of a whole `toolsd version <token> (built ...)` line.
// Handing back the sentence would make every comparison a mismatch.
func TestCommandNameParsesVersionToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"toolsd version 98c75fa-dirty (built 2026-09-20T18:44:36Z)", "98c75fa-dirty"},
		{"toolsd version v1.2.3", "v1.2.3"},
		{"toolsd version dev", "dev"},
		// Not the shape we expect: pass it through rather than inventing a token.
		{"something else entirely", "something else entirely"},
		{"", ""},
	}
	for _, c := range cases {
		if got := commandName(c.in); got != c.want {
			t.Errorf("commandName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIsVendoredDeliverable: only tools we build ourselves may be copied onto an
// agent. ripgrep and the language servers are in distribution registries and
// must go through the package-add path instead, so the classifier has to say no.
func TestIsVendoredDeliverable(t *testing.T) {
	if !isVendoredDeliverable("toolsd") {
		t.Error("toolsd must be deliverable by copy")
	}
	for _, name := range []string{"rg", "gopls", "git", "jq", ""} {
		if isVendoredDeliverable(name) {
			t.Errorf("%q must NOT be a copied deliverable (it belongs to the package-add path)", name)
		}
	}
}

// TestResolveDeliverableBinary: an explicit --binary is validated rather than
// trusted, and a missing one fails with the remedy named.
func TestResolveDeliverableBinary(t *testing.T) {
	dir := t.TempDir()

	executable := filepath.Join(dir, "toolsd")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveDeliverableBinary("toolsd", executable); err != nil || got != executable {
		t.Errorf("explicit executable path: got (%q, %v), want (%q, nil)", got, err, executable)
	}

	if _, err := resolveDeliverableBinary("toolsd", filepath.Join(dir, "nope")); err == nil {
		t.Error("a nonexistent --binary must fail, not fall through to PATH")
	}
	if _, err := resolveDeliverableBinary("toolsd", dir); err == nil {
		t.Error("a directory must be refused")
	}

	notExec := filepath.Join(dir, "toolsd-noexec")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDeliverableBinary("toolsd", notExec); err == nil {
		t.Error("a non-executable --binary must be refused")
	}

	// Absent from PATH and no override: the error must name the remedy.
	_, err := resolveDeliverableBinary("definitely-not-a-real-tool-xyz", "")
	if err == nil {
		t.Fatal("an unresolvable tool must fail")
	}
	if !strings.Contains(err.Error(), "--binary") || !strings.Contains(err.Error(), "make dist") {
		t.Errorf("the failure must name the remedy, got: %v", err)
	}
}

// TestAssertStaticallyLinkedRefusesWhenItCannotConfirm is the load-bearing
// direction of the guard: a file it cannot positively identify as static must be
// REFUSED. A missing path is the cheapest proof that it never defaults to allow.
func TestAssertStaticallyLinkedRefusesWhenItCannotConfirm(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := assertStaticallyLinked(missing)
	if err == nil {
		t.Fatal("a path that cannot be examined must be refused, not allowed")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("the refusal must say it is refusing, got: %v", err)
	}
}

// TestAssertStaticallyLinkedAgreesWithLdd: the guard must not be a constant.
// When the running test binary is itself dynamic, the guard must REFUSE it; when
// it is static, it must accept. Either way it has to agree with an independent
// reading of the same file.
func TestAssertStaticallyLinkedAgreesWithLdd(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot locate the test binary")
	}
	out, lddErr := exec.Command("ldd", self).CombinedOutput()
	if lddErr != nil && len(out) == 0 {
		t.Skip("ldd unavailable; the guard's ldd branch cannot be exercised here")
	}
	isDynamic := strings.Contains(string(out), "=>")

	gotErr := assertStaticallyLinked(self)
	if isDynamic && gotErr == nil {
		t.Errorf("ldd reports a dynamically linked binary (%s) but the guard accepted it", self)
	}
	if !isDynamic && gotErr != nil {
		t.Errorf("ldd reports a static binary (%s) but the guard refused it: %v", self, gotErr)
	}
}

// TestFindingForAndStatusOf: the post-delivery verdict reads the probe result,
// and a tool absent from the report must not read as present.
func TestFindingForAndStatusOf(t *testing.T) {
	r := parseAgentToolOutput("a", "toolsd\tpresent\ttoolsd version v9\nrg\tabsent\t", 0)

	if f := findingFor(r, "toolsd"); f == nil || f.Status != "present" {
		t.Errorf("toolsd should be present, got %v", f)
	}
	if f := findingFor(r, "rg"); f == nil || f.Status != "absent" {
		t.Errorf("rg should be absent, got %v", f)
	}
	// A tool the catalog does not contain is simply not found...
	if f := findingFor(r, "not-a-tool"); f != nil {
		t.Errorf("findingFor returned %v for an unknown tool", f)
	}
	// ...and an unfound tool must never be reported as present.
	if got := statusOf(nil); got != "not reported" {
		t.Errorf("statusOf(nil) = %q, want \"not reported\"", got)
	}
}
