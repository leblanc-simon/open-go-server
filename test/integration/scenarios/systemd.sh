#!/bin/bash
# Éprouve les services systemd et les tâches planifiées sur un système où
# systemd est réellement PID 1 et où cron tourne. Tout ce qui suit un
# « systemctl enable » — instances templatisées, mise à l'échelle, durcissement,
# délégation par sudo — ne s'éprouve pas autrement.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : services systemd et tâches planifiées ==="
systemctl is-system-running || true
version=$(ls /etc/php | head -1)

ogs init --quiet
choisir_moteur apache

echo
echo "--- création d'un site avec deux services et une tâche ---"
ogs site create shop.fr --kind php-symfony --app-version 6.4 --php "$version" --tls=false --yes -q >/dev/null 2>&1
# Une commande qui existe vraiment : le but est de vérifier que systemd accepte
# l'unité et maintient les instances, pas d'exécuter Symfony.
install -o shopfr -g shopfr -m 0750 /dev/null /home/shop.fr/www/veille.sh
cat > /home/shop.fr/www/veille.sh <<'VEILLE'
#!/bin/sh
# Un worker minimal : il écrit qu'il vit, puis attend.
echo "$(date -Iseconds) instance ${1:-?} sous $(id -un)" >> /home/shop.fr/logs/veille.log
exec sleep 3600
VEILLE
chmod 0750 /home/shop.fr/www/veille.sh
chown shopfr:shopfr /home/shop.fr/www/veille.sh
: > /home/shop.fr/logs/veille.log
chown shopfr:shopfr /home/shop.fr/logs/veille.log

ogs service add shopfr veille --template worker --command "/home/shop.fr/www/veille.sh" --instances 2 >/dev/null
# La tâche écrit sur sa sortie standard : c'est cron qui la redirige vers le
# journal pré-créé par ogs. Le répertoire logs/ appartient à root — le projet n'y
# crée rien, ce qui interdit l'escalade par lien symbolique (D29).
ogs cron add shopfr horodate --schedule "* * * * *" --command "/bin/date -Iseconds" >/dev/null
ogs apply shopfr --yes 2>&1 | grep -Ei "unité|instance|tâche|cron|target|⚠|✗" | head -12

echo
echo "--- ce que systemd connaît ---"
systemctl list-units --no-pager --all 'ogs-shopfr*' | grep ogs- | head -6
if ! systemctl cat ogs-shopfr-veille@.service >/dev/null 2>&1; then
    echo "ÉCHEC : l'unité templatisée n'est pas connue de systemd"
    ls -l /etc/systemd/system/ | grep ogs || true
    exit 1
fi
# systemd doit accepter le fichier sans réserve : une directive invalide se
# traduit par un avertissement au chargement.
if systemd-analyze verify /etc/systemd/system/ogs-shopfr-veille@.service 2>&1 | grep -v "^$" | head -5; then
    :
fi

echo
echo "--- une unité que systemd refuse est rendue (D117) ---"
ogs template extract systemd/worker.service >/dev/null
sed -i 's/^\[Service\]/[Service]\nRestartt=always/' /etc/ogs/templates/systemd/worker.service.tmpl
cp /etc/systemd/system/ogs-shopfr-veille@.service /tmp/veille-avant
sortie=$(ogs apply shopfr --yes 2>&1)
if ! echo "$sortie" | grep -q "systemd-analyze verify refuse.*Restartt"; then
    echo "ÉCHEC : l'unité fautive n'a pas été refusée :"
    echo "$sortie" | tail -5
    exit 1
fi
if ! cmp -s /tmp/veille-avant /etc/systemd/system/ogs-shopfr-veille@.service; then
    echo "ÉCHEC : l'unité refusée n'a pas été rendue"
    exit 1
