#!/bin/sh
# Avant suppression : arrêter le renouvellement automatique des certificats.
# Les ressources des projets — comptes, bases, vhosts — ne sont jamais touchées :
# désinstaller l'outil d'administration ne doit pas défaire les sites qu'il gère.
set -e

case "$1" in
remove | upgrade | deconfigure)
    if command -v systemctl >/dev/null 2>&1; then
        systemctl disable --now ogs-renew.timer >/dev/null 2>&1 || true
    fi
    ;;
esac
