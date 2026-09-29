#!/bin/sh
# Avant suppression : arrêter le renouvellement automatique des certificats, au
# seul retrait de l'outil. Les ressources des projets — comptes, bases, vhosts —
# ne sont jamais touchées : désinstaller l'outil d'administration ne doit pas
# défaire les sites qu'il gère.
#
# dpkg appelle aussi ce script à chaque mise à jour, réinstallation ou retour
# arrière (« upgrade », ou « failed-upgrade » quand le prerm de la version en
# place a échoué), et quand l'installation d'un autre paquet oblige à
# déconfigurer celui-ci (« deconfigure ») : l'outil reste alors en place, le
# timer aussi. Les versions jusqu'à 0.0.3 le désactivaient sur « upgrade », et
# rien ne le réactivait — apply ne demande l'activation qu'à l'écriture du
# fichier d'unité : après la première mise à jour, plus aucun certificat ne se
# renouvelait, sans qu'aucune commande ne le dise.
set -e

timer=ogs-renew.timer
unite=/etc/systemd/system/$timer
# L'état du timer au moment du retrait : une réinstallation le rend tel quel
# (postinst), la purge l'efface (postrm).
temoin=/var/lib/ogs/ogs-renew.timer.avant-retrait

case "$1" in
remove)
    if command -v systemctl >/dev/null 2>&1; then
        # Le timer est écrit par ogs, non livré par le paquet : seul celui qui
        # porte son en-tête est relevé. Un timer désactivé par l'administrateur
        # doit le rester à la réinstallation, d'où l'état noté plutôt qu'un
        # simple drapeau.
        if [ -f "$unite" ] && head -n 1 "$unite" | grep -qF 'Généré par ogs'; then
            etat=$(systemctl is-enabled "$timer" 2>/dev/null || true)
            if ! printf '%s\n' "${etat:-inconnu}" 2>/dev/null >"$temoin"; then
                echo "ogs : état de $timer non noté dans $temoin, une réinstallation ne le réactivera pas" >&2
            fi
        fi
        # Le binaire va disparaître : le timer échouerait à chaque passage.
        systemctl disable --now "$timer" >/dev/null 2>&1 || true
    fi
    ;;
esac
