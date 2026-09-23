//go:build unix

package cli

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/deployBunker/bunker/internal/mountdriver"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// MOUNT-006 seam tests for the mount path: backward compatibility (an
// old/pre-seam response still mounts via sshfs), explicit-driver dispatch,
// and the loud named refusal for an unknown driver — never a silent sshfs
// fallback.

// newMountSpecTestServer starts a mock GetAgent whose AgentSummary carries
// the given MountSpec, and points the CLI config at it.
func newMountSpecTestServer(t *testing.T, spec *v1.MountSpec, legacyMount string) {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(&mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:    "df0916a",
				SshfsMount: legacyMount,
				MountSpec:  spec,
			},
		},
	})
	r.Mount(path, h)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"default": {Name: "default", URL: server.URL},
		},
		ActiveServer: "default",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
	t.Setenv(SessionTargetEnvVar, "default")
}

// TestMountCommand_LegacyResponseStillMountsViaSSHFS is the backward-compat
// criterion (AC a, client half): a response shaped like a PRE-SEAM server's
// — no MountSpec at all, only the legacy opaque sshfs_mount string — must
// still dispatch the sshfs driver and mount unchanged.
func TestMountCommand_LegacyResponseStillMountsViaSSHFS(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountSpecTestServer(t, nil, mountFixtureSshfsMount)
	writeMountClientKey(t)
	calls, restore := stubSSHFSRun(t, 0, nil)
	defer restore()

	if err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt"); err != nil {
		t.Fatalf("legacy (no MountSpec) response failed to mount: %v", err)
	}
	if len(*calls) == 0 {
		t.Fatal("no sshfs invocation recorded")
	}
	if got := (*calls)[0][0]; got != "sshfs" {
		t.Errorf("legacy response dispatched %q, want sshfs", got)
	}
}

// TestMountCommand_ExplicitSSHFSMountSpecMounts proves the explicit identity
// path: a MountSpec naming the sshfs driver dispatches per that identity,
// byte-equivalent to the legacy path.
func TestMountCommand_ExplicitSSHFSMountSpecMounts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountSpecTestServer(t, &v1.MountSpec{
		Driver:  mountdriver.DefaultDriver,
		Command: mountFixtureSshfsMount,
	}, mountFixtureSshfsMount)
	writeMountClientKey(t)
	calls, restore := stubSSHFSRun(t, 0, nil)
	defer restore()

	if err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt"); err != nil {
		t.Fatalf("explicit sshfs MountSpec failed to mount: %v", err)
	}
	if len(*calls) == 0 {
		t.Fatal("no sshfs invocation recorded")
	}
	if got := (*calls)[0][0]; got != "sshfs" {
		t.Errorf("explicit-spec dispatch ran %q, want sshfs", got)
	}
}

// TestMountCommand_UnknownDriverIsLoudRefusal is the client-side half of the
// refusal contract (AC b): a server reporting an UNREGISTERED driver name
// must be refused by name — with no sshfs execution — never silently falling
// back to sshfs.
func TestMountCommand_UnknownDriverIsLoudRefusal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountSpecTestServer(t, &v1.MountSpec{
		Driver:  "rclone",
		Command: mountFixtureSshfsMount,
	}, mountFixtureSshfsMount)
	writeMountClientKey(t)
	calls, restore := stubSSHFSRun(t, 0, nil)
	defer restore()

	err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt")
	if err == nil {
		t.Fatal("unknown driver accepted — silent fallback?")
	}
	if len(*calls) != 0 {
		t.Fatalf("sshfs executed %d time(s) for an unknown driver; must refuse before any exec", len(*calls))
	}
	if !errors.Is(err, mountdriver.ErrUnknownDriver) {
		t.Errorf("error does not match mountdriver.ErrUnknownDriver: %v", err)
	}
	if !strings.Contains(err.Error(), "rclone") {
		t.Errorf("refusal does not name the offending driver: %v", err)
	}
}

// TestMountCommand_MountSpecCommandOverridesLegacy proves the command used
// is the MountSpec's (an identity the client trusts over re-parsing the
// legacy field): the spec's REMOTE SOURCE PATH survives the client rewrite
// and reaches the executed argv, so a spec command differing from the legacy
// string proves which one won.
func TestMountCommand_MountSpecCommandOverridesLegacy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	specCommand := "sshfs -o IdentityFile=/etc/bunkerd/ssh/df0916a -o idmap=user bunker-agent@bunker-host:/srv/spec-wins /mnt/bunker/df0916a"
	newMountSpecTestServer(t, &v1.MountSpec{
		Driver:  mountdriver.DefaultDriver,
		Command: specCommand,
	}, mountFixtureSshfsMount)
	writeMountClientKey(t)
	calls, restore := stubSSHFSRun(t, 0, nil)
	defer restore()

	if err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt"); err != nil {
		t.Fatalf("mount failed: %v", err)
	}
	if len(*calls) == 0 {
		t.Fatal("no sshfs invocation recorded")
	}
	foundSpec, foundLegacy := false, false
	for _, arg := range (*calls)[0] {
		if strings.Contains(arg, "/srv/spec-wins") {
			foundSpec = true
		}
		if strings.Contains(arg, "/home/bunker-agent") {
			foundLegacy = true
		}
	}
	if !foundSpec {
		t.Errorf("the MountSpec command's remote path did not reach the executed argv: %v", (*calls)[0])
	}
	if foundLegacy {
		t.Errorf("the legacy field's remote path won over the MountSpec command: %v", (*calls)[0])
	}
}
