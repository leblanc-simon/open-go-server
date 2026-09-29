#!/bin/bash
# Éprouve la création complète d'un site PHP sur un système réel : utilisateur,
# arborescence, pool FPM, vhost, et vérifie que PHP est effectivement exécuté.
# Éprouve aussi le lien des clés autorisées vers le trousseau commun (D61) : le
# lchown et la préservation d'un authorized_keys posé à la main n'existent que sur
# un système réel, en mode simulé rien de tout cela n'atteint le disque.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : site PHP servi de bout en bout ==="
version=$(ls /etc/php | head -1)
echo "version PHP détectée : $version"

apache2ctl start
sleep 1

ogs init --quiet
choisir_moteur apache
ogs site create php.test --kind php-plain --php "$version" --tls=false --yes 2>&1 | tail -5

if [ ! -f "/etc/php/$version/fpm/pool.d/phptest.conf" ]; then
    echo "ÉCHEC : le pool n'a pas été écrit"
    exit 1
fi
echo "pool écrit :"
grep -E '^(user|group|listen) ' "/etc/php/$version/fpm/pool.d/phptest.conf" | sed 's/^/  /'

# La configuration produite doit être acceptée par les deux moteurs.
if ! "/usr/sbin/php-fpm$version" -t 2>&1 | tail -1; then
    echo "ÉCHEC : configuration FPM refusée"
    exit 1
fi
if ! apache2ctl configtest 2>&1 | tail -1; then
    echo "ÉCHEC : configuration Apache refusée"
    exit 1
fi

# Le shell du compte doit être celui déclaré (D62) : useradd le pose, et seul un
# système réel montre ce que /etc/passwd a réellement reçu.
shell_reel=$(getent passwd phptest | cut -d: -f7)
echo "shell du compte : $shell_reel"
if [ "$shell_reel" != /bin/bash ]; then
    echo "ÉCHEC : shell attendu /bin/bash, obtenu $shell_reel"
    exit 1
fi
# Et il est réconcilié : refermé à la main, l'apply le rouvre.
usermod --shell /usr/sbin/nologin phptest
ogs apply phptest --yes 2>&1 | grep -i "shell" | sed 's/^/  /'
shell_reel=$(getent passwd phptest | cut -d: -f7)
if [ "$shell_reel" != /bin/bash ]; then
    echo "ÉCHEC : le shell déclaré n'a pas été rétabli (obtenu $shell_reel)"
    exit 1
fi
echo "shell réconcilié après modification à la main"

# Trousseau de clés commun : posé avant la création, il doit être lié dans le .ssh
# du projet. Seul un système réel le voit — le mode préfixé n'a ni /opt ni root
# pour éprouver le lchown, et un lien appartenant à root reste indétectable en
# mode simulé.
echo 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOpErateur opérateur@parc' > /opt/default_authorized_keys
chown root:root /opt/default_authorized_keys
chmod 0644 /opt/default_authorized_keys
ogs site create keys.test --kind static --tls=false --yes 2>&1 | tail -3

lien=/home/keys.test/.ssh/authorized_keys
if [ ! -L "$lien" ]; then
    echo "ÉCHEC : $lien n'est pas un lien symbolique"
    ls -l /home/keys.test/.ssh/ 2>&1 | sed 's/^/  /'
    exit 1
fi
cible=$(readlink "$lien")
if [ "$cible" != /opt/default_authorized_keys ]; then
    echo "ÉCHEC : le lien pointe sur $cible"
    exit 1
fi
# lchown, non chown : le trousseau commun doit rester à root, faute de quoi sshd
# le refuserait sous StrictModes.
proprio_lien=$(stat -c '%U:%G' "$lien")
proprio_cible=$(stat -c '%U:%G %a' /opt/default_authorized_keys)
echo "clés autorisées : lien $proprio_lien → trousseau $proprio_cible"
if [ "$proprio_lien" != "keystest:keystest" ]; then
    echo "ÉCHEC : le lien devrait appartenir au projet, il appartient à $proprio_lien"
    exit 1
fi
if [ "$proprio_cible" != "root:root 644" ]; then
    echo "ÉCHEC : le trousseau commun a été altéré ($proprio_cible)"
    exit 1
