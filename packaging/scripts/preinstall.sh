#!/bin/sh
# Runs before the files land, on both deb and rpm. The two disagree about $1 --
# deb passes words ("install", "upgrade"), rpm passes counts (1, 2) -- so this
# script does not look at it: creating the account is idempotent and wanted in
# every case.
set -e

# systemd's StateDirectory= creates and chowns /var/lib/silo at first start, so
# all that is needed here is for the account to exist before the unit runs.
if ! getent group silo >/dev/null 2>&1; then
    groupadd --system silo
fi

if ! getent passwd silo >/dev/null 2>&1; then
    useradd --system \
        --gid silo \
        --home-dir /var/lib/silo \
        --no-create-home \
        --shell /usr/sbin/nologin \
        --comment "Silo file sync server" \
        silo
fi

exit 0
