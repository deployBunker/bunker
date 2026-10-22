// The procvis mode's host verifier (NET-BUNKER-011;
// specs/network-isolation.md §1.8 and §5.2 are the design authority).
//
// procvis puts the agent's systemd units behind ProtectProc=invisible, which
// is systemd's per-unit form of hidepid=2: it needs mount namespaces AND a
// kernel whose procfs honors the hidepid mount option. Neither is assumable —
// a kernel without per-mount hidepid SILENTLY ignores the option (systemd
// documents exactly that failure mode for ProtectProc/ProcSubset), and a
// silent no-op boundary is the manufactured confidence §5.2 exists to kill.
// So the daemon VERIFIES the kernel semantics on the real host BEFORE
// creating any unit state, the same read-back discipline GAP-075 applies to
// its /tmp boundary ("a mount option that exited 0 is not proof").
//
// The verifier's kernel experiment, per arm (all unprivileged — Constraint C:
// agents are unprivileged users, and the daemon must not need new privilege):
//
//   - in a private mount namespace (unshare -m, via setpriv so the child is
//     the wrapper's own argv), mount a FRESH procfs instance on a temp dir
//     with hidepid=2, then read /proc/mounts THROUGH that instance and
//     require the hidepid=2 option to be REPORTED BACK — the kernel's own
//     ack, and the exact "reported boundary" §5.2 demands;
//   - prove the mechanism bites (non-vacuity): spawn two processes under
//     different UIDs inside that namespace and require the observer to list
//     the foreign process WITHOUT hidepid and NOT list it WITH hidepid=2.
//
// One probe failure anywhere => the daemon cannot prove the boundary on this
// host and the mode is refused (never degraded to shared). The error text
// names what failed so an operator can fix the host or pick another mode.
//
// These probes are read-only side-effect-free experiments in a throwaway
// namespace; they touch no bunker state and impose nothing on the default
// path (shared never calls this).
package netmode

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// procvisProbeScript is the single-shell experiment the verifier runs. It
// exits 0 only when the kernel proved BOTH properties: hidepid=2 is honored
// and reported on a private procfs mount, and it actually hides a FOREIGN-UID
// process from a FOREIGN-UID observer's readdir while the control arm still
// lists it.
//
// The experiment runs INSIDE a private mount namespace where the shell is
// root (the real bunkerd is root; a userns-capable host reaches the same
// state via --map-root-user). Both actors are therefore placed with setpriv:
//
//   - the VICTIM runs as uid 1 (setpriv --reuid=1);
//   - the OBSERVER — the process doing the readdir — runs as uid 2.
//
// The observer must NOT be root: hidepid's whole question is cross-uid
// visibility, and a root observer can be exempt from hidepid (CAP_SYS_PTRACE
// passes ptrace_may_access for every task), which would make the hidden arm
// pass or fail for the wrong reason. uid 2 sees only its own processes under
// hidepid=2 — the exact semantics the agent's units will run under.
//
// Structure (set -e; every step must pass):
//
//  1. mount -t proc -o hidepid=2 proc $P  — the hidden arm.
//  2. ack: grep the options REPORTED BACK through the new instance ($P/self/
//     mounts — NOT /proc/mounts, which is the wrapper's namespace). THE ACK
//     ACCEPTS BOTH SPELLINGS — hidepid=2 AND hidepid=invisible — because
//     kernels differ in what they report: Ubuntu 6.8 (verified live on the
//     fleet host) normalizes the mount option to the canonical LEVEL NAME
//     `hidepid=invisible`. A verifier pinned to the documentation spelling
//     alone would falsely refuse working hosts; a live ack check exists
//     precisely to catch this class of doc-vs-kernel drift.
//  3. HIDDEN arm: the uid-2 observer lists $P — with hidepid=2 on the mount,
//     uid 1's process must be ABSENT. No ls/grep dependency for the match:
//     /proc readdir via the observer's own ls, pid-shaped lines only.
//  4. mount -t proc -o hidepid=0 proc $P — re-mount the SAME mountpoint
//     WITHOUT hiding (a fresh procfs instance over the old one — the
//     umount-free way to flip the option).
//  5. CONTROL arm: the uid-2 observer lists $P again — uid 1's process MUST
//     be present now, proving the hiding in step 3 was the hidepid option's
//     work and not a race, an artifact of the ns, or a missing process. A
//     control that cannot see the victim invalidates the experiment (exit
//     2), so a kernel that hides EVERYTHING cannot masquerade as a passing
//     one, and a dead victim cannot fake the hidden arm.
//
// Exit codes: 0 proven; 1 hidepid not honored/reported; 2 mechanism failure
// (victim missing in the control arm); 3 probe environment failure (no
// unshare/mount permission in this context).
const procvisProbeScript = `set -e
P="$1"
mount -t proc -o hidepid=2 proc "$P"
grep -Eq 'hidepid=(2|invisible)' "$P/self/mounts" || exit 1
setpriv --reuid=1 --regid=1 --clear-groups sleep 5 &
VICTIM=$!
sleep 1
HIDDEN=1
for d in $(setpriv --reuid=2 --regid=2 --clear-groups sh -c 'echo "$P" >/dev/null; ls -1 "$1" 2>/dev/null' ls "$P" | grep -E '^[0-9]+$'); do
  case "$d" in
    "$VICTIM") HIDDEN=0 ;;
  esac
done
[ "$HIDDEN" = 1 ] || exit 1
mount -t proc -o hidepid=0 proc "$P"
sleep 1
FOUND=0
for d in $(setpriv --reuid=2 --regid=2 --clear-groups sh -c 'ls -1 "$1" 2>/dev/null' ls "$P" | grep -E '^[0-9]+$'); do
  case "$d" in
    "$VICTIM") FOUND=1 ;;
  esac
done
[ "$FOUND" = 1 ] || exit 2
exit 0
`

