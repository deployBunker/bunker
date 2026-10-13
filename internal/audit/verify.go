package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// VerifyResult is the full verdict of one verification pass: the chain walk
// (records/FirstBad), plus the anchor state (GAP-136). It is what
// `bunker audit status` reports from, so a status report can never claim more
// than the verification it just performed.
type VerifyResult struct {
	// Records is the retained-chain record count (live file + backups .1-.3).
	Records int
	// FirstBad is the 1-based index of the first bad record, 0 when the chain
	// itself is clean. An anchor failure reports 0 here: the failure is not in
	// a record.
	FirstBad int
	// Anchor is the anchor that was read (nil when the log is unanchored).
	Anchor *Anchor
	// AnchorState is one of the AnchorState* constants.
	AnchorState string
	// AnchorNote explains anything weaker than a verified keyed anchor
	// (missing anchor file, unkeyed anchor, uncheckable MAC). Empty when the
	// anchor validated under its key.
	AnchorNote string
	// AnchorPath is the anchor file path that was consulted, so a report can
	// name what is missing rather than just asserting it is.
	AnchorPath string
}

// Verified reports whether the chain verified AND the anchor is as strong as
// this host can make it (keyed, with a validated MAC). It is the honest
// predicate `bunker audit status` gates its "healthy" claim on.
func (r *VerifyResult) Verified() bool {
	return r.FirstBad == 0 && r.AnchorState == AnchorStateKeyed
}

// Verify checks the integrity of the audit log at path together with its
// rotated backups (path.1 .. path.MaxBackups, oldest first) and returns the
// total number of records in the chain and the 1-based index of the first bad
// record (0 when every record is good). err is non-nil exactly when a bad
// record was found or the log could not be opened. On a bad record, firstBad
// is the record's 1-based index counted in chain order — oldest backup first,
// the live file last; for a single unrotated file this is simply the record's
// position in the file. records is always the full retained chain count (files
// rotated away beyond MaxBackups are gone and not counted), even when
// verification stops at the first bad record.
//
// A record is bad when:
//   - its line is not valid JSON (including empty lines), or
//   - its "hash" field is not the SHA-256 hex digest of its canonical line —
//     the line's bytes with the hash field empty, exactly the bytes Log
//     digested before inserting the hash — or
//   - its "prev_hash" does not chain to the previous record's hash.
//
// The chain spans rotations: a rotated log's first record legitimately
// carries a non-empty prev_hash pointing at the previous file's last record.
// The head link of the oldest retained file is allowed to point at a
// predecessor that was rotated away beyond MaxBackups — that link is
// unverifiable and accepted as-is (its other end no longer exists). A file
// whose predecessor existed (e.g. a truncated backup hole) fails the head
// check, which is what catches mid-chain backup tampering.
//
// GAP-136: Verify ALSO checks the anchor (path.anchor) when one exists, and
// fails on a tampered anchor or on an anchored head that has vanished from the
// chain (tail truncation — the case a bare hash chain cannot see). Use
// VerifyDetailed to obtain the anchor verdict alongside the verdict on the
// chain, and VerifyWithKey when the anchor key is not at the default location.
func Verify(path string) (records int, firstBad int, err error) {
	return VerifyWithKey(path, "")
}

// VerifyWithKey is Verify with an explicit anchor key (audit.seal_key), for a
// deployment whose key is configured rather than auto-provisioned next to the
// log.
func VerifyWithKey(path, anchorKey string) (records int, firstBad int, err error) {
	res, err := VerifyDetailed(path, anchorKey)
	if res == nil {
		return 0, 0, err
	}
	return res.Records, res.FirstBad, err
}

// VerifyDetailed runs the full verification: the chain walk, then the anchor
// check (GAP-136). The returned result is always non-nil; err is the integrity
// failure (chain break, tamper, truncation, damaged anchor), nil when the
// retained chain and its anchor are both acceptable. A weaker-than-keyed
// anchor is reported through the result, not as an error: an unavailable key
// must not make the verifier unusable.
func VerifyDetailed(path, anchorKey string) (*VerifyResult, error) {
	records, firstBad, err := verifyChain(path)
	res := &VerifyResult{
		Records:     records,
		FirstBad:    firstBad,
		AnchorState: AnchorStateNone,
		AnchorPath:  AnchorPath(path),
	}
	if err != nil {
		// The chain itself failed. Still report the anchor's PRESENCE and its
		// MAC state — a broken chain must not be described as "unanchored",
		// which would understate what is on disk. The truncation check is
		// skipped (its meaning assumes a consistent chain), and the chain error
		// remains the returned verdict.
		v, aerr := checkAnchor(path, anchorKey, nil)
		res.Anchor, res.AnchorState = v.Anchor, v.State
		res.AnchorNote = v.Note
		if aerr != nil {
			res.AnchorState = AnchorStateNone
			res.AnchorNote = "anchor unreadable or invalid: " + aerr.Error()
		}
		if res.AnchorNote != "" {
			res.AnchorNote += " (not checked: the chain failed verification)"
		}
		return res, err
	}
	v, aerr := checkAnchor(path, anchorKey, func(head string) (bool, error) {
		return chainContainsHash(path, head)
	})
	res.Anchor, res.AnchorState, res.AnchorNote = v.Anchor, v.State, v.Note
	return res, aerr
}

