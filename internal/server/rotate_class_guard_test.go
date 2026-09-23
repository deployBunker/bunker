// rotate_class_guard_test.go — DF-BUNKER-46 server-side acceptance: the
// signing secret and the static bearer token are different credential
// classes and RotateJWTSecret must keep them disjoint (a generated secret
// equal to auth.token is refused with CodeInvalidArgument, before any
// mutation), and the rotation audit record must name the class and the
// immediate in-memory switch.
package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
)

const rotate46StaticToken = "static-bearer-token-never-a-signing-secret"

// newRotate46Service wires the key path exactly like New() does post-
// DF-BUNKER-45 (one shared JWTAuth carrying the static-token fallback,
// master-only interceptor derivable from it), plus a static auth.token and
// a real audit log, against temp-dir stores.
func newRotate46Service(t *testing.T) (*bunkerdService, string) {
	t.Helper()
	dir := t.TempDir()
	keyMgr, err := apikey.NewManagerAt(gap131Secret, filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatalf("key manager: %v", err)
	}
	auditPath := filepath.Join(dir, "audit.log")
	l, err := audit.New(auditPath)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	cfg := config.DefaultConfig()
	cfg.Auth.Token = rotate46StaticToken
	svc := &bunkerdService{
		cfg:      cfg,
		logger:   testDiscardLogger(),
		keyMgr:   keyMgr,
		jwtAuth:  auth.NewJWTAuthWithStaticFallback(gap131Secret, cfg.Auth.Token, keyMgr),
		auditLog: l,
	}
	return svc, auditPath
}

// rotate46ForceGenerated pins the secret-generation seam to a fixed value
// and returns a restore func. Test-only: without it the collision case
// (generated secret == static token) depends on a 2^256 random draw.
func rotate46ForceGenerated(fixed string) (restore func()) {
	orig := generateRotateSecretFn
	generateRotateSecretFn = func() (string, error) { return fixed, nil }
	return func() { generateRotateSecretFn = orig }
}

func TestRotateJWTSecret_RefusesCollisionWithStaticToken(t *testing.T) {
	svc, _ := newRotate46Service(t)
	ctx := context.Background()

	restore := rotate46ForceGenerated(rotate46StaticToken)
	defer restore()

	_, err := svc.RotateJWTSecret(ctx, connect.NewRequest(&v1.RotateJWTSecretRequest{OverlapSeconds: 60}))
	if err == nil {
		t.Fatal("rotation must be refused when the generated secret equals the static auth.token")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "auth.token") {
		t.Fatalf("refusal must name the static auth.token class: %v", err)
	}

	// The refusal must be pre-mutation: after the seam is restored, a fresh
	// rotation must report the BOOT secret as the previous one — the failed
	// call must not have moved the signing secret.
	restore()
	resp, err := svc.RotateJWTSecret(ctx, connect.NewRequest(&v1.RotateJWTSecretRequest{OverlapSeconds: 60}))
	if err != nil {
		t.Fatalf("rotation after the refused call: %v", err)
	}
	if want := auth.FingerprintSecret(gap131Secret); resp.Msg.GetPreviousFingerprint() != want {
		t.Fatalf("previous fingerprint = %s, want %s (the refused call must not rotate)", resp.Msg.GetPreviousFingerprint(), want)
	}
}

func TestRotateJWTSecret_RotationStillSucceedsWithoutCollision(t *testing.T) {
	svc, _ := newRotate46Service(t)
	newSecret := strings.Repeat("a", 64) // valid HS256 material, != static token
	restore := rotate46ForceGenerated(newSecret)
	defer restore()

	resp, err := svc.RotateJWTSecret(context.Background(), connect.NewRequest(&v1.RotateJWTSecretRequest{OverlapSeconds: 60}))
	if err != nil {
		t.Fatalf("RotateJWTSecret rpc: %v", err)
	}
	if resp.Msg.GetJwtSecret() != newSecret {
		t.Fatal("rotate must return the generated secret")
	}
}

func TestRotateJWTSecret_AuditNamesClassAndImmediacy(t *testing.T) {
	svc, auditPath := newRotate46Service(t)
	ctx := context.Background()

	if _, err := svc.RotateJWTSecret(ctx, connect.NewRequest(&v1.RotateJWTSecretRequest{OverlapSeconds: 60})); err != nil {
		t.Fatalf("RotateJWTSecret rpc: %v", err)
	}
	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"/bunker.v1.Bunkerd/RotateJWTSecret",
		"JWT signing secret (NOT a bearer token) rotated",
		"in-memory switch is immediate",
		"previous fp=",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("audit log missing %q:\n%s", want, text)
		}
	}
	// No secret material in the chain.
	if strings.Contains(text, gap131Secret) || strings.Contains(text, rotate46StaticToken) {
		t.Fatal("audit log must never carry secret or token material")
	}
}
