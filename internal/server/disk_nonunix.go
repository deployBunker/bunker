//go:build !unix

package server

import "errors"

// errDiskStatsUnsupported is returned where statfs(2) does not exist. Windows
// is the motivating case: it exposes no Statfs_t, and the free-space API there
// reports per-volume data rather than the root filesystem this call describes.
//
// Returning an error (not zeros) keeps both callers honest: they gate on
// err == nil, so the disk fields stay unset and the >90% spawn warning is
// skipped instead of firing on a fabricated 0/0 reading. bunkerd targets Linux,
// so this build is a portability fallback only.
var errDiskStatsUnsupported = errors.New("disk stats are not available on this platform")

func readDiskStats() (used, total uint64, err error) {
	return 0, 0, errDiskStatsUnsupported
}