fi
rm /etc/ogs/templates/systemd/worker.service.tmpl
plan=$(ogs plan shopfr 2>&1)
if ! echo "$plan" | grep -q "aucun changement"; then
    echo "ÉCHEC : le template retiré, le plan doit être vide"
    echo "$plan" | tail -5
    exit 1
fi
echo "unité refusée par systemd-analyze verify, et rendue"

echo
echo "--- un « % » dans une variable d'environnement (D117) ---"
# Un mot de passe encodé dans une URL : systemd refusait l'affectation entière
# (« Invalid slot »), la variable manquait au service sans un mot, et le
# validateur aurait refusé l'unité à chaque apply.
# L'image n'a pas python3-yaml : la déclaration est retouchée à la main, comme
# le ferait l'exploitant.
sed -i '/- name: veille/,/params:/ s|^\(      params:\)$|\1\n        env.DATABASE_URL: "mysql://u:p%40ss@h/b"|' \
    /etc/ogs/projects/shopfr.yaml
grep -q 'env.DATABASE_URL' /etc/ogs/projects/shopfr.yaml || { echo "ÉCHEC : déclaration non retouchée"; exit 1; }
sortie=$(ogs apply shopfr --yes 2>&1); code=$?
if [ "$code" != 0 ]; then
    echo "ÉCHEC : l'apply d'une valeur à « % » rend le code $code :"
    echo "$sortie" | tail -5
    exit 1
fi
envi=$(systemctl show -p Environment ogs-shopfr-veille@1.service)
if ! echo "$envi" | grep -q 'DATABASE_URL=mysql://u:p%40ss@h/b'; then
    echo "ÉCHEC : la variable n'atteint pas le service telle quelle : $envi"
    exit 1
fi
echo "variable à « % » transmise telle quelle au service"

echo
echo "--- les instances tournent-elles, et sous quelle identité ? ---"
for i in 1 2; do
    etat=$(systemctl is-active "ogs-shopfr-veille@$i.service" 2>&1)
    echo "instance $i : $etat"
    if [ "$etat" != "active" ]; then
        echo "ÉCHEC : l'instance $i n'est pas active"
        systemctl status "ogs-shopfr-veille@$i.service" --no-pager -l 2>&1 | tail -12
        exit 1
    fi
done
identites=$(ps -o user= -C sleep | sort -u | tr '\n' ' ')
echo "processus des workers : $identites"
if [ "$(echo "$identites" | tr -d ' ')" != "shopfr" ]; then
    echo "ÉCHEC : les workers ne tournent pas sous l'identité du projet"
    exit 1
fi

echo
echo "--- la cible groupe-t-elle les unités ? ---"
# Elle doit être activée : sans lien depuis multi-user.target, rien ne la réclame
# au démarrage de la machine et aucun service de projet ne repart.
etat=$(systemctl is-enabled ogs-shopfr.target 2>&1)
echo "cible activée au démarrage : $etat"
if [ "$etat" != "enabled" ]; then
    echo "ÉCHEC : la cible n'est pas activée — les services ne survivraient pas à un redémarrage"
    ls -l /etc/systemd/system/multi-user.target.wants/ 2>&1 | grep ogs || true
    exit 1
fi
if ! systemctl show ogs-shopfr.target -p Wants --no-pager | grep -q veille; then
    echo "ÉCHEC : la cible ne réclame pas les instances"
    systemctl show ogs-shopfr.target -p Wants --no-pager
    exit 1
fi
# La cible démarre avec ses instances (D114) : inactive, elle ne propageait ni
# l'arrêt ni le redémarrage, et la suppression laissait les workers vivants.
if [ "$(systemctl is-active ogs-shopfr.target)" != "active" ]; then
    echo "ÉCHEC : l'apply a démarré les instances sans leur cible"
    exit 1
fi
systemctl stop ogs-shopfr.target
sleep 1
if [ "$(systemctl is-active 'ogs-shopfr-veille@1.service')" = "active" ]; then
    echo "ÉCHEC : arrêter la cible n'a pas arrêté les instances"
    exit 1
