//go:build linux

package agent

// ISO-002 — unit tests for the /run/bunker PARENT enumeration seam
// (sockdir_root_linux.go). Mirrors sockdir_test.go's RED/GREEN discipline on
// the per-agent socket dir: every claim is bitten on the real filesystem.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkRunRoot builds an isolated spawnRunRoot path inside t.TempDir().
func mkRunRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "run", "bunker")
}

// stubRunRootOwner swaps the root assertion's ownership read-back seam to
// model uid (root in production) for the duration of the test. The hermetic
// suite cannot chown (not root) — the MODE half of the assertion always
// bites on the REAL directory, untouched by the seam.
func stubRunRootOwner(t *testing.T, uid uint32) {
	t.Helper()
	prev := runRootOwnerForTests
	runRootOwnerForTests = func(string) (uint32, bool) { return uid, true }
	t.Cleanup(func() { runRootOwnerForTests = prev })
}

// TestEnsureAgentRunRoot_Creates0700_Implicitly is AC (a): the root ends up
// 0700 root-owned even when it is CREATED IMPLICITLY as a side effect of
// creating an agent dir — i.e. WITHOUT an explicit MkdirAll(root) first, the
// Ensure the spawn path runs before Step 4c must both create and enforce it.
func TestEnsureAgentRunRoot_Creates0700_Implicitly(t *testing.T) {
	root := mkRunRoot(t)
	stubRunRootOwner(t, RunRootOwnerUID) // modeled 0700 root:root pass-back

	// The root does NOT exist yet (no explicit MkdirAll): the spawn calls
	// EnsureAgentRunRoot, then creates the agent dir underneath.
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("precondition: root %s unexpectedly exists already (%v)", root, err)
	}
	if err := EnsureAgentRunRoot(root); err != nil {
		t.Fatalf("EnsureAgentRunRoot: %v", err)
	}

	// Side-effect agent-dir creation underneath (spawn Step 4c shape).
	agentDir := filepath.Join(root, "df-iso-002", "tmp")
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		t.Fatalf("MkdirAll agent dir: %v", err)
	}

	// The PARENT is still exactly 0700 (umask did not widen it back to
	// 0755, MkdirAll recursion did not leak group/world bits).
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != RunRootMode {
		t.Fatalf("root mode after implicit creation is %#o, required %#o", got, RunRootMode)
	}
	// And the assertion — the same one the spawn runs — PASSES.
	if err := AssertAgentRunRoot(root); err != nil {
		t.Fatalf("AssertAgentRunRoot after implicit creation: %v", err)
	}
}

// TestAssertAgentRunRoot_Reddens_WorldReadable is AC (b): the assertion
// FAILS with the NAMED error (ErrRunRootMode) when the root is world- or
// group-readable — the bit pattern the 0755 parent carried before this fix
// (and the one that made `ls /run/bunker/` enumerate the tenant set). Mode
// is the binding half and is NEVER identity-scoped: a world-readable root
// reddens no matter who owns it or which uid runs the assertion.
func TestAssertAgentRunRoot_Reddens_WorldReadable(t *testing.T) {
	root := mkRunRoot(t)
	stubRunRootOwner(t, RunRootOwnerUID)

	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	err := AssertAgentRunRoot(root)
	if err == nil {
		t.Fatal("AssertAgentRunRoot must FAIL for a 0755 root (the enumeration seam) — got nil")
	}
	if !strings.Contains(err.Error(), ErrRunRootMode.Error()) {
		t.Fatalf("error must carry the NAMED sentinel ErrRunRootMode, got: %v", err)
	}
	if !strings.Contains(err.Error(), "enumeration seam") {
		t.Fatalf("error must name the enumeration seam, got: %v", err)
	}

	// The group-bit variant reddens identically (0750 — world off, group
	// on is still an enumeration seam for any co-tenant sharing gid).
	if err := os.Chmod(root, 0750); err != nil {
		t.Fatal(err)
	}
	if err := AssertAgentRunRoot(root); err == nil {
		t.Fatal("AssertAgentRunRoot must FAIL for a 0750 root — got nil")
	}

	// And the corrected 0700 root passes the same assertion.
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := AssertAgentRunRoot(root); err != nil {
		t.Fatalf("AssertAgentRunRoot must PASS for 0700 root-owned, got: %v", err)
	}

	// Identity scope: under a NON-root process (this suite uid) the owner
	// half is NOT asserted, so a legitimately-owned 0700 root passes even
	// though its real Stat_t owner is the suite uid — and the WORLD/GROUP
	// halves still seal (bite above proved that). The identity-scoped owner
	// half is what production (bunkerd as root) uses.
}

