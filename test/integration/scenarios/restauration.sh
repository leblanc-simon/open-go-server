#!/bin/bash
# Éprouve la restauration d'un projet supprimé, en root et sous un vrai systemd.
#
# Ce que les tests unitaires ne peuvent pas voir — ils tournent sans privilèges,
# et le mode préfixé ne pose aucun propriétaire :
#   — l'extraction ne traverse aucun des liens que l'apply de la restauration
#     vient de poser dans le home : trousseau commun (D61), interpréteur PHP (D70),
#     ~/logs (D71) ; l'archive porte le home tel que l'utilisateur l'avait laissé ;
#   — chaque entrée revient à qui la possédait : au compte recréé ce qui était à
#     l'utilisateur — un authorized_keys que sshd lit sous son identité, les
#     sessions, un site hors de www/ —, à root le reste, dont un lien dur vers un
#     fichier du système posé dans le home ;
#   — l'aller-retour ordinaire rétablit les liens gérés, et l'apply suivant est vide ;
#   — l'ancien ~/logs d'un projet d'avant D71, répertoire root, revient à root, et
#     l'apply final de la restauration le migre (D113) : rendu à l'utilisateur, la
#     migration le refusait à chaque apply comme un répertoire substitué ;
#   — les services ne démarrent qu'après le contenu (D113) : une application dont
#     le script manque au home échoue, et restait « failed » après l'extraction ;
#   — une restauration interrompue se reprend par --resume (D113).
set -uo pipefail
. /scenarios/lib.sh

echec() { echo "ÉCHEC : $*"; exit 1; }
archive_de() { ls -t /var/lib/ogs/archives/"$1"-*.tar.zst 2>/dev/null | head -1; }
proprio() { stat -c '%U:%G' "$1" 2>/dev/null; }

echo "=== scénario : restauration d'un projet supprimé ==="
version=$(version_php)
ogs init --quiet
choisir_moteur apache
systemctl start "php$version-fpm" >/dev/null 2>&1

printf 'ssh-ed25519 AAAA cle-des-administrateurs\n' >/opt/default_authorized_keys
chmod 0644 /opt/default_authorized_keys
trousseau=$(sha256sum /opt/default_authorized_keys)
interpreteur=$(sha256sum "/usr/bin/php$version")
printf 'contenu système\n' >/etc/ogs-cible-essai
chmod 0600 /etc/ogs-cible-essai
cible=$(sha256sum /etc/ogs-cible-essai)

echo
echo "--- un home que l'utilisateur a modifié : aucun lien de l'apply n'est traversé ---"
ogs site create shop.fr --kind php-plain --php "$version" --tls=false --yes >/dev/null 2>&1 ||
    echec "création du site"
home=/home/shop.fr
logs=/var/log/ogs-projects/shopfr
[ "$(readlink $home/.ssh/authorized_keys)" = /opt/default_authorized_keys ] ||
    echec "authorized_keys doit être lié au trousseau commun, sans quoi rien n'est éprouvé"
[ "$(readlink $home/.local/bin/php)" = "/usr/bin/php$version" ] || echec "le lien php doit être posé"
[ "$(readlink $home/logs)" = "$logs" ] || echec "~/logs doit être un lien"
su -s /bin/bash shopfr -c '
    set -e
    rm ~/.ssh/authorized_keys
    printf "ssh-ed25519 BBBB cle-du-projet\n" >~/.ssh/authorized_keys
    chmod 600 ~/.ssh/authorized_keys
    rm ~/.local/bin/php
    printf "#!/bin/sh\necho script du projet\n" >~/.local/bin/php
    chmod 700 ~/.local/bin/php
    rm ~/logs
    mkdir ~/logs
    ln -s /etc/ogs-cible-essai ~/logs/apache-error.log
    echo déposé >~/logs/depose.txt
    mkdir -p ~/var/sessions ~/public_html
    echo session >~/var/sessions/sess_abc
    echo "<?php // hors de www" >~/public_html/index.php
' || echec "l'utilisateur doit pouvoir modifier son home — c'est l'hypothèse du scénario"
# Un lien dur vers un fichier du système : fs.protected_hardlinks, actif sur
# l'hôte, l'interdit à l'utilisateur, d'où root qui le pose à sa place — c'est la
# machine où ce réglage est désactivé qu'on éprouve.
ln /etc/ogs-cible-essai $home/www/lien-dur || echec "lien dur impossible : /etc et /home sur deux systèmes de fichiers ?"

