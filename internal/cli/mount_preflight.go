package cli

// Mount durability and mountpoint policy helpers (GAP-103, GAP-107).
//
// These exist so the mount is a claim that stays TRUE rather than a one-time
// event: a user-writable mountpoint a non-root operator can actually use, a
// durability option set that is asserted present, and a preflight that refuses
// to mount something that is not there.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Durability option values. Kept as constants so the tests assert the exact
// contract rather than a copy of today's numbers.
const (
	// sshfsServerAliveInterval/Count give a dead peer a bound: with 15s × 3
	// an unreachable host is noticed in ~45s instead of hanging forever.
	sshfsServerAliveInterval = "15"
	sshfsServerAliveCountMax = "3"
	// sshfsConnectTimeout bounds the initial connect, matching cp/ssh/deploy.
	sshfsConnectTimeout = "10"
	// sshfsPreflightTimeout bounds the remote path check before mounting.
	sshfsPreflightTimeout = 20 * time.Second
)

// remotePathCheck is the seam the mount preflight runs through. It is a
// package var so tests can stub it exactly like sshfsRun, because the preflight
// shells out to ssh and no unit test can reach a real host.
var remotePathCheck = remotePathExists

// lastRemoteSourcePath returns the source path embedded in a stored sshfs
// command — the "<host>:<path>" argument. Used to default the preflight's
// remote path when the operator passes a mountpoint but not --path.
func lastRemoteSourcePath(mountCmd string) string {
	parts := strings.Fields(mountCmd)
	if len(parts) < 2 {
		return ""
	}
	src := parts[len(parts)-2] // the argument before the mountpoint
	if _, path, ok := strings.Cut(src, ":"); ok {
		return path
	}
	return ""
}

// durableSSHFSArgs returns the option set every mount must carry. It is a
// function rather than an inline literal so the test can assert the real
// contract instead of a copy of today's numbers.
//
// Each option closes a named disconnect failure mode:
//
//	reconnect                  a transport blip must not be permanent
//	ServerAliveInterval/Count  a dead peer must be noticed in ~45s, not hang
//	ConnectTimeout             the initial connect must be bounded
//	auto_unmount               a crash must not strand the mountpoint
//	dir_cache=no               a dead mount must not serve stale entries as if
//	                           the tree were live (the phantom-space failure)
func durableSSHFSArgs() []string {
	return []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "reconnect",
		"-o", "ServerAliveInterval=" + sshfsServerAliveInterval,
		"-o", "ServerAliveCountMax=" + sshfsServerAliveCountMax,
		"-o", "ConnectTimeout=" + sshfsConnectTimeout,
		"-o", "auto_unmount",
		"-o", "dir_cache=no",
	}
}

// safeDefaultMountPoint is the path policy used for help text; the real
// resolution lives in defaultMountPoint.
func safeDefaultMountPoint(agentID string) string {
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		return filepath.Join(run, "bunker", "mnt", agentID)
	}
	return filepath.Join("~", ".bunker", "mnt", agentID)
}

// defaultMountPoint resolves the mountpoint used when the operator does not
// pass one. The previous default was /mnt/bunker/<agent-id>, which is
// root-owned on a default host, so MkdirAll failed for every non-root user and
// the documented default was unusable (DF-BUNKER-14 residual).
//
// Preference order, each validated as writable before it is returned:
//  1. $BUNKER_MOUNT_ROOT (explicit operator override)
//  2. $XDG_RUNTIME_DIR/bunker/mnt/<agent-id> (per-user, per-boot, private)
//  3. ~/.bunker/mnt/<agent-id> (per-user, persistent)
//
// A root-owned /mnt path is never chosen: an operator who wants it can pass it
// explicitly, and then the error names the cause instead of failing obscurely.
func defaultMountPoint(agentID string) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("mount: agent id is required")
	}

	var candidates []string
	if root := os.Getenv("BUNKER_MOUNT_ROOT"); root != "" {
		candidates = append(candidates, filepath.Join(root, agentID))
	}
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		candidates = append(candidates, filepath.Join(run, "bunker", "mnt", agentID))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".bunker", "mnt", agentID))
	}
	return firstWritableMountPoint(candidates)
}

// defaultMountPointForServer is defaultMountPoint with the SERVER as the first
// path component: <root>/<server>/<agent-id>.
//
// Why the server dimension exists (GAP-113): agent ids are unique per server,
// not globally. Two bunkers that each have an agent named `dev` would both
// resolve to <root>/dev and collide — the second mount finds a live mountpoint
// belonging to the other server and fails, or worse, an operator reads the
// wrong tree believing it is the one they mounted. Namespacing by server is
// what makes "deploy any number of bunkers" scale past the first name clash.
//
// Agent ids are also sanitized into a single path component: an id containing
// a slash or a ".." must never be able to escape the mount root.
func defaultMountPointForServer(server, agentID string) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("mount: agent id is required")
	}
	agent := sanitizeMountComponent(agentID)
	if agent == "" {
		return "", fmt.Errorf("mount: agent id %q has no usable characters for a path component", agentID)
	}
	srv := sanitizeMountComponent(server)
	if srv == "" {
		return "", fmt.Errorf("mount: server %q has no usable characters for a path component", server)
	}

	var candidates []string
	if root := os.Getenv("BUNKER_MOUNT_ROOT"); root != "" {
		candidates = append(candidates, filepath.Join(root, srv, agent))
	}
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		candidates = append(candidates, filepath.Join(run, "bunker", "mnt", srv, agent))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".bunker", "mnt", srv, agent))
	}
	return firstWritableMountPoint(candidates)
}

