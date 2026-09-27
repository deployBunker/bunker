package webdav

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is BFS-015's proof for the one rule BFS-004 §6.1 step 5 added to
// the write path after the surface had already landed: the precondition is
// re-validated INSIDE the commit, immediately before the rename.
//
// The trap the row names is explicit, so it is named here too: a test that
// mutates the target BEFORE the request starts proves nothing, and neither
// does one whose mutation lands before the early evaluation runs — the handler
// reads the whole body first, so a write that lands during the transfer is
// caught by the early evaluation unless a metadata-keyed shortcut hides it.
// The cases below therefore each say WHERE their out-of-band write lands:
//
//   - "while_the_body_arrives": inside the request body's first Read. The
//     early evaluation still runs afterwards, so the case is only a window
//     case because the writer restores size and mtime — the one thing §7.3
//     forbids the hash cache from deciding, which is why the commit-time
//     re-validation reads bytes.
//   - "inside_the_commit": driven by the handler's test seam, inside §6.1
//     step 5's critical section, after the early evaluation has passed and
//     before the re-validation runs. This is the window itself.
//   - "before_the_precondition_is_evaluated": the row's trap, kept as a
//     labelled CONTROL — it passes against the unfixed write path too, and
//     that is exactly why it proves nothing.

// outOfBandBody is a request body that performs the out-of-band write on its
// first Read — i.e. while the request's own body is still arriving.
type outOfBandBody struct {
	reader *strings.Reader
	once   sync.Once
	write  func() error
	err    error
}

func newOutOfBandBody(body string, write func() error) *outOfBandBody {
	return &outOfBandBody{reader: strings.NewReader(body), write: write}
}

func (b *outOfBandBody) Read(p []byte) (int, error) {
	b.once.Do(func() { b.err = b.write() })
	return b.reader.Read(p)
}

// doReader drives one request whose body is supplied as a stream, so a test
// can act while the handler is still reading it.
func doReader(t *testing.T, h http.Handler, method, target string, headers map[string]string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// baseHashOf reads the validator a client would hold for a path: one HEAD, the
// cheap "give me the base hash" call of §7.4.
func baseHashOf(t *testing.T, h *Handler, target string) string {
	t.Helper()
	rec := do(t, h, "HEAD", target, nil, "")
	base := normalizeTag(rec.Header().Get("ETag"))
	if base == "" {
		t.Fatalf("HEAD %s did not return an ETag: %v", target, rec.Header())
	}
	return base
}

// noStagingLeft asserts AC-6's "no partial file is ever visible" from the
// other side: a refused write must not leave its temp sibling behind.
func noStagingLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	for _, e := range entries {
		if isTempName(e.Name()) {
			t.Fatalf("a staging file survived the refusal: %s", e.Name())
		}
	}
}