sortie=$(ogs project delete shopfr --no-dump --yes 2>&1)
code=$?
[ "$code" -eq 0 ] || { echo "$sortie" | tail -8; echec "suppression : code $code"; }
archive=$(archive_de shopfr)
[ -n "$archive" ] || echec "aucune archive"
# L'apply final de la restauration (D113) traite ce que l'archive a rendu selon
# ses règles : les fichiers de l'utilisateur à la place des liens en font un
# succès partiel.
sortie=$(ogs project restore "$archive" --yes 2>&1)
code=$?
echo "$sortie" | tail -4
[ "$code" -eq 6 ] || echec "restauration : code $code, attendu 6 (fichiers de l'utilisateur à la place des liens)"

[ "$(sha256sum /opt/default_authorized_keys)" = "$trousseau" ] || echec "le trousseau commun a été réécrit"
[ "$(sha256sum "/usr/bin/php$version")" = "$interpreteur" ] || echec "l'interpréteur du système a été réécrit"
[ "$(sha256sum /etc/ogs-cible-essai)" = "$cible" ] || echec "le fichier système a été réécrit"
[ -e $logs/depose.txt ] && echec "un fichier du projet a été déposé dans le répertoire root des journaux"
[ -L $logs/apache-error.log ] && echec "un lien du projet a été posé dans le répertoire root des journaux"
echo "  trousseau, interpréteur, fichier système et $logs intacts"

for f in .ssh/authorized_keys .local/bin/php logs logs/depose.txt var/sessions/sess_abc public_html public_html/index.php; do
    [ "$(proprio $home/$f)" = shopfr:shopfr ] || echec "$f restauré en $(proprio $home/$f), attendu shopfr:shopfr"
done
[ "$(readlink $home/logs/apache-error.log)" = /etc/ogs-cible-essai ] ||
    echec "le lien du projet doit revenir tel quel dans son home"
su -s /bin/bash shopfr -c 'cat ~/.ssh/authorized_keys ~/var/sessions/sess_abc' >/dev/null ||
    echec "l'utilisateur ne lit pas ses clés ni ses sessions : sshd et PHP non plus"
[ "$(stat -c '%U:%G %a' $home/www/lien-dur)" = "root:root 600" ] ||
    echec "le lien dur vers un fichier du système doit revenir à root : $(stat -c '%U:%G %a' $home/www/lien-dur)"
su -s /bin/bash shopfr -c 'cat ~/www/lien-dur' >/dev/null 2>&1 &&
    echec "l'utilisateur lit le fichier système que son lien dur désignait"
echo "  le home revient à l'utilisateur, le lien dur à root"

# L'apply suivant traite ce que l'archive a rendu selon ses règles : un
# authorized_keys régulier n'est jamais remplacé (D61), le script à la place du
# lien php ni le répertoire à la place de ~/logs ne le sont sans --force.
ogs apply shopfr --yes >/dev/null 2>&1
code=$?
[ "$code" -eq 6 ] || echec "apply suivant : code $code, attendu 6 (fichiers de l'utilisateur à la place des liens)"
[ "$(sha256sum /opt/default_authorized_keys)" = "$trousseau" ] || echec "l'apply a réécrit le trousseau commun"

echo
echo "--- l'aller-retour ordinaire ---"
ogs site create ord.fr --kind php-plain --php "$version" --tls=false --yes >/dev/null 2>&1 ||
    echec "création du second site"
su -s /bin/bash ordfr -c '
    set -e
    echo "<?php echo \"restauré\";" >~/www/index.php
    mkdir ~/www/uploads
    echo image >~/www/uploads/a.jpg
