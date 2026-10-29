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

version=$(version_php)
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
echo "--- le compte a-t-il sa limite de connexions (D127) ? ---"
limite=$(mariadb -N -e "SELECT max_user_connections FROM mysql.user WHERE user='shopfr' AND host='localhost'")
echo "  limite calculée : $limite"
if [ -z "$limite" ] || [ "$limite" -lt 10 ]; then
    echo "ÉCHEC : le compte doit recevoir une limite calculée depuis la déclaration (au moins 10)"
    exit 1
fi
# La surcharge de la déclaration l'emporte, et le serveur l'applique vraiment.
sed -i "/- engine: mysql/,/user:/ s/^\( *\)user: shopfr$/&\n\1maxConnections: 2/" /etc/ogs/projects/shopfr.yaml
ogs apply shopfr --yes 2>&1 | grep -i "connexions" | sed 's/^/  /'
if [ "$(mariadb -N -e "SELECT max_user_connections FROM mysql.user WHERE user='shopfr' AND host='localhost'")" != 2 ]; then
    echo "ÉCHEC : maxConnections: 2 n'a pas été posé"
    exit 1
fi
# Seuls ces deux clients sont attendus : un « wait » nu attendrait aussi
# mariadbd-safe, lancé en tâche de fond plus haut, et ne rendrait jamais la main.
clients=()
for _ in 1 2; do
    mariadb -h 127.0.0.1 -u shopfr -p"$motdepasse" shopfr -e 'SELECT SLEEP(4)' >/dev/null 2>&1 &
    clients+=($!)
done
sleep 1
if refus=$(mariadb -h 127.0.0.1 -u shopfr -p"$motdepasse" shopfr -e 'SELECT 1' 2>&1); then
    echo "ÉCHEC : une troisième connexion simultanée a été acceptée"
    wait "${clients[@]}"
    exit 1
fi
wait "${clients[@]}"
echo "  troisième connexion refusée : $refus"
sed -i '/^ *maxConnections: 2$/d' /etc/ogs/projects/shopfr.yaml
ogs apply shopfr --yes >/dev/null 2>&1
if [ "$(mariadb -N -e "SELECT max_user_connections FROM mysql.user WHERE user='shopfr' AND host='localhost'")" != "$limite" ]; then
    echo "ÉCHEC : sans surcharge, la limite calculée doit revenir"
    exit 1
fi
echo "limite calculée posée, surcharge appliquée par le serveur, calcul rétabli"
# doctor confronte la somme des limites à max_connections, lu sur le serveur.
capacite=$(mariadb -N -e 'SELECT @@max_connections')
ligne=$(ogs doctor 2>&1 | grep "connexions mysql")
echo "  $ligne"
if [[ "$ligne" != *"capacité de $capacite"* ]]; then
    echo "ÉCHEC : doctor doit confronter la somme des limites à max_connections ($capacite)"
    exit 1
fi

echo
echo "--- clone : la préproduction ne reçoit pas le mot de passe de la production ---"
# La configuration de l'application, écrite depuis « ogs secrets env » comme le
# guide l'indique, était recopiée telle quelle : la préproduction écrivait dans
# la base de la production. Un php-plain n'a pas de fichier de configuration
# connu : ogs y masque le mot de passe qu'il a généré, où qu'il soit. Le mode
# simulé ne voit ni le compte réel ni la base : seul ce scénario éprouve que ce
# qui reste ne s'authentifie plus.
www=/home/shop.fr/www
ogs secrets env shopfr 2>/dev/null | grep '^DATABASE_URL=' > "$www/.env"
printf "<?php return ['password' => '%s'];\n" "$motdepasse" > "$www/db.php"
chown shopfr:shopfr "$www/.env" "$www/db.php"
sortie=$(ogs site clone shopfr --to prep --yes 2>&1)
code=$?
prep=$(getent passwd shopfrprep | cut -d: -f6)
if [ "$code" != 6 ] || [[ "$sortie" != *"secret retiré de la copie"*"db.php"* ]] || [ ! -f "$prep/www/db.php" ]; then
    echo "ÉCHEC : le clone n'a pas dit le secret retiré (code $code)"
    echo "$sortie" | tail -12 | sed 's/^/  /'
    exit 1
