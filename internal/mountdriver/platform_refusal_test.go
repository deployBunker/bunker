// The unsupported-platform refusal's classification (BFS-010).
//
// This is the executable half of the seam's contract: the refusal an operator on a
// platform without a binding would see (internal/fsmount's ErrPlatformUnsupported,
// returned by MountAt on any GOOS other than linux) must be classified PERMANENT
// here. "No binding on this platform" is a LOCAL capability failure — no server was
// contacted and nothing about a retry can change it — so its class must say that.
//
// WHY PERMANENT AND NOT THE `unknown` DEFAULT, precisely: only `transient` is
// retried by the mount loop (mountdriver.go, the FailureClassifier doc), so a
// reworded sentence would not be retried either — it would fall through to
// `unknown`, whose stated meaning is "the output carried no recognised signal".
// The difference this test protects is an ANSWER ("retrying cannot help") versus an
// absence of one, and it is one word away from being lost: remove the phrase from
// internal/fsmount/options.go and all three arms below fail (RED-proved by that
// mutation; see docs/evidence/BFS-010-windows-mint-decision.md §5.6).
//
// It runs on Linux because the sentinel is defined on Linux: pinning the coupling
// needs no Windows host, only the shared sentence. The rest of the seam's behaviour
// (no mount handle, no local side effect, no masked option error) is asserted in
// internal/fsmount/platform_unsupported_test.go, which is type-checked for windows
// by `GOOS=windows go vet ./internal/fsmount`.
package mountdriver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/fsmount"
)

// TestPlatformRefusalIsPermanent pins the classification of the exact sentence the
// operator sees, in both the bare and the platform-suffixed (wrapped) forms the
// seam produces.
func TestPlatformRefusalIsPermanent(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		class ClassifierClass
	}{
		{
			name:  "bare sentinel",
			text:  fsmount.ErrPlatformUnsupported.Error(),
			class: ClassPermanent,
		},
		{
			// What internal/fsmount/fs_unsupported.go returns: the sentinel with the
			// platform appended. Wrapping must not lose the fragment the classifier
			// keys on.
			name:  "seam wrapping with the platform",
			text:  fmt.Errorf("%w — this build is windows/amd64, and a mount needs a binding for it", fsmount.ErrPlatformUnsupported).Error(),
			class: ClassPermanent,
		},
		{
			// The shape a CLI prints: the sentinel wrapped again by a caller.
			name:  "cli-style double wrap",
			text:  fmt.Errorf("mount /mnt/x: %w", fsmount.ErrPlatformUnsupported).Error(),
			class: ClassPermanent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, fragment := classifyBunkerFS(tc.text, nil)
			if class != tc.class {
				t.Fatalf("classifyBunkerFS(%q) = %v (%q), want %v — the platform refusal must carry a STATED class (\"retrying cannot help\"), not the `unknown` default (\"no recognised signal\")",
					tc.text, class, fragment, tc.class)
			}
			if !strings.Contains(fragment, "no fuse binding on this platform") {
				t.Errorf("matched fragment %q: the classifier must match on the platform sentence itself, not on incidental words", fragment)
			}
		})
	}
}

// TestPlatformRefusalMessageNamesTheWayOut: the refusal is the only thing an
// operator on an unsupported platform gets, so it has to say what to do instead
// (the default driver, or a stock WebDAV client) rather than only what is missing.
// This asserts the TEXT; the seam's behaviour is asserted in internal/fsmount.
func TestPlatformRefusalMessageNamesTheWayOut(t *testing.T) {
	msg := fsmount.ErrPlatformUnsupported.Error()
	for _, want := range []string{"sshfs", "WebDAV"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the platform refusal does not mention %q, so it names the problem without the way out: %q", want, msg)
		}
	}
}
