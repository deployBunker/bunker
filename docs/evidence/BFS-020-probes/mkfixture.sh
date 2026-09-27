#!/usr/bin/env bash
# mkfixture.sh DIR — build the served tree the BFS-020 arms use: a small git repo
# (so the battery's git block has a real repository to drive) plus the file set
# the committed battery expects (go.mod, src/).
set -euo pipefail
D="${1:?usage: mkfixture.sh DIR}"
mkdir -p "$D/src"
printf 'module fixture\n\ngo 1.21\n' > "$D/go.mod"
i=0
while [ "$i" -lt 12 ]; do printf 'package p%d\n' "$i" > "$D/src/p$i.go"; i=$((i+1)); done
git -C "$D" init -q -b main .
git -C "$D" add -A
git -C "$D" -c user.name=fixture -c user.email=fixture@invalid commit -qm "fixture: initial"
echo "fixture: $D ($(find "$D" -type f -not -path '*/.git/*' | wc -l) files, git HEAD $(git -C "$D" rev-parse --short HEAD))"
