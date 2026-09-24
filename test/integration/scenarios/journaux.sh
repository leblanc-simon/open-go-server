#!/bin/bash
# Éprouve les journaux des projets hors du home (D71), sous un vrai systemd.
#
# Ce que les tests unitaires ne peuvent pas voir, et qui est éprouvé ici :
#   — les journaux vivent sous /var/log/ogs-projects, chaîne entièrement root, et
#     le projet les lit par son lien ~/logs sans pouvoir rien y créer ;
#   — php-error.log reçoit enfin les erreurs PHP du projet : les workers, sous
#     l'identité du projet, écrivent dans le fichier pré-créé, open_basedir
#     compris ;
#   — l'attaque qui a motivé D71 échoue : l'utilisateur renomme ~/logs et le
#     remplace par un répertoire à lui, où apache-error.log est un lien vers un
#     fichier système ; ni le redémarrage d'Apache ni la rotation n'y écrivent ;
#   — logrotate traite le répertoire sans « su », garde les propriétaires, et
#     l'archive reste lisible au projet ;
#   — la migration d'un ancien ~/logs reprend son contenu et laisse Apache écrire
#     au nouvel emplacement ;
#   — doctor voit une chaîne desserrée.
set -uo pipefail
. /scenarios/lib.sh

echec() { echo "ÉCHEC : $*"; exit 1; }
docteur() { ogs doctor 2>&1 || true; }

echo "=== scénario : journaux hors du home (D71) ==="
version=$(ls /etc/php | head -1)
ogs init --quiet
choisir_moteur apache
systemctl start "php$version-fpm" >/dev/null 2>&1

echo
echo "--- les journaux vivent sous /var/log/ogs-projects, le home n'en porte qu'un lien ---"
ogs site create shop.fr --kind php-plain --php "$version" --tls=false --yes >/dev/null 2>&1 ||
    echec "création du site"
