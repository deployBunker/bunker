# Bunker — Agent Isolation Boundary Specification (GAP-075)

**Status:** implemented
**Scope:** per-agent `/tmp` isolation (SSH sessions and transient systemd units),
the cross-agent shared-scratch exchange point, and the host `/tmp` tmpfs bound.

---

## 1. Problem

Before this change every agent shared one isolation class with the host and with
each other:

* `/tmp` was the host's `/tmp`. root's temporary files and every agent's
  temporary files lived in the same directory in the same mount namespace, so
  agents could see (and collide with, and pre-create over) each other's and
  root's files; a predictable temp path was a cross-agent write primitive.
* `TMPDIR=/run/bunker/<id>/tmp` was set for agents, but a per-agent directory in
  a **shared mount namespace** is not a boundary: it only redirected tools that
  honour `TMPDIR`, and it was unbounded (a plain directory on the root
  filesystem, capped only indirectly by `LimitFSIZE`).
* Agents had no sanctioned way to exchange files. Whatever they invented for it
  landed in `/tmp` or in a home directory, both of which are outside any
  declared policy.

`TMPDIR=/run/bunker/<id>/tmp` is therefore **superseded** by an enforced private
`/tmp`. This document defines what replaced it and exactly what the replacement
does and does not guarantee.

## 2. Guarantees

