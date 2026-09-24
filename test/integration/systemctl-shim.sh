#!/bin/sh
# Substitut de systemctl pour les essais en conteneur, où il n'y a pas d'init.
#
# Il ne fait que ce que systemd ferait de vérifiable ici : dispatcher les actions
# sur les services réellement installés. Tout ce qu'il ne sait pas faire échoue
# explicitement — un substitut complaisant rendrait les scénarios vides de sens.
set -u

# Quand systemd tourne réellement — le scénario dédié le démarre en PID 1 —, ce
# substitut n'a rien à faire : il s'efface devant le vrai.
if [ -d /run/systemd/system ]; then
    exec /usr/bin/systemctl "$@"
fi

verbe=""
unites=""
maintenant=0
for arg in "$@"; do
    case "$arg" in
    --now) maintenant=1 ;;
    -*) ;; # --quiet, --no-pager… sans effet ici
    *)
        if [ -z "$verbe" ]; then verbe="$arg"; else unites="$unites $arg"; fi
        ;;
    esac
done

# Réduit « php8.4-fpm.service » à « php8.4-fpm ».
nom() { echo "${1%.service}"; }

fpm_binaire() { echo "/usr/sbin/php-fpm$(echo "$1" | sed 's/^php\(.*\)-fpm$/\1/')"; }

agir() {
    action=$1
    unite=$(nom "$2")
    case "$unite" in
    apache2)
        case "$action" in
        start) apache2ctl start ;;
        stop) apache2ctl stop ;;
        restart) apache2ctl restart ;;
        reload) apache2ctl graceful ;;
        esac
        ;;
    nginx)
        case "$action" in
        start) nginx ;;
        stop) nginx -s quit ;;
        restart) nginx -s quit 2>/dev/null; nginx ;;
        reload) nginx -s reload ;;
        esac
        ;;
    php*-fpm)
        binaire=$(fpm_binaire "$unite")
        [ -x "$binaire" ] || { echo "systemctl (substitut) : $binaire absent" >&2; return 1; }
        case "$action" in
        start) "$binaire" --daemonize ;;
        # USR2 est bien ce que systemd envoie pour un reload de FPM.
        reload) pkill -USR2 -f "$binaire" ;;
        stop) pkill -QUIT -f "$binaire" ;;
        restart) pkill -QUIT -f "$binaire" 2>/dev/null; sleep 1; "$binaire" --daemonize ;;
        esac
        ;;
    ogs-*)
        # Les unités d'ogs sont écrites par l'outil ; sans init, il n'y a rien à
        # démarrer. On l'admet, mais on le dit.
        echo "systemctl (substitut) : $action $unite ignoré, pas d'init dans ce conteneur" >&2
        ;;
    *)
        echo "systemctl (substitut) : unité inconnue $unite" >&2
        return 1
        ;;
    esac
}

case "$verbe" in
daemon-reload | daemon-reexec | preset | reset-failed)
    exit 0
    ;;
enable | disable)
    # L'activation ne fait que poser des liens, que rien ici ne consulte. Avec
    # --now, « disable » arrête : le substitut démarrait l'unité dans les deux cas.
    if [ "$maintenant" = 1 ]; then
        action=start
        [ "$verbe" = disable ] && action=stop
        for u in $unites; do agir "$action" "$u" || exit 1; done
    fi
    exit 0
    ;;
start | stop | restart | reload | reload-or-restart)
    action=$verbe
    [ "$action" = "reload-or-restart" ] && action=restart
    for u in $unites; do agir "$action" "$u" || exit 1; done
    exit 0
    ;;
is-active)
    for u in $unites; do
        case "$(nom "$u")" in
        apache2) pgrep -x apache2 >/dev/null || exit 3 ;;
        nginx) pgrep -x nginx >/dev/null || exit 3 ;;
        # La version, pas tout « php-fpm » : un FPM d'une autre version rendrait
        # active une unité arrêtée. Le maître réécrit son titre — « php-fpm:
        # master process (/etc/php/<v>/fpm/php-fpm.conf) » —, d'où un motif qui
        # reconnaît aussi le binaire « php-fpm<v> ».
        php*-fpm)
            v=$(nom "$u" | sed 's/^php\(.*\)-fpm$/\1/')
            pgrep -f "php-fpm.*$v" >/dev/null || exit 3
            ;;
        # Une unité d'ogs n'est jamais active ici : le dire évite qu'un scénario
        # conclue à tort qu'un service tourne.
        *) exit 3 ;;
        esac
    done
    echo active
    exit 0
    ;;
is-enabled)
    echo enabled
    exit 0
    ;;
show | status | cat | list-units | list-timers | list-unit-files)
    exit 0
    ;;
*)
    echo "systemctl (substitut) : verbe non pris en charge : $verbe" >&2
    exit 1
    ;;
esac
