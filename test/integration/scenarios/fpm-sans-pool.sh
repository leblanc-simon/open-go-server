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
# Et, depuis D105 : un FPM arrêté est relevé par l'apply ; une configuration que
# « php-fpm -t » refuse n'est jamais rechargée, ni à la suppression d'un voisin
# ni par la rotation des journaux, et doctor la nomme.
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

# R1-49 (D105) : un FPM arrêté et désactivé alors que ses pools sont conformes
# n'était relevé qu'à l'écriture d'un pool, donc jamais.
systemctl disable --now "$unite" >/dev/null 2>&1
ogs apply phptest --yes >/dev/null 2>&1
if ! systemctl is-active --quiet "$unite" || [ "$(systemctl is-enabled "$unite" 2>/dev/null)" != enabled ]; then
    echo "ÉCHEC : l'apply n'a pas relevé $unite, arrêté et désactivé avec des pools conformes"
    echecs=1
fi
sortie=$(ogs apply phptest --yes 2>&1)
if grep -q "php/unit\|unité $unite" <<<"$sortie"; then
    echo "ÉCHEC : le plan oscille sur un FPM sain :"
    echo "$sortie" | tail -4
    echecs=1
fi

# Un pool de l'administrateur dont le compte a disparu : « php-fpm -t » le
# refuse, et tout maître qui relirait la configuration mourrait.
legacy="/etc/php/$version/fpm/pool.d/legacy.conf"
poser_legacy() {
    useradd --system --no-create-home legacy 2>/dev/null
    cat >"$legacy" <<CONF
[legacy]
user = legacy
group = legacy
listen = /run/php/legacy.sock
pm = ondemand
pm.max_children = 2
CONF
    systemctl reload "$unite"
    sleep 2
    userdel legacy
    groupdel legacy 2>/dev/null || true
}
repond() { [ "$(curl -s -H 'Host: php.test' http://127.0.0.1/index.php)" = "fpm ok" ]; }

# R3N-12 (D105) : retirer le pool d'un voisin rechargeait FPM sans « -t », qui
# mourait pour toute la version, et ogs annonçait un succès.
ogs site create voisin.test --kind php-plain --php "$version" --tls=false --yes >/dev/null 2>&1 ||
    { echo "ÉCHEC : création du voisin"; echecs=1; }
poser_legacy
pid_avant=$(systemctl show -p MainPID --value "$unite")
sortie=$(ogs project delete voisintest --no-dump --yes 2>&1)
echo "$sortie" | grep -i "fpm" | head -3
if ! grep -q "reste servi jusqu.à correction" <<<"$sortie"; then
    echo "ÉCHEC : le rechargement de FPM sur une configuration refusée n'est pas signalé"
    echecs=1
fi
if ! systemctl is-active --quiet "$unite" || [ "$(systemctl show -p MainPID --value "$unite")" != "$pid_avant" ]; then
    echo "ÉCHEC : la suppression d'un voisin a tué $unite"
    echecs=1
fi
repond || { echo "ÉCHEC : php.test ne répond plus après la suppression du voisin"; echecs=1; }

# R3N-01 (D105) : la rotation de la nuit ré-exécutait FPM (USR2) sans « -t ».
pid_avant=$(systemctl show -p MainPID --value "$unite")
logrotate -f /etc/logrotate.d/ogs 2>/tmp/logrotate.log
grep -q "fpm" /etc/logrotate.d/ogs && { echo "ÉCHEC : la rotation signale encore FPM"; echecs=1; }
if ! systemctl is-active --quiet "$unite" || [ "$(systemctl show -p MainPID --value "$unite")" != "$pid_avant" ]; then
    echo "ÉCHEC : la rotation a touché $unite"
    echecs=1
fi
repond || { echo "ÉCHEC : php.test ne répond plus après la rotation"; echecs=1; }
# doctor voit la configuration refusée, que rien d'autre ne montre.
docteur=$(ogs doctor 2>&1)
# Ni « grep -q » en bout de tube : sous pipefail, l'amont tué par SIGPIPE
# ferait échouer un tube qui a trouvé.
if [[ "$(grep "php-fpm $version" <<<"$docteur")" != *refusée* ]]; then
    echo "ÉCHEC : doctor ne voit pas la configuration FPM refusée"
    echo "$docteur" | tail -25
    echecs=1
fi
rm -f "$legacy"
systemctl reload "$unite"
sleep 2

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
echo "RÉSULTAT : FPM démarré au premier pool, relevé par l'apply, épargné par la suppression d'un voisin et la rotation sur une configuration refusée, arrêté et désactivé au dernier."
exit 0
