#!/bin/bash
# Éprouve les groupes de projets (D135) en root, sous un vrai systemd, avec un
# vrai MariaDB et les deux moteurs web.
#
# Ce que les tests unitaires ne peuvent pas voir — ils tournent sans privilèges,
# et le mode préfixé ne pose aucun propriétaire :
#   — le répertoire du groupe est root:root 0701, et www-data, qui n'est pas
#     membre de root, le traverse : les sites du groupe sont servis, sous Apache
#     comme sous nginx, dont disable_symlinks ouvre chaque composant du chemin ;
#   — la base est préfixée, le compte ne l'est pas, et ce compte s'y connecte ;
#   — l'apply rétablit un mode ou un groupe retouchés, harden n'y touche pas, et
#     un répertoire de groupe tenu par un autre compte fait refuser la création ;
#   — la suppression enchaîne userdel, l'effacement du home et le retrait du
#     répertoire du groupe, qui n'appartient pas au compte supprimé ;
#   — la restauration recrée le répertoire, la base préfixée et le site.
set -uo pipefail
. /scenarios/lib.sh

echec() { echo "ÉCHEC : $*"; exit 1; }
regime() { stat -c '%U:%G %a' "$1" 2>/dev/null; }
archive_de() { ls -t /var/lib/ogs/archives/"$1"-*.tar.zst 2>/dev/null | head -1; }
# sert vérifie que host rend contenu sur le port 80, en laissant à un
# rechargement le temps d'aboutir.
sert() {
    local host=$1 chemin=$2 contenu=$3 obtenu=""
    for _ in $(seq 1 10); do
        obtenu=$(curl -s -H "Host: $host" "http://127.0.0.1$chemin")
        [ "$obtenu" = "$contenu" ] && return 0
        sleep 1
    done
    echo "  $host$chemin a rendu : $(head -c 200 <<<"$obtenu")"
    return 1
}
# deposer écrit un fichier dans le docroot d'un projet, au compte du projet.
deposer() {
    local id=$1 rel=$2 contenu=$3 www
    www=$(getent passwd "$id" | cut -d: -f6)/www
    printf '%s' "$contenu" >"$www/$rel"
    chown "$id:$id" "$www/$rel"
}

echo "=== scénario : groupes de projets ==="
version=$(version_php)
systemctl start mariadb >/dev/null 2>&1
for _ in $(seq 1 30); do
    mariadb -e 'SELECT 1' >/dev/null 2>&1 && break
    sleep 1
done
mariadb -e 'SELECT 1' >/dev/null 2>&1 || echec "MariaDB n'a pas démarré"
systemctl start "php$version-fpm" >/dev/null 2>&1
ogs init --quiet
choisir_moteur apache

echo
echo "--- deux sites du groupe acme, sous Apache ---"
sortie=$(ogs site create shop.test --kind php-plain --php "$version" --db mysql --group acme \
    --tls=false --yes 2>&1) || { echo "$sortie" | tail -8; echec "création de shop.test"; }
sortie=$(ogs site create blog.test --kind php-plain --php "$version" --group acme \
    --tls=false --yes 2>&1) || { echo "$sortie" | tail -8; echec "création de blog.test"; }

[ "$(regime /home/acme)" = "root:root 701" ] ||
    echec "répertoire du groupe : $(regime /home/acme), attendu root:root 701"
[ "$(stat -c %U /home/acme/shop.test)" = shoptest ] || echec "home de shop.test : $(regime /home/acme/shop.test)"
[ "$(getent passwd shoptest | cut -d: -f6)" = /home/acme/shop.test ] || echec "home du compte shoptest"
echo "répertoire du groupe : $(regime /home/acme)"

# www-data n'énumère pas le groupe : il le traverse.
if runuser -u www-data -- ls /home/acme >/dev/null 2>&1; then
    echec "www-data énumère le répertoire du groupe"
fi
deposer shoptest index.php '<?php echo "shop:" . get_current_user();'
deposer blogtest page.txt 'page du blog'
sert shop.test /index.php "shop:shoptest" || echec "shop.test n'est pas servi à travers le répertoire du groupe"
sert blog.test /page.txt "page du blog" || echec "blog.test ne sert pas un fichier statique à travers le groupe"
echo "les deux sites sont servis à travers /home/acme (0701)"

