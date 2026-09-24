#!/bin/bash
# =============================================================================
# migrer.sh — migration d'un ancien serveur vers un serveur géré par ogs
# =============================================================================
#
# Lit un manifeste rempli d'après le modèle docs/migration.yaml, et enchaîne les phases qui y sont
# décrites (§ Déroulé). S'exécute sur la CIBLE, en root ; tire fichiers et bases
# de la source par ssh.
#
# PAR DÉFAUT, RIEN N'EST MODIFIÉ : chaque commande qui changerait quelque chose
# est affichée, pas exécutée. Les lectures (ssh en lecture, dig, ogs plan) sont
# faites pour de bon. `--executer` passe à l'acte.
#
# Usage :
#   migrer.sh -m <manifeste> [options] <phase> [projet | vague:N | tous]...
#
# Phases :
#   controle       valide le manifeste (champs requis, « À VÉRIFIER » restants)
#   prealables     outils, ogs doctor, accès ssh à la source, TTL des zones
#   declarer       dépose la déclaration ogs (crons et services désactivés), apply
#   synchroniser   copie fichiers et bases, réécrit la configuration — répétable,
#                  sans interruption de service
#   verifier       rejoue les vérifications du manifeste contre la cible
#                  (--dns : par le DNS public au lieu de forcer l'adresse)
#   basculer       maintenance, dernière copie, DNS, activation — dans la fenêtre
#   retour         retour arrière : cible désactivée, source rétablie
#   etat           où en est chaque projet
#
# Options :
#   -m, --manifeste <fichier>   le manifeste rempli (obligatoire : le modèle du
#                               dépôt ne se remplit pas en place)
#   --executer                  exécuter pour de bon (sinon simulation)
#   --oui                       ne pas demander de confirmation
#   --dns                       (verifier) passer par le DNS public
#   --delai-dns <secondes>      attente maximale de la propagation (défaut 1800)
#   --delai-amqp <secondes>     attente maximale du vidage des files (défaut 900)
#
# Variable d'environnement : OGS, la commande ogs (défaut « ogs »). Pour un essai
# sans privilèges : OGS="./ogs --prefix ./dev" migrer.sh -m m.yaml declarer shopfr
#
# Dépendances sur la cible : python3 et python3-yaml, rsync, curl, dig, gzip.
# Sur la source : rsync, mysqldump ou pg_dump.
#
# Codes de retour : 0 succès · 1 erreur · 2 usage · 3 manifeste invalide.
# =============================================================================

set -Eeuo pipefail

MANIFESTE=''
EXECUTER=0
OUI=0
PAR_DNS=0
DELAI_DNS=1800
DELAI_AMQP=900
RESOLVEURS=(1.1.1.1 8.8.8.8)
read -ra OGS <<<"${OGS:-ogs}"

# -----------------------------------------------------------------------------
# Affichage
# -----------------------------------------------------------------------------
if [[ -t 1 ]]; then
    C_TITRE=$'\e[1;34m' C_OK=$'\e[32m' C_AVERT=$'\e[33m' C_ERR=$'\e[31m' C_CMD=$'\e[2m' C_RAZ=$'\e[0m'
else
    C_TITRE='' C_OK='' C_AVERT='' C_ERR='' C_CMD='' C_RAZ=''
fi
titre()  { printf '\n%s══ %s%s\n' "$C_TITRE" "$*" "$C_RAZ"; }
etape()  { printf '%s── %s%s\n' "$C_TITRE" "$*" "$C_RAZ"; }
info()   { printf '   %s\n' "$*"; }
ok()     { printf '%s   ✓ %s%s\n' "$C_OK" "$*" "$C_RAZ"; }
avert()  { printf '%s   ⚠ %s%s\n' "$C_AVERT" "$*" "$C_RAZ" >&2; }
erreur() { printf '%s   ✗ %s%s\n' "$C_ERR" "$*" "$C_RAZ" >&2; }
fatal()  { erreur "$*"; exit 1; }

usage() {
    sed -n '/^# Usage/,/^# Codes de retour/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 2
}

# -----------------------------------------------------------------------------
# Arguments
# -----------------------------------------------------------------------------
PHASE=''
SELECTEURS=()
while (($#)); do
    case "$1" in
        -m|--manifeste) MANIFESTE="${2:?}"; shift 2 ;;
        --executer)     EXECUTER=1; shift ;;
        --oui)          OUI=1; shift ;;
        --dns)          PAR_DNS=1; shift ;;
        --delai-dns)    DELAI_DNS="${2:?}"; shift 2 ;;
        --delai-amqp)   DELAI_AMQP="${2:?}"; shift 2 ;;
        -h|--help)      usage ;;
        -*)             erreur "option inconnue : $1"; usage ;;
        *)              if [[ -z "$PHASE" ]]; then PHASE="$1"; else SELECTEURS+=("$1"); fi; shift ;;
    esac
done
[[ -n "$PHASE" ]] || usage
[[ -n "$MANIFESTE" ]] || { erreur "préciser le manifeste rempli avec -m"; usage; }
[[ -r "$MANIFESTE" ]] || fatal "manifeste illisible : $MANIFESTE"
MANIFESTE="$(realpath "$MANIFESTE")"

# -----------------------------------------------------------------------------
# Lecture du manifeste : un assistant Python, le YAML ne se lit pas en shell
# -----------------------------------------------------------------------------
PY="$(mktemp --suffix=.py)"
TEMPORAIRES=("$PY")
BASCULE_EN_COURS=''
nettoyer() {
    local rc=$?
    if [[ -n "$BASCULE_EN_COURS" && $rc -ne 0 ]]; then
        erreur "bascule de $BASCULE_EN_COURS interrompue : la source est peut-être en maintenance,"
        erreur "ses tâches arrêtées. Reprendre : $0 --executer basculer $BASCULE_EN_COURS"
        erreur "ou revenir en arrière :    $0 --executer retour $BASCULE_EN_COURS"
    fi
    rm -rf "${TEMPORAIRES[@]}"
}
trap nettoyer EXIT

cat >"$PY" <<'PYEOF'
import copy, json, re, sys, urllib.parse
try:
    import yaml
except ImportError:
    sys.exit("python3-yaml est absent : apt install python3-yaml")

# Séparateur de champs : un caractère non blanc, sans quoi « read » fusionne les
# champs vides consécutifs et décale tous les suivants.
SEP = "\x1f"
FORMATS = {"dotenv", "parameters-yml", "prestashop-php", "wp-config", "autre"}
MOTEURS = {"mysql", "mariadb", "postgres"}

def charger(chemin):
    with open(chemin, encoding="utf-8") as f:
        texte = f.read()
    return texte, (yaml.safe_load(texte) or {})

def projets(d):
    return d.get("projets") or []

def trouver(d, pid):
    for p in projets(d):
        if p.get("id") == pid:
            return p
    sys.exit(f"projet inconnu dans le manifeste : {pid}")

