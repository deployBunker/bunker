package egress

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// stmts helpers live in firewall.go; this file carries the backend choice,
// the executor seam wiring and the manager lifecycle.

// netLookupHost is the production resolver seam (var: tests swap it when
// they cannot inject the full Manager).
var netLookupHost = func(host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(context.Background(), host)
}

// execLookPath is the PATH-probe seam.
var execLookPath = exec.LookPath

// nftBinaryPresent / iptablesBinaryPresent are the backend PATH probes
// (vars: the test seams). exec.LookPath only — no command is run, so the
// open-mode zero-invocation law is unaffected: these are consulted only
// inside backendChoice, which only Install/Sweep on a restricted mode reach.
var (
	nftBinaryPresent      = func() bool { return lookPath("nft") }
	iptablesBinaryPresent = func() bool { return lookPath("iptables") }
)

func lookPath(binary string) bool {
	_, err := execLookPath(binary)
	return err == nil
}

// ModePolicy is the resolved, validated egress policy for one agent spawn —
// what the daemon carries between config resolution and rule installation.
type ModePolicy struct {
	// Mode is one of the enforced/open vocabulary values (never empty).
	Mode string
	// Allowlist is the raw config entries (CIDRs / IPs / hostnames) for
	// allowlist mode. Empty for open and none.
	Allowlist []string
}

// Enforced reports whether the policy installs a chain at spawn.
func (p ModePolicy) Enforced() bool { return p.Mode == ModeAllowlist || p.Mode == ModeNone }

// Manager installs and removes per-agent egress policy through an injected
// Executor (the seam that makes every claim in this package testable without
// root) and keeps the backend choice process-stable.
type Manager struct {
	exec     Executor
	resolver LookupIP

	// backendResolved/once pin the nft-vs-iptables choice for the process:
	// a spawn that installed under nftables must be removed under nftables.
	backendOnce sync.Once
	backend     string
	backendErr  error

	// mu serializes installs/removals/sweeps: two concurrent spawns must not
	// interleave the probe-then-create sequences against the shared table.
	mu sync.Mutex
}

// NewManager wires the production manager: real commands, the system
// resolver.
func NewManager() *Manager {
	return &Manager{exec: ExecExecutor{}, resolver: defaultResolver{}}
}

// NewManagerWith wires a test manager (executor + resolver seams).
func NewManagerWith(exec Executor, resolver LookupIP) *Manager {
	return &Manager{exec: exec, resolver: resolver}
}

// defaultResolver adapts net.DefaultResolver to LookupIP.
type defaultResolver struct{}

// LookupHost delegates to the net default resolver.
func (defaultResolver) LookupHost(host string) ([]string, error) {
	return netLookupHost(host)
}

// Resolve derives the spawn's effective policy: the per-spawn request
// (empty = defer) wins over the daemon-wide config default, exactly the
// netmode ResolveNetworkMode precedence shape, minus an env rung (no
// operator asked for one; adding it later is additive).
//
// An unknown name from either source is a named hard error — never a silent
// fallback to open. The allowlist rides the daemon config (the admin's
// managed list); a per-spawn request does not carry its own allowlist.
func (m *Manager) Resolve(requested, configMode string, configAllowlist []string) (ModePolicy, error) {
	src := strings.TrimSpace(requested)
	if src == "" {
		src = strings.TrimSpace(configMode)
		srcSource := "agent.egress.mode config"
		if strings.TrimSpace(requested) != "" {
			srcSource = "spawn request"
		}
		if src == "" {
			return ModePolicy{Mode: DefaultMode}, nil
		}
		if !ValidMode(src) {
			return ModePolicy{}, fmt.Errorf("%s: unknown egress mode %q (valid: %v)", srcSource, src, ValidModes())
		}
		return ModePolicy{Mode: src, Allowlist: trimAll(configAllowlist)}, nil
	}
	if !ValidMode(src) {
		return ModePolicy{}, fmt.Errorf("spawn request: unknown egress mode %q (valid: %v)", src, ValidModes())
	}
	return ModePolicy{Mode: src, Allowlist: trimAll(configAllowlist)}, nil
}

