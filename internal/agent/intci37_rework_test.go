package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
)

// ── INT-CI-37 rework: pinned to the two hosted rejection shapes ─────────────
//
// The first hosted post-fix run (35844834462) rejected 36553bd on exactly two
// fault classes, reproduced here against the same seam vars production code
// reads, so a pass pins the hosted shape itself:
//
//  1. Hosted runners source the root fs as `/dev/root` (a devtmpfs NAME,
//     not a partition). resolveWholeDiskDeviceReal only walks /sys/block for
//     the source's base name and answered
//     `no whole-disk device in /sys/block holds partition "root"`.
//  2. memory.high: requested 241591860 → observed 241590272 and requested
//     3865470480 → observed 3865468928; each observed value is
//     floor(requested/4096)*4096, the kernel's page-granular normalization.

// hostPageSize pins the fixture page size: the hosted runners carry 4 KiB
// pages and the fixture numbers below are 4 KiB-derived; on any other page
// size the fixtures would lie, so the suite skips naming the page instead of
// passing for the wrong reason.
func hostPageSize(t *testing.T) uint64 {
	t.Helper()
	const want = uint64(4096)
	if p := os.Getpagesize(); uint64(p) != want {
		t.Skipf("fixtures declare 4 KiB pages, this host reports %d", p)
	}
	return want
}

// ── A. the /dev/root hosted-runner shape ────────────────────────────────────