def chemin(obj, path):
    for part in (path.split(".") if path else []):
        if obj is None:
            return None
        if isinstance(obj, list):
            i = int(part)
            obj = obj[i] if i < len(obj) else None
        elif isinstance(obj, dict):
            obj = obj.get(part)
        else:
            return None
    return obj

def fmt(v):
    if v is None:
        return ""
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, list):
        return ",".join(fmt(x) for x in v)
    return str(v)

def sans_nuls(o):
    if isinstance(o, dict):
        return {k: sans_nuls(v) for k, v in o.items() if v is not None}
    if isinstance(o, list):
        return [sans_nuls(v) for v in o]
    return o

def ecrire_yaml(chemin_fichier, donnees):
    with open(chemin_fichier, "w", encoding="utf-8") as f:
        yaml.safe_dump(donnees, f, allow_unicode=True, sort_keys=False, default_flow_style=False)

def blocs_projets(texte):
    """Découpe le texte brut par projet, pour y chercher les « À VÉRIFIER »."""
    debut = re.search(r"^projets:\s*$", texte, re.M)
    if not debut:
        return {}
    corps = texte[debut.end():]
    morceaux = re.split(r"^  - id:\s*", corps, flags=re.M)[1:]
    out = {}
    for m in morceaux:
        pid = m.split(None, 1)[0] if m.strip() else ""
        out[pid] = m
    return out

# --- commandes -----------------------------------------------------------------

def c_get(m, pid, path):
    _, d = charger(m)
    racine = d if pid == "-" else trouver(d, pid)
    print(fmt(chemin(racine, path)))

def c_lignes(m, pid, path, *champs):
    _, d = charger(m)
    racine = d if pid == "-" else trouver(d, pid)
    for el in chemin(racine, path) or []:
        if champs:
            print(SEP.join(fmt(el.get(c)) if isinstance(el, dict) else "" for c in champs))
        else:
            print(fmt(el))

def c_ids(m, *selecteurs):
    _, d = charger(m)
    tous = sorted(enumerate(projets(d)), key=lambda t: (t[1].get("vague") or 0, t[0]))
    tous = [p for _, p in tous]
    vus, out = set(), []
    for s in selecteurs or ("tous",):
        if s == "tous":
            choix = tous
        elif s.startswith("vague:"):
            n = int(s.split(":", 1)[1])
            choix = [p for p in tous if p.get("vague") == n]
            if not choix:
                sys.exit(f"aucun projet dans la vague {n}")
        else:
            choix = [trouver(d, s)]
        for p in choix:
            if p["id"] not in vus:
                vus.add(p["id"])
                out.append(p["id"])
    print("\n".join(out))

def c_controle(m, *ids):
    texte, d = charger(m)
    erreurs = []
    def e(pid, msg):
        erreurs.append(f"{pid}: {msg}")
    if d.get("version") != 1:
        e("manifeste", "version 1 attendue")
    for cle in ("source.ssh", "cible.ip_publique"):
        if chemin(d, cle) in (None, ""):
            e("manifeste", f"{cle} est requis")
    blocs = blocs_projets(texte)
    vus = set()
    for p in projets(d):
        pid = p.get("id")
        if not pid:
            e("?", "projet sans id")
            continue
        if pid in vus:
            e(pid, "id en double")
        vus.add(pid)
        if ids and pid not in ids:
            continue
        if "À VÉRIFIER" in blocs.get(pid, ""):
            n = blocs[pid].count("À VÉRIFIER")
            e(pid, f"{n} valeur(s) encore « À VÉRIFIER »")
        if not isinstance(p.get("vague"), int):
            e(pid, "vague : entier requis")
        o = p.get("ogs") or {}
        if o.get("apiVersion") != "ogs/v1" or o.get("kind") != "Project":
            e(pid, "ogs : apiVersion ogs/v1 et kind Project requis")
        if chemin(o, "metadata.name") != pid:
            e(pid, f"ogs.metadata.name ({chemin(o, 'metadata.name')}) doit valoir l'id")
        if not chemin(o, "metadata.host"):
            e(pid, "ogs.metadata.host est requis")
        src = p.get("source") or {}
        for cle in ("utilisateur", "racine", "code"):
            if not src.get(cle):
                e(pid, f"source.{cle} est requis")
        if src.get("racine") and not str(src["racine"]).startswith("/"):
            e(pid, "source.racine doit être absolu")
        for dh in src.get("donnees_hors_code") or []:
            if not dh.get("de") or not dh.get("vers"):
                e(pid, "donnees_hors_code : « de » et « vers » requis")
            elif str(dh["vers"]).startswith("/") or ".." in str(dh["vers"]).split("/"):
                e(pid, f"donnees_hors_code : « vers » doit rester sous le home ({dh['vers']})")
        maint = src.get("maintenance")
        if maint != "aucune" and not (isinstance(maint, dict) and maint.get("activer") and maint.get("desactiver")):
            e(pid, "source.maintenance : activer et desactiver requis, ou « aucune »")
        for c in src.get("configuration") or []:
            if c.get("format") not in FORMATS:
                e(pid, f"configuration {c.get('fichier')} : format parmi {sorted(FORMATS)}")
            if not c.get("fichier") or not isinstance(c.get("cles"), dict):
                e(pid, "configuration : fichier et cles requis")
            if c.get("format") == "autre":
                e(pid, f"configuration {c.get('fichier')} : format « autre » non réécrit par le script")
        noms = {db.get("name") for db in chemin(o, "spec.databases") or []}
        for b in p.get("bases") or []:
            if b.get("moteur") not in MOTEURS:
                e(pid, f"bases : moteur parmi {sorted(MOTEURS)}")
            if not b.get("source") or not b.get("cible"):
                e(pid, "bases : source et cible requis")
            elif b["cible"] not in noms:
                e(pid, f"bases : {b['cible']} n'est pas déclarée dans ogs.spec.databases")
        a = p.get("amqp") or {}
        if a.get("present") and (not a.get("vhost_source") or not a.get("transports")):
            e(pid, "amqp : vhost_source et transports requis")
        for w in chemin(p, "arret_a_la_bascule.workers") or []:
            if not isinstance(w, dict) or not w.get("arreter") or not w.get("relancer"):
                e(pid, "arret_a_la_bascule.workers : arreter et relancer requis")
        for v in p.get("verifications") or []:
            if not v.get("url") and not v.get("commande"):
                e(pid, "verifications : url ou commande requis")
    for pid in ids:
        if pid not in vus:
            e(pid, "absent du manifeste")
    for x in erreurs:
        print(x)
    sys.exit(3 if erreurs else 0)

def c_extraire(m, pid, sortie, desactiver):
    _, d = charger(m)
    o = copy.deepcopy(trouver(d, pid)["ogs"])
    if desactiver == "1":
        for cle in ("crons", "services"):
            for el in chemin(o, f"spec.{cle}") or []:
                el["enabled"] = False
    ecrire_yaml(sortie, sans_nuls(o))

