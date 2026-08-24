#!/bin/bash
# Éprouve le provider de bases sur un vrai MariaDB : création, privilèges,
# échappement des motifs de GRANT, idempotence, rotation, dump et suppression.
# Les tests unitaires vérifient les instructions produites ; ici on vérifie que
# le serveur les accepte et qu'elles ont l'effet annoncé.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : bases MariaDB ==="
# Le serveur est démarré à la main : pas d'init dans ce conteneur.
mkdir -p /run/mysqld && chown mysql:mysql /run/mysqld
mariadbd-safe --skip-syslog >/tmp/mariadb.log 2>&1 &
for _ in $(seq 1 30); do
    mariadb -e 'SELECT 1' >/dev/null 2>&1 && break
    sleep 1
done
if ! mariadb -e 'SELECT VERSION()' 2>/dev/null; then
    echo "ÉCHEC : le serveur n'a pas démarré"
    tail -5 /tmp/mariadb.log
    exit 1
fi

version=$(ls /etc/php | head -1)
ogs init --quiet
choisir_moteur apache

echo
echo "--- création d'un site avec sa base ---"
ogs site create shop.fr --kind php-plain --php "$version" --db mysql --tls=false --yes 2>&1 | tail -4

echo
echo "--- la base et l'utilisateur existent-ils vraiment ? ---"
mariadb -N -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name='shopfr'"
mariadb -N -e "SELECT user, host FROM mysql.user WHERE user='shopfr'"
if ! mariadb -N -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name='shopfr'" | grep -q shopfr; then
    echo "ÉCHEC : la base n'a pas été créée"
    exit 1
fi

echo
echo "--- privilèges accordés ---"
mariadb -N -e "SHOW GRANTS FOR 'shopfr'@'localhost'"
# Le privilège doit porter sur la base seule. Un motif non échappé donnerait
# « ON `shopfr`.* » là où « shop_fr » existe aussi : on le vérifie plus bas.
if mariadb -N -e "SHOW GRANTS FOR 'shopfr'@'localhost'" | grep -q "ALL PRIVILEGES ON \*\.\*"; then
    echo "ÉCHEC : privilèges accordés sur tout le serveur"
    exit 1
fi

echo
echo "--- l'utilisateur peut-il se connecter et écrire dans sa base ? ---"
motdepasse=$(ogs secrets show shopfr mysql/shopfr 2>/dev/null | tail -1)
if [ -z "$motdepasse" ]; then
    echo "ÉCHEC : le mot de passe n'a pas pu être relu du keystore"
    ogs secrets list shopfr 2>&1 | tail -5
    exit 1
fi
if ! mariadb -h 127.0.0.1 -u shopfr -p"$motdepasse" shopfr \
    -e "CREATE TABLE essai (id INT); INSERT INTO essai VALUES (1); SELECT COUNT(*) FROM essai;" 2>&1 | tail -1; then
    echo "ÉCHEC : l'utilisateur ne peut pas travailler dans sa base"
    exit 1
fi

echo
echo "--- l'échappement du motif de GRANT protège-t-il une base voisine ? ---"
# Le nom côté « ON » est un motif où « _ » est un joker. Une base additionnelle
# s'appelle « shopfr_stats » : sans échappement, le GRANT ouvrirait aussi
# « shopfrXstats ». On crée ce leurre, qui n'appartient à personne, puis on
# vérifie que l'utilisateur du projet ne peut pas l'atteindre.
mariadb -e "CREATE DATABASE \`shopfrXstats\`"
ogs db add shopfr --role stats --engine mysql >/dev/null
ogs apply shopfr --yes >/dev/null 2>&1
# Chaque base a son propre utilisateur : c'est celui de la base additionnelle
# qu'il faut éprouver, son privilège portant sur le motif « shopfr_stats ».
mariadb -N -e "SHOW GRANTS FOR 'shopfr_stats'@'localhost'" | grep -i stats
statspass=$(ogs secrets show shopfr mysql/shopfr_stats 2>/dev/null | tail -1)
if [ -z "$statspass" ]; then
    echo "ÉCHEC : mot de passe de la base additionnelle introuvable"
    ogs secrets list shopfr
    exit 1
fi
if ! mariadb -h 127.0.0.1 -u shopfr_stats -p"$statspass" -e "USE shopfr_stats" 2>/dev/null; then
    echo "ÉCHEC : l'utilisateur n'atteint pas sa propre base"
    exit 1
fi
if mariadb -h 127.0.0.1 -u shopfr_stats -p"$statspass" -e "USE shopfrXstats" 2>/dev/null; then
    echo "ÉCHEC : le motif n'est pas échappé — le joker « _ » ouvre le leurre shopfrXstats"
    exit 1
