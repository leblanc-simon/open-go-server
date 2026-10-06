#!/bin/bash
# Éprouve la suppression d'un site dont le pool PHP-FPM tourne réellement. C'est
# le seul moyen de voir le défaut que les tests unitaires ne peuvent pas voir :
# les workers FPM appartiennent à l'utilisateur du projet, et userdel refuse de
# supprimer un compte dont un processus tourne encore. Tant que le rechargement
# de FPM venait après la suppression du compte, ogs laissait derrière lui le
# compte, le groupe et le home — en annonçant un succès.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : suppression d'un site dont le pool tourne ==="
version=$(ls /etc/php | head -1)
echo "version PHP détectée : $version"

# L'image porte les deux moteurs web, et sous systemd nginx démarre au boot :
# il occupe le port 80, qu'Apache ne pourrait alors pas ouvrir.
systemctl stop nginx
if ! systemctl start apache2; then
    echo "ÉCHEC : Apache n'a pas démarré, le rechargement du serveur web ne serait pas éprouvé"
    exit 1
fi

ogs init --quiet
choisir_moteur apache
# Preset small (pm dynamic) : le défaut, ondemand (D103), ne tient aucun worker
# tant que rien n'est servi, et le scénario n'aurait plus de processus à éprouver.
ogs site create php.test --kind php-plain --php "$version" --preset small --tls=false --yes 2>&1 | tail -3

# Le maître FPM doit tourner sous systemd, avec le pool du projet : sans workers
# vivants sous l'identité du projet, le scénario n'éprouverait rien — et c'est le
# rechargement par systemd qui doit les faire disparaître.
systemctl restart "php$version-fpm"
systemctl reload apache2
sleep 2

if ! pgrep -u phptest >/dev/null; then
    echo "ÉCHEC : aucun processus ne tourne sous phptest, le scénario n'éprouve rien"
    exit 1
fi
echo "processus actifs sous phptest avant suppression :"
ps -o pid=,comm= -u phptest | sed 's/^/  /'

home=$(getent passwd phptest | cut -d: -f6)
echo "home : $home"

# www-data doit être membre du groupe du projet : c'est ce qui fait échouer
# groupdel — « group has other members » — quand ogs oublie de l'en retirer.
if ! getent group phptest | grep -q www-data; then
    echo "ÉCHEC : www-data n'est pas membre du groupe phptest, le scénario n'éprouve rien"
    getent group phptest
    exit 1
fi
echo "groupe avant suppression : $(getent group phptest)"

# Deux remparts que --force ne lève pas, éprouvés ensemble : ils doivent être
# nommés dans un même refus. L'exploitant qui ne voyait que le premier suivait
# son remède, et découvrait le second au passage suivant.
#
# Un partage monté sous le home ne doit jamais être franchi : os.RemoveAll y
# descendait et le vidait pour tous ceux qui s'en servent. Seul un vrai montage
# le montre — la garde lit la table des montages du noyau.
#
# Un fichier qu'ogs n'a pas écrit, posé à l'un des chemins du projet, reste en
# place à la suppression — il sert peut-être autre chose — et arrête donc
# l'effacement du compte et du home, qu'il désigne peut-être : le vhost fait à la
# main d'un site adopté, qui journalisait sous le home, rendait la configuration
# d'Apache invalide une fois le home effacé.
#
# Les refus tombent à l'entrée : le pool tourne toujours pour la suite du scénario.
partage=$(mktemp -d /srv/partage.XXXXXX)
echo "à garder" > "$partage/commun.txt"
mkdir -p "$home/medias"
monte=0
if mount --bind "$partage" "$home/medias" 2>/dev/null; then
    monte=1
else
    echo "(montage impossible dans ce conteneur : la garde des points de montage n'est pas éprouvée)"
fi
tache=/etc/cron.d/ogs-phptest
pose=0
if [ ! -e "$tache" ]; then
    echo "0 3 * * * phptest true" > "$tache"
    pose=1
