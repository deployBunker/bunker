package hostsetup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file proves the fail-closed precondition by RUNNING the real rendered
// helper, not by asserting substrings:
//
//   - every scenario mutates the sandbox the helper reads (group table, drop-in,
//     helper, manifest, instance parent, config set) and then runs
//     /usr/lib/bunker/pam-tmp-guard exactly as pam_exec would;
//   - the exit code is fed through the rendered block's PAM control fields
//     (scoping_test.go), so each case answers "is the SESSION denied and did
//     pam_namespace stay unreached", not merely "did the script return 1".
//
// The group table is driven by stubbed `getent`/`id` binaries that the helper
// resolves through the PATH it gives itself (Options.PamHelperToolPath), which
// is also why those stubs cannot be reached from a hostile inherited PATH.

// stub state environment variables. The group table and the identity of the
// helper process are driven by the scenario, so a test can exercise the
// ownership half of the trust chain (which the helper checks only when pam_exec
// runs it as root) without being root itself.
const (
	stubGroupExists    = "T_GROUP_EXISTS"
	stubUserExists     = "T_USER_EXISTS"
	stubIDGroups       = "T_ID_GROUPS"
	stubFakeUID        = "T_FAKE_UID"
	stubOwnerHelper    = "T_OWNER_HELPER"
	stubOwnerManifest  = "T_OWNER_MANIFEST"
	stubOwnerHelperDir = "T_OWNER_HELPER_DIR"
	stubOwnerInstance  = "T_OWNER_INSTANCE"
	stubOwnerOther     = "T_OWNER_OTHER"
)

// guardSandbox is a fake host with the Bunker boundary already installed.
type guardSandbox struct {
	t    *testing.T
	o    Options
	rec  *recorder
	root string
	stub string
}

// writeExec writes an executable stub/script.
func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// newGuardSandbox builds the fake host, installs the boundary through
// EnsureTmpNamespace, and returns it ready for scenario mutations.
func newGuardSandbox(t *testing.T) *guardSandbox {
	t.Helper()
	rec := newRecorder(t)
	root := rec.root
	o := Options{Runner: rec.run, Root: root}.WithDefaults()

	for _, mod := range []string{"pam_namespace.so", "pam_succeed_if.so", "pam_exec.so"} {
		writeExec(t, filepath.Join(root, "lib/x86_64-linux-gnu/security", mod), classifierModuleStub)
	}
	writeExec(t, o.SSHDConfigPath, samplePAM)

	stub := filepath.Join(root, "test-stubs")
	// getent group <name>: success only while the scenario says the group
	// exists, and it reports the scenario's member list.
	writeExec(t, filepath.Join(stub, "getent"), `#!/bin/sh
case "$1 $2" in
  "group bunker-agents")
    [ "${`+stubGroupExists+`:-1}" = 1 ] || exit 2
    echo "$2:x:1001:${`+stubIDGroups+`:-,}"
    exit 0
    ;;
esac
exit 2
`)
	// id: `id -u` reports the scenario's effective uid (so the helper's
	// privileged ownership checks can be exercised without root), anything
	// else reports the scenario's supplementary group list.
	writeExec(t, filepath.Join(stub, "id"), `#!/bin/sh
if [ "$1" = -u ]; then
    echo "${`+stubFakeUID+`:-$(/usr/bin/id -u)}"
    exit 0
fi
[ "${`+stubUserExists+`:-1}" = 1 ] || exit 1
echo "${`+stubIDGroups+`:-users bunker-agents}"
`)
	// stat -c '%u:%g' <path>: the scenario's owners. `%a` (the instance parent
	// mode) is delegated to the real stat, so the mode half stays a real check.
	writeExec(t, filepath.Join(stub, "stat"), `#!/bin/sh
# The helper only ever asks for `+"`stat -c '<format>' <path>`"+`.
fmt="$2"
path="$3"
case "$fmt" in
  *u:%g*)
    case "$path" in
      *pam-tmp-guard.sha256) echo "${`+stubOwnerManifest+`:-0:0}" ;;
      *pam-tmp-guard) echo "${`+stubOwnerHelper+`:-0:0}" ;;
      */usr/lib/bunker) echo "${`+stubOwnerHelperDir+`:-0:0}" ;;
      *agent-tmp) echo "${`+stubOwnerInstance+`:-0:0}" ;;
      *) echo "${`+stubOwnerOther+`:-0:0}" ;;
    esac
    ;;
  *)
    for p in /usr/bin/stat /bin/stat; do
      [ -x "$p" ] && exec "$p" -c "$fmt" "$path"
    done
    exit 1
    ;;
esac
`)

	o.PamHelperToolPath = stub + ":/usr/bin:/bin"
	o = o.WithDefaults()

	if _, err := o.EnsureTmpNamespace(context.Background()); err != nil {
		t.Fatalf("install the boundary in the sandbox: %v", err)
	}
	if st, err := o.TmpNamespaceStatus(); err != nil || !st.Active {
		t.Fatalf("sandbox is not isolated after install (err=%v, state=%+v)", err, st)
	}
	return &guardSandbox{t: t, o: o, rec: rec, root: root, stub: stub}
}

