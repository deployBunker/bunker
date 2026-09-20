#!/usr/bin/env python3
"""GAP-106 instance-dimension tests: constant surface, runtime instance set.

The invariant this file exists to pin, in the row's own words: "the number of
registered MCP tools is a CONSTANT ... no matter how many servers the config
contains." A per-bunker design would register one tool per bunker and
re-register on every spawn/destroy; the shim registers a fixed catalog and
treats the bunker as an ARGUMENT resolved per call.

Everything here is offline: the CLI config is a local file, so discovery and
binding need no daemon and no network. Runs the shim's logic in-process
(import, not subprocess), matching tools/test_bunker_shim.py.
"""
import importlib.util
import os
import sys
import tempfile
from pathlib import Path

SHIM = Path(__file__).resolve().parent.parent / "tools" / "bunker-shim.py"
FAILS = []

EXPECTED_TOOL_COUNT = 13  # 10 file/exec verbs (GAP-097) + 3 instance verbs (GAP-106)
INSTANCE_VERBS = ["bunker_hold", "bunker_list", "bunker_switch"]


def check(name, cond, detail=""):
    print(("PASS " if cond else "FAIL ") + name + ("" if cond else f" — {detail}"))
    if not cond:
        FAILS.append(name)


def load_shim():
    spec = importlib.util.spec_from_file_location("bunker_shim_gap106", SHIM)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def write_config(path, names, active=""):
    """The shape internal/cli/config.go ServerEntry writes."""
    lines = ["servers:"]
    for n in names:
        lines += [
            f"  {n}:",
            f"    name: {n}",
            f"    url: http://10.0.0.1:19090",
            f"    token: tok-{n}",
            "    tls_insecure: false",
        ]
    if active:
        lines.append(f"active_server: {active}")
    path.write_text("\n".join(lines) + "\n")
    return path


