# SPEC: the `bunkerd` WebDAV surface — the standard protocol, faithfully, plus a declared extension layer

**Project:** bunker (owning) · **Row:** BFS-004 (P1, complexity 2) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-26
**Status:** proposed — spec of record for the WebDAV wire surface. **No product code was changed by this row.**
**Deliverable:** this document.
**Design authority (inherited by reference, not restated):** `docs/prd/PRD-bunker-fs.md` — the front door for the
filesystem path. This document is the front door for the *wire surface* that PRD names in its server-surface table
(`PRD-bunker-fs.md:209–220`) and that its acceptance criteria grade (`AC-3:128`, `AC-6:131`, `AC-8:133`, `AC-9:134`,
`AC-12:137`).
**Evidence this document builds on (both landed on main):**

- `docs/investigation/BFS-002-multi-protocol-listener.md` — one port number, two transports, one process; the measured
  ALPN matrix; h2c's real shape; Alt-Svc discovery; quic-go's measured cost. Cited below as **BFS-002 §n**.
- `docs/investigation/BFS-003-fuse-client-binding.md` — the client binding (go-fuse, pure Go), the four capabilities the
  client must own, the FUSE floors, the Windows/Mint notes. Cited below as **BFS-003 §n**.

**Implementers:** BFS-006 (h1+h2 surface), BFS-007 (h3 + Alt-Svc), BFS-008/BFS-009 (client + cache/diff), BFS-011
(the 14-op battery), BFS-012 (old-client compatibility + refusal proofs).
**Named consumer:** Muster rows `PROTO-012` / `PROTO-014` / `PROTO-015` — §9 of this document is the cross-repo contract.

**RFC reference set (and the precedence rule).** RFC 4918 (WebDAV) is the method/property authority. RFC 4918 cites
RFC 2616, which is obsoleted: where a header or status-code *semantic* is defined in both, **RFC 9110 governs** the
HTTP-level meaning (conditional requests, `405`+`Allow`, `412`, `501`), and RFC 9111 governs caching. RFC 9113 (HTTP/2)
and RFC 9114 (HTTP/3) govern the transport differences in §4. RFC 4918's own status family (`207`, `422`, `423`, `424`,
`507`) and its XML vocabulary (`DAV:error`, `DAV:multistatus`, `DAV:propstat`) are used exactly as written there.

---

## 0. Verdict in one screen

The surface is **two halves that are never conflated**:

1. **The standard surface.** A stock WebDAV client — davfs2, rclone, Finder, the Windows redirector — performs every
   RFC 4918 class 1 + class 2 operation it uses, **with no configuration and no knowledge of anything bunker-specific**.
   Backed by measurement: HTTP/1.1 keeps working in every configuration measured (BFS-002 §7), the effective TCP ALPN
   list always contains `http/1.1` (BFS-002 §7; stdlib `adjustNextProtos`, `net/http/server.go:3533–3562`), and the
   current deployment already serves cleartext HTTP/1.1 (`BFS-002 §2.3`).
2. **Our extension layer**, declared as **six numbered extensions** (§3), each with a request shape, a response shape and
   an error shape. Nothing in the extension layer is required for a standard operation to succeed. A client that ignores
   every `X-Bunker-*` header gets **correct standard behaviour**; a client that asks for an extension this build, target
   or transport does not have gets a **structured `capability_unavailable`** naming what is absent and the mode actually
   in force — never a silently different behaviour (§5).

**The per-version rule, in one line:** *the HTTP version changes **cost and transport affordance**, never the
vocabulary.* HTTP/1.1, HTTP/2 and HTTP/3 get the same methods, the same properties and the same extension headers; the
version decides how many requests can be in flight on one connection (measured 1.0× at HTTP/1.1 vs 25.0× at HTTP/2,
`PRD-bunker-fs.md:60–61`), whether a long-lived invalidation stream is cheap (h2/h3) or occupies the single connection
(h1.1), and whether HTTP/3 exists at all (UDP + TLS 1.3 required).

**Three decisions this document makes that the PRD and the investigations left open:**

| # | Decision | Reason (evidence) |
|---|---|---|
| D1 | **Content hash is the identity; mtime is never a validator.** `ETag` *is* the content hash (`"sha256:<64 hex>"`, strong), and `If-Match` compares it. | The PRD names touch-without-change as a taxonomy class (`:184`) and the fix as a design rule (`:235`, `:283`). Measured, the kernel's default invalidation is **mtime-driven**: go-fuse's live INIT took `AUTO_INVAL_DATA` and **not** `EXPLICIT_INVAL_DATA` even though the kernel offered both (BFS-003 §7.2, Appendix A.5). |
| D2 | **`PROPFIND Depth: infinity` is refused** (`403` + the RFC's own `propfind-finite-depth`), with the walk collapse moved into the extension layer (`X-Bunker-Op: snapshot`). | RFC 4918 §9.1 sanctions exactly this refusal ("support for infinite-depth requests MAY be disabled, due to the performance and security concerns"); measurement says whole-tree walks are the stall class (sshfs **7 stalls of 14** on dedi-2 / **8** on mvp, `PRD-bunker-fs.md:25`; `diff --stat` **still stalls at 45.09 s** even on rclone's WebDAV backend, `:85`), while the same work **on** the host is **14/14, 0.41 s flat** (`:31`). |
| D3 | **A stale-hash write is refused when it would change bytes, and reported as a no-op when it would not.** Mismatched precondition + different bytes ⇒ `412` naming both hashes; mismatched precondition + bytes already identical ⇒ `204` + `X-Bunker-Verdict: identical_content`, disk untouched. | RFC 9110 §13.1.1 explicitly permits the 2xx form when "the change requested by the user agent has already succeeded"; refusing a write that changes nothing is the nagging-check failure the PRD names as the reason people disable checks (`:283`). The refusal case is the one AC-3 grades (`PRD-bunker-fs.md:128`). |

---

## 1. Scope, and the rule that keeps the two halves apart

**In scope:** the wire contract for `internal/server/webdav/` — methods, status codes, properties, extension headers and
extension operations, capability discovery, per-version behaviour, the refusal vocabulary, and the requirements this
places on the transport work (BFS-006/BFS-007) and the client work (BFS-008/BFS-009).

**Out of scope (owned elsewhere):** the client's cache and its eviction (BFS-005/BFS-009); the FUSE binding (BFS-003,
BFS-008); the agent-side watcher implementation (BFS-009); the transport plumbing (BFS-002, BFS-006, BFS-007);
credentials and their partitioning (the house PRD, `PRD-bunker-remote-editing.md`); execution of any kind — the mount
gives edits, never execution.

**The separation rule (the reason this document exists):**

- **R1 — No extension is load-bearing for a standard operation.** Every standard method and property in §2 works for a
  client that sends no `X-Bunker-*` header and reads none. If an extension is absent, the standard answer is unchanged.
- **R2 — An extension is never a deviation in disguise.** Where we differ from RFC 4918 at all, we differ *loudly* and
  we *say so*: a standard status code, a machine-readable code in the header **and** in the body, and — for the four
  declared deviations — a line in §2.3 and in §8.3.
