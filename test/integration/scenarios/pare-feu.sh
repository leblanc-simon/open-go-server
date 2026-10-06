#!/bin/bash
# Éprouve le pare-feu (D72) sur un système où systemd est PID 1, avec un vrai
# ufw sur un vrai jeu de règles, et des connexions réelles : deux espaces réseau
# reliés au conteneur par des paires veth tiennent lieu de clients, l'un de
# confiance, l'autre étranger. Ce qui est éprouvé est ce que le noyau laisse
# passer, non ce que la sortie d'ufw prétend.
#
# Ce que les tests unitaires ne peuvent pas voir, et qui est éprouvé ici :
#   — ufw accepte le fichier de profils d'ogs, en-tête accentué compris, et les
#     ports détectés — sshd sur deux ports, SFTP de proftpd sur 2222, plage
#     passive du FTP ;
#   — la relecture de user.rules rend un plan vide après l'apply ;
#   — un commentaire corrigé l'est en place, sans règle en double ;
#   — un profil dont le port change entraîne ses règles (« ufw app update ») ;
#   — un refus relu après une autorisation est replacé en tête, et bloque ;
#   — une règle de l'administrateur survit, une règle d'ogs orpheline part ;
#   — la première activation se défait d'elle-même faute de confirmation, et
#     « confirm » dit ensuite qu'ufw est inactif, en code 6 ;
#   — un retour automatique perdu (un redémarrage efface son unité) laisse un
#     témoin, que status, doctor et confirm lisent ;
#   — la garde refuse d'appliquer ce qui couperait la session SSH ;
#   — un Port annoté garde son port, et des ports ssh qui fermeraient celui où
#     sshd écoute sont refusés, le --force d'« ogs apply » n'y pouvant rien —
#     un profil ssh écarté aussi, sauf si l'administrateur ouvre ssh lui-même,
#     fût-ce par une seule interface ;
#   — sshd déplacé : le plan qui suit son nouveau port passe, le port d'une
#     session où sshd n'écoute plus ne comptant pas ;
#   — deux moteurs sans web.engine : bannir ne retire pas l'accès au web ;
#   — une règle de l'administrateur tenue dans une seule famille ne reçoit pas la
#     marque d'ogs, qui pose la sienne dans l'autre famille seulement (D109) ;
#   — un bannissement refusé par la garde n'est pas écrit dans ufw.yaml (D109) ;
#   — relancer l'unité ufw dans le délai ne désactive rien ; désactivé dans le
#     délai, confirm dit qu'il n'y a rien à confirmer, code 6 (D109) ;
#   — un vrai redémarrage dans le délai vaut retour : le complément d'unité posé
#     avec le témoin laisse ufw inactif au démarrage ; confirmé, ufw repart
#     actif, et un complément resté sans témoin ne désactive rien (D109). Le
#     scénario rend alors 100, et run.sh relance le conteneur avant la phase
#     suivante.
set -uo pipefail
. /scenarios/lib.sh

echec() { echo "ÉCHEC : $*"; exit 1; }
docteur() { ogs doctor 2>&1 || true; }

# ouvert <espace> <port> : une connexion TCP aboutit-elle depuis ce client ?
ouvert() {
    local ip=10.200.0.1
    [ "$1" = dehors ] && ip=10.201.0.1
    ip netns exec "$1" nc -z -w 2 "$ip" "$2" 2>/dev/null
}
doit_passer() { ouvert "$1" "$2" || echec "$1 → $2 : connexion refusée, attendue ouverte ($3)"; }
doit_bloquer() { ! ouvert "$1" "$2" || echec "$1 → $2 : connexion ouverte, attendue bloquée ($3)"; }

# changements rend le nombre de changements du plan du pare-feu.
changements() { ogs firewall plan 2>/dev/null | sed -n 's/^\([0-9]*\) changement(s).*/\1/p'; }

temoin=/var/lib/ogs/ufw-unconfirmed
complement=/etc/systemd/system/ufw.service.d/ogs-rollback.conf

