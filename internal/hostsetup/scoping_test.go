package hostsetup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the semantics the ADVERSARIAL REVIEW of the second GAP-075
// revision required, against Linux-PAM's own sources (linux-pam v1.7.0, the
// version this fleet runs):
//
//  1. The agent question must be answered INDEPENDENTLY OF GROUP STATE, from
//     the reserved username pattern. pam_succeed_if's `user !~ <glob>` is
//     evaluate_noglob() -> fnmatch(3) over the PAM_USER item
//     (pam_succeed_if.c), so a deleted group cannot make an agent session look
//     like an operator session. The first revision used `user ingroup <group>`,
//     whose evaluate_ingroup() reports PAM_AUTH_ERR BOTH for a non-member and
//     for a group that does not exist — and its control field jumped those
//     sessions over pam_namespace, i.e. it failed OPEN exactly when the host
//     had lost its agent group.
//
//  2. A missing or wrong pam_namespace rule must DENY an agent session.
//     pam_namespace returns PAM_SUCCESS with no matching polydir
//     (pam_namespace.c: pam_sm_open_session() only calls setup_namespace() when
//     a polydir matched), so the module alone cannot be the precondition; the
//     pam_exec verifier must deny first.
//
//  3. An ordinary non-agent SSH user must skip the ENTIRE managed block, and a
//     pam_namespace rule the operator or the distribution owns must never be
//     touched (see pamownership_test.go).
//
// The model below evaluates PAM control fields exactly as pam.conf(5) defines
// them, and the fail-closed matrix in pamguard_test.go drives the REAL
// rendered helper for its pam_exec return codes, so the semantics are pinned
// against the artifact that ships rather than against a description of it. The
// live proof (real sshd + PAM, real agent sessions) remains the GAP-075 section
// of e2e-full-battery.sh on bunker-mvp.

// ---------------------------------------------------------------------------
// A model of PAM control-field evaluation.
// ---------------------------------------------------------------------------

// pamReturn is a PAM return code (only the ones this boundary can see).
type pamReturn string

const (
	retSuccess     pamReturn = "PAM_SUCCESS"
	retAuthErr     pamReturn = "PAM_AUTH_ERR"
	retServiceErr  pamReturn = "PAM_SERVICE_ERR"
	retSystemErr   pamReturn = "PAM_SYSTEM_ERR"
	retBufErr      pamReturn = "PAM_BUF_ERR"
	retUserUnknown pamReturn = "PAM_USER_UNKNOWN"
)

// retToken maps a return code to its control-field token.
var retToken = map[pamReturn]string{
	retSuccess:     "success",
	retAuthErr:     "auth_err",
	retServiceErr:  "service_err",
	retSystemErr:   "system_err",
	retBufErr:      "buf_err",
	retUserUnknown: "user_unknown",
}

// pamAction is what PAM does after a module returns.
type pamAction struct {
	kind    string // ok | done | ignore | bad | die | jump
	jumpLen int
}

// pamStackLine is one parsed PAM session line.
type pamStackLine struct {
	raw     string
	control string
	module  string
	args    []string
}

// parsePAMStack parses the rendered block into stack lines (comments skipped).
func parsePAMStack(block []string) []pamStackLine {
	var out []pamStackLine
	for _, raw := range block {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 3 {
			continue
		}
		line := pamStackLine{raw: raw, control: fields[1], module: fields[2], args: fields[3:]}
		// A bracketed control field is returned by Fields() as ONE token only
		// when it has no spaces; with spaces it arrives as "[success=2",
		// "auth_err=ignore", "default=die]". Re-join it.
		if strings.HasPrefix(fields[1], "[") && !strings.HasSuffix(fields[1], "]") {
			for i := 2; i < len(fields); i++ {
				line.control += " " + fields[i]
				if strings.HasSuffix(fields[i], "]") {
					line.module = fields[i+1]
					line.args = fields[i+2:]
					break
				}
			}
		}
		out = append(out, line)
	}
	return out
}

// action resolves the control field for a module return code.
func (l pamStackLine) action(code pamReturn) pamAction {
	if !strings.HasPrefix(l.control, "[") {
		switch l.control {
		case "required":
			if code == retSuccess {
				return pamAction{kind: "ok"}
			}
			return pamAction{kind: "bad"}
		case "requisite":
			if code == retSuccess {
				return pamAction{kind: "ok"}
			}
			return pamAction{kind: "die"}
		case "optional":
			return pamAction{kind: "ignore"}
		case "sufficient":
			if code == retSuccess {
				return pamAction{kind: "done"}
			}
			return pamAction{kind: "ignore"}
		default:
			return pamAction{kind: "bad"}
		}
	}
	mapping := map[string]func() pamAction{}
	body := strings.TrimSuffix(strings.TrimPrefix(strings.Join(strings.Fields(l.control), " "), "["), "]")
	for _, pair := range strings.Fields(body) {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		mapping[kv[0]] = actionFor(kv[1])
	}
	if fn, ok := mapping[retToken[code]]; ok && fn != nil {
		return fn()
	}
	if fn, ok := mapping["default"]; ok && fn != nil {
		return fn()
	}
	// PAM's documented default when a return value is not listed: bad.
	return pamAction{kind: "bad"}
}

