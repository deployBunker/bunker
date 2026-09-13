package hostsetup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Per-session private /tmp (GAP-075).
//
// SSH sessions do not run under a systemd unit, so PrivateTmp= cannot reach
// them. The Linux-PAM module pam_namespace is the mechanism the kernel and
// distro provide for exactly this: at session setup (as root, before the user
// is switched to) it gives the session its own mount namespace and binds an
// instance directory over /tmp. Bunker turns it on for AGENT sshd sessions:
//
//	/etc/security/namespace.d/50-bunker-agents.conf   (new file, ours alone)
//	/etc/pam.d/sshd                                   (one marked session block)
//	/usr/lib/bunker/pam-tmp-guard(+.sha256)           (the fail-closed helper)
//	/var/lib/bunkerd/agent-tmp/<bunker-id>            (per-agent instance dir)
//
// Four properties matter and all four are load-bearing:
//
//  1. SCOPING BY NAME, NOT BY GROUP STATE. The namespace.conf fourth field is
//     a list of USERNAMES only (namespace.conf(5): "a comma separated list of
//     user names"; pam_namespace resolves each with getpwnam()), so it cannot
//     express a group and cannot track agents created dynamically. The scoping
//     therefore lives in the PAM stack, keyed on the reserved `bunker-*`
//     username pattern (pam_succeed_if's `user !~` test, which reads the
//     PAM_USER item). Group state cannot influence the answer: if the agent
//     group is deleted, an agent session is still recognised as an agent —
//     and then DENIED by the verifier — instead of being mistaken for an
//     ordinary operator session and handed the shared host /tmp.
//
//  2. A FAIL-CLOSED PRECONDITION. pam_namespace alone cannot prove a Bunker
//     /tmp rule exists: with no matching polydir it returns PAM_SUCCESS and
//     the session keeps the shared /tmp. The agent-only verifier
//     (NamespacePAMVerifyLine -> PamGuardScript) therefore runs between the
//     classifier and the module, and `default=die` denies the session
//     whenever the drop-in is missing, is not the required rule, is malformed,
//     the group or the membership is gone, the instance parent is wrong, or
//     the helper itself has drifted.
//
//  3. NO SHARED-/TMP OPTIONS. The module line is `required` and carries no
//     `ignore_config_error` (which would skip a malformed namespace.conf line
//     and let the session continue with the shared host /tmp) and no
//     `ignore_instance_parent_mode`.
//
//  4. BOUNDED BLAST RADIUS. Only the managed block is agent-scoped: an
//     ordinary non-agent SSH user is jumped over all three Bunker modules and
//     keeps the host /tmp; local logins, sudo and the daemon never use the
//     sshd PAM stack. The block is identified by its marker, so install,
//     repair and uninstall touch Bunker's own lines only — never an
//     operator's or the distribution's pam_namespace/pam_succeed_if rules.
//
// Why not a forced command: `command=` in authorized_keys replaces the
// requested command, which breaks scp/sftp/sshfs (the transports `bunker cp`,
// `bunker deploy` and `bunker mount` depend on) and cannot tell an interactive
// shell from an sftp subsystem request. pam_namespace changes no transport: it
// only changes which directory /tmp resolves to inside the session.
//
// Identity is preserved — the session still runs as the agent user with the
// agent's own uid, unlike a user-namespace wrapper that remaps the caller to
// root. root is excluded in the namespace config as well (defense in depth)
// and is not an agent name, so it keeps the host /tmp.

// namespaceModulePaths are the locations a distro may install the PAM modules
// this boundary needs.
var namespaceModulePaths = []string{
	"/lib/x86_64-linux-gnu/security/pam_namespace.so",
	"/usr/lib/x86_64-linux-gnu/security/pam_namespace.so",
	"/lib/security/pam_namespace.so",
	"/usr/lib/security/pam_namespace.so",
	"/lib64/security/pam_namespace.so",
}

// guardPatternTokens are the byte sequences the classifier module must contain
// for the agent-name pattern test to exist at all.
//
// `user !~ <glob>` (and its alias `noglob`) is implemented by
// evaluate_noglob() -> fnmatch(3), and both the linked symbol and the qualifier
// literal are present in any module that supports it. A module that predates
// them would treat `!~` as an unknown attribute, return PAM_SERVICE_ERR, and —
// under the block's `default=die` — DENY EVERY SESSION ON THE HOST, including
// root's. The installer therefore probes the module BEFORE writing anything and
// refuses, naming the requirement, instead of installing a block that would
// turn a stale PAM into a lockout.
var guardPatternTokens = []string{"fnmatch", "noglob"}

// missingPatternTokens reports which of guardPatternTokens the module at path
// does not contain (empty when the module supports the agent-name pattern).
func missingPatternTokens(path string) []string {
	body, err := os.ReadFile(path)
	if err != nil {
		return append([]string(nil), guardPatternTokens...)
	}
	var missing []string
	for _, tok := range guardPatternTokens {
		if !bytes.Contains(body, []byte(tok)) {
			missing = append(missing, tok)
		}
	}
	return missing
}

// guardModulePaths are the locations the CLASSIFIER module (pam_succeed_if)
// may live in. It ships in the same libpam-modules package as pam_namespace,
// but the provisioner still checks, because activating a guard line for a
// missing module would change how the whole sshd stack behaves.
//
// pam_succeed_if answers "is this an agent session?" from the reserved
// username pattern: its `user !~ <glob>` test is evaluate_noglob() ->
// fnmatch(3) (modules/pam_succeed_if/pam_succeed_if.c) applied to the PAM_USER
// item — not to the environment, and not to group membership — so deleting the
// agent group cannot make an agent session look like an ordinary operator
// session.
var guardModulePaths = []string{
	"/lib/x86_64-linux-gnu/security/pam_succeed_if.so",
	"/usr/lib/x86_64-linux-gnu/security/pam_succeed_if.so",
	"/lib/security/pam_succeed_if.so",
	"/usr/lib/security/pam_succeed_if.so",
	"/lib64/security/pam_succeed_if.so",
}

// execModulePaths are the locations pam_exec.so may live in. pam_exec runs the
// agent-only fail-closed precondition (the root-owned helper) between the
// classifier and pam_namespace: pam_namespace itself cannot require that a
// Bunker /tmp rule exists — with none it returns PAM_SUCCESS and the session
// keeps the shared host /tmp (pam_sm_open_session() in pam_namespace.c only
// calls setup_namespace() when a polydir matched) — so the presence and exact
// content of the rule must be enforced outside the module. pam_exec maps a
// non-zero exit, a signal or a failed execve to PAM_SYSTEM_ERR
// (modules/pam_exec/pam_exec.c), which the block's control field turns into a
// denied session.
var execModulePaths = []string{
	"/lib/x86_64-linux-gnu/security/pam_exec.so",
	"/usr/lib/x86_64-linux-gnu/security/pam_exec.so",
	"/lib/security/pam_exec.so",
	"/usr/lib/security/pam_exec.so",
	"/lib64/security/pam_exec.so",
}

