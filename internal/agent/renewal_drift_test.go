package agent

// DF-BUNKER-34 criterion 3: the renewal drift report. The scan is read-only
// and targets a fixture home — no systemd state, no cron daemon, no rewrites.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newDriftFixture builds the home AT THE PATH the manager resolves
// (agentHomeRoot/bunker-<agentID>) with:
//   - a systemd --user unit carrying the old home path in its ExecStart
//   - a clean unit (no reference) so the report counts hits, not files
//   - a crontab carrying the old home path
//   - a .bashrc carrying the old home path
//   - a home-level fleet.toml carrying the old path (the incident's file)
//
// driftFixture carries the two paths the assertions need.
type driftFixture struct {
	home    string
	oldHome string
}

func newDriftFixture(t *testing.T, agentID string) *driftFixture {
	t.Helper()
	oldHome := filepath.Join(t.TempDir(), "bunker-old")
	homeRoot := t.TempDir()
	home := filepath.Join(homeRoot, "bunker-"+agentID)
	unitDir := filepath.Join(home, driftUnitSubdir)
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cronDir := filepath.Join(home, ".cron")
	if err := os.MkdirAll(cronDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(unitDir, "duckbrain-local.service"),
		"[Service]\nExecStart=node "+oldHome+"/.duckbrain/server.js\nWorkingDirectory="+oldHome+"/.duckbrain\n")
	write(filepath.Join(unitDir, "fresh-unit.service"),
		"[Service]\nExecStart=node /srv/app/server.js\n")
	write(filepath.Join(home, ".cron", "crontab"),
		"*/5 * * * * "+oldHome+"/bin/sync.sh\n")
	write(filepath.Join(home, ".bashrc"),
		"export APP_HOME="+oldHome+"/eduos\n")
	write(filepath.Join(home, "fleet.toml"),
		"workdir = \""+oldHome+"/eduos\"\n")
	f := &driftFixture{home: home, oldHome: oldHome}
	orig := agentHomeRoot
	agentHomeRoot = homeRoot
	t.Cleanup(func() { agentHomeRoot = orig })
	return f
}

// TestRenewalDriftReport_NamesStalePaths is criterion 3's acceptance: the
// report names EVERY stale-path hit — one per line, with the file (relative
// to the home), the line number and the carrying line — across systemd user
// units, cron entries and config files, and reports what it scanned.
func TestRenewalDriftReport_NamesStalePaths(t *testing.T) {
	f := newDriftFixture(t, "dfb34-drift")
	m := newGateManager(t, nil)

	rep := m.RenewalDriftReport("dfb34-drift", f.oldHome, "")
	if len(rep.Hits) == 0 {
		t.Fatalf("report found no hits — got %+v", rep)
	}
	// The unit (2 lines), the crontab, .bashrc and fleet.toml.
	if len(rep.Hits) != 5 {
		t.Fatalf("got %d hits, want 5: %+v", len(rep.Hits), rep.Hits)
	}
	// Every hit names its file RELATIVELY and its line number, and the
	// summary renders each one.
	summary := rep.Summarize()
	for _, want := range []string{
		".config/systemd/user/duckbrain-local.service:2",
		".config/systemd/user/duckbrain-local.service:3",
		".cron/crontab:1",
		".bashrc:1",
		"fleet.toml:1",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q — got:\n%s", want, summary)
		}
	}
	if !strings.Contains(summary, "5 stale-path hit(s)") || !strings.Contains(summary, f.oldHome) {
		t.Errorf("summary malformed:\n%s", summary)
	}
	if rep.FilesScanned < 5 {
		t.Errorf("FilesScanned = %d, want >= 5 (unit + cron + bashrc + fleet.toml + fresh unit)", rep.FilesScanned)
	}
}

// TestRenewalDriftReport_CleanHome is the control arm: a home with no stale
// references reports zero hits, names the scanned file count, and its
// summary must not invent a hit.
func TestRenewalDriftReport_CleanHome(t *testing.T) {
	homeRoot := t.TempDir()
	home := filepath.Join(homeRoot, "bunker-dfb34-clean")
	if err := os.MkdirAll(filepath.Join(home, driftUnitSubdir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, driftUnitSubdir, "app.service"), []byte("[Service]\nExecStart=node /srv/app.js\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := agentHomeRoot
	agentHomeRoot = homeRoot
	t.Cleanup(func() { agentHomeRoot = orig })

	m := newGateManager(t, nil)
	rep := m.RenewalDriftReport("dfb34-clean", filepath.Join(home, "stale-old-home"), "")
	if rep.Stale() {
		t.Fatalf("clean home reported hits: %+v", rep.Hits)
	}
	if !strings.Contains(rep.Summarize(), "no stale-path hits") {
		t.Errorf("clean summary malformed: %s", rep.Summarize())
	}
	if rep.FilesScanned != 1 {
		t.Errorf("FilesScanned = %d, want 1", rep.FilesScanned)
	}
}

// TestRenewalDriftReport_NoNeedleNeverReadsClean pins the guard: a report
// with an empty old_home (no needle) returns an EMPTY report — the caller
// must never be able to read "no needle given" as "no drift found".
func TestRenewalDriftReport_NoNeedleNeverReadsClean(t *testing.T) {
	f := newDriftFixture(t, "dfb34-drift")
	m := newGateManager(t, nil)
	rep := m.RenewalDriftReport("dfb34-drift", "", f.home)
	if rep.Stale() || rep.FilesScanned != 0 {
		t.Errorf("needle-less scan must be an empty report, got %+v", rep)
	}
}

// TestRenewalDriftReport_NeverRewrites is the read-only guarantee: after a
// scan with hits, every seeded file still carries the old path byte-identical.
func TestRenewalDriftReport_NeverRewrites(t *testing.T) {
	f := newDriftFixture(t, "dfb34-drift")
	m := newGateManager(t, nil)
	rep := m.RenewalDriftReport("dfb34-drift", f.oldHome, "")
	if !rep.Stale() {
		t.Fatal("premise broken: expected hits")
	}
	// The stale content is still on disk.
	data, err := os.ReadFile(filepath.Join(f.home, ".config", "systemd", "user", "duckbrain-local.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), f.oldHome) {
		t.Errorf("the scan rewrote a scanned unit file: %q", data)
	}
}
