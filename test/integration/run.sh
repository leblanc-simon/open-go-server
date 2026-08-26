#!/bin/bash
# Construit l'image d'intégration et exécute un scénario. Ces essais exigent un
# système complet — serveur web, PHP, comptes — que les tests unitaires ne
# peuvent pas simuler.
set -euo pipefail

cd "$(dirname "$0")/../.."
scenario="${1:-reload-group}"

echo "compilation du binaire statique…"
CGO_ENABLED=0 go build -trimpath -o ogs ./cmd/ogs

# Les scénarios de runtime déposent une application : c'est un vrai binaire Go,
# compilé ici, qui respecte le contrat d'écoute imposé par ogs.
if [ "$scenario" = "runtimes" ] || [ "$scenario" = "runtimes-compiles" ]; then
    mkdir -p test/integration/dist
    (cd test/integration/app && CGO_ENABLED=0 go build -trimpath -o ../dist/app .)
fi

echo "construction de l'image…"
# Le registre public est un tiers faillible : douze scénarios tirent la même
# image de base en parallèle, et un jeton d'authentification qui n'arrive pas
# fait échouer un essai qui n'a rien à voir avec le code. On réessaie, avec une
# attente croissante, avant de conclure.
for tentative in 1 2 3; do
    if docker build -q -f test/integration/Dockerfile -t ogs-integration . >/dev/null; then
        break
    fi
    if [ "$tentative" = 3 ]; then
        echo "échec de la construction de l'image après trois tentatives" >&2
        exit 1
    fi
    echo "construction en échec, nouvelle tentative dans $((tentative * 15)) s…" >&2
    sleep $((tentative * 15))
done

# Les scénarios de runtime et de services exigent un vrai systemd en PID 1 :
# rien de ce qui suit un « systemctl enable » ne s'éprouve autrement. La
# suppression en fait partie : c'est le rechargement de PHP-FPM par systemd qui
# libère les workers du projet, et donc son compte.
case "$scenario" in
systemd | runtimes | runtimes-compiles | harden | suppression | web-durci)
    nom="ogs-integration-$scenario"
    docker rm -f "$nom" >/dev/null 2>&1 || true
    trap 'docker rm -f '"$nom"' >/dev/null 2>&1' EXIT
    echo "démarrage de systemd en PID 1…"
    docker run -d --name "$nom" --privileged --cgroupns=host \
        --tmpfs /run --tmpfs /run/lock \
        -v "$PWD/test/integration/scenarios:/scenarios:ro" \
        -v "$PWD/test/integration/dist:/depot:ro" \
        --entrypoint /sbin/init ogs-integration >/dev/null
    for _ in $(seq 1 30); do
        etat=$(docker exec "$nom" systemctl is-system-running 2>/dev/null || true)
        case "$etat" in running | degraded) break ;; esac
        sleep 1
    done
    echo "systemd : ${etat:-indéterminé}"
    echo "exécution du scénario « $scenario »…"
    echo
    docker exec "$nom" "/scenarios/$scenario.sh"
    exit $?
    ;;
esac

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