// verifyChain is the original record-chain walk: backups oldest first, then the
// live file. It knows nothing about anchors.
func verifyChain(path string) (records int, firstBad int, err error) {
	// Walk the chain from the oldest retained backup to the live file:
	// .3 -> .2 -> .1 -> path. Each file's records must be internally
	// consistent, and each file's head must chain to the next-older file's
	// tail whenever that file exists.
	var predecessorTail string // tail hash of the next-older file
	haveTail := false          // whether the next-older file exists (head check enforced)
	var n int                  // records verified so far across the chain
	for i := MaxBackups; i >= 1; i-- {
		p := fmt.Sprintf("%s.%d", path, i)
		info, statErr := os.Stat(p)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return n, 0, pathError(p, statErr)
		}
		if info.Size() == 0 {
			continue // empty backup contributes no records and breaks no links
		}
		cnt, bad, tail, err := verifyFile(p, predecessorTail, haveTail)
		if err != nil {
			// Verification stops here, but the record tally must still be
			// the full retained chain, so finish counting the newer files.
			rest, restErr := countRemaining(path, i-1)
			if restErr != nil {
				return n + cnt, n + bad, err // original verification error wins
			}
			return n + cnt + rest, n + bad, err
		}
		n += cnt
		predecessorTail = tail
		haveTail = true
	}

	info, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return 0, 0, fmt.Errorf("audit log %s: %w", path, os.ErrNotExist)
		}
		return 0, 0, pathError(path, statErr)
	}
	if info.Size() > 0 {
		cnt, bad, _, err := verifyFile(path, predecessorTail, haveTail)
		if err != nil {
			return n + cnt, n + bad, err
		}
		n += cnt
	}
	return n, 0, nil
}

// countRemaining counts records in backups path.(from) .. path.1 and in the
// live file — used to complete the record tally after verification stopped
// early at a bad record. Missing files count zero.
func countRemaining(path string, from int) (int, error) {
	total := 0
	for i := from; i >= 1; i-- {
		n, err := countRecords(fmt.Sprintf("%s.%d", path, i))
		if err != nil && !os.IsNotExist(err) {
			return total, err
		}
		if err == nil {
			total += n
		}
	}
	n, err := countRecords(path)
	if err != nil {
		return total, err
	}
	return total + n, nil
}

// countRecords returns the number of lines in a file (records or garbage —
// every line is part of the tally).
func countRecords(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	n := 0
	for sc.Scan() {
		n++
	}
	return n, sc.Err()
}

// chainContainsHash reports whether the retained chain at path declares a
// record whose "hash" is want — the GAP-136 truncation check: an anchor names
// the head of the chain at the moment it was taken, so a head that is no
// longer present anywhere in the retained chain means records were removed.
//
// The live file is scanned first because the anchor normally names its last
// record (the anchor is refreshed per record); the rotated backups follow. The
// declaration is enough here: the caller has already walked the chain and
// re-digested every declared hash, so a declared hash that matches is a record
// that exists.
func chainContainsHash(path, want string) (bool, error) {
	if want == "" {
		return false, nil
	}
	files := make([]string, 0, MaxBackups+1)
	files = append(files, path)
	for i := 1; i <= MaxBackups; i++ {
		files = append(files, fmt.Sprintf("%s.%d", path, i))
	}
	for _, p := range files {
		ok, err := fileContainsHash(p, want)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// fileContainsHash streams one audit file looking for a declared hash. A
// missing file is not an error (it is simply not a place the record can be).
func fileContainsHash(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, pathError(path, err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue // unparseable lines are the walk's verdict, not membership's
		}
		if rec.Hash == want {
			return true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return false, nil
}

// verifyFile verifies one audit file's records. The first record's prev_hash
// must equal head — the hash of the previous file's last record — whenever
// enforceHead is set (the predecessor file exists); otherwise the head link
// is unverifiable and accepted, and every subsequent record's prev_hash must
// equal the previous record's hash. Returns the full record count, the
// 1-based index of the first bad record within this file (0 = clean), and the
// hash of the last record ("" when the file has no records).
func verifyFile(path, head string, enforceHead bool) (records, firstBad int, tail string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, "", pathError(path, err)
	}
	defer f.Close()

	prevHash := head // hash the next record must chain to
	var firstErr error
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		records++
		line := sc.Bytes()
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			if firstBad == 0 {
				firstBad = records
				firstErr = fmt.Errorf("%s: record %d is not valid JSON: %w", path, records, err)
			}
			continue
		}
		// Own hash: re-digest the canonical line (hash field emptied) and
		// compare against the recorded value. Re-marshalling the parsed
		// Record reproduces the writer's canonical bytes exactly: same
		// struct, same field order, same values (all fields round-trip).
		declared := rec.Hash
		rec.Hash = ""
		canonical, err := json.Marshal(rec)
		if err != nil {
			if firstBad == 0 {
				firstBad = records
				firstErr = fmt.Errorf("%s: record %d: re-marshal: %w", path, records, err)
			}
			continue
		}
		sum := sha256.Sum256(canonical)
		if hex.EncodeToString(sum[:]) != declared {
			if firstBad == 0 {
				firstBad = records
				firstErr = fmt.Errorf("%s: record %d: hash mismatch (tampered)", path, records)
			}
			continue
		}
		// Chain link: the first record must chain to the predecessor file's
		// tail (when one exists); every later record to the previous one.
		if (records == 1 && enforceHead && rec.PrevHash != head) ||
			(records > 1 && rec.PrevHash != prevHash) {
			if firstBad == 0 {
				firstBad = records
				firstErr = fmt.Errorf("%s: record %d: prev_hash does not chain (tampered)", path, records)
			}
			continue
		}
		prevHash = declared
	}
	if err := sc.Err(); err != nil {
		return records, 0, "", fmt.Errorf("read %s: %w", path, err)
	}
	if firstBad > 0 {
		return records, firstBad, prevHash, firstErr
	}
	return records, 0, prevHash, nil
}
