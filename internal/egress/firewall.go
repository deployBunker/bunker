package egress

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// ── Executor seam ────────────────────────────────────────────────────────
//
// Every firewall mutation funnels through Executor.Run. Production wires
// exec.Command; tests inject a RecordingExecutor — which is also the seam
// that pins "open mode never invokes a firewall helper" (assert zero
// invocations) and lets allowlist/none behavior be exercised without root.

// Executor runs one backend command. Run returns the command's combined
// output alongside its error (the codebase's exec.CombinedOutput convention)
// so refusals carry the backend's own text.
type Executor interface {
	Run(argv []string) ([]byte, error)
}

// ExecExecutor runs argv on the host. The production implementation.
type ExecExecutor struct{}

// Run executes argv and returns its combined output.
func (ExecExecutor) Run(argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("egress: empty command argv")
	}
	if len(argv) >= 3 && argv[0] == "nft" && argv[1] == "-f" {
		// File-based `nft -f <path>` loads: pipe the file in as stdin via
		// `nft -f -` (the documented stdin form) instead of handing the
		// secret-bearing path to the nft process, where it would sit in
		// argv for the lifetime of the command — visible to any local
		// process listing (ps, /proc/<pid>/cmdline) while nft parses a
		// file it can just as well read from the pipe.
		data, err := os.ReadFile(argv[2])
		if err != nil {
			return nil, fmt.Errorf("egress: read nft payload %s: %w", argv[2], err)
		}
		stdin := bytes.NewReader(data)
		cmd := exec.Command("nft", "-f", "-")
		cmd.Stdin = stdin
		return cmd.CombinedOutput()
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	return cmd.CombinedOutput()
}

// RecordedCall is one Run invocation, kept for assertions.
type RecordedCall struct {
	Argv   []string
	Err    error
	Output []byte
}

// RecordingExecutor is the test double: every invocation is appended (argv +
// outcome) for assertions — including invocations whose programmed failure
// the caller then surfaces, so zero-invocation assertions stay honest even
// on failure paths.
type RecordingExecutor struct {
	Calls  []RecordedCall
	FailOn func(call int, argv []string) error
}

// Run appends the invocation and returns the programmed outcome.
func (r *RecordingExecutor) Run(argv []string) ([]byte, error) {
	var err error
	if r.FailOn != nil {
		err = r.FailOn(len(r.Calls), argv)
	}
	call := RecordedCall{Argv: append([]string(nil), argv...), Err: err}
	if err != nil {
		call.Output = []byte("programmed failure")
	}
	r.Calls = append(r.Calls, call)
	return call.Output, err
}

// Count reports the number of invocations recorded so far.
func (r *RecordingExecutor) Count() int { return len(r.Calls) }

// ── Allow targets ────────────────────────────────────────────────────────

// allowTarget is one resolved allowlist destination: an nftables/iptables
// destination prefix. Domains are resolved at rule-install time
// (requirement 1) — the durable rule is always an IP rule, and
// docs/egress-policy.md records that a rotating DNS answer goes stale until
// the next reinstall.
type allowTarget struct {
	prefix netip.Prefix
	source string // the original config entry (error text only)
}

// classifyAllowTarget turns one config entry into its resolved form: a CIDR,
// a bare IP, or a domain name to resolve. The name check is deliberately
// conservative (letters, digits, dots, hyphens — no underscores, no scheme,
// no port): anything else is a config error at load, not an install-time
// surprise.
func classifyAllowTarget(entry string) (allowTarget, error) {
	e := strings.TrimSpace(entry)
	if e == "" {
		return allowTarget{}, fmt.Errorf("empty allowlist entry")
	}
	if strings.Contains(e, "/") {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			return allowTarget{}, fmt.Errorf("invalid CIDR %q: %w", e, err)
		}
		return allowTarget{prefix: p.Masked(), source: e}, nil
	}
	if p, err := netip.ParseAddr(e); err == nil {
		return allowTarget{prefix: netip.PrefixFrom(p, p.BitLen()), source: e}, nil
	}
	// An IPv4-lookalike that failed to parse (10.0.0.999, 1.2.3.4.5) is a
	// TYPO, not a hostname: every label is numeric, so the hostname grammar
	// below would happily "resolve" it into garbage. Refuse at load.
	if isIPv4Lookalike(e) {
		return allowTarget{}, fmt.Errorf("invalid allowlist entry %q: looks like an IP but does not parse", e)
	}
	if !validDomainName(e) {
		return allowTarget{}, fmt.Errorf("invalid allowlist entry %q: not an IP, CIDR or plausible hostname", e)
	}
	return allowTarget{source: e}, nil
}

