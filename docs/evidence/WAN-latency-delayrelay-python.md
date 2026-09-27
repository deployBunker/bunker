# Delay relay for the WAN-latency measurement (Python source)

`.gitignore:37` ignores `*.py` repo-wide, so — following the same convention BFS-016 used for
`docs/evidence/BFS-016-probes-python.md` — the relay source lives here as a fenced block rather
than as a `.py` file. Save it and run it; it is pure stdlib and needs no dependencies.

Usage: `python3 delayrelay.py <listen_port> <upstream_port> <one_way_seconds>`
For dedi-2's measured 185.24 ms RTT, one-way = `0.09262`.

```python
#!/usr/bin/env python3
"""A delay relay: forwards TCP with a fixed one-way delay, so we can measure the client at a realistic
WAN RTT (dedi-2 measured ~185.24 ms) instead of loopback. Pure stdlib, threaded."""
import socket, threading, time, sys

LISTEN = int(sys.argv[1])
UPSTREAM = int(sys.argv[2])
DELAY = float(sys.argv[3])   # ONE-WAY seconds; RTT is about 2x this

def pump(src, dst, delay):
    try:
        while True:
            b = src.recv(65536)
            if not b:
                break
            if delay:
                time.sleep(delay)
            dst.sendall(b)
    except Exception:
        pass
    finally:
        try: dst.shutdown(socket.SHUT_WR)
        except Exception: pass

def handle(c):
    try:
        u = socket.create_connection(("127.0.0.1", UPSTREAM))
    except Exception:
        c.close(); return
    t1 = threading.Thread(target=pump, args=(c, u, DELAY), daemon=True)
    t2 = threading.Thread(target=pump, args=(u, c, DELAY), daemon=True)
    t1.start(); t2.start()
    t1.join(); t2.join()
    try: c.close()
    except Exception: pass
    try: u.close()
    except Exception: pass

s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", LISTEN)); s.listen(128)
print(f"delay relay :{LISTEN} -> :{UPSTREAM} one-way {DELAY*1000:.1f} ms (RTT ~{DELAY*2000:.1f} ms)", flush=True)
while True:
    c, _ = s.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
```

## Honest limits of this instrument

It delays **every recv chunk** by the same fixed amount. That reproduces **per-request latency** and
nothing else:

- no packet loss, no jitter, no reordering;
- no bandwidth limit — a large transfer is not slowed, only each chunk's arrival is shifted;
- no bufferbloat, which is a real and measured property of this fleet's links.

So a result through this relay is evidence about **latency sensitivity**, not a substitute for a real
DC. The release's both-DCs rule stays unsatisfied until it is measured against a real one.
