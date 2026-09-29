#!/bin/sh
# Après suppression. Seule la purge agit : un retrait simple garde tout ce qu'une
# réinstallation reprend, et le prerm a déjà arrêté le timer.
#
# La seule présence de ce script change le retrait : sans postrm ni fichier de
# configuration, dpkg oubliait le paquet dès « apt remove », et « apt purge »
# n'avait plus rien à faire.
#
# La purge ne retire que ce qui ne sert plus sans l'outil : les deux unités de
# renouvellement, qui appellent un binaire disparu, et l'état que le paquet tient
# pour lui-même. Elle ne touche ni aux déclarations, ni à la clé maître — sans
# elle, ni les secrets ni les archives ne se relisent —, ni à /var/lib/ogs, dont
# les vhosts servent encore les certificats, ni au journal d'audit, et le dit :
# dpkg ne les nomme que d'un « not empty so not removed ». Les archives de
# projets supprimés non plus : elles portent le dump des bases, et leur délai de
# conservation n'était tenu que par le plan global d'« ogs apply », qui ne
# tournera plus. Le paquet les énumère plutôt que d'en décider.
set -e

case "$1" in
purge)
    rm -f /var/lib/ogs/ogs-renew.timer.avant-retrait

    retirees=
    for unite in ogs-renew.timer ogs-renew.service; do
        fichier=/etc/systemd/system/$unite
        # Seules les unités qu'ogs a écrites : une unité de même nom posée par
        # l'administrateur reste en place.
        if [ -f "$fichier" ] && head -n 1 "$fichier" | grep -qF 'Généré par ogs'; then
            rm -f "$fichier"
            retirees="$retirees $unite"
        fi
    done
    if [ -n "$retirees" ] && command -v systemctl >/dev/null 2>&1; then
        systemctl daemon-reload >/dev/null 2>&1 || true
    fi

    if [ -d /etc/ogs ]; then
        echo "ogs : /etc/ogs est conservé — déclarations des projets et clé maître, sans laquelle ni les secrets ni les archives ne se relisent."
    fi
    if [ -d /var/lib/ogs ]; then
        echo "ogs : /var/lib/ogs est conservé — base d'état, comptes ACME et certificats, que les vhosts servent encore."
    fi
    if [ -d /var/log/ogs ]; then
        echo "ogs : /var/log/ogs est conservé — journal d'audit et journal de l'outil."
    fi
    archives=/var/lib/ogs/archives
    if [ -d "$archives" ]; then
        nombre=$(find "$archives" -mindepth 1 -maxdepth 1 -name '*.tar.zst' | wc -l)
        if [ "$nombre" -gt 0 ]; then
            taille=$(du -sh "$archives" 2>/dev/null | cut -f1)
            echo "ogs : $nombre archive(s) de projets supprimés restent dans $archives (${taille:-taille inconnue})."
            echo "  Elles portent le dump de leurs bases, et plus rien ne les purge : les retirer à la main quand elles ne servent plus."
        fi
    fi
    ;;
esac
