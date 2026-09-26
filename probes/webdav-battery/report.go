// The report: one place where the per-protocol, per-concurrency numbers are put
// side by side and the release's own claim is judged.
//
// WHAT IT DECIDES. The battery exists to falsify one hypothesis: that the HTTP
// version is only a CARRIER and concurrent independent requests are the CAUSE of
// the measured win. So the report computes, for every protocol and every link,
// the concurrency=1 / concurrency=N ratio on the whole-tree operations — the
// criterion the PRD writes down as AC-11 ("the same battery at concurrency 1 vs
// N differs by >= 2x on the whole-tree operations ... a green result that
// survives concurrency=1 proves nothing was actually pipelined",
// docs/prd/PRD-bunker-fs.md:136).
//
// A protocol that does NOT degrade at concurrency=1 is reported as a FINDING
// with what it implies, and it is never scored as a battery failure: the number
// is the deliverable, and the only thing this report must not do is talk itself
// into the answer it was hoping for.
package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// armRow is one row of arms.csv, as the report reads it.
type armRow struct {
	Label       string
	Link        string
	Proto       string
	Expected    string
	ObsClient   string
	ObsServer   string
	Conns       int
	Conc        int
	FixtureFile int
	FixtureDirs int
	TotalWall   float64
	Cells       int
	CellsOK     int
	CellsFailed int
	Stalls      int
	Unreliable  int
	NotesFailed int
	WarmupOK    bool
	Contended   bool
	ExitOK      bool
	LoadBefore  string
	LoadAfter   string
	CPUs        int
}

// opRow is one row of ops.csv.
type opRow struct {
	Arm         string
	Link        string
	Proto       string
	Op          string
	Kind        string
	Stage       int
	Conns       int
	Conc        int
	Requests    int
	Verify      int
	Work        float64
	Wall        float64
	Statuses    string
	ClientProto string
	ServerProto string
	Bytes       int64
	OK          bool
	Unreliable  bool
	Class       string
	Detail      string
	ProtoNote   string
}

// degradationTargets are the operations AC-11 grades, in its own words: "the same
// battery at concurrency 1 vs N differs by >= 2x on the whole-tree operations".
// walk-tree is the whole-tree walk the mount performs (one listing per directory
// plus one stat per file); fanout-100 is the PRD's own A/B shape (100 independent
// requests on one connection).
var degradationTargets = []string{"walk-tree", "fanout-100"}

// contextOnly are printed beside the graded rows and deliberately excluded from
// the verdict, each for a reason that is itself a fact about the surface:
//
//   - git-status-shaped is ONE request by construction (that is the point of
//     delegation — PRD-bunker-fs.md:23-31), so it has no concurrency to lose and
//     a ratio of ~1 says nothing about pipelining;
//   - the battery total is reported because it is the figure a release decision
//     reads, but on the delayed link it is dominated by the multi-MiB read/write
//     cells, whose cost there is the relay's per-chunk rate limit rather than any
//     property of the protocol.
var contextOnly = []string{"git-status-shaped", "battery total wall"}

func runReportMode() (int, error) {
	dir := *inFlag
	arms, err := readArms(filepath.Join(dir, "arms.csv"))
	if err != nil {
		return 2, err
	}
	if len(arms) == 0 {
		return 2, fmt.Errorf("no arms in %s", filepath.Join(dir, "arms.csv"))
	}
	ops, err := readOps(filepath.Join(dir, "ops.csv"))
	if err != nil {
		return 2, err
	}

	sortArms(arms)
	printArmTable(arms)
	printObservedCheck(arms)
	printOpMatrices(arms, ops)
	printDegradation(arms, ops)
	failing := printFailures(arms, ops)

	if failing > 0 {
		fmt.Printf("\nreport: %d arm(s) did not pass their own cells or contract notes — see above\n", failing)
		return 1, nil
	}
	fmt.Printf("\nreport: every arm passed its own cells and every recorded protocol matches the arm's claim\n")
	return 0, nil
}

