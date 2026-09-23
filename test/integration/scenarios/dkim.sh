#!/bin/bash
# Éprouve la signature DKIM (D69) sur un système où systemd est PID 1, avec un
# vrai OpenDKIM branché sur un vrai Postfix, et un résolveur local qui tient lieu
# de zone DNS publiée.
#
# Ce que les tests unitaires ne peuvent pas voir, et qui est éprouvé ici :
#   — OpenDKIM accepte les tables d'ogs, en-tête de commentaires compris, et
#     signe réellement le courrier sortant avec la clé qu'ogs a scellée ;
#   — l'enregistrement émis par « records » vérifie la signature produite ;
#   — la vérification de « doctor » et d'« activate » passe par un vrai
#     résolveur, qui rend un enregistrement découpé en plusieurs chaînes ;
#   — une clé trop ouverte n'est pas ignorée mais **refusée** : le courrier reste
#     en file, ce que « doctor » dit et que l'apply répare ;
#   — la rotation ne bascule qu'une fois l'enregistrement publié ;
#   — les tables portent tout le parc, et un rechargement sur une configuration
#     invalide n'arrête pas le démon.
set -uo pipefail
. /scenarios/lib.sh

echec() { echo "ÉCHEC : $*"; exit 1; }

# docteur rend la sortie de « doctor » sans son code : il vaut 1 dès qu'un
# contrôle échoue — ici, les droits de /etc que le scénario ne durcit pas —, et
# sous « pipefail » ce code ferait échouer tout « ogs doctor | grep -q ».
docteur() { ogs doctor 2>&1 || true; }

echo "=== scénario : signature DKIM ==="

# --- Ce qui revient à l'administrateur, non à ogs --------------------------
# ogs ne touche ni au MTA ni à opendkim.conf (D69) : ce qui suit est la part de
# l'exploitant. OpenDKIM écoute en TCP local, Postfix — chrooté — n'atteignant
# pas le socket Unix de la configuration Debian.
sed -i 's#^Socket.*#Socket\t\t\tinet:8891@localhost#' /etc/opendkim.conf
postconf -e "myhostname = essai.local" "mydestination = essai.local, localhost" \
    "inet_interfaces = loopback-only" \
    "smtpd_milters = inet:localhost:8891" "non_smtpd_milters = inet:localhost:8891" \
    "milter_default_action = tempfail"
useradd -m dest 2>/dev/null
systemctl restart opendkim postfix || echec "OpenDKIM ou Postfix ne démarre pas"

# Un résolveur local tient lieu de zone publiée : « publier » y pose les
# enregistrements, comme l'exploitant le ferait chez le registrar du client.
# « local=/fr/ » fait répondre « inexistant » pour tout le reste, sans quoi un
# nom non publié passerait pour un résolveur muet.
publier() {
    python3 - "$@" <<'PY' >/etc/dnsmasq-essai.conf || echec "enregistrements illisibles"
import json, subprocess, sys
print("local=/fr/")
for projet in sys.argv[1:]:
    sortie = subprocess.run(["ogs", "--output", "json", "mail", "dkim", "records", projet],
                            capture_output=True, text=True, check=True).stdout
    for r in json.loads(sortie)["items"]:
        if r["state"] in ("active", "pending"):
            chaines = ",".join('"%s"' % s for s in r["strings"])
            print("txt-record=%s,%s" % (r["name"].rstrip("."), chaines))
PY
    pkill -x dnsmasq 2>/dev/null
    sleep 0.5
    dnsmasq --conf-file=/etc/dnsmasq-essai.conf --no-resolv --no-hosts --bind-interfaces \
        --listen-address=127.0.0.1 --port=53 || echec "dnsmasq ne démarre pas"
    echo "nameserver 127.0.0.1" >/etc/resolv.conf
}