def c_activer(m, pid, fichier):
    """Rend aux crons et services l'état voulu par le manifeste."""
    _, d = charger(m)
    voulu = trouver(d, pid)["ogs"]
    with open(fichier, encoding="utf-8") as f:
        projet = yaml.safe_load(f)
    for cle in ("crons", "services"):
        attendus = {el.get("name"): el.get("enabled") for el in chemin(voulu, f"spec.{cle}") or []}
        for el in chemin(projet, f"spec.{cle}") or []:
            if el.get("name") in attendus:
                if attendus[el["name"]] is None:
                    el.pop("enabled", None)
                else:
                    el["enabled"] = attendus[el["name"]]
    ecrire_yaml(fichier, projet)

def c_desactiver(fichier):
    with open(fichier, encoding="utf-8") as f:
        projet = yaml.safe_load(f)
    for cle in ("crons", "services"):
        for el in chemin(projet, f"spec.{cle}") or []:
            el["enabled"] = False
    ecrire_yaml(fichier, projet)

def c_actifs(fichier):
    """Nombre de crons et services actifs dans un fichier de projet."""
    with open(fichier, encoding="utf-8") as f:
        projet = yaml.safe_load(f)
    n = 0
    for cle in ("crons", "services"):
        n += sum(1 for el in chemin(projet, f"spec.{cle}") or [] if el.get("enabled") is not False)
    print(n)

def c_domaines(m, pid):
    _, d = charger(m)
    o = trouver(d, pid)["ogs"]
    noms = [x["name"] for x in chemin(o, "spec.domains") or [] if x.get("name")]
    print("\n".join(noms or [chemin(o, "metadata.host")]))

def c_export(fichier, path):
    with open(fichier, encoding="utf-8") as f:
        print(fmt(chemin(yaml.safe_load(f), path)))

def c_resoudre(valeur, env_json):
    if not valeur.startswith("@ogs:"):
        print(valeur, end="")
        return
    ref = valeur[5:]
    var, _, partie = ref.partition("#")
    with open(env_json, encoding="utf-8") as f:
        entrees = {x["name"]: x["value"] for x in (json.load(f).get("items") or [])}
    if var not in entrees:
        sys.exit(f"{var} absent de « ogs secrets env » (disponibles : {', '.join(sorted(entrees)) or 'aucune'})")
    v = entrees[var]
    if partie:
        u = urllib.parse.urlsplit(v)
        parties = {
            "scheme": u.scheme, "user": urllib.parse.unquote(u.username or ""),
            "password": urllib.parse.unquote(u.password or ""), "host": u.hostname or "",
            "port": str(u.port or ""), "name": urllib.parse.unquote(u.path.lstrip("/")),
        }
        if partie not in parties:
            sys.exit(f"partie inconnue : {partie} (parmi {', '.join(parties)})")
        v = parties[partie]
    print(v, end="")

def php_chaine(v):
    return "'" + v.replace("\\", "\\\\").replace("'", "\\'") + "'"

def c_config(fichier, format_, cle, valeur):
    with open(fichier, encoding="utf-8") as f:
        texte = f.read()
    k = re.escape(cle)
    if format_ == "dotenv":
        if "'" not in valeur:
            rendu = "'" + valeur + "'"
        else:
            rendu = '"' + re.sub(r'([\\"$])', r"\\\1", valeur) + '"'
        motif = rf"^(\s*(?:export\s+)?{k}=).*$"
        nouveau = lambda mo: mo.group(1) + rendu
    elif format_ == "parameters-yml":
        motif = rf"^(\s+{k}:[ \t]*)(?:\"[^\"]*\"|'[^']*'|[^#\n]*?)([ \t]+#.*)?$"
        nouveau = lambda mo: mo.group(1) + json.dumps(valeur, ensure_ascii=False) + (mo.group(2) or "")
    elif format_ == "prestashop-php":
        motif = rf"""^(\s*['"]{k}['"]\s*=>\s*).*?(,?)\s*$"""
        nouveau = lambda mo: mo.group(1) + php_chaine(valeur) + (mo.group(2) or ",")
    elif format_ == "wp-config":
        motif = rf"""^(\s*define\s*\(\s*['"]{k}['"]\s*,\s*).*?(\)\s*;.*)$"""
        nouveau = lambda mo: mo.group(1) + php_chaine(valeur) + " " + mo.group(2)
    else:
        sys.exit(f"format non réécrit : {format_}")
    texte2, n = re.subn(motif, nouveau, texte, flags=re.M)
    if n == 0:
        if format_ == "dotenv":
            texte2 = texte + ("" if texte.endswith("\n") or not texte else "\n") + f"{cle}={rendu}\n"
        else:
            sys.exit(f"{fichier} : clé {cle} introuvable")
    if texte2 != texte:
        # Réécriture en place : garde propriétaire, groupe et mode du fichier.
        with open(fichier, "w", encoding="utf-8") as f:
            f.write(texte2)

def c_cles(m, pid, fichier):
    _, d = charger(m)
    for c in chemin(trouver(d, pid), "source.configuration") or []:
        if c.get("fichier") == fichier:
            for k, v in (c.get("cles") or {}).items():
                print(f"{k}{SEP}{fmt(v)}")

def c_existe(m, pid, path):
    _, d = charger(m)
    parent, _, cle = path.rpartition(".")
    o = chemin(trouver(d, pid), parent)
    sys.exit(0 if isinstance(o, dict) and cle in o else 1)

COMMANDES = {
    "cles": c_cles, "existe": c_existe,
    "get": c_get, "lignes": c_lignes, "ids": c_ids, "controle": c_controle,
    "extraire": c_extraire, "activer": c_activer, "desactiver": c_desactiver,
    "actifs": c_actifs, "domaines": c_domaines, "export": c_export,
    "resoudre": c_resoudre, "config": c_config,
}
COMMANDES[sys.argv[1]](*sys.argv[2:])
PYEOF

py() { python3 "$PY" "$@"; }
# m <id|-> <chemin> : une valeur du manifeste (vide si absente).
m() { py get "$MANIFESTE" "$@"; }
# ml <id|-> <chemin> [champs...] : les éléments d'une liste, une ligne chacun.
ml() { py lignes "$MANIFESTE" "$@"; }

temporaire() {
    local f
    f="$(mktemp "${TMPDIR:-/tmp}/migrer.XXXXXX")"
    TEMPORAIRES+=("$f")
    printf '%s' "$f"
}

# -----------------------------------------------------------------------------
# Exécution : lecture toujours, modification seulement avec --executer
# -----------------------------------------------------------------------------
cmd_affiche() { printf '%s   $ %s%s\n' "$C_CMD" "$*" "$C_RAZ"; }

# faire <commande> [args...] : une commande qui modifie quelque chose.
faire() {
    local ligne
    ligne="$(printf '%q ' "$@")"
    cmd_affiche "${ligne% }"
    ((EXECUTER)) || return 0
    "$@"
}

# faire_sh <script> : un tube ou une redirection, affiché tel quel.
faire_sh() {
    cmd_affiche "$1"
    ((EXECUTER)) || return 0
    bash -o pipefail -c "$1"
}

