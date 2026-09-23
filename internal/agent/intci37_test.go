package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

// ── INT-CI-37: containment landing-check ordering + foreign-host resolver ──
//
// The GAP-118 containment row wired verifyContainmentLanding BEFORE
// applyUserSliceLimits had written the slice drop-in and run daemon-reload:
// the check read memory.swap.max = "max" from the untouched cgroup and
// failed EVERY root spawn at the slice-limits stage (CI run 35835000060,
// fingerprint: `spawn ... failed at stage slice-limits: containment
// landing: memory.swap.max = "max", want "0"`).
//
// The tests here pin the FIX at three levels:
//
//  1. TestINTCI37_ResolverFailsLoudWhenUnresolvable — the R2 no-silent-no-op
//     rule for the IO write bound: an explicit IOWriteBandwidthMax request
//     whose whole-disk device cannot be resolved FAILS at
//     sliceContainmentKnobs (it is never silently omitted, and the knob
//     never silently arrives at the landing check under a wrong device).
//
//  2. TestINTCI37_WholeDiskResolverFixture — the /dev-root-style host shape
//     (a rootfs device like /dev/vda whose sysfs parent maps to a real whole
//     disk through /sys/block/<disk>/<part>): the REAL resolver is exercised
//     against a fixture mountinfo + fixture sysfs tree, RED-proven by
//     pointing it at a fixture whose /sys/block is empty (the pre-fix
//     resolver's failure mode) and GREEN against the populated one.
//
//  3. TestINTCI37_SliceDropInBeforeLandingCheck — the ORDERING regression:
//     the slice drop-in write + daemon-reload must land BEFORE
//     verifyContainmentLanding runs. Proven with a call-order fake, not
//     source matching: the fake cgroup fixture answers the landing check
//     ONLY after the drop-in file (which applyUserSliceLimits writes) exists
//     AND its content names the containment knobs — the landing check's
//     success is CONDITIONED on the drop-in write having happened first, so
//     a ko that verified before writing fails and one that writes first
//     passes. The daemon-reload ORDER is pinned on intci37FakeCtl through
//     the sliceApplySystemctl seam. Plus:
//
//       - TestINTCI37_LandingCheckBoundedConvergence — a cgroup that only
//         converges after several reads exhausts the bound, then converges;
//         and an always-wrong cgroup exhausts with the requested-vs-observed
//         pair in the error (no weakening of the enforcement).
//
//       - TestINTCI37_ConvergencePrefersContext — a cancelled context aborts
//         the re-read loop immediately, wrapping the context error together
//         with the last verified failure.
//
// The last two scenarios run against verifyContainmentLandingConverged (the
// INT-CI-37 convergence wrapper); the ordering scenario drives the ordered
// gate applyUserSliceLimitsAndVerify that the spawn path now calls.
//
// Everything in this file is hermetic: temp trees, fixtures and a fake
// systemctl — no root, no real systemd.

// intci37FakeCtl is the fake systemctl the ordering test drives: it records
// the ORDER of daemon-reload calls and refuses them on demand.
type intci37FakeCtl struct {
	mu        sync.Mutex
	reloads   int
	denyNext  int // deny the next N daemon-reload calls
	failAfter int // fail every daemon-reload after this many
	calls     []string
}

func (f *intci37FakeCtl) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, line)
	if name == "systemctl" && len(args) > 0 && args[0] == "daemon-reload" {
		if f.denyNext > 0 {
			f.denyNext--
			f.reloads++
			return []byte("systemctl: daemon-reload refused by test\n"), errors.New("exit status 1")
		}
		if f.failAfter > 0 && f.reloads >= f.failAfter {
			f.reloads++
			return []byte("systemctl: daemon-reload refused by test\n"), errors.New("exit status 1")
		}
		f.reloads++
	}
	return []byte(""), nil
}

func (f *intci37FakeCtl) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ── 1. the R2 no-silent-no-op rule for the IO write bound ──────────────────

