package agent

// INT-HOST-006: spawn's ensureRootlesskitAppArmor (rootless.go) writes
// /etc/apparmor.d/home.bunker-<id>.bin.rootlesskit and loads it with
// `apparmor_parser -r`, but no destroy path ever unloaded or removed it —
// every spawn+destroy cycle leaked one loaded profile plus one profile file
// (18,043 orphans measured on bunker-mvp vs ~120 baseline).
// RemoveAgentAppArmorProfile is the destroy-side inverse: best-effort unload,
// then the file removal that keeps /etc/apparmor.d from growing without
// bound. It runs on a budgeted destroy step (BNK-DF-001) like every other
// compensating exec.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// appArmorProfileDir is the directory ensureRootlesskitAppArmor installs
// profiles into. Package-level seam (the QA-BUNKER-61 pattern): tests point
// it at a temp dir so no real AppArmor state is touched; production never
// swaps it.
var appArmorProfileDir = "/etc/apparmor.d"

// appArmorParserRunner runs ONE apparmor_parser invocation. Package-level
// seam so tests can record the unload without root or a real parser.
var appArmorParserRunner = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "apparmor_parser", args...).CombinedOutput()
}

// agentAppArmorProfilePath mirrors the spawn-side path expression in
// ensureRootlesskitAppArmor exactly: profileName =
// fmt.Sprintf("home.%s.bin.rootlesskit", username), profilePath =
// filepath.Join("/etc/apparmor.d", profileName). The duplication is
// deliberate and byte-for-byte: a cleanup path that computed the name any
// differently (slash-replace, id-only, trimmed prefix) would itself leak
// every profile. username carries the bunker-<id> form, same as spawn.
func agentAppArmorProfilePath(username string) string {
	profileName := fmt.Sprintf("home.%s.bin.rootlesskit", username)
	return filepath.Join(appArmorProfileDir, profileName)
}

// RemoveAgentAppArmorProfile unloads and removes the rootlesskit AppArmor
// profile spawn installed for agentID. Context-free entry point for
// non-RPC callers (maintenance sweeps, tests); the destroy path uses
// removeAgentAppArmorProfileCtx with its own budgeted step (BNK-DF-001).
func RemoveAgentAppArmorProfile(agentID string) error {
	return removeAgentAppArmorProfileCtx(context.Background(), agentID)
}

// removeAgentAppArmorProfileCtx is RemoveAgentAppArmorProfile on a caller's
// context. Order and tolerance:
//
//  1. No profile file → nothing to clean (spawn skipped the install, e.g. a
//     host without apparmor_parser, or a previous destroy already removed
//     it): idempotent no-op, the exact inverse of the spawn-side
//     "already exists → assume correct" check.
//  2. Profile file present → `apparmor_parser -R <path>` unloads it. A
//     failed unload (profile never loaded, AppArmor busy) does NOT strand
//     the FILE: the file is the durable leak and a still-loaded name pointing
//     at a removed path is self-limiting, so the removal below proceeds and
//     the unload error is returned for the caller's WARN.
//  3. os.Remove of the file; an absent-at-remove race is tolerated.
func removeAgentAppArmorProfileCtx(ctx context.Context, agentID string) error {
	username := agentUserPrefix + agentID
	profilePath := agentAppArmorProfilePath(username)

	if _, err := os.Stat(profilePath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat apparmor profile %s: %w", profilePath, err)
	}

	var unloadErr error
	if _, err := exec.LookPath("apparmor_parser"); err != nil {
		unloadErr = fmt.Errorf("apparmor_parser not found: %w", err)
	} else if out, perr := appArmorParserRunner(ctx, "-R", profilePath); perr != nil {
		unloadErr = fmt.Errorf("unload apparmor profile %s: %w (output: %s)", profilePath, perr, string(out))
	}

	if err := os.Remove(profilePath); err != nil && !os.IsNotExist(err) {
		if unloadErr != nil {
			return fmt.Errorf("%v; remove apparmor profile %s: %w", unloadErr, profilePath, err)
		}
		return fmt.Errorf("remove apparmor profile %s: %w", profilePath, err)
	}
	return unloadErr
}