fi
if [ "$monte" = 1 ] || [ "$pose" = 1 ]; then
    if refus=$(ogs project delete phptest --no-dump --yes --force 2>&1); then
        echo "ÉCHEC : la suppression a passé outre un point de montage ou un fichier qu'ogs n'a pas écrit"
        exit 1
    fi
    if [ "$monte" = 1 ]; then
        if [ ! -f "$partage/commun.txt" ]; then
            echo "ÉCHEC : le contenu du partage monté a été effacé"
            exit 1
        fi
        if ! grep -qF "$home/medias" <<<"$refus"; then
            echo "ÉCHEC : le refus ne nomme pas le point de montage $home/medias :"
            echo "$refus"
            exit 1
        fi
    fi
    if [ "$pose" = 1 ] && ! grep -qF "$tache" <<<"$refus"; then
        echo "ÉCHEC : le refus ne nomme pas $tache, qu'ogs n'a pas écrit :"
        echo "$refus"
        exit 1
    fi
    if [ ! -d "$home" ] || ! getent passwd phptest >/dev/null; then
        echo "ÉCHEC : le refus devait précéder tout retrait"
        exit 1
    fi
    echo "la suppression refuse, en les nommant ensemble, de franchir un point de montage et d'effacer ce que désigne un fichier qu'ogs n'a pas écrit"
fi
if [ "$monte" = 1 ]; then
    umount "$home/medias"
fi
rmdir "$home/medias"
rm -rf "$partage"
if [ "$pose" = 1 ]; then
    rm -f "$tache"
fi

# Ce que le compte pose hors d'ogs survivait à la suppression (D107) : cron et
# atd relisent par le nom, et un compte recréé sous ce nom reprenait la crontab ;
# useradd redonne l'UID libéré au compte suivant, qui recevait ce que l'ancien
# laissait dans /tmp, /var/tmp et /dev/shm. Le linger garde un gestionnaire
# systemd sous l'UID, qui ferait refuser userdel.
uid=$(id -u phptest)
echo "*/5 * * * * true" | crontab -u phptest -
if ! crontab -l -u phptest >/dev/null 2>&1; then
    echo "ÉCHEC : la crontab personnelle de phptest n'a pas été posée, le scénario n'éprouve rien"
    exit 1
fi
# L'image n'a pas D-Bus : loginctl n'y joint pas logind. Le linger n'est alors
# que son fichier, que la suppression retire elle-même.
lingered=1
if ! loginctl enable-linger phptest 2>/dev/null; then
    echo "(logind injoignable : le linger est posé par son seul fichier)"
    mkdir -p /var/lib/systemd/linger
    touch /var/lib/systemd/linger/phptest
fi
runuser -u phptest -- mkdir -p /tmp/phptest-cache/sous
runuser -u phptest -- sh -c 'echo x > /tmp/phptest-cache/sous/f; echo s > /tmp/sess_phptest; echo v > /var/tmp/phptest.v; echo m > /dev/shm/phptest.m'
# Un lien de l'UID est retiré comme lien : sa cible n'est jamais touchée.
echo "à root" > /root/cible-du-lien
runuser -u phptest -- ln -s /root/cible-du-lien /tmp/phptest-lien
# Un fichier de root n'est pas à l'UID : il reste.
echo "à root" > /tmp/root-garde

# La suppression par défaut, avec archive : --purge exige une confirmation
# interactive et ne se prête pas à un scénario.
sortie=$(ogs project delete phptest --no-dump --yes 2>&1)
code=$?
echo "$sortie" | tail -12
echo "code de retour : $code"

echecs=0
if getent passwd phptest >/dev/null; then
    echo "ÉCHEC : le compte phptest existe encore"
    ps -o pid=,comm= -u phptest | sed 's/^/  /'
    echecs=1
fi
if getent group phptest >/dev/null; then
    echo "ÉCHEC : le groupe phptest existe encore"
    echecs=1
fi
if [ -d "$home" ]; then
    echo "ÉCHEC : le home $home existe encore"
    echecs=1