// actionFor builds an action from a control-field value.
func actionFor(value string) func() pamAction {
	return func() pamAction {
		switch value {
		case "ok":
			return pamAction{kind: "ok"}
		case "done":
			return pamAction{kind: "done"}
		case "ignore":
			return pamAction{kind: "ignore"}
		case "bad":
			return pamAction{kind: "bad"}
		case "die":
			return pamAction{kind: "die"}
		case "reset":
			return pamAction{kind: "ok"}
		}
		n := 0
		for _, r := range value {
			if r < '0' || r > '9' {
				return pamAction{kind: "bad"}
			}
			n = n*10 + int(r-'0')
		}
		return pamAction{kind: "jump", jumpLen: n}
	}
}

// evalPAMStack walks the stack, recording which modules RAN and whether the
// session was denied. codeFor supplies each module's return code.
func evalPAMStack(stack []pamStackLine, codeFor func(pamStackLine) pamReturn) (ran []string, denied bool) {
	for i := 0; i < len(stack); {
		line := stack[i]
		code := codeFor(line)
		ran = append(ran, line.module)
		switch act := line.action(code); act.kind {
		case "die":
			return ran, true
		case "bad":
			denied = true
			i++
		case "done":
			return ran, denied
		case "jump":
			i += act.jumpLen + 1
		default: // ok, ignore
			i++
		}
	}
	return ran, denied
}

