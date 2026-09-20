package agent

import (
	"os"
	"sort"
	"strings"
)

// ── DF-BUNKER-21 (AC5): the residue inventory ──────────────────────────────
//
// QA-BUNKER-19 left bunker-las-03 holding 11 orphan bunker-* users, 0
// registered agents and ~2.5GB under /home while NO API surface reported any of
// it: `bunker status` showed "Agents: 0/N" and nothing else, and `bunker linger`
// only counted linger totals. An operator could not see the leak the daemon had
// created, let alone tell a rolled-back spawn from a clean host.
//
// ResidueInventory answers that question from the host planes themselves — the
// same planes spawn and the rollback create and remove — and reports the four
// counts the board criterion names: orphan users, homes, keys and linger
// entries. It is a PROBE, not a bookkeeping counter: every count is derived by
// reading the plane now, and a plane that cannot be read is reported as such
// (see Status) instead of being silently reported as zero.
//
// ORPHAN means: present on the host and unknown to the daemon — neither tracked
// live (the in-memory tracker the API reports) nor known durably (the GAP-070
// registry). That definition is what makes the count useful after a failed
// spawn: a rolled-back agent is gone from both sets, so any leftover it produced
// counts, while a healthy agent's user/home/key counts zero.

// Residue inventory probe status values. They mirror the tmp_isolation reporting
// convention: an operator always gets either real numbers or an explicit reason
// why the numbers are not available.
const (
	// ResidueStatusOK: every plane was probed.
	ResidueStatusOK = "ok"
	// ResidueStatusPartial: at least one plane could not be read, so the
	// reported counts are a lower bound (see Detail for which planes).
	ResidueStatusPartial = "partial"
	// ResidueStatusUnavailable: no plane could be read at all — typically a
	// daemon running as a user that cannot read the user database or the agent
	// directories. The counts must not be read as "clean host".
	ResidueStatusUnavailable = "unavailable"
)

// ResidueInventory is the host-residue report ServerInfo carries.
type ResidueInventory struct {
	// OrphanUsers counts managed (bunker-*) system users unknown to the daemon.
	OrphanUsers int
	// OrphanHomes counts managed home directories (agentHomeRoot/bunker-*)
	// whose agent is unknown to the daemon. A home whose user is gone is
	// residue too: `userdel` without -r leaves exactly that behind.
	OrphanHomes int
	// OrphanKeys counts persisted agent SSH private keys (cfg.Agent.SSHDir)
	// whose agent is unknown to the daemon.
	OrphanKeys int
	// StaleLinger counts systemd linger entries whose user no longer exists on
	// the host — the state that makes logind resurrect a user manager for a
	// user that is gone (INT-SPAWN-001). Since INT-HOST-004 the plane is NOT
	// name-scoped: logind acts on every entry in the directory, so a stale
	// entry whose name drifted from the managed bunker- pattern (e.g.
	// bunter-1d2b6cef) is counted too. Managed entries the daemon still knows
	// and entries whose user still exists never count, matching what
	// host-local `bunker linger` reports.
	StaleLinger int
	// Registered is the number of agents the daemon knows (tracker + durable
	// registry). It is the denominator that makes the counts interpretable:
	// "11 orphan users / 0 registered" is the QA-BUNKER-19 fingerprint.
	Registered int
	// Status is one of the ResidueStatus* values.
	Status string
	// Detail names every plane that could not be probed and why. Empty when
	// Status is ResidueStatusOK.
	Detail string
}

// Total is the sum of the four residue counts.
func (r ResidueInventory) Total() int {
	return r.OrphanUsers + r.OrphanHomes + r.OrphanKeys + r.StaleLinger
}

// daemonKnowsAgent reports whether the daemon recognises agentID as one of its
// own: live in the tracker or present in the durable registry. Anything else — a
// user a previous daemon created, an agent whose registration never completed
// because the spawn was cancelled, a name that only exists on disk — is
// residue. It is deliberately WIDER than knownAgent (registry-only, which
// answers the destroy-idempotency question): an agent the tracker holds live is
// not residue even if the registry is disabled.
func (m *AgentManager) daemonKnowsAgent(agentID string) bool {
	if m.tracker != nil && m.tracker.Get(agentID) != nil {
		return true
	}
	// A daemon without a registry cannot know an agent durably, so nothing is
	// "known" through that path (the pre-GAP-070 behaviour).
	return m.registry != nil && m.registry.Known(agentID)
}