// ProcVisProbeError is the named refusal the daemon surfaces when the host
// cannot prove the procvis kernel semantics (spec §5.2: refuse loudly, name
// the reason, never fall back to shared). It wraps the probe's own output so
// an operator sees exactly which arm failed.
type ProcVisProbeError struct {
	Detail string
}

func (e *ProcVisProbeError) Error() string {
	return "procvis mode unavailable on this host: " + e.Detail
}

// procvisProbeRunner executes the procvis experiment. A package-level var so
// the kernel-semantics tests can drive real and failure arms without touching
// process setup elsewhere.
var procvisProbeRunner = runProcVisProbe

// runProcVisProbe runs the kernel experiment on the REAL host. The
// experiment needs a private mount namespace whose shell is root (it must
// create a uid-1 victim and a uid-2 observer, and mount the procfs
// instances). The real bunkerd is root: a plain `unshare --mount` reaches
// that state. A non-root caller (dev builds, tests) reaches the equivalent
// state through a user namespace that maps root (--map-root-user) when the
// host permits it; when it does not, the classification reports the
// environment failure and the daemon refuses the mode rather than guessing.
// TempDir provides the mountpoint; it is never touched by anything else and
// removed after the namespace dies.
func runProcVisProbe() error {
	dir, err := os.MkdirTemp("", "bunker-procvis-probe")
	if err != nil {
		return &ProcVisProbeError{Detail: fmt.Sprintf("probe temp dir: %v", err)}
	}
	defer os.RemoveAll(dir)

	inner := []string{"sh", "-c", procvisProbeScript, "procvis-probe", dir}
	args := []string{}
	if os.Geteuid() != 0 {
		args = append(args, "--map-root-user")
	}
	args = append(args, "--mount", "--propagation", "private")
	args = append(args, inner...)
	cmd := exec.Command("unshare", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return classifyProcVisProbe(err, out)
	}
	return nil
}

// classifyProcVisProbe turns the experiment's exit into the named refusal.
// The shell's `exit N` mapping is the contract; wrapper-level failures
// (unshare refused, setpriv missing) are environment failures and say so.
func classifyProcVisProbe(err error, out []byte) error {
	detail := strings.TrimSpace(string(out))
	ee, ok := err.(*exec.ExitError)
	if !ok {
		return &ProcVisProbeError{Detail: fmt.Sprintf("probe could not run: %v (output: %s)", err, detail)}
	}
	switch ee.ExitCode() {
	case 1:
		return &ProcVisProbeError{Detail: fmt.Sprintf("kernel does not honor/report hidepid=2 on a private procfs mount (ProtectProc=invisible would be a silent no-op) (output: %s)", detail)}
	case 2:
		return &ProcVisProbeError{Detail: fmt.Sprintf("hidepid probe control arm failed: the foreign-uid process was not visible without hiding, so the experiment cannot prove anything (output: %s)", detail)}
	default:
		return &ProcVisProbeError{Detail: fmt.Sprintf("probe environment failed (mount namespace unavailable or probe error): %v (output: %s)", err, detail)}
	}
}

// VerifyProcVisHost proves the procvis mode's kernel semantics on this host,
// BEFORE any unit state is created. It runs the live experiment and returns
// a *ProcVisProbeError on any failure. The daemon calls this at spawn Step
// 1e for mode procvis — the same refuse-before-side-effects point the other
// validations live at.
func VerifyProcVisHost() error {
	return procvisProbeRunner()
}