fi
# Rejouer ne doit rien changer : le lien conforme sort du plan.
if ogs apply keystest --yes 2>&1 | grep -qi "authorized"; then
    echo "ÉCHEC : le lien déjà conforme reparaît dans le plan"
    exit 1
fi
# Un authorized_keys posé à la main porte l'accès de quelqu'un : il est préservé.
ogs site create garde.test --kind static --tls=false --yes >/dev/null 2>&1
rm -f /home/garde.test/.ssh/authorized_keys
echo 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIProPre propre@projet' \
    > /home/garde.test/.ssh/authorized_keys
ogs apply gardetest --yes >/dev/null 2>&1
code=$?
if [ ! -f /home/garde.test/.ssh/authorized_keys ] \
    || [ -L /home/garde.test/.ssh/authorized_keys ] \
    || ! grep -q "propre@projet" /home/garde.test/.ssh/authorized_keys; then
    echo "ÉCHEC : un authorized_keys posé à la main a été remplacé par le lien"
    exit 1
fi
echo "authorized_keys posé à la main préservé (apply rend $code)"

# Rotation de la clé de déploiement : hors de tout plan, elle ne peut compter que
# sur elle-même. Elle doit poser la paire au compte du projet — un chown vers un
# autre compte, que seul root réussit —, et refuser un .ssh remplacé par un lien,
# que root suivrait jusque dans /root.
ogs user key rotate keystest --yes >/dev/null 2>&1
proprio=$(stat -c '%U:%G %a' /home/keys.test/.ssh/id_ed25519)
if [ "$proprio" != "keystest:keystest 600" ]; then
    echo "ÉCHEC : clé privée renouvelée en $proprio, attendu keystest:keystest 600"
    exit 1
fi
mkdir -p /root/ssh-temoin
echo "clé de root" > /root/ssh-temoin/id_ed25519
mv /home/keys.test/.ssh /home/keys.test/.ssh.orig
ln -s /root/ssh-temoin /home/keys.test/.ssh
if ogs user key rotate keystest --yes >/dev/null 2>&1; then
    echo "ÉCHEC : la rotation a accepté un .ssh remplacé par un lien"
    exit 1
fi
if [ "$(cat /root/ssh-temoin/id_ed25519)" != "clé de root" ] || [ -e /root/ssh-temoin/id_ed25519.pub ]; then
    echo "ÉCHEC : root a écrit dans la cible du lien"
    ls -l /root/ssh-temoin | sed 's/^/  /'
    exit 1
fi
rm /home/keys.test/.ssh
mv /home/keys.test/.ssh.orig /home/keys.test/.ssh
rm -rf /root/ssh-temoin
echo "clé renouvelée au compte du projet, .ssh lié refusé"

# Un lien physique ne se voit pas comme un lien : une clé publique liée à un
# fichier de root n'est ni affichée par « user key show », ni reprise par la
# rotation. Seul root pose ce lien sous fs.protected_hardlinks, mais c'est un
# réglage de la machine, non une garantie — et seul un système réel donne à un
# fichier du .ssh un autre propriétaire que le projet.
cp -p /home/keys.test/.ssh/id_ed25519.pub /root/pub.orig
echo "secret de root" > /root/temoin-cle
chmod 600 /root/temoin-cle
rm /home/keys.test/.ssh/id_ed25519.pub
ln /root/temoin-cle /home/keys.test/.ssh/id_ed25519.pub
if sortie=$(ogs user key show keystest 2>&1) || [[ "$sortie" == *"secret de root"* ]]; then
    echo "ÉCHEC : « user key show » a lu une clé liée à un fichier de root"
    echo "$sortie" | sed 's/^/  /'
    exit 1
fi
if sortie=$(ogs user key rotate keystest --yes 2>&1) || [[ "$sortie" == *"secret de root"* ]]; then
    echo "ÉCHEC : la rotation a repris une clé liée à un fichier de root"
    echo "$sortie" | sed 's/^/  /'
    exit 1
fi
if [ "$(cat /root/temoin-cle)" != "secret de root" ]; then
    echo "ÉCHEC : le fichier de root a été réécrit"
    exit 1
