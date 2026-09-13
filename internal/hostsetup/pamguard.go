package hostsetup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// The agent-only fail-closed precondition (GAP-075 rework).
//
// pam_namespace cannot express "this host must have a Bunker /tmp rule": with
// no matching polydir it returns PAM_SUCCESS and the session keeps the shared
// host /tmp (pam_namespace.c: pam_sm_open_session() only calls
// setup_namespace() when a polydir matched). The module also cannot express
// "only agent sessions": ./namespace.conf's fourth field is a comma-separated
// list of exact user names, resolved with getpwnam() (process_line() in
// pam_namespace.c), so it can neither name a group nor follow dynamically
// created agents.
//
// Bunker therefore enforces both properties in the PAM block:
//
//	session  [success=2 auth_err=ignore default=die]  pam_succeed_if.so quiet user !~ bunker-*
//	session  [success=ignore default=die]             pam_exec.so quiet <helper> verify <group>
//	session  required                                 pam_namespace.so
//
// The classifier answers the agent question from the reserved username
// pattern alone. The verifier — this file's helper, a root-owned script in a
// root-owned directory — then proves the boundary really is provisioned
// before pam_namespace is allowed to run. pam_exec maps exit 0 to PAM_SUCCESS
// and ANY other outcome (non-zero exit, signal, failed execve) to
// PAM_SYSTEM_ERR, so `default=die` turns every failure INTO A DENIED SESSION.
//
// The helper is managed like the rest of the boundary: written atomically into
// a root-owned 0755 directory, root-owned 0755, content hashed into a manifest
// beside it, idempotent, removed by --uninstall, and reported by --status. At
// session time the helper verifies its whole TRUST CHAIN — the helper
// directory, the manifest (owner, and no group/world write bit) and its own
// bytes against that manifest — because a writable directory or manifest would
// let a local agent swap both files. A hand edit is drift: the manifest no
// longer matches, the helper denies, and every agent session fails closed until
// Bunker repairs it.

