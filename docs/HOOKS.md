# Hooks

Un hook est un script exécuté par `ogs` à un moment déterminé du cycle de vie. Il sert à raccorder l'outil
au reste de l'exploitation : prévenir une supervision, enregistrer un domaine dans un service externe,
recharger un frontal, refuser une opération.

## Deux portées

Un hook est reconnu de deux manières :

- **portée serveur** : un fichier de `/etc/ogs/hooks` portant le nom de l'événement — `postCreate`, ou son
  orthographe équivalente `post-create`. Sa seule présence vaut déclaration ; il s'applique à tous les
  projets ;
- **portée projet** : un script nommé dans `spec.hooks` de la déclaration, qui **remplace** celui du parc
  pour cet événement.

## Emplacement et sûreté

Les hooks vivent dans `/etc/ogs/hooks`. Un script n'est exécuté que s'il satisfait toutes ces conditions :

- il se trouve sous `/etc/ogs/hooks` (les liens symboliques qui en sortent sont refusés) ;
- il est exécutable ;
- il n'est pas accessible en écriture à tous ;
- il appartient à root, quand `ogs` tourne en root.

Ces règles ne sont pas de la précaution décorative : un hook s'exécute avec les privilèges de l'outil, donc
en root. Un script modifiable par un utilisateur de projet serait une élévation de privilèges immédiate.

## Événements

| Événement | Moment | Critique |
|---|---|---|
| `preCreate` | avant la création d'un projet | oui |
| `postCreate` | après la création complète | non |
| `preDelete` | avant la suppression, services encore actifs | oui |
| `postDelete` | après suppression et archivage | non |
| `preApply` | avant l'exécution d'un plan, après confirmation | oui |
| `postApply` | après l'exécution | non |
| `postCertRenew` | après un renouvellement de certificat | non |
| `postPhpSwitch` | après un changement de version PHP | non |

Un hook **critique** qui échoue interrompt l'opération : c'est le principe même d'un `pre-` — il garde la
porte. Un hook non critique qui échoue produit un avertissement ; l'opération est déjà faite, la défaire
pour un script de notification serait pire que l'inverse.

## Contexte transmis

Le contexte arrive de deux façons, pour n'imposer aucun langage :

- sur l'entrée standard, un objet JSON complet ;
- dans l'environnement : `OGS_EVENT`, `OGS_PROJECT`, `OGS_HOST`, `OGS_ENV`, `OGS_KIND`, `OGS_USER`,
  `OGS_PHP`, `OGS_ROOT` (le home du projet), `OGS_DOCROOT`, et `OGS_RESOURCES` — les ressources touchées,
  une par ligne. Les valeurs propres à un événement deviennent des `OGS_<CLÉ>` : `postDelete` fournit
  ainsi `OGS_ARCHIVE`.

```sh
#!/bin/sh
# /etc/ogs/hooks/post-create — déclarer tout nouveau site à la supervision.
set -e
curl -fsS -X POST https://supervision.interne/sites \
    -d "host=$OGS_HOST" -d "user=$OGS_USER" >/dev/null
```

```sh
#!/bin/sh
# /etc/ogs/hooks/pre-delete — refuser la suppression d'un site de production
# non archivé ailleurs. Un code de retour non nul arrête l'opération.
set -e
[ "$OGS_ENV" = "prod" ] || exit 0
if ! /usr/local/bin/sauvegarde-existe "$OGS_PROJECT"; then
    echo "aucune sauvegarde externe pour $OGS_PROJECT" >&2
    exit 1
fi
```

Le JSON complet, plus riche que l'environnement, se lit ainsi :

```sh
#!/bin/sh
contexte=$(cat)
echo "$contexte" | jq -r '.resources[]?' | logger -t ogs
```

## Hooks propres à un projet

La déclaration d'un projet peut nommer ses propres scripts, dans `spec.hooks` ; ils prennent le pas sur
ceux du parc. Ils sont soumis aux mêmes règles d'emplacement : le chemin est résolu sous `/etc/ogs/hooks`,
un projet ne peut donc pas faire exécuter un script de son home — ce serait offrir root à son utilisateur.

```yaml
spec:
  hooks:
    postApply: deploiement-shop.sh
```

## Délais et mise au point

Un hook dispose de cinq minutes ; au-delà, il est tué avec son groupe de processus — un script qui laisse
des enfants derrière lui ne bloque pas l'outil. Sa sortie est journalisée.

```sh
ogs apply shopfr --no-hooks     # appliquer sans exécuter les hooks
ogs apply shopfr -v             # afficher la sortie des hooks
```

En `--dry-run` et avec `ogs plan`, **aucun hook n'est exécuté** : un calcul de plan ne doit rien déclencher
au dehors. Le corollaire mérite d'être connu — le refus d'un hook `pre-` ne se constate qu'à l'`apply`, un
plan calculé n'est donc pas la garantie qu'il sera autorisé.