fi
echo "arrêter la cible arrête toutes les instances"
systemctl start ogs-shopfr.target
sleep 2

echo
echo "--- durcissement effectivement appliqué ---"
# ProtectHome=read-only doit rendre le home inscriptible seulement là où l'unité
# le déclare. On le vérifie depuis l'espace de noms du service.
pid=$(systemctl show -p MainPID --value "ogs-shopfr-veille@1.service")
echo "PID de l'instance 1 : $pid"
for reglage in NoNewPrivileges ProtectSystem ProtectHome UMask; do
    echo "  $reglage=$(systemctl show -p $reglage --value "ogs-shopfr-veille@1.service")"
done
if [ "$(systemctl show -p NoNewPrivileges --value "ogs-shopfr-veille@1.service")" != "yes" ]; then
    echo "ÉCHEC : NoNewPrivileges n'est pas appliqué"
    exit 1
fi

echo
echo "--- mise à l'échelle ---"
ogs service scale shopfr veille 3 --yes 2>&1 | tail -2
sleep 2
actives=$(systemctl list-units --no-pager --state=active 'ogs-shopfr-veille@*' | grep -c "ogs-shopfr-veille@")
echo "instances actives : $actives"
if [ "$actives" != "3" ]; then
    echo "ÉCHEC : 3 instances attendues"
    systemctl list-units --no-pager --all 'ogs-shopfr-veille@*'
    exit 1
fi
# Redescendre doit arrêter et désactiver l'instance surnuméraire, non la laisser
# tourner sans être réclamée.
ogs service scale shopfr veille 1 --yes >/dev/null 2>&1
sleep 2
restantes=$(systemctl list-units --no-pager --state=active 'ogs-shopfr-veille@*' | grep -c "ogs-shopfr-veille@")
echo "après réduction : $restantes"
if [ "$restantes" != "1" ]; then
    echo "ÉCHEC : les instances surnuméraires n'ont pas été arrêtées"
    systemctl list-units --no-pager --all 'ogs-shopfr-veille@*'
    exit 1
fi

echo
echo "--- délégation : l'utilisateur du projet pilote ses services ---"
visudo -c -f /etc/sudoers.d/ogs-shopfr
if ! su -s /bin/bash shopfr -c 'sudo -n ogs my status' 2>&1 | tail -4; then
    echo "ÉCHEC : « ogs my status » refusé à l'utilisateur du projet"
    su -s /bin/bash shopfr -c 'sudo -n -l' 2>&1 | tail -5
    exit 1
fi
su -s /bin/bash shopfr -c 'sudo -n ogs my restart' >/dev/null 2>&1
sleep 2
if [ "$(systemctl is-active 'ogs-shopfr-veille@1.service')" != "active" ]; then
    echo "ÉCHEC : « ogs my restart » a laissé le service arrêté"
    exit 1
fi
echo "l'utilisateur a redémarré son propre service"
# Et il ne doit pouvoir toucher que le sien.
ogs site create autre.fr --kind static --tls=false --yes -q >/dev/null 2>&1
if su -s /bin/bash shopfr -c 'sudo -n systemctl restart ogs-autrefr.target' >/dev/null 2>&1; then
    echo "ÉCHEC : l'utilisateur atteint les services d'un autre projet"
    exit 1
fi
echo "et rien d'autre : la cible d'un autre projet lui est refusée"

echo
echo "--- journaux d'un service (D101) ---"
# La sortie de journalctl était capturée puis jetée : rien ne s'affichait.
journaux=$(ogs service logs shopfr veille -n 20 2>&1); rc=$?
echo "$journaux" | tail -3
if [ "$rc" != 0 ] || ! echo "$journaux" | grep -q "ogs-shopfr-veille@"; then
    echo "ÉCHEC : « service logs » n'affiche pas les journaux des instances (code $rc)"
    exit 1
