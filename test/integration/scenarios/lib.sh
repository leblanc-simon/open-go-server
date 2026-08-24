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
}
