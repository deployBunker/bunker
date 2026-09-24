package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ── DF-BUNKER-34: the renewal drift probe ──────────────────────────────────
//
// The row's observed chain (aa189273 -> eduos-agent -> 2cdce4d0 on a real
// host): a renewal that spawns WITHOUT the stable agent id mints a new home
// path every time, and every long-lived artifact — systemd --user units,
// cron entries, config files — keeps pointing at the OLD home while its user
// no longer exists. Five unit files had to be rewritten by hand after one
// renewal; fleet.toml and scheduler.db carried stale /home paths for days.
//
// The daemon cannot know what an external renewal script will do, but it CAN
// see the drift itself: this probe scans the agent's OWN systemd --user unit
// files, its crontab, and its tracked config files for references to a given
// home path, and reports every hit. The renewal recipe (docs/renewal.md) has
// the operator run it against the OLD home BEFORE the swap and against the
// NEW home after, so nothing goes stale silently.

// driftUnitSubdir is the per-user unit directory, relative to the agent home
// (the same location `bunker surface` installs into, and the same one
// systemd's %h resolves to inside an agent session). Declared here rather
// than imported: the cli package's constant is its own package's surface
// contract, and a second spelling in one file would be drift.
const driftUnitSubdir = ".config/systemd/user"

// maxDriftFileBytes bounds one scanned file's contribution to the report.
// A config file that is itself enormous is truncated, not the report.
const maxDriftFileScanBytes = 1 << 20

// DriftHit is one stale-path reference found in an agent-side file.
type DriftHit struct {
	// File is the path INSIDE the agent home (relative), e.g.
	// .config/systemd/user/duckbrain-local.service.
	File string
	// Line is the 1-based line number carrying the reference.
	Line int
	// Text is the trimmed line content (evidence, bounded by the scanner).
	Text string
}

// DriftReport is the renewal drift check for one agent home.
type DriftReport struct {
	// AgentID is the agent the report is for.
	AgentID string
	// Home is the agent home that was scanned.
	Home string
	// OldHome is the path the scan searched for (the previous home path).
	OldHome string
	// Hits are the stale-path references found, ordered by file then line.
	Hits []DriftHit
	// FilesScanned counts the files the probe actually read.
	FilesScanned int
	// Unreadable names files that exist but could not be read (reported, so
	// an unreadable file never reads as a clean one).
	Unreadable []string
}

// Stale returns true when the report found at least one stale-path hit.
func (r DriftReport) Stale() bool { return len(r.Hits) > 0 }

// Summarize renders the operator-facing one-line-per-hit report.
func (r DriftReport) Summarize() string {
	if len(r.Hits) == 0 {
		if len(r.Unreadable) > 0 {
			return fmt.Sprintf("no stale-path hits in %d scanned files (%d unreadable: %s)",
				r.FilesScanned, len(r.Unreadable), strings.Join(r.Unreadable, ", "))
		}
		return fmt.Sprintf("no stale-path hits in %d scanned files", r.FilesScanned)
	}
	lines := make([]string, 0, len(r.Hits))
	for _, h := range r.Hits {
		lines = append(lines, fmt.Sprintf("  %s:%d: %s", h.File, h.Line, h.Text))
	}
	out := fmt.Sprintf("%d stale-path hit(s) referencing %s across %d scanned file(s):",
		len(r.Hits), r.OldHome, r.FilesScanned)
	return out + "\n" + strings.Join(lines, "\n")
}

