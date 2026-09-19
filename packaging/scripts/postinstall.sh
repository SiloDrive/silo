#!/bin/sh
# deb passes "configure" with the old version as $2 on upgrade; rpm passes 1 on
# a fresh install and 2 on an upgrade. Both are handled below.
set -e

fresh_install() {
    case "$1" in
        configure) [ -z "$2" ] ;;   # deb: no previous version
        1) true ;;                  # rpm: install
        *) false ;;
    esac
}

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
    # Only restart something that is already running. An upgrade should not
    # start a service the operator deliberately left stopped.
    systemctl try-restart silo.service >/dev/null 2>&1 || true
fi

if fresh_install "$@"; then
    cat <<'MSG'

Silo is installed but not enabled. Before opening the port:

  1. Review /etc/silo/silo.conf and /etc/silo/silo.env.
     Behind a reverse proxy, set SILO_TRUST_PROXY_HEADERS=true in silo.env --
     without it every client shares one rate-limit bucket.

  2. Start it:        sudo systemctl enable --now silo

  3. Claim the server, on the host, before anything else can:
                      sudo -u silo silo -d /var/lib/silo setup-token

Silo listens on 127.0.0.1:8082 and speaks plaintext by design. Terminate TLS
at a reverse proxy; see the deployment guide.

MSG
fi

exit 0
