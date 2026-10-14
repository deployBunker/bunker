package cli

// MOUNT-008 — opt-in FUSE over io_uring: runtime capability probe + `--driver`.
//
// This is a VERIFY-THEN-ENABLE row, not a driver implementation. It adds
// (a) a runtime probe answering "can THIS host mount with the FUSE io_uring
// transport?", (b) the `--driver=auto|fuse|fuse-iouring` selection on
// `bunker mount`, and (c) a named, bounded refusal (never a hang) when the
// operator explicitly asks for a transport the host cannot provide.
//
// WHAT THE TRANSPORT IS. A transport change for FUSE itself, not a
// filesystem: it moves the kernel<->userspace request channel from the
// /dev/fuse read/write loop onto io_uring. Upstream requires THREE things
// together:
//
//  1. a kernel built with CONFIG_FUSE_IO_URING (Linux >= 6.14);
//  2. a libfuse built with the io_uring transport;
//  3. the fuse module parameter enabled (root):
//     `echo 1 > /sys/module/fuse/parameters/enable_uring`.
//
// The option that selects it is `-o io_uring` (libfuse's own help text:
// "    -o io_uring            enable io-uring"). It is spelled with an
// underscore; `-o iouring` is an unknown option. It travels
// sshfs -> libfuse3 -> fusermount3 -> mount(2), which is where it is consumed.
//
// MEASURED FINDING — control host, 2026-10-14 (kernel 7.0.0-31-generic):
//
//   - libfuse3 on this host DOES expose the transport. The library is
//     /usr/lib/x86_64-linux-gnu/libfuse3.so.3.18.2 (SONAME `libfuse3.so.4`,
//     package version 3.18.2) and it carries BOTH:
//     nm -D   -> `T fuse_req_is_uring@@FUSE_3.18`
//     strings -> `io_uring`, `io_uring_q_depth=%u`,
//     `-o io_uring            enable io-uring`
//     The earlier record in docs/mount-drivers.md §3.3 ("the packaged libfuse
//     exposes no io_uring transport") was WRONG. Its cause was a probe-path
//     bug, not a missing feature: the check ran `nm -D
//     /usr/lib/x86_64-linux-gnu/libfuse3.so.3`, which does NOT exist on this
//     host (the SONAME here is .so.4), so nm failed with "No such file",
//     grep found nothing, and "no output" was read as "no support". That is
//     exactly why this probe (1) RESOLVES the library instead of hard-coding
//     a filename and (2) reports a FAILED READ as `unknown`, never as
//     `unsupported`.
//   - kernel side: CONFIG_FUSE_IO_URING=y and
//     /sys/module/fuse/parameters/enable_uring exists — ready.
//   - BUT that module parameter reads `N`: the transport is DISABLED at
//     runtime, so a mount with `-o io_uring` cannot succeed until an operator
//     enables it: `echo 1 > /sys/module/fuse/parameters/enable_uring` (root).
//     sshfs itself carries no io_uring strings — expected, it is a filesystem;
//     the option belongs to libfuse. fusermount3 carries none either: it
//     forwards mount options opaquely, so its silence is not evidence.
//
// ENABLE CONDITION (all three; the third is the current blocker here):
//
//	libfuse3 survey = supported  AND  kernel >= 6.14  AND  enable_uring = 1
//
// With `--driver=auto` (the default) the io_uring transport is used only when
// all three hold; otherwise the plain FUSE transport is used, silently, so a
// host without support is byte-identical to the behaviour before this row.
// `--driver=fuse-iouring` turns that into a named refusal before any mount
// attempt.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ── the flag's vocabulary ───────────────────────────────────────────────────

const (
	// FuseTransportAuto is the default: prefer the io_uring transport when
	// this host can actually provide it, otherwise mount exactly as before.
	FuseTransportAuto = "auto"
	// FuseTransportFuse is the plain FUSE transport (the /dev/fuse
	// read/write loop) — the behaviour every bunker mount had before
	// MOUNT-008. It never probes the host, so it is also the escape hatch and
	// the exact-old-behaviour switch.
	FuseTransportFuse = "fuse"
	// FuseTransportFuseIOUring REQUIRES the io_uring transport: when the host
	// cannot provide it the mount is refused with a named error BEFORE any
	// mount attempt (no mountpoint is created, nothing is executed).
	FuseTransportFuseIOUring = "fuse-iouring"
)

