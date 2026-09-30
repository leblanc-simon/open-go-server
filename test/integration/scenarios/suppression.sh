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
ogs site create php.test --kind php-plain --php "$version" --tls=false --yes 2>&1 | tail -3

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

# La suppression par défaut, avec archive : --purge exige une confirmation
# interactive et ne se prête pas à un scénario.
sortie=$(ogs project delete phptest --no-dump --yes 2>&1)
code=$?
echo "$sortie" | tail -8
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

echo "RÉSULTAT : compte, groupe, home, pool et vhost ont tous disparu, et aucun home d'un autre compte n'est repris ni effacé."
exit 0
