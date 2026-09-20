#!/usr/bin/env python3
"""GAP-097 shim tests: constant surface, binding refusal, per-call routing.

Runs the shim's logic in-process (import, not subprocess) with a fake `bunker`
binary on PATH so no daemon or network is needed. The MCP transport itself is
thin (FastMCP stdio) and not what these tests cover.
"""
import ast
import io
import os
import stat
import sys
import tempfile
import textwrap
from pathlib import Path

SHIM = Path(__file__).resolve().parent.parent / "tools" / "bunker-shim.py"
FAILS = []


def check(name, cond, detail=""):
    print(("PASS " if cond else "FAIL ") + name + ("" if cond else f" — {detail}"))
    if not cond:
        FAILS.append(name)


def main():
    src = SHIM.read_text()
    tree = ast.parse(src)
    tools = [n.name for n in ast.walk(tree)
             if isinstance(n, ast.FunctionDef)
             for d in n.decorator_list
             if isinstance(d, ast.Call) and getattr(d.func, "attr", "") == "tool"]
    check("constant surface = exactly 10 verbs", len(tools) == 10, str(tools))

    # Import the shim module (FastMCP decorators run but do not connect)
    sys.path.insert(0, str(SHIM.parent))
    import importlib.util
    spec = importlib.util.spec_from_file_location("bunker_shim", SHIM)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    # --- binding refusal: no explicit server, no env -------------------------
    os.environ.pop("BUNKER_SESSION_TARGET", None)
    try:
        mod._run_bunker(["info"])
        refused = False
    except ValueError as e:
        refused = "no target bound" in str(e) and "BUNKER_SESSION_TARGET" in str(e)
    check("unbound call refused with named remedies", refused)

    # --- explicit server wins over env ---------------------------------------
    with tempfile.TemporaryDirectory() as td:
        fake = Path(td) / "bunker"
        seen = {}
        fake.write_text(
            "#!/bin/sh\n"
            f'echo "$@" > "{td}/argv.txt"\n'
            f'cat > "{td}/stdin.txt" 2>/dev/null || true\n'
            "echo ok\n"
        )
        fake.chmod(fake.stat().st_mode | stat.S_IEXEC)
        os.environ["PATH"] = f"{td}:{os.environ['PATH']}"

        mod._run_bunker(["info"], server="alpha")
        argv = Path(td, "argv.txt").read_text().split()
        check("explicit server → --server alpha first", argv[:3] == ["--server", "alpha", "info"], str(argv))

        os.environ["BUNKER_SESSION_TARGET"] = "beta"
        mod._run_bunker(["info"])
        argv = Path(td, "argv.txt").read_text().split()
        check("session binding used when no explicit server", argv[:3] == ["--server", "beta", "info"], str(argv))

        mod._run_bunker(["info"], server="alpha")
        argv = Path(td, "argv.txt").read_text().split()
        check("explicit server beats session binding", argv[:3] == ["--server", "alpha", "info"], str(argv))

        # --- unverified stays unverified: nonzero exit surfaces exit_code -----
        (Path(td) / "bunker").write_text("#!/bin/sh\necho boom >&2\nexit 7\n")
        os.chmod(Path(td) / "bunker", 0o755)
        r = mod._run_bunker(["exec", "--", "false"], server="alpha")
        check("cli failure keeps exit_code and stderr, no fake success",
              isinstance(r, dict) and r.get("exit_code") == 7 and "boom" in r.get("stderr", ""), str(r)[:200])

    print()
    if FAILS:
        print(f"{len(FAILS)} FAILED: {FAILS}")
        return 1
    print("ALL SHIM TESTS PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