' || echec "dépôt du site"
ogs project delete ordfr --no-dump --yes >/dev/null 2>&1 || echec "suppression du second site"
archive=$(archive_de ordfr)
[ -n "$archive" ] || echec "aucune archive du second site"
# --dry-run ne restaure rien (R3-20) : la commande ne sait pas simuler et refuse
# l'option avant tout geste. Elle réécrivait la déclaration, recréait compte et
# home, appliquait le projet et lançait postCreate.
ogs project restore "$archive" --dry-run --yes >/dev/null 2>&1
code=$?
[ "$code" -eq 2 ] || echec "« project restore --dry-run » doit refuser l'option (code 2), obtenu $code"
[ -e /etc/ogs/projects/ordfr.yaml ] && echec "« project restore --dry-run » a réécrit la déclaration"
getent passwd ordfr >/dev/null && echec "« project restore --dry-run » a recréé le compte"
[ -e /home/ord.fr ] && echec "« project restore --dry-run » a recréé le home"
ogs project restore "$archive" --yes >/dev/null 2>&1
code=$?
[ "$code" -eq 0 ] || echec "restauration ordinaire : code $code"
home=/home/ord.fr
[ "$(readlink $home/.ssh/authorized_keys)" = /opt/default_authorized_keys ] || echec "authorized_keys n'est plus lié au trousseau"
[ "$(readlink $home/.local/bin/php)" = "/usr/bin/php$version" ] || echec "le lien php n'est pas rétabli"
[ "$(readlink $home/logs)" = /var/log/ogs-projects/ordfr ] || echec "~/logs n'est pas rétabli"
for f in .ssh/authorized_keys .local/bin/php logs www/index.php www/uploads www/uploads/a.jpg; do
    [ "$(proprio $home/$f)" = ordfr:ordfr ] || echec "$f restauré en $(proprio $home/$f), attendu ordfr:ordfr"
