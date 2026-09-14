// GAP-073 audit-log retention hardening: remote shipping of rotated audit
// segments, ship-state bookkeeping for `bunker audit status`. Rotation seals
// live in audit.go (they are part of the on-disk chain); this file owns the
// network side. Everything here is opt-in: with no audit.ship_to configured
// no code in this file runs and the audit log behaves exactly as before.
//
// Shipping is FIRE-AND-FORGET: ShipSegment never blocks the write path, and
// a failed ship is retried from a bounded in-memory queue with exponential
// backoff, then dropped with a warning. Losing a shipped copy is always
// preferable to slowing down or failing the audit write.
package audit

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SealMethod is the pseudo-procedure recorded on rotation seal records
// (audit.seal_key enabled). Seals are chained like any other record.
const SealMethod = "/internal/audit/seal"

// ShipQueueCap bounds the in-memory retry queue in SEGMENTS (a segment is
// one rotated audit file, ≤ MaxSize bytes). When full, the oldest segment is
// dropped with a warning — shipping must never grow without bound on a host
// whose remote endpoint is down.
const ShipQueueCap = 100

// MaxShipAttempts is how many times one segment is tried before it is
// dropped: with the default backoff (1s, 2s, 4s, 8s) a segment is given
// ~15s of endpoint availability before being dropped with a warning.
const MaxShipAttempts = 5

const (
	// defaultSyslogPort is used when syslog://host carries no port (the
	// RFC 3164 well-known port).
	defaultSyslogPort = "514"
	// shipHTTPTimeout bounds one webhook POST (fire-and-forget: bounded).
	shipHTTPTimeout = 10 * time.Second
	// shipDialTimeout bounds the syslog UDP dial and the write deadline.
	shipDialTimeout = 5 * time.Second
	// defaultShipBackoff is the first retry delay; it doubles per attempt.
	defaultShipBackoff = time.Second
	// maxShipBackoff caps the exponential retry delay.
	maxShipBackoff = 60 * time.Second
	// shipStateSuffix is appended to the audit log path for the ship-state
	// file the `bunker audit status` command reads.
	shipStateSuffix = ".shipstate"
	// syslogLineBudget keeps one syslog payload inside the RFC 3164 1024
	// byte datagram budget (PRI + timestamp + hostname + tag overhead
	// included). Syslog is the best-effort channel; the HTTPS webhook ships
	// the segment bytes losslessly.
	syslogLineBudget = 900
)

// ShipTarget is a parsed audit.ship_to endpoint URI. Two schemes are
// supported: https/http webhook (segment POSTed as the request body) and
// syslog (RFC 3164 messages over UDP — one datagram per audit record line,
// terminated by a SEAL datagram carrying the chain head).
type ShipTarget struct {
	Scheme string // "https", "http" or "syslog"
	URL    string // full webhook URL (https/http only)
	Addr   string // host:port (syslog only)
}

// ParseShipTo validates an audit.ship_to URI. An error here means the
// operator misconfigured shipping: callers must WARN and disable shipping,
// never crash or block the audit write path.
//
// ParseShipTo returns a *InvalidShipToError for every rejected URI so
// callers can distinguish a bad endpoint (fall back to a plain log) from an
// unopenable log file (give up on the audit trail).
func ParseShipTo(uri string) (ShipTarget, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return ShipTarget{}, &InvalidShipToError{URI: uri, Err: err}
	}
	switch u.Scheme {
	case "https", "http":
		if u.Host == "" {
			return ShipTarget{}, &InvalidShipToError{URI: uri, Err: fmt.Errorf("%s URL needs a host", u.Scheme)}
		}
		return ShipTarget{Scheme: u.Scheme, URL: u.String()}, nil
	case "syslog":
		if u.Host == "" {
			return ShipTarget{}, &InvalidShipToError{URI: uri, Err: fmt.Errorf("syslog URL needs a host")}
		}
		addr := u.Host
		if u.Port() == "" {
			addr = net.JoinHostPort(u.Hostname(), defaultSyslogPort)
		}
		return ShipTarget{Scheme: "syslog", Addr: addr}, nil
	default:
		return ShipTarget{}, &InvalidShipToError{URI: uri, Err: fmt.Errorf("unsupported scheme %q (want https://, http:// or syslog://)", u.Scheme)}
	}
}

// InvalidShipToError marks a rejected audit.ship_to URI. Callers (server.go)
// test for it with errors.As to warn-and-degrade instead of losing the whole
// audit trail.
type InvalidShipToError struct {
	URI string
	Err error
}

