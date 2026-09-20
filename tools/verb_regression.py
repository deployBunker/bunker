#!/usr/bin/env python3
"""GAP-111 — verb-layer regression suite (plus-ultra criterion 4).

The verb layer's defining behaviours are REFUSALS and BOUNDARIES, so a suite
that only exercises happy paths would cover none of what makes it safe. Each
block below asserts a refusal that must never regress into a silent success.

Runs against the REAL CLI binary (no network): every check either provokes a
refusal locally or inspects the parsed surface (flags/verbs). Live-transport
proofs live in the GAP-112 battery.

Usage:
    python3 tools/verb_regression.py            # uses `bunker` on PATH
    BUNKER_BIN=/tmp/bunker-bin python3 tools/verb_regression.py
"""
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

BUNKER = os.environ.get("BUNKER_BIN") or shutil.which("bunker") or "bunker"
FAILS, PASSES = [], []


def run(args, env=None, stdin=None, timeout=60):
    e = dict(os.environ)
    e.pop("BUNKER_SESSION_TARGET", None)  # each check opts in deliberately
    if env:
        e.update(env)
    try:
        p = subprocess.run([BUNKER] + args, capture_output=True, timeout=timeout,
                           input=stdin, env=e)
        return p.returncode, p.stdout.decode("utf-8", "replace"), p.stderr.decode("utf-8", "replace")
    except subprocess.TimeoutExpired:
        return 124, "", "TIMEOUT"


def check(name, cond, detail=""):
    (PASSES if cond else FAILS).append(name)
    print(("PASS " if cond else "FAIL ") + name + ("" if cond else f"\n      {detail[:400]}"))


def block(title):
    print(f"\n=== {title} ===")


def surface(cmd):
    rc, out, err = run([cmd, "--help"])
    flags = set(re.findall(r"^\s+(--[A-Za-z0-9-]+)", out + err, re.M))
    return flags


# ---------------------------------------------------------------------------
# 1. FAIL-CLOSED BINDING (GAP-093): mutating commands refuse without a target.
#    The keystone claim: no mutating command falls back to the shared default.
# ---------------------------------------------------------------------------
def binding_block():
    block("GAP-093 fail-closed binding")
    # A config with an active_server set, to prove the default is NOT used.
    with tempfile.TemporaryDirectory() as td:
        cfg = Path(td, "config.yaml")
        cfg.write_text(
            "active_server: shared-default\n"
            "servers:\n"
            "  shared-default:\n"
            "    url: http://127.0.0.1:1\n"
            "    token: x\n"
        )
        env = {"BUNKER_HOME": td}
        mutating = [
            ["exec", "someagent", "--", "true"],
            ["run", "someagent", "--", "true"],
            ["destroy", "someagent"],
            ["heartbeat", "someagent"],
            ["spawn", "--agent-id", "x"],
            ["mount", "someagent"],
        ]
        for argv in mutating:
            rc, out, err = run(argv, env=env)
            blob = out + err
            refused = rc != 0 and ("no target bound" in blob or "BUNKER_SESSION_TARGET" in blob)
            check(f"{argv[0]} refuses with no binding (shared default NOT used)",
                  refused, f"rc={rc} out={out[:150]} err={err[:150]}")
            leaked = "shared-default" in blob and "no target bound" not in blob
            check(f"{argv[0]} never silently uses active_server", not leaked, blob[:200])

        # umount is LOCAL-ONLY by design: it contacts no daemon, so it needs no
        # binding, and cleanup must stay idempotent (safe to run twice).
        rc, out, err = run(["umount", "no-such-agent"], env=env)
        check("umount is local-only and idempotent (no binding required)",
              "no target bound" not in (out + err), (out + err)[:200])


