package cli

// MOUNT-008 tests for the FUSE-over-io_uring capability probe.
//
// Everything in this file is hermetic: the probe's four host seams
// (libfuse3CandidatePaths, libfuse3Survey, kernelRelease,
// readEnableUringParam) are replaced with fixtures, so the suite passes on a
// host with NO libfuse3, NO io_uring and NO /sys — and on a host that has all
// three. The classification layer is pure and table-driven.

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ── fixtures ────────────────────────────────────────────────────────────────

// fixtureSurveySupportedIOUring is the real shape measured on the control host
// (2026-10-14): a `nm -D` dynamic symbol plus libfuse's own option/usage text.
const fixtureSurveySupportedIOUring = `--- nm -D /usr/lib/x86_64-linux-gnu/libfuse3.so.3.18.2 ---
0000000000005f30 T fuse_mount@@FUSE_3.18
0000000000018460 T fuse_req_is_uring@@FUSE_3.18
0000000000012f60 T fuse_session_new@@FUSE_3.18
--- strings /usr/lib/x86_64-linux-gnu/libfuse3.so.3.18.2 ---
auto_unmount
default_permissions
io_uring
io_uring_q_depth=%u
    -o io_uring            enable io-uring
    -o io_uring_q_depth=<n> io-uring queue depth
`

// fixtureSurveyUnsupportedIOUring is a libfuse3 build WITHOUT the transport:
// the survey read the library successfully (fuse symbols and option strings
// are present) and carries no io_uring anywhere. This is what licenses an
// `unsupported` verdict.
const fixtureSurveyUnsupportedIOUring = `--- nm -D /usr/lib/x86_64-linux-gnu/libfuse3.so.3.16.2 ---
0000000000005f30 T fuse_mount@@FUSE_3.16
0000000000006030 T fuse_unmount@@FUSE_3.16
0000000000012f60 T fuse_session_new@@FUSE_3.16
--- strings /usr/lib/x86_64-linux-gnu/libfuse3.so.3.16.2 ---
auto_unmount
default_permissions
allow_other
kernel_cache
`

// fixtureSurveyNoSuchFile is the fixture that matters most: it is EXACTLY the
// survey the stale docs/mount-drivers.md §3.3 record produced —
// `nm -D /usr/lib/x86_64-linux-gnu/libfuse3.so.3` on a host whose SONAME is
// .so.4. The file does not exist, the tool fails, and the absence of output
// was misread as "this libfuse has no io_uring transport". It must classify
// as UNKNOWN, never as unsupported.
const fixtureSurveyNoSuchFile = `--- nm: exit status 1 ---
nm: '/usr/lib/x86_64-linux-gnu/libfuse3.so.3': No such file
--- strings: exit status 1 ---
strings: '/usr/lib/x86_64-linux-gnu/libfuse3.so.3': No such file
`

// fixtureSurveyNotALibfuse is a survey of something that is not a libfuse
// build at all — no fuse evidence. Nothing can be concluded.
const fixtureSurveyNotALibfuse = `--- nm -D /opt/other/libsomething.so.1 ---
0000000000001234 T malloc@@GLIBC_2.2.5
0000000000001235 T free@@GLIBC_2.2.5
`

// ── unit: the symbol-survey classifier ──────────────────────────────────────