// NamespaceConfRule renders the single rule pam_namespace must see for /tmp:
// polydir, instance prefix (trailing slash REQUIRED — the module appends the
// instance differentiation string), method, and the exclusion list.
//
// NamespaceConf, the fail-closed helper and the tests all derive the rule from
// this one function, so the installed drop-in and the verified string cannot
// drift apart.
func NamespaceConfRule(instanceRoot string) string {
	root := instanceRoot
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	return fmt.Sprintf("/tmp  %s  %s  %s", root, NamespaceMethod, NamespaceUserExclusion)
}

// NamespaceConf renders the pam_namespace drop-in. Pure so tests pin it.
//
// The fourth field excludes root from polyinstantiation. That is defense in
// depth, NOT the scoping mechanism: the sshd PAM block is what keeps non-agent
// users out of the module entirely (see the file comment).
func NamespaceConf(instanceRoot string) string {
	return `# Bunker GAP-075 — per-agent private /tmp (managed by ` + "`bunker host-provision`" + `)
#
# /tmp is polyinstantiated per agent user: each agent session gets its own
# instance directory (bind-mounted over /tmp), so one agent can never see or
# overwrite another agent's /tmp. The sshd PAM stack applies this module ONLY
# to sessions whose user name matches the reserved agent pattern ` + NamespacePAMAgentPattern + `,
# and a pam_exec precondition (pam-tmp-guard) proves this rule is present and
# the session user is in the agent group before the module runs; a session
# whose boundary cannot be proven is denied outright. Ordinary operator SSH
# sessions are jumped over the whole block and keep the host /tmp; root is
# excluded here as well and keeps its own /tmp.
#
# The instance parent must be mode 0000 (pam_namespace enforces this), so the
# instance directories are not reachable from inside any session.
#
# Deleting THIS FILE alone does NOT restore the previous shared-/tmp behavior:
# the sshd PAM block stays installed, and its pam_exec precondition (which is
# what proves this rule exists) then fails closed, so every ` + NamespacePAMAgentPattern + ` session
# is DENIED. An operator who wants the old behavior back must run
# ` + "`bunker host-provision --uninstall --apply`" + `, which removes the PAM block (so
# no session is denied) and then this file. Removing the drop-in by hand locks
# agents out instead of sharing /tmp.
` + NamespaceConfRule(instanceRoot) + "\n"
}

// effectiveNamespaceRules returns the EFFECTIVE rules of a pam_namespace
// configuration file — comment and blank lines dropped, whitespace collapsed —
// exactly as the session-time helper reads it
// (`line=${raw%%#*}; set -- $line; [ $# -gt 0 ]`).
func effectiveNamespaceRules(body string) []string {
	var rules []string
	for _, raw := range splitLines(body) {
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		rules = append(rules, strings.Join(fields, " "))
	}
	return rules
}

// namespaceConfigSetOK mirrors, in Go, the configuration checks the RUNTIME
// helper performs before pam_namespace runs (checks 4 and 5 of PamGuardScript),
// so the status verdict cannot claim a boundary the helper would deny:
//
//   - the Bunker drop-in must declare EXACTLY ONE rule and it must be the
//     required /tmp rule: a deleted, malformed, different or doubled drop-in is
//     a DENIAL, never a silent shared /tmp;
//   - every other file pam_namespace merges (the distribution's namespace.conf
//     and every other drop-in in the directory) must be parseable and must not
//     declare a /tmp polydir of its own, which pam_namespace would apply
//     instead of Bunker's rule.
//
// It returns the reason a false verdict is false, for the operator report.
func namespaceConfigSetOK(o Options, dropInBody string) (bool, string) {
	required := strings.Join(strings.Fields(NamespaceConfRule(o.TmpInstanceRoot)), " ")
	rules := effectiveNamespaceRules(dropInBody)
	if len(rules) != 1 {
		return false, fmt.Sprintf("the drop-in must declare exactly one rule, found %d", len(rules))
	}
	if rules[0] != required {
		return false, "the drop-in rule is not the required Bunker /tmp rule: " + rules[0]
	}
	files := []string{o.NamespaceConfFile}
	if entries, err := os.ReadDir(o.NamespaceConfDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
				continue
			}
			files = append(files, filepath.Join(o.NamespaceConfDir, e.Name()))
		}
	}
	seen := map[string]bool{}
	for _, f := range files {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		body, err := os.ReadFile(f)
		if err != nil {
			continue // absent: the module skips it too
		}
		for _, rule := range effectiveNamespaceRules(string(body)) {
			fields := strings.Fields(rule)
			if len(fields) < 3 {
				return false, f + " has a line pam_namespace cannot parse: " + rule
			}
			// The helper identifies the drop-in by base name (the module reads
			// the directory it lives in), so a same-named file is Bunker's.
			if fields[0] == "/tmp" && path.Base(f) != o.NamespaceConfName {
				return false, f + " also declares a /tmp polydir"
			}
		}
	}
	return true, ""
}

