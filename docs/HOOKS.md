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

- il est désigné sous `/etc/ogs/hooks` ;
- chaque répertoire traversé pour l'atteindre — depuis « / », liens compris, et jusqu'au fichier réel qu'un
  lien désigne — appartient à root et n'est inscriptible ni par son groupe ni par les autres ;
- le fichier réel est exécutable, n'est inscriptible que par son propriétaire, et appartient à root, quand
  `ogs` tourne en root.

Un lien symbolique qui sort de `/etc/ogs/hooks` est **admis** s'il satisfait ces conditions — des parcs en
dépendent — mais `ogs doctor` le signale : copier le script dans le répertoire est préférable. Un lien vers
un répertoire qu'un autre compte que root peut modifier (le home d'un projet, `/tmp`) est refusé, avec le
remède dans le message : ce compte choisirait le script exécuté en root. `ogs doctor` éprouve chaque hook
comme son exécution l'éprouverait. Sur une Debian mise à niveau depuis la 10 ou avant, `/usr/local` et ses
sous-répertoires peuvent être restés en `root:staff` 2775 tant que `/etc/staff-group-for-usr-local` existe :
un hook lié vers `/usr/local/…` y est refusé. Remède : retirer ce fichier, puis
`chgrp root <répertoire> && chmod g-w,g-s <répertoire>` — ou copier le script sous `/etc/ogs/hooks`.

Le hook est lancé par le nom sous lequel il est désigné, non par le chemin réel de sa cible : un même script
lié sous plusieurs noms d'événement peut lire l'événement dans `basename "$0"`.

Ces règles ne sont pas de la précaution décorative : un hook s'exécute avec les privilèges de l'outil, donc
en root. Un script modifiable par un utilisateur de projet serait une élévation de privilèges immédiate.

## Événements

| Événement | Moment | Critique |
|---|---|---|
| `preCreate` | avant le dépôt de la déclaration d'un projet neuf : `project create`, `site create`, `site clone`, `project restore`, assistant de `ogs tui` | oui |
| `postCreate` | au **premier apply réussi** d'un projet déclaré par `project create`, `site create` ou l'assistant — que cet apply vienne de la commande elle-même ou d'un `ogs apply` ultérieur —, une fois le plan du parc exécuté ; pour `site clone`, après la copie du contenu ; pour `project restore`, après l'apply final qui suit le contenu, une seule fois — une reprise par `--resume` ne le relance pas. Jamais pour un projet adopté par `ogs import` | non |
| `preDelete` | avant la suppression, une fois les refus d'entrée levés et la confirmation donnée, services encore actifs | oui |
| `postDelete` | après suppression et archivage, fichiers de parc recalculés | non |
| `preApply` | après la confirmation, avant le premier geste, une fois par projet à changer — **toujours pour un projet**. Un refus arrête tout l'apply | oui |
| `postApply` | après chaque projet appliqué — toujours pour un projet | non |
| `preApplyFleet` | après la confirmation, avant le premier geste, quand le **plan du parc** a quelque chose à faire ; sans projet. Un refus arrête tout l'apply | oui |
| `postApplyFleet` | après l'exécution du plan du parc ; sans projet | non |
| `postCertRenew` | après le rechargement du serveur web qui suit un certificat changé : `cert renew` (même si un autre a échoué), `cert issue`, et tout apply — `ogs apply`, `site create`, `site clone`, `project restore`, interface —, une fois le plan du projet appliqué et rechargé, avant `postApply` | non |
| `postPhpSwitch` | après un changement de version PHP | non |

`postCertRenew` a deux portées qui s'additionnent : le hook de parc est appelé une fois, sans projet, avec
tous les certificats changés (`OGS_CERT`, séparés par des virgules) ; le hook qu'un projet déclare dans
`spec.hooks` l'est **en plus**, pour les certificats qu'il sert — un certificat partagé compte pour chacun
des projets qui le désignent —, avec le contexte du projet. Pour cet événement, le hook de projet ne remplace
donc pas celui du parc : un même script désigné aux deux portées s'exécute deux fois, et un certificat
partagé le fait exécuter une fois par projet qui le sert. `postCertRenew` n'est pas lancé si le serveur web
n'a pas relu les certificats ; son échec rend le code 6.

`ogs apply`, `site create`, `site clone`, `project restore` et `ogs tui` empruntent le même chemin : les
mêmes hooks s'y déclenchent aux mêmes moments. Le **plan du parc** — rotation des journaux, répertoires
d'exécution, supervision, pare-feu, purge des archives — a ses propres événements, `preApplyFleet` et
`postApplyFleet` (fichiers `pre-apply-fleet`, `post-apply-fleet`), qui n'existent qu'en portée serveur et
reçoivent un contexte sans projet : `OGS_PROJECT` et `OGS_ROOT` y sont vides. `preApply` et `postApply`, eux,
ne sont jamais appelés sans projet — un script qui y vide `"$OGS_ROOT"/var/cache` ne vise jamais la racine de
la machine. `postCreate` attend que le plan du parc ait réussi ; en échec, il viendra au prochain apply qui
aboutit. Les projets déclarés avant cette règle, ou après une perte de la base d'état, ne reçoivent pas de
`postCreate` à leur premier apply : sa marque vit dans la base d'état, et son absence fait manquer le hook
plutôt que de le lancer sur un site qui tourne.

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

La règle vaut pour toute commande, de trois façons. Une commande qui sait simuler — `apply`, `site create`,
`site clone`… — s'arrête avant son hook `pre-`. Une commande qui ne le sait pas refuse l'option, code 2,
avant tout geste : `ogs project delete shopfr --dry-run` ne lance ni `preDelete` ni `postDelete`, là où il
supprimait autrefois le projet pour de bon, hooks compris. Et un hook atteint malgré tout sous
`--dry-run` est résolu — emplacement, propriétaire, droits : un script qui serait refusé l'est aussi en
simulation — puis annoncé `non exécuté (--dry-run)`, sans être lancé.

Le `preDelete` garde la porte d'une suppression, mais il n'est pas seul : `project delete` énumère d'abord
tous ses refus — racine partagée, point de montage, fichier qu'ogs n'a pas écrit, base qu'il ne pourrait
pas sauvegarder… — et demande la confirmation. Le hook ne voit donc qu'une suppression que l'outil
accepte à l'entrée, et son refus l'arrête avant que rien ne soit arrêté ni archivé. Son accord n'est pas
pour autant le dernier mot : l'archive faite, les remparts sont éprouvés de nouveau, et un partage monté ou
une déclaration voisine posée entre-temps fait encore renoncer — l'archive retirée, les services relancés,
code du refus, ou 9 pour une interruption reçue pendant l'archivage. `postDelete` n'est alors pas lancé :
un script qui prépare la suppression dans son `preDelete` doit savoir qu'elle peut ne pas avoir lieu.
