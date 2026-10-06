package programalias

import (
	"strings"
	"testing"
)

const shimHome = "/home/bunker-abc123"

func yqAlias() Alias {
	return Alias{
		Name:        "yq",
		Image:       "mikefarah/yq:4",
		Entrypoint:  []string{"yq"},
		Description: "YAML filter",
	}
}

// TestBuildDockerArgvShape pins the complete container invocation: the flags
// that make it safe, the mounts it is allowed to have, and the exact position
// of the image / entrypoint / caller args.
func TestBuildDockerArgvShape(t *testing.T) {
	limits := Limits{MemoryBytes: 4 << 30, CPUs: 3.5, Pids: 128}
	argv, err := BuildDockerArgv(yqAlias(), shimHome, []string{"--version"}, limits, nil)
	if err != nil {
		t.Fatalf("BuildDockerArgv: %v", err)
	}
	got := strings.Join(argv, " ")
	want := "docker run --rm -i --user 0 --memory 4294967296 --cpus 3.5 --pids-limit 128 " +
		"--network none " +
		"-v /home/bunker-abc123:/home/bunker-abc123 -w /home/bunker-abc123 " +
		"--entrypoint yq mikefarah/yq:4 --version"
	if got != want {
		t.Fatalf("argv =\n  %s\nwant\n  %s", got, want)
	}
}

// TestBuildDockerArgvEntrypointIsDockerFlag proves the alias's Entrypoint maps
// onto docker's OWN `--entrypoint` flag (before the image), not onto a command
// vector appended after the image: the latter would run a program twice on an
// image whose entrypoint is already that program.
func TestBuildDockerArgvEntrypointIsDockerFlag(t *testing.T) {
	a := Alias{Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"}}
	argv, err := BuildDockerArgv(a, shimHome, []string{"--version"}, Limits{}, nil)
	if err != nil {
		t.Fatalf("BuildDockerArgv: %v", err)
	}
	ep := -1
	img := -1
	for i, tok := range argv {
		switch tok {
		case "--entrypoint":
			ep = i
		case "mikefarah/yq:4":
			img = i
		}
	}
	if ep < 0 {
		t.Fatalf("no --entrypoint flag in argv: %v", argv)
	}
	if ep >= img {
		t.Fatalf("--entrypoint must precede the image reference: %v", argv)
	}
	if argv[ep+1] != "yq" {
		t.Fatalf("--entrypoint value = %q, want yq (%v)", argv[ep+1], argv)
	}
	if argv[len(argv)-1] != "--version" {
		t.Fatalf("caller args must come last: %v", argv)
	}
}

// TestBuildDockerArgvNeverMountsDockerSocket is the safety conjunction: no
// argv built from a normal alias can name the agent runtime directory (which
// carries docker.sock) or grant extra privilege.
func TestBuildDockerArgvNeverMountsDockerSocket(t *testing.T) {
	argv, err := BuildDockerArgv(yqAlias(), shimHome, nil, Limits{}, nil)
	if err != nil {
		t.Fatalf("BuildDockerArgv: %v", err)
	}
	joined := strings.Join(argv, " ")
	for _, forbidden := range []string{"/run/bunker", "docker.sock", "--privileged", "--pid host", "--pid=host", "--net=host", "--network host", "/var/run"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("argv must not contain %q: %s", forbidden, joined)
		}
	}
}

// TestBuildDockerArgvDefaultsLimits pins that an unspecified envelope becomes
// the modest default rather than "unlimited".
func TestBuildDockerArgvDefaultsLimits(t *testing.T) {
	argv, err := BuildDockerArgv(yqAlias(), shimHome, nil, Limits{}, nil)
	if err != nil {
		t.Fatalf("BuildDockerArgv: %v", err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--memory 2147483648", "--cpus 2", "--pids-limit 512"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing default %q: %s", want, joined)
		}
	}
}

// TestBuildDockerArgvNetworkOptIn proves the network flag is the only way to
// get egress, and that its absence is an explicit `--network none`.
func TestBuildDockerArgvNetworkOptIn(t *testing.T) {
	a := yqAlias()
	a.Network = true
	argv, err := BuildDockerArgv(a, shimHome, nil, Limits{}, nil)
	if err != nil {
		t.Fatalf("BuildDockerArgv: %v", err)
	}
	if strings.Contains(strings.Join(argv, " "), "--network") {
		t.Fatalf("network=true still emitted a --network flag: %v", argv)
	}
}

