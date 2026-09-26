// The op list: the 14 cells BFS-011 grades, plus the fan-out arm that carries
// the measured lever directly.
//
// THE 14 OPS, as filed in the row (docs/prd/PRD-bunker-fs.md's battery, the
// same fixture the study's baselines used):
//
//	 1 stat                    HEAD (the mount's getattr)
//	 2 read-small              GET a 1 KiB file
//	 3 read-large              GET the 4 MiB file
//	 4 write-small             PUT a 1 KiB file
//	 5 write-large             PUT the 4 MiB file
//	 6 list-shallow            PROPFIND Depth: 1 on one collection
//	 7 list-deep               PROPFIND Depth: 1 three levels down
//	 8 create                  PUT with If-None-Match: * (create-only)
//	 9 rename                  MOVE
//	10 delete                  DELETE
//	11 mkdir-rmdir             MKCOL then DELETE on the collection
//	12 walk-tree               the whole-tree walk: one PROPFIND per directory
//	                           (the READDIR) + one HEAD per file (the GETATTR
//	                           storm) — the op class the baselines stall on
//	13 git-status-shaped       one delegated whole-tree read in ONE request
//	                           (X-Bunker-Op: snapshot, depth infinity)
//	14 conflict                a write whose base moved: it MUST be refused
//
// The whole-tree walk recurses with Depth: 1 and never sends Depth: infinity:
// that request is refused by this surface by design (BFS-004 §2.3 deviation 3,
// decision D2), and a battery that tripped it would be measuring the refusal
// rather than the walk. The refusal itself is asserted in the arm's notes.
//
// ONE EXTRA ARM, declared rather than smuggled: `fanout-100`, one hundred
// independent small GETs, which is the shape the PRD's own A/B used to measure
// the lever (100 concurrent requests at MaxConnsPerHost=1: 38.47 s over
// HTTP/1.1 versus 0.79 s over HTTP/2, PRD-bunker-fs.md:60-61) and the shape the
// release's "multiplexing replay" criterion asks a re-run to reproduce
// (PRD-bunker-fs.md:267). It is reported as an extra arm, never folded into the
// 14-op table.
package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	requiredCell = "required"
	extraArm     = "extra"

	// opStageRead is the stage whose cells only read: they run against the
	// fixture exactly as generated, so a listing can never race a write.
	opStageRead = 0
	// opStageWrite holds the independent mutating cells; they touch disjoint
	// paths so they are concurrent by construction.
	opStageWrite = 1
	// opStageRename/opStageDelete are ordered: a rename of a file that does
	// not exist yet is not the same experiment.
	opStageRename = 2
	opStageDelete = 3
)

// cellSpec is one cell of the battery: what it measures, and where it sits in
// the stage order. Cells inside a stage are dispatched concurrently and every
// request they make — including verification — obeys the arm's one in-flight
// bound, so "concurrency" is a single knob with a single meaning.
type cellSpec struct {
	name  string
	kind  string
	stage int
	run   func(ctx context.Context, b *battery) *outcome
}

// outcome is what a cell reports: the calls it timed, the calls it used to
// verify, and the verdict.
//
// It is mutex-guarded because a cell may issue its requests concurrently — the
// whole-tree walk and the fan-out arm both do — and an unsynchronised append to
// a shared slice is a data race that would corrupt the very numbers under
// measurement.
type outcome struct {
	mu     sync.Mutex
	timed  []*call
	verify []*call
	ok     bool
	detail string
}

func (o *outcome) add(c *call) *call {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.timed = append(o.timed, c)
	return c
}

func (o *outcome) addCheck(c *call) *call {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.verify = append(o.verify, c)
	return c
}

func (o *outcome) fail(format string, a ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ok = false
	if o.detail == "" {
		o.detail = fmt.Sprintf(format, a...)
	} else {
		o.detail += "; " + fmt.Sprintf(format, a...)
	}
}

// snapshot copies the outcome for reporting, so the reader never races a worker
// that is still finishing.
func (o *outcome) snapshot() (timed, verify []*call, ok bool, detail string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	timed = append(timed, o.timed...)
	verify = append(verify, o.verify...)
	return timed, verify, o.ok, o.detail
}

// battery holds the harness state for one arm: the client, the fixture it is
// measuring, and the timing configuration.
type battery struct {
	c       *client
	fx      *fixtureInfo
	root    string // local path of the served tree
	base    string // the surface prefix, e.g. /dav
	fanout  int
	opLimit time.Duration
}

// path renders a fixture-relative path as a surface path.
func (b *battery) path(rel string) string {
	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	if rel == "" {
		return b.base
	}
	return b.base + "/" + rel
}

// send is one timed request.
func (b *battery) send(ctx context.Context, o *outcome, spec requestSpec) *call {
	spec.Path = b.path(spec.Path)
	return o.add(b.c.do(ctx, spec))
}

