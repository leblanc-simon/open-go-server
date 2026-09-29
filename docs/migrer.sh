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
#   prealables     outils, ogs doctor, accès ssh à la source, TTL des zones,
#                  domaines résolus exactement vers la source (projets dont la
#                  bascule n'est pas engagée)
#   declarer       dépose la déclaration ogs (crons et services désactivés), apply
#   synchroniser   copie fichiers et bases, réécrit la configuration — répétable,
#                  sans interruption de service, jusqu'à la bascule : refusée
#                  ensuite, la cible faisant foi
#   verifier       rejoue les vérifications du manifeste contre la cible
#                  (--dns : par le DNS public au lieu de forcer l'adresse)
#   basculer       maintenance, dernière copie, DNS, activation — dans la fenêtre ;
#                  reprise sans nouvelle copie si la dernière est déjà faite
#   retour         retour arrière : cible désactivée, DNS revenu, source rétablie
#   etat           où en est chaque projet
#
# Options :
#   -m, --manifeste <fichier>   le manifeste rempli (obligatoire : le modèle du
#                               dépôt ne se remplit pas en place)
#   --executer                  exécuter pour de bon (sinon simulation)
#   --oui                       ne pas demander de confirmation
#   --dns                       (verifier) passer par le DNS public
#   --delai-dns <secondes>      attente maximale de la propagation (défaut 1800)
#   --sans-attente-dns          (basculer, retour) propagation constatée par
#                               l'exploitant, qui la confirme — même sous --oui —,
#                               au lieu d'être attendue : pour un domaine qui ne
#                               résoudra jamais exactement vers l'IP du manifeste
#                               (CDN, répartiteur, plusieurs A) — prealables le signale
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
SANS_ATTENTE_DNS=0
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
        --sans-attente-dns) SANS_ATTENTE_DNS=1; shift ;;
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
# Un répertoire par exécution, supprimé en entier à la sortie : temporaire() est
# appelée dans $(…), un sous-shell où l'ajout à une liste se perdrait — et avec
# lui la suppression de fichiers qui portent des secrets (ogs secrets env).
TMPD="$(mktemp -d "${TMPDIR:-/tmp}/migrer.XXXXXX")"
PY="$TMPD/migrer.py"
TEMPORAIRES=("$TMPD")
BASCULE_EN_COURS=''
RETOUR_EN_COURS=''
nettoyer() {
    local rc=$?
    if ((rc != 0)); then
        local relancer
        relancer="$0 -m $(printf '%q' "$MANIFESTE") --executer"
        if ((SANS_ATTENTE_DNS)); then relancer+=" --sans-attente-dns"; fi
        if [[ -n "$BASCULE_EN_COURS" ]]; then
            erreur "bascule de $BASCULE_EN_COURS interrompue : la source est peut-être en maintenance,"
            erreur "ses tâches arrêtées. Reprendre : $relancer basculer $BASCULE_EN_COURS"
            erreur "ou revenir en arrière :    $relancer retour $BASCULE_EN_COURS"
        elif [[ -n "$RETOUR_EN_COURS" ]]; then
            erreur "retour de $RETOUR_EN_COURS interrompu : la source est peut-être encore en maintenance."
            erreur "Reprendre : $relancer retour $RETOUR_EN_COURS"
        fi
    fi
    rm -rf "${TEMPORAIRES[@]}"
}
trap nettoyer EXIT

cat >"$PY" <<'PYEOF'
import copy, glob, hashlib, json, os, re, sys, urllib.parse
try:
    import yaml
except ImportError:
    sys.exit("python3-yaml est absent : apt install python3-yaml")

# Séparateur de champs : un caractère non blanc, sans quoi « read » fusionne les
# champs vides consécutifs et décale tous les suivants.
SEP = "\x1f"
FORMATS = {"dotenv", "parameters-yml", "prestashop-php", "wp-config", "autre"}
MOTEURS = {"mysql", "mariadb", "postgres"}
# Un nom de base est interpolé dans du SQL (DROP DATABASE) et dans des commandes.
NOM_BASE = re.compile(r"^[A-Za-z0-9_]+$")

def famille(moteur):
    """MySQL et MariaDB occupent le même serveur sur la cible : « mysql:x » et
    « mariadb:x » y désignent la même base."""
    return "mysql" if moteur in ("mysql", "mariadb") else moteur

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

def hote(nom):
    """Un host ou un domaine sous la forme où ogs les compare (naming.NormalizeHost) :
    minuscules, sans point final ni « www. » de tête — shop.fr et www.shop.fr sont
    un même site."""
    h = str(nom or "").strip().lower()
    h = h[:-1] if h.endswith(".") else h
    return h[4:] if h.startswith("www.") else h

# Le domaine de préproduction d'ogs à défaut de configuration (config.Default).
DOMAINE_PREP = "preprod-pp.net"

def conf_ogs(prefixe):
    """Ce que la configuration d'ogs règle des noms, préfixe compris : la racine
    des homes (naming.homeRoot) et le domaine de préproduction
    (environments.prep.domain) — leurs défauts si elle les tait."""
    c = {}
    try:
        with open(prefixe + "/etc/ogs/config.yaml", encoding="utf-8") as f:
            c = yaml.safe_load(f) or {}
    except (OSError, ValueError, yaml.YAMLError):
        pass
    if not isinstance(c, dict):
        c = {}
    prep = chemin(c, "environments.prep.domain")
    return {
        "homes": prefixe + (chemin(c, "naming.homeRoot") or "/home"),
        "prep": str(DOMAINE_PREP if prep is None else prep).strip().lower().lstrip("."),
    }

def slug(s):
    """naming.slug : ce qu'ogs garde d'un nom pour en faire un identifiant."""
    i = re.sub(r"[^a-z0-9]", "", s)
    if not i:
        return None
    if i[0].isdigit():
        i = "s" + i
    if len(i) > 28:
        i = i[:21] + "_" + hashlib.sha256(s.encode()).hexdigest()[:6]
    return i

def hote_effectif(host, env, prep):
    """naming.EffectiveHost, sur un host déjà normalisé : le host réellement
    servi. En préproduction, ogs le construit sur son domaine dédié —
    <label>.<domaine>, le label étant le host privé de son dernier label, si bien
    que shop.fr et shop.com y donnent le même —, sauf s'il y est déjà. None s'il
    n'est pas calculable."""
    if not host or env != "prep":
        return host or None
    if not prep:
        return None
    if host == prep or host.endswith("." + prep):
        return host
    label = slug(".".join(host.split(".")[:-1]))
    return f"{label}.{prep}" if label else None

