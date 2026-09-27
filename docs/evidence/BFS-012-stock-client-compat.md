# BFS-012 (part 1): stock HTTP/1.1 client compatibility — PROVEN

The release's hard constraint is that **HTTP/1.1 must keep working so older clients are not
broken**. This is proved with `curl`, a genuine third-party client that has never heard of
HTTP/2, HTTP/3, `X-Bunker-Op`, or any of our extensions — not with our own client, which
would be circular.

Reproduce: `bash docs/evidence/BFS-012-stock-client-probe.sh`
Raw transcript: `docs/evidence/BFS-012-stock-client-output.txt`

## Results (this run)

| check | result |
|---|---|
| the client is really speaking HTTP/1.1 | `http_version=1.1`, 200 |
| GET | 200, correct body |
| HEAD | 200 |
| OPTIONS | 200 |
| PROPFIND (Depth: 1) | **207** multistatus (the correct WebDAV code) |
| GET a nested path | 200 |
| PUT (plain, no extensions) | **201** (correct for create) |
| read back / on the server's disk | matches, and the bytes landed in the source tree |
| DELETE | **204**, and the file is gone from disk |
| PUT with a STALE `If-Match` | **412**, refused, file UNMODIFIED |
| the refusal body | a real `D:error` with `b:hash-mismatch` and `<b:expected>` |
| PUT with the CORRECT `If-Match` | 204, file updated |
| old User-Agent (`Mozilla/4.0`) | 200 |
| no `Accept` header at all | 200 |

## What this establishes

1. **The 412 refusal is interoperable**, not just internal: a stock client gets a standard
   `412 Precondition Failed` plus a parseable error document naming the expected hash — so a
   client that knows nothing about us can still tell *why* it was refused and what to re-read.
2. **The write path works for a client that sends no extension headers at all** — PUT 201 and
   DELETE 204 are the plain-RFC behaviours, and the bytes were confirmed on the server's disk
   rather than merely acknowledged.
3. **PROPFIND returns 207**, which is the one status a stock WebDAV client actually depends on.

## Status of the row

BFS-012 is NOT closed by this. It also requires the **cache bound** and **cross-platform**
parts, and it chains behind BFS-009/BFS-010. This file discharges the **stock HTTP/1.1 client
compatibility** part — the release's stated hard constraint — with a real client.
