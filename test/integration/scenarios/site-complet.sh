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
echo "--- .htaccess ignoré sous Apache, règles portées par le template (D99) ---"
# ogs n'active plus les .htaccess (AllowOverride None) : servi par www-data, membre
# du groupe de tous les projets, un .htaccess du projet pouvait détourner le serveur
# hors de son site (socket FPM d'un voisin, server-status) d'un simple RewriteRule.
# Les règles des .htaccess que livre Prestashop sont donc portées par le template ;
# un .htaccess du projet est sans effet, ni détournement ni 500.
sortie=$(ogs site create boutique.test --kind php-prestashop --php "$version" --tls=false --yes 2>&1)
echo "$sortie" | grep -E "vhost apache|pool php" | sed 's/^/  /'
# Le seul avertissement attendu est le rechargement de FPM : le substitut de
# systemctl le fait par « pkill -USR2 -f /usr/sbin/php-fpm<v> », qui ne trouve
# jamais le maître — son titre est réécrit en « php-fpm: master process (…) » —,
# donc pkill rend 1 et ogs le signale. On recharge FPM nous-mêmes ci-dessous.
avert=$(echo "$sortie" | grep "⚠")
echo "avertissement(s) de la création : ${avert:-aucun}"
if [ -n "$avert" ] && echo "$avert" | grep -qv "rechargement de php$version-fpm"; then
    echo "ÉCHEC : avertissement inattendu à la création de la boutique"
    exit 1
fi
# On vise le maître FPM par son pidfile : son titre de processus est réécrit, et
# « pkill -f /usr/sbin/php-fpm » le manque.
sock=$(sed -n 's/^listen = //p' "/etc/php/$version/fpm/pool.d/boutiquetest.conf")
kill -USR2 "$(cat "/run/php/php$version-fpm.pid")" 2>/dev/null || true
for _ in $(seq 1 10); do
    [ -S "$sock" ] && break
    sleep 1
done
if [ ! -S "$sock" ]; then
    echo "ÉCHEC : le pool FPM de boutique.test n'écoute pas ($sock)"
    exit 1
