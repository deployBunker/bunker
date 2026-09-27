//go:build !linux || appengine

package webdav

// defaultWatchEnv on a platform with no watch backend this build can use. It is
// a FACT and not a failure: the probe matrix's W-1 exists so that "this build
// has no watch facility" is reported as its own named reason with `backend:
// "none"` instead of collapsing into the same string as a full watch limit or a
// netbacked target (§4.1).
func defaultWatchEnv() watchEnv {
	return watchEnv{
		backendName:  watchBackendNone,
		newBackend:   nil,
		readCeilings: nil,
		readMounts:   nil,
	}
}
