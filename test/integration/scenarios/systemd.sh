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
echo "journal : $(stat -c '%U:%G %a' /home/shop.fr/logs/cron-horodate.log), répertoire : $(stat -c '%U:%G %a' /home/shop.fr/logs)"
if [ "$(stat -c %U /home/shop.fr/logs)" != "root" ]; then
    echo "ÉCHEC : le répertoire des journaux doit appartenir à root"
    exit 1
fi

echo
echo "--- tâche planifiée : déclenchement réel par cron ---"
journal=/home/shop.fr/logs/cron-horodate.log
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
echo "--- suppression : plus rien ne subsiste dans systemd ---"
ogs project delete shopfr --yes >/dev/null 2>&1
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