// ResidueInventory probes the host planes and reports the residue counts. It is
// read-only and never fails: an unreadable plane is named in Status/Detail so a
// caller cannot mistake "I could not look" for "nothing there".
func (m *AgentManager) ResidueInventory() ResidueInventory {
	inv := ResidueInventory{Status: ResidueStatusOK}
	inv.Registered = m.countRegistered()
	var failures []string

	// ── users ────────────────────────────────────────────────────────────
	listFn := m.listSystemAgents
	if listFn == nil {
		listFn = defaultListSystemAgents
	}
	systemAgents, err := listFn()
	if err != nil {
		failures = append(failures, "users: "+err.Error())
	} else {
		for _, sa := range systemAgents {
			if !m.daemonKnowsAgent(sa.AgentID) {
				inv.OrphanUsers++
			}
		}
	}

	// ── homes ────────────────────────────────────────────────────────────
	homes, err := listAgentDirEntries(agentHomeRoot, agentUserPrefix)
	if err != nil {
		failures = append(failures, "homes: "+err.Error())
	} else {
		for _, id := range homes {
			if !m.daemonKnowsAgent(id) {
				inv.OrphanHomes++
			}
		}
	}

	// ── keys ─────────────────────────────────────────────────────────────
	keys, err := listAgentDirEntries(m.cfg.Agent.SSHDir, "")
	if err != nil {
		failures = append(failures, "keys: "+err.Error())
	} else {
		for _, id := range keys {
			if !m.daemonKnowsAgent(id) {
				inv.OrphanKeys++
			}
		}
	}

	// ── linger ───────────────────────────────────────────────────────────
	// INT-HOST-004: this plane is deliberately NOT name-scoped. Until this fix
	// it listed entries through listAgentDirEntries(lingerDir, agentUserPrefix),
	// which silently dropped every entry not named bunker-<valid-id> — measured
	// on bunker-mvp as 50 stale reported while host-local `bunker linger` (which
	// applies no prefix filter, see internal/cli/linger.go scanLingerDir)
	// counted 51 at the same instant, with `bunter-1d2b6cef` on disk. logind
	// acts on EVERY entry in the linger directory, so the residue probe scans
	// the raw directory and classifies by user existence, exactly like the CLI:
	// a managed entry the daemon still knows is not stale (the exemption is
	// checked FIRST, before the user lookup, so a healthy agent's linger entry
	// can never read as residue even on a host where its user vanished mid-probe);
	// everything else is stale when its user no longer exists and ignored when
	// the user is real. A missing directory is an empty plane, not a failure.
	lingerNames, err := lingerRawDirNames(lingerDir)
	if err != nil {
		failures = append(failures, "linger: "+err.Error())
	} else {
		for _, name := range lingerNames {
			if id, ok := strings.CutPrefix(name, agentUserPrefix); ok && validAgentID.MatchString(id) && m.daemonKnowsAgent(id) {
				continue
			}
			// lookupUser reports the user database through user.Lookup,
			// which ERRORS when the user does not exist — the same
			// translation every in-repo consumer applies to this seam
			// (manager_destroy.go treats err != nil as the user being
			// gone; internal/cli lingerUserExists maps it to the prune's
			// target class). Error = user absent = stale linger.
			if _, lookupErr := lookupUser(name); lookupErr != nil {
				inv.StaleLinger++
			}
		}
	}

	switch {
	case len(failures) == 0:
		inv.Status = ResidueStatusOK
	case len(failures) == residuePlaneCount:
		inv.Status = ResidueStatusUnavailable
	default:
		inv.Status = ResidueStatusPartial
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		inv.Detail = strings.Join(failures, "; ")
	}
	return inv
}

// lingerRawDirNames lists the DIRECT entry names of dir with NO name-scoping
// beyond dotfile exclusion, sorted: the linger plane reads the raw directory so
// an entry whose name drifted from the managed bunker- pattern (INT-HOST-004)
// is still seen. Semantics match listAgentDirEntries otherwise: a MISSING
// directory is an empty plane (nil, nil), and any other read error is returned.
// Unique to the linger plane (other planes list managed ids through
// listAgentDirEntries); linger-prefixed so it cannot collide with sibling
// workers' identifiers in this package.
func lingerRawDirNames(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// residuePlaneCount is the number of independently probed planes; the status is
// "unavailable" only when NONE of them could be read.
const residuePlaneCount = 4

// countRegistered counts the agents the daemon knows: live tracker records plus
// durable registry records the tracker does not hold (e.g. between replay and
// the first reconciliation).
func (m *AgentManager) countRegistered() int {
	seen := map[string]bool{}
	n := 0
	if m.tracker != nil {
		for _, rec := range m.tracker.List() {
			if !seen[rec.AgentID] {
				seen[rec.AgentID] = true
				n++
			}
		}
	}
	if m.registry != nil {
		for _, rec := range m.registry.Live() {
			if !seen[rec.AgentID] {
				seen[rec.AgentID] = true
				n++
			}
		}
	}
	return n
}

// listAgentDirEntries lists the direct entries of dir and returns the managed
// agent ids they name. prefix is stripped from each entry name ("bunker-" for
// users, homes and linger entries); an empty prefix means the entry name IS the
// agent id (the SSH key directory). Entries that do not name a valid agent id
// (unrelated files, dotfiles, sub-directories named something else) are skipped,
// and a MISSING directory is an empty plane, not a failure — the plane simply
// does not exist yet.
func listAgentDirEntries(dir, prefix string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if prefix != "" {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			name = strings.TrimPrefix(name, prefix)
		}
		if name == "" || !validAgentID.MatchString(name) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
