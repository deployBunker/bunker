//go:build linux

// Package fsmount — BFS-052's unit cells (table-driven; no live kernel
// needed: the /sys and mountinfo sources are fixture trees pointed at by
// the same vars production reads, and the INIT view is a literal).
//
// WHAT IS ASSERTED HERE, per the row's acceptance criteria:
//
//  2. requested > kernel readahead → degradation with requested value,
//     effective (clamped) value, non-empty reason — TABLE-DRIVEN, plus the
//     granted and kernel-set-other arms of the same comparison and the
//     mountinfo/connection/BDI resolution classes.
//  3. a capability the probe cannot observe renders as UNKNOWN with a
//     reason, NOT as granted — the absent-mountinfo control, the absent
//     connection dir, the unreadable/absent sysfs file, the INIT-less
//     flags, and splice (no observable exists AT ALL), plus the
//     degradations[] derivation that keeps unknown a distinct mode.
//  4. the status struct carries the effective values and the probe path
//     populates them.
//  6. a live probe against THIS host's kernel (go-fuse test mount).
//
// The mutation controls live in bfs052_mutation_test.go.
package fsmount

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// bfs052Fixture is one probe fixture: a mountinfo line, a connection
// directory, and a BDI entry — each present or absent as the arm needs.
type bfs052Fixture struct {
	mountpoint    string
	mountinfoLine string // empty = no line at all
	connID        string // e.g. "905"; empty = no connection dir
	connFiles     map[string]string
	bdiEntry      string // e.g. "0:905"; empty = none
	readAheadKB   string
}

