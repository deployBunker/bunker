package agent

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ── DF-BUNKER-34: live-process probes over /proc ───────────────────────────
//
// Two failure shapes the field measured (cube-las-00, 2026-09) were invisible
// to every existing surface:
//
//  1. a destroy that ran userdel -rf while processes still held the home
//     (userdel failed on the busy directory, the user record was removed,
//     and ~35 uid-1002 processes kept running for 20+ hours — one of them
//     still bound port 3000 and shadowed the next agent's daemon);
//  2. an agent whose user record was long gone while its uid still owned
//     live processes — visible to `ps -eo pid,uid,cmd` but to no API.
//
// Both are answered from /proc/<pid>/status, the one plane that never needs
// root and never needs pgrep (whose -f pattern matching is a self-match and
// quoting hazard). The probes here are read-only and never kill anything.

// procStatusPath is the root of the per-process status tree the probes read
// ("/proc"). Var (not const) so a non-root regression can point it at a
// fixture tree; production code never swaps it.
var procStatusPath = "/proc"

// maxProcScan is the hard cap on the number of /proc entries one probe will
// classify. A host will never approach it; the cap bounds the cost of a
// pathological /proc (and keeps a fixture test honest about its size).
const maxProcScan = 4096

// procCmdlineHeadBytes bounds the command text one process contributes to a
// report or error message. The full cmdline is the evidence a caller may want,
// but an unbounded argv (an env dump in argv, a 100 KB --flag) must not be
// able to flood a log line or an RPC error. 256 bytes keeps pid + uid + a
// readable command head well inside one log line.
const procCmdlineHeadBytes = 256

// userProcess describes one live process owned by a uid.
type userProcess struct {
	// PID is the process id as /proc reports it.
	PID int
	// Cmd is the process's command line head (NUL-separated argv joined with
	// spaces), bounded to procCmdlineHeadBytes.
	Cmd string
}

// isAgentSessionProcess reports whether one live process is the agent uid's
// OWN systemd session manager or its PAM bookkeeping twin — the process pair
// every lingered agent legitimately owns (DF-BUNKER-56). Spawn enables
// linger, so systemd starts user@<uid>.service and the uid always carries
// "systemd --user" plus its "(sd-pam)" child; the dockerd/rootlesskit reap
// never touches them. These are lifecycle infrastructure, NOT orphanable
// operator processes: destroy terminates the manager itself
// (terminateAgentUserManager) and the destroy gate absorbs whatever pair
// processes survive the grace window. The orphan-uid report deliberately
// does NOT use this classifier: there the user record is already gone, so
// ANY live process under the uid is anomaly evidence.
//
// Shapes covered: the CI-measured "/usr/lib/systemd/systemd --user",
// Debian's "/lib/systemd/..." variant, a bare "systemd --user", and the
// (sd-pam) twin in its literal-argv, bracketed-Name and plain forms.
func isAgentSessionProcess(p userProcess) bool {
	cmd := strings.TrimSpace(p.Cmd)
	switch {
	case cmd == "systemd --user",
		strings.HasSuffix(cmd, " systemd --user"),
		strings.HasSuffix(cmd, "/systemd --user"):
		return true
	case cmd == "(sd-pam)",
		cmd == "[sd-pam]",
		cmd == "[(sd-pam)]",
		strings.HasSuffix(cmd, " (sd-pam)"):
		return true
	}
	return false
}