# envoyer expédie un message et attend sa livraison ; il rend « signé »,
# « non signé » ou « en file ».
envoyer() {
    # Vidée, jamais créée : Postfix livre sous l'identité du destinataire, et une
    # boîte créée ici au nom de root lui serait interdite — le message resterait
    # en file, et passerait pour retenu par OpenDKIM.
    [ -f /var/mail/dest ] && : >/var/mail/dest
    printf 'From: %s\nTo: dest@localhost\nSubject: %s\n\ncorps du message\n' "$1" "$2" |
        sendmail -f "$1" dest@localhost
    for _ in $(seq 1 15); do
        if [ -s /var/mail/dest ]; then
            if grep -q '^DKIM-Signature:' /var/mail/dest; then echo signé; else echo "non signé"; fi
            return
        fi
        sleep 1
    done
    echo "en file"
}

# verifier contrôle la signature du dernier message livré avec l'enregistrement
# qu'ogs a émis : c'est la propriété qui compte, la clé publiée vérifie ce que la
# clé scellée a signé.
verifier() {
    local projet=$1 selecteur=$2
    ogs --output json mail dkim records "$projet" >/tmp/records.json
    python3 - "$selecteur" <<'PY' || echec "la signature ne se vérifie pas avec l'enregistrement émis"
import json, mailbox, re, sys
import dkim
selecteur = sys.argv[1]
records = {r["name"].rstrip("."): r["value"] for r in json.load(open("/tmp/records.json"))["items"]}
message = list(mailbox.mbox("/var/mail/dest"))[-1].as_bytes()
signature = re.search(rb"s=([a-z0-9]+);", message)
if not signature or signature.group(1).decode() != selecteur:
    print("sélecteur attendu %s, obtenu %s" % (selecteur, signature and signature.group(1)))
    sys.exit(1)
def dns(nom, timeout=5):
    nom = nom.decode() if isinstance(nom, bytes) else nom
    return records.get(nom.rstrip("."), "").encode()
if not dkim.verify(message, dnsfunc=dns):
    print("dkim.verify a refusé la signature")
    sys.exit(1)
print("  signature vérifiée (s=%s)" % selecteur)
PY
}

ogs init --quiet
choisir_moteur apache

