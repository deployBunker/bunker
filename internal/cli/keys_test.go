// keys_test.go — GAP-132 CLI coverage for `bunker key rotate|list|revoke|fetch`
// against a live in-process connect server. Pins the review-note contract
// that `key rotate` prints the new secret EXACTLY ONCE, that list/revoke
// carry metadata/status only, and (DF-BUNKER-73) that `key fetch` recovers a
// lost local SSH key via the GetAgentKey RPC.
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// keysMockServer records key-lifecycle calls and returns canned responses.
type keysMockServer struct {
	mockBunkerdServer
	rotateCalled    int
	revokeCalled    int
	revokeKeyID     string
	authHeaderSeen  string
	newSecret       string
	rotatePrevFP    string
	revokeStatus    string
	listKeys        []*v1.KeyInfo
	listRequestedID string
	getKeyCalled    int
	getKeyAgentID   string
	sshPrivateKey   string
	keyErr          error
}

func (m *keysMockServer) RotateJWTSecret(ctx context.Context, req *connect.Request[v1.RotateJWTSecretRequest]) (*connect.Response[v1.RotateJWTSecretResponse], error) {
	m.rotateCalled++
	m.authHeaderSeen = req.Header().Get("Authorization")
	return connect.NewResponse(&v1.RotateJWTSecretResponse{
		JwtSecret:           m.newSecret,
		RotatedAt:           "2026-09-22T13:00:00Z",
		OverlapSeconds:      req.Msg.GetOverlapSeconds(),
		PreviousFingerprint: m.rotatePrevFP,
	}), nil
}

func (m *keysMockServer) RevokeKey(ctx context.Context, req *connect.Request[v1.RevokeKeyRequest]) (*connect.Response[v1.RevokeKeyResponse], error) {
	m.revokeCalled++
	m.revokeKeyID = req.Msg.GetKeyId()
	m.authHeaderSeen = req.Header().Get("Authorization")
	return connect.NewResponse(&v1.RevokeKeyResponse{KeyId: m.revokeKeyID, Status: m.revokeStatus}), nil
}

func (m *keysMockServer) KeyList(ctx context.Context, req *connect.Request[v1.KeyListRequest]) (*connect.Response[v1.KeyListResponse], error) {
	m.listRequestedID = req.Msg.GetAgentId()
	m.authHeaderSeen = req.Header().Get("Authorization")
	return connect.NewResponse(&v1.KeyListResponse{Keys: m.listKeys}), nil
}

// GetAgentKey fakes the GAP-128 key-retrieval RPC for the fetch verb
// (DF-BUNKER-73); the embedded mockBunkerdServer default is Unimplemented.
func (m *keysMockServer) GetAgentKey(ctx context.Context, req *connect.Request[v1.GetAgentKeyRequest]) (*connect.Response[v1.GetAgentKeyResponse], error) {
	m.getKeyCalled++
	m.getKeyAgentID = req.Msg.GetAgentId()
	m.authHeaderSeen = req.Header().Get("Authorization")
	if m.keyErr != nil {
		return nil, m.keyErr
	}
	return connect.NewResponse(&v1.GetAgentKeyResponse{
		AgentId:       m.getKeyAgentID,
		SshPrivateKey: m.sshPrivateKey,
	}), nil
}

// Compile-time interface check: keysMockServer must satisfy BunkerdHandler.
var _ bunkerv1connect.BunkerdHandler = (*keysMockServer)(nil)

// newKeysTestSetup writes a scratch CLI config whose active server points at
// an in-process mock and returns the mock.
func newKeysTestSetup(t *testing.T, mock *keysMockServer) {
	t.Helper()
	t.Setenv(SessionTargetEnvVar, "mock")
	t.Setenv("HOME", t.TempDir())
	srv := newTestServer(t, mock)
	t.Cleanup(srv.Close)
	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"mock": {Name: "mock", URL: srv.URL, Token: "master-test-token"},
		},
		ActiveServer: "mock",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("save test config: %v", err)
	}
}

// runKeys captures stdout of `bunker key <args...>`.
func runKeys(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out string
	cmd := NewKeysCommand()
	cmd.SetArgs(args)
	var execErr error
	out = captureStdout(t, func() { execErr = cmd.Execute() })
	return out, execErr
}

