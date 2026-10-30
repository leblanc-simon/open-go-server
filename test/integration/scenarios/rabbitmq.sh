#!/bin/bash
# Éprouve le provider RabbitMQ sur un vrai broker : vhost, utilisateur,
# permissions, exchanges, files, bindings, préréglage rabbitmq:consumer, et le point le
# plus délicat — les arguments de file, immuables une fois la file créée.
set -uo pipefail
. /scenarios/lib.sh

api="http://rabbitmq:15672/api"
auth="admin:motdepasse-essai"

echo "=== scénario : RabbitMQ ==="
curl -sf -u "$auth" "$api/overview" | grep -o '"management_version":"[^"]*"'

version=$(version_php)
ogs init --quiet
choisir_moteur apache
# Le mot de passe passe par l'entrée standard, jamais par la ligne de commande :
# il y figurerait dans le journal de sudo et l'historique du shell (D93).
printf '%s\n' motdepasse-essai | ogs amqp credentials set --user admin \
    --url http://rabbitmq:15672 --amqp-port 5672 >/dev/null ||
    { echo "ÉCHEC : identifiants non enregistrés depuis l'entrée standard"; exit 1; }
grep -q "password: motdepasse-essai" /etc/ogs/credentials/amqp.yaml ||
    { echo "ÉCHEC : le mot de passe lu sur l'entrée standard n'est pas celui enregistré"; exit 1; }

echo
echo "--- création d'un site avec deux transports et leurs files d'échec ---"
# Messenger déclare lui-même sa topologie : ogs ne lui pose que vhost, compte et
# permissions. Le préréglage complet, files d'échec comprises sous --failed, ne
# vaut que pour les workers rabbitmq:consumer de Symfony avant 4.1 : c'est lui
# qu'éprouve ce scénario.
ogs site create shop.fr --kind php-symfony --app-version 3.4 --php "$version" \
    --tls=false --apply=false >/dev/null ||
    { echo "ÉCHEC : déclaration du site refusée"; exit 1; }
ogs amqp messenger shopfr --transport async --transport emails --failed >/dev/null ||
    { echo "ÉCHEC : préréglage AMQP refusé"; exit 1; }
ogs apply shopfr --yes 2>&1 |
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
echo "--- vhost fermé au compte d'administration (D124) ---"
# Le tag « administrator » fait tout lire, mais n'ouvre aucun vhost : un vhost
# créé sous un autre compte d'administration rendait 401 à la première
# déclaration, et l'apply se déroulait sur « identifiants refusés ». Le binding
# retiré oblige l'apply à écrire dans le vhost.
# Le binding part d'abord : sans permissions, l'administrateur lui-même ne
# peut plus le retirer.
curl -sf -u "$auth" -X DELETE "$api/bindings/shopfr/e/messages/q/async/async" ||
    { echo "ÉCHEC : binding non retiré"; exit 1; }
curl -sf -u "$auth" -X DELETE "$api/permissions/shopfr/admin" ||
    { echo "ÉCHEC : permissions du compte d'administration non retirées"; exit 1; }
sortie=$(ogs apply shopfr --yes 2>&1); code=$?
echo "$sortie" | grep -Ei "administration|binding" | head -3
if [ "$code" != 0 ]; then
    echo "ÉCHEC : l'apply rend $code sur un vhost fermé au compte d'administration :"
    echo "$sortie" | tail -6
    exit 1
fi
curl -sf -u "$auth" "$api/permissions/shopfr/admin" | grep -q '"configure":".\*"' ||
    { echo "ÉCHEC : le vhost n'a pas été rouvert au compte d'administration"; exit 1; }
curl -sf -u "$auth" "$api/bindings/shopfr/e/messages/q/async" | grep -q '"routing_key":"async"' ||
    { echo "ÉCHEC : le binding n'a pas été rétabli"; exit 1; }
echo "vhost rouvert au compte d'administration, binding rétabli"

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
echo "--- ce que la déclaration ne porte plus quitte le broker (D100) ---"
# Le transport emails est retiré de la déclaration alors que sa file porte trois
# messages : la file d'échec, vide, et les bindings partent ; la file pleine
# reste, et l'apply le dit (code 6). Une file que l'application a créée
# elle-même n'a jamais été déclarée par ogs : elle reste quoi qu'il arrive.
curl -sf -u "$auth" -X PUT "$api/queues/shopfr/applicative" -H 'content-type: application/json' \
    -d '{"durable":true}' || { echo "ÉCHEC : file applicative non créée"; exit 1; }
