# Fonctions communes aux scénarios.

# choisir_moteur fixe le moteur web dans la configuration. L'image porte Apache
# et nginx : ogs refuse — à raison — de choisir seul, et chaque scénario doit
# donc déclarer celui qu'il éprouve.
choisir_moteur() {
    OGS_MOTEUR="$1" python3 - <<'PY'
import os
import re

chemin = "/etc/ogs/config.yaml"
moteur = os.environ["OGS_MOTEUR"]
s = open(chemin).read()
if re.search(r"^web:$", s, flags=re.M):
    s = re.sub(r"^web:\n(  engine:.*\n)?", "web:\n  engine: %s\n" % moteur, s, count=1, flags=re.M)
else:
    s += "\nweb:\n  engine: %s\n" % moteur
open(chemin, "w").write(s)
PY
    # Et il arrête l'autre. Les deux moteurs sont activés dans l'image : sous un
    # vrai systemd, ils se disputent le port 80 au démarrage, et le perdant reste
    # « failed » — le scénario éprouve alors un serveur qui n'a jamais chargé les
    # vhosts d'ogs, ou la page par défaut de Debian servie par le vainqueur
    # depuis /var/www/html. Le résultat dépendait d'une course.
    if [ -d /run/systemd/system ]; then
        # Le moteur « apache » a pour unité « apache2 » : relancer « apache »
        # visait une unité inexistante, en silence, et Apache restait « failed »
        # jusqu'à ce qu'un apply d'ogs le relève — ce qui masquait le défaut dans
        # tout scénario qui n'interroge pas le port 80 avant son premier apply.
        local unite=$1 autre=nginx
        [ "$1" = apache ] && unite=apache2
        [ "$1" = nginx ] && autre=apache2
        systemctl disable --now "$autre" >/dev/null 2>&1 || true
        # Passé StartLimitBurst, systemd refuse de démarrer alors que la cause est
        # levée : acquitter le compteur avant de relever le moteur retenu.
        systemctl reset-failed "$unite" >/dev/null 2>&1 || true
        systemctl restart "$unite" >/dev/null 2>&1 || true
    fi
}

# relancer_fpm arrête tout FPM de la version, en relance un maître, et ne rend la
# main qu'une fois ce maître prêt et seul à répondre pour le site host, dont le
# docroot reçoit une sonde. Sans init, c'est la seule façon de faire relire
# php.ini à coup sûr.
#
# « pkill -f "php-fpm: master" » puis « --daemonize » après une seconde ne
# suffisait pas : un maître que USR2 ré-exécute porte, jusqu'à ce qu'il réécrive
# son titre, la ligne de commande « /usr/sbin/php-fpm<v> --daemonize », que ce
# motif manque. Il survivait, le maître relancé échouait faute de socket (code
# 70, ignoré), et la réponse venait d'un maître qui avait lu php.ini avant ou
# après sa modification selon quelques millisecondes : « phpinfo, fermée par
# php.ini, a été rouverte par le pool », par intermittence. L'arrêt vise donc
# tout maître de la version, sous son titre comme sous sa ligne de commande,
# jusqu'à ce qu'il n'en reste aucun ; le code du démarrage est vérifié ; et la réponse doit venir d'un
# worker dont le parent est le maître relancé — une réponse de l'ancien, ou de
# l'opcache d'un autre maître, ne passe pas.
relancer_fpm() {
    local version=$1 host=$2 docroot=$3
    local binaire="/usr/sbin/php-fpm$version" pidfile="/run/php/php$version-fpm.pid"
    # Le maître de cette version seule — son titre nomme sa configuration —, ou
    # tout processus qui porte encore la ligne de commande du binaire (maître
    # en cours de ré-exécution). Les workers partent avec leur maître ; un FPM
    # d'une autre version n'est pas touché.
    local v=${version//./\\.}
    local motif="^(php-fpm: master process \\(/etc/php/$v/|/usr/sbin/php-fpm$v( |\$))"
    local _
    for _ in $(seq 1 100); do
        pgrep -f "$motif" >/dev/null || break
        pkill -TERM -f "$motif" 2>/dev/null
        sleep 0.1
    done
    if pgrep -f "$motif" >/dev/null; then
        echo "ÉCHEC : FPM $version ne s'arrête pas"
        pgrep -fa "$motif"
        return 1
    fi
    rm -f "$pidfile"
    if ! "$binaire" --daemonize; then
        echo "ÉCHEC : le maître FPM $version ne démarre pas"
        return 1
    fi
    local maitre
    maitre=$(cat "$pidfile")
    printf '<?php echo "maitre=", posix_getppid();\n' >"$docroot/sonde-fpm.php"
    chown "$(stat -c %U:%G "$docroot")" "$docroot/sonde-fpm.php"
    local reponse=""
    for _ in $(seq 1 100); do
        reponse=$(curl -s -H "Host: $host" "http://127.0.0.1/sonde-fpm.php")
        [ "$reponse" = "maitre=$maitre" ] && break
        sleep 0.1
    done
    rm -f "$docroot/sonde-fpm.php"
    if [ "$reponse" != "maitre=$maitre" ]; then
        echo "ÉCHEC : le maître FPM relancé ($maitre) ne répond pas pour $host : ${reponse:-(rien)}"
        return 1
    fi
}