// isIPv4Lookalike reports whether s is dot-separated numeric labels (the
// shape of an IPv4 literal) — used to refuse typos before the hostname
// grammar can accept them.
func isIPv4Lookalike(s string) bool {
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !isDigits(l) {
			return false
		}
	}
	return true
}

// validDomainName is the syntactic hostname gate for allowlist entries.
func validDomainName(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	labels := strings.Split(strings.TrimSuffix(s, "."), ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			case r >= 'A' && r <= 'Z':
			case r == '-':
				if i == 0 || i == len(label)-1 {
					return false // a leading/trailing hyphen is never valid
				}
			default:
				return false
			}
		}
	}
	return true
}

// LookupIP is the resolver seam (production: a net.DefaultResolver adapter;
// tests: StaticResolver).
type LookupIP interface {
	LookupHost(host string) ([]string, error)
}

// StaticResolver is a test LookupIP over a table.
type StaticResolver map[string][]string

// LookupHost returns the table's addresses or a synthetic error.
func (s StaticResolver) LookupHost(host string) ([]string, error) {
	if addrs, ok := s[strings.ToLower(host)]; ok {
		return addrs, nil
	}
	return nil, fmt.Errorf("no such host %s", host)
}

// resolveAllowTargets resolves every entry to IP prefixes. Domain entries
// fail LOUDLY here (Install refuses the spawn) when unresolvable — a policy
// that cannot state its own accepts must never install a silently weaker
// chain.
func resolveAllowTargets(allowlist []string, resolver LookupIP) ([]allowTarget, error) {
	out := make([]allowTarget, 0, len(allowlist))
	seen := make(map[string]bool, len(allowlist))
	for _, entry := range allowlist {
		t, err := classifyAllowTarget(entry)
		if err != nil {
			return nil, err
		}
		if t.prefix.IsValid() {
			key := t.prefix.String()
			if !seen[key] {
				seen[key] = true
				out = append(out, t)
			}
			continue
		}
		addrs, err := resolver.LookupHost(t.source)
		if err != nil {
			return nil, fmt.Errorf("resolve allowlist domain %q: %w", t.source, err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("resolve allowlist domain %q: no addresses", t.source)
		}
		for _, a := range addrs {
			addr, perr := netip.ParseAddr(strings.TrimSpace(a))
			if perr != nil {
				continue
			}
			key := netip.PrefixFrom(addr, addr.BitLen()).String()
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, allowTarget{prefix: netip.PrefixFrom(addr, addr.BitLen()), source: t.source})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("allowlist resolved to zero destinations")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].prefix.String() < out[j].prefix.String() })
	return out, nil
}

// ValidAllowlistEntry reports whether entry is a usable allowlist entry: a
// CIDR, a bare IP, or a plausible hostname (resolved at rule-install time).
// It is the config-load validator (internal/config re-uses it) — a syntactic
// check only; DNS reachability is install-time.
func ValidAllowlistEntry(entry string) error {
	if _, err := classifyAllowTarget(entry); err != nil {
		return err
	}
	return nil
}

// ── Chain rendering (pure; pinned by tests) ──────────────────────────────

// chainSpec is everything one chain install needs, derived from mode +
// allowlist. Building it is pure so tests pin the exact rule set per mode.
type chainSpec struct {
	Mode   string
	UID    uint32
	Accept []allowTarget
}