- **R3 — Capability absence is an error, never a missing feature.** A capability the build, target, transport or config
  does not have stays on the surface and answers `capability_unavailable` (`501`, with `scope` and `mode`). A silently
  missing capability is strictly worse than a refusal: the caller cannot distinguish "not supported" from "not built"
  from "not loaded" (the PRD's own rule, `PRD-bunker-fs.md:222` and `AC-9:134`).
- **R4 — Refusal is a success of the protocol.** A refusal names the current state (`hash_mismatch` carries both
  hashes), is atomic (nothing partially written), and tells the caller exactly what to do next. Callers are expected to
  branch on codes, not retry blindly.

**Definition used by the "safe refusal" column in §2.1** — a refusal is SAFE when all four hold:

1. **Loud** — a standard HTTP status, plus a machine code in `X-Bunker-Verdict` **and** in the body; never a `2xx` with
   different behaviour.
2. **Atomic** — no partial state: nothing created, nothing written, no lock taken, no destination half-copied.
3. **Sanctioned** — the refusal is prescribed or explicitly permitted by the RFC for that method, or is a declared
   deviation from a `SHOULD`/`MAY` that this document names.
4. **Recoverable** — the caller has a standard way to accomplish the same intent (a different Depth, a legal verb, or a
   named extension operation), or the intent is out of scope by design.

---

## 2. The standard surface — RFC 4918, faithfully

### 2.1 Method matrix (acceptance criterion 1)

`v1` = the target surface once its slice has landed. "Slice" refers to the PRD's C-slices (`PRD-bunker-fs.md:290–298`);
a method whose slice has not landed answers `501` + `not_implemented_yet` with `phase=<slice>` (§5), **never** a
success-shaped no-op.

| Method | RFC 4918 | v1 support | What we return | Why this is faithful / why the refusal is SAFE |
|---|---|---|---|---|
| `OPTIONS` | §10.1 | **Supported, always** | `200`/`204`; `DAV: 1` (and `2` when `LOCK` is live); `Allow`; the extension capability headers (§4.1) | The RFC requires `DAV` on *all* OPTIONS responses of a WebDAV resource (§18.1). This is the discovery entry point, so it can never be gated on a build. |
| `GET` | §9.4 | **Supported** (non-collection). Collections: `405` + `Allow` | `200` + bytes; `ETag` = content hash; `206` for a **single** byte range; `304` on a matching `If-None-Match`; `416` for an unsatisfiable range | Collection GET: §9.4 leaves the entity server-defined ("or something else altogether") and we choose 405 — **deviation 1** (§2.3). A caller that wants structure uses `PROPFIND`, which is the standard way to ask. Multi-range: RFC 9110 §14.2 permits a server to ignore `Range`; we answer `200` with the full body (loud enough: the client sees a 200, not a partial 206). |
| `HEAD` | RFC 9110 | **Supported** | Identical headers to `GET`, no body | The cheap way to fetch a base hash for a conditional write without transferring bytes (§7.4). |
| `PUT` | §9.7 | **Supported** (slice C4 for the precondition flavour) | `201` created / `204` replaced; `409` when the parent collection is missing; `405` on a collection; `412` on a failed `If-Match`/`If-None-Match`; `413` over the size cap; `507` when the agent cannot store it | §9.7.1 requires `409` for a missing parent — we return it rather than auto-creating (§9.7.1's "MUST fail"). Writes land through a temp file + rename, so a failed or killed request leaves the previous bytes intact; no partial file is ever visible (the guarantee `AC-6:131` grades). |
| `DELETE` | §9.6 | **Supported** | `204`/`200`; `404` when unmapped; `400` + `invalid_depth` if a `Depth` header other than `infinity` is sent on a collection | §9.6.1: DELETE on a collection *is* `Depth: infinity`, and "a client MUST NOT submit a Depth header … with any value but infinity". Treating a wrong value as infinity would be a silently different behaviour, so it is a loud `400` — **deviation 4** (§2.3). Locks rooted on the deleted resource are destroyed (§9.6), which is the one server-side MUST we must not miss. |
| `MKCOL` | §9.3 | **Supported** | `201`; `405` when the URL is already mapped; `409` when an ancestor is missing; `415` for any request body; `507` when the agent cannot store it | §9.3: ancestors MUST already exist and "the server MUST NOT create those intermediate collections automatically" — `409` is the RFC's own answer, not a limitation. §9.3's body rule is a MUST: an entity type we do not support ⇒ `415`. v1 supports **no** MKCOL body, so any body is `415`. |
| `PROPFIND` | §9.1 | **Supported**, `Depth: 0` and `1` | `207` + `DAV:multistatus`; `403` + `propfind-finite-depth` for `Depth: infinity`; `400` + `depth_required` when no `Depth` header is present; `404` when unmapped | `Depth: 0`/`1` are the two values the RFC says servers **MUST** support (§9.1). The `infinity` refusal is §9.1's own "MAY be disabled" with its own precondition code (§9.1.1) — **decision D2**. `depth_required`: §9.1 says servers "SHOULD treat a request without a Depth header as if a `Depth: infinity` … was included" — since *that* is refused, deriving a different default (e.g. 1) would answer a different question than the client asked, so we refuse loudly instead. RFC 4918 also makes the field a client **MUST**, so this costs no conformant client anything. |
| `PROPPATCH` | §9.2 | **Supported** as a *method*; every property we hold is protected | `207` + per-property status; `403` with the RFC's `cannot-modify-protected-property` for a live/computed property; `424` for the remaining instructions in the same atomic group; `400` for a malformed or non-`propertyupdate` body | §9.2 is a hard MUST: "All DAV-compliant resources MUST support the PROPPATCH method". We therefore implement the method, the document-order rule and the all-or-nothing rule, and refuse **per property** inside a `207` — the shape the RFC prescribes for exactly this (`:2480` names the `cannot-modify-protected-property` precondition). Refusing per property is atomic (nothing changed) and the client is told which property and why. Dead properties are refused as a declared deviation from a `SHOULD` — **deviation 2** (§2.3). |
| `COPY` | §9.8 | **Supported** for files and collections, `Depth: 0` and `infinity` | `201`/`204`; `412` when `Overwrite: F` and the destination is mapped; `409` when an intermediate destination collection is missing; `502` when the destination is outside this surface's namespace (a different tree); `423` when locked; `507` on storage failure | Destination handling follows §9.8.4/§9.8.5 exactly. The whole-copy work runs **on the agent** in one request, which is the principle the whole design rests on (native: `14/14, 0.41 s`, `PRD-bunker-fs.md:31`); a `502` for a cross-tree destination is the RFC's own code for "the destination namespace refuses to accept the resource". |
| `MOVE` | §9.9 | **Supported** as `COPY` + source removal; destination semantics as above | `201`/`204`; the same `409`/`412`/`423`/`502` family as `COPY`; `403` when source and destination are the same resource | §9.9's default is `Depth: infinity` and a rename is one server-side operation, so nothing here is refused. `403` for identical source/destination is the RFC's recommendation (§9.9.4). |
| `LOCK` | §9.10 | **Supported when backed by the in-tree lease registry** (slice C4). Before that: `501` + `not_implemented_yet`, and `DAV: 1` only | `200` (existing resource) / `201` (unmapped URL ⇒ creates the empty resource, §9.10.4) with a `lockdiscovery` body and a `Lock-Token` header; `200` for a refresh (no `Lock-Token`, `lockdiscovery` body); `423` + `no-conflicting-lock`; `412` + `lock-token-matches-request-uri`; `403` for a shared lock. `Depth: 0` and `infinity` both accepted (§9.10.3 makes the Depth header a MUST for a LOCK-supporting resource). `Timeout` request header honoured by clamping to the lease TTL, and the **granted** value always returned in the `Timeout` response header (§18.2 makes that header part of class 2); `Infinite` is answered with the server's maximum, never an unbounded lease | `DAV: 2` is advertised **only** when this method is live and enforcing (§18.2), because advertising class 2 without enforcement would be a lie a client would trust. `PRD-bunker-fs.md:248` fixes the backing store: the existing lease registry (`<git-common-dir>/agent-leases.json`), **not a second lock system**; its refusal semantics are already proven (CHT-007: 6 processes → 1 grant / 5 refusals). The empty resource that §9.10.4 mandates is deliberate and visible in the tree, which is why a LOCK on an unmapped URL must never be treated as a lock-only no-op |
| `UNLOCK` | §9.11 | **Supported** with `LOCK` | `204`; `400` when no token was supplied; `403` when the principal may not remove the lock; `409` + `lock-token-matches-request-uri` when the URI is outside the token's scope | §9.11: any resource that supports LOCK MUST support UNLOCK; §9.11.1's codes are used verbatim. |
| `POST` | §9.5 | **Supported, extension-only** | `200` + a JSON envelope for a recognised `X-Bunker-Op` (§3 E-4); `400` + `extension_op_missing` when no `X-Bunker-Op` is present; `400` + `op_unknown` for an unrecognised value; `400` + `bad_arguments` when the body does not match the op's argument schema | §9.5 leaves POST semantics entirely server-defined, so this is the one place an extension verb belongs. Those three refusals are the loud alternatives to "a bare POST does something undefined": each names exactly what was wrong with the request rather than guessing an intent. |
| `TRACE`, `CONNECT`, `PATCH` | — (not WebDAV) | **Refused** | `405` + `Allow` | RFC 9110 §15.5.6 requires `Allow` on a `405`; the caller learns the real verb set in the same response. Nothing changes state. |
| `REPORT`, `ACL`, `MKCALENDAR`, `SEARCH`, `VERSION-CONTROL`, `CHECKOUT`, `MKWORKSPACE`, `LABEL`, `MERGE`, `UNCHECKOUT`, `UPDATE` | RFC 3253/3744/4791/5323 | **Refused** | `405` + `Allow` | These are extensions of WebDAV that this surface does not implement. `405` (known method, unsupported on this resource) is the honest verdict for a verb we recognise; per RFC 9110 that is the code for "known by the origin server but not supported by the target resource". No partial behaviour is possible. |
| any other token (e.g. `FROBNICATE`) | — | **Refused** | `501` + `X-Bunker-Verdict: method_unknown` | RFC 9110 §15.6.2 is written for exactly this: "the server does not recognize the request method and is not capable of supporting it for any resource". Distinct from `405` on purpose: *unknown verb* and *known-but-unsupported verb* are different diagnoses, and the row asks for that distinguishability (§5.1). |

### 2.2 Properties (live, protected, and the three this surface adds)

| Property | Kind | In `allprop`? | Notes |
|---|---|---|---|
| `DAV:resourcetype`, `DAV:displayname`, `DAV:getcontentlength`, `DAV:getlastmodified`, `DAV:creationdate`, `DAV:getcontenttype`, `DAV:getetag`, `DAV:supportedlock`, `DAV:lockdiscovery` | live (computed) | yes, except where noted | `DAV:getlastmodified` is served for RFC fidelity and for stock clients that key on it; **it is not an identity** (§7). `DAV:getetag` is the content hash. `DAV:supportedlock`/`DAV:lockdiscovery` are present only in builds where LOCK is live (i.e. when `DAV: 2` is advertised). |
| `DAV:supportedlock` on a build without LOCK | — | no | Absent, not empty: an empty `supportedlock` would read as "supports no locks" while the `DAV` header says `1`. Absent + `DAV: 1` is coherent. |
| `bunkerd:hash` (E-1) | live, computed, **expensive** | **no** | Content hash of the resource. Returned when named in `<prop>`, or via `<include>`. Never in `allprop`: see the reason and the measurement in §7.3. |
| `bunkerd:rev` (E-3) | live, computed, cheap | **yes** | Tree-level revision (opaque string). One read of `HEAD` for a git tree, so it is cheap enough for `allprop`. |
| `bunkerd:tree` (E-3) | live, computed, cheap | **yes** | Tree identity (opaque string). The value a mount binds to (AC-7). |
| dead properties (client-chosen names) | — | no | **Refused on write** (deviation 2, §2.3) and reported in `propstat` as `403`. There is no dead-property store in v1. |

### 2.3 The declared deviations — four, all loud

Every line below is a place a stock client can observe behaviour that differs from a plain reading of RFC 4918. They
are enumerated here (and mirrored into the old-client section, §8.3) so the compatibility battery in BFS-012 tests them
deliberately rather than discovering them.

| # | Deviation | RFC text | Why it is safe |
|---|---|---|---|
| **1** | `GET`/`HEAD` on a collection ⇒ `405` | §9.4 leaves the entity "something else altogether" | `PROPFIND` (the standard mechanism for structure) is unaffected; the refusal is loud and carries `Allow`; nothing is half-rendered. |
| **2** | `PROPPATCH` of a dead property ⇒ `403` per property in a `207` | §9.2 "SHOULD support the setting of arbitrary dead properties" | A `SHOULD`, not a `MUST`; the method itself is fully implemented (`MUST`), so class 1 holds; the per-property `403` is the RFC's own shape for a refused property; and the client is told the property was not stored rather than discovering it on a later read. The reason v1 has no store: the only place a side store could live is inside the served tree, and an in-tree metadata file would appear in the tree's own `git status` — the exact fidelity this product exists to protect. See Open decision O-1. |
| **3** | `PROPFIND Depth: infinity` ⇒ `403` + `propfind-finite-depth` | §9.1: "in practice, support for infinite-depth requests MAY be disabled, due to the performance and security concerns"; §9.1.1 names this code | The RFC prescribes both the refusal and its code; `PROPFIND` is safe and idempotent so no state can be half-read; the caller recurses with `Depth: 1` (the standard client behaviour) or uses E-4's `snapshot` for one call. Measurement is the reason it is worth doing at all (§0, D2). |
| **4** | Wrong `Depth` on a collection `DELETE` ⇒ `400` + `invalid_depth` | §9.6.1: a client "MUST NOT" send a Depth other than `infinity` | The client's request is non-conformant; treating it as `infinity` would delete a tree the client asked to delete *shallowly* — the one thing a DELETE must not get wrong. `400` is atomic and unambiguous. |

---

## 3. The extension layer — six extensions, numbered (acceptance criterion 2)

Every extension below is additive: none of them is required for a standard operation, and none of them changes the
meaning of a standard request. Header names use the registered `X-` form the PRD already fixes
(`PRD-bunker-fs.md:211–219`).

**Namespace.** Extension properties live under `urn:bunker:fs:1` (prefix `b:` in examples), because RFC 4918 says
non-IETF identifiers "SHOULD be Coded-URLs to encourage uniqueness" (§10.1's own words about the `DAV` header's
vocabulary, applied to properties). The namespace URI is **frozen**: a change to any extension's wire shape
increments the capability document's `surface` version (§4.2), it does not silently change meaning.

**Two invariants that apply to every extension:**

- **Read-only or standard.** No extension operation mutates the tree. Mutations are performed with the standard methods
  (`PUT`, `DELETE`, `MKCOL`, `MOVE`, `COPY`, `LOCK`). E-4's operations are therefore safe to retry even though `POST` is
  not idempotent by definition — the reason `POST` is the carrier (§3 E-4).
- **Body-carried as well as header-carried.** Every machine-readable verdict travels in **both** a response header and
  the response body. Reason, measured on the consumer side: Muster's response cache rebuilds 2xx GET responses and
  **drops every header except `Content-Type`** while forging `Proto` to `HTTP/1.1` (PROTO-010 §6, `pkg/client/cache.go:110–126`).
  A contract that lives only in headers is a contract one cache wrapper can erase.

### E-1 — Content-addressed identity: `ETag` + `X-Bunker-Hash`

**What it is.** The resource's identity is the **content hash of its bytes**, exposed twice: as the RFC-native `ETag`
(so every stock client gets hash identity for free, correctly), and as an explicit `X-Bunker-Hash` response header whose
value names the algorithm (so our client never parses an opaque tag format).

- **Request shape.** None. On a `PUT` the request header `X-Bunker-Hash: <alg>:<hex>` is *accepted and verified* when
  present — it declares the hash of the bytes being sent.
- **Response shape.**
  - `ETag: "sha256:<64 lowercase hex>"` on `GET`, `HEAD`, `PUT`, `PROPFIND` (`DAV:getetag`), `COPY`/`MOVE` results.
    A **strong** validator: no `W/` prefix, ever (a weak tag invites the byte-range/conditional-merge semantics the
    refusal rule forbids).
  - `X-Bunker-Hash: sha256:<64 lowercase hex>` — the same value, with the algorithm named separately.
  - The hash is of the **entity bytes**, never of metadata: a pure `touch` (mtime moved, bytes identical) does not change
    it. This is acceptance criterion 5 of this row, specified in §7.
- **Error shape.** `422` + `X-Bunker-Verdict: body_hash_mismatch` (body: `b:body-hash-mismatch` with `b:expected` and
  `b:received`) when a `PUT` declares `X-Bunker-Hash` and the received bytes do not hash to it. Nothing is written.
  Distinct from `412`, which is about the **base** state, not about the arriving bytes.

### E-2 — Conditional writes that refuse: `If-Match` / `If-None-Match`

**What it is.** The RFC 9110 conditional-request machinery applied with hash validators, so a lost update becomes a
loud refusal. This is the extension a *stock* client can use without knowing it is one, because it is pure RFC.

- **Request shape.** `If-Match: "<alg>:<hex>"` (single tag, a list of tags, or `*`), `If-None-Match: *` for
  create-only. Comparison is the **strong** comparison function (RFC 9110 §13.1.1 requires strong for `If-Match`).
- **Response shape.** `412` on failure, `204` when the request is already satisfied, and on success the new `ETag`.
  The full exchange is in §6 (acceptance criterion 4).
- **Error shape.** `412` + `X-Bunker-Verdict: hash_mismatch` + `X-Bunker-Current-Hash` + `X-Bunker-Expected-Hash`, body
  carrying `b:hash-mismatch` with both values. A failure caused by absence (`If-Match` on an unmapped URI) is `412` +
  `precondition_failed` (RFC-plain, no hash) — the two are distinguished because the remedies differ (re-read the file
  vs create it).

### E-3 — Tree revision and tree identity: `X-Bunker-Rev`, `X-Bunker-Tree`

**What it is.** Two cheap, tree-level values. The revision answers "did the tree move since I looked?", the identity
answers "is this the same tree I bound to?".

- **Request shape.** `X-Bunker-Tree: <token>` is accepted on **any** request. If present and it does not match the
  served tree, the request is refused with `409` + `stale_tree` (§5) — checked **before** the method executes.
- **Response shape.**
  - `X-Bunker-Rev: <opaque>` on every response, plus `bunkerd:rev` in `PROPFIND`. For a git tree the token is
    `git:<40-hex>`; otherwise a server-maintained monotonic counter. Callers treat it as opaque; the shape in force is
    named in the capability document (§4.2).
  - `X-Bunker-Tree: <opaque>` on every response, plus `bunkerd:tree` in `PROPFIND`.
  - `X-Bunker-Proto: HTTP/1.1|HTTP/2.0|HTTP/3.0` on every response: the version the server actually **observed**
    (`r.Proto`), so a downgrade is a visible fact rather than an assumption (§4.4).
- **Error shape.** `409` + `X-Bunker-Verdict: stale_tree` (body `b:stale-tree` with `b:expected` and `b:current`) —
  the server-side half of AC-7 (`PRD-bunker-fs.md:132`). The remedy is a re-bind, never an auto-adopt.
- **What a revision is not.** It is **not** a per-resource validator and **not** a content identity: a commit moves
  `rev` without changing any file's bytes, and a working-tree write changes bytes without moving `rev`. Per-resource
  identity is E-1 only. Confusing the two is the single most likely way for a client to build a wrong cache; that is why
  they are separate extensions with separate names.

### E-4 — Delegated whole-tree operations: `X-Bunker-Op`

**What it is.** `POST` with `X-Bunker-Op: <op>` and a JSON body. The operation is executed **on the agent** and its
result returned in one request, which is the only mechanism measured to make tree-wide work fast: the same 14 operations
that stall at a 45 s timeout over a serialized mount complete in **0.41 s flat** with git running on the host
(`PRD-bunker-fs.md:23–31`).

**Transport choice, and why.** `POST` because: RFC 4918 §9.5 leaves POST semantics server-defined; every client and
proxy understands it; a `GET` would make the operation look safe and cacheable (so an intermediate could serve a stale
tree snapshot); and `POST` is expressible today by the named consumer — Muster's CLI attaches a request body for
`POST`/`PUT`/`PATCH` and **not** for `PROPFIND` (measured: `PROPFIND` `bodyLen=0`, `internal/builtin/request.go:144–146`),
so a POST carrier is the one Muster can drive without a change. The price is that `POST` is neither safe nor idempotent
by definition; the mitigation is the invariant above — **every op is read-only**, so a retry is harmless. That invariant
is a test, not a promise (§11 A-9).

- **Request shape.**
  ```
  POST /dav/<path> HTTP/1.1
  X-Bunker-Op: status
  X-Bunker-Tree: <token>            (optional; mismatch ⇒ 409 stale_tree)
  X-Bunker-Max-Bytes: 1048576       (optional; server caps it)
  Content-Type: application/json

  {"path": "src", "short": true}
  ```
  The body supplies only structured arguments from a **fixed** vocabulary. There is no free-form command and no shell:
  each op maps to a fixed argv template. This is the "mount gives edits, never execution" rule made mechanical — an
  op cannot be turned into arbitrary execution because no field is ever interpolated into a command line.
- **Response shape (single-envelope ops).** One JSON object, identical keys on success and failure, so a consumer has
  exactly one shape to decode:
  ```
  HTTP/1.1 200 OK
  Content-Type: application/json; charset=utf-8
  X-Bunker-Op: status
  X-Bunker-Verdict: ok
  X-Bunker-Rev: git:9f2c1a…            X-Bunker-Tree: <token>            X-Bunker-Proto: HTTP/2.0

  {"ok": true, "op": "status", "verdict": "ok", "rev": "git:9f2c1a…", "tree": "<token>",
   "proto": "HTTP/2.0", "duration_ms": 37, "truncated": false,
   "result": { …op-specific… }, "error": null}
  ```
- **Op catalogue (the `result` shape is what a consumer codes against):**

| `op` | Body arguments | `result` shape |
|---|---|---|
| `capabilities` | — | the capability document (§4.2) verbatim, under `result.capabilities` |
| `status` | `path?`, `untracked?` | `result.entries`: `[{x, y, path, orig_path?, sub?}]` — the server parses the porcelain output, so no NUL/newline ambiguity crosses the wire; plus `result.count` |
| `diff` | `path?`, `staged?`, `stat_only?` | `result.text` (unified diff, or the `--stat` form), `result.truncated`; `result.bytes` |
| `rev-parse` | `rev?` (default `HEAD`) | `result.text` (one line), `result.rev` |
| `ls-files` | `path?`, `ignored?` | `result.entries`: `[{path, mode, sha, stage}]` (names + index metadata only, no file reads) |
| `snapshot` | `path?`, `depth` (`1`\|`infinity`), `include_hash` (default **false**) | `result.entries`: `[{path, type, size, mtime_unix_ms, mode, hash?}]` — one call replaces the refused `PROPFIND Depth: infinity` for our own client (and backs the FUSE `READDIRPLUS` collapse BFS-003 §3(d) describes); `hash` is present only when `include_hash` is true |
| `events` | `since_seq?` | `result.events`: the poll form of E-6 |
| `watch` | `paths?`, `since_seq?` | **streams** — see E-6; it is the one op whose response is not a single envelope |

- **Error shape (same envelope, non-2xx status):** `400` + `op_unknown` / `extension_op_missing` / `bad_arguments`;
  `409` + `stale_tree` or `not_a_repo` (an op that needs git on a tree that is not a git repository);
  `413` + `result_too_large` when the result exceeds `X-Bunker-Max-Bytes` even after the server's own cap;
  `501` + `capability_unavailable` when the op exists but this build lacks it. The envelope is always present, so a
  consumer's error path is the same decoder as its success path.

### E-5 — Capability discovery: `OPTIONS` + the capability document

**What it is.** The answer to "which extensions exist here?" — specified in §4 (acceptance criterion 3). It is the
extension *every other extension is discovered through*, and it is deliberately built out of standard parts.

- **Request shape.** `OPTIONS /dav/<path>` (standard, no extension header required) for the cheap summary; and
  `POST` + `X-Bunker-Op: capabilities` (E-4) with an empty body for the full document.
- **Response shape.** On `OPTIONS`: `200` with `DAV: 1` (`1, 2` when `LOCK` is live), `Allow`,
  `X-Bunker-Capabilities: <integer document version>`, `X-Bunker-Extensions: identity,if_match_refuse,rev,tree,op,watch`,
  `X-Bunker-Verdict: ok`, and the standard `Alt-Svc` while an h3 listener is up (exact bytes in
  §10.1). On the `POST`: the JSON document of §4.2 in the E-4 envelope. `OPTIONS *` answers without `DAV`
  (RFC 4918 §10.1), so per-URI discovery stays honest.
- **Error shape.** `501` + `capability_unavailable` (+ `X-Bunker-Capability: <name>;scope=<scope>;mode=<mode>`) when a
  caller asks for a capability this build/target/transport lacks — the client learns "not here" from a description, never
  from an absence of documentation. A `POST` whose body asks about a capability name that does not exist answers
  `400` + `op_unknown`; an unknown `surface` string in the document itself tells a consumer to **fail closed** rather
  than guess (§4.2 rule 3).
- **Deviation-free.** `OPTIONS` is a standard method and all four additions are ignorable header fields; a client that
  knows nothing about bunker reads `DAV`/`Allow` and behaves exactly as RFC 4918 requires.

### E-6 — The invalidation channel: `watch` with a declared poll mode

**What it is.** How the client learns that the agent's tree moved, without polling every path (AC-4,
`PRD-bunker-fs.md:129`). Server-side wire shape only; the client's use of it is BFS-005/BFS-009.

- **Request shape.** `POST` + `X-Bunker-Op: watch`, body `{"paths": ["src"], "since_seq": 41}`. The response is a
  **long-lived stream** of newline-delimited JSON objects (one per line, `Content-Type: application/x-ndjson`), which
  works identically over HTTP/1.1 chunked, HTTP/2 streams and HTTP/3 streams — the reason it is not HTTP/2 server push:
  `net/http` exposes a server-side `Pusher` (`net/http/h2_bundle.go:7050`, `PushOptions` at `http.go:187`) and **no
  client-side API to receive pushed responses** at all, so no Go client on either end of this link could consume push.
- **Response shape.**
  ```
  {"seq":42,"event":"invalidate","paths":["src/main.go"],"rev":"git:9f2c1a…","tree":"<token>"}
  {"seq":43,"event":"heartbeat","rev":"git:9f2c1a…"}          (keeps the stream alive; no path claims)
  {"seq":44,"event":"overflow","paths":[],"rev":"…"}          (watcher overran: drop everything, re-snapshot)
  ```
  `seq` is monotonic per tree; a client reconnects with `since_seq` and gets what it missed — which is also how the
  polling form works: `X-Bunker-Op: events` with the same argument returns the pending events as a single envelope and
  then closes.
- **Error shape, and the mode rule.** When the agent has no watcher, `watch` answers `501` +
  `capability_unavailable` with `scope=target`, `capability=watch` and **`mode=poll`** — the client then uses `events`
  or hash comparison, and `bunker fs status` reports the degradation (AC-9, `PRD-bunker-fs.md:134`). A server that
  silently served a heartbeat-only stream would be indistinguishable from a quiet tree; that is exactly the "silently
  different behaviour" this spec forbids.

---

## 4. Capability discovery, and what changes per HTTP version (acceptance criterion 3)

### 4.1 Discovery is three-layered, and the layers answer different questions

| Layer | Mechanism | Question it answers | Per-version? |
|---|---|---|---|
| **Transport** | TLS ALPN (`h2`) / the UDP QUIC handshake (`h3`) / neither (`http/1.1`) | "which HTTP version is this connection?" | **Yes** — this *is* the version. In-band for h1/h2; for h3 the client chooses the transport (measured: nothing in the Go stack consumes Alt-Svc — PROTO-010 §6.1, and BFS-002 §3 shows the advertisement is emitted but not followed). |
| **Surface** | `OPTIONS` → `DAV`, `Allow`, `X-Bunker-Capabilities`, `X-Bunker-Extensions` | "what does this resource offer?" | **No** — byte-identical on h1.1, h2 and h3. |
| **Extension detail** | `POST` + `X-Bunker-Op: capabilities` → the capability document | "exactly which extension versions, ops, limits and h3 authority?" | **No** — the *document* is version-independent; the `transports` block inside it reports what the current build and config can actually carry. |

A client must never infer capability from the negotiated version. That inference is the failure this section exists to
prevent: it is precisely how "HTTP/1.1 clients are second-class" creeps into a system whose owner's requirement is that
old clients keep working unchanged.

### 4.2 The capability document (JSON; the client's contract)

```json
{
  "surface": "bunkerd-webdav/1",
  "document_version": 1,
  "classes": ["1", "2"],
  "methods": ["OPTIONS","GET","HEAD","PUT","DELETE","MKCOL","PROPFIND","PROPPATCH","COPY","MOVE","LOCK","UNLOCK","POST"],
  "extensions": {
    "identity": {"name":"X-Bunker-Hash","v":1,"etag":"strong","alg":"sha256","format":"sha256:<64 hex>"},
    "if_match_refuse": {"name":"If-Match/If-None-Match","v":1,"methods":["PUT","DELETE","MOVE","COPY","PROPPATCH","LOCK"]},
    "rev":  {"name":"X-Bunker-Rev","v":1,"kind":"git"},
    "tree": {"name":"X-Bunker-Tree","v":1},
    "op":   {"name":"X-Bunker-Op","v":1,"read_only":true,
             "ops":["capabilities","status","diff","rev-parse","ls-files","snapshot","events","watch"],
             "default_max_bytes":1048576,"abs_max_bytes":16777216},
    "watch":{"name":"X-Bunker-Op: watch","v":1,"mode":"push","modes":{"push":"inotify→stream","poll":"X-Bunker-Op: events"}}
  },
  "transports": {
    "http/1.1": {"alpn":null,"multiplexed":false,"server_push":false,"available":true},
    "h2":       {"alpn":"h2","tls":true,"multiplexed":true,"server_push":false,"available":false},
    "h2c":      {"mode":"prior-knowledge","requires_opt_in":true,"upgrade_dance":false,"available":false},
    "h3":       {"alpn":"h3","transport":"quic/udp","requires_tls":"1.3","alt_svc":"h3=\":18080\"; ma=2592000","available":false}
  },
  "limits": {"propfind_depth": [0,1], "propfind_depth_infinity": false, "propfind_allprop_hashes": false,
             "max_request_bytes": 1073741824},
  "server": {"build": "0.1.4", "proto": "HTTP/2.0", "tree": "<token>", "rev": "git:9f2c1a…"},
  "degradations": []
}
```

Rules for this document:

1. **It reports reality, not aspiration.** `available: false` on the `h3` block means this *running* process has no h3
   listener (measured today: the live deployment is `tls.enabled: false`, so QUIC — which always encrypts — cannot be
   served at all, BFS-002 §2.3 and §4).
2. **Degradations are enumerated, not implied.** A missing watcher, a disabled build flag or an `h3` listener that failed
   to bind appears in `degradations` as
   `[{"capability":"watch","scope":"target","mode":"poll","detail":"inotify watcher absent"}]`, so `bunker fs status`
   and a remote client read the same fact (§5.2).
3. **A consumer must be able to fail closed on the document version**: an unknown `surface` value means "this spec
   changed shape", not "proceed anyway".
4. The document is fetched with `POST` (E-4) — the one carrier the named consumer can drive today (§3 E-4).

### 4.3 The per-version matrix

Rows are capabilities; the columns are the three HTTP versions. "Same" means literally the same status codes, headers
and bodies — verified by comparing one battery run per protocol (BFS-011).

| Capability | HTTP/1.1 | HTTP/2 (ALPN `h2`) | HTTP/3 (QUIC) |
|---|---|---|---|
| All §2 methods, all status codes, all properties | **yes** | **yes** | **yes** (measured on the consumer side: GET/PUT/**PROPFIND**/DELETE over h3 all return `HTTP/3.0` through an unmodified client — PROTO-010 §2.3, H3-1…H3-4) |
| All `X-Bunker-*` extension headers/properties | **yes** | **yes** | **yes** (headers are version-independent) |
| E-4 `X-Bunker-Op` ops (single envelope) | **yes** | **yes** | **yes** |
| E-6 `watch` stream | **yes, but it occupies the single connection** — a second TCP connection is required to keep other requests moving | **yes, cheap** — one stream among many on one connection | **yes, cheap** |
| Independent requests in flight on one connection | **1 (serialized)** — measured **1.0×** concurrency gain: 100 concurrent requests took 38.47 s, vs 38.47 s sequential (`PRD-bunker-fs.md:60`) | **many** — measured **25.0×**: 100 concurrent in 0.79 s, 7.88 ms/request = **2.4 %** of one 185.24 ms RTT (`:61,:66`) | many (QUIC streams); **no wall-time claim** — unmeasured on this path |
| Concurrency without h2 | Multiple TCP connections: rclone's WebDAV client got **11 of 14** operations with **4 concurrent connections** and no h2 at all (`:75–93`) | one connection instead of N; connection setup measured **1589 ms vs 278 ms** multiplexed (`:100`) | one connection; 0-RTT is the candidate benefit |
| Server push used by this surface | n/a | **no** (no Go client API to receive pushes) | **no** (same) |
| Cleartext | **yes** | only as **h2c prior-knowledge** (RFC 7540's `Upgrade: h2c` is **not** implemented by `net/http` — measured: the upgrade attempt returned HTTP/1.1, BFS-002 §4) | **impossible** — QUIC always encrypts, so h3 requires `tls.enabled: true` |
| h3 discovery | `Alt-Svc: h3=":<port>"; ma=2592000` on TCP responses, emitted only while an h3 listener is up | same header on h2 responses | n/a (the client is already there) |

**The consequences this document commits to (and BFS-006/BFS-007 must implement):**

- **C-1 — HTTP/1.1 is a first-class protocol, not a fallback.** Every method, property and extension in this spec works
  over h1.1. The only h1.1-specific fact is *cost* (one request at a time per connection). Nothing may be refused
  because the client is on h1.1.
- **C-2 — The ALPN list always contains `http/1.1`.** `net/http` enforces this in `adjustNextProtos`
  (`net/http/server.go:3533–3562`: it keeps only `http/1.1` and `h2`, appends whichever is missing, and deletes
  anything else). The only way to break the guarantee is to bypass the stdlib path with a hand-rolled listener — which
  the BFS-002 §6 sketch deliberately does not do. A test asserts it (§11 A-5).
- **C-3 — h3 never shares the TCP socket's ALPN.** Measured: a TCP handshake offering only `["h3"]` is refused
  (`tls: client requested unsupported application protocols (["h3"])`, BFS-002 §8 F2). Correct per RFC 9114, and it
  means an h3-only client has **no fallback** unless it retries over TCP — so our capability document must always name
  the TCP paths as available and the client owns the retry (§9.3).
- **C-4 — Alt-Svc is emitted only when the h3 listener is really up**, on h1/h2 responses, with the exact value
  measured in BFS-002 §3 — `h3=":<port>"; ma=2592000` (30 days, from quic-go's `generateAltSvcHeader`,
  `http3/server.go:388–399`; `SetQUICHeaders` at `:695`). Advertising an endpoint that is not listening is worse than
  not advertising one.
- **C-5 — h2c is never sold as "h2 for old clients".** It is prior-knowledge only, and a client that tries the RFC-7540
  upgrade silently gets h1.1 (measured). It stays an explicit opt-in for loopback/LAN and for our own client, which can
  be written to send the preface (BFS-002 §4).
- **C-6 — `X-Bunker-Proto` is on every response** (E-3), so a negotiated downgrade — an ALPN-stripping middlebox is
  BFS-002 §8's F4 — is a *visible* fact. The PRD's risk row already requires this: "a downgrade is a visible error,
  never silent" (`PRD-bunker-fs.md:285`).

### 4.4 What a client does when the version it wanted is not available

The sequence a client must implement (this is also the answer to "an older HTTP/1.1 client still works", acceptance
criterion 6):

1. Try the transport it prefers. h2 = TLS + ALPN `h2`; h3 = QUIC first, TCP as the retry.
2. Read the version **from the response**, never from configuration. `X-Bunker-Proto` (E-3) and `resp.Proto` agree on
   h1/h2; measured consumer-side evidence that this is the right source: a hand-built `http.Transport` *without*
   `ForceAttemptHTTP2` returns HTTP/1.1 even against an h2-capable server (PROTO-010 §3, C2), and the ALPN-suppressed
   control reports HTTP/1.1 on both sides (C6).
3. If the observed version is lower than requested, treat it as a **reported downgrade** and report it to the operator
   (`bunker fs status` / the Muster CLI). No silent acceptance, no retry storm.
4. Continue. Nothing in this spec is unavailable on h1.1 (C-1).

---

## 5. The refusal vocabulary (the row's "not supported here" vs "not in this version yet")

### 5.1 The table

Every refusal this surface can produce, with the status, the machine code, and where the code is carried. The code is
**always** in both places (§3's body-carried rule):

| Code (`X-Bunker-Verdict` / body element) | Status | When | Extra fields |
|---|---|---|---|
| `method_unknown` | `501` | the method token is not an HTTP or WebDAV method we recognise | — |
| `method_not_allowed` | `405` + `Allow` | a known method (e.g. `REPORT`, `ACL`, `PATCH`) this resource does not support | — |
| `not_implemented_yet` | `501` | a method that **is** part of this surface, but this build does not serve it yet (e.g. `LOCK`/`UNLOCK` before slice C4) | `capability`, `scope=build`, `phase=C4` |
| `capability_unavailable` | `501` | a declared extension capability absent here | `capability`, `scope`, `mode` (see §5.2) |
| `precondition_failed` | `412` | `If-Match`/`If-None-Match` failed without hash detail (e.g. `If-Match: *` on an unmapped URI) | — |
| `hash_mismatch` | `412` | the base hash did not match (E-2) | `X-Bunker-Current-Hash`, `X-Bunker-Expected-Hash`; body carries both |
| `identical_content` | `204` | the write was already satisfied (same bytes) — a **success**, reported explicitly | `X-Bunker-Noop: 1` |
| `body_hash_mismatch` | `422` | a `PUT` declared `X-Bunker-Hash` and the arriving bytes hash differently | `expected`, `received` |
| `stale_tree` | `409` | `X-Bunker-Tree` does not match the served tree (E-3) | `expected`, `current` |
| `not_a_repo` | `409` | an E-4 op needing git on a tree that is not a repository | `op` |
| `protected_property` | `403` (inside a `207`) | `PROPPATCH` on a live/computed property; carries the RFC's own `cannot-modify-protected-property` precondition element | the property name |
| `dead_properties_unsupported` | `403` (inside a `207`) | `PROPPATCH` set/remove of a dead property | the property name |
| `depth_required` | `400` | `PROPFIND` with no `Depth` header (the RFC default is the refused `infinity`; §2.1) | — |
| `invalid_depth` | `400` | `Depth` on a collection `DELETE` with any value but `infinity` (§2.3 deviation 4) | — |
| `lock_conflict` | `423` | an exclusive lock blocks the request; carries `no-conflicting-lock` / `lock-token-submitted` | the lock token |
| `lock_token_mismatch` | `412`/`409` | `LOCK` refresh or `UNLOCK` out of the lock's scope (RFC's `lock-token-matches-request-uri`) | — |
| `shared_locks_unsupported` | `403` | `LOCK` requesting `<D:shared/>` | scope=surface |
| `unsupported_media_type` | `415` | a `MKCOL` (or other) body of a type we do not support | — |
| `result_too_large` | `413` | an E-4 result exceeds the cap even after the server truncates | `bytes`, `cap` |
| `insufficient_storage` | `507` | the agent cannot store the representation (RFC's own code) | — |
| `internal` | `500` | anything unexpected | a correlation id; never a `200` |

### 5.2 `scope` — the field that separates the two diagnoses

`capability_unavailable` (and `not_implemented_yet`) always carries a `scope`, and the four values *are* the row's
distinction:

| `scope` | Meaning | Example | Stable? |
|---|---|---|---|
| `surface` | this surface will never serve it — a design boundary | shared locks; dead properties; `Depth: infinity` on `PROPFIND` | permanent |
| `build` | this build/version does not have it yet; the roadmap names the phase | `LOCK` before C4 (`not_implemented_yet` + `phase=C4`) | temporary, and the phase is named |
| `transport` | it cannot exist on the negotiated HTTP version | a push-mode `watch` on a cleartext h1.1 connection; h3 on a connection with no TLS | depends only on the negotiation |
| `target` | this agent/tree lacks the piece | no inotify watcher ⇒ `watch` with `mode=poll` (AC-9) | depends on the target's build |

The refusal must also name **`mode`** — the mode actually in force — whenever the caller might otherwise assume the
requested one. `mode=poll` for an absent watcher is the canonical case: the client is told what it is getting, so
"degraded" is never mistaken for "quiet".

### 5.3 Where the codes appear

- `X-Bunker-Verdict: <code>` — a response header, on **every** response, including successes (`ok`,
  `identical_content`), so one extraction point serves all outcomes.
- The body: `DAV:error` with a `urn:bunker:fs:1` child element for standard-method failures —
  the RFC's own vehicle for exactly this (§14.5: "Error responses, particularly 403 Forbidden and 409 Conflict,
  sometimes need more information"; "Any element that is a child of the 'error' element is considered to be a
  precondition or postcondition code"). Example, the AC-3 refusal's body:

  ```xml
  <?xml version="1.0" encoding="utf-8"?>
  <D:error xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1">
    <b:hash-mismatch>
      <b:expected>sha256:3f7a…9c21</b:expected>
      <b:current>sha256:1b2c…a7f0</b:current>
    </b:hash-mismatch>
  </D:error>
  ```
- For E-4 responses the body is the JSON envelope, whose `verdict` field carries the same code — so a JSON consumer
  never parses XML and an XML consumer never parses JSON.

---

## 6. The conflict rule: the exact exchange (acceptance criterion 4)

**The rule (D1/D3):** the identity of a resource is the hash of its bytes; a write declares the base hash it read; a
write whose base no longer matches, and which would *change* the bytes, is **refused** — the write is rejected, not
merged; and the file's content is left byte-identical. This is AC-3 (`PRD-bunker-fs.md:128`) and the PRD's conflict
loop (`:190`).

### 6.1 The exchange, verbatim

**1. The client holds a stale base.** Reader A fetched `/dav/src/main.go` and cached it with
`ETag: "sha256:3f7a…9c21"`. Writer B has since replaced the file (its hash is now `1b2c…a7f0`). A now writes with the
hash it read:

```http
PUT /dav/src/main.go HTTP/1.1
Host: bunker-mvp:18080
Content-Type: text/plain; charset=utf-8
Content-Length: 4096
If-Match: "sha256:3f7a…9c21"
X-Bunker-Hash: sha256:8d0e…44b7
```

**2. The server's algorithm, in order (no step is optional):**

1. Resolve the target path inside the workspace root; a path that escapes the root is refused before anything else
   (`workspace_invalid`, the confinement rule the house PRD fixes).
2. If `X-Bunker-Tree` was sent and does not match the served tree ⇒ `409 stale_tree`, stop.
3. Read the **current** resource bytes and compute their hash. (Computed from the bytes, never from an mtime, a size,
   or a cached record whose freshness is keyed on mtime.)
4. Evaluate the precondition: `If-Match` absent ⇒ unconditional (RFC 9110 §13.1.1 — a stock client is unaffected);
   `If-Match: *` ⇒ true iff the resource exists; `If-Match: "<tag>"` ⇒ true iff the tag equals the current hash
   (strong comparison); `If-None-Match: *` ⇒ true iff the resource does **not** exist.
5. If the precondition is **true**: hash the arriving body; when `X-Bunker-Hash` was declared and disagrees ⇒
   `422 body_hash_mismatch`, nothing written; else write atomically (temp file + rename) and answer `201`/`204` with the
   new `ETag`/`X-Bunker-Hash`.
6. If the precondition is **false**: hash the arriving body. If the resulting bytes **differ** from the current bytes
   ⇒ **refuse** (step 3 below). If they are **identical** ⇒ `204` + `X-Bunker-Verdict: identical_content` +
   `X-Bunker-Noop: 1`, **no disk write, mtime preserved, `ETag` unchanged** (D3; RFC 9110 §13.1.1's provision that a
   state-changing request which "appears to have already been applied" MAY be answered with a 2xx, and this is the
   touch-without-change case the PRD's risk row warns about).

**3. The refusal:**

```http
HTTP/1.1 412 Precondition Failed
Content-Type: application/xml; charset=utf-8
X-Bunker-Verdict: hash_mismatch
X-Bunker-Current-Hash: sha256:1b2c…a7f0
X-Bunker-Expected-Hash: sha256:3f7a…9c21
X-Bunker-Proto: HTTP/1.1
X-Bunker-Rev: git:9f2c1a…
X-Bunker-Tree: <token>
Content-Length: 342

<?xml version="1.0" encoding="utf-8"?>
<D:error xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1">
  <b:hash-mismatch>
    <b:expected>sha256:3f7a…9c21</b:expected>
    <b:current>sha256:1b2c…a7f0</b:current>
  </b:hash-mismatch>
</D:error>
```

**4. What the refusal guarantees (each is a test, §11):**

- The file's bytes are **unchanged** — `sha256sum` before == after (AC-3's own proof).
- Nothing was written anywhere: the temp file is removed before the response is sent; no partial file is ever visible
  under any path (`AC-6:131` requires no partial file even when the transport dies mid-request).
- The response is **not cacheable**: `Cache-Control: no-store` on refusals and on `X-Bunker-Op` responses, because `412`
  is heuristically cacheable by default for some clients and a cached refusal would be a lie about the current hash.
- The refusal names **both** hashes in the header and the body — the client can merge without a second round trip.
- The refusal is the same shape over h1.1, h2 and h3 (nothing about the rule is version-dependent).

**5. Recovery, as the protocol defines it:** re-read (`GET`/`HEAD` → new `ETag`), merge locally, retry with the new
base; if the same path keeps colliding, take a lock first (`LOCK`, which backs onto the lease registry,
`PRD-bunker-fs.md:217`) and the class stops recurring rather than nagging (the PRD's conflict loop, `:190`). A client
that cannot send a precondition at all (a stock client) never sees this path: absent `If-Match` is an unconditional
write per RFC, which is exactly the compatibility guarantee of §8.

**6. The same rule on the other writing methods:** `DELETE`, `MOVE`, `COPY` and `PROPPATCH` accept `If-Match` with the
same comparison; the same `412` + `hash_mismatch` shape applies, with the refused resource named in the body's `href`.

---

## 7. Identity: content hashes, never mtimes (acceptance criterion 5)

### 7.1 The rule

> **The identity of a resource is the hash of its bytes.** `ETag` is that hash. `If-Match` compares that hash. A cache
> entry is valid iff its hash equals the server's hash. `DAV:getlastmodified` is served because the RFC and stock
> clients expect it, but it is a display value: no decision in this surface may be taken from it.

### 7.2 Why mtimes are unusable here

| Reason | Evidence |
|---|---|
| **Under an active build the tree is touched constantly with unchanged bytes.** A tool that rewrites a file with identical content moves the mtime and changes nothing. An mtime-keyed rule refuses writes that are actually safe — and the PRD states the operational consequence plainly: the first thing anyone does with a nagging check is disable it. | The taxonomy class and the rule are stated in `PRD-bunker-fs.md:184` and `:235`; the risk row and its mitigation at `:283`. |
| **The kernel's default invalidation is mtime-driven, so an mtime-keyed surface would hand identity to the kernel.** Measured on a live go-fuse mount: the kernel offered BOTH `AUTO_INVAL_DATA` and `EXPLICIT_INVAL_DATA`; go-fuse's INIT reply selected **`AUTO_INVAL_DATA` and not `EXPLICIT_INVAL_DATA`**. The client must therefore opt in to explicit control deliberately. | BFS-003 §3(a) and §7.2, Appendix A.5 (verbatim INIT/OK lines). |
| **A wrong cache identity is expensive, measurably.** With OPEN replies carrying no data-cache flags, three sequential opens of the same 4 MiB file produced **32 READ requests each — 96 READs, 12,582,912 bytes — with no reuse at all**. Identity mistakes are paid in full, every time. | BFS-003 Appendix A.6. |
| **Bytes are what the caller asked for.** A hash is computed from the bytes the client will receive; an mtime is a claim about when they last changed, made by a filesystem we do not own (the agent's, where a build, a `git checkout`, or a `touch` all move it). | Structural, no measurement claimed. |

**The honest gap, stated rather than papered over:** the specific experiment "run a real build on the agent against a
git tree and count how many files are touched without changing bytes" was **not** run — BFS-003 §9.2 says so
explicitly. The decision above does not depend on its outcome, because the hash is the identity *by construction*
(the server reads the bytes to answer either way, so an mtime shortcut would be an optimisation that can only add a
way to be wrong). The experiment is carried as an open item for BFS-012 (O-7).

### 7.3 Where mtime may still appear

- `DAV:getlastmodified` and the `mtime_unix_ms` field in E-4's `snapshot` result: display and ordering metadata, never
  a validator.
- As a **private cache key** for the server's own hash cache: `(path, size, mtime, inode)` may key a cache whose value
  is a content hash. The mtime decides only whether we *recompute* something we could always recompute from bytes; it
  never decides whether a write is safe, and a metadata mismatch always falls back to hashing.

### 7.4 The cost of hashing, and how the surface stays affordable

Hashing is O(bytes), which is why it appears exactly where bytes are already moving and nowhere else:

- `GET`/`HEAD`: the file is being read anyway; the hash rides along. `HEAD` becomes the cheap "give me the base hash"
  call — **one request, no body transferred** — which is how our client obtains a write precondition for a path it has
  never read (the design point BFS-003 §3(c) flags: a FUSE syscall cannot carry "expected hash", so the client must
  fetch one; `HEAD` is the cheapest legal way).
- `PUT`: the body is being read anyway; the hash rides along and is returned.
- `PROPFIND`: hashing every member of a collection would make a single metadata call O(tree × bytes) — precisely the
  shape of the measured stall class. Therefore `bunkerd:hash` is **not** returned by `allprop`; it is returned only when
  a client names it in `<prop>` (or with `<include>`), which is the RFC's own mechanism for an expensive live property
  (§9.1: "`allprop` does not return values for all live properties"). `bunkerd:rev`/`bunkerd:tree` are cheap (one read)
  and *are* returned in `allprop`.
- The server keeps a hash cache keyed as in §7.3, so a `PROPFIND` naming `bunkerd:hash` over an unchanged tree does not
  re-read unchanged files.

---

## 8. The old-client path (acceptance criterion 6)

### 8.1 The claim

> **A plain HTTP/1.1 WebDAV client interoperates with this surface with no configuration.** It needs no knowledge of
> any `X-Bunker-*` header, no TLS trust setup in the current deployment, no capability negotiation, and no client-side
> extension. Nothing in the extension layer can make a standard request fail.

### 8.2 What makes it true (measured, not asserted)

| Mechanism | Measurement |
|---|---|
| A cleartext HTTP/1.1 listener exists and answers today | Live deployment `bunker-mvp`: `:18080 → http_version=1.1 code=200`; `--http2-prior-knowledge → code 000`; `https → 000`; `ss -tlnp`: `*:18080, *:19090` (BFS-002 §2.3) |
| HTTP/1.1 keeps working in every configuration measured | Cleartext: `curl` default, `--http1.1`, and even the ignored h2c-upgrade attempt all returned `http_version=1.1 / 200`. TLS: `curl --http1.1 → 1.1 200` while the same server negotiates `h2` for a client that offers it (BFS-002 §2.2 A/B, §7) |
| A client that sends **no ALPN at all** is still served | Measured: `No ALPN negotiated`, after which the server answered HTTP/1.1 (BFS-002 §2.2 B) |
| The TCP ALPN list can never lose `http/1.1` | `net/http`'s `adjustNextProtos` keeps `http/1.1` and `h2`, appends whichever is missing, deletes anything else (`net/http/server.go:3533–3562`) — which is also why `h3` can never be negotiated on the TCP port by accident (BFS-002 §7, §8 F2) |
| The h3 listener cannot break h1.1 by existing | It is a different transport (UDP); its presence or absence cannot change the TCP socket's behaviour, and it is only announced via `Alt-Svc`, which old clients ignore (BFS-002 §7) |
| Extension headers are ignorable | Every `X-Bunker-*` **request** header is optional; the only ones validated are the enumerated ones (`X-Bunker-Op`, `X-Bunker-Tree`, `X-Bunker-Hash`, `X-Bunker-Max-Bytes`). An unrecognised `X-Bunker-*` header not in this vocabulary is **ignored**, per HTTP's field-name rule, so another client's or a future client's header cannot break an old one. Conversely, an unrecognised **value** of an enumerated header is refused loudly (`400 op_unknown`) — asking for a specific thing and getting a different thing is the silent-difference failure this spec forbids. |
| Failure bodies do not require extension knowledge | Standard-method failures use `DAV:error` (RFC 4918 §14.5); a client that ignores bodies still sees the correct status code. `X-Bunker-Verdict` is an ignorable header. |

### 8.3 The four declared deviations an old client can observe

Listed once in §2.3 and again here because a compatibility battery must test them deliberately, and because they are
the complete list:

1. `GET`/`HEAD` on a collection ⇒ `405` + `Allow`.
2. `PROPPATCH` of a dead property ⇒ `403` inside a `207` (`dead_properties_unsupported`).
3. `PROPFIND Depth: infinity` ⇒ `403` + `propfind-finite-depth`.
4. Wrong `Depth` on a collection `DELETE` ⇒ `400` + `invalid_depth`.

Each is loud, atomic and standard-shaped; none can silently change a result.

### 8.4 The interop ceiling that must be named (not a defect, a limit)

The OS-native Windows WebDAV redirector is **deprecated and off by default**, and its attribute budget is capped at
**1 MB**, with a separate limit for files larger than **50,000,000 bytes** (BFS-003 §4, quoting MS Learn). "Old clients
keep working" is therefore a claim about WebDAV **clients** — davfs2, rclone, Finder, `curl` — and only conditionally
about that one component. A `207` describing a large collection is where the 1 MB budget can bite; this spec does
**not** truncate a `207` (a truncated multistatus is a silently wrong answer), it documents the ceiling and leaves the
remedy to the caller (`Depth: 0` recursion, or E-4's `snapshot`). See O-4.

### 8.5 Anti-gaming requirement for the compatibility proof

The battery must use a client that **genuinely lacks** our extensions. Testing compatibility with our own client is
circular and passes trivially — BFS-012's own row says this, and it is repeated here because it is the difference
between a proof and a formality. `curl` (raw verbs) plus at least one stock WebDAV client (`rclone`'s WebDAV backend is
already proven on this link: **11 of 14** operations, `PRD-bunker-fs.md:86`) is the minimum; the tests must assert the
standard status codes, not our headers.

---

## 9. Muster: the wire contract this document is for (acceptance criterion 7)

### 9.1 The agreement, stated explicitly

Muster's `PROTO-012` (SPEC: the HTTP/2 + HTTP/3 support matrix, stage contract and gates) names this document as the
server half of its cross-repo contract — its own board row says so: *"Cross-repo contract: the server side of this
agreement is bunker's BFS-004 (WebDAV surface + per-version capability negotiation), so these two specs must agree on
the wire shape"* (PROTO-012 row). Muster's `PROTO-014` (h2 transport) and `PROTO-015` (h3 transport) depend on that
spec.

> **This document agrees to serve HTTP/1.1, HTTP/2 and HTTP/3 clients on the same port number.** HTTP/1.1 always (and
> it is a first-class protocol here, not a fallback). HTTP/2 over TLS via ALPN `h2`, plus h2c prior-knowledge as an
> explicit opt-in. HTTP/3 over QUIC on the same port number, advertised with `Alt-Svc`, **conditional on
> `tls.enabled: true` and quic-go being vendored** (BFS-007) — because QUIC always encrypts, h3 cannot exist in the
> current cleartext deployment (BFS-002 §4 and §2.3), and this document will not promise a transport the config cannot
> carry.

**The two extension points PROTO-012 consumes:**

1. **E-5 — capability discovery and per-version negotiation** (§4). PROTO-012's mandate is that a request which cannot
   use h2/h3 is *a reported fact, not a silent downgrade*; that is exactly what §4.3's matrix, §4.4's read-the-version-
   from-the-response rule and C-6's `X-Bunker-Proto` echo deliver. PROTO-014/015 need precisely two things from the
   server for their gates: a peer that negotiates the version they claim, and a response that lets them *assert* the
   version actually used.
2. **§5 — the refusal vocabulary, body-carried.** PROTO-010 measured that a caller cannot trust a header-only answer on
   the consumer side: Muster's response cache rebuilds 2xx GET responses, **forges `Proto` to `HTTP/1.1` and drops every
   header except `Content-Type`** (`pkg/client/cache.go:110–126`, PROTO-010 §6). A machine-readable verdict that lives
   only in a header is therefore consumed by nobody. §5.3 makes every verdict travel in the body as well — that is the
   second thing PROTO-012 codes against, and the reason it is in this document rather than in the Muster spec alone.

**Explicitly not needed by Muster** (so nobody assumes a dependency that does not exist): E-4's delegated operations
(they serve our own surfaces — `bunker fs status` and the CLI, per BFS-003 §3(d)'s correction), E-6's invalidation
channel (that is our client's), and E-1's `bunkerd:hash` property (a stock client gets the same identity for free from
the `ETag`; E-1 exists for our client's convenience and for the explicit algorithm name).

### 9.2 The wire facts both sides must agree on

Left column: this document's ruling. Right column: the independently measured consumer-side fact, with its source.

| Wire fact | Server side (this document) | Consumer-side measurement |
|---|---|---|
| ALPN `h2` / `http/1.1` is in-band and truthful | `X-Bunker-Proto` echoes `r.Proto`; the ALPN list always contains `http/1.1` (C-2) | Plain client + stdlib: client `HTTP/2.0` == server `HTTP/2.0`; ALPN-suppressed control: `HTTP/1.1` on both sides (PROTO-010 §2.1, C1/C3/C4/C6) |
| Configuration is not enough to get h2 | The server does not care how the client is configured; it answers what ALPN negotiated | A hand-built `http.Transport` without `ForceAttemptHTTP2` reports `HTTP/1.1` even against an h2 server (PROTO-010 §2.1, C2) |
| h3 requires TLS 1.3 | h3 is advertised and served only when TLS is on (C-4) | `MaxVersion=TLS1.2` ⇒ `CRYPTO_ERROR 0x150 (local): tls: no supported versions satisfy MinVersion` (PROTO-010 Appendix B, `seamprobe` 3c) |
| h3 is UDP-only | One port number, two transports; the UDP socket is separate and can be absent | Cleartext request to an h2c-capable server ⇒ `HTTP/1.1` (Appendix B, `seamprobe` 3a); h3 client against a TCP-only endpoint fails, and an h2 client against an h3-only endpoint fails (PROTO-010 §6.3, X-1) |
| `Alt-Svc` is the only h3 advertisement, and nothing auto-consumes it | Emitted on TCP responses whenever the h3 listener is up, value `h3=":<port>"; ma=2592000` (BFS-002 §3, measured) | A server sending `Alt-Svc: h3=":443"; ma=86400` reaches the caller as a literal header and **nothing acts on it**; GOROOT grep for `Alt-Svc` in `net/http` = 0 hits (PROTO-010 §6.1) |
| WebDAV verbs work over every version | The full method set is served on all three versions | PUT / PROPFIND / DELETE / GET over h3 all returned `HTTP/3.0`, 1 MiB PUT intact; PROPFIND over h2 likewise (PROTO-010 §2.3 H3-1…H3-4; §7 for the 1 MiB PUT timings) |
| A `PROPFIND` body must never be required | Empty-bodied `PROPFIND` = `allprop` (RFC 4918 §9.1 MUST), and extension properties are available by name for clients that can send a body | Muster's CLI attaches a body only for `POST`/`PUT`/`PATCH`; a `PROPFIND` reaches the wire with `bodyLen=0` (`internal/builtin/request.go:144–146`, PROTO-010 §8) |
| The version must be assertable, not assumed | `X-Bunker-Proto` on every response + `proto` in the E-4 envelope | PROTO-010's own gate shape (§5.3): assert `resp.Proto` **and** the server-observed `r.Proto`, plus an http/1.1-only control cell |

### 9.3 What the Muster side must do for this agreement to be testable

Named so the two specs cannot drift silently — these are the consumer-side items PROTO-010 already filed:

1. **The cache must stop forging `Proto`/dropping headers** (PROTO-010 §9 item 1) or PROTO-014/017 cannot satisfy "a
   caller can see which version was used". Until then, the server-side `X-Bunker-Proto` echo is not readable on a
   cached 2xx GET — which is why §5.3 also carries verdicts in bodies and why E-4 envelopes repeat `proto`.
2. **`PROPFIND`/`MKCOL`/`LOCK` bodies must be sendable** (PROTO-010 §9 item 3) before Muster can request
   `bunkerd:hash` by name, or drive MKCOL/LOCK with the bodies RFC 4918 gives them. (Not a server requirement: this
   surface must not *require* a body — §9.2 — so Muster's gap is not a blocker for the basic paths.)
3. **h3 discovery is the client's choice, not automatic.** Neither stdlib nor quic-go consumes `Alt-Svc`; a client that
   wants h3 either opts in explicitly or implements a small `Alt-Svc` cache against the value in this document's
   capability block (§4.2).

---

## 10. Wire shapes, collected (what an implementer and a consumer copy from)

### 10.1 `OPTIONS` — discovery

```http
OPTIONS /dav/ HTTP/1.1
Host: bunker-mvp:18080
```
```http
HTTP/1.1 200 OK
Allow: OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, PROPFIND, PROPPATCH, COPY, MOVE, POST
DAV: 1
X-Bunker-Capabilities: 1
X-Bunker-Extensions: identity,if_match_refuse,rev,tree,op,watch
X-Bunker-Verdict: ok
X-Bunker-Proto: HTTP/1.1
X-Bunker-Tree: <token>
X-Bunker-Rev: git:9f2c1a…
Alt-Svc: h3=":18080"; ma=2592000        ← present only while an h3 listener is up
```
`DAV: 1, 2` (and `LOCK, UNLOCK` in `Allow`) once slice C4 has landed. `OPTIONS *` answers `200` **without** the `DAV`
header (RFC 4918 §10.1: "many WebDAV servers do not advertise WebDAV support in response to `OPTIONS *`"), so
per-URI discovery stays honest.

### 10.2 `PROPFIND Depth: 1` — a collection with extension properties

```http
PROPFIND /dav/src HTTP/1.1
Depth: 1
Content-Type: application/xml; charset=utf-8

<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1">
  <D:prop><D:getetag/><D:getcontentlength/><D:resourcetype/><b:hash/><b:rev/></D:prop>
</D:propfind>
```
```http
HTTP/1.1 207 Multi-Status
Content-Type: application/xml; charset=utf-8
X-Bunker-Verdict: ok

<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1">
  <D:response>
    <D:href>/dav/src/</D:href>
    <D:propstat>
      <D:prop><D:resourcetype><D:collection/></D:resourcetype><b:rev>git:9f2c1a…</b:rev></D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
    <D:propstat>
      <D:prop><D:getetag/><D:getcontentlength/><b:hash/></D:prop>
      <D:status>HTTP/1.1 404 Not Found</D:status>   ← properties that do not apply to a collection
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/dav/src/main.go</D:href>
    <D:propstat>
      <D:prop>
        <D:getetag>"sha256:1b2c…a7f0"</D:getetag>
        <D:getcontentlength>4096</D:getcontentlength>
        <D:resourcetype/>
        <b:hash>sha256:1b2c…a7f0</b:hash>
        <b:rev>git:9f2c1a…</b:rev>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>
```
Notes an implementer must honour: per-property `404` is not an error for the request (RFC 4918 §9.1.2); a member's
failure gets its own `propstat`; the response never carries a truncated member list (a lie by omission). An
**empty-bodied** `PROPFIND` is the `allprop` request and must be accepted (`bunkerd:rev` and `bunkerd:tree` appear;
`bunkerd:hash` does not — §7.4); a `PROPFIND` with **no `Depth` header** is `400 depth_required` (§2.1).

### 10.3 `PROPPATCH` — atomic, per-property refusal

```http
PROPPATCH /dav/src HTTP/1.1
Content-Type: application/xml; charset=utf-8

<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:z="urn:example:1">
  <D:set><D:prop><z:colour>blue</z:colour></D:prop></D:set>
  <D:set><D:prop><D:displayname>Source</D:displayname></D:prop></D:set>
</D:propertyupdate>
```
```http
HTTP/1.1 207 Multi-Status
X-Bunker-Verdict: protected_property

<D:multistatus xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1" xmlns:z="urn:example:1">
  <D:response>
    <D:href>/dav/src</D:href>
    <D:propstat>
      <D:prop><z:colour/></D:prop>
      <D:status>HTTP/1.1 403 Forbidden</D:status>
      <D:error><b:dead-properties-unsupported/></D:error>
    </D:propstat>
    <D:propstat>
      <D:prop><D:displayname/></D:prop>
      <D:status>HTTP/1.1 424 Failed Dependency</D:status>   ← all-or-nothing (§9.2)
    </D:propstat>
  </D:response>
</D:multistatus>
```
The live-property case is identical except the code is `protected_property` inside the `propstat` `D:error`, carrying
the RFC's own `D:cannot-modify-protected-property` element, not the bunker one.

### 10.4 E-4 envelope, error and success

```json
{"ok":false,"op":"snapshot","verdict":"capability_unavailable","rev":"git:9f2c1a…","tree":"<token>",
 "proto":"HTTP/1.1","duration_ms":2,"truncated":false,"result":null,
 "error":{"capability":"snapshot","scope":"build","phase":"C5","mode":"poll",
          "detail":"snapshot op not in build 0.1.4"}}
```
```json
{"ok":true,"op":"ls-files","verdict":"ok","rev":"git:9f2c1a…","tree":"<token>","proto":"HTTP/2.0",
 "duration_ms":11,"truncated":false,
 "result":{"count":150,"entries":[{"path":"src/main.go","mode":"100644","sha":"1b2c…a7f0","stage":0}]},
 "error":null}
```

### 10.5 E-6 `watch` stream

```http
POST /dav/ HTTP/1.1
X-Bunker-Op: watch
Content-Type: application/json
Transfer-Encoding: chunked

{"paths":["src"],"since_seq":41}
```
```http
HTTP/1.1 200 OK
Content-Type: application/x-ndjson
X-Bunker-Verdict: ok
X-Bunker-Tree: <token>

{"seq":42,"event":"invalidate","paths":["src/main.go"],"rev":"git:9f2c1a…","tree":"<token>"}
{"seq":43,"event":"heartbeat","rev":"git:9f2c1a…","tree":"<token>"}
{"seq":44,"event":"overflow","paths":[],"rev":"git:9f2c1a…","tree":"<token>"}
```
And its declared-degradation form when the agent has no watcher:

```http
HTTP/1.1 501 Not Implemented
X-Bunker-Verdict: capability_unavailable
X-Bunker-Capability: watch;scope=target;mode=poll
Content-Type: application/json

{"ok":false,"op":"watch","verdict":"capability_unavailable","error":{"capability":"watch","scope":"target",
 "mode":"poll","detail":"inotify watcher absent on this agent"}}
```

---

## 11. Acceptance criteria for the implementers (derived from this spec)

Each is a test with a negative control, because a green result without one proves nothing here.

| # | Criterion | Proof | Owner |
|---|---|---|---|
| A-1 | Every row of §2.1 returns exactly the status and code it names; every refusal carries `X-Bunker-Verdict` in the header **and** the body | table-driven live battery over the method matrix; a negative cell per refusal proving the atomic claim (destination/resource byte-identical after) | BFS-006/BFS-012 |
| A-2 | `DAV: 1` on every `OPTIONS` response; `DAV: 2` **only** when `LOCK` is live and enforcing; `OPTIONS *` carries no `DAV` | live `OPTIONS` + an `OPTIONS *` request; a build without C4 asserts the absence of `2` (the negative control that stops a class being advertised for free) | BFS-006/BFS-012 |
| A-3 | The extension shapes in §3/§10 are byte-exact: property names, header names, JSON keys, XML namespace | a golden-artifact comparison per extension | BFS-006 |
| A-4 | AC-3's exchange: two writers, one stale hash ⇒ `412`, both hashes in header and body, `sha256sum` before == after; and the identical-bytes case ⇒ `204 identical_content` with mtime **unchanged** | the two-arm test from §6; the second arm is the touch-without-change rule | BFS-006/BFS-012 |
| A-5 | The TCP ALPN list always contains `http/1.1`; a client sending no ALPN is served; TCP + ALPN `["h3"]` is refused (not silently served as h1) | the table-driven ALPN matrix with its negative controls (BFS-002 §8 F2's own recipe), plus an assertion that `http/1.1` is present in the effective list | BFS-006 |
| A-6 | **Same battery, three protocols**: the same request set over h1.1, h2 and h3 returns identical statuses, bodies and headers modulo the version echoes; the observed version is asserted on **both** sides (client `resp.Proto` **and** server `r.Proto`) | the three-run comparison + the anti-gaming control that a hand-built transport without `ForceAttemptHTTP2` **fails** the h2 assertion | BFS-011 |
| A-7 | Old-client interop with a client that genuinely lacks our extensions: stock-verb battery (PROPFIND 0/1, GET, HEAD, PUT, DELETE, MKCOL, OPTIONS) with no configuration and no `X-Bunker-*` knowledge | raw `curl` verb battery + a stock WebDAV client (rclone's WebDAV backend is already measured at 11/14 on this link); assert standard status codes only | BFS-012 |
| A-8 | Capability absence is structured: `LOCK` before C4 ⇒ `501 not_implemented_yet` with `phase`; `Depth: infinity` ⇒ `403 propfind-finite-depth`; no watcher ⇒ `501 capability_unavailable` with `scope=target, mode=poll`; no path returns `200` with different behaviour | the four negative cells, each asserting the code, the scope and the mode | BFS-006/BFS-009 |
| A-9 | E-4's read-only invariant: every op leaves the tree byte-identical (including the index) — the property that makes a retried `POST` harmless | snapshot the tree (and `.git` index digest) before/after each op; a mutation control (an op deliberately shelling out to a mutating command) must fail the assertion | BFS-006 |
| A-10 | The capability document reports reality: with the h3 listener down, `transports.h3.available == false` and no `Alt-Svc` is emitted; with it up, both flip | two-arm live test on the running daemon | BFS-007 |
| A-11 | No truncated answers: no `207` is ever truncated (the refused alternative), and E-4 truncation is always reported in `truncated` + the envelope | a large-collection `PROPFIND` and a large `diff` with a small `X-Bunker-Max-Bytes` | BFS-006 |

---

## 12. Boundaries — what this spec does not do

- **No code.** This row's deliverable is the decision record; BFS-006/BFS-007 implement it.
- **No second lock system.** `LOCK`/`UNLOCK` back onto the in-tree lease registry (`<git-common-dir>/agent-leases.json`,
  `PRD-bunker-fs.md:217,:248`), keyed by tree — never by mount or agent.
- **No execution.** `X-Bunker-Op` maps to fixed argv templates for a read-only op catalogue; there is no free-form
  command field, so the surface cannot be turned into an execution path (the house PRD's "the mount gives edits, never
  execution").
- **No content-addressed URLs.** Identity travels as `ETag`/`X-Bunker-Hash`; the URL namespace stays path-shaped, so a
  stock client's assumptions about paths hold.
- **No dead-property store.** See deviation 2 and O-1.
- **Not a standalone file-sharing product.** This surface exists to serve the filesystem path; it is not offered as a
  general WebDAV server (`PRD-bunker-fs.md:311`).
- **No auth design.** Credentials and their partitioning are the house PRD's; the requirement this document places on
  it is only that a stock client must be able to authenticate without any extension (see O-6).

---

## 13. Open decisions (acceptance criterion 9)

Everything above is decided. These are the places this document could not decide from the evidence, each with a
recommendation and the reason. They are the only inputs an owner needs to change any of it.

| # | Question | Recommendation | Reason |
|---|---|---|---|
| **O-1** | Dead properties: permanent refusal, or an out-of-tree sidecar store keyed by tree identity? | **Refuse (as specified).** Add a sidecar store only when a real consumer needs one. | RFC 4918 §9.2 makes dead properties a `SHOULD`, and the only cheap place to keep them is inside the served tree, where a metadata file would appear in the tree's own `git status` — the fidelity this product exists to protect. A sidecar keyed by tree UUID avoids that but is new storage with its own consistency problems, for a feature no measured consumer needs. |
| **O-2** | Should `PROPFIND Depth: infinity` ever be allowed (e.g. behind a server flag with a hard member cap)? | **No in v1.** Revisit only if BFS-012 finds a stock client that cannot recurse with `Depth: 1`. | The RFC permits the refusal with its own precondition code; the measured cost of tree walks is the stall class; and our own clients have a better tool (E-4 `snapshot`). A member cap would introduce the one thing this spec forbids: an answer that is not the answer (a partial listing presented as a listing). |
| **O-3** | Collection `COPY`/`MOVE` with `Depth: infinity`: an internal time limit, and which status on expiry? | Support them unbounded in v1; if a limit is ever needed, answer `503` + `Retry-After` with `X-Bunker-Verdict: operation_timeout`. | The work happens on the agent in one request (the same reason delegation exists), and no measurement yet shows a collection copy exceeding a sane time. Inventing a limit before the measurement repeats the mistake BFS-002 §9 and the PRD's AC-13 both refuse to make. |
| **O-4** | The Windows redirector's 1 MB attribute budget on a large collection: unbounded `207`, a member cap, or a documented ceiling? | **Document the ceiling** (as in §8.4); no cap, no truncation. Add a cap only if BFS-012 can run a Windows host. | No Windows host was available for BFS-003 (its §4 is upstream-source evidence, not a live probe). Truncating a `207` is worse than a large one — it is a false answer — and `Depth: 0` recursion is the standard escape. |
| **O-5** | Which port carries HTTP/3 — the REST/WebDAV port, the gRPC port, or its own? | **Same port number as the WebDAV TCP listener** (`Alt-Svc: h3=":18080"`), and name it in the capability document regardless. | BFS-002 leaves this as an owner call (§9.5). One port to expose, one port to firewall, and the header value the measurement already produced (`h3=":<port>"; ma=2592000`) points at it. The capability field means a non-default choice is still discoverable. |
| **O-6** | Authentication scheme a stock client uses: Basic over TLS only, or also Digest? | **Basic over TLS** in v1; Digest not required. | Every target client supports Basic over TLS; digest adds server state and complexity for a threat (cleartext credential capture) that TLS already closes. The credential model itself belongs to the house PRD and is untouched here. |
| **O-7** | The unmeasured premise behind D1: how often does an active build touch files without changing bytes? | Run the experiment in BFS-012 (build on the agent under a real git tree, count touch-without-change events). | BFS-003 §9.2 says explicitly it was not run. It does not change the decision (the hash is the identity by construction), but it is the number that justifies the *cost* of hashing — so it should be measured rather than assumed. |
| **O-8** | Strict refusal vs idempotent success when the base hash is stale **and** the body is byte-identical to the current content (D3's second case). | **Keep the idempotent `204`** (+ `X-Bunker-Noop`). Flip to `412` with the same code if the owner prefers a single rule. | Refusing a write that would change nothing is exactly the nagging behaviour the PRD names as the reason people disable a safety check (`:283`), and RFC 9110 §13.1.1 explicitly permits the 2xx form. It never applies a change and it is reported in band, so no lost update is possible either way. |
| **O-9** | Should a non-git tree still expose `X-Bunker-Rev` as a monotonic counter, or omit the header? | **Expose a counter**, with `rev.kind` in the capability document. | A client cache wants "did anything move?" on every tree; omitting the header would force a client to special-case the tree type, and the header is ignorable by clients that do not care. |
| **O-10** | Do our writes make an identical-content `PUT` skip the disk write entirely (preserving mtime), or write and let the mtime move? | **Skip** (as specified in §6.1 step 6). | It is the concrete instance of the touch-without-change class, it keeps `DAV:getlastmodified` from moving for a no-op, and it costs one hash comparison we have already computed. The alternative makes our own surface produce the noise the identity rule exists to avoid. |
| **O-11** | Collection locks (`LOCK` with `Depth: infinity`): do they map onto a subtree lease in the existing registry, or is a collection lock refused? | **Map them if the registry can express a subtree lease; otherwise refuse loudly and advertise `DAV: 1` only.** Decide at C4 with the registry's actual semantics measured, not assumed. | RFC 4918 §9.10.3 makes the Depth header a MUST for a LOCK-supporting resource, and §18.2 makes class 2 a package deal — so a resource that cannot honour a depth lock must not advertise class 2. The lease registry's per-path/one-holder shape (proven: 6 processes → 1 grant / 5 refusals, `PRD-bunker-fs.md:248`) is exactly right for a file lock and unproven for a subtree lock; this document will not advertise a class it cannot enforce, and it will not invent a second lock manager to make the class easy (the PRD forbids that). |

---

## Appendix A — every number used in this document, and where it was measured

Nothing here is estimated. Each row cites the measurement's own artifact; the reproduction commands live in those
documents' evidence indexes (`BFS-002 §10`, `BFS-003 Appendix A`, `PRD-bunker-fs.md:19–31,37–43,75–86`).

| Fact | Value | Source |
|---|---|---|
| RTT control host → dedi-2 (Helsinki) | **185.24 ms** (mdev 0.16 ms) | `PRD-bunker-fs.md:21` |
| RTT control host → bunker-mvp (Falkenstein) | **~198 ms** | `:22` |
| sshfs battery: completed / stalled at 45 s | **5 of 14** completed; **7**(dedi-2) / **8**(mvp) stalled | `:23–25` |
| sshfs per-op times | `rev-parse` 4.52/4.11 s; `log -1` 10.32/10.12 s; `log -20` 7.93/7.92 s; `ls-files` 2.72/2.41 s; `status`/`diff --stat`/`commit`/`checkout -b` STALL (rc=124) | `:26–30` |
| Same 14 operations with git **on** the host | **14/14, 0.41 s flat, 0 stalls** | `:31` |
| HTTP/1.1, one connection, 100 concurrent | **38.47 s** (384.67 ms/req) — **1.0×** | `:60` |
| HTTP/2 over TLS, one connection, 100 concurrent | **0.79 s** (7.88 ms/req) — **25.0×**, **2.4 %** of one RTT, **49×** per request | `:61,:66` |
| rclone WebDAV backend (no h2; 4 concurrent TCP connections) | **11 ok / 1 stall**; `commit` 1.04 s, `amend` 0.71 s, `rebase` 0.41 s, `checkout -b` 2.23 s; `diff --stat` STALL 45.09 s | `:75–86` |
| rclone mount variants (SFTP) | wedge: off **1 h 49 m**, full **900 s** zero rows, `--sftp-concurrency 128` **900 s** zero rows; `vfs cache: cleaned: objects 0` | `:40–42` |
| Connection setup | **1589 ms** per connection vs **278 ms** multiplexed (**5.7×**) | `:100` |
| Cleartext listener (live deployment) | `:18080` → `http_version=1.1 code=200`; h2c prior-knowledge → `code 000`; `https` → `000`; `tls: {enabled: false, insecure_dev: true}` | BFS-002 §2.3 |
| h2c | prior-knowledge → `HTTP/2.0 200`; `Upgrade: h2c` → `HTTP/1.1 200` (not implemented by `net/http`) | BFS-002 §4 |
| TLS ALPN matrix | `h2`→h2; `h2,http/1.1`→h2; `http/1.1`→http/1.1; no ALPN → "No ALPN negotiated" then HTTP/1.1 | BFS-002 §2.2 B |
| One port, two transports | TCP **and** UDP listening on the same port in one process (`ss -tlnp` + `ss -ulnp`); Go h3 client → `HTTP/3.0 200` | BFS-002 §3 |
| Alt-Svc header value | `h3=":2443"; ma=2592000` (30 days) | BFS-002 §3 |
| h3-over-TCP | refused: `tls: client requested unsupported application protocols (["h3"])` | BFS-002 §8 F2 |
| HTTP/3 dependency cost | **+2 modules** (quic-go v0.63.0 + qpack v0.6.0), binary **+2,175,060 B = +2.074 MiB (+10.26 %)** at `CGO_ENABLED=1`; no cgo; no toolchain bump | BFS-002 §5 |
| `adjustNextProtos` | keeps only `http/1.1` + `h2`, appends whichever is missing, deletes the rest | BFS-002 §7; `net/http/server.go:3533–3562` |
| Live FUSE INIT (kernel offer vs go-fuse reply) | kernel 7.45 offered `AUTO_INVAL_DATA`, `EXPLICIT_INVAL_DATA`, `WRITEBACK_CACHE`; go-fuse answered 7.28 and took `AUTO_INVAL_DATA`, **not** `EXPLICIT_INVAL_DATA` | BFS-003 §7.2, Appendix A.5 |
| Read accounting, one 4 MiB file, 3 sequential opens | **32 READs per open, 96 total, 12,582,912 bytes**, no reuse | BFS-003 Appendix A.6 |
| Write granularity | 4,194,304 bytes → **1023 WRITE ops**, per-op 94–8,106 bytes | BFS-003 Appendix A.6 |
| go-fuse protocol floor | major 7, minor ≥ **12** (= Linux **2.6.31**); notify floors `INVAL_INODE`/`INVAL_ENTRY` 7.12, `STORE_CACHE` 7.15, `DELETE` 7.18 | BFS-003 §6, Appendix A.7 |
| Release constraint | `linux/amd64`, `linux/arm64`, `CGO_ENABLED=0` | BFS-003 §2; `Makefile:14,95–105` |
| Lease registry | `<git-common-dir>/agent-leases.json`; CHT-007 probe: 6 processes → 1 grant / 5 refusals | `PRD-bunker-fs.md:248` |
| Windows redirector | deprecated; not started by default; **1 MB** attribute budget; 50,000,000-byte file KB | BFS-003 §4 |
| Muster: h2/h3 through its unmodified client | h2 PUT 1 MiB → 200 in **5 ms**; h3 PUT 1 MiB → 200 in **7 ms**; PROPFIND/DELETE over h3 → `HTTP/3.0 200` | PROTO-010 §7 (PUT timings), §2.3 (verb matrix) |
| Muster: h2/h3 negotiation baselines | client `HTTP/2.0` == server `HTTP/2.0` (nil transport, `ForceAttemptHTTP2`, injected transport); hand-built transport without `ForceAttemptHTTP2` = `HTTP/1.1`; ALPN-suppressed control = `HTTP/1.1` | PROTO-010 §2.1 (C1–C4, C6) |
| Muster: h3 requires TLS 1.3 | `MaxVersion=TLS1.2` → `CRYPTO_ERROR 0x150 (local)` | PROTO-010 Appendix B (`seamprobe` 3c) |
| Muster: nothing consumes `Alt-Svc` | server sent `Alt-Svc: h3=":443"; ma=86400`; caller sees the literal header; `grep Alt-Svc net/http/*.go` = **0 hits** | PROTO-010 §6.1 |
| Muster: cache forges the version | 2xx GETs report `Proto=HTTP/1.1` and lose every header except `Content-Type` on miss **and** hit | PROTO-010 §4.3; `pkg/client/cache.go:110–126` |
| Muster: `PROPFIND` body | reaches the wire with `bodyLen=0` (bodies attached only for POST/PUT/PATCH) | PROTO-010 §8; `internal/builtin/request.go:144–146` |
| RFC texts quoted | RFC 4918 §9.1, §9.2, §9.3–9.11, §10.1, §10.2, §10.4, §10.7, §11, §14.5, §18.1–18.3; RFC 9110 §13.1.1, §15.5.6, §15.5.13, §15.6.2 | fetched 2026-09-26 from `rfc-editor.org` |

---

## Appendix B — how this document relates to the documents it inherits

| Document | Relationship |
|---|---|
| `PRD-bunker-fs.md` | Design authority. This document **implements the contract for** its server-surface table (`:209–220`), and **decides** three things it left open: the identity rule's mechanics (D1), the `Depth: infinity` refusal (D2) and the identical-content case (D3). Where the PRD and this document appear to differ, the PRD's *intent* governs and this document's *mechanism* applies — the PRD owns the goals (AC-1…AC-13), this document owns the wire shape. |
| `BFS-002` | Transport evidence. Its §7 old-client guarantee becomes §8 of this document; its §3/§4 Alt-Svc and h2c measurements become C-3/C-4/C-5; its §8 failure modes become the negative controls in §11. Its §9 open questions are carried as O-5 and in §4.3's "unmeasured" cells. |
| `BFS-003` | Client evidence. Its measured kernel-default behaviour is the strongest argument for D1; its §3(c) "base hash for a path never read" is answered in §7.4 (`HEAD`); its §3(d) correction — delegation serves our own surfaces — is why E-4 is **not** claimed as a Muster dependency; its §4 Windows notes become §8.4 and O-4. |
| Muster `PROTO-010` | Consumer evidence. Its measured wire facts are mirrored in §9.2, its filed cache/body findings become §9.3, and its cross-repo clause is what §9.1 answers. |
| `docs/mount-drivers.md`, `internal/mountdriver` | Unaffected: this is the server surface, not the launch path. The mount driver that speaks it is BFS-008. |
