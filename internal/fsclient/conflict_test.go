package fsclient

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestWriteRefusalOnStaleBase is the refusal path of BFS-004 §6 / BFS-005 §5.2,
// driven through the landed server: a write whose base no longer matches is
// refused with the server's own machine code, the target's bytes are unchanged,
// and the base hash is updated from the refusal rather than kept stale.
func TestWriteRefusalOnStaleBase(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)
	dir := t.TempDir()
	cache, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	wp := NewWritePath(c, cache, dir, OnConflictRefuse)
	ctx := context.Background()

	// Read through the client: this read IS the base hash.
	data, meta, oerr := c.Get(ctx, "README.md", "")
	if oerr != nil {
		t.Fatalf("GET: %v", oerr)
	}
	base := meta.Hash
	if base == "" {
		t.Fatalf("the surface served no content hash; the write precondition has no base")
	}
	wp.NoteServed("README.md", base)
	before := HashBytes(data)

	// The agent-side change: a REAL concurrent edit, out of band.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# changed on the agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentHash := HashBytes([]byte("# changed on the agent\n"))

	// Now the client writes with its stale base.
	_, werr := wp.PublishBytes(ctx, "README.md", []byte("# client edit\n"), WriteBase{IfMatch: base, Source: BaseFromServed})
	if werr == nil {
		t.Fatalf("expected a refusal, got a landed write")
	}
	if werr.Cause != CauseConflict {
		t.Fatalf("cause = %s, want conflict", werr.Cause)
	}
	if werr.Errno != syscall.ESTALE {
		t.Fatalf("errno = %v, want ESTALE (full refusal: %v)", werr.Errno, werr)
	}
	if werr.Verdict != VerdictHashMismatch {
		t.Fatalf("verdict = %q, want %q (the server's own machine code)", werr.Verdict, VerdictHashMismatch)
	}
	if werr.CurrentHash != agentHash {
		t.Fatalf("the refusal must name the CURRENT hash: got %q want %q", werr.CurrentHash, agentHash)
	}
	if werr.ExpectedHash != base {
		t.Fatalf("the refusal must name the EXPECTED hash: got %q want %q", werr.ExpectedHash, base)
	}

	// The target's bytes are unchanged — the refusal's own guarantee.
	onDisk, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := HashBytes(onDisk); got != agentHash {
		t.Fatalf("the refused write changed the file: hash now %s", got)
	}
	if bytes.Equal(onDisk, []byte("# client edit\n")) {
		t.Fatalf("the caller's bytes landed despite the refusal")
	}
	_ = before

	// The base is updated from the refusal: the next write goes in with truth.
	if wp.Refusals() != 1 {
		t.Fatalf("refusals = %d, want 1", wp.Refusals())
	}
	last := wp.LastConflict()
	if last == nil || last.Code != VerdictHashMismatch || last.Current != agentHash {
		t.Fatalf("the refusal was not recorded: %+v", last)
	}
	res, werr := wp.PublishBytes(ctx, "README.md", []byte("# merged\n"), WriteBase{IfMatch: agentHash, Source: BaseFromFetchCheck})
	if werr != nil {
		t.Fatalf("recovered write: %v", werr)
	}
	if res.Hash != HashBytes([]byte("# merged\n")) {
		t.Fatalf("landed hash = %q", res.Hash)
	}
	// And the refusal is on disk for `bunker fs conflicts`.
	list, err := ReadConflicts(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Path != "README.md" {
		t.Fatalf("conflict log = %+v", list)
	}
}

