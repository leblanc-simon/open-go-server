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
#   — la première activation se défait d'elle-même faute de confirmation ;
#   — la garde refuse d'appliquer ce qui couperait la session SSH.
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

# --- Seconde activation, confirmée ----------------------------------------
ogs apply --yes >/tmp/apply.log 2>&1 || { cat /tmp/apply.log; echec "apply"; }
ogs firewall confirm | grep -q "confirmée" || echec "confirm"
systemctl is-active --quiet ogs-ufw-rollback.timer && echec "le minuteur doit être arrêté"
sleep 35
ufw status | grep -q "Status: active" || echec "confirmé, ufw doit rester actif"

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

echo "✓ scénario pare-feu réussi"