// iouringMountOption is the libfuse3 mount option that selects the io_uring
// transport (libfuse 3.18.2 help text: "-o io_uring            enable
// io-uring"). Underscore, not a hyphen.
const iouringMountOption = "io_uring"

// enableUringParamPath is the kernel's own switch for the transport. Present
// and enabled is required; present-and-0 is the measured state on the control
// host.
const enableUringParamPath = "/sys/module/fuse/parameters/enable_uring"

// minIOUringKernel{Major,Minor}: upstream's floor — CONFIG_FUSE_IO_URING
// first appeared in Linux 6.14.
const (
	minIOUringKernelMajor = 6
	minIOUringKernelMinor = 14
)

// fuseIOUringEnv is the operator/suite override. `off` (also 0/disable/
// disabled/never) forces the plain FUSE transport for every mount and skips
// host probing entirely — the per-invocation equivalent is `--driver=fuse`.
// Any other value (or unset) means "probe the host". Deliberately NOT a way
// to force the transport ON: a missing transport cannot be asserted into
// existence, and pretending otherwise is how a "silently not applied" option
// ships.
const fuseIOUringEnv = "BUNKER_FUSE_IOURING"

// iouringProbeCommandTimeout bounds each PROBE command (ldconfig, nm,
// strings, uname). The probe must never hang: a survey tool that stalls is
// killed and its partial output is classified (which degrades to `unknown`,
// i.e. the safe fallback).
var iouringProbeCommandTimeout = 5 * time.Second

// iouringAcceptanceProbeTimeout is the named bound on the io_uring transport's
// ACCEPTANCE probe: the first mount attempt that carries `-o io_uring` is the
// live confirmation (the kernel's real answer), so it gets its own named
// deadline — distinct from, and shorter than, the sshfs attempt budget — and
// can never hang the command.
// Package-level var so a test can shrink it and reach the deadline.
var iouringAcceptanceProbeTimeout = 20 * time.Second

// ── the three-valued verdict ────────────────────────────────────────────────

// iouringState is a component's (or the whole probe's) verdict. Three values,
// not two, on purpose: "we read the library and it has no io_uring transport"
// and "we could not read the library at all" are different facts, and
// collapsing them is precisely the bug that produced the stale §3.3 record
// ("nm failed on a path that does not exist" was reported as "no support").
type iouringState string

const (
	iouringSupported   iouringState = "supported"
	iouringUnsupported iouringState = "unsupported"
	iouringUnknown     iouringState = "unknown"
)

// fuseIOUringCapability is the full runtime probe result: the overall verdict
// plus one line per component, each carrying its own named reason so the
// refusal text can say WHICH of the three conditions failed.
type fuseIOUringCapability struct {
	State iouringState

	LibfusePath   string
	LibfuseState  iouringState
	LibfuseDetail string

	KernelRelease string
	KernelState   iouringState
	KernelDetail  string

	EnableUringRaw    string
	EnableUringState  iouringState
	EnableUringDetail string

	// Override names the reason the probe did not run at all (the
	// BUNKER_FUSE_IOURING=off path). Non-empty only in that case.
	Override string
}

// usable reports whether the io_uring transport can actually be mounted here.
func (c fuseIOUringCapability) usable() bool { return c.State == iouringSupported }

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// String renders the finding as one operator-readable line. It always names
// the decisive component first, then the two others, so an operator can see
// the whole enable condition and which part of it is unmet.
func (c fuseIOUringCapability) String() string {
	if c.State == iouringSupported {
		return fmt.Sprintf(
			"FUSE io_uring transport supported: libfuse3 %s exposes it, kernel %s is >= %d.%d, %s is enabled",
			firstNonEmpty(c.LibfusePath, "(resolved)"), firstNonEmpty(c.KernelRelease, "(unknown)"),
			minIOUringKernelMajor, minIOUringKernelMinor, enableUringParamPath)
	}
	if c.Override != "" {
		return "FUSE io_uring transport unavailable: " + c.Override
	}
	head := "FUSE io_uring transport unavailable"
	switch {
	case c.LibfuseState != iouringSupported:
		head += " (libfuse3: " + string(c.LibfuseState) + ")"
	case c.KernelState != iouringSupported:
		head += " (kernel: " + string(c.KernelState) + ")"
	default:
		head += " (kernel module parameter: " + string(c.EnableUringState) + ")"
	}
	parts := []string{head}
	if d := c.LibfuseDetail; d != "" {
		parts = append(parts, "libfuse3: "+d)
	}
	if d := c.KernelDetail; d != "" {
		parts = append(parts, "kernel: "+d)
	}
	if d := c.EnableUringDetail; d != "" {
		parts = append(parts, "enable_uring: "+d)
	}
	return strings.Join(parts, "; ")
}

