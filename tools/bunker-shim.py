#!/usr/bin/env python3
"""bunker-shim: the CONSTANT-SURFACE Hermes tool layer over the bunker CLI (GAP-097, GAP-106).

One stdio MCP server. Exactly thirteen tools, always registered, regardless of
how many bunkers exist — the bunker is an ARGUMENT resolved per call:

    explicit `server` param  >  session hold/switch  >  BUNKER_SESSION_TARGET env  >  REFUSAL

Ten of the thirteen are the file/exec verbs (GAP-097). Three are the INSTANCE
verbs (GAP-106): list / switch / hold — the runtime half of "which bunker".
They make the instance set a runtime value without growing the tool list:

  * bunker_list   — discover which bunkers exist (one local read of the CLI
                    config; no daemon, no network — the config IS the registry)
  * bunker_switch — bind THIS session to a named bunker, persisted for the
                    session; an unknown name refuses and names the known ones
  * bunker_hold   — report which bunker this session is holding and which tier
                    resolved it (name=... also binds, so set-and-read is one call)

Thirteen is the whole surface and it stays thirteen for one server or fifty: a
per-bunker design would register 20 read tools for 20 bunkers and re-register on
every spawn/destroy. The catalog is constant; the instance set is not. That is
the invariant tools/test_bunker_shim_instances.py enumerates and pins.

The hold is SESSION STATE (a small file keyed by the session id), never a tool
registration and never a write to the shared config: a switch must not be able
to re-target a sibling session the way `bunker use` / `active_server` once did.
Discovery reports `active_server` for information and never binds with it.

The refusal NAMES the missing binding and the remedies (`server='...'`,
`bunker_switch`, BUNKER_SESSION_TARGET) plus the known bunkers. It never falls
back to a shared `active_server` default: whichever session last ran
`bunker use` must not silently re-target every other session's calls.

The hold lives at `$BUNKER_SHIM_STATE/<session>.json` (default
`$BUNKER_HOME/.shim-state/<session>.json`), keyed by `$HERMES_SESSION_ID`.

Capability variance is an ERROR, not a dynamic tool list: a verb the target
lacks answers `capability_unavailable` naming what is absent while the verb
STAYS on the surface.

Per-session isolation rides BUNKER_HOME (see DF-BUNKER-16): the session env
sets BUNKER_HOME to a per-session profile dir, so concurrent sessions never
share a server list, token, or key. The binding itself is session STATE —
never a tool registration.

All calls go through the `bunker` CLI (single audited binary, single binding
resolver). Nothing in this file talks SSH or parses bunker config directly.
"""
import json
import os
import shlex
import subprocess
import sys
import time

from mcp.server.fastmcp import FastMCP

MCP = FastMCP("bunker-shim")

BINDING_ENV = "BUNKER_SESSION_TARGET"

# Session-state file for the GAP-106 hold (see the module docstring).
STATE_DIR_ENV = "BUNKER_SHIM_STATE"
SESSION_ID_ENV = "HERMES_SESSION_ID"
CONFIG_FILENAME = "config.yaml"

# ---------------------------------------------------------------------------
# Instance dimension (GAP-106): the CLI config is the source of WHICH bunkers
# exist. Reading it is a plain local file read — the discovery path must work
# with no daemon and no network. PyYAML is NOT assumed importable in the
# session env this runs in, so the shape the Go writer emits is parsed
# directly; anything the subset cannot read lands in `diagnostic` instead of
# raising, because a config the shim cannot read must be VISIBLE, never
# silently empty.
# ---------------------------------------------------------------------------

def _config_path(explicit: str | None = None) -> str:
    """explicit param (the CLI's --config) > $BUNKER_HOME/config.yaml > ~/.bunker/config.yaml."""
    if explicit and explicit.strip():
        return os.path.abspath(explicit.strip())
    home = os.environ.get("BUNKER_HOME", "").strip()
    if home:
        return os.path.join(home, CONFIG_FILENAME)
    return os.path.join(os.path.expanduser("~"), ".bunker", CONFIG_FILENAME)


