// GAP-073: local on-disk status for `bunker audit status`. Reads the log
// tree (live file + rotated backups), the anchor, and the daemon-written
// ship-state file; never talks to the daemon.
//
// GAP-136 / REQ-A2 made this report SELF-CRITICAL. Before this change it
// printed "last verify: n/a" immediately after a successful `bunker audit
// verify` and said nothing about anchoring, so a truncated chain looked
// exactly like a healthy one (SEC-16). It now:
//
//   - verifies the retained chain on the very pass that produces the report
//     and prints that verdict (chain status + gaps);
//   - reports the local anchor state (keyed / unkeyed / unverified / none)
//     and the off-host anchor state, saying "no off-host anchor configured"
//     in plain words when there is none;
//   - derives a one-word health verdict that can only be "healthy" when the
//     chain verified, the local anchor's MAC validated, AND an off-host anchor
//     exists. Anything weaker is reported as degraded or broken — never as
//     healthy.
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Health verdicts reported by StatusReport.Health.
const (
	// HealthDisabled: no audit trail on this host (no live log file).
	HealthDisabled = "disabled"
	// HealthBroken: the chain failed verification — a record was edited, a
	// link was broken, the tail was truncated, or the anchor is damaged. Do
	// not trust this trail.
	HealthBroken = "broken"
	// HealthDegraded: the chain verified, but the strongest claim does not
	// hold here — normally because no off-host anchor is configured, so a
	// rewrite by anything with root on this host would go undetected.
	HealthDegraded = "degraded"
	// HealthHealthy: the chain verified, the local anchor's MAC validated
	// under its key, and an off-host anchor is configured.
	HealthHealthy = "healthy"
)

// StatusReport is the on-disk audit state reported by `bunker audit status`.
// Produced by LocalStatus(path); Shipping is nil when no ship-state file
// exists (shipping off, or the daemon has never shipped anything).
type StatusReport struct {
	// Enabled mirrors the live file's existence: audit.New creates the file,
	// so a missing live file means the audit trail is disabled or never
	// configured on this host.
	Enabled bool
	// ChainHead is the hash of the LAST record of the live file ("" when the
	// live file is empty).
	ChainHead string
	// Records is the total retained-chain record count (live + backups).
	Records int
	// LiveSize is the live file's size in bytes.
	LiveSize int64
	// BackupSizes holds the sizes of path.1 .. path.MaxBackups; -1 marks a
	// backup that does not exist. len == MaxBackups, index 0 = .1.
	BackupSizes []int64
	// RotationsLowerBound is len(BackupSizes>0) — the number of rotated
	// backups present is a LOWER bound on rotations (files rotated beyond
	// the backup budget no longer exist).
	RotationsLowerBound int
	// Shipping is the daemon's last ship attempt (nil = no state file).
	Shipping *ShipState
	// SealingEnabled reports whether any retained record is a rotation seal
	// (Method=/internal/audit/seal). Note: a daemon configured with
	// audit.seal_key that has not rotated yet shows false — there is nothing
	// on disk to prove sealing from.
	SealingEnabled bool
	// LastSeal is the NEWEST seal record found in the retained chain
	// (nil when none): the sealed chain head plus the HMAC seal, so an
	// operator can re-derive/compare against shipped copies.
	LastSeal *LocalSealProbe

	// ── GAP-136: anchoring, chain integrity, honest health ────────────────

	// StoragePath is the live log path this report describes — the storage
	// location an operator needs before trusting, copying or filing anything.
	StoragePath string
	// ChainLength is the number of records in the retained chain (live file
	// plus backups .1-.3), counted on this pass. Same value as Records; named
	// after the status contract.
	ChainLength int
	// LastAnchorHash is the hash of the last record of the live file — the
	// hash a refreshed anchor names (== ChainHead). "" when the live file is
	// empty.
	LastAnchorHash string
	// ChainOK reports whether the retained chain verified on THIS pass. It is
	// the result of a real walk, not a remembered verdict.
	ChainOK bool
	// FirstBadRecord is the 1-based index of the first bad record (0 = none,
	// and the value on an anchor-level failure, which is not record-scoped).
	FirstBadRecord int
	// ChainError is the verification failure, "" when ChainOK. A non-empty
	// value here means the trail is NOT trustworthy.
	ChainError string
	// Gaps lists the chain breaks/gaps found on this pass (empty when ChainOK):
	// an edited record, a broken link, a truncated tail, or a damaged anchor.
	Gaps []string
	// Anchor describes the local anchor and the off-host anchor state.
	Anchor *AnchorStatus
	// Health is the honest one-word verdict (HealthHealthy/HealthDegraded/
	// HealthBroken/HealthDisabled); HealthReason is the sentence behind it.
	Health       string
	HealthReason string
}