def main():
    mod = load_shim()

    # ---------------------------------------------------------------------
    # 1. THE INVARIANT: surface size does not move with the instance set.
    # ---------------------------------------------------------------------
    with tempfile.TemporaryDirectory() as td:
        one = write_config(Path(td) / "one.yaml", ["alpha"])
        many = write_config(Path(td) / "many.yaml", [f"srv-{i:02d}" for i in range(20)])

        # registration happens at import: it is fixed, not recomputed per config
        names_one = mod.tool_names()
        mod._refresh_surface()
        names_many = mod.tool_names()

        check("1-server config: 13 tools registered", len(names_one) == EXPECTED_TOOL_COUNT,
              f"got {len(names_one)}: {names_one}")
        check("20-server config: identical tool set",
              names_one == names_many,
              f"1-server={names_one} 20-server={names_many}")
        check("20-server config does not add per-bunker tools",
              not any("srv-" in n for n in names_many), str(names_many))
        check("the 3 instance verbs are on the constant surface",
              all(v in names_one for v in INSTANCE_VERBS), str(names_one))

        # Discovery must SEE 20 without the surface growing to 20.
        listing = mod.bunker_list(config=str(many))
        check("discovery lists all 20 servers", len(listing["servers"]) == 20,
              str(len(listing.get("servers", []))))
        check("discovery reports the surface as constant",
              listing["surface"]["tools"] == EXPECTED_TOOL_COUNT
              and listing["surface"]["constant"] is True, str(listing.get("surface")))

        # -----------------------------------------------------------------
        # 2. DISCOVERY is a pure read: it never binds.
        # -----------------------------------------------------------------
        os.environ.pop("BUNKER_SESSION_TARGET", None)
        one_list = mod.bunker_list(config=str(one))
        check("discovery reports the registered name",
              [s["name"] for s in one_list["servers"]] == ["alpha"],
              str(one_list.get("servers")))
        # config with active_server set: discovery must still not bind with it.
        act = write_config(Path(td) / "act.yaml", ["alpha", "beta"], active="beta")
        mod.bunker_hold(release=True, config=str(act))
        before = mod._resolve_target_with_source(None, required=False)[1]
        mod.bunker_list(config=str(act))
        after = mod._resolve_target_with_source(None, required=False)[1]
        check("discovery does not bind (resolved_via unchanged)", before == after,
              f"{before} -> {after}")

        # -----------------------------------------------------------------
        # 3. SWITCH / HOLD: session state, and a refusal that names the known set.
        # -----------------------------------------------------------------
        os.environ["BUNKER_SHIM_STATE"] = str(Path(td) / "state")
        os.environ["HERMES_SESSION_ID"] = "test-session-gap106"

        bound = mod.bunker_switch("alpha", config=str(one))
        check("switch binds the session", bound.get("held") == "alpha", str(bound))
        check("switch reports the tier that bound it",
              bound.get("bound_via") == "bunker_switch", str(bound.get("bound_via")))
        check("a switch does NOT write the shared config",
              "active_server" not in one.read_text(), one.read_text())

        # Persists to the NEXT call (the whole point of a hold).
        t, src = mod._resolve_target_with_source(None)
        check("hold persists to the next call", t == "alpha" and src == "session_hold",
              f"{t} via {src}")

        # Another session is untouched — the reason this is not active_server.
        os.environ["HERMES_SESSION_ID"] = "sibling-session"
        st, ssrc = mod._resolve_target_with_source(None, required=False)
        check("a sibling session is not re-targeted", st == "" and ssrc == "unbound",
              f"{st} via {ssrc}")
        os.environ["HERMES_SESSION_ID"] = "test-session-gap106"

        # Unknown name: refuse AND name the known bunkers.
        bad = mod.bunker_switch("nope", config=str(one))
        check("unknown name refuses", bad.get("error") == "unknown_bunker", str(bad))
        check("refusal names the known bunkers", bad.get("known") == ["alpha"], str(bad))
        check("refusal is actionable (says what to do)",
              "known bunkers" in (bad.get("detail") or "")
              or "Switch to one of those" in (bad.get("detail") or ""),
              str(bad.get("detail")))

        # hold with name = set-and-read in one call
        mod.bunker_hold(release=True, config=str(one))
        held = mod.bunker_hold(name="alpha", config=str(one))
        check("hold(name=...) binds and reports", held.get("held") == "alpha", str(held))

        # release clears it
        rel = mod.bunker_hold(release=True, config=str(one))
        t2, s2 = mod._resolve_target_with_source(None, required=False)
        check("hold(release=True) clears the binding", t2 == "" and s2 == "unbound",
              f"{t2} via {s2}")

        # -----------------------------------------------------------------
        # 4. PRECEDENCE: explicit > session hold > env  (fail-closed preserved)
        # -----------------------------------------------------------------
        mod.bunker_switch("alpha", config=str(one))
        os.environ["BUNKER_SESSION_TARGET"] = "beta"
        t3, s3 = mod._resolve_target_with_source("gamma")
        check("explicit beats session hold", t3 == "gamma" and s3 == "explicit",
              f"{t3} via {s3}")
        t4, s4 = mod._resolve_target_with_source(None)
        check("session hold beats env", t4 == "alpha" and s4 == "session_hold",
              f"{t4} via {s4}")
        mod.bunker_hold(release=True, config=str(one))
        t5, s5 = mod._resolve_target_with_source(None)
        check("env is next after the hold is released", t5 == "beta" and s5 == "env",
              f"{t5} via {s5}")
        os.environ.pop("BUNKER_SESSION_TARGET", None)

        # -----------------------------------------------------------------
        # 5. FAIL-CLOSED: unbound mutating call refuses, naming remedies.
        # -----------------------------------------------------------------
        try:
            mod._resolve_target(None)
            refused = False
            detail = "no exception"
        except ValueError as exc:
            refused = "BUNKER_SESSION_TARGET" in str(exc)
            detail = str(exc)
        check("unbound mutating call refuses with named remedies", refused, detail)

        # -----------------------------------------------------------------
        # 6. The dispatcher exposes the instance verbs and refuses by name.
        # -----------------------------------------------------------------
        out = mod._dispatch({"name": "bunker_list", "args": {"config": str(one)}})
        check("_dispatch routes bunker_list", "servers" in out, str(out)[:160])
        out2 = mod._dispatch({"name": "bunker_nope", "args": {}})
        check("_dispatch refuses an unknown verb by name",
              out2.get("error") == "unknown_verb", str(out2))

    print()
    if FAILS:
        print(f"INSTANCES: FAIL ({len(FAILS)}): {', '.join(FAILS)}")
        return 1
    print("INSTANCES: PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
