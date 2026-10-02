package fsclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-031: the mount's OWN state is bounded too — the cache directory can only
// be a directory whose bound is true if the things that are not the cache live
// somewhere else with a bound of their own.
//
// The two growth directions these arms are written against:
//
//   - the refusal log grows with EDIT VOLUME (one line per refused write), so a
//     bound has to be enforced on it — and the entries the bound drops have to
//     stay COUNTED, or a bounded log becomes a silent truncation;
//   - the status document grows with whatever it is made to carry (a server's
//     own sentence, a path), so its cap has to be enforced where it is written,
//     and the enforcement has to say so IN the document.
// ---------------------------------------------------------------------------

// TestTheRefusalLogIsBoundedAndTheDropIsCounted drives the log's bound: with the
// cap lowered to a few lines, appending enough refusals must leave the file
// inside the cap, and the number of entries the bound forced out must be
// readable from a durable count — a bounded log that lost entries silently would
// be the same defect one file over.
func TestTheRefusalLogIsBoundedAndTheDropIsCounted(t *testing.T) {
	dir := t.TempDir()
	// Drive a small cap rather than writing 4 MiB of refusals to reach the real
	// one (the cap is a variable for exactly this reason).
	defer swapInt64(&ConflictsMaxBytes, 2048)()
	defer swapInt64(&ConflictDetailMaxBytes, 256)()

	// The append count is sized by what the assertions need — sustained
	// rotation with a durable dropped count — not by a round number: at the
	// 2048 B cap and ~500 B per entry the log retains ~4 lines, so 24 appends
	// exercise the rotation ~6 times over. Every append pays a real fsync and
	// a rotate (read, rewrite, rename) on the suite's scratch disk, which under
	// fleet-host load measured ~2 s per append — 200 of them cost ~400 s and
	// timed the whole package out (QA-BUNKER-49 class, found by the same
	// verification that fixed TestBFS063AStale...). The assertions themselves
	// are count-adaptive (dropped>0, newest kept, present+dropped == appends),
	// so what this test proves is unchanged; only the fixed stimulus size is
	// bounded to what it must exceed (the log's capacity).
	const appends = 24
	for i := 0; i < appends; i++ {
		c := Conflict{Path: fmt.Sprintf("d/file-%03d.go", i), Code: "hash_mismatch",
			Expected: "sha256:" + strings.Repeat("a", 64), Current: "sha256:" + strings.Repeat("b", 64),
			Detail: strings.Repeat("x", 900)}
		if err := AppendConflict(dir, c); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		info, err := os.Stat(filepath.Join(dir, ConflictsFile))
		if err != nil {
			t.Fatalf("stat log after %d: %v", i, err)
		}
		if info.Size() > ConflictsMaxBytes {
			t.Fatalf("after %d appends the refusal log holds %d B, over its %d B bound", i, info.Size(), ConflictsMaxBytes)
		}
	}
	// The per-entry detail cap is what makes the strongest case hold: one entry
	// can never be larger than the log's own bound.
	if got := readLogDetailLen(t, dir); got > int(ConflictDetailMaxBytes)+128 {
		t.Fatalf("a log entry's detail is %d bytes, over the per-entry cap %d (+ the marker)", got, ConflictDetailMaxBytes)
	}
	dropped := ReadConflictsDropped(dir)
	if dropped <= 0 {
		t.Fatalf("the bound dropped entries and the count says %d: a bounded log must keep the count of what it dropped", dropped)
	}
	// The log still holds the NEWEST refusals: dropping is oldest-first, so the
	// last append is always present.
	last, err := ReadConflicts(dir, 1)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(last) != 1 || last[0].Path != fmt.Sprintf("d/file-%03d.go", appends-1) {
		t.Fatalf("the newest refusal is not in the log: %+v", last)
	}
	// And the counts add up: entries present + entries dropped = entries written.
	present, err := ReadConflicts(dir, 0)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if int64(len(present))+dropped != appends {
		t.Fatalf("present=%d dropped=%d, want %d appended: the log's bound lost entries without counting them",
			len(present), dropped, appends)
	}
	st := MeasureState(dir, 0, 1<<20, 0)
	if st.ConflictsBytes > st.ConflictsMaxBytes {
		t.Fatalf("the measured log is %d B, over the reported bound %d", st.ConflictsBytes, st.ConflictsMaxBytes)
	}
	if st.ConflictsDroppedTotal != dropped {
		t.Fatalf("the reported dropped total (%d) disagrees with the durable count (%d)", st.ConflictsDroppedTotal, dropped)
	}
	t.Logf("log bound: present=%d dropped=%d bytes=%d cap=%d detail_len=%d",
		len(present), dropped, st.ConflictsBytes, st.ConflictsMaxBytes, readLogDetailLen(t, dir))
}