// scenarioEnv is the environment the helper runs with: healthy by default,
// overridden per scenario.
func (s *guardSandbox) scenarioEnv(extra map[string]string) []string {
	env := map[string]string{
		stubGroupExists: "1",
		stubUserExists:  "1",
		stubIDGroups:    "users " + s.o.AgentGroup,
	}
	for k, v := range extra {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// runGuard runs the real helper the way pam_exec does, with pamUser as the
// authenticated name and groupArg as the group the PAM block passes.
func (s *guardSandbox) runGuard(t *testing.T, pamUser, groupArg string, extra map[string]string) (int, string) {
	t.Helper()
	env := append([]string{"PAM_USER=" + pamUser}, s.scenarioEnv(extra)...)
	cmd := exec.Command("/bin/sh", s.o.PamHelperPath(), "verify", groupArg)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	switch e := err.(type) {
	case nil:
		return 0, string(out)
	case *exec.ExitError:
		return e.ExitCode(), string(out)
	default:
		t.Fatalf("run helper: %v", err)
		return -1, ""
	}
}

// agentSession evaluates the rendered block for an agent session using a REAL
// helper exit code as pam_exec's return code, answering whether the session is
// denied and whether pam_namespace was reached.
func (s *guardSandbox) agentSession(t *testing.T, extra map[string]string) (ran []string, denied bool, code int, output string) {
	t.Helper()
	code, output = s.runGuard(t, "bunker-abc123", s.o.AgentGroup, extra)
	stack := parsePAMStack(NamespacePAMBlock(s.o.PamHelperPath(), s.o.AgentGroup))
	ran, denied = evalPAMStack(stack, func(l pamStackLine) pamReturn {
		switch l.module {
		case "pam_succeed_if.so":
			// pam_succeed_if `user !~ bunker-*` over PAM_USER: an agent name
			// matches the pattern, so the classifier answers PAM_AUTH_ERR.
			return retAuthErr
		case "pam_exec.so":
			if code == 0 {
				return retSuccess
			}
			// pam_exec maps a non-zero exit, a signal, or a failed execve
			// (which also exits non-zero) to PAM_SYSTEM_ERR.
			return retSystemErr
		default:
			return retSuccess
		}
	})
	return ran, denied, code, output
}

// operatorSession evaluates the rendered block for an ordinary non-agent SSH
// user. The classifier's answer comes from fnmatch over the name — no group
// state is consulted — and no Bunker module may run.
func (s *guardSandbox) operatorSession(t *testing.T, name string, extra map[string]string) (ran []string, denied bool, code int) {
	t.Helper()
	code, _ = s.runGuard(t, name, s.o.AgentGroup, extra)
	stack := parsePAMStack(NamespacePAMBlock(s.o.PamHelperPath(), s.o.AgentGroup))
	ran, denied = evalPAMStack(stack, func(l pamStackLine) pamReturn {
		switch l.module {
		case "pam_succeed_if.so":
			// `user !~ bunker-*` succeeds for an ordinary name.
			return retSuccess
		case "pam_exec.so":
			t.Fatalf("pam_exec ran for an ordinary session")
			return retSystemErr
		default:
			t.Fatalf("pam_namespace ran for an ordinary session")
			return retSuccess
		}
	})
	return ran, denied, code
}

// statusWant describes what TmpNamespaceStatus must report for a broken-boundary
// scenario: the same condition that denies a session must also stop the host
// from being advertised as isolated — EXCEPT for conditions that are not
// observable from static host state (a live membership lookup, or ownership the
// probe cannot read because it is not root).
type statusWant int

const (
	// statusNotStatic: not observable from static host state; no verdict.
	statusNotStatic statusWant = iota
	// statusInactive: the static probe must report NOT isolated.
	statusInactive
	// statusActive: the boundary is intact as far as static state shows.
	statusActive
)

// boundaryCase is one broken-boundary scenario. ONE table drives both the
// fail-closed matrix (the real rendered helper, fed through the rendered PAM
// control fields) and the status matrix, so the two cannot drift apart: a
// scenario that denies a `bunker-*` session but is still reported as "isolated"
// fails TestStatusInactiveForEveryStaticallyObservableBrokenBoundary.
type boundaryCase struct {
	name          string
	env           map[string]string
	mutate        func(t *testing.T, s *guardSandbox)
	groupArg      string // defaults to the installed block's group
	wantDenied    bool
	wantNamespace bool
	status        statusWant
}

// boundaryCases returns the matrix. Every way the boundary can be missing,
// wrong, tampered with or REPLACED is a row.
func boundaryCases() []boundaryCase {
	return []boundaryCase{
		{
			name:          "healthy boundary",
			wantNamespace: true,
			status:        statusActive,
		},
		{
			name:          "agent group deleted",
			env:           map[string]string{stubGroupExists: "0"},
			mutate:        func(t *testing.T, s *guardSandbox) { s.rec.groupExists = false },
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			// The membership lookup is a LIVE session answer (agents are
			// dynamic and the group file is not the session's view of it), so
			// there is no static verdict to assert.
			name:          "agent membership removed",
			env:           map[string]string{stubIDGroups: "users"},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusNotStatic,
		},
		{
			name:          "agent user does not exist",
			env:           map[string]string{stubUserExists: "0", stubIDGroups: ""},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusNotStatic,
		},
		{
			name:          "drop-in deleted",
			mutate:        func(t *testing.T, s *guardSandbox) { mustRemove(t, s.o.NamespaceConfPath()) },
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name: "drop-in valid but wrong rule",
			mutate: func(t *testing.T, s *guardSandbox) {
				writeFile(t, s.o.NamespaceConfPath(), "/tmp  /tmp-inst/  user:noinit  root\n")
			},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name: "drop-in rule for the wrong polydir",
			mutate: func(t *testing.T, s *guardSandbox) {
				writeFile(t, s.o.NamespaceConfPath(), "/var/tmp  "+s.o.TmpInstanceRoot+"/  user:noinit  root\n")
			},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name: "drop-in malformed",
			mutate: func(t *testing.T, s *guardSandbox) {
				writeFile(t, s.o.NamespaceConfPath(), "/tmp  "+s.o.TmpInstanceRoot+"/\n")
			},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name: "drop-in carries an extra rule",
			mutate: func(t *testing.T, s *guardSandbox) {
				writeFile(t, s.o.NamespaceConfPath(), NamespaceConfRule(s.o.TmpInstanceRoot)+
					"\n/home  /home-inst/  user:noinit  root\n")
			},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name: "drop-in with equivalent spacing is accepted",
			mutate: func(t *testing.T, s *guardSandbox) {
				rule := strings.Join(strings.Fields(NamespaceConfRule(s.o.TmpInstanceRoot)), " ")
				writeFile(t, s.o.NamespaceConfPath(), "# managed\n"+rule+"\n")
			},
			wantNamespace: true,
			status:        statusActive,
		},
		{
			name:          "drop-in group/world writable",
			mutate:        func(t *testing.T, s *guardSandbox) { mustChmod(t, s.o.NamespaceConfPath(), 0o666) },
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			// The trust chain, link by link: a writable directory or manifest
			// lets an agent replace BOTH root-owned files by rename/unlink.
			name:          "helper directory group/world writable",
			mutate:        func(t *testing.T, s *guardSandbox) { mustChmod(t, s.o.PamHelperDir, 0o777) },
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name:          "helper mode unsafe (group writable)",
			mutate:        func(t *testing.T, s *guardSandbox) { mustChmod(t, s.o.PamHelperPath(), 0o775) },
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name:          "helper manifest group/world writable",
			mutate:        func(t *testing.T, s *guardSandbox) { mustChmod(t, s.o.PamHelperManifestPath(), 0o666) },
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			// Ownership is checked by the helper only when pam_exec runs it as
			// root, so the scenario pretends to be root (T_FAKE_UID) and drives
			// the owners through the stat stub. Not statically observable by a
			// non-root status probe, hence statusNotStatic.
			name:          "helper not root-owned",
			env:           map[string]string{stubFakeUID: "0", stubOwnerHelper: "1000:1000"},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusNotStatic,
		},
		{
			name:          "helper manifest not root-owned",
			env:           map[string]string{stubFakeUID: "0", stubOwnerManifest: "1000:0"},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusNotStatic,
		},
		{
			name:          "helper directory not root-owned",
			env:           map[string]string{stubFakeUID: "0", stubOwnerHelperDir: "1000:0"},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusNotStatic,
		},
		{
			name:          "instance parent not root-owned",
			env:           map[string]string{stubFakeUID: "0", stubOwnerInstance: "1000:0"},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusNotStatic,
		},
		{
			name:          "drop-in not root-owned",
			env:           map[string]string{stubFakeUID: "0", stubOwnerOther: "1000:0"},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusNotStatic,
		},
		{
			name: "helper content drifted",
			mutate: func(t *testing.T, s *guardSandbox) {
				body := readFileString(t, s.o.PamHelperPath())
				writeFile(t, s.o.PamHelperPath(), body+"# tampered\n")
			},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name:          "helper manifest deleted",
			mutate:        func(t *testing.T, s *guardSandbox) { mustRemove(t, s.o.PamHelperManifestPath()) },
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name: "helper manifest disagrees",
			mutate: func(t *testing.T, s *guardSandbox) {
				writeFile(t, s.o.PamHelperManifestPath(), strings.Repeat("0", 64)+"\n")
			},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name: "helper group drifted from the PAM block",
			mutate: func(t *testing.T, s *guardSandbox) {
				writeFile(t, s.o.SSHDConfigPath,
					samplePAM+strings.Join(NamespacePAMBlock(s.o.PamHelperPath(), "other-group"), "\n")+"\n")
			},
			groupArg:      "other-group",
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name:          "instance parent mode loosened",
			mutate:        func(t *testing.T, s *guardSandbox) { mustChmod(t, s.o.TmpInstanceRoot, 0o700) },
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name:          "instance parent deleted",
			mutate:        func(t *testing.T, s *guardSandbox) { mustRemove(t, s.o.TmpInstanceRoot) },
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name: "distribution namespace.conf declares /tmp",
			mutate: func(t *testing.T, s *guardSandbox) {
				writeFile(t, s.o.NamespaceConfFile, "/tmp  /tmp-inst/  user:noinit  root,adm\n")
			},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name: "another drop-in is malformed",
			mutate: func(t *testing.T, s *guardSandbox) {
				writeFile(t, filepath.Join(s.o.NamespaceConfDir, "10-vendor.conf"), "/home  /home-inst/\n")
			},
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
		{
			name:          "pam_namespace module removed",
			mutate:        func(t *testing.T, s *guardSandbox) { mustRemove(t, s.o.PamHelperModulePath) },
			wantDenied:    true,
			wantNamespace: false,
			status:        statusInactive,
		},
	}
}

// TestAgentSessionFailsClosedForEveryBrokenBoundary is the matrix the review
// required: each way the boundary can be missing, wrong or tampered with must
// DENY a `bunker-*` session (and leave pam_namespace unreached), while an
// ordinary operator session still skips the whole block.
func TestAgentSessionFailsClosedForEveryBrokenBoundary(t *testing.T) {
	for _, tc := range boundaryCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := newGuardSandbox(t)
			if tc.mutate != nil {
				tc.mutate(t, s)
			}
			groupArg := tc.groupArg
			if groupArg == "" {
				groupArg = s.o.AgentGroup
			}
			code, output := s.runGuard(t, "bunker-abc123", groupArg, tc.env)
			stack := parsePAMStack(NamespacePAMBlock(s.o.PamHelperPath(), s.o.AgentGroup))
			ran, denied := evalPAMStack(stack, func(l pamStackLine) pamReturn {
				switch l.module {
				case "pam_succeed_if.so":
					// pam_succeed_if `user !~ bunker-*` over PAM_USER: an agent
					// name matches the pattern, so the classifier answers
					// PAM_AUTH_ERR and the session continues to the verifier.
					return retAuthErr
				case "pam_exec.so":
					if code == 0 {
						return retSuccess
					}
					// pam_exec maps a non-zero exit (and a failed execve) to
					// PAM_SYSTEM_ERR.
					return retSystemErr
				default:
					return retSuccess
				}
			})
			if denied != tc.wantDenied {
				t.Errorf("agent session denied = %v, want %v (helper exit %d, output: %s, ran: %v)",
					denied, tc.wantDenied, code, strings.TrimSpace(output), ran)
			}
			if got := ranModule(ran, "pam_namespace.so"); got != tc.wantNamespace {
				t.Errorf("pam_namespace ran = %v, want %v (ran: %v)", got, tc.wantNamespace, ran)
			}

			// The operator path is unaffected by a broken agent boundary: an
			// ordinary SSH user must still skip the entire managed block.
			opRan, opDenied, opCode := s.operatorSession(t, "deploy", tc.env)
			if opDenied {
				t.Errorf("ordinary operator session was denied (helper exit %d, ran: %v)", opCode, opRan)
			}
			if ranModule(opRan, "pam_exec.so") || ranModule(opRan, "pam_namespace.so") {
				t.Errorf("ordinary operator session ran a Bunker module: %v", opRan)
			}
		})
	}
}

