// Per-agent egress policy lifecycle (GAP-134, REQ-E1 / SEC-06):
// internal/egress owns the vocabulary and the firewall mechanics; this file
// wires them into the agent lifecycle — install at spawn (AFTER dockerd is
// verified up, BEFORE the agent is registered/tracked as running), remove at
// destroy (best-effort compensating step, budgeted like the others), and the
// stale-chain sweep that rides the reconciliation pass.
//
// The failure law (requirement 4): a failed install in allowlist/none mode
// FAILS the spawn — the standard rollback removes everything the spawn
// created — so an agent is never left running unenforced while its config
// claims it is restricted. Open mode installs nothing and touches nothing:
// zero Executor invocations, pinned by the seam tests in gap134_test.go.
package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/deployBunker/bunker/internal/egress"
)

// egressResolve is the spawn-side resolver (Step 1e): per-spawn request >
// agent.egress.mode config > the declared default (open), through the
// internal/egress vocabulary — a typo is a named hard error, never a silent
// fallback to open. Nil-manager (hand-built test manager) resolution still
// VALIDATES the mode against the vocabulary; only the firewall effects are
// disabled.
func (m *AgentManager) egressResolve(requested string) (egress.ModePolicy, error) {
	cfgMode := ""
	var allowlist []string
	if m.cfg != nil {
		cfgMode = m.cfg.Agent.Egress.Mode
		allowlist = m.cfg.Agent.Egress.Allowlist
	}
	if m.egressMgr == nil {
		// Vocabulary-only resolution: identical refusal law, no effects.
		mode, err := egress.Resolve(firstNonEmptyEgress(requested, cfgMode))
		if err != nil {
			return egress.ModePolicy{}, err
		}
		pol := egress.ModePolicy{Mode: mode}
		if mode == egress.ModeAllowlist {
			pol.Allowlist = allowlist
		}
		return pol, nil
	}
	return m.egressMgr.Resolve(requested, cfgMode, allowlist)
}

// egressInstall runs the rule installation for one spawn (Step 5d.5). A nil
// manager degrades to open behavior; an enforced policy with a nil manager
// is a LOUD refusal (requirement 4) — never a silent skip.
func (m *AgentManager) egressInstall(policy egress.ModePolicy, uid uint32) error {
	if m.egressMgr == nil {
		if policy.Enforced() {
			// Unreachable in production (NewAgentManager always wires the
			// manager), but the refusal is the honest shape for the
			// hand-built-manager degradation: fail loud, never silently
			// skip an enforcement step the config promised.
			return &egressNilManagerError{mode: policy.Mode}
		}
		return nil
	}
	return m.egressMgr.Install(policy, uid)
}

// egressNilManagerError is the enforced-policy-with-nil-manager refusal.
type egressNilManagerError struct{ mode string }

// Error renders the refusal with the mode named.
func (e *egressNilManagerError) Error() string {
	return "egress mode " + e.mode + " is configured but the egress manager is unavailable on this server — refusing the spawn, not leaving the agent unenforced"
}

// egressRemove is the destroy-side compensating step: remove the per-agent
// chain. Open mode and nil-manager removes nothing (zero invocations).
// Best-effort at the call site: the error is logged, the destroy proceeds —
// the periodic sweep is the backstop for a failed removal, and the stale
// chain keeps enforcing against a uid that no longer exists (fail safe).
func (m *AgentManager) egressRemove(username string) error {
	if m.egressMgr == nil {
		return nil
	}
	u, err := lookupAgentUser(username)
	if err != nil {
		// The user record is already gone: no uid → no chain this package
		// could have keyed on it. The sweep covers pre-removal leftovers.
		return nil
	}
	uid, uerr := egress.UIDFromUserString(u.Uid)
	if uerr != nil {
		return fmt.Errorf("egress remove %s: %w", username, uerr)
	}
	return m.egressMgr.Remove(uid)
}

