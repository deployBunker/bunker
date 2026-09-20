package cli

// The do-not-build guard (GAP-105).
//
// A local build inside an SSHFS mount is the one thing the mount is genuinely
// bad at: the toolchain reads thousands of small files over SFTP, which wastes
// exactly the local CPU this design exists to save and can run 10-100x slower
// than a local disk. Worse, it fails SOFTLY -- a `go build ./...` in a mounted
// tree eventually finishes (or times out) with no hint that it should have run
// remotely. The guard makes the mistake impossible rather than merely
// discouraged: mount writes a marker file at the mountpoint root, and the
// build-entry wrapper detects it and refuses loudly, naming the remote path.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MountMarkerName is the marker file mount writes at the mountpoint root.
// Hidden by default but not dot-invisible to a deliberate ls; umount removes
// it with the mountpoint.
const MountMarkerName = ".bunker-mount"

// BuildToolsThatMustRunRemote are the commands the guard intercepts. Kept as a
// slice of full names rather than a map so the error message can list them in a
// stable order.
var BuildToolsThatMustRunRemote = []string{"go", "make", "cargo", "npm", "pnpm", "yarn", "gradle", "mvn"}

// WriteMountMarker writes the marker file into a fresh mountpoint. It is best
// effort: a filesystem that refuses the write (read-only root, quota) still
// mounts, because the mount itself is not wrong -- only the guard's visibility
// is lost. The caller reports that trade-off rather than failing the mount.
func WriteMountMarker(mountPoint string) (bool, error) {
	path := filepath.Join(mountPoint, MountMarkerName)
	content := "This directory is an SSHFS mount of a bunker agent workspace.\n" +
		"Local builds here are slow and waste the local machine: run them remotely instead\n" +
		"(bunker exec <agent-id> -- go build ./...), or see 'bunker build --help'.\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// ReadMountMarker reports whether dir (or any parent up to the filesystem root)
// carries the mount marker. Walking upward matters because a build may be
// invoked from a subdirectory of the mount; stopping at the filesystem root
// keeps it from reading a marker ABOVE the mountpoint (a sibling mount, or the
// operator's home) as if it applied here.
func ReadMountMarker(dir string) (string, bool) {
	dir = filepath.Clean(dir)
	for {
		path := filepath.Join(dir, MountMarkerName)
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// GuardRefusal is the error a build inside a mount produces. It names the
// remote path to use instead, because a refusal that does not say what to do
// just teaches the operator to work around it.
type GuardRefusal struct {
	Tool       string
	MarkerPath string
	RemoteHint string
}

func (g *GuardRefusal) Error() string {
	return fmt.Sprintf(
		"refusing: %s is running inside a bunker SSHFS mount (%s).\n"+
			"A local build over SFTP is pathologically slow and wastes the local machine.\n"+
			"Run it remotely instead: bunker exec <agent-id> -- %s ...\n%s",
		g.Tool, g.MarkerPath, g.Tool, g.RemoteHint)
}

// CheckBuildGuard returns a *GuardRefusal when the working directory sits
// inside a mounted tree and tool is one of the intercepted builders. A nil
// return means "not a mount, or not an intercepted tool" -- the caller proceeds
// exactly as before, so the guard changes nothing outside the mount.
func CheckBuildGuard(tool, workDir string) error {
	if !isInterceptedTool(tool) {
		return nil
	}
	markerPath, found := ReadMountMarker(workDir)
	if !found {
		return nil
	}
	return &GuardRefusal{
		Tool:       tool,
		MarkerPath: markerPath,
		RemoteHint: remoteHintFor(tool),
	}
}

func isInterceptedTool(tool string) bool {
	base := filepath.Base(strings.TrimSpace(tool))
	for _, t := range BuildToolsThatMustRunRemote {
		if base == t {
			return true
		}
	}
	return false
}

// remoteHintFor names the specific remote form for the intercepted tool, so the
// refusal is actionable per tool rather than generically.
func remoteHintFor(tool string) string {
	switch filepath.Base(strings.TrimSpace(tool)) {
	case "go":
		return "(or: bunker build <agent-id> -- ./...)"
	case "make":
		return "(or: bunker exec <agent-id> -- make <target>)"
	default:
		return "(run the equivalent command on the agent via bunker exec)"
	}
}