// buildChainSpec derives the install payload: the allowlist is resolved for
// allowlist mode (loud refusal on any failure), skipped entirely for none.
func buildChainSpec(mode string, uid uint32, allowlist []string, resolver LookupIP) (chainSpec, error) {
	spec := chainSpec{Mode: mode, UID: uid}
	switch mode {
	case ModeAllowlist:
		targets, err := resolveAllowTargets(allowlist, resolver)
		if err != nil {
			return spec, err
		}
		spec.Accept = targets
	case ModeNone:
		// The deny-all chain carries no accepts by definition.
	default:
		return spec, fmt.Errorf("egress mode %q has no chain spec (valid: %v)", mode, SortedValidModes())
	}
	return spec, nil
}

// nftChainStatements renders the per-agent chain's nftables statements, in
// order. The LAST statement is the counted default drop: packets that reach
// it are dropped and COUNTED (a bound nobody can observe is not a bound —
// the netmode §5.2 law), and the counter doubles as the operator's "how much
// did policy block?" read.
//
// Loopback and established/related are accepted in EVERY enforced mode; the
// established/related accept is exactly what keeps the bunker control
// channel (the agent's inbound sshd session, its unix-socket docker control)
// working in allowlist and none modes — see the package comment.
func (s chainSpec) nftChainStatements() []string {
	stmts := []string{
		"ip daddr 127.0.0.0/8 counter accept",
		"ct state invalid counter drop",
		"ct state established,related counter accept",
	}
	switch s.Mode {
	case ModeAllowlist:
		for _, t := range s.Accept {
			if t.prefix.Addr().Is4() {
				stmts = append(stmts, "ip daddr "+t.prefix.String()+" counter accept")
			}
		}
	case ModeNone:
		// no accepts: everything not accepted above hits the default drop
	}
	stmts = append(stmts, "counter drop")
	return stmts
}

// nftJumpStatement is the output-hook rule that routes the agent uid's
// outbound packets into its chain. The uid match (meta skuid) lives HERE, in
// the hook — the per-agent chain only ever sees packets the jump routed, and
// removal of the jump (Remove's first step) disconnects the policy even
// before the chain itself goes away.
func nftJumpStatement(uid uint32) string {
	return fmt.Sprintf("meta skuid %d jump %s", uid, ChainName(uid))
}

// nftArgv builds one `nft` argv. statements carry the rule body; kind
// selects the verb.
func nftArgv(kind string, statements ...string) []string {
	argv := []string{"nft"}
	switch kind {
	case "add-table":
		argv = append(argv, "add", "table", "ip", EgressTableName)
	case "add-chain":
		// statements[0] is the chain name.
		argv = append(argv, "add", "chain", "ip", EgressTableName, statements[0])
	case "flush-chain":
		argv = append(argv, "flush", "chain", "ip", EgressTableName, statements[0])
	case "delete-chain":
		argv = append(argv, "delete", "chain", "ip", EgressTableName, statements[0])
	case "add-rule":
		// statements[0] chain, statements[1:] the rule body.
		argv = append(argv, "add", "rule", "ip", EgressTableName, statements[0])
		argv = append(argv, statements[1:]...)
	case "delete-rule":
		// statements[0] chain, statements[1] handle id.
		argv = append(argv, "delete", "rule", "ip", EgressTableName, statements[0], "handle", statements[1])
	case "list-chain":
		argv = append(argv, "-a", "list", "chain", "ip", EgressTableName, statements[0])
	case "list-table":
		argv = append(argv, "list", "table", "ip", EgressTableName)
	default:
		panic("egress: unknown nft verb " + kind)
	}
	return argv
}