func TestBuildDockerArgvEntrypointAndMounts(t *testing.T) {
	a := Alias{
		Name:       "tool",
		Image:      "alpine:3.20",
		Entrypoint: []string{"sh", "-lc"},
		Mounts: []Mount{
			{Host: shimHome + "/src", ReadOnly: true},
			{Host: shimHome + "/out", Container: shimHome + "/out"},
		},
	}
	argv, err := BuildDockerArgv(a, shimHome, []string{"echo hi"}, Limits{}, nil)
	if err != nil {
		t.Fatalf("BuildDockerArgv: %v", err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"-v " + shimHome + "/src:" + shimHome + "/src:ro",
		"-v " + shimHome + "/out:" + shimHome + "/out ",
		"--entrypoint sh alpine:3.20 -lc echo hi",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %s", want, joined)
		}
	}
	if strings.Count(joined, shimHome+"/src") != 2 {
		t.Errorf("read-only mount should appear once as host and once as container: %s", joined)
	}
}

// TestBuildDockerArgvEmptyEntrypointUsesImageEntrypoint pins the documented
// "entrypoint empty = the image's own ENTRYPOINT/CMD" contract.
func TestBuildDockerArgvEmptyEntrypointUsesImageEntrypoint(t *testing.T) {
	a := Alias{Name: "yq", Image: "mikefarah/yq:4"}
	argv, err := BuildDockerArgv(a, shimHome, []string{"--version"}, Limits{}, nil)
	if err != nil {
		t.Fatalf("BuildDockerArgv: %v", err)
	}
	if strings.Contains(strings.Join(argv, " "), "--entrypoint") {
		t.Fatalf("empty entrypoint must not emit --entrypoint: %v", argv)
	}
	if want := "mikefarah/yq:4 --version"; !strings.HasSuffix(strings.Join(argv, " "), want) {
		t.Fatalf("argv = %s, want suffix %q", strings.Join(argv, " "), want)
	}
}

