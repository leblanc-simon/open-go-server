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