// nftInstallArgvs renders the full nftables install sequence for one agent,
// in execution order. Every step is idempotent against a partially-completed
// earlier attempt (probe-then-create / flush-then-add) so a spawn retry after
// a mid-install failure converges instead of accumulating duplicate rules.
func nftInstallArgvs(spec chainSpec) [][]string {
	chain := ChainName(spec.UID)
	argvs := [][]string{
		nftArgv("list-table"),                // probe: does the table exist?
		nftArgv("add-table"),                 // tolerated EEXIST
		nftArgv("list-chain", HookChainName), // probe: does the hook exist?
		nftArgv("add-chain", HookChainName),  // tolerated EEXIST
		nftArgv("add-chain", chain),          // per-agent chain (tolerated EEXIST)
		nftArgv("flush-chain", chain),        // deterministic rule set on retry
		nftArgv("add-rule", HookChainName, nftJumpStatement(spec.UID)),
	}
	for _, st := range spec.nftChainStatements() {
		argvs = append(argvs, nftArgv("add-rule", chain, st))
	}
	return argvs
}

// nftDestroyArgvs renders the removal sequence: the jump rule first (via
// handles parsed from a `nft -a list chain` listing — Remove discovers them),
// then flush + delete of the per-agent chain. The hook chain and the table
// are SHARED infrastructure and are deliberately never deleted here.
func nftDestroyArgvs(uid uint32, jumpHandles []string) [][]string {
	chain := ChainName(uid)
	argvs := make([][]string, 0, len(jumpHandles)+2)
	for _, h := range jumpHandles {
		argvs = append(argvs, nftArgv("delete-rule", HookChainName, h))
	}
	argvs = append(argvs,
		nftArgv("flush-chain", chain),
		nftArgv("delete-chain", chain),
	)
	return argvs
}

// HookChainName is the shared output-hook base chain in the egress table:
// every agent's jump rule lives here. The declaration (hook + priority) is
// fixed for the process and pinned by the TestNFTHookDeclaration test.
const HookChainName = "output_hook"

// NFTHookChainDecl is the `nft -f` fragment that creates the hook chain.
// priority filter - 10 places the hook AHEAD of the standard filter chain
// (conntrack state is available at any priority — conntrack runs at -200),
// policy accept means a hook that exists but carries no jumps is a no-op for
// every other uid: an agent whose jump was removed has NO egress hook, which
// is exactly the not-yet-enforced / already-removed state.
const NFTHookChainDecl = "table ip " + EgressTableName + " {\n" +
	"\tchain " + HookChainName + " {\n" +
	"\t\ttype filter hook output priority filter - 10; policy accept;\n" +
	"\t}\n" +
	"}\n"

// NFTHookChainCreateArgv installs the hook declaration.
func NFTHookChainCreateArgv(loadFile string) []string {
	return []string{"nft", "-f", loadFile}
}

// ParseNFTChainHandles extracts the rule handles of one chain from a
// `nft -a list chain` listing. Only lines carrying the numeric-handle marker
// "# handle <n>" are returned; the caller filters by statement content
// (ParseNFTJumpHandles).
func ParseNFTChainHandles(listing []byte) []string {
	var out []string
	for _, line := range strings.Split(string(listing), "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, "# handle ")
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(line[idx+len("# handle "):])
		if rest == "" {
			continue
		}
		if end := strings.IndexAny(rest, " \t"); end >= 0 {
			rest = rest[:end]
		}
		if isDigits(rest) {
			out = append(out, rest)
		}
	}
	return out
}

// ParseNFTJumpHandles returns the handles of the jump rules for uid within a
// `nft -a list chain <hook>` listing (the listing text of each rule is the
// jump statement's rendering).
func ParseNFTJumpHandles(listing []byte, uid uint32) []string {
	want := nftJumpStatement(uid)
	var out []string
	for _, line := range strings.Split(string(listing), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, want) {
			continue
		}
		if idx := strings.Index(line, "# handle "); idx >= 0 {
			rest := strings.TrimSpace(line[idx+len("# handle "):])
			if end := strings.IndexAny(rest, " \t"); end >= 0 {
				rest = rest[:end]
			}
			if isDigits(rest) {
				out = append(out, rest)
			}
		}
	}
	return out
}