done
[ "$(proprio /opt/default_authorized_keys)" = root:root ] || echec "le trousseau commun a changé de propriétaire"
reponse=$(curl -s -H 'Host: ord.fr' http://127.0.0.1/)
[ "$reponse" = restauré ] || echec "le site restauré ne répond pas : « $reponse »"
ogs apply ordfr --yes >/dev/null 2>&1 || echec "l'apply suivant l'aller-retour doit rendre 0"
ogs diff ordfr >/dev/null 2>&1 || echec "aucun écart ne doit subsister après l'aller-retour"
echo "  liens rétablis au compte d'ordfr, site servi, apply et diff vides"

echo
echo "--- l'ancien ~/logs d'un projet d'avant D71 ---"
ogs site create vieux.fr --kind static --tls=false --yes >/dev/null 2>&1 || echec "création du projet ancien"
vieux=/var/log/ogs-projects/vieuxfr
decl=/etc/ogs/projects/vieuxfr.yaml
python3 - "$decl" <<'PY' || echec "déclaration ancienne non reconstituée"
import sys
chemin = sys.argv[1]
s = open(chemin).read()
t = s.replace("logDir: /var/log/ogs-projects/vieuxfr", "logDir: /home/vieux.fr/logs", 1)
t = t.replace("path: .ssh", 'path: logs\n        mode: "0710"\n        owner: root\n      - path: .ssh', 1)
if t.count("/home/vieux.fr/logs") != 1 or "path: logs" not in t:
    sys.exit(1)
open(chemin, "w").write(t)
PY
rm /home/vieux.fr/logs
install -d -o root -g vieuxfr -m 0710 /home/vieux.fr/logs
printf 'accès ancien\n' >/home/vieux.fr/logs/apache-access.log
printf 'accès roté\n' >/home/vieux.fr/logs/apache-access.log.1
chown root:vieuxfr /home/vieux.fr/logs/apache-access.log*
chmod 0640 /home/vieux.fr/logs/apache-access.log*
rm -rf $vieux
# La suppression d'un projet resté dans cette disposition peut signaler ce qui lui
# manque (code 6) : seule compte ici l'archive qu'elle produit.
ogs project delete vieuxfr --no-dump --yes >/dev/null 2>&1
code=$?
[ "$code" -eq 0 ] || [ "$code" -eq 6 ] || echec "suppression du projet ancien : code $code"
archive=$(archive_de vieuxfr)
[ -n "$archive" ] || echec "aucune archive du projet ancien"
# L'apply final de la restauration le migre : la migration n'admet qu'un ancien
# ~/logs à root (trustedLegacyEntries), donc sa réussite dit que l'extraction
# l'a rendu à root.
sortie=$(ogs project restore "$archive" --yes 2>&1)
code=$?
[ "$code" -eq 0 ] || { echo "$sortie" | grep -i "journaux\|substitué" | head -3; echec "restauration du projet ancien : code $code"; }
[ "$(readlink /home/vieux.fr/logs)" = "$vieux" ] || echec "l'ancien répertoire n'a pas laissé place au lien"
grep -q "accès ancien" $vieux/apache-access.log || echec "le journal courant n'a pas été repris"
grep -q "accès roté" $vieux/apache-access.log.1 || echec "le journal roté n'a pas été repris sous son nom"
ogs apply vieuxfr --yes >/dev/null 2>&1 || echec "l'apply suivant la restauration doit rendre 0"
echo "  rendu à root, migré par l'apply final de la restauration : journaux sous $vieux, lien posé"

echo
echo "--- les services démarrent après le contenu (D113) ---"
ogs site create svc.fr --kind php-plain --php "$version" --tls=false --cron "*/5 * * * *:echo tâche" --yes >/dev/null 2>&1 ||
    echec "création de svc.fr"
su -s /bin/bash svcfr -c 'printf "#!/bin/sh\nexec sleep 3600\n" >~/www/veille.sh && chmod 700 ~/www/veille.sh' ||
    echec "dépôt du worker"
ogs service add svcfr veille --template worker --command /home/svc.fr/www/veille.sh --instances 1 >/dev/null ||
    echec "déclaration du service"
ogs apply svcfr --yes >/dev/null 2>&1 || echec "apply de svc.fr"
[ "$(systemctl is-active ogs-svcfr-veille@1.service)" = active ] || echec "le worker doit tourner avant la suppression"
sortie=$(ogs project delete svcfr --no-dump --yes 2>&1) || { echo "$sortie" | grep -E "⚠|✗"; echec "suppression de svc.fr"; }
systemctl reset-failed 'ogs-svcfr-*' >/dev/null 2>&1
archive=$(archive_de svcfr)
[ -n "$archive" ] || echec "aucune archive de svc.fr"
sortie=$(ogs project restore "$archive" --yes 2>&1)
code=$?
[ "$code" -eq 0 ] || { echo "$sortie" | tail -6; echec "restauration de svc.fr : code $code"; }
etat=$(systemctl is-active ogs-svcfr-veille@1.service)
[ "$etat" = active ] || echec "le worker doit tourner après la restauration : $etat"
# Démarré avant l'extraction, il échouait sur un script absent (203/EXEC) et
# systemd le relançait : le compteur le dit.
relances=$(systemctl show -p NRestarts --value ogs-svcfr-veille@1.service)
[ "$relances" = 0 ] || echec "le worker a démarré avant son script : $relances relance(s)"
echo "  worker démarré une fois, après le contenu"

echo
echo "--- une restauration interrompue se reprend (D113) ---"
sortie=$(ogs project delete svcfr --no-dump --yes 2>&1) || { echo "$sortie" | grep -E "⚠|✗"; echec "seconde suppression de svc.fr"; }
archive=$(archive_de svcfr)
# Un premier apply défait est une création défaite (D112) : déclaration et
# témoin retirés, la restauration se relance telle quelle.
pool=/etc/php/$version/fpm/pool.d/svcfr.conf
printf '; pool posé hors ogs\n' >"$pool"
sortie=$(ogs project restore "$archive" --yes 2>&1)
code=$?
[ "$code" -ne 0 ] && [ "$code" -ne 6 ] || echec "l'apply devait échouer sur le pool étranger (code $code)"
[ -e /etc/ogs/projects/svcfr.yaml ] && echec "un premier apply défait doit retirer la déclaration"
[ -e /var/lib/ogs/restores/svcfr.json ] && echec "un premier apply défait doit retirer le témoin"
rm "$pool"
# L'apply final, lui, échoue le contenu rendu : la restauration se reprend.
taches=/etc/cron.d/ogs-svcfr
printf '# tâches posées hors ogs\n' >"$taches"
sortie=$(ogs project restore "$archive" --yes 2>&1)
code=$?
[ "$code" -ne 0 ] && [ "$code" -ne 6 ] || echec "l'apply final devait échouer sur les tâches étrangères (code $code)"
echo "$sortie" | grep -q -- "--resume" || { echo "$sortie" | tail -4; echec "l'échec doit nommer la reprise"; }
[ -e /var/lib/ogs/restores/svcfr.json ] || echec "le témoin de la restauration inachevée doit rester"
ogs project restore "$archive" --yes >/dev/null 2>&1
[ $? -eq 4 ] || echec "une relance sans --resume doit être refusée en conflit"
rm "$taches"
sortie=$(ogs project restore --resume "$archive" --yes 2>&1)
code=$?
[ "$code" -eq 0 ] || { echo "$sortie" | tail -6; echec "reprise : code $code"; }
[ -e /var/lib/ogs/restores/svcfr.json ] && echec "le témoin doit partir avec la restauration achevée"
[ "$(systemctl is-active ogs-svcfr-veille@1.service)" = active ] || echec "le worker doit tourner après la reprise"
ogs diff svcfr >/dev/null 2>&1 || echec "aucun écart ne doit subsister après la reprise"
echo "  échec nommé, reprise par --resume, témoin retiré, worker actif"

echo
echo "=== scénario restauration : succès ==="
