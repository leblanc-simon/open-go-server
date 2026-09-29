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

echo
echo "RÉSULTAT : les hooks s'exécutent avec le bon contexte, un « pre » refuse,"
echo "           et un script détournable est écarté."
