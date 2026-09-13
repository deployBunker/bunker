package hostsetup

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Ownership of lines in /etc/pam.d/sshd (GAP-075 rework).
//
// The first two revisions decided ownership from the LINE TEXT: any exact bare
// `session required pam_namespace.so` counted as Bunker's, so install, repair
// and uninstall could delete or relocate a rule the operator or the
// distribution owned. Ownership is now marker/block-based — a block starts at
// Bunker's marker comment or at Bunker's own classifier line, and only the
// lines inside such a block are ever touched. Everything else is preserved
// byte-for-byte, including a bare or optioned pam_namespace rule and an
// operator's pam_succeed_if rule.

// foreignPAMStack is a stack that mixes operator/distribution rules with a
// pre-existing Bunker block, with foreign rules both BEFORE and AFTER it.
const foreignPAMStack = `#%PAM-1.0
@include common-auth
session    required     pam_loginuid.so
session    required     pam_namespace.so
session    required     pam_namespace.so operator-extra-option
session    required     pam_succeed_if.so quiet uid >= 1000
@include common-session
# bunker GAP-075: per-agent private /tmp (managed by ` + "`bunker host-provision`" + `)
session    [success=ok auth_err=1 default=ignore]    pam_succeed_if.so quiet user ingroup stale-group
session    required     pam_namespace.so
session    required     pam_namespace.so after-the-block
session    optional     pam_motd.so  motd=/run/motd.dynamic
`

// foreignLines are the operator/distribution rules that must survive both an
// install/repair and an uninstall, byte for byte.
var foreignLines = []string{
	"session    required     pam_namespace.so",
	"session    required     pam_namespace.so operator-extra-option",
	"session    required     pam_succeed_if.so quiet uid >= 1000",
	"session    required     pam_namespace.so after-the-block",
	"session    optional     pam_motd.so  motd=/run/motd.dynamic",
	"@include common-auth",
	"@include common-session",
	"session    required     pam_loginuid.so",
}

// TestEnsureRepairPreservesForeignRules: repairing a stale Bunker block must
// leave every foreign rule exactly where it was, even when a foreign bare
// pam_namespace rule sits immediately before or after the managed block.
func TestEnsureRepairPreservesForeignRules(t *testing.T) {
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, foreignPAMStack)

	if _, err := o.EnsureTmpNamespace(context.Background()); err != nil {
		t.Fatalf("EnsureTmpNamespace() error = %v", err)
	}
	got := readFileString(t, o.SSHDConfigPath)

	for _, line := range foreignLines {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("foreign rule was dropped or reflowed: %q\n%s", line, got)
		}
	}
	if strings.Contains(got, "stale-group") {
		t.Errorf("the stale Bunker block survived the repair:\n%s", got)
	}
	if n := strings.Count(got, NamespacePAMClassifierLine()); n != 1 {
		t.Errorf("classifier lines = %d, want exactly 1:\n%s", n, got)
	}
	if n := strings.Count(got, "pam_exec.so quiet"); n != 1 {
		t.Errorf("verifier lines = %d, want exactly 1:\n%s", n, got)
	}
	if !o.pamBlockIntact(splitLines(got)) {
		t.Errorf("repaired block is not intact:\n%s", got)
	}
	// The repaired block is appended AFTER every foreign rule (Bunker never
	// inserts itself among the distribution's own lines).
	if idx := strings.LastIndex(got, "pam_motd.so"); idx > strings.Index(got, NamespacePAMClassifierLine()) {
		t.Errorf("managed block was not appended after the foreign rules:\n%s", got)
	}

	// The file must be stable: a second install changes nothing and reports no
	// mutation, even with the foreign bare pam_namespace rule present.
	before := got
	rep, err := o.EnsureTmpNamespace(context.Background())
	if err != nil {
		t.Fatalf("second EnsureTmpNamespace() error = %v", err)
	}
	if after := readFileString(t, o.SSHDConfigPath); after != before {
		t.Errorf("second install changed the file:\n%s", after)
	}
	for _, c := range rep.Mutations() {
		if c.Action == "write" {
			t.Errorf("second install performed a write: %+v", c)
		}
	}
}