// check issues one verification request. It is UNTIMED — it is not part of the
// cell's cost, which is why a read-back cannot flatter a write — but it still
// goes through the shared in-flight bound, so "concurrency" means the same thing
// for verification traffic as for the cells themselves. Callers may pass an
// already-absolute path.
func (b *battery) check(ctx context.Context, spec requestSpec, o *outcome) *call {
	if !strings.HasPrefix(spec.Path, "/") {
		spec.Path = b.path(spec.Path)
	}
	return o.addCheck(b.c.do(ctx, spec))
}

// get/put/move/... are the verb shorthands the cells read best as.
func (b *battery) get(ctx context.Context, o *outcome, rel string) *call {
	return b.send(ctx, o, requestSpec{Method: http.MethodGet, Path: rel})
}

// cells returns the battery in stage order. The order is data, not control flow:
// adding a cell is adding a row.
func (b *battery) cells() []cellSpec {
	return []cellSpec{
		{name: "stat", kind: requiredCell, stage: opStageRead, run: func(ctx context.Context, b *battery) *outcome { return b.opStat(ctx) }},
		{name: "read-small", kind: requiredCell, stage: opStageRead, run: func(ctx context.Context, b *battery) *outcome { return b.opReadSmall(ctx) }},
		{name: "read-large", kind: requiredCell, stage: opStageRead, run: func(ctx context.Context, b *battery) *outcome { return b.opReadLarge(ctx) }},
		{name: "list-shallow", kind: requiredCell, stage: opStageRead, run: func(ctx context.Context, b *battery) *outcome { return b.opListShallow(ctx) }},
		{name: "list-deep", kind: requiredCell, stage: opStageRead, run: func(ctx context.Context, b *battery) *outcome { return b.opListDeep(ctx) }},
		{name: "walk-tree", kind: requiredCell, stage: opStageRead, run: func(ctx context.Context, b *battery) *outcome { return b.opWalkTree(ctx) }},
		{name: "git-status-shaped", kind: requiredCell, stage: opStageRead, run: func(ctx context.Context, b *battery) *outcome { return b.opGitStatusShaped(ctx) }},
		{name: "fanout-100", kind: extraArm, stage: opStageRead, run: func(ctx context.Context, b *battery) *outcome { return b.opFanout(ctx) }},
		{name: "write-small", kind: requiredCell, stage: opStageWrite, run: func(ctx context.Context, b *battery) *outcome { return b.opWriteSmall(ctx) }},
		{name: "write-large", kind: requiredCell, stage: opStageWrite, run: func(ctx context.Context, b *battery) *outcome { return b.opWriteLarge(ctx) }},
		{name: "create", kind: requiredCell, stage: opStageWrite, run: func(ctx context.Context, b *battery) *outcome { return b.opCreate(ctx) }},
		{name: "mkdir-rmdir", kind: requiredCell, stage: opStageWrite, run: func(ctx context.Context, b *battery) *outcome { return b.opMkdirRmdir(ctx) }},
		{name: "conflict", kind: requiredCell, stage: opStageWrite, run: func(ctx context.Context, b *battery) *outcome { return b.opConflict(ctx) }},
		{name: "rename", kind: requiredCell, stage: opStageRename, run: func(ctx context.Context, b *battery) *outcome { return b.opRename(ctx) }},
		{name: "delete", kind: requiredCell, stage: opStageDelete, run: func(ctx context.Context, b *battery) *outcome { return b.opDelete(ctx) }},
	}
}

// local reads a fixture file from the served tree. The battery and the daemon
// run on the same host in the same directory, so the byte-level proofs (AC-3's
// "the file is unchanged", A-1's atomicity control) read the tree directly —
// they are assertions about the server's effect on disk, not about HTTP.
func (b *battery) local(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(b.root, filepath.FromSlash(rel)))
}

// ── 1 stat ───────────────────────────────────────────────────────────────────

func (b *battery) opStat(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	rel := fixSmallFile
	res := b.send(ctx, o, requestSpec{Method: http.MethodHead, Path: rel})
	if res.Status != http.StatusOK {
		o.fail("HEAD %s answered %d, want 200", rel, res.Status)
		return o
	}
	if res.Bytes != 0 {
		o.fail("HEAD transferred %d body bytes; a stat must not fetch the entity", res.Bytes)
	}
	want, err := sha256OfFile(filepath.Join(b.root, rel))
	if err != nil {
		o.fail("hash the fixture: %v", err)
		return o
	}
	if got := res.Header.Get("ETag"); got != `"`+want+`"` {
		o.fail("ETag %q, want the content hash %q", got, want)
	}
	if got := res.Header.Get("X-Bunker-Hash"); got != want {
		o.fail("X-Bunker-Hash %q, want %q", got, want)
	}
	o.detail = "ETag and X-Bunker-Hash are the file's content hash"
	return o
}

// ── 2/3 reads ────────────────────────────────────────────────────────────────

