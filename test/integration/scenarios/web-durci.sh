#!/bin/bash
# Éprouve ogs face à une unité de serveur web durcie par sa distribution. Ubuntu
# livre « apache2.service » avec ProtectHome=read-only : le service voit /home en
# lecture seule dans son seul espace de noms — « mount » affiche rw, root écrit
# sans peine — et le maître s'arrête en ouvrant le journal d'un vhost, emportant
# tous les sites du serveur. Aucun test unitaire ne peut voir cela : il faut un
# vrai systemd, un vrai espace de noms et un vrai serveur.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : moteur web durci (ProtectHome=read-only) ==="
version=$(version_php)
echo "version PHP détectée : $version"

# L'image porte les deux moteurs et nginx occupe le port 80 sous systemd.
systemctl stop nginx

# Le durcissement que livre Ubuntu, reproduit ici : c'est l'état de départ du
# serveur, pas quelque chose qu'ogs aurait fait.
mkdir -p /etc/systemd/system/apache2.service.d
cat > /etc/systemd/system/apache2.service.d/durcissement.conf <<'EOF'
[Service]
ProtectHome=read-only
ProtectSystem=full
ReadWritePaths=/var/log/apache2
EOF
systemctl daemon-reload

if ! systemctl start apache2; then
    echo "ÉCHEC : Apache n'a pas démarré avant même l'intervention d'ogs"
    exit 1
fi
echo "ProtectHome effectif : $(systemctl show apache2 -p ProtectHome)"
if [ "$(systemctl show -p ProtectHome --value apache2)" != "read-only" ]; then
    echo "ÉCHEC : le durcissement n'est pas actif, le scénario n'éprouve rien"
    exit 1
fi

ogs init --quiet
choisir_moteur apache
ogs site create php.test --kind php-plain --php "$version" --tls=false --yes 2>&1 | tail -6

# Le complément d'unité doit avoir été écrit, et la racine des journaux y figurer.
dropin=/etc/systemd/system/apache2.service.d/ogs.conf
if [ ! -f "$dropin" ]; then
    echo "ÉCHEC : $dropin absent"
    exit 1
fi
echo "complément d'unité :"
grep -v '^#' "$dropin" | sed 's/^/  /'
# Les journaux ont quitté /home (D71) : le complément ne porte plus qu'une
# entrée fixe, la racine des journaux, par prudence envers ProtectSystem=strict.
if ! grep -q 'ReadWritePaths=-/var/log/ogs-projects$' "$dropin" || [ "$(grep -c ReadWritePaths "$dropin")" != 1 ]; then
    echo "ÉCHEC : le complément doit porter la seule racine des journaux"
    exit 1
fi

echecs=0
# Le serveur doit être vivant : c'est tout l'enjeu.
if ! systemctl is-active --quiet apache2; then
    echo "ÉCHEC : Apache s'est arrêté — c'est précisément le défaut à éviter"
    journalctl -u apache2 --no-pager | tail -5
    echecs=1
fi

# Et le journal doit être réellement écrit par le serveur, depuis son espace de
# noms : le complément ne vaut que s'il porte.
echo '<?php echo "durci ok";' > /home/php.test/www/index.php
chown phptest:phptest /home/php.test/www/index.php
gpasswd -a www-data phptest >/dev/null
systemctl restart apache2
sleep 2
reponse=$(curl -s -H 'Host: php.test' http://127.0.0.1/index.php)
echo "réponse : $reponse"
if [ "$reponse" != "durci ok" ]; then
    echo "ÉCHEC : le site n'est pas servi"
    echecs=1
fi
if [ ! -s /home/php.test/logs/apache-access.log ]; then
    echo "ÉCHEC : le journal du vhost est vide, le serveur n'y écrit pas"
    ls -l /home/php.test/logs/ 2>&1 | sed 's/^/  /'
    echecs=1
else
    echo "journal du vhost écrit par le serveur :"
    tail -1 /home/php.test/logs/apache-access.log | sed 's/^/  /'
fi

# Sans le complément, le rechargement doit être refusé plutôt que de tuer le
# serveur : c'est la seconde ligne de défense.
rm -f "$dropin"
systemctl daemon-reload
systemctl restart apache2
sortie=$(ogs apply --yes 2>&1)
code=$?
echo "$sortie" | tail -6
if ! systemctl is-active --quiet apache2; then
    echo "ÉCHEC : Apache est mort alors que le complément avait été retiré"
    echecs=1
else
    echo "Apache a survécu au retrait du complément (code $code)"
fi

if [ "$echecs" -ne 0 ]; then
    echo "RÉSULTAT : ogs ne survit pas à une unité web durcie."
    exit 1
fi
echo "RÉSULTAT : le complément d'unité ouvre les journaux au moteur durci,"
echo "           le serveur reste vivant et écrit bien dans le home du projet."
exit 0