def _parse_scalar(raw: str) -> object:
    """A YAML scalar as far as the CLI config ever uses one (quotes + booleans)."""
    v = raw.strip()
    if not v:
        return ""
    if len(v) >= 2 and v[0] == v[-1] and v[0] in ("'", '"'):
        return v[1:-1]
    low = v.lower()
    if low in ("true", "yes", "on"):
        return True
    if low in ("false", "no", "off"):
        return False
    if low in ("null", "~"):
        return ""
    if v in ("|", ">", "|-", ">-", "|+", ">+"):
        return ""  # block scalar: the CLI writer never emits one
    return v


def _parse_cli_config(text: str) -> dict:
    """Minimal reader for the CLI config (CLIConfig in internal/cli/config.go).

        servers:
          alpha:
            name: alpha
            url: http://host:port
            token: ...
            tls_insecure: false
            connected_at: 2026-09-20T03:05:06Z
        active_server: alpha

    One level of fields per server (the Go struct's shape). Indentation is
    relative — the first key under `servers:` sets the child indent, so a
    4-space or 2-space writer both read. Duplicate names keep the last entry
    (the Go loader is a map). Returns
    {"active_server", "servers": [ {field: value} ], "diagnostic"}.
    """
    active = ""
    servers: dict[str, dict] = {}
    diagnostic = ""
    in_servers = False
    servers_indent = 0
    child_indent: int | None = None
    current: str | None = None

    for line in text.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        indent = len(line) - len(line.lstrip(" "))
        body = line.strip()

        if in_servers and indent > servers_indent:
            if ":" not in body:
                diagnostic = diagnostic or f"unparsable servers entry: {body!r}"
                continue
            key, _, rest = body.partition(":")
            if child_indent is None or indent <= child_indent:
                child_indent = indent
                current = str(_parse_scalar(key))
                servers.setdefault(current, {})
                if rest.strip():
                    diagnostic = diagnostic or (
                        f"unsupported inline entry {current!r}: {rest.strip()!r}")
            elif current is None:
                diagnostic = diagnostic or f"field outside any server: {body!r}"
            else:
                servers[current][key.strip()] = _parse_scalar(rest)
            continue

        in_servers = False  # left the block (or never entered it)

        if body.startswith("servers:"):
            rest = body.split(":", 1)[1].strip()
            if rest in ("", "{}"):
                in_servers = rest == ""
                servers_indent = indent
                child_indent = None
                current = None
            else:
                diagnostic = diagnostic or (
                    f"unsupported inline `servers:` value {rest!r}; expected a block mapping")
        elif body.startswith("active_server:"):
            active = str(_parse_scalar(body.split(":", 1)[1]))

    return {
        "active_server": active,
        "servers": [{"name": n, **fields} for n, fields in servers.items()],
        "diagnostic": diagnostic,
    }


def _read_servers(config: str | None = None) -> tuple[list[dict], dict]:
    """The registered bunkers, read offline. Never raises; failures are named.

    A token is reported as `token_present` and never echoed — discovery must not
    become a credential dump."""
    path = _config_path(config)
    meta = {"config_path": path, "exists": False, "active_server": "", "diagnostic": ""}
    try:
        with open(path, "r", errors="replace") as fh:
            text = fh.read()
    except FileNotFoundError:
        meta["diagnostic"] = (f"no CLI config at {path} — register a bunker with "
                              "`bunker connect`, or point BUNKER_HOME/--config at the right profile")
        return [], meta
    except OSError as exc:
        meta["diagnostic"] = f"cannot read CLI config {path}: {exc}"
        return [], meta

    parsed = _parse_cli_config(text)
    meta["exists"] = True
    meta["active_server"] = parsed["active_server"]
    meta["diagnostic"] = parsed["diagnostic"]

    out = []
    for entry in parsed["servers"]:
        name = str(entry.get("name", ""))
        out.append({
            "name": name,
            "url": str(entry.get("url", "")),
            "tls_insecure": bool(entry.get("tls_insecure", False)),
            "connected_at": str(entry.get("connected_at", "")),
            "token_present": bool(str(entry.get("token", "")).strip()),
            # Reported for INFORMATION only. It is never a binding: one session
            # running `bunker use` must not re-target another's calls.
            "shared_default": bool(parsed["active_server"]) and name == parsed["active_server"],
        })
    out.sort(key=lambda s: s["name"])
    return out, meta


# ---------------------------------------------------------------------------
# Session hold (GAP-106): the binding is STATE, not a registration, and it is
# keyed by session so two sessions on one box hold different targets.
# ---------------------------------------------------------------------------

