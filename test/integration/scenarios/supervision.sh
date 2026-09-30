#!/bin/bash
# Éprouve la supervision du parc : le vhost d'état, les entrées de l'agent de
# métriques, les comptes de supervision et les prisons fail2ban.
#
# Ce que les tests unitaires ne peuvent pas voir, et qui est éprouvé ici :
#   — le vhost d'état répond vraiment, et le statut d'un pool traverse le
#     serveur web jusqu'à FPM ;
#   — fail2ban accepte le fichier de prisons, backend compris ;
#   — le compte de supervision MySQL peut lire les métriques et rien d'autre ;
#   — le rôle PostgreSQL s'authentifie « peer », donc sans mot de passe ;
#   — un journal disparu est vu par « doctor » avant qu'il n'empêche fail2ban de
#     démarrer.
set -uo pipefail
. /scenarios/lib.sh

echec() { echo "ÉCHEC : $*"; exit 1; }

echo "=== scénario : supervision ==="

# L'agent de métriques ne vient pas des dépôts Debian : ce qui compte ici est que
# le fragment soit écrit pour lui, avec les bons droits, et que ce qu'il désigne
# réponde. On pose donc son compte et son répertoire, sans l'agent lui-même.
groupadd -r telegraf 2>/dev/null
useradd -r -g telegraf -s /usr/sbin/nologin telegraf 2>/dev/null
mkdir -p /etc/telegraf/telegraf.d

mkdir -p /run/mysqld && chown mysql:mysql /run/mysqld
mariadbd-safe --skip-syslog >/tmp/mariadb.log 2>&1 &
for _ in $(seq 1 30); do
    mariadb -e 'SELECT 1' >/dev/null 2>&1 && break
    sleep 1
done
mariadb -e 'SELECT VERSION()' >/dev/null 2>&1 || echec "MariaDB n'a pas démarré"

pg_ctlcluster "$(ls /etc/postgresql | head -1)" main start >/dev/null 2>&1
for _ in $(seq 1 30); do
    su -s /bin/sh postgres -c 'psql -c "SELECT 1"' >/dev/null 2>&1 && break
    sleep 1
done
# ogs administre PostgreSQL sous un rôle au nom de l'utilisateur système (D44).
su -s /bin/sh postgres -c "createuser --superuser root" >/dev/null 2>&1

version=$(ls /etc/php | head -1)
ogs init --quiet
choisir_moteur apache

echo
echo "--- création d'un site avec base MySQL et PostgreSQL ---"
ogs site create shop.fr --kind php-plain --php "$version" --db mysql --tls=false --yes 2>&1 | tail -3
ogs db add shopfr --role stats --engine postgres >/dev/null
ogs apply shopfr --yes >/dev/null 2>&1

echo
echo "--- le fragment de l'agent est-il écrit, et pour lui seul ? ---"
fragment=/etc/telegraf/telegraf.d/ogs.conf
[ -f "$fragment" ] || echec "aucun fragment écrit dans $fragment"
mode=$(stat -c '%a %U:%G' "$fragment")
echo "  $fragment : $mode"
[ "$(stat -c %a "$fragment")" = "640" ] || echec "le fragment porte des mots de passe : 0640 attendu, obtenu $mode"
[ "$(stat -c %G "$fragment")" = "telegraf" ] || echec "groupe attendu telegraf, obtenu $(stat -c %G "$fragment")"
su -s /bin/sh telegraf -c "cat $fragment" >/dev/null 2>&1 || echec "l'agent ne peut pas lire son propre fragment"
if su -s /bin/sh shopfr -c "cat $fragment" >/dev/null 2>&1; then
    echec "un utilisateur de projet lit les mots de passe de supervision"
fi

echo
echo "--- le fragment est-il un TOML valide, et sans socket FPM ? ---"
python3 - "$fragment" <<'PY' || echec "le fragment n'est pas un TOML valide"
import sys, tomllib
with open(sys.argv[1], "rb") as f:
    conf = tomllib.load(f)
attendus = {"phpfpm", "procstat", "apache", "mysql", "postgresql"}
trouves = set(conf.get("inputs", {}))
manquants = attendus - trouves
if manquants:
    print("entrées manquantes :", ", ".join(sorted(manquants)))
    sys.exit(1)
if "outputs" in conf:
    print("le fragment ne doit décrire aucune destination")
    sys.exit(1)
print("entrées :", ", ".join(sorted(trouves)))
PY
# Parler au socket d'un pool, c'est pouvoir y faire exécuter du code sous
# l'identité du projet : l'agent ne doit jamais l'atteindre directement. Le
# socket de MySQL, lui, y figure légitimement — l'agent s'y authentifie.
grep -q '/run/php' "$fragment" && echec "un socket FPM figure dans le fragment"