func (b *battery) readEntity(ctx context.Context, o *outcome, rel string, wantSize int64, label string) {
	res := b.get(ctx, o, rel)
	if res.Status != http.StatusOK {
		o.fail("%s: GET %s answered %d, want 200", label, rel, res.Status)
		return
	}
	want, err := sha256OfFile(filepath.Join(b.root, rel))
	if err != nil {
		o.fail("%s: hash the fixture: %v", label, err)
		return
	}
	if got := sha256OfBytes(res.Body); got != want {
		o.fail("%s: body hash %s, want %s", label, got, want)
	}
	if res.Bytes != wantSize {
		o.fail("%s: transferred %d bytes, want %d", label, res.Bytes, wantSize)
	}
	if got := res.Header.Get("X-Bunker-Hash"); got != want {
		o.fail("%s: X-Bunker-Hash %q, want %q", label, got, want)
	}
	if res.Bytes == 0 {
		o.fail("%s: EMPTY body counted as a read", label)
	}
	o.detail = fmt.Sprintf("%d bytes, hash matches", res.Bytes)
}

func (b *battery) opReadSmall(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	b.readEntity(ctx, o, fixSmallFile, fixSmallBytes, "read-small")
	return o
}

func (b *battery) opReadLarge(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	b.readEntity(ctx, o, fixBigFile, b.fx.BigBytes, "read-large")
	return o
}

// ── 4/5 writes ───────────────────────────────────────────────────────────────

// putEntity writes one file and verifies the bytes that landed, both through
// the surface and on disk. A write cell that reports 201 without checking what
// is on disk would pass on a server that discarded the body.
func (b *battery) putEntity(ctx context.Context, o *outcome, rel string, body []byte, label string) {
	res := b.send(ctx, o, requestSpec{Method: http.MethodPut, Path: rel, Body: body})
	if res.Status != http.StatusCreated && res.Status != http.StatusNoContent {
		o.fail("%s: PUT %s answered %d, want 201/204", label, rel, res.Status)
		return
	}
	want := sha256OfBytes(body)
	if got := res.Header.Get("X-Bunker-Hash"); got != want {
		o.fail("%s: response hash %q, want %q", label, got, want)
	}
	readBack, err := b.local(rel)
	if err != nil {
		o.fail("%s: read back %s: %v", label, rel, err)
		return
	}
	onDisk := sha256OfBytes(readBack)
	if onDisk != want {
		o.fail("%s: bytes on disk hash %s, want %s — the body did not land", label, onDisk, want)
	}
	back := b.check(ctx, requestSpec{Method: http.MethodGet, Path: b.path(rel)}, o)
	if back.Status != http.StatusOK || sha256OfBytes(back.Body) != want {
		o.fail("%s: read-back answered %d with hash %s, want 200/%s", label, back.Status, sha256OfBytes(back.Body), want)
	}
	o.detail = fmt.Sprintf("%d bytes written and read back", len(body))
}

func (b *battery) opWriteSmall(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	b.putEntity(ctx, o, fixScratchDir+"/write-small.txt", deterministicBytes("write-small", fixSmallBytes), "write-small")
	return o
}

func (b *battery) opWriteLarge(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	body := deterministicBytes("write-large", int(b.fx.BigBytes))
	b.putEntity(ctx, o, fixScratchDir+"/big-write.bin", body, "write-large")
	return o
}

// ── 6/7 listings ─────────────────────────────────────────────────────────────

// propfindIdentity is the property set a client with our identity rule asks for:
// the ETag, the length, the resource type, and the two extension properties that
// carry the content hash and the tree revision. Naming b:hash is deliberate —
// it is the expensive live property (BFS-004 §7.4), and a listing that never
// asks for it would measure a cheaper surface than the mount will use.
const propfindIdentity = `<?xml version="1.0" encoding="utf-8"?>` +
	`<D:propfind xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1">` +
	`<D:prop><D:resourcetype/><D:getcontentlength/><D:getetag/><b:hash/><b:rev/></D:prop>` +
	`</D:propfind>`

type msResponse struct {
	Href string `xml:"href"`
}
type msDoc struct {
	Responses []msResponse `xml:"response"`
}

func parseMultistatus(body []byte) ([]string, error) {
	var doc msDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	hrefs := make([]string, 0, len(doc.Responses))
	for _, r := range doc.Responses {
		hrefs = append(hrefs, r.Href)
	}
	return hrefs, nil
}

// listCollection performs one Depth: 1 PROPFIND and returns the hrefs it
// received, checking the two things that make the cell non-vacuous: it must be a
// 207 with a multistatus body, and the members must be the ones on disk.
func (b *battery) listCollection(ctx context.Context, o *outcome, rel string, label string) ([]string, bool) {
	res := b.send(ctx, o, requestSpec{
		Method:   "PROPFIND",
		Path:     rel,
		Headers:  map[string]string{"Depth": "1"},
		Body:     []byte(propfindIdentity),
		BodyType: "application/xml; charset=utf-8",
	})
	if res.Status != 207 {
		o.fail("%s: PROPFIND %s Depth: 1 answered %d, want 207", label, rel, res.Status)
		return nil, false
	}
	hrefs, err := parseMultistatus(res.Body)
	if err != nil {
		o.fail("%s: multistatus did not parse: %v", label, err)
		return nil, false
	}
	if len(hrefs) == 0 {
		o.fail("%s: 207 with an EMPTY multistatus — zero members counted as a listing", label)
		return nil, false
	}
	if !strings.Contains(string(res.Body), "<b:hash>sha256:") {
		o.fail("%s: the listing carried no b:hash values", label)
	}
	wantSelf := b.path(rel)
	if !strings.HasSuffix(wantSelf, "/") {
		wantSelf += "/"
	}
	if !containsHref(hrefs, wantSelf) {
		o.fail("%s: the multistatus does not describe %s itself (hrefs: %v)", label, wantSelf, hrefs)
	}
	for _, f := range b.fx.FilesInDir[rel] {
		if !containsHref(hrefs, wantSelf+f) {
			o.fail("%s: member %s missing from the multistatus", label, f)
			break
		}
	}
	return hrefs, true
}

