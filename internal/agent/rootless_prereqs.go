// Multi-strategy prerequisite installation for the rootless Docker runtime.
//
// # WHY THIS FILE EXISTS
//
// Rootless Docker on a fresh host needs pieces a minimal image does not ship:
// a setuid newuidmap/newgidmap (the `uidmap` package), fuse-overlayfs,
// slirp4netns, and a working systemd --user session bus (dbus-user-session).
// The install path used to ASSUME all four were present: installRootlessDocker
// went straight to downloading https://get.docker.com/rootless and running it.
// On a bare host the first thing an operator saw was a failure with no
// diagnosis path, and the fix was a manual apt-get. That is exactly what
// happened on a fresh Hetzner box during the remote-bunker round: the first
// spawn died because newuidmap was missing.
//
// WHAT THIS ADDS
//
//  1. ORDERED STRATEGIES PER DEPENDENCY, with the winner reported. Each
//     prerequisite is satisfied by the first strategy that works, tried in
//     this order:
//     present   — already on the host (the cheap, common case)
//     package   — the OS package manager, per distribution family
//     artifact  — download a prebuilt static binary and install it on PATH
//     source    — compile from source, when a build toolchain is present
//     A dependency is never "assumed"; either a probe finds it, or a strategy
//     installs it, or the outcome says so plainly.
//
//  2. DISTRIBUTION DETECTION from /etc/os-release (ID and ID_LIKE), mapped to
//     a package-manager family, so package names are right per family rather
//     than correct on Debian and wrong everywhere else.
//
//  3. A PLAN (dry-run) MODE. Callers can render exactly what would run, which
//     the install path previously had no concept of.
//
//  4. REQUIRED vs OPTIONAL. A missing optional dependency warns and continues;
//     a missing required one fails with every strategy that was tried and why
//     each was unavailable, instead of a bare exec error.
//
// Everything here is a pure function of its Options, with the runners
// injectable, so tests never touch host state (the house pattern: see
// rootHostRunner and userManagerRunner in rootless.go).
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/deployBunker/bunker/internal/supplychain"
)

// ─────────────────────────────────────────────────────────────────────────────
// Distribution detection
// ─────────────────────────────────────────────────────────────────────────────

// pkgFamily is the package-manager family a distribution belongs to. Package
// NAMES differ between families, so every prerequisite carries one name per
// family rather than one name overall.
type pkgFamily string

const (
	familyDebian  pkgFamily = "debian"  // apt-get  (debian, ubuntu, ...)
	familyRHEL    pkgFamily = "rhel"    // dnf/yum  (fedora, rhel, centos, rocky, alma)
	familyAlpine  pkgFamily = "alpine"  // apk      (alpine)
	familyArch    pkgFamily = "arch"    // pacman   (arch, endeavouros, manjaro)
	familySUSE    pkgFamily = "suse"    // zypper   (opensuse, sles)
	familyUnknown pkgFamily = "unknown" // no package strategy available
)

// osReleasePath is where a distribution describes itself. It is a field of
// PrereqOptions so a test can point detection at a fixture.
const osReleasePath = "/etc/os-release"

// depReadFile is a seam over os.ReadFile so tests never read the real host.
var depReadFile = os.ReadFile

// depLookPath is a seam over exec.LookPath so tests never depend on the host
// PATH. It is a variable, not a call, for the same reason.
var depLookPath = exec.LookPath

