package audit

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastShipBackoff shrinks a shipper's retry backoff so tests exercise the
// retry path in milliseconds instead of the production 1s..60s ladder.
func fastShipBackoff(s *Shipper) {
	s.backoffBase = 5 * time.Millisecond
	s.backoffMax = 40 * time.Millisecond
}

// newTestLogOpts creates an AuditLog with GAP-073 options in a temp dir.
func newTestLogOpts(t *testing.T, opts Options) (*AuditLog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := NewWithOptions(path, opts)
	if err != nil {
		t.Fatalf("NewWithOptions(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

// TestShipOnRotationWebhook: rotating the audit log POSTs the just-rotated
// segment's bytes to the configured webhook with the segment's final chain
// head in X-Bunker-Chain-Head.
func TestShipOnRotationWebhook(t *testing.T) {
	type payload struct {
		body []byte
		head string
	}
	got := make(chan payload, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read ship body: %v", err)
		}
		got <- payload{body: b, head: r.Header.Get("X-Bunker-Chain-Head")}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	l, path := newTestLogOpts(t, Options{ShipTo: srv.URL})
	fastShipBackoff(l.shipper)
	l.rotateAt = 128

	writeRecords(t, l, 2) // .1 = [rec-1], live = [rec-2]

	select {
	case p := <-got:
		segment := readLog(t, path+".1")
		if !bytes.Equal(p.body, segment) {
			t.Errorf("shipped body != rotated segment bytes (got %d bytes, want %d)", len(p.body), len(segment))
		}
		wantHead := parseRecords(t, segment)[len(parseRecords(t, segment))-1]["hash"].(string)
		if p.head != wantHead {
			t.Errorf("X-Bunker-Chain-Head = %q, want segment's final chain head %q", p.head, wantHead)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook received no ship payload within 5s")
	}

	// The ship attempt must be reflected in the snapshot and the state file.
	// Both are written asynchronously by the shipper worker — poll.
	deadline := time.Now().Add(5 * time.Second)
	var shipState *ShipState
	for time.Now().Before(deadline) {
		if st := l.StatusSnapshot(); st.LastShipResult == "ok" && st.ShipQueueDepth == 0 {
			if s, err := ReadShipState(path); err == nil {
				shipState = s
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if shipState == nil {
		t.Fatal("ship state file not written/ok within 5s")
	}
	st := l.StatusSnapshot()
	if !st.ShippingEnabled {
		t.Error("StatusSnapshot.ShippingEnabled = false, want true")
	}
	if st.LastShipResult != "ok" {
		t.Errorf("LastShipResult = %q, want ok", st.LastShipResult)
	}
	if st.LastShipAttempt.IsZero() || st.LastShipSuccess.IsZero() {
		t.Error("ship attempt/success timestamps not recorded")
	}
	if st.ShipQueueDepth != 0 {
		t.Errorf("ShipQueueDepth = %d, want 0 (segment delivered)", st.ShipQueueDepth)
	}
	if shipState.LastResult != "ok" || shipState.QueueDepth != 0 {
		t.Errorf("ship state = %+v, want last_result ok / queue_depth 0", shipState)
	}
}

// TestShipWebhookContentType pins the ship request content type.
func TestShipWebhookContentType(t *testing.T) {
	ctCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctCh <- r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	l, _ := newTestLogOpts(t, Options{ShipTo: srv.URL})
	fastShipBackoff(l.shipper)
	l.rotateAt = 128
	writeRecords(t, l, 2)

	select {
	case ct := <-ctCh:
		if ct != "application/x-ndjson" {
			t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ship request within 5s")
	}
}

// TestSealRecordChainsAndVerifies: with seal_key set, every rotation appends
// a SEAL record to the FRESH file whose prev_hash is the rotated segment's
// final chain head and whose seal is HMAC-SHA256(key, head) hex — and the
// hash chain (and Verify) stays intact across the seal.
func TestSealRecordChainsAndVerifies(t *testing.T) {
	const key = "gap073-test-seal-key"
	l, path := newTestLogOpts(t, Options{SealKey: key})
	l.rotateAt = 128

	writeRecords(t, l, 2) // .1 = [rec-1], live = [seal, rec-2]

	backupRecs := parseRecords(t, readLog(t, path+".1"))
	sealedHead := backupRecs[len(backupRecs)-1]["hash"].(string)

	live := parseRecords(t, readLog(t, path))
	if len(live) != 2 {
		t.Fatalf("live file has %d records, want [seal, rec-2]", len(live))
	}
	sealRec := live[0]
	if sealRec["method"] != SealMethod {
		t.Errorf("seal record method = %v, want %v", sealRec["method"], SealMethod)
	}
	if sealRec["caller"] != "bunkerd" {
		t.Errorf("seal record caller = %v, want bunkerd", sealRec["caller"])
	}
	if sealRec["outcome"] != "ok" {
		t.Errorf("seal record outcome = %v, want ok", sealRec["outcome"])
	}
	if sealRec["summary"] != "rotation seal" {
		t.Errorf("seal record summary = %v, want 'rotation seal'", sealRec["summary"])
	}
	if prev, _ := sealRec["prev_hash"].(string); prev != sealedHead {
		t.Errorf("seal prev_hash = %v, want sealed segment head %v", prev, sealedHead)
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(sealedHead))
	if want := hex.EncodeToString(mac.Sum(nil)); sealRec["seal"] != want {
		t.Errorf("seal = %v, want HMAC-SHA256(key, head) %v", sealRec["seal"], want)
	}
	// The seal record itself is chained (its own hash covers it, and the
	// next record chains to it).
	if sealRec["hash"] == "" || live[1]["prev_hash"] != sealRec["hash"] {
		t.Errorf("seal record not chained: hash=%v next prev_hash=%v", sealRec["hash"], live[1]["prev_hash"])
	}

	records, firstBad, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify across sealed rotation: %v", err)
	}
	if records != 3 || firstBad != 0 {
		t.Errorf("Verify = (%d records, firstBad %d), want (3, 0)", records, firstBad)
	}

	st := l.StatusSnapshot()
	if !st.SealingEnabled {
		t.Error("StatusSnapshot.SealingEnabled = false, want true (seal_key configured)")
	}
}

// TestNoSealWithoutKey: empty seal_key means no seal records at all.
func TestNoSealWithoutKey(t *testing.T) {
	l, path := newTestLogOpts(t, Options{})
	l.rotateAt = 128
	writeRecords(t, l, 2)
	for _, p := range []string{path, path + ".1"} {
		for i, rec := range parseRecords(t, readLog(t, p)) {
			if _, has := rec["seal"]; has {
				t.Errorf("%s record %d carries a seal field without seal_key", p, i+1)
			}
		}
	}
}

// TestShipFailureRetriesThenReplays: a failing endpoint must not block
// Log()/rotation, the segment must wait in the retry queue, and the queue
// must drain once the endpoint recovers.
func TestShipFailureRetriesThenReplays(t *testing.T) {
	var calls atomic.Int64
	const failFirst = 2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= failFirst {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	l, path := newTestLogOpts(t, Options{ShipTo: srv.URL})
	s := l.shipper
	// Production-like ladder scaled down: ~100ms first retry so the test
	// can observe the queued state between attempts.
	s.backoffBase = 100 * time.Millisecond
	s.backoffMax = 150 * time.Millisecond
	l.rotateAt = 128

	writeRecords(t, l, 2) // rotation -> ship attempt 1 fails

	// Log() must never block or fail on the dead endpoint.
	start := time.Now()
	if err := l.Log(Record{TS: "t", Caller: "master", Method: "/m", Outcome: "ok", Summary: "post-failure"}); err != nil {
		t.Fatalf("Log during ship failure: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Log took %v during ship failure — write path blocked", d)
	}

	// A failed segment waits in the bounded retry queue (the second record
	// may itself have rotated and joined it — every rotation ships).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, res, depth := s.snapshot(); strings.Contains(res, "error") && depth >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, lastRes, depth := s.snapshot(); !strings.Contains(lastRes, "error") || depth < 1 {
		t.Fatalf("after failed ship: result=%q depth=%d, want error + depth >= 1", lastRes, depth)
	}

	// Endpoint recovered (handler serves 200 from call 3 on): the queue
	// must replay and drain.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, lastRes, depth := s.snapshot(); depth == 0 && lastRes == "ok" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, _, lastRes, depth := s.snapshot(); depth != 0 || lastRes != "ok" {
		t.Fatalf("queue did not drain after recovery: result=%q depth=%d", lastRes, depth)
	}
	if got := calls.Load(); got < failFirst+1 {
		t.Errorf("endpoint received %d calls, want >= %d (2 failures + replay)", got, failFirst+1)
	}
	shipState, err := ReadShipState(path)
	if err != nil {
		t.Fatalf("read ship state: %v", err)
	}
	if shipState.LastResult != "ok" || shipState.QueueDepth != 0 {
		t.Errorf("ship state = %+v, want ok/0 after replay", shipState)
	}
}

// TestShipQueueDropsOldestWhenFull: the in-memory retry queue is bounded at
// ShipQueueCap; enqueueing beyond the cap drops the OLDEST segment.
func TestShipQueueDropsOldestWhenFull(t *testing.T) {
	l, path := newTestLogOpts(t, Options{})
	dir := filepath.Dir(path)
	s, err := NewShipper(path, "https://127.0.0.1:9/nope", nil)
	if err != nil {
		t.Fatalf("NewShipper: %v", err)
	}
	s.Stop() // worker gone: nothing consumes the queue
	l.mu.Lock()
	l.shipper = s
	l.mu.Unlock()

	for i := 0; i < ShipQueueCap+5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("seg-%03d", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("{\"n\":%d}\n", i)), 0o600); err != nil {
			t.Fatal(err)
		}
		s.ShipSegment(p, fmt.Sprintf("head-%03d", i), "")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) != ShipQueueCap {
		t.Fatalf("queue depth = %d, want cap %d", len(s.queue), ShipQueueCap)
	}
	if s.queue[0].seq != 6 {
		t.Errorf("oldest retained seq = %d, want 6 (5 oldest dropped)", s.queue[0].seq)
	}
}

// TestShipSyslogUDP: syslog shipping sends one datagram per record line
// terminated by a SEAL datagram carrying the chain head (and the seal when
// sealing is on).
func TestShipSyslogUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("udp listen unavailable: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	l, path := newTestLogOpts(t, Options{ShipTo: "syslog://" + pc.LocalAddr().String(), SealKey: "syslog-key"})
	l.rotateAt = 128

	type rx struct {
		data string
		err  error
	}
	packets := make(chan rx, 32)
	go func() {
		buf := make([]byte, 4096)
		for {
			_ = pc.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				packets <- rx{err: err}
				return
			}
			packets <- rx{data: string(buf[:n])}
		}
	}()

	writeRecords(t, l, 2) // .1 = [rec-1], live = [seal, rec-2]
	backupHead := parseRecords(t, readLog(t, path+".1"))[0]["hash"].(string)
	wantSeal := computeSeal("syslog-key", backupHead)

	var sawRec1, sawSeal bool
	deadline := time.After(5 * time.Second)
	for (!sawRec1 || !sawSeal) && deadline != nil {
		select {
		case p := <-packets:
			if p.err != nil {
				t.Fatalf("udp read: %v (sawRec1=%v sawSeal=%v)", p.err, sawRec1, sawSeal)
			}
			if !strings.Contains(p.data, "<14>") || !strings.Contains(p.data, "bunkerd-audit:") {
				t.Errorf("datagram missing PRI/tag: %q", p.data)
			}
			if strings.Contains(p.data, `"summary":"rec-1"`) {
				sawRec1 = true
			}
			if strings.Contains(p.data, "SEAL chain_head="+backupHead) && strings.Contains(p.data, " seal="+wantSeal) {
				sawSeal = true
			}
		case <-deadline:
			deadline = nil
		}
	}
	if !sawRec1 {
		t.Error("syslog payload missing the rotated segment's record line")
	}
	if !sawSeal {
		t.Error("syslog payload missing SEAL datagram with chain head + seal")
	}
}

// TestShipToDisabledByDefault: New(path) attaches nothing — StatusSnapshot
// reports both features off and rotations create no ship-state file and no
// seal records (byte-identical pre-GAP-073 behavior).
func TestShipToDisabledByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	st := l.StatusSnapshot()
	if st.ShippingEnabled || st.SealingEnabled || st.ShipTo != "" {
		t.Fatalf("StatusSnapshot with plain New = %+v, want everything disabled", st)
	}

	l.rotateAt = 128
	writeRecords(t, l, 3) // two rotations
	for _, p := range []string{path, path + ".1", path + ".2"} {
		for i, rec := range parseRecords(t, readLog(t, p)) {
			if _, has := rec["seal"]; has {
				t.Errorf("%s record %d carries a seal field — off-by-default broken", p, i+1)
			}
		}
	}
	if _, err := os.Stat(path + ".shipstate"); !os.IsNotExist(err) {
		t.Errorf("ship-state file created without ship_to: %v", err)
	}
	if _, firstBad, err := Verify(path); err != nil || firstBad != 0 {
		t.Errorf("Verify = %v (firstBad %d), want clean chain", err, firstBad)
	}
}

// TestSetShipperAttachDetach covers the setter used by daemon wiring.
func TestSetShipperAttachDetach(t *testing.T) {
	l, path := newTestLogOpts(t, Options{})
	if err := l.SetShipper("ftp://bad-scheme", nil); err == nil {
		t.Fatal("SetShipper with unsupported scheme returned nil error")
	}
	if st := l.StatusSnapshot(); st.ShippingEnabled {
		t.Error("failed SetShipper must leave shipping disabled")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(srv.Close)
	if err := l.SetShipper(srv.URL, nil); err != nil {
		t.Fatalf("SetShipper: %v", err)
	}
	if st := l.StatusSnapshot(); !st.ShippingEnabled {
		t.Error("SetShipper did not enable shipping")
	}
	if err := l.SetShipper("", nil); err != nil {
		t.Fatalf("SetShipper(\"\"): %v", err)
	}
	if st := l.StatusSnapshot(); st.ShippingEnabled {
		t.Error("SetShipper(\"\") did not disable shipping")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("log path changed by SetShipper: %v", err)
	}
}

// TestStatusSnapshotReportsQueueDepth: pending segments are visible in the
// snapshot for the status surface.
func TestStatusSnapshotReportsQueueDepth(t *testing.T) {
	l, path := newTestLogOpts(t, Options{})
	s, err := NewShipper(path, "https://127.0.0.1:9/nope", nil)
	if err != nil {
		t.Fatalf("NewShipper: %v", err)
	}
	s.Stop()
	l.mu.Lock()
	l.shipper = s
	l.mu.Unlock()

	seg := filepath.Join(filepath.Dir(path), "seg")
	if err := os.WriteFile(seg, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.ShipSegment(seg, "head", "")
	st := l.StatusSnapshot()
	if !st.ShippingEnabled || st.ShipQueueDepth != 1 {
		t.Errorf("StatusSnapshot = %+v, want shipping with queue depth 1", st)
	}
	if st.ShipTo != "https://127.0.0.1:9/nope" {
		t.Errorf("ShipTo = %q, want the configured endpoint", st.ShipTo)
	}
}

// TestParseShipTo covers the accepted schemes and the rejected shapes.
func TestParseShipTo(t *testing.T) {
	cases := []struct {
		name    string
		uri     string
		want    ShipTarget
		wantErr bool
	}{
		{name: "https", uri: "https://collector.example.net/v1/audit", want: ShipTarget{Scheme: "https", URL: "https://collector.example.net/v1/audit"}},
		{name: "http", uri: "http://127.0.0.1:9999/hook", want: ShipTarget{Scheme: "http", URL: "http://127.0.0.1:9999/hook"}},
		{name: "syslog with port", uri: "syslog://10.0.0.5:601", want: ShipTarget{Scheme: "syslog", Addr: "10.0.0.5:601"}},
		{name: "syslog default port", uri: "syslog://syslog.internal", want: ShipTarget{Scheme: "syslog", Addr: net.JoinHostPort("syslog.internal", "514")}},
		{name: "unsupported scheme", uri: "ftp://x/y", wantErr: true},
		{name: "https without host", uri: "https:///path", wantErr: true},
		{name: "syslog without host", uri: "syslog://", wantErr: true},
		{name: "not a URI", uri: "not a uri at all", wantErr: true},
		{name: "control character", uri: "https://h/\x7f", wantErr: true},
		{name: "empty", uri: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseShipTo(tc.uri)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseShipTo(%q) = %+v, want error", tc.uri, got)
				}
				var inv *InvalidShipToError
				if !errors.As(err, &inv) {
					t.Errorf("error %T is not *InvalidShipToError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseShipTo(%q): %v", tc.uri, err)
			}
			if got != tc.want {
				t.Errorf("ParseShipTo(%q) = %+v, want %+v", tc.uri, got, tc.want)
			}
		})
	}
}

// TestRedactShipTo: credentials in the ship endpoint never reach logs.
func TestRedactShipTo(t *testing.T) {
	cases := map[string]string{
		"https://user:secret@collector.example.net/v1": "https://collector.example.net/v1",
		"http://127.0.0.1:9999/h":                      "http://127.0.0.1:9999/h",
		"syslog://10.0.0.5:601":                        "syslog://10.0.0.5:601",
		"garbage uri":                                  "redacted",
	}
	for in, want := range cases {
		if got := RedactShipTo(in); got != want {
			t.Errorf("RedactShipTo(%q) = %q, want %q", in, got, want)
		}
	}
	if got := RedactShipTo("https://user:secret@collector.example.net/v1"); strings.Contains(got, "secret") {
		t.Errorf("RedactShipTo leaked credentials: %q", got)
	}
}

// TestReadShipStateMissing: no ship-state file (shipping off) reads as
// not-exist, which the CLI reports as "shipping disabled".
func TestReadShipStateMissing(t *testing.T) {
	if _, err := ReadShipState(filepath.Join(t.TempDir(), "audit.log")); !os.IsNotExist(err) {
		t.Errorf("err = %v, want IsNotExist", err)
	}
}

// TestLocalStatusReportsSealAndSizes: the status view exposes the newest
// seal record, per-file sizes, and the rotation lower bound.
func TestLocalStatusReportsSealAndSizes(t *testing.T) {
	l, path := newTestLogOpts(t, Options{SealKey: "status-key"})
	l.rotateAt = 128
	writeRecords(t, l, 4)
	// Layout with seals (each seal record itself ~rotates the file):
	// .3=[rec-1], .2=[seal, rec-2], .1=[seal, rec-3], live=[seal, rec-4]
	// → 7 retained records (4 data + 3 seals), 3 rotations.

	st, err := LocalStatus(path)
	if err != nil {
		t.Fatalf("LocalStatus: %v", err)
	}
	if !st.Enabled {
		t.Error("Enabled = false, want true")
	}
	if st.Records != 7 {
		t.Errorf("Records = %d, want 7 (4 records + 3 seals)", st.Records)
	}
	if st.RotationsLowerBound != 3 {
		t.Errorf("RotationsLowerBound = %d, want 3", st.RotationsLowerBound)
	}
	if st.BackupSizes[0] <= 0 || st.BackupSizes[1] <= 0 || st.BackupSizes[2] <= 0 {
		t.Errorf("BackupSizes = %v, want .1/.2/.3 all present", st.BackupSizes)
	}
	if st.ChainHead == "" {
		t.Error("ChainHead empty, want last live record's hash")
	}
	// Newest seal = the one in the LIVE file (seals .1's chain).
	if st.LastSeal == nil {
		t.Fatal("LastSeal = nil, want the live file's seal record")
	}
	live := parseRecords(t, readLog(t, path))
	if st.LastSeal.Seal != live[0]["seal"] || st.LastSeal.Hash != live[0]["hash"] {
		t.Errorf("LastSeal = %+v, want live file's seal record", st.LastSeal)
	}
	if st.LastSeal.SealedHead != live[0]["prev_hash"] {
		t.Errorf("SealedHead = %v, want the seal record's prev_hash %v", st.LastSeal.SealedHead, live[0]["prev_hash"])
	}
	if !st.SealingEnabled {
		t.Error("SealingEnabled = false, want true (seal records on disk)")
	}
}

// TestComputeSeal pins the seal derivation.
func TestComputeSeal(t *testing.T) {
	got := computeSeal("key", "head")
	mac := hmac.New(sha256.New, []byte("key"))
	mac.Write([]byte("head"))
	if want := hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Errorf("computeSeal = %s, want %s", got, want)
	}
	if len(got) != 64 {
		t.Errorf("seal length = %d, want 64 hex chars", len(got))
	}
	// Different key or head must change the seal.
	if computeSeal("key2", "head") == got || computeSeal("key", "head2") == got {
		t.Error("seal does not depend on key/head")
	}
}

// TestSealSurvivesRotationIntoNextSegment: the record after the seal (first
// of the NEXT rotation) chains through the seal — the seal is a real link.
func TestSealSurvivesRotationIntoNextSegment(t *testing.T) {
	l, path := newTestLogOpts(t, Options{SealKey: "k2"})
	l.rotateAt = 128
	writeRecords(t, l, 4) // .2=[rec-1], .1=[seal, rec-2], live=[seal, rec-3, rec-4]

	// Live file: first record is the seal for .1's chain.
	one := parseRecords(t, readLog(t, path+".1"))
	live := parseRecords(t, readLog(t, path))
	seal := live[0]
	if seal["method"] != SealMethod {
		t.Fatalf("first live record = %v, want seal", seal["method"])
	}
	if prev, _ := seal["prev_hash"].(string); prev != one[len(one)-1]["hash"].(string) {
		t.Errorf("seal prev_hash = %v, want .1 tail", prev)
	}
	if live[1]["prev_hash"] != seal["hash"] {
		t.Errorf("record after seal prev_hash = %v, want seal hash %v", live[1]["prev_hash"], seal["hash"])
	}
	if _, firstBad, err := Verify(path); err != nil || firstBad != 0 {
		t.Errorf("Verify = %v (firstBad %d), want clean", err, firstBad)
	}
}

// TestShipPayloadJSONLinesIntact: the webhook body is the segment's exact
// JSONL (newline-terminated), replayable by the collector, in FIFO order.
func TestShipPayloadJSONLinesIntact(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	l, _ := newTestLogOpts(t, Options{ShipTo: srv.URL})
	fastShipBackoff(l.shipper)
	l.rotateAt = 128
	writeRecords(t, l, 3) // two rotations → two shipped segments

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("got %d ship payloads within 5s, want >= 2 (one per rotation)", len(bodies))
	}
	// First delivery = first rotated segment = exactly the chain's first
	// record (prev_hash ""), lossless bytes.
	var rec map[string]any
	if err := json.Unmarshal(bodies[0], &rec); err != nil {
		t.Fatalf("shipped line not JSON: %v", err)
	}
	if rec["prev_hash"] != "" {
		t.Errorf("first shipped record prev_hash = %v, want \"\" (chain start)", rec["prev_hash"])
	}
	if rec["hash"] == "" {
		t.Error("first shipped record has empty hash")
	}
}
