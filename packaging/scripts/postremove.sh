#!/bin/sh
# deb: "remove", "purge", "upgrade", ...   rpm: 0 on final removal, 1 on upgrade.
set -e

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

case "$1" in
    remove|purge|0)
        # Deliberately left behind:
        #   /var/lib/silo -- the libraries, silo.db and every stored object.
        #     Removing a package must not remove the data it was serving.
        #   the silo user and group -- /var/lib/silo is still owned by that
        #     uid, and recycling it would hand those files to whoever gets the
        #     number next.
        # `systemctl reset-failed` so a stopped-with-error unit does not linger
        # in the failed list after the package is gone.
        if [ -d /run/systemd/system ]; then
            systemctl reset-failed silo.service >/dev/null 2>&1 || true
        fi
        ;;
esac

exit 0