echo
echo "--- le compte de supervision MySQL lit-il les métriques, et rien d'autre ? ---"
mariadb -N -e "SHOW GRANTS FOR 'telegraf'@'localhost'" || echec "le compte de supervision n'existe pas"
motdepasse=$(ogs secrets show '@parc' mysql/telegraf 2>/dev/null | tail -1)
[ -n "$motdepasse" ] || echec "le mot de passe de supervision n'est pas au keystore"
mariadb -h 127.0.0.1 -u telegraf -p"$motdepasse" -e "SHOW GLOBAL STATUS LIKE 'Threads_connected'" \
    >/dev/null 2>&1 || echec "le compte de supervision ne peut pas lire l'état du serveur"
mariadb -h 127.0.0.1 -u telegraf -p"$motdepasse" -e "SELECT count(*) FROM performance_schema.threads" \
    >/dev/null 2>&1 || echec "le compte de supervision ne lit pas performance_schema"
if mariadb -h 127.0.0.1 -u telegraf -p"$motdepasse" -e "USE shopfr" >/dev/null 2>&1; then
    echec "le compte de supervision atteint la base d'un projet"
fi
echo "  il lit l'état du serveur, pas les données des projets"

echo
echo "--- le rôle PostgreSQL s'authentifie-t-il sans mot de passe ? ---"
su -s /bin/sh postgres -c "psql -tAc \"SELECT rolname FROM pg_roles WHERE rolname='telegraf'\"" | grep -q telegraf \
    || echec "le rôle de supervision n'a pas été créé"
su -s /bin/sh postgres -c "psql -tAc \"SELECT pg_has_role('telegraf','pg_monitor','member')\"" | grep -q t \
    || echec "le rôle de supervision n'est pas membre de pg_monitor"
# « peer » compare l'utilisateur système : le rôle porte donc son nom.
su -s /bin/sh telegraf -c "psql -d postgres -tAc 'SELECT count(*) FROM pg_stat_activity'" >/dev/null 2>&1 \
    || echec "le rôle de supervision ne se connecte pas par le socket local"
echo "  connexion locale « peer », sans mot de passe, avec pg_monitor"

echo
echo "--- le vhost d'état répond-il, et seulement en local ? ---"
systemctl start "php${version}-fpm" >/dev/null 2>&1
apache2ctl start >/dev/null 2>&1 || apache2ctl restart >/dev/null 2>&1
sleep 2
etat=$(curl -sf "http://127.0.0.1:8099/ogs-status/apache?auto" || true)
[ -n "$etat" ] || { apache2ctl -t; tail -20 /var/log/apache2/error.log; echec "le vhost d'état ne répond pas"; }
echo "$etat" | grep -qi "Total Accesses" || echec "mod_status ne rend pas le format machine"
fpm=$(curl -sf "http://127.0.0.1:8099/ogs-status/fpm/shopfr" || true)
[ -n "$fpm" ] || echec "le statut du pool ne traverse pas le serveur web jusqu'à FPM"
echo "$fpm" | grep -qi "pool:" || echec "la réponse du pool n'est pas un statut FPM : $fpm"
echo "  état du serveur et du pool obtenus par la boucle locale"
# Le vhost ne doit rien servir d'autre.
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8099/") || true
[ "$code" = "403" ] || [ "$code" = "404" ] || echec "le vhost d'état sert autre chose que l'état (code $code)"

echo
echo "--- et sous nginx, l'autre moteur de l'image ? ---"
# Les deux moteurs n'exposent pas leur état de la même façon — stub_status contre
# mod_status — et ne transmettent pas SCRIPT_NAME à FPM de la même manière : le
# vhost d'état doit être éprouvé sur les deux.
apache2ctl stop >/dev/null 2>&1
choisir_moteur nginx
ogs apply --yes >/dev/null 2>&1
nginx -t >/dev/null 2>&1 || echec "le vhost d'état rend la configuration nginx invalide"
nginx 2>/dev/null || nginx -s reload
sleep 1
curl -sf "http://127.0.0.1:8099/ogs-status/nginx" | grep -qi "Active connections" \
    || echec "stub_status ne répond pas"
curl -sf "http://127.0.0.1:8099/ogs-status/fpm/shopfr" | grep -qi "pool:" \
    || echec "le statut du pool ne traverse pas nginx jusqu'à FPM"
echo "  état du serveur et du pool obtenus sous nginx aussi"
nginx -s quit 2>/dev/null
choisir_moteur apache
ogs apply --yes >/dev/null 2>&1
apache2ctl start >/dev/null 2>&1

