package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// DF-BUNKER-57: on SSH-based agents there is NO image-spec path reachable from
// the CLI (the image is the host install), yet the probe correctly names rg as
// REQUIRED (search breaks without it) and gopls as the lsp capability. Printing
// the image-spec directive there is advice for a road that does not exist, so
// `agent-tools --install` now DELIVERS both onto the agent's own $HOME/bin —
// without becoming root, without a new RPC, and without giving up the
// supply-chain properties the vendored-only boundary exists to keep.
//
// The division of labour (docs/prd/SPEC-agent-tool-delivery.md) is preserved by
// changing the MECHANISM, not the ownership: a registry tool is still installed
// FROM its registry. Where the package manager itself cannot run (the agent
// user is not root), the delivery pulls the very archive the manager would
// install — `apt-get download` gets the SAME pinned, signature-verified .deb —
// and unpacks it into the agent's own PATH slot. gopls comes from the Go module
// proxy through `go install`, exactly the image-spec go directive's mechanism,
// after bootstrapping a checksum-pinned toolchain because the agent user cannot
// apt-install one.
//
// Every delivered tool is re-probed; a stage that cannot prove its result is a
// FAILED line naming the agent's own words, and the install fails with the
// per-tool causes. Nothing is claimed installed beyond what the probe proves.

// agentPackageToolBudget bounds ONE delivery stage (download + extract, or
// toolchain download + go install). The go stage legitimately takes minutes on
// a cold agent; the parent command's --timeout must cover the whole install,
// and each stage gets what is left up to this cap.
const agentPackageToolBudget = 420 * time.Second

// goBootstrapVersion / goBootstrapSHA256 pin the toolchain the gopls stage
// bootstraps. go.mod requires go 1.26.5 (module-aware go install needs >=
// 1.16; any recent toolchain qualifies), and the pins are the dl.google.com
// release checksums for 1.25.4 — the newest line that dodges the go1.25.4
// checksum re-upload incident (golang/go#76246/#76257: the sha256 on the
// download page changed mid-flight, which is exactly why the checksum is
// pinned HERE rather than trusted from the page at install time).
const goBootstrapVersion = "go1.25.4"

const goBootstrapSHA256AMD64 = "9fa5ffeda4170de60f67f3aa0f824e426421ba724c21e133c1e35d6159ca1bec"
const goBootstrapSHA256ARM64 = "a68e86d4b72c2c2fecf7dfed667680b6c2a071221bbdb6913cf83ce3f80d9ff0"

// packageToolInstaller is the runtime-delivery half of ONE registry tool the
// image-spec package-add path also owns. CatalogName is the probe's name for
// it; PackageToken is the token inside imagespec.AgentToolPackages that
// delivers the same tool at spawn — the pin test requires the two sets to
// match, so a registry row added to the spawn path cannot quietly skip the
// runtime path (and vice versa).
type packageToolInstaller struct {
	CatalogName  string
	PackageToken string
	// Script builds the delivery program run on the agent through the same
	// audited exec path as the probe. It must print machine-readable
	// verdict lines (see the builders below) and treat $HOME/bin as the
	// destination, because the server already puts it on every exec PATH.
	Script func() string
	// Parse turns the script's output into one outcome. It must classify an
	// unparsable line as a failure — silent tolerance is how a half-install
	// would read as a clean one.
	Parse func(output string) packageToolOutcome
}

// packageToolOutcome is ONE stage's result.
type packageToolOutcome struct {
	Name   string
	Status string // "INSTALLED" or "FAILED"
	Detail string // already-present, or the agent's own words
}

// quoteSh returns s wrapped in single quotes with embedded single quotes
// escaped for POSIX sh — the same quoting discipline internal/server applies
// to every remote command it builds. Local to the package-path builders so the
// stage scripts cannot inherit an agent-controlled value as shell syntax.
func quoteSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// agentPackageInstallDir is the destination directory INSIDE the agent,
// relative to its home — the same $HOME/bin the vendored copy uses and the
// server already puts first on every exec PATH.
const agentPackageInstallDir = agentToolInstallDir

