#!/bin/bash
# Éprouve la création complète d'un site PHP sur un système réel : utilisateur,
# arborescence, pool FPM, vhost, et vérifie que PHP est effectivement exécuté.
# Éprouve aussi le lien des clés autorisées vers le trousseau commun (D61) : le
# lchown et la préservation d'un authorized_keys posé à la main n'existent que sur
# un système réel, en mode simulé rien de tout cela n'atteint le disque.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : site PHP servi de bout en bout ==="
version=$(ls /etc/php | head -1)
echo "version PHP détectée : $version"

apache2ctl start
sleep 1

ogs init --quiet
choisir_moteur apache
ogs site create php.test --kind php-plain --php "$version" --tls=false --yes 2>&1 | tail -5

if [ ! -f "/etc/php/$version/fpm/pool.d/phptest.conf" ]; then
    echo "ÉCHEC : le pool n'a pas été écrit"
    exit 1
fi
echo "pool écrit :"
grep -E '^(user|group|listen) ' "/etc/php/$version/fpm/pool.d/phptest.conf" | sed 's/^/  /'

# La configuration produite doit être acceptée par les deux moteurs.
if ! "/usr/sbin/php-fpm$version" -t 2>&1 | tail -1; then
    echo "ÉCHEC : configuration FPM refusée"
    exit 1
fi
if ! apache2ctl configtest 2>&1 | tail -1; then
    echo "ÉCHEC : configuration Apache refusée"
    exit 1
fi

# Le shell du compte doit être celui déclaré (D62) : useradd le pose, et seul un
# système réel montre ce que /etc/passwd a réellement reçu.
shell_reel=$(getent passwd phptest | cut -d: -f7)
echo "shell du compte : $shell_reel"
if [ "$shell_reel" != /bin/bash ]; then
    echo "ÉCHEC : shell attendu /bin/bash, obtenu $shell_reel"
    exit 1
fi
# Et il est réconcilié : refermé à la main, l'apply le rouvre.
usermod --shell /usr/sbin/nologin phptest
ogs apply phptest --yes 2>&1 | grep -i "shell" | sed 's/^/  /'
shell_reel=$(getent passwd phptest | cut -d: -f7)
if [ "$shell_reel" != /bin/bash ]; then
    echo "ÉCHEC : le shell déclaré n'a pas été rétabli (obtenu $shell_reel)"
    exit 1
fi
echo "shell réconcilié après modification à la main"

# Trousseau de clés commun : posé avant la création, il doit être lié dans le .ssh
# du projet. Seul un système réel le voit — le mode préfixé n'a ni /opt ni root
# pour éprouver le lchown, et un lien appartenant à root reste indétectable en
# mode simulé.
echo 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOpErateur opérateur@parc' > /opt/default_authorized_keys
chown root:root /opt/default_authorized_keys
chmod 0644 /opt/default_authorized_keys
ogs site create keys.test --kind static --tls=false --yes 2>&1 | tail -3

lien=/home/keys.test/.ssh/authorized_keys
if [ ! -L "$lien" ]; then
    echo "ÉCHEC : $lien n'est pas un lien symbolique"
    ls -l /home/keys.test/.ssh/ 2>&1 | sed 's/^/  /'
    exit 1
fi
cible=$(readlink "$lien")
if [ "$cible" != /opt/default_authorized_keys ]; then
    echo "ÉCHEC : le lien pointe sur $cible"
    exit 1
fi
# lchown, non chown : le trousseau commun doit rester à root, faute de quoi sshd
# le refuserait sous StrictModes.
proprio_lien=$(stat -c '%U:%G' "$lien")
proprio_cible=$(stat -c '%U:%G %a' /opt/default_authorized_keys)
echo "clés autorisées : lien $proprio_lien → trousseau $proprio_cible"
if [ "$proprio_lien" != "keystest:keystest" ]; then
    echo "ÉCHEC : le lien devrait appartenir au projet, il appartient à $proprio_lien"
    exit 1
fi
if [ "$proprio_cible" != "root:root 644" ]; then
    echo "ÉCHEC : le trousseau commun a été altéré ($proprio_cible)"
    exit 1
fi
# Rejouer ne doit rien changer : le lien conforme sort du plan.
if ogs apply keystest --yes 2>&1 | grep -qi "authorized"; then
    echo "ÉCHEC : le lien déjà conforme reparaît dans le plan"
    exit 1
fi
# Un authorized_keys posé à la main porte l'accès de quelqu'un : il est préservé.
ogs site create garde.test --kind static --tls=false --yes >/dev/null 2>&1
rm -f /home/garde.test/.ssh/authorized_keys
echo 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIProPre propre@projet' \
    > /home/garde.test/.ssh/authorized_keys
ogs apply gardetest --yes >/dev/null 2>&1
code=$?
if [ ! -f /home/garde.test/.ssh/authorized_keys ] \
    || [ -L /home/garde.test/.ssh/authorized_keys ] \
    || ! grep -q "propre@projet" /home/garde.test/.ssh/authorized_keys; then
    echo "ÉCHEC : un authorized_keys posé à la main a été remplacé par le lien"
    exit 1
