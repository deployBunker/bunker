// GAP-073: local on-disk status for `bunker audit status`. Reads the log
// tree (live file + rotated backups) and the daemon-written ship-state file;
// never talks to the daemon.
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
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

// LocalStatus inspects the audit log at path (plus backups and ship-state)
// and returns the status view for the CLI. A missing live file is NOT an
// error: it reports Enabled=false — the command must work on an
// unconfigured host too.
func LocalStatus(path string) (*StatusReport, error) {
	st := &StatusReport{BackupSizes: make([]int64, MaxBackups)}
	for i := range st.BackupSizes {
		st.BackupSizes[i] = -1
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil // disabled/unconfigured: enabled=false
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

	// Live file: records, newest seal probe, and the current chain head
	// (hash of the last record). verifyFile re-digests every line in order;
	// its returned tail IS the head of the live chain when the file is
	// clean. A tampered/unparseable live file still reports: the head is
	// the last parseable record's declared hash ("" when none), and the
	// operator runs `bunker audit verify` for the verdict.
	n, _, seal, tail, err := inspectLive(path)
	if err != nil {
		return nil, err
	}
	st.Records += n
	st.ChainHead = tail
	st.LastSeal = seal
	if seal != nil {
		st.SealingEnabled = true
	}

	// Ship state written by the daemon's shipper (may not exist).
	if ship, err := ReadShipState(path); err == nil {
		st.Shipping = ship
	}
	return st, nil
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