echo
echo "--- la base est préfixée, le compte ne l'est pas ---"
mariadb -N -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name='acme_shoptest'" |
    grep -qx acme_shoptest || echec "base acme_shoptest absente"
if mariadb -N -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name='shoptest'" | grep -q .; then
    echec "une base shoptest non préfixée a été créée"
fi
mariadb -N -e "SELECT user FROM mysql.user WHERE user='shoptest'" | grep -qx shoptest || echec "compte shoptest absent"
# -r : sans lui, le client double la barre oblique de l'échappement à l'affichage.
mariadb -N -r -e "SHOW GRANTS FOR 'shoptest'@'localhost'" | grep -qF 'ON `acme\_shoptest`.*' ||
    echec "le GRANT doit porter sur acme\\_shoptest, motif échappé : $(mariadb -N -e "SHOW GRANTS FOR 'shoptest'@'localhost'")"
dsn=$(ogs secrets env shoptest 2>/dev/null | sed -n 's/^DATABASE_URL="\(.*\)"$/\1/p')
[[ "${dsn%%\?*}" == */acme_shoptest ]] || echec "DATABASE_URL ne désigne pas acme_shoptest : $dsn"
motdepasse=$(ogs secrets show shoptest mysql/shoptest 2>/dev/null | tail -1)
[ -n "$motdepasse" ] || echec "mot de passe de shoptest introuvable"
base=$(runuser -u shoptest -- env MYSQL_PWD="$motdepasse" mariadb -u shoptest -N -e 'SELECT DATABASE()' acme_shoptest) ||
    echec "le compte shoptest ne se connecte pas à acme_shoptest"
[ "$base" = acme_shoptest ] || echec "connexion : $base"
runuser -u shoptest -- env MYSQL_PWD="$motdepasse" mariadb -u shoptest acme_shoptest \
    -e "CREATE TABLE t (v VARCHAR(20)); INSERT INTO t VALUES ('donnée du groupe')" || echec "écriture dans la base"
echo "shoptest se connecte à acme_shoptest, sa seule base"

echo
echo "--- doctor, harden et l'apply jugent le répertoire comme il faut ---"
# Capturée d'abord : doctor rend un code non nul pour d'autres contrôles du
# conteneur, et pipefail ferait échouer le filtre.
diagnostic=$(ogs doctor 2>&1)
grep -F "répertoire du groupe acme" <<<"$diagnostic" | grep -qF "2 membre(s)" ||
    { grep -F groupe <<<"$diagnostic"; echec "doctor ne juge pas le répertoire du groupe conforme"; }
ogs harden --apply --yes >/dev/null 2>&1
[ "$(regime /home/acme)" = "root:root 701" ] || echec "harden a touché le répertoire du groupe : $(regime /home/acme)"
chmod 0755 /home/acme
chgrp www-data /home/acme
plan=$(ogs plan shoptest 2>&1)
grep -F "répertoire du groupe" <<<"$plan" | grep -qF "0755 → 0701" ||
    { tail -20 <<<"$plan"; echec "le plan n'annonce pas le mode à rétablir"; }
ogs apply shoptest --yes >/dev/null 2>&1
[ "$(regime /home/acme)" = "root:root 701" ] || echec "l'apply n'a pas rétabli le répertoire : $(regime /home/acme)"
sert blog.test /page.txt "page du blog" || echec "blog.test ne répond plus après la correction"
echo "mode et groupe retouchés sont rétablis par l'apply ; harden n'y touche pas"

echo
echo "--- un répertoire de groupe tenu par un autre compte est refusé ---"
useradd -M -s /usr/sbin/nologin intrus
mkdir -m 0701 /home/zeta
chown intrus: /home/zeta
if refus=$(ogs site create z.test --kind static --group zeta --tls=false --yes 2>&1); then
    echec "la création a accepté un répertoire de groupe à intrus"
fi
grep -qF "intrus" <<<"$refus" || { echo "$refus" | tail -5; echec "le refus ne nomme pas intrus"; }
[ -e /etc/ogs/projects/ztest.yaml ] && echec "la déclaration a été déposée malgré le refus"
getent passwd ztest >/dev/null && echec "le compte ztest a été créé"
[ "$(regime /home/zeta)" = "intrus:intrus 701" ] || echec "le répertoire d'intrus a changé : $(regime /home/zeta)"
rm -rf /home/zeta
userdel intrus
echo "la création refuse un répertoire de groupe qui n'est pas à root, sans rien déposer"