func containsHref(hrefs []string, want string) bool {
	for _, h := range hrefs {
		if h == want {
			return true
		}
	}
	return false
}

func (b *battery) opListShallow(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	hrefs, ok := b.listCollection(ctx, o, "d0", "list-shallow")
	if ok {
		o.detail = fmt.Sprintf("%d members", len(hrefs))
	}
	return o
}

func (b *battery) opListDeep(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	hrefs, ok := b.listCollection(ctx, o, fixNestedDir, "list-deep")
	if ok {
		o.detail = fmt.Sprintf("%d members at depth 3", len(hrefs))
	}
	return o
}

// ── 12 the whole-tree walk ───────────────────────────────────────────────────

// walkScheduler is the walk's own frontier: work-stealing over directories and
// files, so the walk is as concurrent as the arm's in-flight bound allows. A
// sequential BFS would have measured one request at a time no matter what the
// arm's concurrency was — which is the failure this structure exists to avoid.
type walkScheduler struct {
	mu    sync.Mutex
	cond  *sync.Cond
	dirs  []string
	files []string
	busy  int
	seenD map[string]bool
	seenF map[string]bool
	errs  []string
}

func newWalkScheduler() *walkScheduler {
	w := &walkScheduler{seenD: map[string]bool{}, seenF: map[string]bool{}}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *walkScheduler) next() (kind, rel string, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.dirs) == 0 && len(w.files) == 0 && w.busy > 0 {
		w.cond.Wait()
	}
	switch {
	case len(w.dirs) > 0:
		rel = w.dirs[0]
		w.dirs = w.dirs[1:]
	case len(w.files) > 0:
		rel = w.files[0]
		w.files = w.files[1:]
		kind = "file"
		return kind, rel, true
	default:
		return "", "", false
	}
	w.busy++
	return "dir", rel, true
}

func (w *walkScheduler) finishDir(dir string, subDirs, subFiles []string, errs ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.busy--
	if len(errs) > 0 {
		w.errs = append(w.errs, errs...)
	} else {
		w.seenD[dir] = true
		w.dirs = append(w.dirs, subDirs...)
		w.files = append(w.files, subFiles...)
	}
	w.cond.Broadcast()
}

func (w *walkScheduler) finishFile(rel string, errs ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(errs) > 0 {
		w.errs = append(w.errs, errs...)
	} else {
		w.seenF[rel] = true
	}
	w.cond.Broadcast()
}

func (w *walkScheduler) snapshot() (dirs, files, errs []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for d := range w.seenD {
		dirs = append(dirs, d)
	}
	for f := range w.seenF {
		files = append(files, f)
	}
	sort.Strings(dirs)
	sort.Strings(files)
	return dirs, files, w.errs
}

// opWalkTree is the whole-tree walk: a Depth: 1 PROPFIND for every directory
// discovered (what READDIR costs the mount) plus a HEAD for every file
// discovered (what the GETATTR/stat storm costs it). It discovers the tree from
// the walk itself rather than from the local fixture — a hard-coded target list
// would measure the harness, not the server — and then asserts the discovered
// set against the fixture, so a walk that returns nothing cannot pass.
func (b *battery) opWalkTree(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	sched := newWalkScheduler()
	sched.dirs = append(sched.dirs, "")

	workers := cap(b.c.sem)
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				kind, rel, ok := sched.next()
				if !ok {
					return
				}
				switch kind {
				case "file":
					res := b.c.do(ctx, requestSpec{Method: http.MethodHead, Path: b.path(rel)})
					o.add(res)
					if res.Status != http.StatusOK {
						sched.finishFile(rel, fmt.Sprintf("HEAD %s answered %d", rel, res.Status))
						continue
					}
					sched.finishFile(rel)
				default:
					res := b.c.do(ctx, requestSpec{
						Method:   "PROPFIND",
						Path:     b.path(rel),
						Headers:  map[string]string{"Depth": "1"},
						Body:     []byte(propfindIdentity),
						BodyType: "application/xml; charset=utf-8",
					})
					o.add(res)
					if res.Status != 207 {
						sched.finishDir(rel, nil, nil, fmt.Sprintf("PROPFIND %s answered %d", rel, res.Status))
						continue
					}
					subDirs, subFiles, err := b.classifyMembers(rel, res.Body)
					if err != nil {
						sched.finishDir(rel, nil, nil, fmt.Sprintf("PROPFIND %s: %v", rel, err))
						continue
					}
					sched.finishDir(rel, subDirs, subFiles)
				}
			}
		}()
	}
	wg.Wait()

	dirs, files, errs := sched.snapshot()
	for _, e := range errs {
		o.fail("%s", e)
	}
	if len(files) == 0 {
		o.fail("the walk discovered ZERO files — an empty result is not a fast walk")
	}
	if missing := diffSets(b.fx.Files, files); len(missing) > 0 {
		o.fail("the walk missed %d of %d files (first: %s)", len(missing), len(b.fx.Files), missing[0])
	}
	if extra := diffSets(files, b.fx.Files); len(extra) > 0 {
		o.fail("the walk discovered %d files that are not in the fixture (first: %s)", len(extra), extra[0])
	}
	if missing := diffSets(b.fx.Dirs, dirs); len(missing) > 0 {
		o.fail("the walk missed %d of %d directories (first: %q)", len(missing), len(b.fx.Dirs), missing[0])
	}
	o.detail = fmt.Sprintf("%d dirs + %d files discovered, matching the fixture", len(dirs), len(files))
	return o
}