// TmpNamespaceState is the observed state of the per-session private-/tmp
// provisioning.
type TmpNamespaceState struct {
	// ModulePresent is true when pam_namespace.so is installed on the host.
	ModulePresent bool
	ModulePath    string
	// GuardModulePresent is true when pam_succeed_if.so (the agent-name
	// classifier) is installed.
	GuardModulePresent bool
	GuardModulePath    string
	// GuardModuleSupportsPattern is true when that module carries the glob
	// matching the classifier line needs (`user !~ <pattern>`): without it the
	// line is an unknown attribute, every session is denied by the block's
	// default=die, and the host is NOT usable — so it is part of Active.
	GuardModuleSupportsPattern bool
	// GuardModuleMissingTokens names the tokens the module lacks (diagnostic).
	GuardModuleMissingTokens []string
	// ExecModulePresent is true when pam_exec.so (the runner of the
	// fail-closed precondition) is installed.
	ExecModulePresent bool
	ExecModulePath    string
	// ConfPresent is true when Bunker's namespace.d drop-in exists.
	ConfPresent bool
	ConfPath    string
	ConfBody    string
	// ConfRuleOK is true when the drop-in declares EXACTLY the required Bunker
	// /tmp rule and no other namespace configuration file declares a /tmp
	// polydir or carries a line pam_namespace cannot parse. It mirrors the
	// configuration checks the session-time helper performs, so `Active` cannot
	// claim a boundary the helper would deny. ConfRuleDetail explains a false.
	ConfRuleOK     bool
	ConfRuleDetail string
	// ConfOwner / ConfMode are the drop-in's owner ("uid:gid") and permission
	// bits; ConfOwnerOK / ConfModeOK are the half of the verdict that is
	// statically observable. A drop-in the agent group can rewrite is a
	// boundary an agent can move.
	ConfOwner   string
	ConfMode    os.FileMode
	ConfOwnerOK bool
	ConfModeOK  bool
	// HelperPresent is true when the pam_exec precondition helper exists.
	HelperPresent bool
	HelperPath    string
	// HelperHash is the sha256 of the helper on disk, HelperExpectedHash the
	// hash its manifest records.
	HelperHash         string
	HelperExpectedHash string
	// HelperIntegrityOK is true when the manifest exists and proves the
	// helper's content has not drifted. Without it every agent session is
	// denied, so it is part of the readiness verdict.
	HelperIntegrityOK bool
	// HelperManifestPresent is true when the manifest file exists.
	HelperManifestPresent bool
	// HelperOwner is "uid:gid" of the helper when it can be stat-ed.
	HelperOwner string
	// HelperMode is the permission bits of the helper.
	HelperMode os.FileMode
	// HelperOwnerOK / HelperModeOK are the static checks on the helper itself:
	// root-owned (when observable) and not group/world writable. A helper an
	// agent can rewrite is not a precondition at all.
	HelperOwnerOK bool
	HelperModeOK  bool
	// HelperManifestOwner / HelperManifestMode describe the manifest, and
	// HelperManifestOwnerOK / HelperManifestModeOK are its static checks. The
	// manifest is what makes the helper's bytes trustworthy: a manifest an
	// agent can rewrite would prove anything.
	HelperManifestOwner   string
	HelperManifestMode    os.FileMode
	HelperManifestOwnerOK bool
	HelperManifestModeOK  bool
	// HelperDir is the directory holding the helper and its manifest — the
	// FIRST link of the trust chain the runtime helper verifies. A directory
	// the agent group can write lets an agent replace BOTH files by
	// rename/unlink, so its presence, owner and mode are part of Active.
	HelperDir        string
	HelperDirPresent bool
	HelperDirOwner   string
	HelperDirMode    os.FileMode
	HelperDirOwnerOK bool
	HelperDirModeOK  bool
	// PAMBlockPresent is true when the managed session block is in the sshd
	// PAM stack AND is intact: marker, classifier, verifier and module line
	// present, adjacent, in that order. The classifier's control field jumps
	// over exactly the next two modules, so adjacency is a correctness
	// property, not cosmetics.
	PAMBlockPresent bool
	SSHDConfigPath  string
	// AgentGroup is the group the deployed verifier line passes to the helper.
	AgentGroup string
	// DeployedVerifyGroup is the group Bunker's verifier line currently
	// installed in the sshd stack names ("" when no verifier line is present).
	// When it differs from AgentGroup the host is NOT isolated for this
	// configuration: the helper denies (it refuses a group it did not render),
	// so agents would be locked out rather than silently shared.
	DeployedVerifyGroup string
	// AgentGroupPresent is true when that group exists. Without it the helper
	// denies every agent session, so this is part of the readiness answer.
	AgentGroupPresent bool
	// InstanceRoot is the instance parent directory path.
	InstanceRoot string
	// InstanceRootPresent is true when that directory exists. It must be mode
	// 0000 when it does (pam_namespace refuses a wider instance parent), or a
	// session gets no namespace at all — the helper denies that too.
	InstanceRootPresent bool
	// InstanceRootMode is the mode of the instance parent directory;
	// InstanceRootModeOK is true only when it is exactly 0000.
	InstanceRootMode os.FileMode
	// InstanceRootOwner / InstanceRootOwnerOK are the owner of the instance
	// parent; pam_namespace requires it to be root-owned.
	InstanceRootOwner   string
	InstanceRootOwnerOK bool
	InstanceRootModeOK  bool
	// OwnershipVerifiable is true when this process is root, i.e. when the
	// ownership checks above could actually be observed. When it is false the
	// ownership half is reported as unverifiable (not as passing) — a live
	// `bunker host-provision --status` runs as root and verifies it.
	OwnershipVerifiable bool
	// Active is the single readiness answer. It is the conjunction of every
	// static property the RUNTIME helper enforces before pam_namespace runs:
	// all three modules present (classifier with glob support), the drop-in
	// present with exactly the required /tmp rule and the whole configuration
	// set parseable, the helper trust chain (helper directory -> manifest ->
	// helper) present, root-owned and not group/world writable with a matching
	// hash, the intact agent-scoped PAM block naming the configured group, the
	// agent group present, and the instance parent present, root-owned and mode
	// 0000. Anything that would make the helper DENY (or that would let an
	// agent rewrite the boundary) makes this false.
	Active bool
}