// agentGroupRe is the accepted shape of a configurable group name. Anything
// else is REJECTED rather than rendered into a PAM line, a config file or the
// helper script: a name with whitespace, a newline, a control character, a
// leading '-', or a PAM/shell metacharacter could otherwise inject a module
// line, a shell word or a second rule.
var agentGroupRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,30}$`)

// systemPathRe is the accepted shape of a configurable absolute path embedded
// into PAM/config/helper content. Quotes, whitespace, control characters,
// globs, '~', '$', backticks and backslashes are all rejected.
var systemPathRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)

// ValidateAgentGroup rejects a group name that is not safe to embed in the
// PAM block, the namespace configuration or the helper script.
func ValidateAgentGroup(group string) error {
	if !agentGroupRe.MatchString(group) {
		return fmt.Errorf("unsafe agent group name %q: group names are embedded into /etc/pam.d/sshd and the pam_exec helper, so they must match %s", group, agentGroupRe.String())
	}
	return nil
}

// ValidateSystemPath rejects a path that is not safe to embed in the PAM
// block, a config file or the helper script.
func ValidateSystemPath(what, p string) error {
	if p == "" {
		return fmt.Errorf("%s is empty", what)
	}
	if !systemPathRe.MatchString(p) {
		return fmt.Errorf("unsafe %s %q: paths are embedded into PAM and helper content, so they must be absolute and free of whitespace, quotes, control characters, '~', '$', backticks and globs", what, p)
	}
	if strings.Contains(p, "//") {
		return fmt.Errorf("unsafe %s %q: repeated '/'", what, p)
	}
	for _, element := range strings.Split(p, "/") {
		if element == ".." {
			return fmt.Errorf("unsafe %s %q: '..' path element", what, p)
		}
	}
	return nil
}

// validateTmpNamespaceInputs validates every configurable value that this
// package embeds into PAM, namespace configuration or helper content.
// Callers must run it BEFORE writing anything.
func (o Options) validateTmpNamespaceInputs() error {
	if err := ValidateAgentGroup(o.AgentGroup); err != nil {
		return err
	}
	for _, p := range []struct{ what, path string }{
		{"private /tmp instance root", o.TmpInstanceRoot},
		{"pam_namespace drop-in directory", o.NamespaceConfDir},
		{"pam_namespace drop-in name", o.NamespaceConfPath()},
		{"sshd PAM stack path", o.SSHDConfigPath},
		{"helper path", o.PamHelperPath()},
		{"helper manifest path", o.PamHelperManifestPath()},
		{"namespace configuration directory", o.NamespaceConfDir},
		{"namespace configuration file", o.NamespaceConfFile},
	} {
		if err := ValidateSystemPath(p.what, p.path); err != nil {
			return err
		}
	}
	// An empty module path means "pam_namespace.so was not found on this host",
	// which the installer reports with its own, more precise error just before
	// writing anything.
	if o.PamHelperModulePath != "" {
		if err := ValidateSystemPath("pam_namespace module path", o.PamHelperModulePath); err != nil {
			return err
		}
	}
	for _, dir := range strings.Split(o.PamHelperToolPath, ":") {
		if err := ValidateSystemPath("helper tool PATH entry", dir); err != nil {
			return err
		}
	}
	return nil
}

// guardScriptTokens are the placeholders PamGuardScript replaces. Token
// replacement (not Sprintf) keeps the script text free to contain '%'.
const (
	tokAgentGroup      = "@AGENT_GROUP@"
	tokAgentPattern    = "@AGENT_PATTERN@"
	tokAgentPatternRaw = "@AGENT_PATTERN_RAW@"
	tokRequiredRule    = "@REQUIRED_RULE@"
	tokDropIn          = "@DROPIN@"
	tokInstanceRoot    = "@INSTANCE_ROOT@"
	tokNamespaceFile   = "@NAMESPACE_FILE@"
	tokNamespaceDir    = "@NAMESPACE_DIR@"
	tokNamespaceMod    = "@NAMESPACE_MODULE@"
	tokHelperPath      = "@HELPER@"
	tokHelperDir       = "@HELPER_DIR@"
	tokManifestPath    = "@MANIFEST@"
	tokLogTag          = "@LOG_TAG@"
	tokDropInFileName  = "@DROPIN_NAME@"
	tokToolPath        = "@TOOL_PATH@"
)

// guardScriptTemplate is the helper's source. It is deliberately plain POSIX
// sh: pam_exec runs it with the privileges of the calling process (sshd, i.e.
// root) and with the PAM environment plus the PAM items pam_exec exports
// (PAM_USER among them), so the script must not depend on bash, on a writable
// tmp, or on anything it can be talked out of.
const guardScriptTemplate = `#!/bin/sh
# Bunker GAP-075 — pam_namespace fail-closed precondition.
#
# MANAGED FILE. ` + "`bunker host-provision --apply`" + ` writes it and
# ` + "`bunker host-provision --uninstall --apply`" + ` removes it. Its sha256 is
# recorded in the manifest beside it; if the two disagree the helper denies and
# every @AGENT_PATTERN@ session fails CLOSED rather than running on a host whose
# /tmp boundary cannot be proven.
#
# Invoked from /etc/pam.d/sshd, AFTER the ` + "`@AGENT_PATTERN@`" + ` classifier and
# immediately before pam_namespace.so:
#
#   session    [success=ignore default=die]    pam_exec.so quiet @HELPER@ verify @AGENT_GROUP@
#
# pam_exec exits 0 -> PAM_SUCCESS (the control field continues to
# pam_namespace); any other exit, a signal, or a failed execve -> PAM_SYSTEM_ERR,
# which the control field maps to ` + "`die`" + `: the session is denied.
#
# Checks, all of them fail-closed:
#   1. identity is unambiguous (exactly one PAM_USER in the environment);
#   2. non-agent sessions are left to the PAM stack (nothing to verify);
#   3. the agent group exists and PAM_USER is a member of it;
#   4. the Bunker drop-in exists, is root-owned, is not group/world writable,
#      and declares EXACTLY the required /tmp rule;
#   5. no other namespace configuration file declares a /tmp polydir, and every
#      effective line of the configuration set is structurally valid;
#   6. the instance parent exists, is root-owned and is mode 000;
#   7. the required pam_namespace module is installed;
#   8. the TRUST CHAIN is intact: the helper directory and the manifest are
#      root-owned and not group/world writable (otherwise a local agent could
#      replace both files by rename/unlink), the manifest still matches the
#      helper's content, and the helper itself is root-owned and not
#      group/world writable. A drifted helper denies.

set -u

# The helper decides the boundary, so it must not resolve a single command
# through an inherited PATH (pam_exec hands the child the PAM environment).
PATH='@TOOL_PATH@'
export PATH

MODE=${1:-}
GROUP_ARG=${2:-}

GROUP='@AGENT_GROUP@'
AGENT_PATTERN='@AGENT_PATTERN@'
REQUIRED_RULE='@REQUIRED_RULE@'
DROPIN='@DROPIN@'
DROPIN_NAME='@DROPIN_NAME@'
INSTANCE_ROOT='@INSTANCE_ROOT@'
NAMESPACE_CONF='@NAMESPACE_FILE@'
NAMESPACE_DIR='@NAMESPACE_DIR@'
MODULE='@NAMESPACE_MODULE@'
HELPER='@HELPER@'
HELPER_DIR='@HELPER_DIR@'
MANIFEST='@MANIFEST@'
LOG_TAG='@LOG_TAG@'

