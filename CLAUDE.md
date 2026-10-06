# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Langue

Le projet est **entièrement en français** : code, commentaires, messages d'erreur, documentation, noms de
tests, sorties CLI. S'y conformer, accents compris. Les identifiants techniques (noms de types Go, clés
YAML, directives de configuration) restent dans leur forme d'origine.

Les commits ne mentionnent jamais l'assistance d'un outil d'IA.

## La référence

[`docs/CAHIER-DES-CHARGES.md`](docs/CAHIER-DES-CHARGES.md) fait autorité : 120 décisions d'architecture
arbitrées (D1–D115, D117, D118 et D120 à D122, D116 réservée ; tableau §2), spécification par module, plan de tests, découpage en lots. **Consulter le
tableau des décisions avant de proposer une architecture** — la plupart des questions « pourquoi pas
plutôt… ? » y sont déjà tranchées, avec leur raison. Toute décision nouvelle s'y ajoute.

## Commandes

```sh
make build            # ./ogs, statique (CGO_ENABLED=0), aucune dépendance C
make test             # tests unitaires
make check            # fmt + vet + test
make cover            # couverture (66,5 % actuellement)
make doc              # pages de manuel, complétions, référence Markdown → dist/doc
make package          # paquet Debian → dist/*.deb (nfpm, réseau requis)

go test ./internal/provider/web/ -run TestNginx -v   # un test précis
go test ./packaging/          # scripts du paquet, rejoués comme dpkg les enchaîne (dpkg requis)
go test ./test/migration/     # docs/migrer.sh contre un ancien serveur simulé (rsync, python3-yaml)
```

Ces deux bancs tournent dans `make test` et se sautent d'eux-mêmes quand un outil manque (§16.3 du
cahier). Celui de la migration construit ogs et le lance en mode préfixé ; ssh, moteurs de base, crontab,
`dig`, `curl` et `runuser` y sont simulés, et chaque appel est noté pour que les tests vérifient
l'identité que le script demande à chaque écriture — le banc ne tourne pas en root.

### Essai sans privilèges