# ogs_faire <args...> : une commande ogs qui modifie ; le code 6 (succès partiel,
# typiquement un certificat pas encore émissible) n'arrête pas la migration.
ogs_faire() {
    local rc=0
    faire "${OGS[@]}" "$@" || rc=$?
    case $rc in
        0) ;;
        6) avert "ogs $1 : succès partiel (code 6) — voir les avertissements ci-dessus" ;;
        *) fatal "ogs $* a échoué (code $rc)" ;;
    esac
}

ogs_lire() { "${OGS[@]}" "$@" 2>/dev/null; }

confirmer() {
    ((OUI)) && return 0
    ((EXECUTER)) || return 0
    local reponse
    printf '%s   ? %s [tapez « %s » pour confirmer] %s' "$C_AVERT" "$1" "$2" "$C_RAZ"
    read -r reponse
    [[ "$reponse" == "$2" ]] || fatal "abandon"
}

# -----------------------------------------------------------------------------
# La source, par ssh
# -----------------------------------------------------------------------------
SRC_SSH="$(m - source.ssh)"
SRC_PORT="$(m - source.port_ssh)"; SRC_PORT="${SRC_PORT:-22}"
SSH_OPTS=(-p "$SRC_PORT" -o BatchMode=yes -o ConnectTimeout=15
          -o ControlMaster=auto -o "ControlPath=${TMPDIR:-/tmp}/migrer-ssh-%C" -o ControlPersist=15m)
RSYNC_SSH="ssh $(printf '%q ' "${SSH_OPTS[@]}")"
CIBLE_IP="$(m - cible.ip_publique)"
CIBLE_IP6="$(m - cible.ip6_publique)"
TRAVAIL="$(m - migration.travail)"; TRAVAIL="${TRAVAIL:-/var/lib/ogs-migration}"

# src_lire <script> : lecture sur la source, toujours exécutée.
src_lire() { ssh "${SSH_OPTS[@]}" "$SRC_SSH" "$1"; }
# src_faire <script> : modification sur la source.
src_faire() {
    cmd_affiche "[source] $1"
    ((EXECUTER)) || return 0
    ssh "${SSH_OPTS[@]}" "$SRC_SSH" "$1"
}

# -----------------------------------------------------------------------------
# Ce que ogs sait d'un projet : compte, home — ou ce qu'il en déduirait
# -----------------------------------------------------------------------------
declare -A P_USER P_GROUP P_ROOT P_HOST P_KIND
charger_projet() {
    local id="$1" export_f
    P_HOST[$id]="$(m "$id" ogs.metadata.host)"
    P_KIND[$id]="$(m "$id" ogs.spec.app.kind)"
    export_f="$(temporaire)"
    if ogs_lire project export "$id" >"$export_f" && [[ -s "$export_f" ]]; then
        P_USER[$id]="$(py export "$export_f" spec.user.name)"
        P_GROUP[$id]="$(py export "$export_f" spec.user.group)"
        P_ROOT[$id]="$(py export "$export_f" spec.layout.root)"
    else
        # Projet pas encore déclaré : les défauts d'ogs.
        P_USER[$id]="$id"; P_GROUP[$id]="$id"; P_ROOT[$id]="/home/${P_HOST[$id]}"
    fi
}

fichier_projet() {
    local prefixe=''
    # En mode préfixé (essai), /etc/ogs est sous le préfixe.
    if [[ "${OGS[*]}" =~ --prefix[=\ ]([^ ]+) ]]; then prefixe="${BASH_REMATCH[1]}"; fi
    printf '%s/etc/ogs/projects/%s.yaml' "$prefixe" "$1"
}

# en_projet <id> <commande> : sous le compte du projet, depuis www, son PHP en tête.
en_projet() {
    local id="$1" root="${P_ROOT[$1]}"
    printf 'cd %q && runuser -u %q -- env HOME=%q PATH=%q bash -c %q' \
        "$root/www" "${P_USER[$id]}" "$root" "$root/.local/bin:/usr/local/bin:/usr/bin:/bin" "$2"
}

# -----------------------------------------------------------------------------
# État de la migration, par projet
# -----------------------------------------------------------------------------
noter() {
    ((EXECUTER)) || return 0
    mkdir -p "$TRAVAIL/$1"
    printf '%s %s\n' "$2" "$(date -Is)" >>"$TRAVAIL/$1/etat"
}
dernier() {
    [[ -r "$TRAVAIL/$1/etat" ]] || return 0
    awk -v c="$2" '$1 == c { d = $2 } END { print d }' "$TRAVAIL/$1/etat"
}

# -----------------------------------------------------------------------------
# Contrôle du manifeste
# -----------------------------------------------------------------------------
controler() {
    local sortie rc=0
    sortie="$(py controle "$MANIFESTE" "$@")" || rc=$?
    if ((rc)); then
        while IFS= read -r ligne; do erreur "$ligne"; done <<<"$sortie"
        erreur "manifeste invalide : corriger avant de continuer"
        exit 3
    fi
}

# =============================================================================
# PHASE controle
# =============================================================================
phase_controle() {
    titre "Contrôle du manifeste ($MANIFESTE)"
    controler "$@"
    local id
    for id in "$@"; do
        ok "$id : vague $(m "$id" vague), $(m "$id" ogs.metadata.host)"
        local chemins
        chemins="$(ml "$id" source.chemins_en_dur ou valeur)"
        [[ -z "$chemins" ]] || while IFS=$'\x1f' read -r ou valeur; do
            avert "$id : chemin en dur à corriger à la main — $ou ($valeur)"
        done <<<"$chemins"
    done
}

# =============================================================================
# PHASE prealables
# =============================================================================
ns_autoritaire() {
    local nom="$1" ns
    while [[ "$nom" == *.* ]]; do
        ns="$(dig +short NS "$nom" | head -1)"
        [[ -n "$ns" ]] && { printf '%s' "$ns"; return; }
        nom="${nom#*.}"
    done
}

