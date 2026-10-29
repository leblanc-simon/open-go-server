#!/bin/bash
# Éprouve le moteur nginx sur un vrai serveur : détection, validité de la
# configuration produite pour chaque type de site, exécution PHP par socket
# FastCGI, isolation, TLS, redirection, proxy applicatif, et la question restée
# ouverte pour ce moteur — un rechargement suffit-il à prendre en compte
# l'appartenance de www-data au groupe du projet ?
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : nginx ==="
version=$(version_php)

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
echo "--- valeurs de pool citées : FPM lit ce que la déclaration porte (D115) ---"
# L'analyseur d'ini de FPM lit « ; » comme un commentaire, développe « ${…} »,
# évalue les opérateurs et les mots-clés, refuse « = » : chaque valeur doit
# parvenir telle quelle au script, sauf la citation de l'exploitant (D103) et
# l'expression de constantes, que FPM évalue comme avant.
python3 - <<'PY'
import json
chemin = "/etc/ogs/projects/phptest.yaml"
s = open(chemin).read()
env = {"V1": ".:/a;/b", "V2": "a${HOME}x", "V3": "l'a", "V4": "a & b", "V5": "no",
       "V6": "a=b", "V7": "x$y", "V8": '"x" ${HOME}', "V9": "E_ALL", "V10": "a/b~c (d) !e"}
settings = {"php_value[highlight.string]": "a=b ; c", "php_value[highlight.comment]": "l'apostrophe ~ (x)",
            "php_value[highlight.keyword]": '"/usr/sbin/sendmail -t -i"',
            "php_value[error_reporting]": "E_ALL & ~E_DEPRECATED",
            "php_value[session.gc_maxlifetime]": "3"}
bloc = "        settings:\n" + "".join("          %s: %s\n" % (json.dumps(k), json.dumps(v)) for k, v in settings.items())
# « $PATH » relaie la variable du maître : syntaxe de FPM, jamais refusée.
bloc += "        env:\n" + "".join("          %s: %s\n" % (k, json.dumps(v)) for k, v in dict(env, RELAIS="$PATH").items())
if "highlight.string" not in s:
    s = s.replace("      - name: web\n", "      - name: web\n" + bloc, 1)
open(chemin, "w").write(s)
json.dump({"env": env, "highlight.string": settings["php_value[highlight.string]"],
           "highlight.comment": settings["php_value[highlight.comment]"],
           "highlight.keyword": "/usr/sbin/sendmail -t -i"}, open("/tmp/attendu.json", "w"))
PY
# Sans init, le rechargement par le substitut peut échouer (code 6) : FPM est
# relancé plus bas par son pidfile. Seul un refus de la déclaration compte.
code=0
ogs apply phptest --yes >/tmp/apply-valeurs.out 2>&1 || code=$?
if [ "$code" != 0 ] && [ "$code" != 6 ]; then
    echo "ÉCHEC : apply des valeurs citées refusé (code $code)"
    cat /tmp/apply-valeurs.out
    exit 1
fi
grep -E "^(env\[V|php_value\[(highlight|error))" "/etc/php/$version/fpm/pool.d/phptest.conf"
php-fpm"$version" -t >/dev/null 2>&1 || { echo "ÉCHEC : php-fpm -t refuse le pool rendu"; php-fpm"$version" -t; exit 1; }
sockphp=$(sed -n 's/^listen = //p' "/etc/php/$version/fpm/pool.d/phptest.conf")
kill -USR2 "$(cat "/run/php/php$version-fpm.pid")" 2>/dev/null || true
sleep 1
for _ in $(seq 1 10); do
    [ -S "$sockphp" ] && break
    sleep 1
done

