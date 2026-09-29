package agent

// DF-BUNKER-81: the destroy home archive must (a) not tar the agent's own
// rootless docker data-root, (b) draw its budget from the home's SIZE instead
// of the fixed compensating-step budget that SIGKILLed a 688M home's gzip at
// ~15-28s, and (c) support an explicit operator opt-out that deletes the home
// with no archive at all.
//
// On a rootless-docker agent the docker data-root lives INSIDE $HOME
// (.local/share/docker — 442M of a 688M home on the dogfood host). It is
// RUNTIME STATE: container/overlay layers the daemon rebuilds on demand, not
// user data, and it is the part that made the archive slow enough to be
// killed. Excluding it (a) removes the bytes that made the archive race its
// budget and (b) keeps the archive a USER-DATA artifact instead of a
// 500MB-of-layers dump.
//
// These tests run the REAL /usr/bin/tar against a t.TempDir() home — no docker
// daemon, no rootless setup, no real agent home.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

// dockerHome builds the incident's home shape: a docker data-root (with a
// layer file big enough to be real content) plus genuine user data beside it.
func dockerHome(t *testing.T, agentID string) (root, home string) {
	t.Helper()
	root = t.TempDir()
	home = filepath.Join(root, "bunker-"+agentID)
	if err := os.MkdirAll(filepath.Join(home, ".local", "share", "docker", "overlay2", "l"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".local", "share", "docker", "overlay2", "l", "layer.bin"),
		[]byte(strings.Repeat("layer", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "work", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "work", "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, home
}

// userDataBytes is the byte count of the user-data file dockerHome writes; it
// is what homeArchiveBytes must report for that home (the docker layer is
// excluded from BOTH the archive and the size that budgets it).
const userDataBytes = int64(13) // len("package main\n")

// Tar tzf a produced archive and return the listing.
func tarListing(t *testing.T, archivePath string) string {
	t.Helper()
	out, err := exec.Command("tar", "tzf", archivePath).CombinedOutput()
	if err != nil {
		t.Fatalf("tar tzf %s: %v (%s)", archivePath, err, out)
	}
	return string(out)
}

// TestArchiveAgentHome_ExcludesRootlessDockerDataRoot is the criterion-3 pin:
// the produced archive carries the user data and NOTHING under
// .local/share/docker.
func TestArchiveAgentHome_ExcludesRootlessDockerDataRoot(t *testing.T) {
	m, _, _ := newLifecycleManager(t)
	agentID := "df81excl"
	_, home := dockerHome(t, agentID)

	path, err := m.archiveAgentHome(context.Background(), home, t.TempDir())
	if err != nil {
		t.Fatalf("archiveAgentHome: %v", err)
	}
	got := tarListing(t, path)

	if !strings.Contains(got, "bunker-"+agentID+"/work/src/main.go") {
		t.Errorf("archive misses user data work/src/main.go; listing:\n%s", got)
	}
	for _, banned := range []string{
		".local/share/docker",
		"layer.bin",
		"overlay2",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("archive still contains %q — the rootless docker data-root must be excluded (criterion 3); listing:\n%s", banned, got)
		}
	}
}

// TestArchiveAgentHome_DockerOnlyHomeStillArchivesAndVerifies is the guard
// against the obvious way to over-fix: a home whose ONLY content was the
// docker data-root must still produce an archive that PASSES verification.
// If excluding the data-root left a zero-entry archive, verifyArchiveFile
// would reject it and the fail-closed destroy gate would refuse the delete —
// i.e. the exclusion would have turned one unfinishable destroy into another.
func TestArchiveAgentHome_DockerOnlyHomeStillArchivesAndVerifies(t *testing.T) {
	m, _, _ := newLifecycleManager(t)
	root := t.TempDir()
	home := filepath.Join(root, "bunker-df81only")
	if err := os.MkdirAll(filepath.Join(home, ".local", "share", "docker", "overlay2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".local", "share", "docker", "overlay2", "l.bin"), []byte("layer"), 0o644); err != nil {
		t.Fatal(err)
	}

	archiveDir := t.TempDir()
	path, err := m.archiveAgentHome(context.Background(), home, archiveDir)
	if err != nil {
		t.Fatalf("docker-only home archive must still verify (a rejected archive would refuse the destroy): %v", err)
	}
	entries, derr := os.ReadDir(archiveDir)
	if derr != nil {
		t.Fatal(derr)
	}
	if len(entries) != 1 {
		t.Errorf("archive dir holds %d entries, want 1: %v", len(entries), entries)
	}
	// And the excluded data-root is genuinely gone from the artifact.
	if got := tarListing(t, path); strings.Contains(got, "overlay2") || strings.Contains(got, "l.bin") {
		t.Errorf("docker-only archive still carries the data-root; listing:\n%s", got)
	}
}

// TestArchiveExcludePatterns pins the PATTERNS the tar invocation carries: one
// anchored pair per excluded subdirectory (bare and ./-prefixed), never an
// unanchored pattern (which would also drop a user's own
// .local/share/docker-named project anywhere in the tree) and never a
// trailing-slash spelling (measured: GNU tar 1.35 does not match it, which
// would silently put the whole data-root back in the archive).
func TestArchiveExcludePatterns(t *testing.T) {
	tests := []struct {
		name string
		base string
		want []string
	}{
		{
			name: "agent home base",
			base: "bunker-abc12345",
			want: []string{
				"bunker-abc12345/.local/share/docker",
				"./bunker-abc12345/.local/share/docker",
			},
		},
		{
			name: "any base name keeps the anchoring",
			base: "bunker-z",
			want: []string{
				"bunker-z/.local/share/docker",
				"./bunker-z/.local/share/docker",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := archiveExcludePatterns(tt.base)
			if len(got) != len(tt.want) {
				t.Fatalf("archiveExcludePatterns(%q) = %v, want %v", tt.base, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("pattern[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
			for _, p := range got {
				if !strings.HasPrefix(p, tt.base+"/") && !strings.HasPrefix(p, "./"+tt.base+"/") {
					t.Errorf("pattern %q is not anchored at the archived base %q", p, tt.base)
				}
				if strings.HasSuffix(p, "/") {
					t.Errorf("pattern %q ends with a slash: GNU tar does not match that spelling and the data-root would be archived", p)
				}
			}
			// The args form is what the tar invocation actually receives.
			args := archiveExcludeArgs(tt.base)
			if len(args) != len(tt.want) {
				t.Fatalf("archiveExcludeArgs(%q) = %v, want %d entries", tt.base, args, len(tt.want))
			}
			for i, a := range args {
				if a != "--exclude="+tt.want[i] {
					t.Errorf("arg[%d] = %q, want %q", i, a, "--exclude="+tt.want[i])
				}
			}
		})
	}
}

// TestArchiveAgentHome_TarInvocationCarriesExcludes proves the excludes reach
// the REAL tar process: a PATH wrapper records its argv and then execs the
// genuine /usr/bin/tar, so the archive is a real archive produced with the
// arguments the daemon chose.
func TestArchiveAgentHome_TarInvocationCarriesExcludes(t *testing.T) {
	m, _, _ := newLifecycleManager(t)
	agentID := "df81argv"
	_, home := dockerHome(t, agentID)

	argLog := filepath.Join(t.TempDir(), "tar-argv.log")
	wrapDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(wrapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recorder := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + argLog + "\"\nexec /usr/bin/tar \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrapDir, "tar"), []byte(recorder), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	archiveDir := t.TempDir()
	path, err := m.archiveAgentHome(context.Background(), home, archiveDir)
	if err != nil {
		t.Fatalf("archiveAgentHome: %v", err)
	}

	data, rerr := os.ReadFile(argLog)
	if rerr != nil {
		t.Fatalf("read tar argv log: %v", rerr)
	}
	var createLine string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "czf ") {
			createLine = strings.TrimSpace(line)
			break
		}
	}
	if createLine == "" {
		t.Fatalf("no `tar czf` invocation recorded (log:\n%s)", data)
	}
	base := filepath.Base(home)
	for _, want := range []string{
		"--exclude=" + base + "/.local/share/docker",
		"--exclude=./" + base + "/.local/share/docker",
		"-C " + filepath.Dir(home),
		" " + base,
	} {
		if !strings.Contains(createLine, want) {
			t.Errorf("tar argv %q is missing %q", createLine, want)
		}
	}
	if got := tarListing(t, path); strings.Contains(got, "overlay2") {
		t.Errorf("archive produced by the recorded invocation still carries the data-root; listing:\n%s", got)
	}
}

// TestHomeArchiveBytes pins the SIZE the budget is derived from: it must count
// the files the archive will read, skip the excluded data-root entirely, and
// never over-exclude a same-named directory that is NOT the agent's data-root.
func TestHomeArchiveBytes(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, home string)
		want  int64
	}{
		{
			name: "docker data-root is excluded, user data counted",
			setup: func(t *testing.T, home string) {
				writeTestFile(t, filepath.Join(home, ".local", "share", "docker", "l.bin"), strings.Repeat("x", 20480))
				writeTestFile(t, filepath.Join(home, "work", "main.go"), "package main\n")
			},
			want: userDataBytes,
		},
		{
			name: "docker-only home sizes to zero",
			setup: func(t *testing.T, home string) {
				writeTestFile(t, filepath.Join(home, ".local", "share", "docker", "l.bin"), strings.Repeat("x", 4096))
			},
			want: 0,
		},
		{
			name: "a .local/share/docker OUTSIDE the home root is not excluded",
			setup: func(t *testing.T, home string) {
				writeTestFile(t, filepath.Join(home, "work", ".local", "share", "docker", "l.bin"), strings.Repeat("x", 1024))
			},
			want: 1024,
		},
		{
			name:  "unreadable/missing home is best-effort zero (never an error)",
			setup: func(t *testing.T, home string) {},
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "bunker-df81size")
			if tt.name != "unreadable/missing home is best-effort zero (never an error)" {
				if err := os.MkdirAll(home, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			tt.setup(t, home)
			if got := homeArchiveBytes(home); got != tt.want {
				t.Errorf("homeArchiveBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

// writeTestFile writes content at path, creating parent directories.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDestroy_ArchiveBudgetIsSizeDerivedNotTheFixedStepCap is the criterion-1
// pin on the daemon side, and it is the reason the destroy was UNFINISHABLE:
// the archive used to draw rollbackStepTimeout (15s) — or, before BNK-DF-001,
// the request deadline the CLI set (30s) — and a ~688M rootless-docker home
// needs longer than either, so the gzip was SIGKILLed, the fail-closed gate
// refused, and the agent survived every retry.
//
// The seam captures BOTH numbers the archive step decides: the byte count that
// sized the budget (which must exclude the data-root) and the budget itself
// (which must be the size-derived one, well above the fixed step cap).
func TestDestroy_ArchiveBudgetIsSizeDerivedNotTheFixedStepCap(t *testing.T) {
	m, cfg, userLog := newDestroyArchiveFixture(t)
	agentID := "df81budget"
	root, _ := dockerHome(t, agentID)
	pointHomeAt(t, root)

	archiveDir := t.TempDir()
	cfg.Agent.DestroyArchiveDir = archiveDir
	cfg.Agent.DestroyHomePolicy = config.DestroyPolicyArchive

	registerArchiveAgent(t, m, agentID)

	type captured struct {
		homeBytes int64
		budget    time.Duration
	}
	var got captured
	orig := archiveExecContextFn
	archiveExecContextFn = func(requestCtx context.Context, homeBytes int64) (context.Context, context.CancelFunc) {
		budget := config.ArchiveBudgetForHomeSize(homeBytes)
		got.homeBytes, got.budget = homeBytes, budget
		return context.WithTimeout(context.WithoutCancel(requestCtx), budget)
	}
	t.Cleanup(func() { archiveExecContextFn = orig })

	resp, derr := m.Destroy(context.Background(), agentID, false)
	if derr != nil {
		t.Fatalf("Destroy() error = %v", derr)
	}
	if resp.Status != "destroyed" {
		t.Fatalf("Destroy() status = %q, want destroyed", resp.Status)
	}

	// The budget was sized from the ARCHIVED bytes: the docker data-root
	// (20480 bytes of layer) is not part of the figure.
	if got.homeBytes != userDataBytes {
		t.Errorf("archive budget sized from %d bytes, want %d (the docker data-root must not be counted)", got.homeBytes, userDataBytes)
	}
	if want := config.ArchiveBudgetForHomeSize(userDataBytes); got.budget != want {
		t.Errorf("archive budget = %s, want %s (size-derived)", got.budget, want)
	}
	// The regression itself: the archive must NOT be bounded by the generic
	// compensating-step cap that killed it.
	if got.budget <= rollbackStepTimeout {
		t.Errorf("archive budget %s is not longer than rollbackStepTimeout %s — the archive would be SIGKILLed mid-stream again",
			got.budget, rollbackStepTimeout)
	}
	if got.budget < config.ArchiveBudgetMin {
		t.Errorf("archive budget %s is below the floor %s", got.budget, config.ArchiveBudgetMin)
	}
	// A big home must get a correspondingly bigger budget: the derivation is
	// monotone, so a 700M+ home from the incident class is covered by
	// construction (the floor covers 0..~1.9 GiB; above it the budget tracks
	// the bytes).
	small, big := config.ArchiveBudgetForHomeSize(userDataBytes), config.ArchiveBudgetForHomeSize(4<<30)
	if big <= small {
		t.Errorf("a 4 GiB home gets %s, not more than the %s a 13-byte home gets", big, small)
	}
	if calls := userdelCalls(t, userLog); len(calls) != 1 {
		t.Errorf("userdel calls = %v, want exactly one (the delete after a verified archive)", calls)
	}
}

// TestSkipHomeArchiveOption pins the option itself: the zero option set keeps
// the archive (today's default), and SkipHomeArchive() flips exactly that bit.
// It is the hop the service handler relies on when it sees skip_archive.
func TestSkipHomeArchiveOption(t *testing.T) {
	var opt destroyOptions
	if opt.skipArchive {
		t.Fatal("the zero option set must keep the archive (the default policy)")
	}
	SkipHomeArchive()(&opt)
	if !opt.skipArchive {
		t.Error("SkipHomeArchive() did not set the archive opt-out")
	}
}

// TestDestroy_SkipHomeArchiveOption pins criterion 4's server-side semantics:
// SkipHomeArchive() deletes the home with NO archive — and it does so even when
// the archive dir could not be written, which is what proves the step is
// SKIPPED rather than merely tolerated.
func TestDestroy_SkipHomeArchiveOption(t *testing.T) {
	unwritable := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(unwritable, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unwritable, 0o700) })

	tests := []struct {
		name       string
		opts       []DestroyOption
		archiveDir func(t *testing.T) string
		wantFiles  int
	}{
		{
			name:       "default policy archives (no option)",
			opts:       nil,
			archiveDir: func(t *testing.T) string { return t.TempDir() },
			wantFiles:  1,
		},
		{
			name:       "SkipHomeArchive writes no archive",
			opts:       []DestroyOption{SkipHomeArchive()},
			archiveDir: func(t *testing.T) string { return t.TempDir() },
			wantFiles:  0,
		},
		{
			name:       "SkipHomeArchive does not even try the archive step",
			opts:       []DestroyOption{SkipHomeArchive()},
			archiveDir: func(t *testing.T) string { return unwritable },
			wantFiles:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if os.Geteuid() == 0 && tt.name == "SkipHomeArchive does not even try the archive step" {
				t.Skip("running as root: an unwritable dir cannot be simulated")
			}
			m, cfg, userLog := newDestroyArchiveFixture(t)
			agentID := "df81skip"
			root, _ := dockerHome(t, agentID)
			pointHomeAt(t, root)
			cfg.Agent.DestroyArchiveDir = tt.archiveDir(t)
			cfg.Agent.DestroyHomePolicy = config.DestroyPolicyArchive
			registerArchiveAgent(t, m, agentID)

			resp, derr := m.Destroy(context.Background(), agentID, false, tt.opts...)
			if derr != nil {
				t.Fatalf("Destroy(%v) error = %v", tt.opts, derr)
			}
			if resp.Status != "destroyed" {
				t.Fatalf("Destroy(%v) status = %q, want destroyed", tt.opts, resp.Status)
			}
			entries, rerr := os.ReadDir(cfg.Agent.DestroyArchiveDir)
			if rerr != nil && !os.IsNotExist(rerr) {
				t.Fatal(rerr)
			}
			if len(entries) != tt.wantFiles {
				t.Errorf("archive dir holds %d entries, want %d: %v", len(entries), tt.wantFiles, entries)
			}
			// Either way the delete still runs: the option skips the ARCHIVE,
			// not the destroy. (The delete itself is the PATH recorder stub,
			// so the temp home is not actually removed — the existing archive
			// tests assert on the recorded command for the same reason.)
			calls := userdelCalls(t, userLog)
			if len(calls) != 1 {
				t.Fatalf("userdel calls = %v, want exactly one", calls)
			}
			if !strings.Contains(calls[0], "bunker-"+agentID) {
				t.Errorf("userdel call %q does not target the agent user", calls[0])
			}
		})
	}
}

// TestDestroy_PartialArchiveRemovedWhenTarFailsAfterWriting closes the last
// hole in the partial-archive invariant (criterion 2): a tar that WRITES bytes
// and then fails must not leave a truncated .tar.gz in the archive dir. The
// sibling test covers the SIGKILL shape; this one covers the ordinary non-zero
// exit that had already written output.
func TestDestroy_PartialArchiveRemovedWhenTarFailsAfterWriting(t *testing.T) {
	m, cfg, userLog := newDestroyArchiveFixture(t)
	agentID := "df81partial"
	root, home := dockerHome(t, agentID)
	pointHomeAt(t, root)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Writes real bytes to the archive path argv[2] and then fails.
	stub := "#!/bin/sh\nprintf 'truncated archive bytes' > \"$2\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "tar"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	archiveDir := t.TempDir()
	cfg.Agent.DestroyArchiveDir = archiveDir
	cfg.Agent.DestroyHomePolicy = config.DestroyPolicyArchive
	registerArchiveAgent(t, m, agentID)

	resp, derr := m.Destroy(context.Background(), agentID, false)
	if derr == nil {
		t.Fatal("Destroy() error = nil, want a fail-closed error")
	}
	if resp == nil || resp.Status != StatusHomeRetained {
		t.Fatalf("Destroy() status = %v, want home_retained", resp)
	}
	entries, rerr := os.ReadDir(archiveDir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("archive dir holds %d entr(y/ies) after a failed archive: %v — a partial tarball must never survive as a backup",
			len(entries), names)
	}
	if calls := userdelCalls(t, userLog); len(calls) != 0 {
		t.Errorf("userdel ran despite the failed archive: %v", calls)
	}
	if _, serr := os.Stat(filepath.Join(home, "work", "src", "main.go")); serr != nil {
		t.Errorf("home vanished despite the fail-closed destroy: %v", serr)
	}
}