func TestBuildDockerArgvContainerEnv(t *testing.T) {
	argv, err := BuildDockerArgv(yqAlias(), shimHome, nil, Limits{}, []string{"BUNKER_SANDBOX=1"})
	if err != nil {
		t.Fatalf("BuildDockerArgv: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-e BUNKER_SANDBOX=1") {
		t.Fatalf("container env not injected: %s", joined)
	}
	// Malformed env is refused (it is argv, not a shell string).
	if _, err := BuildDockerArgv(yqAlias(), shimHome, nil, Limits{}, []string{"NOEQUALS"}); err == nil {
		t.Fatal("malformed container env accepted")
	}
}

func TestBuildDockerArgvRequiresAbsoluteHome(t *testing.T) {
	if _, err := BuildDockerArgv(yqAlias(), "relative/home", nil, Limits{}, nil); err == nil {
		t.Fatal("relative home accepted")
	}
	if _, err := BuildDockerArgv(yqAlias(), "", nil, Limits{}, nil); err == nil {
		t.Fatal("empty home accepted")
	}
}

// TestShimRendersExecAndArgsContract pins the shim's essential properties: a
// POSIX shebang, a revision marker, an exec (not a call — no shell may sit
// between docker and the caller), the docker argv, and a QUOTED "$@" so
// arguments with spaces or quotes cross intact.
func TestShimRendersExecAndArgsContract(t *testing.T) {
	shim, err := Shim(yqAlias(), shimHome, Limits{}, nil)
	if err != nil {
		t.Fatalf("Shim: %v", err)
	}
	if !strings.HasPrefix(shim, "#!/bin/sh\n") {
		t.Fatalf("shim does not start with a shebang: %q", shim)
	}
	if !strings.Contains(shim, "\n"+shimRevMarker) {
		t.Fatalf("shim carries no revision marker: %q", shim)
	}
	if !strings.Contains(shim, "\nexec ") {
		t.Fatalf("shim does not exec: %q", shim)
	}
	if !strings.HasSuffix(shim, ` "$@"`+"\n") {
		t.Fatalf("shim does not end with a quoted \"$@\": %q", shim)
	}
	if !strings.Contains(shim, "'docker' 'run'") {
		t.Fatalf("shim does not exec the docker argv: %q", shim)
	}
	// The DOCKER_HOST fallback must be the agent's OWN socket path shape, never
	// a bare host daemon.
	if !strings.Contains(shim, "/run/bunker/") {
		t.Fatalf("shim has no per-agent DOCKER_HOST fallback: %q", shim)
	}
	if _, err := Shim(yqAlias(), shimHome, Limits{}, []string{"BAD"}); err == nil {
		t.Fatal("Shim accepted a malformed container env")
	}
}

// TestShimRevChangesWithContent proves the revision marker is content-derived:
// a changed image (or entrypoint, or limit) yields a new marker, so the
// install step reinstalls and an unchanged alias is left alone.
func TestShimRevChangesWithContent(t *testing.T) {
	base, err := Shim(yqAlias(), shimHome, Limits{}, nil)
	if err != nil {
		t.Fatalf("Shim: %v", err)
	}
	revBase := shimRevOf(base)
	if revBase == "" {
		t.Fatal("no revision marker")
	}

	updated := yqAlias()
	updated.Image = "mikefarah/yq:4.44.3"
	other, err := Shim(updated, shimHome, Limits{}, nil)
	if err != nil {
		t.Fatalf("Shim: %v", err)
	}
	if shimRevOf(other) == revBase {
		t.Fatal("revision did not change when the image changed")
	}

	limited, err := Shim(yqAlias(), shimHome, Limits{MemoryBytes: 1 << 30}, nil)
	if err != nil {
		t.Fatalf("Shim: %v", err)
	}
	if shimRevOf(limited) == revBase {
		t.Fatal("revision did not change when the limits changed")
	}

	// Deterministic: the same alias renders the same revision.
	again, err := Shim(yqAlias(), shimHome, Limits{}, nil)
	if err != nil {
		t.Fatalf("Shim: %v", err)
	}
	if shimRevOf(again) != revBase {
		t.Fatal("revision is not deterministic across renders")
	}
}

// TestRemoteExecScriptIsIdempotentInstallPlusExec pins the script the daemon
// runs: create ~/bin, install the shim ONLY when the revision marker is
// absent, then exec the shim with quoted args — and nothing else.  The
// idempotence is what makes the second run of an alias a pure cache hit.
func TestRemoteExecScriptIsIdempotentInstallPlusExec(t *testing.T) {
	args := []string{"--version", "a b", "it's", "$HOME", ";rm -rf /"}
	script, err := RemoteExecScript(yqAlias(), shimHome, args, Limits{}, nil)
	if err != nil {
		t.Fatalf("RemoteExecScript: %v", err)
	}
	for _, want := range []string{
		"mkdir -p '/home/bunker-abc123/bin'",
		"grep -qF '" + shimRevMarker,
		"cat > '/home/bunker-abc123/bin/yq' <<'BUNKER_ALIAS_EOF'",
		"BUNKER_ALIAS_EOF",
		"chmod 755 '/home/bunker-abc123/bin/yq'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if !strings.Contains(script, "\nexec '/home/bunker-abc123/bin/yq'") {
		t.Fatalf("script does not exec the shim:\n%s", script)
	}
	// Every caller argument must be single-quoted, including the nasty ones.
	for _, want := range []string{"'--version'", "'a b'", `'it'\''s'`, "'$HOME'", "';rm -rf /'"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing quoted arg %q:\n%s", want, script)
		}
	}
	// The conditional install must be the ONLY write to disk.
	if n := strings.Count(script, "cat > "); n != 1 {
		t.Errorf("expected exactly one shim write, found %d:\n%s", n, script)
	}
}

// TestRemoteExecScriptSecondRunShape proves the second-run path is a no-op
// install: the grep guard is the whole mechanism, so a correct shim on disk is
// never rewritten.
func TestRemoteExecScriptSecondRunShape(t *testing.T) {
	shim, err := Shim(yqAlias(), shimHome, Limits{}, nil)
	if err != nil {
		t.Fatalf("Shim: %v", err)
	}
	script, err := RemoteExecScript(yqAlias(), shimHome, nil, Limits{}, nil)
	if err != nil {
		t.Fatalf("RemoteExecScript: %v", err)
	}
	marker := shimRevOf(shim)
	if marker == "" || !strings.Contains(script, marker) {
		t.Fatalf("script guard does not carry the shim revision %q:\n%s", marker, script)
	}
}

func TestRawExecArgvMatchesBuildDockerArgv(t *testing.T) {
	raw, err := RawExecArgv(yqAlias(), shimHome, []string{"--version"}, Limits{}, nil)
	if err != nil {
		t.Fatalf("RawExecArgv: %v", err)
	}
	built, err := BuildDockerArgv(yqAlias(), shimHome, []string{"--version"}, Limits{}, nil)
	if err != nil {
		t.Fatalf("BuildDockerArgv: %v", err)
	}
	if len(raw) != len(built) {
		t.Fatalf("raw argv len %d != built len %d", len(raw), len(built))
	}
	for i := range raw {
		if raw[i] != built[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, raw[i], built[i])
		}
	}
}