func (e *InvalidShipToError) Error() string {
	return fmt.Sprintf("parse ship_to %q: %v", e.URI, e.Err)
}

func (e *InvalidShipToError) Unwrap() error { return e.Err }

// RedactShipTo returns a log-safe form of a raw ship_to URI (userinfo
// stripped); invalid URIs degrade to the scheme only.
func RedactShipTo(uri string) string {
	t, err := ParseShipTo(uri)
	if err != nil {
		if u, perr := url.Parse(uri); perr == nil && u.Scheme != "" {
			return u.Scheme + "://redacted"
		}
		return "redacted"
	}
	return t.Redacted()
}

// Redacted returns a log-safe form of the target: embedded userinfo
// (https://user:pass@host/…) is stripped before the URI is written anywhere.
func (t ShipTarget) Redacted() string {
	if t.Scheme == "syslog" {
		return "syslog://" + t.Addr
	}
	if u, err := url.Parse(t.URL); err == nil {
		u.User = nil
		return u.String()
	}
	return t.Scheme + "://" + "redacted"
}

// ShipState is the on-disk snapshot the shipper writes after every ship
// attempt at <audit-path>.shipstate, so the local-only `bunker audit status`
// command can report shipping health without talking to the daemon.
type ShipState struct {
	ShipTo      string `json:"ship_to"`                // redacted (no credentials)
	LastAttempt string `json:"last_attempt,omitempty"` // RFC3339
	LastResult  string `json:"last_result,omitempty"`  // "ok" or "error: …"
	LastSuccess string `json:"last_success,omitempty"` // RFC3339
	QueueDepth  int    `json:"queue_depth"`
}

// ReadShipState loads the ship-state file written next to the audit log by
// the daemon's shipper. A missing file returns (nil, err) with
// os.IsNotExist(err) — the CLI reports "no ship state".
func ReadShipState(auditPath string) (*ShipState, error) {
	b, err := os.ReadFile(auditPath + shipStateSuffix)
	if err != nil {
		return nil, err
	}
	var st ShipState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", auditPath+shipStateSuffix, err)
	}
	return &st, nil
}

// AuditStatus is the point-in-time shipper/seal view exposed by
// AuditLog.StatusSnapshot (for the daemon and its operator tooling).
type AuditStatus struct {
	ShippingEnabled bool
	ShipTo          string    // redacted
	LastShipAttempt time.Time // zero = no attempt yet
	LastShipSuccess time.Time // zero = never succeeded
	LastShipResult  string    // "" = no attempt yet
	ShipQueueDepth  int
	SealingEnabled  bool
}

// shipItem is one rotated segment awaiting shipment.
type shipItem struct {
	seq       int // monotonic id; guards pop/requeue against queue-full drops
	data      []byte
	chainHead string
	seal      string // rotation seal hex ("" when sealing is off)
	attempts  int
	nextTry   time.Time
}

// Shipper ships rotated audit segments to a remote endpoint. One worker
// goroutine drains a bounded FIFO queue with exponential backoff; enqueue
// (from the rotation path) never blocks beyond reading the segment bytes.
type Shipper struct {
	target    ShipTarget
	auditPath string
	logger    *slog.Logger
	client    *http.Client
	hostname  string

	mu            sync.Mutex
	queue         []shipItem
	nextSeq       int
	lastAttemptAt time.Time
	lastSuccessAt time.Time
	lastResult    string
	// stopped is set by Stop under mu: writeState refuses to touch the
	// filesystem afterwards, so no shipstate write can land once Stop has
	// begun (belt to Stop's done-channel braces).
	stopped bool

	wake     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	// done is closed by the worker goroutine when run() has fully exited;
	// Stop waits on it so shutdown is deterministic.
	done chan struct{}

	// Test knobs: the production backoff is 1s doubling to 60s.
	backoffBase time.Duration
	backoffMax  time.Duration
}

// NewShipper creates a shipper for the audit log at auditPath and starts its
// worker goroutine. shipTo is validated here: an invalid URI is an error and
// the caller must warn and skip attaching the shipper. Stop the shipper via
// AuditLog.Close (or Stop directly in tests).
func NewShipper(auditPath, shipTo string, logger *slog.Logger) (*Shipper, error) {
	target, err := ParseShipTo(shipTo)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	host := "bunkerd"
	if h, herr := os.Hostname(); herr == nil && h != "" {
		host = h
	}
	s := &Shipper{
		target:      target,
		auditPath:   auditPath,
		logger:      logger,
		client:      &http.Client{Timeout: shipHTTPTimeout},
		hostname:    host,
		wake:        make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		backoffBase: defaultShipBackoff,
		backoffMax:  maxShipBackoff,
	}
	go s.run()
	return s, nil
}

