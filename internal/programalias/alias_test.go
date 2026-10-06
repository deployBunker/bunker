package programalias

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"simple", "yq", false},
		{"dotted", "go1.26", false},
		{"plus", "c++", false},
		{"underscore", "my_tool", false},
		{"dash", "jq-1", false},
		{"max length", strings.Repeat("a", NameMaxLen), false},
		{"empty", "", true},
		{"too long", strings.Repeat("a", NameMaxLen+1), true},
		{"leading dash", "-rm", true},
		{"path", "/usr/bin/yq", true},
		{"relative path", "./yq", true},
		{"space", "yq v4", true},
		{"semicolon", "yq;rm", true},
		{"backtick", "yq`id`", true},
		{"dollar", "$(id)", true},
		{"newline", "yq\nid", true},
		{"quote", "y'q", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateName(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateName(%q) = nil, want error", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateName(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}

func TestValidateImage(t *testing.T) {
	valid := []string{
		"yq",
		"mikefarah/yq",
		"mikefarah/yq:4",
		"mikefarah/yq:4.44.3",
		"ghcr.io/jqlang/jq:1.7.1",
		"registry.example.com:5000/team/tool:v1",
		"alpine:3.20",
		"golang:1.26-alpine",
		"debian@sha256:" + strings.Repeat("a", 64),
	}
	for _, img := range valid {
		if err := ValidateImage(img); err != nil {
			t.Errorf("ValidateImage(%q) = %v, want nil", img, err)
		}
	}
	invalid := []string{
		"",
		"yq; rm -rf /",
		"yq --rm",
		"$(id)",
		"yq`id`",
		"../escape",
		"yq\t4",
		"yq\n4",
		"-flag",
		"yq:x:y:z",
	}
	for _, img := range invalid {
		if err := ValidateImage(img); err == nil {
			t.Errorf("ValidateImage(%q) = nil, want error", img)
		}
	}
}

func TestValidateAliasEntrypointTokens(t *testing.T) {
	if err := ValidateAlias(Alias{Name: "yq", Image: "yq", Entrypoint: []string{"sh", "-lc"}}); err != nil {
		t.Fatalf("valid entrypoint refused: %v", err)
	}
	if err := ValidateAlias(Alias{Name: "yq", Image: "yq", Entrypoint: []string{"  "}}); !errors.Is(err, ErrArgInvalid) {
		t.Fatalf("blank entrypoint token: got %v, want ErrArgInvalid", err)
	}
	if err := ValidateAlias(Alias{Name: "yq", Image: "yq", Entrypoint: []string{"a\x00b"}}); !errors.Is(err, ErrArgInvalid) {
		t.Fatalf("NUL entrypoint token: got %v, want ErrArgInvalid", err)
	}
}

const testHome = "/home/bunker-abc123"

func TestValidateMountHomeOnly(t *testing.T) {
	cases := []struct {
		name    string
		mount   Mount
		wantErr error
	}{
		{
			name:  "home itself",
			mount: Mount{Host: testHome},
		},
		{
			name:  "inside home",
			mount: Mount{Host: testHome + "/src", Container: testHome + "/src", ReadOnly: true},
		},
		{
			name:  "deep inside home",
			mount: Mount{Host: testHome + "/a/b/c"},
		},
		{
			name:    "outside the agent-home root",
			mount:   Mount{Host: "/etc"},
			wantErr: ErrMountOutsideHomeRoot,
		},
		{
			name:    "root",
			mount:   Mount{Host: "/"},
			wantErr: ErrMountOutsideHomeRoot,
		},
		{
			name:    "sibling prefix is not inside",
			mount:   Mount{Host: "/home/bunker-abc1234"},
			wantErr: ErrMountNotUnderHome,
		},
		{
			name:    "a different agent's home",
			mount:   Mount{Host: "/home/bunker-other/file"},
			wantErr: ErrMountNotUnderHome,
		},
		{
			name:    "parent traversal from home",
			mount:   Mount{Host: testHome + "/../other"},
			wantErr: ErrMountOutsideHomeRoot,
		},
		{
			name:    "parent traversal to root",
			mount:   Mount{Host: testHome + "/../../etc"},
			wantErr: ErrMountOutsideHomeRoot,
		},
		{
			name:    "relative host",
			mount:   Mount{Host: "src"},
			wantErr: ErrMountHostRelative,
		},
		{
			name:    "empty host",
			mount:   Mount{Host: ""},
			wantErr: ErrMountHostRequired,
		},
		{
			name:    "relative container",
			mount:   Mount{Host: testHome + "/src", Container: "src"},
			wantErr: ErrMountContainerRelative,
		},
		{
			name:    "docker socket by name",
			mount:   Mount{Host: "/var/run/docker.sock"},
			wantErr: ErrMountDockerSock,
		},
		{
			name:    "agent runtime dir (carries the socket)",
			mount:   Mount{Host: "/run/bunker/abc123"},
			wantErr: ErrMountDockerSock,
		},
		{
			name:    "agent runtime socket file",
			mount:   Mount{Host: "/run/bunker/abc123/docker.sock"},
			wantErr: ErrMountDockerSock,
		},
		{
			name:    "runtime dir as container side",
			mount:   Mount{Host: testHome + "/src", Container: "/run/bunker/abc123"},
			wantErr: ErrMountDockerSock,
		},
		{
			name:    "home inside a bad prefix",
			mount:   Mount{Host: filepath.Join(testHome, "x")},
			wantErr: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMount(tc.mount, testHome)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateMount(%+v) = %v, want nil", tc.mount, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateMount(%+v) = nil, want %v", tc.mount, tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateMount(%+v) = %v, want %v", tc.mount, err, tc.wantErr)
			}
		})
	}
}

