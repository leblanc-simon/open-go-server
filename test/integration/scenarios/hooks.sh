#!/bin/bash
# Éprouve les hooks sur un système réel : portée serveur, portée projet, refus
# d'un hook « pre », et refus des scripts qu'un utilisateur pourrait détourner.
set -uo pipefail
. /scenarios/lib.sh

echo "=== scénario : hooks ==="
ogs init --quiet
choisir_moteur apache
mkdir -p /etc/ogs/hooks
trace=/tmp/trace-hooks

# Hook de parc : sa seule présence vaut déclaration.
cat > /etc/ogs/hooks/post-create <<'HOOK'
#!/bin/sh
echo "parc postCreate $OGS_PROJECT $OGS_USER $OGS_KIND $OGS_ROOT" >> /tmp/trace-hooks
# Le contexte complet arrive aussi en JSON sur l'entrée standard.
cat >> /tmp/trace-hooks.json
HOOK
chmod 0750 /etc/ogs/hooks/post-create

ogs site create hooks.test --kind static --tls=false --yes --quiet >/dev/null 2>&1
echo "--- trace du hook de parc ---"
cat $trace
if ! grep -q "parc postCreate hookstest hookstest static /home/hooks.test" $trace; then
    echo "ÉCHEC : le hook de parc n'a pas reçu le bon contexte"
    exit 1
fi
if ! grep -q '"host":"hooks.test"' /tmp/trace-hooks.json; then
    echo "ÉCHEC : le contexte JSON n'a pas été transmis"
    exit 1
fi

# Aucun hook en --dry-run (D40), et rien de supprimé : « project delete » ne sait
# pas simuler, il refuse l'option (code 2) avant tout geste. Il supprimait le
# projet pour de bon, preDelete et postDelete lancés en root (R3-20).
echo "--- project delete --dry-run ---"
for ev in pre-delete post-delete; do
    printf '#!/bin/sh\necho "parc %s $OGS_PROJECT" >> /tmp/trace-hooks\n' "$ev" > /etc/ogs/hooks/$ev
    chmod 0750 /etc/ogs/hooks/$ev
done
sortie=$(ogs project delete hookstest --dry-run --yes 2>&1)
code=$?
if [ "$code" -ne 2 ]; then
    echo "ÉCHEC : « project delete --dry-run » doit refuser l'option (code 2), obtenu $code :"
    echo "$sortie" | tail -3
    exit 1
fi
if [ ! -d /home/hooks.test ] || ! getent passwd hookstest >/dev/null || [ ! -f /etc/ogs/projects/hookstest.yaml ]; then
    echo "ÉCHEC : « project delete --dry-run » a démonté le site"
    exit 1
fi
if grep -q "delete" $trace; then
    echo "ÉCHEC : un hook a été lancé sous --dry-run :"
    grep "delete" $trace
    exit 1
fi
rm -f /etc/ogs/hooks/post-delete
echo "$sortie" | tail -1

# Un hook « pre » qui refuse doit arrêter l'opération.
cat > /etc/ogs/hooks/pre-delete <<'HOOK'
#!/bin/sh
echo "refus de supprimer $OGS_PROJECT" >&2
exit 1
HOOK
chmod 0750 /etc/ogs/hooks/pre-delete

echo "--- un hook « pre » qui refuse ---"
if ogs project delete hookstest --yes >/dev/null 2>&1; then
    echo "ÉCHEC : la suppression a eu lieu malgré le refus du hook"
    exit 1
fi
if [ ! -d /home/hooks.test ]; then
    echo "ÉCHEC : le site a été démonté malgré le refus"
    exit 1
fi
echo "la suppression a bien été refusée, le site est intact"

# --no-hooks doit permettre de passer outre.
echo "--- --no-hooks passe outre ---"
if ! ogs project delete hookstest --yes --no-hooks >/dev/null 2>&1; then
    echo "ÉCHEC : --no-hooks n'a pas permis la suppression"
    exit 1
fi
echo "suppression effectuée avec --no-hooks"

# Un script inscriptible par tous ne doit pas être exécuté : il s'exécuterait en root.
echo "--- refus d'un script inscriptible par tous ---"
cat > /etc/ogs/hooks/pre-create <<'HOOK'
#!/bin/sh
echo "ce hook ne doit jamais s'exécuter" >> /tmp/trace-hooks
HOOK
chmod 0777 /etc/ogs/hooks/pre-create
sortie=$(ogs site create danger.test --kind static --tls=false --yes 2>&1)
if grep -q "ne doit jamais" $trace; then
    echo "ÉCHEC : un script inscriptible par tous a été exécuté"
    exit 1
fi
if ! echo "$sortie" | grep -qi "inscriptible\|écriture"; then
    echo "ÉCHEC : le refus n'a pas été expliqué :"
    echo "$sortie" | tail -3
    exit 1
fi
echo "$sortie" | grep -i "inscriptible\|écriture" | head -1

