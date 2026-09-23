#!/bin/bash
# Éprouve les runtimes applicatifs sur un système réel, avec systemd en PID 1.
#
# Go et Node sont installés pour de vrai : Go n'a pas de gestionnaire — le
# déploiement dépose un binaire — et nvm télécharge une version précompilée.
# Ruby et Python compilent depuis les sources, ce qui prend un quart d'heure et
# des dépendances de compilation : ils ont leur propre scénario,
# « runtimes-compiles », volontairement hors de la série ordinaire.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : runtimes applicatifs ==="
ogs init --quiet
choisir_moteur apache
apache2ctl start >/dev/null 2>&1

echo "--- ce que ogs annonce ---"
ogs runtime list 2>&1 | head -8
ogs app types 2>&1 | head -12

echo
echo "--- Go : le déploiement dépose un binaire, ogs le supervise ---"
ogs site create app.test --kind go --tls=false --yes 2>&1 |
    grep -Ei "unité|application|attente|⚠|✗" | head -6
# L'application arrive après la création du site, comme dans la vraie vie : le
# service ne peut pas démarrer avant que le binaire existe.
install -o apptest -g apptest -m 0750 /depot/app /home/app.test/www/app
ogs apply apptest --yes 2>&1 | grep -Ei "application|unité|attente|⚠" | head -5

echo
echo "--- le lien géré désigne-t-il le binaire ? ---"
ls -l /home/app.test/.local/bin/app
cible=$(readlink -f /home/app.test/.local/bin/app)
if [ "$cible" != "/home/app.test/www/app" ]; then
    echo "ÉCHEC : .local/bin/app ne désigne pas le binaire déposé ($cible)"
    exit 1
fi

echo
echo "--- le service tourne-t-il, et sur quoi écoute-t-il ? ---"
# Go écoute sur un port par défaut : un binaire n'a pas de convention de socket,
# et l'imposer supposerait une coopération du code applicatif (D36).
systemctl is-active 'ogs-apptest-app@1.service' || {
    echo "ÉCHEC : le service applicatif n'est pas actif"
    systemctl status 'ogs-apptest-app@1.service' --no-pager -l 2>&1 | tail -15
    exit 1
}
identite=$(ps -o user= -p "$(systemctl show -p MainPID --value 'ogs-apptest-app@1.service')")
echo "processus sous : $identite"
if [ "$identite" != "apptest" ]; then
    echo "ÉCHEC : l'application ne tourne pas sous l'identité du projet"
    exit 1
fi
echo "écoute :"
ss -ltnp 2>/dev/null | grep 21001 || ss -ltn | grep 21001
# Sur la boucle locale exclusivement : une écoute publique contournerait le
# vhost, donc le TLS et l'authentification.
if ss -ltn 2>/dev/null | grep 21001 | grep -qv "127.0.0.1"; then
    echo "ÉCHEC : l'application écoute au-delà de la boucle locale"
    exit 1
fi