fi
# La sortie dit ce qu'ogs n'a pas retiré : le reste de www, et la base copiée —
# que seul un vrai moteur recopie.
if [[ "$sortie" != *"shopfrprep reprend tel quel de shopfr"* ]] || \
    [[ "$sortie" != *"copie de celle de shopfr"* ]] || [[ "$sortie" == *"ni les identifiants ni les secrets"* ]]; then
    echo "ÉCHEC : le clone n'a pas dit ce qu'il reprend tel quel"
    echo "$sortie" | tail -16 | sed 's/^/  /'
    exit 1
fi
if grep -rqF -- "$motdepasse" "$prep/www"; then
    echo "ÉCHEC : le mot de passe de la production a été recopié dans la préproduction"
    grep -rlF -- "$motdepasse" "$prep/www" | sed 's/^/  /'
    exit 1
fi
masque=$(sed -n "s/.*'password' => '\(.*\)'.*/\1/p" "$prep/www/db.php")
if [ "${#masque}" != "${#motdepasse}" ] || \
    mariadb -h 127.0.0.1 -u shopfr -p"$masque" shopfr -e 'SELECT 1' >/dev/null 2>&1; then
    echo "ÉCHEC : le masque n'a pas la longueur du secret, ou il ouvre la base de la production"
    exit 1
fi
preppass=$(ogs secrets show shopfrprep mysql/shopfrprep 2>/dev/null | tail -1)
if ! mariadb -h 127.0.0.1 -u shopfrprep -p"$preppass" shopfrprep -e 'SELECT COUNT(*) FROM essai' >/dev/null 2>&1; then
    echo "ÉCHEC : la préproduction n'atteint pas sa propre base, données copiées comprises"
    exit 1
fi
ogs project delete shopfrprep --yes >/dev/null 2>&1
rm -f "$www/.env" "$www/db.php"
echo "clone : mot de passe de la production masqué, la préproduction travaille sur sa propre base"

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
echo "--- un compte additionnel retiré de la déclaration part au prochain apply (D100) ---"
ogs db user add shopfr ancien --privileges readonly >/dev/null
ogs apply shopfr --yes >/dev/null 2>&1
if [ -z "$(mariadb -N -e "SELECT user FROM mysql.user WHERE user='ancien'")" ]; then
    echo "ÉCHEC : le compte ancien n'a pas été créé"
    exit 1
fi
sed -i '/- user: ancien$/,/privileges:/d' /etc/ogs/projects/shopfr.yaml
if grep -q ancien /etc/ogs/projects/shopfr.yaml; then
    echo "ÉCHEC : le compte ancien est encore déclaré"
    exit 1
fi
ogs plan shopfr 2>&1 | grep -E "ancien" | head -3
if ! ogs apply shopfr --yes >/tmp/ancien.log 2>&1; then
    echo "ÉCHEC : l'apply qui retire le compte a échoué"
    cat /tmp/ancien.log
    exit 1
fi
if [ -n "$(mariadb -N -e "SELECT user FROM mysql.user WHERE user='ancien'")" ]; then
    echo "ÉCHEC : le compte retiré de la déclaration subsiste"
    exit 1
fi
if ogs secrets list shopfr 2>/dev/null | grep -q "mysql/ancien"; then
    echo "ÉCHEC : le mot de passe du compte retiré est resté au keystore"
    exit 1
fi
if ! mariadb -h 127.0.0.1 -u lecture -p"$lecturepass" shopfr -e "SELECT count(*) FROM essai" >/dev/null 2>&1; then
    echo "ÉCHEC : le compte lecture, toujours déclaré, a perdu l'accès"
    exit 1