// classifyMembers turns one multistatus into the child directories and files it
// names. Collections are identified by the trailing slash §8.3 requires on a
// collection's href — the same discriminator a stock client uses.
func (b *battery) classifyMembers(dir string, body []byte) (subDirs, subFiles []string, err error) {
	hrefs, err := parseMultistatus(body)
	if err != nil {
		return nil, nil, err
	}
	prefix := strings.TrimSuffix(b.base, "/") + "/"
	self := b.path(dir)
	if dir == "" {
		self = b.base
	}
	if !strings.HasSuffix(self, "/") {
		self += "/"
	}
	for _, h := range hrefs {
		if h == self || h == strings.TrimSuffix(self, "/") {
			continue
		}
		if !strings.HasPrefix(h, prefix) {
			continue
		}
		rel := strings.TrimPrefix(h, prefix)
		rel = strings.TrimSuffix(rel, "/")
		if rel == "" {
			continue
		}
		if strings.HasSuffix(h, "/") {
			subDirs = append(subDirs, rel)
			continue
		}
		subFiles = append(subFiles, rel)
	}
	return subDirs, subFiles, nil
}

// diffSets returns the members of want that are absent from got.
func diffSets(want, got []string) []string {
	have := make(map[string]bool, len(got))
	for _, g := range got {
		have[g] = true
	}
	var missing []string
	for _, w := range want {
		if !have[w] {
			missing = append(missing, w)
		}
	}
	return missing
}

// ── 13 the git-status-shaped op ──────────────────────────────────────────────

// snapshotArgs is the E-4 argument vocabulary this cell drives.
type snapshotArgs struct {
	Path        string `json:"path"`
	Depth       string `json:"depth"`
	IncludeHash bool   `json:"include_hash"`
}

// snapshotEnvelope is the part of the E-4 envelope the cell reads. The surface
// answers one JSON shape for success and failure alike, so the cell can decode
// the failure too instead of guessing from a status code.
type snapshotEnvelope struct {
	OK        bool   `json:"ok"`
	Op        string `json:"op"`
	Verdict   string `json:"verdict"`
	Proto     string `json:"proto"`
	Truncated bool   `json:"truncated"`
	Result    struct {
		Count   int `json:"count"`
		Entries []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Hash string `json:"hash"`
		} `json:"entries"`
	} `json:"result"`
	Error *struct {
		Detail string `json:"detail"`
	} `json:"error"`
}

// opGitStatusShaped is the delegated whole-tree read: ONE request that returns
// the state of the whole tree, which is the shape that makes a git-status-class
// operation affordable (PRD-bunker-fs.md:23-31, "the same 14 operations ...
// complete in 0.41 s flat with git running on the host"). It is one request by
// construction, so concurrency cannot change it — which is exactly why the
// delegated path is the answer for whole-tree work and the walk is not.
func (b *battery) opGitStatusShaped(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	body, err := json.Marshal(snapshotArgs{Path: "", Depth: "infinity", IncludeHash: true})
	if err != nil {
		o.fail("marshal snapshot args: %v", err)
		return o
	}
	res := b.send(ctx, o, requestSpec{
		Method:   http.MethodPost,
		Path:     "",
		Headers:  map[string]string{"X-Bunker-Op": "snapshot"},
		Body:     body,
		BodyType: "application/json",
	})
	if res.Status != http.StatusOK {
		o.fail("X-Bunker-Op: snapshot answered %d, want 200", res.Status)
		return o
	}
	var env snapshotEnvelope
	if err := json.Unmarshal(res.Body, &env); err != nil {
		o.fail("envelope did not parse: %v", err)
		return o
	}
	if !env.OK || env.Verdict != "ok" {
		o.fail("envelope ok=%v verdict=%q error=%v", env.OK, env.Verdict, env.Error)
	}
	if env.Truncated {
		o.fail("the delegated read was TRUNCATED: a partial tree is not the tree's state")
	}
	want := len(b.fx.Files) + len(b.fx.Dirs)
	if env.Result.Count != want {
		o.fail("snapshot returned %d entries, want %d (files+dirs)", env.Result.Count, want)
	}
	hashes := 0
	for _, e := range env.Result.Entries {
		if strings.HasPrefix(e.Hash, "sha256:") {
			hashes++
		}
	}
	if hashes == 0 {
		o.fail("include_hash produced no hashes")
	}
	o.detail = fmt.Sprintf("1 request returned %d entries (%d hashes)", env.Result.Count, hashes)
	return o
}