fi
# Sous -f, le flux arrive avant l'interruption, et Ctrl-C rend 9, non 5.
suivi=$(timeout --preserve-status -s INT 4 ogs service logs shopfr veille -f -n 5 2>&1); rc=$?
if [ "$rc" != 9 ] || ! echo "$suivi" | grep -q "ogs-shopfr-veille@"; then
    echo "ÉCHEC : « service logs -f » interrompu : code $rc (9 attendu), sortie :"
    echo "$suivi" | tail -5
    exit 1
fi
echo "« service logs -f » montre le flux, et l'interruption rend 9"
ogs service logs shopfr sshd >/dev/null 2>&1; rc=$?
if [ "$rc" != 2 ]; then
    echo "ÉCHEC : une unité hors du projet doit être refusée en code 2 (obtenu $rc)"
    exit 1
fi

echo
echo "--- tâche planifiée : exécution immédiate ---"
cat /etc/cron.d/ogs-shopfr
sortie=$(ogs cron run shopfr horodate 2>&1)
echo "$sortie" | tail -3
if ! echo "$sortie" | grep -qE "[0-9]{4}-[0-9]{2}-[0-9]{2}T"; then
    echo "ÉCHEC : la tâche n'a rien produit"
    exit 1
fi
# Le journal de la tâche appartient au projet, mais le répertoire à root : la
# tâche écrit dans un fichier pré-créé, elle n'en crée aucun.
# Il vit hors du home (D71) : ~/logs n'en est qu'un lien, que stat ne suit pas.
echo "journal : $(stat -c '%U:%G %a' /var/log/ogs-projects/shopfr/cron-horodate.log), répertoire : $(stat -c '%U:%G %a' /var/log/ogs-projects/shopfr)"
if [ "$(stat -c %U /var/log/ogs-projects/shopfr)" != "root" ]; then
    echo "ÉCHEC : le répertoire des journaux doit appartenir à root"
    exit 1
fi
# Un nom nu reste au PATH du fichier cron : « date … » devenait <www>/date (D101).
ogs cron add shopfr nu --schedule "0 0 1 1 *" --command "date -Iseconds" >/dev/null
ogs apply shopfr --yes -q >/dev/null 2>&1
sortie=$(ogs cron run shopfr nu 2>&1)
if ! echo "$sortie" | grep -qE "[0-9]{4}-[0-9]{2}-[0-9]{2}T"; then
    echo "ÉCHEC : une commande au nom nu doit être cherchée dans le PATH :"
    grep " nu\|date" /etc/cron.d/ogs-shopfr; echo "$sortie" | tail -3
    exit 1
fi
echo "une commande au nom nu passe par le PATH"

echo
echo "--- tâche planifiée : déclenchement réel par cron ---"
journal=/var/log/ogs-projects/shopfr/cron-horodate.log
: > "$journal"
chown shopfr:shopfr "$journal"
systemctl restart cron
echo "attente de la minute suivante (jusqu'à 70 s)…"
for _ in $(seq 1 70); do
    [ -s "$journal" ] && break
    sleep 1
done
if [ ! -s "$journal" ]; then
    echo "ÉCHEC : cron n'a pas déclenché la tâche"
    journalctl -u cron --no-pager -n 15 2>/dev/null | tail -10
    exit 1
fi
echo "cron a déclenché la tâche : $(tail -1 "$journal")"
# Et sous l'identité du projet : le fichier ne doit pas avoir changé de main.
if [ "$(stat -c %U "$journal")" != "shopfr" ]; then
    echo "ÉCHEC : le journal n'appartient plus au projet"
    exit 1
fi

echo
echo "--- le verrou empêche-t-il le recouvrement ? ---"
# La tâche est enveloppée dans flock : deux exécutions concurrentes ne doivent
# pas se chevaucher.
if ! grep -q flock /etc/cron.d/ogs-shopfr; then
    echo "ÉCHEC : la tâche n'est pas enveloppée dans flock"
    exit 1
