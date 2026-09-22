// keys_test.go — GAP-132 CLI coverage for `bunker key rotate|list|revoke`
// against a live in-process connect server. Pins the review-note contract
// that `key rotate` prints the new secret EXACTLY ONCE, and that list/revoke
// carry metadata/status only.
package cli

import (
	"context"
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