fi
rm /home/keys.test/.ssh/id_ed25519.pub /root/temoin-cle
cp -p /root/pub.orig /home/keys.test/.ssh/id_ed25519.pub
rm /root/pub.orig
echo "clé liée physiquement à un fichier de root : ni lue, ni reprise"

# Une clé creuse au compte du projet passe tous les autres contrôles : elle est à
# lui, et ne lui coûte aucun bloc. Ni « show » ni la rotation ne la lisent — root
# la lisait en mémoire, et la rotation la réécrivait pleine dans les sauvegardes,
# sur /var. Le mode simulé ne voit ni blocs alloués ni sauvegardes réelles.
cp -p /home/keys.test/.ssh/id_ed25519 /root/cle.orig
cp -p /home/keys.test/.ssh/id_ed25519.pub /root/pub.orig
avant=$(du -sk /var/lib/ogs/backups 2>/dev/null | cut -f1)
truncate -s 1G /home/keys.test/.ssh/id_ed25519.pub
if sortie=$(ogs user key show keystest 2>&1) || [ "${#sortie}" -gt 4096 ]; then
    echo "ÉCHEC : « user key show » a rendu une clé publique creuse de 1 Gio (${#sortie} caractères)"
    exit 1
fi
cp -p /root/pub.orig /home/keys.test/.ssh/id_ed25519.pub
truncate -s 1G /home/keys.test/.ssh/id_ed25519
if ogs user key rotate keystest --yes >/dev/null 2>&1; then
    echo "ÉCHEC : la rotation a accepté une clé privée creuse de 1 Gio"
    exit 1
fi
apres=$(du -sk /var/lib/ogs/backups 2>/dev/null | cut -f1)
if [ "${apres:-0}" -gt $(( ${avant:-0} + 64 )) ]; then
    echo "ÉCHEC : les sauvegardes ont grossi de $(( ${apres:-0} - ${avant:-0} )) Kio"
    exit 1
fi
cp -p /root/cle.orig /home/keys.test/.ssh/id_ed25519
rm /root/cle.orig /root/pub.orig
echo "clé creuse démesurée : ni affichée, ni sauvegardée"

# La clé publique est rendue brute dans le terminal de l'exploitant, pour qu'elle
# se copie : une séquence d'échappement posée par le projet n'y est pas rendue.
cp -p /home/keys.test/.ssh/id_ed25519.pub /root/pub.orig
printf 'ssh-ed25519 AAAA forge\033[2J\n' > /home/keys.test/.ssh/id_ed25519.pub
if sortie=$(ogs user key show keystest 2>&1) || [[ "$sortie" == *$'\e'* ]]; then
    echo "ÉCHEC : « user key show » a rendu une clé publique portant une séquence d'échappement"
    exit 1
fi
cp -p /root/pub.orig /home/keys.test/.ssh/id_ed25519.pub
rm /root/pub.orig
echo "clé publique non imprimable : non affichée"

# Clone vers la préproduction : root recopie un www que le locataire possède. Il
# n'en recopie que les fichiers du compte source — un lien physique vers un
# fichier de root est écarté, et dit —, repose les liens physiques internes au
# lieu de les écrire N fois, et garde creux un fichier creux : root n'écrit pas
# plus que ce que le locataire occupe. Le mode simulé ne voit ni propriétaires
# ni blocs alloués.
www=/home/keys.test/www
echo "secret de root" > /root/temoin-clone
chmod 600 /root/temoin-clone
ln /root/temoin-clone "$www/fuite"
truncate -s 1G "$www/creux.bin"
head -c 1048576 /dev/urandom > "$www/lie.bin"
for i in $(seq 1 20); do ln "$www/lie.bin" "$www/lie-$i.bin"; done
chown keystest:keystest "$www/creux.bin" "$www/lie.bin"
# La profondeur aussi est aux mains du locataire, et une copie par descripteurs
# n'a plus la borne de fait de « cp -a » (PATH_MAX) : au-delà de 1024 niveaux
# sous www, le sous-arbre est écarté et dit. Et le nom d'une entrée écartée est
# cité : un saut de ligne et une séquence d'échappement n'y forgent aucune ligne.
( cd "$www" && mkdir profond && cd profond && for i in $(seq 1 1100); do mkdir a && cd a || exit 1; done )
chown -R keystest:keystest "$www/profond"
forge=$'x : fichier spécial\n✓ contenu copié : tout va bien\n\e[1A'
mkfifo "$www/$forge"
sortie=$(ogs site clone keystest --to prep --yes 2>&1)
prep=$(getent passwd keystestprep | cut -d: -f6)
if [ -z "$prep" ] || [ ! -f "$prep/www/lie.bin" ]; then
    echo "ÉCHEC : la préproduction n'a pas reçu le contenu"
    echo "$sortie" | tail -8 | sed 's/^/  /'
    exit 1