// intci37ReworkSetup builds the hosted-runner shape inside a temp root and
// patches the production seam vars (all recovered via t.Cleanup):
//
//   - <root>/proc/mountinfo: a real-shaped root line — mount id, parent,
//     <maj>:<min> (field index 2), "/"/" / ", options, "-" separator,
//     fstype, and the devtmpfs NAME `/dev/root` as the source. The root
//     mountinfo record's major:minor is the ONLY reliable device identity
//     on these hosts; stat-ing the /dev/root node would alias other
//     mounts' devices.
//   - <root>/sys/dev/block/<maj>:<min> → ../../block/<disk>/<part>: the
//     kernel's canonical alias (same relative shape, re-rooted).
//   - <root>/sys/block/<disk>/<part>: the whole-disk hierarchy the walker
//     maps partition → parent disk in.
func intci37ReworkSetup(t *testing.T, source, majMin, part, disk string) {
	t.Helper()
	hostPageSize(t)
	root := t.TempDir()

	sysBlock := filepath.Join(root, "sys", "block")
	if err := os.MkdirAll(filepath.Join(sysBlock, disk, part), 0o755); err != nil {
		t.Fatal(err)
	}
	devBlock := filepath.Join(root, "sys", "dev", "block")
	if err := os.MkdirAll(devBlock, 0o755); err != nil {
		t.Fatal(err)
	}
	// Target relative to <root>/sys/dev/block: ../../block → <root>/sys/block.
	if err := os.Symlink(
		filepath.Join("..", "..", "block", disk, part),
		filepath.Join(devBlock, majMin)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := "36 35 " + majMin + " / / rw - ext4 " + source + " rw\n"
	mi := filepath.Join(root, "proc", "mountinfo")
	if err := os.WriteFile(mi, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	prevMountInfo := rootDeviceMountInfoPath
	prevSysBlock := wholeDiskSysBlockRoot
	prevDevBlock := sysDevBlockRoot
	rootDeviceMountInfoPath = mi
	wholeDiskSysBlockRoot = sysBlock
	sysDevBlockRoot = devBlock
	t.Cleanup(func() {
		rootDeviceMountInfoPath = prevMountInfo
		wholeDiskSysBlockRoot = prevSysBlock
		sysDevBlockRoot = prevDevBlock
	})
}

// intci37ReworkSetupNoMapping is the /dev/root shape MINUS any sysfs alias:
// the resolver must refuse loud, name the major:minor it tried, and never
// echo the literal word `root` back as a disk name.
func intci37ReworkSetupNoMapping(t *testing.T) {
	t.Helper()
	root := t.TempDir()

	emptyBlock := filepath.Join(root, "sys", "block")
	if err := os.MkdirAll(emptyBlock, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := "36 35 254:1 / / rw - ext4 /dev/root rw\n"
	mi := filepath.Join(root, "proc", "mountinfo")
	if err := os.WriteFile(mi, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	prevMountInfo := rootDeviceMountInfoPath
	prevSysBlock := wholeDiskSysBlockRoot
	prevDevBlock := sysDevBlockRoot
	rootDeviceMountInfoPath = mi
	wholeDiskSysBlockRoot = emptyBlock
	// sys/dev/block exists in production but holds nothing for this shape:
	// point the seam at a directory with no alias entry.
	sysDevBlockRoot = filepath.Join(root, "sys", "dev", "block")
	t.Cleanup(func() {
		rootDeviceMountInfoPath = prevMountInfo
		wholeDiskSysBlockRoot = prevSysBlock
		sysDevBlockRoot = prevDevBlock
	})
}

// TestINTCI37Rework_RootDeviceViaDevBlock pins the hosted /dev/root shape
// that rejected the first fix: the mount source cannot identify a sysfs
// partition (it is the devtmpfs NAME `root`), so the resolver uses the root
// mountinfo record's major:minor and the /sys/dev/block canonical target to
// identify the backing partition, then maps it to its whole-disk parent
// under /sys/block. The literal word `root` is never treated as a disk name.
func TestINTCI37Rework_RootDeviceViaDevBlock(t *testing.T) {
	intci37ReworkSetup(t, "/dev/root", "254:1", "vda1", "vda")

	dev, err := resolveWholeDiskDeviceReal()
	if err != nil {
		t.Fatalf("resolver failed against the hosted /dev/root fixture: %v", err)
	}
	if dev != "/dev/vda" {
		t.Fatalf("resolver returned %q, want the whole disk /dev/vda", dev)
	}
	if strings.HasSuffix(dev, "1") {
		t.Fatalf("resolver returned a partition-looking device %q (io.max rejects partitions)", dev)
	}
}

// TestINTCI37Rework_RootDeviceViaDevBlockNVMe runs the same hosted shape for
// the nvme family: /sys/dev/block/259:2 → .../nvme0n1/nvme0n1p2, whole disk
// nvme0n1.
func TestINTCI37Rework_RootDeviceViaDevBlockNVMe(t *testing.T) {
	intci37ReworkSetup(t, "/dev/root", "259:2", "nvme0n1p2", "nvme0n1")

	dev, err := resolveWholeDiskDeviceReal()
	if err != nil {
		t.Fatalf("resolver failed against the hosted nvme fixture: %v", err)
	}
	if dev != "/dev/nvme0n1" {
		t.Fatalf("resolver returned %q, want /dev/nvme0n1", dev)
	}
}

// TestINTCI37Rework_RootDeviceRefusalWhenNoSysfsMapping pins the refusal:
// with no /sys/dev/block alias and no /sys/block partition, the resolver
// fails loud naming the major:minor — never guessing a disk, never treating
// `root` as one.
func TestINTCI37Rework_RootDeviceRefusalWhenNoSysfsMapping(t *testing.T) {
	intci37ReworkSetupNoMapping(t)

	dev, err := resolveWholeDiskDeviceReal()
	if err == nil {
		t.Fatalf("resolver invented %q with no sysfs mapping — must refuse, never treat \"root\" as a disk", dev)
	}
	if strings.Contains(err.Error(), `holds partition "root"`) {
		t.Fatalf("refusal echoed the raw /dev/root token as a disk name: %v", err)
	}
	if !strings.Contains(err.Error(), "254:1") {
		t.Fatalf("refusal does not name the major:minor it tried: %v", err)
	}
}

// ── C. the MemoryHigh page-normalization contract ───────────────────────────

// TestINTCI37Rework_MemoryHighPageCanonicalValue pins canonicalMemoryHigh on
// the live shapes: the canonical value is the requested bytes floored to the
// system page size — exactly the number the hosted kernels reported back.
// Rafinement guards: already-aligned values are unchanged; floor-to-zero
// never hides a sub-page request.
func TestINTCI37Rework_MemoryHighPageCanonicalValue(t *testing.T) {
	page := hostPageSize(t)

	for _, tc := range []struct {
		name      string
		requested uint64
	}{
		{"contained default 241591860", 241591860},
		{"admin override 3865470480", 3865470480},
		{"already page-aligned", page},
		{"aligned+1 floors down", page + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := (tc.requested / page) * page
			if got := canonicalMemoryHigh(tc.requested); got != want {
				t.Fatalf("canonicalMemoryHigh(%d) = %d, want floor-to-page %d", tc.requested, got, want)
			}
		})
	}

	// A sub-page request must never canonicalize to 0: that would drop the
	// knob from the emit path (the emit skips 0) and silently disarm the
	// landing check.
	if got := canonicalMemoryHigh(page - 1); got == 0 {
		t.Fatalf("canonicalMemoryHigh(%d) = 0: a sub-page request silently disappeared", page-1)
	}
}

// TestINTCI37Rework_MemoryHighLivePairsVerifyExactly drives the FULL landing
// check with both hosted value pairs, verbatim: the fixture cgroup carries
// the exact observed numbers the hosted run read back, and the request is
// the requested value (verification canonicalizes before comparing).
// There is no tolerance and no near-match — the check passes only because
// the canonicalization IS the kernel's own rounding.
func TestINTCI37Rework_MemoryHighLivePairsVerifyExactly(t *testing.T) {
	m := &AgentManager{cfg: config.DefaultConfig(), logger: testLogger()}
	uid := "4242"

	for _, tc := range []struct {
		name      string
		requested uint64
		observed  uint64
	}{
		{"contained default 241591860 observed 241590272", 241591860, 241590272},
		{"admin override 3865470480 observed 3865468928", 3865470480, 3865468928},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sliceDir := filepath.Join(root, "user.slice", "user-4242.slice")
			if err := os.MkdirAll(sliceDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sliceDir, "memory.high"),
				[]byte(fmt.Sprintf("%d\n", tc.observed)), 0o644); err != nil {
				t.Fatal(err)
			}
			prevRoot := cgroupV2Root
			cgroupV2Root = root
			t.Cleanup(func() { cgroupV2Root = prevRoot })

			want := containmentResolved{memHigh: tc.requested}
			if err := m.verifyContainmentLanding(uid, want); err != nil {
				t.Fatalf("page-normalized pair (%d requested vs %d observed) failed: %v",
					tc.requested, tc.observed, err)
			}
		})
	}
}