// AnchorStatus is the anchoring half of a status report.
type AnchorStatus struct {
	// Present reports whether an anchor file exists on disk.
	Present bool
	// State is one of the AnchorState* constants: how strongly the anchor
	// could actually be checked.
	State string
	// Head is the chain head the anchor names ("" when no anchor file).
	Head string
	// TS is when the anchor was taken ("" when no anchor file).
	TS string
	// KeyID identifies the key that authenticated the anchor ("" when unkeyed
	// or absent). The key itself is never reported.
	KeyID string
	// MACValid is true ONLY when the anchor's MAC was actually validated — an
	// unchecked MAC is not a valid one.
	MACValid bool
	// Note explains anything weaker than a verified keyed anchor: a missing
	// anchor file, an unkeyed anchor, or a MAC that could not be checked.
	Note string
	// OffHost reports whether an off-host anchor (audit.ship_to) is configured
	// for this log. This is the only anchor that survives root on this host.
	OffHost bool
	// OffHostTarget is the redacted ship endpoint when OffHost.
	OffHostTarget string
	// OffHostLastSuccess is the last successful shipment timestamp ("" when
	// nothing has shipped successfully yet).
	OffHostLastSuccess string
	// Statement is the plain-language anchor summary, including the exact
	// words "no off-host anchor configured" when that is the truth.
	Statement string
}

// LocalSealProbe describes the newest rotation seal record on disk.
type LocalSealProbe struct {
	// SealedHead is the final chain head of the segment the seal binds (the
	// seal record's prev_hash — the value the HMAC was computed over).
	SealedHead string
	// Seal is the HMAC-SHA256(key=seal_key, msg=SealedHead) hex.
	Seal string
	// TS is the seal record's timestamp.
	TS string
	// Hash is the seal record's own chain hash.
	Hash string
}

// StatusOptions carries the facts about a deployment that the log tree alone
// cannot show:
//
//   - AnchorKey: the daemon's audit.seal_key. An explicitly configured key is
//     deliberately NOT copied next to the log (the seal key is a secret), so a
//     verifier that knows it must be told; without it the anchor is reported as
//     keyed-but-unverified rather than silently assumed good.
//   - ShipTo: the daemon's audit.ship_to. A configured off-host anchor that has
//     not shipped anything yet leaves no ship-state file behind, and "no
//     off-host anchor configured" would then be a false statement.
//
// Both are optional. With the zero value the report is derived from the log
// tree alone, which is the state of a host using only the built-in defaults.
type StatusOptions struct {
	AnchorKey string
	ShipTo    string
}

// LocalStatus inspects the audit log at path (plus backups, anchor and
// ship-state) and returns the status view for the CLI. A missing live file is
// NOT an error: it reports Enabled=false — the command must work on an
// unconfigured host too.
func LocalStatus(path string) (*StatusReport, error) {
	return LocalStatusWith(path, StatusOptions{})
}

// LocalStatusWith is LocalStatus with the deployment facts above.
func LocalStatusWith(path string, opts StatusOptions) (*StatusReport, error) {
	st := &StatusReport{StoragePath: path, BackupSizes: make([]int64, MaxBackups)}
	for i := range st.BackupSizes {
		st.BackupSizes[i] = -1
	}

	// Ship state first: it is part of the honest anchoring report and it is
	// relevant even when the local log is missing or unreadable.
	ship, _ := ReadShipState(path)
	st.Shipping = ship

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			st.Anchor = buildAnchorStatus(nil, AnchorStateNone, "", ship, opts.ShipTo)
			st.Health, st.HealthReason = HealthDisabled, "no audit log at "+path+"; nothing is anchored on this host"
			return st, nil
		}
		return nil, pathError(path, err)
	}
	st.Enabled = true
	st.LiveSize = info.Size()

	// Backups: oldest first, tallying records and present-file count.
	for i := MaxBackups; i >= 1; i-- {
		p := fmt.Sprintf("%s.%d", path, i)
		bi, serr := os.Stat(p)
		if serr != nil {
			if os.IsNotExist(serr) {
				continue
			}
			return nil, pathError(p, serr)
		}
		st.BackupSizes[i-1] = bi.Size()
		st.RotationsLowerBound++
		n, err := countRecords(p)
		if err != nil {
			return nil, err
		}
		st.Records += n
	}

	// Live file: record count, newest seal probe, and the current chain head
	// (hash of the last record).
	n, _, seal, tail, err := inspectLive(path)
	if err != nil {
		return nil, err
	}
	st.Records += n
	st.ChainLength = st.Records
	st.ChainHead = tail
	st.LastAnchorHash = tail
	st.LastSeal = seal
	if seal != nil {
		st.SealingEnabled = true
	}

	// GAP-136: verify the chain (and its anchor) NOW. The report carries the
	// verdict of a real walk rather than pointing at `bunker audit verify`.
	res, verr := VerifyDetailed(path, opts.AnchorKey)
	st.ChainOK = verr == nil
	st.FirstBadRecord = res.FirstBad
	if verr != nil {
		st.ChainError = verr.Error()
		st.Gaps = []string{verr.Error()}
	}
	st.Anchor = buildAnchorStatus(res.Anchor, res.AnchorState, res.AnchorNote, ship, opts.ShipTo)
	st.Health, st.HealthReason = deriveHealth(st)
	return st, nil
}