// aptDeliverScript builds the ripgrep stage: download the pinned archive deb
// through apt's own fetcher, extract it as the agent user, copy the binary
// into $HOME/bin. Line contract:
//
//	rg	INSTALLED	already-present
//	rg	INSTALLED	<version the binary reports>
//	rg	FAILED	<the failing stage + its last line>
func aptDeliverScript(catalogName, aptPackage string) func() string {
	return func() string {
		var b strings.Builder
		b.WriteString("set +e\n")
		b.WriteString("name=" + quoteSh(catalogName) + "\n")
		b.WriteString("pkg=" + quoteSh(aptPackage) + "\n")
		b.WriteString(`b="$HOME/` + agentPackageInstallDir + `"
mkdir -p "$b" 2>/dev/null || { printf '%s	FAILED	cannot create %s\n' "$name" "$b"; exit 0; }
if command -v "$name" >/dev/null 2>&1; then
  printf '%s	INSTALLED	already-present\n' "$name"; exit 0
fi
if command -v apt-get >/dev/null 2>&1; then
  fetcher=apt-get
elif command -v apt >/dev/null 2>&1; then
  fetcher=apt
else
  printf '%s	FAILED	no apt-get or apt on the agent\n' "$name"; exit 0
fi
tmp=$(mktemp -d 2>/dev/null) || { printf '%s	FAILED	mktemp failed\n' "$name"; exit 0; }
(cd "$tmp" || exit 1
 log="$tmp/apt.log"
 if command -v apt-get >/dev/null 2>&1; then
   fetcher=apt-get
 elif command -v apt >/dev/null 2>&1; then
   fetcher=apt
 else
   printf '%s\tFAILED\tno apt-get or apt on the agent\n' "$name"; exit 0
 fi
 "$fetcher" download "$pkg" >"$log" 2>&1 || {
   printf '%s\tFAILED\t%s download: %s\n' "$name" "$fetcher" "$(tail -n 1 "$log")"; exit 1
 }
 extracted=0
 for d in ./*.deb; do
   [ -e "$d" ] || continue
   dpkg-deb -x "$d" "$tmp/x" >>"$log" 2>&1 || {
     printf '%s\tFAILED\tdpkg-deb -x: %s\n' "$name" "$(tail -n 1 "$log")"; exit 1
   }
   extracted=1
 done
 [ "$extracted" = 1 ] || { printf '%s\tFAILED\t%s download produced no .deb\n' "$name" "$fetcher"; exit 1; }
 bin="$tmp/x/usr/bin/$name"
 [ -f "$bin" ] || { printf '%s\tFAILED\tunexpected deb layout: no usr/bin/%s\n' "$name" "$name"; exit 1; }
 cp "$bin" "$b/$name.new" >>"$log" 2>&1 || {
   printf '%s\tFAILED\tcopy into %s: %s\n' "$name" "$b" "$(tail -n 1 "$log")"; exit 1
 }
 chmod 0755 "$b/$name.new" 2>>"$log"
 mv -f "$b/$name.new" "$b/$name" 2>>"$log" || {
   printf '%s\tFAILED\tfinalize in %s: %s\n' "$name" "$b" "$(tail -n 1 "$log")"; exit 1
 }
 v=$("$b/$name" --version 2>/dev/null | head -n 1)
 printf '%s\tINSTALLED\t%s\n' "$name" "$v"
)
rc=$?
rm -rf "$tmp"
exit $rc
`)
		return b.String()
	}
}