// TestKeyRotate_PrintsSecretExactlyOnce is the load-bearing review-note
// contract: the new secret appears ONCE in the whole output (losing a
// displayed-once credential is unrecoverable; printing it twice doubles the
// surface for it to leak into logs or scrollback copies).
func TestKeyRotate_PrintsSecretExactlyOnce(t *testing.T) {
	mock := &keysMockServer{
		newSecret:    "rotated-secret-value-0123456789abcdef",
		rotatePrevFP: "sha256:deadbeef0011",
	}
	newKeysTestSetup(t, mock)

	out, execErr := runKeys(t, "rotate")
	if execErr != nil {
		t.Fatalf("execute key rotate: %v", execErr)
	}
	if n := strings.Count(out, mock.newSecret); n != 1 {
		t.Fatalf("new secret printed %d times, want EXACTLY 1:\n%s", n, out)
	}
	if mock.rotateCalled != 1 {
		t.Fatalf("rotate called %d times", mock.rotateCalled)
	}
	if mock.authHeaderSeen != "Bearer master-test-token" {
		t.Fatalf("rotation must authenticate with the master token, saw %q", mock.authHeaderSeen)
	}
	for _, want := range []string{"overlap_seconds: 600", "sha256:deadbeef0011", "rotated_at: 2026-09-22T13:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rotate output missing %q:\n%s", want, out)
		}
	}
}

// TestKeyRotate_ForwardsOverlapSeconds pins the flag plumbing: the CLI value
// reaches the RPC request (the mock echoes it back).
func TestKeyRotate_ForwardsOverlapSeconds(t *testing.T) {
	mock := &keysMockServer{newSecret: "s", rotatePrevFP: "fp"}
	newKeysTestSetup(t, mock)

	out, execErr := runKeys(t, "rotate", "--overlap-seconds", "123")
	if execErr != nil {
		t.Fatalf("execute: %v", execErr)
	}
	if mock.rotateCalled != 1 {
		t.Fatalf("rotate called %d times", mock.rotateCalled)
	}
	if !strings.Contains(out, "overlap_seconds: 123") {
		t.Fatalf("--overlap-seconds did not reach the RPC:\n%s", out)
	}
}

// TestKeyList_PrintsMetadataRows covers the list surface end to end,
// including the --agent filter reaching the RPC.
func TestKeyList_PrintsMetadataRows(t *testing.T) {
	mock := &keysMockServer{listKeys: []*v1.KeyInfo{
		{KeyId: "bk_abc", AgentId: "agent-a", CreatedAt: "2026-09-22T10:00:00Z", ExpiresAt: "2026-09-23T10:00:00Z"},
		{KeyId: "bk_old", AgentId: "agent-b", CreatedAt: "2026-09-21T10:00:00Z", ExpiresAt: "2026-09-22T10:00:00Z", Revoked: true},
	}}
	newKeysTestSetup(t, mock)

	out, execErr := runKeys(t, "list")
	if execErr != nil {
		t.Fatalf("execute key list: %v", execErr)
	}
	if mock.authHeaderSeen != "Bearer master-test-token" {
		t.Fatalf("list must authenticate with the master token, saw %q", mock.authHeaderSeen)
	}
	for _, want := range []string{"bk_abc", "agent=a", "bk_old", "REVOKED"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}

	if _, execErr := runKeys(t, "list", "--agent", "agent-a"); execErr != nil {
		t.Fatalf("execute filtered list: %v", execErr)
	}
	if mock.listRequestedID != "agent-a" {
		t.Fatalf("--agent filter not forwarded: %q", mock.listRequestedID)
	}
}

// TestKeyRevoke_SendsKeyIDAndPrintsStatus covers the revoke surface end to
// end: the key id reaches the RPC and the printed status is the daemon's.
func TestKeyRevoke_SendsKeyIDAndPrintsStatus(t *testing.T) {
	mock := &keysMockServer{revokeStatus: "revoked"}
	newKeysTestSetup(t, mock)

	out, execErr := runKeys(t, "revoke", "bk_deadbeef")
	if execErr != nil {
		t.Fatalf("execute key revoke: %v", execErr)
	}
	if mock.revokeCalled != 1 || mock.revokeKeyID != "bk_deadbeef" {
		t.Fatalf("revoke call = %d, keyID = %q", mock.revokeCalled, mock.revokeKeyID)
	}
	if mock.authHeaderSeen != "Bearer master-test-token" {
		t.Fatalf("revoke must authenticate with the master token, saw %q", mock.authHeaderSeen)
	}
	if !strings.Contains(out, "bk_deadbeef: revoked") {
		t.Fatalf("revoke output wrong:\n%s", out)
	}
}