# --- Phase 2 : redémarré dans le délai d'une activation non confirmée --------
redemarre_dans_le_delai() {
    echo "=== scénario : pare-feu ufw, redémarré dans le délai ==="
    systemctl is-failed --quiet ufw && { systemctl status ufw --no-pager; echec "ufw.service ne doit pas échouer"; }
    ufw status | grep -q "Status: inactive" || { ufw status; echec "redémarré dans le délai, ufw doit être resté inactif"; }
    iptables -S ufw-user-input 2>/dev/null | grep -q 10.200.0.2 && echec "aucune règle d'ufw ne doit être chargée"
    journalctl -b -u ufw --no-pager | grep -q "jamais confirmée" ||
        { journalctl -b -u ufw --no-pager; echec "le journal d'ufw doit dire pourquoi il reste inactif"; }
    [ -f "$temoin" ] || echec "le témoin survit au redémarrage"
    systemctl is-active --quiet ogs-ufw-rollback.timer && echec "le minuteur ne survit pas au redémarrage"
    ogs firewall status | grep -q "défaite par le retour automatique" || { ogs firewall status; echec "status : retour joué"; }
    sortie=$(ogs firewall confirm 2>&1)
    code=$?
    [ "$code" = 6 ] || { echo "$sortie"; echec "confirm après le redémarrage doit rendre 6, obtenu $code"; }
    echo "$sortie" | grep -q "le retour automatique a défait" || { echo "$sortie"; echec "confirm : retour joué"; }

    [ -e /run/ogs-ufw-armed ] || echec "le complément pose la marque de /run après avoir joué"

    # Réactivée puis confirmée : plus rien ne la défera.
    ogs firewall apply --yes >/tmp/apply.log 2>&1 || { cat /tmp/apply.log; echec "firewall apply (après redémarrage)"; }
    ufw status | grep -q "Status: active" || echec "ufw doit être actif"
    # Relancer l'unité après le démarrage ne désactive rien : le complément ne
    # joue qu'au premier démarrage d'ufw qui suit celui de la machine.
    systemctl restart ufw || echec "systemctl restart ufw (après redémarrage)"
    ufw status | grep -q "Status: active" || { ufw status; echec "relancé après le démarrage, ufw doit rester actif"; }
    ogs firewall confirm | grep -q "confirmée" || echec "confirm"
    [ -e "$temoin" ] && echec "confirm doit retirer le témoin"
    [ -e "$complement" ] && echec "confirm doit retirer le complément d'unité"
    systemctl cat ufw 2>/dev/null | grep -q ogs-rollback && echec "systemd ne doit plus lire le complément"

    # Un complément resté sans témoin — confirm interrompu entre les deux
    # retraits — ne doit rien désactiver : il le prouve au prochain démarrage.
    cp /root/ogs-rollback.conf.copie "$complement"
    systemctl daemon-reload
    docteur | grep -q "resté sans témoin" || { docteur | grep pare-feu; echec "doctor : complément sans témoin"; }
    exit 100
}

# --- Phase 3 : redémarré après confirmation ----------------------------------
redemarre_apres_confirmation() {
    echo "=== scénario : pare-feu ufw, redémarré après confirmation ==="
    ufw status | grep -q "Status: active" || { ufw status; echec "confirmé, ufw doit repartir actif"; }
    iptables -S ufw-user-input | grep -q 10.200.0.2 || { iptables -S ufw-user-input; echec "les règles doivent être chargées"; }
    ogs firewall confirm >/dev/null || echec "confirm (complément sans témoin)"
    [ -e "$complement" ] && echec "confirm doit retirer un complément resté sans témoin"
    echo "✓ scénario pare-feu réussi"
    exit 0
}

case "${1:-1}" in
2) redemarre_dans_le_delai ;;
3) redemarre_apres_confirmation ;;
esac

echo "=== scénario : pare-feu ufw ==="

ogs init >/dev/null || echec "ogs init"
choisir_moteur apache

# --- La machine : services réels, deux clients ----------------------------
# sshd écoute sur deux ports : chaque directive Port s'ajoute, et un profil figé
# sur 22 laisserait 2200 fermé.
printf 'Port 22\nPort 2200\n' >/etc/ssh/sshd_config.d/essai.conf
systemctl restart ssh || echec "sshd ne démarre pas"

# proftpd : le FTP sur 21 avec sa plage passive, le SFTP sur 2222 dans son
# propre VirtualHost — la configuration que D72 vise.
grep -q '^LoadModule mod_sftp.c' /etc/proftpd/modules.conf ||
    echo 'LoadModule mod_sftp.c' >>/etc/proftpd/modules.conf
cat >/etc/proftpd/conf.d/essai.conf <<'EOF'
PassivePorts 49152 49200
<IfModule mod_sftp.c>
<VirtualHost 0.0.0.0>
  SFTPEngine on
  Port 2222
  SFTPHostKey /etc/ssh/ssh_host_ed25519_key
  SFTPHostKey /etc/ssh/ssh_host_rsa_key
</VirtualHost>
</IfModule>
EOF
systemctl restart proftpd || { journalctl -u proftpd --no-pager | tail -20; echec "proftpd ne démarre pas"; }