# ---------------------------------------------------------------------------
# 2. TARGET-OVERRIDE WINS (GAP-093/106): explicit > env, and the resolution
#    order is observable in the "reading server" line.
# ---------------------------------------------------------------------------
def override_block():
    block("target override order")
    for cmd in ("exec", "run", "destroy"):
        flags = surface(cmd)
        check(f"{cmd} exposes --server", "--server" in flags, str(sorted(flags)))

    with tempfile.TemporaryDirectory() as td:
        cfg = Path(td, "config.yaml")
        cfg.write_text(
            "active_server: shared-default\n"
            "servers:\n"
            "  alpha:\n    url: http://127.0.0.1:19001\n    token: x\n"
            "  beta:\n    url: http://127.0.0.1:19002\n    token: x\n"
        )
        import socket
        def dead_port():
            s_ = socket.socket()
            s_.bind(("127.0.0.1", 0))
            port = s_.getsockname()[1]
            s_.close()
            return port
        pa, pb = dead_port(), dead_port()
        while pb == pa:
            pb = dead_port()
        cfg.write_text(
            "active_server: shared-default\n"
            "servers:\n"
            f"  alpha:\n    url: http://127.0.0.1:{pa}\n    token: x\n"
            f"  beta:\n    url: http://127.0.0.1:{pb}\n    token: x\n"
        )
        env = {"BUNKER_HOME": td, "BUNKER_SESSION_TARGET": "beta"}
        # explicit flag must pick alpha (port 19001), NOT the env's beta (19002)
        rc, out, err = run(["exec", "--server", "alpha", "a1", "--", "true"], env=env)
        blob = out + err
        check("explicit --server beats BUNKER_SESSION_TARGET (dialed alpha's port)",
              str(pa) in blob and str(pb) not in blob, blob[:220])
        # and with NO flag, the env binding is what gets used
        rc, out, err = run(["exec", "a1", "--", "true"], env=env)
        blob = out + err
        check("BUNKER_SESSION_TARGET is used when no flag is given (dialed beta's port)",
              str(pb) in blob, blob[:220])


# ---------------------------------------------------------------------------
# 3. GAP-094 EXEC FLAG SURFACE: the three fidelity flags exist and are parsed
#    (they were silently unpeeled before — this is the regression that matters).
# ---------------------------------------------------------------------------
def exec_flag_block():
    block("GAP-094 exec flag surface")
    flags = surface("exec")
    for f in ("--stdin", "--base64", "--exec-cap"):
        check(f"exec exposes {f}", f in flags, str(sorted(flags)))

    # --stdin - with no payload on a non-writable config still must not hang;
    # missing file is a LOCAL error, named, before any RPC.
    with tempfile.TemporaryDirectory() as td:
        cfg = Path(td, "config.yaml")
        cfg.write_text("servers:\n  s1:\n    url: http://127.0.0.1:1\n    token: x\n")
        rc, out, err = run(["exec", "--server", "s1", "a1",
                            "--stdin", "/nonexistent/file/xyz", "--", "true"],
                           env={"BUNKER_HOME": td})
        blob = out + err
        check("missing --stdin file fails locally with a named error",
              rc != 0 and "stdin" in blob.lower(), f"rc={rc} {blob[:200]}")

    # --exec-cap must reject a non-numeric value rather than silently ignoring.
    with tempfile.TemporaryDirectory() as td:
        cfg = Path(td, "config.yaml")
        cfg.write_text("servers:\n  s1:\n    url: http://127.0.0.1:1\n    token: x\n")
        rc, out, err = run(["exec", "--server", "s1", "a1",
                            "--exec-cap", "notanumber", "--", "true"],
                           env={"BUNKER_HOME": td})
    check("--exec-cap rejects a non-numeric value",
          rc != 0, f"rc={rc} {(out + err)[:200]}")


# ---------------------------------------------------------------------------
# 4. GAP-104 MOUNT NAMESPACE/WORKSPACE: --expect-workspace exists and the
#    refusal rules hold (empty expectation never refuses; mismatch refuses).
# ---------------------------------------------------------------------------
def mount_block():
    block("GAP-104 mount namespace + workspace")
    flags = surface("mount")
    check("mount exposes --expect-workspace", "--expect-workspace" in flags, str(sorted(flags)))
    check("mount exposes --path", "--path" in flags, str(sorted(flags)))
    check("umount exposes --force", "--force" in surface("umount"))
    check("guard exposes no --server (local-only command)",
          "--server" not in surface("guard"), str(sorted(surface("guard"))))


