#!/bin/bash
# Éprouve une version de PHP dont le pool de la distribution (www.conf) a été
# retiré — ce que fait un script d'installation qui ne veut aucun pool sous
# www-data. FPM refuse alors de tourner tant qu'aucun projet ne l'emploie
# (« No pool defined »), et deux défauts restaient invisibles aux tests
# unitaires, où systemd n'est jamais joint :
#   - ogs ne faisait que recharger FPM : au premier site, le reload échouait sur
#     une unité arrêtée, et le site répondait 503 sur un simple avertissement ;
#   - à la suppression du dernier site, le rechargement d'un maître sans pool le
#     tuait, en échec, et l'unité restait activée pour échouer à chaque démarrage.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : FPM sans pool de distribution ==="
version=$(ls /etc/php | head -1)
unite="php$version-fpm"
echo "version PHP détectée : $version"

mv "/etc/php/$version/fpm/pool.d/www.conf" "/etc/php/$version/fpm/pool.d/www.conf.dist"
systemctl disable --now "$unite" >/dev/null 2>&1
systemctl reset-failed "$unite" >/dev/null 2>&1 || true
if systemctl is-active --quiet "$unite"; then
    echo "ÉCHEC : $unite tourne encore sans pool, le scénario n'éprouve rien"
    exit 1
fi

ogs init --quiet
choisir_moteur apache

echecs=0
sortie=$(ogs site create php.test --kind php-plain --php "$version" --tls=false --yes 2>&1)
code=$?
echo "$sortie" | tail -4
echo "code de retour : $code"
if [ "$code" -ne 0 ]; then
    echo "ÉCHEC : code $code à la création, le démarrage de FPM a dû échouer"
    echecs=1
fi
if ! systemctl is-active --quiet "$unite"; then
    echo "ÉCHEC : $unite n'a pas été démarré au premier pool"
    systemctl status "$unite" --no-pager 2>&1 | tail -5
    echecs=1
fi
if [ "$(systemctl is-enabled "$unite" 2>/dev/null)" != enabled ]; then
    echo "ÉCHEC : $unite n'est pas activé, il ne repartirait pas au redémarrage"
    echecs=1
fi

echo '<?php echo "fpm ok";' > /home/php.test/www/index.php
chown phptest:phptest /home/php.test/www/index.php
reponse=$(curl -s -H 'Host: php.test' http://127.0.0.1/index.php)
if [ "$reponse" != "fpm ok" ]; then
    echo "ÉCHEC : le site ne répond pas par PHP : « $reponse »"
    echecs=1
fi

sortie=$(ogs project delete phptest --no-dump --yes 2>&1)
code=$?
echo "$sortie" | tail -4
echo "code de retour : $code"
if [ "$code" -ne 0 ]; then
    echo "ÉCHEC : code $code à la suppression du dernier site"
    echecs=1
fi
etat=$(systemctl is-active "$unite" 2>/dev/null)
if [ "$etat" != inactive ]; then
    echo "ÉCHEC : $unite est « $etat » après le départ de son dernier pool, « inactive » attendu"
    echecs=1
fi
if [ "$(systemctl is-enabled "$unite" 2>/dev/null)" = enabled ]; then
    echo "ÉCHEC : $unite reste activé sans pool, il échouerait à chaque démarrage"
    echecs=1
fi
if getent passwd phptest >/dev/null; then
    echo "ÉCHEC : le compte phptest existe encore"
    echecs=1
fi

if [ "$echecs" -ne 0 ]; then
    echo "RÉSULTAT : FPM n'a pas suivi ses pools."
    exit 1
fi
echo "RÉSULTAT : FPM démarré au premier pool, arrêté et désactivé au dernier."
exit 0