// ── input ────────────────────────────────────────────────────────────────────

func readCSV(path string) ([]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("%s has no data rows", path)
	}
	header := rows[0]
	var out []map[string]string
	for _, r := range rows[1:] {
		m := map[string]string{}
		for i, h := range header {
			if i < len(r) {
				m[h] = r[i]
			}
		}
		out = append(out, m)
	}
	return out, nil
}

func readArms(path string) ([]*armRow, error) {
	raw, err := readCSV(path)
	if err != nil {
		return nil, err
	}
	var out []*armRow
	for _, m := range raw {
		out = append(out, &armRow{
			Label:       m["arm"],
			Link:        m["link"],
			Proto:       m["proto"],
			Expected:    m["expected_proto"],
			ObsClient:   m["observed_client_proto"],
			ObsServer:   m["observed_server_proto"],
			Conns:       atoi(m["conns"]),
			Conc:        atoi(m["conc"]),
			FixtureFile: atoi(m["fixture_files"]),
			FixtureDirs: atoi(m["fixture_dirs"]),
			TotalWall:   atof(m["total_wall_ms"]),
			Cells:       atoi(m["cells"]),
			CellsOK:     atoi(m["cells_ok"]),
			CellsFailed: atoi(m["cells_failed"]),
			Stalls:      atoi(m["stalls"]),
			Unreliable:  atoi(m["unreliable"]),
			NotesFailed: atoi(m["notes_failed"]),
			WarmupOK:    m["warmup_ok"] == "true",
			Contended:   m["contended"] == "true",
			ExitOK:      m["exit_ok"] == "true",
			LoadBefore:  m["loadavg_before"],
			LoadAfter:   m["loadavg_after"],
			CPUs:        atoi(m["cpus"]),
		})
	}
	return out, nil
}

func readOps(path string) ([]*opRow, error) {
	raw, err := readCSV(path)
	if err != nil {
		return nil, err
	}
	var out []*opRow
	for _, m := range raw {
		out = append(out, &opRow{
			Arm:         m["arm"],
			Link:        m["link"],
			Proto:       m["proto"],
			Op:          m["op"],
			Kind:        m["kind"],
			Stage:       atoi(m["stage"]),
			Conns:       atoi(m["conns"]),
			Conc:        atoi(m["conc"]),
			Requests:    atoi(m["requests"]),
			Verify:      atoi(m["verify_requests"]),
			Work:        atof(m["work_ms"]),
			Wall:        atof(m["wall_ms"]),
			Statuses:    m["statuses"],
			ClientProto: m["client_proto"],
			ServerProto: m["server_proto"],
			Bytes:       atoi64(m["bytes"]),
			OK:          m["ok"] == "true",
			Unreliable:  m["unreliable"] == "true",
			Class:       m["class"],
			Detail:      m["detail"],
			ProtoNote:   m["proto_note"],
		})
	}
	return out, nil
}

func atoi(s string) int      { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }
func atoi64(s string) int64  { n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64); return n }
func atof(s string) float64  { f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64); return f }
func protoRank(p string) int { return map[string]int{"h1": 0, "h2": 1, "h3": 2}[p] }

// sortArms orders arms the way the tables read: link, protocol, connections,
// then concurrency.
func sortArms(arms []*armRow) {
	sort.SliceStable(arms, func(i, j int) bool {
		if arms[i].Link != arms[j].Link {
			return arms[i].Link < arms[j].Link
		}
		if protoRank(arms[i].Proto) != protoRank(arms[j].Proto) {
			return protoRank(arms[i].Proto) < protoRank(arms[j].Proto)
		}
		if arms[i].Conns != arms[j].Conns {
			return arms[i].Conns < arms[j].Conns
		}
		return arms[i].Conc < arms[j].Conc
	})
}

// ── tables ───────────────────────────────────────────────────────────────────