fi
echo "authorized_keys posé à la main préservé (apply rend $code)"

echo '<?php echo "php actif sous ", posix_getpwuid(posix_geteuid())["name"];' \
    > /home/php.test/www/index.php
chown phptest:phptest /home/php.test/www/index.php
gpasswd -a www-data phptest >/dev/null
"/usr/sbin/php-fpm$version" --daemonize
apache2ctl restart
sleep 2

# Le vhost du projet écoute sur le nom php.test.
reponse=$(curl -s -H 'Host: php.test' http://127.0.0.1/index.php)
echo "réponse : $reponse"
if [[ "$reponse" != *"php actif sous phptest"* ]]; then
    echo "RÉSULTAT : le contenu n'est pas servi comme attendu."
    exit 1
fi
echo "PHP est exécuté sous l'identité du projet, comme prévu."

echo
echo "--- surcharges de vhost : les deux contextes prennent effet (D63) ---"
# Ce qu'aucun test unitaire ne voit : Apache charge-t-il réellement ces fichiers,
# et dans le contexte annoncé ? Chaque fichier porte une directive que le
# **contexte voisin refuse** : si l'un des deux IncludeOptional se trouvait au
# mauvais endroit, apachectl le dirait — « not allowed here ».
surcharges=/etc/apache2/projects/phptest
if [ ! -d "$surcharges/vhost" ] || [ ! -d "$surcharges/docroot" ]; then
    echo "ÉCHEC : les répertoires de surcharge n'ont pas été créés"
    ls -la /etc/apache2/projects/ 2>&1 | head -5
    exit 1
fi
stat -c '%a %U:%G %n' "$surcharges/vhost" "$surcharges/docroot"

# ServerAlias n'est admis que dans un VirtualHost.
cat > "$surcharges/vhost/10-essai.conf" <<'CONF'
ServerAlias surcharge.test
CONF
# AllowOverride n'est admis que dans un Directory, et l'en-tête prouve l'effet.
cat > "$surcharges/docroot/10-essai.conf" <<'CONF'
AllowOverride None
Header always set X-Ogs-Docroot oui
CONF
if ! apache2ctl configtest 2>&1 | tail -2; then
    echo "ÉCHEC : les surcharges rendent la configuration invalide — l'un des"
    echo "        deux contextes n'est pas celui annoncé"
    exit 1
fi
apache2ctl graceful
sleep 1

reponse=$(curl -s -H 'Host: surcharge.test' http://127.0.0.1/index.php)
echo "contexte vhost   : $reponse"
if [[ "$reponse" != *"php actif sous phptest"* ]]; then
    echo "ÉCHEC : le ServerAlias déposé dans le contexte vhost n'est pas chargé"
    exit 1
fi
if ! curl -sI -H 'Host: php.test' http://127.0.0.1/index.php | grep -qi "X-Ogs-Docroot: oui"; then
    echo "ÉCHEC : la surcharge du contexte docroot n'est pas chargée"
    curl -sI -H 'Host: php.test' http://127.0.0.1/index.php | head -8
    exit 1
fi
echo "contexte docroot : X-Ogs-Docroot présent"

echo
echo "--- IncludeOptional : ni le répertoire ni le moindre fichier n'ont à exister ---"
# C'est ce qui distingue IncludeOptional d'Include : ce dernier échoue sur un
# joker sans correspondance, et Apache refuserait alors toute sa configuration.
rm -f "$surcharges"/vhost/*.conf "$surcharges"/docroot/*.conf
if ! apache2ctl configtest >/dev/null 2>&1; then
    echo "ÉCHEC : un répertoire de surcharge vide est refusé"
    exit 1
fi
rm -rf "$surcharges"
if ! apache2ctl configtest 2>&1 | tail -2; then
    echo "ÉCHEC : un répertoire de surcharge absent est refusé — l'inclusion"
    echo "        n'est pas facultative, et tout le parc en dépend"
    exit 1
fi
apache2ctl graceful
sleep 1
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: php.test' http://127.0.0.1/index.php)
echo "répertoire absent : Apache accepte, le site rend HTTP $code"
if [ "$code" != "200" ]; then
    echo "ÉCHEC : le site n'est plus servi"
    exit 1
fi

echo
echo "--- la suppression du projet laisse ce qu'ogs n'a pas écrit ---"
ogs apply phptest --yes >/dev/null 2>&1
echo 'Header always set X-Ogs-Garde oui' > "$surcharges/vhost/10-garde.conf"
sortie=$(ogs project delete phptest --yes 2>&1)
echo "$sortie" | grep -i "surcharge"
if [ ! -f "$surcharges/vhost/10-garde.conf" ]; then
    echo "ÉCHEC : une surcharge déposée à la main a été détruite avec le projet"
    exit 1
fi
if [ -d "$surcharges/docroot" ]; then
    echo "ÉCHEC : le répertoire vide devait partir avec le vhost"
    exit 1
fi
echo "répertoire vide retiré, fichier déposé conservé"

echo
echo "RÉSULTAT : PHP est exécuté sous l'identité du projet, et les surcharges de"
echo "           vhost sont chargées dans les deux contextes annoncés."
exit 0