func TestClassifyIOUringSymbolSurvey(t *testing.T) {
	cases := []struct {
		name       string
		survey     string
		wantState  iouringState
		wantReason string
		mustNotSay string
	}{
		{
			name:       "supported: dynamic symbol fuse_req_is_uring",
			survey:     "--- nm -D /usr/lib/x86_64-linux-gnu/libfuse3.so.3.18.2 ---\n0000000000018460 T fuse_req_is_uring@@FUSE_3.18\n",
			wantState:  iouringSupported,
			wantReason: "fuse_req_is_uring",
		},
		{
			name:       "supported: libfuse option/usage table",
			survey:     fixtureSurveySupportedIOUring,
			wantState:  iouringSupported,
			wantReason: "io_uring",
		},
		{
			name:       "supported: hyphen spelling from usage text",
			survey:     "--- strings /usr/lib/libfuse3.so.3 ---\n    -o io-uring            enable io-uring\n",
			wantState:  iouringSupported,
			wantReason: "io-uring",
		},
		{
			name:       "supported: underscore-less spelling",
			survey:     "--- strings libfuse3.so ---\nfuse: iouring queue depth\n",
			wantState:  iouringSupported,
			wantReason: "iouring",
		},
		{
			name:       "unsupported: libfuse read successfully, no transport",
			survey:     fixtureSurveyUnsupportedIOUring,
			wantState:  iouringUnsupported,
			wantReason: "read successfully",
		},
		{
			name:       "unsupported: only option strings, still no transport",
			survey:     "auto_unmount\ndefault_permissions\nallow_other\n",
			wantState:  iouringUnsupported,
			wantReason: "no io_uring transport",
		},
		{
			// "uring" must only count as the transport when it is its own
			// token: the words inside this fixture would otherwise be a
			// false-positive.
			name:       "unsupported: uring inside unrelated words is not evidence",
			survey:     "--- strings libfuse3.so.4 ---\nfuse_session_new\nmeasuring buffer sizes while securing the session\n",
			wantState:  iouringUnsupported,
			wantReason: "no io_uring transport",
		},
		{
			// The stale-record fixture. A missing file must NOT read as
			// "this libfuse lacks the transport".
			name:       "unknown: probe path did not exist (the §3.3 misread)",
			survey:     fixtureSurveyNoSuchFile,
			wantState:  iouringUnknown,
			wantReason: "no such file",
			mustNotSay: "no io_uring transport",
		},
		{
			name:       "unknown: empty survey",
			survey:     "   \n\n",
			wantState:  iouringUnknown,
			wantReason: "empty",
		},
		{
			name:       "unknown: no fuse evidence at all",
			survey:     fixtureSurveyNotALibfuse,
			wantState:  iouringUnknown,
			wantReason: "no fuse evidence",
			mustNotSay: "no io_uring transport",
		},
		{
			name:       "unknown: tool missing",
			survey:     "--- nm: exec: \"nm\": executable file not found in $PATH ---\n",
			wantState:  iouringUnknown,
			wantReason: "not found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, reason := classifyIOUringSymbolSurvey(tc.survey)
			if state != tc.wantState {
				t.Errorf("state = %q, want %q (reason: %s)", state, tc.wantState, reason)
			}
			if tc.wantReason != "" && !strings.Contains(strings.ToLower(reason), strings.ToLower(tc.wantReason)) {
				t.Errorf("reason %q does not mention %q", reason, tc.wantReason)
			}
			if tc.mustNotSay != "" && strings.Contains(strings.ToLower(reason), strings.ToLower(tc.mustNotSay)) {
				t.Errorf("reason %q must not claim %q", reason, tc.mustNotSay)
			}
			if reason == "" {
				t.Error("every verdict must carry a named reason")
			}
		})
	}
}

// ── unit: the kernel-release classifier ─────────────────────────────────────

func TestClassifyKernelRelease(t *testing.T) {
	cases := []struct {
		name      string
		release   string
		wantState iouringState
	}{
		{"control host 7.0.0-31-generic", "7.0.0-31-generic", iouringSupported},
		{"exact floor 6.14", "6.14", iouringSupported},
		{"floor with patch suffix", "6.14.0-11-generic", iouringSupported},
		{"later major", "8.1.0", iouringSupported},
		{"just below the floor", "6.13.9-100-generic", iouringUnsupported},
		{"bunker-mvp 6.8.0-117-generic", "6.8.0-117-generic", iouringUnsupported},
		{"old LTS", "5.15.0-100-generic", iouringUnsupported},
		{"empty", "", iouringUnknown},
		{"not a version", "linux", iouringUnknown},
		{"numeric but no minor", "7", iouringUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, reason := classifyKernelRelease(tc.release)
			if state != tc.wantState {
				t.Errorf("release %q => %q, want %q (reason: %s)", tc.release, state, tc.wantState, reason)
			}
			if reason == "" {
				t.Error("every verdict must carry a named reason")
			}
		})
	}
}

// ── unit: the enable_uring parameter classifier ─────────────────────────────