fi
echo "le compte ancien est retiré avec son mot de passe ; lecture reste"

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
echo "--- restauration : sous le compte du projet, jamais sous root (D92) ---"
# Une vue posée par root : sa clause DEFINER nomme un compte que le projet ne
# peut pas désigner. La restauration la retire, et la vue revient au compte qui
# restaure.
mariadb shopfr -e "CREATE DEFINER='root'@'localhost' VIEW vue_essai AS SELECT id FROM essai"
# Une donnée qui ressemble à une clause DEFINER : mariadb-dump écrit chaque
# tuple sur sa ligne, et la restauration ne doit pas y toucher.
mariadb shopfr -e "CREATE TABLE notes (t TEXT); INSERT INTO notes VALUES (CONCAT('ligne1', CHAR(10), 'DEFINER=\`root\`@\`localhost\` x'))"
note_avant=$(mariadb -N shopfr -e "SELECT HEX(t) FROM notes")
ogs db dump shopfr shopfr -O /root/shopfr-vue.sql >/dev/null 2>&1 || { echo "ÉCHEC : dump"; exit 1; }
mariadb shopfr -e "DROP VIEW vue_essai; DROP TABLE essai; DROP TABLE notes"
if ! ogs db restore shopfr shopfr --input /root/shopfr-vue.sql --yes >/tmp/restore.log 2>&1; then
    cat /tmp/restore.log
    echo "ÉCHEC : la restauration sous le compte du projet a échoué"
    exit 1
fi
if [ "$(mariadb -N shopfr -e 'SELECT COUNT(*) FROM essai')" != "1" ]; then
    echo "ÉCHEC : la restauration n'a pas rétabli la table"
    exit 1
fi
if [ "$(mariadb -N shopfr -e 'SELECT HEX(t) FROM notes')" != "$note_avant" ]; then
    echo "ÉCHEC : la restauration a altéré une donnée qui contenait « DEFINER= »"
    mariadb -N shopfr -e 'SELECT t FROM notes'
    exit 1
fi
echo "la donnée qui contient « DEFINER= » est restaurée intacte"
definisseur=$(mariadb -N -e "SELECT DEFINER FROM information_schema.VIEWS WHERE TABLE_SCHEMA='shopfr' AND TABLE_NAME='vue_essai'")
echo "définisseur de la vue restaurée : $definisseur"
if [ "$definisseur" != "shopfr@localhost" ]; then
    echo "ÉCHEC : la vue restaurée doit revenir au compte du projet"
    exit 1
fi
# Un script hostile : une commande shell, et une base hors du projet.
rm -f /tmp/restore-identite
printf '%s\n' '\! id -un > /tmp/restore-identite' 'CREATE DATABASE pirate;' > /root/hostile.sql
if ogs db restore shopfr shopfr --input /root/hostile.sql --yes >/tmp/restore.log 2>&1; then
    echo "ÉCHEC : une restauration qui crée une base hors du projet doit échouer"
    exit 1
fi
if mariadb -N -e "SHOW DATABASES LIKE 'pirate'" | grep -q pirate; then
    echo "ÉCHEC : la restauration a créé une base hors du projet"
    exit 1
fi
if [ -e /tmp/restore-identite ] && [ "$(cat /tmp/restore-identite)" != "shopfr" ]; then
    echo "ÉCHEC : « \\! » s'exécute sous $(cat /tmp/restore-identite), non sous l'utilisateur du projet"
    exit 1
fi
echo "restauration confinée : base hors projet refusée, commande shell sous $(cat /tmp/restore-identite 2>/dev/null || echo personne)"
rm -f /root/hostile.sql /root/shopfr-vue.sql /tmp/restore-identite

echo
echo "--- suppression : les bases partent avec le projet ---"
# « lecture » est retiré de la déclaration sans apply : la suppression du projet
# doit l'emporter quand même (D100) — elle ne visait que les comptes déclarés.
sed -i '/- user: lecture$/,/privileges:/d' /etc/ogs/projects/shopfr.yaml
if grep -q "user: lecture" /etc/ogs/projects/shopfr.yaml; then
    echo "ÉCHEC : lecture encore déclaré"
    exit 1
fi
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
echo "--- une base et un compte homonymes préexistants ne sont ni repris ni supprimés (D91) ---"
# Le site resté hors d'ogs porte les noms qu'ogs dérive du host : la création les
# reprenait sans un mot — GRANT au compte trouvé, mot de passe scellé qu'il
# n'avait jamais posé —, et « project delete » les emportait, DROP DATABASE et
# DROP USER compris.
mariadb -e "CREATE DATABASE blogfr; CREATE USER 'blogfr'@'localhost' IDENTIFIED BY 'ancien';
            GRANT SELECT ON blogfr.* TO 'blogfr'@'localhost';
            CREATE TABLE blogfr.billets (id INT); INSERT INTO blogfr.billets VALUES (1)"
