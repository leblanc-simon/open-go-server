#!/bin/bash
# Éprouve la question laissée en suspens depuis le lot L1 : un rechargement du
# serveur web suffit-il à prendre en compte l'appartenance de www-data au groupe
# d'un projet, ou faut-il un redémarrage complet ?
#
# L'enjeu est concret : si un rechargement ne suffit pas, chaque création de site
# impose une coupure, et la conception de l'application doit en tenir compte.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : prise en compte de l'appartenance de groupe ==="
apache2ctl start
sleep 1

# Un site servi depuis un home 0750, dont seul le groupe donne accès.
ogs init --quiet
choisir_moteur apache
ogs site create site.test --kind static --tls=false --yes --quiet >/dev/null 2>&1 || true
if [ ! -d /home/site.test ]; then
    echo "ÉCHEC : le site n'a pas été créé"
    exit 1
fi
echo "bonjour depuis le site" > /home/site.test/www/index.html
chown sitetest:sitetest /home/site.test/www/index.html
chmod 0640 /home/site.test/www/index.html

# Un vhost minimal servant ce docroot sur le port 80 par défaut.
cat > /etc/apache2/sites-available/000-default.conf <<'VHOST'
<VirtualHost *:80>
    DocumentRoot /home/site.test/www
    <Directory /home/site.test/www>
        Require all granted
        Options -Indexes
    </Directory>
</VirtualHost>
VHOST

# État de départ : www-data hors du groupe. La requête doit échouer, ce qui
# prouve que l'isolation fonctionne.
gpasswd -d www-data sitetest >/dev/null 2>&1 || true
apache2ctl restart
sleep 1
code_avant=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1/index.html)
echo "hors du groupe          : HTTP $code_avant"
if [ "$code_avant" = "200" ]; then
    echo "ÉCHEC : le contenu est servi sans appartenance au groupe — l'isolation ne tient pas"
    exit 1
fi

# On ajoute www-data au groupe, puis on RECHARGE seulement.
gpasswd -a www-data sitetest >/dev/null
apache2ctl graceful
sleep 2
code_reload=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1/index.html)
echo "après un rechargement   : HTTP $code_reload"

# Puis on redémarre, pour comparer.
apache2ctl restart
sleep 2
code_restart=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1/index.html)
echo "après un redémarrage    : HTTP $code_restart"

echo
if [ "$code_reload" = "200" ]; then
    echo "RÉSULTAT : un rechargement SUFFIT — les workers rappellent initgroups() au fork."
    echo "           La conception retenue est correcte, aucune coupure n'est nécessaire."
    exit 0
fi
if [ "$code_restart" = "200" ]; then
    echo "RÉSULTAT : un rechargement NE SUFFIT PAS, un redémarrage est nécessaire."
    echo "           La création d'un site impose donc une brève coupure du serveur web :"
    echo "           l'apply doit émettre un restart, et le cahier des charges doit être corrigé."
    exit 2
fi
echo "RÉSULTAT INDÉTERMINÉ : ni le rechargement ni le redémarrage ne servent le contenu"
echo "                       (HTTP $code_reload puis $code_restart) — cause à chercher ailleurs."
exit 3
