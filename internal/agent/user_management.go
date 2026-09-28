package agent

import "os/exec"

// userManagementCommand resolves account-management utilities even when the
// daemon's restricted service PATH omits /usr/sbin. Tests and installations
// that provide a PATH-local helper retain that helper; the absolute fallback
// is used only when normal lookup cannot find the utility.
func userManagementCommand(name string) string {
	if _, err := exec.LookPath(name); err == nil {
		// Keep the logical command name when PATH resolves it. Besides
		// preserving normal subprocess semantics, this keeps existing
		// command seams and PATH-based test stubs intact.
		return name
	}
	switch name {
	case "useradd", "userdel":
		return "/usr/sbin/" + name
	default:
		return name
	}
}