`ogs` est root-strict. `--prefix <répertoire>` est la seule dérogation : il décale tous les chemins système
sous un répertoire jetable et **simule** les commandes système (`exec.Recorder` au lieu d'`exec.Local`).
C'est le mode de développement, et celui des tests de `internal/cli`.

```sh
make dev-demo                                   # crée ./dev, deux projets, plan, apply
./ogs --prefix ./dev project create shop.fr --kind php-symfony
```

### Essais sur système réel

Vingt scénarios conteneurisés éprouvent ce que les tests unitaires ne peuvent pas voir. Ils ont mis au jour
plus de trente défauts invisibles autrement (§18 du cahier des charges) : **y recourir avant de déclarer un
provider fonctionnel**.

```sh
make integration-all       # les vingt, à la suite
make integration-nginx     # un seul (voir SCENARIOS dans le Makefile)

# Hors série : rbenv et pyenv compilent depuis les sources (un quart d'heure).
make integration-runtimes-compiles
```

Mécanique dans `test/integration/` : une image Debian portant Apache, nginx, PHP-FPM, MariaDB, PostgreSQL,
systemd et cron ; une composition Docker pour ce qui exige un service tiers joignable par le réseau
(RabbitMQ, pebble) ; un conteneur avec **systemd en PID 1** pour le scénario des services et des tâches.
Les scénarios sont montés en volume — les mettre au point ne demande pas de reconstruire l'image.

Pièges du harnais :

- `test/integration/systemctl-shim.sh` remplace `systemctl` quand il n'y a pas d'init, et s'efface de
  lui-même devant le vrai. Il échoue explicitement sur ce qu'il ne sait pas faire : un substitut
  complaisant rendrait les scénarios creux.
- l'image porte les deux moteurs web, donc `ogs` refuse de choisir : chaque scénario appelle
  `choisir_moteur apache|nginx` (`scenarios/lib.sh`) après `ogs init`. Cette fonction **arrête aussi
  l'autre** : les deux moteurs sont activés dans l'image, et sous un vrai systemd ils se disputent le
  port 80 au démarrage. Le perdant restait « failed » et le scénario interrogeait le vainqueur, qui n'avait
  jamais chargé les vhosts d'ogs et servait la page par défaut de Debian depuis `/var/www/html` — un
  scénario dont le verdict dépendait d'une course. Le moteur `apache` a pour unité `apache2` : la
  fonction relançait longtemps une unité `apache` inexistante, en silence, et Apache restait « failed »
  jusqu'au premier apply d'ogs, qui le relevait — seul le scénario `pare-feu`, qui interroge le port 80
  avant tout apply, l'a vu.
- **apt 3 (Debian 13) fixe `DPkg::Path=/usr/sbin:/usr/bin:/sbin:/bin`** : sous `apt-get`, les scripts de
  maintenance ne voient jamais `/usr/local`, donc ni le substitut de `systemctl` ni une trace posée là — ils
  appelaient le vrai, et la mise à jour du paquet n'éprouvait rien. Le scénario `paquet` passe
  `-o DPkg::Path="$PATH"` à chaque `apt-get` (`apt_paquet`).
- **PHP-FPM tourne avec l'opcache** (`revalidate_freq=2`) : un script réécrit puis interrogé dans les deux
  secondes répond encore par l'ancien code, et sans init le substitut recharge FPM par
  `pkill -USR2 -f /usr/sbin/php-fpm<v>`, que le titre réécrit du maître peut faire manquer. Écrire un script
  une fois pour toutes, avec toutes les branches dont le scénario a besoin : `nginx` échouait de façon
  reproductible.
- **Ce qui énumère un répertoire sous `www-data` échoue sur le docroot**, qu'il traverse sans pouvoir le
  lire (0710, D51) : MultiViews (403, AH00686), `Indexes`, et sous nginx `disable_symlinks`, qui ouvre les
  répertoires. Un scénario qui éprouve ces mécanismes le fait dans un sous-répertoire, et pose un
  **témoin** qui établit que le mécanisme est actif — sans quoi un refus ne prouve rien. Le témoin
  MultiViews de `site-complet`, posé à la racine du docroot, faisait échouer le scénario à chaque passage.
- **Une empreinte sha256 d'en-tête contient n'importe quelle suite hexadécimale** : une assertion qui
  cherchait « 443 » dans les lignes retirées d'un diff échouait une fois sur huit. Une assertion sur un
  diff ignore l'en-tête.

## Architecture

### Le modèle : état désiré, réconciliation

Un projet est décrit par un YAML dans `/etc/ogs/projects/<id>.yaml` (`internal/model`, chargé par
`internal/spec`). Chaque provider **observe** l'état réel, le **compare** au désiré et produit des
**étapes** ; le moteur (`internal/engine`) les exécute en saga — une étape critique en échec fait compenser
les précédentes en ordre inverse, une étape non critique produit un avertissement et l'exécution continue
(un certificat qui échoue ne doit pas détruire un site par ailleurs fonctionnel).

`plan`, `diff` et `apply` partagent le même calcul : `apply.go:buildPlan` assemble les providers dans un
ordre qui est une décision, non un détail — système → bases → AMQP → applicatif (runtime, lien, unité) →
PHP → cron → unités et démarrage des instances → attente de disponibilité → paires TLS dépareillées →
web → TLS → bascule TLS du vhost → sudoers. Les services démarrent **avant** le serveur web : un vhost de
proxy branché sur une application qui n'a pas encore démarré exposerait une 502. Une paire clé/certificat
dépareillée se rétablit **avant** tout ce qui écrit la configuration du serveur web (D110) : nginx refuse la
configuration entière dès `nginx -t` tant qu'un vhost la désigne encore. `buildGlobalPlan` porte ce qui vaut pour tout le
parc (logrotate, tmpfiles, unités de renouvellement, vhost de statut, supervision).

**Le plan global lit le parc entier lui-même, jamais la liste des projets visés.** Chacun de ses fichiers
est recalculé *en entier* : le nourrir d'un sous-ensemble ne les tronque pas, il en efface tous les autres
projets. `ogs apply shopfr` retirait ainsi les voisins de la rotation des journaux, des prisons fail2ban,
des entrées Telegraf et de `/etc/tmpfiles.d/ogs.conf` — et comme `systemd-tmpfiles` ne supprime rien, la
panne restait invisible jusqu'au redémarrage suivant, où plus aucun `/run/ogs/<id>` n'était recréé et où
tous les verrous de tâches échouaient. Le **fragment nginx**, lui, a fait le chemin inverse : il définit
`$connection_upgrade`, dont dépend tout vhost de proxy, et le laisser au plan global le posait *après* les
vhosts qui l'emploient — sur une machine neuve, le premier site applicatif faisait échouer `nginx -t` et
l'apply n'aboutissait jamais. Il est écrit dans le plan de projet, comme le complément d'unité (D59).

**Un seul chemin d'apply** (D112) : `ogs apply`, `site create`, `site clone`, `project restore` et la TUI
passent par `applyFleet` (`internal/cli/chemin_apply.go`), `project delete` et la bascule DKIM par
`runGlobal`. Quatre copies de la boucle « projets puis plan global » avaient chacune perdu quelque chose — plan
global ni affiché ni confirmé (activation d'ufw, purge d'archives), parc privé de ses fichiers communs par un
seul projet en échec, TUI sans hooks ni plan global. Le chemin : plans des projets **et du parc** affichés sans
rien réserver, **une seule confirmation**, les `preApply` des projets et `preApplyFleet` du parc — jamais
`preApply` sans projet : un `post-apply` écrit pour un projet, appelé avec `OGS_ROOT` vide, a vidé
`/var/cache` de la machine —, puis chaque
projet **recalculé** — ce qui s'est ajouté depuis l'affichage se reconfirme — et le plan global, exécuté même
si un projet a échoué, où un projet en échec figure par la déclaration du parc et jamais par sa version
refusée. Ne jamais réécrire cette boucle ailleurs : un appelant neuf passe par `applyFleet`.

### Les rechargements, groupés mais pas naïvement

Les étapes déclarent des **intentions** de rechargement (`Step.Reload`), dédupliquées et exécutées en fin
d'apply : cent projets ne provoquent qu'un `systemctl reload apache2`. Deux subtilités durement acquises :

- `Step.FlushReloads` force l'exécution des intentions en attente **avant** l'étape. Le challenge HTTP-01
  en a besoin (le vhost doit être chargé, pas seulement écrit), le démarrage d'une instance systemd aussi.
- les intentions **s'accumulent au fil des étapes appliquées**, jamais d'avance : un vidage anticipé ne doit
  pas consommer celle d'une étape à venir.

Toute exécution de plan doit recevoir `Reload` : l'oublier sur le plan global avait pour effet que
`systemd-tmpfiles` ne tournait pas — tous les verrous de tâches échouaient — et que le timer de
renouvellement des certificats n'était jamais activé.

### Nommage : un identifiant pour tout

`internal/naming` dérive du host un identifiant unique (`shop.fr` → `shopfr`, ≤ 28 caractères + `prep` en
préproduction) qui nomme l'utilisateur Linux, son groupe, les bases, le vhost AMQP, le pool PHP, le vhost
web, les unités systemd et le fichier cron. `internal/validate` contrôle la cohérence d'un projet et
l'unicité dans le parc — **toujours le parc entier** (`App.checkFleetUniqueness`, D94), jamais les seuls
projets visés : `apply blogfr` ou `plan -f` comparaient le projet à lui-même, et un domaine ou une base d'un
voisin entraient sans un mot. Seuls y comptent les conflits qui touchent un projet visé
(`validate.UniquenessFor`), et **tout chemin d'apply ou de déclaration** l'appelle — la TUI l'oubliait. Le nom
d'un certificat est un nom de répertoire, unique dans le parc, et l'identifiant de chaque projet y est
réservé : c'est celui de son certificat implicite. Une clé d'unicité se calcule sur **tout ce que le provider
écrit ou sert**, jamais sur une partie de la déclaration (D106) : les domaines des vhosts déclarés comme
ceux de `spec.domains`, le répertoire de surcharge (nom du vhost) comme le fichier, `mariadb` comme
`mysql` — chacune de ces omissions laissait un projet prendre la ressource d'un voisin. **Ce que `validate.Project` refuse arrête tout calcul de parc**
— il suit chaque chargement : `loadProjects` refuse en code 3 le plan et l'apply de tous les projets, voisins
compris, en nommant chaque déclaration fautive ; `cert renew`, `doctor` et les consultations qui ne
recalculent aucun fichier du parc (`project list`, `status`, la TUI, `ogs my`) passent par `loadFleet`, qui
les met à part (code 6 pour le renouvellement, D120). Tout recalcul du parc (`buildGlobalPlan`) éprouve les
racines. Une règle neuve qui rendrait invalide une déclaration
jusque-là acceptée se porte donc au plan et à l'apply du projet visé (`checkFleetUniqueness`) et à `doctor`,
jamais au chargement. C'est le cas des **racines** : un home hors de `naming.homeRoot` ou des journaux hors de
`logs.root` (`validate.Placement`) se jugent sur le parc entier au plan, à l'apply et à `project delete`
(`checkPlacement`) — changer l'une de ces racines dans `config.yaml` rendait sinon tout le parc invalide,
timer de renouvellement compris (D120).

### Fichiers gérés

`internal/fs` écrit atomiquement avec un en-tête portant **l'empreinte du corps**. `Inspect` compare cette
empreinte, non les octets : l'en-tête contient un horodatage, et comparer les octets ferait passer tout
fichier « en écart » la seconde suivant son écriture. Cinq états : absent, conforme, à mettre à jour,
modifié à la main (exige `--force`), non géré par ogs.

Trois corollaires, chacun payé d'un défaut réel :

- **La relecture ne consomme que l'en-tête d'ogs, jamais une ligne du corps.** Sauter « toute ligne de tête
  vide ou commentée » recalculait l'empreinte sur un corps tronqué dès que le corps commençait lui-même par
  un commentaire — les deux vhosts de statut, le fragment nginx, Telegraf, fail2ban, logrotate sur parc
  vide. Les cinq passaient en « modifié à la main » à chaque relecture, donc réécrits à chaque apply, avec
  rechargement d'Apache, de Telegraf et de fail2ban au passage. Le symptôme ne se voit qu'à la seconde
  suivante : dans la même seconde, les octets sont identiques et le raccourci masque tout.
- **Les cinq états se traitent partout de la même façon.** Un provider qui ne distingue pas « modifié à la
  main » écrase le travail de l'exploitant en silence — et pour une unité systemd, redémarre ses services
  dans la foulée. `--force` est la porte de sortie (§6.4) ; elle doit être branchée sur *chaque* provider
  qui porte le refus, sans quoi le message renvoie à un drapeau sans effet.
- **Un fichier n'est à un projet que par son en-tête**, jamais par son nom (D76). `shop_api.conf` n'est pas
  un pool de `shop`, c'est le pool principal de `shop_api` : l'attribution par préfixe faisait retirer, au
  premier apply de `shop`, le pool et le vhost du voisin, en code 0 et sans copie. Les providers lisent la
  ligne `projet: … ressource: …` là où `Inspect` lit la marque, dans les trois premières lignes
  (`fs.ManagedResource`, `fs.CheckOwner`) — ce que l'un attribue, l'autre doit le juger de même. La **ressource** de l'en-tête compte autant que le projet : `ogs-<id>-app@.service` est l'unité
  applicative (`app/app`) comme le modèle d'un service déclaré « app » (`unit/app`), et le retrait de l'unité
  d'une application quittée, qui n'exigeait que le projet, emportait ce service un apply sur deux (D100). **Tout
  retrait** passe par `removal.File`, sous les cinq états, l'état relu à l'application : un fichier
  retouché ne part que sous `--force`, sauvegardé ; un fichier non géré, ou marqué pour un autre projet,
  ne part jamais ; et un projet n'écrit jamais le fichier d'un autre, `--force` compris (code 4). Seul
  `project delete` garde un critère plus strict, `fs.ManagedProject`, la marque en première ligne : il
  décide seul de ce qu'il efface, et ne pas reconnaître un fichier le laisse en place. Trois exceptions,
  retirées en bloc sans lire d'en-tête : les clés DKIM du projet (`/etc/opendkim/keys/<id>`), le répertoire
  `.wants` de sa cible, et le fichier d'environnement de D75 : ogs n'en écrit pas le contenu, il ne porte donc aucun en-tête, et il
  part hors `--keep-data` — il porte des secrets, dont l'archive garde la copie scellée. Sous `--keep-data`,
  il reste : c'est le contenu de l'exploitant, que rien ne reproduit (D100), et la purge le nomme avant la
  saisie de l'identifiant.

La sauvegarde de `--force` va dans **`/var/lib/ogs/backups`**, jamais à côté de l'original : tout répertoire
de configuration est relu en entier par quelqu'un — son démon, ou ogs lui-même, qui inventorie
`/etc/sudoers.d` et `sites-available`. Et l'horodatage se devine à la seconde près, donc une copie déposée
dans un répertoire qu'un non-root possède se ferait détourner par un lien posé d'avance à ce nom. Le nom
de base n'y est pas unique pour autant — pool et vhost s'appellent tous deux `<id>.conf`, cron et sudoers
`ogs-<id>`, Telegraf et fail2ban `ogs.conf` — et un même apply forcé les sauvegarde dans la même seconde :
`fs.writeBackup` crée en `O_EXCL`, puis `.1`, `.2`…, sans jamais suivre un lien posé à ce nom. Un
`os.WriteFile` écrasait la première copie, que le plan annonçait sauvegardée.

**Quand root touche au home, il passe par un descripteur, jamais par un nom** : le home appartient à
l'utilisateur (D51), qui renomme et substitue n'importe quel composant sans privilège. Les primitives de
`internal/fs`, chacune ouvrant chaque composant par `openat` avec `O_NOFOLLOW` et opérant relativement au
descripteur :

- `fs.Dir` — `fs.OpenDir`, puis `OpenDir`, `OpenFile`, `Mkdir`, `Create`, `Symlink`, `Remove` relatifs au
  répertoire ouvert, un composant à la fois : l'archive et la restauration (D81), le fichier de sauvegarde
  de `db dump`. Le propriétaire
  d'un lien s'y pose par un descripteur `O_PATH` (`Dir.lchown`), et `OpenFile` retente le temps que le
  noyau rompe un bail `F_SETLEASE`, puis échoue en le nommant ;
- `fs.EnsureDirAt`, `SetDirAt`, `LstatAt` : les répertoires de `model.defaultDirs`, depuis la racine du
  projet (D83). `fs.EnsureDir`, qui résout les parents par leur nom, ne convient qu'à un chemin dont tous
  les parents appartiennent à root — la racine d'un projet, `/etc/…` ;
- `fs.SymlinkAt`, `LinkStateAt`, `RemoveLinkAt`, `RemoveEmptyDirAt` : les liens gérés (D70, D61, D71) ;
- `fs.OpenDirAt` et `fs.DirAt` (`Entry`, `ReadFile` borné et contrôlé au propriétaire, `WriteFile`) : la clé
  de déploiement et `composer.json` (D82) ;
- `fs.CopyTreeAt` et `CopyTreeGuardedAt` : la copie de `site clone`, sous la garde `fs.Guard` (D82, D84) ;
- `fs.OpenParentDir` : un chemin **désigné par l'exploitant** hors du home (`db dump -O`, `db restore -i`),
  parcouru depuis « / », qui ne suit un lien que si seul root a pu le poser (D83).

`fs.Dir` et `fs.DirAt` sont nés de deux lots parallèles et restent à fusionner ; aucun nom ne s'y recouvre.

Les templates (`internal/template`, `embed.FS`) sont surchargeables dans `/etc/ogs/templates`. **Le TLS est
servi par tous les templates de vhost**, pas seulement ceux des sites PHP.

### Secrets et état

`internal/secret` chiffre en AES-256-GCM avec une donnée associée qui lie le secret à sa portée ;
`internal/store` est une base SQLite (`modernc.org/sqlite`, pur Go) migrée au démarrage — **jamais recréée
en silence** : disparue d'un parc initialisé (clé maître présente) qui compte des projets, toute commande qui écrit
refuse en code 5 avant d'écrire (`stateGuard`), `openStore` aussi, jusqu'à `ogs init`, et
seul `cert renew` tourne sur une base en mémoire (`store.OpenMemory`), en le disant (D120) ;
`internal/audit` journalise chaque étape avec un identifiant d'exécution.

### Exécution de commandes

`internal/exec` est le seul chemin vers le système, et aucune commande n'y passe par un shell. Les
processus sont placés dans leur propre groupe et l'annulation tue le groupe entier. `Command.AsUser` passe
par **`setpriv --reuid --regid --init-groups`**, `runuser -u` à défaut, **jamais par `su`** (D122) :
`su -c` ouvre une nouvelle session (setsid) dans un processus fils, hors du groupe que l'annulation tue — une
installation de runtime ou `ogs cron run` interrompus continuaient sans ogs —, et sans `-l` il transmettait
tout l'environnement de root. **Ce setsid était aussi une protection**, qu'ogs refait lui-même
(`exec.procAttr` : `Setsid` sous `AsUser`, voir les pièges). L'argv passe tel quel, l'environnement est construit de toutes pièces
(`exec.userEnv` : `HOME`, `USER`, `LOGNAME`, `SHELL=/bin/sh`, le `PATH` des crons, la langue, puis
`Command.Env` ; le mandataire HTTP, qui peut porter des identifiants, n'est relayé qu'à l'installation
des runtimes par `exec.ProxyEnv`), le répertoire courant est `Dir`, « / » à défaut ; qui veut un shell le nomme
(`/bin/bash -c`). Ce qui pèse ce que pèse une base passe en flux (`Command.Stdout`, `Command.StdinStream`).
`exec.Recorder` (mode préfixé) simule tout, consomme un `StdinStream` dans `Stdin`, et permet aux tests
d'assertion sur les commandes.

### Supervision : deux fichiers dérivés du parc

`internal/provider/monitoring` écrit, dans le plan global, les entrées de Telegraf
(`/etc/telegraf/telegraf.d/ogs.conf`) et les prisons de fail2ban (`/etc/fail2ban/jail.d/ogs.conf`), plus les
comptes de supervision des moteurs (D64–D68). ogs n'installe rien et se tait quand l'outil est absent — le
critère est la présence de son répertoire de fragments. Il n'écrit **aucune destination de métriques** et
**aucun filtre fail2ban** : le jeton d'écriture ne concerne aucun projet, et un filtre est une expression
régulière qu'il faudrait maintenir au rythme des changements de format de journal.

L'agent n'atteint jamais un socket FPM. `web.Provider.PlanStatus` pose un vhost `ogs-status` sur la boucle
locale, qui relaie l'état de chaque pool : **parler au socket d'un pool, c'est pouvoir y faire exécuter du
code sous l'identité du projet**, le protocole FastCGI n'ayant aucune authentification.

### Signature DKIM : des clés scellées, deux tables de parc

`internal/provider/mail` (D69) engendre une clé RSA par domaine d'envoi d'un projet qui la demande
(`spec.mail.dkim`), la **scelle** dans le keystore — état compris, pour qu'archive et restauration la
reprennent entière — et l'écrit pour OpenDKIM ; le plan global écrit la `KeyTable` et la `SigningTable`
(`refile:`, d'où `*@domaine`) depuis le parc entier. Le keystore fait foi : un fichier disparu est
**réécrit**, jamais régénéré, une clé neuve exigeant de republier chez le client ; à l'inverse, un fichier en
place que le keystore ne connaît plus (`state.db` perdue) est **rescellé tel quel**, au seul régime d'ogs
(D108). Un fichier de clé ne part que si les tables en place ne la désignent plus **et** qu'OpenDKIM les a
relues (`releasable`) : `FlushReloads` vide la file, il ne dit pas que le rechargement a réussi. Les deux tables
s'écrivent ensemble ou pas du tout : une SigningTable retouchée refusée pendant que la KeyTable était réécrite
désignait un nom absent de celle-ci, et le courrier restait en file. ogs ne touche ni au MTA,
ni à `opendkim.conf`, ni au DNS ; la rotation (`rotate` puis `activate`) ne bascule qu'une fois
l'enregistrement publié, vérifié par le résolveur.

### Certificats : des jeux d'identifiants DNS choisis par zone

`internal/provider/tls/dns.go` (D97) : un fichier de `/etc/ogs/credentials/dns` par jeu, avec ses `zones`.
Le challenge de chaque nom prend le jeu désigné (`dnsCredentials`), sinon celui de la zone la plus longue,
sinon le jeu sans zone s'il est seul, et `routeDNS` aiguille nom à nom. Une demande **sans challenge
déclaré** dont chaque nom relève d'une zone déclarée passe à DNS-01 (`Request.AutoDNS`) et se replie sur
HTTP-01 (`DNSFallbackError`, code 6) — d'où `FlushReloads` sur ces étapes aussi. **Un jeu sans zone ne
déclenche jamais DNS-01 de lui-même** : l'`ovh.yaml` du wildcard de préproduction ferait tenter DNS-01 à
tout le parc. Un jeu illisible est écarté, jamais bloquant.

### Pare-feu : ufw.yaml, des profils à soi, des règles marquées

`internal/provider/firewall` (D72) réconcilie ufw avec `/etc/ogs/ufw.yaml`, **dont l'absence vaut « ne
touche à rien »**. Il écrit un seul fichier de configuration, `/etc/ufw/applications.d/ogs`, aux profils
`ogs-*` dérivés de ce qui est installé (unités, `sshd_config`, `proftpd.conf`…) ; toute règle passe par la
**commande ufw**, jamais par `user.rules`, que la réconciliation relit seulement — ses lignes
`### tuple ###` sont ce qu'ufw lui-même relit. **Une règle est à ogs si son commentaire commence par `ogs:`**, et à
l'administrateur sinon : ogs ne la touche pas. Les étapes sont non critiques, mais politiques et
activation refusent de s'appliquer si une règle a échoué ; la première activation arme un minuteur
systemd qui désactive ufw faute de `ogs firewall confirm`. Les adresses de confiance rejoignent
l'`ignoreip` des prisons fail2ban (`monitoring.Provider.TrustedIP`). **Un `ufw.yaml` invalide ou refusé
n'arrête pas le plan global** : il y devient une étape non bloquante en échec (`firewall.RefusedStep`),
code 6, et seules les commandes `ogs firewall` rendent le code 3 — sans quoi une faute de frappe dans le
pare-feu priverait `project delete` du recalcul des prisons et de la rotation (D68).

Les gardes contre la coupure (D86) jugent **ce qui restera ouvert** après le plan : les règles voulues de
tout profil qui ouvre les ports visés, et les règles de l'administrateur qu'il laisse en place
(`keptOpens`). Elles ne se lèvent que par **`--allow-lockout`**, propre aux commandes `ogs firewall`
(`plan`, `apply`, `ban`, `unban`) : le `--force` des fichiers retouchés ne les lève jamais, le plan global
jamais non plus — porté jusqu'à lui, il retirait la dernière règle ssh dans un `ogs apply <id> --force` sans
rapport. La première activation pose en outre un témoin, `/var/lib/ogs/ufw-unconfirmed`, et un complément
d'unité, `ufw.service.d/ogs-rollback.conf`, qui laisse ufw inactif au **premier démarrage d'ufw après celui
de la machine** tant que le témoin existe (D109) — jamais à une relance de l'unité ; `confirm` retire l'un
puis l'autre, et `status`, `confirm` et `doctor` les lisent. `ban` et `unban` calculent et gardent le plan
**avant** d'écrire `ufw.yaml` — seuls les gardes et les problèmes qu'ils font naître les arrêtent —, et un
`ufw.yaml` illisible garde l'`ignoreip` des prisons en place (`IgnoreIP` rend une erreur, jamais une liste
vide).

### Surfaces particulières

- `ogs my <verbe>` : surface déléguée à l'utilisateur d'un projet par une règle sudoers énumérée, sans
  métacaractère, validée par `visudo` sur un fichier temporaire. L'appelant est identifié par **`SUDO_UID`**,
  pas `getuid()` — ce qui suppose `env_reset` actif, contrôlé par `doctor`.
- `internal/importer` : reprise d'un serveur existant. Corrélation par indices vérifiables (socket FPM →
  pool → utilisateur, docroot sous le home) ; un projet adopté **garde ses noms réels** — son vhost, son
  fichier (`spec.web.vhosts[].file`). Un nom ne rattache une base que s'il ne désigne personne d'autre, aux
  séparateurs près, jamais par préfixe, et un compte que s'il est seul de son nom, hôte réel compris
  (D78) : le préfixe donnait à un projet la base et le compte d'un voisin, que l'apply accordait et que
  `project delete` supprimait.
