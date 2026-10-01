//go:build unix

package registry

import (
	"syscall"
)

// tmpfsMagic is the statfs filesystem-type magic for tmpfs. It is the same
// constant on Linux and the BSDs; on platforms where it differs the check
// simply returns false and the tests fall back to t.TempDir, which is
// correct (just not relocated).
const tmpfsMagic = 0x01021994

// isTmpfs reports whether dir is backed by tmpfs (QA-BUNKER-43 test root
// relocation). See fsync_regression_test.go for why the fsync-heavy tests
// want a RAM-backed root.
func isTmpfs(dir string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false, err
	}
	return st.Type == tmpfsMagic, nil
}
