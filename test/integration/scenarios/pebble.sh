#!/bin/bash
# Éprouve l'émission de certificats contre une vraie autorité ACME (pebble) :
# challenge HTTP-01 servi par le vhost réel, certificat écrit, vhost basculé en
# HTTPS, renouvellement. Ce que les tests unitaires ne peuvent pas faire, faute
# d'interlocuteur.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : ACME avec pebble ==="

# lego doit accepter le certificat de l'API de pebble, signé par son autorité
# d'essai. Sans cela, le dialogue échoue sur la vérification TLS.
export LEGO_CA_CERTIFICATES=/certs/minica.pem

apache2ctl start
sleep 1
ogs init --quiet
choisir_moteur apache

# L'autorité visée et le contact : l'API en exige un pour notifier les expirations.
python3 - <<'PY'
chemin = "/etc/ogs/config.yaml"
s = open(chemin).read()
s = s.replace('acme:\n  email: ""', 'acme:\n  email: admin@acme.test\n  directory: https://pebble:14000/dir')
open(chemin, "w").write(s)
PY
tail -3 /etc/ogs/config.yaml

echo
echo "--- création d'un site avec TLS (challenge HTTP-01 réel) ---"
# Le domaine résout vers ce conteneur : c'est ici que pebble viendra chercher le
# jeton.
ogs site create acme.test --kind static --yes 2>&1 | grep -Ev "^\s*$" | tail -14

cert=/var/lib/ogs/acme/certificates/acmetest
if [ ! -f "$cert/fullchain.pem" ]; then
    echo "ÉCHEC : aucun certificat écrit"
    ls -R /var/lib/ogs/acme 2>/dev/null | head -20
    exit 1
fi

echo
echo "--- le certificat émis ---"
openssl x509 -in "$cert/fullchain.pem" -noout -subject -issuer -dates
if ! openssl x509 -in "$cert/fullchain.pem" -noout -text | grep -q "DNS:acme.test"; then
    echo "ÉCHEC : le certificat ne couvre pas acme.test"
    exit 1
fi
if ! openssl x509 -in "$cert/fullchain.pem" -noout -issuer | grep -qi pebble; then
    echo "ÉCHEC : le certificat ne vient pas de l'autorité visée"
    exit 1
fi
if [ "$(stat -c %a "$cert/privkey.pem")" != "600" ]; then
    echo "ÉCHEC : la clé privée est lisible au-delà de root ($(stat -c %a "$cert/privkey.pem"))"
    exit 1
fi
echo "clé privée en 0600, chaîne complète présente"

