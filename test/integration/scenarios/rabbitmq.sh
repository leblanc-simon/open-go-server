#!/bin/bash
# Éprouve le provider RabbitMQ sur un vrai broker : vhost, utilisateur,
# permissions, exchanges, files, bindings, préréglage Messenger, et le point le
# plus délicat — les arguments de file, immuables une fois la file créée.
set -uo pipefail
. /scenarios/lib.sh

api="http://rabbitmq:15672/api"
auth="admin:motdepasse-essai"

echo "=== scénario : RabbitMQ ==="
curl -sf -u "$auth" "$api/overview" | grep -o '"management_version":"[^"]*"'

version=$(ls /etc/php | head -1)
ogs init --quiet
choisir_moteur apache
ogs amqp credentials set --user admin --password motdepasse-essai \
    --url http://rabbitmq:15672 --amqp-port 5672 >/dev/null

echo
echo "--- création d'un site avec deux transports Messenger ---"
ogs site create shop.fr --kind php-symfony --app-version 6.4 --php "$version" \
    --tls=false --messenger async --messenger emails --yes 2>&1 |
    grep -Ei "queue|exchange|vhost amqp|utilisateur amqp|⚠" | head -12

echo
echo "--- ce que le broker contient réellement ---"
echo "vhost : $(curl -sf -u "$auth" "$api/vhosts/shopfr" | grep -o '"name":"[^"]*"' | head -1)"
echo "user  : $(curl -sf -u "$auth" "$api/users/shopfr" | grep -o '"name":"[^"]*"' | head -1)"
echo "perms : $(curl -sf -u "$auth" "$api/permissions/shopfr/shopfr" | grep -o '"configure":"[^"]*"')"
for file in async emails async_failed emails_failed; do
    if ! curl -sf -u "$auth" "$api/queues/shopfr/$file" >/dev/null 2>&1; then
        echo "ÉCHEC : la file $file est absente"
        exit 1
    fi
done
echo "files : les quatre attendues sont présentes (deux transports, deux files d'échec)"

# Une permission qui déborderait sur « / » donnerait accès aux files des autres.
if curl -sf -u "$auth" "$api/permissions/shopfr/%2F" >/dev/null 2>&1; then
    echo "ÉCHEC : l'utilisateur a des permissions sur le vhost par défaut"
    exit 1
fi
echo "aucune permission sur « / » : l'isolation déclarée est respectée"

echo
echo "--- l'application peut-elle publier et consommer ? ---"
motdepasse=$(ogs secrets show shopfr amqp/shopfr 2>/dev/null | tail -1)
if [ -z "$motdepasse" ]; then
    echo "ÉCHEC : mot de passe AMQP introuvable dans le keystore"
    ogs secrets list shopfr
    exit 1
fi
export OGS_AMQP_PASS="$motdepasse"

# Le compte du projet n'a délibérément aucun droit de management : il faut donc
# parler AMQP, comme le fera l'application. C'est aussi ce qui éprouve le
# binding — un message publié sur la clé « async » doit atterrir dans la file.
python3 /scenarios/amqp-client.py publier || exit 1

echo
echo "--- idempotence ---"
sortie=$(ogs diff shopfr 2>&1)
echo "$sortie" | tail -3
if echo "$sortie" | grep -qiE "\+ queue|\+ exchange|\+ vhost amqp|~ queue"; then
    echo "ÉCHEC : écart signalé alors que rien n'a changé"
    exit 1
fi

echo
echo "--- arguments de file immuables ---"
# RabbitMQ refuse de modifier les arguments d'une file existante
# (PRECONDITION_FAILED). ogs doit le détecter et l'expliquer, et ne recréer la
# file que si on l'y autorise : la recréation détruit les messages en attente.
python3 /scenarios/amqp-client.py ajouter-ttl || exit 1
grep -A 6 "name: async$" /etc/ogs/projects/shopfr.yaml | head -8

sortie=$(ogs apply shopfr --yes 2>&1)
echo "$sortie" | grep -Ei "async|immuable|recreate|precondition" | head -4
if ! echo "$sortie" | grep -qiE "recreate-queues|immuable"; then
    echo "ÉCHEC : le changement d'argument n'a pas été expliqué :"
    echo "$sortie" | tail -6
    exit 1
fi
if curl -sf -u "$auth" "$api/queues/shopfr/async" | grep -q 'x-message-ttl'; then
    echo "ÉCHEC : la file a été modifiée sans autorisation"
    exit 1
fi
echo "la file n'a pas été touchée sans --recreate-queues"

echo
echo "--- recréation explicitement autorisée ---"
ogs apply shopfr --yes --recreate-queues 2>&1 | grep -Ei "queue async" | head -2
ttl=$(curl -sf -u "$auth" "$api/queues/shopfr/async" | grep -o '"x-message-ttl":[0-9]*')
if [ -z "$ttl" ]; then
    echo "ÉCHEC : la file n'a pas été recréée avec le nouvel argument"
    exit 1
fi
echo "argument appliqué après recréation : $ttl"

echo
echo "--- purge d'une file ---"
python3 /scenarios/amqp-client.py remplir-emails
# Les statistiques du broker sont différées : attendre que les messages soient
# comptés, sinon la purge porterait sur une file déjà vide et ne prouverait rien.
compte() { curl -sf -u "$auth" "$api/queues/shopfr/emails" | grep -o '"messages":[0-9]*' | head -1 | cut -d: -f2; }
for _ in $(seq 1 20); do
    [ "$(compte)" = "3" ] && break
    sleep 1
done
avant=$(compte)
if [ "$avant" != "3" ]; then
    echo "ÉCHEC : les 3 messages publiés ne sont pas dans la file (compté : $avant)"
    exit 1
fi
ogs amqp queue purge shopfr emails --yes 2>&1 | tail -1
for _ in $(seq 1 20); do
    [ "$(compte)" = "0" ] && break
    sleep 1
done
apres=$(compte)
echo "avant : $avant message(s), après : $apres"
if [ "$apres" != "0" ]; then
    echo "ÉCHEC : la file n'est pas vide après la purge"
    exit 1
fi

echo
echo "--- suppression : le vhost et l'utilisateur partent avec le projet ---"
ogs project delete shopfr --yes >/dev/null 2>&1
if curl -sf -u "$auth" "$api/vhosts/shopfr" >/dev/null 2>&1; then
    echo "ÉCHEC : le vhost subsiste"
    exit 1
fi
if curl -sf -u "$auth" "$api/users/shopfr" >/dev/null 2>&1; then
    echo "ÉCHEC : l'utilisateur subsiste"
    exit 1
fi
echo "vhost et utilisateur supprimés"

echo
echo "RÉSULTAT : le provider RabbitMQ fonctionne sur un vrai broker — isolation"
echo "           par vhost, bindings effectifs, arguments immuables respectés."