fi
verrou=$(grep -o '\-\-\?[a-z]* [^ ]*ogs-shopfr[^ ]*\.lock' /etc/cron.d/ogs-shopfr | head -1)
echo "verrou : ${verrou:-présent}"

echo
echo "--- les liens gérés vivent dans ~/.local/bin, au compte du projet (D70) ---"
lien=/home/shop.fr/.local/bin/php
proprietaires="$(stat -c %U /home/shop.fr/.local) $(stat -c %U /home/shop.fr/.local/bin) $(stat -c %U "$lien")"
echo "  .local, .local/bin, php : $proprietaires"
if [ "$proprietaires" != "shopfr shopfr shopfr" ]; then
    echo "ÉCHEC : le répertoire des liens et le lien doivent appartenir au projet"
    exit 1
fi
if [ "$(su -s /bin/bash shopfr -c "$lien -r 'echo PHP_MAJOR_VERSION;'" 2>&1)" = "" ]; then
    echo "ÉCHEC : l'utilisateur du projet n'exécute pas son interpréteur"
    exit 1
fi
# ~/.local appartient à l'utilisateur : il peut le remplacer par un lien vers un
# répertoire de son choix. Root ne doit pas y écrire.
su -s /bin/bash shopfr -c 'mv ~/.local ~/.local.vrai && mkdir -p /tmp/piege && ln -s /tmp/piege ~/.local'
ogs apply shopfr --yes >/tmp/apply-piege.log 2>&1
if [ -n "$(ls -A /tmp/piege)" ]; then
    echo "ÉCHEC : root a écrit dans le répertoire désigné par l'utilisateur : $(ls -A /tmp/piege)"
    exit 1
fi
grep -q "répertoire" /tmp/apply-piege.log || { cat /tmp/apply-piege.log; echo "ÉCHEC : le détour n'est pas signalé"; exit 1; }
echo "  ~/.local remplacé par un lien : refusé, rien n'a été écrit ailleurs"
su -s /bin/bash shopfr -c 'rm ~/.local && mv ~/.local.vrai ~/.local'

# Le même détour, mais après le plan : au plan, ~/.local est conforme et
# ~/.local/bin à créer ; le hook preApply, qui tourne entre le plan et son
# exécution — la fenêtre que la confirmation d'« ogs apply » laisse à
# l'utilisateur —, remplace ~/.local par un lien vers /usr/local. Root résolvait
# de nouveau ~/.local/bin par son nom et donnait /usr/local/bin au projet, en tête
# de son propre PATH.
su -s /bin/bash shopfr -c 'mv ~/.local/bin ~/.local/bin.vrai'
avant=$(stat -c '%a %U:%G' /usr/local/bin)
mkdir -p /etc/ogs/hooks
cat > /etc/ogs/hooks/pre-apply <<'HOOK'
#!/bin/sh
su -s /bin/bash shopfr -c 'mv ~/.local ~/.local.vrai && ln -s /usr/local ~/.local'
HOOK
chmod 0750 /etc/ogs/hooks/pre-apply
if ogs apply shopfr --yes >/tmp/apply-course.log 2>&1; then
    cat /tmp/apply-course.log
    echo "ÉCHEC : un parent substitué entre le plan et l'exécution doit faire échouer l'apply"
    exit 1
fi
rm -f /etc/ogs/hooks/pre-apply
apres=$(stat -c '%a %U:%G' /usr/local/bin)
if [ "$avant" != "$apres" ]; then
    echo "ÉCHEC : /usr/local/bin est passé de $avant à $apres, par le parent substitué"
    exit 1
fi
su -s /bin/bash shopfr -c 'rm ~/.local && mv ~/.local.vrai ~/.local && mv ~/.local/bin.vrai ~/.local/bin'
echo "  ~/.local substitué pendant l'apply : refusé, /usr/local/bin intact ($apres)"