dir=/var/log/ogs-projects/shopfr
echo "  $(stat -c '%a %U:%G' /var/log/ogs-projects) /var/log/ogs-projects"
echo "  $(stat -c '%a %U:%G' $dir) $dir"
[ "$(stat -c '%a %U:%G' /var/log/ogs-projects)" = "711 root:root" ] || echec "racine attendue en 0711 root:root"
[ "$(stat -c '%a %U:%G' $dir)" = "750 root:shopfr" ] || echec "répertoire du projet attendu en 0750 root:shopfr"
[ "$(readlink /home/shop.fr/logs)" = "$dir" ] || echec "~/logs doit désigner $dir"
for f in apache-access.log:root apache-error.log:root php-slow.log:root php-error.log:shopfr; do
    nom=${f%%:*} proprio=${f##*:}
    droits=$(stat -c '%a %U:%G' "$dir/$nom" 2>/dev/null)
    [ "$droits" = "640 $proprio:shopfr" ] || echec "$nom attendu en 0640 $proprio:shopfr, obtenu « $droits »"
done
echo "  fichiers pré-créés, au bon propriétaire"

echo
echo "--- le projet lit ses journaux, n'y crée rien, et ne voit pas ses voisins ---"
ogs site create blog.fr --kind static --tls=false --yes >/dev/null 2>&1 || echec "création du voisin"
su -s /bin/bash shopfr -c 'ls ~/logs/ >/dev/null && cat ~/logs/apache-access.log >/dev/null' ||
    echec "l'utilisateur ne lit pas ses journaux par ~/logs"
if su -s /bin/bash shopfr -c 'touch ~/logs/intrus.log' 2>/dev/null; then
    echec "l'utilisateur crée un fichier dans le répertoire de ses journaux"
fi
if su -s /bin/bash shopfr -c 'ls /var/log/ogs-projects/' >/dev/null 2>&1; then
    echec "la racine des journaux s'énumère : les voisins se voient"
fi
if su -s /bin/bash shopfr -c 'cat /var/log/ogs-projects/blogfr/apache-access.log' >/dev/null 2>&1; then
    echec "le projet lit les journaux de son voisin"
fi
echo "  lecture par ~/logs, aucune création, voisins invisibles"

echo
echo "--- php-error.log reçoit les erreurs PHP du projet ---"
cat >/home/shop.fr/www/erreur.php <<'PHP'
<?php trigger_error("erreur-d71", E_USER_WARNING); echo "ok";
PHP
chown shopfr:shopfr /home/shop.fr/www/erreur.php
reponse=$(curl -s -H 'Host: shop.fr' http://127.0.0.1/erreur.php)
[ "$reponse" = ok ] || echec "page d'essai non servie : $reponse"
sleep 1
if ! grep -q erreur-d71 "$dir/php-error.log"; then
    echo "  journal global de FPM : $(grep -h erreur-d71 /var/log/php*-fpm.log 2>/dev/null | tail -1)"
    echec "l'erreur PHP n'est pas arrivée dans php-error.log"
fi
echo "  $(tail -1 $dir/php-error.log)"

echo
echo "--- l'attaque qui a motivé D71 : renommer ~/logs ne mène plus nulle part ---"
printf 'contenu système\n' >/etc/ogs-cible-essai
chmod 600 /etc/ogs-cible-essai
empreinte=$(sha256sum /etc/ogs-cible-essai)
su -s /bin/bash shopfr -c 'mv ~/logs ~/logs.vrai && mkdir ~/logs && ln -s /etc/ogs-cible-essai ~/logs/apache-error.log' ||
    echec "l'utilisateur doit pouvoir manipuler son home — c'est l'hypothèse de l'attaque"
systemctl restart apache2
curl -s -H 'Host: shop.fr' http://127.0.0.1/inexistant >/dev/null
logrotate -f /etc/logrotate.d/ogs 2>/tmp/logrotate.log
[ "$(sha256sum /etc/ogs-cible-essai)" = "$empreinte" ] || echec "root a écrit dans le fichier désigné par l'utilisateur"
# La requête est journalisée dans le journal d'accès — une 404 n'entre pas au
# journal d'erreurs — puis rotée par la rotation forcée.
cat "$dir"/apache-access.log* 2>/dev/null | grep -q "/inexistant" || echec "Apache n'écrit plus son journal"
echo "  fichier système intact, Apache écrit toujours sous $dir"
su -s /bin/bash shopfr -c 'rm -rf ~/logs'
su -s /bin/bash shopfr -c 'mv ~/logs.vrai ~/logs'

echo
echo "--- logrotate : sans « su », propriétaires gardés, archive lisible ---"
if grep -qi "insecure\|parent directory" /tmp/logrotate.log; then
    cat /tmp/logrotate.log
    echec "logrotate refuse le répertoire des journaux"
fi
grep -q '^\s*su ' /etc/logrotate.d/ogs && echec "la rotation ne doit porter aucun « su »"
grep -q '/home/' /etc/logrotate.d/ogs && echec "la rotation ne doit passer par aucun chemin du home"
[ "$(stat -c '%U' $dir/php-error.log)" = shopfr ] || echec "php-error.log recréé sans son propriétaire"
archive_rotee=$(ls $dir/apache-access.log.1* 2>/dev/null | head -1)
[ -n "$archive_rotee" ] || echec "aucune archive après la rotation forcée"
su -s /bin/bash shopfr -c "cat $archive_rotee" >/dev/null || echec "l'archive n'est pas lisible au projet"
echo "  rotation faite, $(basename "$archive_rotee") lisible par le projet"

echo
echo "--- migration d'un ancien ~/logs, dans une disposition d'avant D71 ---"
ogs site create vieux.fr --kind static --tls=false --yes >/dev/null 2>&1 || echec "création du projet à migrer"
vieux=/var/log/ogs-projects/vieuxfr
decl=/etc/ogs/projects/vieuxfr.yaml
sed -i "s#logDir: $vieux#logDir: /home/vieux.fr/logs#" $decl
grep -q "logDir: /home/vieux.fr/logs" $decl || echec "déclaration ancienne non reconstituée"
rm /home/vieux.fr/logs
install -d -o root -g vieuxfr -m 0710 /home/vieux.fr/logs
printf 'accès ancien\n' >/home/vieux.fr/logs/apache-access.log
printf 'accès roté\n' >/home/vieux.fr/logs/apache-access.log.1
rm -rf $vieux
ogs apply vieuxfr --yes 2>&1 | grep -Ei "reprise|ancien" | head -3
[ "$(readlink /home/vieux.fr/logs)" = "$vieux" ] || echec "l'ancien répertoire n'a pas laissé place au lien"
grep -q "accès ancien" $vieux/apache-access.log || echec "le journal courant n'a pas été repris"
grep -q "accès roté" $vieux/apache-access.log.1 || echec "le journal roté n'a pas été repris sous son nom"
curl -s -H 'Host: vieux.fr' http://127.0.0.1/ >/dev/null
sleep 1
[ "$(wc -l <$vieux/apache-access.log)" -ge 2 ] || echec "Apache n'écrit pas au nouvel emplacement après la migration"
echo "  contenu repris, lien posé, Apache écrit sous $vieux"

echo
echo "--- doctor voit une chaîne desserrée ---"
docteur | grep -i "chaîne des journaux" | grep -q "entièrement à root" || echec "doctor n'accepte pas une chaîne conforme"
chmod 0777 /var/log/ogs-projects
docteur | grep -i "chaîne des journaux" | grep -q "inscriptible" || echec "doctor ne voit pas la racine ouverte"
ogs apply shopfr --yes >/dev/null 2>&1
[ "$(stat -c %a /var/log/ogs-projects)" = 711 ] || echec "l'apply n'a pas refermé la racine"
echo "  racine ouverte signalée, puis refermée par l'apply"

echo
echo "--- la suppression archive les journaux puis les retire ---"
ogs project delete blogfr --yes >/dev/null 2>&1
[ -d /var/log/ogs-projects/blogfr ] && echec "les journaux du projet supprimé sont restés"
archive=$(ls -t /var/lib/ogs/archives/blogfr-*.tar.zst 2>/dev/null | head -1)
[ -n "$archive" ] || echec "aucune archive"
echo "  journaux retirés, archive $(basename "$archive")"

echo
echo "=== scénario journaux : succès ==="