echo
echo "--- l'application est-elle servie par le vhost ? ---"
gpasswd -a www-data apptest >/dev/null
systemctl restart apache2
sleep 2
reponse=$(curl -s -H 'Host: app.test' http://127.0.0.1/)
echo "réponse : $reponse"
if [[ "$reponse" != *"application sous apptest"* ]]; then
    echo "ÉCHEC : l'application n'est pas relayée"
    tail -5 /home/app.test/logs/apache-error.log 2>/dev/null
    exit 1
fi

echo
echo "--- bascule sur un socket Unix ---"
# L'autre branche de D36 : le socket, dont les droits restreignent l'accès au
# serveur web et au projet, là où un port local est joignable par tout
# utilisateur de la machine.
python3 - <<'PYEOF'
chemin = "/etc/ogs/projects/apptest.yaml"
s = open(chemin).read()
open(chemin, "w").write(s.replace("  app:\n", "  app:\n    listen: socket\n", 1))
PYEOF
ogs apply apptest --yes 2>&1 | grep -Ei "unité applicative|attente|⚠" | head -4
socket=/run/ogs/apptest/app.sock
if [ ! -S "$socket" ]; then
    echo "ÉCHEC : aucun socket d'écoute"
    ls -l /run/ogs/apptest/ 2>&1
    exit 1
fi
droits=$(stat -c '%a %U:%G' "$socket")
echo "socket : $droits"
if [ "${droits%% *}" != "660" ]; then
    echo "ÉCHEC : le socket doit être en 0660 pour n'être joignable que du serveur web"
    exit 1
fi
systemctl reload apache2
sleep 1
if [[ "$(curl -s -H 'Host: app.test' http://127.0.0.1/)" != *"unix:"* ]]; then
    echo "ÉCHEC : l'application n'est pas servie par le socket"
    curl -s -H 'Host: app.test' http://127.0.0.1/ | head -3
    tail -3 /home/app.test/logs/apache-error.log 2>/dev/null
    exit 1
fi
echo "servie par le socket ; le port n'est plus écouté ($(ss -ltn | grep -c 21001) occurrence)"
# Et doctor ne doit plus rien signaler : aucune écoute applicative en TCP.
ogs doctor 2>&1 | grep -i "écoute applicative" || echo "(doctor ne signale aucune écoute exposée)"
echo
echo "--- redémarrage par l'utilisateur du projet ---"
# Un projet applicatif doit recevoir sa règle sudoers comme un autre : redémarrer
# son application après un déploiement est le geste le plus courant.
visudo -c -f /etc/sudoers.d/ogs-apptest
sortie=$(su -s /bin/bash apptest -c 'sudo -n ogs my restart' 2>&1)
echo "$sortie" | tail -3
if echo "$sortie" | grep -qi "password\|not allowed\|refusé"; then
    echo "ÉCHEC : la délégation ne couvre pas l'application"
    exit 1
fi
sleep 2
if [ "$(systemctl is-active 'ogs-apptest-app@1.service')" != "active" ]; then
    echo "ÉCHEC : le service n'a pas redémarré"
    exit 1
fi
if [[ "$(curl -s -H 'Host: app.test' http://127.0.0.1/)" != *"application sous apptest"* ]]; then
    echo "ÉCHEC : l'application n'est plus servie après redémarrage"
    exit 1
fi
echo "le socket est recréé et le service resservi"

echo
echo "--- Go n'a pas de gestionnaire de versions ---"
sortie=$(ogs runtime install apptest 1.23 2>&1)
echo "$sortie" | tail -2
if ! echo "$sortie" | grep -qi "gestionnaire de versions"; then
    echo "ÉCHEC : le refus doit être expliqué"
    exit 1
fi

echo
echo "--- Node : installation réelle par nvm, dans le home du projet ---"
# La version du runtime est donnée à la création : sans elle, la déclaration est
# refusée à l'apply.
ogs site create node.test --kind node --runtime 22.14.0 --tls=false --yes 2>&1 |
    grep -Ei "runtime|unité|⚠|✗" | head -5
if ! id nodetest >/dev/null 2>&1; then
    echo "ÉCHEC : le compte du projet n'a pas été créé"
    exit 1
fi
echo "installation en cours (téléchargement d'une version précompilée)…"
if ! ogs runtime install nodetest 22.14.0 2>&1 | tail -3; then
    echo "ÉCHEC : l'installation a échoué"
    exit 1
fi
binaire=/home/node.test/.nvm/versions/node/v22.14.0/bin/node
if [ ! -x "$binaire" ]; then
    echo "ÉCHEC : le binaire n'est pas là où ogs l'attend"
    find /home/node.test/.nvm -maxdepth 4 -name node 2>/dev/null | head -5
    exit 1
fi
echo "binaire : $binaire"
echo "propriétaire : $(stat -c '%U:%G' "$binaire")"
if [ "$(stat -c %U "$binaire")" != "nodetest" ]; then
    echo "ÉCHEC : le runtime n'appartient pas au projet — il a été installé en root"
    exit 1
fi
# Le runtime vit dans le home : aucun autre projet ne le partage (D35).
if [ -e /home/app.test/.nvm ]; then
    echo "ÉCHEC : le runtime a fui hors du projet"
    exit 1
fi
echo "version rapportée : $(su -s /bin/bash nodetest -c "$binaire --version")"

echo
echo "--- le lien géré suit la version installée ---"
ls -l /home/node.test/.local/bin/node
if [ "$(readlink -f /home/node.test/.local/bin/node)" != "$binaire" ]; then
    echo "ÉCHEC : .local/bin/node ne désigne pas la version installée"
    exit 1
fi
echo "version vue par ogs : $(ogs runtime list 2>&1 | grep nodetest || echo '(non listée)')"

echo
echo "--- une version inconnue échoue proprement ---"
sortie=$(ogs runtime install nodetest 999.999.999 2>&1)
echo "$sortie" | tail -2
if ! echo "$sortie" | grep -qiE "échec|introuvable|version"; then
    echo "ÉCHEC : l'erreur n'est pas explicite"
    exit 1
fi
# Et le lien ne doit pas avoir bougé.
if [ "$(readlink -f /home/node.test/.local/bin/node)" != "$binaire" ]; then
    echo "ÉCHEC : un échec d'installation a cassé le lien de la version en service"
    exit 1
fi
echo "le lien de la version en service est intact"

echo
echo "--- suppression : le runtime part avec le home ---"
# Sans « --purge » : celui-ci exige une confirmation interactive, car il est sans
# recours. La suppression ordinaire archive puis démonte.
ogs project delete nodetest --yes 2>&1 | tail -2
if [ -d /home/node.test ]; then
    echo "ÉCHEC : le home subsiste — le runtime resterait sur le disque"
    ls -a /home/node.test | head -5
    exit 1
fi
ogs project delete apptest --yes >/dev/null 2>&1
if systemctl list-units --all 'ogs-apptest*' | grep -q ogs-apptest; then
    echo "ÉCHEC : des unités subsistent"
    exit 1
fi
if [ -e /run/ogs/apptest ]; then
    echo "ÉCHEC : le répertoire d'exécution subsiste"
    exit 1
fi
echo "homes, unités et répertoires d'exécution supprimés"

echo
echo "RÉSULTAT : une application Go est supervisée, écoute sur un socket 0660 sous"
echo "           l'identité du projet et est servie par le vhost ; nvm installe"
echo "           Node dans le home du projet, là où ogs l'attend."
