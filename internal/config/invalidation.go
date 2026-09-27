package config

import "github.com/deployBunker/bunker/internal/invalidation"

// BFS-043: the server-side invalidation config surface, as the daemon's config
// layer sees it.
//
// The knob table itself lives in internal/invalidation — one declaration of every
// name, default, range and failure mode, shared by this layer (which refuses a bad
// value at load, before any listener binds) and by internal/server/webdav (which
// obeys the values and reports them at runtime). Nothing about a knob is written
// down twice, so the range this layer enforces and the range the surface reports
// cannot drift.
//
// The pointer fields in invalidation.Spec are what make "the key was not written"
// distinguishable from "the key was written with a value": an absent key takes the
// declared default, a written key is obeyed or REFUSED. A silently substituted
// default is the shape this repository has already shipped twice (BFS-031's bound
// that did not bound, BFS-032's counter that could never move) and the row's
// acceptance forbids it.

// InvalidationValues resolves server.invalidation into the surface the daemon
// serves with. It never returns a substituted value: a value outside its declared
// range (or a broken relation between two knobs) is an error naming the knob, the
// value and the range.
func (c *Config) InvalidationValues() (invalidation.Values, error) {
	if c.Server.Invalidation == nil {
		// No block: every knob keeps its declared default, which is the surface a
		// deployment has today (the poll form is not behind a knob at all).
		return invalidation.DefaultValues(), nil
	}
	return c.Server.Invalidation.Resolve()
}
