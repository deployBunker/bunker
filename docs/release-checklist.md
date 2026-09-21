# Release Checklist — bunker

How to cut a release (next one: `v0.2.0`) without tripping CI's version
checks. This procedure exists because a bare tag push **goes red on the tag
itself**: the tag build check greps the tag's own tree, so a tag pointing at a
pre-bump commit fails even when HEAD is correct — and once the tag is on the
module proxy, the cached zip cannot be refreshed by force-moving the tag
(GAP-043 force-move trap).

## The version-authority parity invariant

CI enforces three-way parity between the newest git tag, the CLI version, and
the CHANGELOG's top release heading. From `.github/workflows/ci.yml`,
"Version authority check" (lines 107-127):

```yaml
- name: Version authority check
  run: |
    set -euo pipefail
    # QA-BUNKER-13: no tags in the checkout (act run / shallow clone /
    # fresh fork / tarball) would kill this step with exit 128 under
    # `set -euo pipefail`. Hosted CI fetches full depth (fetch-depth: 0)
    # and enforces the parity check there; tag-less checkouts skip it
    # with a visible warning annotation instead of failing the job.
    if ! git describe --tags --abbrev=0 >/dev/null 2>&1; then
      echo "::warning::no tags in checkout (act/shallow/fork) — version-authority parity check skipped (hosted CI runs full-depth fetch-depth: 0 and enforces it)"
      exit 0
    fi
    latest_tag=$(git describe --tags --abbrev=0)
    cli_version=$(/tmp/bunker-smoke version | awk 'NR==1 {print $2}')
    # GAP-081: the CHANGELOG's top heading is `## Unreleased` while commits
    # sit past the newest release tag, so the parity check must read the
    # newest RELEASE heading, not the first heading of any kind.
    changelog_top=$(grep -m1 -E '^## [0-9]' CHANGELOG.md | awk '{print $2}')
    echo "latest tag: ${latest_tag} | bunker version: ${cli_version} | CHANGELOG: ${changelog_top}"
    test "${latest_tag}" = "v${cli_version}"
    test "${cli_version}" = "${changelog_top}"
```

Concretely, the invariant is:

    git describe --tags --abbrev=0  ==  `bunker version` (first line, field 2)
                                    ==  newest `## N.N.N` heading in CHANGELOG.md

(`bunker version` prints `bunker <version>` on its first line; CI takes field
2. The parity check reads the newest *release* heading, not the first heading
of any kind, because the top heading is `## Unreleased` between releases —
the GAP-081 comment above.)

## Why a bare tag push goes red

The "Tag build check" step (ci.yml, starting line 136) resolves the newest
tag, derives the expected version from the tag NAME, and greps the TAG'S OWN
TREE — not HEAD — for the matching version declaration:

```yaml
latest_tag=$(git describe --tags --abbrev=0)
expected=$(echo "${latest_tag#v}")
git show "${latest_tag}:internal/version/version.go" | grep -q "Version = \"${expected}\""
```

It then extracts the tag tree via `git archive`, builds it, and requires the
built binary to print `bunker <version>`. So `git tag v0.2.0` on a commit
whose `internal/version/version.go` still says `0.1.4` fails ON THE TAG — a
bump commit pushed afterwards does not repair the failing artefact, which is
the tag object itself. And because `go install ...module@v0.2.0` fetches a
proxy-cached zip of the tag, a force-moved tag does not fix anything either
(GAP-043): the proxy keeps serving the first-seen zip. A wrong tag has no
honest in-place fix — which is why the order below makes a wrong tag
impossible to cut.

## The safe order (v0.2.0 cut procedure)

Everything rides in ONE atomic commit, tagged and pushed together:

1. Edit the five version surfaces in one commit:
   a. `CHANGELOG.md` — rename the `## Unreleased` section to
      `## 0.2.0 (YYYY-MM-DD)` and open a fresh empty `## Unreleased` ABOVE
      it (the GAP-081 release-drift check requires an Unreleased section once
      commits exist past the newest tag).
   b. `internal/version/version.go` — `Version = "0.2.0"`.
   c. `internal/version/version_test.go` — update the `TestDefaults`
      expectation to `"0.2.0"`.
   d. `Makefile` — `VERSION ?= 0.2.0`.
   e. the doc comment in `internal/version/version.go` referencing the
      newest release (currently mentions v0.1.4 / GAP-070).
2. Run `scripts/pre-release-check.sh` (must exit 0), plus `go build ./...`
   and `go test ./internal/version/`.
3. `git commit` — one commit, all five surfaces.
4. `git tag v0.2.0` on that commit.
5. `scripts/pre-release-check.sh --tag v0.2.0` (must exit 0).
6. `git push origin main v0.2.0` — branch and tag in ONE push, so CI never
   observes the tag pointing at an unpushed or stale commit.

Historical race, for the record: the v0.1.4 release push FAILED
"Version authority check" because the release commit raced the CHANGELOG
parity commit (it healed 2 minutes later). This checklist exists so that
never happens again: one commit, one tag, one push.

## Post-tag verification

    go install github.com/deployBunker/bunker/cmd/bunker@v0.2.0
    bunker version   # first line must be: bunker 0.2.0

If `go install` builds and `bunker version` reports 0.2.0, the proxy zip is
good and the release is live.

## Local gate

`scripts/pre-release-check.sh` is the fail-fast local counterpart of the CI
checks above: no arguments verifies the tree (the three version sources agree,
newest tag matches, `## Unreleased` is intact); `--tag <tag>` verifies a
freshly cut tag's content before the push. Run it as the last local step
before `git push` in the procedure above.