fail() {
    echo "$LOG_TAG: $1" >&2
    logger -t "$LOG_TAG" -p auth.err "$1" 2>/dev/null || true
    exit 1
}

[ "$MODE" = verify ] || fail "unknown mode '$MODE' (only 'verify' is supported)"

# (8) self-integrity first: a helper that does not match its manifest must not
# be trusted to make any other decision. The trust chain is DIRECTORY ->
# MANIFEST -> HELPER, and every link is verified before the link's content is
# used: a helper directory or manifest the agent group (or the world) can write
# lets a local agent replace BOTH the root-owned helper and its manifest by
# rename/unlink, which would make every check below meaningless.
[ -d "$HELPER_DIR" ] || fail "helper directory $HELPER_DIR is missing"
[ -z "$(find "$HELPER_DIR" -maxdepth 0 -perm /022 -print 2>/dev/null)" ] || fail "helper directory $HELPER_DIR is group/world writable"
[ -f "$MANIFEST" ] || fail "helper manifest $MANIFEST is missing"
[ -z "$(find "$MANIFEST" -perm /022 -print 2>/dev/null)" ] || fail "helper manifest $MANIFEST is group/world writable"
if [ "$(id -u)" = 0 ]; then
    [ "$(stat -c '%u:%g' "$HELPER_DIR" 2>/dev/null)" = 0:0 ] || fail "helper directory $HELPER_DIR is not root-owned"
    [ "$(stat -c '%u:%g' "$MANIFEST" 2>/dev/null)" = 0:0 ] || fail "helper manifest $MANIFEST is not root-owned"
fi
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is not available"
expected=$(head -n 1 "$MANIFEST" 2>/dev/null)
[ -n "$expected" ] || fail "helper manifest $MANIFEST is empty"
actual=$(sha256sum "$HELPER" 2>/dev/null | cut -d' ' -f1)
[ "$actual" = "$expected" ] || fail "helper content drifted from its manifest ($HELPER)"
[ -z "$(find "$HELPER" -perm /022 -print 2>/dev/null)" ] || fail "helper $HELPER is group/world writable"
if [ "$(id -u)" = 0 ]; then
    [ "$(stat -c '%u:%g' "$HELPER" 2>/dev/null)" = 0:0 ] || fail "helper $HELPER is not root-owned"
fi

# (1) identity: pam_exec APPENDS PAM_USER to the PAM environment, so a second
# PAM_USER (which a PAM-environment-writing module could have planted) means
# the name this check would use is ambiguous.
user_count=$(env 2>/dev/null | grep -c '^PAM_USER=' || true)
[ "$user_count" = 1 ] || fail "expected exactly one PAM_USER in the environment, found ${user_count}"
pam_user=${PAM_USER:-}
[ -n "$pam_user" ] || fail "PAM_USER is empty"

# (2) only agent-named sessions carry the boundary. Anything else reached this
# module because the classifier could not answer; the stack decides, not us.
case $pam_user in
    @AGENT_PATTERN_RAW@) : ;;
    *) exit 0 ;;
esac

# The block and the helper must agree on the group, or the host is mid-upgrade.
[ "$GROUP_ARG" = "$GROUP" ] || fail "the PAM block keys on '$GROUP_ARG' but this helper provisions '$GROUP' (re-run bunker host-provision --apply)"

# (3) the group must exist and the session user must be a member. A missing
# group is NOT "not a member" here: it is a broken boundary, and it denies.
getent group "$GROUP" >/dev/null 2>&1 || fail "agent group $GROUP does not exist"
if ! id -nG "$pam_user" 2>/dev/null | tr ' ' '\n' | grep -qx "$GROUP"; then
    fail "user $pam_user is not a member of $GROUP"
fi

# (4) the drop-in must exist, be ours alone, and declare exactly the required
# /tmp rule. A deleted drop-in, a valid-but-different rule, and a malformed
# rule are all denials: pam_namespace would return SUCCESS for the first two
# and keep the shared host /tmp.
[ -f "$DROPIN" ] || fail "namespace drop-in $DROPIN is missing"
[ -z "$(find "$DROPIN" -perm /022 -print 2>/dev/null)" ] || fail "namespace drop-in $DROPIN is group/world writable"
if [ "$(id -u)" = 0 ]; then
    [ "$(stat -c '%u:%g' "$DROPIN" 2>/dev/null)" = 0:0 ] || fail "namespace drop-in $DROPIN is not root-owned"