// TestAssertAgentRunRoot_OwnerHalf_ReddensUnderRootIdentity pins the
// identity-scoped owner half. The identity scope is REAL: the owner read only
// runs when the process is genuinely root (production bunkerd), and the
// seam models the Stat_t read-back that bites there. Skipped on non-root —
// the scope is by design (see AssertAgentRunRoot and the file header).
func TestAssertAgentRunRoot_OwnerHalf_ReddensUnderRootIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("identity-scoped owner half: the owner read-back only runs under the root identity (production bunkerd); the hermetic suite cannot model the root identity")
	}
	root := mkRunRoot(t)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}

	// Foreign owner seam → reddens (named refusal).
	stubRunRootOwner(t, 61001)
	err := AssertAgentRunRoot(root)
	if err == nil {
		t.Fatal("AssertAgentRunRoot must FAIL when the owner is a foreign uid (root identity)")
	}
	if !strings.Contains(err.Error(), "expected root (uid 0)") {
		t.Fatalf("refusal must name the expected root owner, got: %v", err)
	}

	// Root owner seam → passes.
	stubRunRootOwner(t, RunRootOwnerUID)
	if err := AssertAgentRunRoot(root); err != nil {
		t.Fatalf("AssertAgentRunRoot must PASS for 0700 root-owned, got: %v", err)
	}
}

