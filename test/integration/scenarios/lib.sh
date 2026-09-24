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