// AnchorStatement is the plain-language off-host anchor summary for the
// report, including the exact words "no off-host anchor configured" when that
// is the truth. It exists so the CLI cannot forget to say it.
func (st *StatusReport) AnchorStatement() string {
	if st.Anchor == nil {
		return "unknown — no anchor state was collected"
	}
	if st.Anchor.Statement == "" {
		return "no off-host anchor configured"
	}
	return st.Anchor.Statement
}

// buildAnchorStatus assembles the anchoring half of the report. ship is the
// daemon-written ship-state file (nil when there is none); configuredShipTo is
// the daemon config's audit.ship_to ("" when unknown), which is what makes the
// off-host statement true before anything has actually shipped.
func buildAnchorStatus(a *Anchor, state, note string, ship *ShipState, configuredShipTo string) *AnchorStatus {
	as := &AnchorStatus{State: state, Note: note}
	if a != nil {
		as.Present = true
		as.Head, as.TS, as.KeyID = a.Head, a.TS, a.KeyID
		as.MACValid = state == AnchorStateKeyed
	}

	// Off-host anchor: the ship-state file is the evidence that shipping ran;
	// the config value is the evidence that it is configured. Either one makes
	// the claim true; neither means it is false, and we say so.
	switch {
	case ship != nil && ship.ShipTo != "":
		as.OffHost = true
		as.OffHostTarget = ship.ShipTo
		as.OffHostLastSuccess = ship.LastSuccess
	case configuredShipTo != "":
		as.OffHost = true
		as.OffHostTarget = RedactShipTo(configuredShipTo)
	}

	if !as.OffHost {
		as.Statement = "no off-host anchor configured"
		return as
	}
	as.Statement = "off-host anchor configured (" + as.OffHostTarget + ")"
	if as.OffHostLastSuccess == "" {
		as.Statement += ", nothing shipped successfully yet"
	} else {
		as.Statement += ", last shipped " + as.OffHostLastSuccess
	}
	return as
}

// deriveHealth turns the verified facts into the one-word verdict and the
// sentence behind it. "healthy" is reachable ONLY with a verified chain, a
// validated anchor MAC and an off-host anchor: anything weaker says so.
func deriveHealth(st *StatusReport) (string, string) {
	if !st.Enabled {
		return HealthDisabled, "no audit log at " + st.StoragePath + "; nothing is anchored on this host"
	}
	if !st.ChainOK {
		return HealthBroken, "chain integrity failure: " + st.ChainError
	}
	a := st.Anchor
	if a == nil || !a.OffHost {
		return HealthDegraded, "chain verified and locally anchored, but no off-host anchor configured"
	}
	switch a.State {
	case AnchorStateKeyed:
		return HealthHealthy, "chain verified, anchor MAC validated, off-host anchor configured"
	case AnchorStateUnkeyed:
		return HealthDegraded, "off-host anchor configured, but the local anchor is unkeyed"
	case AnchorStateUnverified:
		return HealthDegraded, "off-host anchor configured, but the local anchor MAC could not be checked"
	default:
		return HealthDegraded, "off-host anchor configured, but no local anchor file is present"
	}
}

// inspectLive scans the live file once: record count, the newest seal
// record, and the declared hash of the LAST parseable record. It deliberately
// does not enforce the chain (that is Verify's job).
func inspectLive(path string) (records, firstBad int, seal *LocalSealProbe, tail string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, nil, "", pathError(path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		records++
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue // unparseable line: not the status command's verdict
		}
		if rec.Method == SealMethod && rec.Seal != "" {
			seal = &LocalSealProbe{
				SealedHead: rec.PrevHash,
				Seal:       rec.Seal,
				TS:         rec.TS,
				Hash:       rec.Hash,
			}
		}
		tail = rec.Hash
	}
	if err := sc.Err(); err != nil {
		return records, 0, seal, tail, fmt.Errorf("read %s: %w", path, err)
	}
	return records, firstBad, seal, tail, nil
}