// TestWriteRefusalWhenTargetAbsent covers §5.2's second refusal class: the base
// is stale AND the target is gone — precondition_failed, no hash detail, and NO
// null "current" anywhere in the exchange.
func TestWriteRefusalWhenTargetAbsent(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)
	dir := t.TempDir()
	cache, _ := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	wp := NewWritePath(c, cache, dir, OnConflictRefuse)
	ctx := context.Background()

	if _, _, oerr := c.Get(ctx, "README.md", ""); oerr != nil {
		t.Fatal(oerr)
	}
	base := HashBytes([]byte(fixReadmeBody))
	// Delete the target out of band.
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	_, werr := wp.PublishBytes(ctx, "README.md", []byte("new bytes\n"), WriteBase{IfMatch: base, Source: BaseFromServed})
	if werr == nil {
		t.Fatal("expected a refusal")
	}
	if werr.Verdict != VerdictPreconditionFailed {
		t.Fatalf("verdict = %q, want %q", werr.Verdict, VerdictPreconditionFailed)
	}
	if werr.CurrentHash != "" || werr.ExpectedHash != "" {
		t.Fatalf("precondition_failed names no hashes; got current=%q expected=%q", werr.CurrentHash, werr.ExpectedHash)
	}
	if werr.Errno != syscall.ESTALE || werr.Cause != CauseConflict {
		t.Fatalf("errno/cause = %v/%s, want ESTALE/conflict", werr.Errno, werr.Cause)
	}
	// The base becomes ABSENT: the next write creates with If-None-Match: *.
	next, oerr := wp.ResolveBase(ctx, "README.md")
	if oerr != nil {
		t.Fatal(oerr)
	}
	if !next.IfNoneMatchStar || next.Source != BaseFromAbsent {
		t.Fatalf("base after an absent-target refusal = %+v, want If-None-Match: * / absent", next)
	}
	last := wp.LastConflict()
	if last == nil || last.Code != VerdictPreconditionFailed || last.Current != "" {
		t.Fatalf("the refusal must be recorded with its own code and NO current hash: %+v", last)
	}
	// The file was really gone, and the create now succeeds.
	res, werr := wp.PublishBytes(ctx, "README.md", []byte("recreated\n"), next)
	if werr != nil {
		t.Fatalf("create after the refusal: %v", werr)
	}
	if res.Status != 201 {
		t.Fatalf("expected 201 Created, got %d", res.Status)
	}
}

// TestWriteIdenticalContentIsARecordedNoop covers D3: the base is stale but the
// arriving bytes are identical, so nothing is written and the mtime is preserved.
func TestWriteIdenticalContentIsARecordedNoop(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)
	dir := t.TempDir()
	cache, _ := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	wp := NewWritePath(c, cache, dir, OnConflictRefuse)
	ctx := context.Background()

	body := []byte(fixReadmeBody)
	hash := HashBytes(body)
	path := filepath.Join(root, "README.md")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	res, werr := wp.PublishBytes(ctx, "README.md", body, WriteBase{IfMatch: HashBytes([]byte("a base that never existed"))})
	if werr != nil {
		t.Fatalf("identical content must be a NOOP, not a refusal: %v", werr)
	}
	if !res.Noop || res.Verdict != VerdictIdenticalContent {
		t.Fatalf("expected identical_content/Noop, got verdict=%q noop=%v", res.Verdict, res.Noop)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("a noop must not move the mtime: %v -> %v", before.ModTime(), after.ModTime())
	}
	if wp.Refusals() != 0 {
		t.Fatalf("a noop is not a refusal; refusals=%d", wp.Refusals())
	}
	_ = hash
}

// TestOverwriteIfUnchangedIsNarrow proves the option's whole point: it recovers a
// metadata-only mismatch and STILL refuses a real concurrent edit.
func TestOverwriteIfUnchangedIsNarrow(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)
	dir := t.TempDir()
	cache, _ := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	wp := NewWritePath(c, cache, dir, OnConflictOverwriteIfUnchanged)
	ctx := context.Background()

	// Arm 1: the file is touched but its BYTES are what we served -> recover.
	served := HashBytes([]byte(fixReadmeBody))
	wp.NoteServed("README.md", served)
	res, werr := wp.PublishBytes(ctx, "README.md", []byte("updated\n"), WriteBase{IfMatch: HashBytes([]byte("stale-but-metadata-only"))})
	if werr != nil {
		t.Fatalf("arm 1: expected recovery, got %v", werr)
	}
	if res.Hash != HashBytes([]byte("updated\n")) {
		t.Fatalf("arm 1: landed hash = %q", res.Hash)
	}

	// Arm 2: a real concurrent edit (current != what we served) is refused.
	agentBytes := []byte("someone else's edit\n")
	if err := os.WriteFile(filepath.Join(root, "README.md"), agentBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	_, werr = wp.PublishBytes(ctx, "README.md", []byte("mine\n"), WriteBase{IfMatch: res.Hash})
	if werr == nil {
		t.Fatalf("arm 2: a real concurrent edit must still be refused")
	}
	if werr.Cause != CauseConflict || werr.CurrentHash != HashBytes(agentBytes) {
		t.Fatalf("arm 2: %v (current=%q)", werr, werr.CurrentHash)
	}
}

