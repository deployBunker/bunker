#!/bin/bash
# install-exec-audit.sh (GAP-048): provision snoopy exec-level audit on a
# bunker host.
#
# Purpose: bunker agents run arbitrary shell commands on the host, but
# nothing records what they actually execute. This script installs and
# configures snoopy (an LD_PRELOAD execve() logger) so EVERY command lands
# in journald with uid/euid/cwd/cmdline — enabling audit trails and the
# uid -> agent-id correlation documented in docs/exec-audit.md.
#
# Target: Debian/Ubuntu bunker hosts. Must run as root (no sudo use).
# Idempotent: a second run is a no-op (exit 0, short message, zero
# duplicate config changes).
#
# Install/configure steps:
#   1. apt-get install -y snoopy (fails hard if the package is missing)
#   2. ensure the snoopy .so path is present in /etc/ld.so.preload
#      (grep-guarded append — never duplicated)
#   3. write /etc/snoopy.ini with a uid/euid/cwd/cmdline message format
#      (existing custom content is backed up to /etc/snoopy.ini.bak-<ts>)
set -uo pipefail

die() {
    echo "ERROR: $*" >&2
    exit 1
}

# --- must be root -----------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
    die "must run as root (EUID=$(id -u)). Re-run with root privileges; this script does not use sudo."
fi

# --- 1. install snoopy ------------------------------------------------------
install_snoopy() {
    echo "installing snoopy via apt-get..."
    if ! apt-get install -y snoopy; then
        die "apt-get install -y snoopy failed — is the snoopy package available in the configured repos? (Debian/Ubuntu required)"
    fi
    if ! dpkg-query -W -f='${Status}' snoopy 2>/dev/null | grep -q 'install ok installed'; then
        die "snoopy package not installed after apt-get (dpkg-query check failed)."
    fi
    echo "snoopy package installed."
}

# --- resolve the snoopy .so path --------------------------------------------
find_snoopy_so() {
    # dpkg -L is authoritative on Debian/Ubuntu; fall back to the common
    # multiarch layout. NEVER append a nonexistent path to /etc/ld.so.preload
    # — that breaks exec() for EVERY program on the host.
    local so
    so="$(dpkg -L snoopy 2>/dev/null | grep -E 'libsnoopy\.so$' | head -n1)"
    if [ -z "$so" ]; then
        so="$(ls /usr/lib/*/snoopy/libsnoopy.so /usr/lib/snoopy/libsnoopy.so 2>/dev/null | head -n1)"
    fi
    if [ -z "$so" ] || [ ! -f "$so" ]; then
        die "libsnoopy.so not found on disk (dpkg -L snoopy returned nothing usable)."
    fi
    echo "$so"
}

# --- 2. enable via /etc/ld.so.preload (grep-guarded) ------------------------
enable_preload() {
    local so="$1" preload="/etc/ld.so.preload"
    if [ -f "$preload" ] && grep -qxF "$so" "$preload"; then
        echo "snoopy already enabled in $preload ($so)."
    else
        touch "$preload"
        echo "$so" >> "$preload"
        echo "enabled snoopy: appended '$so' to $preload."
    fi
}

# --- 3. write /etc/snoopy.ini (backup-then-write, marker-guarded) -----------
write_config() {
    local ini="/etc/snoopy.ini"
    local marker="# Managed by bunker install-exec-audit.sh (GAP-048)"
    if [ -f "$ini" ] && grep -qF "$marker" "$ini"; then
        echo "snoopy config already managed ($ini)."
        return 0
    fi
    if [ -f "$ini" ]; then
        local bak="${ini}.bak-$(date +%Y%m%d%H%M%S)"
        cp -a "$ini" "$bak" || die "failed to back up $ini to $bak"
        echo "backed up existing $ini to $bak."
    fi
    cat > "$ini" <<EOF
$marker
# Exec-level audit for bunker hosts (GAP-048).
# Every execve() on this host is logged to syslog/journald with
# uid, euid, cwd and the full command line.
#
# Note: %{cmdline} is the snoopy 2.x datasource for the full command line
# (the 1.x name %{cmd} was renamed upstream).

[snoopy]
message_format = "uid=%{uid} euid=%{euid} cwd=%{cwd} cmd=%{cmdline}"
EOF
    if ! grep -q '^message_format' "$ini"; then
        die "failed to write $ini (message_format missing after write)."
    fi
    echo "wrote $ini with uid/euid/cwd/cmdline message format."
}

# --- 4. idempotency short-circuit -------------------------------------------
# If everything is already in place, exit 0 without touching anything.
already_configured() {
    local so
    so="$(dpkg -L snoopy 2>/dev/null | grep -E 'libsnoopy\.so$' | head -n1)"
    [ -n "$so" ] && [ -f "$so" ] \
        && [ -f /etc/ld.so.preload ] && grep -qxF "$so" /etc/ld.so.preload \
        && [ -f /etc/snoopy.ini ] && grep -qF "# Managed by bunker install-exec-audit.sh (GAP-048)" /etc/snoopy.ini
}

if already_configured; then
    echo "snoopy already installed and configured — nothing to do (idempotent no-op)."
    exit 0
fi

install_snoopy
SNOOPY_SO="$(find_snoopy_so)"
enable_preload "$SNOOPY_SO"
write_config

echo "OK: snoopy exec-level audit active — every execve() now logs to journald (journalctl -t snoopy)."
exit 0