// ranModule reports whether a module executed.
func ranModule(ran []string, module string) bool {
	for _, m := range ran {
		if m == module {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Pins on the rendered block.
// ---------------------------------------------------------------------------

// TestNamespaceBlockShapeAndJumpArithmetic pins the three module lines, their
// control fields, and the fact that the classifier's jump length is exactly the
// number of managed modules that follow it. Editing the block without keeping
// the arithmetic consistent fails here, not on a live host.
func TestNamespaceBlockShapeAndJumpArithmetic(t *testing.T) {
	helper := "/usr/lib/bunker/pam-tmp-guard"
	block := NamespacePAMBlock(helper, DefaultAgentGroup)
	stack := parsePAMStack(block)
	if len(stack) != 3 {
		t.Fatalf("rendered block has %d module lines, want 3: %v", len(stack), block)
	}
	classifier, verifier, module := stack[0], stack[1], stack[2]

	if classifier.module != "pam_succeed_if.so" {
		t.Fatalf("first module = %q, want the pam_succeed_if classifier", classifier.module)
	}
	if classifier.control != "[success=2 auth_err=ignore default=die]" {
		t.Errorf("classifier control = %q, want the pattern classifier's control field", classifier.control)
	}
	if got := strings.Join(classifier.args, " "); got != "quiet user !~ "+NamespacePAMAgentPattern {
		t.Errorf("classifier args = %q, want the reserved username pattern test", got)
	}
	if classifier.action(retSuccess).kind != "jump" || classifier.action(retSuccess).jumpLen != 2 {
		t.Errorf("non-agent answer must jump over 2 modules, got %+v", classifier.action(retSuccess))
	}
	if classifier.action(retAuthErr).kind != "ignore" {
		t.Errorf("agent answer must continue to the verifier, got %+v", classifier.action(retAuthErr))
	}
	for _, unanswerable := range []pamReturn{retServiceErr, retBufErr, retUserUnknown, retSystemErr} {
		if got := classifier.action(unanswerable).kind; got != "die" {
			t.Errorf("unanswerable classifier result %s = %q, want die (fail closed)", unanswerable, got)
		}
	}

	if verifier.module != "pam_exec.so" {
		t.Fatalf("second module = %q, want pam_exec.so", verifier.module)
	}
	if verifier.control != "[success=ignore default=die]" {
		t.Errorf("verifier control = %q, want the fail-closed precondition's control field", verifier.control)
	}
	if verifier.args[len(verifier.args)-1] != DefaultAgentGroup {
		t.Errorf("verifier args = %v, want the group as its last argument", verifier.args)
	}
	if verifier.action(retSuccess).kind != "ignore" {
		t.Errorf("verified agent session must continue, got %+v", verifier.action(retSuccess))
	}
	if got := verifier.action(retSystemErr).kind; got != "die" {
		t.Errorf("helper failure = %q, want die (deny the session)", got)
	}

	if module.module != "pam_namespace.so" || module.control != "required" {
		t.Fatalf("third module = %q control %q, want required pam_namespace.so", module.module, module.control)
	}
	// PAM counts MODULES, not lines: the jump must be exactly the number of
	// managed modules after the classifier.
	if want, got := len(stack)-1, classifier.action(retSuccess).jumpLen; want != got {
		t.Errorf("classifier jump = %d, want %d (the managed modules after it)", got, want)
	}
}

// TestNamespaceBlockCarriesNoFailOpenOption is the regression guard for the
// rejected `ignore_config_error`: with that option pam_namespace SKIPS a
// malformed config line and the session proceeds with the shared host /tmp.
func TestNamespaceBlockCarriesNoFailOpenOption(t *testing.T) {
	for _, line := range NamespacePAMBlock("/usr/lib/bunker/pam-tmp-guard", DefaultAgentGroup) {
		if strings.Contains(line, "ignore_config_error") {
			t.Errorf("managed PAM line carries the fail-open option: %q", line)
		}
		if strings.Contains(line, "ignore_instance_parent_mode") {
			t.Errorf("managed PAM line weakens the instance-parent requirement: %q", line)
		}
	}
	if strings.Contains(NamespaceConf("/var/lib/bunkerd/agent-tmp"), "ignore_config_error") {
		t.Error("namespace.conf rendering mentions the fail-open option")
	}
	if !strings.HasPrefix(NamespacePAMModuleLine, "session") || !strings.HasSuffix(NamespacePAMModuleLine, "required     pam_namespace.so") {
		t.Errorf("module line = %q, want a bare `session required pam_namespace.so`", NamespacePAMModuleLine)
	}
}

// TestNamespaceConfFourthFieldIsNotTheScopingMechanism documents where the
// scoping lives: namespace.conf can only name users (pam_namespace resolves
// each entry with getpwnam and drops unknown names), so it must never be
// load-bearing — while root's exclusion stays as defense in depth.
func TestNamespaceConfFourthFieldIsNotTheScopingMechanism(t *testing.T) {
	conf := NamespaceConf("/var/lib/bunkerd/agent-tmp")
	if !strings.Contains(conf, "/tmp  /var/lib/bunkerd/agent-tmp/  "+NamespaceMethod+"  root") {
		t.Errorf("namespace.conf line is wrong:\n%s", conf)
	}
	if strings.Contains(conf, DefaultAgentGroup) {
		t.Errorf("namespace.conf names the agent group, which pam_namespace cannot enforce:\n%s", conf)
	}
	if !strings.Contains(conf, NamespacePAMAgentPattern) {
		t.Errorf("namespace.conf does not document the pattern the PAM block scopes on:\n%s", conf)
	}
}

// TestNamespaceConfRuleMatchesRenderedDropIn pins the single source of the
// verified rule string: the helper compares the installed drop-in against
// NamespaceConfRule, so the drop-in and the check must come from it.
func TestNamespaceConfRuleMatchesRenderedDropIn(t *testing.T) {
	rule := NamespaceConfRule("/var/lib/bunkerd/agent-tmp")
	if rule != "/tmp  /var/lib/bunkerd/agent-tmp/  user:noinit  root" {
		t.Fatalf("rule = %q", rule)
	}
	if !strings.Contains(NamespaceConf("/var/lib/bunkerd/agent-tmp"), rule) {
		t.Error("the rendered drop-in does not contain the rule the helper verifies")
	}
}

// TestClassifierSeparatesAgentsFromOperatorsWithoutGroupState is the core
// regression for the group-drift fail-open: whatever the group table says, a
// `bunker-*` name must not be treated as an ordinary session, and an ordinary
// name must not be treated as an agent.
func TestClassifierSeparatesAgentsFromOperatorsWithoutGroupState(t *testing.T) {
	stack := parsePAMStack(NamespacePAMBlock("/usr/lib/bunker/pam-tmp-guard", DefaultAgentGroup))
	classifier := stack[0]

	// pam_succeed_if `user !~ bunker-*`: PAM_SUCCESS for a NON-matching user,
	// PAM_AUTH_ERR for a matching one (evaluate_noglob). Group state is not an
	// input to this test at all — that is the point.
	for _, tc := range []struct {
		user     string
		matches  bool
		wantKind string
	}{
		{"bunker-abc123", true, "ignore"}, // agent: continue to the verifier
		{"bunker-9f", true, "ignore"},     // agent: continue to the verifier
		{"root", false, "jump"},           // operator: skip the managed modules
		{"kara", false, "jump"},           // operator: skip the managed modules
		{"bunker", false, "jump"},         // not the reserved pattern (no dash)
		{"notbunker-x", false, "jump"},    // not the reserved pattern
	} {
		t.Run(tc.user, func(t *testing.T) {
			code := retSuccess
			if tc.matches {
				code = retAuthErr
			}
			act := classifier.action(code)
			if act.kind != tc.wantKind {
				t.Errorf("classifier action for %q (pattern match=%v) = %q, want %q", tc.user, tc.matches, act.kind, tc.wantKind)
			}
			if tc.matches && act.jumpLen != 0 {
				t.Errorf("agent session must not jump, got jumpLen=%d", act.jumpLen)
			}
			if !tc.matches && act.jumpLen != 2 {
				t.Errorf("non-agent session must jump over both managed modules, got jumpLen=%d", act.jumpLen)
			}
		})
	}
}

// TestOperatorSkipsTheEntireManagedBlock: an ordinary non-agent SSH user never
// runs the verifier and never reaches pam_namespace, so the host /tmp survives
// untouched — including when the boundary is broken.
func TestOperatorSkipsTheEntireManagedBlock(t *testing.T) {
	stack := parsePAMStack(NamespacePAMBlock("/usr/lib/bunker/pam-tmp-guard", DefaultAgentGroup))
	for _, name := range []string{"root", "kara", "deploy"} {
		t.Run(name, func(t *testing.T) {
			ran, denied := evalPAMStack(stack, func(l pamStackLine) pamReturn {
				if l.module == "pam_succeed_if.so" {
					return retSuccess // pam_succeed_if: `user !~ bunker-*` succeeded
				}
				t.Fatalf("module %s must not run for an ordinary session", l.module)
				return retSuccess
			})
			if denied {
				t.Errorf("ordinary session %q was denied (ran: %v)", name, ran)
			}
			if ranModule(ran, "pam_exec.so") || ranModule(ran, "pam_namespace.so") {
				t.Errorf("ordinary session %q ran a Bunker module: %v", name, ran)
			}
		})
	}
}

// TestProvisioningRegressions keeps the installer's failure modes loud.
//
// TestEnsureTmpNamespace_ClassifierModuleMissingFailsLoud: without
// pam_succeed_if the agent-name scoping cannot be expressed, so the installer
// must refuse instead of activating a block that would apply to every
// non-root user.
func TestEnsureTmpNamespace_ClassifierModuleMissingFailsLoud(t *testing.T) {
	rec := newRecorder(t)
	root := rec.root
	o := Options{Runner: rec.run, Root: root}.WithDefaults()
	if err := os.MkdirAll(filepath.Dir(o.SSHDConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.SSHDConfigPath, []byte(samplePAM), 0o644); err != nil {
		t.Fatal(err)
	}
	// pam_namespace and pam_exec only — no classifier module.
	for _, mod := range []string{"pam_namespace.so", "pam_exec.so"} {
		module := filepath.Join(root, "lib/x86_64-linux-gnu/security", mod)
		if err := os.MkdirAll(filepath.Dir(module), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(module, []byte(classifierModuleStub), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := o.EnsureTmpNamespace(context.Background())
	if err == nil {
		t.Fatal("expected an error when pam_succeed_if.so is not installed")
	}
	if !strings.Contains(err.Error(), "pam_succeed_if.so not found") {
		t.Errorf("error = %v, want it to name the missing classifier module", err)
	}
	if pam, _ := os.ReadFile(o.SSHDConfigPath); string(pam) != samplePAM {
		t.Errorf("failed install modified the PAM stack:\n%s", pam)
	}
}

// TestEnsureTmpNamespace_ExecModuleMissingFailsLoud: the fail-closed
// precondition cannot run without pam_exec, and a block that invokes a missing
// module would break the whole sshd stack — refuse, and touch nothing.
func TestEnsureTmpNamespace_ExecModuleMissingFailsLoud(t *testing.T) {
	rec := newRecorder(t)
	root := rec.root
	o := Options{Runner: rec.run, Root: root}.WithDefaults()
	if err := os.MkdirAll(filepath.Dir(o.SSHDConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.SSHDConfigPath, []byte(samplePAM), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mod := range []string{"pam_namespace.so", "pam_succeed_if.so"} {
		module := filepath.Join(root, "lib/x86_64-linux-gnu/security", mod)
		if err := os.MkdirAll(filepath.Dir(module), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(module, []byte(classifierModuleStub), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := o.EnsureTmpNamespace(context.Background())
	if err == nil {
		t.Fatal("expected an error when pam_exec.so is not installed")
	}
	if !strings.Contains(err.Error(), "pam_exec.so not found") {
		t.Errorf("error = %v, want it to name the missing precondition module", err)
	}
	if pam, _ := os.ReadFile(o.SSHDConfigPath); string(pam) != samplePAM {
		t.Errorf("failed install modified the PAM stack:\n%s", pam)
	}
	if _, err := os.Stat(o.PamHelperPath()); !os.IsNotExist(err) {
		t.Errorf("failed install wrote the helper: %v", err)
	}
}
