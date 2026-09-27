package webdav

import (
	"strings"
	"time"

	"github.com/deployBunker/bunker/internal/invalidation"
)

// BFS-043: the runtime read-back of the server-side invalidation config surface.
//
// The rule this file exists to obey: THE REPORTED VALUE IS THE VALUE THE RUNNING
// SYSTEM OBEYS, not the value that was requested. So the rows below are filled
// from the RUNNING WATCHER's own options whenever a watcher exists — the same
// struct fields the flush, the stall detector and the install walk read — and
// only fall back to the resolved configuration (marked applied:false, with the
// reason) when nothing is running to obey them. A config surface that reports
// values it does not obey is the defect this row is about.

// pushNotServedReason is why the push knobs are reported as applied:false: this
// build has no push endpoint (BFS-036 owns the wire form), so the values are
// validated and declared and nothing consumes them yet. Saying so is the honest
// answer; reporting them as in force would be a claim about a channel that does
// not exist.
const pushNotServedReason = "the push form is not served in this build (BFS-036 owns the wire endpoint), so this value is declared and validated but nothing applies it yet"

// maxEventBytesAppliedReason is why ONE push knob is APPLIED while its siblings
// are not. `max_event_bytes` is not only a promise about a wire form that does
// not exist yet: it is the bound the EVENT FRAME the `events` poll form
// assembles is measured against today (events.go's ledger push, BFS-062), and
// the number the capability document publishes. A read-back that reported a
// value the code is enforcing as applied:false would be the same class of
// dishonesty as a counter that can never move.
const maxEventBytesAppliedReason = "applied by the event frames the `events` poll form assembles (events.go's ledger push measures each serialized frame against it, BFS-062) and published as the declared bound; the push wire form will obey the same value when BFS-036 lands it"

// invalidationConfigBlock renders the read-back for §8.2's `extensions.watch`
// block: one row per declared knob, the ceiling in force, the unhonourable pairs,
// and the place the applied values were read from.
func (h *Handler) invalidationConfigBlock(st watchStatus) map[string]any {
	rows, source := h.invalidationRows(st)
	return map[string]any{
		"surface":      "server.invalidation",
		"read_at":      time.Now().Format(time.RFC3339),
		"applied_from": source,
		"knobs":        rows,
		"ceiling":      ceilingBlock(st),
		"unhonoured":   unhonouredList(st),
		"declared":     "the knob table (name, default, range, unit, failure mode) is internal/invalidation.Knobs(); every row above is read from the running watcher when one exists, and no row reports a value the code does not use",
	}
}

// invalidationRows builds the per-knob rows for the RUNNING process, and returns
// the sentence that says where the applied values came from.
func (h *Handler) invalidationRows(st watchStatus) ([]map[string]any, string) {
	rows := h.inv.Rows()

	if h.watch == nil {
		// Nothing is running to obey most of these knobs. That is a fact about
		// this deployment, not a value to hide: the values reported are the ones
		// the surface WOULD apply, and every row says whether anything is applying
		// it. watch.enabled is the exception, and it is reported as applied
		// because it is the knob that DECIDES this state: no watcher was started
		// because it says false.
		reason := "no watcher is established on this target, so nothing is applying this knob yet"
		if !h.inv.Watch.Enabled {
			reason = "server.invalidation.watch.enabled is false, so no watcher was started and nothing is applying this knob"
		}
		for i := range rows {
			switch {
			case rows[i].Knob == invalidation.KnobWatchEnabled:
				rows[i].Applied = true
			case rows[i].Knob == invalidation.KnobPushMaxEventBytes:
				// BFS-062: the one push knob a value in force exists for today.
				rows[i].Applied, rows[i].AppliedReason = true, maxEventBytesAppliedReason
			case isWatchKnob(rows[i].Knob):
				rows[i].Applied, rows[i].AppliedReason = false, reason
			default:
				rows[i].Applied, rows[i].AppliedReason = false, pushNotServedReason
			}
		}
		return renderRows(rows), "the resolved configuration (no watcher is established on this target)"
	}

	// A watcher object EXISTS. Which of its knobs are APPLIED depends on whether it
	// is established: an install that reported an absence never started the
	// heartbeat loop or the flush, so claiming those as applied would be the same
	// class of claim as a counter that can never move (BFS-032). The knobs the
	// refused install DID use are still applied — the walk ran with scan_limit, and
	// §4.2's ceiling arithmetic ran with the headroom and the requested ceiling.
	established := st.State == WatchStateWatching || st.State == WatchStateOverflow
	running := h.watch.runningValues()
	notEstablished := "the watcher is not established on this target (state=" + st.State + "), so the machinery this knob bounds never started"
	for i := range rows {
		if isWatchKnob(rows[i].Knob) {
			if v, ok := running[rows[i].Knob]; ok {
				rows[i].Value = v
			}
			rows[i].Source = invalidation.SourceDefault
			if rows[i].Value != rows[i].Default {
				rows[i].Source = invalidation.SourceOperator
			}
			// watch.enabled is applied whenever a watcher object exists: its
			// instruction was carried out (one was built and its install attempted),
			// whatever the install then found. Every other knob bounds machinery
			// that only exists once the install SUCCEEDS.
			rows[i].Applied = rows[i].Knob == invalidation.KnobWatchEnabled ||
				established || knobUsedByARefusedInstall(rows[i].Knob)
			if !rows[i].Applied {
				rows[i].AppliedReason = notEstablished
			}
		} else {
			// BFS-062: `max_event_bytes` is the exception among the push knobs
			// — the events assembly measures frames against it, so it is in
			// force whether or not the push wire form exists.
			if rows[i].Knob == invalidation.KnobPushMaxEventBytes {
				rows[i].Applied, rows[i].AppliedReason = true, maxEventBytesAppliedReason
				continue
			}
			rows[i].Applied, rows[i].AppliedReason = false, pushNotServedReason
		}
	}
	from := "the running watcher's own options"
	if !established {
		from = "the watcher's own options, read while the install is ABSENT on this target (state=" + st.State + "): only the knobs the refused install used are in force"
	}
	return renderRows(rows), from
}