def identite(o, pid, conf):
    """Ce qu'un projet réclame sur la machine, sous les critères d'unicité d'ogs
    (validate.Uniqueness) : domaines servis, compte, home, bases et leurs
    utilisateurs. Relu d'ogs, tout est résolu. Tiré du manifeste, ce qui n'est pas
    écrit prend le défaut d'ogs — compte : l'identifiant ; domaine et home : le
    host effectif, construit en préproduction sur le domaine de `conf`, et le
    home sous sa racine. Un host effectif incalculable laisse domaines et home
    inconnus (None) : c'est alors le couple host et environnement qui les
    représente.

    Le host brut n'est pas un critère : une préproduction clonée d'un site
    (`ogs site clone --to prep`) garde son metadata.host, sans rien partager
    avec lui."""
    spec = o.get("spec") or {}
    bases, comptes = set(), set()
    for db in spec.get("databases") or []:
        if not isinstance(db, dict):
            continue
        f = famille(db.get("engine"))
        if db.get("name"):
            bases.add(f"{f}:{db['name']}")
        for u in [db] + [x for x in db.get("extraUsers") or [] if isinstance(x, dict)]:
            if u.get("user"):
                comptes.add(f"{f}:{u['user']}")
    env = str(chemin(o, "metadata.env") or "").strip().lower() or "prod"
    host = hote(chemin(o, "metadata.host"))
    effectif = hote_effectif(host, env, conf["prep"])
    noms = [x.get("name") for x in chemin(spec, "domains") or [] if isinstance(x, dict) and x.get("name")]
    if noms:
        domaines = {hote(n) for n in noms}
    elif effectif:
        domaines = {effectif}
    else:
        domaines = None
    home = chemin(spec, "layout.root")
    if not home and effectif:
        home = os.path.join(conf["homes"], effectif)
    return {
        "host": host,
        "env": env,
        "domaines": domaines,
        "compte": chemin(spec, "user.name") or pid,
        "home": home,
        "bases": bases,
        "comptes": comptes,
    }

def domaines_communs(a, b):
    """Ce que deux projets serviraient tous deux : (genre, nom). Un domaine inconnu
    (host effectif incalculable) se juge au couple host et environnement : deux
    préproductions d'un même host en auraient le même."""
    if a["domaines"] is not None and b["domaines"] is not None:
        return [("domaine", x) for x in sorted(a["domaines"] & b["domaines"])]
    if a["host"] and (a["host"], a["env"]) == (b["host"], b["env"]):
        return [("host de préproduction", a["host"] + " (ogs.spec.domains non écrit)")]
    return []

def recoupe(a, b):
    a, b = os.path.normpath(a), os.path.normpath(b)
    return os.path.commonpath([a, b]) in (a, b)

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

def c_controle(m, prefixe, *ids):
    texte, d = charger(m)
    conf = conf_ogs(prefixe)
    erreurs = []
    def e(pid, msg):
        erreurs.append(f"{pid}: {msg}")
    if d.get("version") != 1:
        e("manifeste", "version 1 attendue")
    # source.ip_publique : le retour arrière attend que le DNS la désigne de nouveau.
    for cle in ("source.ssh", "source.ip_publique", "cible.ip_publique"):
        if chemin(d, cle) in (None, ""):
            e("manifeste", f"{cle} est requis")
    blocs = blocs_projets(texte)
    vus = set()
    reclames = {}
    identites = []
    for p in projets(d):
        pid = p.get("id")
        if not pid:
            e("?", "projet sans id")
            continue
        if pid in vus:
            e(pid, "id en double")
        vus.add(pid)
        o = p.get("ogs") or {}
        # Ce que chaque projet réclame sur la cible, relevé pour tous — sélectionnés
        # ou non — puisqu'une collision avec un projet non sélectionné compte aussi.
        for b in p.get("bases") or []:
            if isinstance(b, dict) and b.get("cible"):
                reclames.setdefault(("base", f"{famille(b.get('moteur'))}:{b['cible']}"), set()).add(pid)
        ident = identite(o, pid, conf)
        identites.append((pid, ident))
        for x in ident["bases"]:
            reclames.setdefault(("base", x), set()).add(pid)
        for x in ident["comptes"]:
            reclames.setdefault(("utilisateur de base", x), set()).add(pid)
        reclames.setdefault(("compte", ident["compte"]), set()).add(pid)
        if ids and pid not in ids:
            continue
        if "À VÉRIFIER" in blocs.get(pid, ""):
            n = blocs[pid].count("À VÉRIFIER")
            e(pid, f"{n} valeur(s) encore « À VÉRIFIER »")
        if not isinstance(p.get("vague"), int):
            e(pid, "vague : entier requis")
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
        maint = p.get("maintenance_cible")
        if maint not in (None, "aucune") and not (isinstance(maint, dict) and maint.get("activer") and maint.get("desactiver")):
            e(pid, "maintenance_cible : activer et desactiver requis, ou « aucune »")
        for c in src.get("configuration") or []:
            if c.get("format") not in FORMATS:
                e(pid, f"configuration {c.get('fichier')} : format parmi {sorted(FORMATS)}")
            if not c.get("fichier") or not isinstance(c.get("cles"), dict):
                e(pid, "configuration : fichier et cles requis")
            if c.get("format") == "autre":
                e(pid, f"configuration {c.get('fichier')} : format « autre » non réécrit par le script")
        for b in p.get("bases") or []:
            if b.get("moteur") not in MOTEURS:
                e(pid, f"bases : moteur parmi {sorted(MOTEURS)}")
            if not b.get("source") or not b.get("cible"):
                e(pid, "bases : source et cible requis")
            elif not NOM_BASE.match(str(b["cible"])):
                e(pid, f"bases : nom de base cible invalide ({b['cible']}) — caractères [A-Za-z0-9_]")
            elif f"{famille(b.get('moteur'))}:{b['cible']}" not in ident["bases"]:
                e(pid, f"bases : {b['cible']} n'est pas déclarée dans ogs.spec.databases (moteur {b.get('moteur')})")
        a = p.get("amqp") or {}
        if a.get("present") and (not a.get("vhost_source") or not a.get("transports")):
            e(pid, "amqp : vhost_source et transports requis")
        for w in chemin(p, "arret_a_la_bascule.workers") or []:
            if not isinstance(w, dict) or not w.get("arreter") or not w.get("relancer"):
                e(pid, "arret_a_la_bascule.workers : arreter et relancer requis")
        for v in p.get("verifications") or []:
            if not v.get("url") and not v.get("commande"):
                e(pid, "verifications : url ou commande requis")
    # Deux projets qui réclament la même base, le même compte, le même home ou le
    # même domaine se détruiraient l'un l'autre : synchroniser l'un remplace la
    # base et les fichiers de l'autre. Une production et sa préproduction, au même
    # host, ne partagent rien.
    for (genre, nom), qui in sorted(reclames.items()):
        if len(qui) > 1 and (not ids or qui & set(ids)):
            e(", ".join(sorted(qui)), f"{genre} en commun : {nom}")
    for i, (pa, a) in enumerate(identites):
        for pb, b in identites[i + 1:]:
            if pa == pb or (ids and pa not in ids and pb not in ids):
                continue
            qui = ", ".join(sorted((pa, pb)))
            for genre, nom in domaines_communs(a, b):
                e(qui, f"{genre} en commun : {nom}")
            if a["home"] and b["home"] and recoupe(a["home"], b["home"]):
                e(qui, f"home en commun : {a['home']}, {b['home']}")
    for pid in ids:
        if pid not in vus:
            e(pid, "absent du manifeste")
    for x in erreurs:
        print(x)
    sys.exit(3 if erreurs else 0)

def c_parc(liste):
    """Les identifiants que rend « ogs project list -o json »."""
    with open(liste, encoding="utf-8") as f:
        items = json.load(f).get("items") or []
    print("\n".join(x["id"] for x in items if x.get("id")))