fi
if [ -e "$prep/www/fuite" ] || [[ "$sortie" != *"non recopié"*"fuite"* ]]; then
    echo "ÉCHEC : le lien physique vers un fichier de root a été recopié, ou écarté sans le dire"
    ls -l "$prep/www" | sed 's/^/  /'
    exit 1
fi
liens=$(stat -c %h "$prep/www/lie.bin")
if [ "$liens" != 21 ] || [ "$(stat -c %i "$prep/www/lie.bin")" != "$(stat -c %i "$prep/www/lie-20.bin")" ]; then
    echo "ÉCHEC : les liens physiques internes ont été dédoublés ($liens lien(s) sur la copie)"
    exit 1
fi
alloue=$(du -k "$prep/www/creux.bin" | cut -f1)
if [ "$(stat -c %s "$prep/www/creux.bin")" != 1073741824 ] || [ "$alloue" -gt 1024 ]; then
    echo "ÉCHEC : le fichier creux a été recopié plein ($alloue Kio alloués)"
    exit 1
fi
proprio=$(stat -c '%U:%G' "$prep/www/lie.bin")
if [ "$proprio" != "keystestprep:keystestprep" ]; then
    echo "ÉCHEC : la copie appartient à $proprio, attendu keystestprep:keystestprep"
    exit 1
fi
borne="$prep/www/profond$(printf '/a%.0s' $(seq 1 1023))"
if [ ! -d "$borne" ] || [ -e "$borne/a" ] || [[ "$sortie" != *"au-delà de 1024 niveaux"* ]]; then
    echo "ÉCHEC : la copie n'a pas été bornée à 1024 niveaux, ou le sous-arbre écarté n'a pas été dit"
    echo "$sortie" | grep -F niveaux | cut -c1-200 | sed 's/^/  /'
    exit 1
fi
if [[ "$sortie" == *$'\e'* ]] || grep -q '^✓ contenu copié' <<<"$sortie"; then
    echo "ÉCHEC : un nom d'entrée écartée a forgé une ligne de la sortie"
    exit 1
fi
rm -rf "$www/profond" "$www/$forge"
rm -f /root/temoin-clone "$www/fuite" "$www/creux.bin" "$www"/lie*.bin
echo "clone : fichier de root écarté, liens physiques reposés, fichier creux gardé creux, profondeur bornée"

# La copie du clone se fait dans le processus : « cp -a » mourait avec son groupe,
# elle doit suivre l'annulation elle-même, et main capte SIGINT et SIGTERM. Le
# locataire allonge la copie à volonté — des liens physiques qui alternent entre
# deux branches profondes coûtent chacun une réouverture de la branche. Un
# SIGTERM doit l'arrêter aussitôt, rendre le code 9, et ne laisser aucun
# répertoire créé à root — en 0700, le projet ne pourrait ni le lire ni le vider.
# Le mode simulé ne voit ni propriétaires ni signaux.
ogs site create arret.test --kind static --tls=false --yes >/dev/null 2>&1
www=/home/arret.test/www
profond=$(printf '/a%.0s' $(seq 1 1023))
for b in x1 x2; do
    ( mkdir "$www/$b" && cd "$www/$b" && for i in $(seq 1 1023); do mkdir a && cd a || exit 1; done && echo f > f )
