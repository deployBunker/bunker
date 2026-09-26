// Tests for the battery harness itself.
//
// Two kinds of test live here, and the difference matters:
//
//   - Pure table-driven tests over the pieces that decide what a cell reports
//     (classification, protocol observation, ratio verdicts, CSV round-trip,
//     ordering). These are cheap and they pin the vocabularies the report speaks.
//
//   - One end-to-end test that runs a WHOLE ARM against a live, in-process
//     WebDAV surface (httptest + this repository's own handler). Without it the
//     battery's assertions could all be vacuous — a harness that reports "all
//     cells ok" against a server that 404s everything is the failure mode this
//     row exists to prevent.
package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/server/webdav"
)

func TestDeterministicBytes(t *testing.T) {
	a := deterministicBytes("small.txt", 1024)
	b := deterministicBytes("small.txt", 1024)
	c := deterministicBytes("other.txt", 1024)
	if len(a) != 1024 || len(c) != 1024 {
		t.Fatalf("lengths: %d %d, want 1024", len(a), len(c))
	}
	if string(a) != string(b) {
		t.Fatal("the same seed produced different bytes; two arms would not be comparable")
	}
	if string(a) == string(c) {
		t.Fatal("different seeds produced identical bytes")
	}
	if sha256OfBytes(a) != sha256OfBytes(b) {
		t.Fatal("content hash is not stable for a fixed seed")
	}
}

func TestParseProtoAndWireName(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
		wire    string
	}{
		{"h1", false, "HTTP/1.1"},
		{"H1", false, "HTTP/1.1"},
		{"http/1.1", false, "HTTP/1.1"},
		{"h2", false, "HTTP/2.0"},
		{"http2", false, "HTTP/2.0"},
		{"h3", false, "HTTP/3.0"},
		{"http/3", false, "HTTP/3.0"},
		{"h4", true, ""},
		{"", true, ""},
	}
	for _, tc := range cases {
		got, err := parseProto(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseProto(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseProto(%q): %v", tc.in, err)
			continue
		}
		if got.wireName() != tc.wire {
			t.Errorf("parseProto(%q).wireName() = %q, want %q", tc.in, got.wireName(), tc.wire)
		}
	}
}