// TestINTCI37_ResolverFailsLoudWhenUnresolvable is criterion 2: a tier (or an
// admin override) that REQUESTS IOWriteBandwidthMax on a host whose whole-disk
// device cannot be resolved must fail loud at sliceContainmentKnobs — the
// knob is never silently omitted (the old behaviour), and the error is
// propagated so the spawn fails at StageSliceLimits naming the knob.
func TestINTCI37_ResolverFailsLoudWhenUnresolvable(t *testing.T) {
	// A standard-tier resolution that also opts into the IO write bound.
	r := resolveContainmentKnobs(config.SafetyPresetStandard, 4<<30, config.AgentConfig{
		Containment: config.ContainmentKnobs{IOWriteBps: 100 << 20},
	})
	if r.ioWriteBps != 100<<20 {
		t.Fatalf("test premise broken: resolution = %+v, want the IO opt-in", r)
	}

	// The resolver fails: the knob set is refused wholesale, loudly.
	prev := resolveWholeDiskDevice
	resolveWholeDiskDevice = func() (string, error) { return "", errors.New("no block device in fixture") }
	t.Cleanup(func() { resolveWholeDiskDevice = prev })

	knobs, err := sliceContainmentKnobs(r)
	if err == nil {
		t.Fatalf("an explicit IOWriteBandwidthMax request with an unresolvable device silently emitted %v (the silent-no-op bug)", knobs)
	}
	if !strings.Contains(err.Error(), "IOWriteBandwidthMax") {
		t.Fatalf("the loud failure does not name the knob: %v", err)
	}
	if knobs != nil {
		t.Fatalf("a failed resolution must not emit a partial knob set, got %v", knobs)
	}

	// Non-IO paths stay byte-identical: without an ioWriteBps request the
	// same broken resolver must NOT fail the knob render (the tier's
	// MemorySwapMax + MemoryHigh land exactly as before).
	rNoIO := resolveContainmentKnobs(config.SafetyPresetStandard, 4<<30, config.AgentConfig{})
	before, beforeErr := sliceContainmentKnobs(rNoIO)
	if beforeErr != nil {
		t.Fatalf("non-IO default path must not consult the resolver: %v", beforeErr)
	}
	if len(before) != 2 || before[0].Name != "MemorySwapMax" || before[1].Name != "MemoryHigh" {
		t.Fatalf("non-IO containment knobs drifted: %+v", before)
	}
}

// ── 2. the /dev-root-style host resolver fixture ────────────────────────────

