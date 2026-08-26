#!/bin/sh
# Engendre le changelog Debian du paquet, sur la sortie standard, à partir des
# étiquettes du dépôt.
#
# Un paquet natif doit porter « /usr/share/doc/<paquet>/changelog.gz » ; lintian
# en fait une erreur, et à raison : c'est ce que lit un administrateur avant
# d'appliquer une mise à jour sur un parc en production. Le journal est dérivé
# des étiquettes plutôt que tenu à la main — une seule source de vérité, la même
# que celle dont le Makefile tire le numéro de version.
set -eu

paquet=ogs
version="${1:?version attendue en premier argument}"
mainteneur="${OGS_MAINTAINER:-$(sed -n 's/^maintainer: *//p' nfpm.yaml)}"

if [ -z "$mainteneur" ]; then
    echo "changelog.sh : mainteneur introuvable dans nfpm.yaml" >&2
    exit 1
fi

racine=$(git rev-list --max-parents=0 HEAD | tail -1)

# Une entrée : en-tête, sujets des commits de la plage, ligne de signature.
# La date est celle du commit portant l'étiquette, au format RFC 2822 qu'impose
# la politique Debian ; « %cD » n'est pas localisé, la locale est sans effet.
entree() {
    _version="$1"
    _reference="$2"
    _plage="$3"

    printf '%s (%s) unstable; urgency=medium\n\n' "$paquet" "$_version"
    # Les commits de fusion n'apportent pas de sujet lisible.
    git log --no-merges --format='  * %s' "$_plage"
    printf '\n -- %s  %s\n' "$mainteneur" "$(git log -1 --format=%cD "$_reference")"
}

# Les étiquettes accessibles depuis HEAD : une construction sur une branche ne
# doit pas annoncer des versions qu'elle ne contient pas. On les parcourt de la
# plus ancienne à la plus récente — chaque entrée a besoin de celle qui la
# précède pour délimiter sa plage de commits — et on empile le résultat à
# l'envers, le changelog Debian allant du plus récent au plus ancien.
etiquettes=$(git tag --merged HEAD --sort=creatordate 2>/dev/null || true)

if [ -z "$etiquettes" ]; then
    # Dépôt sans étiquette : le Makefile retombe sur 0.0.0, le journal aussi.
    entree "$version" HEAD "$racine..HEAD"
    exit 0
fi

derniere=$(printf '%s\n' "$etiquettes" | tail -1)
journal=
precedente="$racine"
for etiquette in $etiquettes; do
    # Le numéro de l'entrée de tête est celui que le Makefile donne au paquet :
    # les deux sont tirés des mêmes étiquettes, mais un désaccord ferait un
    # paquet dont la version ne figure pas dans son propre journal.
    if [ "$etiquette" = "$derniere" ]; then
        numero="$version"
    else
        numero=$(printf '%s' "$etiquette" | sed 's/^v//')
    fi

    journal="$(entree "$numero" "$etiquette" "$precedente..$etiquette")

$journal"
    precedente="$etiquette"
done

printf '%s\n' "$journal"
