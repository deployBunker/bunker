package hostsetup

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Bounding the host's own /tmp (GAP-075).
//
// Agents get a private /tmp, so the host /tmp is root's. It still must be
// bounded: an unbounded tmpfs is a RAM-exhaustion surface for anything that
// writes to it (root's own tools, and any leak from a session that is not a
// managed agent).
//
// The cap is installed as a systemd drop-in for tmp.mount, which is the unit
// that backs /tmp when the distribution mounts it as a tmpfs:
//
//	/etc/systemd/system/tmp.mount.d/50-bunker-size.conf
//	  [Mount]
//	  Options=<original options>,size=<cap>
//
// /etc/fstab is NEVER read, rewritten, or duplicated by any code path here:
// the drop-in is additive, is removed cleanly by RemoveHostTmpCap, and is
// testable with a redirected root. Applying the cap to a running /tmp is a
// NON-destructive `mount -o remount,size=…` — a remount of tmpfs changes the
// size limit and leaves the existing contents in place (unlike restarting
// tmp.mount, which would delete every file open in /tmp).

// DefaultTmpMountOptions mirrors the Options= of a stock systemd tmp.mount
// unit, used when /tmp is not currently mounted as a tmpfs and its live
// options therefore cannot be read.
const DefaultTmpMountOptions = "mode=1777,strictatime,nosuid,nodev"

// HostTmpState is the observed state of the host /tmp filesystem and of
// Bunker's cap.
type HostTmpState struct {
	// FSType is the filesystem type mounted at /tmp ("" when not a mount).
	FSType string
	// Options are the live mount options.
	Options string
	// SizeBytes is the size= option of the LIVE mount, 0 when absent.
	SizeBytes uint64
	// IsTmpfs is true when /tmp is a tmpfs (the only case a live cap applies).
	IsTmpfs bool
	// DropInPath / DropInPresent / DropInBody describe the installed cap.
	DropInPath    string
	DropInPresent bool
	DropInBody    string
	// DropInCappedBytes is the size= the drop-in specifies, 0 when absent.
	DropInCappedBytes uint64
}

// HostTmpStatus inspects /tmp and the installed drop-in without changing
// anything.
func (o Options) HostTmpStatus(ctx context.Context) (HostTmpState, error) {
	o = o.WithDefaults()
	st := HostTmpState{DropInPath: o.TmpMountDropInPath()}

	out, err := o.run(ctx, "findmnt", "-no", "FSTYPE,OPTIONS", "--target", "/tmp")
	if err == nil {
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) > 0 {
			st.FSType = fields[0]
		}
		if len(fields) > 1 {
			st.Options = fields[1]
		}
		st.IsTmpfs = st.FSType == "tmpfs"
		st.SizeBytes = sizeOption(st.Options)
	}

	if body, err := os.ReadFile(st.DropInPath); err == nil {
		st.DropInPresent = true
		st.DropInBody = string(body)
		st.DropInCappedBytes = sizeOption(RenderTmpMountOptions(string(body)))
	}
	return st, nil
}

// sizeOption extracts the numeric value of a size= option from a
// comma-separated mount-option string (0 when absent).
func sizeOption(options string) uint64 {
	for _, field := range strings.Split(options, ",") {
		field = strings.TrimSpace(field)
		if !strings.HasPrefix(field, "size=") {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimPrefix(field, "size="), 10, 64)
		if err != nil {
			return 0
		}
		return v
	}
	return 0
}

// RenderTmpMountOptions extracts the Options= value from a rendered drop-in
// body ("" when the body has none).
func RenderTmpMountOptions(dropInBody string) string {
	for _, line := range strings.Split(dropInBody, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Options=") {
			return strings.TrimPrefix(line, "Options=")
		}
	}
	return ""
}

// MergeTmpMountOptions returns options with size=<capBytes> set, preserving
// the order and every other token of the input. An existing size= is replaced
// in place; when absent the cap is appended.
func MergeTmpMountOptions(options string, capBytes uint64) string {
	want := fmt.Sprintf("size=%d", capBytes)
	parts := []string{}
	replaced := false
	for _, field := range strings.Split(options, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if strings.HasPrefix(field, "size=") {
			parts = append(parts, want)
			replaced = true
			continue
		}
		parts = append(parts, field)
	}
	if !replaced {
		parts = append(parts, want)
	}
	return strings.Join(parts, ",")
}

