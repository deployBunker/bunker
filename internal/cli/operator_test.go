// operator_test.go — GAP-130 CLI coverage for `bunker operator
// create|list|delete` against a live in-process connect server. Pins the
// review-note contracts: create prints the token EXACTLY ONCE, list carries
// metadata only, delete reports status.
package cli

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// operatorMockServer records operator-lifecycle calls and returns canned
// responses.
type operatorMockServer struct {
	mockBunkerdServer
	createCalled   int
	deleteCalled   int
	listCalled     int
	authHeaderSeen string
	createName     string
	createRole     string
	createTTL      uint32
	deleteName     string
	listAgentID    string
	newToken       string
	operators      []*v1.OperatorInfo
}

func (m *operatorMockServer) OperatorCreate(ctx context.Context, req *connect.Request[v1.OperatorCreateRequest]) (*connect.Response[v1.OperatorCreateResponse], error) {
	m.createCalled++
	m.authHeaderSeen = req.Header().Get("Authorization")
	m.createName = req.Msg.GetName()
	m.createRole = req.Msg.GetRole()
	m.createTTL = req.Msg.GetTtlSeconds()
	return connect.NewResponse(&v1.OperatorCreateResponse{
		Name:       m.createName,
		Role:       m.createRole,
		Token:      m.newToken,
		ExpiresAt:  "2026-11-10T12:00:00Z",
		TtlSeconds: m.createTTL,
	}), nil
}

func (m *operatorMockServer) OperatorList(ctx context.Context, req *connect.Request[v1.OperatorListRequest]) (*connect.Response[v1.OperatorListResponse], error) {
	m.listCalled++
	m.authHeaderSeen = req.Header().Get("Authorization")
	m.listAgentID = req.Msg.GetAgentId()
	return connect.NewResponse(&v1.OperatorListResponse{Operators: m.operators}), nil
}

func (m *operatorMockServer) OperatorDelete(ctx context.Context, req *connect.Request[v1.OperatorDeleteRequest]) (*connect.Response[v1.OperatorDeleteResponse], error) {
	m.deleteCalled++
	m.authHeaderSeen = req.Header().Get("Authorization")
	m.deleteName = req.Msg.GetName()
	return connect.NewResponse(&v1.OperatorDeleteResponse{Name: m.deleteName, Status: "deleted"}), nil
}

// Compile-time interface check: operatorMockServer must satisfy BunkerdHandler.
var _ bunkerv1connect.BunkerdHandler = (*operatorMockServer)(nil)

