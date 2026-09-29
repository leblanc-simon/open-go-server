#!/bin/bash
# Éprouve le moteur nginx sur un vrai serveur : détection, validité de la
# configuration produite pour chaque type de site, exécution PHP par socket
# FastCGI, isolation, TLS, redirection, proxy applicatif, et la question restée
# ouverte pour ce moteur — un rechargement suffit-il à prendre en compte
# l'appartenance de www-data au groupe du projet ?
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : nginx ==="
version=$(ls /etc/php | head -1)

# Les deux moteurs sont installés : Apache doit céder le port 80.
apache2ctl stop >/dev/null 2>&1 || true
ogs init --quiet

echo "--- les deux moteurs sont présents : ogs doit refuser de choisir seul ---"
sortie=$(ogs web engine 2>&1)
echo "$sortie" | tail -2
if ! echo "$sortie" | grep -qiE "ambigu|deux|préciser|web.engine"; then
    echo "ÉCHEC : l'ambiguïté n'est pas signalée"
    exit 1
fi
# Le choix est explicite, comme il doit l'être sur une machine mixte.
choisir_moteur nginx
grep -A 1 "^web:" /etc/ogs/config.yaml
ogs web engine 2>&1 | tail -1

echo
echo "--- site statique en TLS auto-signé ---"
ogs site create static.test --kind static --self-signed --yes 2>&1 |
    grep -Ei "vhost|certificat|réglages|⚠|✗" | head -8
echo "bonjour statique" > /home/static.test/www/index.html
chown statictest:statictest /home/static.test/www/index.html

echo
echo "--- le fragment de portée serveur est-il écrit ? ---"
# Les vhosts de proxy emploient $connection_upgrade : sans ce fragment, nginx
# refuse toute sa configuration.
if [ ! -f /etc/nginx/conf.d/ogs.conf ]; then
    echo "ÉCHEC : aucun fragment de portée serveur"
    exit 1
fi
grep -c "connection_upgrade" /etc/nginx/conf.d/ogs.conf

echo
echo "--- site PHP et site applicatif derrière un proxy ---"
ogs site create php.test --kind php-symfony --app-version 6.4 --php "$version" --self-signed --yes -q >/dev/null 2>&1
ogs site create app.test --kind go --self-signed --yes -q >/dev/null 2>&1
# Une cible de proxy explicite : le provider applicatif la fournit en
# exploitation, ici on la déclare pour éprouver le vhost.
python3 - <<'PY'
chemin = "/etc/ogs/projects/apptest.yaml"
s = open(chemin).read()
if "listen: port" not in s:
    s = s.replace("  app:\n", "  app:\n    listen: port\n", 1)
open(chemin, "w").write(s)
PY
ogs apply apptest --yes 2>&1 | grep -Ei "vhost|proxy|⚠" | head -4

echo
echo "--- nginx accepte-t-il l'ensemble ? ---"
nginx -t 2>&1 | tail -2
if ! nginx -t >/dev/null 2>&1; then
    echo "ÉCHEC : configuration nginx invalide"
    nginx -T 2>&1 | grep -i "error\|unknown" | head -5
    exit 1
fi
ls /etc/nginx/sites-enabled/

echo
echo "--- démarrage et service des sites ---"
systemctl start nginx
systemctl start "php$version-fpm"
sleep 2
# www-data doit être membre des groupes pour traverser les homes.
for u in statictest phptest; do gpasswd -a www-data "$u" >/dev/null; done
systemctl reload nginx
sleep 1

code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: static.test' http://127.0.0.1/index.html)
echo "statique en HTTP  : $code (redirection attendue, le site est en TLS)"
if [ "$code" != "301" ]; then
    echo "ÉCHEC : le site en TLS doit rediriger le HTTP vers HTTPS"
    exit 1