| # | Guarantee | Mechanism |
|---|-----------|-----------|
| G1 | An agent's `/tmp` is private to that agent's session | `pam_namespace` per SSH session, activated ONLY for sessions whose user name matches the reserved agent pattern (`bunker-*`) — never for ordinary operator sessions |
| G2 | root keeps the host's own `/tmp` | root is not an agent name, so the PAM classifier jumps it over the whole managed block; `root` is additionally excluded in `/etc/security/namespace.d/50-bunker-agents.conf` |
| G2b | Ordinary (non-agent) SSH sessions keep the host `/tmp` | the classifier's `user !~ bunker-*` test succeeds for them and jump 2 skips the verifier and the module; the host-wide sshd stack is otherwise untouched |
| G3 | Every transient systemd unit an agent runs under has its own `/tmp` | `--property=PrivateTmp=yes` on the rootless-dockerd unit and on every detached `RunAgent` unit |
| G4 | The only sanctioned cross-agent exchange point is `/srv/bunker-share` | root-owned, group `bunker-agents`, setgid; one directory per agent |
| G5 | Cross-agent exchange is bounded per agent, enforced by the kernel | each agent directory is a `tmpfs` mounted with `size=<cap>` |
| G6 | A scratch directory that cannot be bounded is not created at all | fail-closed provisioning (mountpoint removed on mount failure) |
| G7 | The host's own `/tmp` tmpfs is bounded | systemd drop-in for `tmp.mount` + non-destructive remount; `/etc/fstab` is never modified |
| G8 | A broken boundary is a loud failure, never a silent shared `/tmp` | an agent session reaches `pam_namespace` ONLY after the root-owned `pam_exec` precondition has proved the exact Bunker rule, the group membership, the instance parent and its own integrity; any failure (including a deleted helper, a deleted/wrong/malformed drop-in, a missing group or a lost membership) DENIES the session |
| G8b | Group drift cannot fail OPEN | the agent/non-agent decision comes from the reserved `bunker-*` NAME pattern (`pam_succeed_if user !~`), not from group state, so a deleted group makes an agent session FAIL, not look like an operator session |
| G8d | A classifier that cannot express the agent-name pattern is refused, not installed | the installer probes `pam_succeed_if.so` for the glob test (`fnmatch`/`noglob`, Linux-PAM ≥ 1.6) BEFORE writing the block, because an unknown `!~` attribute under `default=die` would deny every session on the host |
| G8c | Bunker never edits a rule it does not own | PAM ownership is marker/block-based: only the marked block (or Bunker's own classifier-started block) is ever stripped; a foreign bare or optioned `pam_namespace` rule and a foreign `pam_succeed_if` rule survive install, repair and uninstall byte for byte |
| G9 | Disabling the exchange point does not weaken `/tmp` isolation | group membership is provisioned for every agent at spawn, independently of `shared_scratch_enabled` |

## 3. Design

### 3.1 SSH sessions — `pam_namespace`

`bunker exec`, `bunker cp`/`deploy` (scp), `bunker mount` (sshfs/sftp) and the
docker SSH transport all open **sshd sessions**. sshd sessions do not run under a
systemd unit, so `PrivateTmp=` cannot reach them. The kernel/Linux-PAM facility
for this is `pam_namespace`: at session setup — as root, before the user is
switched to — it gives the session its own mount namespace and bind-mounts an
instance directory over `/tmp`.

Bunker attaches it to the **sshd service**, scoped to agent sessions:

```
/etc/security/namespace.d/50-bunker-agents.conf     (new file, ours alone)
/etc/pam.d/sshd                                     (one marked session BLOCK, appended last)
/var/lib/bunkerd/agent-tmp/<bunker-agent-id>         (per-agent instance dir, mode 0700)
```

The drop-in contains exactly one rule:

```
/tmp  /var/lib/bunkerd/agent-tmp/  user:noinit  root
```

* `user` — one instance directory per user name.
* `noinit` — the distribution's `/etc/security/namespace.init` is not run, so
  nothing outside Bunker populates an instance directory.
* `root` (field 4) — polyinstantiation is **not** performed for root. This is
  *defense in depth only*: the field can list **user names only**
  (`namespace.conf(5)`), so it cannot express "members of the agent group" and
  cannot track agents that are created and destroyed dynamically. Scoping is
  therefore done in the PAM stack (below), not here.
* `/var/lib/bunkerd/agent-tmp` is mode `0000` (pam_namespace refuses a wider
  instance parent by default; the module is not given
  `ignore_instance_parent_mode`, which would weaken the isolation).

The sshd stack gets a three-module block, appended after the distribution's own
rules:

```
# bunker GAP-075: per-agent private /tmp (managed by `bunker host-provision`)
session    [success=2 auth_err=ignore default=die]    pam_succeed_if.so quiet user !~ bunker-*
session    [success=ignore default=die]               pam_exec.so quiet /usr/lib/bunker/pam-tmp-guard verify bunker-agents
session    required                                   pam_namespace.so
```

**Classifier (line 1).** `pam_succeed_if`'s `user !~ <glob>` test is
`evaluate_noglob()` → `fnmatch(3)` applied to the **`PAM_USER` item**
(`pam_succeed_if.c`), so the agent/non-agent decision is made from the reserved
username pattern and **not** from group state. Control semantics:

| Session user | classifier returns | action | effect |
|--------------|--------------------|--------|--------|
| not `bunker-*` (root, operator) | `PAM_SUCCESS` | `success=2` → jump 2 modules | the verifier and `pam_namespace` are skipped — the ENTIRE managed block — and the rest of the stack is untouched |
| `bunker-*` (agent) | `PAM_AUTH_ERR` | `auth_err=ignore` → continue | the verifier runs (line 2) |
| any other answer (unknown user, allocation/module error, incomplete condition — `pam_succeed_if.c` returns `PAM_USER_UNKNOWN`/`PAM_SERVICE_ERR`/`PAM_BUF_ERR`) | any other | `default=die` → immediate denial | an unanswerable question DENIES rather than shares |

**Verifier (line 2).** `pam_exec` runs the root-owned, managed helper
`/usr/lib/bunker/pam-tmp-guard` with the agent group as an argument. `pam_exec`
returns `PAM_SUCCESS` only when the command exits 0, and `PAM_SYSTEM_ERR` for a
non-zero exit, a signal or a **failed `execve`** (`pam_exec.c`), so:

| Helper outcome | `pam_exec` returns | action | effect |
|----------------|--------------------|--------|--------|
| boundary proven for this host | `PAM_SUCCESS` | `success=ignore` → continue | `pam_namespace` runs: private `/tmp` |
| helper denied, helper missing, helper crashed | `PAM_SYSTEM_ERR` | `default=die` → immediate denial | the session is DENIED (fail closed) |

The helper (rendered by `hostsetup.PamGuardScript`) verifies, in order:

1. its TRUST CHAIN — the helper directory, the SHA256 manifest and the helper
   itself are all present, root-owned and not group/world writable, and the
   helper's content still matches the manifest (drift ⇒ deny). The directory
   comes first because a group/world-writable `/usr/lib/bunker` would let an
   agent replace BOTH root-owned files by rename/unlink, and a writable manifest
   would let it prove anything;
2. the environment carries exactly one `PAM_USER` (identity is unambiguous);
3. the session user is agent-named, otherwise it steps aside for the stack;
4. the PAM block and the helper agree on the group, the agent group **exists**
   and the session user is a **member** of it;
5. the drop-in exists, is root-owned, is not group/world writable and declares
   **exactly** the required `/tmp` rule;
6. no other file in the namespace configuration set declares a `/tmp` polydir
   and every effective line in it is structurally valid;
7. the instance parent exists, is root-owned and is mode `000`;
8. the `pam_namespace.so` the block activates is installed.

The helper gives itself a fixed `PATH` before running anything: `pam_exec`
passes the PAM environment through, so no tool it uses may be resolved through
an inherited `PATH`.

The three module lines are always written — and repaired — as one adjacent unit,
because the classifier's jump length of `2` counts MODULES: a half-edited block
would land the jump on an unrelated module, so the host reports itself as **not**
active and the next `--apply` rewrites the block.

**Fail-closed.** `pam_namespace` alone cannot require a Bunker rule: with no
matching polydir it returns `PAM_SUCCESS` and the session keeps the shared host
`/tmp` (`pam_sm_open_session()` only calls `setup_namespace()` when a polydir
matched). The installed module line is `required` and carries neither
`ignore_config_error` (which would *skip* a malformed config line and continue —
the fail-open this design exists to eliminate) nor
`ignore_instance_parent_mode`. Verified live by the battery, which removes the
helper, the drop-in and the membership in turn, asserts that the agent session
is DENIED in every case, and restores each.

Deleting the drop-in **by hand does not restore the previous shared-`/tmp`
behavior**: the sshd PAM block stays installed, its `pam_exec` precondition is
what proves the drop-in exists, so every `bunker-*` session is DENIED until the
file is restored (or the block is removed with
`bunker host-provision --uninstall --apply`). A hand-deleted drop-in locks agents
out; it never shares `/tmp`.

A missing member of the stack is also handled: the installer refuses to activate
the block when `pam_namespace.so`, `pam_succeed_if.so` **or** `pam_exec.so` is
absent, and the readiness verdict (`--status`, `--json`) requires all of the
statically observable properties the helper enforces: the three modules (the
classifier with its glob test), the drop-in with exactly the required rule and
the whole namespace configuration set parseable, the drop-in's owner and mode,
the helper trust chain (directory → manifest → helper: present, root-owned, not
group/world writable, matching hash), an intact block naming the same group, the
agent group, and the instance parent present, root-owned and mode `000`.
Anything that would make the helper deny (or let an agent rewrite the boundary)
makes the verdict `false`, so `isolated: true` in `--status --json` cannot be
reported for a host whose sessions are denied or shared.

Why not a forced command (`command=` in `authorized_keys`): it replaces the
requested command, which breaks `scp`, `sftp`/sshfs and the docker transport
(they need the real `scp`/`sftp-server` invocation), and it cannot distinguish an
interactive shell from an sftp subsystem request. `pam_namespace` changes none of
the transports — only which directory `/tmp` resolves to inside the session — and
it preserves the session's uid (no user-namespace remapping).

**Persistence:** an agent's `/tmp` instance directory lives on disk and survives
between sessions, exactly like the old per-agent `TMPDIR` did. Files created in
`/tmp` by one `bunker exec` are visible to the next session of the *same* agent
and to nobody else.

### 3.2 Transient systemd units — `PrivateTmp=yes`

Both units a spawn/run creates carry the property:

```
systemd-run --system --unit=bunker-docker-<id>  --uid=<uid> --gid=<gid> \
    --property=PAMName=login --property=PrivateTmp=yes …
systemd-run --system --unit=bunker-run-<id>-<suffix> --uid=<uid> --gid=<gid> \
    --property=PAMName=login --property=PrivateTmp=yes …
```

systemd mounts a tmpfs private to the unit over `/tmp`, so the rootless dockerd
and everything it starts, and every detached run, cannot observe or collide with
the host `/tmp` or another agent's. The property is unconditional: it is emitted
even when every resource limit is unset.

`TMPDIR` is `/tmp` in every injected environment (units, `authorized_keys`
`environment=`, `.profile`, and the `env(1)` prefix of all exec modes) — the
constant `config.IsolationTmpDir`. The legacy `/run/bunker/<id>/tmp` directory is
still created (mode 0700, agent-owned) as a per-agent scratch path, but it is
**not** advertised as `TMPDIR` and is not part of the boundary.

### 3.3 Shared scratch — `/srv/bunker-share`

```
/srv/bunker-share                 root:bunker-agents  2750  setgid, NO group/world write
/srv/bunker-share/<agent-id>      bunker-<id>:bunker-agents  2770  setgid
                                  tmpfs size=<cap>,nosuid,nodev
```

* An agent is added to the supplementary group **`bunker-agents`** at spawn
  (`usermod -aG bunker-agents bunker-<id>`), so it can traverse the tree and read
  peers' exchange directories. That group is the SAME group the sshd PAM guard
  keys on, and membership is granted for every agent **even when
  `shared_scratch_enabled` is false** — the group is the isolation identity, not
  a property of the exchange directory.
* The root carries the setgid bit but is **not** group-writable and not
  world-accessible (mode `2750`): a member of the agent group may traverse the
  root and read a peer's directory, but cannot create an arbitrary plain
  directory or file directly under it. That is load-bearing, because a plain
  entry created there would be an **uncapped** write surface inside the exchange
  tree — the exact bypass of the per-agent tmpfs cap. Only the daemon (root)
  creates entries under the root, and every entry it creates is a bounded tmpfs
  mount. `EnsureSharedScratch` re-asserts the mode on every apply and STATS the
  directory back afterwards: a `chmod` that exited 0 is not proof, so a root
  that is still group-writable is a hard error, never a warning.
* **The spawn path enforces the same shape** (DF-BUNKER-40). `EnsureAgentScratch`
  — which runs on EVERY spawn — verifies the root before it creates the agent's
  directory under it, and repairs or creates it where the daemon may (it is
  root): the root is `chown`-ed `0:<group>` and `chmod`-ed `2750`, and both the
  mode (read from the real directory) and the ownership (re-observed after the
  repair) are stat-ed back. A host whose root is already correct is left
  completely untouched — the check costs at most two read-only probes — because
  the alternative (`MkdirAll` on the agent directory with the root missing)
  is exactly what creates a `root:root 0750` root that no agent can traverse.
  A root that cannot be brought to `root:<group> 2750` refuses the scratch with
  a message naming `bunker host-provision --apply`; the spawn then reports
  "shared scratch not provisioned" (the private `/tmp` is unaffected) instead of
  claiming a ready scratch the agent cannot reach.
* Each agent directory is a `tmpfs` mounted with an explicit `size=`. The cap is
  enforced by the kernel: a write past it fails with `ENOSPC` (`write: No space
  left on device`), which is what makes the bound real rather than a promise.
* Files created inside an agent directory inherit the `bunker-agents` group
  through the setgid bit, so a peer can read them without any extra step.
* Per-agent directories are mutually writable by design — that is what exchange
  means — but every directory is size-capped, so no agent can exhaust host disk or
  RAM through the exchange point.

Limits and defaults (config `agent.isolation.*`):

| Setting | Default | Meaning |
|---------|---------|---------|
| `agent_group` | `bunker-agents` | isolation group: the verifier requires the membership of every agent session, and the group owns the scratch tree |
| `shared_scratch_enabled` | `true` | provision the exchange point (membership is granted either way) |
| `shared_scratch_root` | `/srv/bunker-share` | exchange directory |
| `shared_scratch_group` | `bunker-agents` | legacy alias of `agent_group` |
| `shared_scratch_per_agent_bytes` | `268435456` (256 MiB) | kernel-enforced cap **per agent directory** |
| `private_tmp_root` | `/var/lib/bunkerd/agent-tmp` | pam_namespace `/tmp` instance parent |

Scratch is **ephemeral by design**: it is RAM-backed, it is unmounted and removed
when the agent is destroyed, and it does not survive a reboot (the daemon
re-provisions it on the next spawn). Agents must not treat it as storage.

### 3.4 Host `/tmp` bound

`/etc/systemd/system/tmp.mount.d/50-bunker-size.conf`:

```
[Mount]
Options=<existing options>,size=<cap>        # default 2147483648 (2 GiB)
```

* The drop-in is the durable half (it applies whenever `tmp.mount` is active).
* The live half is `mount -o remount,size=<cap> /tmp`, a **non-destructive**
  remount: it changes the size limit of a running tmpfs and leaves its contents
  in place. Restarting `tmp.mount` would delete every file open in `/tmp`, so it
  is never done.
* If `/tmp` is not currently a tmpfs, the drop-in is still installed (ready for
  the next activation) and the live step is reported as skipped. Nothing is
  "fixed" by mounting a fresh tmpfs over a populated `/tmp`.
* **`/etc/fstab` is never read, rewritten, or duplicated by any code path** — this
  is asserted by a unit test that fails if any provisioning command mentions it.

## 4. Provisioning

```
bunker host-provision            # dry run: print the plan, change nothing
bunker host-provision --apply    # install (idempotent)
bunker host-provision --status   # report state; --json for machines
bunker host-provision --uninstall --apply   # remove PAM + tmp.mount config
```

Properties:

* **Idempotent.** Every step compares before writing; a second `--apply` performs
  no mutation (asserted by tests), including on a host that carries an
  operator-owned `pam_namespace` rule of its own.
* **Reversible.** `--uninstall` removes the namespace drop-in, strips the managed
  PAM block and deletes the helper with its manifest, restoring `/etc/pam.d/sshd`
  byte-for-byte (asserted by tests). The uninstall never removes the scratch
  tree: it holds agent data.
* **Fail-closed.** `--apply` re-reads the state afterwards and fails if the
  boundary is not actually active. A missing `pam_namespace.so`,
  `pam_succeed_if.so` **or** `pam_exec.so` aborts the PAM step instead of
  activating a block that cannot be enforced, and the agent group is created
  before the helper that requires the membership.
* **Ordered.** Install writes the helper and its manifest BEFORE the block that
  invokes them; uninstall removes the block FIRST, then the helper — so the PAM
  stack never references a file that is not there.
* **Narrow blast radius.** Only the sshd PAM stack is edited, and only through a
  marked block, which affects **agent-named sessions only**. Non-agent sessions
  (root and every ordinary operator account) are jumped over the whole block, so
  a misconfigured drop-in cannot change what their `/tmp` is; agents get a denied
  session instead of a shared `/tmp` (G8).
* **Upgrade-safe.** `--apply` strips every Bunker-managed BLOCK (including the
  first revision's `ignore_config_error` module line and the second revision's
  group-keyed guard) before appending the canonical block, so a host that ran a
  rejected revision converges on exactly one block. Ownership is decided by
  Bunker's marker and Bunker's own line shapes: a bare or optioned
  `pam_namespace` rule and a foreign `pam_succeed_if` rule that Bunker did not
  write are preserved byte for byte, and `--uninstall` reports the
  `pam_namespace` rules that remain (G8c).

The daemon does **not** run the installer. It applies its half at spawn time and
that half has two different failure policies:

* the **agent group membership is fatal** — an agent that cannot be put in the
  isolation group would log in with the host's shared `/tmp`, so the spawn is
  rolled back and returns an error;
* the **scratch directory is best-effort** — a scratch that cannot be bounded is
  not created (never an unbounded fallback) and the spawn still yields a
  correctly isolated agent without the exchange point;
* the **instance directory is best-effort** — pam_namespace creates it on the
  first session (owned by root, mode 1777, mirroring `/tmp`); pre-creating it
  just makes ownership/mode deterministic.

## 5. Verification

Unit and integration (host-independent, `go test ./... -count=1 -short -parallel 4`):

* exact `systemd-run` argv for the rootless-dockerd unit and the detached-run
  unit, including `--property=PrivateTmp=yes` and `TMPDIR=/tmp`;
* exact `mount(8)` argv for the bounded scratch tmpfs, the stale-cap remount, the
  setgid `chmod`, and `usermod -aG bunker-agents` membership — asserted both with
  the exchange point enabled and with `shared_scratch_enabled: false`;
* fail-closed behaviour: a failed mount leaves no directory behind; a spawn whose
  membership cannot be granted returns an error and leaves nothing behind;
* dry runs issue no mutating command and no `/etc/fstab` access;
* **control semantics** of the rendered PAM block, driven by a table of cases
  that feeds the REAL helper's exit code through the block's control fields:
  agent + intact boundary → `pam_namespace` runs; non-agent → the whole block is
  jumped and every Bunker module is skipped; unanswerable classifier result →
  denied; each broken-boundary scenario (missing group, lost membership, missing
  / wrong / malformed / extra-rule drop-in, `/tmp` declared by another config
  file, missing module, helper drift, missing manifest, wrong instance-parent
  mode, missing instance parent, helper/block group drift) → **denied with
  `pam_namespace` unreached**, while the ordinary operator session still skips
  the block. Plus assertions that the block never contains
  `ignore_config_error`, that the module line is bare and `required`, that the
  classifier's jump length equals the number of managed modules after it, and
  that the three lines are adjacent;
* the real helper script is EXECUTED in every one of those scenarios (a sandbox
  host with stubbed `getent`/`id`), so the checks are proven by behaviour rather
  than by substring; the helper is also proven to ignore an inherited `PATH`;
* every configurable value that is embedded in PAM/config/helper content
  (group name, instance root, helper paths, tool PATH) is rejected when it could
  inject a line or a shell word, and nothing is written when it is;
* the managed files are asserted to be installed with the right mode
  (helper directory 0755, helper 0755, manifest 0444) and the manifest to be the
  sha256 of the helper; a pre-existing group/world-writable helper directory is
  REPAIRED and stat-ed back, and `verifyPamHelperChain` refuses a writable
  directory, a writable manifest, a writable helper and a missing manifest;
* **the readiness verdict covers exactly the static boundary**: one table drives
  both the fail-closed matrix and `TmpNamespaceStatus`, so every statically
  observable breakage that denies an agent session also reports
  `isolated: false` (drop-in rule/owner/mode, helper-directory owner/mode,
  helper owner/mode/hash, manifest owner/mode, instance-parent presence/owner/
  mode, block group, agent group), and a pure-state table flips each observed
  property one at a time to prove none is ignored (including the ownership half,
  which is only observable when the probe runs as root — exactly as the runtime
  helper gates it);
* the exchange root is asserted to be setgid and NOT group/world writable (mode
  `2750`) from the REAL directory stat, and `verifyScratchRoot` is proven to
  reject a group-writable, world-writable or non-setgid root;
* **PAM ownership**: install/repair and uninstall preserve a foreign bare
  `pam_namespace` rule, a foreign optioned one, and a foreign `pam_succeed_if`
  rule byte for byte, before and after the managed block; the first revision's
  legacy block is repaired; a half-stripped block is repaired; a foreign rule
  does not force a rewrite;
* the drop-in, the classifier, the verifier and the module line are rendered from
  the same configured group, and the legacy/rejected lines are repaired on
  re-apply and stripped on uninstall;
* `pam_namespace` install/uninstall round-trips `/etc/pam.d/sshd`;
* the real `ExecAgent` RPC (driven through a connect handler with a stub `ssh`)
  puts `TMPDIR=/tmp` in the argv that reaches sshd and never the legacy path;
* the real `Spawn` provisions the bounded scratch and the instance directory
  (anti-phantom: the test drives `Spawn`, not the helper);
* **the spawn path verifies the exchange ROOT** (DF-BUNKER-40): with the root
  correct the bounded scratch is still mounted and the spawn reports it ready;
  with the measured defect (`root:root 0750`) no per-agent directory is created,
  no bounded mount is issued, and the log carries the refusal plus
  `bunker host-provision --apply`. The gate's own table covers a correct root
  (verified, nothing re-paved, on both sides of the privilege split), a
  `root:root 0750` root and a group-writable root (repaired and re-verified),
  an absent root (created correctly), a repair the host ignores (fails rather
  than claiming success), and an unprivileged process facing a wrong root
  (checked and refused, never silently accepted).

Live (`e2e-full-battery.sh`, section 15 — run on the host as root):

1. `bunker host-provision --apply` is idempotent and leaves `/etc/fstab` untouched;
2. agent A writes `/tmp/<f>` and reads it back (positive control), root cannot see
   it, agent B cannot see it, and A cannot see root's `/tmp` file;
3. A writes into its scratch directory, B reads it, the directory is mode 2770 and
   the exchanged file carries the `bunker-agents` group;
4. the kernel reports the configured size for A's scratch directory, and an
   over-cap write fails (`write-exit≠0` / `No space left on device`) when the cap
   is small enough for a battery run;
5. the exchange ROOT is mode 2750 (`stat`), and an agent session that tries to
   create an arbitrary entry directly under it (`/srv/bunker-share/<name>`) is
   refused, while the same session writes freely inside its own sanctioned
   `/srv/bunker-share/<id>` directory — the bypass the mode exists to close;
6. the helper trust chain is enforced live: `/usr/lib/bunker` is root:root and
   not group/world writable, the helper is root:root mode 755 and matches its
   sha256 manifest, and a helper directory an agent could write denies every
   agent session (checked by temporarily widening the directory and restoring
   it);
7. an agent cannot write outside `/srv/bunker-share`, and cannot read the
   `0000` instance parent;
8. `systemctl show bunker-docker-<id> -p PrivateTmp` reports `yes`;
9. an ordinary disposable **non-agent** SSH user on the same host still sees the
   host `/tmp` (writes a file that root can read), while the agent's file stays
   invisible;
10. a deliberately **malformed** Bunker drop-in makes an agent session fail
    (denied) rather than opening with the shared `/tmp`; the drop-in is restored
    immediately afterwards and the session is proven to work again;
11. a re-apply preserves a foreign `pam_succeed_if` rule and leaves the bare
    `pam_namespace` rule count unchanged, and the deployed helper is root:root
    0755 with a matching manifest hash, in a root:root directory that is not
    group/world writable;
12. removing the helper, and separately removing the agent's group membership,
    both DENY the agent session, which is proven to work again after each is
    restored;
13. the agent user is a member of `bunker-agents` (`id -nG`) and the scratch
    directory's group is `bunker-agents`.

The battery is fail-safe around the PAM edit: if an agent session cannot open
after `--apply`, it rolls the Bunker-managed PAM configuration back and reports
the failure instead of leaving the host in a broken state.

## 6. Non-guarantees / limits (honest boundaries)

* **root can read an agent's private `/tmp` on disk.** The instance directories
  live under `/var/lib/bunkerd/agent-tmp/` (mode 0000, root-only). This is host
  administration, not a hole in the agent-vs-agent boundary: no agent can reach
  another agent's instance directory, and no session sees anything at the
  *path* `/tmp/<file>` that another session wrote.
* **Scratch is not durable** (tmpfs, removed with the agent, gone on reboot).
* **Scratch is mutually writable across agents** by design; the bound is
  per-directory, not per-file.
* **`/tmp` data is not migrated.** After enabling the boundary, a file an agent
  wrote to the old shared `/tmp` is not visible in its private one.
* **The boundary is enforced by PAM, so a host whose PAM stack is edited by hand
  can lose it.** `--status` therefore answers the isolation question directly
  (the drop-in rule + its owner/mode, the helper trust chain — directory,
  manifest, helper — with owners and modes, an intact block naming the same
  group, the agent group, and the instance parent's presence/owner/mode), and
  `--apply` repairs a drifted block; but nothing stops a root operator from
  deleting the block, and a host in that state has no agent `/tmp` isolation at
  all — the readiness verdict is the detection, not a runtime mechanism.
* **The helper is only as strong as its whole trust chain.** It refuses to act
  when its content no longer matches the root-owned manifest, when the manifest
  is missing/writable/not root-owned, or when the directory holding either file
  is group/world writable (so an agent could replace both) — so an accidental
  edit, a package overwrite or an interrupted upgrade denies every agent session
  until Bunker repairs it, instead of running a boundary that cannot be proven.
  The installer refuses to write the block before the helper exists and refuses
  to report success while any link of the chain is writable. An attacker with
  root can of course replace both files; root is outside every guarantee in this
  spec.
* **A missing PAM module is a refusal, not a degradation.** Without
  `pam_namespace.so`, `pam_succeed_if.so` or `pam_exec.so` the installer refuses
  to activate the block. The systemd-unit half (G3) and the scratch half (G4–G6)
  do not depend on PAM.
* **The agent-name classifier needs Linux-PAM ≥ 1.6.** `user !~ <glob>` is
  `evaluate_noglob()` → `fnmatch(3)`; an older module treats `!~` as an unknown
  attribute and returns `PAM_SERVICE_ERR`, which the block's `default=die` would
  turn into a full lockout (root included). `--apply` therefore probes the
  module for the `fnmatch`/`noglob` tokens before writing anything and refuses
  with an explicit message, and `--status` reports
  `classifier pattern ok: false` with the missing tokens.
* **`PrivateTmp` covers systemd units only.** Long-lived processes started
  outside a unit (a shell an operator leaves running) are not covered; the
  boundary is per session, not per uid.
* **Group drift denies instead of sharing.** The agent/non-agent decision is made
  from the `bunker-*` name, and the verifier additionally requires the group and
  the membership: a host whose `bunker-agents` group is missing, or an agent
  whose membership was removed, gets a DENIED session until Bunker provisions it
  again — never the host's shared `/tmp`. That is the intended direction (G8b),
  and the installer creates the group before the helper that requires it.
* **An operator added to `bunker-agents` is verified like an agent** but is not
  agent-named, so it is still jumped over the block and keeps the host `/tmp`;
  the group alone no longer decides who is isolated.

## 7. Related

* `specs/agent-lifecycle.md` — spawn/destroy lifecycle this attaches to.
* `specs/containment-disclosure.md` — the (unrelated, opt-in) disclosure marker;
  isolated `/tmp` and disclosure are independent features.
* `internal/hostsetup` — the installer and provisioners described above.
* `internal/agent/isolation.go` — the daemon's spawn/destroy half.