fi
echo "sa propre base est accessible, le leurre shopfrXstats ne l'est pas : le motif est échappé"

echo
echo "--- idempotence : rien ne doit plus être en écart ---"
sortie=$(ogs diff shopfr 2>&1)
echo "$sortie" | tail -4
if echo "$sortie" | grep -qi "base \|utilisateur de base\|privilèges"; then
    echo "ÉCHEC : le provider signale un écart alors que rien n'a changé"
    exit 1
fi

echo
echo "--- rotation du mot de passe ---"
ancien=$motdepasse
ogs db user rotate shopfr shopfr --yes 2>&1 | tail -2
nouveau=$(ogs secrets show shopfr mysql/shopfr 2>/dev/null | tail -1)
if [ "$nouveau" = "$ancien" ]; then
    echo "ÉCHEC : le mot de passe n'a pas changé"
    exit 1
fi
if mariadb -h 127.0.0.1 -u shopfr -p"$ancien" shopfr -e 'SELECT 1' >/dev/null 2>&1; then
    echo "ÉCHEC : l'ancien mot de passe fonctionne encore"
    exit 1
fi
if ! mariadb -h 127.0.0.1 -u shopfr -p"$nouveau" shopfr -e 'SELECT 1' >/dev/null 2>&1; then
    echo "ÉCHEC : le nouveau mot de passe ne fonctionne pas"
    exit 1
fi
echo "l'ancien mot de passe est révoqué, le nouveau fonctionne"

echo
echo "--- compte additionnel, et son sort à la suppression ---"
ogs db user add shopfr lecture --privileges readonly >/dev/null
ogs apply shopfr --yes >/dev/null 2>&1
lecturepass=$(ogs secrets show shopfr mysql/lecture 2>/dev/null | tail -1)
if [ -z "$lecturepass" ]; then
    echo "ÉCHEC : le compte additionnel n'a pas de mot de passe au keystore"
    exit 1
fi
if ! mariadb -h 127.0.0.1 -u lecture -p"$lecturepass" shopfr -e "SELECT count(*) FROM essai" >/dev/null 2>&1; then
    echo "ÉCHEC : le compte de lecture ne peut pas lire"
    exit 1
fi
if mariadb -h 127.0.0.1 -u lecture -p"$lecturepass" shopfr -e "INSERT INTO essai VALUES (2)" >/dev/null 2>&1; then
    echo "ÉCHEC : un compte « readonly » a pu écrire"
    exit 1
fi
echo "le compte lecture lit mais n'écrit pas"

echo
echo "--- dump ---"
ogs db dump shopfr shopfr 2>&1 | tail -2
# Le dump doit atterrir dans un emplacement défini, non dans le répertoire
# courant : il contient les données des clients.
if ! ls /var/lib/ogs/dumps/*.sql >/dev/null 2>&1; then
    echo "ÉCHEC : aucun dump dans /var/lib/ogs/dumps"
    ls -l / | head -5
    exit 1
fi
ls -l /var/lib/ogs/dumps/
if [ "$(stat -c %a /var/lib/ogs/dumps/*.sql)" != "600" ]; then
    echo "ÉCHEC : le dump est lisible au-delà de root"
    exit 1
fi
if ! grep -q "CREATE TABLE" /var/lib/ogs/dumps/*.sql; then
    echo "ÉCHEC : le dump ne contient pas la table créée plus haut"
    exit 1
fi
echo "le dump contient bien la table, en 0600"

echo
echo "--- suppression : les bases partent avec le projet ---"
ogs project delete shopfr --yes >/dev/null 2>&1
restantes=$(mariadb -N -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name IN ('shopfr','shopfr_stats')")
if [ -n "$restantes" ]; then
    echo "ÉCHEC : bases subsistantes après suppression : $restantes"
    exit 1
fi
# Le compte additionnel doit partir aussi : laissé derrière, il garderait la
# faculté de s'authentifier avec un mot de passe que le keystore ne conserve plus.
comptes=$(mariadb -N -e "SELECT user FROM mysql.user WHERE user IN ('shopfr','shopfr_stats','lecture')")
if [ -n "$comptes" ]; then
    echo "ÉCHEC : comptes subsistants : $(echo "$comptes" | tr '\n' ' ')"
    exit 1
fi
echo "bases et utilisateur supprimés ; le leurre, qui n'appartenait pas au projet, est intact :"
mariadb -N -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name='shopfrXstats'"

echo
echo "RÉSULTAT : le provider MySQL fonctionne sur un vrai serveur — privilèges"
echo "           bornés, motif échappé, rotation effective, suppression complète."