fi
rules=0
while IFS= read -r raw; do
    line=${raw%%#*}
    set -f
    set -- $line
    set +f
    [ $# -gt 0 ] || continue
    rules=$((rules + 1))
    [ "$*" = "$REQUIRED_RULE" ] || fail "drop-in rule is not the required Bunker /tmp rule: $line"
done < "$DROPIN"
[ "$rules" = 1 ] || fail "namespace drop-in must declare exactly one rule, found $rules"

# (5) the whole configuration set pam_namespace reads: no other /tmp polydir,
# and no line it cannot parse (a malformed line ANYWHERE aborts the module for
# every session, so this host cannot be trusted to isolate /tmp).
for f in "$NAMESPACE_CONF" "$NAMESPACE_DIR"/*.conf; do
    [ -f "$f" ] || continue
    while IFS= read -r raw; do
        line=${raw%%#*}
        set -f
        set -- $line
        set +f
        [ $# -gt 0 ] || continue
        [ $# -ge 3 ] || fail "$f has a malformed line: $line"
        if [ "$1" = /tmp ] && [ "${f##*/}" != "$DROPIN_NAME" ]; then
            fail "$f also declares a /tmp polydir"
        fi
    done < "$f"
done

# (6) instance parent: pam_namespace requires root-owned mode 000
# (check_inst_parent() in pam_namespace.c) or the session dies anyway.
[ -d "$INSTANCE_ROOT" ] || fail "instance parent $INSTANCE_ROOT is missing"
inst_mode=$(stat -c '%a' "$INSTANCE_ROOT" 2>/dev/null || echo "")
[ -n "$inst_mode" ] || fail "cannot stat instance parent $INSTANCE_ROOT"
[ "$inst_mode" -eq 0 ] || fail "instance parent $INSTANCE_ROOT mode is $inst_mode, want 000"
if [ "$(id -u)" = 0 ]; then
    [ "$(stat -c '%u:%g' "$INSTANCE_ROOT" 2>/dev/null)" = 0:0 ] || fail "instance parent $INSTANCE_ROOT is not root-owned"
fi

# (7) the module the block activates must be present.
[ -f "$MODULE" ] || fail "required PAM module $MODULE is missing"

exit 0
`

// PamGuardScript renders the helper for these options. It returns an error
// instead of rendering when any embedded value is unsafe.
func (o Options) PamGuardScript() (string, error) {
	o = o.WithDefaults()
	if err := o.validateTmpNamespaceInputs(); err != nil {
		return "", err
	}
	module := o.PamHelperModulePath
	if module == "" {
		return "", fmt.Errorf("the pam_exec precondition cannot be rendered: pam_namespace.so was not found in %s", strings.Join(namespaceModulePaths, ", "))
	}
	// The template already quotes every embedded value (`NAME='@TOKEN@'`), and
	// every value has passed ValidateAgentGroup/ValidateSystemPath, which admit
	// no quote, whitespace or control character — so the replacements are raw.
	replacer := strings.NewReplacer(
		tokAgentGroup, o.AgentGroup,
		tokAgentPattern, NamespacePAMAgentPattern,
		// An unquoted case pattern: quoted, `bunker-*` would match a literal
		// name. The value is a package constant, never caller input.
		tokAgentPatternRaw, NamespacePAMAgentPattern,
		// The rule is compared against the drop-in with whitespace collapsed
		// (`set -- $line; [ "$*" = "$REQUIRED_RULE" ]`), so the constant is
		// rendered already collapsed: equivalent spacing in the file is
		// accepted, a different rule is not.
		tokRequiredRule, strings.Join(strings.Fields(NamespaceConfRule(o.TmpInstanceRoot)), " "),
		tokDropIn, o.NamespaceConfPath(),
		tokDropInFileName, path.Base(o.NamespaceConfPath()),
		tokInstanceRoot, o.TmpInstanceRoot,
		tokNamespaceFile, o.NamespaceConfFile,
		tokNamespaceDir, o.NamespaceConfDir,
		tokNamespaceMod, module,
		tokHelperPath, o.PamHelperPath(),
		tokHelperDir, o.PamHelperDir,
		tokManifestPath, o.PamHelperManifestPath(),
		tokLogTag, "bunker-pam-guard",
		tokToolPath, o.PamHelperToolPath,
	)
	script := replacer.Replace(guardScriptTemplate)
	if strings.Contains(script, "@") {
		return "", fmt.Errorf("internal error: unsubstituted token in the rendered helper")
	}
	return script, nil
}

// PamGuardManifest renders the manifest content for a helper script: the
// sha256 of its bytes, so the helper can detect its own drift at session time
// and the installer can detect it without a session.
func PamGuardManifest(script string) string {
	sum := sha256.Sum256([]byte(script))
	return hex.EncodeToString(sum[:]) + "\n"
}

// PamGuardScriptHash returns the sha256 of a helper script, as the manifest
// records it and as the status report compares it.
func PamGuardScriptHash(script string) string {
	sum := sha256.Sum256([]byte(script))
	return hex.EncodeToString(sum[:])
}