// TmpNamespaceStatus reports the per-session private-/tmp provisioning state
// without changing anything. It observes the same static properties the RUNTIME
// helper enforces at session time (and the same trust chain), so `Active` is a
// claim about sessions, not about the installer's intent.
func (o Options) TmpNamespaceStatus() (TmpNamespaceState, error) {
	o = o.WithDefaults()
	st := TmpNamespaceState{
		ConfPath:            o.NamespaceConfPath(),
		SSHDConfigPath:      o.SSHDConfigPath,
		InstanceRoot:        o.TmpInstanceRoot,
		AgentGroup:          o.AgentGroup,
		HelperPath:          o.PamHelperPath(),
		HelperDir:           o.PamHelperDir,
		OwnershipVerifiable: privileged(),
	}
	if path, ok := findModule(o, namespaceModulePaths); ok {
		st.ModulePresent = true
		st.ModulePath = path
	}
	if path, ok := findModule(o, guardModulePaths); ok {
		st.GuardModulePresent = true
		st.GuardModulePath = path
		st.GuardModuleMissingTokens = missingPatternTokens(path)
		st.GuardModuleSupportsPattern = len(st.GuardModuleMissingTokens) == 0
	}
	if path, ok := findModule(o, execModulePaths); ok {
		st.ExecModulePresent = true
		st.ExecModulePath = path
	}
	if body, err := os.ReadFile(st.ConfPath); err == nil {
		st.ConfPresent = true
		st.ConfBody = string(body)
	}
	if owner, mode, ok := statOwnerMode(st.ConfPath); ok {
		st.ConfOwner, st.ConfMode = owner, mode
	}
	st.ConfOwnerOK = st.ConfPresent && ownerTrusted(st.ConfOwner)
	st.ConfModeOK = st.ConfPresent && modeNotGroupOrWorldWritable(st.ConfMode)
	st.ConfRuleOK, st.ConfRuleDetail = namespaceConfigSetOK(o, st.ConfBody)

	if body, err := os.ReadFile(st.HelperPath); err == nil {
		st.HelperPresent = true
		sum := sha256.Sum256(body)
		st.HelperHash = hex.EncodeToString(sum[:])
	}
	if owner, mode, ok := statOwnerMode(st.HelperPath); ok {
		st.HelperOwner, st.HelperMode = owner, mode
	}
	st.HelperOwnerOK = st.HelperPresent && ownerTrusted(st.HelperOwner)
	st.HelperModeOK = st.HelperPresent && modeNotGroupOrWorldWritable(st.HelperMode)

	if body, err := os.ReadFile(o.PamHelperManifestPath()); err == nil {
		st.HelperManifestPresent = true
		st.HelperExpectedHash = strings.TrimSpace(string(body))
		st.HelperIntegrityOK = st.HelperPresent && st.HelperExpectedHash != "" &&
			st.HelperExpectedHash == st.HelperHash
	}
	if owner, mode, ok := statOwnerMode(o.PamHelperManifestPath()); ok {
		st.HelperManifestOwner, st.HelperManifestMode = owner, mode
	}
	st.HelperManifestOwnerOK = st.HelperManifestPresent && ownerTrusted(st.HelperManifestOwner)
	st.HelperManifestModeOK = st.HelperManifestPresent && modeNotGroupOrWorldWritable(st.HelperManifestMode)

	if _, err := os.Stat(o.PamHelperDir); err == nil {
		st.HelperDirPresent = true
	}
	if owner, mode, ok := statOwnerMode(o.PamHelperDir); ok {
		st.HelperDirOwner, st.HelperDirMode = owner, mode
	}
	st.HelperDirOwnerOK = st.HelperDirPresent && ownerTrusted(st.HelperDirOwner)
	st.HelperDirModeOK = st.HelperDirPresent && modeNotGroupOrWorldWritable(st.HelperDirMode)

	if owner, mode, ok := statOwnerMode(o.TmpInstanceRoot); ok {
		st.InstanceRootPresent = true
		st.InstanceRootOwner, st.InstanceRootMode = owner, mode
	}
	st.InstanceRootModeOK = st.InstanceRootPresent && st.InstanceRootMode == 0
	st.InstanceRootOwnerOK = st.InstanceRootPresent && ownerTrusted(st.InstanceRootOwner)

	if body, err := os.ReadFile(o.SSHDConfigPath); err == nil {
		lines := splitLines(string(body))
		st.DeployedVerifyGroup = deployedVerifyGroup(lines)
		st.PAMBlockPresent = o.pamBlockIntact(lines)
	}
	if _, err := o.run(context.Background(), "getent", "group", o.AgentGroup); err == nil {
		st.AgentGroupPresent = true
	}
	st.Active = st.evaluateActive()
	return st, nil
}

// evaluateActive is the readiness verdict as a pure function of the observed
// state, so a test can flip exactly one observed property and assert that the
// verdict changes (including the ownership half, which a non-root test process
// cannot observe on disk).
func (st TmpNamespaceState) evaluateActive() bool {
	return st.ModulePresent && st.GuardModulePresent && st.GuardModuleSupportsPattern &&
		st.ExecModulePresent &&
		st.ConfPresent && st.ConfRuleOK && st.ConfOwnerOK && st.ConfModeOK &&
		st.HelperPresent && st.HelperIntegrityOK && st.HelperOwnerOK && st.HelperModeOK &&
		st.HelperManifestPresent && st.HelperManifestOwnerOK && st.HelperManifestModeOK &&
		st.HelperDirPresent && st.HelperDirOwnerOK && st.HelperDirModeOK &&
		st.PAMBlockPresent && st.AgentGroupPresent && st.DeployedVerifyGroup == st.AgentGroup &&
		st.InstanceRootPresent && st.InstanceRootModeOK && st.InstanceRootOwnerOK
}

// statOwnerMode returns a path's owner ("uid:gid") and permission bits, with
// false when it cannot be stat-ed.
func statOwnerMode(path string) (string, os.FileMode, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, false
	}
	return ownerString(fi), fi.Mode().Perm(), true
}

// ownerString renders "uid:gid" for a file, or "" when the platform does not
// expose it.
func ownerString(fi os.FileInfo) string {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", sys.Uid, sys.Gid)
}