// enableCondition documents the three-part condition in refusal text so the
// operator is told what to change, not just that something is missing.
func enableCondition() string {
	return fmt.Sprintf(
		"the io_uring transport needs all three: (1) a kernel >= %d.%d built with CONFIG_FUSE_IO_URING, (2) a libfuse3 built with the io_uring transport, (3) `echo 1 > %s` (root)",
		minIOUringKernelMajor, minIOUringKernelMinor, enableUringParamPath)
}

// ── the pure decision layer (unit-testable on any host) ─────────────────────

// iouringSurveyTokenRE matches every spelling of the transport that a symbol
// table or option table can carry. Three shapes are real:
//
//	`io_uring` / `io-uring` / `iouring`   — the mount option and libfuse's own
//	                                        usage text;
//	`uring` after a non-alphanumeric      — the dynamic SYMBOL the transport
//	                                        registers, `fuse_req_is_uring`
//	                                        (it does NOT contain "io_uring":
//	                                        this is why a plain `io_uring`
//	                                        pattern would report the control
//	                                        host as unsupported).
//
// The leading `(?:^|[^[:alnum:]])` guard is what keeps `uring` from matching
// inside an unrelated word ("measuring", "securing"), and `[^[:alnum:]]` (not
// `\b`) is required because `_` is a word character — `\buring` does not match
// `fuse_req_is_uring`.
var iouringSurveyTokenRE = regexp.MustCompile(`(?i)(?:^|[^[:alnum:]])(io[_-]?uring|uring)`)

// surveyReadFailureRE matches the noise a FAILED survey leaves in its output
// (nm/strings print these on stderr, which runWithTimeout folds into the
// captured text). Its presence turns an empty-looking survey into `unknown`,
// never into `unsupported`.
var surveyReadFailureRE = regexp.MustCompile(`(?i)no such file|not found|cannot open|permission denied|operation not permitted|file format not recognized|invalid file|too many open files|is a directory`)

// fuseEvidenceTokens are strings any real libfuse3 build carries regardless of
// its transport. Their presence proves the survey actually READ a libfuse
// build, which is what licenses an `unsupported` verdict from the transport's
// absence.
var fuseEvidenceTokens = []string{"fuse_", "FUSE_3.", "libfuse", "auto_unmount", "fusermount"}

// classifyIOUringSymbolSurvey turns one libfuse3 survey (nm/strings output,
// labelled by the seam) into a verdict plus the named reason. This is the
// table-tested heart of the probe: it must pass fixtures on any host.
//
// Verdict rules, in order:
//
//  1. any io_uring spelling present            -> supported;
//  2. an empty survey                        -> unknown (nothing was read);
//  3. a read-failure marker present          -> unknown (absence is not evidence);
//  4. fuse evidence present, no io_uring     -> unsupported (it IS a libfuse
//     build, read successfully, and lacks the transport);
//  5. no fuse evidence at all                -> unknown (this is not
//     recognisably a libfuse3 build).
func classifyIOUringSymbolSurvey(survey string) (iouringState, string) {
	trimmed := strings.TrimSpace(survey)
	if trimmed == "" {
		return iouringUnknown, "the survey is empty — nothing was read from the library, so nothing can be concluded"
	}
	if m := iouringSurveyTokenRE.FindStringSubmatch(trimmed); m != nil {
		return iouringSupported, fmt.Sprintf("the survey carries the io_uring transport (%q); first matching line: %s",
			m[1], firstMatchingLine(trimmed, m[1]))
	}
	if marker := surveyReadFailureRE.FindString(trimmed); marker != "" {
		return iouringUnknown, fmt.Sprintf(
			"the survey did not read the library (output contains %q: %s) — a failed read is not evidence of absence",
			marker, firstMatchingLine(trimmed, marker))
	}
	if tok := fuseEvidenceToken(trimmed); tok != "" {
		return iouringUnsupported, fmt.Sprintf(
			"the library was read successfully (%d output line(s); fuse evidence %q present) and carries no io_uring transport",
			countNonEmptyLines(trimmed), tok)
	}
	return iouringUnknown, fmt.Sprintf(
		"the survey carries no fuse evidence at all (%d output line(s)) — this is not recognisably a libfuse3 build, so nothing can be concluded",
		countNonEmptyLines(trimmed))
}

