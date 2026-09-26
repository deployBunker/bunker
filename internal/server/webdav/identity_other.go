//go:build !unix

package webdav

// rootIdentityParts has no device/inode pair to report where the Stat_t shape
// does not exist (Windows). The tree token falls back to the served path
// alone, which still separates two different trees; it does not distinguish a
// re-created directory at the same path on those platforms. bunkerd targets
// Linux, so this build is a portability fallback only.
func rootIdentityParts(string) (dev uint64, ino uint64) { return 0, 0 }
