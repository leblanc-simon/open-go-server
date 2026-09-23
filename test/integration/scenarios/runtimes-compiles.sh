#!/bin/bash
# Éprouve les runtimes qui compilent depuis les sources : Ruby par rbenv et
# Python par pyenv.
#
# Ce scénario est volontairement hors de la série ordinaire : il télécharge et
# compile, ce qui demande un quart d'heure, du réseau et des dépendances de
# compilation. Ce qu'il vérifie n'est pourtant vérifiable qu'ainsi — que le
# binaire produit se trouve bien là où ogs le cherche, et que le venv de Python
# est créé dans le projet.
set -uo pipefail
. /scenarios/lib.sh

VERSION_RUBY=${VERSION_RUBY:-3.3.6}
VERSION_PYTHON=${VERSION_PYTHON:-3.12.8}

echo "=== scénario : runtimes compilés (Ruby $VERSION_RUBY, Python $VERSION_PYTHON) ==="

echo "--- dépendances de compilation ---"
# Elles ne sont pas dans l'image : elles ne servent qu'ici, et pèsent lourd.
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends \
    build-essential git libssl-dev libyaml-dev zlib1g-dev libffi-dev \
    libreadline-dev libsqlite3-dev libbz2-dev liblzma-dev tk-dev uuid-dev >/dev/null
echo "installées"

ogs init --quiet
choisir_moteur apache
apache2ctl start >/dev/null 2>&1

for cas in "ruby:$VERSION_RUBY" "python:$VERSION_PYTHON"; do
    runtime=${cas%%:*}
    version=${cas##*:}
    hote="$runtime.test"
    projet="${runtime}test"

    echo
    echo "=== $runtime $version ==="
    # La version du runtime est donnée dès la création : sans elle, la déclaration
    # est refusée à l'apply.
    ogs site create "$hote" --kind "$runtime" --runtime "$version" --tls=false --yes 2>&1 |
        grep -Ei "runtime|⚠|✗" | head -3
    if ! id "$projet" >/dev/null 2>&1; then
        echo "ÉCHEC : le compte du projet n'a pas été créé"
        exit 1
    fi

    # Le gestionnaire ne doit pas exister avant : c'est ogs qui l'installe.
    case "$runtime" in
    ruby) gestionnaire="/home/$hote/.rbenv" ;;
    python) gestionnaire="/home/$hote/.pyenv" ;;
    esac
    if [ -e "$gestionnaire" ]; then
        echo "ÉCHEC : le gestionnaire existe déjà"
        exit 1
    fi

    echo "--- installation (compilation, patience) ---"
    debut=$(date +%s)
    if ! ogs runtime install "$projet" "$version" 2>&1 | tail -4; then
        echo "ÉCHEC : l'installation a échoué"
        exit 1
    fi
    echo "durée : $(( ($(date +%s) - debut) / 60 )) minute(s)"

    echo "--- le gestionnaire est-il dans le home, sous l'identité du projet ? ---"
    if [ ! -d "$gestionnaire" ]; then
        echo "ÉCHEC : $gestionnaire absent"
        ls -a "/home/$hote"
        exit 1
    fi
    echo "$gestionnaire : $(stat -c '%U:%G %a' "$gestionnaire")"
    if [ "$(stat -c %U "$gestionnaire")" != "$projet" ]; then
        echo "ÉCHEC : le gestionnaire a été installé en root"
        exit 1
    fi

    echo "--- le binaire est-il là où ogs l'attend ? ---"
    case "$runtime" in
    ruby) attendu="/home/$hote/.rbenv/versions/$version/bin/ruby" ;;
    # Pour Python, le lien désigne le venv du projet, pas l'interpréteur nu : les
    # dépendances doivent suivre l'interpréteur.
    python) attendu="/home/$hote/www/.venv/bin/python" ;;
    esac
    if [ ! -x "$attendu" ]; then
        echo "ÉCHEC : $attendu absent — la convention de chemin d'ogs ne correspond pas"
        find "/home/$hote" -maxdepth 5 -name "$runtime" -type f 2>/dev/null | head -5
        exit 1
    fi
    echo "binaire : $attendu"
    echo "version : $(su -s /bin/bash "$projet" -c "$attendu --version" 2>&1 | head -1)"

    echo "--- le lien géré désigne la version installée ---"
    lien="/home/$hote/.local/bin/$runtime"
    ls -l "$lien"
    if [ "$(readlink -f "$lien")" != "$(readlink -f "$attendu")" ]; then
        echo "ÉCHEC : $lien ne désigne pas la version installée"
        exit 1
    fi

    if [ "$runtime" = "python" ]; then
        echo "--- le venv est-il utilisable par le projet ? ---"
        su -s /bin/bash "$projet" -c "$attendu -m pip --version" 2>&1 | head -1
        if ! su -s /bin/bash "$projet" -c "$attendu -c 'import sys; print(sys.prefix)'" | grep -q ".venv"; then
            echo "ÉCHEC : l'interpréteur n'est pas celui du venv"
            exit 1
        fi
    fi

    echo "--- le runtime reste dans le projet ---"
    if [ -e "/home/ruby.test/.pyenv" ] || [ -e "/home/python.test/.rbenv" ]; then
        echo "ÉCHEC : un runtime a fui vers un autre projet"
        exit 1
    fi
done

echo
echo "--- une seconde installation ne recompile pas ---"
debut=$(date +%s)
ogs runtime install rubytest "$VERSION_RUBY" 2>&1 | tail -2
duree=$(( $(date +%s) - debut ))
echo "durée : ${duree}s"
if [ "$duree" -gt 120 ]; then
    echo "ÉCHEC : la version déjà présente a été recompilée"
    exit 1
fi

echo
echo "RÉSULTAT : rbenv et pyenv installent leur version dans le home du projet,"
echo "           sous son identité, à l'emplacement qu'ogs attend ; le venv de"
echo "           Python est celui que le lien désigne."
