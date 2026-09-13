package hostsetup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// classifierModuleStub is the content of the fake pam_succeed_if.so the tests
// install. It carries the tokens the installer probes for (guardPatternTokens),
// exactly like a real Linux-PAM >= 1.6 libpam-modules build, so the sandbox
// exercises the production happy path; TestEnsureTmpNamespace_ClassifierWithout-
// PatternSupportFailsLoud covers the other side.
const classifierModuleStub = "ELF fnmatch noglob"

// sshSandbox builds an Options tree with a fake sshd PAM stack and a fake
// pam_namespace module, so the namespace provisioner can be exercised without
// touching the host.
func sshSandbox(t *testing.T, rec *recorder, pamBody string) (Options, string) {
	t.Helper()
	root := rec.root
	o := Options{Runner: rec.run, Root: root}
	o = o.WithDefaults()

	if err := os.MkdirAll(filepath.Dir(o.SSHDConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.SSHDConfigPath, []byte(pamBody), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mod := range []string{"pam_namespace.so", "pam_succeed_if.so", "pam_exec.so"} {
		module := filepath.Join(root, "lib/x86_64-linux-gnu/security", mod)
		if err := os.MkdirAll(filepath.Dir(module), 0o755); err != nil {
			t.Fatal(err)
		}
		// The classifier stub carries the tokens the installer probes for, the
		// way the real libpam-modules build does (see guardPatternTokens).
		if err := os.WriteFile(module, []byte(classifierModuleStub), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return o, root
}

const samplePAM = `#%PAM-1.0
@include common-auth
@include common-account
session    required     pam_loginuid.so
@include common-session
session    required     pam_limits.so
`

func TestNamespaceConf(t *testing.T) {
	got := NamespaceConf("/var/lib/bunkerd/agent-tmp")

	// The instance parent must be terminated with a slash: pam_namespace
	// appends the user name to it to build the instance directory path.
	if !strings.Contains(got, "/tmp  /var/lib/bunkerd/agent-tmp/  user:noinit  root") {
		t.Errorf("namespace.conf line is wrong:\n%s", got)
	}
	// root is excluded; every other user (i.e. every agent, including one
	// created without re-running provisioning) is polyinstantiated.
	if strings.Contains(got, "~") {
		t.Errorf("namespace.conf uses an inclusion list, which would fail open for new agents:\n%s", got)
	}
	if !strings.Contains(got, "GAP-075") {
		t.Errorf("namespace.conf is not self-describing:\n%s", got)
	}
}

func TestTmpNamespaceStatus(t *testing.T) {
	rec := newRecorder(t)
	o, root := sshSandbox(t, rec, samplePAM)

	st, err := o.TmpNamespaceStatus()
	if err != nil {
		t.Fatalf("TmpNamespaceStatus() error = %v", err)
	}
	if !st.ModulePresent {
		t.Errorf("module not detected at %s", st.ModulePath)
	}
	if st.ModulePath != filepath.Join(root, "lib/x86_64-linux-gnu/security/pam_namespace.so") {
		t.Errorf("module path = %q", st.ModulePath)
	}
	if st.ConfPresent || st.PAMBlockPresent || st.Active {
		t.Errorf("status before install = %+v, want inactive", st)
	}
}

// TestActiveRequiresEveryBoundaryProperty is the status-side table the review
// asked for: starting from a fully observed, isolated state, breaking ANY single
// property the runtime helper enforces must make the verdict false. It is what
// keeps `Active` from drifting back into a claim about the installer's intent —
// including the ownership half, which a non-root test process cannot observe on
// disk and therefore cannot pin through TmpNamespaceStatus alone.
func TestActiveRequiresEveryBoundaryProperty(t *testing.T) {
	healthy := TmpNamespaceState{
		ModulePresent:              true,
		ModulePath:                 "/lib/security/pam_namespace.so",
		GuardModulePresent:         true,
		GuardModulePath:            "/lib/security/pam_succeed_if.so",
		GuardModuleSupportsPattern: true,
		ExecModulePresent:          true,
		ExecModulePath:             "/lib/security/pam_exec.so",
		ConfPresent:                true,
		ConfPath:                   "/etc/security/namespace.d/50-bunker-agents.conf",
		ConfRuleOK:                 true,
		ConfOwnerOK:                true,
		ConfModeOK:                 true,
		HelperPresent:              true,
		HelperPath:                 "/usr/lib/bunker/pam-tmp-guard",
		HelperIntegrityOK:          true,
		HelperOwnerOK:              true,
		HelperModeOK:               true,
		HelperManifestPresent:      true,
		HelperManifestOwnerOK:      true,
		HelperManifestModeOK:       true,
		HelperDir:                  "/usr/lib/bunker",
		HelperDirPresent:           true,
		HelperDirOwnerOK:           true,
		HelperDirModeOK:            true,
		PAMBlockPresent:            true,
		SSHDConfigPath:             "/etc/pam.d/sshd",
		AgentGroup:                 DefaultAgentGroup,
		DeployedVerifyGroup:        DefaultAgentGroup,
		AgentGroupPresent:          true,
		InstanceRoot:               DefaultTmpInstanceRoot,
		InstanceRootPresent:        true,
		InstanceRootModeOK:         true,
		InstanceRootOwnerOK:        true,
		OwnershipVerifiable:        true,
	}
	if !healthy.evaluateActive() {
		t.Fatal("the fully observed reference state must evaluate active")
	}

	for _, tc := range []struct {
		name string
		brk  func(*TmpNamespaceState)
	}{
		{"pam_namespace missing", func(st *TmpNamespaceState) { st.ModulePresent = false }},
		{"classifier missing", func(st *TmpNamespaceState) { st.GuardModulePresent = false }},
		{"classifier cannot express the agent pattern", func(st *TmpNamespaceState) { st.GuardModuleSupportsPattern = false }},
		{"pam_exec missing", func(st *TmpNamespaceState) { st.ExecModulePresent = false }},
		{"drop-in missing", func(st *TmpNamespaceState) { st.ConfPresent = false }},
		{"drop-in is not the required rule", func(st *TmpNamespaceState) { st.ConfRuleOK = false }},
		{"drop-in not root-owned", func(st *TmpNamespaceState) { st.ConfOwnerOK = false }},
		{"drop-in group/world writable", func(st *TmpNamespaceState) { st.ConfModeOK = false }},
		{"helper missing", func(st *TmpNamespaceState) { st.HelperPresent = false }},
		{"helper drifted from its manifest", func(st *TmpNamespaceState) { st.HelperIntegrityOK = false }},
		{"helper not root-owned", func(st *TmpNamespaceState) { st.HelperOwnerOK = false }},
		{"helper group/world writable", func(st *TmpNamespaceState) { st.HelperModeOK = false }},
		{"manifest missing", func(st *TmpNamespaceState) { st.HelperManifestPresent = false }},
		{"manifest not root-owned", func(st *TmpNamespaceState) { st.HelperManifestOwnerOK = false }},
		{"manifest group/world writable", func(st *TmpNamespaceState) { st.HelperManifestModeOK = false }},
		{"helper directory missing", func(st *TmpNamespaceState) { st.HelperDirPresent = false }},
		{"helper directory not root-owned", func(st *TmpNamespaceState) { st.HelperDirOwnerOK = false }},
		{"helper directory group/world writable", func(st *TmpNamespaceState) { st.HelperDirModeOK = false }},
		{"PAM block missing", func(st *TmpNamespaceState) { st.PAMBlockPresent = false }},
		{"agent group missing", func(st *TmpNamespaceState) { st.AgentGroupPresent = false }},
		{"deployed block names another group", func(st *TmpNamespaceState) { st.DeployedVerifyGroup = "other-group" }},
		{"instance parent missing", func(st *TmpNamespaceState) { st.InstanceRootPresent = false }},
		{"instance parent mode is not 0000", func(st *TmpNamespaceState) { st.InstanceRootModeOK = false }},
		{"instance parent not root-owned", func(st *TmpNamespaceState) { st.InstanceRootOwnerOK = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := healthy
			tc.brk(&st)
			if st.evaluateActive() {
				t.Errorf("%s is ignored by Active: %+v", tc.name, st)
			}
		})
	}
}

// TestActiveRequiresOwnershipWhenVerifiable pins the ownership rule itself: root
// ownership is required when the probe can observe it (root), and is reported
// as unverifiable rather than as passing when it cannot (the helper treats it
// the same way: `if [ "$(id -u)" = 0 ]`).
func TestActiveRequiresOwnershipWhenVerifiable(t *testing.T) {
	if ownerTrusted("0:0") != true {
		t.Error("root ownership must be trusted")
	}
	if ownerTrusted("") != false {
		t.Error("an unreadable owner must never be trusted")
	}
	if privileged() {
		if ownerTrusted("1000:0") {
			t.Error("ownership must not be trusted when this process can verify it")
		}
	} else if !ownerTrusted("1000:0") {
		t.Error("ownership must not be reported as failed when this process cannot observe it")
	}
	if !modeNotGroupOrWorldWritable(0o755) || modeNotGroupOrWorldWritable(0o775) || modeNotGroupOrWorldWritable(0o757) {
		t.Error("modeNotGroupOrWorldWritable does not model group/world write bits")
	}
}

// TestEnsurePamHelperRepairsTheTrustChain drives the INSTALLER against a hostile
// pre-existing state: the directory the helper and its manifest live in is the
// first link of the chain the runtime helper verifies, so a group/world-writable
// /usr/lib/bunker must be repaired BEFORE those files are written — otherwise a
// local agent can replace both root-owned files by rename/unlink.
func TestEnsurePamHelperRepairsTheTrustChain(t *testing.T) {
	t.Run("a pre-existing group-writable helper directory is repaired", func(t *testing.T) {
		rec := newRecorder(t)
		o, _ := sshSandbox(t, rec, samplePAM)
		if err := os.MkdirAll(o.PamHelperDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(o.PamHelperDir, os.ModeSetgid|0o777); err != nil {
			t.Fatal(err)
		}

		rep, err := o.EnsureTmpNamespace(context.Background())
		if err != nil {
			t.Fatalf("EnsureTmpNamespace() error = %v", err)
		}
		fi, err := os.Stat(o.PamHelperDir)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("helper directory still group/world writable after apply: mode %04o", fi.Mode().Perm())
		}
		repaired := false
		for _, c := range rep.Changes {
			if c.Action == "chmod" && c.Target == o.PamHelperDir {
				repaired = true
			}
		}
		if !repaired {
			t.Errorf("the report does not mention the helper-directory repair: %s", rep)
		}
	})

	t.Run("verifyPamHelperChain refuses a writable or missing link", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			mutate  func(t *testing.T, o *Options)
			wantErr string
		}{
			{name: "intact chain is accepted"},
			{
				name:    "writable helper directory",
				mutate:  func(t *testing.T, o *Options) { mustChmod(t, o.PamHelperDir, 0o777) },
				wantErr: "helper directory",
			},
			{
				name:    "writable manifest",
				mutate:  func(t *testing.T, o *Options) { mustChmod(t, o.PamHelperManifestPath(), 0o666) },
				wantErr: "helper manifest",
			},
			{
				name:    "writable helper",
				mutate:  func(t *testing.T, o *Options) { mustChmod(t, o.PamHelperPath(), 0o777) },
				wantErr: "pam_exec precondition helper",
			},
			{
				name:    "missing manifest",
				mutate:  func(t *testing.T, o *Options) { mustRemove(t, o.PamHelperManifestPath()) },
				wantErr: "helper manifest",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := newRecorder(t)
				o, _ := sshSandbox(t, rec, samplePAM)
				if _, err := o.EnsureTmpNamespace(context.Background()); err != nil {
					t.Fatal(err)
				}
				if tc.mutate != nil {
					tc.mutate(t, &o)
				}
				err := o.verifyPamHelperChain()
				if tc.wantErr == "" {
					if err != nil {
						t.Fatalf("verifyPamHelperChain() = %v, want nil", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("verifyPamHelperChain() accepted a broken chain (%s)", tc.name)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want it to name %q", err, tc.wantErr)
				}
			})
		}
	})
}

// TestDropInDeletionAloneDoesNotRestoreSharedTmp pins the corrected statement in
// NamespaceConf: the drop-in is what the pam_exec precondition requires, so
// deleting that file alone leaves the managed PAM block installed and every
// `bunker-*` session is DENIED — it does NOT fall back to the shared host /tmp.
// Only the full uninstall (block removed first) restores the previous behavior.
func TestDropInDeletionAloneDoesNotRestoreSharedTmp(t *testing.T) {
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, samplePAM)
	if _, err := o.EnsureTmpNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(o.NamespaceConfPath()); err != nil {
		t.Fatal(err)
	}

	// The block — and therefore the verifier that denies — is still installed.
	body := readFileString(t, o.SSHDConfigPath)
	if !o.pamBlockIntact(splitLines(body)) {
		t.Fatalf("the managed block vanished with the drop-in:\n%s", body)
	}
	st, err := o.TmpNamespaceStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Active {
		t.Errorf("status still reports isolation with the drop-in deleted: %+v", st)
	}
	if st.ConfRuleOK || st.ConfRuleDetail == "" {
		t.Errorf("the deleted drop-in must be reported as a rule failure, got ruleOK=%v detail=%q", st.ConfRuleOK, st.ConfRuleDetail)
	}

	// The full uninstall is what restores the shared-/tmp behavior.
	if _, err := o.RemoveTmpNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, o.SSHDConfigPath); got != samplePAM {
		t.Errorf("full uninstall did not restore the file:\n%q\nwant\n%q", got, samplePAM)
	}
}

func TestEnsureTmpNamespace(t *testing.T) {
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, samplePAM)

	rep, err := o.EnsureTmpNamespace(context.Background())
	if err != nil {
		t.Fatalf("EnsureTmpNamespace() error = %v", err)
	}
	if !rep.Has("write") {
		t.Errorf("report has no write: %s", rep)
	}

	conf, err := os.ReadFile(o.NamespaceConfPath())
	if err != nil {
		t.Fatalf("namespace drop-in not written: %v", err)
	}
	if string(conf) != NamespaceConf(o.TmpInstanceRoot) {
		t.Errorf("drop-in content = %q, want %q", conf, NamespaceConf(o.TmpInstanceRoot))
	}

	pam, err := os.ReadFile(o.SSHDConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range o.NamespacePAMBlockForOptions() {
		if !strings.Contains(string(pam), want) {
			t.Errorf("sshd PAM stack missing %q:\n%s", want, pam)
		}
	}
	// The block must be present, adjacent and in order: the classifier's
	// control field jumps over exactly the two modules that follow it, and the
	// verifier must run immediately before pam_namespace.
	if !o.pamBlockIntact(splitLines(string(pam))) {
		t.Errorf("sshd PAM block is not intact (marker + classifier + verifier + module):\n%s", pam)
	}
	// The precondition helper and its manifest are managed files, installed
	// BEFORE the block that invokes them.
	if fi, err := os.Stat(o.PamHelperPath()); err != nil {
		t.Errorf("pam_exec precondition helper not installed: %v", err)
	} else if fi.Mode().Perm() != 0o755 {
		t.Errorf("helper mode = %04o, want 0755", fi.Mode().Perm())
	}
	if body, err := os.ReadFile(o.PamHelperManifestPath()); err != nil {
		t.Errorf("helper manifest not installed: %v", err)
	} else if got, want := string(body), PamGuardManifest(readFileString(t, o.PamHelperPath())); got != want {
		t.Errorf("manifest = %q, want %q", got, want)
	}
	if !strings.HasPrefix(string(pam), samplePAM) {
		t.Errorf("existing PAM rules were modified:\n%s", pam)
	}

	backup, err := os.ReadFile(o.SSHDConfigPath + ".bunker-backup")
	if err != nil {
		t.Fatalf("backup not written: %v", err)
	}
	if string(backup) != samplePAM {
		t.Errorf("backup = %q, want the original file", backup)
	}

	if fi, err := os.Stat(o.TmpInstanceRoot); err != nil {
		t.Fatalf("instance parent missing: %v", err)
	} else if fi.Mode().Perm() != 0o000 {
		t.Errorf("instance parent mode = %04o, want 0000 (pam_namespace requirement)", fi.Mode().Perm())
	}

	st, err := o.TmpNamespaceStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Active {
		t.Errorf("status after install is not active: %+v", st)
	}

	// Idempotency: a second install rewrites nothing.
	before := string(pam)
	rec.calls = nil
	rep2, err := o.EnsureTmpNamespace(context.Background())
	if err != nil {
		t.Fatalf("second EnsureTmpNamespace() error = %v", err)
	}
	after, _ := os.ReadFile(o.SSHDConfigPath)
	if string(after) != before {
		t.Errorf("second install changed the PAM stack:\n%s", after)
	}
	for _, c := range rep2.Mutations() {
		if c.Action == "write" {
			t.Errorf("second install performed a file write: %+v", c)
		}
	}
}

func TestEnsureTmpNamespace_ModuleMissingFailsLoud(t *testing.T) {
	rec := newRecorder(t)
	// A sandbox with the PAM stack but no pam_namespace module.
	root := rec.root
	o := Options{Runner: rec.run, Root: root}.WithDefaults()
	if err := os.MkdirAll(filepath.Dir(o.SSHDConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.SSHDConfigPath, []byte(samplePAM), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := o.EnsureTmpNamespace(context.Background())
	if err == nil {
		t.Fatal("expected an error when pam_namespace.so is not installed")
	}
	if !strings.Contains(err.Error(), "pam_namespace.so not found") {
		t.Errorf("error = %v, want it to name the missing module", err)
	}
	pam, _ := os.ReadFile(o.SSHDConfigPath)
	if string(pam) != samplePAM {
		t.Errorf("failed install modified the PAM stack:\n%s", pam)
	}
	if _, statErr := os.Stat(o.NamespaceConfPath()); !os.IsNotExist(statErr) {
		t.Errorf("failed install wrote a namespace drop-in: %v", statErr)
	}
}

// TestRemoveTmpNamespace_RestoresOriginalBytes pins the reversibility of the
// host edit: installing and then uninstalling must leave the sshd PAM stack
// byte-identical to what it was.
func TestRemoveTmpNamespace_RestoresOriginalBytes(t *testing.T) {
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, samplePAM)

	if _, err := o.EnsureTmpNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep, err := o.RemoveTmpNamespace(context.Background())
	if err != nil {
		t.Fatalf("RemoveTmpNamespace() error = %v", err)
	}
	if !rep.Has("remove") {
		t.Errorf("report has no removal: %s", rep)
	}
	pam, err := os.ReadFile(o.SSHDConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(pam) != samplePAM {
		t.Errorf("PAM stack after uninstall:\n%q\nwant:\n%q", pam, samplePAM)
	}
	if _, err := os.Stat(o.NamespaceConfPath()); !os.IsNotExist(err) {
		t.Errorf("namespace drop-in survived uninstall: %v", err)
	}

	// Uninstalling again (or without ever installing) is a no-op, not an error.
	if _, err := o.RemoveTmpNamespace(context.Background()); err != nil {
		t.Fatalf("second RemoveTmpNamespace() error = %v", err)
	}
}

func TestAgentTmpInstanceLifecycle(t *testing.T) {
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, samplePAM)

	rep, err := o.EnsureAgentTmpInstance(context.Background(), "agent-a", "bunker-agent-a", 1001, 1002)
	if err != nil {
		t.Fatalf("EnsureAgentTmpInstance() error = %v", err)
	}
	if !rep.Has("ok") {
		t.Errorf("report = %s", rep)
	}
	dir := o.TmpInstanceDir("agent-a")
	if want := filepath.Join(o.TmpInstanceRoot, "bunker-agent-a"); dir != want {
		t.Errorf("instance dir = %q, want %q", dir, want)
	}
	if !rec.ran("chown 1001:1002 " + dir) {
		t.Errorf("instance dir ownership not set (calls: %s)", rec)
	}
	if !rec.ran("chmod 0700 " + dir) {
		t.Errorf("instance dir mode not set (calls: %s)", rec)
	}
	// pam_namespace requires the instance parent to be mode 0000, which only
	// root can traverse: a non-root test relaxes it before inspecting and
	// removing, the same way the daemon (running as root) need not.
	if os.Geteuid() != 0 {
		if err := os.Chmod(o.TmpInstanceRoot, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("instance dir not created: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("instance dir mode = %04o, want 0700", fi.Mode().Perm())
	}

	if _, err := o.RemoveAgentTmpInstance(context.Background(), "agent-a"); err != nil {
		t.Fatalf("RemoveAgentTmpInstance() error = %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("instance dir survived removal: %v", err)
	}
	// Idempotent removal.
	if _, err := o.RemoveAgentTmpInstance(context.Background(), "agent-a"); err != nil {
		t.Fatalf("second RemoveAgentTmpInstance() error = %v", err)
	}
}

// TestApplyAndStatus covers the operator entry points: a dry run reports the
// plan and mutates nothing, an apply provisions everything, and Status then
// answers the isolation question with a single boolean.
func TestApplyAndStatus(t *testing.T) {
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, samplePAM)

	dry, err := o.Apply(context.Background(), false)
	if err != nil {
		t.Fatalf("Apply(dry) error = %v", err)
	}
	if m := dry.Mutations(); len(m) != 0 {
		t.Errorf("dry run mutated host state: %+v", m)
	}
	if mutating := rec.mutating(); len(mutating) != 0 {
		t.Errorf("dry run issued mutating commands: %v", mutating)
	}
	if _, err := os.Stat(o.NamespaceConfPath()); !os.IsNotExist(err) {
		t.Errorf("dry run wrote the namespace drop-in")
	}

	applied, err := o.Apply(context.Background(), true)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(applied.Mutations()) == 0 {
		t.Errorf("apply reported no changes: %s", applied)
	}

	st, err := o.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !st.Isolated() {
		t.Errorf("Status().Isolated() = false after a successful apply:\n%s", st)
	}
	if !st.AgentGroupPresent || !st.ScratchRootPresent {
		t.Errorf("scratch state not reported: %+v", st)
	}
	if !st.HostTmp.DropInPresent {
		t.Errorf("host /tmp drop-in not reported: %+v", st.HostTmp)
	}
	if out := st.String(); !strings.Contains(out, "ACTIVE:                true") {
		t.Errorf("operator report does not state the isolation verdict:\n%s", out)
	}

	// A second apply must be a no-op for every mutating step.
	rep, err := o.Apply(context.Background(), true)
	if err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}
	for _, c := range rep.Mutations() {
		if c.Action == "write" || c.Action == "mount" || c.Action == "remount" || c.Action == "create" {
			t.Errorf("idempotent apply still performed %s on %s", c.Action, c.Target)
		}
	}
}

// TestIsolatedFalseWithoutPAMLine is the negative control for the status
// verdict: everything installed except the sshd line must NOT report isolated.
func TestIsolatedFalseWithoutPAMLine(t *testing.T) {
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, samplePAM)
	if _, err := o.EnsureTmpNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Simulate an operator removing the PAM line (or a host where a package
	// upgrade restored the distribution file).
	if _, err := o.RemoveTmpNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := o.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Isolated() {
		t.Errorf("host reports isolated without the sshd PAM line: %+v", st.TmpNamespace)
	}
}