// newOperatorTestSetup writes a scratch CLI config whose active server
// points at an in-process mock and returns the mock.
func newOperatorTestSetup(t *testing.T, mock *operatorMockServer) {
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

// runOperator captures stdout of `bunker operator <args...>`.
func runOperator(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out string
	cmd := NewOperatorCommand()
	cmd.SetArgs(args)
	var execErr error
	out = captureStdout(t, func() { execErr = cmd.Execute() })
	return out, execErr
}

// TestOperatorCreate_PrintsTokenExactlyOnce is the load-bearing contract
// (mirrors key rotate): a displayed-once credential printed twice doubles
// the surface for it to leak.
func TestOperatorCreate_PrintsTokenExactlyOnce(t *testing.T) {
	mock := &operatorMockServer{newToken: "op-token-0123456789abcdef"}
	newOperatorTestSetup(t, mock)

	out, err := runOperator(t, "create", "ci-bot", "--role", "operator", "--ttl-seconds", "3600")
	if err != nil {
		t.Fatalf("operator create: %v", err)
	}
	if got := strings.Count(out, "op-token-0123456789abcdef"); got != 1 {
		t.Fatalf("token appears %d times in output, want exactly 1:\n%s", got, out)
	}
	if !strings.Contains(out, "EXACTLY ONCE") || !strings.Contains(out, "ci-bot") || !strings.Contains(out, "operator") {
		t.Fatalf("output missing the once-only warning or identity:\n%s", out)
	}
	if mock.createName != "ci-bot" || mock.createRole != "operator" || mock.createTTL != 3600 {
		t.Fatalf("mock got name=%q role=%q ttl=%d", mock.createName, mock.createRole, mock.createTTL)
	}
	if mock.authHeaderSeen != "Bearer master-test-token" {
		t.Fatalf("request Authorization = %q", mock.authHeaderSeen)
	}
}

// TestOperatorCreate_RequiresRole pins the flag grammar: --role is required.
func TestOperatorCreate_RequiresRole(t *testing.T) {
	mock := &operatorMockServer{newToken: "x"}
	newOperatorTestSetup(t, mock)
	if _, err := runOperator(t, "create", "ci-bot"); err == nil {
		t.Fatal("operator create without --role succeeded, want flag error")
	}
	if mock.createCalled != 0 {
		t.Fatal("server was called without --role")
	}
}

// TestOperatorList_MetadataOnlyAndFilter pins the list surface: rows carry
// metadata (no token column exists at all) and --agent-id reaches the RPC.
func TestOperatorList_MetadataOnlyAndFilter(t *testing.T) {
	mock := &operatorMockServer{operators: []*v1.OperatorInfo{
		{Name: "kelly", Role: "operator", CreatedAt: "2026-10-11T00:00:00Z", ExpiresAt: "2026-11-10T00:00:00Z"},
		{Name: "agent-77", Role: "agent:self", AgentId: "agent-77", CreatedAt: "2026-10-11T00:00:00Z", ExpiresAt: "2026-11-10T00:00:00Z"},
	}}
	newOperatorTestSetup(t, mock)

	out, err := runOperator(t, "list")
	if err != nil {
		t.Fatalf("operator list: %v", err)
	}
	for _, want := range []string{"kelly", "operator", "agent-77", "agent:self", "NAME", "ROLE"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	if mock.listAgentID != "" {
		t.Fatalf("unfiltered list sent agent_id %q", mock.listAgentID)
	}

	if _, err := runOperator(t, "list", "--agent-id", "agent-77"); err != nil {
		t.Fatalf("operator list --agent-id: %v", err)
	}
	if mock.listAgentID != "agent-77" {
		t.Fatalf("--agent-id did not reach the RPC (sent %q)", mock.listAgentID)
	}
}

// TestOperatorDelete_ReportsStatus pins delete: the name reaches the RPC and
// the status is echoed.
func TestOperatorDelete_ReportsStatus(t *testing.T) {
	mock := &operatorMockServer{}
	newOperatorTestSetup(t, mock)

	out, err := runOperator(t, "delete", "ci-bot")
	if err != nil {
		t.Fatalf("operator delete: %v", err)
	}
	if !strings.Contains(out, "ci-bot") || !strings.Contains(out, "deleted") {
		t.Fatalf("delete output missing identity/status:\n%s", out)
	}
	if mock.deleteName != "ci-bot" {
		t.Fatalf("RPC got name %q", mock.deleteName)
	}
	if mock.authHeaderSeen != "Bearer master-test-token" {
		t.Fatalf("request Authorization = %q", mock.authHeaderSeen)
	}
}

// TestOperatorGroupRegistered pins the command tree: all three verbs are
// reachable and each names the --server flag.
func TestOperatorGroupRegistered(t *testing.T) {
	cmd := NewOperatorCommand()
	names := map[string]bool{}
	for _, c := range cmd.Commands() {
		names[c.Name()] = true
	}
	for _, want := range []string{"create", "list", "delete"} {
		if !names[want] {
			t.Fatalf("operator subcommand %q is not registered; registered: %v", want, names)
		}
	}
	for _, name := range []string{"create", "list", "delete"} {
		var out strings.Builder
		c := cmd
		c.SetOut(&out)
		c.SetErr(&out)
		c.SetArgs([]string{name, "--help"})
		if err := c.Execute(); err != nil {
			t.Fatalf("operator %s --help: %v", name, err)
		}
		if !strings.Contains(out.String(), "--server") {
			t.Errorf("`operator %s --help` does not name --server:\n%s", name, out.String())
		}
	}
}
