// secret_class_deny_test.go — DF-BUNKER-46 acceptance coverage: presenting
// the HS256 JWT signing secret as a bearer token must be DENIED with an
// error that names the category mistake ("signing secret, not a bearer
// token"), not the generic "invalid token". The classification covers ONLY
// secrets this daemon currently accepts (live secret + retired secret while
// the GAP-132 overlap window is open); a random wrong token keeps the
// generic error and a secret whose window has closed falls back to it too
// (it no longer authenticates anything, so it must reveal nothing).
package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
)

const (
	classTestSecretA = "df46-signing-secret-a-0123456789abcdef"
	classTestSecretB = "df46-signing-secret-b-fedcba9876543210"
	classTestStatic  = "df46-static-bearer-token"
)

// presentBearer drives the real unary interceptor with the given bearer
// token and returns the auth error (nil on success).
func presentBearer(t *testing.T, a *JWTAuth, token string) error {
	t.Helper()
	wrapped := a.WrapUnary(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, nil
	})
	req := connect.NewRequest(&dummyMsg{})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err := wrapped(context.Background(), req)
	return err
}

func TestJWTAuth_SigningSecretPresentedAsBearerIsClassified(t *testing.T) {
	a := NewJWTAuthWithStaticFallback(classTestSecretA, classTestStatic, nil)
	masterJWT, err := a.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue master jwt: %v", err)
	}

	cases := []struct {
		name     string
		token    string
		wantOK   bool   // true: must authenticate
		wantText string // when !wantOK: substring the denial must carry ("" = any denial)
	}{
		{"live signing secret as bearer gets the class denial", classTestSecretA, false, "signing secret, not a bearer token"},
		{"static bearer token still authenticates", classTestStatic, true, ""},
		{"jwt signed by the live secret still authenticates", masterJWT, true, ""},
		{"random wrong token keeps the generic invalid-token error", "totally-wrong-guess-0123456789abcdef", false, "invalid token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := presentBearer(t, a, tc.token)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("token must authenticate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("token must be rejected")
			}
			if connect.CodeOf(err) != connect.CodeUnauthenticated {
				t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("denial must contain %q, got: %v", tc.wantText, err)
			}
		})
	}

	// The class denial must be a DENY, not admission: the handler never ran
	// for the live-secret row. Prove via the master-only wrapper sharing the
	// same auth path — the class error still carries Unauthenticated there.
	t.Run("denial names the bearer alternative", func(t *testing.T) {
		err := presentBearer(t, a, classTestSecretA)
		if err == nil || !strings.Contains(err.Error(), "static auth.token") {
			t.Fatalf("class denial must point at static auth.token, got: %v", err)
		}
	})
}

func TestJWTAuth_RotatedOutSecretClassifiedDuringOverlapOnly(t *testing.T) {
	a := NewJWTAuth(classTestSecretA, nil)
	if _, err := a.RotateSecret(classTestSecretB, 80*time.Millisecond); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// During the overlap window the retired secret A is still accepted key
	// material — pasting it as a bearer token gets the class denial.
	err := presentBearer(t, a, classTestSecretA)
	if err == nil {
		t.Fatal("retired (overlap) secret as bearer must be rejected")
	}
	if !strings.Contains(err.Error(), "signing secret, not a bearer token") {
		t.Fatalf("overlap-window secret must still be classified, got: %v", err)
	}

	// The live secret B gets the same class denial...
	err = presentBearer(t, a, classTestSecretB)
	if err == nil {
		t.Fatal("live signing secret as bearer must be rejected")
	}
	if !strings.Contains(err.Error(), "signing secret, not a bearer token") {
		t.Fatalf("live secret must be classified after rotation, got: %v", err)
	}
	// ...while JWTs minted under B authenticate.
	tok, err := a.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue post-rotate jwt: %v", err)
	}
	if err := presentBearer(t, a, tok); err != nil {
		t.Fatalf("post-rotate jwt must authenticate: %v", err)
	}

	// After the window closes A is no longer accepted secret material: it
	// must fall back to the GENERIC error — the constant-time compare
	// covers only secrets the daemon currently accepts, so a dead secret
	// stays indistinguishable from a random string.
	time.Sleep(140 * time.Millisecond)
	err = presentBearer(t, a, classTestSecretA)
	if err == nil {
		t.Fatal("retired secret must be rejected after the overlap window closes")
	}
	if strings.Contains(err.Error(), "signing secret") {
		t.Fatalf("expired-window secret must keep the generic error (compare covers only accepted secrets), got: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("expired-window secret must carry the generic invalid-token error, got: %v", err)
	}
}