echo
echo "--- PHP exécuté par le socket FastCGI, sous l'identité du projet ---"
mkdir -p /home/php.test/www/public
# Le front controller répond 404 sur /absent, pour la page d'erreur éprouvée plus
# bas. Il est écrit une fois pour toutes : FPM tourne avec l'opcache
# (revalidate_freq=2), et un script réécrit puis interrogé dans les deux
# secondes répond encore par l'ancien code.
cat > /home/php.test/www/public/index.php <<'PHP'
<?php
$uri = $_SERVER["REQUEST_URI"];
if ($uri === "/valeurs") {
    $env = [];
    foreach (["V1", "V2", "V3", "V4", "V5", "V6", "V7", "V8", "V9", "V10", "RELAIS"] as $k) { $env[$k] = getenv($k); }
    echo json_encode(["env" => $env, "highlight.string" => ini_get("highlight.string"),
        "highlight.comment" => ini_get("highlight.comment"), "highlight.keyword" => ini_get("highlight.keyword"),
        "error_reporting" => error_reporting(), "attendu" => E_ALL & ~E_DEPRECATED]);
    exit;
}
if ($uri === "/session") { session_start(); $_SESSION["x"] = 1; echo session_save_path(), "/sess_", session_id(); exit; }
if ($uri === "/opcache") {
    $s = @opcache_get_status(true);
    echo $s === false ? "API fermée" : implode("\n", array_keys($s["scripts"] ?? []));
    exit;
}
if ($uri === "/absent") { http_response_code(404); }
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
echo "--- chaque valeur parvient telle quelle au script (D115) ---"
curl -sk --resolve php.test:443:127.0.0.1 https://php.test/valeurs >/tmp/valeurs.json
python3 - <<'PY' || exit 1
import json, sys
recu = json.load(open("/tmp/valeurs.json"))
attendu = json.load(open("/tmp/attendu.json"))
fautes = [f"env {k} : reçu {recu['env'].get(k)!r}, déclaré {v!r}" for k, v in attendu["env"].items()
          if recu["env"].get(k) != v]
fautes += [f"{k} : reçu {recu[k]!r}, déclaré {attendu[k]!r}" for k in ("highlight.string", "highlight.comment",
           "highlight.keyword") if recu[k] != attendu[k]]
relais = recu["env"].get("RELAIS") or ""
if relais in ("", "$PATH") or "/bin" not in relais:
    fautes.append(f"env RELAIS = $PATH : reçu {relais!r}, la variable PATH du maître attendue")
if recu["error_reporting"] != recu["attendu"]:
    fautes.append(f"error_reporting {recu['error_reporting']} : l'expression doit être évaluée ({recu['attendu']})")
for f in fautes:
    print("ÉCHEC :", f)
if fautes:
    sys.exit(1)
print("valeurs reçues intactes :", len(attendu["env"]) + 3, "+ error_reporting évalué")
PY

echo
echo "--- sessions hors du home, purgées par systemd-tmpfiles selon leur âge (D115) ---"
session=$(curl -sk --resolve php.test:443:127.0.0.1 https://php.test/session)
echo "session : $session"
case "$session" in
/var/lib/ogs/sessions/phptest/sess_*) ;;
*) echo "ÉCHEC : la session n'est pas rangée sous /var/lib/ogs/sessions/phptest"; exit 1 ;;
esac
[ -f "$session" ] || { echo "ÉCHEC : fichier de session absent"; ls -la /var/lib/ogs/sessions/phptest; exit 1; }
grep "sessions/phptest" /etc/tmpfiles.d/ogs.conf
grep -q "sessions/phptest 0700 phptest phptest 3s" /etc/tmpfiles.d/ogs.conf ||
    { echo "ÉCHEC : la ligne tmpfiles ne suit pas le gc_maxlifetime du pool"; exit 1; }