func printTable(headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	var hdr, sep []string
	for i, h := range headers {
		hdr = append(hdr, pad(h, widths[i]))
		sep = append(sep, strings.Repeat("-", widths[i]))
	}
	fmt.Printf("  %s\n", strings.Join(hdr, "  "))
	fmt.Printf("  %s\n", strings.Join(sep, "  "))
	for _, r := range rows {
		var cells []string
		for i, c := range r {
			if i < len(widths) {
				cells = append(cells, pad(c, widths[i]))
				continue
			}
			cells = append(cells, c)
		}
		fmt.Printf("  %s\n", strings.Join(cells, "  "))
	}
}

func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func printArmTable(arms []*armRow) {
	fmt.Printf("\n== arms ==\n")
	rows := make([][]string, 0, len(arms))
	for _, a := range arms {
		rows = append(rows, []string{
			a.Label, a.Proto, a.Link, strconv.Itoa(a.Conns), strconv.Itoa(a.Conc),
			fmt.Sprintf("%.3f", a.TotalWall/1000), strconv.Itoa(a.CellsOK) + "/" + strconv.Itoa(a.Cells),
			strconv.Itoa(a.Stalls), strconv.Itoa(a.Unreliable), strconv.Itoa(a.NotesFailed),
			strconv.FormatBool(a.Contended), map[bool]string{true: "PASS", false: "FAIL"}[a.ExitOK],
		})
	}
	printTable([]string{"arm", "proto", "link", "conn", "conc", "wall_s", "cells_ok", "stalls", "unreliable", "notes_fail", "contended", "arm"}, rows)
}

// printObservedCheck is the anti-gaming assertion, stated for the whole run:
// every arm's observed protocols must equal the protocol it claims. An arm that
// claims h3 while every response says HTTP/1.1 would otherwise contribute a
// number to a table it does not belong in.
func printObservedCheck(arms []*armRow) {
	fmt.Printf("\n== protocol observed vs claimed (read from resp.Proto and X-Bunker-Proto) ==\n")
	rows := make([][]string, 0, len(arms))
	for _, a := range arms {
		verdict := "MATCH"
		if !containsProto(a.ObsClient, a.Expected) || !containsProto(a.ObsServer, a.Expected) {
			verdict = "MISMATCH"
		}
		if a.Unreliable > 0 {
			verdict = "UNRELIABLE CELLS"
		}
		rows = append(rows, []string{a.Label, a.Expected, a.ObsClient, a.ObsServer, verdict})
	}
	printTable([]string{"arm", "claimed", "client saw", "server saw", "verdict"}, rows)
}

func containsProto(list, want string) bool {
	for _, p := range strings.Split(list, "|") {
		if p == want {
			return true
		}
	}
	return false
}

// printOpMatrices puts the 14 operations side by side, once per link: work_ms
// (what the op's own requests cost) and wall_ms (what it cost including waiting
// for the arm's in-flight bound). The two differ exactly where concurrency
// matters, which is why both are printed.
func printOpMatrices(arms []*armRow, ops []*opRow) {
	links := distinctLinks(arms)
	order := opOrder(ops)
	for _, link := range links {
		linkArms := armsForLink(arms, link)
		labels := make([]string, 0, len(linkArms))
		for _, a := range linkArms {
			labels = append(labels, fmt.Sprintf("%s x%d c%d", a.Proto, a.Conns, a.Conc))
		}
		for _, metric := range []string{"work_ms", "wall_ms"} {
			fmt.Printf("\n== %s — %s per operation (link=%s) ==\n", metric, metric, link)
			headers := append([]string{"op"}, labels...)
			var rows [][]string
			for _, op := range order {
				row := []string{op}
				for _, a := range linkArms {
					row = append(row, fmt.Sprintf("%.3f", opMetric(ops, a, op, metric)))
				}
				rows = append(rows, row)
			}
			rows = append(rows, totalRow(linkArms, ops, metric))
			printTable(headers, rows)
		}
	}
}