// TestStatusInactiveForEveryStaticallyObservableBrokenBoundary is the other
// half of the same table: whenever a broken boundary is observable from the
// static host state, `host-provision --status` must NOT report the host as
// isolated. Before this, Active ignored the helper/manifest/directory
// ownership and modes, the drop-in's content and ownership, and the instance
// parent's owner/mode, so it reported isolated=true for hosts on which every
// agent session was denied (or on which an agent could rewrite the boundary).
func TestStatusInactiveForEveryStaticallyObservableBrokenBoundary(t *testing.T) {
	for _, tc := range boundaryCases() {
		if tc.status == statusNotStatic {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			s := newGuardSandbox(t)
			if tc.mutate != nil {
				tc.mutate(t, s)
			}
			st, err := s.o.TmpNamespaceStatus()
			if err != nil {
				t.Fatalf("TmpNamespaceStatus() error = %v", err)
			}
			want := tc.status == statusActive
			if st.Active != want {
				t.Errorf("Active = %v, want %v for %q\nstate: %+v", st.Active, want, tc.name, st)
			}
			if got := (Status{TmpNamespace: st}).Isolated(); got != want {
				t.Errorf("Isolated() = %v, want %v for %q", got, want, tc.name)
			}
		})
	}
}

// TestHealthyAgentSessionIsIsolated is the positive control for the matrix: an
// intact boundary makes the real helper succeed, so the agent session reaches
// pam_namespace.
func TestHealthyAgentSessionIsIsolated(t *testing.T) {
	s := newGuardSandbox(t)
	ran, denied, code, output := s.agentSession(t, nil)
	if code != 0 {
		t.Fatalf("helper denied a healthy boundary (exit %d): %s", code, output)
	}
	if denied {
		t.Errorf("healthy agent session was denied (ran: %v)", ran)
	}
	if !ranModule(ran, "pam_namespace.so") {
		t.Errorf("healthy agent session did not reach pam_namespace (ran: %v)", ran)
	}
}