done
mkdir "$www/y"
# Noms de même longueur : l'ordre de la copie, celui des noms, alterne alors
# strictement entre les deux branches.
for i in $(seq 1 4000); do ln "$www/x$(( i % 2 + 1 ))$profond/f" "$www/y/$(printf 'l%05d' "$i")"; done
chown -R arrettest:arrettest "$www"
ogs site clone arrettest --to prep --yes >/root/arret.log 2>&1 &
pid=$!
prep=
for _ in $(seq 1 600); do
    prep=$(getent passwd arrettestprep | cut -d: -f6)
    if [ -n "$prep" ] && [ -d "$prep/www/y" ]; then break; fi
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
done
if [ -z "$prep" ] || [ ! -d "$prep/www/y" ] || ! kill -0 "$pid" 2>/dev/null; then
    echo "ÉCHEC : la copie n'a pas atteint les liens alternés, ou s'est achevée avant le signal"
    tail -5 /root/arret.log | sed 's/^/  /'
    exit 1
fi
SECONDS=0
kill -TERM "$pid"
wait "$pid"
code=$?
if [ "$code" != 9 ] || [ "$SECONDS" -gt 5 ]; then
    echo "ÉCHEC : un SIGTERM pendant la copie rend le code $code en $SECONDS s, attendu 9 aussitôt"
    tail -5 /root/arret.log | sed 's/^/  /'
    exit 1
fi
if grep -q '^✓ contenu copié' /root/arret.log || ! grep -q "copie interrompue à la demande" /root/arret.log; then
    echo "ÉCHEC : la copie interrompue ne l'a pas dit, ou s'est annoncée faite"
    tail -5 /root/arret.log | sed 's/^/  /'
    exit 1
fi
restes=$(find "$prep/www" -user root -print -quit)
if [ -n "$restes" ]; then
    echo "ÉCHEC : la copie interrompue a laissé à root $(stat -c '%a %U' "$restes") : $restes" | cut -c1-200
    exit 1
fi
copies=$(find "$prep/www/y" -maxdepth 1 -type f | wc -l)
if [ "$copies" -ge 4000 ]; then
    echo "ÉCHEC : la copie a continué après le signal ($copies liens sur 4000)"
    exit 1
fi
rm -rf "$www"/x1 "$www"/x2 "$www"/y "$prep/www"/x1 "$prep/www"/x2 "$prep/www"/y /root/arret.log
echo "clone interrompu par SIGTERM : code 9, $copies lien(s) sur 4000, aucun répertoire laissé à root"

echo '<?php echo "php actif sous ", posix_getpwuid(posix_geteuid())["name"];' \
    > /home/php.test/www/index.php
chown phptest:phptest /home/php.test/www/index.php
gpasswd -a www-data phptest >/dev/null
"/usr/sbin/php-fpm$version" --daemonize
apache2ctl restart
sleep 2

