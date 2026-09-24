package imagespec

import "testing"

// TestDockerfile_Golden pins the EXACT Dockerfile bytes for every registered
// manager. The expected strings were captured from the pre-registry
// implementation at d1b0654 (hardcoded switch in parse.go Dockerfile) BEFORE
// the ManagerDef refactor, so this test proves the registry renders
// byte-identically — including degenerate inputs the parser would never
// produce (fromWire drops empty package lists).
//
// A registry row that changes rendering for a shipped manager fails here
// first; update the golden bytes deliberately, never silently.
func TestDockerfile_Golden(t *testing.T) {
	tests := []struct {
		name string
		spec *Spec
		want string
	}{
		{
			name: "base only",
			spec: &Spec{Base: DefaultBaseImage},
			want: "FROM docker.io/library/ubuntu:24.04\n",
		},
		{
			name: "apt multi",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerAPT, Packages: []string{"jq", "curl=8.5.0-2ubuntu10"}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN apt-get update && apt-get install -y --no-install-recommends jq curl=8.5.0-2ubuntu10 && rm -rf /var/lib/apt/lists/*\n",
		},
		{
			name: "go multi",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@v0.17.0", "honnef.co/go/tools/cmd/staticcheck@v0.5.1"}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN go install golang.org/x/tools/gopls@v0.17.0\nRUN go install honnef.co/go/tools/cmd/staticcheck@v0.5.1\n",
		},
		{
			name: "npm multi",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerNPM, Packages: []string{"typescript@5.6.3", "@types/node@20.14.0"}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN npm install -g typescript@5.6.3 @types/node@20.14.0\n",
		},
		{
			name: "all three in declaration order",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerAPT, Packages: []string{"jq", "curl"}}, {Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@v0.17.0"}}, {Manager: ManagerNPM, Packages: []string{"typescript@5.6.3"}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN apt-get update && apt-get install -y --no-install-recommends jq curl && rm -rf /var/lib/apt/lists/*\nRUN go install golang.org/x/tools/gopls@v0.17.0\nRUN npm install -g typescript@5.6.3\n",
		},
		{
			name: "empty package lists degenerate",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerAPT, Packages: []string{}}, {Manager: ManagerGo, Packages: []string{}}, {Manager: ManagerNPM, Packages: []string{}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN apt-get update && apt-get install -y --no-install-recommends && rm -rf /var/lib/apt/lists/*\nRUN npm install -g\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.spec.Dockerfile()
			if got != tt.want {
				t.Errorf("Dockerfile() byte drift for %q:\ngot  %q\nwant %q", tt.name, got, tt.want)
			}
		})
	}
}