phase_prealables() {
    titre "Préalables"
    local manque=0 outil
    etape "Outils de la cible"
    ((EUID == 0)) || { if ((EXECUTER)); then fatal "à lancer en root"; else avert "pas root : simulation seulement"; fi; }
    for outil in "${OGS[0]}" rsync curl dig gzip python3 ssh runuser; do
        if command -v "$outil" >/dev/null; then ok "$outil"; else erreur "$outil absent"; manque=1; fi
    done
    ((manque == 0)) || fatal "outils manquants (dig : apt install bind9-dnsutils)"

    etape "ogs doctor"
    local rc=0
    "${OGS[@]}" doctor || rc=$?
    case $rc in 0) ok "doctor : conforme" ;; 6) avert "doctor : avertissements (code 6)" ;; *) fatal "doctor : code $rc" ;; esac

    etape "Pare-feu et identifiants DNS"
    if [[ -e /etc/ogs/ufw.yaml ]]; then ok "/etc/ogs/ufw.yaml présent"
    else avert "/etc/ogs/ufw.yaml absent : ogs ne touche pas au pare-feu (voir parc.pare_feu)"; fi
    local id
    for id in "$@"; do
        if ml "$id" ogs.spec.certificates challenge | grep -qx dns-01; then
            if [[ -e /etc/ogs/credentials/dns/ovh.yaml ]]; then ok "$id : identifiants OVH présents (dns-01)"
            else erreur "$id : certificat dns-01 sans identifiants OVH — ogs cert dns-provider set ovh"; manque=1; fi
        fi
    done

    etape "Accès à la source ($SRC_SSH)"
    src_lire true || fatal "ssh vers la source impossible : autoriser la clé de root de la cible"
    ok "ssh"
    src_lire 'command -v rsync >/dev/null' && ok "rsync sur la source" || { erreur "rsync absent de la source"; manque=1; }
    for id in "$@"; do
        local racine code
        racine="$(m "$id" source.racine)"; code="$(m "$id" source.code)"
        if src_lire "test -d $(printf '%q' "$racine/$code")"; then
            ok "$id : $racine/$code ($(src_lire "du -sh $(printf '%q' "$racine/$code") 2>/dev/null | cut -f1"))"
        else
            erreur "$id : $racine/$code introuvable sur la source"; manque=1
        fi
        while IFS=$'\x1f' read -r moteur base; do
            [[ -n "$moteur" ]] || continue
            case "$moteur" in
                mysql|mariadb) src_lire "mysql -N -e 'SELECT 1' $(printf '%q' "$base") >/dev/null" \
                    && ok "$id : base $base lisible" || { erreur "$id : base $base illisible sur la source"; manque=1; } ;;
                postgres) src_lire "runuser -u postgres -- psql -tAc 'SELECT 1' $(printf '%q' "$base") >/dev/null" \
                    && ok "$id : base $base lisible" || { erreur "$id : base $base illisible sur la source"; manque=1; } ;;
            esac
        done < <(ml "$id" bases moteur source)
    done

    etape "TTL des enregistrements (attendu ≤ $(m - migration.ttl_bascule) s)"
    local ttl_max dom ns ttl
    ttl_max="$(m - migration.ttl_bascule)"; ttl_max="${ttl_max:-300}"
    for id in "$@"; do
        while IFS= read -r dom; do
            ns="$(ns_autoritaire "$dom")"
            ttl="$(dig +noall +answer A "$dom" ${ns:+@"$ns"} | awk '{print $2; exit}')"
            if [[ -z "$ttl" ]]; then avert "$dom : aucun enregistrement A"
            elif ((ttl > ttl_max)); then avert "$dom : TTL $ttl s — l'abaisser 48 h avant la bascule"
            else ok "$dom : TTL $ttl s"; fi
        done < <(py domaines "$MANIFESTE" "$id")
    done
    ((manque == 0)) || fatal "préalables non remplis"
}

# =============================================================================
# PHASE declarer
# =============================================================================
declarer_projet() {
    local id="$1" fichier brouillon desactiver=1
    titre "Déclaration de $id"
    fichier="$(fichier_projet "$id")"
    if bascule_active "$id"; then desactiver=0; fi

    brouillon="$(temporaire)"
    py extraire "$MANIFESTE" "$id" "$brouillon" "$desactiver"
    etape "Validation (ogs plan -f)"
    "${OGS[@]}" plan -f "$brouillon" >/dev/null || fatal "$id : déclaration refusée par ogs plan -f"
    ok "déclaration valide"

    if [[ -e "$fichier" ]]; then
        # Le fichier a pu être complété par ogs (amqp messenger) : ne pas l'écraser.
        info "$fichier existe déjà : laissé tel quel (le supprimer pour le redéposer)"
    else
        etape "Dépôt"
        faire install -m 0644 "$brouillon" "$fichier"
        if [[ "$(m "$id" amqp.present)" == true ]]; then
            local transports=() t
            while IFS= read -r t; do if [[ -n "$t" ]]; then transports+=(--transport "$t"); fi; done < <(ml "$id" amqp.transports)
            ogs_faire amqp messenger "$id" "${transports[@]}"
        fi
    fi
    ((desactiver)) && info "crons et services désactivés jusqu'à la bascule"

    etape "Application"
    ogs_faire apply "$id" --yes
    noter "$id" declare

    if [[ "$(m "$id" ogs.spec.mail.dkim.enabled)" == true ]]; then
        etape "DKIM : enregistrement à publier AVANT la bascule"
        ((EXECUTER)) && { ogs_lire mail dkim records "$id" || avert "ogs mail dkim records $id a échoué"; }
        ((EXECUTER)) || cmd_affiche "ogs mail dkim records $id"
    fi
}

phase_declarer() {
    controler "$@"
    local id
    for id in "$@"; do declarer_projet "$id"; done
}

