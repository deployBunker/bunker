package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

// ── DF-BUNKER-21 (AC5): the residue inventory ──────────────────────────────
//
// These tests pin the operator-facing probe against REAL host planes (temp
// directories standing in for /etc/passwd, /home, the SSH key dir and
// /var/lib/systemd/linger), so the counts are reads of the same artifacts spawn
// and the rollback create and remove. The QA-BUNKER-19 fingerprint the surface
// exists for is "11 orphan users / 0 registered agents": every plane below is
// exercised with a live agent (not residue), an unknown artifact (residue) and
// the unreadable case (which must report partial/unavailable, never zero).

// residueFixture is a temp host plus a manager whose host-path seams point at
// it. Every plane is a real directory, so a count is a filesystem fact.
type residueFixture struct {
	m        *AgentManager
	passwd   string
	homeRoot string
	sshDir   string
	linger   string
}

func newResidueFixture(t *testing.T) *residueFixture {
	t.Helper()
	f := &residueFixture{
		passwd:   filepath.Join(t.TempDir(), "passwd"),
		homeRoot: filepath.Join(t.TempDir(), "home"),
		sshDir:   filepath.Join(t.TempDir(), "ssh"),
		linger:   filepath.Join(t.TempDir(), "linger"),
	}
	for _, dir := range []string{f.homeRoot, f.sshDir, f.linger} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.passwd, []byte("root:x:0:0:root:/root:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	swapStringSeam(t, &agentPasswdPath, f.passwd)
	swapStringSeam(t, &agentHomeRoot, f.homeRoot)
	swapStringSeam(t, &lingerDir, f.linger)

	m := intci5Manager(t)
	m.cfg.Agent.SSHDir = f.sshDir
	f.m = m
	return f
}

// addUser appends one managed user line to the fixture user database.
func (f *residueFixture) addUser(t *testing.T, agentID string) {
	t.Helper()
	line := "bunker-" + agentID + ":x:61001:61001:bunker test agent:" + filepath.Join(f.homeRoot, "bunker-"+agentID) + ":/bin/bash\n"
	file, err := os.OpenFile(f.passwd, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

// addHome / addKey / addLingerEntry materialise the artifact each plane counts.
func (f *residueFixture) addHome(t *testing.T, agentID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.homeRoot, "bunker-"+agentID), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f *residueFixture) addKey(t *testing.T, agentID string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.sshDir, agentID), []byte("PRIVATE KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *residueFixture) addLingerEntry(t *testing.T, username string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.linger, username), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// registerLive puts an agent in the in-memory tracker — the daemon's live set.
func (f *residueFixture) registerLive(t *testing.T, agentID string) {
	t.Helper()
	if err := f.m.tracker.Register(&resource.AgentRecord{AgentID: agentID, Limits: &v1.ResourceLimits{}}); err != nil {
		t.Fatal(err)
	}
}

func TestResidueInventory_CleanHostReportsZeroAndOK(t *testing.T) {
	f := newResidueFixture(t)

	inv := f.m.ResidueInventory()
	if inv.Total() != 0 {
		t.Errorf("a host with no agent artifacts must report zero residue, got %+v", inv)
	}
	if inv.Status != ResidueStatusOK {
		t.Errorf("status = %q, want %q (detail: %s)", inv.Status, ResidueStatusOK, inv.Detail)
	}
	if inv.Registered != 0 {
		t.Errorf("Registered = %d, want 0", inv.Registered)
	}
}

func TestResidueInventory_CountsOrphansOnEveryPlane(t *testing.T) {
	f := newResidueFixture(t)

	// One agent the daemon knows, present on ALL four planes: none of it is
	// residue. The manager's live agent is the control that keeps the probe from
	// simply counting every bunker-* artifact it can see.
	f.registerLive(t, "live")
	f.addUser(t, "live")
	f.addHome(t, "live")
	f.addKey(t, "live")
	f.addLingerEntry(t, "bunker-live")

	// The QA-BUNKER-19 residue: artifacts on the host with no agent behind them.
	f.addUser(t, "ghost1")
	f.addUser(t, "ghost2")
	f.addHome(t, "ghost1")
	f.addKey(t, "ghost1")
	f.addLingerEntry(t, "bunker-ghost-linger")

	inv := f.m.ResidueInventory()
	if inv.OrphanUsers != 2 {
		t.Errorf("OrphanUsers = %d, want 2 (bunker-ghost1 + bunker-ghost2 are in the user database, 'live' is not residue)", inv.OrphanUsers)
	}
	if inv.OrphanHomes != 1 {
		t.Errorf("OrphanHomes = %d, want 1", inv.OrphanHomes)
	}
	if inv.OrphanKeys != 1 {
		t.Errorf("OrphanKeys = %d, want 1", inv.OrphanKeys)
	}
	if inv.StaleLinger != 1 {
		t.Errorf("StaleLinger = %d, want 1", inv.StaleLinger)
	}
	if inv.Registered != 1 {
		t.Errorf("Registered = %d, want 1", inv.Registered)
	}
	if inv.Total() != 5 {
		t.Errorf("Total() = %d, want 5", inv.Total())
	}
	if inv.Status != ResidueStatusOK {
		t.Errorf("status = %q, want %q (detail: %s)", inv.Status, ResidueStatusOK, inv.Detail)
	}
}

// A durable registry record is a known agent too (GAP-070): the sweep must not
// report an agent as residue just because it is not live in the tracker (a
// daemon between replay and its first reconciliation is exactly that state).
func TestResidueInventory_DurableRegistryRecordIsNotResidue(t *testing.T) {
	f := newResidueFixture(t)
	if f.m.registry == nil {
		t.Fatal("test premise broken: the fixture manager has no durable registry")
	}
	if err := f.m.registry.AppendSpawn(&registry.Record{AgentID: "durable", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	f.addUser(t, "durable")
	f.addHome(t, "durable")
	f.addKey(t, "durable")
	f.addLingerEntry(t, "bunker-durable")

	inv := f.m.ResidueInventory()
	if inv.Total() != 0 {
		t.Errorf("a durably registered agent must not count as residue, got %+v", inv)
	}
	if inv.Registered != 1 {
		t.Errorf("Registered = %d, want 1 (the registry record)", inv.Registered)
	}
}

// Residue that is in the tracker AND the registry is counted once, not twice.
func TestResidueInventory_RegisteredCountsEachAgentOnce(t *testing.T) {
	f := newResidueFixture(t)
	f.registerLive(t, "both")
	if err := f.m.registry.AppendSpawn(&registry.Record{AgentID: "both", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if got := f.m.ResidueInventory().Registered; got != 1 {
		t.Errorf("Registered = %d, want 1 (tracker and registry hold the same agent)", got)
	}
}

func TestResidueInventory_MissingPlanesAreEmptyNotFailures(t *testing.T) {
	f := newResidueFixture(t)
	// None of the three directories exists any more (a host where nothing has
	// been spawned yet). A MISSING plane is an empty plane.
	for _, dir := range []string{f.homeRoot, f.sshDir, f.linger} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	inv := f.m.ResidueInventory()
	if inv.Total() != 0 {
		t.Errorf("missing planes must report zero residue, got %+v", inv)
	}
	if inv.Status != ResidueStatusOK {
		t.Errorf("status = %q, want %q: a plane that does not exist yet was reported as unreadable (detail: %s)",
			inv.Status, ResidueStatusOK, inv.Detail)
	}
}

// The whole point of Status/Detail: an unreadable plane must never read as
// "nothing there". A directory that is really a FILE makes the probe fail
// without needing root, which is what the partial case looks like on a daemon
// that cannot read a plane.
func TestResidueInventory_UnreadablePlaneIsPartialNotZero(t *testing.T) {
	f := newResidueFixture(t)
	bogus := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bogus, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.m.cfg.Agent.SSHDir = bogus
	// One real orphan so the test also proves the readable planes still count.
	f.addUser(t, "ghost1")

	inv := f.m.ResidueInventory()
	if inv.Status != ResidueStatusPartial {
		t.Fatalf("status = %q, want %q (a plane could not be read)", inv.Status, ResidueStatusPartial)
	}
	if !strings.Contains(inv.Detail, "keys:") {
		t.Errorf("detail does not name the unreadable plane: %q", inv.Detail)
	}
	if inv.OrphanUsers != 1 {
		t.Errorf("the readable planes must still be counted: OrphanUsers = %d, want 1", inv.OrphanUsers)
	}
}

func TestResidueInventory_NoReadablePlaneIsUnavailable(t *testing.T) {
	f := newResidueFixture(t)
	bogus := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bogus, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// users: a DIRECTORY cannot be parsed as a user database (bufio scan fails);
	// homes/keys/linger: a plain file cannot be listed.
	swapStringSeam(t, &agentPasswdPath, t.TempDir())
	swapStringSeam(t, &agentHomeRoot, bogus)
	swapStringSeam(t, &lingerDir, bogus)
	f.m.cfg.Agent.SSHDir = bogus

	inv := f.m.ResidueInventory()
	if inv.Status != ResidueStatusUnavailable {
		t.Fatalf("status = %q, want %q (no plane could be read) — counts are meaningless and must not be reported as zero residue", inv.Status, ResidueStatusUnavailable)
	}
	for _, want := range []string{"users:", "homes:", "keys:", "linger:"} {
		if !strings.Contains(inv.Detail, want) {
			t.Errorf("detail must name every unreadable plane, %q missing: %q", want, inv.Detail)
		}
	}
}

// Unrelated names in the planes are not managed agents and must not inflate the
// counts (an operator's own files live in these directories too).
func TestResidueInventory_IgnoresUnrelatedEntries(t *testing.T) {
	f := newResidueFixture(t)
	f.addUser(t, "real-ghost")
	// Uppercase is not a valid agent id; "other-" does not carry the prefix; a
	// dotfile is never an agent.
	if err := os.MkdirAll(filepath.Join(f.homeRoot, "bunker-UPPER"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.homeRoot, "other-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.sshDir, "README.md"), []byte("keys live here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.addLingerEntry(t, "kara") // a real system user's linger entry is not residue

	inv := f.m.ResidueInventory()
	if inv.OrphanUsers != 1 || inv.OrphanHomes != 0 || inv.OrphanKeys != 0 || inv.StaleLinger != 0 {
		t.Errorf("unrelated entries inflated the residue counts: %+v", inv)
	}
}

// The users plane must come from the same probe reconciliation uses
// (defaultListSystemAgents over agentPasswdPath) — the seam is only useful if
// the inventory keeps using it.
func TestResidueInventory_UsesTheSharedPasswdProbe(t *testing.T) {
	f := newResidueFixture(t)
	f.addUser(t, "ghost1")
	// Break the shared probe and assert the inventory reports the users plane as
	// unreadable rather than silently counting zero users.
	swapStringSeam(t, &agentPasswdPath, filepath.Join(t.TempDir(), "missing", "passwd"))

	inv := f.m.ResidueInventory()
	if !strings.Contains(inv.Detail, "users:") {
		t.Errorf("a broken shared passwd probe must be reported, got status=%q detail=%q", inv.Status, inv.Detail)
	}
}

func TestListAgentDirEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"bunker-alpha", "bunker-beta", ".hidden", "other", "bunker-UPPER"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := listAgentDirEntries(dir, agentUserPrefix)
	if err != nil {
		t.Fatalf("listAgentDirEntries() error = %v", err)
	}
	want := []string{"alpha", "beta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("listAgentDirEntries() = %v, want %v", got, want)
	}

	// Key directory: the entry name IS the agent id, so a name that is a valid
	// agent id is reported even without the prefix (the key directory holds
	// exactly one file per agent), while dotfiles and ids with invalid
	// characters are skipped.
	keys, err := listAgentDirEntries(dir, "")
	if err != nil {
		t.Fatalf("listAgentDirEntries(key dir) error = %v", err)
	}
	if strings.Join(keys, ",") != "bunker-alpha,bunker-beta,other" {
		t.Errorf("key-dir listing = %v", keys)
	}

	// A missing directory is an empty plane, not an error.
	missing, err := listAgentDirEntries(filepath.Join(t.TempDir(), "nope"), agentUserPrefix)
	if err != nil || len(missing) != 0 {
		t.Errorf("missing dir: got %v, %v; want empty, nil", missing, err)
	}

	// An empty path is an empty plane (no SSH dir configured).
	none, err := listAgentDirEntries("", agentUserPrefix)
	if err != nil || none != nil {
		t.Errorf("empty path: got %v, %v; want nil, nil", none, err)
	}
}

// The inventory must work for a manager whose system probe was never wired
// (constructed directly, as several tests do): the production passwd probe is
// the fallback rather than a panic on a nil func.
func TestResidueInventory_NilSystemProbeFallsBackToPasswd(t *testing.T) {
	f := newResidueFixture(t)
	f.addUser(t, "ghost1")
	f.m.listSystemAgents = nil

	inv := f.m.ResidueInventory()
	if inv.OrphanUsers != 1 {
		t.Errorf("OrphanUsers = %d, want 1 (the passwd fallback found the orphan)", inv.OrphanUsers)
	}
}

func TestResidueInventory_LoggerlessManagerStillProbes(t *testing.T) {
	f := newResidueFixture(t)
	f.m.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	f.addUser(t, "ghost1")
	if got := f.m.ResidueInventory().OrphanUsers; got != 1 {
		t.Errorf("OrphanUsers = %d, want 1", got)
	}
}
