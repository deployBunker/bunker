// Package resource: the MOUNT-006 mount-driver identity helper.
//
// newMountSpecForSummary derives the proto MountSpec an agent record
// surfaces on GetAgentInfo / ListAgents. A pre-seam record (empty
// MountDriver) still reports the sshfs default so the identity is explicit
// for every record; a record whose driver is unknown to the registry keeps
// the recorded name verbatim — reporting a lie ("sshfs") would be worse than
// reporting an unknown truth, and the CLI refuses unknown drivers by name.
package resource

import (
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// DefaultMountDriver is the seam's default driver name. Duplicated as a
// literal (not imported) to keep the resource package free of a mountdriver
// dependency: the value is pinned by tracker_test to equal the seam's.
const DefaultMountDriver = "sshfs"

// newMountSpecForSummary builds the MountSpec for an agent summary from the
// record's driver name and stored mount command.
func newMountSpecForSummary(driver, command string) *v1.MountSpec {
	if command == "" {
		return nil
	}
	if driver == "" {
		driver = DefaultMountDriver
	}
	return &v1.MountSpec{Driver: driver, Command: command}
}