// sanitizeMountComponent reduces a server or agent id to a safe single path
// component: path separators, traversal markers and control characters are
// removed rather than escaped, so the result can never climb out of the mount
// root. Ids that are already clean (the common case) are returned unchanged.
func sanitizeMountComponent(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, string(filepath.Separator), "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	s = strings.ReplaceAll(s, "..", "-")
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Trim(b.String(), "-. ")
	// A single "." is not a usable component either.
	if out == "." {
		return ""
	}
	return out
}

// firstWritableMountPoint creates and validates the first candidate that can
// actually be used, naming the failure of each rejected candidate if none can.
func firstWritableMountPoint(candidates []string) (string, error) {
	var lastErr error
	for _, dir := range candidates {
		// Create the mountpoint itself, private to this user. 0700 (not 0755)
		// is deliberate: only this user can traverse it, which is what keeps a
		// mount without allow_other private.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			lastErr = fmt.Errorf("create mount point %s: %w", dir, err)
			continue
		}
		if err := checkWritableDir(dir); err != nil {
			lastErr = err
			continue
		}
		return dir, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable per-user mount root; set BUNKER_MOUNT_ROOT")
	}
	return "", fmt.Errorf("mount: no writable mountpoint available (%w). Set BUNKER_MOUNT_ROOT to a directory you own, or pass an explicit mountpoint", lastErr)
}

// checkWritableDir verifies the directory is writable by actually creating and
// removing a probe file. os.Stat-based checks lie across NFS/FUSE boundaries.
func checkWritableDir(dir string) error {
	probe := filepath.Join(dir, ".bunker-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("mount point %s is not writable by this user: %w", dir, err)
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return nil
}

// stripAllowOption removes any of the "allow" family (allow_other, allow_root,
// ...) from an sshfs argument list, returning the cleaned list and the option
// it dropped.
//
// It exists because the daemon's generated command always requests allow_other
// (manager_spawn.go:583) while this CLI's mountpoint is private 0700. Passing
// it through would expose the agent's files to every local user for no benefit;
// refusing outright would break every real mount. Stripping is the correct
// middle: the mount still works, and the dropped option is reported.
func stripAllowOption(args []string) ([]string, string, bool) {
	out := make([]string, 0, len(args))
	var dropped string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-o" && i+1 < len(args) {
			kept, hit := filterAllowParts(args[i+1])
			if hit != "" {
				dropped = hit
			}
			if kept == "" {
				i++ // drop the now-empty -o <value> pair entirely
				continue
			}
			out = append(out, a, kept)
			i++
			continue
		}
		// Handle the joined "-oallow_other" / "-o allow_other,idmap=user" shapes.
		if kept, hit := filterAllowParts(a); hit != "" {
			dropped = hit
			if kept == "" {
				continue
			}
			out = append(out, kept)
			continue
		}
		out = append(out, a)
	}
	if dropped == "" {
		return args, "", false
	}
	return out, dropped, true
}

// filterAllowParts removes allow-family entries from a comma-separated option
// value, returning the remaining value and the first dropped option name.
func filterAllowParts(value string) (string, string) {
	parts := strings.Split(value, ",")
	var kept []string
	var dropped string
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if strings.HasPrefix(t, "allow_other") || strings.HasPrefix(t, "allow_root") {
			if dropped == "" {
				dropped = t
			}
			continue
		}
		kept = append(kept, p)
	}
	if dropped == "" {
		return value, ""
	}
	return strings.Join(kept, ","), dropped
}

// unsafeAllowOption reports whether an sshfs argument list requests any of the
// "allow" family (allow_other, allow_root, ...). Those widen access to the
// agent's files to other local users and cannot be un-done once the mount is
// up, so the mount refuses instead of silently producing a world-readable
// mountpoint.
func unsafeAllowOption(args []string) (string, bool) {
	for i, a := range args {
		v := a
		// Handle both "-o allow_other" and "-oallow_other" shapes.
		if a == "-o" && i+1 < len(args) {
			v = args[i+1]
		}
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "allow_other") ||
				strings.HasPrefix(part, "allow_root") ||
				part == "allow_other" {
				return part, true
			}
		}
	}
	return "", false
}

// Workspace identity returned by the preflight. The mount records this in the
// session binding so the operator can see WHICH tree was mounted, and so a
// later mismatch (agent re-created, path replaced) is detectable rather than
// silently served.
type WorkspaceIdentity struct {
	// RemotePath is the path that was resolved.
	RemotePath string
	// GitRemote is the workspace's origin URL, empty when it is not a repo.
	GitRemote string
	// GitHead is the resolved commit, empty when not a repo.
	GitHead string
	// GitBranch is the current branch, or "detached" / empty.
	GitBranch string
	// IsRepo reports whether the path is a git work tree at all.
	IsRepo bool
}

