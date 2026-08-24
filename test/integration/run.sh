#!/bin/bash
# Construit l'image d'intégration et exécute un scénario. Ces essais exigent un
# système complet — serveur web, PHP, comptes — que les tests unitaires ne
# peuvent pas simuler.
set -euo pipefail

cd "$(dirname "$0")/../.."
scenario="${1:-reload-group}"

echo "compilation du binaire statique…"
CGO_ENABLED=0 go build -trimpath -o ogs ./cmd/ogs

echo "construction de l'image…"
docker build -q -f test/integration/Dockerfile -t ogs-integration . >/dev/null

# Les scénarios ont besoin d'un service tiers joignable par le réseau : la
# composition les fournit, et le scénario s'exécute dans le conteneur « runner ».
case "$scenario" in
rabbitmq | pebble)
    echo "démarrage des services tiers…"
    cd test/integration
    trap 'docker compose down -t 1 >/dev/null 2>&1' EXIT
    docker compose up -d --wait >/dev/null
    echo "exécution du scénario « $scenario »…"
    echo
    # « --use-aliases » est indispensable : sans lui le conteneur créé par
    # « run » n'hérite pas des alias réseau, et l'autorité ACME ne résout pas le
    # domaine du certificat.
    docker compose run --rm --no-deps --use-aliases runner "/scenarios/$scenario.sh"
    exit $?
    ;;
esac

# Les scénarios sont montés plutôt que figés dans l'image : les mettre au point
# ne demande pas de reconstruire.
mount=(-v "$PWD/test/integration/scenarios:/scenarios:ro")
if [ "$scenario" = "paquet" ]; then
    echo "construction du paquet…"
    make package >/dev/null
    mount=(-v "$PWD/dist:/paquet:ro")
fi

echo "exécution du scénario « $scenario »…"
echo
docker run --rm "${mount[@]}" ogs-integration "/scenarios/$scenario.sh"