echo
echo "--- un projet qui demande la signature ---"
ogs site create shop.fr --kind static --tls=false --yes >/dev/null 2>&1 || echec "création du site"
ogs mail dkim enable shopfr >/dev/null || echec "« mail dkim enable »"
ogs apply shopfr --yes 2>&1 | grep -Ei "dkim|opendkim|⚠|✗"
cle=$(ls /etc/opendkim/keys/shopfr/shop.fr/*.private 2>/dev/null | head -1)
[ -n "$cle" ] || echec "aucune clé écrite sous /etc/opendkim/keys/shopfr/shop.fr"
selecteur=$(basename "$cle" .private)
droits=$(stat -c '%a %U:%G' "$cle")
echo "  $cle : $droits"
[ "$droits" = "600 opendkim:opendkim" ] || echec "clé en 0600 au compte d'OpenDKIM attendue, obtenu $droits"
if su -s /bin/sh shopfr -c "cat $cle" >/dev/null 2>&1; then
    echec "l'utilisateur du projet lit la clé privée de son domaine"
fi
[ -f /etc/opendkim/ogs/KeyTable ] && [ -f /etc/opendkim/ogs/SigningTable ] || echec "tables absentes"

echo
echo "--- tant qu'opendkim.conf ne désigne pas les tables, doctor le dit ---"
sortie=$(docteur)
echo "$sortie" | grep -i "configuration d'OpenDKIM" | head -2
echo "$sortie" | grep -i "configuration d'OpenDKIM" | grep -q "rien n'est signé" ||
    echec "doctor ne signale pas un opendkim.conf qui ignore les tables d'ogs"
[ "$(envoyer contact@shop.fr avant-tables)" = "non signé" ] || echec "courrier signé sans les tables ?"

# L'exploitant suit la correction proposée.
cat >>/etc/opendkim.conf <<'EOF'
KeyTable		/etc/opendkim/ogs/KeyTable
SigningTable		refile:/etc/opendkim/ogs/SigningTable
EOF
opendkim -n -x /etc/opendkim.conf || echec "OpenDKIM refuse les tables d'ogs"
systemctl restart opendkim || echec "OpenDKIM ne redémarre pas sur les tables d'ogs"
docteur | grep -i "configuration d'OpenDKIM" | grep -q "désigne les tables" ||
    echec "doctor n'accepte pas la configuration corrigée"

echo
echo "--- le courrier sortant est signé, et l'enregistrement émis le vérifie ---"
etat=$(envoyer contact@shop.fr premier)
echo "  premier message : $etat"
[ "$etat" = signé ] || echec "courrier non signé ($etat)"
verifier shopfr "$selecteur"

echo
echo "--- doctor lit l'enregistrement publié, découpé en plusieurs chaînes ---"
docteur | grep -i "enregistrement DKIM de shop.fr" | head -1
docteur | grep -i "enregistrement DKIM de shop.fr" | grep -q "aucun enregistrement" ||
    echec "un enregistrement non publié doit être signalé absent"
publier shopfr
grep -c '","' /etc/dnsmasq-essai.conf >/dev/null || echec "l'enregistrement n'est pas découpé"
docteur | grep -i "enregistrement DKIM de shop.fr" | grep -q "conforme" ||
    echec "doctor ne reconnaît pas l'enregistrement publié"

echo
echo "--- une clé trop ouverte n'est pas ignorée : le courrier reste en file ---"
chmod 0644 "$cle"
etat=$(envoyer contact@shop.fr cle-ouverte)
echo "  clé en 0644 : $etat"
[ "$etat" = "en file" ] || echec "une clé refusée par RequireSafeKeys devait retenir le courrier ($etat)"
docteur | grep -i "droits des clés DKIM" | grep -q "0644" || echec "doctor ne voit pas la clé ouverte"
ogs apply shopfr --yes 2>&1 | grep -i "dkim" | head -2
[ "$(stat -c %a "$cle")" = 600 ] || echec "l'apply n'a pas resserré la clé"
postqueue -f
for _ in $(seq 1 15); do [ -s /var/mail/dest ] && break; sleep 1; done
grep -q '^DKIM-Signature:' /var/mail/dest || echec "le courrier retenu n'est pas reparti signé"
echo "  courrier retenu reparti signé après correction"

echo
echo "--- un fichier disparu est réécrit, jamais régénéré ---"
empreinte=$(sha256sum "$cle" | cut -d' ' -f1)
rm -f "$cle"
ogs apply shopfr --yes 2>&1 | grep -i "réécrite" | head -1
[ "$(sha256sum "$cle" 2>/dev/null | cut -d' ' -f1)" = "$empreinte" ] ||
    echec "la clé réécrite diffère : une clé neuve exigerait de republier chez le client"

echo
echo "--- rotation : rien ne bascule avant la publication ---"
ogs mail dkim rotate shopfr 2>&1 | grep -i "préparée" | head -1
nouveau=$(ogs --output json mail dkim show shopfr | python3 -c \
    'import json,sys; print([k["selector"] for k in json.load(sys.stdin)["items"] if k["state"]=="pending"][0])')
[ -n "$nouveau" ] && [ "$nouveau" != "$selecteur" ] || echec "aucune clé préparée"
grep -q "$nouveau" /etc/opendkim/ogs/SigningTable && echec "la clé préparée est désignée avant la bascule"
ogs mail dkim activate shopfr >/tmp/activate.log 2>&1
code=$?
[ "$code" = 5 ] || { cat /tmp/activate.log; echec "bascule sans publication : code 5 attendu, obtenu $code"; }
echo "  bascule refusée tant que $nouveau n'est pas publiée (code 5)"
publier shopfr
docteur | grep -i "rotation DKIM" | grep -q "préparée et publiée" ||
    echec "doctor ne voit pas la clé préparée publiée"
ogs mail dkim activate shopfr --yes 2>&1 | grep -Ei "conforme|basculée|⚠|✗"
grep -q "$nouveau._domainkey.shop.fr" /etc/opendkim/ogs/SigningTable || echec "la SigningTable ne désigne pas $nouveau"
[ "$(systemctl is-active opendkim)" = active ] || echec "OpenDKIM ne s'est pas relevé du rechargement"
[ -f "$cle" ] || echec "la clé remplacée doit rester en place pendant la rétention"
[ "$(envoyer contact@shop.fr apres-rotation)" = signé ] || echec "courrier non signé après la bascule"
verifier shopfr "$nouveau"

echo
echo "--- les tables portent tout le parc ---"
ogs site create blog.fr --kind static --tls=false --yes >/dev/null 2>&1
ogs mail dkim enable blogfr >/dev/null
ogs apply blogfr --yes >/dev/null 2>&1
for domaine in shop.fr blog.fr; do
    grep -q "^\*@$domaine " /etc/opendkim/ogs/SigningTable ||
        echec "« apply blogfr » a retiré $domaine de la SigningTable"
done
[ "$(envoyer contact@blog.fr voisin)" = signé ] || echec "le voisin n'est pas signé"
echo "  shop.fr et blog.fr signés, après un apply ne visant que blogfr"

echo
echo "--- une table retouchée à la main est refusée, et --force la sauvegarde ---"
echo "*@intrus.fr x._domainkey.intrus.fr" >>/etc/opendkim/ogs/SigningTable
ogs mail dkim disable blogfr >/dev/null
ogs apply blogfr --yes >/tmp/apply.log 2>&1
code=$?
[ "$code" = 6 ] || { cat /tmp/apply.log; echec "table retouchée : code 6 attendu, obtenu $code"; }
grep -q "intrus.fr" /etc/opendkim/ogs/SigningTable || echec "la retouche a été écrasée sans --force"
ogs apply blogfr --yes --force >/dev/null 2>&1
grep -q "intrus.fr\|blog.fr" /etc/opendkim/ogs/SigningTable && echec "--force n'a pas recalculé la table"
ls /var/lib/ogs/backups/SigningTable.ogs-bak-* >/dev/null 2>&1 || echec "aucune sauvegarde de la table retouchée"
echo "  refus, puis écrasement forcé avec sauvegarde"

echo
echo "--- un rechargement sur une configuration invalide n'arrête pas le démon ---"
cp /etc/opendkim.conf /tmp/opendkim.conf.bon
sed -i 's#^SigningTable.*#SigningTable\t\trefile:/nexiste/pas#' /etc/opendkim.conf
ogs mail dkim enable blogfr >/dev/null
ogs apply blogfr --yes >/tmp/apply.log 2>&1
grep -q "configuration d'OpenDKIM refusée" /tmp/apply.log || { cat /tmp/apply.log; echec "rechargement non validé"; }
[ "$(systemctl is-active opendkim)" = active ] || echec "OpenDKIM est tombé sur une configuration invalide"
echo "  rechargement refusé, démon toujours actif"
cp /tmp/opendkim.conf.bon /etc/opendkim.conf
systemctl restart opendkim

echo
echo "--- la préproduction ne signe pas au nom de la production ---"
ogs project create shop.fr --kind static --env prep >/dev/null
ogs mail dkim enable shopfrprep >/tmp/prep.log 2>&1
code=$?
[ "$code" = 3 ] || { cat /tmp/prep.log; echec "préproduction : code 3 attendu, obtenu $code"; }
echo "  refusé (code 3)"

echo
echo "--- la suppression retire les clés et nomme l'enregistrement à retirer ---"
ogs project delete shopfr --yes >/tmp/delete.log 2>&1
grep "enregistrement DKIM à retirer" /tmp/delete.log | head -2
grep -q "$nouveau._domainkey.shop.fr" /tmp/delete.log || echec "l'enregistrement à retirer n'est pas nommé"
[ -d /etc/opendkim/keys/shopfr ] && echec "les clés du projet supprimé sont restées"
grep -q "shop.fr" /etc/opendkim/ogs/SigningTable && echec "la SigningTable désigne encore le projet supprimé"
grep -q "^\*@blog.fr " /etc/opendkim/ogs/SigningTable || echec "la suppression a retiré le voisin"
[ "$(systemctl is-active opendkim)" = active ] || echec "OpenDKIM est tombé à la suppression"
[ "$(envoyer contact@blog.fr apres-suppression)" = signé ] || echec "le voisin n'est plus signé"
echo "  clés retirées, voisin toujours signé"

echo
echo "=== scénario DKIM : succès ==="