def _state_dir() -> str:
    """$BUNKER_SHIM_STATE > $BUNKER_HOME/.shim-state > ~/.bunker/.shim-state."""
    d = os.environ.get(STATE_DIR_ENV, "").strip()
    if d:
        return os.path.abspath(d)
    home = os.environ.get("BUNKER_HOME", "").strip()
    base = home if home else os.path.join(os.path.expanduser("~"), ".bunker")
    return os.path.join(base, ".shim-state")


def _session_key() -> str:
    """$HERMES_SESSION_ID, else the shim's parent pid.

    The parent-pid fallback still keeps two concurrent sessions apart; what it
    does not survive is a restart of that parent, so the payload reports the key
    it used instead of pretending the hold is more durable than it is."""
    sid = os.environ.get(SESSION_ID_ENV, "").strip()
    return sid if sid else f"pid-{os.getppid()}"


def _safe_component(value: str) -> str:
    cleaned = "".join(c if (c.isalnum() or c in "._-") else "_" for c in value)
    return (cleaned or "session")[:200]


def _state_file(session_key: str | None = None) -> str:
    return os.path.join(_state_dir(), _safe_component(session_key or _session_key()) + ".json")


def _read_binding() -> dict:
    """This session's hold. A missing or unreadable file is reported, never raised."""
    path = _state_file()
    info = {"session_key": _session_key(), "state_file": path, "held": "",
            "bound_via": "", "bound_at": "", "read_error": ""}
    try:
        with open(path, "r") as fh:
            data = json.load(fh)
    except FileNotFoundError:
        return info
    except (ValueError, OSError) as exc:
        info["read_error"] = f"unreadable session state {path}: {exc}"
        return info
    if isinstance(data, dict) and isinstance(data.get("target"), str):
        info["held"] = data["target"].strip()
        info["bound_via"] = str(data.get("bound_via", ""))
        info["bound_at"] = str(data.get("bound_at", ""))
    return info