func TestClassifyEnableUringParam(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantState iouringState
		wantText  string
	}{
		{"disabled (control host)", "N\n", iouringUnsupported, "echo 1 >"},
		{"disabled zero", "0", iouringUnsupported, "DISABLED"},
		{"disabled off", "off\n", iouringUnsupported, "DISABLED"},
		{"enabled Y", "Y\n", iouringSupported, ""},
		{"enabled 1", "1", iouringSupported, ""},
		{"enabled on", "on\n", iouringSupported, ""},
		{"empty reads as unknown", "", iouringUnknown, "neither an enabled nor a disabled value"},
		{"garbage reads as unknown", "maybe\n", iouringUnknown, "neither an enabled nor a disabled value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, reason := classifyEnableUringParam(tc.raw)
			if state != tc.wantState {
				t.Errorf("raw %q => %q, want %q (reason: %s)", tc.raw, state, tc.wantState, reason)
			}
			if tc.wantText != "" && !strings.Contains(reason, tc.wantText) {
				t.Errorf("reason %q does not contain %q", reason, tc.wantText)
			}
		})
	}
}

// ── unit: the failure classifier ────────────────────────────────────────────

func TestClassifyIOUringMountFailure(t *testing.T) {
	cases := []struct {
		name      string
		output    string
		wantNamed bool
	}{
		{
			name:      "libfuse rejects the option (unknown option)",
			output:    "fuse: unknown option(s): `-o io_uring'\n",
			wantNamed: true,
		},
		{
			name:      "kernel switch named",
			output:    "fuse: io_uring not enabled; set enable_uring=1\n",
			wantNamed: true,
		},
		{
			name:      "transport reported unsupported",
			output:    "fuse: mount failed: io_uring transport not supported by this kernel\n",
			wantNamed: true,
		},
		{
			name:      "option unrecognised in the other order",
			output:    "unrecognized option '-o io_uring'\n",
			wantNamed: true,
		},
		{
			name:      "generic EINVAL is NOT transport-specific",
			output:    "fusermount3: mount failed: Invalid argument\n",
			wantNamed: false,
		},
		{
			name:      "transport blip is NOT transport-specific",
			output:    "read: Connection reset by peer\n",
			wantNamed: false,
		},
		{
			name:      "auth failure is NOT transport-specific",
			output:    "Permission denied (publickey).\r\n",
			wantNamed: false,
		},
		{
			name:      "empty output",
			output:    "",
			wantNamed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			named, reason := classifyIOUringMountFailure(tc.output)
			if named != tc.wantNamed {
				t.Fatalf("named = %v, want %v (reason: %q)", named, tc.wantNamed, reason)
			}
			if named && reason == "" {
				t.Error("a transport-specific verdict must quote the matched output")
			}
		})
	}
}

// ── unit: argv shaping ──────────────────────────────────────────────────────

func TestFuseIOUringOptionShaping(t *testing.T) {
	plain := []string{"sshfs", "-o", "reconnect", "-o", "auto_unmount", "user@host:/srv", "/mnt/mnt"}

	with := withFuseIOUringOption(plain)
	if !hasFuseIOUringOption(with) {
		t.Fatalf("option was not added: %q", with)
	}
	if with[len(with)-1] != "/mnt/mnt" {
		t.Errorf("mountpoint must stay last, got %q", with)
	}
	// The option must sit immediately before the mountpoint: the position the
	// mount-path contract pins.
	if with[len(with)-2] != iouringMountOption || with[len(with)-3] != "-o" {
		t.Errorf("option not adjacent to the mountpoint: %q", with)
	}
	if got := withFuseIOUringOption(with); len(got) != len(with) {
		t.Errorf("adding twice must be idempotent: %q vs %q", got, with)
	}
	back := withoutFuseIOUringOption(with)
	if len(back) != len(plain) {
		t.Fatalf("removal did not restore the argv: %q vs %q", back, plain)
	}
	for i := range plain {
		if back[i] != plain[i] {
			t.Errorf("argv[%d] = %q, want %q (removal must be byte-exact)", i, back[i], plain[i])
		}
	}
	if hasFuseIOUringOption(plain) {
		t.Error("the plain argv must not read as carrying the option")
	}
	// The joined spelling is recognised too (some callers pass -oio_uring).
	if !hasFuseIOUringOption([]string{"sshfs", "-o" + iouringMountOption, "/mnt"}) {
		t.Error("the joined -oio_uring spelling must be recognised")
	}
	if got := withoutFuseIOUringOption([]string{"sshfs", "-o" + iouringMountOption, "/mnt"}); len(got) != 2 {
		t.Errorf("joined spelling not removed: %q", got)
	}
	if got := withFuseIOUringOption(nil); len(got) != 0 {
		t.Errorf("empty argv must be returned unchanged, got %q", got)
	}
}

