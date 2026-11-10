#!/bin/bash
# Éprouve l'import d'un certificat émis ailleurs (D130) sur un vrai serveur web :
# le site d'une migration est créé avec le certificat de l'ancien serveur, servi
# en HTTPS dès le premier apply sans aucune émission, remplacé à chaud sous
# --force, gardé quand le renouvellement échoue faute de DNS, et servi par nginx
# comme par Apache. Les certificats sont signés par une autorité d'essai que le
# système reconnaît, comme Let's Encrypt l'est d'une vraie machine.
#
# L'autorité ACME désigne un port fermé : toute émission échoue aussitôt, comme
# celle d'une cible dont le DNS désigne encore l'ancien serveur. Le scénario
# vérifie qu'aucune n'est tentée tant que le certificat importé suffit, et que
# celle qu'impose une échéance proche ne retire rien.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : import d'un certificat ==="

apache2ctl start
sleep 1
ogs init --quiet
choisir_moteur apache
python3 - <<'PY'
chemin = "/etc/ogs/config.yaml"
s = open(chemin).read()
s = s.replace('acme:\n  email: ""', 'acme:\n  email: admin@import.test\n  directory: https://127.0.0.1:9/dir')
open(chemin, "w").write(s)
PY
grep -A 2 "^acme:" /etc/ogs/config.yaml

# --- autorité d'essai et paires -------------------------------------------------
pki=/root/pki
mkdir -p "$pki"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 365 \
    -keyout "$pki/ca.key" -out "$pki/ca.pem" -subj "/CN=Autorite essai ogs R1" \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
cp "$pki/ca.pem" /usr/local/share/ca-certificates/ogs-essai.crt
update-ca-certificates >/dev/null 2>&1

# paire <répertoire> <jours> <nom>... : chaîne complète et clé, à la manière du
# live/ de certbot.
paire() {
    local dir=$1 jours=$2
    shift 2
    mkdir -p "$dir"
    local san
    san=$(printf 'DNS:%s,' "$@")
    openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$dir/privkey.pem" \
        -out "$dir/req.csr" -subj "/CN=$1" 2>/dev/null
    openssl x509 -req -in "$dir/req.csr" -CA "$pki/ca.pem" -CAkey "$pki/ca.key" \
        -set_serial "0x$(openssl rand -hex 8)" -days "$jours" -out "$dir/cert.pem" \
        -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n' "${san%,}") \
        2>/dev/null
    cat "$dir/cert.pem" "$pki/ca.pem" >"$dir/fullchain.pem"
}
serie() { openssl x509 -in "$1/cert.pem" -noout -serial; }

# servi <nom> : numéro de série du certificat que le serveur présente pour ce nom.
servi() {
    openssl s_client -connect 127.0.0.1:443 -servername "$1" </dev/null 2>/dev/null |
        openssl x509 -noout -serial 2>/dev/null
}

paire "$pki/un" 80 import.test www.import.test
paire "$pki/deux" 80 import.test www.import.test
paire "$pki/proche" 10 import.test www.import.test
paire "$pki/ailleurs" 80 autre.test
paire "$pki/intrus" 80 import.test

echo
echo "--- une paire dépareillée ne laisse ni déclaration ni certificat ---"
sortie=$(ogs site create import.test --kind static --alias www.import.test \
    --cert "$pki/un/fullchain.pem" --key "$pki/intrus/privkey.pem" --yes 2>&1)
code=$?
echo "$sortie" | tail -1
if [ "$code" != 3 ] || [ -e /etc/ogs/projects/importtest.yaml ] || [ -e /var/lib/ogs/acme/certificates/importtest ]; then
    echo "ÉCHEC : attendu code 3 sans rien d'écrit (code $code)"
    exit 1
fi

echo
echo "--- site créé avec le certificat de l'ancien serveur ---"
sortie=$(ogs site create import.test --kind static --alias www.import.test \
    --cert "$pki/un/fullchain.pem" --key "$pki/un/privkey.pem" --yes 2>&1)
code=$?
echo "$sortie" | grep -Ei "certificat|⚠|✗" | head -6
if [ "$code" != 0 ]; then
    echo "ÉCHEC : site create avec import (code $code)"
    echo "$sortie" | tail -20
    exit 1
fi
if echo "$sortie" | grep -qi "émission du certificat\|127.0.0.1:9"; then
    echo "ÉCHEC : une émission a été tentée alors que le certificat importé couvre le site"
    exit 1
fi
if ! cmp -s "$pki/un/fullchain.pem" /var/lib/ogs/acme/certificates/importtest/fullchain.pem; then
    echo "ÉCHEC : le magasin ne porte pas la chaîne importée"
    exit 1
fi
stat -c '%a %U %n' /var/lib/ogs/acme/certificates/importtest/privkey.pem
if [ "$(stat -c '%a' /var/lib/ogs/acme/certificates/importtest/privkey.pem)" != 600 ]; then
    echo "ÉCHEC : la clé importée doit être en 0600"
    exit 1
fi

echo "site migré" >/home/import.test/www/index.html
chown importtest:importtest /home/import.test/www/index.html
gpasswd -a www-data importtest >/dev/null
apache2ctl graceful
sleep 2