def c_appartenance(m, pid, parc, prefixe):
    """Refuse qu'une étape écrive dans un compte, un home ou une base qu'un autre
    projet du parc réclame, ou qu'elle serve un domaine qu'il sert. `parc` porte
    l'export d'ogs de chaque projet de la cible ; celui de `pid`, s'il y figure,
    fait foi, sinon le manifeste."""
    _, d = charger(m)
    p = trouver(d, pid)
    conf = conf_ogs(prefixe)
    erreurs = []
    notre = os.path.join(parc, pid + ".yaml")
    voulu = identite(p.get("ogs") or {}, pid, conf)
    if os.path.exists(notre):
        with open(notre, encoding="utf-8") as f:
            o = yaml.safe_load(f) or {}
        nous = identite(o, pid, conf)
        if (nous["host"], nous["env"]) != (voulu["host"], voulu["env"]):
            erreurs.append(f"le projet {pid} de la cible sert {nous['host']} ({nous['env']}), "
                           f"le manifeste {voulu['host']} ({voulu['env']})")
    else:
        nous = voulu
    for b in p.get("bases") or []:
        cle = f"{famille(b.get('moteur'))}:{b.get('cible')}"
        if cle not in nous["bases"]:
            erreurs.append(f"la base {cle} n'est pas déclarée par le projet {pid} de la cible")
    for fichier in sorted(glob.glob(os.path.join(parc, "*.yaml"))):
        autre = os.path.basename(fichier)[:-len(".yaml")]
        if autre == pid:
            continue
        with open(fichier, encoding="utf-8") as f:
            lui = identite(yaml.safe_load(f) or {}, autre, conf)
        for genre, nom in domaines_communs(nous, lui):
            erreurs.append(f"le {genre} {nom} est celui du projet {autre}")
        if nous["compte"] == lui["compte"]:
            erreurs.append(f"le compte {nous['compte']} est celui du projet {autre}")
        if nous["home"] and lui["home"] and recoupe(nous["home"], lui["home"]):
            erreurs.append(f"le home {nous['home']} recoupe celui du projet {autre} ({lui['home']})")
        for x in sorted(nous["bases"] & lui["bases"]):
            erreurs.append(f"la base {x} est déclarée par le projet {autre}")
        for x in sorted(nous["comptes"] & lui["comptes"]):
            erreurs.append(f"l'utilisateur de base {x} est celui du projet {autre}")
    for x in erreurs:
        print(x)
    sys.exit(3 if erreurs else 0)

def c_neuf(m, pid, prefixe):
    """Ce que la première déclaration va créer, une ligne par élément : compte,
    home (s'il est connu), bases et utilisateurs de base, « famille:nom »."""
    _, d = charger(m)
    ident = identite(trouver(d, pid).get("ogs") or {}, pid, conf_ogs(prefixe))
    lignes = [("compte", ident["compte"])]
    if ident["home"]:
        lignes.append(("home", ident["home"]))
    lignes += [("base", x) for x in sorted(ident["bases"])]
    lignes += [("utilisateur", x) for x in sorted(ident["comptes"])]
    for genre, nom in lignes:
        # Interpolés dans du SQL : ogs plan -f les a validés, mais une garde ne se
        # fie pas à l'ordre des appels.
        if genre in ("base", "utilisateur") and not NOM_BASE.match(nom.split(":", 1)[1]):
            sys.exit(f"{genre} invalide : {nom}")
        print(f"{genre}{SEP}{nom}")

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

def c_domaines(m, pid, prefixe):
    """Les noms à faire pointer vers la cible, tels qu'écrits — www. compris, un
    nom DNS à part entière. Sans spec.domains, le host effectif : en préproduction,
    celui qu'ogs construit, et non le host de la production."""
    _, d = charger(m)
    o = trouver(d, pid)["ogs"]
    noms = [x["name"] for x in chemin(o, "spec.domains") or [] if x.get("name")]
    effectif = identite(o, pid, conf_ogs(prefixe))["domaines"]
    print("\n".join(noms or sorted(effectif or []) or [chemin(o, "metadata.host")]))

def c_home(m, pid, prefixe):
    """Le home qu'ogs donnerait au projet, avant qu'il soit déclaré."""
    _, d = charger(m)
    print(identite(trouver(d, pid).get("ogs") or {}, pid, conf_ogs(prefixe))["home"] or "")

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

def c_config(fichier, format_, cle):
    # La valeur arrive par l'environnement, pas par la ligne de commande : celle-ci
    # est lisible de tout compte de la machine (ps), et elle porte des mots de passe.
    valeur = os.environ["MIGRER_VALEUR"]
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
    "parc": c_parc, "appartenance": c_appartenance, "home": c_home, "neuf": c_neuf,
}
COMMANDES[sys.argv[1]](*sys.argv[2:])
PYEOF

py() { python3 "$PY" "$@"; }
# m <id|-> <chemin> : une valeur du manifeste (vide si absente).
m() { py get "$MANIFESTE" "$@"; }
# ml <id|-> <chemin> [champs...] : les éléments d'une liste, une ligne chacun.
ml() { py lignes "$MANIFESTE" "$@"; }

# temporaire [-d] : un fichier (un répertoire) dans celui de l'exécution.
temporaire() { mktemp "$@" "$TMPD/XXXXXX"; }

# -----------------------------------------------------------------------------
# Exécution : lecture toujours, modification seulement avec --executer
# -----------------------------------------------------------------------------
# Aucune commande ne lit l'entrée standard du script, hormis la confirmation :
# lancée dans une boucle « while read … done < <(…) », elle avalerait les
# éléments suivants de la liste, qui ne seraient jamais traités.
cmd_affiche() { printf '%s   $ %s%s\n' "$C_CMD" "$*" "$C_RAZ"; }

# faire <commande> [args...] : une commande qui modifie quelque chose.
faire() {
    local ligne
    ligne="$(printf '%q ' "$@")"
    cmd_affiche "${ligne% }"
    ((EXECUTER)) || return 0
    "$@" </dev/null
}

