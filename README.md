# ogs

Utilitaire CLI (et à terme TUI) d'administration de serveurs web mutualisés, écrit en Go.

`ogs` industrialise le cycle de vie d'un site hébergé : utilisateur système, arborescence, bases MySQL /
PostgreSQL, vhost et files RabbitMQ, pool PHP-FPM, vhost Apache ou Nginx, certificat TLS, tâches planifiées
et services systemd. Un projet est décrit par un fichier YAML ; un moteur de réconciliation idempotent amène
le serveur dans l'état décrit.

Le cahier des charges technique complet — 90 décisions d'architecture arbitrées, spécification par module,
plan de tests, découpage en lots — est dans [`docs/CAHIER-DES-CHARGES.md`](docs/CAHIER-DES-CHARGES.md).
C'est la référence : toute contribution s'y adosse.

Pour l'usage — déclarer un site, l'appliquer, lancer ses services —, voir le
[guide d'utilisation](docs/UTILISATION.md).

## État d'avancement

| Lot | Contenu | État |
|---|---|---|
| **L0** | Socle : nommage, modèle, validation, YAML, écriture atomique, base d'état, keystore, audit, moteur (plan/apply/diff/saga/verrou), provider système, CLI | **livré** |
| **L1** | Web + PHP : templates surchargeables, pools multi-version 7.4→8.5 avec numérotation, vhosts Apache et Nginx, types Prestashop et Symfony 3.4→8, clé de déploiement, lien `~/.local/bin/php`, `php switch` avec garde-fou de compatibilité, rotation des logs | **livré** |
| **L2** | Bases MySQL/MariaDB et PostgreSQL : privilèges nommés, échappement des motifs de `GRANT`, keystore, multi-cluster PostgreSQL, dump/restore, `secrets env` | **livré** |
| **L3** | TLS : ACME embarqué (lego), HTTP-01 et DNS-01 OVH, wildcard partagé pour les préproductions, timer de renouvellement, suspension après échecs répétés | **livré** |
| **L4** | RabbitMQ : vhosts, utilisateurs, exchanges, files, bindings, policies, préréglage Messenger, arguments immuables détectés | **livré** |
| **L5** | Tâches planifiées, services systemd templatisés avec instances multiples, délégation sudoers, répertoires d'exécution | **livré** |
| **L6** | Composition : `site create` et `site clone`, suppression avec archivage, restauration, `status` | **livré** |
| **L7** | Interface en mode texte : inspecteur, actions contextuelles, assistant de création | **livré** |
| **L8** | Import d'un serveur existant : scan en lecture seule, corrélation, adoption sans renommage | **livré** |
| **L9** | Applications Go, Node, Ruby et Python : runtimes par projet, service supervisé, écoute sur socket, proxy | **livré** |
| **L10** | Industrialisation : hooks de parc et de projet, complétions dynamiques, pages de manuel, paquet `.deb`, CI, scénarios d'intégration conteneurisés | **livré** |
| **L11** | Supervision : prisons fail2ban et entrées Telegraf dérivées du parc, vhost d'état local, comptes de supervision MySQL, PostgreSQL et RabbitMQ | **livré** |
| **L12** | Signature DKIM : une clé par domaine d'envoi, scellée dans le keystore, tables d'OpenDKIM dérivées du parc, enregistrements à publier, rotation en deux temps (D69) | **livré** |
| **L13** | Pare-feu ufw décrit par `/etc/ogs/ufw.yaml` : profils dérivés des services installés, sources nommées, confiance, bannissements, politiques, retour automatique de la première activation (D72) | **livré** |

## Compilation

Aucune dépendance C : le binaire est statique et se déploie seul.

```sh
make build        # produit ./ogs
make test         # tests unitaires
make cover        # couverture
make all          # fmt, vet, test, build
make doc          # pages de manuel, complétions, référence Markdown
make package      # paquet Debian dans dist/
```

### Essais sur un système réel

Certains comportements ne se simulent pas : qu'un rechargement d'Apache prenne en compte une nouvelle
appartenance de groupe, qu'un pool FPM serve bien sous l'identité du projet, qu'un paquet s'installe et se
retire sans défaire les sites. Ces scénarios s'exécutent dans un conteneur Debian complet.

```sh
make integration           # un reload suffit-il pour l'appartenance de www-data au groupe ?
make integration-php       # site PHP servi de bout en bout, sous l'identité du projet
make integration-nginx     # nginx réel : quatre types de vhost, FastCGI, TLS, isolation
make integration-hooks     # portées, refus d'un « pre », rejet d'un script détournable
make integration-mysql     # MariaDB réel : privilèges, échappement des motifs, rotation
make integration-postgres  # cluster réel : schéma public durci, extensions, multi-cluster
make integration-rabbitmq  # broker réel : isolation par vhost, bindings, arguments immuables
make integration-acme      # autorité ACME réelle (pebble) : émission, HTTPS, renouvellement
make integration-systemd   # systemd en PID 1 : instances, durcissement, délégation, cron réel
make integration-runtimes  # application Go supervisée, socket 0660, Node installé par nvm
make integration-harden    # /etc et /home fermés aux utilisateurs, sans qu'aucun service en souffre
make integration-package   # installation, usage et désinstallation du .deb
make integration-suppression # userdel n'aboutit que si les workers du pool sont déjà libérés
make integration-web-durci # unité apache2 durcie (ProtectHome) : les journaux restent atteignables
make integration-pare-feu  # ufw réel, connexions réelles : profils détectés, refus en tête, retour automatique
make integration-restauration # aucun lien traversé, chaque fichier rendu à son propriétaire
make integration-all       # les vingt, à la suite

# Hors série : rbenv et pyenv compilent depuis les sources (un quart d'heure).
make integration-runtimes-compiles
```

Ces scénarios ne sont pas décoratifs : ils ont mis au jour plus de trente défauts que les tests unitaires ne
pouvaient pas voir — dont l'impossibilité de servir un site statique ou une application non-PHP en
HTTPS, un challenge HTTP-01 qui ne pouvait aboutir sur aucun serveur, un accès PostgreSQL
d'administration qui ne pouvait jamais s'authentifier, des services qui ne démarraient jamais au
premier apply, des tâches planifiées dont le verrou échouait systématiquement, et un seul site
applicatif suffisant à rendre toute la configuration nginx invalide.

## Prise en main sans privilèges

`ogs` est root-strict : toute opération d'administration exige root. Le drapeau `--prefix` est la seule
dérogation — il décale l'ensemble des chemins système sous un répertoire jetable et simule les commandes
système. C'est le mode de développement et d'essai.

```sh
make dev-demo     # crée ./dev, déclare deux projets, calcule le plan, applique
```

ou pas à pas :

```sh
./ogs --prefix ./dev init
./ogs --prefix ./dev project create shop.fr --kind php-symfony --app-version 6.4
./ogs --prefix ./dev plan shopfr
./ogs --prefix ./dev apply shopfr --yes
./ogs --prefix ./dev diff shopfr          # « conforme » : le plan est idempotent
./ogs --prefix ./dev project list
```

## Commandes principales

```
ogs init | doctor | harden [--explain] [--apply] | version [--license]
ogs project create <host> [--kind] [--app-version] [--runtime] [--php] [--env prep] [--id] [--host]
ogs project list | show | export
ogs plan | apply | diff  [id...] [-f fichier]
ogs php list | ports | switch <id> <version> [--pool] [--force]
ogs web engine | test | reload
ogs user key show | rotate <id>
ogs template list | show | extract <nom>
ogs db list | add | remove | dump | restore
ogs db user add | rotate <id> <utilisateur>
ogs secrets list | show | env <id>
ogs cert list | issue | renew | revoke | delete
ogs cert dns-provider set ovh --application-key … | --client-id …   (secrets demandés)
ogs amqp status | messenger | queue add | queue purge
ogs amqp credentials set --user …           (mot de passe demandé)
ogs cron add | remove | list | enable | disable | run
ogs firewall init | status | profiles | rules | sources | whois <adresse>
ogs firewall plan | apply | confirm | ban | unban <adresse|source>
ogs service add | remove | list | start | stop | restart | scale | logs
ogs my start | stop | restart | status      (surface déléguée par sudo)
ogs site create <host> [--db] [--messenger] [--service] [--cron] [--runtime] …
ogs site clone <id> --to prep
ogs project delete <id> [--purge] [--keep-data] [--no-dump]
ogs project restore <archive> | archives
ogs status [id...]
ogs tui                                    (ou « ogs » sans argument)
ogs import scan | adopt <id> | unmanaged
ogs app types | set <id> --kind <type>
ogs runtime list | install <id> <version>
ogs completion bash | zsh | fish
```

Toutes les commandes acceptent `-o json` ou `-o yaml`, et disposent de complétions dynamiques : les
identifiants de projet, les services d'un projet, ses bases et ses files sont proposés par le shell.

## Utilisation réelle

```sh
sudo ogs init                                     # /etc/ogs, base d'état, clé maître
sudo ogs doctor                                   # prérequis, permissions, cohérence
sudo ogs project create shop.fr --kind php-symfony
sudo ogs plan shopfr                              # ce qui va changer
sudo ogs apply shopfr                             # application, avec confirmation
```

Le fichier d'état désiré vit dans `/etc/ogs/projects/<identifiant>.yaml` : il est lisible, éditable à la
main et versionnable dans git. `ogs diff` montre l'écart entre ce fichier et le système réel.

### Sauvegarder la clé maître

`ogs init` génère `/etc/ogs/master.key` (mode 0400). Elle chiffre tous les secrets de la base d'état. **Sans
elle, ils sont définitivement irrécupérables** ; avec elle, ils sont tous lisibles. À sauvegarder hors de la
machine, et à protéger comme un secret de premier rang.

### Bascule de version PHP

```sh
sudo ogs php switch shopfr 8.4
```

La compatibilité est lue dans la contrainte `require.php` du `composer.json` de
l'application — pas dans une table figée. Une bascule incompatible est refusée avec
le motif et sa source ; `--force` l'impose et le consigne. Le fichier est lu sans
suivre de lien et borné à 1 Mio : un `composer.json` lié, spécial ou démesuré n'est
pas lu, et c'est la table indicative qui décide, annoncée comme telle. L'ordre d'application est
structurant : nouveau pool, puis réécriture du vhost sur le nouveau socket, puis
seulement retrait de l'ancien pool. Les fichiers cron et les unités systemd ne sont
jamais réécrits, puisqu'ils invoquent `~/.local/bin/php`.

### Bases de données et secrets

```sh
sudo ogs project create shop.fr --kind php-symfony --db mysql
sudo ogs apply shopfr
sudo ogs secrets env shopfr        # DATABASE_URL prête à coller dans le .env
```

Le mot de passe est généré à la première application, scellé dans le keystore et
**jamais régénéré** ensuite : le régénérer invaliderait la configuration applicative
en place. `ogs db user rotate` le fait explicitement quand c'est voulu.

Un moteur injoignable ne bloque pas la création du site : l'étape est non critique,
le site est déployé, et l'échec est rapporté avec son motif (code de retour 6).

### Tests d'intégration des bases

Le test qui valide la syntaxe SQL contre un vrai serveur PostgreSQL crée puis
supprime une base : il est donc désactivé par défaut.

```sh
OGS_TEST_POSTGRES=1 go test ./internal/provider/db/
```

### Certificats TLS

```sh
sudo ogs apply shopfr          # vhost HTTP, puis certificat, puis bascule en TLS
sudo ogs cert list
sudo ogs cert renew --all      # ce que fait le timer ogs-renew deux fois par jour
```

L'ordre importe : le vhost HTTP doit répondre avant le challenge HTTP-01, et le
`/.well-known/acme-challenge` est servi **avant** la redirection vers HTTPS — sans
quoi aucun renouvellement n'aboutirait. L'émission est non bloquante : si elle
échoue, un site neuf reste en ligne en HTTP avec un avertissement et une commande de
reprise, et un site déjà en HTTPS **le reste** — un vhost est rendu d'après le
certificat présent, jamais d'après la demande, et ajouter un alias ne lui retire pas
son bloc 443.

Le certificat d'un projet couvre aussi ses **redirections**. Une commande ACME
échouant tout entière sur un seul nom refusé — une redirection dont le DNS pointe
encore ailleurs —, l'émission se replie par paliers : sans les redirections en
attente, sans aucune redirection, sur ce que couvre le certificat en place. Le site
reste couvert, le nom refusé est nommé, le code est 6 ; loin de l'échéance, aucun nom
déjà servi n'est retiré. `cert renew` recharge le serveur web dès qu'un certificat a
changé, et nomme — au lieu de le passer en silence — un projet dont la demande est
refusée.

Les **préproductions partagent un wildcard** `*.preprod-pp.net` : créer une préprod
ne déclenche donc aucun appel à l'autorité. Ce certificat exige le challenge DNS-01,
donc des identifiants OVH :

```sh
sudo ogs cert dns-provider set ovh --client-id …   # le secret client est demandé sans écho
sudo ogs cert issue @wildcard-prep
```

Les secrets ne passent pas en option : ils resteraient dans le journal de sudo et
l'historique du shell. Hors terminal, ils se lisent sur l'entrée standard, une ligne
chacun.

Après cinq échecs consécutifs sur un même jeu de domaines, les tentatives sont
suspendues une heure : insister ferait perdre le droit d'émettre pour le domaine.

### Files RabbitMQ

```sh
sudo ogs amqp credentials set --user ogs-admin     # mot de passe demandé sans écho
sudo ogs amqp messenger shopfr --transport async,priority
sudo ogs apply shopfr
sudo ogs secrets env shopfr        # MESSENGER_TRANSPORT_DSN incluse
```

Le préréglage produit ce qu'attend le transport AMQP de Symfony Messenger : un
exchange direct, une file par transport, un binding par clé de routage et une file
d'échec pour rejouer les messages rejetés.

**Les arguments d'une file sont immuables.** Modifier un TTL, un exchange de rejet
ou le type d'une file existante exige de la recréer, ce qui détruit les messages en
attente. `ogs` détecte l'écart, l'affiche avec le nombre de messages et de consumers
concernés, et refuse d'agir sans `--recreate-queues` :

```
~ queue async : x-message-ttl 3600000 → absent — recréation requise (1240 message(s) en attente)
  ⚠ les arguments d'une queue sont immuables : recréer async détruirait 1240 message(s)
    en attente et 3 consumer(s) seraient déconnectés — relancer avec --recreate-queues
```

### Tâches planifiées et services

```sh
sudo ogs cron add shopfr scheduler --schedule "* * * * *" --command "bin/console schedule:run"
sudo ogs service add shopfr messenger-async --transport async --instances 3
sudo ogs apply shopfr
```

Chaque tâche est enveloppée dans `flock` par défaut, s'exécute sous l'utilisateur du
projet et journalise dans `logs/` — un fichier **pré-créé par ogs**, puisque `logs/`
appartient à root et que la tâche ne pourrait pas le créer elle-même. Les occurrences
à venir sont affichées à la déclaration, pour confirmer que l'expression dit bien ce
qu'on croit.

Les services sont des **unités templatisées** : mettre à l'échelle ne duplique aucun
fichier, et `ogs service scale` agit immédiatement sans modifier la déclaration — le
prochain `apply` rétablit le nombre déclaré.

### Délégation à l'utilisateur du projet

```sh
# depuis le compte du projet
sudo ogs my restart
```

Le fichier `/etc/sudoers.d/ogs-<id>` énumère les commandes autorisées, **sans le
moindre métacaractère** : un `*` laisserait passer des arguments imprévus, et un
`systemctl` sans unité ouvrirait `link`, `isolate`, `mask` ou l'arrêt de la machine.
La commande `ogs my` ne prend aucun argument de projet — il est déduit de `SUDO_UID`,
que sudo positionne après avoir purgé l'environnement. `ogs doctor` vérifie que
`env_reset` est bien active, sans quoi cette déduction ne serait plus fiable, et
audite les fichiers de délégation à la recherche d'un élargissement manuel.

### Créer un site complet

```sh
sudo ogs site create shop.fr \
  --kind php-symfony --app-version 6.4 --php 8.5 \
  --alias www.shop.fr --redirect ancien-shop.fr \
  --db mysql --messenger async \
  --service symfony-messenger:async:2 \
  --cron "* * * * *:bin/console schedule:run"
```

Une seule saga assemble l'utilisateur, l'arborescence, la base, le transport, le
pool PHP, les vhosts, le certificat, les tâches et les services. Une étape critique
en échec défait les précédentes ; les étapes non critiques — base injoignable,
certificat, démarrage de service — laissent le site en place avec un avertissement.

### Supprimer et restaurer

```sh
sudo ogs project delete shopfr           # archive puis supprime
sudo ogs project archives
sudo ogs project restore /var/lib/ogs/archives/shopfr-20260823-143000.tar.zst
```

L'archive contient la déclaration, les configurations générées, les dumps de bases,
l'arborescence, les secrets **restés chiffrés** et un manifeste décrivant tout ce qui
a été retiré. L'espace disponible est vérifié avant de commencer : échouer au milieu
d'une suppression laisserait le système à moitié démonté.

`--purge` supprime sans archive et **exige la saisie de l'identifiant** : `--yes` ne
suffit pas pour une opération sans recours. `--keep-data` ne retire que les
configurations.

Avant tout geste, et avant même la confirmation, la suppression énumère **ensemble**
tout ce qui la refuse, chaque refus avec son remède : une racine qu'un autre projet
partage ou englobe, un point de montage sous l'arborescence ou les journaux, un fichier
de configuration qu'ogs n'a pas écrit pour ce projet, une base qu'elle ne pourrait pas
sauvegarder (`--no-dump` lève ce refus) et, sans `--force`, le compte ou les bases d'un
projet adopté. Le site — vhost, pool, tâches — est coupé avant l'archive et le dump
(D114). Une interruption reçue avant le premier retrait abandonne la suppression, rétablit
le site et relance les services arrêtés ; reçue après, elle laisse la suppression aller à
son terme. `--dry-run` y est refusé (code 2) : la commande ne sait pas simuler.

La restauration applique d'abord la déclaration, puis remet le home tel que
l'utilisateur l'avait laissé, **sans traverser aucun lien** — ni celui du trousseau
commun, ni celui de l'interpréteur que l'apply vient de poser — et en rendant chaque
fichier à qui le possédait : ce qui n'était pas à l'utilisateur revient à root.

### Superviser

```sh
sudo ogs status
PROJET  ENV   HOST     PHP  ÉCARTS  SERVICES  TLS   ÉTAT
shopfr  prod  shop.fr  8.5  0       2/2       89 j  sain
```

Le code de retour est non nul dès qu'un écart, un service manquant ou un certificat
proche de l'échéance est détecté.

### Interface en mode texte

```sh
sudo ogs          # ou « ogs tui »
```

Liste des projets à gauche, détail à droite, actions au clavier : `d` pour les
écarts, `a` pour appliquer, `s` pour redémarrer, `c` pour renouveler le certificat,
`n` pour l'assistant de création, `/` pour filtrer.

Toute action mutante demande confirmation et passe par **le même moteur** que la
ligne de commande : l'interface n'a pas de chemin privilégié, et la sortie affichée
est celle qu'aurait produite `ogs apply`. `NO_COLOR` et `TERM=dumb` sont respectés —
l'information reste portée par les symboles (`●` sain, `⚠` dérive, `✕` erreur), donc
lisible sans couleur.

### Reprendre un serveur existant

```sh
sudo ogs import scan       # lecture seule : rien n'est modifié
sudo ogs import adopt boutiquefr
sudo ogs diff boutiquefr   # ce qu'un premier apply changerait
```

Le scan rapproche les ressources par ce qui est **vérifiable** — le socket FPM
référencé par un vhost, l'utilisateur du pool, le docroot sous un home — et énonce
ce qui ne l'est pas : la propriété d'une base n'est pas déductible. Une base n'est
donc rattachée qu'au seul site que son nom désigne, aux séparateurs près — `shop_fr`
pour `shop.fr`, jamais `shopping` —, et déclarée qu'avec le seul compte de son nom,
sous son hôte réel (`'shop'@'%'` reste `'%'`) ; sinon elle reste hors gestion, et
l'adoption le dit avant d'écrire.

**L'adoption ne renomme jamais.** Un site servi depuis `/var/www/boutique` par
l'utilisateur `boutique` garde ces noms : les changer exigerait de recréer le compte
et de migrer la base. Son vhost garde son fichier (`spec.web.vhosts[].file`), repris
en place par `ogs apply --force`, qui en sauvegarde l'original. Les écarts à la
convention sont affichés à l'adoption, et `ogs diff` montre ce qu'un premier apply
normaliserait — avant de le faire.

Les mots de passe existants sont inconnus d'ogs : ils ne sont **ni devinés ni
réinitialisés**. `ogs db user rotate` permet d'en reprendre la main quand on le
décide. Tout ce qui n'a pas été rattaché reste hors gestion et ne sera jamais touché.

### Applications Go, Node, Ruby et Python

```sh
sudo ogs site create app.fr --kind node
sudo ogs runtime install appfr 22.11.0
sudo ogs apply appfr
```

Le processus est supervisé par systemd, le vhost devient un reverse-proxy, et
l'ordre compte : le service démarre et **doit répondre** avant que le vhost ne soit
écrit — sinon le premier rechargement exposerait un 502.

L'écoute passe par un **socket Unix** dès que le runtime le permet : ses permissions
(`0770`, groupe du projet) restreignent l'accès au serveur web, alors qu'un port
local est joignable par tout utilisateur de la machine. Un binaire Go, qui n'a pas de
convention de socket, écoute sur un port `2<runtime><incrément>` borné à `127.0.0.1`.
`ogs doctor` signale toute application qui écouterait au-delà.

`ogs` ne peut pas **forcer** une application Node à écouter sur un socket : le chemin
est transmis par `SOCKET_PATH`, et si le code l'ignore, l'attente de disponibilité
échoue avec un message qui nomme la cause.

## Conventions structurantes

- **Un identifiant unique par projet**, dérivé du host (`shop.fr` → `shopfr`), qui nomme l'utilisateur
  Linux, son groupe, les bases, le vhost RabbitMQ, le pool PHP, le vhost web, les unités systemd et le cron.
- **Home** en `/home/<host effectif>`, en 0711, ses répertoires en 0710 ; le code applicatif dans `www/`, le docroot dépendant du
  type d'application (`www` pour Prestashop, `www/public` pour Symfony 4+, `www/web` pour Symfony 3.4).
- **Journaux hors du home** (D71) : sous `/var/log/ogs-projects/<id>/` (réglable par `logs.root`), chaîne
  entièrement root, fichiers pré-créés ; le home n'en porte qu'un lien `~/logs` pour la lecture, qu'aucun
  processus root n'emprunte. Un répertoire root dans le home ne protégeait rien : l'utilisateur, qui possède
  son home, le renommait et en recréait un à lui, où un journal que le serveur web ouvre en root devenait
  un lien vers un fichier système.
- **Liens gérés dans `~/.local/bin`** (D70) : interpréteur PHP et runtimes, au compte de l'utilisateur et
  sur son PATH. Le lien est rétabli à chaque apply ; root l'écrit sans jamais suivre de lien, et un fichier
  posé à sa place n'est remplacé que sous `--force`.
- **Traverser sans énumérer** : le home est en 0711 et les répertoires du projet en 0710. Servir un fichier
  n'exige que la traversée du répertoire et la lecture du fichier ; pouvoir lister le contenu ne sert qu'à
  explorer, ce dont personne n'a besoin — pas même le serveur web. Le bit de groupe du home est
  indispensable : `www-data` étant membre du groupe, Unix ne se replierait pas sur les droits « autres ».
- **Isolation** : chaque projet a son groupe privé, et c'est `www-data` qui en devient membre secondaire —
  jamais l'inverse, qui rendrait chaque site lisible par tous les autres.
- **Préproduction** : `--env prep` produit un projet distinct sur un domaine dédié
  (`<identifiant>.preprod-pp.net`), sans partager la moindre ressource avec la production. Elle est
  **accessible sans authentification** — protégée de l'indexation, pas des regards : n'y mettez pas de
  données réelles.
- **Privilèges MySQL** : le nom de base côté `ON` est un motif où `_` fait joker — il est donc
  systématiquement échappé. Le nom d'utilisateur, lui, est un littéral exact et ne l'est pas.
- **Archives** : conservées 14 jours, puis purgées par une étape du plan — annoncée, auditée, jamais
  silencieuse. `archives.retentionDays: 0` conserve indéfiniment, et doit être un choix explicite.
- **Fournisseur DNS** : OVH seul est géré, refusé dès la déclaration pour tout autre.
- **Répertoires sensibles** : `ogs harden` ferme les répertoires de configuration aux utilisateurs du
  serveur — qui y ont un compte local — sans jamais desserrer ce qui l'est déjà plus, et sans toucher
  `/etc/php`, où le PHP des crons lit son `php.ini`. Il ne ferme rien sans le groupe du service qui lit :
  MariaDB lit `/etc/mysql` sous `mysql`, les workers web lisent `.htpasswd` sous `www-data`, et une cible
  dont le groupe manque est refusée plutôt que fermée. La racine des homes passe en 0711 : traversable, donc
  les sites sont servis comme avant, mais plus énumérable — un `ls /home` ne donne plus la carte du parc.
  Le gain est partiel et annoncé comme tel : `/etc/passwd` énumère déjà les homes.
- **Supervision** : ogs n'installe ni fail2ban ni Telegraf, il leur écrit ce qu'eux seuls ne peuvent pas
  deviner — quels journaux appartiennent à un projet, quels pools tournent sous quelle version. Aucune
  destination de métriques, aucun filtre fail2ban : le jeton d'écriture ne concerne aucun projet, et un
  filtre se maintient au rythme des changements de format de journal. L'état des pools passe par un vhost
  qui n'écoute que la boucle locale, jamais par le socket FPM — parler à ce socket, c'est pouvoir y faire
  exécuter du code sous l'identité du projet.
- **Signature DKIM** : ogs tient une clé par domaine d'envoi et les tables d'OpenDKIM, mais ne touche ni
  au MTA ni à `opendkim.conf`, et ne publie rien dans le DNS — `ogs mail dkim records` émet
  l'enregistrement, `doctor` vérifie ce qui l'a été. La rotation ne bascule qu'une fois la nouvelle clé
  publiée : basculer vers une clé non publiée casserait la signature de tout envoi. Refusée en
  préproduction, qui signerait sinon au nom du domaine de production.
- **Pare-feu** : sans `/etc/ogs/ufw.yaml`, ogs ne touche pas à ufw. Avec lui, il écrit ses propres
  profils (`ogs-ssh`, `ogs-sftp`, `ogs-mysql`…) aux ports qu'il lit sur la machine — un sshd sur 2200, le
  SFTP de proftpd sur 2222 —, et ne touche qu'aux règles dont le commentaire commence par `ogs:` ; celles
  de l'administrateur restent intactes. La première activation se défait d'elle-même au bout de cinq
  minutes faute de `ogs firewall confirm` lancé depuis une nouvelle session, et une déclaration qui
  couperait la session SSH en cours, ou fermerait un port où sshd écoute, est refusée. Seul
  `--allow-lockout`, propre aux commandes `ogs firewall`, lève ce refus : jamais le `--force` des
  fichiers retouchés.
- **Appartenance** : un fichier n'est à un projet que par l'en-tête qu'ogs y écrit, jamais par son nom.
  Un fichier posé à la main selon la convention de nommage n'est ni retiré ni écrasé en silence ; un
  fichier retouché n'est remplacé, ou retiré, que sous `--force`, qui le sauvegarde dans
  `/var/lib/ogs/backups` ; le fichier d'un autre projet ne l'est jamais.
- **Docroots** : le serveur web ne suit un lien que vers ce qui appartient à son auteur — `www-data`,
  membre du groupe de chaque projet, servirait sinon les fichiers d'un voisin —, et ne sert ni les
  fichiers cachés (`.git`, `.env`), ni `composer.json`, `/.well-known/` excepté.
- **`--dry-run`** : simulé par les commandes qui le savent, refusé (code 2) par les autres — jamais
  ignoré. Aucun hook ne s'exécute sous ce drapeau.

## Codes de retour

| Code | Signification | | Code | Signification |
|---|---|---|---|---|
| 0 | succès | | 5 | dépendance absente |
| 1 | erreur d'exécution | | 6 | succès partiel, ou écart détecté par `diff` |
| 2 | erreur d'usage | | 7 | verrou indisponible |
| 3 | échec de validation | | 8 | privilèges insuffisants |
| 4 | conflit | | 9 | opération annulée |

Le 2 couvre aussi `--dry-run` demandé à une commande qui ne sait pas simuler ; le 9, un refus à la
confirmation ou une interruption — notamment pendant le calcul d'un plan, avant le point de non-retour
d'une suppression, ou pendant la copie d'un clone.

## Documentation

- [`docs/CAHIER-DES-CHARGES.md`](docs/CAHIER-DES-CHARGES.md) — la référence : 90 décisions d'architecture
  arbitrées, spécification par module, plan de tests, découpage en lots.
- [`docs/UTILISATION.md`](docs/UTILISATION.md) — déclarer un site, l'appliquer, le faire vivre ; fichiers
  retouchés et `--force`, suppression et restauration, migration d'un ancien serveur.
- [`docs/INSTALLATION.md`](docs/INSTALLATION.md) — installation, premier démarrage, sauvegarde, mise à jour.
- [`docs/HOOKS.md`](docs/HOOKS.md) — raccorder l'outil au reste de l'exploitation.
- `man ogs`, et une page par commande (`man ogs-site-create`) — engendrées depuis l'arbre des commandes,
  elles ne peuvent pas dériver du code.

## Licence

WTFPL — voir [`LICENSE`](LICENSE). La WTFPL ne comportant aucune clause d'exclusion de garantie, celle-ci
est portée par [`NOTICE`](NOTICE) : cet outil s'exécute en root et peut détruire des données, lisez cet
avertissement avant de l'employer en production.