echo
echo "--- fail2ban accepte-t-il le fichier de prisons ? ---"
jails=/etc/fail2ban/jail.d/ogs.conf
[ -f "$jails" ] || echec "aucun fichier de prisons écrit"
grep -c '^\[ogs-' "$jails" | xargs echo "  prisons générées :"
# Le contrôle qui compte : une prison refusée empêche le démon de démarrer, et
# toutes les autres tombent avec elle.
fail2ban-client -t 2>&1 | tail -3
fail2ban-client -t >/dev/null 2>&1 || echec "fail2ban refuse la configuration produite"
# Aucune prison d'ogs ne doit recouvrir celle de l'administrateur.
grep -q '^\[sshd\]' "$jails" && echec "une prison non préfixée recouvrirait celle de l'administrateur"

echo
echo "--- un journal disparu est-il vu avant de bloquer le démon ? ---"
journal=$(grep -o '/var/log/ogs-projects/[^ ]*\.log' "$jails" | head -1)
[ -n "$journal" ] || echec "aucune prison ne surveille le journal d'un projet"
mv "$journal" "$journal.deplace"
sortie=$(ogs doctor 2>&1)
echo "$sortie" | grep -i "prisons fail2ban" || true
echo "$sortie" | grep -qi "journal.*absent" || echec "doctor ne signale pas le journal disparu"
# Et fail2ban confirme le diagnostic : la configuration est bien refusée.
if fail2ban-client -t >/dev/null 2>&1; then
    echo "  note : fail2ban tolère ce chemin absent sur cette version"
else
    echo "  fail2ban refuse effectivement la configuration : le diagnostic était juste"
fi
mv "$journal.deplace" "$journal"
# Un apply recalcule le fichier depuis les journaux réellement présents.
ogs apply --yes >/dev/null 2>&1
ogs doctor 2>&1 | grep -i "prisons fail2ban" | head -1

echo
echo "--- la suppression d'un projet retire-t-elle ses entrées ? ---"
ogs project delete shopfr --yes >/dev/null 2>&1
grep -q shopfr "$fragment" && echec "le projet supprimé subsiste dans les entrées de l'agent"
grep -q shopfr /etc/apache2/sites-available/ogs-status.conf \
    && echec "le vhost d'état proxifie encore un pool disparu"
grep -q "shop.fr" "$jails" && echec "une prison désigne encore le journal d'un projet supprimé"
fail2ban-client -t >/dev/null 2>&1 || echec "la configuration de fail2ban est cassée après la suppression"
apache2ctl -t 2>&1 | tail -1
apache2ctl -t >/dev/null 2>&1 || echec "le vhost d'état est invalide après la suppression"
echo "  fichiers de parc recalculés, configurations toujours valides"

echo
echo "--- un compte homonyme de l'administrateur est-il laissé tel quel ? (D93) ---"
# L'administrateur collecte déjà sous « collecte » ; ogs, réglé sur ce nom, lui
# posait un mot de passe neuf par ALTER USER et cassait sa collecte.
mariadb -e "CREATE USER 'collecte'@'localhost' IDENTIFIED BY 'celui-de-l-administrateur';
            GRANT PROCESS ON *.* TO 'collecte'@'localhost'" || echec "compte de l'administrateur non créé"
cp /etc/ogs/config.yaml /tmp/config.yaml.avant
python3 - <<'PY'
chemin = "/etc/ogs/config.yaml"
s = open(chemin).read()
assert "  telegraf: {}\n" in s, "réglage telegraf inattendu"
s = s.replace("  telegraf: {}\n", "  telegraf:\n    account: collecte\n", 1)
open(chemin, "w").write(s)
PY
sortie=$(ogs apply --yes 2>&1)
code=$?
echo "$sortie" | grep -i "collecte" | head -2
[ "$code" = 6 ] || echec "le refus doit rendre le code 6 (code $code)"
echo "$sortie" | grep -q "sans avoir été créé par ogs" || echec "le refus n'est pas annoncé"
mariadb -h 127.0.0.1 -u collecte -p'celui-de-l-administrateur' -e "SELECT 1" >/dev/null 2>&1 \
    || echec "le mot de passe du compte de l'administrateur a changé"
mariadb -N -e "SHOW GRANTS FOR 'collecte'@'localhost'" | grep -qi performance_schema \
    && echec "les droits du compte de l'administrateur ont changé"
grep -q "collecte" "$fragment" && echec "le fragment porte un compte qu'ogs n'a pas posé"
cp /tmp/config.yaml.avant /etc/ogs/config.yaml
echo "  mot de passe, droits et fragment intacts"

echo
echo "RÉSULTAT : la supervision décrit un parc réel — état servi en local, comptes"
echo "           de métriques bornés, prisons acceptées par fail2ban, et rien qui"
echo "           survive à la suppression d'un projet."
