# BFS-018 — a symlink round-trips through the mount, and is never a file holding its target path

**Row.** `BFS-018` (P1, DEFECT, found by dogfooding). **Tree.** `wt/BFS-018`. **Server + client.** Every claim below is
from a run whose transcript is committed beside this file; the instrument is `BFS-018-arms.sh` and the mutations are in
`BFS-018-mutations/`.

---

## 0. Verdict in one screen

The row's sentence — *"a symlink appears as a regular file whose content is the link target"* — is now false, on both
sides of the wire, and it is false in a way that **fails loudly rather than plausibly** wherever the surface cannot act.

| Question | Answer |
|---|---|
| Does the wire carry any signal that the entry is a symlink? | **Partly — and this decided the design.** The E-4 `snapshot` op has always declared it (`"type":"symlink"`, `"size":5`, `"mode":"0777"`) and the client *threw it away*. `PROPFIND` carried **nothing**, and worse, its named-entry arm described the link as its **target** (`getcontentlength=26`, the target's ETag) while its child-entry arm described the link itself (`5` bytes) — two carriers disagreeing and neither saying "symlink". `GET` dereferenced (`200`, the target's 26 bytes). |
| (a) a declared attribute, or (b) a loud refusal? | **(a)**, with (b)'s discipline kept everywhere the surface cannot act. Because the type signal already existed on one carrier and the vocabulary is already declared, (b) alone would have been a refusal of something the surface can describe — and it would have thrown away the headline use case (a git work tree) rather than fixing it. |
| Which carriers changed? | `PROPFIND` gains the declared live properties `b:type` (every entry) and `b:link-target` (links only), and stops describing the *named* entry from a `Stat`; the E-4 snapshot entry gains the additive `link_target`; `GET`/`HEAD` on a link is refused `405 symlink_not_a_file`; `PUT` + `X-Bunker-Link-Target` (empty body) creates a link; a body-bearing `PUT` onto a link is refused `409 symlink_undeclared_replace`. The capability document declares `extensions.symlink` v1 (spec `docs/spec/BFS-004-webdav-surface.md` §3 E-7, deviations 5 and 6, and three new verdict codes in §5.1). |
| Rounded-trip or refusal — measured? | **Round-trip**: `ls -l` through the mount prints `lrwxrwxrwx … link-to-a -> a.txt`; `readlink` returns the target; `ln -s` through the mount lands a real symlink on the server; `git checkout -f HEAD` **through the mount** of a tree carrying two links returns `rc=0` and `git status --porcelain` is **empty**. |
| Is the corruption path closed? | Yes, on both sides. The mount's type bit is `S_IFLNK` (the kernel resolves the link, so `>`/`>>` reach the target and never replace the link), and the **server** refuses the write-back shape itself (`409` + the link's current target), so even a client that never learned the extension cannot replace a link with a file. |
| What is refused by name, with a named cause? | `GET`/`HEAD` on a link (405 `symlink_not_a_file`); a body-bearing `PUT` onto a link (409 `symlink_undeclared_replace`); a bad declared target (400 `symlink_target_invalid`); a **hardlink** (`EOPNOTSUPP` — the wire has no inode or link count, so it cannot be represented); an entry whose declared type this client cannot present (`EIO`, never a regular file); a link operation against a surface that never declared the extension (`EOPNOTSUPP`, and **nothing is sent**); a link whose target the surface did not publish (`EOPNOTSUPP`, never an invented target). Every one of them is counted and named in the mount's own report. |
| Neighbouring semantics (BFS-020/021/025/030/033/038)? | Unchanged, with their own cells run and counted: `BFS-018-neighbouring-cells.txt`. |

---

## 1. The RED, reproduced verbatim

Two independent reproductions: the dogfooding flow (mount a work tree, `ls -l`, `git status`) and the raw wire probe.

**(i) The filed line, live, with the base binaries** (`BFS-018-red.txt`, the `ls -l` block):

