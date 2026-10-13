// GAP-136 / REQ-A2: ANCHORED tamper-evidence, armed by default.
//
// The record chain (audit.go) makes every record's bytes tamper-evident, but a
// plain SHA-256 chain is UNKEYED and UNANCHORED, which is exactly the SEC-09
// finding: truncation and forgery both verify OK. Two gaps:
//
//   - truncation: removing trailing records leaves a shorter chain that still
//     verifies from genesis, because nothing outside the file says how far the
//     chain had got;
//   - forgery: anyone who can write the log can also recompute every hash and
//     prev_hash, producing a chain that verifies perfectly.
//
// This file closes both, ON BY DEFAULT — no config flag:
//
//   - after every record is durably appended, the writer refreshes an anchor
//     record next to the log (<audit-path>.anchor) naming the current chain
//     head and when it was taken;
//   - the anchor is authenticated with HMAC-SHA256 under a key only root can
//     read. The key is audit.seal_key when that is configured; otherwise it is
//     auto-provisioned at <audit-path>.sealkey (mode 0600, 32 random bytes) on
//     first use. That auto-provisioning is what "on by default" means here:
//     nothing has to be enabled for anchoring to happen;
//   - Verify requires the anchor's head to still be PRESENT in the retained
//     chain, so removing trailing records now fails; and requires the anchor's
//     MAC to validate, so a rewritten chain cannot silently re-anchor itself
//     without the key.
//
// THE CLAIM IS CONDITIONAL (REQ-A2). The key and the anchor live on the same
// host as the log, so an attacker with root on that host can rewrite all
// three. What the local anchor buys is detection of changes made by anything
// that can write the log but cannot read the key — a compromised agent, a
// rogue component, a buggy writer, a rolled-back file. Only an OFF-BOX anchor
// (audit.ship_to, shipped segments held elsewhere) survives root on this host,
// which is why `bunker audit status` says plainly when no off-host anchor is
// configured instead of claiming "healthy".
//
// Losing the key is not fatal and not silent: the writer falls back to an
// unkeyed anchor (truncation-detectable, forgery not) with a warning, and
// every verifier reports the weaker state as its own verdict — unkeyed or
// unverified — rather than as a pass.
package audit

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// AnchorSuffix is appended to the audit log path for the anchor file the
	// writer refreshes after every record.
	AnchorSuffix = ".anchor"
	// AnchorKeySuffix is appended to the audit log path for the
	// auto-provisioned anchor key (mode 0600). audit.seal_key overrides it.
	AnchorKeySuffix = ".sealkey"
	// AnchorKeyBytes is the auto-provisioned key length (256 bits).
	AnchorKeyBytes = 32
	// AnchorVersion is the on-disk anchor format version.
	AnchorVersion = 1
)

// Anchor is the on-disk anchor record: the chain head observed at TS, MAC'd
// with the anchor key when one was available.
//
// Head is the hash of the last record of the LIVE file at anchor time. Verify
// requires that hash to still be present somewhere in the retained chain, so
// an anchor whose head has vanished is proof that records were removed.
type Anchor struct {
	// Version is the anchor format version (AnchorVersion).
	Version int `json:"version"`
	// Head is the chain head (hash of the last live record) at TS.
	Head string `json:"head"`
	// TS is when the anchor was taken (RFC3339Nano UTC).
	TS string `json:"ts"`
	// KeyID identifies the key that produced MAC, so a verifier can tell "the
	// key I have has signed this anchor" from "some other key" without seeing
	// the key. Empty on an unkeyed anchor.
	KeyID string `json:"key_id,omitempty"`
	// MAC is HMAC-SHA256(key, canonical anchor bytes) hex, or "" when the
	// anchor was written without a key.
	MAC string `json:"mac,omitempty"`
}

// Anchor states reported by a verification (and by `bunker audit status`).
const (
	// AnchorStateNone: no anchor file. The chain is checked, but nothing
	// outside it says how far it had got: tail truncation is undetectable.
	AnchorStateNone = "none"
	// AnchorStateKeyed: anchor present and its MAC validated against the key.
	AnchorStateKeyed = "keyed"
	// AnchorStateUnkeyed: anchor present with no MAC (no key was available when
	// it was written) — truncation-detectable, forgery not.
	AnchorStateUnkeyed = "unkeyed"
	// AnchorStateUnverified: the anchor declares a MAC but the matching key is
	// not available, so the MAC could not be checked. The truncation check
	// still ran; the MAC check did not.
	AnchorStateUnverified = "unverified"
)

