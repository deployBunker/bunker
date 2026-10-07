// Package mountdriver: the rclone SFTP driver registration (MOUNT-007).
//
// rclone is OPT-IN and is NOT the default: DefaultDriver stays "sshfs"
// (the MOUNT-B2 recorded ruling). Registering it here is what lets a caller
// ASK for it by name (`--mount-driver rclone` at spawn, honoured by
// `bunker mount` from the stamped MountSpec) without changing what a request
// that names no driver gets. rclone speaks SFTP to the sshd/sftp-server the
// agent already runs, so the agent needs NOTHING installed — only the client
// needs the rclone binary.
//
// This driver deliberately declares NO failure classifier. The registry
// invariant is that every driver either carries a classifier or declares its
// absence with a named reason; the sshfs and bunker-fs classifiers match
// output fragments from their own binaries, and an rclone classifier would
// have to read rclone's VFS/retry vocabulary (shaped by rclone's own
// --retries/--low-level-retries), which has not been ported. Declaring
// absence keeps the client mount path fail-fast on the first error — never
// a silent inheritance of the sshfs-shaped retry loop — and the registry
// invariant test asserts the declaration is present and non-empty.
package mountdriver

// DriverRclone is the driver identity carried in proto MountSpec.driver for
// an rclone SFTP mount.
const DriverRclone = "rclone"

func init() {
	Register(Driver{
		Name: DriverRclone,
		NoClassifier: &NoClassifierReason{
			Why: "rclone-mount surfaces VFS/retry diagnostics in its own vocabulary (its --retries/--low-level-retries shape the output), so the sshfs fragment set would misclassify it and no rclone classifier has been ported; declaring absence keeps the mount fail-fast on the first error instead of silently inheriting sshfs-shaped retry logic",
		},
	})
}