// TestINTCI37Rework_MemoryHighWrongValueStillFailsLoud proves the contract
// is NOT a tolerance: a value one page above or below the canonical number —
// or the raw un-normalized request itself — still fails with the
// requested-vs-observed pair intact.
func TestINTCI37Rework_MemoryHighWrongValueStillFailsLoud(t *testing.T) {
	page := hostPageSize(t)
	m := &AgentManager{cfg: config.DefaultConfig(), logger: testLogger()}
	uid := "4242"
	canon := canonicalMemoryHigh(241591860)

	for _, tc := range []struct {
		name     string
		observed uint64
	}{
		{"one page above", canon + page},
		{"one page below", canon - page},
		{"raw un-normalized request", 241591860},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sliceDir := filepath.Join(root, "user.slice", "user-4242.slice")
			if err := os.MkdirAll(sliceDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sliceDir, "memory.high"),
				[]byte(fmt.Sprintf("%d\n", tc.observed)), 0o644); err != nil {
				t.Fatal(err)
			}
			prevRoot := cgroupV2Root
			cgroupV2Root = root
			t.Cleanup(func() { cgroupV2Root = prevRoot })

			err := m.verifyContainmentLanding(uid, containmentResolved{memHigh: 241591860})
			if err == nil {
				t.Fatalf("memory.high=%d passed a %d request (canonicalization became a tolerance)", tc.observed, canon)
			}
			if !strings.Contains(err.Error(), "memory.high") {
				t.Fatalf("failure does not name the knob: %v", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", tc.observed)) {
				t.Fatalf("observed value lost from diagnostics: %v", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("want %d", canon)) {
				t.Fatalf("canonical requested value lost from diagnostics: %v", err)
			}
		})
	}
}