// procMountsPath is the file VerifyProcVisRuntime reads. A package-level
// var so the hermetic tests and the agent package's gate wiring can point
// it at a fixture instead of the live /proc. NEVER set it to a relative
// path in production code — the daemon reads the REAL /proc/mounts.
var procMountsPath = "/proc/mounts"

// SetProcMountsPathForGate repoints the verifier's mounts file and returns
// the restore func. The agent package's gate tests use this to route the
// verification at a fixture; production code (the daemon) MUST NOT call
// it — the real gate reads /proc/mounts and nothing else. (Tests only;
// not concurrency-safe with a live verifier call in flight in tests.)
func SetProcMountsPathForGate(p string) func() {
	orig := procMountsPath
	procMountsPath = p
	return func() { procMountsPath = orig }
}

// ProcVisMountOptionsError is the named refusal for the REAL-/proc half of
// the ISO-001 gate (spec §5.2 refuse-loudly, never degrade to shared). It
// states both the missing property and the operator-visible consequence:
// the gate can no longer see what the agent's own units are doing, so the
// enforced boundary cannot be read back — the exact no-silent-no-op
// discipline the probe's ack implements for an ephemeral mount.
type ProcVisMountOptionsError struct {
	Detail string
}

func (e *ProcVisMountOptionsError) Error() string {
	return "procvis mode unavailable on this host: " + e.Detail
}

// VerifyProcVisRuntime is the REAL-/proc half of the ISO-001 gate. The
// kernel experiment (above) proves the CAPABILITY in a throwaway namespace;
// this helper proves the DEPLOYED STATE: the running system /proc that hosts
// every bunkerd unit carries the host-wide hiding (hidepid/invisible) and a
// proc group. Without them the proc gate would be blind to every foreign
// task, the units are in each other's view, and the mode would deliver less
// than it was asked for.
//
// The option spellings are the SAME pair the ephemeral ack accepts: kernels
// normalize the mount option to the canonical level name (Ubuntu 6.8 reports
// `hidepid=invisible`), so both spellings are accepted — the report shape, not
// the documentation spelling, is the contract.
func VerifyProcVisRuntime() error {
	data, err := os.ReadFile(procMountsPath)
	if err != nil {
		return &ProcVisMountOptionsError{Detail: fmt.Sprintf("read %s: %v", procMountsPath, err)}
	}
	line, ok := findProcMountLine(string(data))
	if !ok {
		return &ProcVisMountOptionsError{Detail: fmt.Sprintf("%s has no procfs line — cannot verify the real /proc the units' mounts are backed by", procMountsPath)}
	}
	// hidepid=0 means "no hiding": the default mount state. The kernel
	// normalizes that level to a NON-option line in this context, so the
	// hidepid token's ABSENCE is the un-hidden state, not a different spelling
	// (pinned by the option-parsing tests). Without the option the sibling
	// agents' processes are visible to each other through the real /proc —
	// the exact ISO-001 leak — and the spawn REFUSES with this named error:
	// per-unit ProtectProc=invisible is not the host-wide boundary, and
	// never degrading to shared means refusing here.
	if !strings.Contains(line, "hidepid=") {
		return &ProcVisMountOptionsError{Detail: "real /proc carries no hidepid option — sibling agents' processes are visible to each other (the per-unit ProtectProc=invisible mount is below the proc gate; the proc gate itself cannot be held)"}
	}
	if !strings.Contains(line, "procgroupid=") && !strings.Contains(line, "gid=") {
		return &ProcVisMountOptionsError{Detail: "real /proc carries no gid/procgroupid option — none of bunkerd's unprivileged unit readers can open the hidepid gate, so a procvis unit's verified view cannot be distinguished from a boundary-less read"}
	}
	return nil
}

// findProcMountLine returns the /etc-format line whose mountpoint (field 2)
// is exactly "/proc". Fields are whitespace-separated with \040 (and the
// octal family) as escaped whitespace per the fstab convention; splitting on
// plain spaces is the standard /proc/mounts read and is what the probe's ack
// grep does too. No line, no refusal — never a silent pass.
func findProcMountLine(mounts string) (string, bool) {
	for _, line := range strings.Split(mounts, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "/proc" {
			return line, true
		}
	}
	return "", false
}

// ProcVisProbeScript exposes the experiment text for reporting and for the
// hermetic tests (which execute THIS exact script — never a test-local copy).
func ProcVisProbeScript() string { return procvisProbeScript }