- `internal/cli/dryrun.go` tient la conduite de **`--dry-run`** pour chaque commande, sur son chemin
  complet (D88) : simulation — la commande s'arrête avant son hook « pre » et avant sa première écriture —,
  consultation, ou, par défaut, **refus en code 2** avant toute préparation. Une commande nouvelle refuse
  l'option tant qu'on ne lui a pas appris à simuler, qu'on ne l'a pas inscrite et qu'un cas ne l'éprouve
  dans `TestDryRunChaqueCommandeSimuleOuRefuse`.
- `site clone` (D84) recopie `www/` par `fs.CopyTreeGuardedAt` sous une garde (`internal/cli/clone.go`) :
  fichiers de configuration du type retenus (`AppType.Config`, `AppType.Cache`), secrets qu'ogs connaît
  masqués à longueur égale dans la copie comme dans la déclaration, et tout ce qui n'a pas pu être
  neutralisé nommé, code 6.
- `docs/migrer.sh` migre un ancien serveur vers une machine gérée (D90) : script à part, exécuté sur la
  cible, qui ne touche ogs que par ses commandes. Il a son banc, `test/migration`, lu par `make test`.
- `internal/tui` : Bubble Tea derrière une interface `Backend`, testable sans terminal.
- `internal/hook` : hooks de parc (un fichier de `/etc/ogs/hooks` nommé d'après l'événement) ou de projet
  (`spec.hooks`, qui l'emporte — sauf `postCertRenew`, où il s'ajoute à celui du parc, D117). Un hook
  s'exécute en root : il est désigné sous `/etc/ogs/hooks`, et résolu depuis « / » — chaque répertoire
  traversé, lien compris, à root et fermé en écriture au groupe et aux autres ; le fichier réel à root,
  inscriptible par lui seul. Un lien qui sort du répertoire reste admis à ces conditions (`doctor` le
  signale) : refusé, il coupait des hooks en service. Sous `--dry-run`, `hook.Runner` le résout,
  contrôles compris, et ne le lance pas.

## Pièges de plateforme à connaître

Ils ont tous coûté un défaut réel ; les tests unitaires ne les voyaient pas.

- **Home en 0711, répertoires du projet en 0710** (D51) : traverser sans énumérer. Le bit de groupe du home
  ne peut pas être ôté — Unix retient la première classe applicable sans repli, donc un 0701 refuserait la
  traversée à `www-data` *parce qu'il est membre du groupe*. Un seul endroit décide de ces modes —
  `model.defaultDirs` : un provider qui les imposerait aussi ferait osciller le plan entre deux valeurs.
- **Un répertoire root dans le home ne protège rien contre l'utilisateur** : le home lui appartient (D51),
  donc il **renomme** un sous-répertoire root sans privilège et en recrée un à lui. C'est pourquoi les liens
  gérés vivent dans `~/.local/bin`, au compte du projet (D70) — un `.bin/` root ne garantissait rien — et
  pourquoi les journaux vivent sous `/var/log/ogs-projects/<id>/` (D71), chaîne entièrement root, le home
  n'en portant qu'un lien `~/logs` qu'aucun processus root n'emprunte. Corollaire : **quand root
  écrit dans un répertoire de l'utilisateur, il ne suit jamais un lien** — `fs.SymlinkAt`, `LinkStateAt`,
  `RemoveLinkAt` ouvrent chaque composant par `openat` avec `O_NOFOLLOW` et opèrent relativement au
  descripteur, comme les autres primitives de « Fichiers gérés ». Écrire par le nom, c'est laisser
  l'utilisateur choisir où root crée ou supprime ; c'était le cas du lien `.ssh/authorized_keys` jusqu'à D70.
- **Les parents aussi** (D83) : `O_NOFOLLOW` ne protège que le dernier composant. Un parent jugé conforme au
  plan peut être remplacé par un lien avant l'étape de l'enfant — la confirmation d'« ogs apply » en laisse
  tout le temps, un hook `preApply` aussi : `~/.local` remplacé par un lien vers `/usr/local` faisait donner
  `/usr/local/bin` au projet, en tête de son propre PATH, pendant que le plan annonçait « mode 0755 → 0700 »
  pour la cible du lien. Tout sous-répertoire du home passe par `fs.EnsureDirAt`, `SetDirAt` et `LstatAt`,
  depuis la racine du projet, à l'observation, à l'écriture comme à la compensation ; un parent lié fait
  échouer l'étape en le nommant (`DirState.LinkedParent`).