// fuseEvidenceToken returns the first fuse-evidence token present anywhere in
// the survey, or "" when the survey carries none (used for the
// operator-readable reason and as the licence for an `unsupported` verdict).
func fuseEvidenceToken(survey string) string {
	for _, tok := range fuseEvidenceTokens {
		if strings.Contains(survey, tok) {
			return tok
		}
	}
	return ""
}

// firstMatchingLine returns the first survey line containing token
// (case-insensitive), trimmed and bounded, for embedding in a reason.
func firstMatchingLine(text, token string) string {
	lower := strings.ToLower(token)
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(strings.ToLower(line), lower) {
			s := strings.TrimSpace(line)
			const max = 160
			if len(s) > max {
				s = s[:max] + "..."
			}
			return s
		}
	}
	return strings.TrimSpace(text)
}

func countNonEmptyLines(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// kernelReleaseRE captures the major.minor of a kernel release string
// ("7.0.0-31-generic" -> 7.0).
var kernelReleaseRE = regexp.MustCompile(`^(\d+)\.(\d+)`)

// classifyKernelRelease decides whether this kernel can carry
// CONFIG_FUSE_IO_URING (>= 6.14). An unparsable release is `unknown`, never
// `unsupported`: we may not have been able to read it, and guessing here
// would silently disable a capable host.
func classifyKernelRelease(release string) (iouringState, string) {
	r := strings.TrimSpace(release)
	if r == "" {
		return iouringUnknown, "the kernel release string could not be read — nothing can be concluded"
	}
	m := kernelReleaseRE.FindStringSubmatch(r)
	if m == nil {
		return iouringUnknown, fmt.Sprintf("the kernel release %q is not a parsable X.Y[.Z] version", r)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major > minIOUringKernelMajor || (major == minIOUringKernelMajor && minor >= minIOUringKernelMinor) {
		return iouringSupported, fmt.Sprintf("kernel %s is >= %d.%d, the first release with CONFIG_FUSE_IOURING",
			r, minIOUringKernelMajor, minIOUringKernelMinor)
	}
	return iouringUnsupported, fmt.Sprintf("kernel %s is older than %d.%d, the first release with CONFIG_FUSE_IOURING",
		r, minIOUringKernelMajor, minIOUringKernelMinor)
}

// classifyEnableUringParam interprets /sys/module/fuse/parameters/enable_uring.
// A bool_ops sysfs parameter prints Y/N on most kernels and 1/0 on some; both
// spellings (and their synonyms) are accepted.
func classifyEnableUringParam(raw string) (iouringState, string) {
	v := strings.TrimSpace(raw)
	switch strings.ToLower(v) {
	case "y", "yes", "1", "true", "on", "enabled":
		return iouringSupported, fmt.Sprintf("%s is enabled (=%s)", enableUringParamPath, v)
	case "n", "no", "0", "false", "off", "disabled":
		return iouringUnsupported, fmt.Sprintf(
			"%s is DISABLED (=%s) — this kernel has the transport but has it switched off; enable it with `echo 1 > %s` (root)",
			enableUringParamPath, v, enableUringParamPath)
	default:
		return iouringUnknown, fmt.Sprintf(
			"%s read %q, which is neither an enabled nor a disabled value (%s does not exist, or this kernel does not expose CONFIG_FUSE_IOURING here)",
			enableUringParamPath, v, enableUringParamPath)
	}
}

// iouringFailureRE matches mount output that names the io_uring transport (or
// the option carrying it) as the CAUSE of a failure. Only such a failure is
// treated as transport-specific: a generic failure (a refused connection, bad
// key, missing host) keeps the ordinary sshfs classification and retry path,
// so this row cannot reclassify unrelated failures as transport problems.
var iouringFailureRE = regexp.MustCompile(`(?i)(io[_-]?uring[^\n]{0,100}(unknown option|unknown|unsupported|not supported|unrecognized|invalid|unavailable|not enabled|disabled|failed|denied)|(unknown option|unsupported|not supported|unrecognized option|invalid option|unknown)[^\n]{0,40}io[_-]?uring|enable_uring)`)

// classifyIOUringMountFailure reports whether a failed attempt's captured
// output names the io_uring transport as the cause, plus the matched line as
// the reason. Callers use this to stop retrying (the transport, not the
// attempt, is the problem) and to name the remedy.
func classifyIOUringMountFailure(output string) (bool, string) {
	m := iouringFailureRE.FindString(output)
	if m == "" {
		return false, ""
	}
	return true, firstMatchingLine(output, m)
}

// ── the imperative probe ────────────────────────────────────────────────────

// fuseIOUringDisabledByEnv reports whether the environment pins the plain
// transport. The returned reason is operator-readable and names the variable.
func fuseIOUringDisabledByEnv() (bool, string) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(fuseIOUringEnv)))
	switch v {
	case "off", "0", "false", "no", "disable", "disabled", "never":
		return true, fmt.Sprintf("disabled by %s=%s (an operator or test pinned the plain FUSE transport)", fuseIOUringEnv, v)
	}
	return false, ""
}