// ParseNFTTableChains lists the chain names in a `nft list table` listing.
func ParseNFTTableChains(listing []byte) []string {
	var out []string
	for _, line := range strings.Split(string(listing), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "chain ") {
			name := strings.TrimPrefix(t, "chain ")
			if end := strings.IndexAny(name, " \t{"); end >= 0 {
				name = name[:end]
			}
			if name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// isDigits reports whether s is a non-empty ASCII digit string.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ── iptables fallback ────────────────────────────────────────────────────
//
// The fallback is chosen when the nft binary is missing from PATH (resolved
// once per process, test-overridable through NFTBinaryPresent). Rule CONTENT
// is equivalent; the jump is an INSERT into OUTPUT at position 1 so the
// per-agent policy evaluates ahead of any other OUTPUT rules on the host.
// The fallback is IPv4-only (documented in docs/egress-policy.md): IPv6
// egress stays unrestricted on an iptables-fallback host — an honest,
// loudly-documented gap of the fallback path, never a claim in the boundary
// strings.

// IPTablesChainStatements renders the per-agent chain's iptables rule
// bodies, in append order. Same content contract as the nft render.
func (s chainSpec) IPTablesChainStatements() []string {
	stmts := []string{
		"-m conntrack --ctstate INVALID -j DROP",
		"-m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"-d 127.0.0.0/8 -j ACCEPT",
	}
	switch s.Mode {
	case ModeAllowlist:
		for _, t := range s.Accept {
			if t.prefix.Addr().Is4() {
				stmts = append(stmts, "-d "+t.prefix.String()+" -j ACCEPT")
			}
		}
	case ModeNone:
		// deny-all: no accepts
	}
	stmts = append(stmts, "-j DROP")
	return stmts
}

// iptablesJumpSpec is the OUTPUT-chain jump rule body routing the agent uid
// into its chain (the iptables spelling of nftJumpStatement).
func iptablesJumpSpec(uid uint32) string {
	return "-m owner --uid-owner " + strconv.FormatUint(uint64(uid), 10) + " -j " + IPTablesChainName(uid)
}

// iptablesInstallArgvs renders the full iptables install sequence for one
// agent, in execution order (probe-then-create / flush-then-add, the same
// idempotence contract as the nft sequence).
func iptablesInstallArgvs(spec chainSpec) [][]string {
	chain := IPTablesChainName(spec.UID)
	jump := iptablesJumpSpec(spec.UID)
	argvs := [][]string{
		{"iptables", "-nL", chain},              // probe: chain exists?
		{"iptables", "-N", chain},               // tolerated "chain exists"
		{"iptables", "-C", "OUTPUT", jump},      // probe: jump present?
		{"iptables", "-I", "OUTPUT", "1", jump}, // tolerated "exists"
		{"iptables", "-F", chain},               // deterministic rule set on retry
	}
	for _, st := range spec.IPTablesChainStatements() {
		argvs = append(argvs, append([]string{"iptables", "-A", chain}, strings.Fields(st)...))
	}
	return argvs
}

// iptablesDestroyArgvs renders the removal sequence: the jump rule first,
// then flush + delete of the per-agent chain. Every step is best-effort
// (Remove reports the first hard error but always attempts the rest).
func iptablesDestroyArgvs(uid uint32) [][]string {
	chain := IPTablesChainName(uid)
	jump := iptablesJumpSpec(uid)
	return [][]string{
		{"iptables", "-D", "OUTPUT", jump},
		{"iptables", "-F", chain},
		{"iptables", "-X", chain},
	}
}

// ParseIPTablesSaveChains lists this package's chain names from an
// `iptables-save` dump (lines of the form `:BUNKER-EGRESS-<uid> - [0:0]`).
func ParseIPTablesSaveChains(dump []byte) []string {
	var out []string
	for _, line := range strings.Split(string(dump), "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, ":") {
			continue
		}
		name := strings.TrimPrefix(t, ":")
		if end := strings.IndexAny(name, " \t"); end >= 0 {
			name = name[:end]
		}
		if strings.HasPrefix(name, ipTablesChainPrefix) {
			out = append(out, name)
		}
	}
	return out
}
