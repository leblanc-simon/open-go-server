#!/bin/bash
# Éprouve le provider PostgreSQL sur un vrai cluster : rôle, base, durcissement
# du schéma public, extensions, isolation entre deux projets, sélection de
# cluster, rotation, dump et suppression.
set -uo pipefail
. /scenarios/lib.sh

version=$(ls /etc/postgresql | head -1)
echo "=== scénario : PostgreSQL $version ==="

pg_ctlcluster "$version" main start
# Un second cluster, sur un autre port : la sélection par « --cluster » est une
# fonctionnalité annoncée, elle doit être éprouvée. Il est créé sans locale, ce
# qui reproduit un cluster dont la base modèle n'est pas en UTF8 — un cas qui
# doit fonctionner.
pg_createcluster "$version" second -p 5433 --start >/dev/null 2>&1
sleep 2
pg_lsclusters | tail -3

# L'authentification « peer » exige un rôle au nom de l'utilisateur système.
# C'est le prérequis documenté d'une installation, pas un contournement.
for cluster in main second; do
    port=5432
    [ "$cluster" = "second" ] && port=5433
    su - postgres -c "createuser -p $port --superuser root" 2>/dev/null
done

ogs init --quiet
choisir_moteur apache

echo
echo "--- ce que doctor voit des clusters ---"
ogs doctor 2>&1 | grep -i "cluster postgres" || echo "(aucun contrôle de cluster rapporté)"

echo
echo "--- création d'un site avec sa base ---"
ogs site create shop.fr --kind static --db postgres --tls=false --yes 2>&1 |
    grep -Ei "postgres|base|⚠|✗" | head -6

# Interrogation d'administration, par le compte postgres du système.
pg() { su - postgres -c "psql -p ${2:-5432} -tAc \"$1\"" ; }
if ! pg "SELECT datname FROM pg_database WHERE datname='shopfr'" | grep -q shopfr; then
    echo "ÉCHEC : la base n'a pas été créée"
    exit 1
fi
echo "base : $(pg "SELECT datname || ' propriétaire ' || pg_get_userbyid(datdba) FROM pg_database WHERE datname='shopfr'")"
echo "rôle : $(pg "SELECT rolname || ' login=' || rolcanlogin::text || ' superuser=' || rolsuper::text FROM pg_roles WHERE rolname='shopfr'")"
if pg "SELECT rolsuper FROM pg_roles WHERE rolname='shopfr'" | grep -q t; then
    echo "ÉCHEC : le rôle du projet est superutilisateur"
    exit 1
fi

echo
echo "--- durcissement du schéma public ---"
proprietaire=$(su - postgres -c "psql -tAc \"SELECT nspowner::regrole FROM pg_namespace WHERE nspname='public'\" shopfr")
echo "propriétaire du schéma public : $proprietaire"
if [ "$proprietaire" != "shopfr" ]; then
    echo "ÉCHEC : le schéma public n'appartient pas au projet"
    exit 1
fi
if su - postgres -c "psql -tAc \"SELECT has_schema_privilege('public','public','CREATE')\" shopfr" | grep -q t; then
    echo "ÉCHEC : PUBLIC peut encore créer dans le schéma public"
    exit 1
fi
echo "PUBLIC n'a plus aucun droit sur le schéma public"

echo
echo "--- l'application peut-elle travailler dans sa base ? ---"
motdepasse=$(ogs secrets show shopfr postgres/shopfr 2>/dev/null | tail -1)
if [ -z "$motdepasse" ]; then
    echo "ÉCHEC : mot de passe introuvable dans le keystore"
    ogs secrets list shopfr
    exit 1
fi
export PGPASSWORD="$motdepasse"
nouveau_ou_initial="$motdepasse"
if ! psql -h 127.0.0.1 -U shopfr -d shopfr \
    -c "CREATE TABLE essai (id int); INSERT INTO essai VALUES (1); SELECT count(*) FROM essai;" 2>&1 | tail -3; then
    echo "ÉCHEC : l'utilisateur ne peut pas travailler dans sa base"
    exit 1
fi

echo
echo "--- extension déclarée ---"
ogs db add shopfr --role stats --engine postgres --extension pg_trgm >/dev/null
ogs apply shopfr --yes 2>&1 | grep -Ei "postgres|extension" | head -4
exts=$(su - postgres -c "psql -tAc \"SELECT extname FROM pg_extension ORDER BY extname\" shopfr_stats")
echo "extensions de shopfr_stats : $(echo "$exts" | tr '\n' ' ')"
if ! echo "$exts" | grep -q pg_trgm; then
    echo "ÉCHEC : l'extension déclarée n'est pas installée"
    exit 1
