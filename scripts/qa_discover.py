#!/usr/bin/env python3
"""qa_discover.py — QA foreman discovery: pick N projects that need a full CI pass.

Selection (weighted, deterministic-ish):
  1. NEVER QA'd first (no record in the QA ledger)
  2. Then by days-since-last-QA, oldest first
  3. Tiebreak: random among candidates (the "hint of chaos" — the fleet is swept
     unevenly on purpose so bugs don't only live where we always look)
  4. Excludes: disabled projects, my-project, coding-hermes-scheduler (meta),
     projects QA'd in the last 24h, and candidates whose scheduler-resolved
     workdir is absent or is not accepted by git as a worktree.

Ledger: ~/.hermes/qa/ledger.jsonl  {ts, project, status, cells, server}
"""
import argparse, datetime, json, os, random, sqlite3, subprocess

SCHED_DB = '/home/kara/.hermes/scheduler.db'
QA_LEDGER = '/home/kara/.hermes/qa/ledger.jsonl'
EXCLUDE = {'my-project', 'coding-hermes-scheduler', 'duckbrain'}


def is_git_worktree(path):
    """Return true only for an existing Git repository root."""
    if not path or not os.path.isdir(path):
        return False
    try:
        inside = subprocess.run(
            ['git', '-C', path, 'rev-parse', '--is-inside-work-tree'],
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            timeout=5,
            check=False,
        )
        if inside.stdout.strip() != 'true':
            return False
        root = subprocess.run(
            ['git', '-C', path, 'rev-parse', '--show-toplevel'],
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            timeout=5,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    # A scheduler workdir must be the project repository itself. This rejects
    # a non-repo stub nested inside an unrelated parent repository (for
    # example ~/.hermes/stand-in/* under /home/kara's Git checkout).
    return root.returncode == 0 and os.path.realpath(root.stdout.strip()) == os.path.realpath(path)


def load_ledger():
    last = {}
    if os.path.exists(QA_LEDGER):
        for line in open(QA_LEDGER):
            try:
                r = json.loads(line)
                last[r['project']] = r['ts']
            except Exception:
                pass
    return last


def project_name_matches(query, ledger_project):
    """Match a project name or its hyphen-delimited suffix.

    A base-name query such as ``h3`` matches ``coding-hermes-h3`` but not
    ``coding-hermes-h3-sdk``.  The reverse direction is also accepted so a
    scheduler's full name (for example ``h3-foreman``) can find its base-name
    ledger row (``h3``).
    """
    if not query or not ledger_project:
        return False
    return (
        query == ledger_project
        or ledger_project.endswith('-' + query)
        or query.endswith('-' + ledger_project)
    )


def ledger_timestamp(project, ledger):
    """Return the exact ledger timestamp, or the newest matching suffix row."""
    if project in ledger:
        return ledger[project]
    matches = [
        timestamp
        for ledger_project, timestamp in ledger.items()
        if project_name_matches(project, ledger_project)
    ]
    return max(matches) if matches else None


def positive_int(value):
    """argparse type: reject N < 1 before any fleet scan can run."""
    try:
        iv = int(value)
    except ValueError:
        raise argparse.ArgumentTypeError('%r is not an integer' % (value,))
    if iv < 1:
        raise argparse.ArgumentTypeError(
            'N must be a positive integer (got %d)' % iv)
    return iv


class _Parser(argparse.ArgumentParser):
    """One-line errors: stock argparse prints the usage block above every
    error, which buries the actual message. Keep usage for --help only."""

    def error(self, message):
        self.exit(2, '%s: error: %s\n' % (self.prog, message))


def parse_args(argv=None):
    """Parse argv WITHOUT touching the scheduler DB or the ledger."""
    parser = _Parser(
        prog='qa_discover.py',
        description='QA foreman discovery: pick N projects that need a full CI pass.')
    parser.add_argument('n', nargs='?', type=positive_int, default=2,
                        metavar='N',
                        help='number of projects to pick (default: 2, must be >= 1)')
    return parser.parse_args(argv)


def pick_projects(n):
    """Return (out_dict, picks) for the N best candidates. Pure DB/ledger read."""
    random.seed()  # hint of chaos: non-reproducible tiebreak on purpose
    con = sqlite3.connect(SCHED_DB)
    enabled = [r[0] for r in con.execute("SELECT name FROM projects WHERE enabled = 1")]
    # Only REAL foreman projects: exclude role-driver rows (-pm, -sync,
    # dogfood/meta rows) — QA'ing a scheduler row's workdir is meaningless.
    enabled = [p for p in enabled
               if not p.endswith(('-pm', '-sync', '-qa', '-releng'))
               and 'dogfood' not in p.lower()
               and 'sync' not in p.lower()]
    # Real workdir per project (names can differ from paths — Bane 09-01:
    # ai-plays-poke → /home/kara/ai_plays_poke). Emitted per pick so the
    # pipeline never guesses repo paths.
    wd = {}
    for name, path in con.execute("SELECT name, workdir FROM projects"):
        wd[name] = path
    con.close()

    last = load_ledger()
    now = datetime.datetime.now(datetime.timezone.utc)
    cands = []
    for p in enabled:
        if p in EXCLUDE:
            continue
        ts = ledger_timestamp(p, last)
        if ts:
            try:
                age_days = (now - datetime.datetime.fromisoformat(ts.replace('Z', '+00:00'))).total_seconds() / 86400
            except Exception:
                age_days = 999
            if age_days < 1:
                continue
        else:
            age_days = 9999  # never QA'd
        repo = wd.get(p) or '/home/kara/' + p
        cands.append((p, age_days, repo))

    # A scheduler row can survive with a stub or missing workdir even when its
    # name does not match a role suffix. Exclude those candidates by resolved
    # workdir, not by name, and retain the reason in additive output fields.
    excluded = []
    valid = []
    for p, d, repo in cands:
        if not is_git_worktree(repo):
            excluded.append({'project': p, 'repo': repo, 'reason': 'workdir-not-git'})
            continue
        valid.append((p, d, repo))

    # sort: never-QA'd first, then oldest; random tiebreak among equal ages
    valid.sort(key=lambda x: (-1 if x[1] == 9999 else x[1], random.random()))
    picks = []
    for p, d, repo in valid[:n]:
        picks.append({
            'project': p,
            'repo': repo,
            'days_since_qa': round(d, 1) if d != 9999 else None,
            'reason': 'never-qa' if d == 9999 else 'oldest-qa',
        })

    out = {'count': n, 'as_of': now.isoformat(timespec='seconds'), 'picks': picks,
           'excluded': len(excluded), 'skipped': excluded,
           'note': 'QA foreman discovery — fresh-system CI pass candidates'}
    return out


def main(argv=None):
    args = parse_args(argv)
    print(json.dumps(pick_projects(args.n), indent=1))


if __name__ == '__main__':
    main()