fi
if [ -f "/etc/php/$version/fpm/pool.d/phptest.conf" ]; then
    echo "ÉCHEC : le pool FPM existe encore"
    echecs=1
fi
if [ -f /etc/apache2/sites-available/phptest.conf ]; then
    echo "ÉCHEC : le vhost existe encore"
    echecs=1
fi
if [ -e /var/spool/cron/crontabs/phptest ]; then
    echo "ÉCHEC : la crontab personnelle de phptest a survécu au compte"
    echecs=1
fi
if ! grep -qF "crontab personnelle de phptest" <<<"$sortie"; then
    echo "ÉCHEC : la sortie ne nomme pas la crontab retirée"
    echecs=1
fi
if [ "$lingered" = 1 ] && { [ -e /var/lib/systemd/linger/phptest ] || ! grep -qF "linger de phptest" <<<"$sortie"; }; then
    echo "ÉCHEC : le linger de phptest a survécu, ou n'est pas nommé"
    echecs=1
fi
for f in /tmp/phptest-cache /tmp/sess_phptest /var/tmp/phptest.v /dev/shm/phptest.m /tmp/phptest-lien; do
    if [ -e "$f" ] || [ -L "$f" ]; then
        echo "ÉCHEC : $f, à l'UID $uid, a survécu au compte"
        echecs=1
    fi
done
if ! grep -qF "fichiers de l'UID $uid retirés" <<<"$sortie" || ! grep -qF "/tmp/phptest-cache" <<<"$sortie"; then
    echo "ÉCHEC : la sortie ne nomme pas les fichiers de l'UID retirés"
    echecs=1
fi
if [ "$(cat /root/cible-du-lien 2>/dev/null)" != "à root" ] || [ ! -f /tmp/root-garde ]; then
    echo "ÉCHEC : la purge a touché la cible d'un lien ou un fichier de root"
    echecs=1
fi
rm -f /root/cible-du-lien /tmp/root-garde
if grep -qiE "atq|tâches at" <<<"$sortie" && ! command -v atq >/dev/null; then
    echo "ÉCHEC : at n'est pas installé, la suppression ne doit pas en parler"
    echecs=1
fi
# Le numéro du pool revient à la plage (D100) : il restait attribué à jamais.
if ogs php ports 2>&1 | grep -q phptest; then
    echo "ÉCHEC : le numéro du pool est resté attribué"
    ogs php ports
    echecs=1
fi

# Une suppression complète ne doit pas se solder par un succès partiel : le
# code 6 signalerait qu'une ressource est restée en place.
if [ "$code" -ne 0 ]; then
    echo "ÉCHEC : code $code, une suppression complète doit rendre 0"
    echecs=1
fi

if [ "$echecs" -ne 0 ]; then
    echo "RÉSULTAT : la suppression a laissé des ressources derrière elle."
    exit 1
fi
echo "compte, groupe, home, pool et vhost ont tous disparu."

# Un home laissé par un autre compte n'est jamais repris (D95, §7.1.7). Le compte
# du projet est créé sans --create-home : la création donnait la racine d'alice
# au projet, sans le dire, et « project delete » l'effaçait ensuite en entier.
# Le refus précède le dépôt de la déclaration, qui arrêterait sinon tout apply.
useradd -m -d /home/autre.test -s /bin/bash alice
echo "à alice" > /home/autre.test/notes.txt
chown alice: /home/autre.test/notes.txt
if refus=$(ogs site create autre.test --kind static --tls=false --yes 2>&1); then
    echo "ÉCHEC : la création a repris le home d'alice :"
    echo "$refus" | tail -20
    ls -la /home/autre.test
    exit 1
fi
if ! grep -qF "appartient au compte alice" <<<"$refus"; then
    echo "ÉCHEC : le refus ne nomme pas le compte qui tient le home :"
    echo "$refus"
    exit 1