def _write_binding(target: str, verb: str) -> dict:
    """Persist this session's hold atomically (temp + rename, 0600)."""
    directory = _state_dir()
    os.makedirs(directory, mode=0o700, exist_ok=True)
    path = _state_file()
    payload = {
        "target": target,
        "bound_via": verb,
        "bound_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "session_key": _session_key(),
        "shim": "bunker-shim",
    }
    tmp = path + f".tmp.{os.getpid()}"
    with open(tmp, "w") as fh:
        json.dump(payload, fh, indent=2, sort_keys=True)
        fh.write("\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)
    return {"state_dir": directory, **payload}


# ---------------------------------------------------------------------------
# Target resolution — the exact GAP-093 chain, mirrored client-side so a call
# without any binding is refused HERE, before a CLI run can surprise us.
# ---------------------------------------------------------------------------

def _refusal_message() -> str:
    """The refusal names every remedy AND the known bunkers: a refusal that does
    not say what to do just teaches workarounds."""
    names = [s["name"] for s in _read_servers()[0]]
    known = ("known bunkers: " + ", ".join(names)) if names else (
        "no bunkers are registered in the CLI config — add one with `bunker connect`")
    return (
        f"no target bound: pass server='...' explicitly, bind this session with "
        f"bunker_switch(name='...') or bunker_hold(name='...'), or set {BINDING_ENV} "
        f"in this session's env ({known}; {BINDING_ENV} is the session binding — the "
        f"shared `active_server` default is NEVER consulted for a call, which is how "
        f"one session used to re-target another)"
    )


def _resolve_target_with_source(server: str | None, required: bool = True) -> tuple[str, str]:
    """(target, source). Sources: explicit | session_hold | env | unbound."""
    if server and server.strip():
        return server.strip(), "explicit"
    held = _read_binding()["held"]
    if held:
        return held, "session_hold"
    env = os.environ.get(BINDING_ENV, "").strip()
    if env:
        return env, "env"
    if not required:
        return "", "unbound"
    raise ValueError(_refusal_message())


def _resolve_target(server: str | None) -> str:
    return _resolve_target_with_source(server)[0]


def _run_bunker(args: list[str], server: str | None = None, timeout: int = 120,
                stdin: bytes | None = None) -> dict:
    """Run one bunker CLI command with the resolved target and structured output."""
    target = _resolve_target(server)
    argv = ["bunker", "--server", target] + args
    try:
        proc = subprocess.run(
            argv, input=stdin, timeout=timeout,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
    except FileNotFoundError:
        return {"error": "bunker_cli_missing",
                "detail": "the `bunker` binary is not on PATH; install it or fix PATH"}
    except subprocess.TimeoutExpired:
        return {"error": "timeout", "detail": f"bunker {' '.join(args[:2])} exceeded {timeout}s"}
    out = proc.stdout.decode("utf-8", errors="replace")
    err = proc.stderr.decode("utf-8", errors="replace")
    result = {"exit_code": proc.returncode, "server": target}
    if proc.returncode != 0:
        result["error"] = "cli_failed"
        result["stderr"] = err[-4000:]
    if out:
        result["stdout"] = out
    if err and proc.returncode == 0:
        result["stderr"] = err[-2000:]
    # Unverified calls must stay unverified: exit_code + raw streams are the
    # proof; nothing here is rendered as success on the caller's behalf.
    return result


def _capability_unavailable(verb: str, what: str) -> dict:
    return {"error": "capability_unavailable", "verb": verb,
            "detail": f"{what} (the verb stays on the surface; this target lacks it)"}


# ---------------------------------------------------------------------------
# The ten verbs — fixed surface. Tool schemas mirror the PRD S6 table.
# ---------------------------------------------------------------------------

@MCP.tool()
def bunker_read(path: str, server: str | None = None, offset: int = 1,
                limit: int = 2000, max_bytes: int = 100_000) -> dict:
    """Read a file on the target bunker: line-numbered window with total_lines/next_offset."""
    r = _run_bunker(["exec", "--", "awk", f'NR>={offset} && NR<{offset + limit} '
                    f'{{printf "%d\\t%s\\n", NR, $0}} END {{print "TOTAL_LINES:" NR}} '
                    f'ERRNO==0 {{}}', path], server=server)
    if "error" in r and r.get("error") != "cli_failed":
        return r
    return r


@MCP.tool()
def bunker_search(pattern: str, server: str | None = None, path: str = ".",
                  target: str = "content", output_mode: str = "content",
                  context: int = 0, file_glob: str | None = None,
                  limit: int = 50) -> dict:
    """Search the remote tree (ripgrep semantics): target=content|files, output_mode=content|files_only|count."""
    if target not in ("content", "files"):
        return {"error": "bad_param", "detail": "target must be content|files"}
    if output_mode not in ("content", "files_only", "count"):
        return {"error": "bad_param", "detail": "output_mode must be content|files_only|count"}
    # -e before user pattern; pattern is passed as ONE argv element (no shell)
    flags = ["-n", "--no-heading", "-m", str(limit)]
    if output_mode == "files_only":
        flags = ["-l"]
    elif output_mode == "count":
        flags = ["-c"]
    if context:
        flags += ["-C", str(context)]
    if file_glob:
        flags += ["-g", file_glob]
    args = ["exec", "--", "rg", *flags, "-e", pattern, path]
    if target == "files":
        args = ["exec", "--", "rg", "--files", path]
        if file_glob:
            args += ["-g", file_glob]
        # filter by pattern only in content mode; files mode lists paths
    return _run_bunker(args, server=server)


@MCP.tool()
def bunker_write(path: str, content: str, server: str | None = None) -> dict:
    """Whole-file write, atomic (temp + rename on the remote)."""
    script = (
        'd=$(dirname "$1"); t=$(mktemp "$d/.bunker-write.XXXXXX") || exit 1; '
        'cat > "$t" && mv -f "$t" "$1" || { rm -f "$t"; exit 1; }'
    )
    r = _run_bunker(["exec", "--", "sh", "-c", script, "bunker-write", path],
                    server=server, stdin=content.encode("utf-8", errors="surrogateescape"))
    return r


@MCP.tool()
def bunker_edit(path: str, old_string: str, new_string: str,
                replace_all: bool = False, server: str | None = None) -> dict:
    """Exact-match edit (no fuzz). Refuses when the match is not unique — naming the count."""
    script = r'''
count=$(grep -F -c -- "$B_OLD" "$B_PATH" 2>/dev/null || echo 0)
if [ "$count" -eq 0 ]; then echo "NO_MATCH"; exit 3; fi
if [ "$count" -gt 1 ] && [ "$B_ALL" != "1" ]; then echo "AMBIGUOUS:$count"; exit 4; fi
python3 - "$B_PATH" <<'PYEOF'
import os, sys
path = sys.argv[1]
old, new = os.environ["B_OLD"], os.environ["B_NEW"]
replace_all = os.environ["B_ALL"] == "1"
with open(path, "r", errors="surrogateescape") as f:
    s = f.read()
n = s.count(old)
if n == 0:
    print("NO_MATCH"); sys.exit(3)
if n > 1 and not replace_all:
    print(f"AMBIGUOUS:{n}"); sys.exit(4)
s = s.replace(old, new) if replace_all else s.replace(old, new, 1)
t = path + ".bunker-edit-tmp"
with open(t, "w", errors="surrogateescape") as f:
    f.write(s)
os.replace(t, path)
print("OK")
PYEOF
'''
    env_note = ("exact-match edit; a refusal names the match count "
                "(NO_MATCH / AMBIGUOUS:N) — no fuzzy matching by design")
    r = _run_bunker(["exec", "--env", f"B_OLD={old}", "--env", f"B_NEW={new}",
                     "--env", f"B_ALL={'1' if replace_all else '0'}",
                     "--env", f"B_PATH={path}",
                     "--", "sh", "-c", script, "bunker-edit"],
                    server=server)
    r["edit_semantics"] = env_note
    return r


@MCP.tool()
def bunker_patch(diff: str, server: str | None = None) -> dict:
    """Apply a unified diff, strict no-fuzz (git apply --check first, then apply)."""
    script = ('git apply --check --unsafe-paths "$1" || exit 5; '
              'git apply --unsafe-paths "$1"')
    r = _run_bunker(["exec", "--", "sh", "-c",
                     'cat > /tmp/.bunker-patch.$$.diff && '
                     'git apply --check /tmp/.bunker-patch.$$.diff && '
                     'git apply /tmp/.bunker-patch.$$.diff; rc=$?; '
                     'rm -f /tmp/.bunker-patch.$$.diff; exit $rc'],
                    server=server, stdin=diff.encode("utf-8"))
    return r


@MCP.tool()
def bunker_apply(files: list[dict], server: str | None = None) -> dict:
    """Atomic multi-file apply: each entry {path, content}; all-or-nothing (staged temp files, single rename pass)."""
    if not files:
        return {"error": "bad_param", "detail": "files must be a non-empty list"}
    payload = json.dumps(files)
    script = r'''
python3 - <<'PYEOF'
import json, os, sys
files = json.loads(os.environ["B_FILES"])
tmps = []
try:
    for f in files:
        d = os.path.dirname(f["path"]) or "."
        t = f"{d}/.bunker-apply.{os.getpid()}.{len(tmps)}"
        with open(t, "w", errors="surrogateescape") as fh:
            fh.write(f["content"])
        tmps.append((t, f["path"]))
    for t, dest in tmps:
        os.replace(t, dest)
    print(f"APPLIED:{len(tmps)}")
except Exception as e:
    for t, _ in tmps:
        try: os.remove(t)
        except OSError: pass
    print(f"FAILED:{e}"); sys.exit(6)
PYEOF
'''
    r = _run_bunker(["exec", "--env", f"B_FILES={payload}",
                     "--", "python3", "-c", script],
                    server=server)
    return r


@MCP.tool()
def bunker_exec(command: str, args: list[str] | None = None,
                stdin: str | None = None, base64: bool = False,
                exec_cap: int | None = None, timeout: int = 120,
                server: str | None = None) -> dict:
    """Run one command on the target bunker (streamed output, optional stdin/base64/cap)."""
    argv = ["exec"]
    if base64:
        argv.append("--base64")
    if exec_cap:
        argv += ["--exec-cap", str(exec_cap)]
    if stdin is not None:
        argv += ["--stdin", "-"]
    argv += ["--", command] + (args or [])
    r = _run_bunker(argv, server=server, timeout=timeout,
                    stdin=stdin.encode() if stdin is not None else None)
    return r


@MCP.tool()
def bunker_run(command: str, args: list[str] | None = None, detach: bool = True,
               name: str | None = None, timeout: int = 600,
               server: str | None = None) -> dict:
    """Run a long job on the target bunker; detach=true returns a run id immediately."""
    argv = ["run"]
    if detach:
        argv.append("--detach")
    if name:
        argv += ["--name", name]
    argv += ["--", command] + (args or [])
    return _run_bunker(argv, server=server, timeout=timeout)


@MCP.tool()
def bunker_lsp(op: str, file: str, server: str | None = None,
               line: int | None = None, character: int | None = None,
               root: str | None = None) -> dict:
    """Symbol query via the remote language server: op=definition|references|check."""
    if op not in ("definition", "references", "check"):
        return {"error": "bad_param", "detail": "op must be definition|references|check"}
    # toolsd lsp runs ON the agent; if toolsd is absent this surfaces
    # capability_unavailable (structured), not a missing tool.
    argv = ["exec", "--", "toolsd", "lsp", op, "--file", file]
    if line is not None:
        argv += ["--line", str(line)]
    if character is not None:
        argv += ["--character", str(character)]
    if root:
        argv += ["--root", root]
    r = _run_bunker(argv, server=server)
    if r.get("exit_code") != 0 and "toolsd" in r.get("stderr", ""):
        return _capability_unavailable("bunker_lsp", "toolsd not installed on the target agent")
    return r


@MCP.tool()
def bunker_lease(action: str, holder: str, paths: list[str] | None = None,
                 ttl: int = 300, server: str | None = None) -> dict:
    """Edit lease on the remote tree: action=acquire|renew|release|status; registry lives on the tree (shared across worktrees)."""
    if action not in ("acquire", "renew", "release", "status"):
        return {"error": "bad_param", "detail": "action must be acquire|renew|release|status"}
    argv = ["exec", "--", "toolsd", "lease", action, "--holder", holder, "--ttl", str(ttl)]
    for p in (paths or []):
        argv += ["--path", p]
    r = _run_bunker(argv, server=server)
    if r.get("exit_code") != 0 and "toolsd" in r.get("stderr", ""):
        return _capability_unavailable("bunker_lease", "toolsd not installed on the target agent")
    return r


# ---------------------------------------------------------------------------
# The three INSTANCE verbs (GAP-106) — the runtime half of "which bunker".
# Constant surface: these exist once, for any number of bunkers.
# ---------------------------------------------------------------------------

def _listing_payload(config: str | None = None) -> dict:
    servers, meta = _read_servers(config)
    session = _read_binding()
    payload = {
        "servers": servers,
        "count": len(servers),
        # Reported, never used as a binding (the GAP-093 hazard).
        "shared_default": meta["active_server"],
        "held": session["held"],
        "resolved_target": "",
        "resolved_via": "unbound",
        "session_key": session["session_key"],
        "state_file": session["state_file"],
        "config_path": meta["config_path"],
        "surface": {"tools": len(tool_names()), "constant": True},
        "precedence": "explicit server > session hold (switch/hold) > BUNKER_SESSION_TARGET > refusal",
        "mutating_verbs_never_use": (
            "the shared `active_server` default: binding here is session state, so a "
            "switch cannot re-target a sibling session"
        ),
    }
    target, source = _resolve_target_with_source(None, required=False)
    payload["resolved_target"] = target
    payload["resolved_via"] = source
    if session["read_error"]:
        payload["state_error"] = session["read_error"]
    if meta["diagnostic"]:
        payload["diagnostic"] = meta["diagnostic"]
    return payload


@MCP.tool()
def bunker_list(server: str | None = None, config: str | None = None) -> dict:
    """Discover which bunkers exist, and which one this session will call (one local config read; no daemon, no network).

    `server`/`config` are accepted for symmetry with the other verbs but never
    bind: a discovery call must not change what the session targets. A name that
    appears in this list is what `bunker_switch` and the per-call `server=...`
    parameter accept."""
    return _listing_payload(config)


@MCP.tool()
def bunker_switch(name: str, config: str | None = None) -> dict:
    """Bind THIS session to a named bunker; every later call without an explicit `server` routes there.

    The bind is session state (one file keyed by the session id), never a tool
    registration and never a write to the shared CLI config — a sibling session's
    target is untouched. An unknown name refuses and names the known bunkers."""
    servers, meta = _read_servers(config)
    known = [s["name"] for s in servers]
    if not servers:
        return {"error": "no_bunkers_registered", "requested": name,
                "detail": (meta["diagnostic"] or "the CLI config lists no servers") +
                          "; `bunker connect` registers one",
                "config_path": meta["config_path"]}
    if name not in known:
        return {"error": "unknown_bunker", "requested": name, "known": known,
                "detail": (f"no bunker named {name!r}; known bunkers: "
                           f"{', '.join(known)}. Switch to one of those, or pass "
                           f"server={name!r} explicitly on the call if you meant a "
                           f"bunker that is not registered here."),
                "config_path": meta["config_path"]}
    written = _write_binding(name, "bunker_switch")
    return {"held": name, "bound_via": "bunker_switch", "bound_at": written["bound_at"],
            "session_key": written["session_key"], "state_file": _state_file(),
            "known": known,
            "precedence": "explicit server > session hold (switch/hold) > BUNKER_SESSION_TARGET > refusal",
            "shared_default_not_used": meta["active_server"] or None}


@MCP.tool()
def bunker_hold(name: str | None = None, release: bool = False, config: str | None = None) -> dict:
    """Report the bunker THIS session is holding and which tier resolved it; `name` also binds it, `release` clears it.

    Reading the hold is how a session answers "which bunker am I on" at any
    point, and the reported `resolved_via` is the proof: `explicit` beats a hold,
    a hold beats BUNKER_SESSION_TARGET."""
    if release:
        path = _state_file()
        try:
            os.remove(path)
            released = True
        except FileNotFoundError:
            released = False
        except OSError as exc:
            return {"error": "state_unwritable", "state_file": path, "detail": str(exc)}
        payload = _listing_payload(config)
        payload.update({"released": released, "held": "",
                        "resolved_via": _resolve_target_with_source(None, required=False)[1]})
        return payload

    if name and name.strip():
        bound = bunker_switch(name.strip(), config=config)
        if "error" in bound:
            return bound

    payload = _listing_payload(config)
    payload["bound_via"] = _read_binding()["bound_via"]
    return payload


# ---------------------------------------------------------------------------
# Surface introspection — the constant-tool-count proof is code, not a comment.
# ---------------------------------------------------------------------------

_TOOL_NAMES = None


def tool_names() -> list[str]:
    """The REGISTERED MCP tool names, enumerated from the live FastMCP server.

    Cached: registration is fixed at import, and the count is the invariant
    tools/test_bunker_shim_instances.py pins for a 1-server and a 20-server
    config."""
    global _TOOL_NAMES
    if _TOOL_NAMES is None:
        import asyncio
        tools = asyncio.run(MCP.list_tools())
        _TOOL_NAMES = sorted(t.name for t in tools)
    return list(_TOOL_NAMES)


def _refresh_surface() -> None:
    """Test-only seam: re-list after import-time registration."""
    global _TOOL_NAMES
    _TOOL_NAMES = None


def _dispatch(call: dict) -> dict:
    """Route one {name, args} call to its verb, or refuse it by name."""
    handlers = {
        "bunker_read": bunker_read, "bunker_search": bunker_search,
        "bunker_write": bunker_write, "bunker_edit": bunker_edit,
        "bunker_patch": bunker_patch, "bunker_apply": bunker_apply,
        "bunker_exec": bunker_exec, "bunker_run": bunker_run,
        "bunker_lsp": bunker_lsp, "bunker_lease": bunker_lease,
        "bunker_list": bunker_list, "bunker_switch": bunker_switch,
        "bunker_hold": bunker_hold,
    }
    name = call.get("name", "")
    if name not in handlers:
        return {"error": "unknown_verb", "verb": name,
                "detail": f"verbs are exactly: {', '.join(tool_names())}"}
    args = dict(call.get("args") or {})
    try:
        return handlers[name](**args)
    except TypeError as exc:
        return {"error": "bad_args", "verb": name, "detail": str(exc)}
    except ValueError as exc:  # the fail-closed binding refusal
        return {"error": "no_target_bound", "verb": name,
                "detail": str(exc), "known": tool_names()}


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

if __name__ == "__main__":
    MCP.run()  # stdio transport by default
