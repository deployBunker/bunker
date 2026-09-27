package fsclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-048, the half that makes the fix a REPORT and not only a comment.
//
// The measured defect: on a git tree the served revision token is the resolved
// HEAD, so an out-of-band working-tree edit moves NOTHING the client's
// last-resort revision poll can see (the webdav arms measure the token side of
// that fact). Correcting the comment alone leaves the mount still unable to say
// what it is and is not vouching for, so this file pins the honest behaviour:
// while the revision poll is the mechanism IN FORCE, the client reports which
// kind it is polling, the class of change that token moves for, and the class of
// change it cannot see at all — derived from the capability document's own
// declaration and from nothing else (SPEC-watcher-capability §7.1, §7.2 R-V4,
// §8.1).
//
// The kind is never inferred from the token's shape, so both arms below build
// the third tier out of a kind the LANDED surface actually declared: they
// handshake with the real surface first, then serve that declared kind from a
// stub that refuses both the push stream and the declared poll form — which is
// the only build shape the revision poll exists for.
// ---------------------------------------------------------------------------

// gitTokenBFS048 is a well-formed git-kind token. It is only ever served as a
// header value; the client never reads a token's shape to decide the kind.
const gitTokenBFS048 = "git:0123456789abcdef0123456789abcdef01234567"

// TestRevCoverageIsKindScopedAndHasNoDefault pins the table itself, including
// the arm that matters most: a kind this client does not know claims NO coverage
// rather than inheriting an old promise.
func TestRevCoverageIsKindScopedAndHasNoDefault(t *testing.T) {
	cases := []struct {
		kind, vouchesFor, gap string
	}{
		{RevKindGit, RevVouchesForCommits, RevGapUncommittedWrites},
		{RevKindCounter, RevVouchesForSurfaceWrites, RevGapForeignWrites},
		// A kind a future server may declare: unknown here, so nothing is claimed.
		{"composite", RevVouchesForNothing, RevGapKindUndeclared},
		// No declaration at all (a build with no capability document).
		{"", RevVouchesForNothing, RevGapKindUndeclared},
		// An unrecognised spelling is not guessed at either: the kind is a
		// declared value, not a case-insensitive hint.
		{"GIT", RevVouchesForNothing, RevGapKindUndeclared},
	}
	for _, tc := range cases {
		gotFor, gotGap := revCoverage(tc.kind)
		if gotFor != tc.vouchesFor || gotGap != tc.gap {
			t.Errorf("revCoverage(%q) = (%q, %q), want (%q, %q)", tc.kind, gotFor, gotGap, tc.vouchesFor, tc.gap)
		}
	}
	// Non-vacuity: the two kinds a build really declares must not produce the
	// same report, or a constant would pass every row above.
	gitFor, gitGap := revCoverage(RevKindGit)
	counterFor, counterGap := revCoverage(RevKindCounter)
	if gitFor == counterFor || gitGap == counterGap {
		t.Fatalf("git and counter report identically (%q/%q): the table cannot be a constant", gitFor, gitGap)
	}
}