// AnchorPath returns the anchor file path for the audit log at auditPath.
func AnchorPath(auditPath string) string { return auditPath + AnchorSuffix }

// AnchorKeyPath returns the auto-provisioned anchor key path for the log at
// auditPath.
func AnchorKeyPath(auditPath string) string { return auditPath + AnchorKeySuffix }

// keyID is the public identifier of an anchor key: the first 8 bytes of its
// SHA-256, hex. It names a key without revealing it.
func keyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// anchorMAC returns HMAC-SHA256(key, canonical anchor bytes) hex. The MAC
// field is empty while hashing, so the digest cannot depend on its own value —
// the same convention the record chain uses for its own hash.
func anchorMAC(key string, a Anchor) (string, error) {
	a.MAC = ""
	canonical, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("marshal anchor: %w", err)
	}
	m := hmac.New(sha256.New, []byte(key))
	if _, err := m.Write(canonical); err != nil {
		return "", fmt.Errorf("hmac anchor: %w", err)
	}
	return hex.EncodeToString(m.Sum(nil)), nil
}

// ReadAnchor loads the anchor file for the audit log at auditPath.
//
// A MISSING anchor file is not an error: it returns (nil, nil) and the caller
// reports "unanchored" rather than failing — logs written before GAP-136, and
// trees copied without their anchor, must still be verifiable.
//
// A PRESENT but unusable anchor IS an error: the file exists and cannot be
// trusted (not JSON, unknown version, no head). Reporting that as "unanchored"
// would let a damaged anchor read as a pass.
func ReadAnchor(auditPath string) (*Anchor, error) {
	p := AnchorPath(auditPath)
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, pathError(p, err)
	}
	var a Anchor
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("%s: anchor is not valid JSON (tampered or truncated): %w", p, err)
	}
	if a.Version != AnchorVersion {
		return nil, fmt.Errorf("%s: anchor version %d is not supported (want %d)", p, a.Version, AnchorVersion)
	}
	if a.Head == "" {
		return nil, fmt.Errorf("%s: anchor carries no head hash", p)
	}
	return &a, nil
}

// writeAnchorFile publishes one anchor atomically: a temp file in the same
// directory, then rename. A reader therefore sees either the previous anchor
// or the new one, never a half-written file — a torn anchor would read as
// tampering and turn a healthy host into a false alarm.
func writeAnchorFile(auditPath string, a Anchor) error {
	b, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("marshal anchor: %w", err)
	}
	b = append(b, '\n')
	target := AnchorPath(auditPath)
	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return pathError(tmp, err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish anchor %s: %w", target, err)
	}
	return nil
}

// resolveAnchorKey returns the key the WRITER authenticates anchors with,
// provisioning one when necessary.
//
// Precedence: explicit (audit.seal_key) wins; otherwise the key file
// <audit-path>.sealkey is read, and created with 32 random bytes at mode 0600
// when it does not exist. No config flag is involved in either path — a log
// opened with plain New is anchored and keyed.
//
// A failure to obtain a key is NOT fatal: callers warn and write unkeyed
// anchors. An audit trail must keep flowing on a host where the key file
// cannot be created; the degradation is reported by every verifier as the
// unkeyed state rather than hidden.
func resolveAnchorKey(auditPath, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	p := AnchorKeyPath(auditPath)
	if b, err := os.ReadFile(p); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
	} else if !os.IsNotExist(err) {
		return "", pathError(p, err)
	}

	buf := make([]byte, AnchorKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate anchor key: %w", err)
	}
	k := hex.EncodeToString(buf)
	// O_EXCL makes the create race-safe: if two processes open the same log,
	// one wins the create and the loser reads the winner's key. Without it the
	// two would sign anchors with different keys and each call the other's
	// anchor tampered.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			if b, rerr := os.ReadFile(p); rerr == nil {
				if k2 := strings.TrimSpace(string(b)); k2 != "" {
					return k2, nil
				}
			}
		}
		return "", pathError(p, err)
	}
	if _, err := f.WriteString(k + "\n"); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write anchor key %s: %w", p, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close anchor key %s: %w", p, err)
	}
	return k, nil
}

