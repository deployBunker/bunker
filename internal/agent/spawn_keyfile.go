package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// INT-CI-042: the spawn keyfile lives at a DETERMINISTIC path
// (/tmp/bunker-key-<agentID>). A previous spawn of the same agent id that
// was killed before its rollback could run — the CI job/step timeout
// SIGKILLs bunkerd; run 37654405376's 15-minute step timeout did exactly
// that mid-spawn — leaves the private key (and .pub) behind. The next
// spawn's ssh-keygen then sees the occupied path:
//   - run 37666760535: the prompt "Overwrite (y/n)?" on the daemon's closed
//     stdin exits nonzero and the spawn dies at stage keygen in <1s;
//   - run 37654405376: the daemon stalls pre-registration, burns the
//     caller's whole 300s RPC budget, and the QA-BUNKER-58 re-list probe
//     reports "NOT found after 5 attempts".

// keyFilePath is the single source of the deterministic spawn keyfile path.
func keyFilePath(agentID string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("bunker-key-%s", agentID))
}

// preClearSpawnKeyFiles removes a STALE pair at the deterministic path
// before keygen runs. Blast radius is exactly the two files of THIS agent's
// pair — every other path, including another agent's keyfile, is untouched.
// keygen itself recreates a fresh pair; the spawn's existing rollback and
// deferred cleanup keep owning the files this spawn creates.
func preClearSpawnKeyFiles(agentID string) error {
	keyFile := keyFilePath(agentID)
	for _, path := range []string{keyFile, keyFile + ".pub"} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear stale spawn keyfile %s: %w", path, err)
		}
	}
	return nil
}

// newKeygenCmd builds the spawn keygen command (Step 3 of manager_spawn):
// ed25519, no passphrase, comment bunker-<agentID>, writing to the
// deterministic path. Shared by the spawn path and the INT-CI-042 tests so
// the regression test exercises the exact argv the daemon runs.
func newKeygenCmd(ctx context.Context, agentID string) *exec.Cmd {
	return exec.CommandContext(ctx, "ssh-keygen",
		"-t", "ed25519",
		"-f", keyFilePath(agentID),
		"-N", "",
		"-C", "bunker-"+agentID,
	)
}
