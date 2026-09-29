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
echo "RÉSULTAT : compte, groupe, home, pool et vhost ont tous disparu."
exit 0