func totalRow(linkArms []*armRow, ops []*opRow, metric string) []string {
	row := []string{"TOTAL(battery wall)"}
	for _, a := range linkArms {
		if metric == "work_ms" {
			var sum float64
			for _, o := range ops {
				if o.Arm == a.Label {
					sum += o.Work
				}
			}
			row = append(row, fmt.Sprintf("%.3f", sum))
			continue
		}
		row = append(row, fmt.Sprintf("%.3f", a.TotalWall))
	}
	return row
}

func opMetric(ops []*opRow, arm *armRow, op, metric string) float64 {
	for _, o := range ops {
		if o.Arm != arm.Label || o.Op != op {
			continue
		}
		if metric == "work_ms" {
			return o.Work
		}
		return o.Wall
	}
	return 0
}

func distinctLinks(arms []*armRow) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range arms {
		if !seen[a.Link] {
			seen[a.Link] = true
			out = append(out, a.Link)
		}
	}
	sort.Strings(out)
	return out
}

func armsForLink(arms []*armRow, link string) []*armRow {
	var out []*armRow
	for _, a := range arms {
		if a.Link == link {
			out = append(out, a)
		}
	}
	return out
}

// opOrder is the canonical op order: by stage, then by first appearance.
func opOrder(ops []*opRow) []string {
	type entry struct {
		stage int
		first int
	}
	seen := map[string]entry{}
	for i, o := range ops {
		if _, ok := seen[o.Op]; !ok {
			seen[o.Op] = entry{stage: o.Stage, first: i}
			continue
		}
		e := seen[o.Op]
		if e.first > i {
			e.first = i
			seen[o.Op] = e
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if seen[names[i]].stage != seen[names[j]].stage {
			return seen[names[i]].stage < seen[names[j]].stage
		}
		return seen[names[i]].first < seen[names[j]].first
	})
	return names
}

// ── the degradation analysis ─────────────────────────────────────────────────