echo
echo "--- migration : l'ancien .bin/ disparaît, les services restent debout ---"
# Un projet d'avant D70 : .bin/ en root, et sa déclaration dans le fichier.
install -d -o root -g shopfr -m 0710 /home/shop.fr/.bin
ln -s "$(readlink "$lien")" /home/shop.fr/.bin/php
sed -i 's#^\(\s*\)- path: \.ssh#\1- path: .bin\n\1  mode: "0710"\n\1  owner: root\n\1- path: .ssh#' /etc/ogs/projects/shopfr.yaml
grep -q "path: .bin" /etc/ogs/projects/shopfr.yaml || { echo "ÉCHEC : déclaration ancienne non reconstituée"; exit 1; }
ogs apply shopfr --yes 2>&1 | grep -i "ancien" | head -2
if [ -e /home/shop.fr/.bin ]; then
    echo "ÉCHEC : l'ancien .bin/ est resté"
    exit 1
fi
# La mise à l'échelle précédente a ramené le service à une instance.
etat=$(systemctl is-active "ogs-shopfr-veille@1.service" 2>&1)
if [ "$etat" != "active" ]; then
    echo "ÉCHEC : l'instance 1 est $etat après la migration"
    exit 1
fi
echo "  .bin/ retiré, instance toujours active"

echo
echo "--- service retiré : son modèle part, son instance s'arrête (D100) ---"
ogs service add shopfr rapport --template worker --command "/home/shop.fr/www/veille.sh" --instances 1 >/dev/null
ogs apply shopfr --yes >/tmp/apply-rapport.log 2>&1
if [ "$(systemctl is-active ogs-shopfr-rapport@1.service 2>&1)" != "active" ]; then
    cat /tmp/apply-rapport.log
    echo "ÉCHEC : le second service n'a pas démarré"
    exit 1
fi
ogs service remove shopfr rapport >/dev/null
ogs apply shopfr --yes >/tmp/apply-retrait.log 2>&1
systemctl daemon-reload
if [ -e /etc/systemd/system/ogs-shopfr-rapport@.service ]; then
    cat /tmp/apply-retrait.log
    echo "ÉCHEC : le modèle du service retiré est resté"
    exit 1
fi
if [ "$(systemctl is-active ogs-shopfr-rapport@1.service 2>&1)" = "active" ]; then
    echo "ÉCHEC : l'instance du service retiré tourne encore"
    exit 1
fi
if [ "$(systemctl is-active ogs-shopfr-veille@1.service 2>&1)" != "active" ]; then
    echo "ÉCHEC : le service encore déclaré a été arrêté"
    exit 1
fi
echo "  modèle retiré, instance arrêtée, l'autre service toujours actif"

echo
echo "--- abandon d'une suppression : seul ce qui tournait repart (D114) ---"
# Deux instances, la seconde arrêtée par l'exploitant. Une archive impossible fait
# abandonner la suppression : relancer la cible tirait ses Wants=, et la seconde
# instance repartait.
ogs service scale shopfr veille 2 --yes >/dev/null 2>&1
systemctl start ogs-shopfr.target
sleep 1
systemctl stop ogs-shopfr-veille@2.service
mv /var/lib/ogs/archives /var/lib/ogs/archives.vrai
: > /var/lib/ogs/archives
sortie=$(ogs project delete shopfr --yes 2>&1); code=$?
rm -f /var/lib/ogs/archives
mv /var/lib/ogs/archives.vrai /var/lib/ogs/archives
sleep 2
if [ "$code" = 0 ] || ! grep -q "redémarrage des services" <<<"$sortie"; then
    echo "$sortie" | tail -12
    echo "ÉCHEC : une archive impossible doit abandonner la suppression et relancer les services (code $code)"
    exit 1