rm -f /etc/ogs/hooks/pre-create

# D117 : un lien qui sort du répertoire des hooks reste admis vers un script de
# root dans une chaîne de root, et doctor le signale ; vers un répertoire qu'un
# autre compte possède, il est refusé — ce compte choisirait le script exécuté
# en root.
echo "--- lien sortant vers un script sûr : admis et signalé ---"
install -o root -g root -m 0750 /dev/null /usr/local/sbin/ogs-notifier
printf '#!/bin/sh\necho "notifier $OGS_PROJECT" >> /tmp/trace-hooks\n' > /usr/local/sbin/ogs-notifier
ln -sf /usr/local/sbin/ogs-notifier /etc/ogs/hooks/post-create
: > $trace
ogs site create lien.test --kind static --tls=false --yes >/dev/null 2>&1
if ! grep -q "notifier lientest" $trace; then
    echo "ÉCHEC : le hook lié vers un script sûr n'a pas été exécuté"
    exit 1
fi
diagnostic=$(ogs doctor 2>&1)
if ! echo "$diagnostic" | grep -q "hors de /etc/ogs/hooks"; then
    echo "ÉCHEC : doctor ne signale pas le lien sortant"
    exit 1
fi
echo "lien sûr exécuté, et signalé par doctor"

echo "--- lien sortant vers un répertoire d'un autre compte : refusé ---"
id intrus >/dev/null 2>&1 || useradd -m intrus
install -o intrus -g intrus -m 0755 -d /home/intrus/bin
install -o root -g root -m 0750 /dev/null /home/intrus/bin/notifier
printf '#!/bin/sh\necho "ce hook ne doit jamais s exécuter" >> /tmp/trace-hooks\n' > /home/intrus/bin/notifier
ln -sf /home/intrus/bin/notifier /etc/ogs/hooks/post-create
: > $trace
sortie=$(ogs site create intrus.test --kind static --tls=false --yes 2>&1)
if grep -q "ne doit jamais" $trace; then
    echo "ÉCHEC : un script dont un autre compte contrôle le répertoire a été exécuté"
    exit 1
fi
if ! echo "$sortie" | grep -q "peut être modifié par un autre compte"; then
    echo "ÉCHEC : le refus n'est pas expliqué :"
    echo "$sortie" | tail -3
    exit 1
fi
echo "$sortie" | grep -o "le répertoire /home/intrus/bin[^:]*" | head -1
diagnostic=$(ogs doctor 2>&1)
if ! echo "$diagnostic" | grep -q "refusé à l'exécution"; then
    echo "ÉCHEC : doctor ne signale pas le hook qui sera refusé"
    exit 1
fi

echo "--- lien vers un répertoire root:staff 2775 (Debian mise à niveau) : refusé, remède nommé ---"
install -d -o root -g staff -m 2775 /srv/outils
printf '#!/bin/sh\necho "ce hook ne doit jamais s exécuter" >> /tmp/trace-hooks\n' > /srv/outils/notif
chmod 0750 /srv/outils/notif
ln -sf /srv/outils/notif /etc/ogs/hooks/post-create
diagnostic=$(ogs doctor 2>&1)
if ! echo "$diagnostic" | grep -q "staff-group-for-usr-local"; then
    echo "ÉCHEC : doctor ne nomme pas le remède du groupe staff"
    echo "$diagnostic" | grep -i hook | head -3
    exit 1
fi
echo "groupe staff nommé par doctor avec son remède"

echo "--- un répartiteur lié sous deux noms voit le nom de l'événement ---"
install -o root -g root -m 0750 /dev/null /etc/ogs/hooks/repartiteur
printf '#!/bin/sh\necho "evenement $(basename "$0")" >> /tmp/trace-hooks\n' > /etc/ogs/hooks/repartiteur
ln -sfn repartiteur /etc/ogs/hooks/post-create
ln -sfn repartiteur /etc/ogs/hooks/pre-create
: > $trace
ogs site create trois.test --kind static --tls=false --yes >/dev/null 2>&1
if ! grep -q "evenement post-create" $trace || ! grep -q "evenement pre-create" $trace; then
    echo "ÉCHEC : le répartiteur ne voit pas le nom désigné :"
    cat $trace
    exit 1
fi
echo "répartiteur : $(sort -u $trace | tr '\n' ' ')"
rm -f /etc/ogs/hooks/post-create /etc/ogs/hooks/pre-create

