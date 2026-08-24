#!/bin/bash
# Éprouve l'installation, l'usage et la désinstallation du paquet Debian sur un
# système propre : un paquet qui se construit n'est pas un paquet qui s'installe.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : cycle de vie du paquet ==="
# L'image porte un binaire pour les autres scénarios : on l'écarte, sinon
# l'installation ne serait pas éprouvée.
rm -f /usr/bin/ogs
if [ -e /usr/bin/ogs ]; then
    echo "ÉCHEC : un ogs subsiste avant installation"
    exit 1
fi

apt-get install -y -qq /paquet/*.deb 2>&1 | grep -vi "^Selecting\|^Reading\|^Preparing\|^Unpacking\|^Setting up" | head -5

echo "--- version installée ---"
ogs version

echo "--- page de manuel ---"
if ! man -w ogs >/dev/null 2>&1; then
    echo "ÉCHEC : la page de manuel n'est pas trouvée par man"
    exit 1
fi
man ogs 2>/dev/null | head -4

echo "--- complétion chargée par bash ---"
bash -lc 'source /usr/share/bash-completion/completions/ogs && complete -p ogs' || {
    echo "ÉCHEC : le script de complétion ne se charge pas"
    exit 1
}

echo "--- l'outil fonctionne après installation ---"
version=$(ls /etc/php | head -1)
ogs init --quiet
choisir_moteur apache
ogs site create paquet.test --kind php-plain --php "$version" --tls=false --yes 2>&1 | tail -3
if [ ! -d /home/paquet.test ]; then
    echo "ÉCHEC : le site n'a pas été créé après installation du paquet"
    exit 1
fi

echo "--- désinstallation ---"
# Le projet créé doit survivre à la désinstallation : retirer l'outil
# d'administration ne doit pas défaire les sites qu'il gère.
apt-get remove -y -qq ogs 2>&1 | tail -1
if [ ! -d /home/paquet.test ]; then
    echo "ÉCHEC : la désinstallation a supprimé le home du projet"
    exit 1
fi
if [ ! -f /etc/ogs/projects/paquettest.yaml ]; then
    echo "ÉCHEC : la désinstallation a supprimé la déclaration du projet"
    exit 1
fi
# On éprouve le fichier, non « command -v » : bash garde en mémoire le chemin
# des commandes déjà lancées et répondrait encore.
if [ -e /usr/bin/ogs ]; then
    echo "ÉCHEC : le binaire est encore présent"
    exit 1
fi
echo
echo "RÉSULTAT : le paquet s'installe, fonctionne, et se retire sans toucher aux projets."