# =============================================================================
# PHASE synchroniser — fichiers, bases, configuration
# =============================================================================
synchroniser_fichiers() {
    local id="$1" root racine code excl=() motif de vers
    root="${P_ROOT[$id]}"
    racine="$(m "$id" source.racine)"; code="$(m "$id" source.code)"
    etape "Fichiers : $racine/$code → $root/www"
    while IFS= read -r motif; do if [[ -n "$motif" ]]; then excl+=(--exclude="$motif"); fi; done < <(ml "$id" source.exclure)
    # Les destinations sous www/ des données hors code sont protégées du --delete
    # du code — et un lien symbolique de même nom (release Deployer) n'est pas copié.
    while IFS=$'\x1f' read -r de vers; do
        if [[ "$vers" == www/* ]]; then excl+=(--exclude="/${vers#www/}"); fi
    done < <(ml "$id" source.donnees_hors_code de vers)

    local base=(rsync -rlptH --delete --chmod=o-rwx --chown="${P_USER[$id]}:${P_GROUP[$id]}" -e "$RSYNC_SSH")
    faire "${base[@]}" "${excl[@]}" "$SRC_SSH:$racine/$code/" "$root/www/"

    while IFS=$'\x1f' read -r de vers; do
        [[ -n "$de" ]] || continue
        if src_lire "test -d $(printf '%q' "$racine/$de")"; then
            faire install -d -o "${P_USER[$id]}" -g "${P_GROUP[$id]}" -m 0750 "$root/$vers"
            faire "${base[@]}" "$SRC_SSH:$racine/$de/" "$root/$vers/"
        else
            faire "${base[@]}" "$SRC_SSH:$racine/$de" "$root/$vers"
        fi
    done < <(ml "$id" source.donnees_hors_code de vers)
}

MYSQLDUMP_OPTS=''
options_mysqldump() {
    [[ -n "$MYSQLDUMP_OPTS" ]] && return
    MYSQLDUMP_OPTS='--single-transaction --quick --routines --triggers --hex-blob --default-character-set=utf8mb4 --no-tablespaces'
    # Options propres au client MySQL (absentes de celui de MariaDB).
    if src_lire 'mysqldump --help 2>/dev/null | grep -q -- --set-gtid-purged'; then
        MYSQLDUMP_OPTS+=' --set-gtid-purged=OFF'
    fi
}

synchroniser_bases() {
    local id="$1" dir
    dir="$TRAVAIL/$id"
    ((EXECUTER)) && install -d -m 0700 "$dir"
    local cible_mariadb=0
    mysql --version 2>/dev/null | grep -qi mariadb && cible_mariadb=1

    while IFS=$'\x1f' read -r moteur src cible charset collation definers evenements exclues; do
        [[ -n "$moteur" ]] || continue
        local dump="$dir/$cible.sql.gz" user
        user="$(ml "$id" ogs.spec.databases name user | awk -F$'\x1f' -v n="$cible" '$1 == n { print $2 }')"
        user="${user:-$cible}"
        etape "Base $src → $cible ($moteur)"
        case "$moteur" in
            mysql|mariadb)
                options_mysqldump
                local opts="$MYSQLDUMP_OPTS" t
                [[ "$evenements" == true ]] && { opts+=' --events'; avert "EVENT copiés : ils tourneront AUSSI sur la cible dès maintenant"; }
                IFS=',' read -ra tables <<<"$exclues"
                for t in "${tables[@]}"; do if [[ -n "$t" ]]; then opts+=" --ignore-table=$src.$t"; fi; done
                faire_sh "$(printf 'ssh %s %q %q | gzip -1 > %q' "$(printf '%q ' "${SSH_OPTS[@]}")" "$SRC_SSH" \
                    "mysqldump $opts $(printf '%q' "$src")" "$dump")"
                # DEFINER réécrits vers l'utilisateur ogs ; collations MySQL 8 ramenées
                # à leur équivalent MariaDB si la cible est MariaDB.
                local filtre="sed -E 's/DEFINER=\`[^\`]*\`@\`[^\`]*\`/DEFINER=\`$user\`@\`localhost\`/g'"
                ((cible_mariadb)) && filtre+=" | sed -E 's/utf8mb4_0900_[a-z_]+/utf8mb4_unicode_ci/g'"
                local creation="CREATE DATABASE \`$cible\` CHARACTER SET ${charset:-utf8mb4}"
                [[ -n "$collation" && ! ( $cible_mariadb == 1 && "$collation" == utf8mb4_0900_* ) ]] && creation+=" COLLATE $collation"
                faire mysql -e "DROP DATABASE IF EXISTS \`$cible\`; $creation"
                faire_sh "zcat $(printf '%q' "$dump") | $filtre | mysql --default-character-set=utf8mb4 $(printf '%q' "$cible")"
                ;;
            postgres)
                local excl='' t
                IFS=',' read -ra tables <<<"$exclues"
                for t in "${tables[@]}"; do if [[ -n "$t" ]]; then excl+=" --exclude-table-data=$t"; fi; done
                faire_sh "$(printf 'ssh %s %q %q | gzip -1 > %q' "$(printf '%q ' "${SSH_OPTS[@]}")" "$SRC_SSH" \
                    "runuser -u postgres -- pg_dump --no-owner --no-privileges --clean --if-exists$excl $(printf '%q' "$src")" "$dump")"
                # Chargé sous le rôle du projet (propriétaire des objets) ; les
                # extensions sont l'affaire d'ogs (spec.databases[].extensions).
                faire_sh "{ printf 'SET ROLE %s;\n' $(printf '%q' "\"$user\""); zcat $(printf '%q' "$dump") | grep -vE '^(CREATE|DROP|COMMENT ON) EXTENSION'; } | runuser -u postgres -- psql -q -v ON_ERROR_STOP=1 -d $(printf '%q' "$cible")"
                ;;
        esac
        if [[ "$definers" == true ]]; then info "DEFINER réécrits vers $user@localhost"; fi
    done < <(ml "$id" bases moteur source cible charset collation definers evenements tables_exclues)
}

reecrire_configuration() {
    local id="$1" env_json fichier format cle ref valeur
    [[ -n "$(ml "$id" source.configuration fichier)" ]] || return 0
    etape "Configuration applicative"
    env_json="$(temporaire)"
    if ! ogs_lire secrets env "$id" -o json >"$env_json"; then
        ((EXECUTER)) && fatal "ogs secrets env $id a échoué : bases et transports sont-ils créés ?"
        info "(simulation : chaînes de connexion indisponibles, projet non appliqué)"
        return 0
    fi
    while IFS=$'\x1f' read -r fichier format; do
        [[ -n "$fichier" ]] || continue
        local chemin_f="${P_ROOT[$id]}/$fichier" n=0
        while IFS=$'\x1f' read -r cle ref; do
            valeur="$(py resoudre "$ref" "$env_json")" || fatal "$id : $fichier, $cle : valeur irrésoluble"
            cmd_affiche "réécrire $cle dans $chemin_f ($format)"
            if ((EXECUTER)); then
                [[ -f "$chemin_f" ]] || fatal "$chemin_f absent après la copie"
                py config "$chemin_f" "$format" "$cle" "$valeur"
            fi
            n=$((n + 1))
        done < <(py cles "$MANIFESTE" "$id" "$fichier")
        ok "$fichier : $n clé(s)"
    done < <(ml "$id" source.configuration fichier format)
}

apres_synchro() {
    local id="$1" cmds=() c
    if py existe "$MANIFESTE" "$id" apres_synchro; then
        while IFS= read -r c; do if [[ -n "$c" ]]; then cmds+=("$c"); fi; done < <(ml "$id" apres_synchro)
    else
        case "${P_KIND[$id]}" in
            php-symfony)    cmds=("php bin/console cache:clear") ;;
            php-prestashop) cmds=("find var/cache -mindepth 1 -maxdepth 1 -exec rm -rf {} +") ;;
        esac
    fi
    ((${#cmds[@]})) || return 0
    etape "Après synchronisation"
    for c in "${cmds[@]}"; do
        faire_sh "$(en_projet "$id" "$c")" || fatal "$id : « $c » a échoué"
    done
}

synchroniser_projet() {
    local id="$1"
    charger_projet "$id"
    [[ -n "$(dernier "$id" declare)" ]] || ((EXECUTER == 0)) || fatal "$id : non déclaré — phase declarer d'abord"
    titre "Synchronisation de $id"
    synchroniser_fichiers "$id"
    synchroniser_bases "$id"
    reecrire_configuration "$id"
    apres_synchro "$id"
    etape "Modes et propriétaires (ogs apply)"
    ogs_faire apply "$id" --yes
    noter "$id" synchro
}

phase_synchroniser() {
    controler "$@"
    local id
    local rc=0
    for id in "$@"; do
        synchroniser_projet "$id"
        verifier_projet "$id" force || { avert "$id : vérifications en échec — à corriger avant la bascule"; rc=1; }
    done
    return $rc
}

# =============================================================================
# PHASE verifier
# =============================================================================
# verifier_projet <id> force|dns : rend 1 si une vérification bloquante échoue.
verifier_projet() {
    local id="$1" mode="$2" echecs=0 url statut contient commande attendu
    charger_projet "$id"
    etape "Vérifications de $id ($([[ $mode == dns ]] && echo 'par le DNS public' || echo "forcées vers $CIBLE_IP"))"
    while IFS=$'\x1f' read -r url statut contient commande attendu; do
        if [[ -n "$url" ]]; then
            local hote corps code essai="$url" resolve=() degrade=0
            hote="${url#*://}"; hote="${hote%%/*}"; hote="${hote%%:*}"
            corps="$(temporaire)"
            [[ $mode == force ]] && resolve=(--resolve "$hote:443:$CIBLE_IP" --resolve "$hote:80:$CIBLE_IP" -k)
            code="$(curl -sS -o "$corps" -w '%{http_code}' --max-time 30 "${resolve[@]}" "$essai" 2>/dev/null || true)"
            if [[ $mode == force && "$code" == 000 && "$url" == https://* ]]; then
                # Pas encore de certificat sur la cible : le vhost ne sert que HTTP.
                essai="http://${url#https://}"; degrade=1
                code="$(curl -sS -o "$corps" -w '%{http_code}' --max-time 30 "${resolve[@]}" "$essai" 2>/dev/null || true)"
            fi
            if [[ -n "$statut" && "$code" != "$statut" ]]; then
                if ((degrade)); then avert "$essai : $code (attendu $statut en HTTPS ; pas de certificat encore)"
                else erreur "$essai : $code, attendu $statut"; echecs=$((echecs + 1)); fi
            elif [[ -n "$contient" ]] && ! grep -qF -- "$contient" "$corps"; then
                erreur "$essai : « $contient » absent de la page"; echecs=$((echecs + 1))
            else
                ok "$essai : $code"
            fi
        elif [[ -n "$commande" ]]; then
            local sortie
            if ((EXECUTER)) || [[ -d "${P_ROOT[$id]}/www" ]]; then
                sortie="$(bash -c "$(en_projet "$id" "$commande")" 2>&1 || true)"
                if [[ -z "$attendu" ]] || grep -qF -- "$attendu" <<<"$sortie"; then ok "$commande"
                else erreur "$commande : « $attendu » absent de la sortie"; echecs=$((echecs + 1)); fi
            else
                cmd_affiche "$(en_projet "$id" "$commande")"
            fi
        fi
    done < <(ml "$id" verifications url statut contient commande attendu)
    ((echecs == 0))
}

phase_verifier() {
    controler "$@"
    local id rc=0
    for id in "$@"; do verifier_projet "$id" "$([[ $PAR_DNS == 1 ]] && echo dns || echo force)" || rc=1; done
    return $rc
}

# =============================================================================
# PHASE basculer
# =============================================================================
source_maintenance() {
    local id="$1" sens="$2" cmd
    [[ "$(m "$id" source.maintenance)" == aucune ]] && return 0
    cmd="$(m "$id" "source.maintenance.$sens")"
    src_faire "$cmd"
}

attendre_amqp() {
    local id="$1" vhost fin total
    [[ "$(m "$id" amqp.present)" == true ]] || return 0
    vhost="$(m "$id" amqp.vhost_source)"
    etape "Vidage des files AMQP de la source (vhost $vhost)"
    ((EXECUTER)) || { cmd_affiche "[source] rabbitmqctl list_queues -p $vhost messages — jusqu'à 0"; return 0; }
    fin=$((SECONDS + DELAI_AMQP))
    while :; do
        total="$(src_lire "rabbitmqctl -q list_queues -p $(printf '%q' "$vhost") messages" | awk '$1 ~ /^[0-9]+$/ { s += $1 } END { print s + 0 }')"
        ((total == 0)) && { ok "files vides"; return 0; }
        ((SECONDS < fin)) || fatal "$total message(s) encore en file après ${DELAI_AMQP} s"
        info "$total message(s) en attente…"; sleep 15
    done
}

arreter_source() {
    local id="$1" user sauvegarde f w
    user="$(m "$id" source.utilisateur)"
    sauvegarde="/root/migration-ogs/$id"
    etape "Arrêt des tâches et workers de la source"
    src_faire "install -d -m 0700 $(printf '%q' "$sauvegarde")"
    if [[ "$(m "$id" arret_a_la_bascule.crontab_utilisateur)" == true ]]; then
        src_faire "crontab -l -u $(printf '%q' "$user") > $(printf '%q' "$sauvegarde/crontab") && crontab -r -u $(printf '%q' "$user")"
    fi
    # cron ignore les fichiers de /etc/cron.d dont le nom contient un point.
    while IFS= read -r f; do
        if [[ -n "$f" ]]; then src_faire "test ! -e $(printf '%q' "$f") || mv $(printf '%q' "$f") $(printf '%q' "$f.migre-ogs")"; fi
    done < <(ml "$id" arret_a_la_bascule.fichiers_cron)
    while IFS=$'\x1f' read -r w _; do
        if [[ -n "$w" ]]; then src_faire "$w"; fi
    done < <(ml "$id" arret_a_la_bascule.workers arreter relancer)
}

relancer_source() {
    local id="$1" user sauvegarde f w
    user="$(m "$id" source.utilisateur)"
    sauvegarde="/root/migration-ogs/$id"
    etape "Relance des tâches et workers de la source"
    if [[ "$(m "$id" arret_a_la_bascule.crontab_utilisateur)" == true ]]; then
        src_faire "test ! -s $(printf '%q' "$sauvegarde/crontab") || crontab -u $(printf '%q' "$user") $(printf '%q' "$sauvegarde/crontab")"
    fi
    while IFS= read -r f; do
        if [[ -n "$f" ]]; then src_faire "test ! -e $(printf '%q' "$f.migre-ogs") || mv $(printf '%q' "$f.migre-ogs") $(printf '%q' "$f")"; fi
    done < <(ml "$id" arret_a_la_bascule.fichiers_cron)
    while IFS=$'\x1f' read -r _ w; do
        if [[ -n "$w" ]]; then src_faire "$w"; fi
    done < <(ml "$id" arret_a_la_bascule.workers arreter relancer)
}

resolu_vers_cible() {
    local dom="$1" r a aaaa
    for r in "${RESOLVEURS[@]}"; do
        a="$(dig +short A "$dom" @"$r" | grep -E '^[0-9.]+$' | sort -u | tr '\n' ' ')"
        [[ "$a" == "$CIBLE_IP " ]] || return 1
        aaaa="$(dig +short AAAA "$dom" @"$r" | grep ':' | sort -u | tr '\n' ' ')"
        if [[ -n "$CIBLE_IP6" ]]; then [[ -z "$aaaa" || "$aaaa" == "$CIBLE_IP6 " ]] || return 1
        else [[ -z "$aaaa" ]] || return 1; fi
    done
}

attendre_dns() {
    local id="$1" dom fin restants
    etape "DNS : faire pointer vers la cible"
    while IFS= read -r dom; do
        info "$dom  A $CIBLE_IP${CIBLE_IP6:+  AAAA $CIBLE_IP6}${CIBLE_IP6:-  (supprimer tout AAAA)}"
    done < <(py domaines "$MANIFESTE" "$id")
    ((EXECUTER)) || return 0
    fin=$((SECONDS + DELAI_DNS))
    while :; do
        restants=()
        while IFS= read -r dom; do resolu_vers_cible "$dom" || restants+=("$dom"); done < <(py domaines "$MANIFESTE" "$id")
        ((${#restants[@]} == 0)) && { ok "propagation constatée sur ${RESOLVEURS[*]}"; return 0; }
        ((SECONDS < fin)) || fatal "DNS pas encore propagé après ${DELAI_DNS} s : ${restants[*]}"
        info "en attente : ${restants[*]}"; sleep 20
    done
}

# bascule_active <id> : basculé, et pas revenu depuis.
bascule_active() {
    local b r
    b="$(dernier "$1" bascule)"; r="$(dernier "$1" retour)"
    [[ -n "$b" && "$b" > "$r" ]]
}

basculer_projet() {
    local id="$1" fichier
    charger_projet "$id"
    titre "Bascule de $id — fenêtre : $(m "$id" fenetre)"
    [[ -n "$(dernier "$id" declare)" ]] || ((EXECUTER == 0)) || fatal "$id : non déclaré"
    [[ -n "$(dernier "$id" synchro)" ]] || avert "$id : jamais pré-synchronisé — la fenêtre sera longue"
    if bascule_active "$id"; then
        info "$id : déjà basculé le $(dernier "$id" bascule)"; return 0
    fi
    confirmer "basculer $id (maintenance sur la source) ?" "$id"
    BASCULE_EN_COURS="$id"

    etape "1. Maintenance de la source"
    source_maintenance "$id" activer
    attendre_amqp "$id"
    arreter_source "$id"

    synchroniser_fichiers "$id"
    synchroniser_bases "$id"
    reecrire_configuration "$id"
    apres_synchro "$id"
    ogs_faire apply "$id" --yes

    if ! verifier_projet "$id" force; then
        if ((EXECUTER)); then
            erreur "vérifications en échec avant le changement de DNS : la source est rétablie"
            relancer_source "$id"; source_maintenance "$id" desactiver
            BASCULE_EN_COURS=''
            fatal "$id : bascule abandonnée, rien n'a changé pour les visiteurs"
        fi
    fi

    attendre_dns "$id"

    etape "Activation des crons et services, certificat"
    fichier="$(fichier_projet "$id")"
    cmd_affiche "crons et services de $fichier rendus à l'état du manifeste"
    ((EXECUTER)) && py activer "$MANIFESTE" "$id" "$fichier"
    ogs_faire apply "$id" --yes
    noter "$id" bascule
    BASCULE_EN_COURS=''

    verifier_projet "$id" dns || avert "$id : vérifications par le DNS en échec — résolveurs pas tous à jour ?"
    ((EXECUTER)) || { info "(simulation : bascule non effectuée)"; return 0; }
    local rc=0
    "${OGS[@]}" diff "$id" >/dev/null || rc=$?
    if ((rc == 0)); then ok "$id : ogs diff conforme"; else avert "$id : ogs diff signale un écart (code $rc)"; fi
    ok "$id basculé. Source conservée $(m - migration.conservation_source_jours) jours, en maintenance."
}

phase_basculer() {
    controler "$@"
    local id
    for id in "$@"; do basculer_projet "$id"; done
}

# =============================================================================
# PHASE retour
# =============================================================================
retour_projet() {
    local id="$1" fichier
    charger_projet "$id"
    titre "Retour arrière de $id"
    confirmer "remettre $id sur l'ancien serveur ?" "$id"
    fichier="$(fichier_projet "$id")"
    etape "Cible : crons et services désactivés"
    cmd_affiche "enabled: false sur les crons et services de $fichier"
    ((EXECUTER)) && [[ -e "$fichier" ]] && py desactiver "$fichier"
    ogs_faire apply "$id" --yes
    etape "DNS : faire pointer de nouveau vers la source ($(m - source.ip_publique))"
    py domaines "$MANIFESTE" "$id" | while IFS= read -r dom; do info "$dom"; done
    relancer_source "$id"
    source_maintenance "$id" desactiver
    noter "$id" retour
    avert "les données écrites sur la cible depuis la bascule n'ont PAS été reportées sur la source"
}

phase_retour() {
    controler "$@"
    local id
    for id in "$@"; do retour_projet "$id"; done
}

# =============================================================================
# PHASE etat
# =============================================================================
phase_etat() {
    titre "État de la migration"
    printf '   %-28s %-6s %-26s %-26s %-26s %s\n' PROJET VAGUE DÉCLARÉ SYNCHRONISÉ BASCULÉ RETOUR
    local id
    for id in "$@"; do
        printf '   %-28s %-6s %-26s %-26s %-26s %s\n' "$id" "$(m "$id" vague)" \
            "$(dernier "$id" declare)" "$(dernier "$id" synchro)" "$(dernier "$id" bascule)" "$(dernier "$id" retour)"
    done
}

# =============================================================================
# Principal
# =============================================================================
python3 -c 'import yaml' 2>/dev/null || fatal "python3-yaml est absent : apt install python3-yaml"
mapfile -t IDS < <(py ids "$MANIFESTE" "${SELECTEURS[@]}") || exit 2
((${#IDS[@]})) || fatal "aucun projet sélectionné"

if ((EXECUTER)); then
    ((EUID == 0)) || fatal "--executer exige root"
    install -d -m 0700 "$TRAVAIL"
    JOURNAL="$TRAVAIL/journal-$(date +%Y%m%d-%H%M%S)-$PHASE.log"
    exec > >(tee -a "$JOURNAL") 2>&1
    exec 9>"$TRAVAIL/.verrou"
    flock -n 9 || fatal "une autre migration est en cours ($TRAVAIL/.verrou)"
    info "journal : $JOURNAL"
else
    case "$PHASE" in controle|etat|verifier) ;; *)
        printf '%s   SIMULATION : rien n’est modifié ; --executer pour passer à l’acte%s\n' "$C_AVERT" "$C_RAZ" ;;
    esac
fi

case "$PHASE" in
    controle)     phase_controle "${IDS[@]}" ;;
    prealables)   controler "${IDS[@]}"; phase_prealables "${IDS[@]}" ;;
    declarer)     phase_declarer "${IDS[@]}" ;;
    synchroniser) phase_synchroniser "${IDS[@]}" ;;
    verifier)     phase_verifier "${IDS[@]}" ;;
    basculer)     phase_basculer "${IDS[@]}" ;;
    retour)       phase_retour "${IDS[@]}" ;;
    etat)         phase_etat "${IDS[@]}" ;;
    *)            erreur "phase inconnue : $PHASE"; usage ;;
esac