fi
racineps=/home/boutique.test/www
poser() { mkdir -p "$(dirname "$racineps/$1")" && printf '%s\n' "$2" > "$racineps/$1"; }
poser index.php '<?php echo "front boutique: ", ($_GET["controller"] ?? "accueil"), "\n";'
poser webservice/dispatcher.php '<?php echo "api ok\n";'
# Répertoire d'administration renommé à l'installation : reconnu à son
# cron_currency_rates.php ; une route sans fichier va à son index.php.
poser admin7x2k/cron_currency_rates.php '<?php'
poser admin7x2k/index.php '<?php echo "back-office\n";'
poser admin7x2k/backups/sauvegarde.sql.bz2 'secret'
poser admin7x2k/export/clients.csv 'secret'
poser admin7x2k/import/produits.csv 'secret'
poser admin-api/index.php '<?php echo "api admin\n";'
poser modules/m/ajax.php '<?php echo "ajax module\n";'
poser modules/m/logs/m.log 'secret'
poser js/retro-compat.js.php '<?php echo "retro ", $_GET["file"] ?? "", "\n";'
poser img/p/1/1-t-shirt.jpg 'image-1'
poser img/p/1/2/3/123-home_default.jpg 'image-123'
poser img/p/1/2/3/4/5/6/7/1234567-large_default.jpeg 'image-1234567'
poser img/c/3_thumb-category_default.jpg 'vignette-3'
poser img/notes.txt 'secret'
poser themes/classic/config/theme.yml 'secret'
poser themes/classic/assets/css/theme.css 'body{}'
poser js/porte.php '<?php echo "exécuté";'
poser police.woff2 'police'
poser upload/x.txt 'secret'
poser var/logs/debug.log 'secret'
poser app/config/parameters.php 'secret'
poser mails/fr/order_conf.html 'secret'
poser templates/index.tpl 'secret'
poser docs/csv_import/produits.csv 'exemple'
poser translations/cldr/fr.json '{}'
poser tools/foundation/foundation.js 'outil'
poser relais/page.txt 'contenu relais'
# Le code déployé est celui d'une 9 : le vhost, écrit à la création sur un
# docroot vide, porte encore les règles de la branche par défaut (la 8).
poser src/Core/Version.php "<?php
final class Version
{
    public const VERSION = '9.0.0';"
# Un .htaccess racine tel que l'engendre Prestashop — ici pour une boutique en
# sous-répertoire, que le template ne reprend pas — suivi, hors du bloc, d'un
# détournement par le drapeau [H] ; dans relais/, un SetHandler vers le socket
# d'un voisin. Tous ignorés.
cat > "$racineps/.htaccess" <<'HT'
# ~~start~~ Do not remove this comment, Prestashop will keep automatically the code outside this comment when .htaccess will be generated again
<IfModule mod_rewrite.c>
RewriteEngine on
RewriteRule . - [E=REWRITEBASE:/sub/]
RewriteRule ^api(?:/(.*))?$ %{ENV:REWRITEBASE}webservice/dispatcher.php?url=$1 [QSA,L]
RewriteRule ^.*$ %{ENV:REWRITEBASE}index.php [NC,L]
</IfModule>
# ~~end~~ Do not remove this comment, Prestashop will keep automatically the code outside this comment when .htaccess will be generated again
RewriteEngine on
RewriteRule ^ - [H=server-status]
HT
cat > "$racineps/relais/.htaccess" <<HT
SetHandler "proxy:unix:/run/php/php$version-fpm-voisin.sock|fcgi://localhost"
HT
chown -R boutiquetest:boutiquetest "$racineps"
if ! apache2ctl configtest 2>&1 | tail -1; then
    echo "ÉCHEC : le vhost Prestashop est refusé"
    exit 1
fi
apache2ctl graceful
sleep 1
# attendu URI code [contenu] : le contenu, s'il est donné, doit figurer dans la
# réponse, et « Server Status » jamais.
attendu() {
    local uri=$1 code=$2 contenu=${3:-} obtenu corps
    corps=$(curl -s -o /tmp/ps.out -w '%{http_code}' -H 'Host: boutique.test' "http://127.0.0.1$uri")
    obtenu=$corps
    printf '  %-42s %s %s\n' "$uri" "$obtenu" "$(head -c 40 /tmp/ps.out | tr '\n' ' ')"
    if [ "$obtenu" != "$code" ] || grep -q "Server Status" /tmp/ps.out ||
        { [ -n "$contenu" ] && ! grep -q "$contenu" /tmp/ps.out; }; then
        echo "ÉCHEC : $uri devait répondre $code${contenu:+ avec « $contenu »}"
        exit 1
    fi
}
# 0. Règles de la 8, écrites avant le déploiement : les scripts de modules et
#    retro-compat.js.php s'exécutent, et doctor dit que le vhost ne suit pas la 9.
attendu /modules/m/ajax.php 200 "ajax module"
attendu /js/ancien-module.js 200 "retro ancien-module.js"
diag=$(ogs doctor 2>&1 | grep "prestashop boutiquetest")
echo "  doctor : $diag"
if [[ "$diag" != *"ne porte pas les règles de la 9"* ]]; then
    echo "ÉCHEC : doctor ne signale pas le vhost resté aux règles de la 8"
    exit 1
fi
# L'apply réécrit le vhost pour la 9 ; la boutique en sous-répertoire du
# .htaccess racine y est annoncée par une étape non critique (code 6).
sortie=$(ogs apply boutiquetest --yes 2>&1)
code=$?
echo "$sortie" | grep -E "vhost|sous-répertoire" | sed 's/^/  /'
if [ "$code" != 6 ] || [[ "$sortie" != *"sous-répertoire (REWRITEBASE /sub/)"* ]]; then
    echo "ÉCHEC : l'apply devait annoncer la boutique en sous-répertoire (code $code)"
    exit 1
fi
if ! grep -q "Règles de Prestashop 9\." /etc/apache2/sites-available/boutiquetest.conf; then
    echo "ÉCHEC : le vhost n'a pas été réécrit pour la 9"
    exit 1
fi
sleep 1
# 1. Front controller et routes, malgré le .htaccess racine malveillant.
attendu / 200 "front boutique"
attendu /ma-categorie/mon-produit 200 "front boutique"
attendu /api/products 200 "api ok"
attendu /admin7x2k/sell/catalog/products 200 "back-office"
attendu /admin-api/oauth/token 200 "api admin"
# Refus des sauvegardes sous un répertoire réel seulement : une URL propre passe.
attendu /fr/import/12-produit.html 200 "front boutique"
attendu /module/export/ajax 200 "front boutique"
attendu /js/ancien-module.js 200 "front boutique"
# 2. Images de produit, identifiants de 1, 3 et 7 chiffres, .jpeg compris, et
#    vignette de catégorie de la 9.
attendu /1-t-shirt/vue.jpg 200 image-1
attendu /123-home_default/t-shirt.jpg 200 image-123
attendu /1234567-large_default/pull.jpeg 200 image-1234567
attendu /c/3_thumb-category_default/vetements.jpg 200 vignette-3
# 3. Ce que les .htaccess livrés refusent, et ce qu'ils ouvrent ; en 9, modules/
#    refuse aussi ses scripts et ses journaux.
for uri in /upload/x.txt /var/logs/debug.log /app/config/parameters.php \
    /admin7x2k/backups/sauvegarde.sql.bz2 /admin7x2k/export/clients.csv \
    /admin7x2k/import/produits.csv /img/notes.txt /themes/classic/config/theme.yml \
    /js/porte.php /js/retro-compat.js.php /mails/fr/order_conf.html /templates/index.tpl \
    /modules/m/ajax.php /modules/m/logs/m.log; do
    attendu "$uri" 403
done
attendu /docs/csv_import/produits.csv 200 exemple
attendu /translations/cldr/fr.json 200
attendu /tools/foundation/foundation.js 200 outil
attendu /themes/classic/assets/css/theme.css 200
# 4. Le SetHandler d'un .htaccess vers un voisin est ignoré : la page est servie.
attendu /relais/page.txt 200 "contenu relais"
police=$(curl -sI -H 'Host: boutique.test' http://127.0.0.1/police.woff2 | grep -i '^content-type')
echo "  police : $police"
[[ "$police" == *"font/woff2"* ]] || { echo "ÉCHEC : AddType des polices absent"; exit 1; }
# 5. doctor nomme ce que le template ne reprend pas — la boutique en
#    sous-répertoire, les directives hors du bloc, le .htaccess de relais/ —, et
#    rien des .htaccess que livre Prestashop ; le vhost suit désormais la 9.
diag=$(ogs doctor 2>&1 | grep "htaccess boutiquetest")
echo "  doctor : $diag"
for mot in "sous-répertoire" "/sub/" "hors du bloc" "relais"; do
    if [[ "$diag" != *"$mot"* ]]; then
        echo "ÉCHEC : doctor ne signale pas « $mot »"
        exit 1
    fi
done
diag=$(ogs doctor 2>&1 | grep "prestashop boutiquetest")
echo "  doctor : $diag"
if [[ "$diag" != *"vhost aux règles de la 9"* ]]; then
    echo "ÉCHEC : doctor ne voit pas le vhost aux règles de la 9"
    exit 1
fi
ogs project delete boutiquetest --yes >/dev/null 2>&1
kill -USR2 "$(cat "/run/php/php$version-fpm.pid")" 2>/dev/null || true
echo "le .htaccess est ignoré, les règles du template servent le site, les répertoires protégés sont refusés"

echo "--- disable_functions et security.limit_extensions sous un vrai FPM (D103) ---"
# Le rendu ne prouve rien : seul le maître FPM dit si la valeur d'un pool est
# effective, et comment elle se combine avec celle du php.ini de la distribution.
pool="/etc/php/$version/fpm/pool.d/phptest.conf"
grep -E '^(php_admin_value\[disable_functions\]|security\.limit_extensions) ' "$pool" | sed 's/^/  /'
phpini="/etc/php/$version/fpm/php.ini"
distribution=$(sed -n 's/^disable_functions *= *//p' "$phpini" | tr -d ' ')
echo "  php.ini de la distribution : disable_functions = ${distribution:-(vide)}"
# Debian 13 n'y ferme rien : sans une valeur, la combinaison ne serait pas
# éprouvée. php.ini en reçoit une, rendue à la fin de l'épreuve.
cp "$phpini" /tmp/php.ini.dist
if [ -z "$distribution" ]; then
    sed -i 's/^disable_functions *=.*/disable_functions = phpinfo,getmypid/' "$phpini"
    distribution=phpinfo,getmypid
fi
pkill -f "php-fpm: master" || true
sleep 1
"/usr/sbin/php-fpm$version" --daemonize
sleep 1
# Un script écrit une fois pour toutes (opcache, revalidate_freq=2) : il dit
# l'état de chaque fonction et l'effet d'un appel.
cat > /home/php.test/www/fonctions.php <<'PHP'
<?php
echo "valeur=", ini_get('disable_functions'), "\n";
echo "include_path=", ini_get('include_path'), "\n";
echo "user_agent=", ini_get('user_agent'), "\n";
echo "sendmail_path=", ini_get('sendmail_path'), "\n";
foreach (explode(',', $_GET['f'] ?? '') as $f) {
    if ($f !== '') { echo $f, "=", function_exists($f) ? "ouverte" : "fermée", "\n"; }
}
try { exec('id -un', $sortie); echo "appel=", implode('', $sortie), "\n"; }
catch (\Error $e) { echo "appel=refusé ", get_class($e), "\n"; }
PHP
chown phptest:phptest /home/php.test/www/fonctions.php
liste_pool="exec,passthru,shell_exec,system,proc_open,popen,pcntl_exec"
reponse=$(curl -s -H 'Host: php.test' "http://127.0.0.1/fonctions.php?f=$liste_pool,$distribution")
echo "$reponse" | sed 's/^/  /'
for f in exec passthru shell_exec system proc_open popen; do
    if ! grep -qx "$f=fermée" <<<"$reponse"; then
        echo "ÉCHEC : $f reste ouverte sous php-plain"
        exit 1
    fi
done
if ! grep -q "^appel=refusé" <<<"$reponse"; then
    echo "ÉCHEC : exec() s'exécute encore"
    exit 1
fi
# La valeur du pool s'ajoute à celle de php.ini, elle ne la remplace pas : une
# fonction que la distribution ferme reste fermée — alors même que
# ini_get('disable_functions') ne rend que la valeur du pool.
for f in ${distribution//,/ }; do
    if ! grep -qx "$f=fermée" <<<"$reponse"; then
        echo "ÉCHEC : $f, fermée par php.ini, a été rouverte par le pool"
        exit 1
    fi
done
echo "  les fonctions de la liste sont fermées, celles de php.ini le restent"
cp /tmp/php.ini.dist "$phpini"

# allowFunctions rouvre une fonction sans recopier la liste. Dans le même
# apply : FPM ne distingue pas la casse des directives — « User: root » doit être
# écarté comme « user » —, un include ferait lire au maître root un pool entier,
# et une valeur portant « ; » ou « ${ » est citée pour être lue telle qu'écrite.
cat > /tmp/evil.conf <<'CONF'
[evil]
user = root
group = root
listen = /run/php/evil.sock
pm = static
pm.max_children = 1
CONF
sed -i 's/^        number: auto$/        number: auto\n        allowFunctions: [exec]\n        settings:\n          User: root\n          PM.max_children: "3"\n          include: \/tmp\/evil.conf\n          php_value[include_path]: ".:\/a;\/b"\n          php_value[user_agent]: "${HOME}x"\n          php_admin_value[sendmail_path]: \x27"\/usr\/sbin\/sendmail -t -i"\x27/' /etc/ogs/projects/phptest.yaml
if ! grep -q 'allowFunctions: \[exec\]' /etc/ogs/projects/phptest.yaml; then
    echo "ÉCHEC : le scénario n'a pas pu déclarer allowFunctions"
    exit 1
fi
ogs apply phptest --yes >/dev/null 2>&1
grep -E '^php_admin_value\[disable_functions\] ' "$pool" | sed 's/^/  /'
# Sans init, le substitut recharge FPM par un signal que le titre réécrit du
# maître peut faire manquer : le maître est relancé, pour que la réponse dise
# ce que le pool écrit produit, et non ce qu'un rechargement manqué a laissé.
pkill -f "php-fpm: master" || true
sleep 1
"/usr/sbin/php-fpm$version" --daemonize
sleep 1
reponse=$(curl -s -H 'Host: php.test' "http://127.0.0.1/fonctions.php?f=exec,system")
echo "$reponse" | sed 's/^/  /'
if ! grep -qx "exec=ouverte" <<<"$reponse" || ! grep -qx "system=fermée" <<<"$reponse" ||
    ! grep -qx "appel=phptest" <<<"$reponse"; then
    echo "ÉCHEC : allowFunctions n'a pas rouvert exec seule"
    exit 1
fi
echo "  allowFunctions: [exec] rouvre exec, et elle seule"
if grep -qiE '^(user = root|include)' "$pool" || [ "$(grep -ci '^pm.max_children = ' "$pool")" != 1 ]; then
    echo "ÉCHEC : le pool porte une directive verrouillée, ou deux fois la même :"
    grep -iE '^(user|include|pm.max_children) ' "$pool" | sed 's/^/  /'
    exit 1
fi
if [ -S /run/php/evil.sock ] || pgrep -f "pool evil" >/dev/null; then
    echo "ÉCHEC : le pool inclus a été chargé par le maître"
    exit 1
fi
if ! grep -qx "include_path=.:/a;/b" <<<"$reponse" || ! grep -qx 'user_agent=${HOME}x' <<<"$reponse"; then
    echo "ÉCHEC : une valeur portant « ; » ou « \${ » n'est pas lue telle qu'écrite"
    grep -E '^php_value' "$pool" | sed 's/^/  /'
    exit 1
fi
# Une valeur déjà citée à la manière de FPM se rend telle quelle : recouverte
# d'apostrophes, FPM la préfixait (« /usr/"/usr/sbin/sendmail -t -i" »).
if ! grep -qx "sendmail_path=/usr/sbin/sendmail -t -i" <<<"$reponse"; then
    echo "ÉCHEC : sendmail_path cité à la manière de FPM n'est pas lu tel quel"
    grep -E 'sendmail' "$pool" | sed 's/^/  /'
    exit 1
fi
echo "  User: root et include écartés, une seule pm.max_children, « ; », « \${ » et sendmail_path cité lus tels qu'écrits"
rm -f /tmp/evil.conf

# security.limit_extensions : un fichier qui n'est pas un .php n'est pas
# exécuté, même nommé à FPM. Le vhost ne lui en route aucun : on parle donc au
# socket, comme le ferait une configuration adoptée ou une surcharge (D63).
socket=$(sed -n 's/^listen = //p' "$pool")
cat > /tmp/fcgi.py <<'PY'
import socket, struct, sys

def record(kind, content):
    return struct.pack("!BBHHBx", 1, kind, 1, len(content), 0) + content

def pair(k, v):
    k, v = k.encode(), v.encode()
    size = lambda n: struct.pack("!B", n) if n < 128 else struct.pack("!I", n | 0x80000000)
    return size(len(k)) + size(len(v)) + k + v

def recv_exact(s, n):
    data = b""
    while len(data) < n:
        chunk = s.recv(n - len(data))
        if not chunk:
            break
        data += chunk
    return data

path, script, name = sys.argv[1], sys.argv[2], sys.argv[3]
params = {"SCRIPT_FILENAME": script, "SCRIPT_NAME": name, "REQUEST_URI": name,
          "REQUEST_METHOD": "GET", "QUERY_STRING": "", "SERVER_PROTOCOL": "HTTP/1.1",
          "GATEWAY_INTERFACE": "CGI/1.1", "SERVER_NAME": "php.test", "SERVER_PORT": "80",
          "REMOTE_ADDR": "127.0.0.1"}
s = socket.socket(socket.AF_UNIX)
s.connect(path)
body = b"".join(pair(k, v) for k, v in params.items())
s.sendall(record(1, struct.pack("!HB5x", 1, 0)) + record(4, body) + record(4, b"") + record(5, b""))
out = b""
while True:
    head = recv_exact(s, 8)
    if len(head) < 8:
        break
    _, kind, _, length, pad = struct.unpack("!BBHHBx", head)
    content = recv_exact(s, length + pad)[:length]
    if kind in (6, 7):
        out += content
    if kind == 3:
        break
sys.stdout.write(out.decode("utf-8", "replace"))
PY
echo '<?php echo "exécuté-php5\n";' > /home/php.test/www/essai.php5
echo '<?php echo "exécuté-php\n";' > /home/php.test/www/essai.php
chown phptest:phptest /home/php.test/www/essai.php5 /home/php.test/www/essai.php
php5=$(python3 /tmp/fcgi.py "$socket" /home/php.test/www/essai.php5 /essai.php5)
temoin=$(python3 /tmp/fcgi.py "$socket" /home/php.test/www/essai.php /essai.php)
etat=$(python3 /tmp/fcgi.py "$socket" /home/php.test/www/ogs-fpm-status /ogs-fpm-status)
echo "  .php5 : $(echo "$php5" | grep -iE 'status|denied|exécuté' | tr '\n' ' ')"
echo "  .php (témoin) : $(echo "$temoin" | grep 'exécuté' | tr '\n' ' ')"
if ! grep -q "exécuté-php" <<<"$temoin"; then
    echo "ÉCHEC : le témoin .php n'est pas exécuté par le socket — l'épreuve ne prouve rien"
    exit 1
fi
if grep -q "exécuté-php5" <<<"$php5"; then
    echo "ÉCHEC : un fichier .php5 est exécuté malgré security.limit_extensions"
    exit 1
fi
if ! grep -qi "^pool:" <<<"$etat"; then
    echo "ÉCHEC : le point d'état du pool ne répond plus sous security.limit_extensions"
    echo "$etat" | head -5 | sed 's/^/  /'
    exit 1
fi
echo "  .php5 refusé, .php exécuté, point d'état servi"
rm -f /home/php.test/www/essai.php5 /home/php.test/www/essai.php /tmp/fcgi.py

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
echo "--- paire dépareillée laissée en place, apply d'un projet sans rapport (D110, B2) ---"
# Une clé recopiée à la main sur shop.test, puis la création d'un autre site. Le
# rechargement de fin d'apply chargeait la paire : sous Apache, « apachectl -t »
# répondait « Syntax OK » et le maître mourait après le retour de graceful —
# tous les sites tombaient, code 0, rien de signalé. Le rechargement est refusé,
# nommé, et le serveur continue de servir sa configuration précédente.
ogs site create shop.test --kind static --redirect www.shop.test --self-signed --yes -q >/dev/null 2>&1
echo "boutique" > /home/shop.test/www/index.html
chown shoptest:shoptest /home/shop.test/www/index.html
gpasswd -a www-data shoptest >/dev/null
apache2ctl graceful
sleep 2
paire=/var/lib/ogs/acme/certificates/shoptest
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$paire/privkey.pem" 2>/dev/null
if ! apache2ctl -t >/dev/null 2>&1; then
    echo "ÉCHEC : le témoin manque — apachectl -t ne charge aucune clé et doit répondre Syntax OK"
    exit 1
fi
echo "témoin : apachectl -t répond Syntax OK sur la paire dépareillée"
sortie=$(ogs site create neuf.test --kind static --self-signed --yes 2>&1)
code=$?
echo "$sortie" | grep -E "dépareill|refusé|✗" | head -3
echo "site create neuf.test : code $code"
sleep 3
if ! pgrep -x apache2 >/dev/null; then
    echo "ÉCHEC : le serveur web est mort après l'apply d'un projet sans rapport"
    exit 1
fi
if ! [ "$code" = 6 ] || ! echo "$sortie" | grep -q "ogs cert renew shoptest"; then
    echo "ÉCHEC : attendu code 6, le certificat dépareillé nommé avec son remède"
    echo "$sortie" | tail -12
    exit 1
fi
reponse=$(curl -sk --resolve shop.test:443:127.0.0.1 https://shop.test/index.html)
echo "serveur vivant ; shop.test toujours servi : $reponse"
if [ "$reponse" != "boutique" ]; then
    echo "ÉCHEC : les sites doivent rester servis sur la configuration précédente"
    exit 1
fi
ogs doctor 2>&1 | grep -E "certificats des vhosts" | head -1
sortie=$(ogs cert renew shoptest 2>&1)
code=$?
echo "$sortie" | tail -2
sleep 3
if [ "$code" != 0 ] || ! pgrep -x apache2 >/dev/null; then
    echo "ÉCHEC : « cert renew » doit réémettre la paire et recharger le serveur (code $code)"
    exit 1
fi
ogs apply neuftest --yes >/dev/null 2>&1
sleep 2
reponse=$(curl -sk --resolve shop.test:443:127.0.0.1 https://shop.test/index.html)
if [ "$reponse" != "boutique" ] || ! pgrep -x apache2 >/dev/null; then
    echo "ÉCHEC : après « cert renew », le site doit être servi sur la paire rétablie"
    exit 1
fi
echo "paire réémise par « cert renew », serveur rechargé et vivant"
ogs project delete neuftest --yes >/dev/null 2>&1
ogs project delete shoptest --yes >/dev/null 2>&1

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
