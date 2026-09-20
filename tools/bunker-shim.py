#!/usr/bin/env python3
"""bunker-shim: the CONSTANT-SURFACE Hermes tool layer over the bunker CLI (GAP-097).

One stdio MCP server. Exactly ten tools, always registered, regardless of how
many bunkers exist — the bunker is an ARGUMENT resolved per call:

    explicit `server` param  >  BUNKER_SESSION_TARGET env  >  REFUSAL

The refusal NAMES the missing binding and the two remedies (`--server` flag on
`bunker use`, or BUNKER_SESSION_TARGET in the session env). It never falls
back to a shared `active_server` default: whichever session last ran
`bunker use` must not silently re-target every other session's calls.

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

from mcp.server.fastmcp import FastMCP

MCP = FastMCP("bunker-shim")

BINDING_ENV = "BUNKER_SESSION_TARGET"

# ---------------------------------------------------------------------------
# Target resolution — the exact GAP-093 chain, mirrored client-side so a call
# without any binding is refused HERE, before a CLI run can surprise us.
# ---------------------------------------------------------------------------

def _resolve_target(server: str | None) -> str:
    if server and server.strip():
        return server.strip()
    env = os.environ.get(BINDING_ENV, "").strip()
    if env:
        return env
    raise ValueError(
        f"no target bound: pass server='...' explicitly, or set {BINDING_ENV} "
        f"in this session's env (per-session isolation: {BINDING_ENV} is the "
        f"session binding; `bunker use` writes a SHARED config and is refused "
        f"by the CLI for mutating commands — never a global default)"
    )


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
# main
# ---------------------------------------------------------------------------

if __name__ == "__main__":
    MCP.run()  # stdio transport by default