// detectFamily reads /etc/os-release and maps it to a package family. An
// unreadable or unrecognised file yields familyUnknown, which is not an
// error: it only costs the package strategy.
func detectFamily(ctx context.Context, opts PrereqOptions) pkgFamily {
	path := opts.OSReleasePath
	if path == "" {
		path = osReleasePath
	}
	data, err := depReadFile(path)
	if err != nil {
		return familyUnknown
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}

	// ID is the primary signal; ID_LIKE covers derivatives (linuxmint is
	// ubuntu-like, rocky is rhel-like, and so on).
	haystack := strings.ToLower(fields["ID"] + " " + fields["ID_LIKE"])
	switch {
	case containsAny(haystack, "debian", "ubuntu"):
		return familyDebian
	case containsAny(haystack, "fedora", "rhel", "centos", "rocky", "almalinux"):
		return familyRHEL
	case containsAny(haystack, "alpine"):
		return familyAlpine
	case containsAny(haystack, "arch", "manjaro", "endeavouros"):
		return familyArch
	case containsAny(haystack, "opensuse", "suse", "sles"):
		return familySUSE
	}
	return familyUnknown
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// Strategies
// ─────────────────────────────────────────────────────────────────────────────

// prereqStrategy names the way a dependency was satisfied. The value is
// reported to the operator, because "it worked" is less useful than "it worked
// because the distribution already had it".
type prereqStrategy string

const (
	strategyPresent  prereqStrategy = "present"  // already available
	strategyPackage  prereqStrategy = "package"  // installed via the OS package manager
	strategyArtifact prereqStrategy = "artifact" // prebuilt binary downloaded
	strategySource   prereqStrategy = "source"   // compiled from source
	strategySkipped  prereqStrategy = "skipped"  // optional and unavailable
	strategyFailed   prereqStrategy = "failed"   // required and unavailable
)

// artifact describes a prebuilt static binary that can stand in for a package
// when the package strategy is unavailable.
type artifact struct {
	// PinName is the supply-chain manifest artifact name (GAP-135,
	// internal/supplychain). It supplies BOTH the download URL and the
	// sha256 the bytes must match, so the version that is pinned and the
	// version that is installed cannot drift apart — and a moving
	// `releases/latest/download` URL is not representable.
	PinName string
	// BinaryName is the name the binary is installed under in
	// /usr/local/bin.
	BinaryName string
	// Note is shown in the plan, e.g. why this binary is a fair substitute.
	Note string
}

// prerequisite is one dependency of the rootless runtime, with every known way
// of satisfying it.
type prerequisite struct {
	Name     string
	Why      string // one line: what breaks without it
	Required bool

	// Probes are command or path names; finding ANY of them means the
	// dependency is already satisfied.
	Probes []string

	// Packages maps a family to the package that provides it. A family that
	// is absent from the map has no package strategy.
	Packages map[pkgFamily]string

	// Artifact, when set, is the download fallback.
	Artifact *artifact
}

// rootlessPrerequisites is the dependency set the rootless runtime needs.
// Required entries abort the install; optional ones warn.
func rootlessPrerequisites() []prerequisite {
	return []prerequisite{
		{
			Name:     "uidmap",
			Why:      "rootless Docker cannot map container uids without setuid newuidmap/newgidmap",
			Required: true,
			Probes:   []string{"newuidmap", "newgidmap"},
			Packages: map[pkgFamily]string{
				familyDebian: "uidmap",
				familyRHEL:   "shadow-utils",
				familyAlpine: "shadow",
				familyArch:   "shadow",
				familySUSE:   "shadow",
			},
		},
		{
			Name:     "fuse-overlayfs",
			Why:      "rootless Docker needs it for overlay2 storage; without it, containers are slow or fail",
			Required: false,
			Probes:   []string{"fuse-overlayfs"},
			Packages: map[pkgFamily]string{
				familyDebian: "fuse-overlayfs",
				familyRHEL:   "fuse-overlayfs",
				familyAlpine: "fuse-overlayfs",
				familyArch:   "fuse-overlayfs",
				familySUSE:   "fuse-overlayfs",
			},
			Artifact: &artifact{
				PinName:    supplychain.ArtifactFuseOverlayfs,
				BinaryName: "fuse-overlayfs",
				Note:       "upstream publishes a pinned static binary used by distros that lack the package",
			},
		},
		{
			Name:     "slirp4netns",
			Why:      "provides the userland network stack for rootless containers; without it, no container networking",
			Required: true,
			Probes:   []string{"slirp4netns"},
			Packages: map[pkgFamily]string{
				familyDebian: "slirp4netns",
				familyRHEL:   "slirp4netns",
				familyAlpine: "slirp4netns",
				familyArch:   "slirp4netns",
				familySUSE:   "slirp4netns",
			},
			Artifact: &artifact{
				PinName:    supplychain.ArtifactSlirp4netns,
				BinaryName: "slirp4netns",
				Note:       "upstream publishes a pinned static binary",
			},
		},
		{
			Name:     "dbus-user-session",
			Why:      "the rootless installer drives systemctl --user, which needs a working session bus",
			Required: false,
			Probes:   []string{"dbus-daemon"},
			Packages: map[pkgFamily]string{
				familyDebian: "dbus-user-session",
				familyRHEL:   "dbus-daemon",
				familyAlpine: "dbus",
				familyArch:   "dbus",
				familySUSE:   "dbus-1",
			},
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Outcomes
// ─────────────────────────────────────────────────────────────────────────────

// PrereqOutcome records how one prerequisite ended up, in terms an operator can
// act on without reading the code.
type PrereqOutcome struct {
	Name      string
	Satisfied bool
	Strategy  prereqStrategy
	Detail    string // what actually ran, or why nothing could
	Required  bool
}

// String renders one line for a log or a plan.
func (o PrereqOutcome) String() string {
	mark := "ok"
	if !o.Satisfied {
		if o.Required {
			mark = "MISSING"
		} else {
			mark = "missing (optional)"
		}
	}
	if o.Detail == "" {
		return fmt.Sprintf("%s: %s [%s]", o.Name, mark, o.Strategy)
	}
	return fmt.Sprintf("%s: %s [%s] %s", o.Name, mark, o.Strategy, o.Detail)
}

// PrereqOptions is the whole input to ensurePrerequisites. The zero value is
// usable: every empty field falls back to its production default, so a caller
// that only wants to redirect detection can set one field.
type PrereqOptions struct {
	// Apply runs the strategies. When false the call is a PLAN: it detects,
	// reports what it would do, and changes nothing.
	Apply bool
	// OSReleasePath overrides /etc/os-release (tests, or a sandbox).
	OSReleasePath string
	// Runner executes the privileged commands. Defaults to rootHostRunner,
	// which is the same root-side seam the rest of the install path uses.
	Runner systemRunner
	// LookPath resolves probes and the package manager. Defaults to
	// depLookPath.
	LookPath func(string) (string, error)
	// Logger receives progress. Nil is allowed and means silent.
	Logger *slog.Logger
	// Arch overrides runtime.GOARCH for artifact URLs (tests).
	Arch string
	// Verify checks a downloaded artifact against its supply-chain pin before
	// it may be installed (GAP-135). It defaults to the embedded manifest
	// (supplychain.VerifyFile); tests pin fixtures instead, so no case needs
	// the network.
	Verify func(name, arch, path string) error
}

func (o PrereqOptions) runner() systemRunner {
	if o.Runner != nil {
		return o.Runner
	}
	return rootHostRunner
}

func (o PrereqOptions) lookPath() func(string) (string, error) {
	if o.LookPath != nil {
		return o.LookPath
	}
	return depLookPath
}

func (o PrereqOptions) arch() string {
	if o.Arch != "" {
		return o.Arch
	}
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

// verify is the checksum gate installArtifact consults. The zero PrereqOptions
// means production: the embedded supply-chain manifest.
func (o PrereqOptions) verify() func(string, string, string) error {
	if o.Verify != nil {
		return o.Verify
	}
	return supplychain.VerifyFile
}

// runArgv adapts an argv slice to the systemRunner signature. Command builders
// here return a whole argv (name first); the runner seam takes name + args. An
// empty argv yields an error instead of a panic, so a family with no command
// is a clean strategy failure rather than a crash.
func runArgv(ctx context.Context, r systemRunner, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("no command available for this distribution family")
	}
	return r(ctx, argv[0], argv[1:]...)
}

// ─────────────────────────────────────────────────────────────────────────────
// The engine
// ─────────────────────────────────────────────────────────────────────────────

// ensurePrerequisites walks every rootless prerequisite and satisfies it with
// the first strategy that works, returning one outcome per dependency in
// declaration order.
//
// It returns an error only when a REQUIRED prerequisite could not be satisfied.
// Every outcome is returned either way, so a caller can log the full picture
// without re-deriving it.
func ensurePrerequisites(ctx context.Context, opts PrereqOptions) ([]PrereqOutcome, error) {
	family := detectFamily(ctx, opts)
	outcomes := make([]PrereqOutcome, 0, len(rootlessPrerequisites()))

	for _, p := range rootlessPrerequisites() {
		outcome := satisfyOne(ctx, opts, family, p)
		outcomes = append(outcomes, outcome)
		if opts.Logger != nil {
			if outcome.Satisfied {
				opts.Logger.Info("prerequisite satisfied", "name", outcome.Name,
					"strategy", string(outcome.Strategy), "detail", outcome.Detail)
			} else if outcome.Required {
				opts.Logger.Warn("prerequisite MISSING (required)", "name", outcome.Name,
					"detail", outcome.Detail)
			} else {
				opts.Logger.Warn("prerequisite missing (optional)", "name", outcome.Name,
					"detail", outcome.Detail)
			}
		}
	}

	if err := requiredFailure(outcomes); err != nil {
		return outcomes, err
	}
	return outcomes, nil
}

// satisfyOne applies the strategies in order to a single prerequisite.
func satisfyOne(ctx context.Context, opts PrereqOptions, family pkgFamily, p prerequisite) PrereqOutcome {
	base := PrereqOutcome{Name: p.Name, Required: p.Required}

	// 1. present — the cheap and common case.
	if found := probePresent(opts, p); found != "" {
		base.Satisfied = true
		base.Strategy = strategyPresent
		base.Detail = "found " + found
		return base
	}

	// 2. package — per-family name, with one retry after a metadata refresh.
	if pkg, ok := p.Packages[family]; ok {
		if opts.Apply {
			if out, err := runArgv(ctx, opts.runner(), packageInstallCommand(family, pkg)); err != nil {
				// A stale index is the most common reason a first install
				// fails on a fresh host; refresh once and retry before
				// giving up on this strategy.
				refresh := packageRefreshCommand(family)
				detailRefresh := ""
				if len(refresh) > 0 {
					if _, rerr := runArgv(ctx, opts.runner(), refresh); rerr != nil {
						detailRefresh = " (metadata refresh also failed)"
					}
				}
				if out2, err2 := runArgv(ctx, opts.runner(), packageInstallCommand(family, pkg)); err2 == nil {
					base.Satisfied = true
					base.Strategy = strategyPackage
					base.Detail = fmt.Sprintf("installed %s via %s after refresh%s", pkg, family, detailRefresh)
					return base
				} else {
					base.Detail = fmt.Sprintf("package strategy failed: %s%s", condenseInstall(string(out2)), detailRefresh)
					_ = out
				}
			} else {
				base.Satisfied = true
				base.Strategy = strategyPackage
				base.Detail = fmt.Sprintf("installed %s via %s", pkg, family)
				return base
			}
		} else {
			base.Detail = fmt.Sprintf("plan: would install %s via %s", pkg, family)
		}
	} else if family != familyUnknown {
		base.Detail = fmt.Sprintf("no package name mapped for family %s", family)
	} else {
		base.Detail = "no package strategy: unrecognised distribution"
	}

	// 3. artifact — a prebuilt static binary, pinned and verified (GAP-135).
	if p.Artifact != nil {
		url, urlErr := supplychain.ArtifactURL(p.Artifact.PinName, opts.arch())
		switch {
		case urlErr != nil:
			// An artifact the manifest does not pin cannot be verified, so it
			// cannot be used: the strategy reports the unpinned name instead
			// of downloading bytes nothing can vouch for.
			base.Detail = strings.TrimSpace(base.Detail + "; artifact strategy unavailable: " + urlErr.Error())
		case opts.Apply:
			if out, err := installArtifact(ctx, opts, p.Artifact, url); err == nil {
				base.Satisfied = true
				base.Strategy = strategyArtifact
				base.Detail = fmt.Sprintf("downloaded %s from %s", p.Artifact.BinaryName, url)
				return base
			} else {
				base.Detail = strings.TrimSpace(base.Detail + "; artifact strategy failed: " + condenseInstall(string(out)))
			}
		default:
			base.Detail = strings.TrimSpace(base.Detail + fmt.Sprintf("; plan: would download %s", url))
		}
	}

	// 4. source — only honest when a compiler is present; otherwise say so
	// rather than pretend the strategy exists.
	if cxx, err := opts.lookPath()("cc"); err == nil && cxx != "" {
		base.Detail = strings.TrimSpace(base.Detail + "; source strategy available (cc present) but not attempted: no source recipe for " + p.Name)
	} else {
		base.Detail = strings.TrimSpace(base.Detail + "; source strategy unavailable (no cc)")
	}

	base.Strategy = strategySkipped
	if p.Required {
		base.Strategy = strategyFailed
	}
	return base
}

// probePresent reports the first probe that resolves, or "".
func probePresent(opts PrereqOptions, p prerequisite) string {
	for _, probe := range p.Probes {
		if strings.Contains(probe, "/") {
			if st, err := os.Stat(probe); err == nil && !st.IsDir() {
				return probe
			}
			continue
		}
		if path, err := opts.lookPath()(probe); err == nil && path != "" {
			return path
		}
	}
	return ""
}

// requiredFailure returns an error naming every required prerequisite that
// could not be satisfied, with the strategies that were tried.
func requiredFailure(outcomes []PrereqOutcome) error {
	var missing []string
	for _, o := range outcomes {
		if o.Required && !o.Satisfied {
			missing = append(missing, fmt.Sprintf("%s (%s)", o.Name, o.Detail))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("rootless prerequisites unsatisfied: %s", strings.Join(missing, "; "))
}

// ─────────────────────────────────────────────────────────────────────────────
// Command construction (pure, so the plan can be asserted in tests)
// ─────────────────────────────────────────────────────────────────────────────

// packageCommand is the metadata refresh command for a family, as a full argv.
func packageRefreshCommand(family pkgFamily) []string {
	switch family {
	case familyDebian:
		return []string{"env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "update"}
	case familyRHEL:
		// dnf/yum refresh implicitly on install; no explicit step.
		return nil
	case familyAlpine:
		return []string{"apk", "update"}
	case familyArch:
		return []string{"pacman", "-Sy", "--noconfirm"}
	case familySUSE:
		return []string{"zypper", "--non-interactive", "refresh"}
	}
	return nil
}

// packageInstallCommand is the install argv for one package on one family.
// Non-interactive flags are explicit: this runs unattended inside a spawn.
func packageInstallCommand(family pkgFamily, pkg string) []string {
	switch family {
	case familyDebian:
		return []string{"env", "DEBIAN_FRONTEND=noninteractive",
			"apt-get", "install", "-y", "--no-install-recommends", pkg}
	case familyRHEL:
		return []string{"dnf", "install", "-y", pkg}
	case familyAlpine:
		return []string{"apk", "add", "--no-cache", pkg}
	case familyArch:
		return []string{"pacman", "-S", "--noconfirm", "--needed", pkg}
	case familySUSE:
		return []string{"zypper", "--non-interactive", "install", "-y", pkg}
	}
	return nil
}

// installArtifact downloads url into /usr/local/bin, verifies the bytes against
// the artifact's supply-chain pin, and only then marks them executable and
// installs them. It uses curl or wget, whichever is present, and refuses to
// guess when neither is.
//
// ORDER IS THE CONTROL (GAP-135): verification happens AFTER the download and
// BEFORE the chmod+mv, and a failure REMOVES the partial download. A binary
// that fails the gate is never installed on PATH, and no later step can
// install it — the error aborts the strategy. There is no "install anyway"
// branch, and no fallback to the unverified bytes.
func installArtifact(ctx context.Context, opts PrereqOptions, a *artifact, url string) ([]byte, error) {
	dest := "/usr/local/bin/" + a.BinaryName
	tmp := dest + ".download"

	if path, err := opts.lookPath()("curl"); err == nil && path != "" {
		if out, err := opts.runner()(ctx, "curl", "-fsSL", "-o", tmp, url); err != nil {
			return out, err
		}
	} else if path, err := opts.lookPath()("wget"); err == nil && path != "" {
		if out, err := opts.runner()(ctx, "wget", "-qO", tmp, url); err != nil {
			return out, err
		}
	} else {
		return nil, fmt.Errorf("neither curl nor wget is available to download %s", url)
	}

	if err := opts.verify()(a.PinName, opts.arch(), tmp); err != nil {
		_ = os.Remove(tmp)
		// The failure text is also returned as "output" so the operator-facing
		// outcome detail names the verification failure (that is the channel
		// every other failure in this strategy reports through), while the
		// error keeps the named sentinel for errors.Is callers.
		return []byte(fmt.Sprintf("verification failed: %v", err)),
			fmt.Errorf("verify %s artifact: %w", a.PinName, err)
	}

	if out, err := opts.runner()(ctx, "chmod", "0755", tmp); err != nil {
		return out, err
	}
	return opts.runner()(ctx, "mv", tmp, dest)
}

// condenseInstall trims installer output to its last meaningful line, so an
// outcome stays one line instead of a wall of package-manager noise.
func condenseInstall(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			if len(line) > 200 {
				line = line[:200] + "..."
			}
			return line
		}
	}
	return "no output"
}

// ─────────────────────────────────────────────────────────────────────────────
// Operator-facing plan
// ─────────────────────────────────────────────────────────────────────────────

// PlanPrerequisites renders what the install would do, without doing any of
// it. This is the surface an operator (or an error message) can show when a
// spawn fails on a host whose prerequisites are not met.
func PlanPrerequisites(ctx context.Context, opts PrereqOptions) ([]string, error) {
	opts.Apply = false
	outcomes, err := ensurePrerequisites(ctx, opts)
	lines := make([]string, 0, len(outcomes)+1)
	family := detectFamily(ctx, opts)
	lines = append(lines, fmt.Sprintf("detected distribution family: %s", family))
	for _, o := range outcomes {
		lines = append(lines, o.String())
	}
	return lines, err
}

// EnsureRootlessPrerequisites is the entry point the install path calls before
// it attempts to fetch the rootless installer. It applies the strategies (or
// plans them) and returns the outcomes so the caller can log them.
func EnsureRootlessPrerequisites(ctx context.Context, opts PrereqOptions) ([]PrereqOutcome, error) {
	return ensurePrerequisites(ctx, opts)
}
