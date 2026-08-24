#!/bin/bash
# Éprouve « ogs harden » sur un système où les services tournent réellement.
#
# Le risque de cette commande n'est pas de mal appliquer des droits : c'est de
# couper un service qui lisait sa configuration sous une identité non-root. Le
# scénario vérifie donc l'effet voulu — un utilisateur de projet ne voit plus
# rien — et l'absence d'effet de bord : le site est toujours servi, PHP lit
# toujours son php.ini, les clients de bases se connectent toujours, cron
# déclenche toujours.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : durcissement des droits de /etc ==="
version=$(ls /etc/php | head -1)

# Les serveurs sont démarrés avant : on veut constater qu'ils survivent.
mkdir -p /run/mysqld && chown mysql:mysql /run/mysqld
mariadbd-safe --skip-syslog >/tmp/mariadb.log 2>&1 &
pg_ctlcluster "$(ls /etc/postgresql | head -1)" main start >/dev/null 2>&1
su - postgres -c "createuser --superuser root" 2>/dev/null
for _ in $(seq 1 30); do mariadb -e 'SELECT 1' >/dev/null 2>&1 && break; sleep 1; done

ogs init --quiet
choisir_moteur apache
# Un répertoire de configuration d'un agent de métriques, pour éprouver un cas
# où le service tourne sous sa propre identité.
mkdir -p /etc/telegraf && chmod 0755 /etc/telegraf
printf 'token = "secret-de-metriques"\n' > /etc/telegraf/telegraf.conf
chmod 0644 /etc/telegraf/telegraf.conf
groupadd -r telegraf 2>/dev/null

ogs site create shop.fr --kind php-symfony --app-version 6.4 --php "$version" \
    --db mysql --tls=false --yes >/dev/null 2>&1
ogs cron add shopfr horodate --schedule "* * * * *" --command "/bin/date -Iseconds" >/dev/null
ogs apply shopfr --yes >/dev/null 2>&1
mkdir -p /home/shop.fr/www/public
printf '<?php echo "php ", PHP_VERSION, " ini=", php_ini_loaded_file() ? "oui" : "non";' \
    > /home/shop.fr/www/public/index.php
chown -R shopfr:shopfr /home/shop.fr/www
gpasswd -a www-data shopfr >/dev/null
systemctl restart apache2 "php$version-fpm"
sleep 2

echo
echo "--- avant : ce qu'un utilisateur de projet peut voir ---"
for d in /etc/nginx /etc/apache2 /etc/mysql /etc/ogs /etc/telegraf /etc/cron.d; do
    [ -d "$d" ] || continue
    if su -s /bin/sh shopfr -c "ls $d" >/dev/null 2>&1; then
        echo "  $d : LISIBLE ($(stat -c %a "$d"))"
    else
        echo "  $d : fermé ($(stat -c %a "$d"))"
    fi
done
# Le jeton de l'agent est-il lisible ?
if su -s /bin/sh shopfr -c 'cat /etc/telegraf/telegraf.conf' 2>/dev/null | grep -q secret; then
    echo "  le jeton de métriques est lisible par le projet"
else
    echo "ÉCHEC : le scénario devait partir d'une configuration ouverte"
    exit 1
fi

echo
echo "--- rapport ---"
ogs harden 2>&1 | head -20
code=${PIPESTATUS[0]}
echo "code de retour : $code (6 attendu : il reste à restreindre)"
if [ "$code" != "6" ]; then
    echo "ÉCHEC : le rapport doit rendre le code 6 tant qu'il reste des droits à restreindre"
    exit 1
fi

echo
echo "--- un répertoire déjà plus strict ne doit pas être desserré ---"
chmod 0700 /etc/apache2
ogs harden 2>&1 | grep -E "apache2"
ogs harden --apply --yes >/dev/null 2>&1
if [ "$(stat -c %a /etc/apache2)" != "700" ]; then
    echo "ÉCHEC : /etc/apache2 a été desserré ($(stat -c %a /etc/apache2))"
    exit 1
fi
echo "/etc/apache2 est resté en 0700"

