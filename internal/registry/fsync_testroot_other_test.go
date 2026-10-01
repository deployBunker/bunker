//go:build !unix

package registry

// isTmpfs has no portable answer off unix; the fsync-heavy tests fall back
// to t.TempDir (see fsync_regression_test.go).
func isTmpfs(dir string) (bool, error) {
	return false, nil
}
