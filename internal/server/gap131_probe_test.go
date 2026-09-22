package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/auth"
)

func TestGap131Probe(t *testing.T) {
	keyMgr := apikey.NewManager(gap131Secret)
	minter := auth.NewJWTAuth(gap131Secret, keyMgr)
	tok, err := minter.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	t.Log("issued", tok)

	// Drive the full interceptor path instead of the unexported method.
	interceptor := auth.NewMasterOnlyAuthInterceptor(gap131Secret, keyMgr, "static", true)
	reached := 0
	wrapped := interceptor.WrapUnary(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		reached++
		return connect.NewResponse(&v1.ServerInfoResponse{}), nil
	})
	req := connect.NewRequest(&v1.ServerInfoRequest{})
	req.Header().Set("Authorization", "Bearer "+tok)
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if reached != 1 {
		t.Fatalf("reached=%d", reached)
	}
}