// ── D. the emit path carries the canonical value ────────────────────────────

// TestINTCI37Rework_EmitPathUsesCanonicalValue pins the emit-side half of
// the canonical contract: sliceContainmentKnobs renders the page-floored
// number so the drop-in itself carries the value the kernel will hold,
// keeping emit and verify on one deterministic value.
func TestINTCI37Rework_EmitPathUsesCanonicalValue(t *testing.T) {
	page := hostPageSize(t)

	r := containmentResolved{swapBarred: true, memHigh: 241591860}
	knobs, err := sliceContainmentKnobs(r)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	sawHigh := false
	want := fmt.Sprintf("%d", (241591860/page)*page)
	for _, k := range knobs {
		if k.Name == "MemoryHigh" {
			sawHigh = true
			if k.Value != want {
				t.Fatalf("MemoryHigh render = %q, want the page-canonical %q", k.Value, want)
			}
		}
	}
	if !sawHigh {
		t.Fatalf("render dropped the MemoryHigh knob: %+v", knobs)
	}
}

// TestINTCI37Rework_ResolvedKnobsCarryCanonicalDiscovery covers the resolve
// half end-to-end: the tier table's 90% cushion of 4GiB must arrive in the
// resolution already canonical (3865468928), in both the knob render and the
// landing check — the hosted value pair pinned as the baseline-default case.
func TestINTCI37Rework_ResolvedKnobsCarryCanonicalDiscovery(t *testing.T) {
	const wantHigh = uint64(3865468928) // canonical(resolved 90% of 4GiB)

	r := resolveContainmentKnobsWithPage(4294967296, 4096)
	if r.memHigh != wantHigh {
		t.Fatalf("resolveContainmentKnobs memHigh = %d, want the page-canonical %d", r.memHigh, wantHigh)
	}
	knobs, err := sliceContainmentKnobs(r)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	for _, k := range knobs {
		if k.Name == "MemoryHigh" && k.Value != fmt.Sprintf("%d", wantHigh) {
			t.Fatalf("MemoryHigh render = %q, want %q", k.Value, fmt.Sprintf("%d", wantHigh))
		}
	}
	// And the landing check verifies exactly the canonical readback the
	// hosted run observed for this shape.
	root := t.TempDir()
	sliceDir := filepath.Join(root, "user.slice", "user-4242.slice")
	if err := os.MkdirAll(sliceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sliceDir, "memory.swap.max"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sliceDir, "memory.high"),
		[]byte(fmt.Sprintf("%d\n", wantHigh)), 0o644); err != nil {
		t.Fatal(err)
	}
	prevRoot := cgroupV2Root
	cgroupV2Root = root
	t.Cleanup(func() { cgroupV2Root = prevRoot })
	if err := (&AgentManager{cfg: config.DefaultConfig(), logger: testLogger()}).verifyContainmentLanding("4242", r); err != nil {
		t.Fatalf("the canonical resolution failed its own landing check: %v", err)
	}
}

// resolveContainmentKnobsWithPage drives the production resolution with the
// page size pinned, so the fixture page (4 KiB, the hosted runners') is
// provable rather than assumed from the test host.
func resolveContainmentKnobsWithPage(memMax, page uint64) containmentResolved {
	prev := systemPageSizeBytes
	systemPageSizeBytes = page
	return resolveContainmentKnobsForTest(memMax, func() { systemPageSizeBytes = prev })
}

// resolveContainmentKnobsForTest is the production resolver with a deferred
// page-seam restore applied by the caller.
func resolveContainmentKnobsForTest(memMax uint64, restore func()) containmentResolved {
	defer restore()
	return resolveContainmentKnobs(config.SafetyPresetStandard, memMax, config.AgentConfig{})
}
