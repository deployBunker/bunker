package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/programalias"
	"github.com/deployBunker/bunker/internal/resource"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

const (
	paTestAgent = "abc123"
	paTestKey   = "/keys/abc123"
	paTestHome  = "/home/bunker-abc123"
)

// newAliasTestService builds a bunkerdService over a temp alias store.
func newAliasTestService(t *testing.T) (*bunkerdService, *programalias.Registry) {
	t.Helper()
	reg := programalias.NewRegistry(filepath.Join(t.TempDir(), programalias.DefaultFileName))
	svc := &bunkerdService{
		cfg:            config.DefaultConfig(),
		logger:         testDiscardLogger(),
		tracker:        resource.NewTracker(10, testDiscardLogger()),
		programAliases: reg,
	}
	return svc, reg
}

// ── RPC CRUD ────────────────────────────────────────────────────

func TestProgramAliasRPCs(t *testing.T) {
	svc, _ := newAliasTestService(t)
	ctx := context.Background()

	// Empty store lists cleanly.
	list, err := svc.ListProgramAliases(ctx, connect.NewRequest(&v1.ListProgramAliasesRequest{}))
	if err != nil {
		t.Fatalf("ListProgramAliases (empty): %v", err)
	}
	if len(list.Msg.GetAliases()) != 0 {
		t.Fatalf("empty store returned %d aliases", len(list.Msg.GetAliases()))
	}
	if list.Msg.GetStorePath() == "" {
		t.Fatal("StorePath is empty")
	}

	// Create.
	put, err := svc.PutProgramAlias(ctx, connect.NewRequest(&v1.PutProgramAliasRequest{
		Alias: &v1.ProgramAlias{
			Name:        "yq",
			Image:       "mikefarah/yq:4",
			Entrypoint:  []string{"yq"},
			Description: "YAML filter",
		},
	}))
	if err != nil {
		t.Fatalf("PutProgramAlias: %v", err)
	}
	if put.Msg.GetStatus() != "created" {
		t.Fatalf("status = %q, want created", put.Msg.GetStatus())
	}
	if put.Msg.GetAlias().GetImage() != "mikefarah/yq:4" || len(put.Msg.GetAlias().GetEntrypoint()) != 1 {
		t.Fatalf("echoed alias = %+v", put.Msg.GetAlias())
	}

	// Read back through List.
	list, err = svc.ListProgramAliases(ctx, connect.NewRequest(&v1.ListProgramAliasesRequest{}))
	if err != nil {
		t.Fatalf("ListProgramAliases: %v", err)
	}
	if len(list.Msg.GetAliases()) != 1 || list.Msg.GetAliases()[0].GetName() != "yq" {
		t.Fatalf("list = %+v", list.Msg.GetAliases())
	}

	// Update is reported as an update, not a create.
	put, err = svc.PutProgramAlias(ctx, connect.NewRequest(&v1.PutProgramAliasRequest{
		Alias: &v1.ProgramAlias{Name: "yq", Image: "mikefarah/yq:4.44.3", Entrypoint: []string{"yq"}},
	}))
	if err != nil {
		t.Fatalf("PutProgramAlias (update): %v", err)
	}
	if put.Msg.GetStatus() != "updated" {
		t.Fatalf("status = %q, want updated", put.Msg.GetStatus())
	}
	list, _ = svc.ListProgramAliases(ctx, connect.NewRequest(&v1.ListProgramAliasesRequest{}))
	if got := list.Msg.GetAliases()[0].GetImage(); got != "mikefarah/yq:4.44.3" {
		t.Fatalf("update did not land: %q", got)
	}

	// Delete, then a second delete is NOT_FOUND (a real miss, not a silent ok).
	del, err := svc.DeleteProgramAlias(ctx, connect.NewRequest(&v1.DeleteProgramAliasRequest{Name: "yq"}))
	if err != nil {
		t.Fatalf("DeleteProgramAlias: %v", err)
	}
	if !del.Msg.GetDeleted() || del.Msg.GetName() != "yq" {
		t.Fatalf("delete response = %+v", del.Msg)
	}
	_, err = svc.DeleteProgramAlias(ctx, connect.NewRequest(&v1.DeleteProgramAliasRequest{Name: "yq"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("second delete code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestProgramAliasRPCPutRejectsBadInput(t *testing.T) {
	svc, _ := newAliasTestService(t)
	ctx := context.Background()
	cases := []struct {
		name  string
		alias *v1.ProgramAlias
	}{
		{"empty name", &v1.ProgramAlias{Image: "yq"}},
		{"path as name", &v1.ProgramAlias{Name: "/usr/bin/yq", Image: "yq"}},
		{"flag injection in name", &v1.ProgramAlias{Name: "yq -x", Image: "yq"}},
		{"empty image", &v1.ProgramAlias{Name: "yq"}},
		{"shell in image", &v1.ProgramAlias{Name: "yq", Image: "yq; rm -rf /"}},
		{"relative mount", &v1.ProgramAlias{Name: "yq", Image: "yq",
			Mounts: []*v1.ProgramAliasMount{{Host: "relative/path"}}}},
		{"non-home-root mount", &v1.ProgramAlias{Name: "yq", Image: "yq",
			Mounts: []*v1.ProgramAliasMount{{Host: "/etc"}}}},
		{"docker socket mount", &v1.ProgramAlias{Name: "yq", Image: "yq",
			Mounts: []*v1.ProgramAliasMount{{Host: "/run/bunker/abc123"}}}},
		{"relative container path", &v1.ProgramAlias{Name: "yq", Image: "yq",
			Mounts: []*v1.ProgramAliasMount{{Host: "/home/bunker-abc123/x", Container: "x"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.PutProgramAlias(ctx, connect.NewRequest(&v1.PutProgramAliasRequest{Alias: tc.alias}))
			if err == nil {
				t.Fatal("PutProgramAlias accepted invalid input")
			}
			if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (%v)", code, err)
			}
		})
	}
	// Nothing above may have mutated the store.
	list, err := svc.ListProgramAliases(ctx, connect.NewRequest(&v1.ListProgramAliasesRequest{}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Msg.GetAliases()) != 0 {
		t.Fatalf("invalid puts mutated the store: %+v", list.Msg.GetAliases())
	}
}

func TestProgramAliasRPCsRefuseWithoutRegistry(t *testing.T) {
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: testDiscardLogger()}
	ctx := context.Background()
	if _, err := svc.ListProgramAliases(ctx, connect.NewRequest(&v1.ListProgramAliasesRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("List code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
	if _, err := svc.PutProgramAlias(ctx, connect.NewRequest(&v1.PutProgramAliasRequest{
		Alias: &v1.ProgramAlias{Name: "yq", Image: "yq"},
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Put code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
	if _, err := svc.DeleteProgramAlias(ctx, connect.NewRequest(&v1.DeleteProgramAliasRequest{Name: "yq"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Delete code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

// ── Alias resolution ────────────────────────────────────────────

func TestLookupProgramAliasBareNameOnly(t *testing.T) {
	svc, reg := newAliasTestService(t)
	if err := reg.Put(programalias.Alias{Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, ok := svc.lookupProgramAlias("yq"); !ok {
		t.Fatal("bare registered name did not resolve")
	}
	for _, cmd := range []string{
		"/usr/bin/yq",  // absolute path = explicit native choice
		"./yq",         // relative path
		"yq --version", // not a program name
		"yq ",          // trailing space
		" unknown",     // punctuation
		"jq",           // unregistered
		"",             // nothing to resolve
		"$(id)",        // shell metacharacter
		"yq;rm",        // injection attempt
		"run/yq",       // slash anywhere
		strings.Repeat("a", programalias.NameMaxLen+1), // over the name cap
	} {
		if _, ok := svc.lookupProgramAlias(cmd); ok {
			t.Errorf("lookupProgramAlias(%q) resolved; want no resolution", cmd)
		}
	}

	// A service with no registry never resolves anything.
	bare := &bunkerdService{logger: testDiscardLogger()}
	if _, ok := bare.lookupProgramAlias("yq"); ok {
		t.Fatal("registry-less service resolved an alias")
	}
}

func TestProgramAliasLimitsFromAgentRecord(t *testing.T) {
	rec := &resource.AgentRecord{Limits: &v1.ResourceLimits{CpuQuota: 3.5, MemoryMaxBytes: 5 << 30}}
	got := programAliasLimits(rec)
	if got.CPUs != 3.5 || got.MemoryBytes != 5<<30 {
		t.Fatalf("limits = %+v", got)
	}
	// An agent with no envelope gets defaults at build time, never unlimited.
	zero := programAliasLimits(&resource.AgentRecord{})
	argv, err := programalias.BuildDockerArgv(programalias.Alias{Name: "yq", Image: "yq", Entrypoint: []string{"yq"}}, paTestHome, nil, zero, nil)
	if err != nil {
		t.Fatalf("BuildDockerArgv: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--memory 2147483648") || !strings.Contains(joined, "--cpus 2") {
		t.Fatalf("default limits missing: %s", joined)
	}
}

// ── Command builders ────────────────────────────────────────────

func TestBuildAgentProgramAliasCommandShape(t *testing.T) {
	a := programalias.Alias{Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"}}
	cmd, err := buildAgentProgramAliasCommand(paTestAgent, paTestHome, a, []string{"--version"}, programalias.Limits{}, false)
	if err != nil {
		t.Fatalf("buildAgentProgramAliasCommand: %v", err)
	}
	// The alias script rides inside `sh -c '...'`, so its own quoting appears
	// escaped; normalize before asserting on the intended text.
	flat := normalizeShellText(cmd)
	for _, want := range []string{
		// The daemon's usual env preamble.
		"/run/bunker/abc123/env",
		"PATH=/home/bunker-abc123/bin:",
		"DOCKER_HOST=unix:///run/bunker/abc123/docker.sock",
		"TMPDIR=/tmp",
		"sh -c ",
		// The alias script: shim install then exec of the SAME shim.
		"mkdir -p /home/bunker-abc123/bin",
		"# rev=",
		"exec /home/bunker-abc123/bin/yq --version",
		// The docker invocation the shim carries.
		"docker run --rm -i --user 0",
		"--network none",
		"--entrypoint yq",
		"mikefarah/yq:4",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("alias exec command missing %q:\n%s", want, flat)
		}
	}
	// No docker socket is bind-mounted into the alias container (the runtime
	// dir appears only as DOCKER_HOST for the host-side docker CLI).
	if strings.Contains(cmd, "-v /run/bunker") || strings.Contains(cmd, "/run/bunker/abc123:/run/bunker/abc123") {
		t.Errorf("alias exec bind-mounts the agent runtime dir:\n%s", cmd)
	}
	// Disclosure off by default: no sandbox marker.
	if strings.Contains(cmd, containmentSandboxEnv) {
		t.Errorf("undisclosed alias exec carries the sandbox marker:\n%s", cmd)
	}

	// Disclosure on carries the marker both into the session env and into the
	// container.
	cmdOn, err := buildAgentProgramAliasCommand(paTestAgent, paTestHome, a, nil, programalias.Limits{}, true)
	if err != nil {
		t.Fatalf("buildAgentProgramAliasCommand(disclosed): %v", err)
	}
	if n := strings.Count(cmdOn, containmentSandboxEnv); n != 2 {
		t.Errorf("disclosed alias exec should carry the marker twice (session env + container -e), got %d:\n%s", n, cmdOn)
	}
}

func TestBuildAgentProgramAliasRawCommandShape(t *testing.T) {
	a := programalias.Alias{Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"}}
	argv, err := buildAgentProgramAliasRawCommand(paTestAgent, paTestHome, a, []string{"--version"}, programalias.Limits{}, false)
	if err != nil {
		t.Fatalf("buildAgentProgramAliasRawCommand: %v", err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"env PATH=/home/bunker-abc123/bin:",
		"DOCKER_HOST=unix:///run/bunker/abc123/docker.sock",
		"docker run --rm -i --user 0",
		"--network none",
		"-v /home/bunker-abc123:/home/bunker-abc123 -w /home/bunker-abc123",
		"--entrypoint yq mikefarah/yq:4 --version",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("raw alias argv missing %q:\n%s", want, joined)
		}
	}
	// Raw mode keeps its no-shell contract: no shim install, no sh -c.
	if strings.Contains(joined, "sh -c") || strings.Contains(joined, "mkdir") {
		t.Errorf("raw alias argv introduced a shell:\n%s", joined)
	}
	// No runtime-dir bind (only DOCKER_HOST for the host-side CLI).
	if strings.Contains(joined, "-v /run/bunker") {
		t.Errorf("raw alias argv bind-mounts the runtime dir:\n%s", joined)
	}
}

// TestBuildAgentProgramAliasRejectsNonHomeMount is the exec-time home-only
// rule: the builder must refuse, so no command exists that would hand the
// container another agent's home.  A path outside the agent-home ROOT is
// refused even earlier, at registration (TestValidateMountShapeAtRegistration).
func TestBuildAgentProgramAliasRejectsNonHomeMount(t *testing.T) {
	a := programalias.Alias{
		Name:       "yq",
		Image:      "mikefarah/yq:4",
		Entrypoint: []string{"yq"},
		Mounts:     []programalias.Mount{{Host: "/home/bunker-someone-else/data"}},
	}
	if _, err := buildAgentProgramAliasCommand(paTestAgent, paTestHome, a, nil, programalias.Limits{}, false); !errors.Is(err, programalias.ErrMountNotUnderHome) {
		t.Fatalf("shell builder = %v, want ErrMountNotUnderHome", err)
	}
	if _, err := buildAgentProgramAliasRawCommand(paTestAgent, paTestHome, a, nil, programalias.Limits{}, false); !errors.Is(err, programalias.ErrMountNotUnderHome) {
		t.Fatalf("raw builder = %v, want ErrMountNotUnderHome", err)
	}
	// A mount of a host path outside the agent-home root never even reaches a
	// builder: the RPC refuses it at registration.
	if _, err := svcPutAlias(t, programalias.Alias{
		Name: "yq", Image: "mikefarah/yq:4",
		Mounts: []programalias.Mount{{Host: "/etc"}},
	}); !errors.Is(err, programalias.ErrMountOutsideHomeRoot) {
		t.Fatalf("registration of /etc mount = %v, want ErrMountOutsideHomeRoot", err)
	}
	// A home-inside mount is accepted.
	ok := a
	ok.Mounts = []programalias.Mount{{Host: paTestHome + "/src", ReadOnly: true}}
	if _, err := buildAgentProgramAliasCommand(paTestAgent, paTestHome, ok, nil, programalias.Limits{}, false); err != nil {
		t.Fatalf("home-inside mount refused: %v", err)
	}
}

// svcPutAlias registers an alias through the real RPC handler (so the
// registration-time validation is exercised, not just the package helper).
func svcPutAlias(t *testing.T, a programalias.Alias) (*v1.PutProgramAliasResponse, error) {
	t.Helper()
	svc, _ := newAliasTestService(t)
	resp, err := svc.PutProgramAlias(context.Background(), connect.NewRequest(&v1.PutProgramAliasRequest{
		Alias: programAliasToProto(a),
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestBuildAgentProgramAliasRefusesUnresolvedUser(t *testing.T) {
	orig := resolveAgentUID
	resolveAgentUID = func(string) (int, bool) { return 0, false }
	defer func() { resolveAgentUID = orig }()

	a := programalias.Alias{Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"}}
	if _, err := buildAgentProgramAliasCommand(paTestAgent, paTestHome, a, nil, programalias.Limits{}, false); !errors.Is(err, errProgramAliasIdentity) {
		t.Fatalf("shell builder = %v, want errProgramAliasIdentity", err)
	}
	if _, err := buildAgentProgramAliasRawCommand(paTestAgent, paTestHome, a, nil, programalias.Limits{}, false); !errors.Is(err, errProgramAliasIdentity) {
		t.Fatalf("raw builder = %v, want errProgramAliasIdentity", err)
	}
}

// ── First-run pull / cached second run ──────────────────────────

// installRecordingSSH puts a stub `ssh` first on PATH that APPENDS each
// invocation's argv (separated by a `#ARGV` line) to a log file, and fails
// when the remote command is an `image inspect` — so the daemon's
// inspect-then-pull flow is observable exactly as it is in production.
func installRecordingSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "ssh-log")
	stub := "#!/bin/sh\n" +
		"{\n  printf '#ARGV\\n'\n  for a in \"$@\"; do printf '%s\\n' \"$a\"; done\n} >> " + logPath + "\n" +
		"case \" $* \" in\n  *\" image inspect \"*) exit 1 ;;\nesac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// sshInvocations returns the recorded argv of every stubbed ssh call, in
// order.
func sshInvocations(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("read ssh log: %v", err)
	}
	var out [][]string
	var cur []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		switch {
		case line == "#ARGV":
			if cur != nil {
				out = append(out, cur)
			}
			cur = []string{}
		case line == "" && cur == nil:
			// trailing newline
		default:
			if cur == nil {
				cur = []string{}
			}
			cur = append(cur, line)
		}
	}
	if cur != nil {
		out = append(out, cur)
	}
	return out
}

func TestEnsureProgramAliasImagePullsOnceThenCaches(t *testing.T) {
	logPath := installRecordingSSH(t)
	svc, _ := newAliasTestService(t)
	ctx := context.Background()

	// First use: inspect fails -> pull runs -> cached.
	dur, pulled, err := svc.ensureProgramAliasImage(ctx, paTestAgent, paTestKey, "mikefarah/yq:4")
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if !pulled {
		t.Fatal("first use did not report a pull")
	}
	if dur <= 0 {
		t.Fatalf("first-run pull duration = %v, want > 0", dur)
	}
	invs := sshInvocations(t, logPath)
	if len(invs) != 2 {
		t.Fatalf("first use made %d docker calls, want 2 (inspect + pull): %v", len(invs), invs)
	}
	if !strings.Contains(strings.Join(invs[0], " "), "image inspect mikefarah/yq:4") {
		t.Fatalf("first call is not an inspect: %v", invs[0])
	}
	if !strings.Contains(strings.Join(invs[1], " "), "docker pull mikefarah/yq:4") {
		t.Fatalf("second call is not a pull: %v", invs[1])
	}
	// The pull runs against the agent's OWN rootless socket.
	if !strings.Contains(strings.Join(invs[1], " "), "DOCKER_HOST=unix:///run/bunker/abc123/docker.sock") {
		t.Fatalf("pull did not target the agent socket: %v", invs[1])
	}

	// Second use: served entirely from the cache — no ssh call at all, which
	// is the observable "second run is cached" property.
	dur2, pulled2, err := svc.ensureProgramAliasImage(ctx, paTestAgent, paTestKey, "mikefarah/yq:4")
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if pulled2 || dur2 != 0 {
		t.Fatalf("second use pulled again: pulled=%v dur=%v", pulled2, dur2)
	}
	if got := len(sshInvocations(t, logPath)); got != 2 {
		t.Fatalf("second use made %d extra docker calls, want 0 (total 2)", got-2)
	}

	// A different image is inspected on its own.
	if _, _, err := svc.ensureProgramAliasImage(ctx, paTestAgent, paTestKey, "ghcr.io/jqlang/jq:1.7.1"); err != nil {
		t.Fatalf("third ensure: %v", err)
	}
	if got := len(sshInvocations(t, logPath)); got != 4 {
		t.Fatalf("distinct image made %d calls, want 2 more (total 4)", got-2)
	}
}

func TestEnsureProgramAliasImageFailsLoudlyOnPullError(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\ncase \" $* \" in\n  *\" image inspect \"*) exit 1 ;;\nesac\necho 'pull access denied' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	svc, _ := newAliasTestService(t)
	_, pulled, err := svc.ensureProgramAliasImage(context.Background(), paTestAgent, paTestKey, "nope/missing:1")
	if err == nil {
		t.Fatal("pull failure was swallowed")
	}
	if !pulled {
		t.Fatal("pull failure not reported as an attempted pull")
	}
	if !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("error does not carry the docker output: %v", err)
	}
	// A failed image is NOT cached, so a retry re-attempts.
	if svc.programAliasImages.Has("nope/missing:1") {
		t.Fatal("failed image was cached as present")
	}
}

// ── ExecAgent integration ───────────────────────────────────────

// normalizeShellText strips POSIX single-quote escaping and collapses
// whitespace, so a test can assert on the intended command text even when it
// crosses two or three shell layers (the ssh remote shell plus the
// container-side sh -c).  Token ORDER is preserved, so a false match would
// require the builder to emit the same tokens in the same order.
func normalizeShellText(s string) string {
	s = strings.NewReplacer(`'`, "", `\`, "").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// execAgentOnce drives the real ExecAgent handler over an httptest server for
// one request, and returns the stream error (nil on success).
func execAgentOnce(t *testing.T, svc *bunkerdService, req *v1.ExecAgentRequest) error {
	t.Helper()
	path, handler := bunkerv1connect.NewBunkerdHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
	stream, err := client.ExecAgent(context.Background(), connect.NewRequest(req))
	if err != nil {
		return err
	}
	for stream.Receive() {
		_ = stream.Msg()
	}
	if err := stream.Err(); err != nil {
		return err
	}
	if err := stream.Close(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func TestExecAgentResolvesProgramAlias(t *testing.T) {
	logPath := installRecordingSSH(t)
	svc, reg := newAliasTestService(t)
	if err := reg.Put(programalias.Alias{
		Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"},
	}); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	if err := svc.tracker.Register(&resource.AgentRecord{
		AgentID:           paTestAgent,
		Status:            "running",
		SshPrivateKeyPath: paTestKey,
		Limits:            &v1.ResourceLimits{CpuQuota: 1.5, MemoryMaxBytes: 1 << 30},
	}); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	if err := execAgentOnce(t, svc, &v1.ExecAgentRequest{
		AgentId: paTestAgent,
		Command: "yq",
		Args:    []string{"--version"},
	}); err != nil {
		t.Fatalf("ExecAgent: %v", err)
	}

	invs := sshInvocations(t, logPath)
	if len(invs) != 3 {
		t.Fatalf("first alias run made %d ssh calls, want 3 (inspect, pull, exec): %v", len(invs), invs)
	}
	// The exec argv crosses two shell layers (the ssh remote shell and the
	// container-side sh -c), so the shim is nested inside quoted strings;
	// normalizing the quote escape makes the intent readable without weakening
	// the assertion (token order is preserved).
	exec := normalizeShellText(strings.Join(invs[2], " "))
	for _, want := range []string{
		"bunker-abc123@localhost",
		"exec /home/bunker-abc123/bin/yq --version",
		"docker run --rm -i --user 0 --memory 1073741824 --cpus 1.5 --pids-limit 512 --network none",
		"-v /home/bunker-abc123:/home/bunker-abc123 -w /home/bunker-abc123",
		"--entrypoint yq mikefarah/yq:4",
	} {
		if !strings.Contains(exec, want) {
			t.Errorf("alias exec argv missing %q:\n%s", want, exec)
		}
	}
	// The agent's own spawn-time envelope reaches the container limits, and no
	// pull is delegated to the agent (the exec command carries none).
	if strings.Contains(exec, "docker pull") {
		t.Errorf("alias exec delegates the pull to the agent:\n%s", exec)
	}

	// Second run: cache hit, no inspect/pull — one ssh call.
	if err := execAgentOnce(t, svc, &v1.ExecAgentRequest{
		AgentId: paTestAgent, Command: "yq", Args: []string{"--version"},
	}); err != nil {
		t.Fatalf("ExecAgent (second): %v", err)
	}
	if got := len(sshInvocations(t, logPath)); got != 4 {
		t.Fatalf("second alias run made %d ssh calls, want 1 more (total 4)", got-3)
	}
}

// TestExecAgentUnregisteredCommandIsUntouched is the byte-identity control:
// with aliases registered but a command that is NOT one, ExecAgent must build
// exactly the pre-GAP-066 command.
func TestExecAgentUnregisteredCommandIsUntouched(t *testing.T) {
	logPath := installRecordingSSH(t)
	svc, reg := newAliasTestService(t)
	if err := reg.Put(programalias.Alias{Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"}}); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	if err := svc.tracker.Register(&resource.AgentRecord{
		AgentID: paTestAgent, Status: "running", SshPrivateKeyPath: paTestKey,
	}); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	if err := execAgentOnce(t, svc, &v1.ExecAgentRequest{
		AgentId: paTestAgent, Command: "docker", Args: []string{"version"},
	}); err != nil {
		t.Fatalf("ExecAgent: %v", err)
	}
	invs := sshInvocations(t, logPath)
	if len(invs) != 1 {
		t.Fatalf("plain command made %d ssh calls, want 1 (no alias step): %v", len(invs), invs)
	}
	got := invs[0] // the stub records argv[1:] (a script's "$@" excludes argv[0])
	want := goldenSSHArgv(goldenShellOff)[1:]
	if len(got) != len(want) {
		t.Fatalf("plain argv length %d != golden %d\n got: %q\nwant: %q", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("plain argv drifted at %d:\n got: %q\nwant: %q", i, got, want)
		}
	}
}

// TestExecAgentExplicitPathBypassesAlias pins the escape hatch: an absolute
// path always runs the native binary, even when its base name is an alias.
func TestExecAgentExplicitPathBypassesAlias(t *testing.T) {
	logPath := installRecordingSSH(t)
	svc, reg := newAliasTestService(t)
	if err := reg.Put(programalias.Alias{Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"}}); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	if err := svc.tracker.Register(&resource.AgentRecord{
		AgentID: paTestAgent, Status: "running", SshPrivateKeyPath: paTestKey,
	}); err != nil {
		t.Fatalf("register agent: %v", err)
	}
	if err := execAgentOnce(t, svc, &v1.ExecAgentRequest{
		AgentId: paTestAgent, Command: "/usr/bin/yq", Args: []string{"--version"},
	}); err != nil {
		t.Fatalf("ExecAgent: %v", err)
	}
	invs := sshInvocations(t, logPath)
	if len(invs) != 1 {
		t.Fatalf("explicit path made %d ssh calls, want 1 (native, no alias): %v", len(invs), invs)
	}
	joined := strings.Join(invs[0], " ")
	// "docker" appears in DOCKER_HOST too, so look for the docker ARGV the
	// alias path would have built instead.
	if strings.Contains(joined, "'docker'") || strings.Contains(joined, "docker run") {
		t.Fatalf("explicit path was aliased:\n%s", joined)
	}
	if !strings.Contains(joined, "/usr/bin/yq") {
		t.Fatalf("explicit path did not run natively:\n%s", joined)
	}
}

// TestExecAgentScriptPathIsUntouchedByAliases proves the containment: a script
// upload is not alias-resolved (it has no single command token), so the alias
// feature cannot silently rewrite a script exec.
func TestExecAgentScriptPathIsUntouchedByAliases(t *testing.T) {
	logPath := installRecordingSSH(t)
	svc, reg := newAliasTestService(t)
	if err := reg.Put(programalias.Alias{Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"}}); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	if err := svc.tracker.Register(&resource.AgentRecord{
		AgentID: paTestAgent, Status: "running", SshPrivateKeyPath: paTestKey,
	}); err != nil {
		t.Fatalf("register agent: %v", err)
	}
	// The command token IS an alias name, but a script payload is present, so
	// the script branch wins.
	if err := execAgentOnce(t, svc, &v1.ExecAgentRequest{
		AgentId: paTestAgent, Command: "yq", ScriptContent: "echo scripted\n",
	}); err != nil {
		t.Fatalf("ExecAgent: %v", err)
	}
	invs := sshInvocations(t, logPath)
	if len(invs) != 1 {
		t.Fatalf("script exec made %d ssh calls, want 1: %v", len(invs), invs)
	}
	joined := strings.Join(invs[0], " ")
	if strings.Contains(joined, "'docker'") {
		t.Fatalf("script exec was alias-resolved:\n%s", joined)
	}
	if !strings.Contains(joined, "EOFSCRIPT") {
		t.Fatalf("script payload was not uploaded:\n%s", joined)
	}
}