// findModule reports the first installed module path from candidates,
// honouring the Options.Root sandbox prefix.
func findModule(o Options, candidates []string) (string, bool) {
	for _, p := range candidates {
		candidate := p
		if o.Root != "" {
			candidate = filepath.Join(o.Root, p)
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// pamBlockIntact reports whether the canonical managed block — marker,
// classifier, verifier and module line, adjacent and in that order — is in the
// sshd PAM stack. The classifier's jump length is exactly 2 and the verifier
// must run immediately before pam_namespace, so adjacency is a correctness
// property, not cosmetics.
func (o Options) pamBlockIntact(lines []string) bool {
	block := o.NamespacePAMBlockForOptions()
	modules := block[1:] // classifier, verifier, module — the jump counts these
	for i := 0; i+len(modules) <= len(lines); i++ {
		if i == 0 || normalizeLine(lines[i-1]) != normalizeLine(namespaceMarker) {
			continue
		}
		ok := true
		for j, want := range modules {
			if normalizeLine(lines[i+j]) != normalizeLine(want) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ensureTmpNamespaceApplied runs the private-/tmp installer, or renders the
// same report with no side effects when apply is false.
func (o Options) ensureTmpNamespaceApplied(ctx context.Context, apply bool) (*Report, error) {
	if apply {
		return o.EnsureTmpNamespace(ctx)
	}
	rep := &Report{}
	o = o.WithDefaults()
	if err := o.validateTmpNamespaceInputs(); err != nil {
		return rep, err
	}
	st, err := o.TmpNamespaceStatus()
	if err != nil {
		return rep, err
	}
	if !st.ModulePresent {
		rep.Add("fail", "pam_namespace.so", "not installed — install libpam-modules", false)
		return rep, fmt.Errorf("pam_namespace.so is not installed on this host")
	}
	if !st.GuardModulePresent {
		rep.Add("fail", "pam_succeed_if.so", "not installed — install libpam-modules", false)
		return rep, fmt.Errorf("pam_succeed_if.so is not installed on this host")
	}
	if !st.ExecModulePresent {
		rep.Add("fail", "pam_exec.so", "not installed — install libpam-modules", false)
		return rep, fmt.Errorf("pam_exec.so is not installed on this host")
	}
	if !st.GuardModuleSupportsPattern {
		rep.Add("fail", st.GuardModulePath, "does not support the agent-name pattern test (needs Linux-PAM >= 1.6)", false)
		return rep, fmt.Errorf("pam_succeed_if.so on this host cannot express `user !~ %s`", NamespacePAMAgentPattern)
	}
	rep.Add("ok", st.ModulePath, "module present", false)
	if st.AgentGroupPresent {
		rep.Add("ok", "group:"+o.AgentGroup, "agent group already exists", false)
	} else {
		rep.Add("create", "group:"+o.AgentGroup, "create the agent isolation group", false)
	}
	rep.Add("create", o.TmpInstanceRoot, "create instance parent (mode 0000)", false)
	if st.ConfPresent {
		rep.Add("ok", o.NamespaceConfPath(), "drop-in already present", false)
	} else {
		rep.Add("write", o.NamespaceConfPath(), "write pam_namespace drop-in", false)
	}
	if st.HelperIntegrityOK {
		rep.Add("ok", o.PamHelperPath(), "pam_exec precondition helper present and unchanged", false)
	} else {
		rep.Add("write", o.PamHelperPath(), "install the pam_exec precondition helper (+ manifest)", false)
	}
	if st.PAMBlockPresent {
		rep.Add("ok", o.SSHDConfigPath, "agent-scoped session block already present", false)
	} else {
		rep.Add("append", o.SSHDConfigPath, "append the agent-scoped pam_namespace session block", false)
	}
	return rep, nil
}

// EnsureTmpNamespace installs the per-session private-/tmp provisioning: the
// agent isolation group, the 0000 instance parent, the root-owned pam_exec
// precondition helper with its hash manifest, the namespace.d drop-in, and the
// agent-scoped session block in the sshd PAM stack (with a one-time backup of
// that file). Idempotent — a second run reports skips.
//
// Order is load-bearing:
//
//  1. every configurable value is validated BEFORE anything is written, so an
//     unsafe group name or path is never rendered into a PAM line, a config
//     file or the helper;
//  2. the modules are checked, because activating a PAM line for a missing
//     module would change how the whole sshd stack behaves;
//  3. the group comes before the block, because the helper requires the
//     membership of every agent session it verifies;
//  4. the helper and its manifest are installed BEFORE the block that invokes
//     it, so the block never exists in a state where it cannot be satisfied.
//     Uninstall reverses exactly that: block first, helper last.
func (o Options) EnsureTmpNamespace(ctx context.Context) (*Report, error) {
	o = o.WithDefaults()
	rep := &Report{}

	if err := o.validateTmpNamespaceInputs(); err != nil {
		return rep, err
	}

	st, err := o.TmpNamespaceStatus()
	if err != nil {
		return rep, err
	}
	if !st.ModulePresent {
		return rep, fmt.Errorf("pam_namespace.so not found in %s — install libpam-modules (the module ships with Linux-PAM); refusing to activate a PAM line for a missing module", strings.Join(namespaceModulePaths, ", "))
	}
	if !st.GuardModulePresent {
		return rep, fmt.Errorf("pam_succeed_if.so not found in %s — install libpam-modules; the agent-name classifier that scopes pam_namespace cannot be expressed without it", strings.Join(guardModulePaths, ", "))
	}
	if !st.ExecModulePresent {
		return rep, fmt.Errorf("pam_exec.so not found in %s — install libpam-modules; the fail-closed precondition that proves the Bunker /tmp rule and the agent membership cannot run without it", strings.Join(execModulePaths, ", "))
	}
	if !st.GuardModuleSupportsPattern {
		return rep, fmt.Errorf("%s does not support the agent-name pattern test (missing tokens: %s) — the block's classifier (`user !~ %s`) would be an unknown attribute and its default=die would deny EVERY session on this host, so no block was written; upgrade libpam-modules to Linux-PAM >= 1.6 (the release that added the glob/noglob tests)", st.GuardModulePath, strings.Join(st.GuardModuleMissingTokens, ", "), NamespacePAMAgentPattern)
	}
	rep.Add("ok", st.ModulePath, "pam_namespace module present", true)
	rep.Add("ok", st.GuardModulePath, "pam_succeed_if classifier module present", true)
	rep.Add("ok", st.ExecModulePath, "pam_exec precondition module present", true)

	if err := o.ensureAgentGroup(ctx, rep); err != nil {
		return rep, err
	}

	// Instance parent: pam_namespace requires mode 0000 unless the module is
	// given ignore_instance_parent_mode, which would weaken the isolation.
	// Create with a usable mode first (a 0000 intermediate would make its own
	// creation fail) and restrict the leaf immediately after.
	if err := os.MkdirAll(o.TmpInstanceRoot, 0o700); err != nil {
		return rep, fmt.Errorf("create tmp instance parent %s: %w", o.TmpInstanceRoot, err)
	}
	if _, err := o.run(ctx, "chown", "0:0", o.TmpInstanceRoot); err != nil {
		return rep, fmt.Errorf("chown tmp instance parent: %w", err)
	}
	// os.Chmod (not the chmod binary): 0000 has no Go-vs-shell mode ambiguity,
	// and doing it in-process keeps the required mode in place even when the
	// command runner is not a real shell.
	if err := os.Chmod(o.TmpInstanceRoot, 0o000); err != nil {
		return rep, fmt.Errorf("chmod tmp instance parent: %w", err)
	}
	rep.Add("chmod", o.TmpInstanceRoot, "mode 0000 (pam_namespace instance parent)", true)

	if err := o.ensurePamHelper(ctx, rep); err != nil {
		return rep, err
	}

	wrote, err := writeFileIdempotent(o.NamespaceConfPath(), []byte(NamespaceConf(o.TmpInstanceRoot)), 0o644)
	if err != nil {
		return rep, err
	}
	rep.Add("write", o.NamespaceConfPath(), "pam_namespace drop-in", wrote)

	changed, err := o.ensureSSHDNamespaceBlock()
	if err != nil {
		return rep, err
	}
	rep.Add("write", o.SSHDConfigPath, "agent-scoped pam_namespace session block", changed)

	// Re-read: the report must reflect the state the next session will see,
	// not the intent of this run.
	after, err := o.TmpNamespaceStatus()
	if err != nil {
		return rep, err
	}
	if !after.Active {
		return rep, fmt.Errorf("tmp namespace provisioning incomplete after apply (module=%v classifier=%v classifier_pattern=%v pam_exec=%v dropin=%v helper=%v helper_hash=%v pam_block=%v agent_group=%v deployed_group=%q)",
			after.ModulePresent, after.GuardModulePresent, after.GuardModuleSupportsPattern, after.ExecModulePresent, after.ConfPresent,
			after.HelperPresent, after.HelperIntegrityOK, after.PAMBlockPresent, after.AgentGroupPresent, after.DeployedVerifyGroup)
	}
	return rep, nil
}

// ensurePamHelper installs (or repairs) the pam_exec precondition helper and
// its sha256 manifest, root-owned and not group/world writable, atomically and
// idempotently. It is a hard error to fail here: the PAM block's verifier
// denies every agent session unless this helper and its manifest agree.
//
// The DIRECTORY is repaired and verified too, and it is repaired FIRST. The
// session-time helper checks its whole trust chain (directory -> manifest ->
// helper); chowning a pre-existing group/world-writable /usr/lib/bunker while
// leaving its mode alone would let a local agent replace both the root-owned
// helper and its manifest by rename/unlink — the trust chain would then be
// satisfied by the agent's own copies. "The chmod exited 0" is not proof
// either, so the chain is STAT-ED BACK after the write and an error is
// returned if any link is still group/world writable.
func (o Options) ensurePamHelper(ctx context.Context, rep *Report) error {
	script, err := o.PamGuardScript()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.PamHelperDir, PamHelperDirMode); err != nil {
		return fmt.Errorf("create helper directory %s: %w", o.PamHelperDir, err)
	}
	if _, err := o.run(ctx, "chown", "0:0", o.PamHelperDir); err != nil {
		return fmt.Errorf("chown helper directory: %w", err)
	}
	// Tighten (never widen): a directory that is already tighter than 0755 is
	// left alone, one with a group/world write bit is repaired.
	if fi, err := os.Stat(o.PamHelperDir); err != nil || !modeNotGroupOrWorldWritable(fi.Mode()) {
		if err := os.Chmod(o.PamHelperDir, PamHelperDirMode); err != nil {
			return fmt.Errorf("chmod helper directory %s: %w", o.PamHelperDir, err)
		}
		rep.Add("chmod", o.PamHelperDir, "helper directory repaired to root:root 0755 (was group/world writable)", true)
	} else {
		rep.Add("ok", o.PamHelperDir, "helper directory not group/world writable", false)
	}

	wrote, err := writeFileIdempotent(o.PamHelperPath(), []byte(script), 0o755)
	if err != nil {
		return err
	}
	if _, err := o.run(ctx, "chown", "0:0", o.PamHelperPath()); err != nil {
		return fmt.Errorf("chown pam_exec precondition helper: %w", err)
	}
	if err := os.Chmod(o.PamHelperPath(), 0o755); err != nil {
		return fmt.Errorf("chmod pam_exec precondition helper: %w", err)
	}
	rep.Add("write", o.PamHelperPath(), "pam_exec fail-closed precondition helper (root:root 0755)", wrote)

	manifestWrote, err := writeFileIdempotent(o.PamHelperManifestPath(), []byte(PamGuardManifest(script)), 0o444)
	if err != nil {
		return err
	}
	if _, err := o.run(ctx, "chown", "0:0", o.PamHelperManifestPath()); err != nil {
		return fmt.Errorf("chown helper manifest: %w", err)
	}
	if err := os.Chmod(o.PamHelperManifestPath(), 0o444); err != nil {
		return fmt.Errorf("chmod helper manifest: %w", err)
	}
	rep.Add("write", o.PamHelperManifestPath(), "sha256 manifest of the pam_exec precondition helper", manifestWrote)

	if err := o.verifyPamHelperChain(); err != nil {
		return err
	}
	return nil
}

// verifyPamHelperChain re-reads the trust chain the RUNTIME helper enforces —
// helper directory, manifest, helper — and fails when any link is group/world
// writable (always checkable) or not root-owned (checkable when this process is
// root, which is the only way the installer runs against a real host). It reads
// the filesystem, not the chown/chmod argv, so a filesystem or an attacker that
// defeated the mode change is reported instead of being reported as repaired.
func (o Options) verifyPamHelperChain() error {
	for _, link := range []struct {
		what string
		path string
	}{
		{"helper directory", o.PamHelperDir},
		{"helper manifest", o.PamHelperManifestPath()},
		{"pam_exec precondition helper", o.PamHelperPath()},
	} {
		fi, err := os.Stat(link.path)
		if err != nil {
			return fmt.Errorf("helper trust chain: stat %s %s: %w", link.what, link.path, err)
		}
		if !modeNotGroupOrWorldWritable(fi.Mode()) {
			return fmt.Errorf("helper trust chain: %s %s is group/world writable (mode %04o) — an agent could replace the helper and its manifest", link.what, link.path, fi.Mode().Perm())
		}
		if owner := ownerString(fi); !ownerTrusted(owner) {
			return fmt.Errorf("helper trust chain: %s %s is not root-owned (%s)", link.what, link.path, owner)
		}
	}
	return nil
}

// privileged reports whether this process can verify (and enforce) POSIX file
// ownership the way a session-time helper — which pam_exec always runs as root
// — does.
func privileged() bool { return os.Geteuid() == 0 }

// ownerTrusted reports whether an owner string ("uid:gid") satisfies the
// boundary. Root ownership is required; when this process is NOT root the
// ownership cannot be observed at all, and the mode (which is always
// observable) remains the check that is applied — exactly as the helper itself
// treats ownership (`if [ "$(id -u)" = 0 ]`), because a helper that ran
// unprivileged could not enforce it either. An empty owner string means the
// file could not be stat-ed, which is never trusted.
func ownerTrusted(owner string) bool {
	if owner == "" {
		return false
	}
	if owner == "0:0" {
		return true
	}
	return !privileged()
}

// modeNotGroupOrWorldWritable reports whether a file's permission bits keep the
// group and the world from writing it.
func modeNotGroupOrWorldWritable(mode os.FileMode) bool { return mode.Perm()&0o022 == 0 }

// ensureSSHDNamespaceBlock writes Bunker's managed session block into the sshd
// PAM stack when it is missing, stale (an older group name or helper path, or
// the rejected first-revision shape) or half-edited, and reports whether it
// changed the file.
//
// The file is backed up once (before the first Bunker edit) and written
// atomically through a temp file, because a torn /etc/pam.d/sshd would break
// every login on the host. Every Bunker-managed BLOCK is removed before the
// canonical block is appended, so an upgrade can never leave two blocks (or a
// block without its verifier) behind — and an operator's own pam_namespace or
// pam_succeed_if rules are never touched, because ownership is decided by
// Bunker's marker and Bunker's own line shapes, not by the mere presence of a
// module name (see managedBlockStart).
func (o Options) ensureSSHDNamespaceBlock() (bool, error) {
	body, err := os.ReadFile(o.SSHDConfigPath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", o.SSHDConfigPath, err)
	}
	if o.pamBlockIntact(splitLines(string(body))) && !hasManagedDrift(string(body), o.NamespacePAMBlockForOptions()) {
		return false, nil
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(o.SSHDConfigPath); err == nil {
		mode = fi.Mode().Perm()
	}
	backup := o.SSHDConfigPath + ".bunker-backup"
	if _, err := os.Stat(backup); os.IsNotExist(err) {
		if err := os.WriteFile(backup, body, mode); err != nil {
			return false, fmt.Errorf("back up %s: %w", o.SSHDConfigPath, err)
		}
	}
	updated := appendManagedBlock(string(body), o.NamespacePAMBlockForOptions())
	return writeFileIdempotent(o.SSHDConfigPath, []byte(updated), mode)
}

// appendManagedBlock strips every Bunker-managed block from body and appends
// block at the end, preserving the rest of the file byte for byte.
func appendManagedBlock(body string, block []string) string {
	kept := stripManagedBlocks(splitLines(body))
	// Drop trailing blank lines left by a removed block, then append ours.
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	out := strings.Join(kept, "\n")
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + strings.Join(block, "\n") + "\n"
}

// stripManagedBlocks removes every Bunker-managed block and returns the other
// lines untouched, in order. A non-block line is never inspected further: the
// decision to remove a line is made by managedBlockStart/blockMember, which
// recognise only Bunker's marker and Bunker's own line shapes.
func stripManagedBlocks(lines []string) []string {
	kept := make([]string, 0, len(lines))
	skip := 0
	for _, r := range managedBlockRanges(lines) {
		kept = append(kept, lines[skip:r[0]]...)
		skip = r[1]
	}
	kept = append(kept, lines[skip:]...)
	return kept
}

// managedBlockRanges returns the [start,end) index pairs of every Bunker-managed
// block in lines.
func managedBlockRanges(lines []string) [][2]int {
	var out [][2]int
	for i := 0; i < len(lines); i++ {
		if !managedBlockStart(lines[i]) {
			continue
		}
		start := i
		for i+1 < len(lines) && blockMember(lines[i+1]) {
			i++
		}
		out = append(out, [2]int{start, i + 1})
	}
	return out
}

// managedBlockStart reports whether a line BEGINS a Bunker-managed block: the
// marker comment, or Bunker's own classifier line (which also lets a block
// whose marker was edited away be repaired instead of duplicated). Nothing
// else — in particular not a bare `session required pam_namespace.so`, which is
// a perfectly ordinary operator or distribution rule — starts one.
func managedBlockStart(line string) bool {
	n := normalizeLine(line)
	if n == "" {
		return false
	}
	if n == normalizeLine(namespaceMarker) {
		return true
	}
	return n == normalizeLine(NamespacePAMClassifierLine())
}

// blockMember reports whether a line is part of the Bunker block that the
// preceding managedBlockStart opened: the marker, Bunker's own classifier and
// verifier lines (any group/helper path, so a stale block is recognised), the
// module line, the first revision's legacy lines, and — only inside a Bunker
// block — a bare or optioned pam_namespace module line left behind by a
// half-edited install.
func blockMember(line string) bool {
	n := normalizeLine(line)
	switch {
	case n == "":
		return false
	case n == normalizeLine(namespaceMarker):
		return true
	case n == normalizeLine(NamespacePAMClassifierLine()):
		return true
	case strings.HasPrefix(n, namespaceVerifyPrefix()):
		return true
	case n == normalizeLine(NamespacePAMModuleLine):
		return true
	// Legacy (first revision, rejected): the module line carried the fail-open
	// option, and the guard line was group-keyed. Both are matched EXACTLY —
	// an operator's or the distribution's optioned pam_namespace rule is not
	// ours and must never be deleted.
	case n == normalizeLine(NamespacePAMModuleLine+" ignore_config_error"):
		return true
	case strings.HasPrefix(n, legacyGuardPrefix()):
		return true
	}
	return false
}

// namespaceVerifyPrefix is the normalized prefix of every Bunker verifier
// line; the helper path and the group follow it.
func namespaceVerifyPrefix() string {
	return normalizeLine("session " + namespacePAMVerifyControl + " pam_exec.so quiet")
}

// legacyGuardPrefix is the normalized prefix of the FIRST revision's guard
// line, kept so an upgrade cleans it up. Only Bunker's own shape — with its
// exact control field — is matched: an operator's `pam_succeed_if` rule (for
// example `user notingroup …`) is never matched and never removed.
func legacyGuardPrefix() string {
	return normalizeLine("session [success=ok auth_err=1 default=ignore] pam_succeed_if.so quiet user ingroup")
}

// deployedVerifyGroup returns the group a Bunker verifier line ALREADY in the
// sshd stack passes to the helper, or "" when there is none. It exists so
// config drift is visible: a daemon configured with agent_group X while the
// host's block names Y would leave every agent session denied by the helper's
// own group check.
func deployedVerifyGroup(lines []string) string {
	for _, line := range lines {
		n := normalizeLine(line)
		if !strings.HasPrefix(n, namespaceVerifyPrefix()) {
			continue
		}
		fields := strings.Fields(n)
		return fields[len(fields)-1]
	}
	return ""
}

// hasManagedDrift reports whether body carries any Bunker-managed line beyond
// the canonical block for these options (a stale group name or helper path, or
// a legacy line). Only lines INSIDE a Bunker block are considered: an
// operator's own pam_namespace or pam_succeed_if rule elsewhere in the file is
// not Bunker's business and must not force a rewrite.
func hasManagedDrift(body string, canonicalBlock []string) bool {
	canonical := map[string]bool{}
	for _, l := range canonicalBlock {
		canonical[normalizeLine(l)] = true
	}
	lines := splitLines(body)
	for _, r := range managedBlockRanges(lines) {
		for _, line := range lines[r[0]:r[1]] {
			if !canonical[normalizeLine(line)] {
				return true
			}
		}
	}
	return false
}

// RemoveTmpNamespace removes Bunker's namespace.d drop-in, strips the managed
// block from the sshd PAM stack, and deletes the pam_exec helper and its
// manifest. This is the ONLY path that restores the previous shared-/tmp
// behavior: the drop-in is what the precondition requires, so deleting that
// file alone leaves the block in place and every `bunker-*` session is DENIED
// (see NamespaceConf).
//
// Order matters in the opposite direction from install: the block goes FIRST,
// so the verifier stops denying before the drop-in it proves disappears, and
// the file that invokes the helper never exists without it. Nothing Bunker did
// not add is removed or rewritten; a foreign pam_namespace line is left in
// place and reported.
func (o Options) RemoveTmpNamespace(ctx context.Context) (*Report, error) {
	o = o.WithDefaults()
	rep := &Report{}

	// 1. The PAM block, before anything it depends on.
	body, err := os.ReadFile(o.SSHDConfigPath)
	if err != nil {
		return rep, fmt.Errorf("read %s: %w", o.SSHDConfigPath, err)
	}
	if !hasManagedBlock(string(body)) {
		rep.Add("skip", o.SSHDConfigPath, "no Bunker-managed PAM block present", false)
	} else {
		lines := splitLines(string(body))
		kept := stripManagedBlocks(lines)
		// Collapse nothing else: the managed lines are appended at the end of
		// the file, so removing them restores the original bytes exactly
		// whenever the original ended with a newline (the normal case for a
		// PAM config).
		updated := strings.Join(kept, "\n")
		if !strings.HasSuffix(updated, "\n") {
			updated += "\n"
		}
		mode := os.FileMode(0o644)
		if fi, err := os.Stat(o.SSHDConfigPath); err == nil {
			mode = fi.Mode().Perm()
		}
		if _, err := writeFileIdempotent(o.SSHDConfigPath, []byte(updated), mode); err != nil {
			return rep, err
		}
		rep.Add("write", o.SSHDConfigPath, "Bunker-managed PAM block removed", true)

		// An operator-owned pam_namespace line is not ours to delete, but
		// leaving it silently would make "uninstalled" a lie — say so.
		for _, line := range splitLines(updated) {
			if strings.Contains(line, "pam_namespace.so") {
				rep.Add("note", o.SSHDConfigPath, "a non-Bunker pam_namespace line is still present: "+strings.TrimSpace(line), false)
			}
		}
	}

	// 2. The drop-in.
	if _, err := os.Stat(o.NamespaceConfPath()); err == nil {
		if err := os.Remove(o.NamespaceConfPath()); err != nil {
			return rep, fmt.Errorf("remove %s: %w", o.NamespaceConfPath(), err)
		}
		rep.Add("remove", o.NamespaceConfPath(), "pam_namespace drop-in removed", true)
	} else {
		rep.Add("skip", o.NamespaceConfPath(), "no drop-in present", false)
	}

	// 3. The helper and its manifest, last.
	for _, path := range []string{o.PamHelperManifestPath(), o.PamHelperPath()} {
		if _, err := os.Stat(path); err != nil {
			rep.Add("skip", path, "absent", false)
			continue
		}
		if err := os.Remove(path); err != nil {
			return rep, fmt.Errorf("remove %s: %w", path, err)
		}
		rep.Add("remove", path, "pam_exec precondition helper removed", true)
	}
	// The directory only goes when Bunker put nothing else in it.
	if _, err := os.Stat(o.PamHelperDir); err == nil {
		if err := os.Remove(o.PamHelperDir); err == nil {
			rep.Add("remove", o.PamHelperDir, "empty helper directory removed", true)
		}
	}
	return rep, nil
}

// hasManagedBlock reports whether a config file carries a Bunker-managed
// block.
func hasManagedBlock(body string) bool {
	for _, line := range splitLines(body) {
		if managedBlockStart(line) {
			return true
		}
	}
	return false
}

// EnsureAgentTmpInstance pre-creates an agent's /tmp instance directory
// (0700, owned by the agent). pam_namespace would create it on demand (mode
// 1777, owned by root, mirroring /tmp), but pre-creating it makes the ownership
// and mode deterministic and lets the isolation be verified before the first
// session opens.
func (o Options) EnsureAgentTmpInstance(ctx context.Context, agentID, username string, uid, gid int) (*Report, error) {
	o = o.WithDefaults()
	rep := &Report{}
	if agentID == "" || username == "" {
		return rep, fmt.Errorf("agent id and username are required")
	}
	dir := o.TmpInstanceDir(agentID)
	if err := os.MkdirAll(o.TmpInstanceRoot, 0o700); err != nil {
		return rep, fmt.Errorf("create tmp instance parent %s: %w", o.TmpInstanceRoot, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return rep, fmt.Errorf("create tmp instance dir %s: %w", dir, err)
	}
	// Restrict the parent LAST: pam_namespace requires mode 0000, and it must
	// not be 0000 while we are still creating the instance directory under it.
	if err := os.Chmod(o.TmpInstanceRoot, 0o000); err != nil {
		return rep, fmt.Errorf("chmod tmp instance parent: %w", err)
	}
	if _, err := o.run(ctx, "chown", fmt.Sprintf("%d:%d", uid, gid), dir); err != nil {
		return rep, fmt.Errorf("chown tmp instance dir: %w", err)
	}
	if _, err := o.run(ctx, "chmod", "0700", dir); err != nil {
		return rep, fmt.Errorf("chmod tmp instance dir: %w", err)
	}
	rep.Add("ok", dir, "private /tmp instance directory (0700)", true)
	return rep, nil
}

// RemoveAgentTmpInstance removes an agent's /tmp instance directory. Contents
// are the agent's private /tmp data, so they go away with the agent.
func (o Options) RemoveAgentTmpInstance(ctx context.Context, agentID string) (*Report, error) {
	o = o.WithDefaults()
	rep := &Report{}
	dir := o.TmpInstanceDir(agentID)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			rep.Add("skip", dir, "tmp instance directory absent", false)
			return rep, nil
		}
		return rep, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return rep, fmt.Errorf("remove tmp instance dir %s: %w", dir, err)
	}
	rep.Add("remove", dir, "tmp instance directory removed", true)
	return rep, nil
}

// splitLines splits a config file into lines without the line terminator.
func splitLines(body string) []string {
	if body == "" {
		return nil
	}
	trimmed := strings.TrimSuffix(body, "\n")
	return strings.Split(trimmed, "\n")
}

// normalizeLine collapses whitespace so PAM lines compare independent of the
// spacing an operator (or an older Bunker version) used.
func normalizeLine(line string) string {
	return strings.Join(strings.Fields(line), " ")
}