func printDegradation(arms []*armRow, ops []*opRow) {
	fmt.Printf("\n== the row's MUST: does concurrency=1 degrade? ==\n")
	fmt.Printf("   (ratio = concurrency-1 value / concurrency-N value on the SAME link and connection pool;\n")
	fmt.Printf("    >=2.0x is AC-11's threshold. GRADED measures are the whole-tree operations AC-11 names:\n")
	fmt.Printf("    walk-tree and fanout-100. Rows marked * are context: git-status-shaped is one request by\n")
	fmt.Printf("    construction, and the battery total is dominated on the delayed link by the multi-MiB\n")
	fmt.Printf("    cells, whose cost there is the delay relay's per-chunk rate limit, not the protocol.)\n")

	type verdict struct {
		link, proto string
		degraded    []string
		notDegraded []string
	}
	var verdicts []verdict

	for _, link := range distinctLinks(arms) {
		linkArms := armsForLink(arms, link)
		hiConc := 1
		for _, a := range linkArms {
			if a.Conc > hiConc {
				hiConc = a.Conc
			}
		}
		protos := map[string][]*armRow{}
		for _, a := range linkArms {
			protos[a.Proto] = append(protos[a.Proto], a)
		}
		protoNames := make([]string, 0, len(protos))
		for p := range protos {
			protoNames = append(protoNames, p)
		}
		sort.Slice(protoNames, func(i, j int) bool { return protoRank(protoNames[i]) < protoRank(protoNames[j]) })

		for _, p := range protoNames {
			base, hi := pickPair(protos[p])
			if base == nil || hi == nil {
				continue
			}
			fmt.Printf("\n   link=%s proto=%s   conc=1 arm %q vs conc=%d arm %q (conns %d)\n",
				link, p, base.Label, hi.Conc, hi.Label, base.Conns)
			rows := [][]string{
				{"battery total wall *", fmt.Sprintf("%.3f", base.TotalWall/1000), fmt.Sprintf("%.3f", hi.TotalWall/1000), ratioText(base.TotalWall, hi.TotalWall)},
			}
			for _, op := range degradationTargets {
				b, h := opMetric(ops, base, op, "wall_ms"), opMetric(ops, hi, op, "wall_ms")
				if b == 0 && h == 0 {
					continue
				}
				rows = append(rows, []string{op + " (wall)", fmt.Sprintf("%.3f", b), fmt.Sprintf("%.3f", h), ratioText(b, h)})
			}
			for _, op := range contextOnly {
				if op == "battery total wall" {
					continue
				}
				b, h := opMetric(ops, base, op, "wall_ms"), opMetric(ops, hi, op, "wall_ms")
				if b == 0 && h == 0 {
					continue
				}
				rows = append(rows, []string{op + " (wall) *", fmt.Sprintf("%.3f", b), fmt.Sprintf("%.3f", h), ratioText(b, h)})
			}
			// The normalised shape of the fan-out arm: milliseconds per request,
			// which is the figure the study's own A/B published (7.88 ms/request
			// over HTTP/2, 384.67 ms over HTTP/1.1 — PRD-bunker-fs.md:60-61) and
			// which is comparable across links in a way wall time is not.
			if n := 100; n > 0 {
				rows = append(rows, []string{"fanout-100 mean per request",
					fmt.Sprintf("%.3f", opMetric(ops, base, "fanout-100", "wall_ms")/float64(n)),
					fmt.Sprintf("%.3f", opMetric(ops, hi, "fanout-100", "wall_ms")/float64(n)),
					"-"})
			}
			printTable([]string{"measure", "conc=1", fmt.Sprintf("conc=%d", hi.Conc), "ratio"}, rows)

			// A link with no round trip has nothing for concurrency to recover:
			// the study's own lever is measured in fractions of one 185 ms RTT,
			// so a sub-millisecond per-request cost means the ratio measured
			// here is a floor for that link, not a verdict about the protocol.
			mean := opMetric(ops, hi, "fanout-100", "wall_ms") / 100
			if mean > 0 && mean < 5 {
				fmt.Printf("     ^ link=%s: mean %.3f ms per independent request — this link has effectively no round trip,\n", link, mean)
				fmt.Printf("       so the RTT-borne part of the lever cannot appear here. The number is reported as measured;\n")
				fmt.Printf("       the link where the lever is visible is the delayed one.\n")
			}

			v := verdict{link: link, proto: p}
			for _, r := range rows {
				if strings.Contains(r[0], "*") || strings.HasPrefix(r[0], "fanout-100 mean") {
					continue
				}
				if classifyRatio(r[3]) {
					v.degraded = append(v.degraded, r[0])
				} else {
					v.notDegraded = append(v.notDegraded, r[0])
				}
			}
			verdicts = append(verdicts, v)
		}

		// The carrier test: with the SAME in-flight concurrency, changing only the
		// number of CONNECTIONS. On HTTP/1.1 a connection is the only way to be
		// concurrent, so if the 8-connection arm beats the 1-connection arm at the
		// same concurrency, then concurrency — not the HTTP version — is what the
		// wall time responds to. This is the arm that separates the two readings,
		// which is why it compares at the SAME concurrency rather than across it.
		if h1 := protos["h1"]; len(h1) >= 2 {
			one, many := pickByConns(h1, hiConc)
			if one != nil && many != nil && one.Conns != many.Conns {
				fmt.Printf("\n   CARRIER TEST (link=%s): HTTP/1.1 with %d connection(s) vs %d, same concurrency=%d\n",
					link, one.Conns, many.Conns, one.Conc)
				printTable([]string{"measure", fmt.Sprintf("conns=%d", one.Conns), fmt.Sprintf("conns=%d", many.Conns), "ratio"},
					[][]string{
						{"battery total wall *", fmt.Sprintf("%.3f", one.TotalWall/1000), fmt.Sprintf("%.3f", many.TotalWall/1000), ratioText(one.TotalWall, many.TotalWall)},
						{"walk-tree (wall)", fmt.Sprintf("%.3f", opMetric(ops, one, "walk-tree", "wall_ms")), fmt.Sprintf("%.3f", opMetric(ops, many, "walk-tree", "wall_ms")), ratioText(opMetric(ops, one, "walk-tree", "wall_ms"), opMetric(ops, many, "walk-tree", "wall_ms"))},
					})
			}
		}
	}

	fmt.Printf("\n== verdict ==\n")
	for _, v := range verdicts {
		switch {
		case len(v.notDegraded) == 0:
			fmt.Printf("   %-12s %s: DEGRADED at concurrency=1 on every whole-tree measure (%s)\n",
				v.link, v.proto, strings.Join(v.degraded, ", "))
		case len(v.degraded) == 0:
			fmt.Printf("   %-12s %s: NOT DEGRADED at concurrency=1 on any whole-tree measure (%s) — FINDING\n",
				v.link, v.proto, strings.Join(v.notDegraded, ", "))
		default:
			fmt.Printf("   %-12s %s: PARTIAL — degraded on %s; not degraded on %s — FINDING\n",
				v.link, v.proto, strings.Join(v.degraded, ", "), strings.Join(v.notDegraded, ", "))
		}
	}
	fmt.Printf("\n   What each outcome means, stated before the numbers are read:\n")
	fmt.Printf("   - DEGRADED on a multiplexed protocol: concurrency is doing the work on this link, which is\n")
	fmt.Printf("     what the release's central premise says. The protocol is the carrier that makes it possible.\n")
	fmt.Printf("   - NOT DEGRADED on a multiplexed protocol: the concurrency=1 arm was as fast as the concurrent\n")
	fmt.Printf("     arm, so something OTHER than concurrency explains the win — the release's premise needs\n")
	fmt.Printf("     rewriting before the number is published. This is a MAJOR finding, not a broken battery.\n")
	fmt.Printf("   - A protocol with no concurrency gain at all (an HTTP/1.1 arm with one connection) is not a\n")
	fmt.Printf("     finding: a serialized transport has no pipelining to lose. Its counterpart arm — HTTP/1.1\n")
	fmt.Printf("     with more connections — is what separates \"the version\" from \"concurrency\".\n")
	fmt.Printf("   - On a link with no round trip the lever has almost nothing to recover, so a ~1x ratio there\n")
	fmt.Printf("     is a property of the LINK, not evidence about the protocol; the report prints the\n")
	fmt.Printf("     milliseconds-per-request behind that statement instead of leaving it to the reader.\n")
}