// readLogDetailLen reports the length of the LAST log entry's detail, so the
// per-entry cap can be asserted on real bytes rather than on the constant.
func readLogDetailLen(t *testing.T, dir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ConflictsFile))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := splitLogLines(raw)
	if len(lines) == 0 {
		t.Fatal("the log is empty")
	}
	var c Conflict
	if err := json.Unmarshal(lines[len(lines)-1], &c); err != nil {
		t.Fatalf("decode last entry: %v", err)
	}
	return len(c.Detail)
}

// TestTheStatusDocumentIsBoundedByItsOwnCap drives the document's bound: a
// document made to carry oversized fields must land on disk inside its cap, and
// must SAY that it was reduced — a document that silently stopped describing the
// mount is this row's defect with the client's own report as its subject.
func TestTheStatusDocumentIsBoundedByItsOwnCap(t *testing.T) {
	dir := t.TempDir()
	// The cap is 8 KiB against a schema that renders at ~4.3 KB indented, so the
	// arms below say something about a document that MEANS to fit: an ordinary
	// document is not reduced, and a document carrying oversized fields is.
	defer swapInt64(&StatusMaxBytes, 8192)()
	defer swapInt(&StatusStringMaxBytes, 256)()

	big := strings.Repeat("S", 20000)
	st := Status{
		Mount:      "bounded",
		Mode:       "poll",
		Endpoint:   "http://127.0.0.1:1/dav",
		Mountpoint: "/tmp/mnt",
		Conflicts: ConflictState{
			RefusalsTotal: 3,
			Last:          &Conflict{Path: big, Code: "hash_mismatch", Detail: big},
		},
		Cache: CacheStats{DirBytes: 1234, MaxBytes: 1 << 20, DirPeakBytes: 2345,
			DirBytesByClass: map[string]int64{DirClassBlobs: 1200, DirClassIndex: 34}},
		// A passthrough block with an oversized field: the server's own words
		// travel verbatim, and they are the other unbounded input.
		Snapshot: SnapshotState{Source: big, Nodes: 3, Calls: 1},
	}
	if err := WriteStatus(dir, st); err != nil {
		t.Fatalf("write status: %v", err)
	}
	assertStatusWithinCap(t, dir)
	got, err := ReadStatus(dir)
	if err != nil {
		t.Fatalf("the reduced document must still be readable: %v", err)
	}
	if !got.Reduced {
		t.Fatal("the document was reduced and does not say so: Reduced is false")
	}
	if !hasReasonClass(got.ReducedReason) {
		t.Fatalf("the reduction reason does not open with a vocabulary class: %q", got.ReducedReason)
	}
	if got.Mount != "bounded" {
		t.Fatalf("the reduced document lost the mount's identity: %q", got.Mount)
	}
	t.Logf("reduced document: %d bytes (cap %d): %s", fileSizeOrZero(t, filepath.Join(dir, StatusFile)), StatusMaxBytes, got.ReducedReason)
	// The oversized FIELDS were capped with the marker in the text, so a reader
	// of the document can see that the text was cut rather than reading a
	// sentence that happens to end mid-word.
	if raw, rerr := os.ReadFile(filepath.Join(dir, StatusFile)); rerr == nil {
		if !strings.Contains(string(raw), "[truncated") {
			t.Fatal("the document was reduced and no field carries the truncation marker")
		}
	}

	// A NORMAL document is not reduced: the cap must not be paid on every write.
	normal := Status{Mount: "normal", Mode: "poll", Endpoint: "http://127.0.0.1:1/dav", Mountpoint: "/tmp/mnt",
		Cache: CacheStats{MaxBytes: 1 << 20, UsedBytes: 26, IndexBytes: 26, DirBytes: 26, DirPeakBytes: 52,
			DirBytesByClass: map[string]int64{DirClassIndex: 26}}, State: MeasureState(dir, 26, 1<<20, 0)}
	if err := WriteStatus(dir, normal); err != nil {
		t.Fatalf("write normal status: %v", err)
	}
	assertStatusWithinCap(t, dir)
	plain, err := ReadStatus(dir)
	if err != nil {
		t.Fatalf("read normal status: %v", err)
	}
	if plain.Reduced {
		t.Fatalf("an ordinary document was reported as reduced: %q", plain.ReducedReason)
	}
	if plain.Cache.UsedBytes != 26 {
		t.Fatalf("the ordinary document lost its figures: used_bytes=%d", plain.Cache.UsedBytes)
	}
}