// ── 8/9/10 create, rename, delete ────────────────────────────────────────────

func (b *battery) opCreate(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	rel := fixScratchDir + "/new.txt"
	body := deterministicBytes("create", 256)
	res := b.send(ctx, o, requestSpec{
		Method:  http.MethodPut,
		Path:    rel,
		Headers: map[string]string{"If-None-Match": "*"},
		Body:    body,
	})
	if res.Status != http.StatusCreated {
		o.fail("create-only PUT answered %d, want 201", res.Status)
		return o
	}
	back := b.check(ctx, requestSpec{Method: http.MethodGet, Path: b.path(rel)}, o)
	if back.Status != http.StatusOK || sha256OfBytes(back.Body) != sha256OfBytes(body) {
		o.fail("created file reads back as %d/%s", back.Status, sha256OfBytes(back.Body))
	}
	o.detail = "201 created, bytes verified"
	return o
}

func (b *battery) opRename(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	src := fixScratchDir + "/new.txt"
	dst := fixScratchDir + "/renamed.txt"
	res := b.send(ctx, o, requestSpec{
		Method:  "MOVE",
		Path:    src,
		Headers: map[string]string{"Destination": b.path(dst)},
	})
	if res.Status != http.StatusCreated && res.Status != http.StatusNoContent {
		o.fail("MOVE answered %d, want 201/204", res.Status)
		return o
	}
	gone := b.check(ctx, requestSpec{Method: http.MethodHead, Path: b.path(src)}, o)
	if gone.Status != http.StatusNotFound {
		o.fail("the source still answers %d after the rename", gone.Status)
	}
	back := b.check(ctx, requestSpec{Method: http.MethodGet, Path: b.path(dst)}, o)
	if back.Status != http.StatusOK {
		o.fail("the destination answers %d after the rename", back.Status)
	}
	o.detail = "source gone, destination present"
	return o
}

func (b *battery) opDelete(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	rel := fixScratchDir + "/renamed.txt"
	res := b.send(ctx, o, requestSpec{Method: http.MethodDelete, Path: rel})
	if res.Status != http.StatusNoContent && res.Status != http.StatusOK {
		o.fail("DELETE answered %d, want 204/200", res.Status)
		return o
	}
	gone := b.check(ctx, requestSpec{Method: http.MethodHead, Path: b.path(rel)}, o)
	if gone.Status != http.StatusNotFound {
		o.fail("the deleted file still answers %d", gone.Status)
	}
	o.detail = "deleted, 404 on read-back"
	return o
}

// ── 11 mkdir / rmdir ─────────────────────────────────────────────────────────

func (b *battery) opMkdirRmdir(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	rel := fixScratchDir + "/dir"
	mk := b.send(ctx, o, requestSpec{Method: "MKCOL", Path: rel})
	if mk.Status != http.StatusCreated {
		o.fail("MKCOL answered %d, want 201", mk.Status)
		return o
	}
	rm := b.send(ctx, o, requestSpec{Method: http.MethodDelete, Path: rel})
	if rm.Status != http.StatusNoContent && rm.Status != http.StatusOK {
		o.fail("collection DELETE answered %d, want 204/200", rm.Status)
		return o
	}
	gone := b.check(ctx, requestSpec{
		Method:  "PROPFIND",
		Path:    b.path(rel),
		Headers: map[string]string{"Depth": "0"},
	}, o)
	if gone.Status != http.StatusNotFound {
		o.fail("the removed collection still answers %d", gone.Status)
	}
	o.detail = "201 then 204, 404 on read-back"
	return o
}

// ── 14 the conflict ──────────────────────────────────────────────────────────

