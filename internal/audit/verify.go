package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

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
func Verify(path string) (records int, firstBad int, err error) {
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
			return n, 0, fmt.Errorf("stat %s: %w", p, statErr)
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
		return 0, 0, fmt.Errorf("stat %s: %w", path, statErr)
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
		return 0, 0, "", fmt.Errorf("open %s: %w", path, err)
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