ogs site create blog.fr --kind static --db mysql --tls=false --yes >/tmp/blog.log 2>&1
code=$?
if [ "$code" != 6 ] || ! grep -q "sans qu'ogs l'ait créé" /tmp/blog.log || ! grep -q "renommer" /tmp/blog.log; then
    echo "ÉCHEC : la reprise de l'homonyme n'est pas refusée (code $code)"
    tail -8 /tmp/blog.log | sed 's/^/  /'
    exit 1
fi
if mariadb -N -e "SHOW GRANTS FOR 'blogfr'@'localhost'" | grep -q "ALL PRIVILEGES"; then
    echo "ÉCHEC : le compte préexistant a reçu les droits du projet"
    exit 1
fi
if ogs secrets list blogfr 2>/dev/null | grep -q "mysql/blogfr"; then
    echo "ÉCHEC : un mot de passe a été scellé pour un compte qu'ogs n'a pas créé"
    exit 1
fi
ogs project delete blogfr --yes >/tmp/blog.log 2>&1
if [ "$(mariadb -N -e "SELECT COUNT(*) FROM blogfr.billets" 2>/dev/null)" != 1 ] || \
    ! mariadb -h 127.0.0.1 -u blogfr -pancien blogfr -e 'SELECT 1' >/dev/null 2>&1; then
    echo "ÉCHEC : la suppression du projet a emporté la base ou le compte d'un autre"
    tail -8 /tmp/blog.log | sed 's/^/  /'
    exit 1
fi
if ! grep -q "conservé" /tmp/blog.log; then
    echo "ÉCHEC : la suppression ne dit pas ce qu'elle laisse"
    tail -8 /tmp/blog.log | sed 's/^/  /'
    exit 1
fi
echo "l'homonyme est refusé à la création, laissé intact à la suppression, et nommé"

echo
echo "--- les comptes d'administration ne sont pas déclarables (D91) ---"
ogs site create admin.fr --kind static --db mysql --tls=false --yes >/dev/null 2>&1
for compte in root mysql telegraf; do
    ogs db user add adminfr "$compte" >/tmp/admin.log 2>&1
    code=$?
    if [ "$code" != 3 ]; then
        echo "ÉCHEC : « db user add adminfr $compte » accepté (code $code)"
        tail -3 /tmp/admin.log | sed 's/^/  /'
        exit 1
    fi
done
ogs project delete adminfr --yes >/dev/null 2>&1
if ! mariadb -e 'SELECT 1' >/dev/null 2>&1; then
    echo "ÉCHEC : root n'administre plus MariaDB par le socket"
    exit 1
fi
echo "root, mysql et telegraf sont refusés ; root administre toujours le moteur par le socket"

echo
echo "--- un compte existant sous un autre hôte n'est pas masqué ---"
# Le cas d'un serveur repris : 'vitrinefr'@'%', dont l'application connaît le mot
# de passe. MySQL retient pour chaque connexion le compte à l'hôte le plus
# précis : un 'vitrinefr'@'localhost' créé à côté ferait refuser ce mot de passe
# par le socket comme par 127.0.0.1.
mariadb -e "CREATE DATABASE vitrinefr; CREATE USER 'vitrinefr'@'%' IDENTIFIED BY 'ancien';
            GRANT ALL PRIVILEGES ON vitrinefr.* TO 'vitrinefr'@'%'"
ogs site create vitrine.fr --kind static --db mysql --tls=false --yes >/tmp/vitrine.log 2>&1
# Hors adoption, la base préexistante est refusée (D91) ; un serveur repris l'est
# par « ogs import », qui marque le projet adopté — ce que la déclaration fait ici.
if ! grep -q "sans qu'ogs l'ait créé" /tmp/vitrine.log; then
    echo "ÉCHEC : la base préexistante d'un projet non adopté n'est pas refusée"
    tail -5 /tmp/vitrine.log
    exit 1