// opConflict is the cell the whole write path was reconciled for (BFS-004 §6,
// BFS-014's in-commit re-validation): reader A holds a base hash, writer B moves
// the file, and A's write with the STALE base must be refused — with both hashes
// named, and with the file left byte-identical. Every part of that sentence is
// asserted here; a 412 without the hash pair, or a 412 that still wrote, fails.
func (b *battery) opConflict(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	rel := fixConflictFile

	// A reads the base, and holds it.
	head := b.send(ctx, o, requestSpec{Method: http.MethodHead, Path: rel})
	if head.Status != http.StatusOK {
		o.fail("HEAD %s answered %d, want 200", rel, head.Status)
		return o
	}
	staleBase := head.Header.Get("ETag")
	if staleBase == "" {
		o.fail("HEAD %s carried no ETag — there is no base to be stale about", rel)
		return o
	}

	// B moves the file, unconditionally (a stock client's write).
	byB := deterministicBytes("conflict-writer-B", fixSmallBytes)
	moved := b.send(ctx, o, requestSpec{Method: http.MethodPut, Path: rel, Body: byB})
	if moved.Status != http.StatusNoContent && moved.Status != http.StatusCreated {
		o.fail("writer B's PUT answered %d, want 204/201", moved.Status)
		return o
	}

	// A writes with the base it read. This MUST be refused.
	byA := deterministicBytes("conflict-writer-A", fixSmallBytes)
	res := b.send(ctx, o, requestSpec{
		Method:  http.MethodPut,
		Path:    rel,
		Headers: map[string]string{"If-Match": staleBase},
		Body:    byA,
	})
	if res.Status != http.StatusPreconditionFailed {
		o.fail("STALE-BASE WRITE ANSWERED %d, WANT 412 — the conflict rule did not refuse", res.Status)
		return o
	}
	if got := res.Header.Get("X-Bunker-Verdict"); got != "hash_mismatch" {
		o.fail("refusal verdict %q, want hash_mismatch", got)
	}
	wantCurrent := sha256OfBytes(byB)
	if got := res.Header.Get("X-Bunker-Current-Hash"); got != wantCurrent {
		o.fail("X-Bunker-Current-Hash %q, want %q", got, wantCurrent)
	}
	if got := res.Header.Get("X-Bunker-Expected-Hash"); strings.Trim(got, `"`) != strings.Trim(staleBase, `"`) {
		o.fail("X-Bunker-Expected-Hash %q, want the stale base %q", got, staleBase)
	}
	if !strings.Contains(string(res.Body), "<b:current>") || !strings.Contains(string(res.Body), "<b:expected>") {
		o.fail("the refusal body names only one hash: %s", truncate(string(res.Body), 200))
	}

	// The refused write changed nothing: the file still holds writer B's bytes.
	onDiskRaw, err := b.local(rel)
	if err != nil {
		o.fail("read %s back: %v", rel, err)
		return o
	}
	onDisk := sha256OfBytes(onDiskRaw)
	if onDisk != wantCurrent {
		o.fail("the REFUSED write changed the file: disk hash %s, want %s", onDisk, wantCurrent)
	}
	if onDisk == sha256OfBytes(byA) {
		o.fail("the refused writer's bytes are the ones on disk")
	}

	// The same base with IDENTICAL bytes is the reported no-op (D3), not a
	// second refusal — the touch-without-change rule that stops the check from
	// nagging. Reported inside this cell because it is the same exchange.
	noop := b.check(ctx, requestSpec{
		Method:  http.MethodPut,
		Path:    b.path(rel),
		Headers: map[string]string{"If-Match": staleBase},
		Body:    byB,
	}, o)
	if noop.Status != http.StatusNoContent || noop.Header.Get("X-Bunker-Verdict") != "identical_content" {
		o.fail("stale base + identical bytes answered %d/%s, want 204/identical_content",
			noop.Status, noop.Header.Get("X-Bunker-Verdict"))
	}
	o.detail = "412 hash_mismatch with both hashes; bytes unchanged; identical-content arm is 204"
	return o
}

// ── the extra arm: 100 independent requests ──────────────────────────────────

// opFanout is one hundred INDEPENDENT requests, issued concurrently and bounded
// only by the arm's in-flight limit. That is the exact shape the PRD's own A/B
// used to measure the lever — "100 sequential 19.66 s / 100 concurrent 0.79 s"
// over HTTP/2 at MaxConnsPerHost=1 (PRD-bunker-fs.md:60-61) — and the reason it
// dispatches goroutines rather than looping: a sequential loop would report the
// "100 sequential" arm at every concurrency, which is a battery that cannot see
// the thing it exists to measure.
func (b *battery) opFanout(ctx context.Context) *outcome {
	o := &outcome{ok: true}
	want, err := sha256OfFile(filepath.Join(b.root, fixSmallFile))
	if err != nil {
		o.fail("hash the fixture: %v", err)
		return o
	}
	var wg sync.WaitGroup
	for i := 0; i < b.fanout; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := b.c.do(ctx, requestSpec{Method: http.MethodGet, Path: b.path(fixSmallFile)})
			o.add(res)
			if res.Status != http.StatusOK || sha256OfBytes(res.Body) != want {
				o.fail("fan-out request %d answered %d (hash %s)", i, res.Status, sha256OfBytes(res.Body))
			}
		}(i)
	}
	wg.Wait()
	timed, _, _, _ := o.snapshot()
	o.detail = fmt.Sprintf("%d requests issued concurrently (bounded at %d in flight), all verified", len(timed), cap(b.c.sem))
	return o
}