// TestResolveBaseTable pins §5.3's four rows.
func TestResolveBaseTable(t *testing.T) {
	c, _, _ := fixtureEndpoint(t)
	dir := t.TempDir()
	cache, _ := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	ctx := context.Background()

	t.Run("served wins", func(t *testing.T) {
		wp := NewWritePath(c, cache, dir, OnConflictRefuse)
		wp.NoteServed("README.md", "sha256:"+strings.Repeat("a", 64))
		b, err := wp.ResolveBase(ctx, "README.md")
		if err != nil {
			t.Fatal(err)
		}
		if b.Source != BaseFromServed || !strings.HasPrefix(b.IfMatch, "sha256:") {
			t.Fatalf("got %+v", b)
		}
	})
	t.Run("never read, target exists: fetch-then-check", func(t *testing.T) {
		wp := NewWritePath(c, cache, dir, OnConflictRefuse)
		b, err := wp.ResolveBase(ctx, "src/main.go")
		if err != nil {
			t.Fatal(err)
		}
		if b.Source != BaseFromFetchCheck || b.IfMatch != HashBytes([]byte(fixMainBody)) {
			t.Fatalf("got %+v, want the server's current hash", b)
		}
	})
	t.Run("never read, target absent: If-None-Match", func(t *testing.T) {
		wp := NewWritePath(c, cache, dir, OnConflictRefuse)
		b, err := wp.ResolveBase(ctx, "nope.txt")
		if err != nil {
			t.Fatal(err)
		}
		if !b.IfNoneMatchStar || b.Source != BaseFromAbsent {
			t.Fatalf("got %+v", b)
		}
	})
	t.Run("invalidated without a hash: back to fetch-then-check", func(t *testing.T) {
		wp := NewWritePath(c, cache, dir, OnConflictRefuse)
		wp.NoteServed("src/main.go", HashBytes([]byte(fixMainBody)))
		if _, err := wp.ResolveBase(ctx, "src/main.go"); err != nil {
			t.Fatal(err)
		}
		wp.NoteInvalidated("src/main.go")
		b, err := wp.ResolveBase(ctx, "src/main.go")
		if err != nil {
			t.Fatal(err)
		}
		if b.Source != BaseFromFetchCheck {
			t.Fatalf("an invalidated base must be re-learned, got %+v", b)
		}
	})
}

// TestPutWithoutPreconditionIsUnconditional documents the compatibility rule the
// client relies on but never uses on its own: absent If-Match is an RFC 9110
// unconditional write, which is what keeps stock clients working.
func TestPutWithoutPreconditionIsUnconditional(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)
	res, oerr := c.PutBody(context.Background(), "unconditional.txt", strings.NewReader("no precondition\n"), -1, PutPrecondition{})
	if oerr != nil {
		t.Fatalf("PUT: %v", oerr)
	}
	if res.Status != 201 {
		t.Fatalf("status = %d, want 201", res.Status)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "unconditional.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != "no precondition\n" {
		t.Fatalf("bytes on disk = %q", string(onDisk))
	}
}

// TestBodyHashMismatchIs422 covers E-1's declared-body-hash verification: a write
// that lies about its own body hash is refused and nothing is written.
func TestBodyHashMismatchIs422(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)
	req, err := c.newRequest(context.Background(), "PUT", "declared.txt", strings.NewReader("real bytes\n"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Bunker-Hash", HashBytes([]byte("different bytes")))
	resp, oerr := c.do(context.Background(), req)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "declared.txt")); !os.IsNotExist(err) {
		t.Fatalf("a body-hash mismatch must write nothing; file exists or stat failed: %v", err)
	}
}

// TestStreamedBodyIsOneRequest proves the write is ONE PUT carrying the whole
// file, not one request per chunk: the measured pattern is 1023 WRITE ops for a
// 4 MiB file, and the request count must not follow it.
func TestStreamedBodyIsOneRequest(t *testing.T) {
	c, _, _ := fixtureEndpoint(t)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	before := c.Requests()
	_, oerr := c.PutBody(ctx, "one-request.bin", bytes.NewReader(payload), int64(len(payload)), PutPrecondition{IfNoneMatchStar: true})
	if oerr != nil {
		t.Fatal(oerr)
	}
	if got := c.Requests() - before; got != 1 {
		t.Fatalf("a whole-file publish must be ONE request; it took %d", got)
	}
	data, _, oerr := c.Get(ctx, "one-request.bin", "")
	if oerr != nil {
		t.Fatal(oerr)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("round trip mismatch: %d bytes back for %d sent", len(data), len(payload))
	}
}