echo
echo "--- suppression : le répertoire part avec le dernier membre ---"
sortie=$(ogs project delete shoptest --yes 2>&1); code=$?
[ "$code" -eq 0 ] || { echo "$sortie" | tail -10; echec "suppression de shoptest : code $code"; }
archive=$(archive_de shoptest)
[ -n "$archive" ] || echec "aucune archive de shoptest"
[ -d /home/acme/shop.test ] && echec "le home de shoptest existe encore"
getent passwd shoptest >/dev/null && echec "le compte shoptest existe encore"
[ "$(regime /home/acme)" = "root:root 701" ] || echec "le répertoire du groupe doit rester pour blog.test"
sert blog.test /page.txt "page du blog" || echec "blog.test ne répond plus après la suppression de son voisin"
sortie=$(ogs project delete blogtest --no-dump --yes 2>&1); code=$?
[ "$code" -eq 0 ] || { echo "$sortie" | tail -10; echec "suppression de blogtest : code $code"; }
[ -e /home/acme ] && echec "le répertoire du groupe a survécu à son dernier membre"
echo "le répertoire du groupe reste tant qu'il a un membre, et part avec le dernier"

echo
echo "--- restauration : le groupe, sa base et le site reviennent ---"
sortie=$(ogs project restore "$archive" --yes 2>&1); code=$?
if [ "$code" -ne 0 ] && [ "$code" -ne 6 ]; then
    echo "$sortie" | tail -10
    echec "restauration : code $code"
fi
[ "$(regime /home/acme)" = "root:root 701" ] || echec "répertoire du groupe après restauration : $(regime /home/acme)"
[ "$(stat -c %U /home/acme/shop.test)" = shoptest ] || echec "home restauré : $(regime /home/acme/shop.test)"
donnee=$(mariadb -N -e 'SELECT v FROM acme_shoptest.t' 2>&1)
[ "$donnee" = "donnée du groupe" ] || echec "la base acme_shoptest n'a pas été rendue : $donnee"
systemctl reload apache2 >/dev/null 2>&1
sert shop.test /index.php "shop:shoptest" || echec "shop.test n'est pas servi après restauration"
sortie=$(ogs project delete shoptest --no-dump --yes 2>&1) || { echo "$sortie" | tail -5; echec "seconde suppression"; }
[ -e /home/acme ] && echec "le répertoire du groupe a survécu à la seconde suppression"
echo "la restauration recrée le répertoire, la base préfixée et le site"

echo
echo "--- sous nginx : disable_symlinks traverse le répertoire du groupe ---"
choisir_moteur nginx
sortie=$(ogs site create shop.test --kind php-plain --php "$version" --group beta --tls=false --yes 2>&1) ||
    { echo "$sortie" | tail -8; echec "création de shop.test sous nginx"; }
[ "$(regime /home/beta)" = "root:root 701" ] || echec "répertoire du groupe beta : $(regime /home/beta)"
grep -rqs "disable_symlinks" /etc/nginx/sites-available/shoptest.conf ||
    echec "le vhost nginx ne porte pas disable_symlinks : le scénario n'éprouve rien"
deposer shoptest index.php '<?php echo "nginx:" . get_current_user();'
deposer shoptest page.txt 'page nginx'
sert shop.test /index.php "nginx:shoptest" || echec "nginx n'exécute pas le PHP à travers le répertoire du groupe"
sert shop.test /page.txt "page nginx" || echec "nginx ne sert pas un fichier statique à travers le répertoire du groupe"
sert shop.test / "nginx:shoptest" || echec "nginx ne sert pas l'accueil à travers le répertoire du groupe"
sortie=$(ogs project delete shoptest --no-dump --yes 2>&1) || { echo "$sortie" | tail -5; echec "suppression sous nginx"; }
[ -e /home/beta ] && echec "le répertoire du groupe beta a survécu"
echo "nginx sert les sites du groupe malgré disable_symlinks, et le répertoire part avec eux"

echo
echo "RÉSULTAT : le répertoire du groupe est root:root 0701, traversé par les deux moteurs, jugé par l'apply seul, retiré avec son dernier membre et recréé par la restauration ; la base est préfixée et le compte s'y connecte."
exit 0