// ShipSegment enqueues the just-rotated segment (now at segmentPath) for
// delivery with its final chain head and, when sealing is enabled, the
// rotation seal. Fire-and-forget: called under the audit log's write lock, it
// only reads the segment bytes (≤ MaxSize, page-cache warm) and appends to
// the bounded queue — no network I/O, no blocking on a dead endpoint. When
// the queue is full the OLDEST segment is dropped with a warning.
func (s *Shipper) ShipSegment(segmentPath, chainHead, seal string) {
	data, err := os.ReadFile(segmentPath)
	if err != nil {
		// The file existed a moment ago (we just rotated it); a read
		// failure here is abnormal and retrying would not fix it.
		s.logger.Warn("audit ship: rotated segment unreadable, not queued",
			"path", segmentPath, "error", err)
		s.recordResult(fmt.Sprintf("error: read segment: %v", err), 0)
		return
	}
	s.mu.Lock()
	if len(s.queue) >= ShipQueueCap {
		dropped := s.queue[0]
		s.queue = s.queue[1:]
		s.logger.Warn("audit ship queue full; dropped oldest segment",
			"dropped_seq", dropped.seq, "dropped_chain_head", dropped.chainHead)
	}
	s.nextSeq++
	s.queue = append(s.queue, shipItem{
		seq:       s.nextSeq,
		data:      data,
		chainHead: chainHead,
		seal:      seal,
		nextTry:   time.Now(),
	})
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run drains the queue until Stop. Single consumer: queue[0] is the item in
// flight, later items wait behind it (FIFO preserves segment order). Closing
// done is the worker's last act — everything Stop guarantees (no goroutine,
// no filesystem writes) holds only after it.
func (s *Shipper) run() {
	defer close(s.done)
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-s.wake:
			case <-s.stop:
				return
			}
			continue
		}
		item := s.queue[0]
		wait := time.Until(item.nextTry)
		s.mu.Unlock()

		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-s.wake: // a new segment arrived; re-evaluate (FIFO unchanged)
				t.Stop()
			case <-s.stop:
				t.Stop()
				return
			}
			continue
		}

		err := s.attempt(item)
		result := "ok"
		if err != nil {
			result = fmt.Sprintf("error: %v", err)
		}
		now := time.Now()
		s.mu.Lock()
		s.lastAttemptAt = now
		s.lastResult = result
		if err == nil {
			s.lastSuccessAt = now
		}
		// The queue-full drop in ShipSegment may have removed queue[0]
		// while we shipped; the seq guard keeps us from popping the
		// wrong item.
		if len(s.queue) > 0 && s.queue[0].seq == item.seq {
			if err == nil {
				s.queue = s.queue[1:]
			} else {
				item.attempts++
				if item.attempts >= MaxShipAttempts {
					s.queue = s.queue[1:]
					s.logger.Warn("audit ship: dropping segment after max attempts",
						"chain_head", item.chainHead,
						"attempts", item.attempts, "last_error", err)
				} else {
					item.nextTry = now.Add(s.backoffFor(item.attempts))
					s.queue[0] = item
				}
			}
		}
		depth := len(s.queue)
		s.mu.Unlock()
		s.writeState(now, result, depth)
	}
}

// backoffFor returns the delay before retry number attempt (1-based):
// base, 2×base, 4×base … capped at backoffMax.
func (s *Shipper) backoffFor(attempt int) time.Duration {
	d := s.backoffBase << (attempt - 1)
	if d <= 0 || d > s.backoffMax {
		return s.backoffMax
	}
	return d
}

// attempt performs one delivery of item. Errors are the caller's signal to
// retry; nothing here touches the audit log.
func (s *Shipper) attempt(item shipItem) error {
	if s.target.Scheme == "syslog" {
		return s.attemptSyslog(item)
	}
	return s.attemptHTTP(item)
}

// attemptHTTP POSTs the segment bytes as the request body
// (application/x-ndjson) with the chain head in X-Bunker-Chain-Head. Any 2xx
// counts as delivered.
func (s *Shipper) attemptHTTP(item shipItem) error {
	req, err := http.NewRequest(http.MethodPost, s.target.URL, bytes.NewReader(item.data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("X-Bunker-Chain-Head", item.chainHead)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) // drain for connection reuse
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("endpoint returned %s", resp.Status)
	}
	return nil
}

