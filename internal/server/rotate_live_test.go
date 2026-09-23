// rotate_live_test.go — DF-BUNKER-45 acceptance coverage: a jwt_secret
// rotation performed through the REAL RotateJWTSecret RPC must take effect on
// the REAL request-validation interceptors of BOTH service mounts, without a
// restart.
//
// The defect this pins: server.go used to pass the raw secret string to
// auth.NewMasterOnlyAuthInterceptor / auth.NewJWTAuthInterceptor, which each
// built a PRIVATE JWTAuth holding the boot secret forever. RotateJWTSecret
// mutated only the service's own instance, so (live-proven on the demo
// daemon) a JWT minted with the secret the rotate RPC returned was rejected
// 401 "signature is invalid" immediately after every rotation, while a JWT
// minted with the boot secret kept validating forever. The overlap window was
// unreachable.
//
// This test crosses the HTTP wire (httptest server + connect clients) exactly
// like TestHeartbeatAgent_LiveMasterOnlyInterceptor, so a mount-level
// regression (interceptor built from a private instance again) is caught
// behaviorally, not just by the source pin in heartbeat_test.go.
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// rotateOverlapWindow is the dual-accept window the test rotates with: long
// enough that the during-overlap probes (a few in-process HTTP round trips)
// cannot flake, short enough that the whole test stays fast.
const rotateOverlapWindow = 2 * time.Second

// newRotateLiveHarness builds the key path EXACTLY as server.go does since
// DF-BUNKER-45 — one shared JWTAuth, both interceptors derived from it — and
// serves the real Bunkerd and Agent handlers behind those interceptors on one
// httptest server reached by real connect clients.
func newRotateLiveHarness(t *testing.T) (
	*bunkerdService,
	bunkerv1connect.BunkerdClient,
	bunkerv1connect.AgentClient,
) {
	t.Helper()
	dir := t.TempDir()
	keyMgr, err := apikey.NewManagerAt(gap131Secret, filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatalf("key manager: %v", err)
	}
	// Mirror of server.go's post-DF-BUNKER-45 wiring: ONE instance, both
	// mounts derived from it. A private per-mount instance here would make
	// the phases below fail exactly like the live daemon did.
	shared := auth.NewJWTAuthWithStaticFallback(gap131Secret, "", keyMgr)
	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  testDiscardLogger(),
		keyMgr:  keyMgr,
		jwtAuth: shared,
	}
	masterOnly := auth.NewMasterOnlyAuthInterceptorFromAuth(shared, true)
	permissive := auth.NewJWTAuthInterceptorFromAuth(shared, true)

	mux := http.NewServeMux()
	bunkerdPath, bunkerdHandler := bunkerv1connect.NewBunkerdHandler(
		svc,
		connect.WithInterceptors(masterOnly),
	)
	mux.Handle(bunkerdPath, bunkerdHandler)
	// The Agent service gets a real tracker so agent-scoped JWTs reach
	// GetInfo's record lookup the way a live daemon serves them.
	tracker := resource.NewTracker(10, testDiscardLogger())
	if err := tracker.Register(&resource.AgentRecord{
		AgentID:   "agent-45",
		Status:    "running",
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}); err != nil {
		t.Fatalf("register agent-45: %v", err)
	}
	agentPath, agentHandler := bunkerv1connect.NewAgentHandler(
		&agentService{logger: testDiscardLogger(), tracker: tracker},
		connect.WithInterceptors(permissive),
	)
	mux.Handle(agentPath, agentHandler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return svc,
		bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL),
		bunkerv1connect.NewAgentClient(srv.Client(), srv.URL)
}

// callKeyList drives the real Bunkerd mount (KeyList handler behind the
// master-only interceptor) with the given bearer token.
func callKeyList(t *testing.T, ctx context.Context, c bunkerv1connect.BunkerdClient, token string) error {
	t.Helper()
	req := connect.NewRequest(&v1.KeyListRequest{})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}
	_, err := c.KeyList(ctx, req)
	return err
}

// callGetInfo drives the real Agent mount (GetInfo handler behind the
// permissive interceptor) with the given bearer token.
func callGetInfo(t *testing.T, ctx context.Context, c bunkerv1connect.AgentClient, token string) error {
	t.Helper()
	req := connect.NewRequest(&v1.GetInfoRequest{})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}
	_, err := c.GetInfo(ctx, req)
	return err
}

