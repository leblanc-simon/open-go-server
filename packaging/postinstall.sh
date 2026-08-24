#!/bin/sh
# Après installation : préparer l'état sans rien imposer. La configuration
# proprement dite reste du ressort de « ogs init », qui engendre la clé maître.
set -e

case "$1" in
configure)
    if [ ! -f /etc/ogs/config.yaml ]; then
        echo "ogs installé. Préparer l'instance avec :"
        echo "  ogs init"
    else
        # Mise à jour : les migrations de schéma sont appliquées au besoin.
        ogs doctor --quiet >/dev/null 2>&1 || true
    fi
    ;;
esac