// TestRemovePreservesForeignRules: uninstalling must remove exactly Bunker's
// block — the foreign bare and optioned pam_namespace rules and the operator's
// pam_succeed_if rule stay untouched, and the file is left with no Bunker lines.
func TestRemovePreservesForeignRules(t *testing.T) {
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, foreignPAMStack)

	rep, err := o.RemoveTmpNamespace(context.Background())
	if err != nil {
		t.Fatalf("RemoveTmpNamespace() error = %v", err)
	}
	got := readFileString(t, o.SSHDConfigPath)

	for _, line := range foreignLines {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("uninstall dropped or reflowed a foreign rule: %q\n%s", line, got)
		}
	}
	for _, bunker := range []string{namespaceMarker, "stale-group", NamespacePAMClassifierLine(), "pam_exec.so"} {
		if strings.Contains(got, bunker) {
			t.Errorf("Bunker line %q survived uninstall:\n%s", bunker, got)
		}
	}
	// Removing the block restores the file to its pre-install bytes plus the
	// foreign lines only: nothing else may be rewritten.
	if got != foreignPAMStackAfterUninstall {
		t.Errorf("uninstall result differs from the original file:\n%q\nwant\n%q", got, foreignPAMStackAfterUninstall)
	}
	// Foreign pam_namespace rules ARE reported, so "uninstalled" cannot be a lie.
	if !rep.Has("note") {
		t.Errorf("operator-owned pam_namespace rules were not reported: %s", rep)
	}
}

// foreignPAMStackAfterUninstall is the exact bytes expected after uninstalling
// Bunker from foreignPAMStack: the managed block (its marker, the stale legacy
// guard and the bare module line) is gone, every other line is untouched and in
// order.
const foreignPAMStackAfterUninstall = `#%PAM-1.0
@include common-auth
session    required     pam_loginuid.so
session    required     pam_namespace.so
session    required     pam_namespace.so operator-extra-option
session    required     pam_succeed_if.so quiet uid >= 1000
@include common-session
session    required     pam_namespace.so after-the-block
session    optional     pam_motd.so  motd=/run/motd.dynamic
`

// TestManagedBlockOwnershipBoundary pins the exact ownership rule as a table, so
// a future edit to managedBlockStart/blockMember cannot quietly widen what
// Bunker is willing to delete.
func TestManagedBlockOwnershipBoundary(t *testing.T) {
	bare := "session    required     pam_namespace.so"
	optioned := "session    required     pam_namespace.so ignore_config_error"
	foreignOptioned := "session    required     pam_namespace.so vendor-option"
	operatorSucceedIf := "session    required     pam_succeed_if.so quiet uid >= 1000"
	legacyGuard := "session    [success=ok auth_err=1 default=ignore]    pam_succeed_if.so quiet user ingroup old-group"

	for _, tc := range []struct {
		name       string
		line       string
		wantStart  bool
		wantMember bool
	}{
		{name: "marker starts a block", line: namespaceMarker, wantStart: true, wantMember: true},
		{name: "canonical classifier starts a block", line: NamespacePAMClassifierLine(), wantStart: true, wantMember: true},
		{name: "canonical verifier is a member", line: "session    [success=ignore default=die]    pam_exec.so quiet /usr/lib/bunker/pam-tmp-guard verify bunker-agents", wantMember: true},
		{name: "legacy module line is a member", line: optioned, wantMember: true},
		{name: "legacy guard line is a member", line: legacyGuard, wantMember: true},
		{name: "bare module line is NOT a start", line: bare, wantMember: true},
		{name: "foreign optioned module line is NOT ours", line: foreignOptioned},
		{name: "operator pam_succeed_if rule is NOT ours", line: operatorSucceedIf},
		{name: "distribution include is NOT ours", line: "@include common-session"},
		{name: "empty line is NOT ours", line: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedBlockStart(tc.line); got != tc.wantStart {
				t.Errorf("managedBlockStart(%q) = %v, want %v", tc.line, got, tc.wantStart)
			}
			if got := blockMember(tc.line); got != tc.wantMember {
				t.Errorf("blockMember(%q) = %v, want %v", tc.line, got, tc.wantMember)
			}
		})
	}
}

// TestBareOperatorRuleAloneIsNeverStripped: with no marker anywhere, a bare
// pam_namespace line must survive install, a second install and uninstall.
func TestBareOperatorRuleAloneIsNeverStripped(t *testing.T) {
	body := "#%PAM-1.0\n@include common-auth\nsession    required     pam_namespace.so\n"
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, body)

	if _, err := o.EnsureTmpNamespace(context.Background()); err != nil {
		t.Fatalf("EnsureTmpNamespace() error = %v", err)
	}
	if got, want := readFileString(t, o.SSHDConfigPath), body+strings.Join(o.NamespacePAMBlockForOptions(), "\n")+"\n"; got != want {
		t.Errorf("install changed more than the appended block:\n%q\nwant\n%q", got, want)
	}
	if _, err := o.RemoveTmpNamespace(context.Background()); err != nil {
		t.Fatalf("RemoveTmpNamespace() error = %v", err)
	}
	if got := readFileString(t, o.SSHDConfigPath); got != body {
		t.Errorf("uninstall did not restore the file byte-for-byte:\n%q\nwant\n%q", got, body)
	}
}