fi
sed -i 's/^metadata:$/metadata:\n  labels:\n    adopted: "true"/' /etc/ogs/projects/vitrinefr.yaml
ogs apply vitrinefr --yes >/tmp/vitrine.log 2>&1
if mariadb -N -e "SELECT host FROM mysql.user WHERE user='vitrinefr'" | grep -qx localhost; then
    echo "ÉCHEC : 'vitrinefr'@'localhost' a été créé à côté de 'vitrinefr'@'%'"
    exit 1
fi
if ! grep -q "existe déjà sous un autre hôte" /tmp/vitrine.log; then
    echo "ÉCHEC : le refus de créer l'homonyme n'est pas annoncé"
    tail -5 /tmp/vitrine.log
    exit 1
fi
# Déclaré sous son hôte réel — ce que fait désormais l'import —, le compte est
# reconnu : ni second compte, ni mot de passe posé, ni écart au diff suivant.
sed -i "s/^\( *\)host: localhost$/\1host: '%'/" /etc/ogs/projects/vitrinefr.yaml
ogs apply vitrinefr --yes >/dev/null 2>&1
if mariadb -N -e "SELECT host FROM mysql.user WHERE user='vitrinefr'" | grep -qx localhost; then
    echo "ÉCHEC : l'hôte déclaré n'est pas celui que l'apply emploie"
    exit 1
fi
if ! mariadb -h 127.0.0.1 -u vitrinefr -pancien vitrinefr -e 'SELECT 1' >/dev/null 2>&1; then
    echo "ÉCHEC : l'application a perdu l'accès à sa base"
    exit 1
fi
if ogs diff vitrinefr 2>&1 | grep -qi "base mysql vitrinefr"; then
    echo "ÉCHEC : le compte relu sous '%' reste en écart"
    ogs diff vitrinefr 2>&1 | tail -4
    exit 1
fi
echo "'vitrinefr'@'%' est reconnu sous son hôte réel, et son mot de passe fonctionne toujours"

echo
echo "--- un GRANT à joker est remplacé par le GRANT échappé (R1-47, D111) ---"
# Le GRANT d'un serveur repris est souvent écrit sans échappement : « vitrine_fr »,
# où le souligné est un joker. Relu sous le même nom que le GRANT échappé d'ogs, il
# l'écrasait ou le masquait, et n'était jamais retiré.
mariadb -e "CREATE DATABASE vitrine_fr; GRANT ALL PRIVILEGES ON vitrine_fr.* TO 'vitrinefr'@'%';
            CREATE TABLE vitrine_fr.pages (id INT); INSERT INTO vitrine_fr.pages VALUES (1)"
sed -i "/- engine: mysql/,/user:/ s/name: vitrinefr$/name: vitrine_fr/" /etc/ogs/projects/vitrinefr.yaml
grep -q "name: vitrine_fr" /etc/ogs/projects/vitrinefr.yaml || { echo "ÉCHEC : déclaration non modifiée"; exit 1; }
ogs plan vitrinefr 2>&1 | grep -i "joker" | head -2
ogs apply vitrinefr --yes >/tmp/vitrine.log 2>&1
grants=$(mariadb -N -e "SHOW GRANTS FOR 'vitrinefr'@'%'")
echo "$grants" | grep -i vitrine | sed 's/^/  /'
if echo "$grants" | grep -q '`vitrine_fr`'; then
    echo "ÉCHEC : le GRANT à joker n'a pas été retiré"
    tail -5 /tmp/vitrine.log
    exit 1
fi
# mariadb -N échappe la contre-oblique de sa sortie : une ou deux selon le client.
if ! echo "$grants" | grep -Eq '`vitrine\\+_fr`'; then
    echo "ÉCHEC : le GRANT échappé n'a pas été posé"
    exit 1
fi
if ! mariadb -h 127.0.0.1 -u vitrinefr -pancien vitrine_fr -e 'SELECT * FROM pages' >/dev/null 2>&1; then
    echo "ÉCHEC : l'application a perdu l'accès à sa base"
    exit 1