# Deux clients : 10.200.0.2 (le bureau, de confiance) et 10.201.0.2 (dehors).
for c in confiance:200 dehors:201; do
    nom=${c%%:*} net=${c##*:}
    ip netns add "$nom"
    ip link add "v-$nom" type veth peer name "p-$nom"
    ip link set "p-$nom" netns "$nom"
    ip addr add "10.$net.0.1/24" dev "v-$nom"
    ip link set "v-$nom" up
    ip netns exec "$nom" ip addr add "10.$net.0.2/24" dev "p-$nom"
    ip netns exec "$nom" ip link set "p-$nom" up
    ip netns exec "$nom" ip link set lo up
done
# Témoin : sans pare-feu, tout passe. Sans ce témoin, un blocage plus loin
# pourrait venir du montage et non d'ufw.
ufw --force disable >/dev/null
for p in 22 2200 2222 21 80; do
    doit_passer dehors "$p" "témoin, pare-feu inactif"
done

# --- Déclaration de départ ------------------------------------------------
ogs firewall init >/dev/null || echec "firewall init"
for motif in "sftp .*disponible .*2222/tcp" "ftp .*disponible .*21/tcp|49152:49200/tcp" \
    "ssh .*disponible .*22/tcp|2200/tcp"; do
    grep -Eq "$motif" /etc/ogs/ufw.yaml || { cat /etc/ogs/ufw.yaml; echec "détection : $motif"; }
done

cat >/etc/ogs/ufw.yaml <<'EOF'
# Pare-feu d'essai
policy:
  incoming: deny
  outgoing: allow
logging: low
rollbackAfter: 30s

sources:
  bureau:
    owner: "Agence de Nantes"
    addresses: [10.200.0.2]
  prestataire:
    owner: "ACME SARL"
    addresses: [10.201.0.0/24]

trusted: [bureau]

profiles:
  web:
    allow: [any]
  mysql:
    allow: [prestataire]

ban: []
EOF

# --- La garde : une session SSH que le pare-feu couperait ------------------
# sudo efface SSH_CONNECTION : ogs la cherche aussi chez ses parents. Ici elle
# est dans l'environnement même.
sortie=$(SSH_CONNECTION="192.0.2.50 50000 10.200.0.1 22" ogs firewall apply --yes 2>&1)
code=$?
[ "$code" = 3 ] || { echo "$sortie"; echec "la garde doit refuser (code 3), obtenu $code"; }
echo "$sortie" | grep -q "192.0.2.50" || { echo "$sortie"; echec "la garde doit nommer la session"; }
ufw status | grep -q "Status: inactive" || echec "rien ne doit avoir été appliqué"
# Dans le plan global, le refus est un avertissement : le reste du parc
# s'applique — une déclaration refusée ne doit pas priver fail2ban ni la
# rotation des journaux de leur recalcul.
sortie=$(SSH_CONNECTION="192.0.2.50 50000 10.200.0.1 22" ogs apply --yes 2>&1)
code=$?
[ "$code" = 6 ] || { echo "$sortie"; echec "apply doit rendre 6 sur un pare-feu refusé, obtenu $code"; }
echo "$sortie" | grep -q "déclaration refusée" || { echo "$sortie"; echec "apply doit dire le refus"; }
ufw status | grep -q "Status: inactive" || echec "apply ne doit rien avoir tenté sur ufw"
[ -f /etc/logrotate.d/ogs ] || echec "le reste du plan global doit avoir été appliqué"

# --- Première activation, sans confirmation : retour automatique -----------
ogs apply --yes >/tmp/apply.log 2>&1 || { cat /tmp/apply.log; echec "apply"; }
grep -q "ogs firewall confirm" /tmp/apply.log || { cat /tmp/apply.log; echec "l'apply doit dire de confirmer"; }
ufw status | grep -q "Status: active" || echec "ufw doit être actif"
systemctl is-active --quiet ogs-ufw-rollback.timer || echec "le retour automatique doit être programmé"
docteur | grep -q "n'a pas été confirmée" || echec "doctor doit signaler l'activation non confirmée"

doit_passer confiance 22 "bureau, confiance"
doit_passer confiance 2200 "bureau, second port de sshd"
doit_passer confiance 2222 "bureau, SFTP"
doit_passer dehors 80 "web ouvert à tous"
doit_bloquer dehors 22 "ssh fermé à l'étranger"
doit_bloquer dehors 2222 "SFTP fermé à l'étranger"

echo "attente du retour automatique…"
for _ in $(seq 1 45); do
    ufw status | grep -q "Status: inactive" && break
    sleep 1
done
ufw status | grep -q "Status: inactive" || echec "sans confirmation, ufw doit s'être désactivé"
doit_passer dehors 22 "après le retour automatique"
# Un « confirm » tardif ne doit pas laisser croire le pare-feu en place, ni
# rendre le code d'une confirmation.
sortie=$(ogs firewall confirm 2>&1)
code=$?
[ "$code" = 6 ] || { echo "$sortie"; echec "confirm après le retour doit rendre 6, obtenu $code"; }
echo "$sortie" | grep -q "ufw est inactif" || { echo "$sortie"; echec "confirm après le retour doit dire qu'ufw est inactif"; }
echo "$sortie" | grep -q "le retour automatique a défait" || { echo "$sortie"; echec "confirm doit dire que le retour a joué"; }
ogs firewall status | grep -q "défaite par le retour automatique" || { ogs firewall status; echec "status : retour joué"; }

# --- Seconde activation, confirmée ----------------------------------------
ogs apply --yes >/tmp/apply.log 2>&1 || { cat /tmp/apply.log; echec "apply"; }
ogs firewall confirm | grep -q "confirmée" || echec "confirm"
systemctl is-active --quiet ogs-ufw-rollback.timer && echec "le minuteur doit être arrêté"
[ -e /var/lib/ogs/ufw-unconfirmed ] && echec "confirm doit retirer le témoin de l'activation"
sleep 35
ufw status | grep -q "Status: active" || echec "confirmé, ufw doit rester actif"

# --- Retour perdu : un redémarrage dans le délai ---------------------------
# L'unité transitoire du retour vit sous /run : un redémarrage dans le délai
# l'efface, et ufw repart actif, sans filet. L'arrêter à la main en tient lieu —
# redémarrer le conteneur emporterait les espaces réseau des clients. Le témoin,
# sous /var/lib/ogs, survit : c'est lui qui le fait savoir.
ufw --force disable >/dev/null
ogs firewall apply --yes >/dev/null || echec "firewall apply (troisième activation)"
[ -f /var/lib/ogs/ufw-unconfirmed ] || echec "l'activation doit poser son témoin"
[ -f "$complement" ] || echec "l'activation doit poser le complément d'unité d'ufw"
systemctl cat ufw | grep -q "ogs-rollback.conf" || { systemctl cat ufw; echec "systemd doit lire le complément"; }
ogs firewall status | grep -q "un redémarrage d'ici là" || { ogs firewall status; echec "status : complément"; }
systemctl stop ogs-ufw-rollback.timer
ufw status | grep -q "Status: active" || echec "ufw doit rester actif jusqu'au prochain démarrage"
ogs firewall status | grep -q "retour automatique perdu" || { ogs firewall status; echec "status : retour perdu"; }
docteur | grep -q "retour automatique a disparu" || echec "doctor doit signaler le retour perdu"
ogs firewall confirm | grep -q "avait déjà disparu" || echec "confirm doit confirmer l'activation au retour perdu"
[ -e /var/lib/ogs/ufw-unconfirmed ] && echec "confirm doit retirer le témoin"
ufw status | grep -q "Status: active" || echec "confirmé, ufw reste actif"

# ufw accepte le fichier de profils d'ogs : ports détectés.
ufw app info ogs-sftp | grep -q "2222/tcp" || { ufw app info ogs-sftp; echec "profil sftp"; }
ufw app info ogs-ftp | grep -q "49152:49200/tcp" || { ufw app info ogs-ftp; echec "profil ftp"; }
ufw app info ogs-ssh | grep -q "2200/tcp" || { ufw app info ogs-ssh; echec "profil ssh"; }

# Commentaires : le propriétaire de la source y figure.
ufw status | grep "10.200.0.2" | grep -q "ogs: confiance — bureau (Agence de Nantes)" ||
    { ufw status; echec "commentaire de confiance"; }
ufw status | grep "10.201.0.0/24" | grep -q "ogs: prestataire (ACME SARL)" ||
    { ufw status; echec "commentaire d'accès"; }

# Idempotence : relire ce qu'ufw a rangé ne doit rien redemander.
n=$(changements)
[ "$n" = 0 ] || { ogs firewall plan; echec "plan non vide après apply ($n changement(s))"; }

# --- Ports de sshd : gardés comme un retrait -------------------------------
# sshd admet un commentaire en fin de ligne ; relu tel quel, il faisait retomber
# le profil au port 22 et fermait 2200.
printf 'Port 22 # annoté\nPort 2200 # second port\n' >/etc/ssh/sshd_config.d/essai.conf
sshd -t || echec "montage : sshd doit admettre le commentaire de fin de ligne"
[ "$(changements)" = 0 ] || { ogs firewall plan; echec "un Port annoté ne doit rien changer"; }
# Des ports ssh qui ferment 2200, où sshd écoute : refusé, et le --force des
# fichiers retouchés n'y peut rien. La session, arrivée sur 2200, passerait
# encore par 22 : c'est la garde des ports de sshd qui parle.
cp /etc/ogs/ufw.yaml /tmp/ufw.yaml.avant
sed -i 's#^profiles:#profiles:\n  ssh:\n    ports: [22/tcp]#' /etc/ogs/ufw.yaml
sortie=$(SSH_CONNECTION="10.200.0.2 50000 10.200.0.1 2200" ogs firewall apply --yes --force 2>&1)
code=$?
[ "$code" = 3 ] || { echo "$sortie"; echec "des ports ssh qui ferment 2200 doivent être refusés (code 3), obtenu $code"; }
echo "$sortie" | grep -q "sshd écoute sur le port 2200" || { echo "$sortie"; echec "la garde doit nommer le port de sshd"; }
sortie=$(ogs apply --yes --force 2>&1)
code=$?
[ "$code" = 6 ] || { echo "$sortie"; echec "« apply --force » : le pare-feu refusé doit rendre 6, obtenu $code"; }
ufw app info ogs-ssh | grep -q "2200/tcp" || { ufw app info ogs-ssh; echec "le --force d'apply a levé la garde"; }
doit_passer confiance 2200 "second port de sshd, gardé"
cp /tmp/ufw.yaml.avant /etc/ogs/ufw.yaml
[ "$(changements)" = 0 ] || { ogs firewall plan; echec "plan non vide après la garde"; }

# Un profil ssh écarté voit ses règles retirées : depuis la console, sans
# session à protéger, la garde des ports de sshd doit refuser quand même.
sed -i 's#^profiles:#profiles:\n  ssh:\n    available: false#' /etc/ogs/ufw.yaml
sortie=$(ogs firewall apply --yes 2>&1)
code=$?
[ "$code" = 3 ] || { echo "$sortie"; echec "un profil ssh écarté doit être refusé (code 3), obtenu $code"; }
echo "$sortie" | grep -q "sshd écoute sur le port 22, que plus rien n'ouvrirait" ||
    { echo "$sortie"; echec "la garde doit nommer le port de sshd"; }
doit_passer confiance 22 "profil ssh écarté, refusé"
# Qui ouvre ssh par ses propres règles n'est pas refusé : ogs ne les touche pas.
ufw limit 22/tcp comment 'ssh géré à la main' >/dev/null
ufw allow 2200/tcp comment 'ssh géré à la main' >/dev/null
sortie=$(ogs firewall apply --yes 2>&1) || { echo "$sortie"; echec "les règles de l'administrateur ouvrent les ports de sshd"; }
doit_passer confiance 22 "ssh ouvert par l'administrateur"
doit_passer confiance 2200 "ssh ouvert par l'administrateur"
cp /tmp/ufw.yaml.avant /etc/ogs/ufw.yaml
ogs firewall apply --yes >/dev/null || echec "firewall apply (profil ssh rétabli)"
ufw delete limit 22/tcp >/dev/null
ufw delete allow 2200/tcp >/dev/null
doit_passer confiance 22 "profil ssh rétabli"
[ "$(changements)" = 0 ] || { ogs firewall plan; echec "plan non vide, profil ssh rétabli"; }

# Qui n'ouvre ssh que par un lien — un VPN ; ici, celui du bureau — garde un
# accès : depuis la console, une règle liée à une interface ouvre le port, et la
# garde ne refuse plus. Pour une session, rien ne dit par quelle interface elle
# est arrivée : la garde refuse, et le dit ; une règle vers l'adresse du serveur
# qu'elle a jointe, elle, se vérifie.
sed -i 's#^profiles:#profiles:\n  ssh:\n    available: false#' /etc/ogs/ufw.yaml
ufw allow in on v-confiance to any port 22 proto tcp comment 'ssh par le lien du bureau' >/dev/null
ufw allow in on v-confiance to any port 2200 proto tcp comment 'ssh par le lien du bureau' >/dev/null
sortie=$(ogs firewall plan 2>&1) || { echo "$sortie"; echec "une règle liée à une interface ouvre les ports de sshd"; }
sortie=$(SSH_CONNECTION="10.200.0.2 50000 10.200.0.1 22" ogs firewall apply --yes 2>&1)
code=$?
[ "$code" = 3 ] || { echo "$sortie"; echec "une session par le lien, sans autre règle, doit être gardée (code 3), obtenu $code"; }
echo "$sortie" | grep -q "liée à une interface" || { echo "$sortie"; echec "le refus doit nommer la règle liée à une interface"; }
ufw allow proto tcp to 10.200.0.1 port 22 comment 'ssh vers le lien du bureau' >/dev/null
sortie=$(SSH_CONNECTION="10.200.0.2 50000 10.200.0.1 22" ogs firewall apply --yes 2>&1) ||
    { echo "$sortie"; echec "la règle vers l'adresse jointe par la session la couvre"; }
doit_passer confiance 22 "ssh vers l'adresse du lien"
doit_passer confiance 2200 "ssh par le lien du bureau"
doit_bloquer dehors 22 "ssh fermé hors du lien du bureau"
doit_bloquer dehors 2200 "ssh fermé hors du lien du bureau"
cp /tmp/ufw.yaml.avant /etc/ogs/ufw.yaml
ogs firewall apply --yes >/dev/null || echec "firewall apply (profil ssh rétabli, lien)"
ufw delete allow in on v-confiance to any port 22 proto tcp >/dev/null
ufw delete allow in on v-confiance to any port 2200 proto tcp >/dev/null
ufw delete allow proto tcp to 10.200.0.1 port 22 >/dev/null
doit_passer confiance 22 "profil ssh rétabli, lien"
[ "$(changements)" = 0 ] || { ogs firewall plan; echec "plan non vide, profil ssh rétabli après le lien"; }

# sshd déplacé sur 2200 seul et redémarré, la session en cours arrivée sur 22 :
# ss ne montre plus sshd sur 22. Le plan qui porte le profil sur 2200 rend
# l'accès ; la garde comptait le port de la session, et refusait la réparation.
printf 'Port 2200\n' >/etc/ssh/sshd_config.d/essai.conf
systemctl restart ssh || echec "sshd (2200 seul)"
sortie=$(SSH_CONNECTION="10.200.0.2 50000 10.200.0.1 22" ogs firewall apply --yes 2>&1) ||
    { echo "$sortie"; echec "le plan qui suit sshd sur 2200 doit passer"; }
ufw app info ogs-ssh | grep -Eq "^ *22/tcp" && { ufw app info ogs-ssh; echec "le profil ssh doit suivre sshd"; }
doit_passer confiance 2200 "sshd déplacé, profil suivi"
printf 'Port 22 # annoté\nPort 2200 # second port\n' >/etc/ssh/sshd_config.d/essai.conf
systemctl restart ssh || echec "sshd (22 et 2200)"
ogs firewall apply --yes >/dev/null || echec "firewall apply (sshd rétabli sur 22 et 2200)"
doit_passer confiance 22 "sshd rétabli sur 22"
[ "$(changements)" = 0 ] || { ogs firewall plan; echec "plan non vide, sshd rétabli"; }

# --- Commentaire corrigé en place -----------------------------------------
avant=$(ufw status numbered | grep -c "ogs:")
sed -i 's/Agence de Nantes/Agence de Nantes, 2e étage/' /etc/ogs/ufw.yaml
ogs firewall apply --yes >/dev/null || echec "firewall apply (commentaire)"
apres=$(ufw status numbered | grep -c "ogs:")
[ "$avant" = "$apres" ] || echec "un commentaire corrigé ne doit pas doubler la règle ($avant → $apres)"
ufw status | grep -q "2e étage" || { ufw status; echec "commentaire non corrigé"; }
[ "$(changements)" = 0 ] || echec "plan non vide après correction"

# --- Port changé : les règles suivent le profil ----------------------------
sed -i 's/Port 2222/Port 2223/' /etc/proftpd/conf.d/essai.conf
systemctl restart proftpd || echec "proftpd (2223)"
ogs firewall plan | grep -q "ports modifiés : ogs-sftp" || { ogs firewall plan; echec "le plan doit annoncer le port"; }
ogs firewall apply --yes >/dev/null || echec "firewall apply (port)"
doit_passer confiance 2223 "SFTP déplacé"
iptables -S ufw-user-input | grep -q "dport 2222 " && echec "l'ancien port 2222 est encore ouvert"
iptables -S ufw-user-input | grep -q "dports 2223\|dport 2223" || { iptables -S ufw-user-input; echec "2223 absent"; }

# --- Bannissement ---------------------------------------------------------
# dehors reçoit le web de tous : un refus placé en tête doit l'emporter.
sed -i 's#^  prestataire:#  prestataire-old:#; s#\[prestataire\]#[prestataire-old]#; s#10.201.0.0/24#10.201.9.0/24#' /etc/ogs/ufw.yaml
ogs firewall ban 10.201.0.2 --comment "essai de bannissement" --yes >/dev/null || echec "ban"
ufw status numbered | sed -n 's/^\[ *1\] //p' | grep -q "DENY IN.*10.201.0.2" ||
    { ufw status numbered; echec "le refus doit être la première règle"; }
doit_bloquer dehors 80 "bannie"
doit_passer confiance 80 "le bureau n'est pas banni"

# Un refus relu après une autorisation est replacé en tête.
ufw delete deny from 10.201.0.2 >/dev/null
ufw deny from 10.201.0.2 comment 'ogs: bannie — essai de bannissement' >/dev/null
ufw status numbered | sed -n 's/^\[ *1\] //p' | grep -q "10.201.0.2" && echec "montage : le refus devait être en fin"
doit_passer dehors 80 "refus placé après l'autorisation, donc inopérant"
ogs firewall plan | grep -q "replacé en tête" || { ogs firewall plan; echec "le plan doit replacer le refus"; }
ogs firewall apply --yes >/dev/null || echec "firewall apply (replacement)"
doit_bloquer dehors 80 "refus replacé en tête"

ogs firewall unban 10.201.0.2 --yes >/dev/null || echec "unban"
doit_passer dehors 80 "bannissement levé"

# Deux moteurs sans web.engine : une inconnue, non l'absence de serveur web.
# Bannir retirait l'accès public au web de tous les sites.
cp /etc/ogs/config.yaml /tmp/config.yaml.avant
sed -i '/^  engine: apache$/d' /etc/ogs/config.yaml
sortie=$(ogs firewall ban 10.201.0.3 --yes 2>&1) || { echo "$sortie"; echec "ban sans moteur retenu"; }
echo "$sortie" | grep -q "retrait" && { echo "$sortie"; echec "bannir sans moteur retenu ne doit retirer aucun accès"; }
doit_passer dehors 80 "web ouvert à tous, moteur non retenu"
ogs firewall unban 10.201.0.3 --yes >/dev/null || echec "unban sans moteur retenu"
cp /tmp/config.yaml.avant /etc/ogs/config.yaml

# --- Règle de l'administrateur, règle orpheline ---------------------------
ufw allow from 10.201.0.2 to any port 2200 proto tcp comment 'poste de secours' >/dev/null
sed -i 's#^    allow: \[prestataire-old\]#    allow: []#' /etc/ogs/ufw.yaml
ogs firewall apply --yes >/dev/null || echec "firewall apply (orpheline)"
ufw status | grep -q "10.201.9.0/24" && echec "la règle d'ogs devenue sans objet doit partir"
ufw status | grep -q "poste de secours" || echec "la règle de l'administrateur doit survivre"
doit_passer dehors 2200 "règle de l'administrateur"
[ "$(changements)" = 0 ] || echec "plan non vide en présence d'une règle étrangère"

# --- doctor ---------------------------------------------------------------
# MariaDB n'écoute que la boucle locale : une règle mysql n'ouvrirait rien.
sed -i 's#^    allow: \[\]#    allow: [10.201.0.2]#' /etc/ogs/ufw.yaml
docteur | grep -q "pare-feu : mysql.*boucle locale" || { docteur | grep pare-feu; echec "doctor : mysql en boucle locale"; }
docteur | grep -q "règle(s) de l'administrateur" || echec "doctor : règles étrangères"

# --- Désactivation déclarée -----------------------------------------------
sed -i 's/^rollbackAfter: 30s/active: false\nrollbackAfter: 30s/' /etc/ogs/ufw.yaml
ogs firewall apply --yes >/dev/null || echec "firewall apply (désactivation)"
ufw status | grep -q "Status: inactive" || echec "active: false doit désactiver ufw"

# --- Famille par famille ---------------------------------------------------
# L'administrateur ouvre le web en IPv4 seulement ; ufw.yaml le veut ouvert à
# tous. La commande commune donnait à sa règle la marque d'ogs.
sed -i '/^active: false$/d' /etc/ogs/ufw.yaml
ogs firewall apply --yes >/dev/null || echec "firewall apply (réactivation)"
ogs firewall confirm >/dev/null || echec "confirm (réactivation)"
ufw delete allow from any to any app ogs-web >/dev/null || echec "montage : retrait de l'accès au web"
ufw allow from 0.0.0.0/0 to any app ogs-web comment 'web, à moi' >/dev/null
ogs firewall apply --yes >/tmp/apply.log 2>&1 || { cat /tmp/apply.log; echec "firewall apply (famille)"; }
ufw status | grep "ogs-web" | grep -v "(v6)" | grep -q "web, à moi" ||
    { ufw status; echec "la règle IPv4 de l'administrateur doit garder son commentaire"; }
ufw status | grep "ogs-web (v6)" | grep -q "ogs: accès public à web" ||
    { ufw status; echec "la moitié IPv6 doit être posée par ogs"; }
[ "$(changements)" = 0 ] || { ogs firewall plan; echec "plan non vide, familles partagées"; }
# ufw.yaml ne demande plus le web : seule la moitié d'ogs part.
cp /etc/ogs/ufw.yaml /tmp/ufw.yaml.famille
python3 - <<'PY'
import re
p = "/etc/ogs/ufw.yaml"
s = open(p).read()
s = re.sub(r"^  web:\n    allow: \[any\]\n", "", s, flags=re.M)
open(p, "w").write(s)
PY
ogs firewall apply --yes >/tmp/apply.log 2>&1 || { cat /tmp/apply.log; echec "firewall apply (web retiré)"; }
ufw status | grep -q "web, à moi" || { ufw status; echec "la règle de l'administrateur doit survivre au retrait"; }
ufw status | grep "ogs-web (v6)" | grep -q "ogs:" && { ufw status; echec "la moitié d'ogs doit être retirée"; }
ufw delete allow from 0.0.0.0/0 to any app ogs-web >/dev/null
cp /tmp/ufw.yaml.famille /etc/ogs/ufw.yaml
ogs firewall apply --yes >/dev/null || echec "firewall apply (web rétabli)"
[ "$(changements)" = 0 ] || { ogs firewall plan; echec "plan non vide, web rétabli"; }

# --- Bannissement refusé : ufw.yaml intact ----------------------------------
# La session vient de 10.202.0.5 : bannir son réseau la couperait. Refusé, le
# bannissement ne doit pas rester dans ufw.yaml, où le prochain apply le
# retrouverait.
cp /etc/ogs/ufw.yaml /tmp/ufw.yaml.ban
sortie=$(SSH_CONNECTION="10.202.0.5 50000 10.200.0.1 22" ogs firewall ban 10.202.0.0/24 --yes 2>&1)
code=$?
[ "$code" = 3 ] || { echo "$sortie"; echec "bannir la session doit être refusé (code 3), obtenu $code"; }
echo "$sortie" | grep -q "bannirait" || { echo "$sortie"; echec "c'est la garde qui doit refuser"; }
cmp -s /etc/ogs/ufw.yaml /tmp/ufw.yaml.ban || { diff /tmp/ufw.yaml.ban /etc/ogs/ufw.yaml; echec "un bannissement refusé ne doit pas être écrit"; }

# --- Redémarrage dans le délai : un vrai ------------------------------------
# Les phases suivantes passent par un redémarrage du conteneur : /run repart
# vide, l'unité transitoire du retour avec lui. Le délai est long, pour que
# seul le complément d'unité puisse laisser ufw inactif au démarrage.
systemctl enable ufw >/dev/null 2>&1
systemctl is-enabled --quiet ufw || echec "montage : ufw.service doit démarrer avec la machine"
sed -i 's/^rollbackAfter: 30s/rollbackAfter: 10m/' /etc/ogs/ufw.yaml
ufw --force disable >/dev/null
ogs firewall apply --yes >/tmp/apply.log 2>&1 || { cat /tmp/apply.log; echec "firewall apply (avant redémarrage)"; }
[ -f "$temoin" ] && [ -f "$complement" ] || echec "l'activation doit poser témoin et complément"
docteur | grep -q "prochain démarrage" || { docteur | grep pare-feu; echec "doctor : un redémarrage d'ici là"; }
iptables -S ufw-user-input | grep -q 10.200.0.2 || echec "montage : règles chargées avant le redémarrage"
[ -e /run/ogs-ufw-armed ] || echec "l'armement doit poser la marque de /run"

# Relancer l'unité dans le délai n'est pas redémarrer la machine : ExecStartPre
# tourne à chaque démarrage de l'unité, et la v1 du complément désactivait ufw
# — que status disait encore « en attente » et que confirm déclarait confirmé,
# code 0, ufw inactif (D109).
systemctl restart ufw || echec "systemctl restart ufw (dans le délai)"
systemctl is-failed --quiet ufw && echec "ufw.service ne doit pas échouer à la relance"
grep -q '^ENABLED=yes' /etc/ufw/ufw.conf || { cat /etc/ufw/ufw.conf; echec "relancé dans le délai, ufw.conf doit garder ENABLED=yes"; }
ufw status | grep -q "Status: active" || { ufw status; echec "relancé dans le délai, ufw doit rester actif"; }
iptables -S ufw-user-input | grep -q 10.200.0.2 || echec "relancé dans le délai, les règles doivent être rechargées"
ogs firewall status | grep -q "un redémarrage d'ici là" || { ogs firewall status; echec "status : toujours en attente"; }

# Désactivé dans le délai, il n'y a rien à confirmer : confirm le dit, code 6,
# et status aussi.
ufw --force disable >/dev/null
ogs firewall status | grep -q "désactivé dans le délai" || { ogs firewall status; echec "status : désactivé dans le délai"; }
sortie=$(ogs firewall confirm 2>&1)
code=$?
[ "$code" = 6 ] || { echo "$sortie"; echec "confirm, ufw inactif dans le délai : code 6 attendu, obtenu $code"; }
echo "$sortie" | grep -q "rien n'est confirmé" || { echo "$sortie"; echec "confirm doit dire que rien n'est confirmé"; }
[ -f "$temoin" ] || echec "confirm sans effet : le témoin reste"
docteur | grep -q "désactivé dans le délai" || { docteur | grep pare-feu; echec "doctor : désactivé dans le délai"; }
ogs firewall apply --yes >/tmp/apply.log 2>&1 || { cat /tmp/apply.log; echec "firewall apply (réarmement)"; }
ufw status | grep -q "Status: active" || echec "ufw doit être réactivé"
[ -f "$temoin" ] && [ -f "$complement" ] && [ -e /run/ogs-ufw-armed ] || echec "le réarmement pose témoin, complément et marque"

# --- Un projet « ufw » ne touche pas au retour automatique (D114) ------------
# Le motif ogs-<id>-* d'un projet « ufw » retenait ogs-ufw-rollback.timer :
# « service stop » et « project delete » désarmaient le retour du pare-feu.
# L'identifiant est refusé à la création ; un projet déclaré avant la règle est
# reconstitué à la main.
systemctl is-active --quiet ogs-ufw-rollback.timer || echec "montage : le retour automatique doit être armé"
sortie=$(ogs project create ufw.test --id ufw 2>&1); code=$?
[ "$code" = 3 ] && echo "$sortie" | grep -q "réservé" || { echo "$sortie"; echec "l'identifiant ufw doit être refusé à la création, code $code"; }
ogs project create ufw.test --kind static >/dev/null || echec "project create ufw.test"
sed 's/ufwtest/ufw/g' /etc/ogs/projects/ufwtest.yaml >/etc/ogs/projects/ufw.yaml
rm /etc/ogs/projects/ufwtest.yaml
# Sans courriel de contact, le certificat échoue : code 6, sans rapport ici.
ogs apply ufw --yes >/tmp/apply-ufw.log 2>&1
code=$?
[ "$code" = 0 ] || [ "$code" = 6 ] || { cat /tmp/apply-ufw.log; echec "apply du projet ufw, code $code"; }
ogs service stop ufw >/dev/null 2>&1
systemctl is-active --quiet ogs-ufw-rollback.timer || echec "« service stop ufw » a désarmé le retour automatique"
sortie=$(ogs project delete ufw --no-dump --yes 2>&1)
code=$?
[ "$code" = 0 ] || [ "$code" = 6 ] || { echo "$sortie" | tail -8; echec "project delete ufw, code $code"; }
getent passwd ufw >/dev/null && { echo "$sortie" | tail -8; echec "le compte ufw a survécu à la suppression"; }
systemctl is-active --quiet ogs-ufw-rollback.timer || { echo "$sortie" | tail -8; echec "« project delete ufw » a désarmé le retour automatique"; }
echo "un projet « ufw » laisse le retour automatique du pare-feu armé"

cp "$complement" /root/ogs-rollback.conf.copie
sync
echo "redémarrage dans le délai…"
exit 100