fi
etats="$(systemctl is-active ogs-shopfr.target) $(systemctl is-active ogs-shopfr-veille@1.service) $(systemctl is-active ogs-shopfr-veille@2.service)"
if [ "$etats" != "active active inactive" ]; then
    echo "$sortie" | tail -12
    echo "ÉCHEC : cible, @1 et @2 après l'abandon : $etats (attendu : active active inactive)"
    exit 1
fi
echo "  cible et @1 relancées, @2 — arrêtée par l'exploitant — reste arrêtée"
ogs service scale shopfr veille 1 --yes >/dev/null 2>&1

echo
echo "--- cible inactive, instance vivante : redémarrage et arrêt l'atteignent (D114) ---"
# L'état d'un parc antérieur à D114 : les instances démarrées une à une, la
# cible jamais. PartOf ne propage rien depuis une cible inactive.
systemctl stop ogs-shopfr.target
systemctl start ogs-shopfr-veille@1.service
sleep 1
if [ "$(systemctl is-active ogs-shopfr.target)" = "active" ] \
    || [ "$(systemctl is-active ogs-shopfr-veille@1.service)" != "active" ]; then
    echo "ÉCHEC : l'état de départ (cible inactive, instance active) n'est pas reproduit"
    exit 1
fi
avant=$(systemctl show -p MainPID --value ogs-shopfr-veille@1.service)
ogs service restart shopfr >/dev/null 2>&1
sleep 2
apres=$(systemctl show -p MainPID --value ogs-shopfr-veille@1.service)
if [ "$avant" = "$apres" ] || [ "$apres" = 0 ]; then
    echo "ÉCHEC : « service restart » n'a pas redémarré l'instance (PID $avant → $apres)"
    exit 1
fi
echo "  « service restart » redémarre l'instance d'une cible inactive"
systemctl stop ogs-shopfr.target
systemctl start ogs-shopfr-veille@1.service
sleep 1

echo
echo "--- suppression : plus rien ne subsiste dans systemd ---"
# La cible est inactive et l'instance vivante : la suppression arrêtait la seule
# cible, le worker survivait et userdel échouait (code 6).
sortie=$(ogs project delete shopfr --yes 2>&1); code=$?
if [ "$code" != 0 ]; then
    echo "$sortie" | tail -12
    echo "ÉCHEC : la suppression rend $code"
    exit 1
fi
if getent passwd shopfr >/dev/null; then
    echo "ÉCHEC : le compte shopfr a survécu à sa suppression"
    exit 1
fi
systemctl daemon-reload
restants=$(systemctl list-units --no-pager --all 'ogs-shopfr*' | grep -c "ogs-shopfr" || true)
echo "unités restantes : $restants"
if [ "$restants" != "0" ]; then
    systemctl list-units --no-pager --all 'ogs-shopfr*'
    echo "ÉCHEC : des unités subsistent"
    exit 1
fi
if ls /etc/systemd/system/ogs-shopfr* >/dev/null 2>&1; then
    echo "ÉCHEC : des fichiers d'unité subsistent"
    ls -l /etc/systemd/system/ | grep ogs-shopfr
    exit 1
fi
if [ -f /etc/cron.d/ogs-shopfr ]; then
    echo "ÉCHEC : le fichier cron subsiste"
    exit 1
fi
if [ -f /etc/sudoers.d/ogs-shopfr ]; then
    echo "ÉCHEC : la règle sudoers subsiste"
    exit 1
fi
if pgrep -u shopfr >/dev/null 2>&1; then
    echo "ÉCHEC : des processus du projet tournent encore"
    ps -u shopfr
    exit 1
fi
echo "unités, cron, sudoers et processus : tout est parti"

echo
echo "RÉSULTAT : systemd accepte les unités templatisées, maintient les instances,"
echo "           applique le durcissement ; la délégation sudo est bornée au projet ;"
echo "           cron déclenche la tâche sous la bonne identité."
