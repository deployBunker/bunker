//go:build linux && !appengine

package webdav

import (
	"os"
	"strconv"
	"strings"
)

// defaultWatchEnv is the production environment: the inotify backend (fsnotify,
// already in go.mod), the configured ceilings from /proc/sys/fs/inotify, and the
// mount table from /proc/self/mountinfo.
//
// The three ceilings are the WHOLE of what procfs exposes (measured, App. A.3):
// the per-user TOTAL in use is not observable anywhere, which is why §4.2's probe
// is built as attempt-and-classify with a reported headroom rather than as a
// pre-flight computation.
func defaultWatchEnv() watchEnv {
	return watchEnv{
		backendName:  watchBackendInotify,
		newBackend:   newFsnotifyBackend,
		readCeilings: readInotifyCeilings,
		readMounts:   readMountTable,
	}
}

func readInotifyCeilings() watchCeilings {
	c := watchCeilings{}
	var missing []string
	read := func(path string) int64 {
		raw, err := os.ReadFile(path)
		if err != nil {
			missing = append(missing, path)
			return 0
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			missing = append(missing, path)
			return 0
		}
		return n
	}
	c.MaxUserWatches = read("/proc/sys/fs/inotify/max_user_watches")
	c.MaxUserInstances = read("/proc/sys/fs/inotify/max_user_instances")
	c.MaxQueuedEvents = read("/proc/sys/fs/inotify/max_queued_events")
	if len(missing) > 0 {
		// A ceiling the owner cannot see is not a bound (PRD §2.7): an unreadable
		// one is reported as a null WITH a reason, never as zero.
		c.Reason = "these ceiling files could not be read on this host: " + strings.Join(missing, ", ")
	}
	return c
}

// readMountTable parses /proc/self/mountinfo into the records the probe matrix
// needs. An empty result means "could not be read", and the caller reports the
// superblock relation as UNKNOWN rather than as false.
func readMountTable() []mountRecord {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	var out []mountRecord
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		// The separator field is "-"; everything after it is fstype, source,
		// super-options. Before it: mount id, parent, major:minor, root,
		// mount point, options, [optional fields].
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) {
			continue
		}
		out = append(out, mountRecord{
			MountPoint: unescapeMountField(fields[4]),
			FSType:     fields[sep+1],
			Source:     unescapeMountField(fields[sep+2]),
		})
	}
	return out
}

// unescapeMountField decodes the octal escapes the kernel uses in mountinfo
// fields (\040 for a space, \011 tab, \012 newline, \134 backslash). A field that
// carried an escape and was read raw would never match a real path by prefix.
func unescapeMountField(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
