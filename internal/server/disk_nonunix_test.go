//go:build !unix

package server

import "testing"

// TestReadDiskStatsRefusesOnUnsupportedPlatform pins the portability fallback:
// the disk figures must be reported as UNAVAILABLE (an error) rather than as a
// zero reading, so the >90% spawn warning cannot fire on fabricated numbers and
// the status response leaves the disk fields unset. Cross-compiled and
// type-checked by `GOOS=windows go vet ./...` rather than run on the Linux CI
// host.
func TestReadDiskStatsRefusesOnUnsupportedPlatform(t *testing.T) {
	used, total, err := readDiskStats()
	if err == nil {
		t.Fatalf("readDiskStats() on an unsupported platform = (%d, %d, nil), want a non-nil error", used, total)
	}
	if used != 0 || total != 0 {
		t.Errorf("readDiskStats() = (%d, %d), want zeros alongside the error", used, total)
	}
}