fi

echo
echo "--- isolation entre deux projets ---"
# PostgreSQL accorde CONNECT à PUBLIC sur toute base nouvelle : sans révocation,
# le rôle d'un projet peut se connecter à la base d'un autre et y lire le
# catalogue — noms de tables et de colonnes comprises.
ogs site create autre.fr --kind static --db postgres --tls=false --yes -q >/dev/null 2>&1
voisin=$(ogs secrets show autrefr postgres/autrefr | tail -1)
if PGPASSWORD="$voisin" psql -h 127.0.0.1 -U autrefr -d shopfr -c "SELECT 1" >/dev/null 2>&1; then
    echo "ÉCHEC : le rôle autrefr se connecte à la base shopfr — CONNECT n'a pas été"
    echo "        révoqué à PUBLIC, et le voisin peut lire du catalogue"
    exit 1
fi
echo "le rôle d'un projet ne peut pas se connecter à la base d'un autre"
# Et le propriétaire, lui, doit continuer à entrer chez lui.
if ! PGPASSWORD="$nouveau_ou_initial" psql -h 127.0.0.1 -U shopfr -d shopfr -c "SELECT 1" >/dev/null 2>&1; then
    echo "ÉCHEC : la révocation a aussi fermé la base à son propriétaire"
    exit 1
fi
echo "le propriétaire garde son accès"

echo
echo "--- compte additionnel en lecture seule ---"
ogs db user add shopfr lecture --privileges readonly >/dev/null
ogs apply shopfr --yes 2>&1 | grep -Ei "lecture" | head -3
lecturepass=$(ogs secrets show shopfr postgres/lecture 2>/dev/null | tail -1)
if [ -z "$lecturepass" ]; then
    echo "ÉCHEC : le compte additionnel n'a pas de mot de passe au keystore"
    ogs secrets list shopfr
    exit 1
fi
if ! PGPASSWORD="$lecturepass" psql -h 127.0.0.1 -U lecture -d shopfr -tAc "SELECT count(*) FROM essai" >/dev/null 2>&1; then
    echo "ÉCHEC : le compte de lecture ne peut pas lire la table"
    exit 1
fi
if PGPASSWORD="$lecturepass" psql -h 127.0.0.1 -U lecture -d shopfr -c "INSERT INTO essai VALUES (2)" >/dev/null 2>&1; then
    echo "ÉCHEC : un compte « readonly » a pu écrire"
    exit 1
fi
echo "le compte lecture lit mais n'écrit pas"
# Les tables créées ensuite doivent l'être avec les mêmes droits.
PGPASSWORD="$nouveau_ou_initial" psql -h 127.0.0.1 -U shopfr -d shopfr \
    -c "CREATE TABLE apres (id int)" >/dev/null
if ! PGPASSWORD="$lecturepass" psql -h 127.0.0.1 -U lecture -d shopfr -c "SELECT * FROM apres" >/dev/null 2>&1; then
    echo "ÉCHEC : le compte de lecture est aveugle aux tables créées après lui"
    exit 1
fi
echo "et il voit les tables créées après son ajout (privilèges par défaut)"

echo
echo "--- un rôle additionnel retiré de la déclaration part au prochain apply (D100) ---"
ogs db user add shopfr ancien --privileges readonly >/dev/null
ogs apply shopfr --yes >/dev/null 2>&1
if ! pg "SELECT rolname FROM pg_roles WHERE rolname='ancien'" | grep -q ancien; then
    echo "ÉCHEC : le rôle ancien n'a pas été créé"
    exit 1
fi
sed -i '/- user: ancien$/,/privileges:/d' /etc/ogs/projects/shopfr.yaml
if grep -q ancien /etc/ogs/projects/shopfr.yaml; then
    echo "ÉCHEC : le rôle ancien est encore déclaré"
    exit 1
fi
ogs plan shopfr 2>&1 | grep -E "ancien" | head -3
if ! ogs apply shopfr --yes >/tmp/ancien.log 2>&1; then
    echo "ÉCHEC : l'apply qui retire le rôle a échoué"
    cat /tmp/ancien.log
    exit 1
fi
# Ses droits et privilèges par défaut vivaient dans la base : DROP ROLE les
# refuserait sans la réattribution et le DROP OWNED qui le précèdent.
if pg "SELECT rolname FROM pg_roles WHERE rolname='ancien'" | grep -q ancien; then
    echo "ÉCHEC : le rôle retiré de la déclaration subsiste"
    cat /tmp/ancien.log
    exit 1
