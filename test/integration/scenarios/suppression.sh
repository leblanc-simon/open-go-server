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
