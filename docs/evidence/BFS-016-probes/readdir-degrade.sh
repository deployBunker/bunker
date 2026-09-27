#!/usr/bin/env bash
# BFS-016 — is the empty readdir a property of a FRESH mount, or does it degrade?
# Everything here is bounded; the mount under test is started by the caller.
set -uo pipefail
M=${1:?mountpoint}
T=${T:-/tmp/bfs016/tree}

root() { ls -1 "$M" 2>&1 | tr '\n' ' '; }
say() { printf '%s\n' "$*"; }

say "fresh mount root listing     : [$(root)]"
say "native root listing          : [$(ls -1 "$T" | tr '\n' ' ')]"

say ""
say "-- trigger 1: create a FILE natively (out of band, agent side)"
printf 'x\n' > "$T/oob.txt"
say "   mount root now            : [$(root)]"
sleep 3
say "   after 3s (poll interval)  : [$(root)]"

say ""
say "-- trigger 2: create a DIRECTORY natively"
mkdir -p "$T/oobdir"
say "   mount root now            : [$(root)]"
sleep 3
say "   after 3s                  : [$(root)]"

say ""
say "-- trigger 3: DELETE that directory natively (the native-rmdir case)"
rmdir "$T/oobdir"
say "   mount root now            : [$(root)]"
sleep 3
say "   after 3s                  : [$(root)]"

say ""
say "-- trigger 4: a directory created through the MOUNT, then removed natively"
mkdir "$M/mdir" 2>/dev/null
say "   after mkdir via mount     : [$(root)]"
rmdir "$T/mdir" 2>/dev/null
say "   after native rmdir        : [$(root)]"
sleep 3
say "   after 3s                  : [$(root)]"

say ""
say "-- trigger 5: a plain through-the-mount write and delete"
printf 'y\n' > "$M/oob2.txt" 2>/dev/null; sleep 1
rm -f "$M/oob2.txt" 2>/dev/null; sleep 1
say "   mount root now            : [$(root)]"

say ""
say "-- full walk through the mount vs native"
say "   mount  find -mindepth 1   : $(find "$M" -mindepth 1 2>/dev/null | wc -l)"
say "   native find -mindepth 1   : $(find "$T" -mindepth 1 2>/dev/null | wc -l)"
rm -f "$T/oob.txt" 2>/dev/null
say ""
say "-- does an unrelated fresh directory still list correctly?"
say "   mount src entries         : $(ls -1 "$M/src" 2>/dev/null | wc -l)  (native: $(ls -1 "$T/src" | wc -l))"
say "   mount .git/objects dirs   : $(ls -1 "$M/.git/objects" 2>/dev/null | wc -l)  (native: $(ls -1 "$T/.git/objects" | wc -l))"
say "   mount pkg/deep/a/b/c      : [$(ls -1 "$M/pkg/deep/a/b/c" 2>&1 | tr '\n' ' ')]"