// TestHelperDeniesWithoutPAMUser: the helper's own copy of the code is what
// makes it fail closed, so an agent session whose environment carries no
// PAM_USER must be denied rather than verified against an empty name.
func TestHelperDeniesWithoutPAMUser(t *testing.T) {
	s := newGuardSandbox(t)
	cmd := exec.Command("/bin/sh", s.o.PamHelperPath(), "verify", s.o.AgentGroup)
	cmd.Env = s.scenarioEnv(nil) // no PAM_USER at all
	out, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() == 0 {
		t.Fatalf("helper accepted a session with no PAM_USER (err=%v, output: %s)", err, out)
	}
	if !strings.Contains(string(out), "PAM_USER") {
		t.Errorf("helper diagnostic does not mention PAM_USER: %s", out)
	}
	// The ambiguous-identity guard is asserted in TestPamGuardScriptRendering:
	// pam_exec APPENDS PAM_USER to the PAM environment, so a second entry means
	// the name is not the one the PAM item carries. (Go's os/exec de-duplicates
	// the environment it passes, so this case cannot be driven from here.)
}

// TestHelperMissingDeniesAgentSession: pam_exec maps a failed execve to a
// non-zero exit (pam_exec.c execve() then _exit(errno)), i.e. PAM_SYSTEM_ERR,
// which the verifier's `default=die` turns into a denied session. A deleted
// helper therefore fails CLOSED — and an ordinary operator session, jumped over
// the block before it, is unaffected.
func TestHelperMissingDeniesAgentSession(t *testing.T) {
	s := newGuardSandbox(t)
	stack := parsePAMStack(NamespacePAMBlock(s.o.PamHelperPath(), s.o.AgentGroup))
	ran, denied := evalPAMStack(stack, func(l pamStackLine) pamReturn {
		switch l.module {
		case "pam_succeed_if.so":
			return retAuthErr
		case "pam_exec.so":
			// Missing helper: execve fails, pam_exec exits non-zero.
			return retSystemErr
		default:
			return retSuccess
		}
	})
	if !denied || ranModule(ran, "pam_namespace.so") {
		t.Errorf("a missing helper must deny the agent session, got denied=%v ran=%v", denied, ran)
	}
	if err := os.Remove(s.o.PamHelperPath()); err != nil {
		t.Fatal(err)
	}
	if st, err := s.o.TmpNamespaceStatus(); err != nil || st.Active {
		t.Errorf("status must stop reporting isolation without the helper (err=%v, active=%v)", err, st.Active)
	}
}