// install points the probe's source vars at the fixture tree for the life
// of one cell and restores them after.
func (f bfs052Fixture) install(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	mi := filepath.Join(root, "mountinfo")
	lines := []string{}
	if f.mountinfoLine != "" {
		lines = append(lines, f.mountinfoLine)
	}
	if err := os.WriteFile(mi, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldMI, oldConn, oldBDI := procMountinfo, sysFSFuseConnections, sysClassBDI
	procMountinfo, sysFSFuseConnections, sysClassBDI = mi, filepath.Join(root, "connections"), filepath.Join(root, "bdi")
	t.Cleanup(func() { procMountinfo, sysFSFuseConnections, sysClassBDI = oldMI, oldConn, oldBDI })

	if f.connID != "" {
		dir := filepath.Join(sysFSFuseConnections, f.connID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, val := range f.connFiles {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(val+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if f.bdiEntry != "" {
		dir := filepath.Join(sysClassBDI, f.bdiEntry)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if f.readAheadKB != "" {
			if err := os.WriteFile(filepath.Join(dir, "read_ahead_kb"), []byte(f.readAheadKB+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// bfs052MountLine is a mountinfo line for mountpoint with device dev. The
// tail fields (fstype, source, options) are the FUSE shape.
func bfs052MountLine(mountpoint, dev string) string {
	return "1166 42 " + dev + " / " + mountpoint + " rw,relatime shared:1059 - fuse.bunker-fs bunker-fs rw,user_id=1000,group_id=1000"
}

// bfs052Settings is the literal INIT-view builder for the cells.
var bfs052Settings bfs052SettingsBuilder

type bfs052SettingsBuilder struct{}

// flags returns a settings view with the given negotiated flags.
func (bfs052SettingsBuilder) flags(flags uint64) *fuseKernelSettings {
	return &fuseKernelSettings{flagsKnown: true, flags: flags}
}

// none returns the "no kernel settings" view.
func (bfs052SettingsBuilder) none() *fuseKernelSettings { return nil }

// capFor fetches one capability entry from a probe result.
func capFor(t *testing.T, st FuseState, name string) Capability {
	t.Helper()
	c, ok := effectiveCap(st.Capabilities, name)
	if !ok {
		t.Fatalf("capability %s missing from the probe result (%d entries)", name, len(st.Capabilities))
	}
	return c
}

// ---------------------------------------------------------------------------
// ACCEPTANCE CRITERION 2 — table-driven: requested readahead above the
// kernel's is a degradation carrying requested, effective and a named
// reason. The sibling arms pin the granted and kernel-set-other classes and
// the unset request, so the comparison's three outcomes are all witnessed.
// ---------------------------------------------------------------------------

func TestBFS052ReadAheadComparisonIsHonest(t *testing.T) {
	ks := bfs052Settings.flags(0)
	type bfs052Case struct {
		name         string
		requestedKB  string // the BDI knob the "kernel" runs, in KB
		requested    int    // what the mount asked, in bytes
		noMountinfo  bool   // the mount line is absent from mountinfo
		wantState    CapabilityState
		wantReason   string // "" = must be empty
		wantEffBytes uint64
	}
	cases := []bfs052Case{
		{
			// THE ROW'S OWN ARM: the mount asked for more than the kernel
			// allows, the kernel clamped, and the entry must say so with
			// both figures and the mechanism.
			name:         "requested_above_kernel_limit_is_clamped",
			requestedKB:  "128", // 128 KiB — the kernel's classic ceiling
			requested:    1 << 20,
			wantState:    CapabilityStateDegraded,
			wantReason:   reasonKernelClamp,
			wantEffBytes: 128 << 10,
		},
		{
			name:         "requested_within_limit_is_granted",
			requestedKB:  "512",
			requested:    512 << 10,
			wantState:    CapabilityStateGranted,
			wantEffBytes: 512 << 10,
		},
		{
			// The kernel runs something else and it is LARGER than the
			// request: a divergence, still both figures, its own reason —
			// never blurred into the clamp class (and the direction check
			// proves the reason is derived from the comparison, not
			// hardcoded).
			name:         "kernel_running_larger_value_is_degraded_with_own_reason",
			requestedKB:  "512",
			requested:    64 << 10,
			wantState:    CapabilityStateDegraded,
			wantReason:   reasonKernelSetOther,
			wantEffBytes: 512 << 10,
		},
		{
			// No request, kernel default: in force, nothing of ours refused.
			name:         "unset_request_reports_kernel_value_as_granted",
			requestedKB:  "128",
			requested:    0,
			wantState:    CapabilityStateGranted,
			wantEffBytes: 128 << 10,
		},
		{
			// The mountpoint cannot be found in mountinfo at all (an
			// exotic namespace, a raced unmount): the kernel is running
			// SOME value, and the probe cannot claim OUR requested value
			// is in force. That is unknown-with-source, never granted —
			// the same rule the numeric knobs obey.
			name:         "within_limit_but_mountinfo_line_absent_is_unknown",
			requestedKB:  "512",
			requested:    512 << 10,
			noMountinfo:  true,
			wantState:    CapabilityStateUnknown,
			wantReason:   "not observable via",
			wantEffBytes: 512 << 10,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mp := "/mnt/bfs052-readahead"
			fix := bfs052Fixture{
				mountpoint:    mp,
				mountinfoLine: bfs052MountLine(mp, "0:905"),
				connID:        "905",
				connFiles:     map[string]string{"max_background": "32", "congestion_threshold": "24"},
				bdiEntry:      "0:905",
				readAheadKB:   tc.requestedKB,
			}
			if tc.noMountinfo {
				fix.mountinfoLine = ""
			}
			fix.install(t)
			req := fuseRequests{readAhead: tc.requested}
			st := probeFuseCapabilities(mp, ks, req)
			c := capFor(t, st, capMaxReadAhead)
			if c.State != tc.wantState {
				t.Fatalf("state = %q, want %q (entry %+v)", c.State, tc.wantState, c)
			}
			if tc.wantReason != "" {
				if !strings.Contains(c.Reason, tc.wantReason) {
					t.Fatalf("reason = %q, want it to contain %q", c.Reason, tc.wantReason)
				}
			} else if c.Reason != "" {
				t.Fatalf("reason = %q, want empty on a granted entry", c.Reason)
			}
			// Requested and effective both travel in the entry (spec §6:
			// both printed). For the unknown arm there is no effective
			// figure by contract; every resolved arm carries the kernel's
			// value — the CLAMPED one when the clamp fired.
			if tc.wantState == CapabilityStateUnknown {
				if c.Effective != "" {
					t.Fatalf("an unknown entry carries no effective figure; got %q", c.Effective)
				}
				return
			}
			if !strings.Contains(c.Effective, strconv.FormatUint(tc.wantEffBytes, 10)) {
				t.Fatalf("Effective = %q, want it to carry the kernel's %d bytes", c.Effective, tc.wantEffBytes)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ACCEPTANCE CRITERION 3 — the honest unknown. Every arm here is a source
// the probe cannot read; every entry must come out unknown WITH a reason
// and never as granted.
// ---------------------------------------------------------------------------

func TestBFS052UnobservableIsUnknownNeverGranted(t *testing.T) {
	cases := []struct {
		name string
		fix  bfs052Fixture
		ks   *fuseKernelSettings
		req  fuseRequests
		cap  string
	}{
		{
			name: "mountinfo_line_absent_degrades_every_numeric_cap",
			fix:  bfs052Fixture{mountpoint: "/mnt/gone"},
			ks:   bfs052Settings.none(),
			req:  fuseRequests{maxBackground: 32},
			cap:  capMaxBackground,
		},
		{
			name: "connection_dir_absent_unknowns_the_connection_knobs",
			fix: bfs052Fixture{
				mountpoint:    "/mnt/noconn",
				mountinfoLine: bfs052MountLine("/mnt/noconn", "0:404"),
				// connID deliberately empty: mountinfo knows the device,
				// the kernel dropped the directory (or it was never ours).
			},
			ks:  bfs052Settings.flags(0),
			req: fuseRequests{},
			cap: capCongestion,
		},
		{
			name: "sysfs_file_missing_unknowns_that_capability",
			fix: bfs052Fixture{
				mountpoint:    "/mnt/nofile",
				mountinfoLine: bfs052MountLine("/mnt/nofile", "0:906"),
				connID:        "906",
				connFiles:     map[string]string{"max_background": "32"}, // congestion_threshold absent
			},
			ks:  bfs052Settings.flags(0),
			req: fuseRequests{},
			cap: capCongestion,
		},
		{
			name: "non_integer_sysfs_value_is_not_a_number",
			fix: bfs052Fixture{
				mountpoint:    "/mnt/garbage",
				mountinfoLine: bfs052MountLine("/mnt/garbage", "0:907"),
				connID:        "907",
				connFiles:     map[string]string{"max_background": "not-a-number"},
			},
			ks:  bfs052Settings.flags(0),
			req: fuseRequests{},
			cap: capMaxBackground,
		},
		{
			name: "readahead_bdi_entry_absent",
			fix: bfs052Fixture{
				mountpoint:    "/mnt/nobdi",
				mountinfoLine: bfs052MountLine("/mnt/nobdi", "0:908"),
				connID:        "908",
				connFiles:     map[string]string{"max_background": "32", "congestion_threshold": "24"},
				// bdiEntry empty
			},
			ks:  bfs052Settings.flags(0),
			req: fuseRequests{},
			cap: capMaxReadAhead,
		},
		{
			name: "init_settings_absent_unknowns_the_behaviours",
			fix: bfs052Fixture{
				mountpoint:    "/mnt/noinit",
				mountinfoLine: bfs052MountLine("/mnt/noinit", "0:909"),
				connID:        "909",
				connFiles:     map[string]string{"max_background": "32", "congestion_threshold": "24"},
				bdiEntry:      "0:909",
				readAheadKB:   "128",
			},
			ks:  bfs052Settings.none(),
			req: fuseRequests{locks: true, symlinkCache: true},
			cap: capLocks,
		},
		{
			// SPLICE IS THE PERMANENT CASE: no kernel observable for it
			// exists at all, whatever the fixture provides.
			name: "splice_has_no_kernel_observable_at_all",
			fix: bfs052Fixture{
				mountpoint:    "/mnt/full",
				mountinfoLine: bfs052MountLine("/mnt/full", "0:910"),
				connID:        "910",
				connFiles:     map[string]string{"max_background": "32", "congestion_threshold": "24"},
				bdiEntry:      "0:910",
				readAheadKB:   "128",
			},
			ks:  bfs052Settings.flags(fuse.CAP_SPLICE_READ | fuse.CAP_SPLICE_WRITE),
			req: fuseRequests{disableSplice: false},
			cap: capSplice,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.fix.install(t)
			st := probeFuseCapabilities(tc.fix.mountpoint, tc.ks, tc.req)
			c := capFor(t, st, tc.cap)
			// THE ASSERTION THE ROW EXISTS FOR: unknown, with a reason that
			// names its source, and NO effective figure.
			if c.State != CapabilityStateUnknown {
				t.Fatalf("capability %s rendered as %q; an unobservable source must render unknown, never granted (entry %+v)", tc.cap, c.State, c)
			}
			if !strings.Contains(c.Reason, "not observable via") {
				t.Fatalf("unknown's reason must name its source (\"not observable via ...\"); got %q", c.Reason)
			}
			if c.Effective != "" {
				t.Fatalf("an unknown entry carries no effective figure; got %q", c.Effective)
			}
		})
	}
}

// TestBFS052UnknownAndDegradedStayDistinctModes pins the degradations[]
// derivation: unknown is its own mode, never degraded and never silently
// granted — the DF-BUNKER-9 vocabulary rule.
func TestBFS052UnknownAndDegradedStayDistinctModes(t *testing.T) {
	mp := "/mnt/modes"
	fix := bfs052Fixture{
		mountpoint:    mp,
		mountinfoLine: bfs052MountLine(mp, "0:911"),
		connID:        "911",
		connFiles:     map[string]string{"max_background": "12"}, // a clamp vs the request below
		bdiEntry:      "0:911",
		readAheadKB:   "128",
	}
	fix.install(t)
	req := fuseRequests{maxBackground: 32}
	ks := bfs052Settings.flags(0) // behaviours unknown: no INIT view
	st := probeFuseCapabilities(mp, ks, req)

	degraded, unknown := 0, 0
	for _, d := range st.Degradations {
		switch d.Mode {
		case string(CapabilityModeDegraded):
			degraded++
			if d.Detail == "" || !strings.Contains(d.Detail, "requested") {
				t.Errorf("degraded entry for %s must carry both figures in its detail; got %q", d.Capability, d.Detail)
			}
		case string(CapabilityModeUnknown):
			unknown++
			if !strings.Contains(d.Detail, "not observable via") {
				t.Errorf("unknown entry for %s must name its source; got %q", d.Capability, d.Detail)
			}
		default:
			t.Errorf("mode %q is not in the vocabulary (degraded|unknown)", d.Mode)
		}
		if d.Scope != fuseScope {
			t.Errorf("scope = %q, want %q", d.Scope, fuseScope)
		}
	}
	if degraded == 0 || unknown == 0 {
		t.Fatalf("this fixture must produce BOTH a degraded and an unknown entry; got %d degraded, %d unknown", degraded, unknown)
	}
	// Every non-granted capability has exactly one degradation entry.
	grantedByCensus := 0
	for _, c := range st.Capabilities {
		if c.State == CapabilityStateGranted {
			grantedByCensus++
		}
	}
	if got := len(st.Degradations); got != len(st.Capabilities)-grantedByCensus {
		t.Fatalf("degradations (%d) must be exactly the non-granted capabilities (%d of %d)", got, len(st.Capabilities)-grantedByCensus, len(st.Capabilities))
	}
}

// ---------------------------------------------------------------------------
// ACCEPTANCE CRITERION 4 — the status plumbing: the mount struct carries the
// effective values and the probe path populates them.
// ---------------------------------------------------------------------------

func TestBFS052StatusCarriesTheProbedFigures(t *testing.T) {
	m, _ := testMount(t, "bfs-052 status carries the probe\n")
	// The probe resolves THIS MOUNT's own mountinfo line, so the fixture
	// must carry the mountpoint the handle actually names (testMount leaves
	// Mountpoint empty; point the fixture at a real temp path and aim the
	// handle at it).
	mp := filepath.Join(t.TempDir(), "mnt")
	m.opts.Mountpoint = mp
	fix := bfs052Fixture{
		mountpoint:    mp,
		mountinfoLine: bfs052MountLine(mp, "0:912"),
		connID:        "912",
		connFiles:     map[string]string{"max_background": "32", "congestion_threshold": "24"},
		bdiEntry:      "0:912",
		readAheadKB:   "128",
	}
	fix.install(t)

	// The probe path (the same call MountAt makes): populates the mount's
	// stored state from the requested options.
	m.probeFuseState(fuse.MountOptions{MaxBackground: 32, CongestionThreshold: 24, MaxWrite: 1 << 20})
	if m.FuseState().Source == "" {
		t.Fatal("the probe stored no source: the status document would carry figures nobody can attribute")
	}

	st := m.Status()
	fuseAny := st.Fuse
	fs, ok := fuseAny.(FuseState)
	if !ok {
		t.Fatalf("Status().Fuse is %T, want fsmount.FuseState", fuseAny)
	}
	if fs.Source == "" {
		t.Fatal("Status().Fuse.Source is empty: the effective values must be attributable to their source (spec §6)")
	}
	bg, ok := effectiveCap(fs.Capabilities, capMaxBackground)
	if !ok {
		t.Fatal("Status().Fuse carries no max_background entry")
	}
	if bg.Effective == "" {
		t.Fatal("the effective max_background figure is missing from the status document — the §6 law is exactly that it appears there")
	}
	if !strings.Contains(bg.Effective, "32") {
		t.Fatalf("effective max_background = %q, want the kernel's 32", bg.Effective)
	}
	if fs.Connection == nil || *fs.Connection != 912 {
		t.Fatalf("connection = %v, want 912", fs.Connection)
	}
}

// ---------------------------------------------------------------------------
// ACCEPTANCE CRITERION 6 — the live probe against THIS host's kernel: a real
// go-fuse mount, the real INIT exchange, the real mountinfo/sysfs reads. The
// assertions are about HONESTY (sources named, unknowns carried, no
// fabricated granted states), not about particular kernel values, which are
// this host's business and change.
// ---------------------------------------------------------------------------

func TestBFS052LiveProbeAgainstThisKernel(t *testing.T) {
	m, dir := bfs052LiveMount(t)
	defer func() {
		m.Unmount()
		_ = dir
	}()
	<-time.After(50 * time.Millisecond) // let the INIT exchange land

	st := m.FuseState()
	if st.Source == "" {
		t.Fatal("the live probe recorded no source: it must name mountinfo and the connection dir it read")
	}
	if !strings.Contains(st.Source, "/proc/self/mountinfo") {
		t.Fatalf("source %q does not name mountinfo", st.Source)
	}
	if st.Connection == nil {
		t.Fatalf("the live mount's connection id is missing (source %q) — this mount IS a FUSE mount, so the id is readable", st.Source)
	}
	if len(st.Capabilities) == 0 {
		t.Fatal("the live probe recorded no capabilities")
	}
	seen := map[string]bool{}
	for _, c := range st.Capabilities {
		seen[c.Capability] = true
		switch c.State {
		case CapabilityStateGranted, CapabilityStateDegraded:
			if c.Effective == "" {
				t.Errorf("%s claims %q with no effective figure", c.Capability, c.State)
			}
		case CapabilityStateUnknown:
			if !strings.Contains(c.Reason, "not observable via") {
				t.Errorf("%s is unknown without a named source: %q", c.Capability, c.Reason)
			}
		default:
			t.Errorf("%s carries state %q, outside the vocabulary", c.Capability, c.State)
		}
	}
	for _, want := range []string{capMaxBackground, capCongestion, capMaxReadAhead, capMaxWrite, capLocks, capSymlinkCache, capReadDirPlus, capSplice} {
		if !seen[want] {
			t.Errorf("capability %s was not probed", want)
		}
	}
	// max_background and congestion_threshold come from THIS mount's own
	// connection directory, and the mount itself requested 32 (its
	// Concurrency default) — so granted is the expected live verdict, and
	// the effective figure must be non-zero.
	for _, name := range []string{capMaxBackground, capCongestion} {
		c := capFor(t, st, name)
		if c.Effective == "" || strings.HasPrefix(c.Effective, "0 ") {
			t.Errorf("%s effective = %q; the kernel's own connection file must carry the live value", name, c.Effective)
		}
	}
	// The connection dir the probe named must exist and carry the files it
	// claims to have read (the battery can diff them independently).
	connDir := filepath.Join(sysFSFuseConnections, strconv.Itoa(*st.Connection))
	if _, err := os.Stat(filepath.Join(connDir, "max_background")); err != nil {
		t.Fatalf("the probed connection dir %s is not readable: %v", connDir, err)
	}
	_ = dir
}

// ---------------------------------------------------------------------------
// The resolution seams: mountinfo parsing, mountpoint matching, and the
// source strings — the classes the live cells cannot hit with one host.
// ---------------------------------------------------------------------------

func TestBFS052MountinfoMatchingAndEscapes(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		mountpoint string
		wantDev    string
		wantHit    bool
	}{
		{
			name:       "exact_mountpoint_matches",
			line:       bfs052MountLine("/mnt/a", "0:905"),
			mountpoint: "/mnt/a",
			wantDev:    "0:905",
			wantHit:    true,
		},
		{
			name:       "another_mounts_line_does_not_match",
			line:       bfs052MountLine("/mnt/other", "0:905"),
			mountpoint: "/mnt/a",
			wantHit:    false,
		},
		{
			name:       "octal_escaped_mountpoint_matches_raw_path",
			line:       bfs052MountLine("/mnt/with\\040space", "0:906"),
			mountpoint: "/mnt/with space",
			wantDev:    "0:906",
			wantHit:    true,
		},
		{
			name:       "malformed_line_is_skipped",
			line:       "garbage without separator",
			mountpoint: "/mnt/a",
			wantHit:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fix := bfs052Fixture{mountpoint: tc.mountpoint, mountinfoLine: tc.line}
			fix.install(t)
			line, ok := findMountLine(tc.mountpoint)
			if ok != tc.wantHit {
				t.Fatalf("findMountLine hit=%v, want %v", ok, tc.wantHit)
			}
			if tc.wantHit && line.dev != tc.wantDev {
				t.Fatalf("dev = %q, want %q", line.dev, tc.wantDev)
			}
		})
	}
}

func TestBFS052ConnectionDirRejectsNonFUSEDevices(t *testing.T) {
	if _, ok := connectionDir("8:1"); ok {
		t.Fatal("a major != 0 device is not a FUSE connection id; the probe must not guess one")
	}
	if _, ok := connectionDir(""); ok {
		t.Fatal("an empty device resolves nothing")
	}
	if _, ok := connectionDir("not:a:device"); ok {
		t.Fatal("a malformed device resolves nothing")
	}
}

func TestBFS052NegativeControlSourcePointsAtRealFiles(t *testing.T) {
	// The sources must be the REAL kernel paths in production, not the
	// fixture vars a test left behind (restore discipline witness).
	if procMountinfo != "/proc/self/mountinfo" || sysClassBDI != "/sys/class/bdi" {
		t.Fatal("production source vars were not restored by a previous cell")
	}
}