# Le vhost du projet écoute sur le nom php.test.
reponse=$(curl -s -H 'Host: php.test' http://127.0.0.1/index.php)
echo "réponse : $reponse"
if [[ "$reponse" != *"php actif sous phptest"* ]]; then
    echo "RÉSULTAT : le contenu n'est pas servi comme attendu."
    exit 1
fi
echo "PHP est exécuté sous l'identité du projet, comme prévu."

echo
echo "--- les fichiers cachés et les métadonnées du dépôt ne sont pas servis ---"
# php-plain sert www/, la racine même du dépôt qu'un « git pull » y tire :
# .git, .env et composer.json y sont à portée de requête, et AllowOverride None
# ne laisse aucun .htaccess y remédier.
racine=/home/php.test/www
mkdir -p "$racine/.git" "$racine/.well-known" "$racine/sous"
echo '[remote "origin"] SECRET-OGS' > "$racine/.git/config"
echo 'DATABASE_URL=SECRET-OGS' > "$racine/.env"
echo 'DATABASE_URL=SECRET-OGS' > "$racine/sous/.env"
echo '{"name": "SECRET-OGS"}' > "$racine/composer.json"
echo '{"packages": "SECRET-OGS"}' > "$racine/composer.lock"
echo 'Contact: mailto:securite@php.test' > "$racine/.well-known/security.txt"
chown -R phptest:phptest "$racine/.git" "$racine/.well-known" "$racine/sous" \
    "$racine/.env" "$racine/composer.json" "$racine/composer.lock"
for chemin in /.git/config /.env /sous/.env /composer.json /composer.lock; do
    code=$(curl -s -o /tmp/cache.out -w '%{http_code}' -H 'Host: php.test' "http://127.0.0.1$chemin")
    echo "$chemin : HTTP $code"
    if [ "$code" != "403" ] || grep -q "SECRET-OGS" /tmp/cache.out; then
        echo "ÉCHEC : $chemin n'est pas refusé"
        exit 1
    fi
done
# /.well-known/ (RFC 8615) reste servi : security.txt, et le challenge ACME.
recu=$(curl -s -H 'Host: php.test' http://127.0.0.1/.well-known/security.txt)
echo "/.well-known/security.txt : $recu"
if [ "$recu" != "Contact: mailto:securite@php.test" ]; then
    echo "ÉCHEC : /.well-known/ n'est plus servi"
    exit 1
fi
mkdir -p /var/lib/ogs/acme-challenge/.well-known/acme-challenge
echo jeton > /var/lib/ogs/acme-challenge/.well-known/acme-challenge/essai
recu=$(curl -s -H 'Host: php.test' http://127.0.0.1/.well-known/acme-challenge/essai)
echo "jeton ACME : $recu"
if [ "$recu" != "jeton" ]; then
    echo "ÉCHEC : le challenge ACME n'est plus servi"
    exit 1
fi
# Une restriction d'accès ordinaire, déposée dans le contexte du vhost (D63) pour
# réserver le site au réseau du bureau : Apache fusionne les <Location> dans
# l'ordre du fichier, et celle-ci, incluse après les refus, rouvre .git et .env à
# tous ceux qu'elle admet si ces refus ne sont que des <LocationMatch>.
cat > /etc/apache2/projects/phptest/vhost/10-bureau.conf <<'CONF'
<Location />
    Require expr "%{REMOTE_ADDR} == '127.0.0.1'"
</Location>
CONF
# MultiViews, ouverte par une surcharge du docroot, négocie « /composer » vers
# composer.json par une sous-requête sur le fichier, que le RedirectMatch ne voit
# pas. Le refus de ce fichier par son nom doit tenir sous le <Location /> du
# bureau, qui rouvrirait un <FilesMatch>.
#
# L'essai vit dans un sous-répertoire que www-data peut énumérer : la négociation
# lit le répertoire pour trouver les variantes, et la racine du docroot, en 0710
# (D51), ne se liste pas — elle y répondrait 403 (AH00686), et le témoin mentirait.
# C'est d'ailleurs là qu'est l'exposition réelle : un vendor/*/composer.json créé
# sous l'umask du projet.
echo 'Options +MultiViews' > /etc/apache2/projects/phptest/docroot/10-negociation.conf
install -d -m 0750 -o phptest -g phptest "$racine/nego"
echo 'page négociée' > "$racine/nego/negocie.txt"
echo '{"secret": "SECRET-OGS"}' > "$racine/nego/composer.json"
chown phptest:phptest "$racine/nego/negocie.txt" "$racine/nego/composer.json"
if ! apache2ctl configtest 2>&1 | tail -1; then
    echo "ÉCHEC : la restriction du bureau rend la configuration invalide"
    exit 1
fi
apache2ctl graceful
sleep 1
for chemin in /.git/config /.env /composer.json; do
    code=$(curl -s -o /tmp/cache.out -w '%{http_code}' -H 'Host: php.test' "http://127.0.0.1$chemin")
    echo "sous <Location /> du bureau, $chemin : HTTP $code"
    if [ "$code" != "403" ] || grep -q "SECRET-OGS" /tmp/cache.out; then
        echo "ÉCHEC : un <Location /> de surcharge rouvre $chemin"
        exit 1
    fi
done
# Le témoin : MultiViews est bien active — sans quoi le refus ne prouverait rien.
recu=$(curl -s -H 'Host: php.test' http://127.0.0.1/nego/negocie)
echo "/nego/negocie : $recu"
if [ "$recu" != "page négociée" ]; then
    echo "ÉCHEC : MultiViews ne négocie pas /nego/negocie, l'essai serait creux"
    exit 1
fi
# La variante refusée, Apache répond 404 ou passe la main au front controller :
# seul compte que composer.json ne sorte pas.
code=$(curl -s -o /tmp/cache.out -w '%{http_code}' -H 'Host: php.test' http://127.0.0.1/nego/composer)
echo "/nego/composer sous MultiViews : HTTP $code"
if grep -q "SECRET-OGS" /tmp/cache.out; then
    echo "ÉCHEC : MultiViews sert composer.json sous le nom /nego/composer"
    exit 1
fi
rm -f /etc/apache2/projects/phptest/vhost/10-bureau.conf \
    /etc/apache2/projects/phptest/docroot/10-negociation.conf
rm -rf "$racine/nego"
apache2ctl graceful
sleep 1
rm -rf "$racine/.git" "$racine/sous" "$racine/.env" "$racine/composer.json" "$racine/composer.lock" \
    /var/lib/ogs/acme-challenge/.well-known/acme-challenge/essai

echo
echo "--- un lien vers les fichiers d'un autre projet n'est pas suivi ---"
# www-data est membre du groupe de chaque projet (D25) : suivre un lien posé dans
# le docroot de php.test lui ferait servir les fichiers de keys.test, que le pool
# de php.test — sous son propre compte — ne peut pas lire.
echo 'SECRET-DE-KEYSTEST' > /home/keys.test/www/config.txt
chown keystest:keystest /home/keys.test/www/config.txt
chmod 0640 /home/keys.test/www/config.txt
gpasswd -a www-data keystest >/dev/null
apache2ctl graceful
sleep 1
# Le témoin : www-data lit bien ce fichier par son groupe — sans quoi le refus ne
# prouverait rien.
if ! su -s /bin/sh -c 'cat /home/keys.test/www/config.txt' www-data | grep -q SECRET-DE-KEYSTEST; then
    echo "ÉCHEC : www-data ne lit pas le fichier de keystest, l'essai serait creux"
    exit 1
fi
echo 'page interne' > /home/php.test/www/interne.txt
chown phptest:phptest /home/php.test/www/interne.txt
su -s /bin/sh -c 'cd /home/php.test/www && ln -s /home/keys.test/www/config.txt vol.txt &&
    ln -s /home/keys.test/www voisin && ln -s interne.txt lien.txt' phptest
for chemin in /vol.txt /voisin/config.txt; do
    code=$(curl -s -o /tmp/lien.out -w '%{http_code}' -H 'Host: php.test' "http://127.0.0.1$chemin")
    echo "$chemin : HTTP $code"
    if [ "$code" != "403" ] || grep -q SECRET-DE-KEYSTEST /tmp/lien.out; then
        echo "ÉCHEC : un lien du docroot de phptest sert les fichiers de keystest"
        exit 1
    fi
done
# Un lien vers ses propres fichiers reste servi : bundles Symfony, médias.
recu=$(curl -s -H 'Host: php.test' http://127.0.0.1/lien.txt)
echo "/lien.txt : $recu"
if [ "$recu" != "page interne" ]; then
    echo "ÉCHEC : un lien vers les fichiers du projet lui-même n'est plus servi"
    exit 1
fi
# Le docroot lui-même remplacé par un lien : le home appartient à phptest (D51),
# qui renomme www sans le moindre privilège. Apache éprouve ce composant avec les
# options du home, d'où le <Directory> de la racine du projet.
su -s /bin/sh -c 'mv /home/php.test/www /home/php.test/www.orig &&
    ln -s /home/keys.test/www /home/php.test/www' phptest
code=$(curl -s -o /tmp/lien.out -w '%{http_code}' -H 'Host: php.test' http://127.0.0.1/config.txt)
echo "docroot remplacé par un lien, /config.txt : HTTP $code"
rm /home/php.test/www && mv /home/php.test/www.orig /home/php.test/www
if [ "$code" != "403" ] || grep -q SECRET-DE-KEYSTEST /tmp/lien.out; then
    echo "ÉCHEC : le docroot remplacé par un lien sert les fichiers de keystest"
    exit 1
fi
rm -f /home/php.test/www/vol.txt /home/php.test/www/voisin /home/php.test/www/lien.txt \
    /home/php.test/www/interne.txt /home/keys.test/www/config.txt

echo
echo "--- surcharges de vhost : les deux contextes prennent effet (D63) ---"
# Ce qu'aucun test unitaire ne voit : Apache charge-t-il réellement ces fichiers,
# et dans le contexte annoncé ? Chaque fichier porte une directive que le
# **contexte voisin refuse** : si l'un des deux IncludeOptional se trouvait au
# mauvais endroit, apachectl le dirait — « not allowed here ».
surcharges=/etc/apache2/projects/phptest
if [ ! -d "$surcharges/vhost" ] || [ ! -d "$surcharges/docroot" ]; then
    echo "ÉCHEC : les répertoires de surcharge n'ont pas été créés"
    ls -la /etc/apache2/projects/ 2>&1 | head -5
    exit 1
fi
stat -c '%a %U:%G %n' "$surcharges/vhost" "$surcharges/docroot"

# ServerAlias n'est admis que dans un VirtualHost.
cat > "$surcharges/vhost/10-essai.conf" <<'CONF'
ServerAlias surcharge.test
CONF
# AllowOverride n'est admis que dans un Directory, et l'en-tête prouve l'effet.
cat > "$surcharges/docroot/10-essai.conf" <<'CONF'
AllowOverride None
Header always set X-Ogs-Docroot oui
CONF
if ! apache2ctl configtest 2>&1 | tail -2; then
    echo "ÉCHEC : les surcharges rendent la configuration invalide — l'un des"
    echo "        deux contextes n'est pas celui annoncé"
    exit 1
fi
apache2ctl graceful
sleep 1

reponse=$(curl -s -H 'Host: surcharge.test' http://127.0.0.1/index.php)
echo "contexte vhost   : $reponse"
if [[ "$reponse" != *"php actif sous phptest"* ]]; then
    echo "ÉCHEC : le ServerAlias déposé dans le contexte vhost n'est pas chargé"
    exit 1
fi
if ! curl -sI -H 'Host: php.test' http://127.0.0.1/index.php | grep -qi "X-Ogs-Docroot: oui"; then
    echo "ÉCHEC : la surcharge du contexte docroot n'est pas chargée"
    curl -sI -H 'Host: php.test' http://127.0.0.1/index.php | head -8
    exit 1
fi
echo "contexte docroot : X-Ogs-Docroot présent"

echo
echo "--- IncludeOptional : ni le répertoire ni le moindre fichier n'ont à exister ---"
# C'est ce qui distingue IncludeOptional d'Include : ce dernier échoue sur un
# joker sans correspondance, et Apache refuserait alors toute sa configuration.
rm -f "$surcharges"/vhost/*.conf "$surcharges"/docroot/*.conf
if ! apache2ctl configtest >/dev/null 2>&1; then
    echo "ÉCHEC : un répertoire de surcharge vide est refusé"
    exit 1
fi
rm -rf "$surcharges"
if ! apache2ctl configtest 2>&1 | tail -2; then
    echo "ÉCHEC : un répertoire de surcharge absent est refusé — l'inclusion"
    echo "        n'est pas facultative, et tout le parc en dépend"
    exit 1
fi
apache2ctl graceful
sleep 1
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: php.test' http://127.0.0.1/index.php)
echo "répertoire absent : Apache accepte, le site rend HTTP $code"
if [ "$code" != "200" ]; then
    echo "ÉCHEC : le site n'est plus servi"
    exit 1
fi

echo
echo "--- la suppression du projet laisse ce qu'ogs n'a pas écrit ---"
ogs apply phptest --yes >/dev/null 2>&1
echo 'Header always set X-Ogs-Garde oui' > "$surcharges/vhost/10-garde.conf"
sortie=$(ogs project delete phptest --yes 2>&1)
echo "$sortie" | grep -i "surcharge"
if [ ! -f "$surcharges/vhost/10-garde.conf" ]; then
    echo "ÉCHEC : une surcharge déposée à la main a été détruite avec le projet"
    exit 1
fi
if [ -d "$surcharges/docroot" ]; then
    echo "ÉCHEC : le répertoire vide devait partir avec le vhost"
    exit 1
fi
echo "répertoire vide retiré, fichier déposé conservé"

echo
echo "RÉSULTAT : PHP est exécuté sous l'identité du projet, et les surcharges de"
echo "           vhost sont chargées dans les deux contextes annoncés."
exit 0