// loadAnchorKey returns the key a VERIFIER can check anchors with: the
// explicitly supplied key when non-empty, else the auto-provisioned key file.
// It never creates a key.
//
// An empty return with a nil error means "no key available" — the verifier
// reports the anchor as unverified instead of guessing. (Anchors written with
// audit.seal_key have no key file next to the log by design: the seal key is
// not copied to disk. Callers that know the configured key pass it in.)
func loadAnchorKey(auditPath, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	p := AnchorKeyPath(auditPath)
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", pathError(p, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// AnchorVerdict is the anchor half of a verification: what was found, how
// strongly it could be checked, and (for anything weaker than keyed) why.
type AnchorVerdict struct {
	// Anchor is the anchor that was read (nil when absent).
	Anchor *Anchor
	// State is one of the AnchorState* values.
	State string
	// Note explains a non-fatal weaker state (missing or uncheckable key, no
	// anchor file). Empty for a keyed verdict.
	Note string
}

// checkAnchor validates the anchor of the log at auditPath.
//
// contains reports whether the anchored head is still present in the retained
// chain — that is the truncation check, and it needs no key. explicitKey, when
// non-empty, is the configured anchor key (audit.seal_key).
//
// It returns a non-nil error ONLY for a real integrity failure: a malformed
// anchor, an anchor MAC that does not validate, or an anchored head that has
// vanished from the chain. A missing anchor file and an uncheckable MAC are
// reported through the verdict, never as a failure — a verifier that fails
// closed on a missing key would be a verifier nobody can run.
func checkAnchor(auditPath, explicitKey string, contains func(head string) (bool, error)) (AnchorVerdict, error) {
	a, err := ReadAnchor(auditPath)
	if err != nil {
		return AnchorVerdict{State: AnchorStateNone}, err
	}
	if a == nil {
		note := "no anchor file (" + AnchorPath(auditPath) + ")"
		if _, kerr := os.Stat(AnchorKeyPath(auditPath)); kerr == nil {
			// A key without an anchor is the signature of a DELETED anchor:
			// the writer provisions the key at open and writes an anchor with
			// its first record, so a keyed tree with records and no anchor has
			// had its anchor removed. Named, not failed — the chain walk above
			// is still the verdict on the records themselves.
			note += "; an anchor key exists at " + AnchorKeyPath(auditPath) + " — an anchor was expected, so the anchor file was removed or never written"
		}
		note += "; the chain is unanchored and a truncated tail verifies"
		return AnchorVerdict{State: AnchorStateNone, Note: note}, nil
	}
	v := AnchorVerdict{Anchor: a}

	switch a.MAC {
	case "":
		v.State = AnchorStateUnkeyed
		v.Note = "anchor carries no MAC (written without a key)"
	default:
		key, kerr := loadAnchorKey(auditPath, explicitKey)
		if kerr != nil {
			return v, kerr
		}
		switch {
		case key == "":
			v.State = AnchorStateUnverified
			v.Note = fmt.Sprintf("anchor is keyed (key id %s) but no key is available at %s — MAC not checked",
				a.KeyID, AnchorKeyPath(auditPath))
		case a.KeyID != "" && keyID(key) != a.KeyID:
			v.State = AnchorStateUnverified
			v.Note = fmt.Sprintf("anchor key id %s does not match the available key id %s — MAC not checked",
				a.KeyID, keyID(key))
		default:
			want, merr := anchorMAC(key, *a)
			if merr != nil {
				return v, merr
			}
			if !hmac.Equal([]byte(want), []byte(a.MAC)) {
				return v, fmt.Errorf("%s: anchor MAC mismatch (anchor tampered: head %s at %s)",
					AnchorPath(auditPath), a.Head, a.TS)
			}
			v.State = AnchorStateKeyed
		}
	}

	// Truncation check — independent of the key, so it still runs when the MAC
	// cannot be checked. The anchored head must still be in the chain: if it is
	// not, records that existed when the anchor was taken are gone.
	//
	// contains == nil means the caller has no consistent chain to search (its
	// walk already failed): the anchor's presence and MAC are still reported,
	// and only this check is skipped.
	if contains == nil {
		return v, nil
	}
	ok, cerr := contains(a.Head)
	if cerr != nil {
		return v, cerr
	}
	if !ok {
		return v, fmt.Errorf("%s: chain truncated: anchored head %s is not present in the retained chain",
			AnchorPath(auditPath), a.Head)
	}
	return v, nil
}

// newAnchor builds the anchor the writer publishes for a chain head, keyed when
// a key is available.
func newAnchor(key, head string) (Anchor, error) {
	a := Anchor{
		Version: AnchorVersion,
		Head:    head,
		TS:      time.Now().UTC().Format(time.RFC3339Nano),
	}
	if key == "" {
		return a, nil
	}
	a.KeyID = keyID(key)
	mac, err := anchorMAC(key, a)
	if err != nil {
		return Anchor{}, err
	}
	a.MAC = mac
	return a, nil
}