// Describe renders the identity for an operator-facing message. It never
// claims a repo when the path is not one: an empty tree says so.
func (w WorkspaceIdentity) Describe() string {
	if !w.IsRepo {
		return fmt.Sprintf("%s (not a git work tree)", w.RemotePath)
	}
	head := w.GitHead
	if len(head) > 8 {
		head = head[:8]
	}
	remote := w.GitRemote
	if remote == "" {
		remote = "(no origin)"
	}
	return fmt.Sprintf("%s [%s @ %s]", remote, w.GitBranch, head)
}

// MatchesExpected reports whether this workspace is the one the operator said
// they expected. An empty `expect` means "no expectation" and always matches --
// refusals must be for a stated expectation, never for the absence of one.
func (w WorkspaceIdentity) MatchesExpected(expect string) bool {
	if expect == "" {
		return true
	}
	if w.GitRemote == "" {
		return false
	}
	// Accept either the full URL or the "owner/repo" suffix so an operator can
	// pass the short form they actually think in.
	if w.GitRemote == expect {
		return true
	}
	trimmed := strings.TrimSuffix(strings.TrimSuffix(w.GitRemote, ".git"), "/")
	return strings.HasSuffix(trimmed, strings.TrimSuffix(expect, ".git"))
}

// remotePathExists checks the remote path exists and is a directory, using the
// same ssh identity/host the mount will use, and returns the workspace's
// identity. This is the preflight that kills the silent-empty-tree failure:
// without it, mounting an agent whose home is empty (a re-created agent) or a
// --path that does not exist SUCCEEDS and presents an empty tree that looks
// mounted and correct.
//
// It deliberately resolves the identity in the SAME round trip as the existence
// check: a second ssh would be a second chance to disagree.
func remotePathExists(userAtHost, keyPath, remotePath string) (WorkspaceIdentity, error) {
	ident := WorkspaceIdentity{RemotePath: remotePath}
	if userAtHost == "" {
		return ident, fmt.Errorf("mount preflight: no ssh target resolved")
	}
	if remotePath == "" {
		return ident, fmt.Errorf("mount preflight: no remote path to check")
	}
	if keyPath == "" {
		return ident, fmt.Errorf("mount preflight: no ssh key resolved")
	}

	// One round trip: confirm the directory, then report the git identity if
	// there is one. `-C` keeps it safe on a non-repo (git exits non-zero and
	// prints nothing, which the parser treats as "not a repo").
	script := `set -e
p=` + shellQuote(remotePath) + `
[ -d "$p" ] || { echo "__BUNKER_NO_DIR__"; exit 3; }
echo "__BUNKER_DIR__"
echo "remote="$(git -C "$p" remote get-url origin 2>/dev/null || true)
echo "head="$(git -C "$p" rev-parse HEAD 2>/dev/null || true)
echo "branch="$(git -C "$p" rev-parse --abbrev-ref HEAD 2>/dev/null || true)
`
	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "ConnectTimeout="+sshfsConnectTimeout,
		"-i", keyPath,
		userAtHost,
		"sh", "-s",
	)
	cmd.Stdin = strings.NewReader(script)
	out, err := runWithTimeout(cmd, sshfsPreflightTimeout)
	text := string(out)
	if err != nil {
		// Distinguish "does not exist / not a directory" from "could not reach
		// the host at all": they call for different operator action.
		if strings.Contains(text, "__BUNKER_NO_DIR__") {
			return ident, fmt.Errorf("mount preflight: remote path %q does not exist or is not a directory on %s", remotePath, userAtHost)
		}
		details := strings.TrimSpace(text)
		if details == "" {
			return ident, fmt.Errorf("mount preflight: remote path %q does not exist or is not a directory on %s", remotePath, userAtHost)
		}
		return ident, fmt.Errorf("mount preflight: cannot reach %s to verify %q: %s", userAtHost, remotePath, details)
	}
	if !strings.Contains(text, "__BUNKER_DIR__") {
		return ident, fmt.Errorf("mount preflight: remote path %q does not exist or is not a directory on %s", remotePath, userAtHost)
	}

	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "remote="):
			ident.GitRemote = strings.TrimPrefix(line, "remote=")
		case strings.HasPrefix(line, "head="):
			ident.GitHead = strings.TrimPrefix(line, "head=")
		case strings.HasPrefix(line, "branch="):
			ident.GitBranch = strings.TrimPrefix(line, "branch=")
		}
	}
	ident.IsRepo = ident.GitHead != ""
	return ident, nil
}

// shellQuote single-quotes a value for a POSIX shell, so a path with a space or
// metacharacter cannot change the command shape.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runWithTimeout runs a command with a hard deadline, returning combined output.
func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) (string, error) {
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() {
		out, err := cmd.CombinedOutput()
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return string(r.out), r.err
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("timed out after %s", timeout)
	}
}