fi
reponse=$(curl -sk --resolve static.test:443:127.0.0.1 https://static.test/index.html)
echo "statique en HTTPS : $reponse"
if [ "$reponse" != "bonjour statique" ]; then
    echo "ÉCHEC : le site statique n'est pas servi en HTTPS"
    curl -skv --resolve static.test:443:127.0.0.1 https://static.test/index.html 2>&1 | tail -8
    exit 1
fi

echo
echo "--- le challenge ACME reste servi en clair, malgré la redirection ---"
mkdir -p /var/lib/ogs/acme-challenge/.well-known/acme-challenge
echo jeton > /var/lib/ogs/acme-challenge/.well-known/acme-challenge/essai
recu=$(curl -s -H 'Host: static.test' http://127.0.0.1/.well-known/acme-challenge/essai)
echo "jeton servi en HTTP : $recu"
if [ "$recu" != "jeton" ]; then
    echo "ÉCHEC : le challenge ne serait pas servi, aucun certificat ne pourrait être renouvelé"
    exit 1
fi

echo
echo "--- PHP exécuté par le socket FastCGI, sous l'identité du projet ---"
mkdir -p /home/php.test/www/public
cat > /home/php.test/www/public/index.php <<'PHP'
<?php echo "php actif sous ", posix_getpwuid(posix_geteuid())["name"];
PHP
chown -R phptest:phptest /home/php.test/www
reponse=$(curl -sk --resolve php.test:443:127.0.0.1 https://php.test/)
echo "réponse : $reponse"
if [[ "$reponse" != *"php actif sous phptest"* ]]; then
    echo "ÉCHEC : PHP n'est pas servi sous l'identité du projet"
    tail -5 /home/php.test/logs/nginx-error.log 2>/dev/null
    exit 1
fi

echo
echo "--- aucun autre script PHP n'est exécutable ---"
# Un fichier déposé dans un répertoire d'upload ne doit pas devenir un point
# d'entrée : c'est la règle « location ~ \.php$ { return 404; } ».
mkdir -p /home/php.test/www/public/uploads
echo '<?php echo "exécuté"; ' > /home/php.test/www/public/uploads/piege.php
chown -R phptest:phptest /home/php.test/www/public/uploads
code=$(curl -sk -o /tmp/piege.out -w '%{http_code}' --resolve php.test:443:127.0.0.1 https://php.test/uploads/piege.php)
echo "HTTP $code, corps : $(head -c 40 /tmp/piege.out)"
if grep -q "exécuté" /tmp/piege.out; then
    echo "ÉCHEC : un script déposé dans un upload a été exécuté"
    exit 1
fi
if [ "$code" = "200" ] && [ -s /tmp/piege.out ]; then
    echo "ÉCHEC : le fichier PHP a été servi tel quel — son code source est exposé"
    exit 1
fi

echo
echo "--- vhost de proxy : nginx relaie, l'applicatif est absent ---"
code=$(curl -sk -o /dev/null -w '%{http_code}' --resolve app.test:443:127.0.0.1 https://app.test/)
echo "HTTP $code (502 attendu : le vhost est valide, l'applicatif ne tourne pas)"
if [ "$code" != "502" ] && [ "$code" != "504" ]; then
    echo "ÉCHEC : le vhost de proxy ne relaie pas comme prévu"
    tail -3 /home/app.test/logs/nginx-error.log 2>/dev/null
    exit 1
fi

echo
echo "--- domaine redirigé ---"
ogs site create ancien.test --kind static --alias vieux.test --tls=false --yes -q >/dev/null 2>&1
python3 - <<'PY'
# Un domaine de rôle « redirect » : c'est le vhost de redirection qu'on éprouve.
chemin = "/etc/ogs/projects/ancientest.yaml"
s = open(chemin).read()
s = s.replace("    - name: vieux.test\n      role: alias\n",
              "    - name: vieux.test\n      role: redirect\n      target: ancien.test\n")
open(chemin, "w").write(s)
PY
ogs apply ancientest --yes 2>&1 | grep -Ei "vhost|redirect" | head -3
systemctl reload nginx
sleep 1
cible=$(curl -s -o /dev/null -w '%{redirect_url}' -H 'Host: vieux.test' http://127.0.0.1/page)
echo "vieux.test/page → $cible"
if [[ "$cible" != *"ancien.test/page"* ]]; then
    echo "ÉCHEC : la redirection ne pointe pas vers le domaine principal"
    exit 1
fi

echo
echo "--- surcharges de vhost : les deux contextes prennent effet (D63) ---"
# Ce qu'aucun test unitaire ne voit : nginx charge-t-il réellement ces fichiers,
# et dans le contexte annoncé ?
surcharges=/etc/nginx/projects/statictest
if [ ! -d "$surcharges/vhost" ] || [ ! -d "$surcharges/docroot" ]; then
    echo "ÉCHEC : les répertoires de surcharge n'ont pas été créés"
    ls -la /etc/nginx/projects/ 2>&1 | head -5
    exit 1
fi
stat -c '%a %U:%G %n' "$surcharges/vhost" "$surcharges/docroot"

# Contexte du vhost : un location de premier niveau. S'il était inclus dans le
# « location / » d'ogs, ce chemin ne serait jamais atteint — nginx choisit le
# location de premier niveau avant d'examiner les imbriqués.
cat > "$surcharges/vhost/10-essai.conf" <<'CONF'
location = /surcharge-vhost {
    default_type "text/plain";
    return 200 "surcharge vhost\n";
}
CONF
# Contexte du répertoire servi. L'en-tête doit apparaître, et le HSTS du server
# doit disparaître sur ce seul chemin : un add_header dans un location **annule**
# tous ceux qu'il hérite, ce qui prouve que l'inclusion est bien dans le location
# et non au niveau du server.
cat > "$surcharges/docroot/10-essai.conf" <<'CONF'
add_header X-Ogs-Docroot oui always;
CONF
if ! nginx -t >/dev/null 2>&1; then
    echo "ÉCHEC : les surcharges rendent la configuration invalide"
    nginx -t 2>&1 | tail -3
    exit 1
fi
systemctl reload nginx
sleep 1

recu=$(curl -sk --resolve static.test:443:127.0.0.1 https://static.test/surcharge-vhost)
echo "contexte vhost   : $recu"
if [ "$recu" != "surcharge vhost" ]; then
    echo "ÉCHEC : la surcharge du contexte vhost n'est pas chargée"
    exit 1
fi
entetes=$(curl -skI --resolve static.test:443:127.0.0.1 https://static.test/index.html)
if ! echo "$entetes" | grep -qi "X-Ogs-Docroot: oui"; then
    echo "ÉCHEC : la surcharge du contexte docroot n'est pas chargée"
    echo "$entetes" | head -8
    exit 1
fi
echo "contexte docroot : X-Ogs-Docroot présent"
if echo "$entetes" | grep -qi "Strict-Transport-Security"; then
    echo "ÉCHEC : le HSTS survit à un add_header du location — l'inclusion du"
    echo "        contexte docroot n'est donc pas dans le location, ou nginx a"
    echo "        changé de sémantique (les add_header cumuleraient)"
    exit 1
fi
echo "le HSTS du server est bien annulé par l'add_header du location, comme"
echo "documenté : une surcharge du docroot doit reposer les en-têtes du vhost"

echo
echo "--- le masque rend l'inclusion facultative : rien n'a à exister ---"
# C'est tout l'intérêt de la forme « /*.conf » : un répertoire vide, puis retiré
# à la main, ne doit pas empêcher nginx de recharger. Un chemin littéral
# introuvable serait fatal, et tous les sites tomberaient.
rm -f "$surcharges/vhost/10-essai.conf" "$surcharges/docroot/10-essai.conf"
if ! nginx -t >/dev/null 2>&1; then
    echo "ÉCHEC : un répertoire de surcharge vide est refusé"
    exit 1
fi
rm -rf "$surcharges"
if ! nginx -t >/dev/null 2>&1; then
    echo "ÉCHEC : un répertoire de surcharge absent est refusé — l'inclusion"
    echo "        n'est pas facultative, et tout le parc en dépend"
    nginx -t 2>&1 | tail -3
    exit 1
fi
systemctl reload nginx
sleep 1
code=$(curl -sk -o /dev/null -w '%{http_code}' --resolve static.test:443:127.0.0.1 https://static.test/index.html)
echo "répertoire absent : nginx accepte, le site rend HTTP $code"
if [ "$code" != "200" ]; then
    echo "ÉCHEC : le site n'est plus servi"
    exit 1
fi

echo
echo "--- doctor voit ce que nginx ne dit pas ---"
ogs apply statictest --yes >/dev/null 2>&1
# Un fichier hors des sous-répertoires de contexte : chargé par personne, et
# rien ne le signale — le site fonctionne, la surcharge est sans effet.
echo '# jamais lu' > /etc/nginx/projects/statictest/perdu.conf
# Un fichier inscriptible par tous, lu en root par le maître : quiconque peut
# l'écrire dicte la configuration du serveur.
echo 'add_header X-Ogs-Essai oui always;' > /etc/nginx/projects/statictest/vhost/20-ouvert.conf
chmod 0666 /etc/nginx/projects/statictest/vhost/20-ouvert.conf
sortie=$(ogs doctor 2>&1)
echo "$sortie" | grep -i "surcharge" | sed 's/  */ /g' | head -4
if ! echo "$sortie" | grep -q "perdu.conf"; then
    echo "ÉCHEC : le fichier hors contexte n'est pas signalé"
    exit 1
fi
if ! echo "$sortie" | grep -q "20-ouvert.conf"; then
    echo "ÉCHEC : un fichier de surcharge inscriptible par tous n'est pas signalé"
    exit 1
fi
rm -rf /etc/nginx/projects/statictest

echo
echo "--- un vhost posé à la main selon la convention n'est jamais retiré ---"
# L'attribution par préfixe rangeait « statictest_maintenance » parmi les vhosts
# obsolètes du projet : son apply le retirait, lien compris, sans --force ni
# copie. Seul l'en-tête d'ogs rattache désormais un fichier à un projet.
cat > /etc/nginx/sites-available/statictest_maintenance <<'CONF'
server {
    listen 80;
    server_name maintenance.static.test;
    return 503;
}
CONF
ln -sf ../sites-available/statictest_maintenance /etc/nginx/sites-enabled/statictest_maintenance
# Le code est vérifié : un apply en échec pour une autre raison laisserait aussi
# le fichier en place, et l'assertion passerait pour rien.
sortie=$(ogs apply statictest --yes 2>&1)
code=$?
if [ "$code" != "0" ] && [ "$code" != "6" ]; then
    echo "ÉCHEC : apply statictest a rendu le code $code"
    echo "$sortie" | tail -5
    exit 1
fi
if echo "$sortie" | grep -q "statictest_maintenance"; then
    echo "ÉCHEC : le plan de statictest s'occupe d'un vhost qu'ogs n'a pas écrit :"
    echo "$sortie" | grep "statictest_maintenance"
    exit 1
fi
if [ ! -f /etc/nginx/sites-available/statictest_maintenance ] ||
    [ ! -L /etc/nginx/sites-enabled/statictest_maintenance ]; then
    echo "ÉCHEC : un vhost non géré, nommé selon la convention, a été retiré par l'apply du projet"
    exit 1
fi
echo "vhost posé à la main et son lien : en place"
rm -f /etc/nginx/sites-enabled/statictest_maintenance /etc/nginx/sites-available/statictest_maintenance

echo
echo "--- un rechargement suffit-il pour l'appartenance de groupe ? ---"
# La même question que pour Apache, jamais éprouvée sur nginx : les workers
# rappellent-ils initgroups() au fork ?
gpasswd -d www-data statictest >/dev/null
systemctl restart nginx
sleep 1
avant=$(curl -sk -o /dev/null -w '%{http_code}' --resolve static.test:443:127.0.0.1 https://static.test/index.html)
# nginx répond 404 là où Apache répond 403 : ne pouvant pas traverser le home, il
# ne trouve pas le fichier. Ce qui compte est qu'il ne serve pas le contenu.
echo "hors du groupe        : HTTP $avant"
if [ "$avant" = "200" ]; then
    echo "ÉCHEC : le contenu est servi sans appartenance au groupe"
    exit 1
fi
gpasswd -a www-data statictest >/dev/null
systemctl reload nginx
sleep 2
apres=$(curl -sk -o /dev/null -w '%{http_code}' --resolve static.test:443:127.0.0.1 https://static.test/index.html)
echo "après un rechargement : HTTP $apres"
if [ "$apres" != "200" ]; then
    echo "RÉSULTAT PARTIEL : sur nginx, un rechargement NE SUFFIT PAS — un"
    echo "                   redémarrage est nécessaire, donc une coupure."
    systemctl restart nginx
    sleep 1
    apres=$(curl -sk -o /dev/null -w '%{http_code}' --resolve static.test:443:127.0.0.1 https://static.test/index.html)
    echo "après un redémarrage  : HTTP $apres"
    exit 2
fi
echo "un rechargement suffit, comme sur Apache"

echo
echo "--- suppression : le vhost part et la configuration reste valide ---"
ogs project delete phptest --yes >/dev/null 2>&1
if ls /etc/nginx/sites-enabled/ | grep -q phptest; then
    echo "ÉCHEC : le vhost subsiste"
    exit 1
fi
nginx -t 2>&1 | tail -1
code=$(curl -sk -o /dev/null -w '%{http_code}' --resolve php.test:443:127.0.0.1 https://php.test/ 2>/dev/null)
echo "php.test après suppression : HTTP $code"
if [ "$code" = "200" ]; then
    echo "ÉCHEC : le site supprimé est encore servi"
    exit 1
fi

echo
echo "RÉSULTAT : nginx sert les quatre types de vhost, exécute PHP par socket"
echo "           sous l'identité du projet, refuse les scripts déposés, et"
echo "           prend en compte l'appartenance de groupe sur rechargement."
