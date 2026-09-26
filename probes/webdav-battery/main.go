// Command webdav-battery is the per-protocol measurement instrument for
// BFS-011: the 14-operation filesystem battery on ONE fixture, run over
// HTTP/1.1, HTTP/2 and HTTP/3, at BOTH the arm's in-flight concurrency and at
// concurrency 1.
//
// THE CLAIM IT EXISTS TO FALSIFY. The release rests on one proven lever —
// CONCURRENT REQUESTS (25.0x faster over HTTP/2 at MaxConnsPerHost=1,
// docs/prd/PRD-bunker-fs.md:60-61). The hypothesis is that the protocol VERSION
// is only a carrier and concurrency is the cause. So every protocol gets a run
// at concurrency=1 and a run at concurrency>1, and the concurrency=1 arm MUST
// degrade: if it does not, some other mechanism explains the win and the
// release's premise needs rewriting. That outcome is a finding, not a bug in the
// battery, and the report says which of the two it measured.
//
// PROTOCOL IS OBSERVED, NEVER ASSUMED. Every request records `resp.Proto` (what
// the client's stack received) and `X-Bunker-Proto` (what the SERVER observed
// via r.Proto), plus the ALPN the handshake negotiated. A cell whose observations
// disagree with the arm's protocol is marked UNRELIABLE and exits non-zero; no
// cell reports the version it was merely configured for.
//
// A STALL IS REPORTED AS A STALL — with the protocol, the op, the concurrency
// and the wall time — and never smoothed into a pass. A zero-duration or
// empty-result cell is a failure, not a success with no error: an empty listing
// and an empty deque are the shapes a stall hides behind.
//
// MODES
//
//	-mode battery   run the battery once against -url (the arm)
//	-mode fixture   generate Fixture A under -root
//	-mode relay     run the delay relay (-relay-listen -> -relay-target)
//	-mode report    build the per-protocol tables from a results dir
//
// USAGE (the driver wraps all of this: probes/webdav-battery.sh)
//
//	go build -o /tmp/webdav-battery ./probes/webdav-battery
//	/tmp/webdav-battery -mode battery -url https://127.0.0.1:18443 -proto h2 \
//	  -conns 1 -concurrency 8 -root /tmp/fixture -out /tmp/results -link delay20ms
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	modeFlag      = flag.String("mode", "battery", "battery | fixture | relay | report")
	urlFlag       = flag.String("url", "", "surface base URL, e.g. https://127.0.0.1:18443 (the /dav prefix is added by the battery)")
	protoFlag     = flag.String("proto", "", "h1 | h2 | h3")
	connsFlag     = flag.Int("conns", 1, "client connection-pool bound (MaxConnsPerHost)")
	concFlag      = flag.Int("concurrency", 1, "in-flight request bound for every arm")
	opTimeoutFlag = flag.Duration("op-timeout", 45*time.Second, "per-cell deadline; a cell that exceeds it is reported as a STALL")
	labelFlag     = flag.String("label", "", "arm label (default: <proto>-<link>-conns<N>-conc<M>)")
	linkFlag      = flag.String("link", "loopback", "link label for the report (loopback | delay20ms | ...)")
	rootFlag      = flag.String("root", "", "local path of the served fixture tree")
	userFlag      = flag.String("user", "", "Basic-auth username")
	tokenFlag     = flag.String("token", "", "Basic-auth token (never echoed)")
	outFlag       = flag.String("out", ".", "results directory (arms.csv, ops.csv, requests.csv)")
	fanoutFlag    = flag.Int("fanout", 100, "requests in the fan-out arm")
	dirsFlag      = flag.Int("dirs", 6, "fixture: top-level collections (Fixture A: 6)")
	perDirFlag    = flag.Int("per-dir", 25, "fixture: files per collection (Fixture A: 25)")
	bigMiBFlag    = flag.Int("big-mib", 4, "fixture: size of big.bin in MiB (the study's write granularity is a 4 MiB file)")
	relayListen   = flag.String("relay-listen", "", "relay: local address to listen on (TCP and UDP, same port)")
	relayTarget   = flag.String("relay-target", "", "relay: address to forward to")
	relayDelay    = flag.Duration("relay-delay", 20*time.Millisecond, "relay: delay added per forwarded chunk, per direction")
	inFlag        = flag.String("in", ".", "report: results directory to read")
)