// assertStatusWithinCap is the bound's assertion, applied to the file on disk.
func assertStatusWithinCap(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, StatusFile))
	if err != nil {
		t.Fatalf("stat status: %v", err)
	}
	if info.Size() > StatusMaxBytes {
		t.Fatalf("the status document on disk is %d B, over its declared cap %d", info.Size(), StatusMaxBytes)
	}
}

// fileSizeOrZero is a test-side size read (0 when the file is missing).
func fileSizeOrZero(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// TestTheStateMeasurementNamesEveryResidentAndItsBound is the "name the
// exclusion" half: the state's figures are an independent measurement of the
// mount directory's files, and the footprint is the cache directory plus the
// state — with the two bounds it obeys.
func TestTheStateMeasurementNamesEveryResidentAndItsBound(t *testing.T) {
	mount := t.TempDir()
	st := Status{Mount: "state-arm", Mode: "poll", Endpoint: "http://127.0.0.1:1/dav"}
	if err := WriteStatus(mount, st); err != nil {
		t.Fatalf("write status: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := AppendConflict(mount, Conflict{Path: fmt.Sprintf("p/%d", i), Code: "hash_mismatch"}); err != nil {
			t.Fatalf("append conflict: %v", err)
		}
	}
	spill := []byte(strings.Repeat("w", 512))
	if err := os.WriteFile(filepath.Join(mount, WriteBufPrefix+"123"), spill, 0o600); err != nil {
		t.Fatalf("spill file: %v", err)
	}
	statusBytes := fileSizeOrZero(t, filepath.Join(mount, StatusFile))
	logBytes := fileSizeOrZero(t, filepath.Join(mount, ConflictsFile))
	const cacheBytes = 4096
	const cacheMax = 1 << 20
	got := MeasureState(mount, cacheBytes, cacheMax, 0)
	if got.Reason != "" {
		t.Fatalf("the measurement reports a reason on a readable directory: %q", got.Reason)
	}
	if got.StatusBytes != statusBytes {
		t.Fatalf("status bytes = %d, want the file's %d", got.StatusBytes, statusBytes)
	}
	if got.ConflictsBytes != logBytes {
		t.Fatalf("conflict bytes = %d, want the log's %d", got.ConflictsBytes, logBytes)
	}
	if got.SpillBytes != int64(len(spill)) {
		t.Fatalf("spill bytes = %d, want %d", got.SpillBytes, len(spill))
	}
	if got.Bytes != statusBytes+logBytes+int64(len(spill)) {
		t.Fatalf("state bytes = %d, want the sum of its classes %d", got.Bytes, statusBytes+logBytes+int64(len(spill)))
	}
	if got.MaxBytes != StateMaxBytes() || got.ConflictsMaxBytes != ConflictsMaxBytes || got.StatusMaxBytes != StatusMaxBytes {
		t.Fatalf("the declared bounds are not reported: %+v", got)
	}
	if got.FootprintBytes != got.Bytes+cacheBytes {
		t.Fatalf("footprint bytes = %d, want the state plus the cache directory (%d)", got.FootprintBytes, got.Bytes+cacheBytes)
	}
	if got.FootprintMaxBytes != cacheMax+StateMaxBytes() {
		t.Fatalf("footprint max = %d, want the cache bound + the state bound (%d)", got.FootprintMaxBytes, cacheMax+StateMaxBytes())
	}
	if got.Bytes > got.MaxBytes || got.FootprintBytes > got.FootprintMaxBytes {
		t.Fatalf("the state is over its own bound: %+v", got)
	}
	// An unreadable directory is ABSENT WITH A REASON, never a zero that reads as
	// "the state is empty".
	missing := MeasureState(filepath.Join(mount, "nope"), cacheBytes, cacheMax, 0)
	if missing.Reason == "" {
		t.Fatalf("an unreadable mount directory reported no reason: %+v", missing)
	}
	if !hasReasonClass(missing.Reason) {
		t.Fatalf("the absence reason does not open with a vocabulary class: %q", missing.Reason)
	}
	t.Logf("state: bytes=%d (status=%d/%d conflicts=%d/%d spill=%d) dropped=%d footprint=%d/%d",
		got.Bytes, got.StatusBytes, got.StatusMaxBytes, got.ConflictsBytes, got.ConflictsMaxBytes,
		got.SpillBytes, got.ConflictsDroppedTotal, got.FootprintBytes, got.FootprintMaxBytes)
}

// TestTheStateBoundHoldsAcrossASession is the session-shaped arm: a run that
// writes the status document on the mount's cadence and refuses writes must
// leave both directories inside their bounds — and the footprint inside its own.
func TestTheStateBoundHoldsAcrossASession(t *testing.T) {
	mount := t.TempDir()
	cacheDir := MountCacheDir(mount)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("cache dir: %v", err)
	}
	defer swapInt64(&ConflictsMaxBytes, 8192)()
	defer swapInt64(&ConflictDetailMaxBytes, 512)()
	cache := newTestCacheWithBounds(t, CacheConfig{Dir: cacheDir, MaxBytes: 4096, MaxEntryBytes: 4096, MaxAge: time.Hour, DirMeasureInterval: -1})

	// The session is sized by what it must exceed, not by a round number: at
	// the 8 KiB log cap and ~770 B per entry it retains ~10 lines, so 30 rounds
	// rotate the log at least twice. Every round pays a cache insert, an fsync'd
	// append and a status write on the suite's scratch disk (~2 s per append
	// under fleet-host load, measured on the same primitives in
	// TestTheRefusalLogIsBoundedAndTheDropIsCounted), so the fixed count is a
	// package-timeout liability (QA-BUNKER-49 class). Every assertion is
	// per-round or count-adaptive ("60 refusals" in the message below is the
	// dropped-count sanity arm's own wording for "the rotations happened").
	for i := 0; i < 30; i++ {
		body := blobBytes(t, i, 512)
		if _, err := cache.Insert(pathFor(i), HashBytes(body), body); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		if err := AppendConflict(mount, Conflict{Path: pathFor(i), Code: "hash_mismatch", Detail: strings.Repeat("y", 700)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		cs := cache.Stats()
		if err := WriteStatus(mount, Status{Mount: "session", Mode: "poll", Endpoint: "http://127.0.0.1:1/dav", Cache: cs}); err != nil {
			t.Fatalf("write status %d: %v", i, err)
		}
		ss := MeasureState(mount, cs.DirBytes, cs.MaxBytes, 0)
		if ss.Bytes > ss.MaxBytes {
			t.Fatalf("after %d rounds the state holds %d B, over its bound %d", i, ss.Bytes, ss.MaxBytes)
		}
		if ss.FootprintBytes > ss.FootprintMaxBytes {
			t.Fatalf("after %d rounds the footprint is %d B, over %d", i, ss.FootprintBytes, ss.FootprintMaxBytes)
		}
		if walk := cacheDirWalk(t, cacheDir); walk > cs.MaxBytes {
			t.Fatalf("after %d rounds the cache directory holds %d B, over its bound %d", i, walk, cs.MaxBytes)
		}
	}
	cs := cache.Stats()
	ss := MeasureState(mount, cs.DirBytes, cs.MaxBytes, 0)
	if walk := cacheDirWalk(t, cacheDir); walk > cs.MaxBytes {
		t.Fatalf("the cache directory holds %d B at the end, over %d", walk, cs.MaxBytes)
	}
	if got := cacheDirWalk(t, mount); got != ss.FootprintBytes {
		t.Fatalf("the footprint (%d) disagrees with an independent walk of the mount directory (%d)", ss.FootprintBytes, got)
	}
	if ss.ConflictsDroppedTotal <= 0 {
		t.Fatal("30 refusals under an 8 KiB log cap dropped nothing: the arm never reached the log's bound")
	}
	t.Logf("session: state=%d/%d dropped=%d cache_dir=%d/%d footprint=%d/%d", ss.Bytes, ss.MaxBytes,
		ss.ConflictsDroppedTotal, cs.DirBytes, cs.MaxBytes, ss.FootprintBytes, ss.FootprintMaxBytes)
}

// swapInt64 sets a package bound and returns the restore, so an arm can drive a
// small cap and leave the value exactly as it found it.
func swapInt64(p *int64, v int64) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

// swapInt is swapInt64 for an int-valued bound.
func swapInt(p *int, v int) func() {
	old := *p
	*p = v
	return func() { *p = old }
}