// describeFailing names the calls inside a cell that did not succeed, with the
// protocol each side reported and their wall times — the shape this row requires
// a stall to be reported in: protocol, op, concurrency, wall time, never smoothed
// into a pass.
func describeFailing(calls []*call) string {
	var parts []string
	for _, c := range calls {
		if c.class() == "ok" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s [%s]", c.describe(), ms(c.Dur)))
		if len(parts) == 3 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "failing calls: " + strings.Join(parts, " | ")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── notes: the declared deviations and capabilities, asserted not assumed ─────

// note is one untimed contract assertion about the surface under measurement.
type note struct {
	name   string
	ok     bool
	detail string
}

// notes asserts the four declared deviations and the capability answers this
// battery depends on. They are not timed cells: they exist so the numbers in the
// table are known to come from the surface the spec describes — a battery that
// silently measured a different surface would be worse than no battery.
func (b *battery) notes(ctx context.Context) []note {
	out := make([]note, 0, 6)
	pf := func(spec requestSpec) *call {
		spec.Path = b.path(spec.Path)
		return b.c.do(ctx, spec)
	}

	opts := pf(requestSpec{Method: http.MethodOptions, Path: ""})
	out = append(out, note{
		name: "OPTIONS advertises DAV: 1 + Allow + extensions",
		ok: opts.Status == http.StatusOK && opts.Header.Get("DAV") == "1" &&
			strings.Contains(opts.Header.Get("Allow"), "PROPFIND") &&
			strings.Contains(opts.Header.Get("X-Bunker-Extensions"), "op"),
		detail: fmt.Sprintf("status=%d DAV=%q allow=%q", opts.Status, opts.Header.Get("DAV"), truncate(opts.Header.Get("Allow"), 120)),
	})

	// Deviation 3: the walk must never need this, and the surface must refuse it.
	inf := pf(requestSpec{
		Method:   "PROPFIND",
		Path:     "d0",
		Headers:  map[string]string{"Depth": "infinity"},
		Body:     []byte(propfindIdentity),
		BodyType: "application/xml; charset=utf-8",
	})
	out = append(out, note{
		name:   "PROPFIND Depth: infinity is refused (deviation 3) — the walk recursed instead",
		ok:     inf.Status == http.StatusForbidden && inf.Header.Get("X-Bunker-Verdict") == "propfind_finite_depth",
		detail: fmt.Sprintf("status=%d verdict=%q", inf.Status, inf.Header.Get("X-Bunker-Verdict")),
	})

	// Deviation 1: GET on a collection.
	col := pf(requestSpec{Method: http.MethodGet, Path: "d0"})
	out = append(out, note{
		name:   "GET on a collection is 405 + Allow (deviation 1)",
		ok:     col.Status == http.StatusMethodNotAllowed && strings.Contains(col.Header.Get("Allow"), "PROPFIND"),
		detail: fmt.Sprintf("status=%d", col.Status),
	})

	// The E-4 op the task row names, and the honest answer this build gives.
	statusOp := pf(requestSpec{
		Method:   http.MethodPost,
		Path:     "",
		Headers:  map[string]string{"X-Bunker-Op": "status"},
		Body:     []byte(`{"path":""}`),
		BodyType: "application/json",
	})
	out = append(out, note{
		name:   "X-Bunker-Op: status is NOT in this build — it answers a structured 501, never a silent no-op",
		ok:     statusOp.Status == http.StatusNotImplemented && statusOp.Header.Get("X-Bunker-Verdict") == "capability_unavailable",
		detail: fmt.Sprintf("status=%d body=%s", statusOp.Status, truncate(string(statusOp.Body), 160)),
	})

	// No watcher on this target: the declared degradation, naming the mode.
	watch := pf(requestSpec{
		Method:   http.MethodPost,
		Path:     "",
		Headers:  map[string]string{"X-Bunker-Op": "watch"},
		Body:     []byte(`{}`),
		BodyType: "application/json",
	})
	out = append(out, note{
		name:   "X-Bunker-Op: watch degrades loudly with mode=poll",
		ok:     watch.Status == http.StatusNotImplemented && strings.Contains(watch.Header.Get("X-Bunker-Capability"), "mode=poll"),
		detail: fmt.Sprintf("status=%d capability=%q", watch.Status, watch.Header.Get("X-Bunker-Capability")),
	})

	// An unknown op is a different diagnosis from an absent capability.
	unknown := pf(requestSpec{
		Method:   http.MethodPost,
		Path:     "",
		Headers:  map[string]string{"X-Bunker-Op": "frobnicate"},
		Body:     []byte(`{}`),
		BodyType: "application/json",
	})
	out = append(out, note{
		name:   "an op outside the catalogue is 400 op_unknown (distinct from capability_unavailable)",
		ok:     unknown.Status == http.StatusBadRequest && unknown.Header.Get("X-Bunker-Verdict") == "op_unknown",
		detail: fmt.Sprintf("status=%d verdict=%q", unknown.Status, unknown.Header.Get("X-Bunker-Verdict")),
	})

	return out
}

// warmUp establishes the connection (and, over QUIC, the handshake) before any
// timed cell, so the first cell does not pay for the transport setup the study's
// per-request figures never paid. It is untimed and reported as a note.
func (b *battery) warmUp(ctx context.Context) (string, bool) {
	c := b.c.do(ctx, requestSpec{Method: http.MethodGet, Path: b.path(fixRootFile)})
	if c.Status != http.StatusOK {
		return fmt.Sprintf("warm-up GET answered %d (%v)", c.Status, c.Err), false
	}
	return fmt.Sprintf("warm-up GET 200, %s (client %s / server %s)",
		c.Dur.Round(time.Microsecond), orNone(c.ClientProto), orNone(c.ServerProto)), true
}