// listUserProcesses returns every live process owned by uid, read from
// /proc/<pid>/status (the Uid: line) — sorted by PID for stable reports.
// An unreadable entry is skipped, not failed: a process can exit between the
// directory read and the status read, and /proc/<pid> of an exiting process
// legitimately disappears. A /proc that cannot be listed at all is the one
// real failure, because "cannot look" must never read as "nothing there".
func listUserProcesses(uid uint32) ([]userProcess, error) {
	entries, err := os.ReadDir(procStatusPath)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", procStatusPath, err)
	}
	var out []userProcess
	scanned := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, perr := strconv.Atoi(e.Name()); perr != nil {
			continue // not a pid directory
		}
		scanned++
		if scanned > maxProcScan {
			break
		}
		status, rerr := os.ReadFile(filepath.Join(procStatusPath, e.Name(), "status"))
		if rerr != nil {
			continue // vanished mid-scan, or unreadable: not a failure
		}
		if !statusFileOwnedByUID(status, uid) {
			continue
		}
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil {
			continue
		}
		out = append(out, userProcess{PID: pid, Cmd: procCmdlineHead(e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// statusFileOwnedByUID reports whether a /proc/<pid>/status body's Uid line
// lists uid in any of its fields (real, effective, saved-set or fs). Any of
// the four means the kernel attributes the process to that user.
func statusFileOwnedByUID(status []byte, uid uint32) bool {
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
		for _, f := range fields {
			v, err := strconv.ParseUint(f, 10, 32)
			if err == nil && uint32(v) == uid {
				return true
			}
		}
		return false // the Uid line was there; its fields simply did not match
	}
	return false
}

// procCmdlineHead reads /proc/<pid>/cmdline and renders the command head.
// NUL-separated argv becomes spaces; a zombie or kernel thread with no
// cmdline falls back to the (bracketed) comm from the status body — the
// report must never render an empty command.
func procCmdlineHead(pid string) string {
	data, err := os.ReadFile(filepath.Join(procStatusPath, pid, "cmdline"))
	if err != nil || len(data) == 0 {
		if status, serr := os.ReadFile(filepath.Join(procStatusPath, pid, "status")); serr == nil {
			for _, line := range strings.Split(string(status), "\n") {
				if strings.HasPrefix(line, "Name:") {
					return "[" + strings.TrimSpace(strings.TrimPrefix(line, "Name:")) + "]"
				}
			}
		}
		return "(unknown)"
	}
	head := strings.ReplaceAll(string(data[:minLen(len(data), procCmdlineHeadBytes)]), "\x00", " ")
	head = strings.TrimSpace(head)
	if len(data) > procCmdlineHeadBytes {
		head += " …"
	}
	return head
}

// minLen returns the smaller of a and b (local helper so this file carries no
// dependency on the toolchain's min builtin availability contract).
func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// resolveUsernameUID resolves a system username to its uid through the
// package's user-database seam (lookupUser — the same lookup every in-repo
// consumer applies). A user that does not resolve reports (0, false).
func resolveUsernameUID(username string) (uint32, bool) {
	u, err := lookupUser(username)
	if err != nil || u == nil {
		return 0, false
	}
	v, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// orphanUIDCheck is the result of the orphan-uid probe for one agent: the
// state the residue planes cannot see — a user record and its processes
// disagreeing about the user's existence.
type orphanUIDCheck struct {
	// UID is the agent user's uid. It is resolved through the PERSISTED
	// metadata the agent itself carries (see probeOrphanUID), so it stays
	// valid even after the user record is gone — that is the whole point.
	UID uint32
	// UIDKnown reports whether the uid could be determined at all. A probe
	// with UIDKnown=false (no persisted metadata) is UNKNOWN, never orphan.
	UIDKnown bool
	// UserExists reports whether bunker-<id> still resolves in the user
	// database.
	UserExists bool
	// Processes are the live processes owned by the agent's uid. Empty when
	// the user exists (not an orphan case) or when nothing runs under the
	// uid.
	Processes []userProcess
	// ProbeErr names a probe that could not run (e.g. /proc unreadable). A
	// check with an error is UNKNOWN, never clean.
	ProbeErr string
}

// IsOrphan is the row's classification: the user record is GONE while
// processes still run under the uid. A user that exists, a uid that owns
// nothing, or an undetermined uid is never classified orphan — the check
// fails closed (unknown stays unknown instead of reading as clean).
func (c orphanUIDCheck) IsOrphan() bool {
	return c.UIDKnown && !c.UserExists && len(c.Processes) > 0
}

// Describe renders the operator-facing one-line summary: the uid, the process
// count and every process head. It is what lands in `bunker info` and the
// destroy refusal.
func (c orphanUIDCheck) Describe() string {
	if len(c.Processes) == 0 {
		if c.ProbeErr != "" {
			return "process probe unavailable: " + c.ProbeErr
		}
		return "no live processes under uid"
	}
	parts := make([]string, 0, len(c.Processes))
	for _, p := range c.Processes {
		parts = append(parts, fmt.Sprintf("pid %d: %s", p.PID, p.Cmd))
	}
	return fmt.Sprintf("%d live process(es) under uid %d: %s", len(c.Processes), c.UID, strings.Join(parts, "; "))
}

// checkOrphanUID probes one agent's orphan-uid state from persisted metadata:
// the uid comes from the agent's OWN durable record or its persisted
// home-side metadata — never from a live user lookup, which is exactly the
// record that may be gone. username is the system user the agent spawned as
// (bunker-<id>); it is resolved through the package's user-database seam for
// the UserExists arm.
//
// Result classes:
//   - user resolves and /proc shows nothing: healthy, not an orphan;
//   - user resolves but processes ARE running: not an orphan (the destroy
//     gate is the caller that must refuse on this state — orphan is the
//     "user gone" class);
//   - user gone + live uid processes: the orphan (row consequence (c)/(d));
//   - no readable uid source: UIDKnown=false, unknown, never clean.
func (m *AgentManager) checkOrphanUID(agentID string) orphanUIDCheck {
	username := agentUserPrefix + agentID
	_, uerr := lookupUser(username)
	check := orphanUIDCheck{UserExists: uerr == nil}

	// UID source 1: the user record still resolves — the healthy path.
	if check.UserExists {
		if uid, ok := resolveUsernameUID(username); ok {
			check.UID = uid
			check.UIDKnown = true
		}
	}

	// UID source 2 (the orphan path): the user is gone, so the uid must come
	// from durable state the user record is not needed for. The agent's home
	// (<agentHomeRoot>/<username>) carries its owner on disk — the uid the
	// spawn chowned everything to — and that ownership survives exactly as
	// long as the home does, which is the shape userdel-without-clean-home
	// leaves (row consequence (c): userdel -rf failed on the busy dir).
	if !check.UIDKnown {
		home := filepath.Join(agentHomeRoot, username)
		if st, serr := os.Stat(home); serr == nil {
			if uid, ok := statOwnerUID(st); ok {
				check.UID = uid
				check.UIDKnown = true
			}
		}
	}
	if !check.UIDKnown {
		check.ProbeErr = "uid unknown: user record gone and no readable persisted metadata at " +
			filepath.Join(agentHomeRoot, username)
		return check
	}
	procs, err := listUserProcesses(check.UID)
	if err != nil {
		check.ProbeErr = err.Error()
		return check
	}
	check.Processes = procs
	return check
}

// OrphanUIDSummary is the one-line orphan-uid verdict for one agent, as the
// info/list wire surface carries it (DF-BUNKER-34 criterion 4). Empty means
// healthy, unknown, or not applicable — the field is only non-empty for the
// orphan class itself (user record gone + live uid processes), so absence
// never fabricates a "healthy" verdict. The manager method is what the
// server wires into orphanUIDSummarizer.
func (m *AgentManager) OrphanUIDSummary(agentID string) string {
	if agentID == "" || !validAgentID.MatchString(agentID) {
		return ""
	}
	c := m.checkOrphanUID(agentID)
	if !c.IsOrphan() {
		return ""
	}
	return fmt.Sprintf("ORPHANED UID: user record bunker-%s is GONE from the host but %s — these processes survive every destroy and hold the home (and possibly ports) under a user that no longer exists",
		agentID, c.Describe())
}

// SwapProcStatusPath replaces the /proc root the probes read (test seam for
// out-of-package fixtures) and returns the previous value; restore it with
// SwapProcStatusPath(prev).
func SwapProcStatusPath(root string) string {
	prev := procStatusPath
	procStatusPath = root
	return prev
}

// SwapLookupUser replaces the package's user-lookup seam (test seam for
// out-of-package fixtures) and returns the previous value.
func SwapLookupUser(fn func(username string) (*user.User, error)) func(string) (*user.User, error) {
	prev := lookupUser
	lookupUser = fn
	return prev
}