// TestRotateJWTSecret_LiveOnBothInterceptorMounts proves the full rotation
// contract through the real handler+interceptor stack of BOTH services:
//
//	boot        pre-rotation JWTs (boot secret) validate;
//	rotate      the RotateJWTSecret RPC returns the new secret once and the
//	            previous fingerprint;
//	immediate   JWTs minted with the RETURNED secret validate on both mounts
//	            with no restart — the assertion the defect failed (401
//	            "signature is invalid" right after every rotation);
//	overlap     pre-rotation JWTs keep validating while the window is open
//	            (GAP-132 dual accept);
//	closed      once the window closes, pre-rotation JWTs are 401 on both
//	            mounts while new-secret JWTs still validate (the new secret is
//	            authoritative, not merely dual-accepted).
func TestRotateJWTSecret_LiveOnBothInterceptorMounts(t *testing.T) {
	svc, bunkerd, agent := newRotateLiveHarness(t)
	ctx := context.Background()

	// --- boot: credentials minted with the BOOT secret, before any rotate.
	oldMaster, err := svc.jwtAuth.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue boot master jwt: %v", err)
	}
	oldAgent, err := svc.jwtAuth.IssueAgentToken("agent-45", time.Hour)
	if err != nil {
		t.Fatalf("issue boot agent jwt: %v", err)
	}
	if err := callKeyList(t, ctx, bunkerd, oldMaster); err != nil {
		t.Fatalf("boot-secret master JWT must validate on Bunkerd pre-rotate: %v", err)
	}
	if err := callGetInfo(t, ctx, agent, oldAgent); err != nil {
		t.Fatalf("boot-secret agent JWT must validate on Agent pre-rotate: %v", err)
	}

	// --- rotate through the REAL RPC (auditing disabled: nil auditLog).
	resp, err := svc.RotateJWTSecret(ctx, connect.NewRequest(&v1.RotateJWTSecretRequest{
		OverlapSeconds: uint32(rotateOverlapWindow / time.Second),
	}))
	if err != nil {
		t.Fatalf("RotateJWTSecret rpc: %v", err)
	}
	newSecret := resp.Msg.GetJwtSecret()
	if newSecret == "" {
		t.Fatal("rotate response must carry the new secret exactly once")
	}
	if resp.Msg.GetPreviousFingerprint() == "" {
		t.Fatal("rotate response must carry the previous fingerprint")
	}
	if got := time.Duration(resp.Msg.GetOverlapSeconds()) * time.Second; got != rotateOverlapWindow {
		t.Fatalf("overlap echo = %s, want %s", got, rotateOverlapWindow)
	}

	// JWTs minted with the secret the RPC RETURNED (the credential an
	// operator would re-authenticate with immediately after rotating).
	newMaster, err := svc.jwtAuth.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue post-rotate master jwt: %v", err)
	}
	newAgent, err := svc.jwtAuth.IssueAgentToken("agent-45", time.Hour)
	if err != nil {
		t.Fatalf("issue post-rotate agent jwt: %v", err)
	}

	// --- immediate: new-secret JWTs validate on BOTH mounts with no restart.
	// Before DF-BUNKER-45 both calls failed 401 "signature is invalid":
	// each mount's interceptor still held only the boot secret.
	if err := callKeyList(t, ctx, bunkerd, newMaster); err != nil {
		t.Fatalf("new-secret master JWT must validate on Bunkerd IMMEDIATELY after rotate: %v", err)
	}
	infoErr := callGetInfo(t, ctx, agent, newAgent)
	if infoErr != nil {
		t.Fatalf("new-secret agent JWT must validate on Agent IMMEDIATELY after rotate: %v", infoErr)
	}

	// --- overlap: boot-secret JWTs keep validating while the window is open
	// (GAP-132 dual accept), on both mounts.
	if err := callKeyList(t, ctx, bunkerd, oldMaster); err != nil {
		t.Fatalf("boot-secret master JWT must validate DURING the overlap window on Bunkerd: %v", err)
	}
	if err := callGetInfo(t, ctx, agent, oldAgent); err != nil {
		t.Fatalf("boot-secret agent JWT must validate DURING the overlap window on Agent: %v", err)
	}

	// --- window closed: sleep out the remaining overlap, then the boot
	// secret must be dead on both mounts while new-secret JWTs still work
	// (new secret authoritative, retired secret expired — not a dual accept).
	time.Sleep(rotateOverlapWindow + 200*time.Millisecond)
	if err := callKeyList(t, ctx, bunkerd, oldMaster); err == nil {
		t.Fatal("boot-secret master JWT must be rejected on Bunkerd AFTER the overlap window closes")
	} else if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("closed-window rejection code = %v, want Unauthenticated", connect.CodeOf(err))
	}
	if err := callGetInfo(t, ctx, agent, oldAgent); err == nil {
		t.Fatal("boot-secret agent JWT must be rejected on Agent AFTER the overlap window closes")
	} else if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("closed-window rejection code = %v, want Unauthenticated", connect.CodeOf(err))
	}
	if err := callKeyList(t, ctx, bunkerd, newMaster); err != nil {
		t.Fatalf("new-secret master JWT must still validate after the window closes: %v", err)
	}
	if err := callGetInfo(t, ctx, agent, newAgent); err != nil {
		t.Fatalf("new-secret agent JWT must still validate after the window closes: %v", err)
	}

	// Second rotation (the defect survived EVERY rotation live): the same
	// immediate-visibility property must hold for the third secret too.
	resp2, err := svc.RotateJWTSecret(ctx, connect.NewRequest(&v1.RotateJWTSecretRequest{
		OverlapSeconds: uint32(rotateOverlapWindow / time.Second),
	}))
	if err != nil {
		t.Fatalf("second RotateJWTSecret rpc: %v", err)
	}
	if resp2.Msg.GetJwtSecret() == "" || resp2.Msg.GetJwtSecret() == newSecret {
		t.Fatal("second rotate must return a fresh secret")
	}
	thirdMaster, err := svc.jwtAuth.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue third-generation master jwt: %v", err)
	}
	if err := callKeyList(t, ctx, bunkerd, thirdMaster); err != nil {
		t.Fatalf("third-generation secret must validate immediately on Bunkerd: %v", err)
	}
	// The FIRST-generation secret stays dead after the second rotation.
	if err := callKeyList(t, ctx, bunkerd, oldMaster); err == nil {
		t.Fatal("first-generation secret must stay rejected after the second rotation")
	}
}
