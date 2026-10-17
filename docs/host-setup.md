# Host Setup

## Post-commit hook

The host-wide post-commit hook lives at `/home/kara/.hermes/hooks/post-commit`.
It runs `hilo graph warm --changed` after every commit to keep the dependency
graph up to date across the fleet.

**Status**: Repaired 2026-10-17. Previously corrupted with 48 stacked `#!/bin/sh`
lines — core.hooksPath=/home/kara/.hermes/hooks bypasses all repo-local hooks,
so every repo on the host had HILO graph warm silently skipped.

## Core hooks path

The Git config `core.hooksPath=/home/kara/.hermes/hooks` points every repo's
hooks to this single file. A failing hook still allows commits to land (git
ignores post-commit exit status), so corruption is silent. Verify with:

    sh -n /home/kara/.hermes/hooks/post-commit   # must exit 0
    grep -c '#!/bin/sh' /home/kara/.hermes/hooks/post-commit  # must be 1