```
-rw-rw-r-- 1 kara kara   26 Sep 28 19:23 a.txt
-rwxrwxrwx 1 kara kara    5 Sep 28 19:23 link-to-a
```

A **regular file** (type `-`), mode **0777**, size **5** — 5 being `len("a.txt")`. Not the link, and not the 26-byte
target content either: a third, wronger thing. The same run's `readlink` returns nothing —
`readlink link-to-a -> '' (rc=1)` — and `ln -s` through the mount is `Operation not supported`.

The dogfooding flow in one paragraph, with the base client (`BFS-018-red.txt`): replace the name with a regular file
through the mount, `git status` reports **` T link-to-a`** and **` T vendored/sibling-link`** — the type change the
developer never made — then `git checkout -f HEAD` **through the mount** fails with
`error: unable to create symlink link-to-a: Operation not supported`, *and the two links are now gone from the tree*
(` D` in the status, and the server's directory listing has no links left). That is the corruption, end to end.

**(ii) What the wire carries for that entry** (`BFS-018-wire-probe.txt`, base surface, the client's own request bodies):

| Carrier | Base surface | Fixed surface |
|---|---|---|
| `OPTIONS` `X-Bunker-Extensions` | `identity,if_match_refuse,rev,tree,op,watch` | `…,watch,symlink` |
| `PROPFIND Depth: 0` on the link (the client's exact body, which asks for `b:type`/`b:mode`/`b:mtime`/`b:hash`) | `D:getcontentlength = 26` (**the target's size**), `D:getetag` = the **target's** hash, and `b:type`/`b:mode`/`b:mtime` answered **404** | `D:getcontentlength = 5` (the link's own size), `<b:type>symlink</b:type>`, and no `getetag`/`b:hash` (404 — a link has no content hash) |
| E-4 `snapshot` entry | `{"path":"link-to-a","type":"symlink","size":5,"mtime_unix_ms":…,"mode":"0777"}` | `{…,"mode":"0777","link_target":"a.txt"}` |
| `GET` on the link | `200`, `Content-Length: 26` — the **target's bytes** under the link's name (a copy) | `405` + `X-Bunker-Verdict: symlink_not_a_file` (+ `Allow`) |
| `GET` on the link's **target** (control) | `200`, 26 bytes | `200`, 26 bytes — unchanged |

Two facts about the base surface that this probe found and the fix removes, neither of them in the row as filed:

* **the carriers disagreed**: the child entry in a `Depth: 1` listing described the link (`5` bytes, `0777`, no hash)
  while the *named* entry in the same listing described its target (`26` bytes, the target's ETag) — because the named
  entry was built from `os.Stat` and the children from `os.Lstat`;
* **`GET` dereferenced**: asking for the link delivered the target's content, i.e. a copy of another resource under
  this name, with no way for the caller to tell.

The client's own wire capture (`BFS-018-wire-base-client.txt`) shows the same thing from the client's side: the mount's
one-call `snapshot` at bind received `"type":"symlink"` and the client's next `ls -la` printed a regular file — the
signal was on the wire and the decoder dropped it (`fsclient/normaliseKind` did not exist; `type` was folded into
`IsDir`).

---

## 2. The (a) vs (b) decision, from the evidence

**Decision: (a) — the surface carries symlinks as a declared, versioned attribute — and every operation the surface
cannot act on is refused loudly instead of substituted.** The argument is in the table above, and it turns on three
facts:

1. **The type vocabulary already existed and was already declared** on the E-4 carrier (`type` per entry, in the spec's
   own result shape). The defect's first half was therefore not "the wire cannot say it" but "the client discarded what
   the wire said" — and (b) alone would have refused an operation the surface already describes.
2. **`PROPFIND` carried nothing**, and it is the fallback carrier for every client that binds against a build without
   the op. So the attribute had to be *added* there: `b:type` (all entries) and `b:link-target` (links), both
   request-only, never in `allprop`, so a stock client's response is byte-identical. This is the genuine extension of
   the wire, and it is declared (spec §3 E-7) and versioned (capability document `extensions.symlink` v1).
3. **`GET` had a third answer available — dereference — and it is wrong.** A link is not its target: serving the
   target's bytes under the link's name is a *copy*, and a copy is exactly what corrupts a tree on the next write-back.
   So `GET`/`HEAD` on a link is refused `405 symlink_not_a_file` and the target travels in the declared property
   instead. This is the one place where (b)'s discipline is applied to a **read**.

Where the surface genuinely cannot act, the refusal is named rather than faked — the whole list is in §0's last table,
and each has a control (`BFS-018-live-through-the-mount.txt`, `BFS-018-mutations.txt`). Nothing on this surface
materialises a link target as content, on any path: the target is only ever a declared argument (a request header) or a
declared property (a response field), never bytes.

**What the row's "do not invent a mechanism to make the choice easy" cost me:** the honest statement of the residual.
Against a surface that publishes no `b:type` — a build predating this extension — the client *cannot* distinguish a
link from a file, and no inference is available (no standard property expresses a link; both guesses are corruptions).
That arm is measured, not hidden: `BFS-018-old-surface.txt` mounts the **new** client against the **base** server — the
link is typed correctly (from the snapshot's `type`, which the base server always sent), its target is refused by name
(`READLINK … published no target for it … the read is refused`), `ln -s` is refused with nothing sent, the mount reports
`declared=false … refusals_total=4`, and `materialised=0`.

---

## 3. What changed

**Server** (`internal/server/webdav/`)
* `props.go` — the shared Lstat classifier `entryTypeOf`; the live properties `b:type` on every entry and
  `b:link-target` on links; `PROPFIND` describes the **named** entry from its `Lstat` (the two carriers can no longer
  disagree); a link reports `getcontentlength` = its own size and **no** `getetag`/`b:hash`.
* `handler.go` — `GET`/`HEAD` refuse a link (`405` + `symlink_not_a_file` + `Allow`); `PUT` + `X-Bunker-Link-Target`
  creates a link through the **same** staged-sibling + one-rename publication a staged body uses (BFS-020/038's
  publication point, under the same per-path commit lock, with the commit-time re-validation); a body-bearing `PUT`
  onto a link is refused `409` naming the link's current target (BFS-033: nothing lands, proven by the cell);
  `hashIfRegular` is Lstat-based so a link's precondition hash is *none* rather than its target's.
* `ops.go` — the snapshot entry carries `link_target` for links (additive; `type`/`size`/`mode` unchanged), and the
  capability document declares `extensions.symlink`.
* `verdict.go` — `symlink_not_a_file`, `symlink_undeclared_replace`, `symlink_target_invalid`.

**Client** (`internal/fsclient/`, `internal/fsmount/`)
* `fsclient.Node.Kind` (`file`|`dir`|`symlink`, `unknown`, or unreported) and `Node.LinkTarget`, populated from **both**
  carriers; an unreported type resolves to the standard file/collection distinction and **never** to a link; an unknown
  type is `KindUnknown` and `KindHonoured()==false`, so a type this build cannot present is refused rather than
  described (the class the row is about, one type over). A link never keeps a content hash.
* `modeOf` renders `S_IFLNK | 0777`; `fillAttr` reports the link's own size; `Getattr`/`Lookup` refuse an unhonoured
  kind with `EIO` and a named cause.
* `node.Readlink` answers the **declared** target (zero round trips from the node tree, one `PROPFIND Depth: 0` when it
  is not held) and refuses by name when the surface published none — it never fetches the path's bytes.
* `node.Symlink` / `Mount.createLink` create through the declared header; a surface that never declared the extension
  is refused `EOPNOTSUPP` with **no request sent** (a build that ignored the header would answer `201` for a file).
* `node.Link` refuses hardlinks, argued from the wire: one entry per path, no inode and no link count, so two names for
  one inode would get two independent metadata and cache records and a write through one name would leave the other
  stale. `EOPNOTSUPP` is what Linux answers for a filesystem without hard links, and `ln` reports it as such.
* `bunker fs status` (and the status document) gains the `symlink` block — the surface's own declaration, the readlink
  and create counts, and a bounded ring of the most recent refusals.

**Spec** (`docs/spec/BFS-004-webdav-surface.md`) — §3 **E-7** (the type vocabulary, both read carriers, the write
shape, the precondition limit, the "fail closed" rule and the named residual); §2.3 deviations **5** and **6**; §2.2's
property table; §5.1's verdict table; §4.2's capability document; §8.3; §10.1/§10.2's wire shapes.

---

## 4. The round-trip and the TYPE assertion (live)

From `BFS-018-live-through-the-mount.txt` — a real mount, a real work tree carrying two links (`link-to-a -> a.txt`,
`vendored/sibling-link -> ../a.txt`), everything through the kernel:

```
=== [live] ls -l through the mount (the TYPE line) ===
-rw-rw-r-- 1 kara kara   26 Sep 28 19:23 a.txt
lrwxrwxrwx 1 kara kara    5 Sep 28 19:23 link-to-a -> a.txt
drwxrwxr-x 2 kara kara 4096 Sep 28 19:23 vendored
…
lrwxrwxrwx 1 kara kara    8 Sep 28 19:23 sibling-link -> ../a.txt
=== [live] readlink through the mount ===
readlink link-to-a -> 'a.txt' (rc=0)
readlink vendored/sibling-link -> '../a.txt' (rc=0)
```

**The assertion is the TYPE, not the target string.** `l` in the first column is `S_IFLNK`; the unit cell asserts
`symlinkThroughTheMountIsALinkNotAFile` on `out.Mode & syscall.S_IFMT == S_IFLNK` *and* on the size
(`len("a.txt") == 5`, explicitly `!= len(target content)`), and the mutation that removes the `S_IFLNK` branch turns
exactly that cell RED with the filed shape (`mode 0100644, size 5`). A correct `-l` line through this mount is
`lrwxrwxrwx … <len(target)> <name> -> <target>`; the `0777` is the convention for a link's permission bits (Linux
ignores them — there is no `chmod` on a link), and reporting that same octet under `S_IFREG` is precisely what was
filed.

The ordinary POSIX surface through the same mount (`BFS-018-posix-and-transparency.txt`):

```
cat link-to-a                     -> hello from the source tree            rc=0
cat sub/up-link                   -> hello from the source tree            rc=0
stat -c %F link-to-a              -> symbolic link                         rc=0
readlink -f link-to-a             -> /…/mnt/a.txt                          rc=0
find -type l / find -type f       -> the two links / the two files
ln -s a.txt new-link              -> rc=0, readlink 'a.txt', cat follows it
printf ' MORE' >> link-to-a       -> rc=0 (the kernel followed to a.txt; the LINK stayed a link)
```

and the transparency control, which is the comparison that matters for the one shape the surface refuses:

```
printf 'rewritten' > a.txt        -> Operation not supported  (rc=2)   ← BFS-030's pre-existing in-place-rewrite rule
printf 'rewritten' > link-to-a    -> Operation not supported  (rc=2)   ← the SAME answer: the link is transparent
printf x > tmp.new && mv tmp.new a.txt -> rc=0; a.txt = 'rewritten'; link-to-a STILL -> a.txt
```

---

## 5. The corruption path, closed at both ends

**Through the mount** (`BFS-018-live-through-the-mount.txt`): replace the link with a regular file *through the mount*,
then let git restore it through the mount.

```
=== [live] the type change, made THROUGH the mount ===
rm link-to-a rc=0
wrote a regular file over the name rc=0
[live] server: now a regular file
=== [live] git status through the mount (the type change a developer never made) ===
 T link-to-a
[live] status lines before checkout: 1
=== [live] git checkout -f HEAD THROUGH the mount ===
checkout rc=0
=== [live] git status through the mount, after the checkout ===
[live] status lines after checkout: 0
[live] SUMMARY ls_type=l server_kind=symlink status_after=0 materialised=0 checkout_rc=0
```

`git status` **clean** after the checkout, the server holding real symlinks again, and the materialisation scan (a walk
of the server's tree looking for a regular file whose content is a link target) is **0**. The base client on the same
flow cannot even perform the checkout (`rc=1`, "unable to create symlink … Operation not supported") and loses the
links — that is the defect, live, side by side.

**At the server** (the cell, mutation-controlled): a body-bearing `PUT` onto a link is refused `409` +
`symlink_undeclared_replace` naming the link's current target, the link is still a link with its **old** target, and the
target file's bytes are unchanged (sha256 compared before/after) — so a client that never learned the extension, or a
stale one that still holds the materialised pseudo-file, cannot corrupt the tree through this surface. After `DELETE`
the same `PUT` lands (`201`), which is the control that the refusal is about the **link type**, not about writing.

---

## 6. Refusals, each with its control

| Refusal | Where | Its control |
|---|---|---|
| `405 symlink_not_a_file` on `GET`/`HEAD` of a link | server cell + wire probe | the link's **target** still reads (`200`, 26 bytes) and an ordinary file still reads |
| `409 symlink_undeclared_replace` on a body-bearing `PUT` onto a link | server cell | after `DELETE` the same `PUT` lands; nothing landed on the refusal (link intact, target bytes identical) |
| `400 bad_arguments` — a body with `X-Bunker-Link-Target` | server cell | a link create with an empty body and a valid target is `201` |
| `400 symlink_target_invalid` — empty / oversize / NUL target | server cell | a single-space target (legal, if useless) is created |
| `EOPNOTSUPP` on a link create against an **undeclared** surface | mount cell + `BFS-018-old-surface.txt` | the same create on a declared surface succeeds; no request is sent on the refusal (request counter unchanged) |
| `EOPNOTSUPP` on `READLINK` when the surface published no target | mount cell + `BFS-018-old-surface.txt` | the same read against a surface that publishes the target returns it (zero round trips from the node tree) |
| `EOPNOTSUPP` on `Link` (hardlink) | mount cell | the refusal is *reported*: `refusals_total=1` with the named cause; the attribution cell stays green |
| `EIO` on any operation over an entry whose declared type this client cannot present | mount cell | ordinary entries in the same tree keep their modes, sizes and hashes (the attribution cell) |

Every refusal is recorded in the mount's own report — `BFS-018-old-surface.txt`:

```
symlink      : declared=false type_property=- target_property=-
             : readlinks_total=0 created_total=0 refusals_total=4
  refusal    : READLINK link-to-a: this surface declares the entry a symlink but published no target for it (b:link-target / link_target absent): the link cannot be read without inventing a target, so the read is refused
  refusal    : SYMLINK created: this surface did not declare the symlink extension (extensions.symlink), so it cannot be told to create a symlink; creating a file whose content is the target path is refused instead
```

and on the fixed surface the same block reports the work that *was* done
(`declared=v1 (X-Bunker-Link-Target)`, `readlinks_total=10`, `created_total=1`, `refusals_total=0`).

---

## 7. Negative controls: one mutation per cell, attribution green, byte-identical restore

`BFS-018-mutations.txt` (instrument: `BFS-018-arms.sh mutations`). Each mutation is applied to the source, the tree is
rebuilt, **its** cell must go RED, the **attribution cell** must stay GREEN, the file is restored from a byte copy and
the sha256 must match, and the cell must be GREEN again after the restore.

| # | Mutation | Its cell goes RED with | Attribution cell |
|---|---|---|---|
| 1 | the surface stops classifying a symlink (`entryTypeOf`) | `b:type = "file" … want "symlink"` | GREEN |
| 2 | the snapshot entry stops carrying `link_target` | `snapshot link_target = "" … want "a.txt"` | GREEN |
| 3 | `GET` stops refusing a link | `GET on a link -> 200, want 405` | GREEN |
| 4 | a body-bearing `PUT` over a link stops being refused | `PUT body over a link -> 204, want 409` | GREEN |
| 5 | **the original defect**: the client resolves a declared symlink to a file | `the mount reports the link as a REGULAR FILE (mode 0100644, size 5) — the defect as filed` | GREEN |
| 6 | `modeOf` stops rendering `S_IFLNK` | `the mount reports the link as a REGULAR FILE …` | GREEN |
| 7 | the link target is sent as the **body** instead of the declared header | `the server holds file (target ""); want a symlink to "a.txt"` | GREEN |
| 8 | a refusal stops being counted | `refusals_total = 0, want 1: the refusal is not reported` | GREEN |

All eight restores are sha256-identical (the hashes are in the transcript). The attribution cells are
`TestSymlinkChangeAttributionCell` (server: a collection is still a collection, a file still carries its hash and its
bytes' hash, `allprop` gains nothing) and `TestSymlinkChangeMountAttributionCell` (mount: an ordinary file's type, size,
mode and hash are unchanged and no link activity is reported when none happened).

---

## 8. What this does not do (residuals, named)

1. **A surface that publishes no `b:type`** (a build predating E-7): the mount cannot distinguish a link from a file on
   the `PROPFIND` fallback path and reports `declared=false` rather than guessing. Measured in
   `BFS-018-old-surface.txt`. This is the one hole the wire genuinely cannot close, and it is a named degradation, not
   a silent one.
2. **`MOVE`/`COPY` with overwrite onto a link** still replaces it (an explicit tree operation — the deliberate remedy
   the `409` names for a caller that wants a file there). Not refused, by decision: refusing it would break the
   lock-rename shape atomic writers use.
3. **Hardlinks** are refused rather than represented (§3).
4. **No xattrs, no `Mknod`, no FIFOs** — out of the row's scope; an entry of such a type arriving from a future surface
   is refused by name (`EIO`) rather than presented as a file.
5. **Two new declared deviations** for a stock client (§2.3 5 and 6): a `GET` of a link is now refused instead of
   dereferenced, and a body-bearing `PUT` onto one is refused. Both are loud, both are enumerated in the spec, and
   neither fires for any request that does not name a symlink.

---

## 9. Files and how to re-run

```
docs/evidence/BFS-018-arms.sh                    the instrument: live | red | old-surface | mutations | suites | all
docs/evidence/BFS-018-mutations/mutate-*.py       one mutation per cell (exact-match rewrites)
docs/evidence/BFS-018-red.txt                     the filed defect, live, with the BASE binaries
docs/evidence/BFS-018-live-through-the-mount.txt  the acceptance: type, readlink, the type change, the checkout, clean status
docs/evidence/BFS-018-wire-probe.txt              what the wire carries per carrier, base vs fixed (raw request/response)
docs/evidence/BFS-018-wire-base-client.txt        the base client's own captured wire (it received type:"symlink")
docs/evidence/BFS-018-wire-fixed-client.txt       the fixed client's captured wire (it sends b:link-target and the header)
docs/evidence/BFS-018-posix-and-transparency.txt  the ordinary POSIX surface + the transparency control
docs/evidence/BFS-018-old-surface.txt             the new client against the base surface (the named residual)
docs/evidence/BFS-018-mutations.txt               the mutation table with restores and attribution
docs/evidence/BFS-018-neighbouring-cells.txt      BFS-020/021/025/030/033/038 cells, counted
```

```sh
REPO=<tree> BIN=/tmp/bfs018/bin bash docs/evidence/BFS-018-arms.sh all
```

The base binaries the `red` and `old-surface` arms compare against are built from the commit this row started at
(`644499d`) into `/tmp/bfs018/base-repo` (`git clone --shared`, read-only on the source repo). Every arm bounds its
mounts, fuses and `fusermount3` calls with `timeout` and kills by explicit PID.
