#!/bin/bash
# Éprouve l'installation, la mise à jour, l'usage, la désinstallation et la purge
# du paquet Debian sur un système propre : un paquet qui se construit n'est pas
# un paquet qui s'installe, et un paquet qui s'installe n'est pas un paquet qui
# se met à jour.
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

# Une mise à jour ne s'éprouve qu'entre deux versions : le paquet construit est
# réétiqueté trois fois. La première variante porte le prerm des versions
# publiées jusqu'à 0.0.3, qui désactivait le timer de renouvellement à chaque
# mise à jour, et pas de postrm : c'est le prerm de la version en place que dpkg
# exécute, et le postinst nouveau doit réparer ce qu'il a fait.
arch=$(dpkg --print-architecture)
source_deb=$(ls -1 /paquet/ogs_*_"$arch".deb 2>/dev/null | sort -V | tail -1)
if [ -z "$source_deb" ]; then
    echo "ÉCHEC : aucun paquet $arch dans /paquet"
    exit 1
fi
version=$(dpkg-deb -f "$source_deb" Version)
atelier=$(mktemp -d)
trap 'rm -rf "$atelier"' EXIT
# apt lit les paquets locaux sous le compte _apt.
chmod 0755 "$atelier"

reetiqueter() {
    local nouvelle=$1 sortie=$2 arbre="$atelier/arbre"
    rm -rf "$arbre"
    dpkg-deb -R "$source_deb" "$arbre"
    sed -i "s/^Version: .*/Version: $nouvelle/" "$arbre/DEBIAN/control"
    if [ "${3:-}" = fautive ]; then
        cat >"$arbre/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e

case "$1" in
remove | upgrade | deconfigure)
    if command -v systemctl >/dev/null 2>&1; then
        systemctl disable --now ogs-renew.timer >/dev/null 2>&1 || true
    fi
    ;;
esac
EOF
        chmod 0755 "$arbre/DEBIAN/prerm"
        rm -f "$arbre/DEBIAN/postrm"
    fi
    dpkg-deb -b "$arbre" "$sortie" >/dev/null
}
reetiqueter 0.0.3 "$atelier/ogs-0.0.3.deb" fautive
reetiqueter "$version+essai1" "$atelier/ogs-suivant.deb"
reetiqueter "$version+essai2" "$atelier/ogs-ensuite.deb"

# Le substitut de systemctl répond « enabled » à tout : sans init, l'état
# d'activation d'une unité ne se lit nulle part. Une trace des appels en tient
# lieu — ceux d'ogs comme ceux des scripts du paquet —, et c'est le dernier
# « enable » ou « disable » du timer qui dit son état. Sous un vrai systemd,
# c'est lui qu'on interroge.
#
# Les scripts du paquet ne voient la trace que si apt le veut : apt 3
# (Debian 13) fixe DPkg::Path à « /usr/sbin:/usr/bin:/sbin:/bin », et sous
# apt-get, dpkg les lance sans /usr/local — ni la trace, ni le substitut. Ils
# appelaient le vrai systemctl, qui échoue sans init et échappe à la trace sous
# systemd : la mise à jour n'éprouvait rien. Chaque apt-get leur rend donc le
# PATH du scénario.
apt_paquet() {
    apt-get -o DPkg::Path="$PATH" "$@"
}
trace=/tmp/systemctl.trace
: >"$trace"
cat >/usr/local/sbin/systemctl <<EOF
#!/bin/sh
echo "\$*" >> $trace
exec /usr/local/bin/systemctl "\$@"
EOF
chmod +x /usr/local/sbin/systemctl
if [ "$(command -v systemctl)" != /usr/local/sbin/systemctl ]; then
    echo "ÉCHEC : la trace de systemctl n'est pas en tête du PATH ($(command -v systemctl))"
    exit 1
fi

etat_timer() {
    if [ -d /run/systemd/system ]; then
        systemctl is-enabled ogs-renew.timer 2>/dev/null || true
        return
    fi
    case "$(grep -E '^(enable|disable) (.* )?ogs-renew\.timer( |$)' "$trace" | tail -1)" in
    enable*) echo enabled ;;
    disable*) echo disabled ;;
    *) echo inconnu ;;
    esac
}

exiger_timer_actif() {
    local etat
    etat=$(etat_timer)
    if [ "$etat" != enabled ]; then
        echo "ÉCHEC : ogs-renew.timer est « $etat » $1 : plus aucun certificat ne se renouvelle"
        exit 1
    fi
    if [ -d /run/systemd/system ] && ! systemctl is-active --quiet ogs-renew.timer; then
        echo "ÉCHEC : ogs-renew.timer est activé mais arrêté $1"
        exit 1
    fi
    echo "ogs-renew.timer actif $1"
}

# verifier_scripts échoue sur ce que signalent les scripts de maintenance : ils
# ne parlent d'un problème que sous « ogs : » — migration en échec, clé maître
# absente, timer non activé ou dont l'état n'a pas pu être noté.
verifier_scripts() {
    local sortie=$1 moment=$2
    if printf '%s\n' "$sortie" | grep -q '^ogs : '; then
        printf '%s\n' "$sortie" | grep '^ogs : '
        echo "ÉCHEC : les scripts du paquet signalent un problème $moment"
        exit 1
    fi
    # apt donne à dpkg un pseudo-terminal : une commande d'ogs qui y écrit
    # interroge le terminal sur sa couleur de fond, attend cinq secondes faute de
    # réponse, et la requête reste dans le journal d'apt.
    if printf '%s' "$sortie" | grep -qF $'\e]11;?'; then
        echo "ÉCHEC : une commande d'ogs interroge le terminal d'apt $moment"
        exit 1
    fi
}