sleep 4
systemd-tmpfiles --clean /etc/tmpfiles.d/ogs.conf
[ -e "$session" ] && { echo "ÉCHEC : session expirée non purgée"; exit 1; }
[ -d /var/lib/ogs/sessions/phptest ] || { echo "ÉCHEC : le répertoire des sessions a été retiré"; exit 1; }
echo "session expirée purgée, répertoire en place"

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
echo "--- Prestashop : les règles de ses .htaccess sont portées par le template (D99) ---"
# nginx ne lit pas les .htaccess : les mêmes règles que sous Apache sont portées
# par le template — images de produit de 1 à 7 chiffres, refus des répertoires et
# listes blanches par extension des .htaccess de sous-répertoires livrés.
ogs site create boutique.test --kind php-prestashop --php "$version" --tls=false --yes -q >/dev/null 2>&1
# FPM : rechargé par son pidfile, le titre réécrit du maître échappant à pkill -f.
sock=$(sed -n 's/^listen = //p' "/etc/php/$version/fpm/pool.d/boutiquetest.conf")
kill -USR2 "$(cat "/run/php/php$version-fpm.pid")" 2>/dev/null || true
for _ in $(seq 1 10); do
    [ -S "$sock" ] && break
    sleep 1
done
racineps=/home/boutique.test/www
poser() { mkdir -p "$(dirname "$racineps/$1")" && printf '%s\n' "$2" > "$racineps/$1"; }
poser index.php '<?php echo "front boutique\n";'
poser admin7x2k/cron_currency_rates.php '<?php'
poser admin7x2k/index.php '<?php echo "back-office\n";'
poser webservice/dispatcher.php '<?php echo "api ok\n";'
poser admin7x2k/backups/sauvegarde.sql.bz2 'secret'
poser admin7x2k/export/clients.csv 'secret'
poser img/p/1/2/3/123-home_default.jpg 'image-123'
poser img/p/1/2/3/4/5/6/7/1234567-large_default.jpeg 'image-1234567'
poser img/notes.txt 'secret'
poser themes/classic/config/theme.yml 'secret'
poser themes/classic/assets/css/theme.css 'body{}'
poser upload/x.txt 'secret'
poser var/logs/debug.log 'secret'
poser mails/fr/order_conf.html 'secret'
poser templates/index.tpl 'secret'
poser docs/csv_import/produits.csv 'exemple'
poser translations/cldr/fr.json '{}'
poser tools/foundation/foundation.js 'outil'
poser img/c/3_thumb-category_default.jpg 'vignette-3'
poser modules/m/logs/m.log 'secret'
poser modules/m/views/css/front.css 'body{}'
poser modules/m/ajax.php '<?php echo "module\n";'
poser admin7x2k/ajax.php '<?php echo "ancien point du back-office\n";'
poser init.php '<?php echo "init exécuté\n";'
poser sansmarque/outil.php '<?php echo "outil exécuté\n";'
# Une 9 : l'apply réécrit le vhost, écrit à la création sur un docroot vide, avec
# les règles de cette branche.
poser src/Core/Version.php "<?php
final class Version
{
    public const VERSION = '9.0.0';"
chown -R boutiquetest:boutiquetest "$racineps"
ogs apply boutiquetest --yes >/dev/null 2>&1
if ! grep -q "Règles de Prestashop 9\." /etc/nginx/sites-available/boutiquetest*; then
    echo "ÉCHEC : le vhost nginx n'a pas été réécrit pour la 9"
    exit 1
fi
# L'appartenance de www-data au groupe du projet n'est prise qu'au démarrage des
# workers (voir plus bas) : un rechargement en relance.
systemctl reload nginx
sleep 1
attendu() {
    local uri=$1 code=$2 contenu=${3:-} obtenu
    obtenu=$(curl -s -o /tmp/ps.out -w '%{http_code}' -H 'Host: boutique.test' "http://127.0.0.1$uri")
    printf '  %-42s %s %s\n' "$uri" "$obtenu" "$(head -c 40 /tmp/ps.out | tr '\n' ' ')"
    if [ "$obtenu" != "$code" ] || { [ -n "$contenu" ] && ! grep -q "$contenu" /tmp/ps.out; }; then
        echo "ÉCHEC : $uri devait répondre $code${contenu:+ avec « $contenu »}"
        exit 1
    fi
}
attendu / 200 "front boutique"
attendu /ma-categorie/mon-produit 200 "front boutique"
attendu /123-home_default/t-shirt.jpg 200 image-123
attendu /1234567-large_default/pull.jpeg 200 image-1234567
attendu /c/3_thumb-category_default/vetements.jpg 200 vignette-3
for uri in /admin7x2k/backups/sauvegarde.sql.bz2 /admin7x2k/export/clients.csv \
    /admin7x2k/backups/absent.sql /img/notes.txt /themes/classic/config/theme.yml \
    /upload/x.txt /var/logs/debug.log /mails/fr/order_conf.html /templates/index.tpl \
    /modules/m/logs/m.log; do
    attendu "$uri" 403
done
attendu /modules/m/views/css/front.css 200
# Une URL propre dont le deuxième segment est « export » ou « import » n'est
# refusée que sous un répertoire réel : catégorie sous un préfixe de langue,
# contrôleur front d'un module ainsi nommé.
attendu /fr/export/12-produit.html 200 "front boutique"
attendu /module/export/ajax 200 "front boutique"
attendu /docs/csv_import/produits.csv 200 exemple
attendu /translations/cldr/fr.json 200
attendu /tools/foundation/foundation.js 200 outil
attendu /themes/classic/assets/css/theme.css 200
# Points d'entrée PHP de la branche (D119) : le front controller, appelé aussi
# directement, le webservice, et le répertoire d'administration, reconnu à son
# cron_currency_rates.php sous le docroot en 0710 et disable_symlinks — ses
# scripts s'exécutent, ses routes vont à son index.php. Aucun autre script.
attendu /index.php 200 "front boutique"
attendu /api/products 200 "api ok"
attendu /admin7x2k/ 200 back-office
attendu /admin7x2k/index.php 200 back-office
attendu /admin7x2k/sell/catalog/products 200 back-office
attendu /admin7x2k/ajax.php 200 "ancien point du back-office"
attendu /init.php 404
attendu /sansmarque/outil.php 404
# En 9, les scripts des modules sont refusés comme par leur .htaccess livré.
attendu /modules/m/ajax.php 403
echo
echo "--- OPcache : ce qu'un projet voit des scripts de ses voisins (constat, §20) ---"
curl -s -o /dev/null -H 'Host: boutique.test' http://127.0.0.1/
curl -s -o /dev/null -H 'Host: boutique.test' http://127.0.0.1/admin7x2k/index.php
vus=$(curl -sk --resolve php.test:443:127.0.0.1 https://php.test/opcache)
if printf '%s\n' "$vus" | grep -q "/home/boutique.test/"; then
    echo "(constat) opcache_get_status() de phptest énumère les scripts de boutiquetest : R4-34 reste ouvert"
else
    echo "(constat) phptest ne voit pas les scripts de boutiquetest : $vus" | head -3
fi

ogs project delete boutiquetest --yes >/dev/null 2>&1
echo "les règles de Prestashop sont servies sous nginx comme sous Apache"

echo
echo "--- php-plain : tout script existant est un point d'entrée (D119) ---"
ogs site create plain.test --kind php-plain --php "$version" --tls=false --yes -q >/dev/null 2>&1
sock=$(sed -n 's/^listen = //p' "/etc/php/$version/fpm/pool.d/plaintest.conf")
kill -USR2 "$(cat "/run/php/php$version-fpm.pid")" 2>/dev/null || true
for _ in $(seq 1 10); do
    [ -S "$sock" ] && break
    sleep 1
done
mkdir -p /home/plain.test/www/outils
echo '<?php echo "accueil plain\n";' > /home/plain.test/www/index.php
echo '<?php echo "outil plain ", $_SERVER["PATH_INFO"] ?? "", "\n";' > /home/plain.test/www/outils/outil.php
chown -R plaintest:plaintest /home/plain.test/www
systemctl reload nginx
sleep 1
plain() {
    local uri=$1 code=$2 contenu=${3:-} obtenu
    obtenu=$(curl -s -o /tmp/plain.out -w '%{http_code}' -H 'Host: plain.test' "http://127.0.0.1$uri")
    printf '  %-42s %s %s\n' "$uri" "$obtenu" "$(head -c 40 /tmp/plain.out | tr '\n' ' ')"
    if [ "$obtenu" != "$code" ] || { [ -n "$contenu" ] && ! grep -q "$contenu" /tmp/plain.out; }; then
        echo "ÉCHEC : $uri devait répondre $code${contenu:+ avec « $contenu »}"
        tail -3 /var/log/ogs-projects/plaintest/nginx-error.log 2>/dev/null
        exit 1
    fi
}
plain / 200 "accueil plain"
plain /outils/outil.php 200 "outil plain"
plain /outils/outil.php/suite 200 "outil plain /suite"
plain /absent.php 404
ogs project delete plaintest --yes >/dev/null 2>&1

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
# Une cible déclarée autre que le domaine principal est honorée (D119), par un
# vhost de redirection qui lui est propre.
python3 - <<'PY'
chemin = "/etc/ogs/projects/ancientest.yaml"
s = open(chemin).read()
s = s.replace("      target: ancien.test\n",
              "      target: ancien.test\n    - name: promo.test\n      role: redirect\n      target: ailleurs.example\n", 1)
open(chemin, "w").write(s)
PY
ogs apply ancientest --yes >/dev/null 2>&1
systemctl reload nginx
sleep 1
cible=$(curl -s -o /dev/null -w '%{redirect_url}' -H 'Host: promo.test' http://127.0.0.1/page)
echo "promo.test/page → $cible"
if [ "$cible" != "http://ailleurs.example/page" ]; then
    echo "ÉCHEC : la cible déclarée de promo.test n'est pas honorée"
    ls /etc/nginx/sites-enabled/
    exit 1
fi
cible=$(curl -s -o /dev/null -w '%{redirect_url}' -H 'Host: vieux.test' http://127.0.0.1/page)
if [[ "$cible" != *"ancien.test/page"* ]]; then
    echo "ÉCHEC : vieux.test ne redirige plus vers ancien.test : $cible"
    exit 1
fi
if [ ! -e /etc/nginx/sites-enabled/ancientest_redirect-ailleurs.example.conf ]; then
    echo "ÉCHEC : vhost de redirection propre à la cible absent"
    ls /etc/nginx/sites-enabled/
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
echo "--- paire clé/certificat dépareillée sur un projet à redirection (D110) ---"
# Deux vhosts sur le même certificat : le site et sa redirection. Une autre clé
# remplace privkey.pem. L'étape web réécrivait le premier vhost en HTTP seul,
# « nginx -t » lisait le second, qui désignait toujours la paire — « key values
# mismatch », étape critique, saga déroulée, code 1 —, et la réparation, placée
# après le web, n'était jamais atteinte. Elle précède désormais les vhosts.
ogs site create shop.test --kind static --redirect www.shop.test --self-signed --yes -q >/dev/null 2>&1
echo "boutique" > /home/shop.test/www/index.html
chown shoptest:shoptest /home/shop.test/www/index.html
gpasswd -a www-data shoptest >/dev/null
systemctl reload nginx
sleep 1
paire=/var/lib/ogs/acme/certificates/shoptest
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$paire/privkey.pem" 2>/dev/null
if nginx -t >/dev/null 2>&1; then
    echo "ÉCHEC : le témoin manque — nginx doit refuser une paire dépareillée"
    exit 1
fi
echo "témoin : nginx -t refuse la paire dépareillée"
plan=$(ogs plan shoptest 2>&1)
echo "$plan" | grep -E "dépareill|vhost nginx" | head -4
ligne_paire=$(echo "$plan" | grep -n "réémis avant les vhosts" | head -1 | cut -d: -f1)
ligne_vhost=$(echo "$plan" | grep -n "vhost nginx" | head -1 | cut -d: -f1)
if [ -z "$ligne_paire" ] || { [ -n "$ligne_vhost" ] && [ "$ligne_paire" -gt "$ligne_vhost" ]; }; then
    echo "ÉCHEC : la réémission de la paire doit précéder les vhosts"
    echo "$plan"
    exit 1
fi
ogs doctor 2>&1 | grep -E "dépareill" | head -2
sortie=$(ogs apply shoptest --yes 2>&1)
code=$?
echo "$sortie" | grep -E "dépareill|✗|⚠" | head -4
echo "apply : code $code"
if [ "$code" != 0 ] && [ "$code" != 6 ]; then
    echo "ÉCHEC : l'apply doit rétablir la paire au lieu d'échouer sur le vhost"
    echo "$sortie" | tail -15
    exit 1
fi
if ! nginx -t >/dev/null 2>&1; then
    echo "ÉCHEC : configuration refusée après l'apply"
    nginx -t 2>&1 | tail -3
    exit 1
fi
reponse=$(curl -sk --resolve shop.test:443:127.0.0.1 https://shop.test/index.html)
redir=$(curl -sk -o /dev/null -w '%{http_code}' --resolve www.shop.test:443:127.0.0.1 https://www.shop.test/)
echo "shop.test en HTTPS : $reponse ; redirection en HTTPS : $redir"
if [ "$reponse" != "boutique" ] || [ "$redir" != "301" ]; then
    echo "ÉCHEC : le site et sa redirection doivent être servis en HTTPS sur la paire rétablie"
    exit 1
fi

echo "--- écriture interrompue : achevée avant les vhosts, sans les réécrire ---"
# Ce que laisse un kill -9 entre deux renommages : la clé neuve en place, la
# chaîne neuve en attente. Elle s'achève sans émission, et les vhosts, lus
# comme servis par la paire achevée, ne passent pas en HTTP seul.
tmp=$(mktemp -d)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 90 \
    -subj /CN=shop.test -addext "subjectAltName=DNS:shop.test,DNS:www.shop.test" \
    -keyout "$tmp/k" -out "$tmp/c" 2>/dev/null
cp "$tmp/k" "$paire/privkey.pem"
cp "$tmp/c" "$paire/fullchain.pem.ogs-new"
rm -rf "$tmp"
avant=$(md5sum /etc/nginx/sites-available/shoptest.conf | cut -d' ' -f1)
sortie=$(ogs apply shoptest --yes 2>&1)
code=$?
echo "$sortie" | grep -E "interrompue|✗" | head -2
echo "apply : code $code"
apres=$(md5sum /etc/nginx/sites-available/shoptest.conf | cut -d' ' -f1)
if [ "$code" != 0 ] || [ "$avant" != "$apres" ] || [ -e "$paire/fullchain.pem.ogs-new" ] || ! nginx -t >/dev/null 2>&1; then
    echo "ÉCHEC : l'écriture interrompue doit s'achever sans réécrire le vhost (code $code)"
    echo "$sortie" | tail -10
    exit 1
fi
echo "écriture achevée, vhost inchangé, configuration valide"
ogs project delete shoptest --yes >/dev/null 2>&1

echo
echo "--- paire dépareillée laissée en place, apply d'un projet sans rapport (D110, B2) ---"
# Une clé recopiée à la main sur shop.test, puis la création d'un autre site. Le
# rechargement de fin d'apply chargeait la paire : sous Apache, « apachectl -t »
# répondait « Syntax OK » et le maître mourait après le retour de graceful —
# tous les sites tombaient, code 0, rien de signalé. Le rechargement est refusé,
# nommé, et le serveur continue de servir sa configuration précédente.
ogs site create shop.test --kind static --redirect www.shop.test --self-signed --yes -q >/dev/null 2>&1
echo "boutique" > /home/shop.test/www/index.html
chown shoptest:shoptest /home/shop.test/www/index.html
gpasswd -a www-data shoptest >/dev/null
systemctl reload nginx
sleep 1
paire=/var/lib/ogs/acme/certificates/shoptest
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$paire/privkey.pem" 2>/dev/null
echo "(nginx -t refuse déjà la paire : éprouvé plus haut)"
sortie=$(ogs site create neuf.test --kind static --self-signed --yes 2>&1)
code=$?
echo "$sortie" | grep -E "dépareill|refusé|✗" | head -3
echo "site create neuf.test : code $code"
sleep 3
if ! pgrep -x nginx >/dev/null; then
    echo "ÉCHEC : le serveur web est mort après l'apply d'un projet sans rapport"
    exit 1
fi
if ! [ "$code" = 1 ] || [ "$code" = 6 ] || ! echo "$sortie" | grep -q "ogs cert renew shoptest"; then
    echo "ÉCHEC : attendu code 1 (le vhost neuf ne peut être validé) ou 6, le certificat dépareillé nommé avec son remède"
    echo "$sortie" | tail -12
    exit 1
fi
reponse=$(curl -sk --resolve shop.test:443:127.0.0.1 https://shop.test/index.html)
echo "serveur vivant ; shop.test toujours servi : $reponse"
if [ "$reponse" != "boutique" ]; then
    echo "ÉCHEC : les sites doivent rester servis sur la configuration précédente"
    exit 1
fi
ogs doctor 2>&1 | grep -E "certificats des vhosts" | head -1
sortie=$(ogs cert renew shoptest 2>&1)
code=$?
echo "$sortie" | tail -2
sleep 3
if [ "$code" != 0 ] || ! pgrep -x nginx >/dev/null; then
    echo "ÉCHEC : « cert renew » doit réémettre la paire et recharger le serveur (code $code)"
    exit 1
fi
ogs apply neuftest --yes >/dev/null 2>&1
sleep 2
reponse=$(curl -sk --resolve shop.test:443:127.0.0.1 https://shop.test/index.html)
if [ "$reponse" != "boutique" ] || ! pgrep -x nginx >/dev/null; then
    echo "ÉCHEC : après « cert renew », le site doit être servi sur la paire rétablie"
    exit 1
fi
echo "paire réémise par « cert renew », serveur rechargé et vivant"
ogs project delete neuftest --yes >/dev/null 2>&1
ogs project delete shoptest --yes >/dev/null 2>&1

echo
echo "--- la réouverture des journaux les donne à www-data : pas un écart (D116) ---"
# USR1 — ce que logrotate envoie chaque nuit : le maître rouvre les journaux et
# les donne au compte de ses workers. L'apply exigeait root et les reprenait à
# chaque passage.
kill -USR1 "$(cat /run/nginx.pid)"
for _ in $(seq 1 20); do
    [ "$(stat -c %U /var/log/ogs-projects/statictest/nginx-access.log)" = www-data ] && break
    sleep 0.5
done
stat -c '%a %U:%G %n' /var/log/ogs-projects/statictest/nginx-*.log
if [ "$(stat -c %U /var/log/ogs-projects/statictest/nginx-access.log)" != www-data ]; then
    echo "ÉCHEC : nginx n'a pas donné le journal à www-data — l'essai ne prouve rien"
    exit 1
fi
plan=$(ogs plan statictest 2>&1)
if grep -q "journal .*nginx" <<<"$plan"; then
    echo "ÉCHEC : le plan veut reprendre les journaux de nginx"
    echo "$plan"
    exit 1
fi
echo "  journaux à www-data après réouverture, plan sans écart sur eux"

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
if [ -e /var/lib/ogs/sessions/phptest ]; then
    echo "ÉCHEC : les sessions du projet supprimé restent"
    exit 1
fi

echo
echo "RÉSULTAT : nginx sert les quatre types de vhost, exécute PHP par socket"
echo "           sous l'identité du projet, refuse les scripts déposés, et"
echo "           prend en compte l'appartenance de groupe sur rechargement."