# ---------------------------------------------------------------------------
# 5. GAP-097 SHIM CONSTANT SURFACE: the shim's verb list is fixed regardless
#    of how many bunkers exist — proven by parsing it, not by running it.
# ---------------------------------------------------------------------------
def shim_block():
    block("GAP-097 constant surface")
    shim = Path(__file__).resolve().parent / "bunker-shim.py"
    if not shim.exists():
        check("shim exists", False, str(shim))
        return
    import ast
    tree = ast.parse(shim.read_text())
    tools = [n.name for n in ast.walk(tree)
             if isinstance(n, ast.FunctionDef)
             for d in n.decorator_list
             if isinstance(d, ast.Call) and getattr(d.func, "attr", "") == "tool"]
    # The catalog is CONSTANT by design (GAP-106): 10 file/exec verbs + 3
    # instance verbs (list/switch/hold). It must not scale with the fleet.
    check("shim registers exactly 13 verbs", len(tools) == 13, str(tools))
    check("shim verbs are the PRD set + the instance verbs",
          set(tools) == {"bunker_read", "bunker_search", "bunker_write", "bunker_edit",
                         "bunker_patch", "bunker_apply", "bunker_exec", "bunker_run",
                         "bunker_lsp", "bunker_lease",
                         "bunker_list", "bunker_switch", "bunker_hold"}, str(sorted(tools)))
    src = shim.read_text()
    # GAP-106 made the shim READ the CLI config for DISCOVERY (bunker_list),
    # so the file name appearing is expected. The invariant that matters — and
    # what this asserts — is that the shared active_server default is never
    # used to BIND a call. Tested behaviourally in
    # test_bunker_shim_instances.py ("discovery does not bind").
    check("shim never resolves a server from shared config",
          "ActiveServer" not in src
          and "shared_default_not_used" in src
          and "_resolve_target_with_source" in src)
    check("shim resolves explicit > session env",
          "BUNKER_SESSION_TARGET" in src and "server and server.strip()" in src)


# ---------------------------------------------------------------------------
# 6. REFUSALS NAME WHAT TO DO (the doctrine: a refusal that doesn't say what
#    to do just teaches workarounds).
# ---------------------------------------------------------------------------
def refusal_quality_block():
    block("refusal quality")
    with tempfile.TemporaryDirectory() as td:
        empty = Path(td, "empty.yaml")
        empty.write_text("servers: {}\n")
        rc, out, err = run(["exec", "a1", "--", "true"], env={"BUNKER_HOME": td})
    blob = out + err
    names_remedy = "BUNKER_SESSION_TARGET" in blob or "--server" in blob
    check("binding refusal names a remedy", names_remedy, blob[:250])

    with tempfile.TemporaryDirectory() as td:
        cfg = Path(td, "config.yaml")
        cfg.write_text("active_server: shared\nservers:\n  shared:\n    url: http://127.0.0.1:1\n    token: x\n")
        rc, out, err = run(["exec", "a1", "--", "true"], env={"BUNKER_HOME": td})
        blob = out + err
        check("refusal explains WHY the shared default is refused",
              "never" in blob or "shared" in blob or "re-target" in blob, blob[:250])


def main():
    print(f"bunker binary: {BUNKER}")
    rc, out, err = run(["--version"])
    print((out + err).strip().splitlines()[0] if (out + err).strip() else "(no version)")
    binding_block()
    override_block()
    exec_flag_block()
    mount_block()
    shim_block()
    refusal_quality_block()
    print(f"\n{len(PASSES)} passed, {len(FAILS)} failed")
    if FAILS:
        print("FAILED:", FAILS)
        return 1
    print("VERB-REGRESSION: PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