fi
if [ -e /etc/ogs/projects/autretest.yaml ]; then
    echo "ÉCHEC : la déclaration a été déposée malgré le refus"
    exit 1
fi
if getent passwd autretest >/dev/null; then
    echo "ÉCHEC : le compte autretest a été créé sur le home d'alice"
    exit 1
fi
if [ "$(stat -c %U /home/autre.test)" != alice ] || ! grep -q "à alice" /home/autre.test/notes.txt; then
    echo "ÉCHEC : le home d'alice a changé de mains ou perdu son contenu"
    exit 1
fi
echo "la création refuse le home d'un autre compte, avant d'en déposer la déclaration"

# Un compte supprimé à la main se recrée : son home ne porte que l'arborescence
# d'ogs, à un UID devenu orphelin. Refuser tout home non vide l'en empêchait.
ogs site create relu.test --kind static --tls=false --yes >/dev/null 2>&1
userdel relutest
if ! sortie=$(ogs apply relutest --yes 2>&1); then
    echo "ÉCHEC : l'apply ne recrée pas un compte supprimé à la main :"
    echo "$sortie" | tail -10
    exit 1
fi
if ! getent passwd relutest >/dev/null || [ "$(stat -c %U /home/relu.test)" != relutest ]; then
    echo "ÉCHEC : le compte relutest n'a pas été recréé, ou son home ne lui a pas été rendu"
    exit 1
fi
echo "un compte supprimé à la main est recréé, son home repris"

# « project create » et « site clone » éprouvent le home avant de déposer la
# déclaration, comme « site create ».
if refus=$(ogs project create autre.test --kind static 2>&1) || [ -e /etc/ogs/projects/autretest.yaml ]; then
    echo "ÉCHEC : project create a déposé la déclaration d'un home tenu par alice :"
    echo "$refus" | tail -5
    exit 1
fi
useradd -m -d /home/relu.preprod-pp.net -s /bin/bash carol
if refus=$(ogs site clone relutest --to prep --yes 2>&1) || [ -e /etc/ogs/projects/relutestprep.yaml ]; then
    echo "ÉCHEC : site clone a déposé la préproduction sur le home de carol :"
    echo "$refus" | tail -5
    exit 1
fi
if ! grep -qF "appartient au compte carol" <<<"$refus"; then
    echo "ÉCHEC : le refus du clone ne nomme pas carol :"
    echo "$refus" | tail -5
    exit 1
fi
echo "project create et site clone refusent le home d'un autre compte sans rien déposer"

# Le home d'un compte supprimé sans -r reste, à un UID orphelin que useradd
# redonne au compte suivant : repris, il donnait au projet les fichiers de bob
# et, par son authorized_keys régulier — jamais remplacé (D61) —, un shell sous
# le compte du projet à la clé de bob, puis « project delete » l'effaçait.
useradd -m -d /home/ancien.test -s /bin/bash bob
mkdir -p /home/ancien.test/.ssh
echo "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGJvYg bob" > /home/ancien.test/.ssh/authorized_keys
echo "à bob" > /home/ancien.test/secret.txt
chown -R bob: /home/ancien.test
userdel bob
if refus=$(ogs site create ancien.test --kind static --tls=false --yes 2>&1); then
    echo "ÉCHEC : la création a repris le home de bob, supprimé :"
    echo "$refus" | tail -10
    exit 1
fi
if ! grep -qF "authorized_keys" <<<"$refus" || [ -e /etc/ogs/projects/ancientest.yaml ] \
    || getent passwd ancientest >/dev/null || ! grep -q "à bob" /home/ancien.test/secret.txt; then
    echo "ÉCHEC : le refus ne nomme pas l'accès ssh de bob, ou quelque chose a été déposé ou touché :"
    echo "$refus" | tail -10
    exit 1
fi
echo "le home d'un compte supprimé n'est pas repris : ni ses fichiers ni son accès ssh"