// TestHalfStrippedBunkerBlockIsRepaired documents the one deliberate edge of the
// ownership rule: INSIDE a Bunker block (marker or classifier first), a
// leftover bare module line is treated as Bunker's own half-stripped line and
// cleaned up — outside a block nothing is touched.
func TestHalfStrippedBunkerBlockIsRepaired(t *testing.T) {
	body := "#%PAM-1.0\n@include common-auth\n" + namespaceMarker + "\nsession    required     pam_namespace.so\n"
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, body)

	if _, err := o.EnsureTmpNamespace(context.Background()); err != nil {
		t.Fatalf("EnsureTmpNamespace() error = %v", err)
	}
	got := readFileString(t, o.SSHDConfigPath)
	if n := strings.Count(got, "pam_namespace.so"); n != 1 {
		t.Errorf("pam_namespace lines = %d, want exactly the managed one:\n%s", n, got)
	}
	if !o.pamBlockIntact(splitLines(got)) {
		t.Errorf("half-stripped block was not repaired into the canonical block:\n%s", got)
	}
}

// TestEnsureRepairsRejectedFirstRevisionShape: an operator upgrading from the
// first revision (marker + fail-open module line) must end up with the
// canonical block, the fail-open line gone, and no foreign rule disturbed.
func TestEnsureRepairsRejectedFirstRevisionShape(t *testing.T) {
	body := "#%PAM-1.0\n@include common-auth\n" +
		"session    required     pam_namespace.so\n" + // operator's own rule, before
		namespaceMarker + "\n" +
		"session    required     pam_namespace.so ignore_config_error\n" +
		"session    optional     pam_motd.so  motd=/run/motd.dynamic\n" // foreign, after
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, body)

	if _, err := o.EnsureTmpNamespace(context.Background()); err != nil {
		t.Fatalf("EnsureTmpNamespace() error = %v", err)
	}
	got := readFileString(t, o.SSHDConfigPath)
	if strings.Contains(got, "ignore_config_error") {
		t.Errorf("fail-open module line survived the upgrade:\n%s", got)
	}
	if !strings.Contains(got, "session    required     pam_namespace.so\n") {
		t.Errorf("the operator's own bare pam_namespace rule was removed:\n%s", got)
	}
	if !strings.Contains(got, "pam_motd.so") {
		t.Errorf("the rule after the legacy block was removed:\n%s", got)
	}
	if n := strings.Count(got, "pam_namespace.so"); n != 2 {
		t.Errorf("pam_namespace lines = %d, want the operator's + the managed one:\n%s", n, got)
	}
	if !o.pamBlockIntact(splitLines(got)) {
		t.Errorf("upgraded block is not intact:\n%s", got)
	}
}

// TestForeignRuleDoesNotForceAWrite: an operator rule that merely looks like a
// Bunker line shape must not make the installer rewrite the file on every run
// (that would be a silent, repeated mutation of a distribution file).
func TestForeignRuleDoesNotForceAWrite(t *testing.T) {
	body := "#%PAM-1.0\nsession    required     pam_namespace.so custom-operator-option\n"
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, body)

	if _, err := o.EnsureTmpNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := readFileString(t, o.SSHDConfigPath)
	rep, err := o.EnsureTmpNamespace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after := readFileString(t, o.SSHDConfigPath); after != before {
		t.Errorf("a foreign rule forced a rewrite:\n%s", after)
	}
	for _, c := range rep.Mutations() {
		if c.Action == "write" {
			t.Errorf("second install performed a file write: %+v", c)
		}
	}
}

// TestUninstallRemovesHelperAndManifest pins the managed-file inventory and the
// ordering promise (block first, helper last).
func TestUninstallRemovesHelperAndManifest(t *testing.T) {
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, samplePAM)
	if _, err := o.EnsureTmpNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(o.PamHelperPath()); err != nil {
		t.Fatalf("helper not installed: %v", err)
	}
	if _, err := o.RemoveTmpNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{o.PamHelperPath(), o.PamHelperManifestPath()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived uninstall (%v)", path, err)
		}
	}
	// The block must be gone from the PAM stack together with them.
	if got := readFileString(t, o.SSHDConfigPath); got != samplePAM {
		t.Errorf("PAM stack after uninstall:\n%q\nwant\n%q", got, samplePAM)
	}
}