echo
echo "--- le site est-il servi en HTTPS ? ---"
echo "bonjour en TLS" > /home/acme.test/www/index.html
chown acmetest:acmetest /home/acme.test/www/index.html
gpasswd -a www-data acmetest >/dev/null
apache2ctl configtest 2>&1 | tail -1
systemctl restart apache2
sleep 2
# La racine de confiance de pebble pour les certificats qu'il délivre.
curl -sk https://pebble:15000/roots/0 -o /tmp/pebble-root.pem
reponse=$(curl -s --cacert /tmp/pebble-root.pem https://acme.test/index.html)
echo "réponse HTTPS : $reponse"
if [ "$reponse" != "bonjour en TLS" ]; then
    echo "ÉCHEC : le site n'est pas servi en HTTPS avec le certificat émis"
    curl -sv --cacert /tmp/pebble-root.pem https://acme.test/index.html 2>&1 | tail -12
    exit 1
fi
echo "le certificat est bien celui présenté par Apache, et il est validé par la chaîne"

echo
echo "--- inventaire ---"
ogs cert list 2>&1 | head -5

echo
echo "--- un renouvellement non forcé ne doit rien consommer ---"
# Le certificat vient d'être émis : le renouvellement ne doit pas y toucher, ni
# a fortiori en créer un nouveau.
serie_avant=$(openssl x509 -in "$cert/fullchain.pem" -noout -serial)
ogs cert renew 2>&1 | tail -2
if [ "$(openssl x509 -in "$cert/fullchain.pem" -noout -serial)" != "$serie_avant" ]; then
    echo "ÉCHEC : le certificat a été renouvelé sans nécessité"
    exit 1
fi
echo "certificat inchangé ($serie_avant)"

# serie_servie rend le numéro de série du certificat que présente le port 443.
serie_servie() {
    openssl s_client -connect 127.0.0.1:443 -servername "$1" </dev/null 2>/dev/null |
        openssl x509 -noout -serial 2>/dev/null
}

echo
echo "--- renouvellement forcé ---"
ogs cert renew --force 2>&1 | tail -3
serie_apres=$(openssl x509 -in "$cert/fullchain.pem" -noout -serial)
if [ "$serie_apres" = "$serie_avant" ]; then
    echo "ÉCHEC : --force n'a pas réémis le certificat"
    exit 1
fi
echo "nouveau certificat : $serie_apres"
if ! openssl x509 -in "$cert/fullchain.pem" -noout -text | grep -q "DNS:acme.test"; then
    echo "ÉCHEC : le certificat renouvelé ne couvre plus le domaine"
    exit 1
fi

echo
echo "--- le site reste servi après renouvellement, sans rechargement manuel ---"
# --force tente aussi le wildcard de préproduction, qui échoue faute
# d'identifiants DNS : le renouvellement est partiel (code 6), et « cert renew »
# sortait alors avant de recharger Apache, qui présentait l'ancien certificat.
sleep 1
if [ "$(serie_servie acme.test)" != "$serie_apres" ]; then
    echo "ÉCHEC : Apache présente toujours l'ancien certificat ($(serie_servie acme.test))"
    exit 1
fi
if [ "$(curl -s --cacert /tmp/pebble-root.pem https://acme.test/index.html)" != "bonjour en TLS" ]; then
    echo "ÉCHEC : le site n'est plus servi après renouvellement"
    exit 1
fi
echo "servi avec le nouveau certificat"

# ajouter_alias remplace les alias déclarés du projet par celui donné.
ajouter_alias() {
    python3 - "$1" <<'PY'
import re, sys
chemin = "/etc/ogs/projects/acmetest.yaml"
s = open(chemin).read()
s = re.sub(r"    - name: [a-z.]+\n      role: alias\n", "", s)
s = s.replace("    - name: acme.test\n      role: primary\n",
              "    - name: acme.test\n      role: primary\n    - name: %s\n      role: alias\n" % sys.argv[1])
open(chemin, "w").write(s)
PY
}

echo
echo "--- un alias dont l'émission échoue ne retire pas le HTTPS du site ---"
# absent.test ne résout nulle part : pebble refuse son challenge. Le vhost était
# réécrit en HTTP seul avant l'émission, et le restait : HSTS rendait alors le
# site injoignable. Il doit garder son bloc 443 sur le certificat en place.
ajouter_alias absent.test
ogs apply acmetest --yes 2>&1 | grep -E "vhost|certificat|rechargement" | head -8
if ! grep -q "SSLEngine on" /etc/apache2/sites-available/acmetest.conf; then
    echo "ÉCHEC : l'ajout d'un alias a retiré le bloc 443 du vhost"
    exit 1
fi
if [ "$(curl -s --cacert /tmp/pebble-root.pem https://acme.test/index.html)" != "bonjour en TLS" ]; then
    echo "ÉCHEC : le site n'est plus servi en HTTPS après l'échec de l'émission de l'alias"
    exit 1
fi
echo "l'émission pour absent.test a échoué, acme.test reste servi en HTTPS"

echo
echo "--- un alias qui résout, à côté d'une redirection refusée : couvert sans attendre ---"
# www.acme.test résout vers ce conteneur ; absent.test, ajouté en redirection, ne
# résout nulle part : pebble refuse son challenge, et toute la commande avec lui.
# Le repli couvre quand même l'alias, qui ajoute un nom sans en retirer aucun —
# il attendait sinon que la redirection soit réparée. Le vhost, qui déclare déjà
# l'alias, ne change pas : c'est l'étape d'émission qui doit recharger Apache.
ajouter_alias www.acme.test
python3 - <<'PY'
chemin = "/etc/ogs/projects/acmetest.yaml"
s = open(chemin).read()
s = s.replace("    - name: acme.test\n      role: primary\n",
              "    - name: acme.test\n      role: primary\n"
              "    - name: absent.test\n      role: redirect\n      target: acme.test\n")
open(chemin, "w").write(s)
PY
ogs apply acmetest --yes 2>&1 | grep -E "vhost|certificat|rechargement" | head -10
if ! openssl x509 -in "$cert/fullchain.pem" -noout -text | grep -q "DNS:www.acme.test"; then
    echo "ÉCHEC : la redirection refusée a empêché de couvrir www.acme.test"
    exit 1
fi
if openssl x509 -in "$cert/fullchain.pem" -noout -text | grep -q "DNS:absent.test"; then
    echo "ÉCHEC : le certificat couvre absent.test, que pebble devait refuser"
    exit 1
fi
sleep 1
if [ "$(curl -s --cacert /tmp/pebble-root.pem https://www.acme.test/index.html)" != "bonjour en TLS" ]; then
    echo "ÉCHEC : www.acme.test n'est pas présenté avec le certificat réémis"
    curl -sv --cacert /tmp/pebble-root.pem https://www.acme.test/index.html 2>&1 | tail -8
    exit 1
fi
if grep -q "SSLEngine on" /etc/apache2/sites-available/acmetest_redirect.conf; then
    echo "ÉCHEC : le vhost de absent.test présente un certificat qui ne le couvre pas"
    exit 1
fi
echo "www.acme.test servi en HTTPS avec le certificat réémis, absent.test écarté"

echo
echo "--- une redirection refusée ne coûte pas le renouvellement du site ---"
# Une redirection est couverte par le certificat du projet (D43). Le
# renouvellement du site ne doit pas dépendre de absent.test : il se replie sur
# les noms du vhost principal, sans quoi le site tomberait à l'échéance.
serie_avant=$(openssl x509 -in "$cert/fullchain.pem" -noout -serial)
ogs cert renew acmetest --force 2>&1 | tail -4
code=${PIPESTATUS[0]}
serie_apres=$(openssl x509 -in "$cert/fullchain.pem" -noout -serial)
if [ "$serie_apres" = "$serie_avant" ]; then
    echo "ÉCHEC : la redirection refusée a empêché le renouvellement du site"
    exit 1
fi
for nom in acme.test www.acme.test; do
    if ! openssl x509 -in "$cert/fullchain.pem" -noout -text | grep -q "DNS:$nom\b"; then
        echo "ÉCHEC : le certificat renouvelé ne couvre plus $nom"
        exit 1
    fi
done
sleep 1
if [ "$(serie_servie acme.test)" != "$serie_apres" ]; then
    echo "ÉCHEC : le certificat renouvelé n'est pas présenté"
    exit 1
fi
echo "site renouvelé et présenté malgré la redirection refusée (code $code)"

echo
echo "--- une redirection depuis une adresse IP ne bloque ni l'apply ni le renouvellement ---"
# Aucun certificat ACME ne peut porter une adresse IP, que la validation des
# domaines admet : mise dans la demande, elle la faisait refuser entière — plus
# d'apply (code 3), et « cert renew --all » passait le projet en silence, son
# certificat allant à l'expiration. absent.test part du même coup : le parc
# revient à l'état conforme.
python3 - <<'PY'
chemin = "/etc/ogs/projects/acmetest.yaml"
s = open(chemin).read()
s = s.replace("    - name: absent.test\n      role: redirect\n      target: acme.test\n",
              "    - name: 203.0.113.10\n      role: redirect\n      target: acme.test\n")
open(chemin, "w").write(s)
PY
ogs apply acmetest --yes 2>&1 | tail -2
code=${PIPESTATUS[0]}
if [ "$code" != 0 ]; then
    echo "ÉCHEC : une redirection depuis une adresse IP fait rendre le code $code à l'apply"
    exit 1
fi
serie_avant=$(openssl x509 -in "$cert/fullchain.pem" -noout -serial)
ogs cert renew acmetest --force 2>&1 | tail -3
code=${PIPESTATUS[0]}
if [ "$code" != 0 ] || [ "$(openssl x509 -in "$cert/fullchain.pem" -noout -serial)" = "$serie_avant" ]; then
    echo "ÉCHEC : le certificat du site n'est plus renouvelé (code $code)"
    exit 1
fi
if grep -q "SSLEngine on" /etc/apache2/sites-available/acmetest_redirect.conf; then
    echo "ÉCHEC : le vhost de 203.0.113.10 présente un certificat qui ne peut pas le couvrir"
    exit 1
fi
echo "redirection depuis 203.0.113.10 servie en HTTP, site renouvelé"

echo
echo "--- une déclaration illisible ne coûte pas le renouvellement du parc (D120) ---"
# Un seul fichier de projet refusé au chargement arrêtait « cert renew --all »
# en code 3, le timer compris, et plus aucun certificat ne se renouvelait.
printf 'metadata: [\n' >/etc/ogs/projects/casse.yaml
serie_avant=$(openssl x509 -in "$cert/fullchain.pem" -noout -serial)
ogs cert renew --all --force >/tmp/renew.log 2>&1
code=$?
tail -3 /tmp/renew.log
if [ "$code" != 6 ] || ! grep -q "casse.yaml" /tmp/renew.log ||
    [ "$(openssl x509 -in "$cert/fullchain.pem" -noout -serial)" = "$serie_avant" ]; then
    cat /tmp/renew.log
    echo "ÉCHEC : la déclaration illisible arrête le renouvellement (code $code)"
    exit 1
fi
rm -f /etc/ogs/projects/casse.yaml
echo "casse.yaml nommée, acmetest renouvelé, code 6"

echo
echo "--- une base d'état disparue : refus en code 5, renouvellement maintenu (D120) ---"
mkdir -p /tmp/etat
mv /var/lib/ogs/state.db* /tmp/etat/
code=0
ogs plan acmetest >/tmp/plan.log 2>&1 || code=$?
if [ "$code" != 5 ] || [ -e /var/lib/ogs/state.db ]; then
    cat /tmp/plan.log
    echo "ÉCHEC : sans base d'état, plan rend $code ou la recrée"
    exit 1
fi
serie_avant=$(openssl x509 -in "$cert/fullchain.pem" -noout -serial)
ogs cert renew acmetest --force >/tmp/renew.log 2>&1
code=$?
tail -2 /tmp/renew.log
if [ "$code" != 6 ] || ! grep -q "state.db" /tmp/renew.log || [ -e /var/lib/ogs/state.db ] ||
    [ "$(openssl x509 -in "$cert/fullchain.pem" -noout -serial)" = "$serie_avant" ]; then
    cat /tmp/renew.log
    echo "ÉCHEC : sans base d'état, le renouvellement rend $code, ou recrée la base, ou ne renouvelle rien"
    exit 1
fi
mv /tmp/etat/state.db* /var/lib/ogs/
echo "plan refusé (code 5), certificat renouvelé sans recréer la base (code 6)"

echo
echo "--- idempotence ---"
ogs diff acmetest 2>&1 | tail -3

echo
echo "RÉSULTAT : ogs dialogue avec une vraie autorité ACME — challenge HTTP-01"
echo "           servi par le vhost, certificat installé, HTTPS effectif,"
echo "           renouvellement maîtrisé."
