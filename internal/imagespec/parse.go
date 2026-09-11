package imagespec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DefaultBaseImage is the base image every agent uses today; a spec may only
// re-declare one of the allowed bases, never introduce a new source.
const DefaultBaseImage = "docker.io/library/ubuntu:24.04"

// allowedBases is the closed set of base images a spec may name. Everything
// else — private registries, localhost pulls, scratch — is rejected so a spec
// can never change where the agent image comes from beyond this list.
var allowedBases = map[string]bool{
	DefaultBaseImage:                 true,
	"docker.io/library/ubuntu:22.04": true,
	"docker.io/library/debian:12":    true,
	"docker.io/library/debian:11":    true,
}

// tokenAllowed matches a constrained package name/version token. The class
// ranges deliberately exclude EVERY shell metacharacter: whitespace, | & ; < >
// ( ) $ ` \ " ' ! # ? * ~ [ ] { } , and control characters — so chaining,
// substitution, redirection, and globbing are all structurally impossible.
// Allowed: letters, digits, and . + - _ / : @ =
const tokenRe = `^[A-Za-z0-9.+\-_:/@=]+$`

// Parse validates raw JSON bytes into a Spec. It is total: any error means no
// part of the input was accepted.
func Parse(data []byte) (*Spec, error) {
	if len(data) > MaxSpecBytes {
		return nil, fmt.Errorf("spec is %d bytes, exceeds %d byte limit", len(data), MaxSpecBytes)
	}

	// Reject unknown fields so Dockerfile-instruction-shaped inputs ("from",
	// "user", "env", "run", ...) fail loudly instead of being ignored.
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var raw wireImageSpec
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid image spec: %w", err)
	}
	// Only one JSON object per spec — no trailing garbage.
	if dec.More() {
		return nil, fmt.Errorf("invalid image spec: unexpected trailing data")
	}
	return fromWire(&raw)
}

// Default returns the no-op spec: the standard base image with no package
// additions. Agents spawned with it are indistinguishable from today's
// bootstrap image.
func Default() *Spec {
	return &Spec{Base: DefaultBaseImage}
}

// fromWire validates the shared wire shape (JSON and proto both map onto it).
func fromWire(raw *wireImageSpec) (*Spec, error) {
	spec := &Spec{Base: DefaultBaseImage}
	if raw.Base != "" {
		if !allowedBases[raw.Base] {
			return nil, fmt.Errorf("base image %q is not allowed (allowed: %v)", raw.Base, AllowedBases())
		}
		spec.Base = raw.Base
	}

	seen := make(map[PackageManager]bool, len(raw.Packages))
	if len(raw.Packages) > MaxDirectives {
		return nil, fmt.Errorf("too many directives: %d exceeds limit %d", len(raw.Packages), MaxDirectives)
	}
	for _, p := range raw.Packages {
		if !p.Manager.Valid() {
			return nil, fmt.Errorf("unsupported package manager %q (allowed: apt, go, npm)", string(p.Manager))
		}
		if seen[p.Manager] {
			return nil, fmt.Errorf("duplicate directive for package manager %q", string(p.Manager))
		}
		seen[p.Manager] = true
		if len(p.Packages) > MaxPackagesPerDirective {
			return nil, fmt.Errorf("too many packages for %q: %d exceeds limit %d", string(p.Manager), len(p.Packages), MaxPackagesPerDirective)
		}
		d := PackageAdd{Manager: p.Manager, Packages: make([]string, 0, len(p.Packages))}
		for _, name := range p.Packages {
			if err := validateToken(p.Manager, name); err != nil {
				return nil, fmt.Errorf("%s package %q: %w", string(p.Manager), name, err)
			}
			d.Packages = append(d.Packages, name)
		}
		if len(d.Packages) > 0 {
			spec.Packages = append(spec.Packages, d)
		}
	}
	return spec, nil
}

// PackageAddSpec is the JSON wire form of a package-add directive.

// AllowedBases returns the sorted list of allowed base images (for error text
// and docs).
func AllowedBases() []string {
	out := make([]string, 0, len(allowedBases))
	for b := range allowedBases {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// validateToken enforces the constrained token grammar on a single package
// name or name@version / name=version token. Slashes are allowed only for go
// module paths and npm scoped names — never for apt, so absolute paths like
// /var/run/docker.sock or --mount=type=bind,source=/etc,target=/etc cannot
// ride through as "packages" (a mounted path is not a package name).
func validateToken(m PackageManager, tok string) error {
	if tok == "" {
		return fmt.Errorf("empty package token")
	}
	if len(tok) > MaxTokenBytes {
		return fmt.Errorf("token exceeds %d bytes", MaxTokenBytes)
	}
	// Hand-rolled character check (equivalent to tokenRe): every byte must be
	// in [A-Za-z0-9] or one of . + - _ : / @ =
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '+' || c == '-' || c == '_' || c == ':' || c == '/' || c == '@' || c == '=':
		default:
			return fmt.Errorf("illegal character %q at position %d: package tokens may contain only letters, digits, and . + - _ : / @ =", string(c), i+1)
		}
	}
	if m == ManagerAPT && strings.Contains(tok, "/") {
		return fmt.Errorf("apt package names may not contain %q", "/")
	}
	return nil
}

// CacheKey returns the deterministic SHA-256 hex digest of the canonical spec.
// Two specs with identical (normalized) content share one key regardless of
// JSON field or directive order; any content change yields a different key.
func (s *Spec) CacheKey() string {
	var b strings.Builder
	b.WriteString("base=")
	b.WriteString(s.Base)
	b.WriteString("\n")
	norm := make([]PackageAdd, len(s.Packages))
	copy(norm, s.Packages)
	sort.Slice(norm, func(i, j int) bool { return norm[i].Manager < norm[j].Manager })
	for _, d := range norm {
		b.WriteString(string(d.Manager))
		b.WriteString("=")
		pkgs := append([]string(nil), d.Packages...)
		sort.Strings(pkgs)
		for _, p := range pkgs {
			b.WriteString(p)
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Hash parses and canonicalizes raw spec bytes into the cache key.
func Hash(data []byte) (string, error) {
	spec, err := Parse(data)
	if err != nil {
		return "", err
	}
	return spec.CacheKey(), nil
}

// Dockerfile renders the validated spec into the exact Dockerfile the builder
// writes. The grammar guarantees every line below is builder-generated — the
// caller's bytes never reach this text.
func (s *Spec) Dockerfile() string {
	var b strings.Builder
	b.WriteString("FROM ")
	b.WriteString(s.Base)
	b.WriteString("\n")
	for _, d := range s.Packages {
		switch d.Manager {
		case ManagerAPT:
			b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends")
			for _, p := range d.Packages {
				b.WriteString(" ")
				b.WriteString(p)
			}
			b.WriteString(" && rm -rf /var/lib/apt/lists/*\n")
		case ManagerGo:
			for _, p := range d.Packages {
				b.WriteString("RUN go install ")
				b.WriteString(p)
				b.WriteString("\n")
			}
		case ManagerNPM:
			b.WriteString("RUN npm install -g")
			for _, p := range d.Packages {
				b.WriteString(" ")
				b.WriteString(p)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