python3 /scenarios/amqp-client.py remplir-emails
for _ in $(seq 1 20); do
    [ "$(compte)" = "3" ] && break
    sleep 1
done
[ "$(compte)" = "3" ] || { echo "ÉCHEC : les 3 messages ne sont pas comptés dans emails"; exit 1; }
# « amqp messenger » complète la déclaration sans rien en retirer (D121) : il
# remplaçait toute la déclaration, et c'est par lui que ce scénario retirait
# emails. Le retrait se fait désormais à la main, comme le ferait l'exploitant
# (l'image n'a pas python3-yaml).
ogs amqp messenger shopfr --transport async --failed >/dev/null
grep -q 'name: emails$' /etc/ogs/projects/shopfr.yaml ||
    { echo "ÉCHEC : « amqp messenger » a retiré le transport emails de la déclaration"; exit 1; }
awk '
    /^  amqp:/ { dans = 1 }
    dans && /^  [a-z]/ && !/^  amqp:/ { dans = 0 }
    function vider() { if (bloc != "" && bloc !~ /(name|queue): emails/) printf "%s", bloc; bloc = "" }
    dans && /^      - / { vider(); bloc = $0 "\n"; next }
    dans && bloc != "" && /^        / { bloc = bloc $0 "\n"; next }
    { vider(); print }
    END { vider() }
' /etc/ogs/projects/shopfr.yaml >/tmp/shopfr.yaml && cat /tmp/shopfr.yaml >/etc/ogs/projects/shopfr.yaml
if grep -q 'emails' <(sed -n '/^  amqp:/,/^  [a-z][a-z]*:$/p' /etc/ogs/projects/shopfr.yaml | sed '1d;$d'); then
    echo "ÉCHEC : le transport emails n'a pas quitté la déclaration"
    exit 1
fi
sortie=$(ogs apply shopfr --yes --recreate-queues 2>&1)
code=$?
echo "$sortie" | grep -Ei "emails|retiré" | head -6
if [ "$code" != 6 ] || ! echo "$sortie" | grep -q "emails.*3 message(s)"; then
    echo "ÉCHEC : la file pleine devait être conservée et nommée, en code 6 (code $code)"
    echo "$sortie" | tail -8
    exit 1
fi
curl -sf -u "$auth" "$api/queues/shopfr/emails" >/dev/null ||
    { echo "ÉCHEC : la file emails, pleine, est partie"; exit 1; }
if curl -sf -u "$auth" "$api/queues/shopfr/emails_failed" >/dev/null 2>&1; then
    echo "ÉCHEC : la file emails_failed, vide et retirée de la déclaration, subsiste"
    exit 1
fi
if curl -sf -u "$auth" "$api/bindings/shopfr/e/messages/q/emails" | grep -q '"routing_key":"emails"'; then
    echo "ÉCHEC : le binding messages → emails, retiré de la déclaration, subsiste"
    exit 1
fi
ogs amqp queue purge shopfr emails --yes >/dev/null 2>&1
for _ in $(seq 1 20); do
    [ "$(compte)" = "0" ] && break
    sleep 1
done
sortie=$(ogs apply shopfr --yes 2>&1)
code=$?
if [ "$code" != 0 ] || curl -sf -u "$auth" "$api/queues/shopfr/emails" >/dev/null 2>&1; then
    echo "ÉCHEC : vidée, la file emails devait partir au passage suivant (code $code)"
    echo "$sortie" | tail -6
    exit 1
fi
curl -sf -u "$auth" "$api/queues/shopfr/applicative" >/dev/null ||
    { echo "ÉCHEC : la file créée par l'application est partie"; exit 1; }
echo "file d'échec et bindings retirés, file pleine gardée puis retirée une fois vide, file applicative intacte"

echo
echo "--- vhost et compte remplacés dans la déclaration (D100) ---"
sed -i -e 's/^    vhost: shopfr$/    vhost: shopfr2/' -e 's/^    user: shopfr$/    user: shopfr2/' \
    /etc/ogs/projects/shopfr.yaml
