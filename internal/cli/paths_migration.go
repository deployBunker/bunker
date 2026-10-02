package cli

import (
	"fmt"
	"os"
)

// GAP-181 migration notices. The legacy → config-dir adoption must be
// VISIBLE (an operator diffing two hosts must see that one has moved) but
// never loud enough to break machine-consumed stdout: both notices go to
// stderr, once per process, best-effort — a closed stderr is not an error.
// The vars are hooks so tests can capture the notices without a tty.

// recordLegacyMigration prints the one-line adoption notice: the legacy
// config was copied into the documented config dir, the legacy file was left
// in place untouched.
var recordLegacyMigration = func(legacyPath, adoptedPath string) {
	fmt.Fprintf(os.Stderr, "bunker: adopted legacy config %s into %s (legacy file left in place; move any keys/ you keep there to the new location)\n", legacyPath, adoptedPath)
}

// recordLegacyMigrationError prints why an adoption was skipped. The legacy
// location stays authoritative for this run; nothing is lost.
var recordLegacyMigrationError = func(err error) {
	fmt.Fprintf(os.Stderr, "bunker: warning: could not adopt the legacy config into the per-user config dir (%v); continuing from the legacy location\n", err)
}