- **Hors apply, rien ne protège root dans le home** (D82) : `user key show|rotate`, `site clone` et
  `php switch` tournent sans le `statDir` du plan. `user key rotate` créait la paire dans la cible d'un
  `.ssh` lié — `/etc/apt/apt.conf.d`, et une exécution en root au prochain `apt-get update` —, `cp -a www/.`
  résolvait un `www` lié et `chown -R` donnait le résultat à la préproduction. Ils passent par des
  descripteurs, jamais par un nom. Un **lien physique** ne se voit pas comme un lien : le propriétaire se
  contrôle sur le descripteur ouvert, et c'est l'uid du compte source qui fait foi, non le compte de liens,
  qu'un `useradd` ramène à 1 en réécrivant `/etc/shadow` par renommage. Tout ce que le locataire fixe se
  borne — la taille (un fichier creux ne lui coûte rien, et root le lisait en mémoire), la profondeur
  (1024 niveaux ; `cp -a` s'arrêtait de lui-même sur `PATH_MAX`), les entrées nommées (50, citées `%q`), les
  données écrites (jamais plus que la source n'en occupait) —, et aucun chemin n'est retenu par niveau : la
  mémoire de root croissait comme le carré de la profondeur. Un binaire que désigne un lien de
  l'utilisateur ne se lance que sous son identité (`Installer.Version`).
- **Une restauration n'écrit jamais par le nom, et ne rend à l'utilisateur que ce qui était à lui** (D81).
  L'apply qui la précède pose dans le home neuf des liens vers le trousseau commun, l'interpréteur et le
  répertoire root des journaux : extraire par le nom réécrivait en root `/opt/default_authorized_keys` ou
  `/usr/bin/php8.x`. L'extraction (`fs.Dir`, création exclusive) remplace ces liens **en tant que liens**,
  sans jamais les traverser, et pose le propriétaire sur chaque descripteur, d'après l'uid que l'en-tête de
  l'archive porte, remappé sur le compte recréé ; tout ce qui n'était pas à l'utilisateur revient à root avec
  son mode — un lien dur vers `/etc/shadow`, posé sous `fs.protected_hardlinks=0`, ne doit pas lui revenir
  lisible. Une archive sans propriétaires se juge sur sa déclaration **avant** normalisation : celle-ci
  retire l'entrée `logs` root, l'ancien `~/logs` revenait à l'utilisateur, et la migration de D71 le
  refusait à chaque apply. Le même parcours par descripteurs vaut pour l'archivage : `filepath.Walk` et
  `os.Open` laissaient l'utilisateur y faire entrer `/etc/ogs/master.key` en substituant un ancêtre.
- **Une restauration ne lance le code du projet qu'après son contenu, et se reprend** (D113). Son premier
  apply passe sous `App.holdServices` : `heldBack` retire du plan l'activation des instances, l'attente de
  disponibilité et le fichier de tâches planifiées, et `postCreate` attend ; un second apply les pose une fois
  l'arborescence et les bases rendues — l'application démarrait sur un home vide, épuisait `StartLimitBurst`,
  et rien ne la relançait. Un témoin (`/var/lib/ogs/restores/<id>.json`) court de la déclaration au dernier
  apply : une erreur entre les deux nomme `--resume` et `project delete`, jamais un retrait sans
  compensation. Les sauvegardes sont rangées `dumps/<moteur>/<base>.sql` : deux bases homonymes sur deux
  moteurs partageaient `dumps/<base>.sql`, et la sauvegarde MySQL était rechargée dans PostgreSQL.
- **Un chemin désigné par l'exploitant** (`db dump -O`, `db restore -i`) est parcouru par
  `fs.OpenParentDir` : un lien n'y est suivi que si seul root a pu le poser — dans un répertoire à root, non
  inscriptible par le groupe ni par les autres, atteint par une chaîne entièrement de ce type —, et seul un
  fichier ordinaire, ou un nom absent pour une sauvegarde, est admis ; un tube nommé bloquait la commande.
  `/dev/stdout`, `/dev/stderr`, `/dev/null` et `/dev/stdin` sont ouverts par leur nom : les trois flux sont des
  liens « magiques » vers `/proc/self/fd`, et `/dev/null` un périphérique caractère. Refuser tout lien aurait cassé `project delete` sur une machine où
  `/var/lib/ogs` en est un.
- **`/var/lib/ogs` en 0751**, pas 0750 : le serveur web doit traverser jusqu'au répertoire du challenge ACME.
- Le client ACME écrit sous **`<webroot>/.well-known/acme-challenge/`** : Apache aliase ce répertoire,
  nginx prend le webroot et y accole l'URI. Les deux directives n'ont pas la même sémantique.
- Les vhosts de proxy nginx emploient `$connection_upgrade`, fourni par le fragment géré
  `/etc/nginx/conf.d/ogs.conf`. Toute variable employée par un vhost doit être définie quelque part — un
  test unitaire le vérifie.
- **Les surcharges de vhost sont incluses par un masque, et c'est le masque qui les rend facultatives**
  (D63) : nginx comme Apache traitent un joker sans correspondance comme un silence, là où un chemin
  littéral introuvable est fatal — d'où `IncludeOptional` et non `Include`, ce dernier échouant sur un
  joker vide. Les contextes offerts sont **déduits du vhost rendu**, jamais d'une table : les templates
  étant surchargeables, une table créerait un répertoire qu'aucun vhost n'inclut, où les fichiers déposés
  seraient ignorés sans un mot. Trois choses que le moteur ne dit pas et que `doctor` porte : ces fichiers
  sont lus par le processus **maître, en root** — même régime que les hooks —, un `.conf` posé hors des
  sous-répertoires de contexte n'est chargé par personne, et un répertoire orphelin garde des fichiers que
  leur auteur croit actifs. Deux pièges de sémantique exposés du même coup : sur nginx une directive
  **dupliquée** est fatale, là où Apache retient la dernière, et un `add_header` dans le contexte du
  docroot **annule tous ceux hérités du `server`** — HSTS compris. Le vhost de redirection n'offre aucun
  point de surcharge : son unique `server` porte le challenge ACME, qu'un `rewrite` déposé là mettrait par
  terre.
- `StartLimitBurst` et `StartLimitIntervalSec` appartiennent à **`[Unit]`** ; dans `[Service]`, systemd les
  ignore en silence — systemd 257 accepte encore `StartLimitBurst` à cet endroit par compatibilité, mais
  plus `StartLimitIntervalSec`.
- **Un validateur se plaint de ce qu'ogs n'a pas écrit** (D117) : `systemd-analyze verify` juge aussi les
  unités tierces et un binaire pas encore déployé (« is not executable »), `logrotate -d` un journal absent,
  `fail2ban-client -t` les prisons de l'administrateur. Refuser sur son code de retour bloquerait le parc :
  seules comptent les lignes localisées dans le fichier d'ogs (`chemin:ligne`), et pour fail2ban la
  différence avec le fichier précédent rendu. À l'inverse, **logrotate ignore une option inconnue en
  code 0** (« warning: … unknown option ») : l'avertissement localisé fait refuser. Le fichier refusé est
  rendu par l'étape qui l'a écrit (`verify.Restore`) ; s'il existait, le refus est toléré
  (`engine.Tolerated`, code 6) — une garde neuve n'arrête pas un site qui tournait.
- **Un `%` dans `Environment=` est un spécificateur** : `p%40ss` ou `%2f` font refuser l'affectation entière
  (« Invalid slot »), la variable manque au service sans autre signe qu'une ligne de journal, et
  `systemd-analyze verify` refuse l'unité. `systemdEnv` double tout `%` qui n'introduit pas un spécificateur
  connu de systemd (D117) ; `%i` reste, mais un spécificateur valide au milieu d'une valeur (`p%nss`) est
  développé comme avant — l'écrire `%%`. `%D`, inconnu de systemd 252, est toujours doublé. Le premier
  apply réécrit, donc redémarre, les unités dont une valeur portait un `%` non spécificateur.
- `ReadWritePaths` d'une unité est **facultatif** (préfixe `-`) : `var/` est créé par l'application, et un
  chemin absent fait échouer le service en 226/NAMESPACE avant toute exécution.
- Les étapes d'unité s'ordonnent par **cycle de vie**, pas par nom : `unit/instance` précède
  `unit/service` dans l'alphabet, ce qui ferait démarrer une instance avant l'écriture de son modèle.
- **Un lien d'activation ne dit pas qu'une instance tourne** (D121) : une instance morte, ou dont le
  démarrage avait échoué, passait pour « active » à jamais. L'observation relit l'état systemd et relance
  ce qui est **`failed`**, jamais ce qui est `inactive` — l'état d'un `service stop` de l'exploitant, que
  l'arrêt d'ogs garantit par `reset-failed` : un worker qui rend 1 sur SIGTERM en sortait `failed`, et l'apply
  le relançait ;
  l'activation démarre avant de poser le lien, et la cible démarre sous `--job-mode=ignore-requirements`,
  sans quoi elle tire ses `Wants=` et relance une instance arrêtée.
- La cible d'un projet doit être liée à `multi-user.target`, sinon les services ne repartent pas après un
  redémarrage.
- **`PartOf` ne propage rien depuis une cible inactive** (D114) : les instances démarrées une à une, la
  cible jamais, `systemctl stop ogs-<id>.target` laissait tourner les workers — `userdel` échouait à la
  suppression, et le redémarrage après une unité modifiée ne relançait rien. Arrêt et redémarrage nomment
  la cible et chaque instance (`unit.Provider.ControlID`), l'arrêt vise ce que systemd tient actif
  (`Running`), et l'activation d'une instance démarre aussi la cible. Deux corollaires : le motif d'un
  projet porte le « @ » (`ogs-<id>-*@*.service`) — sans lui, celui d'un projet `ufw` désarmait
  `ogs-ufw-rollback.timer` —, et relancer une cible tire tous ses `Wants=` : un abandon la démarre sous
  `--job-mode=ignore-requirements`, sans quoi un worker arrêté par l'exploitant repartait. Seul un vrai
  systemd le montre (scénario `systemd`).
- **`project delete` coupe le site avant l'archive** (D114) : vhosts, pools et cron quittent le disque
  avant le dump, gardés en mémoire pour l'archive et pour l'abandon, qui les rend avant le point de
  non-retour. Une étape ajoutée entre la coupure et ce point doit passer par `abandon`.
- Une unité **modifiée** exige un redémarrage : `daemon-reload` relit la définition, il ne relance rien.
  L'intention `systemd:restart-target:<id>` s'en charge, et l'attente de disponibilité la vide avant
  d'attendre.
- Une **application supervisée** est un service comme un autre : le provider applicatif écrit son unité,
  le provider d'unités active et démarre ses instances. Les deux sont nécessaires — l'unité seule ne
  démarre rien.
- `ogs harden` restreint les droits de `/etc` (D56, D85). Trois invariants : il **ne desserre jamais** — un
  répertoire déjà plus strict est laissé tel quel, et le mode posé est l'intersection du constaté et de la
  cible, setgid et sticky conservés — ; il **préserve le bit d'exécution** des fichiers, sans quoi il rendrait
  les hooks inexécutables donc refusés par ogs lui-même ; et il **ne ferme rien sans le groupe du service
  qui lit**. MariaDB lit ses fichiers d'options sous `mysql` et ignore sans un mot celui qu'il ne peut pas
  ouvrir : fermé en root:root, `/etc/mysql` le faisait repartir sur ses défauts, à l'écoute de toutes les
  interfaces. Les workers web ouvrent `.htpasswd` et `RewriteMap` sous `www-data` (500 sinon, `apachectl -t`
  au vert) ; Redis réécrit sa configuration par renommage dans `/etc/redis`, dont il garde le propriétaire.
  Un groupe introuvable fait **refuser** la cible, au lieu d'un « ✓ » qui ne fermait rien ; toute écriture
  passe par un descripteur ouvert sans suivre de lien. `/etc/php` reste ouvert : le PHP en ligne de
  commande des crons y lit son `php.ini`, et le fermer changerait le comportement de PHP sans le moindre
  message. La **racine des homes** y figure aussi, en 0711 (D60) : traversable sans
  être énumérable, jamais récursivement — sous elle, `model.defaultDirs` est le seul juge. Le gain est
  partiel et doit être annoncé comme tel : `/etc/passwd` énumère déjà tous les homes. Et « ne desserre
  jamais » mord ici — un `/home` en 0700 ne rend plus un fichier et reste en place ; c'est `doctor` qui
  le dit.
- **Un rechargement du serveur web peut tuer le serveur.** Sur Debian, `apachectl graceful` lance bien
  `$HTTPD -t` avant de signaler, et refuse une configuration invalide ; mais rien ne garantit la même chose
  pour nginx selon son unité, et entre ce `-t` et le signal le maître relit ce qui a pu changer — un maître
  qui relit une configuration invalide meurt, et tous les sites tombent. D'où la revalidation dans
  `web.Provider.Reload`, en plus de celle de l'écriture — les intentions étant groupées en fin d'apply, un
  fichier référencé a pu disparaître entre les deux —, et le constat `is-active` après.
- **Un redémarrage est un rechargement sans filet** (D118) : le `restart` qui suit le complément d'unité ne
  validait rien, et un maître qui refuse de repartir ne laisse pas l'ancien servir. Il porte les contrôles de
  `Reload` (paires, `-t`, `CheckLogs`), l'observation de 2 s, et refusé, **rend le complément** — sans quoi le
  plan suivant le dirait conforme et ne redémarrerait jamais. De même, **`Listen` sur un port pris passe
  `apachectl -t`** et tue le maître au rechargement : le vhost de supervision exige un port libre
  (`/proc/net/tcp`), et seul lui est refusé.
- Aucune vérification en amont d'un rechargement ne couvre tout : `ProtectHome` dans une unité fait voir
  `/home` en lecture seule **au seul service**, là où ogs écrit sans peine. D'où le constat en aval —
  `web.Provider.Reload` observe le moteur quelques instants après le rechargement (une mort peut suivre
  le retour de la commande), relève le serveur s'il est mort et le signale même alors.
- **L'unité du serveur web est durcie par la distribution** : Ubuntu livre `apache2.service` avec
  `ProtectHome=read-only`, qui montre `/home` en lecture seule **au seul service** — `mount` affiche `rw`,
  root écrit sans peine, et le maître meurt en ouvrant le journal d'un vhost. D'où le complément d'unité
  géré (D59), écrit **avant** les vhosts et suivi d'un **redémarrage** : l'espace de noms est établi au
  démarrage, un `reload` ne le refait pas. Les chemins portent le préfixe `-`, un `ReadWritePaths` absent
  faisant échouer le service en 226/NAMESPACE.
- Avant toute relance du moteur, **acquitter le compteur d'échecs** (`systemctl reset-failed`) : passé
  `StartLimitBurst`, systemd refuse de démarrer et le serveur reste mort alors que la cause est levée.
- **`apachectl -t` ne charge aucune clé TLS, et `graceful` rend la main avant la mort du maître** (D110) :
  une clé recopiée sur un site, puis l'apply d'un projet sans rapport — « Syntax OK », « ↻ rechargement »,
  code 0, et le maître mourait en chargeant la paire (AH02565), tous les sites en 000 ; `is-active` relu
  aussitôt le voyait vivant. Avant tout rechargement, toute validation et tout redémarrage,
  `web.Provider.CheckCertificates` relit les paires que désignent les vhosts activés — achève une écriture
  interrompue, refuse sinon le rechargement en nommant le certificat (code 6) —, et `Reload` observe le
  moteur 2 s (`MainPID`, `NRestarts`, `is-active`) avant de le croire vivant.
- **`apachectl -t` répond « Syntax OK » sans ouvrir un seul journal**, alors que le maître les ouvre tous au
  (re)démarrage et s'arrête sur le premier qui résiste (`AH00019: Unable to open logs, exiting`). Un `/home`
  remonté en lecture seule après une erreur disque a ainsi tué un serveur entier, configuration valide à
  l'appui. `web.Provider.CheckLogs` éprouve les journaux de tous les vhosts gérés avant le rechargement, et
  `doctor` porte le même contrôle.
- **`useradd --no-create-home` ne regarde pas ce qui est déjà là** (D95) : l'arborescence posée ensuite
  donnait au nouveau compte le home qu'un autre avait laissé à cet emplacement, racine chownée sous une
  étape « mode », et le contrôle du propriétaire de la racine à la suppression (D77) voyait alors le compte
  du projet — tout partait. Le critère est **le propriétaire, jamais le contenu** : un home non vide à un
  UID orphelin est ce que laisse un `userdel` à la main ou une saga de création déroulée, et l'apply doit
  le reprendre — mais c'est aussi celui d'un compte supprimé sans `-r`, dont **useradd redonne l'UID** au
  compte suivant : le projet héritait de ses fichiers et, par l'`authorized_keys` régulier que D61 ne
  remplace jamais, de son **accès ssh**. Un home non vide à root ou à un UID orphelin n'est donc repris que
  connu de la base d'état (`KnownHome`) ou réduit au squelette d'ogs (`notSkeleton`).
  `system.Provider.PreexistingHome` refuse une racine à un autre compte existant, ou dont une
  entrée de premier niveau est à un compte ordinaire (`system.ForeignEntries` : plage de `login.defs`, hors
  `www-data`) ; `site create`, `site clone` et `project create` l'éprouvent avant de déposer la
  déclaration, et `project delete` applique le même critère. Sous préfixe, le compte n'existe jamais : le
  contrôle s'y tait. Et **aucun registre ne remplace l'état réel** : les ressources laissées de côté par
  l'import ne sont pas mémorisées, chaque garde lit le système.
- **Ce que le compte laisse hors du home passe à son successeur** (D107) : cron et atd relisent la crontab et
  les tâches at **par le nom**, useradd redonne l'**UID** libéré au compte suivant, et un groupe homonyme
  préexistant était repris avec ses membres — qui recevaient le socket FPM du projet. La suppression archive
  puis retire crontab, tâches at et linger, purge les fichiers de l'UID de `/tmp`, `/var/tmp` et `/dev/shm`
  par descripteur (`system.Provider.PurgeUID`, sans lien ni montage franchi), et ne retire le groupe que s'il
  était le groupe primaire du compte et que personne d'autre ne le tient ; la création refuse un groupe que
  d'autres tiennent (`PreexistingGroup`). Le linger garde `user@<uid>.service`, dont les processus font
  refuser userdel : il est arrêté avant. La purge lit `/proc/self/mountinfo` : un montage lié du **même**
  système de fichiers garde le `st_dev` de sa source, et la purge l'a traversé, effaçant la source. Et
  `webAccess: none` ne retire jamais `www-data` du groupe : un site à docroot (0710) passé de `group` à `none`
  répondait 403 après un apply en code 0.
- `userdel` **refuse un compte dont un processus tourne encore**. À la suppression d'un projet, les
  rechargements de PHP-FPM et du serveur web doivent donc précéder la suppression du compte : ce sont eux
  qui libèrent les workers du pool. L'ordre inverse laissait compte, groupe et home en place — en annonçant
  un succès. Le scénario `suppression` le verrouille, et lui seul le voit : en mode simulé, ni `userdel` ni
  les rechargements n'atteignent le système.
- **FPM refuse de tourner sans pool** (D74) : une version dont le `www.conf` de la distribution a été retiré reste
  arrêtée tant qu'aucun projet ne l'emploie. `php.Provider.Reload` n'est donc pas un simple `systemctl reload` :
  il **démarre et active** une unité arrêtée qui a des pools, **arrête et désactive** celle qui n'en a plus —
  recharger un maître sans pool le tue. Le critère est tout `pool.d/*.conf`, pas seulement ceux d'ogs. Le
  substitut `systemctl` démarrait l'unité sur `disable --now` : le scénario `fpm-sans-pool` l'a révélé.
- **Le `reload` de FPM est un USR2, rendu aussitôt** (D105) : le maître se ré-exécute ensuite, relit toute
  sa configuration — pools de l'administrateur compris — et meurt sur le moindre refus, `Restart=on-failure`
  s'épuisant sur la même cause. Retirer un pool ne passe par aucune écriture, donc par aucun `-t` : un pool
  resté d'un compte supprimé faisait tomber toute la version à la suppression d'un voisin, en code 0. D'où
  `php-fpm -t` avant tout rechargement et le constat après (`is-active` pendant deux secondes, `NRestarts`).
  La rotation des journaux, elle, ne signale plus FPM : PHP ouvre `error_log` à chaque message, FPM le
  `slowlog` à chaque trace, et le `reload … || true` de chaque nuit coupait les requêtes en cours.
- **Une activation s'observe** (D105) : n'émettre `enable` qu'à l'écriture d'un fichier laisse à jamais
  arrêté ce qu'un incident a arrêté — FPM tué par la rotation, timer de renouvellement stoppé. Le plan relit
  `is-active`/`is-enabled` ; « actif » n'a de sens que sous systemd (`/run/systemd/system`), sans quoi le
  substitut des conteneurs ferait osciller le plan. Un timer **désactivé** est signalé, jamais réactivé :
  D89 respecte ce choix.
- **Un déroulage rejoue les rechargements déjà vidés** (D105) : `FlushReloads` a fait charger aux démons ce
  que la compensation défait. Rejoués en fin de déroulage, ils arriveraient après `userdel`, que les workers
  du pool défait font échouer : chaque intention l'est dès la compensation de la dernière étape qui la porte.
- **FPM retient la dernière occurrence d'une directive, sans tenir compte de la casse, et lit son pool en
  root** (D103) : `spec.php.pools[].settings: {user: root}` — ou `User: root` — rendait une seconde ligne
  après celle du template, et le pool tournait en root ; `include` lui fait lire un pool entier ; une valeur portant une fin de ligne y injectait une directive. D'où les clés
  verrouillées (`validate.LockedPoolKey`, sur la clé canonique `validate.CanonicalPoolKey` : identité,
  `listen*` d'accès, `slowlog`, `access.log`, `error_log`, `include`, `prefix`),
  écartées du rendu en code 6, et la grammaire des clés et valeurs (`validate.PoolKeyProblem`), refusée au
  plan du projet visé (code 3) — ni l'une ni l'autre au chargement. Les couches de la machine
  (`php.pool` de config.yaml) sont **vivantes**, à l'inverse du shell : rien n'en est recopié dans une
  déclaration, et la normalisation n'écrit plus de preset — un pool sans preset écrit suit
  `defaultPreset`, une déclaration antérieure garde le `medium` qu'elle porte. `php.Resolve` est le seul
  calcul des réglages : le vhost de supervision lit `PoolPlan.Resolved.Settings`, jamais la déclaration.
- **L'analyseur d'ini de FPM ne rend pas une valeur nue telle quelle** (D115) : `;` commente, `${…}` se
  développe, `& | ~ ( ) !` et les constantes s'évaluent (`a & b` → 0), `no`/`off`/`null` deviennent vides —
  `env[X] = no` fait refuser le pool —, `=` est une erreur de syntaxe. `template.FPMValue` cite toute valeur
  qui l'exige, `$""{` pour `${` (`\${` en tête de chaîne garde la barre avant PHP 8.1), et laisse nues la
  citation de l'exploitant, le booléen d'un flag et l'expression de constantes (`error_reporting` lit une
  chaîne par `atoi` : citée, elle vaut 0). Un `$` en tête d'`env[]` relaie, après l'analyse, la variable
  de ce nom dans l'environnement du maître (`env[HOSTNAME] = $HOSTNAME`, www.conf de Debian) : c'est une
  fonction de FPM, jamais un refus — le refuser rendait le code 3 à des déclarations qui marchaient. Le banc
  `TestFPMLitLesValeursCiteesTellesQuelles` éprouve chaque FPM installé par `php-fpm -tt`.
- **systemd-tmpfiles résout les parents par leur nom, en root** : il refuse un lien au dernier composant,
  jamais aux parents. Une ligne de purge sous un home (`~/var/sessions`) laissait l'utilisateur, qui renomme
  `~/var`, désigner le répertoire `sessions` de son choix. D'où les sessions sous `/var/lib/ogs/sessions/<id>`
  (D115) : toute ligne tmpfiles vise une chaîne entièrement root.
- **`opcache.restrict_api` ne cloisonne pas** : il ne juge que le script appelant. Restreint à la racine de
  chaque projet, le script d'un projet énumère les scripts des voisins et vide le cache commun de la version
  (§20, R4-34).
- **Le shell des comptes de projet est ouvert** — `/bin/bash` (D62), réglable par `user.shell` dans
  `/etc/ogs/config.yaml` et par `spec.user.shell`. Trois points à ne pas défaire : le défaut de la
  configuration est un défaut **de création**, le fichier du projet faisant autorité ensuite — le propager
  réécrirait le shell de tout le parc au prochain apply ; le shell déclaré est **réconcilié** par `usermod`,
  dans la **même étape** que la correction du home, deux étapes de même ressource se marchant dessus dans le
  plan comme dans l'audit ; et un projet **adopté garde son shell réel**, l'import le reprenant du système.
  `internal/exec` ne passe par aucun shell (D122) : qui en veut un le nomme, un parc pouvant avoir refermé
  celui du compte.
- **Un lien symbolique n'a pas de droits propres** sous Linux : son mode est 0777 et immuable. Le
  `.ssh/authorized_keys` lié au trousseau commun `/opt/default_authorized_keys` (D61) en dépend
  entièrement — sshd **résout le lien** avant tout contrôle, donc ce sont les droits de la cible qui
  décident, et le mode du lien ne peut pas servir de critère de dérive. Trois conséquences : sous
  `StrictModes`, un trousseau n'appartenant pas à root ou inscriptible au-delà de son propriétaire est ignoré
  **en silence** ; il est lu sous l'identité de l'utilisateur du projet, donc il doit **lui être lisible**
  (0644 root:root) ; et c'est `lchown`, jamais `chown`, qui pose le propriétaire du lien — suivre le lien
  donnerait le trousseau commun à l'utilisateur du dernier projet appliqué. `doctor` porte ces contrôles,
  qu'aucune vérification de configuration ne voit. Un `authorized_keys` régulier déjà présent n'est **jamais**
  remplacé : il porte l'accès de quelqu'un.
- **Une prison fail2ban dont aucun journal n'existe empêche le démon de démarrer**, et il s'arrête alors
  tout entier — les prisons valides comprises. D'où trois choses : les journaux sont énumérés un à un et
  filtrés par existence à l'écriture, le fichier est **recalculé à la suppression d'un projet** (sans quoi
  retirer un site cassait fail2ban au prochain redémarrage, sans un mot), et `doctor` nomme les journaux
  disparus. Le même raisonnement décide du backend de la prison sshd : `/var/log/auth.log` n'existe plus
  sur une Debian sans rsyslog, mais le backend `systemd` exige `python3-systemd`, qui n'est qu'une
  recommandation du paquet — on choisit d'après ce qui existe, en préférant le fichier.
- **Apache traite les sections `<Location>` dans l'ordre du fichier**, non par spécificité. Un
  `<Location />  Require all denied` placé en dernier annule les autorisations accordées plus haut : le
  vhost paraît correct et répond 403 à tout le monde.
- **ogs n'active jamais les `.htaccess` sous Apache** (`AllowOverride None`, D99), et porte les règles utiles
  dans le template comme sous nginx. La raison : `www-data` sert tous les projets, et un `.htaccess` écrit par
  le compte du projet pouvait détourner le serveur hors de son site par un **simple `RewriteRule`** —
  `[H=server-status]` (activité du parc), `[H=proxy:unix:…/<voisin>.sock|fcgi://localhost]` (FPM d'un voisin),
  `[P]` vers `unix:…<voisin>.sock|fcgi://localhost/tmp/x.php` (exécution d'un script choisi, `/tmp` étant dans
  l'`open_basedir` du voisin). Une borne par `AllowOverrideList` **ne suffit pas** : elle contrôle quelles
  directives sont admises, pas les drapeaux d'un `RewriteRule` — que Prestashop emploie légitimement —, et
  `<Proxy> Require all denied` ne filtre pas `[P]` (vérifié sur le vrai Apache). Un `AllowOverrideList` qui
  oublie une directive livrée (p. ex. `AddOutputFilterByType`, bloc « Optimisations Apache » de Prestashop)
  fait en outre répondre **500 à toute la boutique**, sans que `apply` ni `doctor` le voient. D'où le choix
  d'ignorer le `.htaccess`. Reprendre ses règles se fait aux sources réelles : `Tools::generateHtaccess` (le
  `.htaccess` racine) et les 32 `.htaccess` de sous-répertoires, à traduire en `RewriteRule` dans le
  `<Directory>` du docroot (mêmes sémantiques qu'en `.htaccess`) et en `RedirectMatch 403` — **listes blanches
  par extension comprises** (`img/`, `themes/`, `js/`) et exceptions (`docs/csv_import`, `mails/themes`,
  `translations/cldr`, `tools/` hors `profiling`), faute de quoi un portage « par répertoires » sert ce que
  Prestashop refusait (`themes/**/config/theme.yml`, un `.php` de thème exécuté). Les images de produit ont
  **une règle par longueur d'identifiant, 1 à 7 chiffres** ; le répertoire d'administration, renommé, se
  reconnaît à son `cron_currency_rates.php` — `get-file-admin.php` n'existe plus en 9, et ses sauvegardes
  repassaient en 200. **Les `.htaccess` livrés diffèrent d'une branche à l'autre** (1.7, 8, 9 : images,
  listes blanches, `upload/`, scripts de `modules/`, `admin-api/`) : le template les rend d'après la version
  lue au rendu dans `src/Core/Version.php` ou `app/AppKernel.php` (par descripteur), la 8 pour une version
  indéterminée — refuser les `.php` de modules d'une 8 casserait ses appels ajax. Sous-répertoire
  (`REWRITEBASE`) et URI virtuelles ne sont pas repris : le plan et `doctor` les annoncent. Une règle de ce
  template ne se juge qu'**URL par URL contre les `.htaccess` livrés actifs**, sur un vrai Apache et un vrai
  nginx : c'est ainsi qu'est apparu qu'Apache rend 403 à un fichier absent sous un répertoire absent d'`img/`
  ou de `js/`, son `<Files>` jugeant le premier composant manquant et non le nom demandé, et qu'un refus de
  `^/[^/]+/(backups|export|import)` sans condition coupait des URL propres (`/fr/import/12-x.html`,
  `/module/export/ajax`) : il ne vaut que sous un **répertoire réel**. Sous nginx, ce contrôle vit au niveau
  du `server` (`if (-d $var)`) : une location regex qui le porterait devrait inclure les surcharges du docroot
  (D63), où une location imbriquée est fatale ; et la variable part d'un chemin absolu qui ne peut être un
  répertoire (`/dev/null/ogs`). **Sous `disable_symlinks`** (`on` ou `if_not_owner`, avec ou sans `from=`),
  nginx ouvre le chemin composant par composant et, d'un chemin vide, éprouve le répertoire de départ :
  `if (-d $v)` est vrai pour `$v` vide, et **tout** le site répondait 403. Sans `disable_symlinks`, faux —
  un essai sur un nginx nu ne le montre pas. Les tests émulent l'assertion négative de PCRE (`correspond`) : elle
  doit **finir** le motif, et un `(?i)` de tête vaut pour elle. Reste ouverte la course `SymLinksIfOwnerMatch`
  de D87, qui tient à l'identité `www-data` (D25, §20).
- **`mod_proxy_fcgi` dérive `SCRIPT_NAME` de l'URI demandée**, non du chemin de la cible `fcgi://`. Un
  `ProxyPass /x unix:…|fcgi://localhost/ogs-fpm-status` ne suffit donc pas : FPM ne reconnaît pas son
  `pm.status_path`, cherche un fichier et répond « Primary script unknown ». `ProxyFCGISetEnvIf` réécrit la
  variable. Nginx n'a pas ce problème — mais son `fastcgi_param SCRIPT_NAME` doit venir **après**
  `include fastcgi_params`, qui le fixe.
- **MariaDB rend « BINLOG MONITOR » là où MySQL rend « REPLICATION CLIENT »** : le GRANT est accepté sous
  les deux noms, la relecture n'en rend qu'un. Comparer au seul libellé de MySQL faisait annoncer
  éternellement une correction qu'on venait d'appliquer.
- **Un privilège global ne ressort pas de `SCHEMA_PRIVILEGES`** : cette vue est alimentée par `mysql.db` et
  ne porte que le niveau base. `PROCESS` et `REPLICATION CLIENT`, accordés `ON *.*`, vivent dans
  `mysql.user` et ne se relisent que par `USER_PRIVILEGES` ou `SHOW GRANTS`. Les chercher dans la mauvaise
  table rendait le compte de supervision **structurellement** non conforme : le même défaut d'oscillation
  que ci-dessus, déplacé d'un cran. Corollaire du même endroit : un mot de passe scellé dans le keystore
  n'est pas un mot de passe posé sur le moteur — les deux ne se comparent pas, donc la création du secret
  doit forcer l'`ALTER USER`, sans quoi le fragment Telegraf porte un mot de passe que le moteur ignore.
- **Une section `[DEFAULT]` dans `jail.d` n'est pas locale à son fichier** : fail2ban fusionne `jail.conf`,
  `jail.d/*.conf` et `jail.local` dans un même analyseur, et la section commune s'impose alors à *toutes*
  les prisons de la machine, celles de l'administrateur comprises. Le plus grave est `ignoreip`, qui est
  **remplacé et non complété** : la liste blanche d'administration disparaît, et quelques échecs
  d'authentification suffisent à verrouiller l'administrateur hors de son propre serveur. Le préfixe
  `ogs-` protège les noms de prisons, pas les réglages communs — d'où des réglages portés prison par prison.
- **Une clé DKIM refusée ne laisse pas partir le courrier sans signature** : OpenDKIM rend une erreur
  temporaire et le MTA garde en file **tout** le courrier du domaine. Une clé lisible au-delà de son
  propriétaire l'est (`RequireSafeKeys`), une clé absente aussi. D'où trois règles : clés en 0600 au compte
  d'OpenDKIM, jamais une ligne de table pour un fichier manquant, et le fichier d'un domaine qui cesse
  d'être signé retiré **après** le rechargement des tables (`FlushReloads`) — OpenDKIM garde en mémoire
  celles qu'il a chargées. Le fichier d'une clé, lui, est relu à chaque message : l'écrire ne demande
  aucun rechargement. Le critère de présence est `/etc/opendkim.conf` : le paquet Debian ne crée pas
  `/etc/opendkim`.
- **Les journaux d'un projet sont pré-créés** (D71) : le répertoire n'est inscriptible que par root, donc
  un worker PHP ne peut pas créer `php-error.log` — PHP se rabat alors, sans un mot, sur le journal global
  de FPM —, et un maître root crée les siens avec son umask. Ne **jamais** rendre ce répertoire inscriptible
  au groupe du projet : logrotate refuserait de le traiter sans `su`, et la substitution que D71 ferme
  redeviendrait possible. Une migration de journaux doit **recharger** les serveurs même si aucun vhost ne
  change : ils écrivent par leur descripteur dans le fichier qu'ils ont ouvert.
- **Deux juges ne doivent jamais régler le même chemin** : ogs pose `/etc/opendkim` et ses tables au
  régime qu'`ogs harden` exige (0750 / 0640 root:opendkim). `fs.EnsureDir` repose le mode d'un répertoire
  existant : en désaccord, l'apply rouvrirait à chaque passage ce que harden referme.
- **Une création défaite doit retirer sa déclaration** (D112) : `site create` dont la saga déroulait tout
  laissait `/etc/ogs/projects/<id>.yaml`, que le plan global lisait aussitôt — la ligne tmpfiles d'un compte
  inexistant faisait rendre 65 à `systemd-tmpfiles`, et l'apply suivant d'un voisin 6 (« Failed to resolve
  user »), vu sur un vrai système et invisible en mode préfixé, où rien n'exécute `systemd-tmpfiles`. Seule
  une création déroulée **sans reste** (ni compensation en échec, ni étape résiduelle) la retire : sinon le
  projet a laissé quelque chose, et c'est `project delete` qui le démonte.
- **`systemd-tmpfiles --create` ne reçoit que le fichier d'ogs** : sans argument, il traite toute la
  configuration de la machine, crée ce qu'elle déclare et rend 65 sur une ligne en double d'un fichier de
  la distribution.
- **Toute intention de rechargement doit être routée** par `reloadFunc`, et un rechargement en échec doit
  être **affiché** (`Options.OnReloadWarning`), pas seulement compté. L'intention nue `web`, émise depuis le
  lot L0 à l'ajout de `www-data` au groupe d'un projet, n'était routée nulle part : chaque création de
  projet rendait le code 6 sur un avertissement invisible. `TestToutesLesIntentionsDeRechargementSontRoutees`
  le verrouille.
- **ufw refuse l'apostrophe droite dans un commentaire** — le français en est plein : `SanitizeUFWComment`
  la remplace par l'apostrophe typographique. Il **remplace en place** le commentaire d'une règle
  identique, mais `ufw prepend` **ignore** une règle existante : un commentaire se corrige par la commande
  d'ajout ordinaire, jamais par `prepend`. Un refus doit précéder toute autorisation plus large — ufw
  retient la première règle qui correspond —, d'où `prepend` à la pose et le replacement en tête d'un
  refus relu trop bas.
- **Une règle d'application porte les ports du profil au moment de sa pose** : réécrire le profil ne la
  change pas, d'où `ufw app update` pour chaque profil dont les ports changent. Et ufw **refuse une
  application inconnue jusque dans `ufw delete`** : la règle d'un profil qui disparaît part *avant* le
  fichier de profils, et un profil qu'une règle de l'administrateur emploie reste défini. Un profil dont
  une liste ou une plage de ports n'a pas de protocole est **écarté d'un simple avertissement** : la
  validation l'exige.
- **Un réseau aux bits d'hôte non nuls ne se relit pas comme il s'écrit** : ufw ramène `10.0.0.5/24` à
  `10.0.0.0/24` en IPv4 mais le garde tel quel en IPv6 — le plan reposerait la règle à chaque apply. La
  validation le refuse. Les adresses relues sont comparées **après analyse**, jamais comme chaînes.
- **Tout ce qui retire un accès se garde**, pas seulement l'activation : retirer l'ancienne règle quand sa
  remplaçante a échoué déplace l'accès nulle part. Et une règle de l'administrateur **de même définition**
  qu'une règle voulue ne reçoit jamais la marque d'ogs — ufw remplaçant le commentaire en place, ogs la
  retirerait le jour où ufw.yaml cesserait de la demander.
- **sudo efface `SSH_CONNECTION`** (`env_reset`, que `doctor` exige) : la garde contre la coupure de la
  session la cherche dans l'environnement des processus parents, et compare le profil ssh aux ports où
  sshd **écoute réellement** (`ss -tlnp`, et les `ListenStream` d'une `ssh.socket` active : sous l'activation
  par socket d'Ubuntu, systemd tient seul le port jusqu'à la première connexion, et `ss` n'y montre aucun sshd) — `Port=2222`, `ListenAddress hôte:port`, aucune lecture de
  `sshd_config` ne couvre tout. Sans elle, un pare-feu mal écrit coupe
  la session qui l'active, et plus rien de ce qu'elle a lancé ne tourne pour réparer — d'où le minuteur
  systemd, qui ne dépend pas d'elle.
- **`EnvironmentFile=` est lu par systemd en root**, avant le changement d'identité : un fichier que
  l'utilisateur d'un projet peut remplacer par un lien lui ferait injecter les secrets d'un voisin. D'où
  l'emplacement imposé du fichier d'environnement applicatif, `/etc/ogs/env/<id>.env` (D75), en 0600
  root:root — le régime d'`ogs harden` sous `/etc/ogs` —, un lien à sa place refusé, et son contenu jamais
  réécrit par ogs. Il l'emporte sur les `Environment=` de l'unité, `PORT` compris.
- **`Environment=` se découpe sur les blancs** : `K=deux mots` définit `K=deux` et écarte `mots` sans autre
  signe qu'une ligne de journal. La fonction de template `systemdEnv` cite l'affectation **seulement si la
  valeur l'exige** — une valeur simple garde son rendu, et l'unité n'est ni réécrite ni redémarrée. Un `%`
  reste un spécificateur s'il en introduit un (`INSTANCE=%i`), il est doublé sinon (D117).
- **Un port de la machine au-dessus de 1024 est à qui le prend** : broker ou plugin de management
  arrêté, l'utilisateur d'un projet ouvre `127.0.0.1:15672` et reçoit l'authentification Basic de
  l'administrateur du broker. Le client vérifie donc **l'autre bout de chaque connexion** (D93) — le socket
  accepté, cherché dans `/proc/net/tcp{,6}` par ses deux adresses, hérite de l'uid du socket d'écoute —, et
  n'admet que root et le compte du broker, quelle que soit l'adresse qui désigne la machine. Relire l'écoute
  avant de se connecter laisserait une fenêtre. Un socket introuvable vers la machine est une redirection
  DNAT (Docker sans userland-proxy) : refus. `/proc/net/tcp` écrit les adresses par mots de 32 bits
  **dans l'ordre de la machine**, et une connexion IPv4 vers un serveur en `::` y figure sous sa forme
  projetée. Les tests tiennent leurs faux brokers sous leur propre compte : ils le déclarent en
  `brokerAccount`.
- **Sur un moteur partagé, un secret scellé est la preuve de propriété — une par ressource** (D93) : un
  vhost, un compte AMQP ou un compte de supervision homonymes sans elle appartiennent à l'administrateur.
  Les « réparer », c'était leur poser un mot de passe neuf, puis les détruire à la suppression, messages
  compris. Le mot de passe d'un compte ne prouve pas son vhost : `spec.amqp.vhost` peut désigner après
  coup celui d'un autre, d'où la marque `amqp-vhost/<vhost>`. Le critère du parc antérieur (permissions du
  compte sur le vhost) ne vaut que pour un projet **sans aucune marque**, et le premier apply la pose : sans
  borne, une permission accordée par l'administrateur sur son vhost commun suffisait à le faire adopter.
  Trois corollaires : la preuve est scellée
  **avant** la création (une pose interrompue laisserait sinon une ressource d'ogs refusée pour toujours),
  elle part avec la ressource (sinon elle ferait adopter l'homonyme créé ensuite), et, scellée avant, elle
  ne dit plus que le moteur a le compte — le fragment de Telegraf attend de l'avoir observé.
- Telegraf n'a **pas d'équivalent d'`apachectl -t`** : son option de test exécute réellement la collecte.
  Le rechargement se contente d'un constat en aval. Et un fragment que l'agent ne peut pas lire ne produit
  **aucune erreur** — il démarre, ne collecte rien, et se tait ; d'où le contrôle de `doctor`.
- Apache a besoin de sept modules (`ssl`, `proxy`, `proxy_fcgi`, `proxy_http`, `rewrite`, `headers`,
  `status`) ; sans eux la configuration est acceptée puis rend une 500 illisible — ou, pour `status`, une
  page vide. `doctor` les énumère.
- **Une base ou un compte SGBD n'est à ogs que s'il l'a créé** (D91) : un compte, si son mot de passe est
  scellé dans la portée du projet ; une base, si ce compte y a ses droits. `CREATE … IF NOT EXISTS` ne dit
  rien de l'appartenance — il reprenait en silence l'homonyme d'un site resté hors d'ogs, que la
  suppression du projet emportait ensuite. Hors adoption, l'homonyme est refusé à la création et laissé à
  la suppression ; une compensation ne retire que ce que son étape a créé, jamais le compte qu'une autre
  base du projet emploie. Sous MySQL, **compte, GRANT, puis base** : le GRANT est accepté sur une base
  absente, et une base créée d'abord restait, sur un `CREATE USER` refusé, sans droits donc étrangère —
  une étape en échec n'est jamais compensée. Et **`ALTER USER 'root'@'localhost' IDENTIFIED BY` remplace,
  sous MariaDB, l'authentification `unix_socket`** par laquelle ogs administre le moteur : d'où les
  comptes réservés (`validate.ReservedDBAccount`), refusés aux points d'écriture d'une déclaration,
  **jamais au chargement** — un seul projet fautif rendait le code 3 à tout le parc, `cert renew` du timer
  compris —, et qu'aucune suppression ne retire.
- MySQL : le nom de base après `ON` est un motif où `_` est un joker, donc échappé — **et déséchappé à la
  relecture**, sans quoi toute base contenant un souligné reste éternellement « en écart ». Le nom
  d'utilisateur, lui, est un littéral. **Un motif à joker se relit à part** (D111) : le `GRANT` non échappé
  `` `shop_fr` `` — de l'administrateur d'un serveur repris, ou d'une ancienne version — et le
  `` `shop\_fr` `` d'ogs tombaient sous la même clé, le dernier relu l'emportant — le plan révoquait à chaque
  apply le `GRANT` échappé, jamais le joker. Le joker part après le `GRANT` échappé, sauf s'il couvre une
  autre base existante, dont il porte l'accès (`doctor` le nomme) ; compte de supervision compris. Il
  se retire **par les privilèges relus**, `GRANT OPTION` compris quand `IS_GRANTABLE` le dit
  (`SCHEMA_PRIVILEGES` ne le liste jamais : la ligne survivait, relue « USAGE »), jamais `ALL` : MariaDB refuse à root `REVOKE ALL PRIVILEGES`
  sur `performance_schema` (ERROR 1044), et la ligne restait, code 6 à chaque apply.
- **Root ne se connecte qu'au socket du vrai moteur** (D92). `/tmp` figurait parmi les emplacements
  cherchés : tout compte local y pose `mysql.sock` ou `.s.PGSQL.5432`, et le jour où le moteur est arrêté,
  root s'y connectait — mots de passe du parc en clair dans les `CREATE USER`, contenu des bases d'un clone.
  `checkSocket` exige un socket et un répertoire à root ou au compte du moteur, inscriptible par nul autre ;
  un cluster dont le socket est refusé ne retombe **jamais** sur `/var/run/postgresql` (`Cluster.SocketDir`).
- **Une restauration est un script, et le client l'exécute** (D92) : `\!` lance une commande shell sous
  mysql comme sous psql. `db restore`, `site clone` et `project restore` chargent donc sous le **compte de
  la base**, par un client lancé **sous l'utilisateur Linux du projet** (`AsUser`), environnement construit
  de toutes pièces (`exec.userEnv`, D122) — `su` sans `-l` gardait `MYSQL_PWD` et `PGPASSWORD`. Deux conséquences : une clause
  `DEFINER` nommant un autre compte exige `SET_USER_ID`, d'où son retrait ; et
  `COMMENT ON EXTENSION` exige d'être propriétaire de l'extension, créée sous le rôle d'administration,
  d'où le retrait de la seule instruction que pg_dump pose sous son en-tête — sous `ON_ERROR_STOP`, la
  restauration s'y arrêtait. Ces retraits ne touchent que la **tête d'une ligne de définition** :
  mariadb-dump écrit chaque tuple sur sa propre ligne, et un retrait « hors des lignes `INSERT` »
  corrompait les données d'un clone en silence. Le mot de passe ne va qu'au vrai moteur : socket admis
  exigé, et toute écoute TCP de la boucle locale sur le port au compte du moteur (`/proc/net/tcp*`) — le
  port d'un moteur arrêté est à quiconque s'y lie ; une écoute sur `::1` ne reçoit pas `127.0.0.1`, et
  PostgreSQL pose `V6ONLY` sur ses écoutes IPv6. Un compte additionnel `all` ne restaure **jamais sous
  l'utilisateur du projet**, qui lirait son mot de passe dans l'environnement du client : `systemd-run -p
  DynamicUser=yes`, qui exige D-Bus — absent de l'image d'intégration, où `systemd-run` échoue sur le bus.
- **PostgreSQL : le compte principal est propriétaire de sa base**, et la propriété emporte tous les droits.
  `crud` ou `readonly` sur lui étaient annoncés par le plan et sans effet : ils sont refusés (D92), le jeu
  borné allant sur un compte additionnel. Ne pas le déplacer dans `validate` : `loadProjects` rejette le
  premier projet invalide, et une déclaration ancienne bloquerait tout le parc.
- PostgreSQL : l'authentification `peer` exige un rôle au nom de l'utilisateur système (`root`), et
  `CONNECT` doit être révoqué à `PUBLIC` — il est accordé par défaut sur toute base nouvelle. Créer depuis
  `template0`, jamais `template1`. **La révocation se juge sur l'ACL relue** (`datacl`, nulle = ouverte à
  `PUBLIC`) et **ne vaut que pour une base créée par ogs** (D111) : juger sur la seule propriété laissait un
  `GRANT CONNECT … TO PUBLIC` invisible, et l'exiger d'une base adoptée — dont l'import déclare le rôle
  d'après le nom, pas d'après le propriétaire — faisait repasser l'étape à chaque apply, schéma `public`
  donné au rôle déclaré et autres rôles coupés. Une base adoptée ouverte est signalée (plan, `doctor`).
- **Un mot de passe n'est scellé que posé** (D91, D111) : le compte existant d'un projet adopté sans
  secret ne reçoit ni mot de passe ni `CREATE USER` — `IF NOT EXISTS` ne posait pas celui qu'ogs tirait et
  publiait. Recréé (restauration après `delete --force`), il est **toujours** annoncé en code 6 : avec un mot de
  passe neuf, ou avec le secret scellé — qu'un apply antérieur à D111 a pu sceller sans le poser, et que
  l'archive réinjecte.
- **Deux comptes MySQL du même nom sous deux hôtes sont deux comptes**, et MySQL retient pour chaque
  connexion celui dont l'hôte est le plus précis. Un `'x'@'localhost'` créé à côté de `'x'@'%'` fait refuser
  le mot de passe de l'application, par le socket comme par 127.0.0.1 : l'adoption coupait l'hôte et
  déclarait `localhost`. L'import déclare l'hôte réel (D78), et le provider refuse de créer un homonyme sous
  un autre hôte — étape non critique, rien d'exécuté ni de scellé —, les déclarations écrites avant la
  correction portant encore `host: localhost`.
- **Un vhost nginx s'appelle souvent `shop.fr`, sans extension** (usage Debian) : le nom du vhost suivi de
  `.conf` ne le désigne pas. Le nom tronqué rangeait `shop.fr` et `shop.de` parmi les obsolètes d'un même
  projet, retirés avec le site voisin, et `project delete` laissait le vhost adopté en place, en code 0. Un
  vhost adopté porte son fichier réel dans `spec.web.vhosts[].file` ; tout code qui compose le chemin d'un
  vhost passe par `web.Provider.VHostFiles` ou `model.Project.VHostFile`, jamais par
  `Engine.FileName(VHostNames)`. La liste des vhosts et le nom de leurs fichiers ont une seule source,
  `model.Project` : le contrôle d'unicité la lit comme le provider.
- **Une suppression ne retire que ce qu'ogs a écrit pour le projet, et ce qu'elle laisse peut désigner ce
  qu'elle efface** (D77). Le vhost d'origine d'un site adopté journalise sous le home, son pool tourne sous
  le compte : effacer le home en laissant ces fichiers rendait la configuration d'Apache invalide, et le
  rechargement suivant faisait tomber le parc ; un pool partagé avec un site voisin le faisait tomber en 502.
  D'où un refus, prononcé **avant la confirmation**, avec tous les autres d'un seul coup : un refus qui en
  masque un autre fait suivre un remède coûteux — retirer le vhost, c'est couper le site — avant d'apprendre
  le suivant, et un refus qui tombe après le point de non-retour ne sauve plus que ce qu'il garde.
- **`os.RemoveAll` franchit les points de montage**, et seule `/proc/self/mountinfo` montre un montage lié :
  un partage NFS ou un `mount --bind` sous le home était vidé pour tous ceux qui s'en servent. La racine et
  les journaux sont éprouvés à l'entrée de `project delete`, au point de non-retour, puis juste avant
  l'effacement.
- **Une commande de projet ne doit jamais garder le terminal de contrôle de root** (D122). `setpriv`
  change d'identité sans changer de session : la commande ouvrait `/dev/tty` et y poussait par
  `ioctl(TIOCSTI)` une ligne que le shell root exécutait au retour d'ogs (noyau < 6.2, ou
  `dev.tty.legacy_tiocsti=1`) — par `cron run`, par le `nvm.sh` qu'un runtime source, par le `\!` d'un
  dump restauré. Le `setsid` de `su -c` était la protection d'util-linux contre cela ; remplacer `su` l'a
  retirée. Sous `AsUser`, `exec.Local` lance donc la commande en **nouvelle session** (`Setsid`, son
  groupe a son pid : l'annulation l'atteint toujours) et ne lui passe **aucun descripteur de root** — un
  `*os.File` en sortie est masqué pour qu'os/exec pose un tube, et refusé en entrée : la copie d'un
  terminal ou d'un tube jamais fermé bloquait `Wait` au-delà de `WaitDelay`. `TestAsUserSansTerminalDeControle` le vérifie en
  root, sous `docker run -t`. Une commande root (`journalctl --no-pager`) garde la session d'ogs.
- **Une interruption annule le contexte, elle n'arrête pas le processus** (`cmd/ogs/signaux.go` : SIGINT,
  SIGTERM et **SIGHUP**, D122 — la session ssh coupée tuait ogs au milieu d'une saga). Un signal ignoré au
  lancement le reste (`nohup` : Go le rétablit sinon dès qu'on s'y abonne), un second est dit, et SIGPIPE
  est capté **sans** annuler — Go tue sinon le programme qui écrit vers une sortie fermée sur les
  descripteurs 1 et 2, et un client SGBD qui s'arrête lève le même signal sur son entrée. Une question à
  l'exploitant (`readAnswer`) rend la main dès l'interruption. Une commande longue qui franchit un point de non-retour consulte
  `ctx.Err()` juste avant, et y voit un abandon (code 9) qui défait ce qu'elle a arrêté ; au-delà, elle se
  détache (`context.WithoutCancel`), sans quoi chaque commande échoue sur « context canceled ».
  `project delete` laissait ainsi un home effacé sous un compte et des workers FPM vivants, et des fichiers
  de parc jamais recalculés. Une copie faite dans le processus consulte le contexte avant chaque entrée et
  chaque bloc : `cp -a` mourait avec son groupe de processus, la copie de `site clone` non.
- **Le rendu TLS d'un vhost suit le certificat présent, jamais la demande** (D79). Juger sur la demande
  faisait redescendre le vhost en HTTP seul dès qu'un alias était ajouté, rechargé ainsi avant même le
  challenge, et il le restait si l'émission échouait — HSTS rendait le site injoignable. Le bloc 443 est
  rendu dès que le certificat présent et non expiré couvre l'un des noms, et la bascule compare le rendu au
  fichier en place. Un certificat réémis n'est présenté qu'après rechargement : l'étape d'émission porte
  l'intention (`tls.Provider.WebReload`), et `cert renew` recharge dès qu'un certificat a changé, même si un
  autre a échoué.
- **Un certificat ne quitte le disque que si aucun vhost écrit ne le désigne** (D110) : la compensation
  d'une émission retirait le certificat neuf pendant que la bascule TLS, sans compensation, laissait le bloc
  443 qui le désigne — le moteur refusait la configuration au rechargement suivant, le parc avec ce site. La
  bascule rend le fichier d'avant, et `cert delete` comme la compensation lisent les vhosts écrits
  (`web.Provider.CertificateReferences`), jamais les seules déclarations. Clé et certificat s'écrivent sous
  le verrou du répertoire (`flock`), noms d'attente puis renommages, et toute lecture vérifie qu'ils
  s'appareillent : `Store.Read` tient une paire dépareillée pour absente — le moteur la refuserait avec
  toute sa configuration —, `ReadPair` la dit dépareillée au plan. `cert renew` ne prend pas le verrou
  global : deux émissions concurrentes entrelaçaient leurs fichiers en quelques tours. **La paire se
  rétablit avant les vhosts** (`tls.Provider.PlanPairs`) : placée à l'étape d'émission, après le web, la
  réparation n'était jamais atteinte — l'étape web réécrivait le premier vhost en HTTP seul, `nginx -t`
  lisait le second, la redirection du même certificat, qui désignait toujours la paire, et la saga
  déroulait l'apply en code 1. **`apachectl -t` ne voit pas une paire dépareillée** (« Syntax OK ») : le
  maître meurt au rechargement (AH02565) ; une règle sur la paire se juge donc sous les deux moteurs. Une
  écriture interrompue que les fichiers en attente achèvent se lit comme achevée, et `acme.staging` ne
  remplace jamais un certificat de production au renouvellement : le timer le faisait, sans annonce, HSTS
  compris.
- **Une commande ACME échoue tout entière sur un seul nom refusé** (D80) : une redirection, couverte par le
  certificat du projet, dont le DNS pointe ailleurs empêchait sinon le renouvellement du site. L'émission se
  replie par paliers ; le repli sur les seuls noms déjà couverts laissait un alias ajouté à jamais sans
  certificat, et loin de l'échéance aucun palier ne retire un nom servi. Un nom qu'aucune demande ACME ne
  peut porter — une adresse IP en redirection — n'entre jamais dans la demande : il la faisait refuser
  entière, et `cert renew --all` passait le projet par un `continue` muet, le timer rendant 0 pendant que le
  certificat expirait. Le renouvellement du parc ne passe jamais un projet en silence.
- **Changer les ports d'un profil qu'une règle emploie est un resserrement** (D86) : `ufw app update` ferme
  l'ancien port. Un `Port 2222 # annoté` — sshd coupe au « # » qui ouvre un mot — faisait retomber le profil
  à 22 et fermait le port où sshd écoutait, sans garde et en code 0. La garde des ports où sshd écoute ne
  dépend pas de la disponibilité du profil : un profil ssh écarté ou non détecté perd ses règles.
- **Deux questions, selon qu'une session est à protéger** (D86). Sans session, la garde demande si le port
  restera ouvert du dehors : une règle de l'administrateur liée à une interface ou vers une adresse du
  serveur y répond oui — la compter pour rien refusait, depuis la console, chaque bannissement de qui
  administre par un VPN. Pour une session, elle demande si la suivante passera par le même chemin : une
  règle vers une adresse du serveur se compare au 3e champ de `SSH_CONNECTION` (vu après translation, comme
  ufw le voit), une règle liée à une interface ne compte pas, et le refus le dit. Le port de la session ne
  vaut que là où `ss` se tait : sshd déplacé et redémarré, la session reste sur l'ancien port, et le
  compter refusait le plan même qui ouvrait le nouveau.
- **Deux moteurs web sans `web.engine` ne sont pas l'absence de serveur web** : un profil web tenu pour
  absent voyait ses règles retirées, et `firewall ban` retirait l'accès public de tous les sites, garde
  HTTP-01 comprise. Le mode préfixé, qui se rabat sur Apache, ne produit jamais cet échec : le test passe
  par `firewallWebEngine`.
- **L'unité transitoire du retour automatique d'ufw vit sous `/run`** : un redémarrage dans le délai
  l'efface, et ufw repartait actif sans filet. Le complément `ufw.service.d/ogs-rollback.conf` (D109) range
  `ENABLED=no` avant qu'ufw-init ne lise `ufw.conf`, **si le témoin existe** — une unité sautée par une
  condition laisserait `ENABLED=yes`, et `ufw status` dirait actif un pare-feu qui ne filtre rien. Mais
  **`ExecStartPre` tourne à chaque démarrage de l'unité**, pas au seul démarrage de la machine : la v1 du
  complément faisait d'un `systemctl restart ufw` dans le délai un retour, que `status` disait encore « en
  attente » et que `confirm` déclarait confirmé, code 0, ufw inactif. D'où la marque `/run/ogs-ufw-armed`,
  posée à l'armement et par le complément après avoir joué — `/run` vidé, c'est la machine qui démarre —, à
  la racine de `/run` : `/run/ogs` naît de `systemd-tmpfiles`, après `ufw.service`. Le scénario `pare-feu`
  éprouve la relance et un vrai redémarrage du conteneur : un scénario qui rend 100 demande à `run.sh` de le
  relancer (`/run` repart vide) et reprend à la phase suivante.
- **Une règle « any » d'ufw tient deux familles, et une seule commande les pose ou les retire** : quand
  l'administrateur en tient une, la commande commune marquait sa moitié — ufw remplace le commentaire en
  place — ou l'emportait au retrait. Chaque commande vise alors sa famille (`from ::/0`, `out to 0.0.0.0/0
  port …`), que ufw range sous le même tuple que la moitié d'une règle « any » (D109). Les gardes sans
  session jugent de même chaque famille : une règle ssh de l'administrateur en IPv4 seule comptait comme
  ouverture pour tous, et le retrait de la moitié IPv6 d'ogs fermait ssh en IPv6 sans refus.
- **Le serveur web suit les liens du docroot sous une identité membre de tous les groupes** (D25, D87) : un
  lien posé dans un docroot lui faisait servir publiquement les fichiers d'un voisin, que le pool du projet
  ne pouvait même pas lire. Apache `SymLinksIfOwnerMatch` et nginx `disable_symlinks if_not_owner`,
  **depuis la racine du projet** — l'utilisateur remplace sans privilège le docroot lui-même par un lien —,
  et `-FollowSymLinks` explicite, Debian le posant sur `<Directory />`. Sous Apache, le contrôle précède
  l'ouverture : une course subsiste pour un compte doté d'un shell (§20 du cahier). Sous nginx,
  `disable_symlinks` ouvre les répertoires en lecture au lieu d'un `stat()`, et le docroot en 0710 ne se lit
  pas : `try_files $uri` manquait l'accueil — d'où `$ogs_fichier`, défini par le fragment, qui conduit « / »
  au nom réservé `/.ogs-accueil/`.
- **MultiViews négocie « /composer » vers `composer.json` par une sous-requête sur le fichier**, qui ne
  passe par aucun `RedirectMatch`, et un `<Location />` de surcharge (D63) rouvre un `<FilesMatch>`, les
  sections fusionnant dans l'ordre. D'où, en plus du `RedirectMatch 403` rendu dès la traduction de l'URL,
  un refus du nom de fichier dans un `<If "%{REQUEST_FILENAME} …">`, qu'Apache fusionne après les
  `<Location>`. La négociation énumère le répertoire : elle échoue sur le docroot en 0710 (AH00686) et ne
  s'exerce que dans un sous-répertoire que `www-data` peut lister — un `vendor/*/composer.json`.
- **La sortie d'un clone ne promet que ce que la garde a fait** (D84). La garde retient les fichiers de
  configuration connus du type et masque les secrets qu'ogs connaît — à longueur égale : fichiers creux,
  chaînes PHP sérialisées et liens physiques restent intacts. Tout le reste passe tel quel, et la sortie le
  dit pour tous les types : le `.env` versionné de Symfony, un fichier de `config/`, l'utilisateur et la base
  d'un DSN dont seul le mot de passe est masqué, le contenu de la base. Une variable déclarée reprise est
  nommée et compte dans le code 6 — elle atteint l'application par FPM ou systemd sans passer par sa
  configuration —, un lien dont la cible sort de la préproduction aussi : la copie ne le suit pas, la
  préproduction le suivra. Un fichier qui porte un mot de passe est masqué, jamais retiré : un WordPress
  sans `wp-config.php` offre son installeur, comme un Prestashop dont `install/` a survécu.
- **Le moteur ne compense jamais l'étape qui échoue**, seulement celles qui l'ont précédée : une étape qui
  écrit un fichier géré puis lance une commande doit **rendre le fichier** si la commande échoue. Laissé en
  place, il passe pour conforme au plan suivant, et la commande n'est jamais rejouée — `ufw app update` après
  le fichier de profils (la règle gardait l'ancien port de sshd, et les sessions neuves ne passaient plus),
  `daemon-reload` après le complément d'unité du serveur web (Q-06). Une commande qui n'est qu'une intention
  de rechargement (`Step.Reload`) n'est pas concernée.
- **Chaque vhost sert le certificat qu'il désigne, s'il le couvre** (D102) : `web.Provider.Certificate`
  reçoit le vhost, câblé sur `tls.Provider.VHostCoverage`. La référence était lue puis ignorée, et le vhost
  d'un second certificat restait en HTTP seul ; retenue dès qu'elle existait, elle retirait au contraire le
  bloc 443 d'un site que le certificat désigné ne couvrait pas — `spec.web.tls.certificate` vaut pour tous
  les vhosts —, en code 0, HSTS actif. Une référence ne compte que si sa demande couvre un nom du vhost, la
  préproduction garde le wildcard, et un désigné pas encore émis laisse servir le présent jusqu'à la
  bascule (`Coverage.Pending`, D79). Sans objet, elle retombe sur le choix par défaut et le plan la
  signale — jamais un refus.
- **Une attente se borne sur l'opération entière, pas pas à pas** : l'attente d'un bail `F_SETLEASE`, bornée
  fichier par fichier, s'additionnait — 45 s par bail, qu'un locataire pose sans privilège à chaque
  répertoire — et tenait le verrou global des heures durant. L'archive porte un budget (`fs.LeaseBudget`) et
  suit le contexte jusque dans l'attente (`Dir.OpenFileContext`, `Writer.AddTree(ctx, …)`) (Q-86).
- **Bubble Tea sort sur SIGTERM sans attendre ses commandes en vol**, et `q` rendait `tea.Quit` pendant une
  action : la saga était abandonnée entre deux étapes, le processus sortant sous elle. La TUI écarte son
  gestionnaire de signaux (`tea.WithoutSignalHandler`), reçoit l'interruption par le contexte de la commande,
  n'admet qu'une action à la fois et ne se ferme qu'après elle ; `tuiBackend` sérialise ses appels, qui
  partagent l'App (Q-101). `go test -race ./internal/tui/ ./internal/cli/` le verrouille. Bubble Tea sort
  aussi **sur une lecture du terminal en échec** — EIO, terminal d'une session ssh coupée —, sans attendre :
  `tui.run` annule alors l'action et l'attend (`actionGate`) avant de rendre la main (D122).
- **`--dry-run` est une option globale, donc reçue par des commandes qui ne la lisaient pas** (D88) :
  `project delete --dry-run` supprimait pour de bon — archive, bases, vhost, compte, home, `preDelete` et
  `postDelete` en root — et rendait 0 ; `project restore` et `cert renew` de même. La conduite de toute
  commande est tenue dans `internal/cli/dryrun.go` — qui simule, qui ne fait que consulter ; une commande
  qui n'y est pas inscrite refuse l'option en code 2, avant de lire quoi que ce soit. N'y inscrire une
  commande qu'après lui avoir appris à simuler.
- **À une mise à jour, dpkg exécute le prerm de la version en place** (`upgrade <nouvelle>`), puis le
  postinst de la nouvelle (`configure <ancienne>`) : un défaut de prerm ne se corrige que dans le postinst
  suivant. Celui des versions jusqu'à 0.0.3 désactivait le timer de renouvellement à chaque mise à jour, et
  plus aucun certificat ne se renouvelait, sans un mot (D89). Un paquet sans postrm ni fichier de
  configuration est oublié par dpkg dès le retrait, et `apt purge` n'a plus rien à faire.
- **Cobra rend 0 sur un groupe mal suivi** (D101) : une commande qui n'existe que par ses sous-commandes
  n'est pas exécutable, et cobra affiche son aide puis rend `nil` pour toute sous-commande inconnue —
  `ogs db dmup shopfr > dump.sql` écrivait l'aide dans la sauvegarde, en code 0. Une feuille sans validateur
  ignore ses arguments. `guardArgs` (`help.go`) rend chaque groupe exécutable et pose `NoArgs` par défaut ;
  `ogs completion`, que cobra n'ajoute qu'à l'exécution, est créé d'avance pour y passer — il échappait aussi
  à la traduction, et `ogs completion bash` exigeait root. Une sortie d'avertissement passe par `warnf`,
  jamais par `printf`, que `--quiet` et `-o json` font taire.
- **Un enfant d'`exec.Local` n'est pas au premier plan du terminal** (`Setpgid`) : un pager qu'il lance
  reste arrêté sur sa première lecture. Une commande dont la sortie est branchée sur le terminal
  (`Command.Stdout`) le désactive — `journalctl --no-pager`.
- **bubbletea interroge le terminal dès l'initialisation** (couleur de fond : OSC 11 puis CSI 6n) : toute
  commande d'ogs dont la sortie standard est un terminal sans répondant attend jusqu'à cinq secondes et
  laisse des séquences d'échappement. C'est le pseudo-terminal qu'apt ouvre pour dpkg, même sous
  unattended-upgrades : un script de maintenance écarte la sortie standard d'ogs (`>/dev/null`) et garde
  stderr pour les erreurs.
- **Un répertoire de travail root rempli depuis une source non fiable n'est pas une chaîne sûre** (D90) :
  les liens qu'il reçoit (`rsync -l`) sont suivis par toute écriture faite ensuite par le nom sous ce
  répertoire. Un `current/public -> ../../../victime` reçu de la source menait root hors du miroir de
  `docs/migrer.sh`. Jamais d'écriture root sous un chemin dont un composant vient de la source : chaque copie
  a son sous-arbre (`miroir/code`, `miroir/donnees/<empreinte>.d|.f`), et le côté qui écrit dans le home
  tourne sous le compte du projet (`rsync -s`, `runuser`).
- **ssh lit l'entrée standard** : dans une boucle `while read`, il consomme les lignes suivantes, et seul le
  premier élément de chaque liste était traité. `ssh -n` partout où ssh ne porte pas un protocole. Son socket
  de multiplexage ne vit jamais sous un nom stable de `/tmp` — un compte local y tenait le rôle de connexion
  maître pour les commandes root vers la source —, et un secret ne passe jamais en argument :
  `/proc/<pid>/cmdline` est lisible de tous.
- **`state.db` ne sait pas seule quel port sert quel projet** (D94) : le vhost d'un voisin relaie déjà vers
  le port écrit dans son unité, ou vers celui de son pool TCP. Une base perdue redistribuait les numéros
  dans l'ordre alphabétique, et chaque site était servi par l'application de l'autre. D'où la reprise, avant
  la première attribution, des numéros que portent les fichiers écrits (`Store.Claim`) — la base l'emporte
  quand elle sait. Et **une connexion qui aboutit ne dit pas qui répond** : tout compte local peut tenir un
  port libre. Une écoute se juge à son uid (`/proc/net/tcp`, `app.ReadListeners`), l'attribution écarte les
  ports en écoute, et une écoute étrangère — ni le projet, ni root — rend l'attente critique. doctor relit le
  port dans l'unité écrite : il n'attribue rien, sans quoi, sans base, tout projet aurait reçu le premier.
- **Un numéro ne se libère qu'une fois son fichier parti, et ne s'inscrit qu'à l'apply** (D100). `plan`,
  `diff`, `status` et la TUI réservaient un numéro à chaque pool calculé, la suppression n'en libérait aucun,
  et la plage d'une version PHP se saturait. Les commandes qui calculent sans appliquer passent le store en
  aperçu (`Store.PreviewAllocations`) ; un apply refusé ou déroulé rend ses réservations
  (`CancelAllocations`). Libérer le numéro d'un pool ou d'une unité restés sur le disque — non écrits par
  ogs, retrait en échec — le ferait donner à un autre projet, dont le vhost relaierait vers ce qui tourne
  encore : la libération suit le retrait, et une compensation qui rend le fichier rend le numéro.
- **Une preuve de propriété part après sa ressource, jamais avant** (D100). La suppression vide le keystore
  du projet, mais les secrets `mysql/`, `mariadb/`, `postgres/`, `amqp/`, `amqp-vhost/` et
  `amqp-declared/` restent l'affaire de leur provider : retirés d'avance, ils feraient d'une base ou d'un
  vhost resté en place — moteur injoignable, retrait en échec, `--keep-data` — une ressource étrangère
  qu'ogs ne reprendrait plus jamais.
- **Un ancien compte ne part qu'une fois son remplaçant observé, jamais dans l'apply qui le crée** (D100).
  Un utilisateur SGBD renommé, un hôte changé, un vhost ou un compte AMQP remplacés : l'étape qui crée le
  nouveau le compense en cas de déroulage, et le retrait de l'ancien, lui, ne se compense pas. Une étape
  critique en échec plus loin laissait la base sans aucun compte ni secret, refusée ensuite comme étrangère,
  pendant que la CLI annonçait « revenu à son état initial ». Le retrait attend l'apply suivant ; un hôte
  changé laisse une marque scellée (`<moteur>-moved/<compte>`) qui porte **les hôtes** sous lesquels ogs a
  tenu le compte, seule preuve, entre deux comptes de même nom, de ceux qu'ogs a posés (D78) : l'apply
  suivant retire ceux des hôtes quittés — un hôte d'origine retrouvé ne voyait sinon rien partir, et la
  marque restait à jamais —, et `project delete` les emporte tous, faute de quoi un compte à mot de passe
  survivait sans preuve entre les deux applies. **Jamais pour un projet adopté** : son mot de passe scellé, tiré au
  premier apply d'un compte qui existait, n'est pas celui que le compte porte — `CREATE USER IF NOT EXISTS`
  ne le pose pas. Le recréer avec sous l'hôte déclaré coupait l'application (MySQL préfère l'hôte le plus
  précis), puis l'apply suivant détruisait le compte réel ; un adopté garde le refus de D78.
- **Une lecture en échec n'est pas une absence** (Q-20) : sur le broker, une lecture d'exchanges, de files,
  de bindings ou de policies qui échoue faisait créer, en étapes critiques, ce qui existait, et la trace de
  D100 oubliait des objets restés sur le broker. L'observation incomplète ne fonde ni création ni retrait. Et **la restauration ne réinjecte que les preuves de ce que la
  déclaration nomme** : l'archive précède les retraits, et la preuve d'une ressource détruite depuis
  faisait retirer par l'apply l'homonyme que l'administrateur avait créé entre-temps.
- **Un rôle PostgreSQL qui a reçu des droits ne se retire pas par `DROP ROLE`** : ses privilèges, privilèges
  par défaut compris, en dépendent, et `DROP OWNED` seul supprime les tables qu'il a créées. D'où
  `REASSIGN OWNED` vers le propriétaire de la base, puis `DROP OWNED`, base par base. `REASSIGN OWNED` touche
  aussi les objets partagés, bases comprises : un rôle qui possède encore une base n'est pas retiré. Sous
  MySQL, `DROP DATABASE` laisse les droits de `mysql.db` : une base « où le compte a ses droits » se juge
  sur son existence, pas sur `SCHEMA_PRIVILEGES` seul.
- **Une file quorum refuse `if-empty` et `if-unused`** : `DELETE /api/queues/<v>/<f>?if-empty=true&if-unused=true`
  rend 400, et le préréglage Messenger déclare ses transports en quorum. Le retrait d'une file relit ses
  compteurs juste avant de la retirer sans condition ; seule une file classique est jugée par le broker
  lui-même. Et ce que l'application crée elle-même dans son vhost (Messenger, `auto_setup`) n'est jamais
  retiré : seul ce que la trace `amqp-declared/<vhost>` dit qu'ogs a déclaré l'est (D100).
- **Une restauration qui ne rend pas tout n'est pas un succès** (D104) : une base non rechargée, un fichier
  d'environnement non réécrit — un fichier déjà posé là, une autre clé maître — rendaient 0 ou un motif
  vague, audit « ok ». Chaque manque est nommé avec son remède, code 6, audit « avertissement » ; le fichier
  d'environnement non écrit reste scellé au keystore (`app/envfile`), et le modèle vide que l'apply pose
  ensuite ne l'emporte jamais sur cette copie à la suppression suivante — archivé à sa place, il perdait
  les secrets. Le remède d'une base est `ogs db restore --from-archive`, jamais `tar --zstd` : Debian
  n'installe pas `zstd`. Toute ligne de commande affichée comme remède cite ses arguments
  (`exec.ShellLine`) : un nom venu d'une archive se copie en root. Et une assertion d'audit cherche
  `résultat=<valeur>` : la ligne n'a pas de tabulations, et un test qui y cherchait `\tok\t` ne pouvait
  jamais échouer.
- **Un rétablissement de liens en échec se remonte** (D104) : `removal.File` le jetait, et un vhost désactivé
  restait hors ligne sous une erreur qui ne parlait que du retrait. L'erreur est marquée
  `engine.ResidualError`, sans quoi `reportRollback` annonçait « revenu à son état initial » au-dessus.

## Codes de retour

0 succès · 1 erreur · 2 usage · 3 validation · 4 conflit · 5 dépendance absente · 6 succès partiel ou écart
détecté · 7 verrou indisponible · 8 privilèges insuffisants · 9 annulé.

Le 2 couvre aussi `--dry-run` demandé à une commande qui ne sait pas simuler, et une entrée de
`db restore -i` refusée ; le 9, un refus à la confirmation ou une interruption — SIGINT ou SIGTERM —,
notamment pendant le calcul d'un plan, avant le point de non-retour d'une suppression, ou pendant la copie
d'un clone. Le 4 est le fichier d'un autre projet, que `--force` ne
lève pas ; un retrait refusé (fichier non géré, ou retouché sans `--force`) est une étape non critique, 6.

Le code 6 est structurant : le timer de renouvellement le tolère, et `diff` s'en sert pour signaler un écart
sans le traiter comme une erreur.