// pickPair returns the concurrency-1 arm and the highest-concurrency arm with the
// same connection bound.
func pickPair(arms []*armRow) (base, hi *armRow) {
	for _, a := range arms {
		if a.Conns != 1 {
			continue
		}
		if a.Conc == 1 {
			base = a
			continue
		}
		if hi == nil || a.Conc > hi.Conc {
			hi = a
		}
	}
	return base, hi
}

// pickByConns returns the one-connection and many-connection arms AT the given
// concurrency, so the only variable between them is the connection count.
func pickByConns(arms []*armRow, conc int) (one, many *armRow) {
	for _, a := range arms {
		if a.Conc != conc {
			continue
		}
		if one == nil || a.Conns < one.Conns {
			one = a
		}
		if many == nil || a.Conns > many.Conns {
			many = a
		}
	}
	return one, many
}

// ratioText renders a ratio and whether it clears AC-11's threshold. Four
// decimals, not three: at three, a ratio of 1.9999 prints as "2.000x
// not-degraded", which reads as a contradiction to anyone scanning the table.
func ratioText(a, b float64) string {
	if b <= 0 {
		return "n/a"
	}
	r := a / b
	return fmt.Sprintf("%.4fx%s", r, map[bool]string{true: " DEGRADED", false: " not-degraded"}[r >= 2.0])
}

// classifyRatio re-reads the flag out of a rendered ratio cell.
func classifyRatio(cell string) bool { return strings.Contains(cell, "DEGRADED") }

// ── failures ─────────────────────────────────────────────────────────────────