// TestTheLandedSurfaceDeclaresTheRevisionKindTheReportIsBuiltFrom is the
// provenance arm: the kind the report uses must be the one the running surface
// declared in its capability document (§8.1 — never a config flag, a build tag
// or a negotiated HTTP version). It drives a real handshake against the landed
// surface over a git work tree and over a tree with no .git at all.
func TestTheLandedSurfaceDeclaresTheRevisionKindTheReportIsBuiltFrom(t *testing.T) {
	gitClient, gitRoot, _ := fixtureEndpoint(t)
	writeGitWorkTreeBFS048(t, gitRoot, strings.Repeat("ab", 20))
	if _, oerr := gitClient.Handshake(context.Background()); oerr != nil {
		t.Fatalf("handshake (git tree): %v", oerr)
	}
	if got := gitClient.RevKind(); got != RevKindGit {
		t.Fatalf("declared revision kind on a git tree = %q, want %q (extensions.rev.kind is the only admissible source)", got, RevKindGit)
	}

	plainClient, _, _ := fixtureEndpoint(t)
	if _, oerr := plainClient.Handshake(context.Background()); oerr != nil {
		t.Fatalf("handshake (non-git tree): %v", oerr)
	}
	if got := plainClient.RevKind(); got != RevKindCounter {
		t.Fatalf("declared revision kind on a non-git tree = %q, want %q", got, RevKindCounter)
	}

	// A client that never handshook has no declaration and must not invent one.
	bare, err := NewClient(Options{BaseURL: "http://127.0.0.1:1/dav", BindTimeout: time.Second, OpTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := bare.RevKind(); got != "" {
		t.Fatalf("an unbound client reported revision kind %q, want none declared", got)
	}
}

// TestTheRevisionTierReportsTheClassOfChangeItCannotSee is BFS-048's GREEN: on
// the last-resort tier, a git-tree mount must say that what it polls can vouch
// for commits only, instead of reading as current for a tree whose working
// bytes it cannot see.
func TestTheRevisionTierReportsTheClassOfChangeItCannotSee(t *testing.T) {
	for _, tc := range []struct {
		name           string
		gitTree        bool
		wantKind       string
		wantVouchesFor string
		wantGap        string
	}{
		{
			name: "git tree", gitTree: true,
			wantKind: RevKindGit, wantVouchesFor: RevVouchesForCommits, wantGap: RevGapUncommittedWrites,
		},
		{
			name: "non-git tree", gitTree: false,
			wantKind: RevKindCounter, wantVouchesFor: RevVouchesForSurfaceWrites, wantGap: RevGapForeignWrites,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, kind := thirdTierStateBFS048(t, tc.gitTree)
			if kind != tc.wantKind {
				t.Fatalf("the landed surface declared kind %q, want %q", kind, tc.wantKind)
			}
			if st.Mechanism != MechanismRev {
				t.Fatalf("mechanism in force = %q, want %q (this arm is about the last-resort tier)", st.Mechanism, MechanismRev)
			}
			if st.RevKind != tc.wantKind {
				t.Fatalf("rev_kind = %q, want %q (the DECLARED kind, not a guess)", st.RevKind, tc.wantKind)
			}
			if st.RevVouchesFor != tc.wantVouchesFor {
				t.Fatalf("rev_vouches_for = %q, want %q", st.RevVouchesFor, tc.wantVouchesFor)
			}
			if st.RevGap != tc.wantGap {
				t.Fatalf("rev_gap = %q, want %q: the mechanism cannot move for that class of change and must say so", st.RevGap, tc.wantGap)
			}
			// The report has to reach a CONSUMER, not only a Go reader: the
			// status document is what `bunker fs status --json` prints.
			raw, err := json.Marshal(st)
			if err != nil {
				t.Fatalf("marshal state: %v", err)
			}
			doc := string(raw)
			for _, want := range []string{
				`"rev_kind":"` + tc.wantKind + `"`,
				`"rev_vouches_for":"` + tc.wantVouchesFor + `"`,
				`"rev_gap":"` + tc.wantGap + `"`,
			} {
				if !strings.Contains(doc, want) {
					t.Fatalf("the status document does not carry %s: %s", want, doc)
				}
			}
			// The tier's own reason still quotes the server's refusal: adding
			// coverage must not cost the record its provenance.
			if !strings.Contains(st.Reason, "capability_unavailable") {
				t.Fatalf("reason = %q, want the server's own refusal quoted", st.Reason)
			}
		})
	}
}

// TestTheCoverageReportIsSilentWhenAnotherMechanismIsInForce is the arm that
// keeps the report honest in the other direction: a mount answered by the
// per-path poll (`events`) observes the tree directly, so it must NOT carry a
// revision gap. An implementation that reported the gap unconditionally would
// fail here — which is what makes the green arm above non-vacuous.
func TestTheCoverageReportIsSilentWhenAnotherMechanismIsInForce(t *testing.T) {
	c, _, _ := fixtureEndpoint(t)
	if _, oerr := c.Handshake(context.Background()); oerr != nil {
		t.Fatalf("handshake: %v", oerr)
	}
	rec := &collectDrops{}
	inv := NewInvalidator(c, InvalidateOptions{
		Mode:         "auto",
		PollInterval: 20 * time.Millisecond,
		OnDrop:       rec.drop,
		OnResync:     rec.resync,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = inv.Run(ctx) }()

	if !waitFor(5*time.Second, func() bool { return inv.State().Available }) {
		t.Fatalf("the channel never answered: %+v", inv.State())
	}
	st := inv.State()
	if st.Mechanism != MechanismEvents {
		t.Fatalf("mechanism = %q, want %q (the poll form the landed surface serves)", st.Mechanism, MechanismEvents)
	}
	if st.RevKind != "" || st.RevVouchesFor != "" || st.RevGap != "" {
		t.Fatalf("the revision coverage report fired while %q was the mechanism in force: %+v", st.Mechanism, st)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if strings.Contains(string(raw), "rev_gap") {
		t.Fatalf("rev_gap reached the status document while events was in force: %s", raw)
	}
}

// thirdTierStateBFS048 takes a client to the third tier — a build that serves
// neither the push stream nor the declared poll form — and returns the state it
// reports there, plus the revision kind the LANDED surface declared for the
// same tree (the stub serves that string, so the arm is built from a real
// declaration rather than from a literal).
func thirdTierStateBFS048(t *testing.T, gitTree bool) (InvalidationState, string) {
	t.Helper()
	real, root, _ := fixtureEndpoint(t)
	if gitTree {
		writeGitWorkTreeBFS048(t, root, gitTokenBFS048[4:])
	}
	if _, oerr := real.Handshake(context.Background()); oerr != nil {
		t.Fatalf("handshake against the landed surface: %v", oerr)
	}
	kind := real.RevKind()
	if kind == "" {
		t.Fatal("the landed surface declared no revision kind: this arm cannot build the third tier")
	}

	token := "rev:7"
	if gitTree {
		token = gitTokenBFS048
	}
	srv := revTierStubBFS048(t, kind, token)
	defer srv.Close()

	c, err := NewClient(Options{BaseURL: srv.URL + "/dav", Concurrency: 4, OpTimeout: 5 * time.Second, BindTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, oerr := c.Handshake(context.Background()); oerr != nil {
		t.Fatalf("handshake against the third-tier build: %v", oerr)
	}
	rec := &collectDrops{}
	inv := NewInvalidator(c, InvalidateOptions{
		Mode:         "auto",
		PollInterval: 20 * time.Millisecond,
		OnDrop:       rec.drop,
		OnResync:     rec.resync,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = inv.Run(ctx) }()

	if !waitFor(5*time.Second, func() bool { return inv.Mechanism() == MechanismRev && inv.State().Available }) {
		t.Fatalf("the revision tier never answered: %+v", inv.State())
	}
	// A constant token means the tier is genuinely quiet: no resync may be
	// invented, and quiet is exactly the state the report must qualify.
	if n := len(rec.resyncs); n != 0 {
		t.Fatalf("the revision poll fired %d resync(s) with no revision movement: %v", n, rec.resyncs)
	}
	return inv.State(), kind
}

// revTierStubBFS048 serves the third tier: OPTIONS carries the tree/rev headers
// every response carries, `watch` is refused with the declared degradation, and
// the capability document declares the given revision kind with NO poll form —
// so the client's poll tier has nowhere to go but the revision poll.
func revTierStubBFS048(t *testing.T, kind, rev string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("DAV", "1")
			w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND, POST")
			w.Header().Set("X-Bunker-Capabilities", "1")
			w.Header().Set("X-Bunker-Extensions", "identity,if_match_refuse,rev,tree,op")
			w.Header().Set("X-Bunker-Tree", "tree:0123456789abcdef")
			w.Header().Set("X-Bunker-Rev", rev)
			w.WriteHeader(http.StatusOK)
			return
		}
		op := strings.TrimSpace(r.Header.Get("X-Bunker-Op"))
		switch op {
		case "capabilities":
			writeRevEnvelopeBFS048(w, http.StatusOK, "ok", map[string]any{"capabilities": map[string]any{
				"surface":          "bunkerd-webdav/1",
				"document_version": 1,
				"extensions": map[string]any{
					// No poll form is declared: this build serves neither the
					// push stream nor the events poll (SPEC-watcher-capability
					// §2.1's third row is the tier this arm is about).
					"watch": map[string]any{"name": "X-Bunker-Op: watch", "v": 1, "mode": "poll",
						"modes": map[string]any{"push": "inotify\u2192stream"}},
					"rev": map[string]any{"name": "X-Bunker-Rev", "v": 1, "kind": kind},
				},
				"degradations": []any{},
			}}, nil, rev)
		default:
			// watch and events: the declared degradation, as the landed build
			// answers on a target with no watcher.
			writeRevEnvelopeBFS048(w, http.StatusNotImplemented, "capability_unavailable", nil, map[string]any{
				"capability": "watch", "scope": "target", "mode": "poll",
				"detail": "no inotify watcher on this target; the declared poll form X-Bunker-Op: events carries the channel (mode=poll)",
			}, rev)
		}
	}))
}

// writeRevEnvelopeBFS048 writes the E-4 envelope shape with the rev/tree headers
// of the token the caller named (writeStubEnvelope pins a git-shaped token,
// which would be a lie for the counter arm).
func writeRevEnvelopeBFS048(w http.ResponseWriter, status int, verdict string, result any, eerr map[string]any, rev string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Bunker-Verdict", verdict)
	w.Header().Set("X-Bunker-Tree", "tree:0123456789abcdef")
	w.Header().Set("X-Bunker-Rev", rev)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": status == http.StatusOK, "op": "", "verdict": verdict,
		"rev": rev, "tree": "tree:0123456789abcdef",
		"proto": "HTTP/1.1", "duration_ms": 0, "truncated": false,
		"result": result, "error": eerr,
	})
}

// writeGitWorkTreeBFS048 adds the minimum a git work tree has (HEAD + a loose
// ref) so the landed surface serves the git kind for this root. It is the same
// fixture shape the server-side arms use, kept local because the client package
// cannot see another package's test helpers.
func writeGitWorkTreeBFS048(t *testing.T, root, head string) {
	t.Helper()
	mustWrite(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(root, ".git", "refs", "heads", "main"), head+"\n")
}
