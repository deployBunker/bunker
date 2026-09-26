package fsclient

import "time"

// clockNow is the package's clock seam. Tests replace it to drive the
// invalidation idle rule and the cache TTL without waiting real seconds; nothing
// else in the package calls time.Now directly.
var clockNow = time.Now

// timeNow reads the client clock.
func timeNow() time.Time { return clockNow() }

// timeSince measures elapsed time against the client clock.
func timeSince(t time.Time) time.Duration { return clockNow().Sub(t) }