fi
if ogs secrets list shopfr 2>/dev/null | grep -q "postgres/ancien"; then
    echo "ÉCHEC : le mot de passe du rôle retiré est resté au keystore"
    exit 1
fi
if ! PGPASSWORD="$lecturepass" psql -h 127.0.0.1 -U lecture -d shopfr -tAc "SELECT count(*) FROM essai" >/dev/null 2>&1; then
    echo "ÉCHEC : le compte lecture, toujours déclaré, a perdu l'accès"
    exit 1
fi
echo "le rôle ancien est retiré avec son mot de passe ; lecture et les données restent"

echo
echo "--- sélection explicite d'un cluster ---"
ogs db add shopfr --role archive --engine postgres --cluster "$version/second" >/dev/null
ogs apply shopfr --yes 2>&1 | grep -Ei "archive" | head -3
if ! pg "SELECT datname FROM pg_database WHERE datname='shopfr_archive'" 5433 | grep -q shopfr_archive; then
    echo "ÉCHEC : la base n'a pas été créée sur le cluster demandé"
    pg "SELECT datname FROM pg_database" 5433
    exit 1
fi
if pg "SELECT datname FROM pg_database WHERE datname='shopfr_archive'" | grep -q shopfr_archive; then
    echo "ÉCHEC : la base a été créée sur le cluster par défaut"
    exit 1
fi
echo "shopfr_archive est bien sur $version/second, et absente du cluster main"

echo
echo "--- idempotence ---"
sortie=$(ogs diff shopfr 2>&1)
echo "$sortie" | tail -3
if echo "$sortie" | grep -qiE "\+ base|~ base|\+ rôle"; then
    echo "ÉCHEC : écart signalé alors que rien n'a changé"
    exit 1
fi

echo
echo "--- rotation du mot de passe ---"
ancien=$motdepasse
ogs db user rotate shopfr shopfr --yes 2>&1 | tail -1
nouveau=$(ogs secrets show shopfr postgres/shopfr | tail -1)
nouveau_ou_initial="$nouveau"
if [ "$nouveau" = "$ancien" ]; then
    echo "ÉCHEC : le mot de passe n'a pas changé"
    exit 1
fi
if PGPASSWORD="$ancien" psql -h 127.0.0.1 -U shopfr -d shopfr -c "SELECT 1" >/dev/null 2>&1; then
    echo "ÉCHEC : l'ancien mot de passe fonctionne encore"
    exit 1
fi
if ! PGPASSWORD="$nouveau" psql -h 127.0.0.1 -U shopfr -d shopfr -c "SELECT 1" >/dev/null 2>&1; then
    echo "ÉCHEC : le nouveau mot de passe ne fonctionne pas"
    exit 1
fi
echo "l'ancien mot de passe est révoqué, le nouveau fonctionne"