// checkHomeDrift scans the agent's home at home for every reference to
// oldHome in the artifact classes the row names: systemd --user unit files,
// cron entries, and the agent's own shell/env config files (which is where
// the observed fleet.toml / .profile / .bashrc stale paths lived). The scan
// is read-only and reports; it never rewrites — the recipe documents the
// operator's rewrite (an automatic rewrite of a user's service units is a
// data mutation the daemon has no authority over).
func (m *AgentManager) checkHomeDrift(ctx context.Context, agentID, home, oldHome string) DriftReport {
	rep := DriftReport{AgentID: agentID, Home: home, OldHome: oldHome}
	if home == "" || oldHome == "" {
		return rep
	}

	// Class 1: every systemd --user unit file. The whole directory is scanned
	// (not a fixed unit list) — a renewal's own new units are part of the
	// drift surface exactly like the long-lived ones.
	unitDir := filepath.Join(home, driftUnitSubdir)
	rep.scanDir(&unitDirWalk{root: unitDir}, oldHome)

	// Class 2: the crontab.
	rep.scanFile(filepath.Join(home, driftCrontabRel), oldHome)

	// Class 3: the tracked dotfiles a renewal restore touches — the exact
	// files the incident's stale paths lived in (.bashrc, .profile, and any
	// *.env / *.conf / *.toml / *.json / *.yaml file in the home TOP level,
	// which keeps the scan bounded and skips the deep trees like repos and
	// .hermes state that are re-owned by the renewal anyway).
	for _, name := range shellEnvFileNames {
		rep.scanFile(filepath.Join(home, name), oldHome)
	}
	entries, err := os.ReadDir(home)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !isDriftConfigName(e.Name()) {
				continue
			}
			rep.scanFile(filepath.Join(home, e.Name()), oldHome)
		}
	}
	sort.Slice(rep.Hits, func(i, j int) bool {
		if rep.Hits[i].File != rep.Hits[j].File {
			return rep.Hits[i].File < rep.Hits[j].File
		}
		return rep.Hits[i].Line < rep.Hits[j].Line
	})
	return rep
}

// shellEnvFileNames are the fixed home-level files every renewal restore
// touches (and the ones the incident's stale paths were found in). The
// crontab path lives here too as a named constant the scanner uses directly.
var shellEnvFileNames = []string{".bashrc", ".profile", ".bash_profile", ".env"}

// driftCrontabRel is the crontab path INSIDE the agent home the scan reads.
const driftCrontabRel = ".cron/crontab"

// isDriftConfigName reports whether a home-root filename belongs to the
// config classes the row names (config/env files carrying home paths).
// Extensions, not names: fleet.toml, settings.json, app.yaml all qualify.
var driftConfigExtRe = regexp.MustCompile(`(?i)\.(toml|json|ya?ml|env|conf|cfg|ini|sh)$`)

func isDriftConfigName(name string) bool {
	return driftConfigExtRe.MatchString(name)
}

// unitDirWalk is a tiny scan target abstraction so files under one directory
// share the reader.
type unitDirWalk struct{ root string }

// scanDir reads every regular file under root (non-recursive is enough: a
// systemd user unit dir is flat) for references to oldHome.
func (r *DriftReport) scanDir(w *unitDirWalk, oldHome string) {
	entries, err := os.ReadDir(w.root)
	if err != nil {
		return // absent unit dir: nothing to scan, not an error
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		r.scanFile(filepath.Join(w.root, e.Name()), oldHome)
	}
}

// scanFile reads one file and records every line carrying oldHome. The
// path recorded in the hit is relative to the scanned home when possible, so
// the report reads the same on any host.
func (r *DriftReport) scanFile(path, oldHome string) {
	rel := path
	if base := r.Home; base != "" && strings.HasPrefix(path, base+string(filepath.Separator)) {
		rel, _ = filepath.Rel(base, path)
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		r.Unreadable = append(r.Unreadable, rel)
		return
	}
	defer f.Close()
	r.FilesScanned++
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxDriftFileScanBytes)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		text := sc.Text()
		if strings.Contains(text, oldHome) {
			r.Hits = append(r.Hits, DriftHit{File: rel, Line: lineNo, Text: strings.TrimSpace(text)})
		}
	}
}

// RenewalDriftReport is the manager-level entry the server RPC routes to: it
// resolves the home to scan (explicit home override, else the agent's own
// home under agentHomeRoot) and runs the read-only drift scan. It never
// fails: a home that does not exist reports zero scanned files, which the
// summary renders honestly.
func (m *AgentManager) RenewalDriftReport(agentID, oldHome, home string) DriftReport {
	if home == "" {
		home = filepath.Join(agentHomeRoot, agentUserPrefix+agentID)
	}
	return m.checkHomeDrift(context.Background(), agentID, home, oldHome)
}

// RenewalIdentityError is the fail-loud refusal the row names: a renewal
