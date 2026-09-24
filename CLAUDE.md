# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Langue

Le projet est **entièrement en français** : code, commentaires, messages d'erreur, documentation, noms de
tests, sorties CLI. S'y conformer, accents compris. Les identifiants techniques (noms de types Go, clés
YAML, directives de configuration) restent dans leur forme d'origine.

Les commits ne mentionnent jamais l'assistance d'un outil d'IA.

## La référence

[`docs/CAHIER-DES-CHARGES.md`](docs/CAHIER-DES-CHARGES.md) fait autorité : 74 décisions d'architecture
arbitrées (D1–D74, tableau §2), spécification par module, plan de tests, découpage en lots. **Consulter le
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
```

### Essai sans privilèges

`ogs` est root-strict. `--prefix <répertoire>` est la seule dérogation : il décale tous les chemins système
sous un répertoire jetable et **simule** les commandes système (`exec.Recorder` au lieu d'`exec.Local`).
C'est le mode de développement, et celui des tests de `internal/cli`.

```sh
make dev-demo                                   # crée ./dev, deux projets, plan, apply
./ogs --prefix ./dev project create shop.fr --kind php-symfony
```

### Essais sur système réel

Dix-neuf scénarios conteneurisés éprouvent ce que les tests unitaires ne peuvent pas voir. Ils ont mis au jour
plus de trente défauts invisibles autrement (§18 du cahier des charges) : **y recourir avant de déclarer un
provider fonctionnel**.

```sh
make integration-all       # les dix-neuf, à la suite
make integration-nginx     # un seul (voir SCENARIOS dans le Makefile)

# Hors série : rbenv et pyenv compilent depuis les sources (un quart d'heure).
make integration-runtimes-compiles
```

Mécanique dans `test/integration/` : une image Debian portant Apache, nginx, PHP-FPM, MariaDB, PostgreSQL,
systemd et cron ; une composition Docker pour ce qui exige un service tiers joignable par le réseau
(RabbitMQ, pebble) ; un conteneur avec **systemd en PID 1** pour le scénario des services et des tâches.
Les scénarios sont montés en volume — les mettre au point ne demande pas de reconstruire l'image.

Deux pièges du harnais :

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

## Architecture

### Le modèle : état désiré, réconciliation

Un projet est décrit par un YAML dans `/etc/ogs/projects/<id>.yaml` (`internal/model`, chargé par
`internal/spec`). Chaque provider **observe** l'état réel, le **compare** au désiré et produit des
**étapes** ; le moteur (`internal/engine`) les exécute en saga — une étape critique en échec fait compenser
les précédentes en ordre inverse, une étape non critique produit un avertissement et l'exécution continue
(un certificat qui échoue ne doit pas détruire un site par ailleurs fonctionnel).

`plan`, `diff` et `apply` partagent le même calcul : `apply.go:buildPlan` assemble les providers dans un
ordre qui est une décision, non un détail — système → bases → AMQP → applicatif (runtime, lien, unité) →
PHP → cron → unités et démarrage des instances → attente de disponibilité → web → TLS → bascule TLS du
vhost → sudoers. Les services démarrent **avant** le serveur web : un vhost de proxy branché sur une
application qui n'a pas encore démarré exposerait une 502. `buildGlobalPlan` porte ce qui vaut pour tout le
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
l'unicité dans le parc.

### Fichiers gérés

`internal/fs` écrit atomiquement avec un en-tête portant **l'empreinte du corps**. `Inspect` compare cette
empreinte, non les octets : l'en-tête contient un horodatage, et comparer les octets ferait passer tout
fichier « en écart » la seconde suivant son écriture. Cinq états : absent, conforme, à mettre à jour,
modifié à la main (exige `--force`), non géré par ogs.

Deux corollaires, chacun payé d'un défaut réel :

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

La sauvegarde de `--force` va dans **`/var/lib/ogs/backups`**, jamais à côté de l'original : tout répertoire
de configuration est relu en entier par quelqu'un — son démon, ou ogs lui-même, qui inventorie
`/etc/sudoers.d` et `sites-available`. Et l'horodatage se devine à la seconde près, donc une copie déposée
dans un répertoire qu'un non-root possède se ferait détourner par un lien posé d'avance à ce nom.

Les templates (`internal/template`, `embed.FS`) sont surchargeables dans `/etc/ogs/templates`. **Le TLS est
servi par tous les templates de vhost**, pas seulement ceux des sites PHP.

### Secrets et état

`internal/secret` chiffre en AES-256-GCM avec une donnée associée qui lie le secret à sa portée ;
`internal/store` est une base SQLite (`modernc.org/sqlite`, pur Go) migrée au démarrage ;
`internal/audit` journalise chaque étape avec un identifiant d'exécution.

### Exécution de commandes

`internal/exec` est le seul chemin vers le système. `Command.AsUser` passe par `su -s /bin/bash -c` et
**cite chaque argument** — recoller un argv en script shell sans citation fait redécouper tout argument
contenant une espace. Les processus sont placés dans leur propre groupe et l'annulation tue le groupe
entier. `exec.Recorder` (mode préfixé) simule tout et permet aux tests d'assertion sur les commandes.

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
**réécrit**, jamais régénéré, une clé neuve exigeant de republier chez le client. ogs ne touche ni au MTA,
ni à `opendkim.conf`, ni au DNS ; la rotation (`rotate` puis `activate`) ne bascule qu'une fois
l'enregistrement publié, vérifié par le résolveur.

### Pare-feu : ufw.yaml, des profils à soi, des règles marquées

`internal/provider/firewall` (D72) réconcilie ufw avec `/etc/ogs/ufw.yaml`, **dont l'absence vaut « ne
touche à rien »**. Il écrit un seul fichier, `/etc/ufw/applications.d/ogs`, aux profils `ogs-*` dérivés de
ce qui est installé (unités, `sshd_config`, `proftpd.conf`…) ; toute règle passe par la **commande ufw**,
jamais par `user.rules`, que la réconciliation relit seulement — ses lignes `### tuple ###` sont ce
qu'ufw lui-même relit. **Une règle est à ogs si son commentaire commence par `ogs:`**, et à
l'administrateur sinon : ogs ne la touche pas. Les étapes sont non critiques, mais politiques et
activation refusent de s'appliquer si une règle a échoué ; la première activation arme un minuteur
systemd qui désactive ufw faute de `ogs firewall confirm`. Les adresses de confiance rejoignent
l'`ignoreip` des prisons fail2ban (`monitoring.Provider.TrustedIP`). **Un `ufw.yaml` invalide ou refusé
n'arrête pas le plan global** : il y devient une étape non bloquante en échec (`firewall.RefusedStep`),
code 6, et seules les commandes `ogs firewall` rendent le code 3 — sans quoi une faute de frappe dans le
pare-feu priverait `project delete` du recalcul des prisons et de la rotation (D68).