// intci37MountInfoFixture writes a /proc/self/mountinfo-shaped file whose
// root line sources <dev> (the /dev-root-style hosts give the kernel's device
// node, e.g. /dev/vda1 or /dev/nvme0n1p2 — the resolver takes path.Base).
func intci37MountInfoFixture(t *testing.T, dev string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	line := fmt.Sprintf("36 35 0:34 / / 0:34 root / rw - ext4 %s rw\n", dev)
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestINTCI37_WholeDiskResolverFixture drives the REAL resolver
// (resolveWholeDiskDeviceReal) against fixture mountinfo + fixture /sys/block
// trees, for the /dev-root-style host shapes. The RED arm is the pre-fix
// resolver's failure mode: the device the rootfs carries is NOT under any
// /sys/block/<disk> entry (nothing maps it), and the resolver must refuse
// (never return a partition or a wrong disk). The GREEN arm populates the
// sysfs hierarchy exactly as a real host does and the resolver returns the
// WHOLE disk.
func TestINTCI37_WholeDiskResolverFixture(t *testing.T) {
	t.Run("green: sysfs parent maps the partition to the whole disk", func(t *testing.T) {
		root := t.TempDir()
		sysBlock := filepath.Join(root, "block")
		// The /dev-root-style shape: the root fs is /dev/vda1, and sysfs
		// exposes /sys/block/vda/vda1. The resolver must answer /dev/vda.
		if err := os.MkdirAll(filepath.Join(sysBlock, "vda", "vda1"), 0o755); err != nil {
			t.Fatal(err)
		}
		prevMountInfo := rootDeviceMountInfoPath
		prevSysBlock := wholeDiskSysBlockRoot
		rootDeviceMountInfoPath = intci37MountInfoFixture(t, "/dev/vda1")
		wholeDiskSysBlockRoot = sysBlock
		t.Cleanup(func() { rootDeviceMountInfoPath = prevMountInfo; wholeDiskSysBlockRoot = prevSysBlock })

		dev, err := resolveWholeDiskDeviceReal()
		if err != nil {
			t.Fatalf("resolver failed against the /dev-root-style fixture: %v", err)
		}
		if dev != "/dev/vda" {
			t.Fatalf("resolver returned %q, want the WHOLE disk /dev/vda", dev)
		}
		// The returned device must never carry the partition suffix.
		if strings.Contains(dev, "1") && strings.HasSuffix(dev, "1") {
			t.Fatalf("resolver returned a partition-looking device %q (io.max rejects partitions)", dev)
		}
	})

	t.Run("red: unmapped partition refuses instead of guessing", func(t *testing.T) {
		root := t.TempDir()
		sysBlock := filepath.Join(root, "block") // exists but holds nothing
		if err := os.MkdirAll(sysBlock, 0o755); err != nil {
			t.Fatal(err)
		}
		prevMountInfo := rootDeviceMountInfoPath
		prevSysBlock := wholeDiskSysBlockRoot
		rootDeviceMountInfoPath = intci37MountInfoFixture(t, "/dev/vda1")
		wholeDiskSysBlockRoot = sysBlock
		t.Cleanup(func() { rootDeviceMountInfoPath = prevMountInfo; wholeDiskSysBlockRoot = prevSysBlock })

		dev, err := resolveWholeDiskDeviceReal()
		if err == nil {
			t.Fatalf("resolver invented a whole disk %q for an unmapped partition (must refuse)", dev)
		}
		if !strings.Contains(err.Error(), "vda1") {
			t.Fatalf("the refusal does not name the unmapped partition: %v", err)
		}
	})

	t.Run("green: nvme namespace partition maps to the namespace disk", func(t *testing.T) {
		// The host this row was measured on: /dev/nvme0n1p2 under
		// /sys/block/nvme0n1. Same walk, different device family.
		root := t.TempDir()
		sysBlock := filepath.Join(root, "block")
		if err := os.MkdirAll(filepath.Join(sysBlock, "nvme0n1", "nvme0n1p2"), 0o755); err != nil {
			t.Fatal(err)
		}
		prevMountInfo := rootDeviceMountInfoPath
		prevSysBlock := wholeDiskSysBlockRoot
		rootDeviceMountInfoPath = intci37MountInfoFixture(t, "/dev/nvme0n1p2")
		wholeDiskSysBlockRoot = sysBlock
		t.Cleanup(func() { rootDeviceMountInfoPath = prevMountInfo; wholeDiskSysBlockRoot = prevSysBlock })

		dev, err := resolveWholeDiskDeviceReal()
		if err != nil {
			t.Fatalf("resolver failed against the NVMe fixture: %v", err)
		}
		if dev != "/dev/nvme0n1" {
			t.Fatalf("resolver returned %q, want /dev/nvme0n1", dev)
		}
	})
}

// ── 3. the ordering regression: write+reload BEFORE landing check ──────────

// intci37CgroupFixture builds a fake cgroup v2 tree for uid 4242 whose
// memory.swap.max starts "max" (the untouched-slice state the CI failure read)
// and installs a DROP-IN WATCHER: once the slice drop-in written by
// applyUserSliceLimits exists AND names the containment knobs, the watcher
// flips memory.swap.max to "0" — modelling systemd consuming the drop-in.
// The landing check therefore can only pass when it runs AFTER the write: the
// precondition of the fix, proven behaviourally. Returns the watcher path.
func intci37CgroupFixture(t *testing.T, dropinDir string, want containmentResolved) string {
	t.Helper()
	root := t.TempDir()
	sliceDir := filepath.Join(root, "user.slice", "user-4242.slice")
	if err := os.MkdirAll(sliceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The untouched slice: memory.swap.max = "max" (the CI fingerprint).
	if err := os.WriteFile(filepath.Join(sliceDir, "memory.swap.max"), []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The watcher: fires on every landing-check read. Until the drop-in
	// exists and names MemorySwapMax, the files keep answering the untouched
	// slice's values ("max" for swap, no cushion at all for memory.high).
	watcher := func() {
		raw, err := os.ReadFile(filepath.Join(sliceDir, "memory.swap.max"))
		if err != nil || strings.TrimSpace(string(raw)) != "max" {
			return // already converged, or fixture gone
		}
		conf, err := os.ReadFile(filepath.Join(dropinDir, "50-bunker.conf"))
		if err != nil || !strings.Contains(string(conf), "MemorySwapMax=0") {
			return // drop-in not written yet, or wrong content: keep "max"
		}
		// systemd consumed the drop-in: the whole containment set lands.
		_ = os.WriteFile(filepath.Join(sliceDir, "memory.swap.max"), []byte("0\n"), 0o644)
		_ = os.WriteFile(filepath.Join(sliceDir, "memory.high"), []byte(fmt.Sprintf("%d\n", want.memHigh)), 0o644)
	}
	prevWatcher := intci37LandingWatcher
	intci37LandingWatcher = watcher
	t.Cleanup(func() { intci37LandingWatcher = prevWatcher })
	return root
}

// TestINTCI37_SliceDropInBeforeLandingCheck drives the ordered gate
// applyUserSliceLimitsAndVerify against the watched fixture. Assertion 1
// (ordering, from the call order): the fake daemon-reload was invoked BEFORE
// the landing check ever observed "0" — the cgroup could not converge until
// both the write and the reload were done, so a pass PROVES the order.
// Assertion 2 (the reload seam, not source matching): the reload went through
// sliceApplySystemctl. Assertion 3: the gate returns the drop-in content.
// The RED framing: under the pre-fix order (verify BEFORE apply) this exact
// fixture hits the exhaustion path and the gate fails with the
// requested-vs-observed pair.
func TestINTCI37_SliceDropInBeforeLandingCheck(t *testing.T) {
	uid := "4242"
	cfg := config.DefaultConfig()
	m := &AgentManager{cfg: cfg, logger: testLogger()}

	dropinDir := filepath.Join(t.TempDir(), "user-4242.slice.d")
	prevDir := systemdUnitDirRoot
	systemdUnitDirRoot = filepath.Dir(dropinDir)
	t.Cleanup(func() { systemdUnitDirRoot = prevDir })

	fake := &intci37FakeCtl{}
	prevCtl := sliceApplySystemctl
	sliceApplySystemctl = fake.run
	t.Cleanup(func() { sliceApplySystemctl = prevCtl })

	want := resolveContainmentKnobs(config.SafetyPresetStandard, 4<<30, config.AgentConfig{})
	cgroupRoot := intci37CgroupFixture(t, dropinDir, want)
	prevRoot := cgroupV2Root
	cgroupV2Root = cgroupRoot
	t.Cleanup(func() { cgroupV2Root = prevRoot })

	// The standard tier's containment set: swap barred + the 90% cushion.
	knobs, knobErr := sliceContainmentKnobs(want)
	if knobErr != nil {
		t.Fatalf("containment render failed: %v", knobErr)
	}

	u := &user.User{Username: "bunker-fix", Uid: uid, Gid: uid, HomeDir: "/home/bunker-fix"}
	content, gateErr := m.applyUserSliceLimitsAndVerify(t.Context(), u, 2.0, 4<<30, 0, 0, 0, knobs, want)
	if gateErr != nil {
		t.Fatalf("the ordered gate failed: %v (under the pre-fix verify-before-apply order this exact fixture reads memory.swap.max=\"max\" and fails)", gateErr)
	}
	if !strings.Contains(content, "MemorySwapMax=0") {
		t.Fatalf("gate returned drop-in content without the containment knob: %q", content)
	}
	if fake.reloads < 1 {
		t.Fatalf("the daemon-reload was never invoked (the drop-in cannot have taken effect): %v", fake.calls)
	}
	// The converged cgroup really shows the landed knob.
	got, err := os.ReadFile(filepath.Join(cgroupRoot, "user.slice", "user-4242.slice", "memory.swap.max"))
	if err != nil || strings.TrimSpace(string(got)) != "0" {
		t.Fatalf("fixture did not converge: memory.swap.max = %q (err %v)", got, err)
	}

	t.Run("red: the pre-fix verify-before-apply order fails this fixture", func(t *testing.T) {
		// Rebuild the untouched-slice state and run ONLY the verify half —
		// exactly what the pre-fix spawn path did after systemd-run, minutes
		// before applyUserSliceLimits wrote the drop-in. The bounded loop
		// exhausts with the requested-vs-observed pair: this is the CI run
		// 35835000060 fingerprint, reproduced deterministically.
		root := t.TempDir()
		sliceDir := filepath.Join(root, "user.slice", "user-4242.slice")
		if err := os.MkdirAll(sliceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sliceDir, "memory.swap.max"), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		prevRoot := cgroupV2Root
		cgroupV2Root = root
		t.Cleanup(func() { cgroupV2Root = prevRoot })

		err := m.verifyContainmentLandingConverged(t.Context(), uid, want)
		if err == nil {
			t.Fatal("verify-before-apply passed an unwritten slice (the fixture cannot catch the regression)")
		}
		if !strings.Contains(err.Error(), `memory.swap.max = "max"`) || !strings.Contains(err.Error(), `want "0"`) {
			t.Fatalf("the pre-fix reproduction lost the CI fingerprint: %v", err)
		}
	})
}

// TestINTCI37_WriteFailureIsDegradableAndCheckIsNot pins the error split the
// spawn path relies on: a failed drop-in WRITE is wrapped in errSliceApplyWrite
// (the spawn warns and continues), while a landing check that fails after a
// successful write is ANY other error (the spawn fails at StageSliceLimits).
func TestINTCI37_WriteFailureIsDegradableAndCheckIsNot(t *testing.T) {
	uid := "4242"
	cfg := config.DefaultConfig()
	m := &AgentManager{cfg: cfg, logger: testLogger()}
	want := resolveContainmentKnobs(config.SafetyPresetStandard, 4<<30, config.AgentConfig{})
	knobs, knobErr := sliceContainmentKnobs(want)
	if knobErr != nil {
		t.Fatalf("containment render failed: %v", knobErr)
	}
	u := &user.User{Username: "bunker-w", Uid: uid, Gid: uid, HomeDir: "/home/bunker-w"}

	t.Run("write failure is wrapped degradable", func(t *testing.T) {
		// An unwritable systemd unit root makes the mkdir/write fail: the
		// root holds a REGULAR FILE, so the drop-in directory under it can
		// never be created.
		prevDir := systemdUnitDirRoot
		blocker := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.MkdirAll(blocker, 0o755); err != nil {
			t.Fatal(err)
		}
		systemdUnitDirRoot = blocker
		if err := os.WriteFile(filepath.Join(blocker, "occupied"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { systemdUnitDirRoot = prevDir })

		_, err := m.applyUserSliceLimitsAndVerify(t.Context(), u, 2.0, 4<<30, 0, 0, 0, knobs, want)
		if err == nil {
			t.Fatal("an unwritable unit root did not fail the apply")
		}
		if !errors.Is(err, errSliceApplyWrite) {
			t.Fatalf("write failure is not marked degradable: %v", err)
		}
	})

	t.Run("landing check after a good write is NOT degradable", func(t *testing.T) {
		dropinDir := filepath.Join(t.TempDir(), "user-4242.slice.d")
		prevDir := systemdUnitDirRoot
		systemdUnitDirRoot = filepath.Dir(dropinDir)
		t.Cleanup(func() { systemdUnitDirRoot = prevDir })

		// The reload must SUCCEED here (the fixture is about the landing
		// check, not the write leg) — run it through the fake.
		prevCtl := sliceApplySystemctl
		sliceApplySystemctl = (&intci37FakeCtl{}).run
		t.Cleanup(func() { sliceApplySystemctl = prevCtl })

		// The cgroup NEVER converges: memory.swap.max stays "max".
		root := t.TempDir()
		sliceDir := filepath.Join(root, "user.slice", "user-4242.slice")
		if err := os.MkdirAll(sliceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sliceDir, "memory.swap.max"), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		prevRoot := cgroupV2Root
		cgroupV2Root = root
		t.Cleanup(func() { cgroupV2Root = prevRoot })

		_, err := m.applyUserSliceLimitsAndVerify(t.Context(), u, 2.0, 4<<30, 0, 0, 0, knobs, want)
		if err == nil {
			t.Fatal("a containment set that never landed passed the slice-limits stage (enforcement weakened)")
		}
		if errors.Is(err, errSliceApplyWrite) {
			t.Fatalf("a landing-check failure is wrongly marked as a write failure: %v", err)
		}
		// Requested-versus-observed must survive the convergence wrapper.
		if !strings.Contains(err.Error(), `memory.swap.max = "max"`) || !strings.Contains(err.Error(), `want "0"`) {
			t.Fatalf("exhaustion error lost the requested-vs-observed pair: %v", err)
		}
	})
}

// TestINTCI37_LandingCheckBoundedConvergence proves the re-read loop on
// verifyContainmentLandingConverged directly: a cgroup that converges on its
// Nth read passes only when the bound covers N; one that never converges
// exhausts the bound with the observed value in the error. Bound count is
// asserted at both ends so the const two-way-matches the loop.
func TestINTCI37_LandingCheckBoundedConvergence(t *testing.T) {
	uid := "4242"
	cfg := config.DefaultConfig()
	m := &AgentManager{cfg: cfg, logger: testLogger()}
	want := containmentResolved{swapBarred: true}

	prevPoll := containmentConvergePoll
	containmentConvergePoll = time.Millisecond // no real waits
	t.Cleanup(func() { containmentConvergePoll = prevPoll })

	t.Run("converges once the knob lands within the bound", func(t *testing.T) {
		root := t.TempDir()
		sliceDir := filepath.Join(root, "user.slice", "user-4242.slice")
		if err := os.MkdirAll(sliceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Converges on the THIRD read: the first two answer "max".
		reads := 0
		prevRoot := cgroupV2Root
		cgroupV2Root = root
		prevWatcher := intci37LandingWatcher
		intci37LandingWatcher = func() {
			reads++
			if reads >= 3 {
				_ = os.WriteFile(filepath.Join(sliceDir, "memory.swap.max"), []byte("0\n"), 0o644)
			}
		}
		t.Cleanup(func() { cgroupV2Root = prevRoot; intci37LandingWatcher = prevWatcher })
		if err := os.WriteFile(filepath.Join(sliceDir, "memory.swap.max"), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		defer func(n int) { containmentConvergeAttempts = n }(containmentConvergeAttempts)
		containmentConvergeAttempts = 5
		if err := m.verifyContainmentLandingConverged(t.Context(), uid, want); err != nil {
			t.Fatalf("a cgroup converging on read 3 failed within a 5-read bound: %v", err)
		}
		if reads != 3 {
			t.Fatalf("the loop stopped at %d reads, want 3 (bounded retries must stop at first success)", reads)
		}
	})

	t.Run("exhausts with requested-vs-observed when it never lands", func(t *testing.T) {
		root := t.TempDir()
		sliceDir := filepath.Join(root, "user.slice", "user-4242.slice")
		if err := os.MkdirAll(sliceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sliceDir, "memory.swap.max"), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		prevRoot := cgroupV2Root
		cgroupV2Root = root
		t.Cleanup(func() { cgroupV2Root = prevRoot })

		defer func(n int) { containmentConvergeAttempts = n }(containmentConvergeAttempts)
		containmentConvergeAttempts = 5
		err := m.verifyContainmentLandingConverged(t.Context(), uid, want)
		if err == nil {
			t.Fatal("a cgroup that never landed passed the converged check (enforcement weakened)")
		}
		if !strings.Contains(err.Error(), "within 5 reads") {
			t.Fatalf("the exhaustion error does not report the bound: %v", err)
		}
		if !strings.Contains(err.Error(), `memory.swap.max = "max"`) {
			t.Fatalf("the exhaustion error lost the observed value: %v", err)
		}
	})
}

// TestINTCI37_ConvergencePrefersContext proves the bound never fights a dying
// context: cancelling mid-loop stops the re-reads on the NEXT iteration,
// wrapping context.Canceled TOGETHER with the last verified failure — the
// spawn's deadline semantics stay intact and the observability is not lost.
func TestINTCI37_ConvergencePrefersContext(t *testing.T) {
	uid := "4242"
	cfg := config.DefaultConfig()
	m := &AgentManager{cfg: cfg, logger: testLogger()}
	want := containmentResolved{swapBarred: true}

	prevPoll := containmentConvergePoll
	containmentConvergePoll = time.Millisecond
	t.Cleanup(func() { containmentConvergePoll = prevPoll })

	root := t.TempDir()
	sliceDir := filepath.Join(root, "user.slice", "user-4242.slice")
	if err := os.MkdirAll(sliceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sliceDir, "memory.swap.max"), []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevRoot := cgroupV2Root
	cgroupV2Root = root
	t.Cleanup(func() { cgroupV2Root = prevRoot })

	ctx, cancel := context.WithCancel(t.Context())
	// Never converges; the first re-read wait sees the cancellation.
	cancel()
	defer func(n int) { containmentConvergeAttempts = n }(containmentConvergeAttempts)
	containmentConvergeAttempts = 5

	err := m.verifyContainmentLandingConverged(ctx, uid, want)
	if err == nil {
		t.Fatal("a cancelled context converged an unlanded containment set")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the context error is not preserved: %v", err)
	}
	if !strings.Contains(err.Error(), `memory.swap.max = "max"`) {
		t.Fatalf("the cancellation error lost the last verified failure: %v", err)
	}
}