// goDeliverScript builds the gopls stage: bootstrap a checksum-pinned Go
// toolchain into a temp dir (the agent user cannot apt-install one), then run
// the SAME module-aware install the image-spec go directive renders, with
// GOBIN aimed at $HOME/bin. Line contract matches the apt stage, with
// catalogName=gopls.
func goDeliverScript(catalogName, goPackage string) func() string {
	return func() string {
		var b strings.Builder
		b.WriteString("set +e\n")
		b.WriteString("name=" + quoteSh(catalogName) + "\n")
		b.WriteString("gpkg=" + quoteSh(goPackage) + "\n")
		b.WriteString(`b="$HOME/` + agentPackageInstallDir + `"
mkdir -p "$b" 2>/dev/null || { printf '%s	FAILED	cannot create %s\n' "$name" "$b"; exit 0; }
if command -v "$name" >/dev/null 2>&1; then
  printf '%s	INSTALLED	already-present\n' "$name"; exit 0
fi
if command -v go >/dev/null 2>&1; then
  goroot=""
else
  case "$(uname -m)" in
    x86_64) garch=amd64; gsha=` + "`" + goBootstrapSHA256AMD64 + "`" + ` ;;
    aarch64|arm64) garch=arm64; gsha=` + "`" + goBootstrapSHA256ARM64 + "`" + ` ;;
    *) printf '%s\tFAILED\tunsupported architecture %s for the Go toolchain\n' "$name" "$(uname -m)"; exit 1 ;;
  esac
  tmp=$(mktemp -d) || { printf '%s\tFAILED\tmktemp failed\n' "$name"; exit 0; }
  ( cd "$tmp" || exit 1
    log="$tmp/go-bootstrap.log"
    curl -fsSL "https://dl.google.com/go/` + goBootstrapVersion + `.linux-$garch.tar.gz" -o go.tgz >"$log" 2>&1 || {
      printf '%s\tFAILED\ttoolchain download: %s\n' "$name" "$(tail -n 1 "$log")"; exit 1
    }
    echo "$gsha  go.tgz" | sha256sum -c - >>"$log" 2>&1 || {
      printf '%s\tFAILED\ttoolchain checksum mismatch (expected %s for %s)\n' "$name" "$gsha" "$garch"; exit 1
    }
    tar -xzf go.tgz >>"$log" 2>&1 || {
      printf '%s\tFAILED\ttoolchain extract: %s\n' "$name" "$(tail -n 1 "$log")"; exit 1
    }
    printf '%s\n' "$tmp/go"
  )
  rc=$?; [ $rc = 0 ] && goroot=$(cd "$tmp" 2>/dev/null && pwd)/go
  if [ $rc != 0 ]; then rm -rf "$tmp"; exit $rc; fi
fi
work=$(mktemp -d 2>/dev/null) || { printf '%s\tFAILED\tmktemp failed\n' "$name"; exit 0; }
( cd "$work" || exit 1
  log="$work/gopls.log"
  env_file="$work/env"
  {
    [ -n "$goroot" ] && printf 'GOROOT=%s\nPATH=%s/bin:%s\n' "$goroot" "$goroot" "$PATH"
    printf 'GOBIN=%s\nGOCACHE=%s/cache\nGOMODCACHE=%s/mod\nGOPATH=%s/gopath\n' "$b" "$work" "$work" "$work"
  } >"$env_file"
  unset PATH; set -a; . "$env_file"; set +a
  go install "$gpkg" >"$log" 2>&1 || {
    printf '%s\tFAILED\tgo install: %s\n' "$name" "$(tail -n 1 "$log")"; exit 1
  }
  [ -x "$b/$name" ] || { printf '%s\tFAILED\tgo install finished but %s/%s is not there\n' "$name" "$b" "$name"; exit 1; }
  v=$("$b/$name" version 2>/dev/null | head -n 1)
  printf '%s\tINSTALLED\t%s\n' "$name" "$v"
)
rc=$?
rm -rf "$work"
[ -n "$goroot" ] && rm -rf "$goroot"
exit $rc
`)
		return b.String()
	}
}

// parsePackageToolOutput reads a delivery stage's verdict lines. A line it
// cannot parse is a FAILED outcome carrying the line itself: the stage may have
// half-run, and "no verdict" must never read as success.
func parsePackageToolOutput(name, output string) packageToolOutcome {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[0]) != name {
			continue
		}
		return packageToolOutcome{
			Name:   strings.TrimSpace(parts[0]),
			Status: strings.TrimSpace(parts[1]),
			Detail: strings.TrimSpace(parts[2]),
		}
	}
	head := strings.TrimSpace(output)
	if len(head) > 120 {
		head = head[:120] + "..."
	}
	if head == "" {
		head = "(no output)"
	}
	return packageToolOutcome{Name: name, Status: "FAILED", Detail: "unparsable output: " + head}
}