grep -q "^    vhost: shopfr2$" /etc/ogs/projects/shopfr.yaml ||
    { echo "ÉCHEC : l'édition de la déclaration n'a pas mordu"; exit 1; }
sortie=$(ogs apply shopfr --yes 2>&1)
code=$?
echo "$sortie" | grep -Ei "vhost amqp|compte amqp|utilisateur amqp" | head -4
if [ "$code" != 0 ]; then
    echo "ÉCHEC : le remplacement devait aboutir (code $code)"
    echo "$sortie" | tail -8
    exit 1
fi
# L'apply qui crée les remplaçants garde les anciens : une saga déroulée plus
# loin retirerait les nouveaux, et le projet resterait sans aucun (D100).
if ! curl -sf -u "$auth" "$api/vhosts/shopfr" >/dev/null 2>&1 ||
    ! curl -sf -u "$auth" "$api/users/shopfr" >/dev/null 2>&1; then
    echo "ÉCHEC : l'ancien vhost ou l'ancien compte est parti dans l'apply qui crée le remplaçant"
    exit 1
fi
sortie=$(ogs apply shopfr --yes 2>&1)
code=$?
echo "$sortie" | grep -Ei "vhost amqp|compte amqp" | head -4
if [ "$code" != 0 ]; then
    echo "ÉCHEC : le retrait des anciens devait aboutir (code $code)"
    echo "$sortie" | tail -8
    exit 1
fi
if curl -sf -u "$auth" "$api/vhosts/shopfr" >/dev/null 2>&1 ||
    curl -sf -u "$auth" "$api/users/shopfr" >/dev/null 2>&1; then
    echo "ÉCHEC : l'ancien vhost ou l'ancien compte d'ogs subsiste"
    exit 1
fi
curl -sf -u "$auth" "$api/vhosts/shopfr2" >/dev/null ||
    { echo "ÉCHEC : le nouveau vhost n'a pas été créé"; exit 1; }
if ogs secrets list shopfr 2>/dev/null | grep -qE "amqp(-vhost|-declared)?/shopfr\b"; then
    echo "ÉCHEC : les preuves de l'ancien vhost ou de l'ancien compte restent au keystore"
    ogs secrets list shopfr
    exit 1
fi
echo "ancien vhost et ancien compte retirés avec leurs preuves"

echo
echo "--- suppression : le vhost et l'utilisateur partent avec le projet ---"
ogs project delete shopfr --yes >/dev/null 2>&1
if curl -sf -u "$auth" "$api/vhosts/shopfr2" >/dev/null 2>&1; then
    echo "ÉCHEC : le vhost subsiste"
    exit 1
fi
if curl -sf -u "$auth" "$api/users/shopfr2" >/dev/null 2>&1; then
    echo "ÉCHEC : l'utilisateur subsiste"
    exit 1
fi
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
echo "--- homonymes créés hors d'ogs : ni repris, ni supprimés (D93) ---"
# L'administrateur a déjà un vhost et un compte « atelierfr », avec un message
# en attente. ogs les reprenait sans connaître le mot de passe, puis les
# détruisait avec leurs files à la suppression du projet.
curl -sf -u "$auth" -X PUT "$api/vhosts/atelierfr" -H 'content-type: application/json' -d '{}' ||
    { echo "ÉCHEC : vhost de l'administrateur non créé"; exit 1; }
curl -sf -u "$auth" -X PUT "$api/users/atelierfr" -H 'content-type: application/json' \
    -d '{"password":"celui-de-l-administrateur","tags":""}' ||
    { echo "ÉCHEC : compte de l'administrateur non créé"; exit 1; }
curl -sf -u "$auth" -X PUT "$api/queues/atelierfr/commandes" -H 'content-type: application/json' \
    -d '{"durable":true}' || { echo "ÉCHEC : file de l'administrateur non créée"; exit 1; }
empreinte() { curl -sf -u "$auth" "$api/users/atelierfr" | grep -o '"password_hash":"[^"]*"'; }
avant=$(empreinte)
[ -n "$avant" ] || { echo "ÉCHEC : empreinte du compte de l'administrateur illisible"; exit 1; }
sortie=$(ogs site create atelier.fr --kind php-symfony --app-version 3.4 --php "$version" \
    --tls=false --messenger async --yes 2>&1)