// trimAll returns the non-empty trimmed entries, preserving order.
func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Install enforces policy for a freshly spawned agent and is the LOUD
// failure point (requirement 4): any error here aborts the spawn —
// internal/agent rolls the agent back — so an agent never runs unenforced
// while its config claims it is restricted.
//
// Open mode installs nothing and touches nothing: zero Executor
// invocations, pinned by TestInstall_OpenModeZeroFirewallCalls.
//
// Install is idempotent per (uid, policy): a retry after a mid-install
// failure flushes and rebuilds the chain instead of stacking rules.
func (m *Manager) Install(policy ModePolicy, uid uint32) error {
	switch policy.Mode {
	case ModeOpen:
		return nil
	case ModeAllowlist, ModeNone:
		// enforced: fall through
	default:
		return fmt.Errorf("egress: refusing unknown mode %q (valid: %v)", policy.Mode, SortedValidModes())
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	spec, err := buildChainSpec(policy.Mode, uid, policy.Allowlist, m.resolver)
	if err != nil {
		return fmt.Errorf("egress %s spec for uid %d: %w", policy.Mode, uid, err)
	}

	argvs, err := m.installArgvs(spec)
	if err != nil {
		return fmt.Errorf("egress %s for uid %d: %w", policy.Mode, uid, err)
	}
	for _, argv := range argvs {
		out, err := m.exec.Run(argv)
		if err == nil {
			continue
		}
		if tolerateProbe(argv, out, err) {
			continue
		}
		return fmt.Errorf("egress %s install for uid %d failed at %q: %w (output: %s)",
			policy.Mode, uid, strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// tolerateProbe reports whether a failed step was a probe whose "not found"
// outcome is the expected path (missing table/chain/jump on first install).
// A probe failure carrying any OTHER error (permission denied, binary
// missing) is a real refusal — never tolerated.
func tolerateProbe(argv []string, out []byte, err error) bool {
	if len(argv) < 2 {
		return false
	}
	switch argv[0] {
	case "nft":
		// probes: "nft list table X" / "nft list chain X Y"; the
		// not-found output ends with "No such file or directory."
		if argv[1] != "list" {
			return false
		}
		return strings.Contains(string(out), "No such file or directory")
	case "iptables":
		// probes: "iptables -nL CHAIN" (missing chain) and
		// "iptables -C OUTPUT <jump>" (rule absent). Both exit non-zero
		// with empty-ish output on the expected-absent path; a real
		// refusal (permission, missing table) says so in words.
		if len(argv) >= 2 && argv[1] == "-nL" {
			text := strings.ToLower(string(out))
			return strings.Contains(text, "no such chain") ||
				strings.Contains(text, "no chain/target/match by that name") ||
				strings.TrimSpace(string(out)) == ""
		}
		if len(argv) >= 2 && argv[1] == "-C" {
			// -C exits 1 with (typically) empty output when the rule is
			// absent; anything naming a real fault (permission denied,
			// "table does not exist") must refuse.
			text := strings.ToLower(string(out))
			if strings.Contains(text, "permission denied") ||
				strings.Contains(text, "could not fetch rule set") ||
				strings.Contains(text, "table does not exist") {
				return false
			}
			return true
		}
		return false
	}
	return false
}

// backendChoice resolves (once per process) whether this host enforces via
// nftables or falls back to iptables: nft is preferred and chosen when the
// binary answers; otherwise the iptables path. The resolution failure (neither
// binary usable) is returned by every later install.
func (m *Manager) backendChoice() (string, error) {
	m.backendOnce.Do(func() {
		if nftBinaryPresent() {
			m.backend = "nft"
			return
		}
		if iptablesBinaryPresent() {
			m.backend = "iptables"
			return
		}
		m.backendErr = fmt.Errorf("egress: neither nft nor iptables is available on this host — refusing to install an unenforced policy (valid modes: %v)", SortedValidModes())
	})
	return m.backend, m.backendErr
}

// installArgvs renders the backend sequence for spec (nft preferred, iptables
// fallback — requirement 1).
func (m *Manager) installArgvs(spec chainSpec) ([][]string, error) {
	backend, err := m.backendChoice()
	if err != nil {
		return nil, err
	}
	switch backend {
	case "nft":
		// One-shot declarations first (table + hook chain via `nft -f`),
		// then the per-agent sequence.
		argvs := [][]string{
			{"nft", "-f", NFTHookDeclFile()},
		}
		argvs = append(argvs, nftInstallArgvs(spec)...)
		return argvs, nil
	case "iptables":
		return iptablesInstallArgvs(spec), nil
	default:
		return nil, fmt.Errorf("egress: unknown backend %q", backend)
	}
}

// NFTHookDeclFile renders the `nft -f` payload carrying the table + hook
// chain declaration (idempotent: `add table`/`add chain` statements — nft
// tolerates re-adding an existing table/chain in a -f payload only when the
// chain is EMPTY, which the hook is at declaration time on every retry).
func NFTHookDeclFile() string {
	return "table ip " + EgressTableName + "\n" + NFTHookChainDecl
}

// Remove lifts the per-agent policy at destroy (requirement 2). Best-effort
// by contract at the destroy call site, but the removal itself reports the
// first hard error after attempting every step (the jump disconnect FIRST —
// cutting the hook routing is what stops enforcement-relevant packets — then
// flush + delete of the chain).
//
// Open mode removes nothing (zero invocations — the open-mode law).
func (m *Manager) Remove(uid uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.removeLocked(uid)
}

func (m *Manager) removeLocked(uid uint32) error {
	backend, err := m.backendChoice()
	if err != nil {
		return err
	}
	switch backend {
	case "nft":
		// Discover the jump rule handles, delete them, then flush+delete
		// the chain. A listing failure is fatal for the removal verdict —
		// a chain we cannot list we cannot honestly claim to have removed.
		out, err := m.exec.Run(nftArgv("list-chain", HookChainName))
		if err != nil {
			// No hook chain: nothing of this agent's is hooked either.
			if strings.Contains(string(out), "No such file or directory") {
				return nil
			}
			return fmt.Errorf("egress remove uid %d: list hook chain: %w", uid, err)
		}
		handles := ParseNFTJumpHandles(out, uid)
		var firstErr error
		for _, argv := range nftDestroyArgvs(uid, handles) {
			o, rerr := m.exec.Run(argv)
			if rerr != nil && firstErr == nil && !tolerateProbe(argv, o, rerr) {
				firstErr = fmt.Errorf("egress remove uid %d failed at %q: %w (output: %s)",
					uid, strings.Join(argv, " "), rerr, strings.TrimSpace(string(o)))
			}
		}
		return firstErr
	case "iptables":
		var firstErr error
		for _, argv := range iptablesDestroyArgvs(uid) {
			o, rerr := m.exec.Run(argv)
			if rerr != nil && firstErr == nil && !tolerateProbe(argv, o, rerr) {
				firstErr = fmt.Errorf("egress remove uid %d failed at %q: %w (output: %s)",
					uid, strings.Join(argv, " "), rerr, strings.TrimSpace(string(o)))
			}
		}
		return firstErr
	default:
		return fmt.Errorf("egress: unknown backend %q", backend)
	}
}

// SweepRemoval describes one stale per-agent chain the sweep removed.
type SweepRemoval struct {
	UID     uint32
	Backend string
}

// Sweep is the orphan reconciliation for egress (requirement 2): it lists
// every per-agent chain the backend holds and removes the ones whose uid is
// not in liveUIDs — stale chains from agents destroyed while the daemon was
// down (or whose destroy-time removal failed). Live agents' chains are left
// strictly alone; the shared hook chain and the table are never touched.
//
// Sweep is MECHANICAL: whether a daemon runs it at all is the caller's
// policy (internal/agent gates it on the configured egress policy and the
// tracked agents' enforced modes — in a pure-open fleet nothing of ours has
// ever been in the firewall, so there is nothing to reconcile). A listing
// failure that is not "no table" is returned; a missing table means no
// orphans by definition.
func (m *Manager) Sweep(liveUIDs map[uint32]bool) ([]SweepRemoval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	backend, err := m.backendChoice()
	if err != nil {
		return nil, err
	}
	var removed []SweepRemoval
	var firstErr error
	record := func(r SweepRemoval) { removed = append(removed, r) }

	switch backend {
	case "nft":
		out, err := m.exec.Run(nftArgv("list-table"))
		if err != nil {
			// No table: no orphans, nothing to do.
			if strings.Contains(string(out), "No such file or directory") {
				return nil, nil
			}
			return nil, fmt.Errorf("egress sweep: list table: %w", err)
		}
		for _, chain := range ParseNFTTableChains(out) {
			uid, ok := ParseChainUID(chain)
			if !ok || liveUIDs[uid] {
				continue
			}
			if err := m.removeLocked(uid); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			record(SweepRemoval{UID: uid, Backend: backend})
		}
	case "iptables":
		out, err := m.exec.Run([]string{"iptables-save"})
		if err != nil {
			return nil, fmt.Errorf("egress sweep: iptables-save: %w", err)
		}
		for _, chain := range ParseIPTablesSaveChains(out) {
			uid, ok := ParseChainUID(chain)
			if !ok || liveUIDs[uid] {
				continue
			}
			if err := m.removeLocked(uid); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			record(SweepRemoval{UID: uid, Backend: backend})
		}
	default:
		return nil, fmt.Errorf("egress: unknown backend %q", backend)
	}
	return removed, firstErr
}

// anyEnforcedHint is retired: the open-mode gate for the sweep lives at the
// agent layer (egressShouldSweep), which knows the configured policy and the
// tracked agents' enforced modes — a per-process hint cannot survive a
// restart, and the daemon-start sweep (requirement 2) must run exactly when
// an earlier process may have left chains behind.

// UIDFromUserString converts a user database uid string ("61392") for the
// call sites that hold uids as strings. A non-numeric uid is a caller bug —
// the error names the value (the DF-BUNKER-63 convention of never guessing).
func UIDFromUserString(s string) (uint32, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("uid %q is not numeric: %w", s, err)
	}
	return uint32(v), nil
}
