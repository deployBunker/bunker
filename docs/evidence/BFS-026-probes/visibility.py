#!/usr/bin/env python3
"""visibility.py — the BFS-026 visibility arm, on ONE clock.

Everything is timed from a single process so the transcript can be read without
arithmetic: the edit, the immediate read, the control read, the moment the
mount's own record reports the change, and the first read that serves the new
bytes.

Three facts are separated on purpose, because collapsing them is how a probe
claims credit for the wrong mechanism:

  * REFUSED-AND-REPAIRED — a never-read path whose published size is stale: the
    immediate read is refused (BFS-025's rule) and the mount repairs itself on
    that refusal, so a read 0.4 s later succeeds WITHOUT the channel. Measured
    here, with the mount's own log lines quoted by the caller.
  * THE CHANNEL — the same edit seen in the mount's record: events_total /
    paths_dropped_total move, at a measured latency.
  * SERVED-FRESH — the read that finally serves the new bytes: for a cached path
    this only happens AFTER the channel reported the change, and the control
    reads (immediate, +0.4 s) prove nothing else repaired it.

usage:
  visibility.py --status FILE --tree DIR --mount DIR --path REL --body NEW
                [--hold SECONDS] [--control-delay SECONDS]
"""
import argparse
import json
import os
import sys
import time


def ms(t0):
    return int((time.time() - t0) * 1000)


def read_record(status):
    """The mount's own invalidation record, or None while it is being rewritten."""
    try:
        with open(status, "r", encoding="utf-8") as fh:
            return json.load(fh)["invalidation"]
    except Exception:
        return None


def counters(inv):
    return (inv["events_total"], inv["paths_dropped_total"], inv["resyncs_total"], inv["seq"])


def read_via_mount(mount, rel):
    try:
        with open(os.path.join(mount, rel), "rb") as fh:
            return fh.read().decode("utf-8", "replace")
    except OSError as exc:
        return "REFUSED: %s: %s" % (exc.__class__.__name__, exc.strerror or exc)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--status", required=True)
    ap.add_argument("--tree", required=True)
    ap.add_argument("--mount", required=True)
    ap.add_argument("--path", required=True, help="path relative to both tree and mount")
    ap.add_argument("--body", required=True, help="the new content written on the agent")
    ap.add_argument("--hold", type=float, default=15.0, help="seconds to wait for the channel")
    ap.add_argument("--control-delay", type=float, default=0.4)
    args = ap.parse_args()

    t0 = time.time()
    before = read_record(args.status)
    c_before = counters(before) if before else None
    print("label            : %s" % args.path)
    print("record before    : events=%s dropped=%s resyncs=%s seq=%s available=%s mechanism=%s"
          % (*c_before, before["channel_available"], before["mechanism"]) if before else "record before    : unreadable")

    # The edit, made ON THE AGENT: no request of ours, no surface call.
    target = os.path.join(args.tree, args.path)
    with open(target, "w", encoding="utf-8") as fh:
        fh.write(args.body)
    print("agent bytes now  : %r (%d bytes)  [edit at +%d ms]"
          % (args.body, len(args.body.encode()), ms(t0)))

    # The immediate read: what the mount holds right now.
    got = read_via_mount(args.mount, args.path)
    print("read +%4d ms      : %s" % (ms(t0), got if got.startswith("REFUSED") else repr(got)))

    # The control read: still inside the declared window, and BEFORE the channel
    # has reported anything. If this one already serves the new bytes, then
    # something other than the channel repaired it and the probe must say so.
    time.sleep(args.control_delay)
    got2 = read_via_mount(args.mount, args.path)
    print("read +%4d ms      : %s   (control: still inside the declared window)"
          % (ms(t0), got2 if got2.startswith("REFUSED") else repr(got2)))
    control_fresh = got2 == args.body

    # Wait for the CHANNEL, read from the mount's own record.
    delivered_at = None
    after = before
    while ms(t0) < args.hold * 1000:
        now = read_record(args.status)
        if now is not None:
            after = now
            if before is None or counters(now) != c_before:
                delivered_at = ms(t0)
                break
        time.sleep(0.02)

    if delivered_at is None:
        print("channel reported : NONE within %.1f s — the record never moved" % args.hold)
    else:
        print("channel reported : +%d ms  (events=%s dropped=%s resyncs=%s seq=%s)"
              % (delivered_at, *counters(after)))

    # The read that finally serves the new bytes.
    served_at = None
    if got == args.body:
        served_at = 0
    elif control_fresh:
        served_at = None  # repaired before the channel: not attributable to it
    else:
        while ms(t0) < args.hold * 1000:
            got3 = read_via_mount(args.mount, args.path)
            if got3 == args.body:
                served_at = ms(t0)
                break
            time.sleep(0.05)
    if served_at is not None:
        print("served fresh at  : +%d ms" % served_at)
    elif control_fresh:
        print("served fresh at  : +%d ms  (BEFORE the channel: repaired by the read path, not by the channel)"
              % int(args.control_delay * 1000))
    else:
        print("served fresh at  : NEVER within %.1f s" % args.hold)

    print("record after     : events=%s dropped=%s resyncs=%s seq=%s available=%s mechanism=%s reason=%r"
          % (*counters(after), after["channel_available"], after["mechanism"], after.get("reason", "")))
    print("SUMMARY %s edit=0 control_fresh=%s channel_ms=%s fresh_ms=%s"
          % (args.path, control_fresh, delivered_at if delivered_at is not None else "none",
             served_at if served_at is not None else "none"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