// RenderTmpMountDropIn renders the systemd drop-in that caps /tmp. Pure.
func RenderTmpMountDropIn(options string, capBytes uint64) string {
	return fmt.Sprintf(`# Bunker GAP-075 — host /tmp tmpfs cap (managed by `+"`bunker host-provision`"+`)
#
# Bounds the tmpfs that backs /tmp so nothing on the host can exhaust RAM by
# writing to it. This is a systemd drop-in for tmp.mount; /etc/fstab is not
# modified. Remove it with `+"`bunker host-provision --uninstall`"+`.
[Mount]
Options=%s
`, MergeTmpMountOptions(options, capBytes))
}

// EnsureHostTmpCap installs (or refreshes) the /tmp tmpfs cap. When apply is
// false it only reports what it would do. The drop-in is written whenever its
// content differs, systemd is reloaded, and a live tmpfs /tmp is remounted to
// the configured size WITHOUT losing its contents.
func (o Options) EnsureHostTmpCap(ctx context.Context, apply bool) (*Report, error) {
	o = o.WithDefaults()
	rep := &Report{}

	before, err := o.HostTmpStatus(ctx)
	if err != nil {
		return rep, err
	}

	base := before.Options
	if !before.IsTmpfs {
		base = DefaultTmpMountOptions
		rep.Add("skip", "/tmp", fmt.Sprintf("/tmp is %s, not a tmpfs — the drop-in caps it when tmp.mount is active", fstypeOrNothing(before.FSType)), false)
	}

	content := RenderTmpMountDropIn(base, o.HostTmpMaxBytes)
	if !apply {
		rep.Add("write", before.DropInPath, fmt.Sprintf("Options=%s", RenderTmpMountOptions(content)), false)
		if before.IsTmpfs && before.SizeBytes != o.HostTmpMaxBytes {
			rep.Add("remount", "/tmp", fmt.Sprintf("size=%d (non-destructive)", o.HostTmpMaxBytes), false)
		}
		return rep, nil
	}

	wrote, err := writeFileIdempotent(before.DropInPath, []byte(content), 0o644)
	if err != nil {
		return rep, err
	}
	rep.Add("write", before.DropInPath, fmt.Sprintf("Options=%s", RenderTmpMountOptions(content)), wrote)

	if wrote {
		if _, err := o.run(ctx, "systemctl", "daemon-reload"); err != nil {
			return rep, fmt.Errorf("systemctl daemon-reload: %w", err)
		}
		rep.Add("ok", "systemd", "daemon-reload", true)
	} else {
		rep.Add("ok", before.DropInPath, "drop-in already current", true)
	}

	if !before.IsTmpfs {
		return rep, nil
	}
	if before.SizeBytes == o.HostTmpMaxBytes {
		rep.Add("ok", "/tmp", fmt.Sprintf("already bounded at size=%d", o.HostTmpMaxBytes), true)
		return rep, nil
	}
	if _, err := o.run(ctx, "mount", "-o", fmt.Sprintf("remount,size=%d", o.HostTmpMaxBytes), "/tmp"); err != nil {
		return rep, fmt.Errorf("remount /tmp at size=%d: %w", o.HostTmpMaxBytes, err)
	}
	rep.Add("remount", "/tmp", fmt.Sprintf("size=%d (non-destructive)", o.HostTmpMaxBytes), true)
	return rep, nil
}

// RemoveHostTmpCap removes the drop-in and reloads systemd. The live size
// limit stays in effect until /tmp is next mounted; that is reported, not
// silently "fixed" by touching the running filesystem.
func (o Options) RemoveHostTmpCap(ctx context.Context, apply bool) (*Report, error) {
	o = o.WithDefaults()
	rep := &Report{}
	path := o.TmpMountDropInPath()
	if _, err := os.Stat(path); err != nil {
		rep.Add("skip", path, "no drop-in present", false)
		return rep, nil
	}
	if !apply {
		rep.Add("remove", path, "tmp.mount size drop-in", false)
		return rep, nil
	}
	if err := os.Remove(path); err != nil {
		return rep, fmt.Errorf("remove %s: %w", path, err)
	}
	rep.Add("remove", path, "tmp.mount size drop-in removed", true)
	if _, err := o.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return rep, fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	rep.Add("ok", "systemd", "daemon-reload", true)
	return rep, nil
}

// fstypeOrNothing renders a filesystem type for human-readable reports.
func fstypeOrNothing(fstype string) string {
	if fstype == "" {
		return "not a mount"
	}
	return fstype
}