// firstNonEmptyEgress returns the first non-empty (post-trim) value.
func firstNonEmptyEgress(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// egressSweepInterval is the periodic stale-chain sweep cadence: the same
// 1-minute tick the TTL reaper rides, which bounds a stale chain's lifetime
// to roughly one tick after its agent's user record disappears.
const egressSweepInterval = 1 * time.Minute

// egressShouldSweep is the OPEN-MODE GATE for the stale-chain sweep (the
// zero-behavior-change law, requirement 1): a daemon whose configured policy
// is open AND whose tracked agents carry no enforced mode has never had a
// chain in the firewall — nothing of ours is there to reconcile, so the
// sweep never runs and never touches the firewall. Any enforced signal (the
// daemon-wide config names allowlist/none, or any tracked agent record
// stamps an enforced mode — including records replayed from a previous
// process that ran restricted) arms the sweep. The gate is deliberately
// restart-safe: it reads durable/config state, not process memory.
func (m *AgentManager) egressShouldSweep() bool {
	if m.cfg != nil {
		switch strings.TrimSpace(m.cfg.Agent.Egress.Mode) {
		case egress.ModeAllowlist, egress.ModeNone:
			return true
		}
	}
	if m.tracker != nil {
		for _, rec := range m.tracker.List() {
			if rec == nil {
				continue
			}
			if rec.EgressMode == egress.ModeAllowlist || rec.EgressMode == egress.ModeNone {
				return true
			}
		}
	}
	return false
}

// startEgressSweeper runs the periodic stale-chain reconciliation (GAP-134
// requirement 2): at daemon start (after reconciliation restored the tracker
// — the same grace the TTL reaper honors) and every egressSweepInterval
// thereafter. Exits when ttlStop closes. A nil manager (open mode /
// hand-built tests) never starts a goroutine at all — zero firewall
// interaction in open mode, structurally.
func (m *AgentManager) startEgressSweeper() {
	if m.egressMgr == nil {
		return
	}
	go func() {
		// The first pass runs AFTER reconciliation so a replayed live set
		// protects its chains before the first sweep decides anything
		// (the reaper's grace contract).
		select {
		case <-m.reconcileDone:
		case <-time.After(30 * time.Second):
			m.logger.Warn("egress sweeper starting without completed reconciliation", "grace", (30 * time.Second).String())
		case <-m.ttlStop:
			return
		}
		// Daemon-start pass (requirement 2: run at daemon start).
		m.egressSweepStaleChains()
		ticker := time.NewTicker(egressSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.egressSweepStaleChains()
			case <-m.ttlStop:
				return
			}
		}
	}()
}

// egressSweepStaleChains removes stale per-agent egress chains (GAP-134
// requirement 2): every chain whose uid does not belong to a live managed
// agent is an orphan — an agent destroyed while the daemon was down, or one
// whose destroy-time removal failed — and is removed. Live agents' chains
// are left strictly alone; the shared hook chain and the table are never
// touched (they are infrastructure, and removing them would unhook every
// other agent's policy).
//
// The live-uid set comes from the passwd walk (the same authority the orphan
// sweep itself uses), so the two sweeps can never disagree about who is
// alive. Open mode sweeps nothing: with no enforcement configured the daemon
// owns no chains, and probing/mutating the firewall would break the
// zero-behavior-change law (requirement 1) — pinned by the seam tests.
//
// Errors are logged by the caller (the reconciliation pass treats the sweep
// as best-effort — the NEXT pass retries; a stale chain's policy continues
// to be enforced against a uid that no longer exists, which fails safe).
func (m *AgentManager) egressSweepStaleChains() {
	if m.egressMgr == nil {
		return
	}
	// Open-mode gate (requirement 1): a pure-open daemon has never touched
	// the firewall; the sweep must not even LIST it. Any enforced signal in
	// config or the durable records arms the sweep.
	if !m.egressShouldSweep() {
		return
	}
	systemAgents, err := m.listSystemAgents()
	if err != nil {
		m.logger.Warn("egress sweep skipped: cannot enumerate system agents", "error", err)
		return
	}
	live := make(map[uint32]bool, len(systemAgents))
	for _, sa := range systemAgents {
		u, lerr := lookupAgentUser(sa.Username)
		if lerr != nil {
			// The user vanished between the walk and the lookup: no uid to
			// protect — the chain (if any) is stale by definition.
			m.logger.Warn("egress sweep: cannot resolve uid for system agent; its chains will be treated as stale",
				"agent_id", sa.AgentID, "username", sa.Username, "error", lerr)
			continue
		}
		uid, uerr := egress.UIDFromUserString(u.Uid)
		if uerr != nil {
			m.logger.Warn("egress sweep: non-numeric uid for system agent; skipping (fail closed)",
				"agent_id", sa.AgentID, "username", sa.Username, "error", uerr)
			continue
		}
		live[uid] = true
	}
	removed, serr := m.egressMgr.Sweep(live)
	if serr != nil {
		m.logger.Warn("egress sweep completed with errors", "removed", len(removed), "error", serr)
	}
	for _, r := range removed {
		m.logger.Info("egress sweep removed stale chain",
			"uid", r.UID, "backend", r.Backend)
	}
}