# verifier_https <série attendue> <étape> : les deux noms servis, chaîne vérifiée
# contre l'autorité du système, par le certificat attendu.
verifier_https() {
    local attendu=$1 etape=$2 nom reponse
    for nom in import.test www.import.test; do
        reponse=$(curl -s --resolve "$nom:443:127.0.0.1" "https://$nom/index.html")
        if [ "$reponse" != "site migré" ]; then
            echo "ÉCHEC ($etape) : $nom n'est pas servi en HTTPS vérifié — reçu « $reponse »"
            curl -sv --resolve "$nom:443:127.0.0.1" "https://$nom/index.html" 2>&1 | grep -E "^\*" | tail -6
            exit 1
        fi
        if [ "$(servi "$nom")" != "$attendu" ]; then
            echo "ÉCHEC ($etape) : $nom présente $(servi "$nom"), attendu $attendu"
            exit 1
        fi
    done
    echo "$etape : import.test et www.import.test servis par $attendu"
}
verifier_https "$(serie "$pki/un")" "premier apply"

# Le certificat importé suffit : le plan n'a plus rien à faire, émission comprise.
code=0
ogs diff importtest >/tmp/diff.out 2>&1 || code=$?
if [ "$code" != 0 ]; then
    echo "ÉCHEC : « ogs diff » doit être conforme après le premier apply (code $code)"
    cat /tmp/diff.out
    exit 1
fi
echo "ogs diff importtest : conforme"

echo
echo "--- remplacement à chaud sous --force ---"
sortie=$(ogs cert import importtest --cert "$pki/deux/fullchain.pem" --key "$pki/deux/privkey.pem" 2>&1)
code=$?
echo "$sortie" | tail -1
if [ "$code" != 4 ]; then
    echo "ÉCHEC : sans --force, un certificat en place doit être refusé en code 4 (code $code)"
    exit 1
fi
sortie=$(ogs cert import importtest --cert "$pki/deux/fullchain.pem" --key "$pki/deux/privkey.pem" --force 2>&1)
code=$?
echo "$sortie" | grep -E "sauvegarde|remplacé|rechargement"
if [ "$code" != 0 ] || ! echo "$sortie" | grep -q rechargement; then
    echo "ÉCHEC : le remplacement d'un certificat servi doit recharger le serveur (code $code)"
    echo "$sortie"
    exit 1
fi
sleep 2
# Sans apply : le rechargement de l'import suffit.
verifier_https "$(serie "$pki/deux")" "après remplacement"
if [ "$(find /var/lib/ogs/backups -name 'importtest-*' | wc -l)" != 2 ]; then
    echo "ÉCHEC : chaîne et clé remplacées attendues dans /var/lib/ogs/backups"
    ls -l /var/lib/ogs/backups
    exit 1
fi

echo
echo "--- un import refusé laisse le certificat servi ---"
sortie=$(ogs cert import importtest --cert "$pki/ailleurs/fullchain.pem" --key "$pki/ailleurs/privkey.pem" --force 2>&1)
code=$?
echo "$sortie" | tail -1
if [ "$code" != 3 ]; then
    echo "ÉCHEC : un certificat qui ne couvre aucun nom doit être refusé en code 3 (code $code)"
    exit 1
fi
verifier_https "$(serie "$pki/deux")" "après refus"

echo
echo "--- nginx sert le certificat importé ---"
apache2ctl stop
sleep 1
choisir_moteur nginx
nginx
sleep 1
code=0
ogs apply importtest --yes >/tmp/apply-nginx.out 2>&1 || code=$?
if [ "$code" != 0 ] && [ "$code" != 6 ]; then
    echo "ÉCHEC : apply sous nginx (code $code)"
    cat /tmp/apply-nginx.out
    exit 1
fi
nginx -t 2>&1 | tail -1
nginx -s reload
sleep 1
verifier_https "$(serie "$pki/deux")" "sous nginx"

echo
echo "--- échéance proche : le renouvellement échoue, le certificat importé reste servi ---"
sortie=$(ogs cert import importtest --cert "$pki/proche/fullchain.pem" --key "$pki/proche/privkey.pem" --force 2>&1)
code=$?
echo "$sortie" | grep -E "⚠" | cut -c1-140
if [ "$code" != 6 ] || ! echo "$sortie" | grep -q "expire dans"; then
    echo "ÉCHEC : une échéance proche est un avertissement, code 6 (code $code)"
    exit 1
fi
sleep 1
verifier_https "$(serie "$pki/proche")" "import à échéance proche"
code=0
ogs apply importtest --yes >/tmp/apply-proche.out 2>&1 || code=$?
grep -Ei "renouvellement|certificat importtest" /tmp/apply-proche.out | head -3 | cut -c1-160
if [ "$code" != 6 ]; then
    echo "ÉCHEC : le renouvellement impossible doit rendre 6 (code $code)"
    cat /tmp/apply-proche.out
    exit 1
fi
if ! cmp -s "$pki/proche/fullchain.pem" /var/lib/ogs/acme/certificates/importtest/fullchain.pem; then
    echo "ÉCHEC : un renouvellement en échec ne doit pas toucher au certificat importé"
    exit 1
fi
if ! pgrep -x nginx >/dev/null; then
    echo "ÉCHEC : nginx est mort"
    exit 1
fi
verifier_https "$(serie "$pki/proche")" "après le renouvellement en échec"

echo
echo "✓ scénario import-certificat réussi"
