//go:build !unix

package agent

import (
	"fmt"
	"os"
)

// lockSubIDFileAttempt creates the lock file but provides no cross-process
// exclusion: flock(2) does not exist on this platform, so the nonBlocking flag
// has nothing to reject (the attempt always succeeds). bunkerd targets Linux,
// so this is a portability fallback only — the subordinate-ID databases
// (/etc/subuid, /etc/subgid) and their user-namespace semantics are Linux-only
// as well.
func lockSubIDFileAttempt(path string, nonBlocking bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}