// libfuse3CandidatePaths resolves the libfuse3 shared object at RUNTIME
// instead of hard-coding a SONAME. The stale §3.3 record is the reason: this
// host ships SONAME `libfuse3.so.4` (real file `libfuse3.so.3.18.2`), so the
// hard-coded `libfuse3.so.3` path did not exist, the survey command failed,
// and "no output" was misread as "no support". Discovery order:
//
//  1. $BUNKER_LIBFUSE3 (explicit override: operators and tests);
//  2. `ldconfig -p` — the loader's own answer for THIS host;
//  3. the standard lib directories, globbed (`libfuse3.so.*` covers .so,
//     .so.3, .so.4 and .so.X.Y.Z).
var libfuse3CandidatePaths = func() []string {
	if env := strings.TrimSpace(os.Getenv("BUNKER_LIBFUSE3")); env != "" {
		return resolveExistingPaths([]string{env})
	}

	var raw []string
	if ldconfig, err := exec.LookPath("ldconfig"); err == nil {
		if text, runErr := runWithTimeout(execCommand(ldconfig, "-p"), iouringProbeCommandTimeout); runErr == nil {
			for _, line := range strings.Split(text, "\n") {
				_, after, ok := strings.Cut(line, "=>")
				if !ok {
					continue
				}
				p := strings.TrimSpace(after)
				if strings.HasPrefix(filepath.Base(p), "libfuse3.so") {
					raw = append(raw, p)
				}
			}
		}
	}
	for _, dir := range []string{"/usr/lib", "/usr/lib64", "/lib", "/lib64", "/usr/local/lib", "/usr/local/lib64"} {
		for _, pattern := range []string{filepath.Join(dir, "libfuse3.so.*"), filepath.Join(dir, "*", "libfuse3.so.*")} {
			if matches, err := filepath.Glob(pattern); err == nil {
				raw = append(raw, matches...)
			}
		}
	}
	return resolveExistingPaths(raw)
}