func TestCallClassification(t *testing.T) {
	cases := []struct {
		name string
		call *call
		want string
	}{
		{"200", &call{Status: 200}, "ok"},
		{"207", &call{Status: 207}, "ok"},
		{"412 is a refusal, not an error", &call{Status: 412}, "refused"},
		{"404", &call{Status: 404}, "refused"},
		{"deadline is a stall", &call{Err: context.DeadlineExceeded}, "stall"},
		{"wrapped deadline is a stall", &call{Err: errors.New("x: " + context.DeadlineExceeded.Error())}, "error"},
		{"transport error", &call{Err: errors.New("connection refused")}, "error"},
	}
	// A wrapped deadline must still read as a stall: the cell's deadline is what
	// turns a hung transport into a reported number, and losing that distinction
	// would smooth a stall into an error.
	wrapped := &call{Err: errTimeout{}}
	cases = append(cases, struct {
		name string
		call *call
		want string
	}{"a timeout() error is a stall", wrapped, "stall"})

	for _, tc := range cases {
		if got := tc.call.class(); got != tc.want {
			t.Errorf("%s: class() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

type errTimeout struct{}

func (errTimeout) Error() string   { return "i/o timeout" }
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }

func TestProtoOK(t *testing.T) {
	cases := []struct {
		name                 string
		client, server, want string
		ok                   bool
	}{
		{"h2 both sides", "HTTP/2.0", "HTTP/2.0", "HTTP/2.0", true},
		{"h3 both sides", "HTTP/3.0", "HTTP/3.0", "HTTP/3.0", true},
		{"client downgraded", "HTTP/1.1", "HTTP/1.1", "HTTP/2.0", false},
		{"server saw something else", "HTTP/2.0", "HTTP/1.1", "HTTP/2.0", false},
		{"no observation at all", "", "", "HTTP/2.0", false},
	}
	for _, tc := range cases {
		c := &call{ClientProto: tc.client, ServerProto: tc.server}
		if got := c.protoOK(tc.want); got != tc.ok {
			t.Errorf("%s: protoOK(%q) = %v, want %v", tc.name, tc.want, got, tc.ok)
		}
	}
}

func TestStatusList(t *testing.T) {
	cases := []struct {
		in   []int
		want string
	}{
		{nil, "-"},
		{[]int{200}, "200"},
		{[]int{207, 200, 200}, "207,2x200"},
		{[]int{200, 204, 412}, "200,204,412"},
		{[]int{200, 200, 200}, "3x200"},
	}
	for _, tc := range cases {
		if got := statusList(tc.in); got != tc.want {
			t.Errorf("statusList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsContended(t *testing.T) {
	cases := []struct {
		before, after string
		cpus          int
		want          bool
	}{
		{"1.0", "1.0", 16, false},
		{"15.9", "2.0", 16, false},
		{"16.0", "1.0", 16, true},
		{"1.0", "20.0", 16, true},
		{"unavailable", "1.0", 16, false},
		{"4.0", "4.0", 0, false},
	}
	for _, tc := range cases {
		if got := isContended(tc.before, tc.after, tc.cpus); got != tc.want {
			t.Errorf("isContended(%q,%q,%d) = %v, want %v", tc.before, tc.after, tc.cpus, got, tc.want)
		}
	}
}

func TestRatioTextAndClassify(t *testing.T) {
	cases := []struct {
		a, b       float64
		degraded   bool
		wantSubstr string
	}{
		{2000, 1000, true, "2.0000x DEGRADED"},
		{1999.9, 1000, false, "1.9999x not-degraded"},
		{500, 1000, false, "0.5000x not-degraded"},
		{1000, 0, false, "n/a"},
	}
	for _, tc := range cases {
		got := ratioText(tc.a, tc.b)
		if !strings.Contains(got, tc.wantSubstr) {
			t.Errorf("ratioText(%v,%v) = %q, want it to contain %q", tc.a, tc.b, got, tc.wantSubstr)
		}
		if classifyRatio(got) != tc.degraded {
			t.Errorf("classifyRatio(%q) = %v, want %v", got, !tc.degraded, tc.degraded)
		}
	}
}

// A representative multistatus: the collection's own href carries the trailing
// slash §8.3 requires, and its members are a sub-collection and a file.
const sampleMultistatus = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1">
  <D:response><D:href>/dav/d0/</D:href><D:propstat><D:prop><D:resourcetype><D:collection></D:collection></D:resourcetype><b:hash>sha256:aa</b:hash></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
  <D:response><D:href>/dav/d0/nested/</D:href><D:propstat><D:prop><D:resourcetype><D:collection></D:collection></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
  <D:response><D:href>/dav/d0/f00.txt</D:href><D:propstat><D:prop><D:getetag>"sha256:bb"</D:getetag><b:hash>sha256:bb</b:hash></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
</D:multistatus>`

func TestParseMultistatusAndClassifyMembers(t *testing.T) {
	hrefs, err := parseMultistatus([]byte(sampleMultistatus))
	if err != nil {
		t.Fatalf("parseMultistatus: %v", err)
	}
	want := []string{"/dav/d0/", "/dav/d0/nested/", "/dav/d0/f00.txt"}
	if len(hrefs) != len(want) {
		t.Fatalf("hrefs = %v, want %v", hrefs, want)
	}
	for i := range want {
		if hrefs[i] != want[i] {
			t.Fatalf("href[%d] = %q, want %q", i, hrefs[i], want[i])
		}
	}

	b := &battery{base: "/dav"}
	dirs, files, err := b.classifyMembers("d0", []byte(sampleMultistatus))
	if err != nil {
		t.Fatalf("classifyMembers: %v", err)
	}
	if len(dirs) != 1 || dirs[0] != "d0/nested" {
		t.Errorf("sub-collections = %v, want [d0/nested]", dirs)
	}
	if len(files) != 1 || files[0] != "d0/f00.txt" {
		t.Errorf("files = %v, want [d0/f00.txt]", files)
	}

	// The root's own href is the collection, not a member of itself.
	rootDirs, rootFiles, err := b.classifyMembers("", []byte(sampleMultistatus))
	if err != nil {
		t.Fatalf("classifyMembers(root): %v", err)
	}
	if len(rootDirs) != 2 || len(rootFiles) != 1 {
		t.Errorf("root members = %v/%v, want the collection's members, not the collection itself", rootDirs, rootFiles)
	}
}

func TestDiffSets(t *testing.T) {
	cases := []struct {
		name      string
		want, got []string
		missing   int
	}{
		{"identical", []string{"a", "b"}, []string{"a", "b"}, 0},
		{"one missing", []string{"a", "b"}, []string{"a"}, 1},
		{"empty got", []string{"a"}, nil, 1},
		{"extra ignored", []string{"a"}, []string{"a", "b"}, 0},
	}
	for _, tc := range cases {
		if got := diffSets(tc.want, tc.got); len(got) != tc.missing {
			t.Errorf("%s: diffSets = %v, want %d missing", tc.name, got, tc.missing)
		}
	}
}

func TestOpOrderIsStageThenAppearance(t *testing.T) {
	rows := []*opRow{
		{Op: "rename", Stage: 2},
		{Op: "stat", Stage: 0},
		{Op: "write-small", Stage: 1},
		{Op: "read-small", Stage: 0},
	}
	got := opOrder(rows)
	want := []string{"stat", "read-small", "write-small", "rename"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("opOrder = %v, want %v", got, want)
	}
}

// TestSinksRoundTripAndReportReadsThem proves the report's input contract: the
// files the battery appends are the files the report parses, with the columns it
// reads.
func TestSinksRoundTripAndReportReadsThem(t *testing.T) {
	dir := t.TempDir()
	arm := &armResult{
		label:          "h2-delay20-c1x8",
		link:           "delay20",
		proto:          protoH2,
		conns:          1,
		conc:           8,
		fixture:        &fixtureInfo{Dirs: []string{"", "d0"}, Files: []string{"a", "b"}},
		totalWall:      1500 * time.Millisecond,
		stageWalls:     []stageWall{{stage: 0, wall: 1400 * time.Millisecond}},
		notes:          []note{{name: "n", ok: true, detail: "d"}},
		warmUpOK:       true,
		exitOK:         true,
		observedClient: []string{"HTTP/2.0"},
		observedServer: []string{"HTTP/2.0"},
		cells: []*cellResult{{
			name: "walk-tree", kind: requiredCell, stage: 0, requests: 3, verify: 1,
			work: 30 * time.Millisecond, wall: 40 * time.Millisecond,
			statuses: []int{207, 200, 200}, ok: true, class: "ok",
			clientProtos: []string{"HTTP/2.0"}, serverProtos: []string{"HTTP/2.0"},
		}},
		loadBefore: "1.0", loadAfter: "1.1", cpus: 16,
	}
	if err := writeArmRows(dir, arm); err != nil {
		t.Fatalf("writeArmRows: %v", err)
	}
	arms, err := readArms(filepath.Join(dir, "arms.csv"))
	if err != nil {
		t.Fatalf("readArms: %v", err)
	}
	if len(arms) != 1 || arms[0].Label != arm.label || arms[0].Conns != 1 || arms[0].Conc != 8 {
		t.Fatalf("readArms = %+v", arms)
	}
	if !arms[0].ExitOK || arms[0].TotalWall != 1500 {
		t.Errorf("arm row lost values: %+v", arms[0])
	}
	ops, err := readOps(filepath.Join(dir, "ops.csv"))
	if err != nil {
		t.Fatalf("readOps: %v", err)
	}
	if len(ops) != 1 || ops[0].Op != "walk-tree" || ops[0].Requests != 3 || !ops[0].OK {
		t.Fatalf("op row = %+v", ops[0])
	}
	// Appending a second arm must not duplicate the header.
	arm.label = "h2-delay20-c1x1"
	if err := writeArmRows(dir, arm); err != nil {
		t.Fatalf("second writeArmRows: %v", err)
	}
	arms, err = readArms(filepath.Join(dir, "arms.csv"))
	if err != nil {
		t.Fatalf("re-read arms: %v", err)
	}
	if len(arms) != 2 {
		t.Fatalf("expected 2 arms after the append, got %d", len(arms))
	}
}

// TestArmAgainstLiveWebDAVSurface runs the WHOLE battery once, at concurrency=2,
// against this repository's own WebDAV handler in-process. It is the test that
// makes the other tests mean something: if a cell's expectation were vacuous
// (say the conflict cell accepted anything), it fails here.
func TestArmAgainstLiveWebDAVSurface(t *testing.T) {
	root := t.TempDir()
	fx, err := generateFixture(root, 2, 3, 64*1024)
	if err != nil {
		t.Fatalf("generateFixture: %v", err)
	}
	handler, err := webdav.New(webdav.Config{Root: root, Build: "battery-test"})
	if err != nil {
		t.Fatalf("webdav.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	out := t.TempDir()
	arm, err := runArm(context.Background(), armConfig{
		kind:    protoH1,
		base:    srv.URL,
		root:    root,
		fixture: fx,
		conns:   2,
		conc:    2,
		fanout:  8,
		opLimit: 30 * time.Second,
		timeout: 60 * time.Second,
		label:   "test-arm",
		link:    "loopback",
		outDir:  out,
	})
	if err != nil {
		t.Fatalf("runArm: %v", err)
	}
	if !arm.exitOK {
		for _, c := range arm.cells {
			if !c.ok || c.unreliable {
				t.Errorf("cell %s failed: class=%s detail=%s proto=%s", c.name, c.class, c.detail, c.protoNote)
			}
		}
		for _, n := range arm.notes {
			if !n.ok {
				t.Errorf("contract note failed: %s (%s)", n.name, n.detail)
			}
		}
		t.Fatal("the arm did not pass against a live surface")
	}

	// The specific cells the row grades, asserted on their own so a future
	// regression names the cell rather than "the arm failed".
	byName := map[string]*cellResult{}
	for _, c := range arm.cells {
		byName[c.name] = c
	}
	for _, name := range []string{
		"stat", "read-small", "read-large", "write-small", "write-large",
		"list-shallow", "list-deep", "walk-tree", "git-status-shaped",
		"create", "rename", "delete", "mkdir-rmdir", "conflict", "fanout-100",
	} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("the battery no longer runs the cell %q", name)
		}
		if c.requests == 0 {
			t.Errorf("cell %s issued ZERO requests", name)
		}
	}
	if got := byName["conflict"].statuses; len(got) < 3 || got[len(got)-1] != 412 || !hasStatus(got, 204) {
		t.Errorf("the conflict cell's statuses are %v; want HEAD 200, writer B 204, and the STALE-BASE write last at 412", got)
	}
	if got := byName["walk-tree"].statuses; !hasStatus(got, 207) || !hasStatus(got, 200) {
		t.Errorf("the walk's statuses are %v; want listings and stats", got)
	}
	if len(byName["fanout-100"].statuses) != 8 {
		t.Errorf("the fan-out arm issued %d requests, want 8", len(byName["fanout-100"].statuses))
	}
}

func hasStatus(list []int, want int) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestFixtureGeneratorLayout pins the shape the battery's expectations are built
// from, including the two facts a listing assertion depends on: 25 files per
// collection (Fixture A) and the four root files.
func TestFixtureGeneratorLayout(t *testing.T) {
	root := t.TempDir()
	fx, err := generateFixture(root, 2, 3, 1024)
	if err != nil {
		t.Fatalf("generateFixture: %v", err)
	}
	if len(fx.Files) != 4+2*3+3 {
		t.Errorf("files = %d (%v), want 4 root + 2x3 + 3 nested", len(fx.Files), fx.Files)
	}
	// Dirs: root, d0, d1, d0/nested, d0/nested/deeper, scratch.
	if len(fx.Dirs) != 6 {
		t.Errorf("dirs = %v, want 6", fx.Dirs)
	}
	if fx.BigBytes != 1024 {
		t.Errorf("big.bin bytes = %d, want 1024", fx.BigBytes)
	}
	// The generator's expectation set and the scanner's must be the SAME set:
	// the battery asserts against the scanner (it reads the served tree), while
	// -mode fixture prints the generator's view, and two views of one tree that
	// disagree are a bug that hides as a passing cell.
	scanned, err := scanFixture(root)
	if err != nil {
		t.Fatalf("scanFixture: %v", err)
	}
	if strings.Join(fx.Files, ",") != strings.Join(scanned.Files, ",") {
		t.Errorf("generator files %v != scanner files %v", fx.Files, scanned.Files)
	}
	if strings.Join(fx.Dirs, ",") != strings.Join(scanned.Dirs, ",") {
		t.Errorf("generator dirs %v != scanner dirs %v", fx.Dirs, scanned.Dirs)
	}
	for _, name := range []string{fixRootFile, fixSmallFile, fixBigFile, fixConflictFile} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("root file %s missing: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "scratch")); err != nil {
		t.Errorf("the scratch collection is missing: %v", err)
	}

	// Regenerating must produce a byte-identical tree: that is what makes two
	// arms comparable.
	before, err := sha256OfTreeForTest(root)
	if err != nil {
		t.Fatalf("hash tree: %v", err)
	}
	if _, err := generateFixture(root, 2, 3, 1024); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	after, err := sha256OfTreeForTest(root)
	if err != nil {
		t.Fatalf("hash tree: %v", err)
	}
	if before != after {
		t.Fatal("regenerating the fixture changed its bytes; two arms would not be comparable")
	}
}

func sha256OfTreeForTest(root string) (string, error) {
	info, err := scanFixture(root)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, rel := range info.Files {
		h, err := sha256OfFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		sb.WriteString(rel + " " + h + "\n")
	}
	return sb.String(), nil
}
