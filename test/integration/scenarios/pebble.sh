#!/bin/bash
# Éprouve l'émission de certificats contre une vraie autorité ACME (pebble) :
# challenge HTTP-01 servi par le vhost réel, certificat écrit, vhost basculé en
# HTTPS, renouvellement. Ce que les tests unitaires ne peuvent pas faire, faute
# d'interlocuteur.
set -uo pipefail

echo "=== scénario : ACME avec pebble ==="

# lego doit accepter le certificat de l'API de pebble, signé par son autorité
# d'essai. Sans cela, le dialogue échoue sur la vérification TLS.
export LEGO_CA_CERTIFICATES=/certs/minica.pem

apache2ctl start
sleep 1
ogs init --quiet

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
echo "--- le site reste servi après renouvellement ---"
systemctl reload apache2
sleep 1
if [ "$(curl -s --cacert /tmp/pebble-root.pem https://acme.test/index.html)" != "bonjour en TLS" ]; then
    echo "ÉCHEC : le site n'est plus servi après renouvellement"
    exit 1
fi
echo "servi avec le nouveau certificat"

echo
echo "--- idempotence ---"
ogs diff acmetest 2>&1 | tail -3

echo
echo "RÉSULTAT : ogs dialogue avec une vraie autorité ACME — challenge HTTP-01"
echo "           servi par le vhost, certificat installé, HTTPS effectif,"
echo "           renouvellement maîtrisé."
