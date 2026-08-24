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