// TestHelperIgnoresInheritedToolPath: the helper gives itself a fixed PATH, so
// a hostile inherited PATH (pam_exec passes the PAM environment through) cannot
// substitute the tools that decide the boundary.
func TestHelperIgnoresInheritedToolPath(t *testing.T) {
	s := newGuardSandbox(t)
	script := readFileString(t, s.o.PamHelperPath())
	if !strings.Contains(script, "PATH='"+s.o.PamHelperToolPath+"'") {
		t.Fatalf("helper does not pin its own PATH:\n%s", script)
	}

	evil := filepath.Join(s.root, "evil-path")
	// A lying sha256sum: if the helper resolved its tools through the inherited
	// PATH, this would make the drift check pass.
	writeExec(t, filepath.Join(evil, "sha256sum"), "#!/bin/sh\necho deadbeef\n")
	writeExec(t, filepath.Join(evil, "getent"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(evil, "id"), "#!/bin/sh\necho "+DefaultAgentGroup+"\n")

	cmd := exec.Command("/bin/sh", s.o.PamHelperPath(), "verify", s.o.AgentGroup)
	cmd.Env = append([]string{"PAM_USER=bunker-abc123", "PATH=" + evil}, s.scenarioEnv(nil)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper must ignore the inherited PATH, got %v (output: %s)", err, out)
	}
}

// TestEnsureTmpNamespace_ClassifierWithoutPatternSupportFailsLoud: a
// pam_succeed_if that predates the glob/noglob test cannot express
// `user !~ bunker-*`; the block's default=die would then deny EVERY session on
// the host (root included), so the installer must refuse BEFORE writing
// anything rather than install a lockout.
func TestEnsureTmpNamespace_ClassifierWithoutPatternSupportFailsLoud(t *testing.T) {
	rec := newRecorder(t)
	root := rec.root
	o := Options{Runner: rec.run, Root: root}.WithDefaults()
	if err := os.MkdirAll(filepath.Dir(o.SSHDConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.SSHDConfigPath, []byte(samplePAM), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mod := range []string{"pam_namespace.so", "pam_exec.so"} {
		writeExec(t, filepath.Join(root, "lib/x86_64-linux-gnu/security", mod), "ELF")
	}
	// The classifier exists but carries neither token: a Linux-PAM build
	// without the glob test.
	writeExec(t, filepath.Join(root, "lib/x86_64-linux-gnu/security", "pam_succeed_if.so"), "ELF strcmp ingroup notingroup")

	_, err := o.EnsureTmpNamespace(context.Background())
	if err == nil {
		t.Fatal("expected an error when pam_succeed_if cannot express the agent-name pattern")
	}
	if !strings.Contains(err.Error(), "agent-name pattern") {
		t.Errorf("error = %v, want it to name the pattern requirement", err)
	}
	if pam := readFileString(t, o.SSHDConfigPath); pam != samplePAM {
		t.Errorf("refused install modified the PAM stack:\n%s", pam)
	}
	for _, path := range []string{o.NamespaceConfPath(), o.PamHelperPath(), o.PamHelperManifestPath()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("refused install wrote %s", path)
		}
	}
	st, err := o.TmpNamespaceStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.GuardModuleSupportsPattern || st.Active {
		t.Errorf("status must report the missing classifier capability (state=%+v)", st)
	}
	if out := (Status{TmpNamespace: st}).String(); !strings.Contains(out, "classifier pattern ok: false") ||
		!strings.Contains(out, "1.6") {
		t.Errorf("operator report hides the stale classifier:\n%s", out)
	}
}

// TestPamGuardScriptRendering pins the helper's shape: the values it embeds,
// the unquoted agent pattern in its case statement, and the absence of any
// fail-open switch.
func TestPamGuardScriptRendering(t *testing.T) {
	o := Options{}.WithDefaults()
	o.PamHelperModulePath = "/lib/security/pam_namespace.so"
	script, err := o.PamGuardScript()
	if err != nil {
		t.Fatalf("PamGuardScript() error = %v", err)
	}
	for _, want := range []string{
		"PATH='" + DefaultPamHelperToolPath + "'",
		"GROUP='" + DefaultAgentGroup + "'",
		"DROPIN='" + o.NamespaceConfPath() + "'",
		"REQUIRED_RULE='/tmp /var/lib/bunkerd/agent-tmp/ user:noinit root'",
		"INSTANCE_ROOT='" + o.TmpInstanceRoot + "'",
		"MANIFEST='" + o.PamHelperManifestPath() + "'",
		// The trust chain the helper verifies: directory -> manifest -> helper.
		"HELPER_DIR='" + o.PamHelperDir + "'",
		`find "$HELPER_DIR" -maxdepth 0 -perm /022`,
		`find "$MANIFEST" -perm /022`,
		`helper manifest $MANIFEST is not root-owned`,
		`helper directory $HELPER_DIR is not root-owned`,
		"MODULE='/lib/security/pam_namespace.so'",
		"case $pam_user in\n    bunker-*) : ;;\n    *) exit 0 ;;\nesac",
		"sha256sum \"$HELPER\"",
		"getent group \"$GROUP\"",
		// The ambiguous-identity guard (see TestHelperDeniesWithoutPAMUser).
		"'^PAM_USER='",
		"find \"$HELPER\" -perm /022",
		"find \"$DROPIN\" -perm /022",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("rendered helper is missing %q", want)
		}
	}
	if strings.Contains(script, "ignore_config_error") {
		t.Error("rendered helper mentions the fail-open option")
	}
	if strings.Contains(script, "@AGENT_GROUP@") || strings.Contains(script, "@TOOL_PATH@") {
		t.Error("rendered helper still contains an unsubstituted token")
	}
	// The manifest must hash exactly the bytes the installer writes.
	if got, want := PamGuardManifest(script), PamGuardScriptHash(script)+"\n"; got != want {
		t.Errorf("manifest = %q, want %q", got, want)
	}
}

// TestTmpNamespaceInputValidation: every configurable value that ends up in a
// PAM line, a config file or the helper script is rejected when it could inject
// content — and nothing is written when it is.
func TestTmpNamespaceInputValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		group string
		extra func(*Options)
	}{
		{name: "newline injection into the PAM block", group: DefaultAgentGroup + "\nsession required pam_permit.so"},
		{name: "space injection", group: "bunker agents"},
		{name: "PAM comment injection", group: "#bunker-agents"},
		{name: "leading dash", group: "-bunker-agents"},
		{name: "colon (PAM group list)", group: "bunker-agents:root"},
		{name: "shell metacharacter", group: "bunker-agents;id"},
		{name: "glob", group: "bunker-*"},
		{name: "too long", group: strings.Repeat("a", 33)},
		{
			name:  "instance root path injection",
			group: DefaultAgentGroup,
			extra: func(o *Options) { o.TmpInstanceRoot = "/var/lib/bunkerd/agent-tmp\nrule" },
		},
		{
			name:  "instance root with a quote",
			group: DefaultAgentGroup,
			extra: func(o *Options) { o.TmpInstanceRoot = "/var/lib/bunkerd/agent-tmp'" },
		},
		{
			name:  "relative instance root",
			group: DefaultAgentGroup,
			extra: func(o *Options) { o.TmpInstanceRoot = "var/lib/agent-tmp" },
		},
		{
			name:  "tool path with a non-absolute entry",
			group: DefaultAgentGroup,
			extra: func(o *Options) { o.PamHelperToolPath = "bin:/usr/bin" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder(t)
			o := Options{Runner: rec.run, Root: rec.root}.WithDefaults()
			for _, mod := range []string{"pam_namespace.so", "pam_succeed_if.so", "pam_exec.so"} {
				writeExec(t, filepath.Join(rec.root, "lib/x86_64-linux-gnu/security", mod), classifierModuleStub)
			}
			writeExec(t, o.SSHDConfigPath, samplePAM)
			if tc.group != "" || tc.name == "empty" {
				o.AgentGroup = tc.group
			}
			if tc.extra != nil {
				tc.extra(&o)
			}
			o = o.WithDefaults()

			if _, err := o.EnsureTmpNamespace(context.Background()); err == nil {
				t.Fatal("expected the installer to reject an unsafe value")
			}
			if pam := readFileString(t, o.SSHDConfigPath); pam != samplePAM {
				t.Errorf("unsafe value reached the PAM stack:\n%s", pam)
			}
			for _, path := range []string{o.NamespaceConfPath(), o.PamHelperPath(), o.PamHelperManifestPath()} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("unsafe value caused a write to %s", path)
				}
			}
		})
	}
}

