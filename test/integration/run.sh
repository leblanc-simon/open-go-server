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

# Le scénario de paquet a besoin du .deb, pas du binaire nu.
mount=()
if [ "$scenario" = "paquet" ]; then
    echo "construction du paquet…"
    make package >/dev/null
    mount=(-v "$PWD/dist:/paquet:ro")
fi

echo "exécution du scénario « $scenario »…"
echo
docker run --rm "${mount[@]}" ogs-integration "/scenarios/$scenario.sh"