# Un home repris avant D95 : la racine est au projet, mais ce qu'alice y avait
# laissé est toujours à elle. La suppression le voit et s'arrête.
ogs site create repris.test --kind static --tls=false --yes >/dev/null 2>&1
home=$(getent passwd repristest | cut -d: -f6)
if [ -z "$home" ] || [ ! -d "$home" ]; then
    echo "ÉCHEC : le projet repris.test n'a pas été créé"
    exit 1
fi
echo "à alice" > "$home/.bashrc-alice"
chown alice: "$home/.bashrc-alice"
if refus=$(ogs project delete repristest --no-dump --yes --force 2>&1); then
    echo "ÉCHEC : la suppression a effacé un home dont une entrée est à alice"
    exit 1
fi
if ! grep -qF ".bashrc-alice" <<<"$refus" || [ ! -f "$home/.bashrc-alice" ]; then
    echo "ÉCHEC : le refus ne nomme pas l'entrée d'alice, ou l'a effacée :"
    echo "$refus"
    exit 1
fi
echo "la suppression refuse d'effacer un home où un autre compte a laissé des entrées"

# Un groupe que d'autres comptes tiennent n'est pas repris par le compte à créer
# (D107) : ses membres recevraient le socket FPM du projet.
groupadd groupetest
useradd -M -s /usr/sbin/nologin erin
gpasswd -a erin groupetest >/dev/null
if refus=$(ogs site create groupe.test --kind static --tls=false --yes 2>&1) || [ -e /etc/ogs/projects/groupetest.yaml ]; then
    echo "ÉCHEC : la création a repris le groupe dont erin est membre :"
    echo "$refus" | tail -5
    exit 1
fi
if ! grep -qF "erin" <<<"$refus"; then
    echo "ÉCHEC : le refus ne nomme pas erin :"
    echo "$refus" | tail -5
    exit 1
fi
echo "la création refuse un groupe que d'autres comptes tiennent"

# À la suppression, le groupe d'un projet que d'autres tiennent reste, nommé, et
# la purge ne franchit pas un point de montage sous un répertoire de l'UID :
# deux restes, donc le code 6 — et le compte part quand même.
useradd -M -s /usr/sbin/nologin dave
gpasswd -a dave relutest >/dev/null
monte=0
partage=$(mktemp -d /srv/partage-uid.XXXXXX)
echo "à garder" > "$partage/commun.txt"
chown relutest "$partage/commun.txt"
runuser -u relutest -- mkdir -p /tmp/relutest-dir/monte
if mount --bind "$partage" /tmp/relutest-dir/monte 2>/dev/null; then
    monte=1
fi
# Un montage lié issu du MÊME système de fichiers garde le st_dev de sa source :
# seule la table des montages le révèle, et la purge le traversait, effaçant la
# source (relecture de D107). /tmp peut être un tmpfs : /var/tmp et /srv
# partagent le système de fichiers racine.
lie=0
source_lie=$(mktemp -d /srv/lie-uid.XXXXXX)
echo "à garder" > "$source_lie/f"
chown relutest "$source_lie/f"
runuser -u relutest -- mkdir -p /var/tmp/relutest-mt/monte
if [ "$(stat -c %d "$source_lie")" = "$(stat -c %d /var/tmp/relutest-mt)" ] &&
    mount --bind "$source_lie" /var/tmp/relutest-mt/monte 2>/dev/null; then
    lie=1
fi
sortie=$(ogs project delete relutest --no-dump --yes 2>&1)
code=$?
if [ "$monte" = 1 ]; then
    umount /tmp/relutest-dir/monte
fi
if [ "$lie" = 1 ]; then
    umount /var/tmp/relutest-mt/monte
    if [ ! -f "$source_lie/f" ] || ! grep -qF "/var/tmp/relutest-mt/monte" <<<"$sortie"; then
        echo "ÉCHEC : la purge a franchi un montage lié du même système de fichiers, ou ne le nomme pas :"
        echo "$sortie" | tail -10
        exit 1
    fi
    echo "la purge ne franchit pas un montage lié du même système de fichiers, et le nomme"
