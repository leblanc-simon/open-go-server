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
    # Le gestionnaire ne doit pas exister avant la création : c'est ogs qui
    # l'installe, et la création le fait déjà (étape non critique de l'apply).
    case "$runtime" in
    ruby) gestionnaire="/home/$hote/.rbenv" ;;
    python) gestionnaire="/home/$hote/.pyenv" ;;
    esac
    if [ -e "$gestionnaire" ]; then
        echo "ÉCHEC : le gestionnaire existe avant la création"
        exit 1
    fi
    # La version du runtime est donnée dès la création : sans elle, la déclaration
    # est refusée à l'apply.
    ogs site create "$hote" --kind "$runtime" --runtime "$version" --tls=false --yes 2>&1 |
        grep -Ei "runtime|⚠|✗" | head -3
    if ! id "$projet" >/dev/null 2>&1; then
        echo "ÉCHEC : le compte du projet n'a pas été créé"
        exit 1
    fi

    # Placé ici, après la création, ce contrôle échouait toujours : la création
    # installe déjà le runtime. « runtime install » qui suit doit réussir sans
    # rien recompiler.

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
    # dépendances doivent suivre l'interpréteur. Un venv par version, hors de
    # www (D123).
    python) attendu="/home/$hote/.venv-$version/bin/python" ;;
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
        if ! su -s /bin/bash "$projet" -c "$attendu -c 'import sys; print(sys.prefix)'" | grep -q ".venv-$version"; then
            echo "ÉCHEC : l'interpréteur n'est pas celui du venv"
            exit 1
        fi
        # Une seconde installation de la même version ne recrée pas le venv : il
        # porte les dépendances de l'application (D123).
        cfg="/home/$hote/.venv-$version/pyvenv.cfg"
        avant=$(stat -c %Y "$cfg")
        sleep 1
        ogs runtime install "$projet" "$version" >/dev/null 2>&1
        if [ "$(stat -c %Y "$cfg")" != "$avant" ]; then
            echo "ÉCHEC : le venv de la version en service a été recréé"
            exit 1
        fi
        if [ -e "/home/$hote/www/.venv" ]; then
            echo "ÉCHEC : un venv a été créé dans www, que le clone recopierait"
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
echo "--- une application Python sert depuis son venv, dépendance comprise (D123) ---"
# ~/.local/bin/python n'active pas le venv : Python cherche pyvenv.cfg à côté du
# chemin invoqué sans résoudre le lien, et lance l'interpréteur de base. L'unité
# passe par le lien de répertoire ~/.venv. Aucun scénario ne lançait jusqu'ici
# une application Python réelle.
racine=/home/python.test
unite='ogs-pythontest-app@1.service'
cat >"$racine/www/app.py" <<'PYEOF'
import sys
import gunicorn
def app(environ, start_response):
    start_response('200 OK', [('Content-Type', 'text/plain')])
    return [('prefix ' + sys.prefix + ' gunicorn ' + gunicorn.__version__).encode()]
PYEOF
chown pythontest:pythontest "$racine/www/app.py"
echo "lien ~/.venv : $(readlink "$racine/.venv")"
if [ "$(readlink "$racine/.venv")" != "$racine/.venv-$VERSION_PYTHON" ]; then
    echo "ÉCHEC : ~/.venv ne désigne pas le venv de la version"
    exit 1
fi
grep '^ExecStart=' /etc/systemd/system/ogs-pythontest-app@.service
if ! grep -q "^ExecStart=$racine/.venv/bin/python -m gunicorn" /etc/systemd/system/ogs-pythontest-app@.service; then
    echo "ÉCHEC : l'unité ne lance pas l'interpréteur du venv par ~/.venv"
    exit 1
fi
runuser -u pythontest -- "$racine/.venv/bin/python" -m pip install -q gunicorn 2>&1 | tail -2
# Sans gunicorn, l'instance boucle sur ses relances, ou les a épuisées : elle
# est relancée à la main, l'apply ne relevant qu'une instance « failed » (D121).
systemctl reset-failed "$unite" 2>/dev/null
systemctl restart "$unite"
repond() { curl -s --max-time 5 --unix-socket /run/ogs/pythontest/app.sock http://localhost/; }
for _ in $(seq 1 20); do [ -n "$(repond)" ] && break; sleep 1; done
reponse=$(repond)
echo "réponse : $reponse"
if [[ "$reponse" != "prefix $racine/.venv gunicorn "* ]]; then
    echo "ÉCHEC : l'application ne tourne pas dans son venv"
    journalctl -u "$unite" --no-pager -n 15 2>&1
    exit 1
fi
# Un apply de plus ne change rien et ne redémarre rien.
pid=$(systemctl show -p MainPID --value "$unite")
ogs apply pythontest --yes >/dev/null 2>&1
if [ "$(systemctl show -p MainPID --value "$unite")" != "$pid" ]; then
    echo "ÉCHEC : un apply sans changement a redémarré l'application"
    exit 1
fi
echo "l'application sert depuis son venv, gunicorn importé"

echo
echo "--- un venv d'avant D123 reste celui de sa version ---"
# www/.venv créé par le pyenv du home, comme ogs le faisait : son pyvenv.cfg
# réel désigne l'interpréteur de ce home, et il est repris tel quel.
runuser -u pythontest -- "$racine/.pyenv/versions/$VERSION_PYTHON/bin/python" -m venv "$racine/www/.venv"
runuser -u pythontest -- "$racine/www/.venv/bin/python" -m pip install -q gunicorn 2>&1 | tail -2
grep '^home' "$racine/www/.venv/pyvenv.cfg"
mv "$racine/.venv-$VERSION_PYTHON" /root/venv-ecarte
sortie=$(ogs apply pythontest --yes 2>&1); code=$?
echo "$sortie" | grep -Ei "lien|runtime|⚠|✗" | head -5
if [ "$(readlink "$racine/.venv")" != "$racine/www/.venv" ]; then
    echo "ÉCHEC : l'ancien venv de la version n'est pas repris (code $code)"
    exit 1
fi
if [ -e "$racine/.venv-$VERSION_PYTHON" ]; then
    echo "ÉCHEC : un venv vide a été créé à la place de l'ancien, qui sert"
    exit 1
fi
systemctl restart "$unite"
for _ in $(seq 1 20); do [ -n "$(repond)" ] && break; sleep 1; done
echo "réponse : $(repond)"
if [[ "$(repond)" != "prefix $racine/.venv gunicorn "* ]]; then
    echo "ÉCHEC : l'application ne sert plus depuis l'ancien venv"
    exit 1
fi
echo "l'ancien venv sert sa version, sans rien recréer"

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
echo "           Python est celui que le lien désigne, et l'application y"
echo "           trouve ses dépendances."
