#!/usr/bin/env bash
# archive-run-artifacts.sh — keep the small, load-bearing files from a run
# directory and prove the rest can go.
#
# A 640 MiB tree leaves ~270 MiB of cache behind, and this fleet's root filesystem
# has been tight. The reusable evidence is the SAMPLES and the status document, not
# the blobs: everything in blobs/ is content-addressed bytes that the fixture
# regenerates deterministically. So: copy what the report cites, then report what
# was removed and what df says, so the cleanup is auditable rather than assumed.
#
# usage: archive-run-artifacts.sh <run-dir> <archive-dir>
set -uo pipefail
RUN="$1"; ARCH="$2"
[ -d "$RUN" ] || { echo "no such run dir: $RUN" >&2; exit 2; }
mkdir -p "$ARCH"
NAME="$(basename "$RUN")"
for f in sample.csv status.json index.json conflicts.jsonl du-total.txt du-blobs.txt \
         real-blobs-bytes.txt blob-count.txt other-files-bytes.txt cachedir-ls.txt reader.out; do
  [ -e "$RUN/$f" ] && cp "$RUN/$f" "$ARCH/$NAME--$f"
done
echo "archived from $RUN:"
ls -la "$ARCH" | awk -v n="$NAME" '$0 ~ n {printf "  %s\n", $9}' | head -20