// TestValidateHelpers documents the accepted shapes directly.
func TestValidateHelpers(t *testing.T) {
	for _, ok := range []string{"bunker-agents", "bunker_agents", "a1", "_x", "Bunker-Agents"} {
		if err := ValidateAgentGroup(ok); err != nil {
			t.Errorf("ValidateAgentGroup(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", " bunker", "bunker ", "bunker\nagents", "-b", "b:c", "b/c", "b*", "b$c", "b`c`"} {
		if err := ValidateAgentGroup(bad); err == nil {
			t.Errorf("ValidateAgentGroup(%q) = nil, want an error", bad)
		}
	}
	for _, ok := range []string{"/srv/bunker-share", "/var/lib/bunkerd/agent-tmp", "/etc/pam.d/sshd", "/a-b_c.d/e"} {
		if err := ValidateSystemPath("test", ok); err != nil {
			t.Errorf("ValidateSystemPath(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "relative/path", "/tmp/x y", "/tmp/x'y", "/tmp/x\"y", "/tmp/x\ty", "/tmp/../etc", "/tmp//x", "/tmp/x$y", "/tmp/x`id`", "/tmp/*"} {
		if err := ValidateSystemPath("test", bad); err == nil {
			t.Errorf("ValidateSystemPath(%q) = nil, want an error", bad)
		}
	}
}

// mustRemove removes a path or fails the test.
func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

// mustChmod changes a path's mode or fails the test.
func mustChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// writeFile writes a file or fails the test. A managed file may already exist
// with a restrictive mode (the manifest is 0444), so relax the mode first.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readFileString reads a config file the provisioners wrote.
func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