else
    echo "(montage lié sur le même système de fichiers impossible : cas non éprouvé)"
fi
rm -rf "$source_lie" /var/tmp/relutest-mt
if [ "$code" -ne 6 ]; then
    echo "ÉCHEC : un groupe conservé est un succès partiel, code 6 attendu, obtenu $code :"
    echo "$sortie" | tail -10
    exit 1
fi
if getent passwd relutest >/dev/null || ! getent group relutest | grep -q dave; then
    echo "ÉCHEC : le compte devait partir, et le groupe de dave rester :"
    getent passwd relutest; getent group relutest
    exit 1
fi
if ! grep -qF "groupe relutest conservé" <<<"$sortie" || ! grep -qF "dave" <<<"$sortie"; then
    echo "ÉCHEC : la sortie ne nomme pas le groupe conservé ni dave :"
    echo "$sortie" | tail -10
    exit 1
fi
if [ "$monte" = 1 ]; then
    if [ ! -f "$partage/commun.txt" ] || ! grep -qF "point de montage" <<<"$sortie"; then
        echo "ÉCHEC : la purge a franchi un point de montage, ou ne le nomme pas :"
        echo "$sortie" | tail -10
        exit 1
    fi
    echo "la purge des fichiers de l'UID ne franchit pas un point de montage, et le nomme"
else
    echo "(montage impossible : la garde des points de montage de la purge n'est pas éprouvée)"
fi
rm -rf "$partage" /tmp/relutest-dir
echo "la suppression garde, en le nommant, un groupe que d'autres comptes tiennent"

# Le site est coupé avant l'archive (D114) — vhost, pool et tâches retirés,
# serveurs rechargés — et rétabli si la suppression renonce avant le point de
# non-retour. Une archive impossible (un fichier à la place du répertoire des
# archives) est un tel abandon : le site doit de nouveau répondre, servi par un
# FPM que la coupure a pu arrêter faute de pool (D74).
ogs site create coupe.test --kind php-plain --php "$version" --tls=false --yes >/dev/null 2>&1
echo '<?php echo "servi";' > /home/coupe.test/www/index.php
chown coupetest:coupetest /home/coupe.test/www/index.php
repond() { [ "$(curl -s -H 'Host: coupe.test' http://127.0.0.1/index.php)" = "servi" ]; }
sleep 1
if ! repond; then
    echo "ÉCHEC : coupe.test ne répond pas avant la suppression, le scénario n'éprouve rien"
    exit 1
fi
mv /var/lib/ogs/archives /var/lib/ogs/archives.vrai
: > /var/lib/ogs/archives
sortie=$(ogs project delete coupetest --yes 2>&1); code=$?
rm -f /var/lib/ogs/archives
mv /var/lib/ogs/archives.vrai /var/lib/ogs/archives
if [ "$code" = 0 ] || ! grep -q "coupure du site" <<<"$sortie" || ! grep -q "rétablissement du site" <<<"$sortie"; then
    echo "$sortie" | tail -12
    echo "ÉCHEC : une archive impossible doit abandonner la suppression en rétablissant le site (code $code)"
    exit 1
fi
sleep 1
if ! repond || [ ! -f "/etc/php/$version/fpm/pool.d/coupetest.conf" ] || [ ! -e /etc/apache2/sites-enabled/coupetest.conf ]; then
    echo "$sortie" | tail -12
    echo "ÉCHEC : le site n'est pas rétabli après l'abandon"
    exit 1
fi
if ogs plan coupetest 2>&1 | grep -qE "coupetest.conf|ogs-coupetest"; then
    ogs plan coupetest
    echo "ÉCHEC : le site rétabli n'est pas conforme à sa déclaration"
    exit 1
fi
echo "la suppression coupe le site avant l'archive, et le rétablit quand elle renonce"

echo "RÉSULTAT : compte, groupe, home, pool et vhost ont tous disparu, et aucun home d'un autre compte n'est repris ni effacé."
exit 0