code=$?
echo "$sortie" | grep -i "vhost amqp" | head -2
if [ "$code" != 6 ] || ! echo "$sortie" | grep -q "sans avoir été créé"; then
    echo "ÉCHEC : le vhost homonyme devait être refusé en code 6 (code $code)"
    echo "$sortie" | tail -6
    exit 1
fi
if curl -sf -u "$auth" "$api/queues/atelierfr/async" >/dev/null 2>&1; then
    echo "ÉCHEC : ogs a déclaré une file dans le vhost de l'administrateur"
    exit 1
fi
if [ "$(empreinte)" != "$avant" ]; then
    echo "ÉCHEC : le mot de passe du compte de l'administrateur a changé"
    exit 1
fi
ogs project delete atelierfr --yes >/dev/null 2>&1
if ! curl -sf -u "$auth" "$api/queues/atelierfr/commandes" >/dev/null 2>&1; then
    echo "ÉCHEC : le vhost de l'administrateur est parti avec le projet"
    exit 1
fi
if ! curl -sf -u "$auth" "$api/users/atelierfr" >/dev/null 2>&1; then
    echo "ÉCHEC : le compte de l'administrateur est parti avec le projet"
    exit 1
fi
echo "vhost, file et compte de l'administrateur intacts"

echo
echo "--- port de management local tenu par un autre compte (D93) ---"
# Broker arrêté, tout compte local peut ouvrir 127.0.0.1:15672 : ogs n'y envoie
# ses identifiants d'administration que si le port est tenu par root ou par le
# compte du broker. Le vrai broker vit dans un autre conteneur et n'écoute pas
# sur la boucle locale de celui-ci : un écouteur factice y tient sa place.
cat > /tmp/ecoute.py <<'PY'
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(sys.argv[1], "a") as f:
            f.write("%s %s\n" % (self.path, self.headers.get("Authorization", "-")))
        self.send_response(404)
        self.end_headers()
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", 15672), H).serve_forever()
PY
journal=/tmp/ecoute.journal
: > "$journal" && chmod 666 "$journal"
ecouter() {  # ecouter <compte>
    su -s /bin/sh "$1" -c "python3 /tmp/ecoute.py $journal" 2>/dev/null &
    ecouteur=$!
    for _ in $(seq 1 20); do
        ss -tln | grep -q '127.0.0.1:15672 ' && return 0
        sleep 0.2
    done
    echo "ÉCHEC : l'écouteur factice n'a pas démarré"; exit 1
}
arreter() { pkill -f /tmp/ecoute.py; wait "$ecouteur" 2>/dev/null; }
printf '%s\n' motdepasse-essai | ogs amqp credentials set --user admin \
    --url http://127.0.0.1:15672 --amqp-port 5672 >/dev/null
ogs project create ecoute.fr --kind php-symfony --app-version 6.4 --php "$version" >/dev/null
ogs amqp messenger ecoutefr --transport async >/dev/null
ecouter nobody
sortie=$(ogs amqp status ecoutefr 2>&1)
echo "$sortie" | grep -o "tenu par l'uid [0-9]*, ni root ni le compte rabbitmq" | head -1
if grep -q "Basic" "$journal"; then
    echo "ÉCHEC : les identifiants sont partis vers un port tenu par nobody"; exit 1
fi
echo "$sortie" | grep -q "tenu par l'uid" || { echo "ÉCHEC : refus non expliqué :"; echo "$sortie"; exit 1; }
arreter
# Le même port tenu par root reçoit la requête : la garde ne bloque pas un
# broker légitime.
ecouter root
ogs amqp status ecoutefr >/dev/null 2>&1
arreter
grep -q "Basic" "$journal" || { echo "ÉCHEC : un port tenu par root doit recevoir la requête"; exit 1; }
echo "identifiants refusés au port tenu par nobody, envoyés au port tenu par root"
printf '%s\n' motdepasse-essai | ogs amqp credentials set --user admin \
    --url http://rabbitmq:15672 --amqp-port 5672 >/dev/null
ogs project delete ecoutefr --yes >/dev/null 2>&1

echo
echo "RÉSULTAT : le provider RabbitMQ fonctionne sur un vrai broker — isolation"
echo "           par vhost, bindings effectifs, arguments immuables respectés."