// TestValidateMountRejectsNonHomeThroughBuild is the end-to-end shape of the
// HOME-ONLY rule: an alias carrying a mount outside the target agent's home
// cannot produce an argv at all, so no exec can be built around it.
func TestValidateMountRejectsNonHomeThroughBuild(t *testing.T) {
	// An absolute path outside the agent-home ROOT is refused even earlier (at
	// registration / shape time); a path inside ANOTHER agent's home passes the
	// shape check and must be refused by the per-agent check.
	for _, host := range []string{"/etc", "/home/bunker-other/file"} {
		a := Alias{Name: "yq", Image: "mikefarah/yq:4", Entrypoint: []string{"yq"},
			Mounts: []Mount{{Host: host}}}
		if _, err := BuildDockerArgv(a, testHome, nil, Limits{}, nil); err == nil {
			t.Fatalf("BuildDockerArgv accepted mount %q", host)
		}
		if _, err := Shim(a, testHome, Limits{}, nil); err == nil {
			t.Fatalf("Shim accepted mount %q", host)
		}
		if _, err := RemoteExecScript(a, testHome, nil, Limits{}, nil); err == nil {
			t.Fatalf("RemoteExecScript accepted mount %q", host)
		}
	}
	// The precise exec-time refusal is still distinguishable from the shape one.
	a := Alias{Name: "yq", Image: "yq", Mounts: []Mount{{Host: "/home/bunker-other/x"}}}
	if _, err := BuildDockerArgv(a, testHome, nil, Limits{}, nil); !errors.Is(err, ErrMountNotUnderHome) {
		t.Fatalf("other agent's home: got %v, want ErrMountNotUnderHome", err)
	}
}

// TestValidateMountShapeAtRegistration proves the registration-time half of the
// rule: a mount that can never be valid for ANY agent is refused by
// ValidateAlias (and therefore by Registry.Put) before any agent is involved,
// while a mount inside a (possibly other) agent home is stored and judged
// precisely at exec time.
func TestValidateMountShapeAtRegistration(t *testing.T) {
	if err := ValidateAlias(Alias{Name: "yq", Image: "yq", Mounts: []Mount{{Host: "relative"}}}); !errors.Is(err, ErrMountHostRelative) {
		t.Fatalf("relative mount accepted at registration: %v", err)
	}
	if err := ValidateAlias(Alias{Name: "yq", Image: "yq", Mounts: []Mount{{Host: "/run/bunker/abc"}}}); !errors.Is(err, ErrMountDockerSock) {
		t.Fatalf("docker-socket mount accepted at registration: %v", err)
	}
	if err := ValidateAlias(Alias{Name: "yq", Image: "yq", Mounts: []Mount{{Host: "/etc"}}}); !errors.Is(err, ErrMountOutsideHomeRoot) {
		t.Fatalf("non-home-root mount accepted at registration: %v", err)
	}
	if err := ValidateAlias(Alias{Name: "yq", Image: "yq", Mounts: []Mount{{Host: "/home/bunker-someone/data"}}}); err != nil {
		t.Fatalf("agent-home mount should register (checked per-agent at exec time): %v", err)
	}
	if err := ValidateAlias(Alias{Name: "yq", Image: "yq", Mounts: []Mount{{Host: "/var/run/docker.sock"}}}); !errors.Is(err, ErrMountDockerSock) {
		t.Fatalf("host docker socket accepted at registration: %v", err)
	}
}