// TestEnsureAgentRunRoot_RepairsResetMode pins the tmpfs-reboot discipline:
// a pre-existing, WRONG-mode root (what a rebooted tmpfs /run leaves) is
// repaired in situ by Ensure (no boot-time config required) — and a second
// Ensure on an already-correct root is a verified no-op, not a widened one.
func TestEnsureAgentRunRoot_RepairsResetMode(t *testing.T) {
	root := mkRunRoot(t)
	stubRunRootOwner(t, RunRootOwnerUID)

	// Simulate the rebooted-tmpfs state: the path exists at 0755.
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureAgentRunRoot(root); err != nil {
		t.Fatalf("EnsureAgentRunRoot must REPAIR a reset 0755 root in situ: %v", err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != RunRootMode {
		t.Fatalf("repaired root mode is %#o, required %#o", got, RunRootMode)
	}

	// Idempotent second call: no error, mode still exactly 0700.
	if err := EnsureAgentRunRoot(root); err != nil {
		t.Fatalf("second Ensure must be a verified no-op: %v", err)
	}
	if got, _ := os.Stat(root); got.Mode().Perm() != RunRootMode {
		t.Fatalf("mode drifted after second Ensure: %#o", got.Mode().Perm())
	}
}

// TestEnsureAgentRunRoot_FailsClosed_MissingPostStatDev pins the named refusal
// when the foreign-mount guard cannot even READ the stat device. The BEFORE
// read failing sets hadMount=false (nothing to compare); the AFTER read
// failing is what refuses: Ensure fails closed with
// ErrRunRootForeignMount-wrapped wording, never a silent pass.
func TestEnsureAgentRunRoot_FailsClosed_MissingPostStatDev(t *testing.T) {
	root := mkRunRoot(t)
	prevRunRootMount := statRunRootMountForTests
	defer func() { statRunRootMountForTests = prevRunRootMount }()

	var calls int
	statRunRootMountForTests = func(string) (uint64, bool) {
		calls++
		return 0, false // every device read fails (before AND after)
	}
	err := EnsureAgentRunRoot(root)
	if err == nil {
		t.Fatal("Ensure with unreadable stat devices must FAIL")
	}
	if !strings.Contains(err.Error(), ErrRunRootForeignMount.Error()) {
		t.Fatalf("refusal must carry the NAMED ErrRunRootForeignMount, got: %v", err)
	}
	if calls < 1 {
		t.Fatalf("expected at least one device read, got %d", calls)
	}
}

// TestEnsureAgentRunRoot_ForeignMountRefusal pins the NAMED refusal when the
// stat device CHANGES across the repair — the "foreign filesystem mount owns
// the path" case from the brief. Before: device 11; after: device 12 —
// the repair landed on a filesystem we do not own, so the spawn must refuse
// rather than create agent state underneath.
func TestEnsureAgentRunRoot_ForeignMountRefusal(t *testing.T) {
	root := mkRunRoot(t)
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	prevRunRootMount := statRunRootMountForTests
	defer func() { statRunRootMountForTests = prevRunRootMount }()

	dev := uint64(11)
	statRunRootMountForTests = func(string) (uint64, bool) {
		defer func() { dev = 12 }()
		return dev, true
	}

	err := EnsureAgentRunRoot(root)
	if err == nil {
		t.Fatal("Ensure with a device flip (foreign mount swap) must REFUSE")
	}
	if !strings.Contains(err.Error(), ErrRunRootForeignMount.Error()) {
		t.Fatalf("refusal must carry the NAMED ErrRunRootForeignMount, got: %v", err)
	}
	if !strings.Contains(err.Error(), "11") || !strings.Contains(err.Error(), "12") {
		t.Fatalf("refusal must name both devices (before → after), got: %v", err)
	}

	// And a STABLE device (the normal world) passes clean.
	dev = 42
	stable := func(string) (uint64, bool) {
		defer func() { dev = 42 }()
		return dev, true
	}
	statRunRootMountForTests = stable
	stubRunRootOwner(t, RunRootOwnerUID)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureAgentRunRoot(root); err != nil {
		t.Fatalf("Ensure with a stable device on an already-correct root must PASS, got: %v", err)
	}
}

// TestSpawnRunRootEnforcementWired pins the WIRING (the byte-level contract,
// same shape as TestSpawnSocketDirContractByteLevel): the spawn path calls
// EnsureAgentRunRoot BEFORE the per-agent MkdirAll sites and runs the
// read-back assert, so the parent cannot be created implicitly at 0755
// between spawns.
func TestSpawnRunRootEnforcementWired(t *testing.T) {
	code, err := os.ReadFile("manager_spawn.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(code)
	if !strings.Contains(src, "EnsureAgentRunRoot(spawnRunRoot)") {
		t.Fatal("manager_spawn.go does not ENFORCE the runtime root before creating agent dirs (ISO-002)")
	}
	if !strings.Contains(src, "AssertAgentRunRoot(spawnRunRoot)") {
		t.Fatal("manager_spawn.go does not ASSERT the runtime root's mode/ownership back after enforcement")
	}
	// The enforcement must sit BEFORE the first per-agent MkdirAll under
	// spawnRunRoot within the rootless-install stage (the socket-dir setup
	// below it) — the ISO-002 fix is an ordering property, not just the
	// presence of a call.
	ensureIdx := strings.Index(src, "EnsureAgentRunRoot(spawnRunRoot)")
	sockensureIdx := strings.Index(src, "EnsureAgentSocketDir(ctx, username, sockDir)")
	if ensureIdx < 0 || sockensureIdx < 0 || ensureIdx > sockensureIdx {
		t.Fatalf("spawn ordering broken: EnsureAgentRunRoot must precede the per-agent socket-dir setup (ensure@%d, sock@%d)", ensureIdx, sockensureIdx)
	}
}