# faire_sh <script> : un tube ou une redirection, affiché tel quel.
faire_sh() {
    cmd_affiche "$1"
    ((EXECUTER)) || return 0
    bash -o pipefail -c "$1" </dev/null
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

ogs_lire() { "${OGS[@]}" "$@" 2>/dev/null </dev/null; }

# confirmer <question> <réponse> [toujours] : --oui dispense de confirmer une
# intention, pas d'affirmer un fait que seul l'exploitant peut constater —
# « toujours » pose alors la question même sous --oui. Sans réponse (entrée
# fermée), c'est un abandon.
confirmer() {
    if [[ "${3:-}" != toujours ]] && ((OUI)); then return 0; fi
    ((EXECUTER)) || return 0
    local reponse=''
    printf '%s   ? %s [tapez « %s » pour confirmer] %s' "$C_AVERT" "$1" "$2" "$C_RAZ"
    read -r reponse || true
    [[ "$reponse" == "$2" ]] || fatal "abandon"
}

# -----------------------------------------------------------------------------
# La source, par ssh
# -----------------------------------------------------------------------------
SRC_SSH="$(m - source.ssh)"
SRC_PORT="$(m - source.port_ssh)"; SRC_PORT="${SRC_PORT:-22}"
SRC_IP="$(m - source.ip_publique)"
SRC_IP6="$(m - source.ip6_publique)"
SSH_OPTS=(-p "$SRC_PORT" -o BatchMode=yes -o ConnectTimeout=15
          -o ControlMaster=auto -o "ControlPath=${TMPDIR:-/tmp}/migrer-ssh-%C" -o ControlPersist=15m)
# Sans -n : rsync parle son protocole par l'entrée standard de ssh.
RSYNC_SSH="ssh $(printf '%q ' "${SSH_OPTS[@]}")"
CIBLE_IP="$(m - cible.ip_publique)"
CIBLE_IP6="$(m - cible.ip6_publique)"
TRAVAIL="$(m - migration.travail)"; TRAVAIL="${TRAVAIL:-/var/lib/ogs-migration}"

# ssh -n partout ailleurs : sans lui, ssh lit l'entrée standard jusqu'au bout et
# la transmet au distant — multiplexé ou non —, et dans une boucle « while read »
# il avale tous les éléments suivants.
# src_lire <script> : lecture sur la source, toujours exécutée.
src_lire() { ssh -n "${SSH_OPTS[@]}" "$SRC_SSH" "$1"; }
# src_faire <script> : modification sur la source.
src_faire() {
    cmd_affiche "[source] $1"
    ((EXECUTER)) || return 0
    ssh -n "${SSH_OPTS[@]}" "$SRC_SSH" "$1"
}

# -----------------------------------------------------------------------------
# Ce que ogs sait d'un projet : compte, home — ou ce qu'il en déduirait
# -----------------------------------------------------------------------------
# prefixe_ogs : en mode préfixé (essai), /etc/ogs et les homes sont sous le préfixe.
prefixe_ogs() { if [[ "${OGS[*]}" =~ --prefix[=\ ]([^ ]+) ]]; then printf '%s' "${BASH_REMATCH[1]}"; fi; }
fichier_projet() { printf '%s/etc/ogs/projects/%s.yaml' "$(prefixe_ogs)" "$1"; }
# Le préfixe sous lequel lire la configuration d'ogs : racine des homes et
# domaine de préproduction décident du home et des domaines d'un projet qui ne
# les écrit pas.
PREFIXE_OGS="$(prefixe_ogs)"
# domaines_de <id> : les noms DNS du projet, un par ligne.
domaines_de() { py domaines "$MANIFESTE" "$1" "$PREFIXE_OGS"; }

declare -A P_USER P_GROUP P_ROOT P_KIND
charger_projet() {
    local id="$1" export_f
    P_KIND[$id]="$(m "$id" ogs.spec.app.kind)"
    export_f="$(temporaire)"
    if ogs_lire project export "$id" >"$export_f" && [[ -s "$export_f" ]]; then
        P_USER[$id]="$(py export "$export_f" spec.user.name)"
        P_GROUP[$id]="$(py export "$export_f" spec.user.group)"
        P_ROOT[$id]="$(py export "$export_f" spec.layout.root)"
    else
        # Projet pas encore déclaré : les défauts d'ogs.
        P_USER[$id]="$id"; P_GROUP[$id]="$id"; P_ROOT[$id]="$(py home "$MANIFESTE" "$id" "$PREFIXE_OGS")"
    fi
    # Vide, chaque copie viserait la racine du système.
    [[ -n "${P_ROOT[$id]}" ]] || fatal "$id : home indéterminé (ni ogs project export, ni host effectif calculable)"
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
# Une ligne par événement, ajoutée à la fin : depot (fichier de projet déposé),
# declare, synchro, arret (bascule engagée : source en maintenance),
# arret:<empreinte> et relance:<empreinte> (un worker de la source arrêté,
# relancé), copie (copie finale faite et vérifiée), bascule, retour (source de
# nouveau en service).
noter() {
    ((EXECUTER)) || return 0
    mkdir -p "$TRAVAIL/$1"
    printf '%s %s\n' "$2" "$(date -Is)" >>"$TRAVAIL/$1/etat"
}
dernier() {
    [[ -r "$TRAVAIL/$1/etat" ]] || return 0
    awk -v c="$2" '$1 == c { d = $2 } END { print d }' "$TRAVAIL/$1/etat"
}
# actif <id> <événement> : noté, et aucun retour ne l'a suivi. L'ordre des lignes
# fait foi, pas les dates : un changement d'heure rendrait leur décalage trompeur.
actif() {
    [[ -r "$TRAVAIL/$1/etat" ]] || return 1
    awk -v c="$2" '$1 == c { e = NR } $1 == "retour" { r = NR } END { exit !(e > r) }' "$TRAVAIL/$1/etat"
}
# bascule_active <id> : basculé, et pas revenu depuis.
bascule_active() { actif "$1" bascule; }
# empreinte <commande> : de quoi la désigner dans l'état, sans blanc.
empreinte() { printf '%s' "$1" | sha256sum | cut -c1-12; }
# de_la_migration <id> : le projet de la cible a été déposé par cette migration.
de_la_migration() { [[ -n "$(dernier "$1" depot)$(dernier "$1" declare)" ]]; }
# etat_bascule <id> : « basculé le … » ou « bascule engagée le … » ; code 1 sinon.
etat_bascule() {
    if bascule_active "$1"; then printf 'basculé le %s' "$(dernier "$1" bascule)"
    elif actif "$1" arret; then printf 'bascule engagée le %s, non terminée' "$(dernier "$1" arret)"
    else return 1; fi
}

# -----------------------------------------------------------------------------
# Appartenance : ce que le script écrase doit être au seul projet migré
# -----------------------------------------------------------------------------
# garde_appartenance <id> : le parc est relu d'ogs lui-même. `ogs apply <id>` ne
# compare le projet à aucun autre : une base homonyme d'un voisin serait reprise
# et accordée au projet migré, puis supprimée par la synchronisation (DROP), et un
# projet de même identifiant verrait son home écrasé (rsync --delete).
garde_appartenance() {
    local id="$1" parc liste autre sortie rc=0 ligne
    parc="$(temporaire -d)"; liste="$(temporaire)"
    ogs_lire project list -o json >"$liste" \
        || fatal "$id : parc de la cible illisible (ogs project list) — à qui appartiennent home et bases ?"
    while IFS= read -r autre; do
        [[ -n "$autre" ]] || continue
        ogs_lire project export "$autre" >"$parc/$autre.yaml" \
            || fatal "$id : projet $autre de la cible illisible (ogs project export $autre)"
    done < <(py parc "$liste")
    if [[ -s "$parc/$id.yaml" ]] && ! de_la_migration "$id"; then
        fatal "$id : un projet de ce nom existe déjà sur la cible, sans avoir été déposé par cette migration ($TRAVAIL/$id/etat) — refus : ce n'est pas celui du manifeste"
    fi
    sortie="$(py appartenance "$MANIFESTE" "$id" "$parc" "$PREFIXE_OGS")" || rc=$?
    if ((rc)); then
        while IFS= read -r ligne; do erreur "$id : $ligne"; done <<<"$sortie"
        fatal "$id : refus — ce que la migration écraserait appartient à un autre projet de la cible"
    fi
}

# cible_neuve <id> : à la première déclaration, rien de ce qu'elle va créer — ni
# le compte du projet, ni son home, ni une base ou un utilisateur de base — ne doit
# déjà exister sur la cible. ogs les reprendrait tels quels (CREATE DATABASE IF
# NOT EXISTS, droits accordés à l'utilisateur trouvé), et la synchronisation
# écraserait ce qu'aucun projet du parc ne déclare : les restes d'un projet
# supprimé, ou un site hors d'ogs.
cible_neuve() {
    local id="$1" genre nom famille objet n requete lignes
    lignes="$(py neuf "$MANIFESTE" "$id" "$PREFIXE_OGS")" || fatal "$id : déclaration illisible"
    while IFS=$'\x1f' read -r genre nom; do
        case "$genre" in
            compte)
                if getent passwd "$nom" >/dev/null; then
                    fatal "$id : le compte $nom existe déjà sur la cible, avant cette migration — refus : ses fichiers seraient écrasés"
                fi
                continue ;;
            home)
                if [[ -e "$nom" || -L "$nom" ]]; then
                    fatal "$id : le home $nom existe déjà sur la cible, avant cette migration — refus : la synchronisation remplacerait ce qu'il contient"
                fi
                continue ;;
        esac
        famille="${nom%%:*}"; objet="${nom#*:}"
        case "$famille:$genre" in
            mysql:base)        requete="SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '$objet'" ;;
            mysql:utilisateur) requete="SELECT COUNT(*) FROM mysql.user WHERE User = '$objet'" ;;
            postgres:base)        requete="SELECT COUNT(*) FROM pg_database WHERE datname = '$objet'" ;;
            postgres:utilisateur) requete="SELECT COUNT(*) FROM pg_roles WHERE rolname = '$objet'" ;;
            *) continue ;;
        esac
        n=''
        if [[ $famille == mysql ]]; then
            n="$(mysql -N -B -e "$requete" </dev/null 2>/dev/null)" || n=''
        else
            n="$(runuser -u postgres -- psql -tAc "$requete" </dev/null 2>/dev/null)" || n=''
        fi
        n="${n//[[:space:]]/}"
        if [[ $genre == base ]]; then genre="la base"; else genre="l'utilisateur de base"; fi
        case "$n" in
            0) ;;
            '') ((EXECUTER)) && fatal "$id : impossible de vérifier que $genre $objet n'existe pas déjà ($famille injoignable)"
                avert "$id : existence de $genre $objet non vérifiée ($famille injoignable)" ;;
            *)  fatal "$id : $genre $objet existe déjà sur la cible, avant cette migration — refus : ogs se l'approprierait, aux dépens de qui l'emploie" ;;
        esac
    done <<<"$lignes"
}

