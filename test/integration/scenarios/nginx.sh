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
# Le front controller répond 404 sur /absent, pour la page d'erreur éprouvée plus
# bas. Il est écrit une fois pour toutes : FPM tourne avec l'opcache
# (revalidate_freq=2), et un script réécrit puis interrogé dans les deux
# secondes répond encore par l'ancien code.
cat > /home/php.test/www/public/index.php <<'PHP'
<?php if ($_SERVER["REQUEST_URI"] === "/absent") { http_response_code(404); }
echo "php actif sous ", posix_getpwuid(posix_geteuid())["name"];
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
echo "--- l'accueil reste servi sous disable_symlinks ---"
# « / » désigne le docroot lui-même, en 0710 (D51) : sous disable_symlinks,
# nginx n'éprouve plus un répertoire par stat() mais l'ouvre en lecture, ce que
# www-data ne peut pas. try_files manquerait l'accueil — 404 sur un site
# statique, une ligne « crit » par visite sur un site PHP, dont l'accueil est
# éprouvé plus haut.
recu=$(curl -sk --resolve static.test:443:127.0.0.1 https://static.test/)
echo "static.test/ : $recu"
if [ "$recu" != "bonjour statique" ]; then
    echo "ÉCHEC : l'accueil du site statique n'est plus servi"
    exit 1
fi
# Sans try_files, un répertoire demandé sans barre finale est redirigé vers son
# nom qui la porte, comme sous Apache, puis servi par son index.
mkdir -p /home/static.test/www/doc
echo 'index du répertoire' > /home/static.test/www/doc/index.html
chown -R statictest:statictest /home/static.test/www/doc
cible=$(curl -sk -o /dev/null -w '%{http_code} %{redirect_url}' --resolve static.test:443:127.0.0.1 https://static.test/doc)
recu=$(curl -sk --resolve static.test:443:127.0.0.1 https://static.test/doc/)
echo "static.test/doc : $cible ; /doc/ : $recu"
if [ "$cible" != "301 https://static.test/doc/" ] || [ "$recu" != "index du répertoire" ]; then
    echo "ÉCHEC : un répertoire du site statique n'est plus servi par son index"
    exit 1
fi
rm -rf /home/static.test/www/doc
# L'accueil d'un site PHP atteint le front controller quelle que soit la méthode :
# le module index ne traite que GET, HEAD et POST, et une location qui lui
# confierait « / » répondrait 405 au pré-vol CORS d'une API servie à la racine.
for methode in OPTIONS PUT DELETE PATCH; do
    code=$(curl -sk -o /tmp/methode.out -w '%{http_code}' -X "$methode" --resolve php.test:443:127.0.0.1 https://php.test/)
    echo "$methode php.test/ : HTTP $code"
    if ! grep -q "php actif sous phptest" /tmp/methode.out; then
        echo "ÉCHEC : $methode sur l'accueil n'atteint pas le front controller"
        exit 1
    fi
done
if grep -h "Permission denied" /var/log/ogs-projects/statictest/nginx-error.log \
    /var/log/ogs-projects/phptest/nginx-error.log 2>/dev/null | grep -q "/www"; then
    echo "ÉCHEC : nginx tente d'ouvrir en lecture un répertoire du projet"
    grep -h "Permission denied" /var/log/ogs-projects/*/nginx-error.log | tail -2
    exit 1
fi

echo
echo "--- l'accueil reste sous les surcharges du docroot (D63) ---"
# Le contexte du docroot est celui de location / : ce qu'une surcharge y dépose
# vaut pour « / » comme pour le reste du site. Une location dédiée à l'accueil
# l'y soustrairait — un contrôle d'accès protégerait /autre-page, et laisserait
# servir l'accueil —, et y inclure le contexte rendrait fatale toute location
# qu'une surcharge imbrique.
recharger_nginx() {
    if ! nginx -t 2>/tmp/nginx-t.out; then
        echo "ÉCHEC : nginx refuse la configuration"
        tail -2 /tmp/nginx-t.out
        exit 1
    fi
    systemctl reload nginx
    sleep 1
}
docroot_php=/etc/nginx/projects/phptest/docroot
echo 'deny all;' > "$docroot_php/10-essai.conf"
recharger_nginx
for chemin in / /autre-page; do
    code=$(curl -sk -o /tmp/surcharge.out -w '%{http_code}' --resolve php.test:443:127.0.0.1 "https://php.test$chemin")
    echo "deny all, php.test$chemin : HTTP $code"
    if [ "$code" != "403" ] || grep -q "php actif" /tmp/surcharge.out; then
        echo "ÉCHEC : le contrôle d'accès du docroot ne protège pas $chemin"
        exit 1
    fi
done
# Une directive de réécriture — maintenance — vaut aussi pour l'accueil.
echo 'return 503;' > "$docroot_php/10-essai.conf"
recharger_nginx
code=$(curl -sk -o /dev/null -w '%{http_code}' --resolve php.test:443:127.0.0.1 https://php.test/)
echo "return 503, php.test/ : HTTP $code"
if [ "$code" != "503" ]; then
    echo "ÉCHEC : la maintenance déposée dans le contexte du docroot épargne l'accueil"
    exit 1
fi
# Une location imbriquée reste admise : nginx refuse une location préfixe sous
# une regex ou une correspondance exacte (« location "/prive/" is outside
# location "^/$" »), et le parc entier avec elle.
printf 'location /prive/ {\n    deny all;\n}\n' > "$docroot_php/10-essai.conf"
recharger_nginx
recu=$(curl -sk --resolve php.test:443:127.0.0.1 https://php.test/)
if [[ "$recu" != *"php actif sous phptest"* ]]; then
    echo "ÉCHEC : l'accueil n'est plus servi avec une location imbriquée"
    exit 1
fi
rm -f "$docroot_php/10-essai.conf"
# Une page d'erreur du front controller, reprise par nginx : la redirection
# interne change l'URI, et $ogs_fichier doit la suivre — une valeur gardée de la
# première évaluation renverrait au front controller au lieu de la page. Le
# témoin : sans interception, /absent rend bien le 404 du front controller.
code=$(curl -sk -o /tmp/absent.out -w '%{http_code}' --resolve php.test:443:127.0.0.1 https://php.test/absent)
echo "php.test/absent sans interception : HTTP $code, $(cat /tmp/absent.out)"
if [ "$code" != "404" ] || ! grep -q "php actif sous phptest" /tmp/absent.out; then
    echo "ÉCHEC : le front controller ne rend pas son 404, l'essai serait creux"
    exit 1
fi
echo 'page erreur' > /home/php.test/www/public/erreur.html
chown phptest:phptest /home/php.test/www/public/erreur.html
printf 'fastcgi_intercept_errors on;\nerror_page 404 /erreur.html;\n' > /etc/nginx/projects/phptest/vhost/10-essai.conf
recharger_nginx
recu=$(curl -sk --resolve php.test:443:127.0.0.1 https://php.test/absent)
echo "page d'erreur de php.test/absent : $recu"
rm -f /etc/nginx/projects/phptest/vhost/10-essai.conf /home/php.test/www/public/erreur.html
recharger_nginx
if [ "$recu" != "page erreur" ]; then
    echo "ÉCHEC : la page d'erreur statique n'est pas servie après une redirection interne"
    exit 1
fi

echo
echo "--- les fichiers cachés et les métadonnées du dépôt ne sont pas servis ---"
# Le dépôt tiré dans www/ en est la racine, et www/ est le docroot d'un site
# statique : .git, .env et composer.json y sont à portée de requête.
racine=/home/static.test/www
mkdir -p "$racine/.git" "$racine/.well-known" "$racine/sous"
echo '[remote "origin"] SECRET-OGS' > "$racine/.git/config"
echo 'DATABASE_URL=SECRET-OGS' > "$racine/.env"
echo 'DATABASE_URL=SECRET-OGS' > "$racine/sous/.env"
echo '{"name": "SECRET-OGS"}' > "$racine/composer.json"
echo '{"packages": "SECRET-OGS"}' > "$racine/composer.lock"
echo 'Contact: mailto:securite@static.test' > "$racine/.well-known/security.txt"
chown -R statictest:statictest "$racine/.git" "$racine/.well-known" "$racine/sous" \
    "$racine/.env" "$racine/composer.json" "$racine/composer.lock"
for chemin in /.git/config /.env /sous/.env /composer.json /composer.lock; do
    code=$(curl -sk -o /tmp/cache.out -w '%{http_code}' --resolve static.test:443:127.0.0.1 "https://static.test$chemin")
    echo "$chemin : HTTP $code"
    if [ "$code" = "200" ] || grep -q "SECRET-OGS" /tmp/cache.out; then
        echo "ÉCHEC : $chemin est servi"
        exit 1
    fi
done
# /.well-known/ (RFC 8615) reste servi depuis le docroot, et le challenge ACME
# depuis son webroot — il est éprouvé plus haut.
recu=$(curl -sk --resolve static.test:443:127.0.0.1 https://static.test/.well-known/security.txt)
echo "/.well-known/security.txt : $recu"
if [ "$recu" != "Contact: mailto:securite@static.test" ]; then
    echo "ÉCHEC : /.well-known/ n'est plus servi"
    exit 1
fi
# Symfony sert www/public : un .env déposé par erreur dans le docroot est refusé
# de même.
echo 'APP_SECRET=SECRET-OGS' > /home/php.test/www/public/.env
chown phptest:phptest /home/php.test/www/public/.env
if curl -sk --resolve php.test:443:127.0.0.1 https://php.test/.env | grep -q "SECRET-OGS"; then
    echo "ÉCHEC : le .env du docroot Symfony est servi"
    exit 1
fi
rm -rf "$racine/.git" "$racine/sous" "$racine/.env" "$racine/composer.json" "$racine/composer.lock" \
    /home/php.test/www/public/.env

echo
echo "--- un lien vers les fichiers d'un autre projet n'est pas suivi ---"
# www-data est membre du groupe de chaque projet (D25) : suivre un lien posé dans
# le docroot de static.test lui ferait servir les fichiers de php.test, que le
# compte de static.test ne peut pas lire. Le site statique n'a pas de try_files :
# nginx répond 403, comme Apache. Un site PHP retomberait sur son front
# controller ; ce qui compte alors est que le contenu ne sorte pas.
echo 'SECRET-DE-PHPTEST' > /home/php.test/www/config.txt
chown phptest:phptest /home/php.test/www/config.txt
chmod 0640 /home/php.test/www/config.txt
# Le témoin : www-data lit bien ce fichier par son groupe — sans quoi le refus ne
# prouverait rien.
if ! su -s /bin/sh -c 'cat /home/php.test/www/config.txt' www-data | grep -q SECRET-DE-PHPTEST; then
    echo "ÉCHEC : www-data ne lit pas le fichier de phptest, l'essai serait creux"
    exit 1
fi
echo 'page interne' > /home/static.test/www/interne.txt
chown statictest:statictest /home/static.test/www/interne.txt
su -s /bin/sh -c 'cd /home/static.test/www && ln -s /home/php.test/www/config.txt vol.txt &&
    ln -s /home/php.test/www voisin && ln -s interne.txt lien.txt' statictest
for chemin in /vol.txt /voisin/config.txt; do
    code=$(curl -sk -o /tmp/lien.out -w '%{http_code}' --resolve static.test:443:127.0.0.1 "https://static.test$chemin")
    echo "$chemin : HTTP $code"
    if [ "$code" != "403" ] || grep -q SECRET-DE-PHPTEST /tmp/lien.out; then
        echo "ÉCHEC : un lien du docroot de statictest sert les fichiers de phptest"
        exit 1
    fi
done
# Un lien vers ses propres fichiers reste servi : bundles Symfony, médias.
recu=$(curl -sk --resolve static.test:443:127.0.0.1 https://static.test/lien.txt)
echo "/lien.txt : $recu"
if [ "$recu" != "page interne" ]; then
    echo "ÉCHEC : un lien vers les fichiers du projet lui-même n'est plus servi"
    exit 1
fi
# Le docroot lui-même remplacé par un lien : le home appartient à statictest
# (D51), qui renomme www sans le moindre privilège.
su -s /bin/sh -c 'mv /home/static.test/www /home/static.test/www.orig &&
    ln -s /home/php.test/www /home/static.test/www' statictest
code=$(curl -sk -o /tmp/lien.out -w '%{http_code}' --resolve static.test:443:127.0.0.1 https://static.test/config.txt)
echo "docroot remplacé par un lien, /config.txt : HTTP $code"
rm /home/static.test/www && mv /home/static.test/www.orig /home/static.test/www
if [ "$code" != "403" ] || grep -q SECRET-DE-PHPTEST /tmp/lien.out; then
    echo "ÉCHEC : le docroot remplacé par un lien sert les fichiers de phptest"
    exit 1
fi
rm -f /home/static.test/www/vol.txt /home/static.test/www/voisin /home/static.test/www/lien.txt \
    /home/static.test/www/interne.txt /home/php.test/www/config.txt

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