// printFailures lists every cell that failed, every cell whose protocol
// observation was unreliable, and every cross-concurrency asymmetry (an op that
// passed at one concurrency and did not at the other) — the row's rule that no
// pass is inferred from the absence of an error.
func printFailures(arms []*armRow, ops []*opRow) int {
	bad := 0
	fmt.Printf("\n== failures, stalls and asymmetries ==\n")
	for _, a := range arms {
		if !a.ExitOK {
			bad++
			fmt.Printf("   ARM FAILED: %s (cells_ok=%d/%d stalls=%d unreliable=%d notes_failed=%d warmup_ok=%v contended=%v)\n",
				a.Label, a.CellsOK, a.Cells, a.Stalls, a.Unreliable, a.NotesFailed, a.WarmupOK, a.Contended)
		}
		if a.Contended {
			fmt.Printf("   CONTENDED: %s ran at loadavg %s -> %s on %d cpus — the number is labelled, not corrected\n",
				a.Label, a.LoadBefore, a.LoadAfter, a.CPUs)
		}
	}
	for _, o := range ops {
		if o.Class == "stall" {
			fmt.Printf("   STALL: arm=%s op=%s concurrency=%d wall=%.3fms work=%.3fms statuses=%s detail=%s\n",
				o.Arm, o.Op, o.Conc, o.Wall, o.Work, o.Statuses, o.Detail)
			bad++
			continue
		}
		if !o.OK {
			fmt.Printf("   FAIL:  arm=%s op=%s class=%s statuses=%s detail=%s\n", o.Arm, o.Op, o.Class, o.Statuses, o.Detail)
			bad++
		}
		if o.Unreliable {
			fmt.Printf("   UNRELIABLE PROTOCOL: arm=%s op=%s %s\n", o.Arm, o.Op, o.ProtoNote)
			bad++
		}
		if o.Requests == 0 && o.Kind == requiredCell {
			fmt.Printf("   ZERO-REQUEST CELL: arm=%s op=%s\n", o.Arm, o.Op)
			bad++
		}
	}

	// Cross-concurrency asymmetry: the same op passing in one arm of a
	// (link, proto, conns, conc) family and failing in another.
	type key struct {
		link, proto, op string
		conns, conc     int
	}
	status := map[key]bool{}
	for _, o := range ops {
		status[key{o.Link, o.Proto, o.Op, o.Conns, o.Conc}] = o.OK && !o.Unreliable
	}
	type family struct {
		link, proto, op string
	}
	families := map[family]map[int]bool{}
	for k, ok := range status {
		f := family{k.link, k.proto, k.op}
		if families[f] == nil {
			families[f] = map[int]bool{}
		}
		if _, seen := families[f][k.conc]; !seen {
			families[f][k.conc] = ok
			continue
		}
		families[f][k.conc] = families[f][k.conc] || ok
	}
	var fams []family
	for f := range families {
		fams = append(fams, f)
	}
	sort.Slice(fams, func(i, j int) bool {
		if fams[i].link != fams[j].link {
			return fams[i].link < fams[j].link
		}
		if fams[i].proto != fams[j].proto {
			return fams[i].proto < fams[j].proto
		}
		return fams[i].op < fams[j].op
	})
	asymmetries := 0
	for _, f := range fams {
		if len(families[f]) < 2 {
			continue
		}
		anyPass, anyFail := false, false
		for _, ok := range families[f] {
			if ok {
				anyPass = true
			} else {
				anyFail = true
			}
		}
		if anyPass && anyFail {
			asymmetries++
			fmt.Printf("   ASYMMETRY: link=%s proto=%s op=%s passes at one concurrency and fails at another\n", f.link, f.proto, f.op)
		}
	}
	if bad == 0 && asymmetries == 0 {
		fmt.Printf("   none: no failing cell, no stall, no unreliable protocol observation, no asymmetry\n")
	}
	return bad
}

// LoadBefore/LoadAfter/CPUs are read straight off arms.csv so a contended arm is
// labelled where the numbers are printed.