// knobUsedByARefusedInstall reports whether an install that ended in an absence
// still USED a knob. Three did: the install walk ran under the scan bound, and
// §4.2's ceiling arithmetic ran under the requested ceiling and the headroom. The
// rest — the heartbeat period, the flush cadence, the flush path bound — belong to
// machinery that never started, and a read-back that reported them as applied would
// be claiming an application nobody can observe.
func knobUsedByARefusedInstall(knob string) bool {
	switch knob {
	case invalidation.KnobWatchScanLimit, invalidation.KnobWatchMaxWatches, invalidation.KnobWatchHeadroom:
		return true
	}
	return false
}

func isWatchKnob(knob string) bool {
	return strings.HasPrefix(knob, "server.invalidation.watch.")
}

// renderRows turns the rows into wire objects, with the bool knob rendered as a
// JSON boolean rather than as the table's 0/1 (the table is numeric so one
// validator can judge every knob; the wire should not make an operator decode
// it).
func renderRows(rows []invalidation.Row) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		var value any = r.Value
		if r.Unit == invalidation.UnitBool {
			value = r.Value == 1
		}
		row := map[string]any{
			"knob": r.Knob, "value": value, "default": r.Default,
			"min": r.Min, "max": r.Max, "unit": r.Unit, "source": r.Source,
			"applied": r.Applied,
		}
		if r.AppliedReason != "" {
			row["applied_reason"] = r.AppliedReason
		}
		out = append(out, row)
	}
	return out
}

// ceilingBlock reports §4.2's arithmetic as numbers: what the deployment asked
// for, what the platform gives, what the tree needs, and which of them is the
// ceiling in force. requested is null when the knob is AUTO — a fabricated 0 would
// read as "the operator asked for nothing" (§3.3 rule 2).
func ceilingBlock(st watchStatus) map[string]any {
	if st.Ceiling == nil {
		return nil
	}
	c := st.Ceiling
	blk := map[string]any{
		"platform_max_user_watches": c.Platform,
		"ceiling_in_force":          c.Effective(),
		"binding":                   c.Binding(),
	}
	if c.Requested > 0 {
		blk["requested_max_watches"] = c.Requested
	} else {
		blk["requested_max_watches"] = nil
	}
	if c.Desired > 0 {
		blk["watches_desired"] = c.Desired
	} else {
		blk["watches_desired"] = nil
		blk["watches_desired_reason"] = "no install walk has run on this target, so the number of directories the watch set would need has not been measured"
	}
	blk["headroom"] = c.Headroom
	return blk
}

// unhonouredList is the configured-vs-observed carrier for the document. An empty
// list is reported as an empty list (not as null): "nothing was asked for that
// could not be given" is a fact, and it is the one an operator is checking for.
func unhonouredList(st watchStatus) []invalidation.Unhonoured {
	if st.Unhonoured == nil {
		return []invalidation.Unhonoured{}
	}
	return []invalidation.Unhonoured{*st.Unhonoured}
}

// runningValues reads the watcher knobs out of the watcher's OWN options — the
// fields its flush, walk and heartbeat read. It is the ONLY source for the
// read-back when a watcher exists; reading the requested config here would make
// the surface report a value nobody obeys.
//
// watch.max_watches is deliberately NOT overridden here: the row reports the
// REQUESTED value (the knob), while the ceiling actually in force and the platform
// ceiling travel in the ceiling block and the unhonourable pair. Reporting the
// platform's number as if it were the knob would make an AUTO configuration read
// as one an operator wrote.
func (w *watcher) runningValues() map[string]int64 {
	if w == nil {
		return nil
	}
	return map[string]int64{
		invalidation.KnobWatchHeartbeatMS:   int64(w.opts.heartbeat / time.Millisecond),
		invalidation.KnobWatchHeadroom:      int64(w.opts.headroom),
		invalidation.KnobWatchFlushEveryMS:  int64(w.opts.flushEvery / time.Millisecond),
		invalidation.KnobWatchFlushMaxPaths: int64(w.opts.flushMaxPaths),
		invalidation.KnobWatchScanLimit:     int64(w.opts.scanLimit),
	}
}
