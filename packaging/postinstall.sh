#!/bin/sh
# Après installation : préparer l'état sans rien imposer. La configuration
# proprement dite reste du ressort de « ogs init », qui engendre la clé maître :
# sur une machine neuve, le paquet se contente de l'annoncer.
set -e

timer=ogs-renew.timer
unite=/etc/systemd/system/$timer
# Noté par le prerm au retrait : l'état du timer avant qu'il ne l'arrête.
temoin=/var/lib/ogs/ogs-renew.timer.avant-retrait
# Dernière version dont le prerm désactivait le timer à chaque mise à jour. Une
# construction postérieure à l'étiquette porte encore ce numéro tant qu'aucune
# autre n'est posée (Makefile) : la réparation s'y applique aussi, et rallume
# alors un timer que l'administrateur avait éteint. D'où une étiquette au-delà
# de 0.0.3 avant toute diffusion de ce correctif — apt tiendrait d'ailleurs pour
# déjà installé un paquet de même numéro.
derniere_fautive=0.0.3

# timer_d_ogs dit si le timer en place est celui qu'écrit ogs. Il n'est pas
# livré par le paquet mais par le plan global d'« ogs apply » : un timer de même
# nom posé par l'administrateur ne regarde pas le paquet.
timer_d_ogs() {
    [ -f "$unite" ] && head -n 1 "$unite" | grep -qF 'Généré par ogs'
}

activer_timer() {
    systemctl enable --now "$timer" >/dev/null 2>&1 ||
        echo "ogs : échec de l'activation de $timer" >&2
}

# retablir_timer rend au timer l'état que le paquet lui a pris, et rien d'autre.
# Deux cas seulement :
# - un retrait l'a arrêté après avoir noté son état : on rend cet état ;
# - la version en place jusqu'ici ne dépasse pas 0.0.3 : dpkg a exécuté son
#   prerm, qui a désactivé le timer sans rien noter. Or ogs active le timer
#   chaque fois qu'il l'écrit : un timer portant son en-tête était actif.
# Hors de là, le paquet n'y a pas touché et l'état est celui qu'a voulu
# l'administrateur. Un timer désactivé est alors signalé, sans passer outre :
# c'est un parc dont plus aucun certificat ne se renouvelle, et aucune commande
# d'ogs ne le dit.
retablir_timer() {
    command -v systemctl >/dev/null 2>&1 || return 0
    if [ -f "$temoin" ]; then
        etat=$(cat "$temoin" 2>/dev/null || true)
        rm -f "$temoin"
        if [ "$etat" = enabled ] && timer_d_ogs; then
            activer_timer
        fi
    elif [ -n "${1:-}" ] && dpkg --compare-versions "$1" le "$derniere_fautive" && timer_d_ogs; then
        activer_timer
    fi
    if timer_d_ogs && [ "$(systemctl is-enabled "$timer" 2>/dev/null || true)" != enabled ]; then
        echo "ogs : $timer n'est pas activé, aucun certificat ne sera renouvelé. Pour le rétablir :" >&2
        echo "  systemctl enable --now $timer" >&2
    fi
}

case "$1" in
configure)
    if [ ! -f /etc/ogs/config.yaml ]; then
        echo "ogs installé. Préparer l'instance avec :"
        echo "  ogs init"
    elif [ ! -e /etc/ogs/master.key ]; then
        # La clé maître a disparu d'une instance configurée. « ogs init » en
        # engendrerait une nouvelle, et sous --quiet sans un mot : les secrets
        # scellés par l'ancienne ne se reliraient plus, et c'est la nouvelle que
        # l'administrateur sauvegarderait. Le choix lui revient.
        echo "ogs : clé maître absente (/etc/ogs/master.key) : « ogs init » n'est pas relancé, il en engendrerait une nouvelle." >&2
        echo "  La restaurer depuis sa sauvegarde — les secrets scellés ne se relisent qu'avec elle —, ou lancer « ogs init » pour repartir d'une clé neuve." >&2
    else
        # Mise à jour ou réinstallation. « ogs init » est idempotent : il crée
        # les répertoires qu'attend la version nouvelle et migre la base d'état.
        # Toute commande migrerait aussi, mais c'est ici qu'un échec se lit — pas
        # dans le journal du premier passage nocturne du timer. Il n'est jamais
        # fatal : le binaire nouveau est en place, et un paquet laissé à moitié
        # configuré n'y changerait rien.
        #
        # Sa sortie standard est écartée, ses erreurs non : apt donne à dpkg un
        # pseudo-terminal, et toute commande d'ogs y interroge le terminal sur
        # sa couleur de fond. Sans personne pour répondre, comme sous
        # unattended-upgrades, elle attend cinq secondes, et la requête reste
        # dans le journal d'apt.
        if ! ogs init --quiet >/dev/null; then
            echo "ogs : « ogs init » a échoué, la base d'état n'est peut-être pas migrée. Le relancer une fois la cause levée." >&2
        fi
    fi
    # Configuration ou non : un timer que le retrait a arrêté est rendu même si
    # config.yaml a disparu d'ici la réinstallation. ogs tourne alors sur ses
    # réglages par défaut, et apply ne réactive le timer qu'en réécrivant son
    # fichier.
    retablir_timer "${2:-}"
    ;;
abort-remove)
    # Le prerm a échoué : le paquet reste en place, le timer doit repartir.
    retablir_timer
    ;;
esac