// resolveExistingPaths dedupes candidates by the file the loader would read
// (symlinks resolved) and keeps only regular files, so the probe never reads
// a dangling symlink and never surveys the same library twice.
func resolveExistingPaths(raw []string) []string {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p == "" {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		key := p
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			key = resolved
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// libfuse3Survey runs the two cheap static surveys over one resolved library
// and returns them as one labelled text block. Both commands are bounded;
// `nm -D` reads the dynamic symbol table (the FUSE transport registers
// `fuse_req_is_uring`), `strings` reads the option/usage table (the transport
// is also an option string). Their output is classified by
// classifyIOUringSymbolSurvey, so a failed read stays `unknown`.
var libfuse3Survey = func(path string) (string, error) {
	var (
		b        strings.Builder
		firstErr error
	)
	for _, tool := range []struct {
		name string
		args []string
	}{
		{"nm", []string{"-D", path}},
		{"strings", []string{path}},
	} {
		found, lookErr := exec.LookPath(tool.name)
		if lookErr != nil {
			fmt.Fprintf(&b, "--- %s: %v ---\n", tool.name, lookErr)
			if firstErr == nil {
				firstErr = lookErr
			}
			continue
		}
		out, runErr := runWithTimeout(execCommand(found, tool.args...), iouringProbeCommandTimeout)
		fmt.Fprintf(&b, "--- %s %s ---\n", tool.name, path)
		b.WriteString(out)
		if !strings.HasSuffix(out, "\n") {
			b.WriteString("\n")
		}
		if runErr != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", tool.name, runErr)
		}
	}
	return b.String(), firstErr
}

// kernelRelease reads this kernel's release string. Bounded like the rest of
// the probe: a stalled `uname` (a wedged NSS or a broken PATH shim) is killed
// by the shared timeout rather than hanging the mount.
var kernelRelease = func() string {
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	if uname, err := exec.LookPath("uname"); err == nil {
		if out, runErr := runWithTimeout(execCommand(uname, "-r"), iouringProbeCommandTimeout); runErr == nil {
			return strings.TrimSpace(out)
		}
	}
	return ""
}

// readEnableUringParam reads the kernel's switch for the transport.
var readEnableUringParam = func() (string, error) {
	b, err := os.ReadFile(enableUringParamPath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// probeFuseIOUringCapability runs the full runtime probe and combines the
// three components into one verdict. It never returns an error: an
// unavailable probe is `unknown`, which the caller treats exactly like
// `unsupported` (use the plain transport) — the probe must never be able to
// break a mount.
func probeFuseIOUringCapability() fuseIOUringCapability {
	cap := fuseIOUringCapability{
		State:            iouringUnknown,
		LibfuseState:     iouringUnknown,
		KernelState:      iouringUnknown,
		EnableUringState: iouringUnknown,
	}
	if off, why := fuseIOUringDisabledByEnv(); off {
		cap.State = iouringUnsupported
		cap.Override = why
		return cap
	}

	// 1. libfuse3 transport. Candidates are ranked so a later `unsupported`
	// never overwrites an earlier `supported`, and an `unknown` (a read
	// failure) is only the answer when nothing else was readable.
	surveyRank := func(s iouringState) int {
		switch s {
		case iouringSupported:
			return 2
		case iouringUnsupported:
			return 1
		default:
			return 0
		}
	}
	best := -1
	paths := libfuse3CandidatePaths()
	if len(paths) == 0 {
		cap.LibfuseDetail = "no libfuse3 shared object was found (searched $BUNKER_LIBFUSE3, `ldconfig -p` and the standard library directories)"
	}
	for _, p := range paths {
		survey, _ := libfuse3Survey(p)
		state, detail := classifyIOUringSymbolSurvey(survey)
		if rank := surveyRank(state); rank > best {
			best = rank
			cap.LibfusePath = p
			cap.LibfuseState = state
			cap.LibfuseDetail = detail
		}
		if state == iouringSupported {
			break
		}
	}
	if cap.LibfuseDetail == "" {
		cap.LibfuseDetail = fmt.Sprintf("%d candidate library path(s) were resolved but none could be surveyed", len(paths))
	}

	// 2. kernel version.
	cap.KernelRelease = kernelRelease()
	cap.KernelState, cap.KernelDetail = classifyKernelRelease(cap.KernelRelease)

	// 3. the kernel's own switch.
	raw, err := readEnableUringParam()
	if err != nil {
		cap.EnableUringDetail = fmt.Sprintf("could not read %s (%v) — this kernel does not expose CONFIG_FUSE_IOURING's switch, or sysfs is not visible here",
			enableUringParamPath, err)
	} else {
		cap.EnableUringRaw = strings.TrimSpace(raw)
		cap.EnableUringState, cap.EnableUringDetail = classifyEnableUringParam(raw)
	}

	// Combine: the FIRST unmet condition is the decisive one, in the same
	// order the enable condition is stated.
	switch {
	case cap.LibfuseState != iouringSupported:
		cap.State = cap.LibfuseState
	case cap.KernelState != iouringSupported:
		cap.State = cap.KernelState
	case cap.EnableUringState != iouringSupported:
		cap.State = cap.EnableUringState
	default:
		cap.State = iouringSupported
	}
	return cap
}

// ── selection ───────────────────────────────────────────────────────────────

// fuseTransportDecision is the resolved transport for one mount invocation.
type fuseTransportDecision struct {
	Requested  string
	Transport  string // FuseTransportFuse | FuseTransportFuseIOUring
	Capability fuseIOUringCapability
}

// ioUringActive reports whether the io_uring transport was selected.
func (d fuseTransportDecision) ioUringActive() bool {
	return d.Transport == FuseTransportFuseIOUring
}

// errInvalidFuseDriver is the named refusal for a `--driver` value outside the
// flag's vocabulary. It is raised before any config load, probe or exec.
func errInvalidFuseDriver(value string) error {
	return fmt.Errorf("invalid --driver %q: must be one of %s, %s, %s (auto prefers the FUSE io_uring transport when this host supports it and falls back to the plain FUSE transport otherwise)",
		value, FuseTransportAuto, FuseTransportFuse, FuseTransportFuseIOUring)
}

// isValidFuseDriver reports whether value is in the flag's vocabulary.
func isValidFuseDriver(value string) bool {
	switch value {
	case FuseTransportAuto, FuseTransportFuse, FuseTransportFuseIOUring:
		return true
	}
	return false
}

// selectFuseTransport is the PURE decision: given what the operator asked for
// and a probe result, which transport does this mount use?
//
//   - fuse          -> the plain transport. The probe result is not consulted
//     (and, upstream of this call, not even computed): this is the
//     exact-old-behaviour switch and the escape hatch.
//   - fuse-iouring  -> the io_uring transport, or a NAMED error listing which
//     of the three conditions is unmet plus the remedy. Never a silent
//     fallback: an operator who asked for this transport must not be handed
//     a different one without knowing.
//   - auto          -> io_uring when the host can provide it, plain otherwise.
//     The caller prints nothing on the fallback path, which is what makes a
//     host without support byte-identical to the pre-MOUNT-008 behaviour.
//   - anything else -> a named refusal listing the vocabulary.
func selectFuseTransport(requested string, cap fuseIOUringCapability) (fuseTransportDecision, error) {
	switch requested {
	case FuseTransportFuse:
		return fuseTransportDecision{Requested: requested, Transport: FuseTransportFuse}, nil
	case FuseTransportAuto:
		if cap.usable() {
			return fuseTransportDecision{Requested: requested, Transport: FuseTransportFuseIOUring, Capability: cap}, nil
		}
		return fuseTransportDecision{Requested: requested, Transport: FuseTransportFuse, Capability: cap}, nil
	case FuseTransportFuseIOUring:
		if cap.usable() {
			return fuseTransportDecision{Requested: requested, Transport: FuseTransportFuseIOUring, Capability: cap}, nil
		}
		return fuseTransportDecision{}, fmt.Errorf(
			"--driver=%s: %s — refusing to mount with a transport this host cannot provide (no mount attempt was made). Re-run with --driver=%s for the plain FUSE transport. To enable it: %s",
			FuseTransportFuseIOUring, cap.String(), FuseTransportFuse, enableCondition())
	default:
		return fuseTransportDecision{}, errInvalidFuseDriver(requested)
	}
}

// resolveFuseTransport is the imperative wrapper the mount command uses: it
// runs the probe only when it could change the answer (`auto` and
// `fuse-iouring`); `fuse` never probes, so selecting the plain transport
// costs nothing and cannot be affected by the host's FUSE build.
func resolveFuseTransport(requested string) (fuseTransportDecision, error) {
	if requested == FuseTransportFuse {
		return selectFuseTransport(requested, fuseIOUringCapability{})
	}
	return selectFuseTransport(requested, probeFuseIOUringCapability())
}

// ── argv shaping ────────────────────────────────────────────────────────────

// hasFuseIOUringOption reports whether the option is already present (both the
// `-o io_uring` pair and the joined `-oio_uring` spelling).
func hasFuseIOUringOption(args []string) bool {
	for i, a := range args {
		if a == "-o" && i+1 < len(args) && args[i+1] == iouringMountOption {
			return true
		}
		if a == "-o"+iouringMountOption {
			return true
		}
	}
	return false
}

// withFuseIOUringOption inserts `-o io_uring` immediately before the trailing
// mountpoint (the argv's last element, whose position the mount path
// contract pins) and is idempotent. The option is passed through
// sshfs -> libfuse3 -> fusermount3 -> mount(2): libfuse3 owns the option, so
// it is the component that consumes it.
func withFuseIOUringOption(args []string) []string {
	if len(args) == 0 || hasFuseIOUringOption(args) {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, args[:len(args)-1]...)
	out = append(out, "-o", iouringMountOption)
	return append(out, args[len(args)-1])
}

// withoutFuseIOUringOption removes the option again — the auto fallback path,
// so the retry runs the exact argv a plain mount would have built.
func withoutFuseIOUringOption(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" && i+1 < len(args) && args[i+1] == iouringMountOption {
			i++
			continue
		}
		if args[i] == "-o"+iouringMountOption {
			continue
		}
		out = append(out, args[i])
	}
	return out
}