// packageToolInstallers is the runtime delivery registry. The pin test
// (TestPackageToolInstallersMatchAgentToolPackages) requires its PackageToken
// set to equal imagespec.AgentToolPackages' package set, so the spawn-time
// delivered set and the runtime-deliverable set can never drift apart.
var packageToolInstallers = []packageToolInstaller{
	{CatalogName: "rg", PackageToken: "ripgrep",
		Script: aptDeliverScript("rg", "ripgrep"),
		Parse:  func(output string) packageToolOutcome { return parsePackageToolOutput("rg", output) }},
	{CatalogName: "gopls", PackageToken: "golang.org/x/tools/gopls@latest",
		Script: goDeliverScript("gopls", "golang.org/x/tools/gopls@latest"),
		Parse:  func(output string) packageToolOutcome { return parsePackageToolOutput("gopls", output) }},
}

// deliverMissingPackageTools is the DF-BUNKER-57 half of --install: for every
// still-missing registry tool, run its delivery stage on the agent. It returns
// the successfully delivered catalog names and one named failure per tool that
// could not be proven installed — the caller decides the verdict, and no
// partial stage is ever reported as installed.
func deliverMissingPackageTools(cmd *cobra.Command, ctx context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID string, report agentToolReport) (delivered []string, failures []string, err error) {

	errOut := cmd.ErrOrStderr()
	for _, inst := range packageToolInstallers {
		f := findingFor(report, inst.CatalogName)
		if f == nil || f.Status == "present" {
			continue
		}
		_, _ = fmt.Fprintf(errOut, "bunker: delivering %s (package path: %s) onto the agent's $HOME/%s\n",
			inst.CatalogName, inst.PackageToken, agentPackageInstallDir)

		budget := stageBudgetSeconds(ctx, agentPackageToolBudget)
		out, exitCode, err := execOnAgentScript(ctx, client, entry, agentID, inst.Script(), budget)
		if err != nil {
			return delivered, failures, fmt.Errorf("deliver %s on agent %q: %w", inst.CatalogName, agentID, err)
		}
		if exitCode != 0 && strings.TrimSpace(out) == "" {
			return delivered, failures, fmt.Errorf("deliver %s on agent %q: the agent exited %d with no output",
				inst.CatalogName, agentID, exitCode)
		}
		outcome := inst.Parse(out)
		switch outcome.Status {
		case "INSTALLED":
			delivered = append(delivered, inst.CatalogName)
			_, _ = fmt.Fprintf(errOut, "bunker: %s delivered (%s)\n", inst.CatalogName, outcome.Detail)
		case "FAILED":
			failures = append(failures, fmt.Sprintf("%s: %s", inst.CatalogName, outcome.Detail))
			_, _ = fmt.Fprintf(errOut, "bunker: WARNING %s delivery FAILED: %s\n", inst.CatalogName, outcome.Detail)
		default:
			failures = append(failures, fmt.Sprintf("%s: stage ended in state %q (%s)",
				inst.CatalogName, outcome.Status, outcome.Detail))
		}
	}
	return delivered, failures, nil
}

// stageBudgetSeconds caps ONE stage at cap, but never beyond what the parent
// command's context has left — a stage must not outlive the RPC that carries it.
func stageBudgetSeconds(ctx context.Context, cap time.Duration) uint32 {
	remaining := ctxDeadlineSeconds(ctx)
	s := int(cap.Seconds())
	if s < remaining {
		remaining = s
	}
	if remaining < 1 {
		remaining = 1
	}
	return uint32(remaining)
}

// isPackagePathTool reports whether a catalog name has a runtime delivery
// stage. Used by the post-install report to distinguish "attempted" from
// "unknown how to deliver here".
func isPackagePathTool(name string) bool {
	for _, inst := range packageToolInstallers {
		if inst.CatalogName == name {
			return true
		}
	}
	return false
}