echo
echo "--- après : ce qu'un utilisateur de projet peut voir ---"
for d in /etc/apache2 /etc/mysql /etc/ogs /etc/telegraf /etc/cron.d /etc/logrotate.d; do
    [ -d "$d" ] || continue
    if su -s /bin/sh shopfr -c "ls $d" >/dev/null 2>&1; then
        echo "ÉCHEC : $d reste lisible ($(stat -c %a "$d"))"
        exit 1
    fi
    echo "  $d : fermé ($(stat -c %a "$d") $(stat -c %U:%G "$d"))"
done
if su -s /bin/sh shopfr -c 'cat /etc/telegraf/telegraf.conf' 2>/dev/null | grep -q secret; then
    echo "ÉCHEC : le jeton de métriques reste lisible"
    exit 1
fi
echo "  le jeton de métriques n'est plus lisible"

echo
echo "--- et rien n'est cassé ---"
# Le site, servi par Apache dont le maître lit la configuration en root.
reponse=$(curl -s -H 'Host: shop.fr' http://127.0.0.1/)
echo "site : $reponse"
if [[ "$reponse" != *"ini=oui"* ]]; then
    echo "ÉCHEC : le site n'est plus servi, ou PHP a perdu son php.ini"
    tail -5 /home/shop.fr/logs/apache-error.log 2>/dev/null
    exit 1
fi

# Le PHP en ligne de commande, sous l'identité du projet : c'est lui qui lit
# /etc/php, et c'est pour lui que ce répertoire reste ouvert.
ini=$(su -s /bin/bash shopfr -c "/home/shop.fr/.bin/php -r 'echo php_ini_loaded_file();'" 2>&1)
echo "php.ini vu par le cron : ${ini:-aucun}"
if [ -z "$ini" ] || [[ "$ini" == *"rror"* ]]; then
    echo "ÉCHEC : le PHP du projet ne lit plus son php.ini"
    exit 1
fi

# Les clients de bases, sous l'identité du projet.
motdepasse=$(ogs secrets show shopfr mysql/shopfr | tail -1)
if ! su -s /bin/bash shopfr -c "mariadb -h 127.0.0.1 -u shopfr -p'$motdepasse' shopfr -e 'SELECT 1'" >/dev/null 2>&1; then
    echo "ÉCHEC : le client MySQL du projet ne se connecte plus"
    exit 1
fi
echo "client MySQL : connecté"
if ! su - postgres -c "psql -tAc 'SELECT 1'" >/dev/null 2>&1; then
    echo "ÉCHEC : PostgreSQL n'est plus administrable"
    exit 1
fi
echo "PostgreSQL : administrable"

# Le rechargement d'Apache, qui relit sa configuration désormais fermée.
if ! systemctl reload apache2 2>&1; then
    echo "ÉCHEC : Apache ne recharge plus sa configuration"
    exit 1
fi
apache2ctl configtest 2>&1 | tail -1

# cron, qui lit /etc/cron.d fermé.
journal=/home/shop.fr/logs/cron-horodate.log
: > "$journal"; chown shopfr:shopfr "$journal"
systemctl restart cron 2>/dev/null || service cron restart >/dev/null 2>&1
echo "attente de la minute suivante (jusqu'à 70 s)…"
for _ in $(seq 1 70); do [ -s "$journal" ] && break; sleep 1; done
if [ ! -s "$journal" ]; then
    echo "ÉCHEC : cron ne déclenche plus la tâche"
    exit 1
fi
echo "cron : a déclenché la tâche ($(tail -1 "$journal"))"

echo
echo "--- ogs lui-même fonctionne toujours ---"
ogs harden 2>&1 | tail -2
ogs doctor 2>&1 | grep -iE "droits de /etc"
ogs project list 2>&1 | tail -2

echo
echo "--- et l'utilisateur ne voit pas la carte du parc ---"
if su -s /bin/sh shopfr -c 'ls /etc/ogs/projects' >/dev/null 2>&1; then
    echo "ÉCHEC : les déclarations de projets sont lisibles"
    exit 1
fi
echo "  /etc/ogs/projects est hors de portée"

echo
echo "RÉSULTAT : les répertoires de configuration sont fermés aux utilisateurs de"
echo "           projet, sans qu'aucun service ni ogs lui-même n'en souffre ; un"
echo "           répertoire déjà plus strict n'est pas desserré."
