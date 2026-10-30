"""Tests for qa_discover.py CLI arg handling and ledger matching.

Regression coverage for:
- `--help` must print usage and exit 0 (was: ValueError traceback from
  int() on sys.argv[1] before any parsing).
- Negative / zero N must be rejected with a one-line stderr error and a
  nonzero exit, never dumping the whole fleet.
- Positive N and the no-arg default are unchanged.
- Base project names match full scheduler names at a hyphen-delimited suffix.
"""
import pytest

import qa_discover


def test_default_n_is_2_when_no_args():
    args = qa_discover.parse_args([])
    assert args.n == 2


def test_positive_n_accepted():
    assert qa_discover.parse_args(['5']).n == 5
    assert qa_discover.parse_args(['1']).n == 1


def test_help_exits_zero_with_usage(capsys):
    with pytest.raises(SystemExit) as exc:
        qa_discover.parse_args(['--help'])
    assert exc.value.code == 0
    out = capsys.readouterr().out
    assert 'usage:' in out
    assert 'N' in out
    assert 'projects that need a full CI pass' in out


def test_negative_n_rejected(capsys):
    with pytest.raises(SystemExit) as exc:
        qa_discover.parse_args(['-3'])
    assert exc.value.code != 0
    err = capsys.readouterr().err
    assert 'positive integer' in err
    assert '-3' in err


def test_zero_n_rejected(capsys):
    with pytest.raises(SystemExit) as exc:
        qa_discover.parse_args(['0'])
    assert exc.value.code != 0
    assert 'positive integer' in capsys.readouterr().err


def test_non_integer_n_rejected(capsys):
    with pytest.raises(SystemExit) as exc:
        qa_discover.parse_args(['abc'])
    assert exc.value.code != 0
    assert 'integer' in capsys.readouterr().err


def test_positive_int_type_boundary():
    assert qa_discover.positive_int('7') == 7
    with pytest.raises(Exception):
        qa_discover.positive_int('-1')


def test_exact_project_name_matches():
    assert qa_discover.project_name_matches("bunker", "bunker")


def test_base_name_matches_full_scheduler_name_suffix():
    assert qa_discover.project_name_matches("h3", "coding-hermes-h3")
    assert qa_discover.project_name_matches("crier", "coding-hermes-crier")


def test_full_scheduler_name_matches_base_ledger_name():
    assert qa_discover.project_name_matches("coding-hermes-h3", "h3")


def test_foreman_project_names_match_repository_names():
    assert qa_discover.project_name_matches("h3", "h3-foreman")
    assert qa_discover.project_name_matches("crier", "crier-foreman")
    assert qa_discover.project_name_matches("h3-foreman", "h3")


def test_workdir_basename_matches_ledger_row():
    ledger = {"h3": "2026-10-30T00:00:00+00:00"}
    assert qa_discover.ledger_timestamp(
        "h3-foreman", ledger, "/home/kara/h3") == ledger["h3"]
    assert qa_discover.ledger_timestamp(
        "crier-foreman", {"crier": ledger["h3"]}, "/home/kara/crier") == ledger["h3"]


def test_workdir_basename_preserves_hyphen_boundary():
    ledger = {"h3-sdk": "2026-10-30T00:00:00+00:00"}
    assert qa_discover.ledger_timestamp(
        "h3-foreman", ledger, "/home/kara/h3") is None
    assert qa_discover.ledger_timestamp(
        "h3-sdk-foreman", ledger, "/home/kara/h3-sdk") == ledger["h3-sdk"]


def test_workdir_basename_prevents_false_never_qa_pick(monkeypatch):
    class FakeConnection:
        def execute(self, query):
            if "SELECT name FROM projects" in query:
                return [("h3-foreman",)]
            return [("h3-foreman", "/home/kara/h3")]

        def close(self):
            pass

    monkeypatch.setattr(qa_discover.sqlite3, "connect", lambda _: FakeConnection())
    monkeypatch.setattr(
        qa_discover,
        "load_ledger",
        lambda: {"h3": "2026-10-30T00:00:00+00:00"},
    )
    monkeypatch.setattr(qa_discover, "is_git_worktree", lambda _: True)

    result = qa_discover.pick_projects(1)

    assert result["picks"] == []
    assert result["excluded"] == 0


def test_longer_suffix_does_not_match_shorter_query():
    assert not qa_discover.project_name_matches("h3", "coding-hermes-h3-sdk")


def test_unrelated_names_do_not_match():
    assert not qa_discover.project_name_matches("h3", "crier")
    assert not qa_discover.project_name_matches("bunker", "bunkerd")


def test_exact_match_has_priority_over_suffix_matches():
    ledger = {
        "h3": "2026-10-01T00:00:00+00:00",
        "coding-hermes-h3": "2026-10-02T00:00:00+00:00",
    }
    assert qa_discover.ledger_timestamp("h3", ledger) == ledger["h3"]


def test_suffix_match_uses_newest_matching_ledger_row():
    ledger = {
        "coding-hermes-h3": "2026-10-01T00:00:00+00:00",
        "fleet-h3": "2026-10-02T00:00:00+00:00",
        "coding-hermes-h3-sdk": "2026-10-03T00:00:00+00:00",
    }
    assert qa_discover.ledger_timestamp("h3", ledger) == ledger["fleet-h3"]