# D112 : le plan du parc a ses hooks à lui, pre-apply-fleet et post-apply-fleet.
# Une archive expirée lui donne une purge à faire ; un pre-apply-fleet qui
# refuse l'arrête.
echo "--- pre-apply-fleet garde le plan du parc ---"
cat > /etc/ogs/hooks/pre-apply-fleet <<'HOOK'
#!/bin/sh
echo "plan du parc refusé" >&2
exit 1
HOOK
chmod 0750 /etc/ogs/hooks/pre-apply-fleet
mkdir -p /var/lib/ogs/archives
vieille=/var/lib/ogs/archives/ancien-20200101-0000.tar.zst
echo "archive expirée" > $vieille
touch -d '3 years ago' $vieille
sortie=$(ogs apply --yes 2>&1)
code=$?
if [ "$code" -eq 0 ] || [ ! -f $vieille ]; then
    echo "ÉCHEC : le plan du parc est passé outre le refus de pre-apply-fleet (code $code) :"
    echo "$sortie" | tail -5
    exit 1
fi
if ! grep -q "purge de 1 archive" <<<"$sortie"; then
    echo "ÉCHEC : le plan du parc n'a pas été montré avant le hook :"
    echo "$sortie" | tail -5
    exit 1
fi
rm -f /etc/ogs/hooks/pre-apply-fleet
if ! ogs apply --yes >/dev/null 2>&1 || [ -f $vieille ]; then
    echo "ÉCHEC : la purge confirmée n'a pas eu lieu"
    exit 1
fi
echo "le refus de pre-apply-fleet a gardé le plan du parc, levé il le laisse passer"

# Relecture de D112 : un post-apply écrit pour un projet n'est jamais appelé sans
# projet. Appelé avec OGS_ROOT vide, il vidait /var/cache de la machine.
echo "--- post-apply n'est appelé que pour un projet ---"
mkdir -p /var/cache/temoin-exploitant
echo x > /var/cache/temoin-exploitant/f
cat > /etc/ogs/hooks/post-apply <<'HOOK'
#!/bin/sh
echo "post-apply projet=[$OGS_PROJECT]" >> /tmp/trace-apply
rm -rf "$OGS_ROOT"/var/cache/*
HOOK
chmod 0750 /etc/ogs/hooks/post-apply
ogs site create neuf.test --kind static --tls=false --yes --quiet >/dev/null 2>&1
rm -f /etc/ogs/hooks/post-apply
if [ ! -f /var/cache/temoin-exploitant/f ] || grep -q "projet=\[\]" /tmp/trace-apply; then
    echo "ÉCHEC : post-apply appelé sans projet :"
    cat /tmp/trace-apply
    exit 1
fi
echo "post-apply appelé pour le seul projet : $(cat /tmp/trace-apply)"

# Relecture de D112 : une création refusée avant le premier geste — par
# pre-apply, ou à la confirmation — ne laisse pas sa déclaration.
echo "--- une création refusée ne laisse pas sa déclaration ---"
cat > /etc/ogs/hooks/pre-apply <<'HOOK'
#!/bin/sh
echo "refusé" >&2
exit 1
HOOK
chmod 0750 /etc/ogs/hooks/pre-apply
ogs site create refus1.test --kind static --tls=false --yes >/dev/null 2>&1
rm -f /etc/ogs/hooks/pre-apply
echo n | ogs site create refus2.test --kind static --tls=false >/dev/null 2>&1
for id in refus1test refus2test; do
    if [ -f /etc/ogs/projects/$id.yaml ]; then
        echo "ÉCHEC : la déclaration de $id, création refusée, est restée"
        exit 1
    fi
done
sortie=$(ogs apply neuftest --yes 2>&1)
code=$?
if [ "$code" -ne 0 ]; then
    echo "ÉCHEC : l'apply d'un voisin après deux créations refusées rend $code :"
    echo "$sortie" | tail -3
    exit 1
fi
echo "les créations refusées n'ont rien laissé au parc"

# D112 : postCreate au premier apply réussi d'un projet que « project create » a
# déclaré — le cycle documenté —, et une seule fois.
echo "--- postCreate du cycle « project create » puis « apply » ---"
cat > /etc/ogs/hooks/post-create <<'HOOK'
#!/bin/sh
echo "parc postCreate $OGS_PROJECT" >> /tmp/trace-hooks
HOOK
chmod 0750 /etc/ogs/hooks/post-create
ogs project create cycle.test --kind static --quiet >/dev/null 2>&1
if grep -q "parc postCreate cycletest" $trace; then
    echo "ÉCHEC : postCreate lancé avant tout apply"
    exit 1
fi
ogs apply cycletest --yes --quiet >/dev/null 2>&1
ogs apply cycletest --yes --quiet >/dev/null 2>&1
n=$(grep -c "parc postCreate cycletest" $trace)
if [ "$n" != 1 ]; then
    echo "ÉCHEC : postCreate attendu une fois au premier apply, obtenu $n"
    exit 1
fi
echo "postCreate lancé une fois, au premier apply"

echo
echo "RÉSULTAT : les hooks s'exécutent avec le bon contexte, un « pre » refuse,"
echo "           un script détournable est écarté, pre-apply-fleet garde le plan du parc,"
echo "           post-apply n'est jamais appelé sans projet, une création refusée ne reste pas"
echo "           et postCreate suit le premier apply."