// assertHashRefusal asserts the refusal shape §6.1 fixes for a base that moved
// (both hashes, 412, hash_mismatch) AND that the bytes which were on disk when
// the write would have landed are the bytes still on disk.
func assertHashRefusal(t *testing.T, rec *httptest.ResponseRecorder, path, base, surviving string) {
	t.Helper()
	if rec.Code != 412 {
		t.Fatalf("status = %d, want 412 (a base that moved during the transfer must be refused, not overwritten); body=%s",
			rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictHashMismatch) {
		t.Fatalf("verdict = %q, want hash_mismatch", got)
	}
	if got, want := rec.Header().Get("X-Bunker-Current-Hash"), contentHash(surviving); got != want {
		t.Fatalf("X-Bunker-Current-Hash = %q, want the hash of the bytes on disk %q", got, want)
	}
	if got := rec.Header().Get("X-Bunker-Expected-Hash"); got != base {
		t.Fatalf("X-Bunker-Expected-Hash = %q, want %q", got, base)
	}
	refusal := rec.Body.String()
	if !strings.Contains(refusal, "<b:expected>"+base+"</b:expected>") ||
		!strings.Contains(refusal, "<b:current>"+contentHash(surviving)+"</b:current>") {
		t.Fatalf("the refusal body does not name both hashes: %s", refusal)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != surviving {
		t.Fatalf("the refused write clobbered the file: got %q, want the bytes that landed during the transfer %q",
			got, surviving)
	}
	noStagingLeft(t, filepath.Dir(path))
}

// TestExpectedHashIsRevalidatedInsideTheCommit is acceptance criterion 2 for
// the If-Match rule: a conditional PUT whose target changes after its
// precondition was evaluated is REFUSED with 412 + hash_mismatch, and the
// bytes that landed in the meantime survive.
func TestExpectedHashIsRevalidatedInsideTheCommit(t *testing.T) {
	const arriving = "package main\n\nfunc main() { /* A's edit */ }\n"

	t.Run("while_the_body_arrives", func(t *testing.T) {
		h := newTestHandler(t)
		path := filepath.Join(h.Root(), "src", "main.go")
		base := baseHashOf(t, h, "/dav/src/main.go")
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		// Writer B overwrites the file in place, SAME SIZE, SAME MTIME: the
		// case a metadata-keyed shortcut cannot see. A's own body is still
		// arriving when B lands.
		outOfBand := strings.Replace(fixtureMainBody, "main() {}", "util() {}", 1)
		if len(outOfBand) != len(fixtureMainBody) || outOfBand == fixtureMainBody || outOfBand == arriving {
			t.Fatalf("the out-of-band body is not a distinct same-size rewrite: %q", outOfBand)
		}
		body := newOutOfBandBody(arriving, func() error {
			if err := os.WriteFile(path, []byte(outOfBand), 0o644); err != nil {
				return err
			}
			return os.Chtimes(path, fi.ModTime(), fi.ModTime())
		})

		rec := doReader(t, h, "PUT", "/dav/src/main.go", map[string]string{
			"If-Match": `"` + base + `"`,
		}, body)
		if body.err != nil {
			t.Fatalf("the out-of-band write failed: %v", body.err)
		}
		assertHashRefusal(t, rec, path, base, outOfBand)
	})

	t.Run("inside_the_commit", func(t *testing.T) {
		h := newTestHandler(t)
		path := filepath.Join(h.Root(), "src", "main.go")
		base := baseHashOf(t, h, "/dav/src/main.go")
		// A plain `echo > file`: different size, fresh mtime. The write lands
		// after the early evaluation has already passed.
		outOfBand := "package main\n\n// writer B got here during A's transfer\n"
		oobErr := new(error)
		h.testBeforeCommit = func(string) {
			if err := os.WriteFile(path, []byte(outOfBand), 0o644); err != nil {
				*oobErr = err
			}
		}

		rec := do(t, h, "PUT", "/dav/src/main.go", map[string]string{
			"If-Match": `"` + base + `"`,
		}, arriving)
		if *oobErr != nil {
			t.Fatalf("the out-of-band write failed: %v", *oobErr)
		}
		assertHashRefusal(t, rec, path, base, outOfBand)
	})

	t.Run("inside_the_commit_with_identical_bytes", func(t *testing.T) {
		h := newTestHandler(t)
		path := filepath.Join(h.Root(), "src", "main.go")
		base := baseHashOf(t, h, "/dav/src/main.go")
		// The base moved, but to exactly the bytes A is sending: D3's
		// reported no-op — a success, not a refusal — and no disk write.
		h.testBeforeCommit = func(string) {
			if err := os.WriteFile(path, []byte(arriving), 0o644); err != nil {
				t.Errorf("out-of-band write: %v", err)
			}
		}

		rec := do(t, h, "PUT", "/dav/src/main.go", map[string]string{
			"If-Match": `"` + base + `"`,
		}, arriving)
		if rec.Code != 204 {
			t.Fatalf("status = %d, want 204 (the requested change already happened); body=%s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictIdenticalContent) {
			t.Fatalf("verdict = %q, want identical_content", got)
		}
		if got := rec.Header().Get("X-Bunker-Noop"); got != "1" {
			t.Fatalf("X-Bunker-Noop = %q, want 1", got)
		}
		if got, want := rec.Header().Get("ETag"), `"`+contentHash(arriving)+`"`; got != want {
			t.Fatalf("ETag = %q, want %q", got, want)
		}
		noStagingLeft(t, filepath.Dir(path))
	})
}

// TestCreateOnlyRuleIsRevalidatedInsideTheCommit is acceptance criterion 2 for
// rule two (§6.1 step 4 / §8.2): an If-None-Match: * create whose target
// appears while the request is in flight is REFUSED rather than clobbering
// what appeared.
func TestCreateOnlyRuleIsRevalidatedInsideTheCommit(t *testing.T) {
	const arriving = "package main\n\nfunc createdByA() {}\n"
	const outOfBand = "package main\n\nfunc createdByB() {}\n"

	// assertCreateRefusal: 412 + precondition_failed (the absence case carries
	// no hash detail, §5.1), and the resource that appeared is untouched.
	assertCreateRefusal := func(t *testing.T, rec *httptest.ResponseRecorder, path string) {
		t.Helper()
		if rec.Code != 412 {
			t.Fatalf("status = %d, want 412 (create-only against a resource that appeared mid-transfer); body=%s",
				rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictPreconditionFailed) {
			t.Fatalf("verdict = %q, want precondition_failed", got)
		}
		if got := rec.Header().Get("X-Bunker-Current-Hash"); got != "" {
			t.Fatalf("precondition_failed carried a current hash: %q", got)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the refused create-only did not leave the resource that appeared: %v", err)
		}
		if string(got) != outOfBand {
			t.Fatalf("the refused create-only clobbered what appeared: got %q, want %q", got, outOfBand)
		}
		noStagingLeft(t, filepath.Dir(path))
	}

	t.Run("inside_the_commit", func(t *testing.T) {
		h := newTestHandler(t)
		path := filepath.Join(h.Root(), "src", "appeared.go")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("fixture precondition: %s should be unmapped (%v)", path, err)
		}
		h.testBeforeCommit = func(string) {
			if err := os.WriteFile(path, []byte(outOfBand), 0o644); err != nil {
				t.Errorf("out-of-band create: %v", err)
			}
		}

		rec := do(t, h, "PUT", "/dav/src/appeared.go", map[string]string{"If-None-Match": "*"}, arriving)
		assertCreateRefusal(t, rec, path)
	})

	t.Run("before_the_precondition_is_evaluated", func(t *testing.T) {
		// THE TRAP, kept as a labelled control: this mutation lands while the
		// body is arriving, i.e. BEFORE the early evaluation runs, so the
		// early evaluation catches it. It passes against the unfixed write
		// path and against the fixed one — which is the point: only the
		// window cases above can tell the two apart.
		h := newTestHandler(t)
		path := filepath.Join(h.Root(), "src", "appeared.go")
		body := newOutOfBandBody(arriving, func() error {
			return os.WriteFile(path, []byte(outOfBand), 0o644)
		})
		rec := doReader(t, h, "PUT", "/dav/src/appeared.go", map[string]string{"If-None-Match": "*"}, body)
		assertCreateRefusal(t, rec, path)
	})
}

// TestCommitSectionIsExclusive proves §6.1 step 5's "in the same critical
// section as the rename" is real and wired: the test seam fires INSIDE that
// section, so with the path's commit lock held by one writer no second writer
// can be in it. The writers send NO precondition, so every one of them passes
// its early evaluation and reaches the commit — the overlap this test measures
// is the one the lock has to prevent.
func TestCommitSectionIsExclusive(t *testing.T) {
	const writers = 4
	h := newTestHandler(t)

	var mu sync.Mutex
	inside, maxInside := 0, 0
	h.testBeforeCommit = func(string) {
		mu.Lock()
		inside++
		if inside > maxInside {
			maxInside = inside
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond) // hold the section so overlap is visible
		mu.Lock()
		inside--
		mu.Unlock()
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := do(t, h, "PUT", "/dav/src/main.go", nil, fmt.Sprintf("package main\n\nfunc w%d() {}\n", i))
			if rec.Code != 204 {
				t.Errorf("writer %d -> %d (%s)", i, rec.Code, rec.Body.String())
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if maxInside != 1 {
		t.Fatalf("%d writers were inside §6.1 step 5's critical section at once, want exactly 1 "+
			"(the re-validation and the rename are one section per path)", maxInside)
	}
}

// TestConcurrentConditionalWritesCannotBothLand is the other half of step 5:
// the re-validation and the rename are ONE critical section per path, so
// writers holding the SAME base cannot both land — the losers re-validate
// under the lock and are refused. Without the lock, two writers that both
// passed their evaluation could both rename, and the later rename would
// silently discard the earlier write: the lost update step 5 exists to close.
func TestConcurrentConditionalWritesCannotBothLand(t *testing.T) {
	const writers = 4
	h := newTestHandler(t)
	path := filepath.Join(h.Root(), "src", "main.go")
	base := baseHashOf(t, h, "/dav/src/main.go")

	bodies := make([]string, writers)
	recs := make([]*httptest.ResponseRecorder, writers)
	for i := range bodies {
		bodies[i] = fmt.Sprintf("package main\n\nfunc writer%d() {}\n", i)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recs[i] = do(t, h, "PUT", "/dav/src/main.go", map[string]string{
				"If-Match": `"` + base + `"`,
			}, bodies[i])
		}(i)
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, rec := range recs {
		switch rec.Code {
		case 204, 201:
			if winner != -1 {
				t.Fatalf("writers %d and %d both landed on one base: exactly one write may win", winner, i)
			}
			winner = i
		case 412:
			if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictHashMismatch) {
				t.Fatalf("writer %d refused with %q, want hash_mismatch", i, got)
			}
		default:
			t.Fatalf("writer %d -> %d (%s)", i, rec.Code, rec.Body.String())
		}
	}
	if winner == -1 {
		t.Fatalf("no writer landed")
	}
	// Every refusal names the winning bytes as the current state: a loser may
	// be refused by the early evaluation or inside the commit, and the two
	// must not disagree about what is on disk.
	for i, rec := range recs {
		if i == winner {
			continue
		}
		if got, want := rec.Header().Get("X-Bunker-Current-Hash"), contentHash(bodies[winner]); got != want {
			t.Fatalf("writer %d saw current hash %q, want the winner's bytes %q", i, got, want)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != bodies[winner] {
		t.Fatalf("the file holds %q, want the winner's bytes %q", got, bodies[winner])
	}
	noStagingLeft(t, filepath.Dir(path))
}