# -----------------------------------------------------------------------------
# Contrôle du manifeste
# -----------------------------------------------------------------------------
controler() {
    local sortie rc=0
    sortie="$(py controle "$MANIFESTE" "$PREFIXE_OGS" "$@")" || rc=$?
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
        done < <(domaines_de "$id")
    done

    # Bascule et retour attendent que chaque résolveur rende exactement l'adresse
    # d'une machine : un domaine qui ne désigne pas déjà exactement la source ne
    # désignera pas davantage la cible, et l'attente échouerait en pleine fenêtre.
    # Un projet dont la bascule est engagée ou faite a ses domaines, ou une part
    # d'entre eux, vers la cible : rien à en attendre ici.
    etape "Résolution actuelle vers la source ($SRC_IP${SRC_IP6:+, $SRC_IP6})"
    local etat
    for id in "$@"; do
        if etat="$(etat_bascule "$id")"; then info "$id : $etat — résolution vers la source non contrôlée"; continue; fi
        while IFS= read -r dom; do
            if resolu_vers "$dom" "$SRC_IP" "$SRC_IP6"; then ok "$dom → $SRC_IP"
            elif sans_adresse "$dom"; then
                # Un domaine nouveau, que seul ogs déclare : il sera créé vers la
                # cible, et la bascule l'attendra ainsi ; le retour, lui, ne le
                # verra jamais désigner la source.
                avert "$dom : aucune adresse — domaine nouveau ? à créer vers la cible à la bascule ; un retour exigera --sans-attente-dns"
            else
                erreur "$dom ne résout pas exactement vers $SRC_IP${SRC_IP6:+ et $SRC_IP6} sur ${RESOLVEURS[*]} ($(dig +short A "$dom" @"${RESOLVEURS[0]}" | tr '\n' ' '))"
                erreur "$dom : corriger source.ip_publique / ip6_publique, ou prévoir --sans-attente-dns (CDN, répartiteur, plusieurs A)"
                manque=1
            fi
        done < <(domaines_de "$id")
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
    "${OGS[@]}" plan -f "$brouillon" >/dev/null </dev/null || fatal "$id : déclaration refusée par ogs plan -f"
    ok "déclaration valide"

    etape "Appartenance (parc de la cible)"
    garde_appartenance "$id"
    de_la_migration "$id" || cible_neuve "$id"
    ok "ni compte, ni home, ni base d'un autre projet"

    if [[ -e "$fichier" ]]; then
        # Le fichier a pu être complété par ogs (amqp messenger) : ne pas l'écraser.
        info "$fichier existe déjà : laissé tel quel (le supprimer pour le redéposer)"
    else
        etape "Dépôt"
        faire install -m 0644 "$brouillon" "$fichier"
        noter "$id" depot
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
# La source est tirée en root vers un miroir hors du home (<travail>/<id>/miroir,
# chaîne entièrement root), puis posée dans le home par un rsync dont le côté qui
# écrit tourne sous le compte du projet. Le home appartient à ce compte (D51), qui
# peut y remplacer tout répertoire par un lien : root qui y écrirait par le nom
# écrirait, supprimerait (--delete) et donnerait au projet (--chown) ce que le
# lien désigne — l'invariant de D70 et D71. Le récepteur est lancé par
# « -e runuser … -- » vers l'hôte « env » : rsync appelle alors
# « runuser … -- env rsync --server … », et seul ce processus écrit. Sans shell
# entre les deux, -s (--secluded-args) est requis : rsync 3.2.4 et suivants
# échappent les chemins distants pour un shell, et « www/mes fichiers »
# arriverait en « www/mes\ fichiers » ; avec -s, les chemins passent par le
# protocole, jamais par la ligne de commande.
#
# Le miroir n'est pas une copie du home : son contenu vient de la source, liens
# compris (-l), et un lien recopié y serait suivi par toute écriture faite par le
# nom au-dessous de lui. Le code va dans <miroir>/code, chaque donnée hors code
# dans un sous-arbre à elle, <miroir>/donnees/<empreinte de vers>.d (un
# répertoire) ou .f (un fichier) : jamais sous la copie du code, dont un
# composant de « vers » — www/public, pour www/public/uploads — peut être un lien
# de la source, que root suivait hors du miroir pour y créer, écrire et
# supprimer. Aucun composant d'une destination de root n'est ainsi tiré de la
# source ; c'est le récepteur du compte du projet qui les parcourt, dans son home.
synchroniser_fichiers() {
    local id="$1" root racine code miroir donnees excl=() motif de vers k
    root="${P_ROOT[$id]}"; miroir="$TRAVAIL/$id/miroir"; donnees="$miroir/donnees"
    racine="$(m "$id" source.racine)"; code="$(m "$id" source.code)"
    etape "Fichiers : $racine/$code → $root/www (par $miroir)"
    while IFS= read -r motif; do if [[ -n "$motif" ]]; then excl+=(--exclude="$motif"); fi; done < <(ml "$id" source.exclure)
    # Les destinations sous www/ des données hors code sont protégées du --delete
    # du code — et un lien symbolique de même nom (release Deployer) n'est pas copié.
    while IFS=$'\x1f' read -r de vers; do
        if [[ "$vers" == www/* ]]; then excl+=(--exclude="/${vers#www/}"); fi
    done < <(ml "$id" source.donnees_hors_code de vers)

    local tirer=(rsync -rlptH --delete --chmod=o-rwx -e "$RSYNC_SSH")
    local poser=(rsync -rlptH --delete -s -e "runuser -u ${P_USER[$id]} -g ${P_GROUP[$id]} --")
    local projet=(runuser -u "${P_USER[$id]}" -g "${P_GROUP[$id]}" --)
    faire install -d -m 0700 "$TRAVAIL/$id" "$miroir" "$donnees"
    faire "${tirer[@]}" "${excl[@]}" "$SRC_SSH:$racine/$code/" "$miroir/code/"
    faire "${poser[@]}" "${excl[@]}" "$miroir/code/" "env:$root/www/"

    while IFS=$'\x1f' read -r de vers; do
        [[ -n "$de" ]] || continue
        k="$donnees/$(empreinte "$vers")"
        if src_lire "test -d $(printf '%q' "$racine/$de")"; then
            faire "${tirer[@]}" "$SRC_SSH:$racine/$de/" "$k.d/"
            faire "${projet[@]}" install -d -m 0750 "$root/$vers"
            faire "${poser[@]}" "$k.d/" "env:$root/$vers/"
        else
            faire "${tirer[@]}" "$SRC_SSH:$racine/$de" "$k.f"
            faire "${poser[@]}" "$k.f" "env:$root/$vers"
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
                faire_sh "$(printf 'ssh -n %s %q %q | gzip -1 > %q' "$(printf '%q ' "${SSH_OPTS[@]}")" "$SRC_SSH" \
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
                faire_sh "$(printf 'ssh -n %s %q %q | gzip -1 > %q' "$(printf '%q ' "${SSH_OPTS[@]}")" "$SRC_SSH" \
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
            cmd_affiche "réécrire $cle dans $chemin_f ($format), sous le compte ${P_USER[$id]}"
            if ((EXECUTER)); then
                [[ -f "$chemin_f" ]] || fatal "$chemin_f absent après la copie"
                # Sous le compte du projet : le fichier est dans son home, où un lien
                # posé à sa place — ou recopié de la source — mènerait root ailleurs.
                # Le script Python arrive par l'entrée standard, le fichier de root
                # n'étant pas lisible de ce compte.
                MIGRER_VALEUR="$valeur" runuser -u "${P_USER[$id]}" -g "${P_GROUP[$id]}" -- \
                    python3 - config "$chemin_f" "$format" "$cle" <"$PY" \
                    || fatal "$id : $fichier, $cle : réécriture impossible"
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
    local id="$1" etat
    charger_projet "$id"
    [[ -n "$(dernier "$id" declare)" ]] || ((EXECUTER == 0)) || fatal "$id : non déclaré — phase declarer d'abord"
    if etat="$(etat_bascule "$id")"; then fatal "$id : $etat — synchronisation refusée"; fi
    garde_appartenance "$id"
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
    local id etat refus=0
    # Une fois la bascule engagée, la cible fait foi : la resynchroniser depuis la
    # source, figée en maintenance, supprimerait sa base (DROP DATABASE) et ce
    # qu'elle a reçu depuis (rsync --delete). Refus avant toute copie, pour
    # qu'un sélecteur oublié ne laisse pas la phase à moitié faite.
    for id in "$@"; do
        if etat="$(etat_bascule "$id")"; then
            erreur "$id : $etat — la cible fait foi : synchroniser supprimerait sa base et ce qu'elle a reçu depuis"
            refus=1
        fi
    done
    ((refus == 0)) || fatal "synchronisation refusée, rien n'a été fait : désigner les seuls projets à synchroniser (ex. vague:2), ou « retour » d'abord"
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
# http_reponse <corps> <url> [options curl...] : « code cible_de_redirection »,
# code 000 sans réponse.
http_reponse() {
    local corps="$1" url="$2"; shift 2
    curl -sS -o "$corps" -w '%{http_code} %{redirect_url}' --max-time 30 "$@" "$url" </dev/null 2>/dev/null || true
}

# verifier_projet <id> force|dns : rend 1 si une vérification bloquante échoue.
verifier_projet() {
    local id="$1" mode="$2" echecs=0 url statut contient commande attendu
    charger_projet "$id"
    etape "Vérifications de $id ($([[ $mode == dns ]] && echo 'par le DNS public' || echo "forcées vers $CIBLE_IP"))"
    while IFS=$'\x1f' read -r url statut contient commande attendu; do
        if [[ -n "$url" ]]; then
            local hote corps reponse code redirection essai="$url" resolve=() degrade=0
            hote="${url#*://}"; hote="${hote%%/*}"; hote="${hote%%:*}"
            corps="$(temporaire)"
            # Sans -k : tant que la cible n'a pas de certificat pour ce domaine, le
            # premier vhost TLS de la machine répond à sa place — un voisin, dont le
            # 200 ne dit rien de ce site. Un certificat qui ne couvre pas le domaine
            # rend 000, et la vérification passe en HTTP, par le vhost du projet.
            [[ $mode == force ]] && resolve=(--resolve "$hote:443:$CIBLE_IP" --resolve "$hote:80:$CIBLE_IP")
            reponse="$(http_reponse "$corps" "$essai" "${resolve[@]}")"
            code="${reponse%% *}"; code="${code:-000}"
            if [[ $mode == force && "$code" == 000 && "$url" == https://* ]]; then
                # Pas encore de certificat sur la cible : le vhost ne sert que HTTP.
                essai="http://${url#https://}"; degrade=1
                reponse="$(http_reponse "$corps" "$essai" "${resolve[@]}")"
                code="${reponse%% *}"; code="${code:-000}"
            fi
            redirection="${reponse#"$code"}"; redirection="${redirection# }"
            if [[ -z "$statut" && ( "$code" == 000 || "$code" == 5[0-9][0-9] ) ]]; then
                # Sans statut attendu, il faut au moins que le site réponde, et sans
                # erreur du serveur : une URL muette n'a rien vérifié.
                erreur "$essai : $code$([[ $code == 000 ]] && echo ' (aucune réponse)'), attendu une réponse du site"
                echecs=$((echecs + 1))
            elif [[ -n "$statut" && "$code" != "$statut" ]]; then
                if ((degrade)) && [[ "$code" == 3[0-9][0-9] ]] \
                    && [[ "$redirection" == "https://$hote" || "$redirection" == "https://$hote/"* || "$redirection" == "https://$hote:"* ]]; then
                    # Le site renvoie lui-même vers HTTPS, que la cible ne sert pas
                    # encore : la réponse vient de lui, mais sa page reste invérifiable.
                    avert "$essai : $code vers $redirection (attendu $statut en HTTPS ; pas de certificat encore) — page non vérifiée"
                else
                    erreur "$essai : $code, attendu $statut"; echecs=$((echecs + 1))
                fi
            elif [[ -n "$contient" ]] && ! grep -qF -- "$contient" "$corps"; then
                erreur "$essai : « $contient » absent de la page"; echecs=$((echecs + 1))
            else
                ok "$essai : $code"
            fi
        elif [[ -n "$commande" ]]; then
            local sortie
            if ((EXECUTER)) || [[ -d "${P_ROOT[$id]}/www" ]]; then
                sortie="$(bash -c "$(en_projet "$id" "$commande")" 2>&1 </dev/null || true)"
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

# cible_maintenance <id> activer|desactiver : maintenance_cible du manifeste, sous
# le compte du projet, depuis www/. Facultative : sans elle, la cible continue de
# servir le site pendant que le DNS revient vers la source.
cible_maintenance() {
    local id="$1" sens="$2" cmd
    cmd="$(m "$id" "maintenance_cible.$sens")"
    if [[ -z "$cmd" ]]; then
        if [[ $sens == activer && "$(m "$id" maintenance_cible)" != aucune ]]; then
            avert "$id : pas de maintenance_cible — la cible sert le site jusqu'à ce que le DNS désigne la source ; ce qu'elle reçoit d'ici là est perdu"
        fi
        return 0
    fi
    etape "Cible : maintenance ($sens)"
    faire_sh "$(en_projet "$id" "$cmd")" || erreur "$id : maintenance de la cible ($sens) en échec"
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
    local id="$1" user sauvegarde f w u s marque
    user="$(m "$id" source.utilisateur)"
    sauvegarde="/root/migration-ogs/$id"
    etape "Arrêt des tâches et workers de la source"
    src_faire "install -d -m 0700 $(printf '%q' "$sauvegarde")"
    if [[ "$(m "$id" arret_a_la_bascule.crontab_utilisateur)" == true ]]; then
        # Sauvegardée par renommage, et seulement si la crontab existe : une reprise
        # la trouve déjà retirée, et une redirection directe viderait la sauvegarde
        # du premier passage avant l'échec de « crontab -l ».
        u="$(printf '%q' "$user")"; s="$(printf '%q' "$sauvegarde/crontab")"
        src_faire "if crontab -l -u $u >/dev/null 2>&1; then crontab -l -u $u > $s.tmp && mv $s.tmp $s && crontab -r -u $u; fi"
    fi
    # cron ignore les fichiers de /etc/cron.d dont le nom contient un point.
    while IFS= read -r f; do
        if [[ -n "$f" ]]; then src_faire "test ! -e $(printf '%q' "$f") || mv $(printf '%q' "$f") $(printf '%q' "$f.migre-ogs")"; fi
    done < <(ml "$id" arret_a_la_bascule.fichiers_cron)
    # Chaque arrêt réussi est noté. Une bascule reprise le rejoue — un worker
    # relancé à la main entre-temps doit s'arrêter —, mais l'échec d'une commande
    # d'arrêt qui avait réussi au premier passage (supervisorctl rend 1 sur un
    # programme arrêté) n'interrompt plus la reprise. Le script n'en sait pas
    # davantage — l'échec peut avoir une autre cause — et l'avertissement ne dit
    # que cela. L'échec d'un arrêt jamais réussi, lui, interrompt la bascule : le
    # worker tournerait sur les deux serveurs.
    while IFS=$'\x1f' read -r w _; do
        [[ -n "$w" ]] || continue
        marque="arret:$(empreinte "$w")"
        if src_faire "$w"; then
            noter "$id" "$marque"
        elif actif "$id" "$marque"; then
            avert "« $w » a échoué ; la même commande avait réussi le $(dernier "$id" "$marque"), plus tôt dans cette bascule — reprise poursuivie"
        else
            fatal "$id : « $w » a échoué — le worker tournerait sur les deux serveurs"
        fi
    done < <(ml "$id" arret_a_la_bascule.workers arreter relancer)
}

# relancer_source <id> : rend 1 si une relance a échoué, sans s'arrêter — chaque
# tâche rétablie compte, et c'est le chemin du retour arrière. Comme l'arrêt, une
# relance réussie est notée : un retour repris qui la rejoue sur un worker déjà
# relancé n'y voit pas d'échec.
relancer_source() {
    local id="$1" user sauvegarde f w u s rc=0 marque
    user="$(m "$id" source.utilisateur)"
    sauvegarde="/root/migration-ogs/$id"
    etape "Relance des tâches et workers de la source"
    if [[ "$(m "$id" arret_a_la_bascule.crontab_utilisateur)" == true ]]; then
        # Mise de côté une fois réinstallée : un second retour ne doit pas remettre
        # en place la crontab d'une bascule ancienne.
        u="$(printf '%q' "$user")"; s="$(printf '%q' "$sauvegarde/crontab")"
        src_faire "test ! -s $s || { crontab -u $u $s && mv $s $s.retablie; }" \
            || { erreur "crontab de $user non rétablie ($sauvegarde/crontab)"; rc=1; }
    fi
    while IFS= read -r f; do
        if [[ -n "$f" ]]; then
            src_faire "test ! -e $(printf '%q' "$f.migre-ogs") || mv $(printf '%q' "$f.migre-ogs") $(printf '%q' "$f")" \
                || { erreur "$f non rétabli"; rc=1; }
        fi
    done < <(ml "$id" arret_a_la_bascule.fichiers_cron)
    while IFS=$'\x1f' read -r _ w; do
        [[ -n "$w" ]] || continue
        marque="relance:$(empreinte "$w")"
        if src_faire "$w"; then
            noter "$id" "$marque"
        elif actif "$id" "$marque"; then
            avert "« $w » a échoué ; la même commande avait réussi le $(dernier "$id" "$marque"), plus tôt dans ce retour"
        else
            erreur "« $w » a échoué"; rc=1
        fi
    done < <(ml "$id" arret_a_la_bascule.workers arreter relancer)
    return $rc
}

# resolu_vers <domaine> <ipv4> <ipv6> : chaque résolveur rend exactement ces
# adresses — et aucun AAAA si l'IPv6 est vide.
resolu_vers() {
    local dom="$1" ip4="$2" ip6="$3" r a aaaa
    for r in "${RESOLVEURS[@]}"; do
        a="$(dig +short A "$dom" @"$r" | grep -E '^[0-9.]+$' | sort -u | tr '\n' ' ')"
        [[ "$a" == "$ip4 " ]] || return 1
        aaaa="$(dig +short AAAA "$dom" @"$r" | grep ':' | sort -u | tr '\n' ' ')"
        if [[ -n "$ip6" ]]; then [[ -z "$aaaa" || "$aaaa" == "$ip6 " ]] || return 1
        else [[ -z "$aaaa" ]] || return 1; fi
    done
}

# sans_adresse <domaine> : aucun résolveur ne lui connaît de A ni de AAAA.
sans_adresse() {
    local dom="$1" r
    for r in "${RESOLVEURS[@]}"; do
        [[ -z "$(dig +short A "$dom" @"$r" | grep -E '^[0-9.]+$')$(dig +short AAAA "$dom" @"$r" | grep ':')" ]] || return 1
    done
}

# attendre_dns <id> <ipv4> <ipv6> <machine> : jusqu'à ce que chaque domaine du
# projet désigne ces adresses — ou, sous --sans-attente-dns, que l'exploitant
# l'affirme : un domaine derrière un CDN ou un répartiteur ne les désignera
# jamais, et l'attente finirait en échec, source et cible en maintenance. Cette
# affirmation est demandée même sous --oui : sans elle, un retour rouvrirait la
# source quand les domaines désignent encore la cible, et chacune recevrait sa
# part des écritures.
attendre_dns() {
    local id="$1" ip4="$2" ip6="$3" machine="$4" dom fin restants
    etape "DNS : faire pointer vers $machine"
    while IFS= read -r dom; do
        info "$dom  A $ip4${ip6:+  AAAA $ip6}${ip6:-  (supprimer tout AAAA)}"
    done < <(domaines_de "$id")
    ((EXECUTER)) || return 0
    if ((SANS_ATTENTE_DNS)); then
        avert "propagation non vérifiée (--sans-attente-dns) : à constater soi-même, vers $machine"
        confirmer "les domaines de $id désignent-ils $machine, sur tous les résolveurs ?" "$id" toujours
        return 0
    fi
    fin=$((SECONDS + DELAI_DNS))
    while :; do
        restants=()
        while IFS= read -r dom; do resolu_vers "$dom" "$ip4" "$ip6" || restants+=("$dom"); done < <(domaines_de "$id")
        ((${#restants[@]} == 0)) && { ok "propagation constatée sur ${RESOLVEURS[*]}"; return 0; }
        if ((SECONDS >= fin)); then
            erreur "DNS pas encore propagé après ${DELAI_DNS} s : ${restants[*]}"
            fatal "si un domaine ne peut pas désigner exactement $ip4${ip6:+ et $ip6} (CDN, répartiteur, plusieurs A) : constater la propagation soi-même, puis reprendre avec --sans-attente-dns"
        fi
        info "en attente : ${restants[*]}"; sleep 20
    done
}

basculer_projet() {
    local id="$1" fichier reprise=0
    charger_projet "$id"
    titre "Bascule de $id — fenêtre : $(m "$id" fenetre)"
    [[ -n "$(dernier "$id" declare)" ]] || ((EXECUTER == 0)) || fatal "$id : non déclaré"
    [[ -n "$(dernier "$id" synchro)" ]] || avert "$id : jamais pré-synchronisé — la fenêtre sera longue"
    if bascule_active "$id"; then
        info "$id : déjà basculé le $(dernier "$id" bascule)"; return 0
    fi
    if actif "$id" copie; then reprise=1; else garde_appartenance "$id"; fi
    confirmer "basculer $id (maintenance sur la source) ?" "$id"
    BASCULE_EN_COURS="$id"

    if ((reprise)); then
        # Copie finale faite et vérifiée, source figée depuis : la refaire
        # écraserait ce que la cible a pu recevoir des résolveurs déjà basculés.
        info "copie finale faite le $(dernier "$id" copie), source figée depuis : reprise à l'attente du DNS"
    else
        etape "1. Maintenance de la source"
        noter "$id" arret
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
                relancer_source "$id" || avert "$id : relance incomplète de la source, voir ci-dessus"
                source_maintenance "$id" desactiver
                noter "$id" retour
                BASCULE_EN_COURS=''
                fatal "$id : bascule abandonnée, rien n'a changé pour les visiteurs"
            fi
        fi
        noter "$id" copie
    fi

    attendre_dns "$id" "$CIBLE_IP" "$CIBLE_IP6" "la cible"

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
# Dans cet ordre : la cible cesse de recevoir des écritures (maintenance) et de
# faire tourner ses tâches, la source reprend les siennes, le DNS revient, et la
# source ne sort de maintenance qu'une fois qu'il la désigne — sans quoi, pendant
# la propagation, les deux machines écriraient chacune de leur côté. Un apply en
# échec sur la machine qu'on quitte ne bloque pas le retour : il le signale.
CIBLES_NON_ARRETEES=()
retour_projet() {
    local id="$1" fichier rc=0 arretee=1
    charger_projet "$id"
    titre "Retour arrière de $id"
    # Sans bascule engagée, il n'y a rien à défaire : relancer la source y
    # réinstallerait la crontab d'une bascule ancienne.
    etat_bascule "$id" >/dev/null || fatal "$id : aucune bascule engagée ni faite ($TRAVAIL/$id/etat) — rien à défaire"
    confirmer "remettre $id sur l'ancien serveur ?" "$id"
    RETOUR_EN_COURS="$id"
    fichier="$(fichier_projet "$id")"

    cible_maintenance "$id" activer
    etape "Cible : crons et services désactivés"
    cmd_affiche "enabled: false sur les crons et services de $fichier"
    if ((EXECUTER)) && [[ -e "$fichier" ]]; then py desactiver "$fichier" || arretee=0; fi
    faire "${OGS[@]}" apply "$id" --yes || rc=$?
    case $rc in
        0) ;;
        6) avert "ogs apply : succès partiel (code 6) — voir les avertissements ci-dessus" ;;
        *) arretee=0
           erreur "ogs apply $id --yes a échoué (code $rc) : crons et services de la cible peut-être encore actifs"
           faire "${OGS[@]}" service stop "$id" || erreur "ogs service stop $id a échoué" ;;
    esac

    relancer_source "$id" || avert "$id : relance incomplète de la source, voir ci-dessus"
    attendre_dns "$id" "$SRC_IP" "$SRC_IP6" "la source ($SRC_IP)"
    etape "Source : sortie de maintenance"
    source_maintenance "$id" desactiver
    cible_maintenance "$id" desactiver
    noter "$id" retour
    RETOUR_EN_COURS=''
    avert "les données écrites sur la cible depuis la bascule n'ont PAS été reportées sur la source"
    if ((arretee == 0)); then
        erreur "$id : la cible n'a pas été désactivée — ses crons peuvent tourner en même temps que ceux de la source :"
        erreur "en corriger la cause puis « ogs apply $id » (crons et services y sont marqués enabled: false),"
        erreur "ou retirer /etc/cron.d/ogs-$id de la cible"
        CIBLES_NON_ARRETEES+=("$id")
    fi
}

phase_retour() {
    controler "$@"
    local id
    for id in "$@"; do retour_projet "$id"; done
    ((${#CIBLES_NON_ARRETEES[@]} == 0)) || fatal "retour fait, cible non désactivée : ${CIBLES_NON_ARRETEES[*]}"
}

# =============================================================================
# PHASE etat
# =============================================================================
phase_etat() {
    titre "État de la migration"
    printf '   %-28s %-6s %-26s %-26s %-26s %s\n' PROJET VAGUE DÉCLARÉ SYNCHRONISÉ BASCULÉ RETOUR
    local id etat
    for id in "$@"; do
        printf '   %-28s %-6s %-26s %-26s %-26s %s\n' "$id" "$(m "$id" vague)" \
            "$(dernier "$id" declare)" "$(dernier "$id" synchro)" "$(dernier "$id" bascule)" "$(dernier "$id" retour)"
        # Une bascule interrompue laisse la source en maintenance, et fait refuser
        # synchroniser : elle se voit ici, pas seulement dans le journal.
        if ! bascule_active "$id" && etat="$(etat_bascule "$id")"; then
            if actif "$id" copie; then etat+=", copie finale faite le $(dernier "$id" copie)"; fi
            avert "$id : $etat — reprendre « basculer $id », ou « retour $id »"
        fi
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