# installer pose un paquet et vérifie ce qu'en disent les scripts de maintenance.
installer() {
    local sortie
    if ! sortie=$(apt_paquet install -y -qq "$1" 2>&1); then
        echo "$sortie"
        echo "ÉCHEC : l'installation de $1 a échoué"
        exit 1
    fi
    echo "$sortie" | grep -vi "^Selecting\|^Reading\|^Preparing\|^Unpacking\|^Setting up\|^(Reading" | head -5
    verifier_scripts "$sortie" "à l'installation de $(basename "$1")"
}

echo "--- installation d'une version jusqu'à 0.0.3 ---"
installer "$atelier/ogs-0.0.3.deb"

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
php=$(version_php)
ogs init --quiet
choisir_moteur apache
ogs site create paquet.test --kind php-plain --php "$php" --tls=false --yes 2>&1 | tail -3
if [ ! -d /home/paquet.test ]; then
    echo "ÉCHEC : le site n'a pas été créé après installation du paquet"
    exit 1
fi
if ! head -n 1 /etc/systemd/system/ogs-renew.timer 2>/dev/null | grep -qF 'Généré par ogs'; then
    echo "ÉCHEC : le plan global n'a pas écrit ogs-renew.timer"
    exit 1
fi
exiger_timer_actif "après la création du premier site"

echo "--- mise à jour depuis 0.0.3 ---"
debut=$(wc -l <"$trace")
installer "$atelier/ogs-suivant.deb"
if ! tail -n +"$((debut + 1))" "$trace" | grep -q '^disable .*ogs-renew\.timer'; then
    echo "ÉCHEC : le prerm de 0.0.3 n'a pas désactivé le timer, ou pas par la trace (DPkg::Path d'apt ?) : la mise à jour n'éprouve rien"
    exit 1
fi
exiger_timer_actif "après la mise à jour depuis 0.0.3"

echo "--- mise à jour entre deux versions corrigées ---"
debut=$(wc -l <"$trace")
installer "$atelier/ogs-ensuite.deb"
if tail -n +"$((debut + 1))" "$trace" | grep -Eq '^(disable|stop) .*ogs-renew\.timer'; then
    echo "ÉCHEC : la mise à jour a arrêté le timer :"
    tail -n +"$((debut + 1))" "$trace"
    exit 1
fi
exiger_timer_actif "après une mise à jour"

echo "--- désinstallation ---"
# Le projet créé doit survivre à la désinstallation : retirer l'outil
# d'administration ne doit pas défaire les sites qu'il gère.
if ! sortie=$(apt_paquet remove -y -qq ogs 2>&1); then
    echo "$sortie"
    echo "ÉCHEC : le retrait a échoué"
    exit 1
fi
echo "$sortie" | tail -1
verifier_scripts "$sortie" "au retrait"
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
# Le timer appellerait un binaire disparu : le retrait, lui, l'arrête.
if [ "$(etat_timer)" = enabled ]; then
    echo "ÉCHEC : le timer est encore activé après le retrait"
    exit 1
fi
# Le postrm garde le paquet en « rc » : sans lui, dpkg l'oubliait, et la purge
# n'avait plus rien à faire.
if [ "$(dpkg-query -W -f='${db:Status-Abbrev}' ogs 2>/dev/null)" != "rc " ]; then
    echo "ÉCHEC : le paquet retiré n'est pas en « rc » : $(dpkg-query -W -f='${db:Status-Abbrev}' ogs 2>&1)"
    exit 1
fi

echo "--- réinstallation après retrait ---"
installer "$atelier/ogs-ensuite.deb"
exiger_timer_actif "après la réinstallation"

echo "--- purge ---"
if ! sortie=$(apt_paquet purge -y -qq ogs 2>&1); then
    echo "$sortie"
    echo "ÉCHEC : la purge a échoué"
    exit 1
fi
echo "$sortie" | grep -v "^(Reading"
# dpkg ne nomme ce qui reste que d'un « not empty so not removed » : c'est au
# paquet de dire ce qu'il garde, et pourquoi.
for garde in /etc/ogs /var/lib/ogs /var/log/ogs; do
    if ! printf '%s\n' "$sortie" | grep -qF "ogs : $garde est conservé"; then
        echo "ÉCHEC : la purge ne dit pas que $garde est conservé"
        exit 1
    fi
done
for unite in ogs-renew.service ogs-renew.timer; do
    if [ -e "/etc/systemd/system/$unite" ]; then
        echo "ÉCHEC : $unite survit à la purge, et appellerait un binaire disparu"
        exit 1
    fi
done
for garde in /etc/ogs/master.key /etc/ogs/projects/paquettest.yaml /var/lib/ogs/state.db /home/paquet.test; do
    if [ ! -e "$garde" ]; then
        echo "ÉCHEC : la purge a supprimé $garde"
        exit 1
    fi
done
if [ -e /var/lib/ogs/ogs-renew.timer.avant-retrait ]; then
    echo "ÉCHEC : la purge laisse l'état du timer noté par le paquet"
    exit 1
fi
if dpkg-query -W ogs >/dev/null 2>&1; then
    echo "ÉCHEC : dpkg connaît encore le paquet après la purge"
    exit 1
fi
echo
echo "RÉSULTAT : le paquet s'installe, se met à jour sans arrêter le renouvellement, fonctionne, se retire et se purge sans toucher aux projets."
