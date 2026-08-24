#!/bin/bash
# Éprouve la création complète d'un site PHP sur un système réel : utilisateur,
# arborescence, pool FPM, vhost, et vérifie que PHP est effectivement exécuté.
set -uo pipefail

echo "=== scénario : site PHP servi de bout en bout ==="
version=$(ls /etc/php | head -1)
echo "version PHP détectée : $version"

apache2ctl start
sleep 1

ogs init --quiet
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
if [[ "$reponse" == *"php actif sous phptest"* ]]; then
    echo "RÉSULTAT : PHP est exécuté sous l'identité du projet, comme prévu."
    exit 0
fi
echo "RÉSULTAT : le contenu n'est pas servi comme attendu."
exit 1