// ── DF-BUNKER-73: `key fetch` — CLI recovery path for a keyless agent ──────

// TestKeyFetchCommand_WritesKeyFile pins the fetch verb's happy path end to
// end: the agent id reaches the GetAgentKey RPC, the returned private key is
// written to the client-local keys dir at the exact path the SSH-family
// verbs read (keys/<agent-id>, dir created as needed), the file holds the
// key at 0600, and the master credential authenticates the RPC.
func TestKeyFetchCommand_WritesKeyFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	mock := &keysMockServer{sshPrivateKey: "fetched-private-key-data"}
	newKeysTestSetup(t, mock)

	out, execErr := runKeys(t, "fetch", "eduos-agent")
	if execErr != nil {
		t.Fatalf("execute key fetch: %v", execErr)
	}
	if mock.getKeyCalled != 1 {
		t.Fatalf("GetAgentKey called %d times, want 1", mock.getKeyCalled)
	}
	if mock.getKeyAgentID != "eduos-agent" {
		t.Fatalf("GetAgentKey asked for agent %q, want eduos-agent", mock.getKeyAgentID)
	}
	if mock.authHeaderSeen != "Bearer master-test-token" {
		t.Fatalf("fetch must authenticate with the master token, saw %q", mock.authHeaderSeen)
	}

	// The key file lands exactly where the SSH-family verbs look: the keys
	// dir next to the CLI config, named by agent id. Resolve the config path
	// the same way the command does instead of guessing ~/.bunker vs
	// ~/.config/bunker.
	cfgPath, err := configFilePath()
	if err != nil {
		t.Fatalf("resolve config path: %v", err)
	}
	keyPath := filepath.Join(filepath.Dir(cfgPath), "keys", "eduos-agent")
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("key file not written under %s: %v", keyPath, err)
	}
	if string(raw) != "fetched-private-key-data" {
		t.Errorf("key file content %q, want the key returned by GetAgentKey", raw)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("key file mode %v, want 0600", got)
	}
	keyDirInfo, err := os.Stat(filepath.Dir(keyPath))
	if err != nil {
		t.Fatalf("stat keys dir: %v", err)
	}
	if got := keyDirInfo.Mode().Perm(); got != 0700 {
		t.Errorf("keys dir mode %v, want 0700", got)
	}
	if !strings.Contains(out, "eduos-agent") {
		t.Errorf("output should name the agent, got:\n%s", out)
	}
}

// TestKeyFetchCommand_AgentNotFound pins the failure arm: a daemon
// CodeNotFound answer (wrong agent id, stopped key, missing key file
// server-side) surfaces as the command's error — exit 1 with a message
// naming the agent — rather than a silent success or a written empty file.
func TestKeyFetchCommand_AgentNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mock := &keysMockServer{
		keyErr: connect.NewError(connect.CodeNotFound, fmt.Errorf(`agent "ghost" not found`)),
	}
	newKeysTestSetup(t, mock)

	out, execErr := runKeys(t, "fetch", "ghost")
	if execErr == nil {
		t.Fatalf("fetch of a missing agent must fail, got success output:\n%s", out)
	}
	if !strings.Contains(execErr.Error(), "agent not found") {
		t.Fatalf("error must include the daemon's agent-not-found verdict, got: %v", execErr)
	}
	if !strings.Contains(execErr.Error(), "ghost") {
		t.Fatalf("error must name the agent, got: %v", execErr)
	}
	if mock.getKeyCalled != 1 {
		t.Fatalf("GetAgentKey called %d times, want 1", mock.getKeyCalled)
	}
	// Nothing must be written locally on failure.
	cfgPath, err := configFilePath()
	if err != nil {
		t.Fatalf("resolve config path: %v", err)
	}
	keyPath := filepath.Join(filepath.Dir(cfgPath), "keys", "ghost")
	if _, statErr := os.Stat(keyPath); !os.IsNotExist(statErr) {
		t.Errorf("no key file may be written for a failed fetch, found %s", keyPath)
	}
}
