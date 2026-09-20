package audit

import "testing"

// Unit tests for the DF-BUNKER-29 recovery helper: chain head of the live
// file, across backups, on an empty file, and its refusal on a damaged tail.
func TestAuditChainHead(t *testing.T) {
	t.Run("fresh log", func(t *testing.T) {
		l1, path := newTestLog(t)
		writeRecords(t, l1, 3)
		want := lastRecord(t, path)["hash"].(string)
		got, err := auditChainHead(path)
		if err != nil || got != want {
			t.Errorf("auditChainHead = (%q, %v), want (%q, nil)", got, err, want)
		}
	})

	t.Run("across rotation", func(t *testing.T) {
		l1, path := newTestLog(t)
		l1.rotateAt = 128
		writeRecords(t, l1, 3) // .1 = [rec-1, rec-2], live = [rec-3]
		want := lastRecord(t, path)["hash"].(string)
		got, err := auditChainHead(path)
		if err != nil || got != want {
			t.Errorf("auditChainHead = (%q, %v), want live tail %q", got, err, want)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		_, path := newTestLog(t) // created, zero bytes
		got, err := auditChainHead(path)
		if err != nil || got != "" {
			t.Errorf("auditChainHead = (%q, %v), want (\"\", nil)", got, err)
		}
	})

	t.Run("damaged tail refused", func(t *testing.T) {
		l1, path := newTestLog(t)
		writeRecords(t, l1, 2)
		tamperSummary(t, path, 2)
		if _, err := auditChainHead(path); err == nil {
			t.Error("auditChainHead on damaged tail returned nil error")
		}
	})
}