// TestConflictRefusalIgnoresMtimePreservedEdit is the case the rest of this file
// does not reach: an agent-side edit that preserves BOTH the size and the mtime,
// so every metadata key a (size, mtime) comparison could use is UNCHANGED while
// the bytes are not.
//
// It exists because "hash, never mtime" (BFS-005 §5.1) is only a rule if it is
// tested where mtime would give the wrong answer. Every other refusal test edits
// through os.WriteFile, which moves the mtime; an implementation that compared
// (size, mtime) would pass all of them and still clobber here.
//
// Two arms, both with a base the client genuinely holds for the PRE-edit bytes:
//
//	arm 1 — the default policy must REFUSE, naming the post-edit hash.
//	arm 2 — `overwrite-if-unchanged` must ALSO refuse: that policy exists to
//	        absorb a METADATA-ONLY mismatch (the bytes we served are still the
//	        bytes on the server), and an edit that leaves size and mtime alone
//	        but changes the bytes is a real concurrent edit, so retrying it
//	        would be the silent overwrite the spec forbids.
//
// The arms are deliberately not "resolve the base from the server after the
// edit": that path is a legitimate base==current write whenever the surface
// answers honestly, so it is not a refusal case. What the surface answers for
// such an edit is measured in the row's evidence instead.
func TestConflictRefusalIgnoresMtimePreservedEdit(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)
	dir := t.TempDir()
	cache, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	target := filepath.Join(root, "src", "main.go")

	// The bytes the client reads, and therefore the base it will carry.
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	base := HashBytes(before)
	// Same length, different bytes: the size key cannot move.
	edited := append([]byte(nil), before...)
	for i := range edited {
		if edited[i] != 'x' {
			edited[i] = 'x'
		}
	}
	if len(edited) != len(before) {
		t.Fatalf("fixture: the edit changed the length (%d -> %d)", len(before), len(edited))
	}
	if bytes.Equal(edited, before) {
		t.Fatal("fixture: the edit did not change the bytes")
	}
	editedHash := HashBytes(edited)

	// Warm the surface's identity cache the way any real read does, then edit
	// out of band and RESTORE the mtime exactly: nothing a metadata comparison
	// can see has moved.
	if _, _, oerr := c.Get(ctx, "src/main.go", ""); oerr != nil {
		t.Fatalf("warming GET: %v", oerr)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != st.Size() || !after.ModTime().Equal(st.ModTime()) {
		t.Fatalf("fixture: the metadata moved (size %d->%d, mtime %v->%v); the case is not exercised",
			st.Size(), after.Size(), st.ModTime(), after.ModTime())
	}
	if got := HashBytes(mustRead(t, target)); got != editedHash {
		t.Fatalf("fixture: on-disk bytes are %s, want %s", got, editedHash)
	}

	assertRefused := func(arm string, wp *WritePath) {
		t.Helper()
		_, werr := wp.PublishBytes(ctx, "src/main.go", []byte("client clobber attempt\n"), WriteBase{IfMatch: base, Source: BaseFromServed})
		if werr == nil {
			t.Fatalf("%s: the write LANDED; a same-size, mtime-preserved edit must still be refused", arm)
		}
		if werr.Cause != CauseConflict || werr.Errno != syscall.ESTALE {
			t.Fatalf("%s: errno/cause = %v/%s, want ESTALE/conflict", arm, werr.Errno, werr.Cause)
		}
		if werr.Verdict != VerdictHashMismatch {
			t.Fatalf("%s: verdict = %q, want %q", arm, werr.Verdict, VerdictHashMismatch)
		}
		if werr.CurrentHash != editedHash {
			t.Fatalf("%s: the refusal must name the CURRENT hash %s, got %q", arm, editedHash, werr.CurrentHash)
		}
		if got := HashBytes(mustRead(t, target)); got != editedHash {
			t.Fatalf("%s: the refused write changed the file: %s", arm, got)
		}
		if wp.LastConflict() == nil {
			t.Fatalf("%s: the refusal was not recorded (silent refusal)", arm)
		}
	}

	t.Run("default policy refuses and names the post-edit hash", func(t *testing.T) {
		wp := NewWritePath(c, cache, dir, OnConflictRefuse)
		assertRefused("arm 1", wp)
		if wp.Refusals() != 1 {
			t.Fatalf("arm 1: refusals = %d, want 1", wp.Refusals())
		}
		last := wp.LastConflict()
		if last.Code != VerdictHashMismatch || last.Current != editedHash {
			t.Fatalf("arm 1: the refusal was not recorded loudly: %+v", last)
		}
		list, err := ReadConflicts(dir, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Path != "src/main.go" {
			t.Fatalf("arm 1: conflict log = %+v", list)
		}
	})

	t.Run("overwrite-if-unchanged must not treat it as metadata-only", func(t *testing.T) {
		wp := NewWritePath(c, cache, dir, OnConflictOverwriteIfUnchanged)
		// We served these exact bytes, which is the ONLY condition under which
		// that policy may retry. The server's bytes are no longer those, even
		// though size and mtime say otherwise.
		wp.NoteServed("src/main.go", base)
		assertRefused("arm 2", wp)
		if wp.Refusals() != 1 {
			t.Fatalf("arm 2: refusals = %d, want 1 (a retry would have been a silent overwrite)", wp.Refusals())
		}
	})
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestWriteBodyReaderSeesWholeFile proves the body we stream is the file: a
// streamed reader is consumed exactly once and cannot be re-read, which is why
// the mount buffers to a temp file rather than re-sending a reader.
func TestWriteBodyReaderSeesWholeFile(t *testing.T) {
	body := []byte("stream-me")
	r := io.LimitReader(bytes.NewReader(body), int64(len(body)))
	h, n, err := HashReader(r)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(body)) || h != HashBytes(body) {
		t.Fatalf("HashReader: n=%d h=%s", n, h)
	}
}

// TestTouchOnlyChangeIsNotAConflict is the OTHER half of BFS-004 §6.1's rule, and
// the half a metadata-comparing implementation gets wrong in the direction that
// costs a user their work: the rule is the CONTENT HASH, never the mtime. An edit
// that preserves size and mtime must be refused (that is
// TestConflictRefusalIgnoresMtimePreservedEdit, which follows a real refactor to a
// (size, mtime) shortcut). A change that moves ONLY the mtime, with the bytes
// identical, must NOT be a conflict: there is nothing to refuse — the base we hold
// still describes the bytes on the server — and a false refusal here is a build
// that cannot write a file until someone re-reads it.
//
// The test is discriminating rather than decorative: folding the mtime into the
// served content identity (the mutation recorded in
// docs/evidence/BFS-012-probes/mutation-mtime-identity.sh) turns this arm RED
// while the refusal arms stay green.
func TestTouchOnlyChangeIsNotAConflict(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)
	dir := t.TempDir()
	cache, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	wp := NewWritePath(c, cache, dir, OnConflictRefuse)
	ctx := context.Background()
	target := filepath.Join(root, "src", "main.go")

	// The read that fixes the base, then a touch that moves ONLY the mtime.
	served, _, oerr := c.Get(ctx, "src/main.go", "")
	if oerr != nil {
		t.Fatalf("GET: %v", oerr)
	}
	base := HashBytes(served)
	wp.NoteServed("src/main.go", base)

	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	touched := st.ModTime().Add(5 * time.Second)
	if err := os.Chtimes(target, touched, touched); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	// Assert the fixture is the case it claims to be BEFORE asserting anything
	// about the write: a "touch-only" arm whose bytes moved proves nothing.
	if after.Size() != st.Size() {
		t.Fatalf("fixture: the size moved (%d -> %d)", st.Size(), after.Size())
	}
	if after.ModTime().Equal(st.ModTime()) {
		t.Fatalf("fixture: the touch did not move the mtime (%v)", st.ModTime())
	}
	if got := HashBytes(mustRead(t, target)); got != base {
		t.Fatalf("fixture: the bytes moved (%s -> %s); this is not a touch-only change", base, got)
	}

	body := []byte("package main\n\n// rewritten after a touch-only change\nfunc main() {}\n")
	res, werr := wp.PublishBytes(ctx, "src/main.go", body, WriteBase{IfMatch: base, Source: BaseFromServed})
	if werr != nil {
		t.Fatalf("a write whose base still matches the CONTENT must land; the mtime is not the identity: %v", werr)
	}
	if res.Hash != HashBytes(body) {
		t.Fatalf("landed hash = %q, want %q", res.Hash, HashBytes(body))
	}
	if res.Noop {
		t.Fatalf("the bodies differ; this must be a real write, not an identical-content noop")
	}
	if wp.Refusals() != 0 {
		t.Fatalf("a touch-only change is not a conflict; refusals=%d", wp.Refusals())
	}
	if got := mustRead(t, target); !bytes.Equal(got, body) {
		t.Fatalf("the write reported success but the bytes on the server are %q", got)
	}
	list, err := ReadConflicts(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("a refusal was recorded for a touch-only change: %+v", list)
	}
}