fi
ecart=$(ogs diff vitrinefr 2>&1)
if echo "$ecart" | grep -qi "base mysql vitrine_fr"; then
    echo "ÉCHEC : la base reste en écart après le retrait du joker"
    ogs diff vitrinefr 2>&1 | tail -4
    exit 1
fi
# R2-21 : l'étape a tourné sur un compte adopté dont ogs ne tient pas le mot de
# passe — elle n'en scelle aucun, puisqu'elle n'en pose aucun.
if ogs secrets list vitrinefr 2>/dev/null | grep -q "mysql/vitrinefr"; then
    echo "ÉCHEC : un mot de passe jamais posé a été scellé pour le compte adopté"
    exit 1
fi
echo "joker retiré, GRANT échappé en place, l'ancien mot de passe sert toujours, rien de scellé"

echo
echo "--- un joker posé WITH GRANT OPTION part entier (D111) ---"
# SCHEMA_PRIVILEGES ne liste jamais GRANT OPTION, seulement IS_GRANTABLE : le
# REVOKE des seuls privilèges relus laissait la ligne, relue « USAGE », et le plan
# repassait sans fin.
mariadb -e "GRANT SELECT ON vitrine_fr.* TO 'vitrinefr'@'%' WITH GRANT OPTION"
mariadb -N -e "SHOW GRANTS FOR 'vitrinefr'@'%'" | grep -q '`vitrine_fr`' || { echo "ÉCHEC : joker non posé"; exit 1; }
ogs apply vitrinefr --yes >/tmp/vitrine.log 2>&1
code=$?
if [ "$code" != 0 ]; then
    echo "ÉCHEC : apply en code $code, 0 attendu"
    tail -6 /tmp/vitrine.log
    exit 1
fi
if mariadb -N -e "SHOW GRANTS FOR 'vitrinefr'@'%'" | grep -q '`vitrine_fr`'; then
    echo "ÉCHEC : la ligne à joker WITH GRANT OPTION est restée"
    mariadb -N -e "SHOW GRANTS FOR 'vitrinefr'@'%'"
    exit 1
fi
ecart=$(ogs diff vitrinefr 2>&1)
if [ $? != 0 ]; then
    echo "ÉCHEC : le plan suivant n'est pas conforme"
    echo "$ecart" | tail -4
    exit 1
fi
echo "joker WITH GRANT OPTION retiré, plan suivant conforme"

echo
echo "--- restauration d'un projet adopté : compte recréé, mot de passe neuf annoncé (R1-60, D111) ---"
ogs project delete vitrinefr --force --yes >/tmp/vitrine.log 2>&1 || { tail -5 /tmp/vitrine.log; echo "ÉCHEC : suppression"; exit 1; }
if mariadb -N -e "SELECT user FROM mysql.user WHERE user='vitrinefr'" | grep -q vitrinefr; then
    echo "ÉCHEC : le compte adopté devait partir sous --force"
    exit 1
fi
archive=$(ls -t /var/lib/ogs/archives/vitrinefr-*.tar.zst | head -1)
ogs project restore "$archive" --yes >/tmp/vitrine.log 2>&1
code=$?
if [ "$code" != 6 ] || ! grep -q "ogs secrets show vitrinefr mysql/vitrinefr" /tmp/vitrine.log; then
    echo "ÉCHEC : le mot de passe neuf doit être annoncé (code $code, 6 attendu)"
    tail -8 /tmp/vitrine.log | sed 's/^/  /'
    exit 1
fi
neuf=$(ogs secrets show vitrinefr mysql/vitrinefr | tail -1)
if [ "$(mariadb -h 127.0.0.1 -u vitrinefr -p"$neuf" vitrine_fr -N -e 'SELECT COUNT(*) FROM pages' 2>/dev/null)" != 1 ]; then
    echo "ÉCHEC : le mot de passe scellé n'est pas celui du compte recréé, ou la base n'est pas restaurée"
    exit 1
fi
grep "secrets show" /tmp/vitrine.log | head -1 | sed 's/^/  /'
echo "compte recréé, mot de passe neuf scellé, posé et annoncé ; la base est restaurée"

echo
echo "RÉSULTAT : le provider MySQL fonctionne sur un vrai serveur — privilèges"
echo "           bornés, motif échappé, rotation effective, suppression complète."
