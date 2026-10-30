#!/bin/sh
# check-go-vendor-dir.sh — refuse a top-level `vendor/` that is not a Go module
# vendor tree.
#
# WHY (QA-BUNKER-25): the go command selects -mod=vendor whenever a `vendor`
# directory exists at the module root, even when vendor/modules.txt is absent
# (cmd/go/internal/modload readVendorList, Go 1.26: "Some vendor directories
# exist that don't contain modules.txt. This mostly happens when converting to
# modules. We want to preserve the behavior that mod=vendor is set"). With
# modules.txt missing, every build in a FRESH checkout dies with
#
#     go: inconsistent vendoring in <root>:
#         <mod>@<v>: is explicitly required in go.mod, but not marked as
#                    explicit in vendor/modules.txt
#
# and `go test ./...` then prints "[build failed]" once per package. That is
# exactly how a vendored NON-Go tree (vendor/sshfs, MOUNT-015) broke the
# clean-machine QA pass while developer boxes looked fine.
#
# Rule: `vendor/` must either not exist, or be a real module vendor tree
# (modules.txt present, no foreign-language sources at the top levels).
# Vendored non-Go projects belong under probes/ or another explicit path.
#
# Usage: check-go-vendor-dir.sh [root]        (default: current directory)
# Exit 0 = OK (one line), 1 = FAIL with a named reason.
set -u

root=${1:-.}
vendor=$root/vendor

if [ ! -d "$vendor" ]; then
	printf 'OK: no top-level vendor/ directory (go stays in module mode)\n'
	exit 0
fi

if [ ! -f "$vendor/modules.txt" ]; then
	printf 'FAIL: %s/ exists but has no modules.txt — the go command still selects -mod=vendor for it, so EVERY build/test in a fresh checkout fails with "inconsistent vendoring" (QA-BUNKER-25). Either delete the directory or move the vendored tree out of the reserved "vendor" name (see probes/sshfs-readdirplus/upstream/).\n' "$vendor" >&2
	exit 1
fi

# A module vendor tree holds Go packages. Foreign-language sources at its top
# levels mean somebody vendored a whole project, not dependencies.
stray=$(find "$vendor" -maxdepth 3 -type f \
	\( -name '*.c' -o -name '*.h' -o -name '*.cc' -o -name '*.py' -o -name '*.rs' \) \
	2>/dev/null | head -n 3)
if [ -n "$stray" ]; then
	printf 'FAIL: %s/ looks like a vendored PROJECT, not a module vendor tree; foreign sources found:\n%s\nMove that tree outside vendor/ (module vendoring owns this name).\n' "$vendor" "$stray" >&2
	exit 1
fi

printf 'OK: vendor/ is a module vendor tree (modules.txt present, no foreign sources)\n'
exit 0