// attemptSyslog sends one RFC 3164 UDP datagram per audit record line,
// terminated by a SEAL datagram carrying the chain head (and the rotation
// seal when sealing is enabled). UDP syslog is a best-effort channel: lines
// are clipped to the 1024-byte datagram budget, so the HTTPS webhook is the
// lossless path.
func (s *Shipper) attemptSyslog(item shipItem) error {
	conn, err := net.DialTimeout("udp", s.target.Addr, shipDialTimeout)
	if err != nil {
		return fmt.Errorf("syslog dial %s: %w", s.target.Addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(shipDialTimeout))
	stamp := time.Now().Format(time.Stamp)
	for _, line := range bytes.Split(bytes.TrimRight(item.data, "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(conn, "<14>%s %s bunkerd-audit: %s\n",
			stamp, s.hostname, clipSyslogLine(line)); err != nil {
			return err
		}
	}
	sealPart := ""
	if item.seal != "" {
		sealPart = " seal=" + item.seal
	}
	_, err = fmt.Fprintf(conn, "<14>%s %s bunkerd-audit: SEAL chain_head=%s%s\n",
		stamp, s.hostname, item.chainHead, sealPart)
	return err
}

// clipSyslogLine keeps one record inside the RFC 3164 datagram budget.
func clipSyslogLine(line []byte) string {
	if len(line) > syslogLineBudget {
		return string(line[:syslogLineBudget])
	}
	return string(line)
}

// Stop terminates the worker goroutine and waits for it to fully exit, so
// after Stop returns there is no shipper goroutine left and no further
// filesystem writes (the shipstate file is never touched again). Idempotent
// and safe on a nil shipper (no shipper attached). An in-flight HTTP attempt
// finishes within its timeout first; queued segments are dropped — the
// daemon is exiting anyway.
func (s *Shipper) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		close(s.stop)
	})
	<-s.done
}

// recordResult records a ship attempt that happened outside the worker loop
// (segment read failures at enqueue) and refreshes the state file.
func (s *Shipper) recordResult(result string, depth int) {
	now := time.Now()
	s.mu.Lock()
	s.lastAttemptAt = now
	s.lastResult = result
	s.mu.Unlock()
	s.writeState(now, result, depth)
}

// writeState persists the post-attempt snapshot for `bunker audit status`.
// Best effort: a failed state write warns but never affects shipping.
//
// Two guarantees matter for shutdown determinism:
//   - Once Stop has set the stopped flag, the file is never touched again
//     (Stop's <-s.done makes this transitively true for its callers).
//   - The write is atomic (temp file in the same dir + rename), so a
//     concurrent reader (the `bunker audit status` CLI) never sees a torn
//     or partially written state file.
func (s *Shipper) writeState(now time.Time, result string, depth int) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	st := ShipState{
		ShipTo:      s.target.Redacted(),
		LastAttempt: now.UTC().Format(time.RFC3339),
		LastResult:  result,
		QueueDepth:  depth,
	}
	if !s.lastSuccessAt.IsZero() {
		st.LastSuccess = s.lastSuccessAt.UTC().Format(time.RFC3339)
	}
	path := s.auditPath + shipStateSuffix
	s.mu.Unlock()

	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := writeFileAtomic(path, b, 0o600); err != nil {
		s.logger.Warn("audit ship: writing ship state file failed", "error", err)
	}
}

// snapshot returns the status fields under the shipper lock.
func (s *Shipper) snapshot() (lastAttempt, lastSuccess time.Time, lastResult string, depth int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAttemptAt, s.lastSuccessAt, s.lastResult, len(s.queue)
}

// writeFileAtomic writes b to path via a temp file in the same directory +
// os.Rename, so readers never observe a torn or partially written file and a
// crash leaves either the old or the new content, never a mix. The rename
// keeps the write atomic on the same filesystem (same dir guarantees that).
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// computeSeal derives the rotation seal for a chain head:
// HMAC-SHA256(key=seal_key, msg=chain_head), hex-encoded. Used at rotation
// when audit.seal_key is set; the seal record and the syslog SEAL datagram
// both carry this value.
func computeSeal(key, chainHead string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(chainHead))
	return hex.EncodeToString(mac.Sum(nil))
}
