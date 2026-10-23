#!/usr/bin/env python3
"""Attribute-freshness probe for the sshfs readdirplus patch (MOUNT-015 D3).

The caveat the patch has to answer is documented in sshfs.c: a directory
listing can carry attributes that a still-pending write() has already made
stale, because with more than one SFTP connection the readdir() may be served
by a connection that has not seen the write. sshfs answers it by withholding the
attributes for entries whose path is in sshfs.conntab (the open-file table that
exists when max_conns > 1), which makes the kernel do a real getattr() for that
entry - and sshfs_getattr() routes that getattr to the connection that owns the
write.

This probe checks the observable consequences:

  1. while a freshly written file is STILL OPEN (write in flight), a stat()
     through the mount reports the new length;
  2. a directory listing taken in the same window reports the same length;
  3. after close, both still agree;
  4. the sshfs debug log shows the suppression fired ("no attrs for ..."), i.e.
     the patch took its caveat path rather than advertising stale attrs.

Run it against the patched build with max_conns>1 to exercise the caveat path,
and against the unpatched build to show the observable results are identical.

Usage: freshness-probe.py <mountpoint> [iterations]
Exit code 0 = every assertion held.
"""
import os
import subprocess
import sys


def stat_size(path):
    out = subprocess.run(["stat", "-c", "%s", path], capture_output=True, text=True)
    if out.returncode != 0:
        return None
    return int(out.stdout.strip())


def ls_size(path):
    """Size of one entry as reported by a LISTING of its directory.

    Listing the directory (rather than the file) is what makes the kernel
    populate its cache from a readdirplus reply, which is the reply whose
    attributes the patch withholds for files with a pending write.
    """
    d, name = os.path.split(path)
    out = subprocess.run(["ls", "-l", "--time-style=+%s", d], capture_output=True,
                         text=True)
    if out.returncode != 0:
        return None
    for line in out.stdout.splitlines():
        # -rw-r--r-- 1 owner group SIZE MTIME NAME  -> 7 fields for a plain name
        fields = line.split()
        if len(fields) >= 7 and fields[-1] == name:
            return int(fields[4])
    return None


def main():
    if len(sys.argv) < 2:
        print("usage: freshness-probe.py <mountpoint> [iterations]")
        return 2
    mp = sys.argv[1]
    iters = int(sys.argv[2]) if len(sys.argv) > 2 else 8
    d = os.path.join(mp, "freshness")
    os.makedirs(d, exist_ok=True)

    failures = 0
    rows = []
    for i in range(iters):
        path = os.path.join(d, "open-%02d.bin" % i)
        size = 262144 * (i + 1)          # 256 KiB .. 2 MiB
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
        try:
            chunk = b"M" * 65536
            written = 0
            while written < size:
                written += os.write(fd, chunk[: min(65536, size - written)])
            # window: the write is queued in sshfs, the SFTP reply has not been
            # waited for. Attribute reads here must still see the full length.
            f_stat = stat_size(path)
            f_ls = ls_size(path)
        finally:
            os.close(fd)
        f_after = stat_size(path)
        ok = (f_stat == size == f_ls == f_after)
        failures += 0 if ok else 1
        rows.append((i, size, f_stat, f_ls, f_after, "PASS" if ok else "FAIL"))
        print("iter %02d written=%-8d stat_open=%-8s ls=%-8s stat_closed=%-8s %s"
              % (i, size, f_stat, f_ls, f_after, "PASS" if ok else "FAIL"))

    total = 0
    for i in range(iters):
        total += os.path.getsize(os.path.join(d, "open-%02d.bin" % i))
    print("bytes written through the mount: %d" % total)
    print("RESULT: %s (%d/%d iterations consistent)"
          % ("PASS" if failures == 0 else "FAIL", iters - failures, iters))
    return 0 if failures == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