func main() {
	flag.Parse()
	code, err := dispatch()
	if err != nil {
		fmt.Fprintf(os.Stderr, "webdav-battery: %v\n", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func dispatch() (int, error) {
	switch strings.ToLower(strings.TrimSpace(*modeFlag)) {
	case "battery":
		return runBatteryMode()
	case "fixture":
		return runFixtureMode()
	case "relay":
		return runRelayMode()
	case "report":
		return runReportMode()
	default:
		return 2, fmt.Errorf("unknown -mode %q", *modeFlag)
	}
}

// ── fixture mode ─────────────────────────────────────────────────────────────

func runFixtureMode() (int, error) {
	if *rootFlag == "" {
		return 2, errors.New("-root is required")
	}
	big := int64(*bigMiBFlag) << 20
	info, err := generateFixture(*rootFlag, *dirsFlag, *perDirFlag, big)
	if err != nil {
		return 2, err
	}
	fmt.Printf("fixture %s: %d dirs, %d files, big.bin=%d bytes (deterministic content)\n",
		info.Root, len(info.Dirs), len(info.Files), info.BigBytes)
	return 0, nil
}

// ── relay mode ───────────────────────────────────────────────────────────────

func runRelayMode() (int, error) {
	if *relayListen == "" || *relayTarget == "" {
		return 2, errors.New("-relay-listen and -relay-target are required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := func(tcp, udp string) {
		// One line the driver can wait for, naming both transports of the ONE
		// port number the relay (and the daemon) serve.
		fmt.Printf("relay ready: tcp=%s udp=%s -> %s delay=%s\n", tcp, udp, *relayTarget, *relayDelay)
	}
	err := runRelay(ctx, *relayListen, *relayTarget, *relayDelay, ready)
	if err != nil {
		return 2, err
	}
	return 0, nil
}

// ── the arm runner ───────────────────────────────────────────────────────────

// cellResult is one row of the battery's output: what the cell did, what it
// cost, and what both sides of the wire said about the protocol.
type cellResult struct {
	name     string
	kind     string
	stage    int
	requests int
	verify   int
	work     time.Duration
	wall     time.Duration
	statuses []int
	bytes    int64
	ok       bool
	class    string
	detail   string

	clientProtos []string
	serverProtos []string
	unreliable   bool
	protoNote    string
}

// armResult is one full battery run.
type armResult struct {
	label      string
	link       string
	proto      protoKind
	conns      int
	conc       int
	fixture    *fixtureInfo
	totalWall  time.Duration
	stageWalls []stageWall
	cells      []*cellResult
	notes      []note
	warmUp     string
	warmUpOK   bool

	loadBefore string
	loadAfter  string
	cpus       int
	contended  bool
	exitOK     bool

	// observed is the union of the protocols both sides reported across the
	// whole arm — the arm-level answer to "which protocol was actually used".
	observedClient []string
	observedServer []string
}

type stageWall struct {
	stage int
	wall  time.Duration
}

func runBatteryMode() (int, error) {
	if *urlFlag == "" {
		return 2, errors.New("-url is required")
	}
	if *rootFlag == "" {
		return 2, errors.New("-root is required (the local path of the served tree)")
	}
	kind, err := parseProto(*protoFlag)
	if err != nil {
		return 2, err
	}
	fx, err := scanFixture(*rootFlag)
	if err != nil {
		return 2, err
	}
	label := *labelFlag
	if label == "" {
		label = fmt.Sprintf("%s-%s-conns%d-conc%d", kind, *linkFlag, *connsFlag, *concFlag)
	}
	arm, err := runArm(context.Background(), armConfig{
		kind: kind,
		// The client's base is the ORIGIN only: the surface paths the cells
		// build already carry the /dav prefix, and a base that carried it too
		// would request /dav/dav/<path> — which is how this battery first
		// produced a table of 404s against a working server.
		base:     strings.TrimSuffix(*urlFlag, "/"),
		root:     fx.Root,
		fixture:  fx,
		conns:    *connsFlag,
		conc:     *concFlag,
		fanout:   *fanoutFlag,
		opLimit:  *opTimeoutFlag,
		user:     *userFlag,
		token:    *tokenFlag,
		label:    label,
		link:     *linkFlag,
		timeout:  *opTimeoutFlag + 30*time.Second,
		outDir:   *outFlag,
		writeRaw: true,
	})
	if err != nil {
		return 2, err
	}
	if !arm.exitOK {
		return 1, nil
	}
	return 0, nil
}

// armConfig is everything one arm needs.
type armConfig struct {
	kind    protoKind
	base    string
	root    string
	fixture *fixtureInfo
	conns   int
	conc    int
	fanout  int
	opLimit time.Duration
	timeout time.Duration
	user    string
	token   string
	label   string
	link    string
	outDir  string
	// writeRaw writes the per-request CSV; the report mode reads the op-level
	// rows, and the request-level rows are the raw record behind them.
	writeRaw bool
}

// runArm runs the whole battery once and returns everything it measured.
func runArm(ctx context.Context, cfg armConfig) (*armResult, error) {
	cl, err := newClient(cfg.kind, cfg.base, cfg.conns, cfg.conc, cfg.user, cfg.token, cfg.timeout)
	if err != nil {
		return nil, err
	}
	defer cl.close()

	arm := &armResult{
		label:      cfg.label,
		link:       cfg.link,
		proto:      cfg.kind,
		conns:      cfg.conns,
		conc:       cfg.conc,
		fixture:    cfg.fixture,
		loadBefore: loadAvg(),
		cpus:       hostCPUCount(),
	}
	b := &battery{c: cl, fx: cfg.fixture, root: cfg.root, base: "/dav", fanout: cfg.fanout, opLimit: cfg.opLimit}

	arm.warmUp, arm.warmUpOK = b.warmUp(ctx)

	rawSink, err := openSink(filepath.Join(cfg.outDir, "requests.csv"), requestHeader())
	if err != nil {
		return nil, err
	}
	if !cfg.writeRaw {
		rawSink = nil
	}

	cells := b.cells()
	armsets := map[int][]cellSpec{}
	var stages []int
	for _, c := range cells {
		if _, seen := armsets[c.stage]; !seen {
			stages = append(stages, c.stage)
		}
		armsets[c.stage] = append(armsets[c.stage], c)
	}
	sort.Ints(stages)

	for _, stage := range stages {
		start := time.Now()
		specs := armsets[stage]
		results := make([]*cellResult, len(specs))
		var wg sync.WaitGroup
		for i, spec := range specs {
			wg.Add(1)
			go func(i int, spec cellSpec) {
				defer wg.Done()
				results[i] = runCell(ctx, b, spec, cl, cfg, rawSink)
			}(i, spec)
		}
		wg.Wait()
		arm.stageWalls = append(arm.stageWalls, stageWall{stage: stage, wall: time.Since(start)})
		arm.totalWall += time.Since(start)
		arm.cells = append(arm.cells, results...)
	}

	arm.notes = b.notes(ctx)
	arm.loadAfter = loadAvg()
	if rawSink != nil {
		_ = rawSink.close()
	}
	arm.contended = isContended(arm.loadBefore, arm.loadAfter, arm.cpus)

	// Arm-level protocol observation: the union of what both sides reported.
	arm.observedClient = unionProtos(arm.cells, func(c *cellResult) []string { return c.clientProtos })
	arm.observedServer = unionProtos(arm.cells, func(c *cellResult) []string { return c.serverProtos })

	arm.exitOK = true
	for _, c := range arm.cells {
		if !c.ok || c.unreliable {
			arm.exitOK = false
		}
	}
	for _, n := range arm.notes {
		if !n.ok {
			arm.exitOK = false
		}
	}
	if !arm.warmUpOK {
		arm.exitOK = false
	}

	if err := writeArmRows(cfg.outDir, arm); err != nil {
		return nil, err
	}
	printArm(arm)
	return arm, nil
}

// runCell times and classifies one cell. The cell's own deadline covers its
// requests, so a stall is a reported number rather than an open-ended wait.
func runCell(ctx context.Context, b *battery, spec cellSpec, cl *client, cfg armConfig, raw *csvSink) *cellResult {
	cellCtx, cancel := context.WithTimeout(ctx, cfg.opLimit)
	defer cancel()

	res := &cellResult{name: spec.name, kind: spec.kind, stage: spec.stage}
	start := time.Now()
	o := spec.run(cellCtx, b)
	res.wall = time.Since(start)

	if o == nil {
		o = &outcome{}
		o.fail("cell returned no outcome")
	}
	timed, verify, ok, detail := o.snapshot()
	for _, c := range timed {
		res.work += c.Dur
		res.statuses = append(res.statuses, c.Status)
		res.bytes += c.Bytes
	}
	res.requests = len(timed)
	res.verify = len(verify)
	res.ok = ok
	res.detail = detail

	// A cell that timed out is a stall, named as one.
	stalled := false
	errored := false
	for _, c := range timed {
		switch c.class() {
		case "stall":
			stalled = true
		case "error":
			errored = true
		}
	}
	switch {
	case stalled:
		res.class = "stall"
		res.ok = false
	case errored:
		res.class = "error"
		res.ok = false
	case res.ok:
		res.class = "ok"
	default:
		res.class = "cell-failed"
	}
	if res.requests == 0 {
		res.ok = false
		res.class = "cell-failed"
		res.detail = appendDetail(res.detail, "the cell issued ZERO requests")
	}
	if res.ok && res.work <= 0 {
		res.ok = false
		res.class = "cell-failed"
		res.detail = appendDetail(res.detail, fmt.Sprintf("a passing cell reported a zero-duration work time (%s)", res.work))
	}
	if !res.ok {
		if d := describeFailing(timed); d != "" {
			res.detail = appendDetail(res.detail, d)
		}
	}

	// The anti-gaming check: both sides must have observed the arm's protocol.
	want := cfg.kind.wireName()
	for _, c := range timed {
		res.clientProtos = addProto(res.clientProtos, c.ClientProto)
		res.serverProtos = addProto(res.serverProtos, c.ServerProto)
		if c.ClientProto == "" || c.ServerProto == "" {
			res.unreliable = true
			res.protoNote = appendDetail(res.protoNote, fmt.Sprintf("%s %s: a side reported no protocol (client=%q server=%q)", c.Method, c.Path, c.ClientProto, c.ServerProto))
			continue
		}
		if !c.protoOK(want) {
			res.unreliable = true
			res.protoNote = appendDetail(res.protoNote, fmt.Sprintf("%s %s: observed client=%s server=%s, arm claims %s", c.Method, c.Path, c.ClientProto, c.ServerProto, want))
		}
	}
	if raw != nil {
		seq := map[string]int{}
		for _, c := range timed {
			seq[spec.name]++
			_ = raw.writeRow([]string{
				cfg.label, spec.name, strconv.Itoa(seq[spec.name]), c.Method, c.Path,
				strconv.Itoa(c.Status), c.ClientProto, c.ServerProto, c.ALPN, c.Verdict,
				ms(c.Dur), strconv.FormatInt(c.Bytes, 10), errText(c.Err),
			})
		}
	}
	return res
}

// ── sinks ────────────────────────────────────────────────────────────────────

// csvSink appends rows to a CSV, writing the header only when the file is new.
// Every arm of a run appends to the same three files, so the report can group
// them without a second pass over the filesystem.
type csvSink struct {
	f    *os.File
	w    *csv.Writer
	path string
}

func openSink(path string, header []string) (*csvSink, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	info, statErr := os.Stat(path)
	fresh := statErr != nil || info.Size() == 0
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	s := &csvSink{f: f, w: csv.NewWriter(f), path: path}
	if fresh {
		if err := s.w.Write(header); err != nil {
			return nil, err
		}
		s.w.Flush()
		return s, s.w.Error()
	}
	return s, nil
}

func (s *csvSink) writeRow(vals []string) error {
	if err := s.w.Write(vals); err != nil {
		return err
	}
	s.w.Flush()
	return s.w.Error()
}

func (s *csvSink) close() error {
	s.w.Flush()
	if err := s.w.Error(); err != nil {
		return err
	}
	return s.f.Close()
}

func requestHeader() []string {
	return []string{"arm", "op", "seq", "method", "path", "status", "client_proto", "server_proto", "alpn", "verdict", "dur_ms", "bytes", "err"}
}

func opsHeader() []string {
	return []string{"arm", "link", "proto", "conns", "conc", "stage", "op", "kind", "requests", "verify_requests",
		"work_ms", "wall_ms", "statuses", "client_proto", "server_proto", "bytes", "ok", "unreliable", "class", "detail", "proto_note"}
}

func armsHeader() []string {
	return []string{"arm", "link", "proto", "conns", "conc", "expected_proto", "observed_client_proto", "observed_server_proto",
		"fixture_dirs", "fixture_files", "total_wall_ms", "stages_ms", "cells", "cells_ok", "cells_failed", "stalls",
		"unreliable", "notes_failed", "warmup_ok", "loadavg_before", "loadavg_after", "cpus", "contended", "exit_ok", "fanout"}
}

// writeArmRows appends one arm row and one row per cell.
func writeArmRows(outDir string, arm *armResult) error {
	ops, err := openSink(filepath.Join(outDir, "ops.csv"), opsHeader())
	if err != nil {
		return err
	}
	defer func() { _ = ops.close() }()

	for _, c := range arm.cells {
		if err := ops.writeRow([]string{
			arm.label, arm.link, string(arm.proto), strconv.Itoa(arm.conns), strconv.Itoa(arm.conc),
			strconv.Itoa(c.stage), c.name, c.kind, strconv.Itoa(c.requests), strconv.Itoa(c.verify),
			ms(c.work), ms(c.wall), statusList(c.statuses), strings.Join(c.clientProtos, "|"), strings.Join(c.serverProtos, "|"),
			strconv.FormatInt(c.bytes, 10), strconv.FormatBool(c.ok), strconv.FormatBool(c.unreliable), c.class, c.detail, c.protoNote,
		}); err != nil {
			return err
		}
	}

	arms, err := openSink(filepath.Join(outDir, "arms.csv"), armsHeader())
	if err != nil {
		return err
	}
	defer func() { _ = arms.close() }()

	var stagesMS []string
	for _, s := range arm.stageWalls {
		stagesMS = append(stagesMS, fmt.Sprintf("stage%d:%.1f", s.stage, float64(s.wall.Microseconds())/1000))
	}
	cellsOK, failed, stalls, unreliable, notesFailed := 0, 0, 0, 0, 0
	for _, c := range arm.cells {
		if c.ok {
			cellsOK++
		} else {
			failed++
		}
		if c.class == "stall" {
			stalls++
		}
		if c.unreliable {
			unreliable++
		}
	}
	for _, n := range arm.notes {
		if !n.ok {
			notesFailed++
		}
	}
	return arms.writeRow([]string{
		arm.label, arm.link, string(arm.proto), strconv.Itoa(arm.conns), strconv.Itoa(arm.conc),
		arm.proto.wireName(), strings.Join(arm.observedClient, "|"), strings.Join(arm.observedServer, "|"),
		strconv.Itoa(len(arm.fixture.Dirs)), strconv.Itoa(len(arm.fixture.Files)),
		ms(arm.totalWall), strings.Join(stagesMS, " "), strconv.Itoa(len(arm.cells)), strconv.Itoa(cellsOK),
		strconv.Itoa(failed), strconv.Itoa(stalls), strconv.Itoa(unreliable), strconv.Itoa(notesFailed),
		strconv.FormatBool(arm.warmUpOK), arm.loadBefore, arm.loadAfter, strconv.Itoa(arm.cpus),
		strconv.FormatBool(arm.contended), strconv.FormatBool(arm.exitOK), strconv.Itoa(*fanoutFlag),
	})
}

// ── printing ─────────────────────────────────────────────────────────────────

func printArm(arm *armResult) {
	fmt.Printf("== arm %s ==\n", arm.label)
	fmt.Printf("   protocol expected by the arm : %s\n", arm.proto.wireName())
	fmt.Printf("   observed (client resp.Proto) : %s\n", joinOrNone(arm.observedClient))
	fmt.Printf("   observed (server X-Bunker-Proto): %s\n", joinOrNone(arm.observedServer))
	fmt.Printf("   link=%s conns=%d concurrency=%d fixture=%d dirs/%d files\n",
		arm.link, arm.conns, arm.conc, len(arm.fixture.Dirs), len(arm.fixture.Files))
	fmt.Printf("   loadavg %s -> %s (%d cpus) contended=%v\n", arm.loadBefore, arm.loadAfter, arm.cpus, arm.contended)
	fmt.Printf("   warm-up: %s\n", arm.warmUp)
	var stages []string
	for _, s := range arm.stageWalls {
		stages = append(stages, fmt.Sprintf("stage%d=%.3fs", s.stage, s.wall.Seconds()))
	}
	fmt.Printf("   battery wall time: %.3fs  (%s)\n", arm.totalWall.Seconds(), strings.Join(stages, " "))
	fmt.Printf("   %-18s %5s %5s %10s %10s  %-16s %-22s %s\n", "op", "reqs", "vfy", "work_ms", "wall_ms", "statuses", "client/server", "verdict")
	for _, c := range arm.cells {
		verdict := c.class
		if c.unreliable {
			verdict += " UNRELIABLE"
		}
		fmt.Printf("   %-18s %5d %5d %10.3f %10.3f  %-16s %-22s %s\n",
			c.name, c.requests, c.verify, msFloat(c.work), msFloat(c.wall), statusList(c.statuses),
			fmt.Sprintf("%s/%s", strings.Join(c.clientProtos, "|"), strings.Join(c.serverProtos, "|")), verdict)
		if !c.ok || c.unreliable {
			if c.detail != "" {
				fmt.Printf("   %-18s   detail: %s\n", "", c.detail)
			}
			if c.protoNote != "" {
				fmt.Printf("   %-18s   protocol: %s\n", "", c.protoNote)
			}
		}
	}
	fmt.Printf("   notes (untimed contract assertions):\n")
	for _, n := range arm.notes {
		state := "ok  "
		if !n.ok {
			state = "FAIL"
		}
		fmt.Printf("     %s %s\n", state, n.name)
		fmt.Printf("          %s\n", n.detail)
	}
	fmt.Printf("   ARM RESULT: %s\n\n", map[bool]string{true: "all cells ok", false: "FAILED — see the rows above"}[arm.exitOK])
}

// ── helpers ──────────────────────────────────────────────────────────────────

func ms(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 3, 64)
}

func msFloat(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func statusList(st []int) string {
	if len(st) == 0 {
		return "-"
	}
	counts := map[int]int{}
	var order []int
	for _, s := range st {
		if counts[s] == 0 {
			order = append(order, s)
		}
		counts[s]++
	}
	var parts []string
	for _, s := range order {
		if counts[s] == 1 {
			parts = append(parts, strconv.Itoa(s))
			continue
		}
		parts = append(parts, fmt.Sprintf("%dx%d", counts[s], s))
	}
	return strings.Join(parts, ",")
}

func addProto(list []string, p string) []string {
	for _, x := range list {
		if x == p {
			return list
		}
	}
	return append(list, p)
}

func unionProtos(cells []*cellResult, get func(*cellResult) []string) []string {
	var out []string
	for _, c := range cells {
		for _, p := range get(c) {
			out = addProto(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func joinOrNone(list []string) string {
	if len(list) == 0 {
		return "<none>"
	}
	return strings.Join(list, "|")
}

func appendDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// isContended reports whether the load average was at or above the core count
// during the arm — the flag that says a number was taken on a busy host. The
// row that filed this battery recorded that contention had already corrupted one
// set of numbers in this study, so a contended arm is labelled rather than
// quietly published.
func isContended(before, after string, cpus int) bool {
	if cpus <= 0 {
		return false
	}
	for _, v := range []string{before, after} {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		if f >= float64(cpus) {
			return true
		}
	}
	return false
}
