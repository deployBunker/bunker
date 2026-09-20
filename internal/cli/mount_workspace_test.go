package cli

// Tests for the workspace identity check (GAP-104).
//
// The point of this check is that aiming a mount at the WRONG tree stops being
// silent. These tests pin the two halves that make it safe: it refuses only for
// a STATED expectation (never for the absence of one), and it accepts the short
// "owner/repo" form an operator actually types.

import (
	"strings"
	"testing"
)

func TestWorkspaceIdentity_Describe(t *testing.T) {
	cases := []struct {
		name string
		id   WorkspaceIdentity
		want []string
	}{
		{
			name: "repo with remote",
			id: WorkspaceIdentity{
				RemotePath: "/home/bunker-x/proj",
				GitRemote:  "https://github.com/deployBunker/bunker.git",
				GitHead:    "3438b6b1234567890",
				GitBranch:  "main",
				IsRepo:     true,
			},
			want: []string{"deployBunker/bunker", "main", "3438b6b1"},
		},
		{
			name: "repo without remote",
			id: WorkspaceIdentity{
				RemotePath: "/tmp/x",
				GitHead:    "abcdef1234567890",
				GitBranch:  "main",
				IsRepo:     true,
			},
			want: []string{"(no origin)", "main"},
		},
		{
			name: "not a repo says so rather than claiming a commit",
			id:   WorkspaceIdentity{RemotePath: "/tmp/empty"},
			want: []string{"/tmp/empty", "not a git work tree"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.id.Describe()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("Describe() = %q, want it to contain %q", got, w)
				}
			}
		})
	}
}

// TestWorkspaceIdentity_EmptyExpectationNeverRefuses is the safety property: a
// check that refused when no expectation was stated would break every ordinary
// mount. Refusal must be earned by a stated expectation.
func TestWorkspaceIdentity_EmptyExpectationNeverRefuses(t *testing.T) {
	ids := []WorkspaceIdentity{
		{RemotePath: "/a", IsRepo: true, GitRemote: "https://github.com/x/y.git", GitHead: "a", GitBranch: "main"},
		{RemotePath: "/b"}, // not even a repo
	}
	for _, id := range ids {
		if !id.MatchesExpected("") {
			t.Errorf("an empty expectation must always match, but %s refused", id.Describe())
		}
	}
}

func TestWorkspaceIdentity_MatchesExpected(t *testing.T) {
	id := WorkspaceIdentity{
		RemotePath: "/home/bunker-x/bunker",
		GitRemote:  "https://github.com/deployBunker/bunker.git",
		GitHead:    "3438b6b1",
		GitBranch:  "main",
		IsRepo:     true,
	}
	cases := []struct {
		name   string
		expect string
		want   bool
	}{
		{"full URL", "https://github.com/deployBunker/bunker.git", true},
		{"full URL without .git", "https://github.com/deployBunker/bunker", true},
		{"short owner/repo", "deployBunker/bunker", true},
		{"short with .git", "deployBunker/bunker.git", true},
		{"ssh form suffix", "git@github.com:deployBunker/bunker.git", false},
		{"different repo", "deployBunker/other", false},
		{"different owner", "someoneelse/bunker", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := id.MatchesExpected(tc.expect); got != tc.want {
				t.Errorf("MatchesExpected(%q) = %v, want %v", tc.expect, got, tc.want)
			}
		})
	}
}

// TestWorkspaceIdentity_NonRepoNeverMatchesAStatedExpectation: a directory that
// is not a repo cannot satisfy "this is project X", so it must refuse rather
// than pass because both sides are empty.
func TestWorkspaceIdentity_NonRepoNeverMatchesAStatedExpectation(t *testing.T) {
	id := WorkspaceIdentity{RemotePath: "/tmp/not-a-repo"}
	if id.MatchesExpected("deployBunker/bunker") {
		t.Fatal("a non-repo workspace matched a stated expectation; the empty-tree case must refuse")
	}
}

// TestWorkspaceIdentity_RepoWithoutOriginRefusesStatedExpectation: a local repo
// with no origin cannot be proven to be the expected project.
func TestWorkspaceIdentity_RepoWithoutOriginRefusesStatedExpectation(t *testing.T) {
	id := WorkspaceIdentity{RemotePath: "/tmp/local", IsRepo: true, GitHead: "abc", GitBranch: "main"}
	if id.MatchesExpected("deployBunker/bunker") {
		t.Fatal("a repo with no origin matched a stated expectation; it cannot be proven")
	}
}

// TestShellQuote guards the one place a remote path crosses into a shell.
func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/home/x/proj", "'/home/x/proj'"},
		{"/home/x/my proj", "'/home/x/my proj'"},
		{"/home/x/it's", `'/home/x/it'\''s'`},
		{"/tmp/$(rm -rf /)", "'/tmp/$(rm -rf /)'"},
		{"/tmp/`whoami`", "'/tmp/`whoami`'"},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