// ── unit: transport selection ───────────────────────────────────────────────

func TestSelectFuseTransport(t *testing.T) {
	supported := fuseIOUringCapability{State: iouringSupported, LibfusePath: "/usr/lib/libfuse3.so.4"}
	unsupportedHost := fuseIOUringCapability{State: iouringUnsupported, LibfuseState: iouringUnsupported, LibfuseDetail: "no io_uring transport", KernelState: iouringSupported, EnableUringState: iouringSupported}
	unknownHost := fuseIOUringCapability{State: iouringUnknown, LibfuseState: iouringUnknown, LibfuseDetail: "the survey is empty"}

	cases := []struct {
		name          string
		requested     string
		cap           fuseIOUringCapability
		wantTransport string
		wantErrText   []string
	}{
		{"auto on a capable host prefers io_uring", FuseTransportAuto, supported, FuseTransportFuseIOUring, nil},
		{"auto on an unsupported host falls back silently", FuseTransportAuto, unsupportedHost, FuseTransportFuse, nil},
		{"auto on an unknown host falls back silently", FuseTransportAuto, unknownHost, FuseTransportFuse, nil},
		{"fuse never consults the probe", FuseTransportFuse, supported, FuseTransportFuse, nil},
		{"fuse-iouring accepted on a capable host", FuseTransportFuseIOUring, supported, FuseTransportFuseIOUring, nil},
		{"fuse-iouring refused on an unsupported host", FuseTransportFuseIOUring, unsupportedHost, "", []string{
			FuseTransportFuseIOUring, "refusing", "--driver=fuse", "enable_uring",
		}},
		{"fuse-iouring refused on an unknown host", FuseTransportFuseIOUring, unknownHost, "", []string{
			FuseTransportFuseIOUring, "refusing", "the survey is empty",
		}},
		{"unknown value refused with the vocabulary", "fuse-io-uring", supported, "", []string{
			"invalid --driver", FuseTransportAuto, FuseTransportFuse, FuseTransportFuseIOUring,
		}},
		{"empty value refused", "", supported, "", []string{"invalid --driver"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectFuseTransport(tc.requested, tc.cap)
			if len(tc.wantErrText) > 0 {
				if err == nil {
					t.Fatalf("expected a refusal, got transport %q", got.Transport)
				}
				for _, want := range tc.wantErrText {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not mention %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Transport != tc.wantTransport {
				t.Errorf("transport = %q, want %q", got.Transport, tc.wantTransport)
			}
		})
	}
}

// ── the probe against stubbed hosts ─────────────────────────────────────────

// iouringHostFixture stands in for a host: what its libfuse3 survey says, its
// kernel release, and its enable_uring parameter value.
type iouringHostFixture struct {
	survey     string
	noLibrary  bool
	release    string
	paramRaw   string
	paramErr   error
	surveySeen *int
}

// stubIOUringHost replaces the probe's four host seams so the capability
// matrix below can be exercised anywhere. It also pins BUNKER_FUSE_IOURING to
// the probing mode, because the package suite (TestMain) pins it to "off" so
// the ordinary mount tests do not depend on the test host's FUSE build.
func stubIOUringHost(t *testing.T, fx iouringHostFixture) {
	t.Helper()
	t.Setenv(fuseIOUringEnv, "auto")

	oldPaths, oldSurvey, oldRelease, oldParam := libfuse3CandidatePaths, libfuse3Survey, kernelRelease, readEnableUringParam
	t.Cleanup(func() {
		libfuse3CandidatePaths, libfuse3Survey, kernelRelease, readEnableUringParam = oldPaths, oldSurvey, oldRelease, oldParam
	})

	libfuse3CandidatePaths = func() []string {
		if fx.noLibrary {
			return nil
		}
		return []string{"/fixture/lib/libfuse3.so.4"}
	}
	libfuse3Survey = func(path string) (string, error) {
		if fx.surveySeen != nil {
			*fx.surveySeen++
		}
		return fx.survey, nil
	}
	kernelRelease = func() string { return fx.release }
	readEnableUringParam = func() (string, error) { return fx.paramRaw, fx.paramErr }
}

func TestProbeFuseIOUringCapability_StubbedHosts(t *testing.T) {
	cases := []struct {
		name         string
		fx           iouringHostFixture
		wantState    iouringState
		wantContains []string
	}{
		{
			name: "all three conditions met",
			fx: iouringHostFixture{
				survey: fixtureSurveySupportedIOUring, release: "7.0.0-31-generic", paramRaw: "Y\n",
			},
			wantState:    iouringSupported,
			wantContains: []string{"supported", "libfuse3", "7.0.0-31-generic", "enable_uring"},
		},
		{
			name: "libfuse lacks the transport",
			fx: iouringHostFixture{
				survey: fixtureSurveyUnsupportedIOUring, release: "7.0.0-31-generic", paramRaw: "Y\n",
			},
			wantState:    iouringUnsupported,
			wantContains: []string{"libfuse3: unsupported", "no io_uring transport"},
		},
		{
			name: "libfuse read failed (unknown, not unsupported)",
			fx: iouringHostFixture{
				survey: fixtureSurveyNoSuchFile, release: "7.0.0-31-generic", paramRaw: "Y\n",
			},
			wantState:    iouringUnknown,
			wantContains: []string{"libfuse3: unknown"},
		},
		{
			name: "no libfuse3 on the host",
			fx: iouringHostFixture{
				noLibrary: true, release: "7.0.0-31-generic", paramRaw: "Y\n",
			},
			wantState:    iouringUnknown,
			wantContains: []string{"no libfuse3 shared object was found"},
		},
		{
			name: "kernel below the floor",
			fx: iouringHostFixture{
				survey: fixtureSurveySupportedIOUring, release: "6.8.0-117-generic", paramRaw: "Y\n",
			},
			wantState:    iouringUnsupported,
			wantContains: []string{"kernel: unsupported", "6.8.0-117-generic"},
		},
		{
			name: "kernel release unreadable",
			fx: iouringHostFixture{
				survey: fixtureSurveySupportedIOUring, release: "", paramRaw: "Y\n",
			},
			wantState:    iouringUnknown,
			wantContains: []string{"kernel: unknown"},
		},
		{
			// The measured state of the control host: library and kernel are
			// ready, the module parameter is off.
			name: "kernel switch disabled (control host)",
			fx: iouringHostFixture{
				survey: fixtureSurveySupportedIOUring, release: "7.0.0-31-generic", paramRaw: "N\n",
			},
			wantState:    iouringUnsupported,
			wantContains: []string{"kernel module parameter: unsupported", "echo 1 >", enableUringParamPath},
		},
		{
			name: "kernel switch unreadable",
			fx: iouringHostFixture{
				survey: fixtureSurveySupportedIOUring, release: "7.0.0-31-generic", paramErr: os.ErrNotExist,
			},
			wantState:    iouringUnknown,
			wantContains: []string{"could not read", enableUringParamPath},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubIOUringHost(t, tc.fx)
			cap := probeFuseIOUringCapability()
			if cap.State != tc.wantState {
				t.Fatalf("state = %q, want %q\n%s", cap.State, tc.wantState, cap.String())
			}
			if cap.usable() != (tc.wantState == iouringSupported) {
				t.Errorf("usable() = %v for state %q", cap.usable(), cap.State)
			}
			text := cap.String()
			for _, want := range tc.wantContains {
				if !strings.Contains(text, want) {
					t.Errorf("finding %q does not mention %q", text, want)
				}
			}
		})
	}
}

// TestProbeFuseIOUringCapability_EnvOverride pins the operator/suite escape
// hatch: BUNKER_FUSE_IOURING=off must short-circuit every probe (no survey is
// run at all) and report a named reason.
func TestProbeFuseIOUringCapability_EnvOverride(t *testing.T) {
	for _, value := range []string{"off", "0", "disabled", "never"} {
		t.Run(value, func(t *testing.T) {
			seen := 0
			stubIOUringHost(t, iouringHostFixture{
				survey: fixtureSurveySupportedIOUring, release: "7.0.0-31-generic", paramRaw: "Y\n", surveySeen: &seen,
			})
			t.Setenv(fuseIOUringEnv, value)

			cap := probeFuseIOUringCapability()
			if cap.State != iouringUnsupported {
				t.Fatalf("state = %q, want unsupported", cap.State)
			}
			if seen != 0 {
				t.Errorf("the override must skip host probing entirely; survey ran %d time(s)", seen)
			}
			if !strings.Contains(cap.String(), fuseIOUringEnv) {
				t.Errorf("reason must name the override variable: %q", cap.String())
			}
			// And the selection layer must then refuse the explicit request
			// while letting auto fall back.
			if _, err := selectFuseTransport(FuseTransportFuseIOUring, cap); err == nil {
				t.Error("fuse-iouring must still be refused when the override pins the plain transport")
			}
			dec, err := selectFuseTransport(FuseTransportAuto, cap)
			if err != nil || dec.Transport != FuseTransportFuse {
				t.Errorf("auto must fall back under the override: %q, %v", dec.Transport, err)
			}
		})
	}
}

// TestProbeFuseIOUringCapability_DoesNotProbeForPlainFuse pins the "fuse never
// probes" contract through the imperative wrapper the command uses.
func TestProbeFuseIOUringCapability_DoesNotProbeForPlainFuse(t *testing.T) {
	seen := 0
	stubIOUringHost(t, iouringHostFixture{
		survey: fixtureSurveySupportedIOUring, release: "7.0.0-31-generic", paramRaw: "Y\n", surveySeen: &seen,
	})
	dec, err := resolveFuseTransport(FuseTransportFuse)
	if err != nil {
		t.Fatalf("resolveFuseTransport(fuse): %v", err)
	}
	if dec.Transport != FuseTransportFuse {
		t.Errorf("transport = %q, want %q", dec.Transport, FuseTransportFuse)
	}
	if seen != 0 {
		t.Errorf("--driver=fuse must not survey the host; survey ran %d time(s)", seen)
	}
}

// TestProbeFuseIOUringCapability_LiveHost is a smoke test of the real IO layer
// (ldconfig/glob discovery, nm/strings survey, /proc + /sys reads). It asserts
// only that the probe COMPLETES and returns a valid verdict — never a
// particular host's answer — so it passes on any machine, including one with
// no libfuse3 and no /sys.
func TestProbeFuseIOUringCapability_LiveHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe's host IO layer (ldconfig/nm/strings/sysfs) is unix-only")
	}
	t.Setenv(fuseIOUringEnv, "auto")
	t.Setenv("BUNKER_LIBFUSE3", "")
	t.Setenv("PATH", os.Getenv("PATH"))

	start := time.Now()
	var cap fuseIOUringCapability
	done := make(chan struct{})
	go func() {
		defer close(done)
		cap = probeFuseIOUringCapability()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the probe hung — it must always be bounded")
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Errorf("the probe took %s; every survey command is bounded by %s", elapsed, iouringProbeCommandTimeout)
	}
	switch cap.State {
	case iouringSupported, iouringUnsupported, iouringUnknown:
	default:
		t.Fatalf("probe returned an out-of-vocabulary state %q", cap.State)
	}
	if got := cap.String(); !strings.Contains(got, "FUSE io_uring transport") {
		t.Errorf("finding is not operator-readable: %q", got)
	}
	t.Logf("live probe on this host: %s", cap.String())
}

// pinPlainFuseTransportForSuite pins the plain FUSE transport for the whole
// CLI test package (called from TestMain). Every existing mount test asserts
// an argv shape that predates this row; without the pin those assertions would
// depend on the test host's kernel and libfuse build (a host with
// enable_uring=1 would gain `-o io_uring` and fail them). Tests that need the
// probe override this per-test with stubIOUringHost.
func pinPlainFuseTransportForSuite() {
	_ = os.Setenv(fuseIOUringEnv, "off")
}
