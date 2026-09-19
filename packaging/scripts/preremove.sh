#!/bin/sh
# Stop the service only when the package is actually going away.
#   deb: $1 is "remove" on removal, "upgrade" when a new version follows.
#   rpm: $1 is 0 on final removal, 1 during an upgrade.
set -e

case "$1" in
    remove|purge|0)
        if [ -d /run/systemd/system ]; then
            systemctl --no-reload disable --now silo.service >/dev/null 2>&1 || true
        fi
        ;;
esac

exit 0