echo
echo "--- dump et restauration ---"
ogs db dump shopfr shopfr 2>&1 | tail -1
dump=$(ls -t /var/lib/ogs/dumps/*.sql | head -1)
if ! grep -q "essai" "$dump"; then
    echo "ÉCHEC : le dump ne contient pas la table créée"
    head -20 "$dump"
    exit 1
fi
# La sauvegarde est un script SQL : la base doit être remise à plat avant, sinon
# la recréation des tables échoue sur ce qui existe déjà.
PGPASSWORD="$nouveau" psql -h 127.0.0.1 -U shopfr -d shopfr \
    -c "DROP TABLE essai; DROP TABLE apres" >/dev/null
ogs db restore shopfr shopfr --input "$dump" --yes 2>&1 | tail -1
lignes=$(PGPASSWORD="$nouveau" psql -h 127.0.0.1 -U shopfr -d shopfr -tAc "SELECT count(*) FROM essai" 2>/dev/null)
echo "lignes retrouvées après restauration : ${lignes:-aucune}"
if [ "${lignes:-0}" != "1" ]; then
    echo "ÉCHEC : la restauration n'a pas rétabli la table"
    exit 1
fi

echo
echo "--- restauration : sous le rôle du projet, jamais sous root (D92) ---"
rm -f /tmp/restore-identite
printf '%s\n' '\! id -un > /tmp/restore-identite' 'CREATE ROLE pirate SUPERUSER;' > /root/hostile.sql
if ogs db restore shopfr shopfr --input /root/hostile.sql --yes >/tmp/restore.log 2>&1; then
    echo "ÉCHEC : une restauration qui crée un rôle superutilisateur doit échouer"
    exit 1
fi
if pg "SELECT rolname FROM pg_roles WHERE rolname='pirate'" | grep -q pirate; then
    echo "ÉCHEC : la restauration a créé un rôle superutilisateur"
    exit 1
fi
identite=$(cat /tmp/restore-identite 2>/dev/null)
if [ "$identite" != "shopfr" ]; then
    echo "ÉCHEC : « \\! » s'exécute sous « $identite », non sous l'utilisateur du projet"
    exit 1
fi
echo "rôle superutilisateur refusé, commande shell sous $identite"
rm -f /root/hostile.sql /tmp/restore-identite
# Une base à extension, sous un rôle qui n'est pas l'utilisateur Linux : le
# commentaire de l'extension, qu'un non-propriétaire ne peut poser, est écarté.
ogs db dump shopfr shopfr_stats -O /root/stats.sql >/dev/null 2>&1 || { echo "ÉCHEC : dump de shopfr_stats"; exit 1; }
grep -c "COMMENT ON EXTENSION" /root/stats.sql | sed 's/^/commentaires d’extension dans la sauvegarde : /'
if ! ogs db restore shopfr shopfr_stats --input /root/stats.sql --yes >/tmp/restore.log 2>&1; then
    cat /tmp/restore.log
    echo "ÉCHEC : la restauration d'une base à extension échoue sous son rôle"
    exit 1
fi
rm -f /root/stats.sql
echo "base à extension restaurée sous le rôle shopfr_stats"

echo
echo "--- mot de passe haché en md5 : la restauration renvoie vers la rotation (D92) ---"
# Un cluster mis à niveau : pg_hba en md5, mot de passe haché en md5. Le serveur
# demande alors un échange md5, que PGREQUIREAUTH refuse ; la rotation repose le
# mot de passe en SCRAM, que le serveur mène même sous « md5 ».
hba=$(ls /etc/postgresql/"$version"/main/pg_hba.conf)
cp "$hba" /root/pg_hba.orig
sed -i 's/scram-sha-256$/md5/' "$hba"
pg_ctlcluster "$version" main reload
actuel=$(ogs secrets show shopfr postgres/shopfr | tail -1)
su - postgres -c "psql -q -c \"SET password_encryption = 'md5'; ALTER ROLE shopfr PASSWORD '$actuel'\"" 2>/dev/null
if ogs db restore shopfr shopfr --input "$dump" --yes >/tmp/restore.log 2>&1; then
    echo "ÉCHEC : un échange md5 ne doit pas recevoir le mot de passe"
    exit 1
fi
if ! grep -q "encore haché en md5.*ogs db user rotate shopfr shopfr" /tmp/restore.log; then
    cat /tmp/restore.log
    echo "ÉCHEC : le refus doit nommer la rotation"
    exit 1
fi
ogs db user rotate shopfr shopfr --yes >/dev/null 2>&1
hachage=$(pg "SELECT left(rolpassword, 13) FROM pg_authid WHERE rolname='shopfr'")
echo "hachage après rotation : $hachage"
PGPASSWORD="$(ogs secrets show shopfr postgres/shopfr | tail -1)" psql -h 127.0.0.1 -U shopfr -d shopfr \
    -c "DROP TABLE IF EXISTS essai; DROP TABLE IF EXISTS apres" >/dev/null 2>&1
if ! ogs db restore shopfr shopfr --input "$dump" --yes >/tmp/restore.log 2>&1; then
    cat /tmp/restore.log
    echo "ÉCHEC : après rotation, la restauration doit passer"
    exit 1
fi
cp /root/pg_hba.orig "$hba"
pg_ctlcluster "$version" main reload
echo "md5 refusé avec la sortie nommée ; après rotation ($hachage), restauration réussie"

echo
echo "--- cluster arrêté : son port n'obtient aucun mot de passe (D92) ---"
# Le port d'un cluster arrêté est à quiconque s'y lie. Un faux serveur, sous
# nobody, note toute connexion : la restauration ne doit pas lui parler.
pg_ctlcluster "$version" main stop
rm -f /tmp/faux-pg
cat > /tmp/faux-pg.py <<'PY'
import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", 5432))
s.listen(1)
s.settimeout(20)
try:
    c, _ = s.accept()
    open("/tmp/faux-pg", "w").write("connexion\n")
    c.close()
except socket.timeout:
    pass
PY
su -s /bin/sh nobody -c "python3 /tmp/faux-pg.py" >/dev/null 2>&1 &
faux=$!
sleep 1
if ogs db restore shopfr shopfr --input "$dump" --yes >/tmp/restore.log 2>&1; then
    cat /tmp/restore.log
    echo "ÉCHEC : une restauration vers un cluster arrêté doit être refusée"
    exit 1
fi
tail -1 /tmp/restore.log
if [ -e /tmp/faux-pg ]; then
    echo "ÉCHEC : la restauration s'est connectée au faux serveur"
    exit 1
fi
kill "$faux" 2>/dev/null; wait "$faux" 2>/dev/null
rm -f /tmp/faux-pg.py
pg_ctlcluster "$version" main start
sleep 2
echo "cluster arrêté : restauration refusée, le faux serveur n'a rien reçu"

echo
echo "--- dump et restauration : aucun lien posé par le projet n'est suivi ---"
# Le home appartient à l'utilisateur (D51). Un lien à la place de la destination
# faisait tronquer puis réécrire par root le fichier de son choix ; un lien à la
# place de l'entrée faisait charger dans sa base un fichier que root seul lit —
# ici la sauvegarde qui vient d'être faite.
printf 'témoin\n' > /root/temoin-dump
su -s /bin/bash shopfr -c 'ln -sfn /root/temoin-dump ~/sauvegarde.sql'
if ogs db dump shopfr shopfr -O /home/shop.fr/sauvegarde.sql >/tmp/dump-lien.log 2>&1; then
    cat /tmp/dump-lien.log
    echo "ÉCHEC : une sauvegarde à travers un lien du home doit être refusée"
    exit 1
fi
if [ "$(cat /root/temoin-dump)" != "témoin" ]; then
    echo "ÉCHEC : root a réécrit la cible du lien posé par le projet"
    exit 1
fi
su -s /bin/bash shopfr -c "ln -sfn $dump ~/a-restaurer.sql"
if ogs db restore shopfr shopfr --input /home/shop.fr/a-restaurer.sql --yes >/tmp/restore-lien.log 2>&1; then
    cat /tmp/restore-lien.log
    echo "ÉCHEC : une restauration à travers un lien du home doit être refusée"
    exit 1
fi
su -s /bin/bash shopfr -c 'rm -f ~/sauvegarde.sql ~/a-restaurer.sql'
rm -f /root/temoin-dump
# Un lien que seul root a pu poser reste suivi : /var/run vers /run.
if [ -L /var/run ]; then
    ogs db dump shopfr shopfr -O /var/run/ogs-essai-dump.sql >/dev/null 2>&1 ||
        { echo "ÉCHEC : un lien posé par root (/var/run) doit être suivi"; exit 1; }
    [ -s /run/ogs-essai-dump.sql ] || { echo "ÉCHEC : la sauvegarde n'est pas au bout de /var/run"; exit 1; }
    rm -f /run/ogs-essai-dump.sql
fi
echo "liens du projet refusés, lien de root suivi"

echo
echo "--- suppression ---"
ogs project delete shopfr --yes >/dev/null 2>&1
restantes=$(pg "SELECT datname FROM pg_database WHERE datname LIKE 'shopfr%'")
if [ -n "$restantes" ]; then
    echo "ÉCHEC : bases subsistantes : $restantes"
    exit 1
fi
# Tous les rôles du projet, comptes additionnels compris : PostgreSQL refuse de
# supprimer un rôle dont des objets ou des privilèges dépendent encore.
restants=$(pg "SELECT rolname FROM pg_roles WHERE rolname IN ('shopfr','shopfr_stats','shopfr_archive','lecture')")
if [ -n "$restants" ]; then
    echo "ÉCHEC : rôles subsistants : $(echo "$restants" | tr '\n' ' ')"
    exit 1
fi
# La base du second cluster doit partir elle aussi.
if pg "SELECT datname FROM pg_database WHERE datname='shopfr_archive'" 5433 | grep -q shopfr_archive; then
    echo "ÉCHEC : la base du second cluster subsiste"
    exit 1
fi
echo "bases et rôles supprimés sur les deux clusters ; le projet voisin est intact :"
pg "SELECT datname FROM pg_database WHERE datname='autrefr'"

echo
echo "RÉSULTAT : le provider PostgreSQL fonctionne sur un vrai cluster — schéma"
echo "           public durci, extensions, multi-cluster, isolation entre projets."
