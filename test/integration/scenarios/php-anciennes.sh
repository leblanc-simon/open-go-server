#!/bin/bash
# Éprouve PHP 7.1 et 7.2 sur leurs vrais FPM (D125) : le pool rendu doit être
# accepté — decorate_workers_output, né avec 7.3, y ferait refuser le pool
# entier —, servir le site, et le client MySQL de ces versions doit se connecter
# à sa base avec le mot de passe d'ogs (D126 : MariaDB authentifie par
# mysql_native_password, et ogs ne doit rien y imposer). La bascule entre une
# version ancienne et celle de la distribution doit aboutir dans les deux sens.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : PHP 7.1 et 7.2 ==="
recente=$(version_php)
for v in 7.1 7.2; do
    if [ ! -x "/usr/sbin/php-fpm$v" ]; then
        echo "ÉCHEC : php-fpm$v absent de l'image"
        exit 1
    fi
done
echo "versions : 7.1, 7.2 et $recente (distribution)"

mkdir -p /run/mysqld && chown mysql:mysql /run/mysqld
mariadbd-safe --skip-syslog >/tmp/mariadb.log 2>&1 &
for _ in $(seq 1 30); do
    mariadb -e 'SELECT 1' >/dev/null 2>&1 && break
    sleep 1
done
if ! mariadb -e 'SELECT 1' >/dev/null 2>&1; then
    echo "ÉCHEC : MariaDB n'a pas démarré"
    tail -5 /tmp/mariadb.log
    exit 1
fi

apache2ctl start
sleep 1
ogs init --quiet
choisir_moteur apache

# sonde dépose un script qui dit sa version de PHP et se connecte à sa base,
# puis rend la réponse du site. Le PHP de la distribution n'a pas mysqli dans
# l'image : seuls 7.1 et 7.2 éprouvent la connexion.
sonde() {
    local host=$1 id=$2
    local motdepasse
    motdepasse=$(ogs secrets show "$id" "mysql/$id" 2>/dev/null | tail -1)
    cat >"/home/$host/www/sonde.php" <<PHP
<?php
echo "php=", PHP_MAJOR_VERSION, ".", PHP_MINOR_VERSION, "\n";
if (!class_exists("mysqli")) { echo "base=sans-mysqli\n"; exit; }
mysqli_report(MYSQLI_REPORT_OFF);
\$c = @new mysqli("localhost", "$id", "$motdepasse", "$id");
echo \$c->connect_errno ? "base=" . \$c->connect_error : "base=ok", "\n";
PHP
    chown "$id:$id" "/home/$host/www/sonde.php"
    curl -s -H "Host: $host" http://127.0.0.1/sonde.php
}

for v in 7.1 7.2; do
    host="php${v/./}.test" id="php${v/./}test"
    echo
    echo "--- site en PHP $v ---"
    ogs site create "$host" --kind php-plain --php "$v" --db mysql --tls=false --yes 2>&1 | tail -3
    pool="/etc/php/$v/fpm/pool.d/$id.conf"
    if [ ! -f "$pool" ]; then
        echo "ÉCHEC : le pool $pool n'a pas été écrit"
        exit 1
    fi
    if grep -q '^decorate_workers_output' "$pool"; then
        echo "ÉCHEC : decorate_workers_output rendu dans un pool $v"
        exit 1
    fi
    if ! "php-fpm$v" -t 2>&1 | tail -1; then
        echo "ÉCHEC : FPM $v refuse le pool rendu"
        "php-fpm$v" -t
        exit 1
    fi
    relancer_fpm "$v" "$host" "/home/$host/www" || exit 1
    reponse=$(sonde "$host" "$id")
    echo "$reponse" | sed 's/^/  /'
    if ! grep -qx "php=$v" <<<"$reponse"; then
        echo "ÉCHEC : le site n'est pas servi par PHP $v"
        exit 1
    fi
    if ! grep -qx "base=ok" <<<"$reponse"; then
        echo "ÉCHEC : PHP $v ne se connecte pas à sa base"
        exit 1
    fi
    plugin=$(mariadb -N -e "SELECT plugin FROM mysql.user WHERE User='$id'")
    echo "  plugin du compte : $plugin"
    if [ "$plugin" != mysql_native_password ]; then
        echo "ÉCHEC : plugin inattendu sous MariaDB ($plugin)"
        exit 1
    fi
done

echo
echo "--- bascule 7.1 → $recente → 7.1 ---"
for cible in "$recente" 7.1; do
    # « php switch » modifie la déclaration ; l'apply pose le pool et le vhost.
    if ! { ogs php switch php71test "$cible" --yes && ogs apply php71test --yes; } >/tmp/switch.log 2>&1; then
        echo "ÉCHEC : bascule vers $cible refusée"
        tail -5 /tmp/switch.log
        exit 1
    fi
    if ! relancer_fpm "$cible" php71.test /home/php71.test/www; then
        sed 's/^/  /' /tmp/switch.log
        grep -rn "fcgi\|proxy:unix" /etc/apache2/sites-enabled/php71test.conf | sed 's/^/  /'
        ls /etc/php/*/fpm/pool.d/ | sed 's/^/  /'
        pgrep -fa php-fpm | sed 's/^/  /'
        exit 1
    fi
    reponse=$(sonde php71.test php71test)
    attendu=base=ok
    [ "$cible" = "$recente" ] && attendu=base=sans-mysqli
    if ! grep -qx "php=$cible" <<<"$reponse" || ! grep -qx "$attendu" <<<"$reponse"; then
        echo "ÉCHEC : après bascule vers $cible :"
        echo "$reponse" | sed 's/^/  /'
        exit 1
    fi
    echo "  servi par PHP $cible ($attendu)"
done

echo
echo "--- rejouer ne change rien ---"
for id in php71test php72test; do
    if ! ogs diff "$id" >/tmp/diff.log 2>&1; then
        echo "ÉCHEC : $id n'est pas conforme après apply"
        cat /tmp/diff.log
        exit 1
    fi
done
echo "plans vides"

echo
echo "=== scénario php-anciennes : succès ==="