### Surfaces particulières

- `ogs my <verbe>` : surface déléguée à l'utilisateur d'un projet par une règle sudoers énumérée, sans
  métacaractère, validée par `visudo` sur un fichier temporaire. L'appelant est identifié par **`SUDO_UID`**,
  pas `getuid()` — ce qui suppose `env_reset` actif, contrôlé par `doctor`.
- `internal/importer` : reprise d'un serveur existant. Corrélation par indices vérifiables (socket FPM →
  pool → utilisateur, docroot sous le home) ; un projet adopté **garde ses noms réels**.
- `internal/tui` : Bubble Tea derrière une interface `Backend`, testable sans terminal.
- `internal/hook` : hooks de parc (un fichier de `/etc/ogs/hooks` nommé d'après l'événement) ou de projet
  (`spec.hooks`, qui l'emporte). Un hook s'exécute en root : il doit vivre sous `/etc/ogs/hooks`,
  appartenir à root et n'être inscriptible par personne d'autre.

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
  descripteur. Écrire par le nom, c'est laisser l'utilisateur choisir où root crée ou supprime ; c'était le
  cas du lien `.ssh/authorized_keys` jusqu'à D70.
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
  ignore en silence.
- `ReadWritePaths` d'une unité est **facultatif** (préfixe `-`) : `var/` est créé par l'application, et un
  chemin absent fait échouer le service en 226/NAMESPACE avant toute exécution.
- Les étapes d'unité s'ordonnent par **cycle de vie**, pas par nom : `unit/instance` précède
  `unit/service` dans l'alphabet, ce qui ferait démarrer une instance avant l'écriture de son modèle.
- La cible d'un projet doit être liée à `multi-user.target`, sinon les services ne repartent pas après un
  redémarrage.
- Une unité **modifiée** exige un redémarrage : `daemon-reload` relit la définition, il ne relance rien.
  L'intention `systemd:restart-target:<id>` s'en charge, et l'attente de disponibilité la vide avant
  d'attendre.
- Une **application supervisée** est un service comme un autre : le provider applicatif écrit son unité,
  le provider d'unités active et démarre ses instances. Les deux sont nécessaires — l'unité seule ne
  démarre rien.
- `ogs harden` restreint les droits de `/etc` (D56). Deux invariants : il **ne desserre jamais** — un
  répertoire déjà plus strict est laissé tel quel — et il **préserve le bit d'exécution** des fichiers,
  sans quoi il rendrait les hooks inexécutables donc refusés par ogs lui-même. `/etc/php` reste ouvert :
  le PHP en ligne de commande des crons y lit son `php.ini`, et le fermer changerait le comportement de
  PHP sans le moindre message. La **racine des homes** y figure aussi, en 0711 (D60) : traversable sans
  être énumérable, jamais récursivement — sous elle, `model.defaultDirs` est le seul juge. Le gain est
  partiel et doit être annoncé comme tel : `/etc/passwd` énumère déjà tous les homes. Et « ne desserre
  jamais » mord ici — un `/home` en 0700 ne rend plus un fichier et reste en place ; c'est `doctor` qui
  le dit.
- **Un rechargement du serveur web peut tuer le serveur.** `apachectl graceful` ne teste pas la
  configuration : un maître Apache qui relit une configuration invalide meurt, et tous les sites tombent.
  D'où la revalidation dans `web.Provider.Reload`, en plus de celle de l'écriture — les intentions étant
  groupées en fin d'apply, un fichier référencé a pu disparaître entre les deux.
- Aucune vérification en amont d'un rechargement ne couvre tout : `ProtectHome` dans une unité fait voir
  `/home` en lecture seule **au seul service**, là où ogs écrit sans peine. D'où le constat en aval —
  `web.Provider.Reload` vérifie `is-active` après le rechargement, relève le serveur s'il est mort et le
  signale même alors.
- **L'unité du serveur web est durcie par la distribution** : Ubuntu livre `apache2.service` avec
  `ProtectHome=read-only`, qui montre `/home` en lecture seule **au seul service** — `mount` affiche `rw`,
  root écrit sans peine, et le maître meurt en ouvrant le journal d'un vhost. D'où le complément d'unité
  géré (D59), écrit **avant** les vhosts et suivi d'un **redémarrage** : l'espace de noms est établi au
  démarrage, un `reload` ne le refait pas. Les chemins portent le préfixe `-`, un `ReadWritePaths` absent
  faisant échouer le service en 226/NAMESPACE.
- Avant toute relance du moteur, **acquitter le compteur d'échecs** (`systemctl reset-failed`) : passé
  `StartLimitBurst`, systemd refuse de démarrer et le serveur reste mort alors que la cause est levée.
- **`apachectl -t` répond « Syntax OK » sans ouvrir un seul journal**, alors que le maître les ouvre tous au
  (re)démarrage et s'arrête sur le premier qui résiste (`AH00019: Unable to open logs, exiting`). Un `/home`
  remonté en lecture seule après une erreur disque a ainsi tué un serveur entier, configuration valide à
  l'appui. `web.Provider.CheckLogs` éprouve les journaux de tous les vhosts gérés avant le rechargement, et
  `doctor` porte le même contrôle.
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
- **Le shell des comptes de projet est ouvert** — `/bin/bash` (D62), réglable par `user.shell` dans
  `/etc/ogs/config.yaml` et par `spec.user.shell`. Trois points à ne pas défaire : le défaut de la
  configuration est un défaut **de création**, le fichier du projet faisant autorité ensuite — le propager
  réécrirait le shell de tout le parc au prochain apply ; le shell déclaré est **réconcilié** par `usermod`,
  dans la **même étape** que la correction du home, deux étapes de même ressource se marchant dessus dans le
  plan comme dans l'audit ; et un projet **adopté garde son shell réel**, l'import le reprenant du système.
  `su -s /bin/bash` reste nécessaire dans `internal/exec` : un parc peut avoir refermé le shell.
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
- Telegraf n'a **pas d'équivalent d'`apachectl -t`** : son option de test exécute réellement la collecte.
  Le rechargement se contente d'un constat en aval. Et un fragment que l'agent ne peut pas lire ne produit
  **aucune erreur** — il démarre, ne collecte rien, et se tait ; d'où le contrôle de `doctor`.
- Apache a besoin de sept modules (`ssl`, `proxy`, `proxy_fcgi`, `proxy_http`, `rewrite`, `headers`,
  `status`) ; sans eux la configuration est acceptée puis rend une 500 illisible — ou, pour `status`, une
  page vide. `doctor` les énumère.
- MySQL : le nom de base après `ON` est un motif où `_` est un joker, donc échappé — **et déséchappé à la
  relecture**, sans quoi toute base contenant un souligné reste éternellement « en écart ». Le nom
  d'utilisateur, lui, est un littéral.
- PostgreSQL : l'authentification `peer` exige un rôle au nom de l'utilisateur système (`root`), et
  `CONNECT` doit être révoqué à `PUBLIC` — il est accordé par défaut sur toute base nouvelle. Créer depuis
  `template0`, jamais `template1`.

## Codes de retour

0 succès · 1 erreur · 2 usage · 3 validation · 4 conflit · 5 dépendance absente · 6 succès partiel ou écart
détecté · 7 verrou indisponible · 8 privilèges insuffisants · 9 annulé.

Le code 6 est structurant : le timer de renouvellement le tolère, et `diff` s'en sert pour signaler un écart
sans le traiter comme une erreur.
