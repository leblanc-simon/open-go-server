# Installation et mise en service

## Paquet Debian

Le paquet contient le binaire, les pages de manuel, les scripts de complétion pour bash, zsh et fish, et
crée les répertoires attendus. Il ne crée aucune configuration : celle-ci est engendrée par `ogs init`, qui
produit également la clé maître du keystore.

```sh
apt install ./ogs_1.0.0_amd64.deb
ogs init
```

Le paquet ne dépend que de `sudo` — nécessaire à la délégation des services aux utilisateurs de projet. Les
serveurs administrés (Apache, Nginx, MySQL, PostgreSQL, RabbitMQ, PHP-FPM) ne sont pas des dépendances :
`ogs` administre ce qui est présent, et `ogs doctor` énumère ce qui manque.

Désinstaller retire l'outil, jamais les projets : comptes, bases, vhosts, certificats et déclarations
survivent. Seul le timer de renouvellement est arrêté, et son état noté : une réinstallation le rend tel
qu'il était. La purge (`apt purge ogs`) retire en outre les deux unités de renouvellement, et garde — en
le disant — `/etc/ogs` (déclarations, clé maître), `/var/lib/ogs` (état, comptes ACME, certificats que les
vhosts servent encore, archives) et `/var/log/ogs`. Elle énumère les archives restantes : plus rien ne les
purge, et elles portent le dump des bases.

Le paquet ne livre ni les unités `ogs-renew`, ni `tmpfiles.d/ogs.conf`, ni `logrotate.d/ogs` : c'est
`ogs apply` qui les écrit, d'après le parc.

## Depuis les sources

```sh
make build            # produit ./ogs, statique, sans dépendance C
make install          # installe dans /usr/bin
make package          # produit dist/*.deb (nécessite Go et le réseau pour nfpm)
```

## Premier démarrage

`ogs init` crée `/etc/ogs` (droits 0700), la base d'état `/var/lib/ogs/state.db`, la clé maître
`/etc/ogs/master.key` et les répertoires de journalisation.

**La clé maître chiffre tous les secrets** — mots de passe de bases, d'AMQP, jetons DNS. Elle n'est pas
sauvegardée par `ogs`. Sans elle, une archive de projet est inexploitable. La sauvegarder hors du serveur
est la première chose à faire après `ogs init`.

```sh
ogs doctor            # état de l'environnement : serveurs détectés, versions PHP, droits
ogs init              # idempotent : relançable sans risque
```

## Vérification de l'installation

```sh
ogs doctor
ogs version
man ogs
ogs completion bash > /etc/bash_completion.d/ogs   # inutile avec le paquet
```

Sur une machine où Apache est le moteur retenu, `doctor` énumère aussi les modules dont les vhosts
dépendent (`ssl`, `proxy`, `proxy_fcgi`, `proxy_http`, `rewrite`, `headers`). Leur absence ne se traduit
pas par un refus clair : Apache accepte la configuration puis rend une erreur 500 dont le message
n'oriente vers rien.

`ogs doctor` distingue ce qui empêche de travailler (base d'état illisible, clé maître absente) de ce qui
restreint le champ d'action (aucun serveur MySQL détecté, PHP 7.4 non installé). Le code de retour ne vaut
0 que si rien ne bloque.

## Configuration

`/etc/ogs/config.yaml` porte les réglages de portée serveur : moteur web par défaut, domaine de
préproduction, fournisseur DNS, certificats partagés, connexions aux serveurs de bases. Le fichier est
validé à chaque lecture ; une valeur invalide est refusée plutôt qu'ignorée. Une clé inconnue — une faute
d'orthographe, dont le réglage serait perdu — est nommée par son chemin (`archives.retentionDay`) : les
commandes qui écrivent la refusent (code 3), les consultations et le renouvellement des certificats l'annoncent
sur la sortie d'erreur sans s'arrêter, et `ogs doctor` la compte en échec (D101).

`naming.homeRoot` et `logs.root` sont des **défauts de création** : chaque déclaration porte le home
(`spec.layout.root`) et le répertoire de journaux (`spec.layout.logDir`) qu'elle a reçus, et ogs ne déplace
ni un home, ni un compte, ni des journaux. Changer l'une de ces racines alors que des projets existent est
donc refusé (D120) : le plan et l'apply — d'un projet comme du parc, création comprise — et
`project delete` rendent le code 3 en nommant les projets restés sous l'ancienne racine, et `ogs doctor` le
compte en échec. L'ancienne valeur se lit dans les déclarations — le répertoire qui contient leurs homes ou
leurs journaux — et le remède est de la remettre dans `config.yaml`. Le renouvellement des certificats, lui,
continue : rien n'en dépend.

Les identifiants d'administration n'y figurent pas : ils vivent en 0600 sous `/etc/ogs/credentials/`.
MySQL et PostgreSQL n'en demandent aucun — ogs s'y connecte en root par le socket local. RabbitMQ et OVH,
eux, en exigent :

```sh
ogs amqp credentials set --user ogs-admin          # demande le mot de passe, sans écho
ogs cert dns-provider set ovh --client-id <id>     # demande le secret client, sans écho
printf '%s\n' "$mdp" | ogs amqp credentials set --user ogs-admin   # hors terminal
```

Les secrets ne se passent pas en option : ils resteraient dans le journal de sudo et l'historique du
shell de root. Vers cette machine, ogs n'envoie les identifiants du broker qu'à un port tenu par root ou
par le compte `rabbitmq` (`--broker-account`, nom ou uid, pour un autre) : broker arrêté, tout compte local
pourrait ouvrir le port de management. Un broker en conteneur dont le port est publié par DNAT (Docker sans
userland-proxy) se désigne par l'adresse du conteneur (`--url`).

### Pools PHP-FPM : les défauts de la machine

La section `php.pool` fixe ce que la machine impose ou propose à tous les pools (D103). Elle se place
entre le socle d'ogs et le preset ; le type d'application et la déclaration du projet passent après elle :

```yaml
php:
  pool:
    defaultPreset: ondemand          # preset d'un pool qui n'en déclare pas (défaut : ondemand)
    defaults:
      pm.max_requests: "1000"        # socle d'ogs : 500
      request_slowlog_timeout: 5s    # socle d'ogs : 10s
    presets:                         # redéfinit un preset livré clé par clé, ou en ajoute un
      large:
        pm.max_children: "80"
    disableFunctions: [exec, passthru, shell_exec, system, proc_open, popen, pcntl_exec]  # php-plain
    capacityWarnPercent: 200         # seuil d'avertissement de doctor (0 : jamais)
```

Ces valeurs sont **vivantes**, à la différence de `user.shell` : les changer réécrit, au prochain
`ogs apply`, tous les pools qu'elles touchent, et FPM est rechargé une fois par version. Un pool dont la
déclaration nomme un preset le garde ; les projets créés avant cette section portent `preset: medium` en
toutes lettres et ne suivent donc pas `defaultPreset`. Les clés `user`, `group`, `listen` et les `listen.*` d'accès,
`slowlog`, `access.log`, `php_admin_value[error_log]`, `include` et `prefix` sont refusées ici comme dans
un projet, quelle que soit leur casse : `doctor`
et le plan les nomment, la configuration se charge malgré elles. `doctor` affiche aussi, par version, la
somme des `pm.max_children × memory_limit` rapportée à la mémoire de la machine — toujours, en
information — et n'avertit (code 6) qu'au-delà de `capacityWarnPercent`, 200 % par défaut : la somme est un
pire cas, tous les workers présents à leur limite, qu'un parc n'atteint presque jamais.

### PostgreSQL : un rôle d'administration à créer une fois

`ogs` administre un cluster par socket Unix, où PostgreSQL emploie l'authentification `peer` : le rôle
demandé doit porter le nom de l'utilisateur système. `ogs` s'exécutant en root, il lui faut donc un rôle
`root`, qu'une installation neuve ne comporte pas :

```sh
su - postgres -c 'createuser --superuser root'
```

À faire une fois par cluster. `ogs doctor` le signale, avec cette commande, tant que ce n'est pas fait.
Pour employer un autre rôle, renseigner `OGS_PG_ADMIN` — il devra alors être accepté par le `pg_hba.conf`.

Chaque base créée est fermée aux autres : `CONNECT` est révoqué à `PUBLIC` et accordé au seul rôle du
projet. C'est nécessaire — PostgreSQL ouvre par défaut toute base nouvelle à tous les rôles du cluster.

## Mise à jour

```sh
apt install ./ogs_1.1.0_amd64.deb
```

Les migrations de schéma de la base d'état sont appliquées par le paquet lui-même, qui relance
`ogs init --quiet` : un échec y est affiché, jamais fatal. Si la clé maître a disparu, le paquet ne relance
pas `init` — il en engendrerait une neuve, et les secrets scellés ne se reliraient plus — et le dit, avec
les deux remèdes : la restaurer depuis sa sauvegarde, ou lancer `ogs init` pour repartir d'une clé neuve.
De même pour la base d'état disparue d'un parc qui compte des projets : `init` la recréerait vide, le paquet
ne le relance pas et le dit (voir « Sauvegarde »).
Une mise à jour ne touche pas au timer de renouvellement ; un timer d'ogs trouvé désactivé est signalé,
sans passer outre. Les messages du paquet commencent par « `ogs :` ».

Les déclarations de projet sont lues telles quelles : une version plus récente d'`ogs` n'exige jamais de
réécrire les fichiers existants. Après une mise à jour, `ogs diff` sur l'ensemble du parc indique si les
templates livrés ont évolué — et donc quels fichiers un `apply` réécrirait.

Les versions jusqu'à 0.0.3 désactivaient le timer à chaque mise à jour, sans un mot : la mise à jour
depuis l'une d'elles le réactive. `systemctl is-enabled ogs-renew.timer` dit où il en est, et
`systemctl enable --now ogs-renew.timer` le rétablit.

## Applications Go, Node, Ruby et Python

Un projet non-PHP est servi par un vhost de proxy et supervisé par systemd. Deux choses le distinguent :

```sh
# La version du runtime est indispensable dès la création : sans elle, la
# déclaration est refusée à l'application.
ogs site create app.fr --kind node --runtime 22.14.0
ogs runtime install appfr 22.14.0     # nvm, rbenv ou pyenv, dans le home du projet
```

Le runtime est installé **par projet**, dans son home (D35) : deux sites ne partagent rien, et une mise à
jour se fait site par site. Ruby et Python compilent depuis les sources, ce qui demande les paquets de
compilation de la distribution et plusieurs minutes ; Node télécharge une version précompilée.

L'application écoute sur un socket Unix `0660` — joignable par le serveur web et par elle seule — ou, pour
Go dont aucune convention ne s'impose, sur un port de la boucle locale. Le mode se déclare par
`spec.app.listen`. `ogs doctor` signale toute écoute qui déborderait sur une interface publique : elle
contournerait le vhost, donc le TLS et l'authentification.

Le propriétaire du projet redémarre son application lui-même :

```sh
sudo ogs my restart
```

## Fermer les répertoires sensibles

Sur un serveur mutualisé, les utilisateurs de projet ont un compte local. Sans restriction, ils lisent
`/etc/nginx`, `/etc/mysql` ou `/etc/telegraf`, y trouvent des chemins, des noms de bases et des jetons, et
dressent la carte du serveur — celle des sites de leurs voisins comprise. Un simple `ls /home` la leur
donne aussi.

```sh
ogs harden              # rapporte, ne touche à rien ; code 6 s'il reste à restreindre
ogs harden --explain    # pourquoi chaque restriction, et ce qu'elle peut casser
ogs harden --apply      # applique, après confirmation
```

Trois règles gouvernent la commande : elle **ne desserre jamais** — un répertoire déjà plus strict que la
cible est laissé tel quel, et le mode posé est l'intersection du mode constaté et de la cible, setgid et
sticky conservés — ; elle **ne suit aucun lien symbolique**, répertoire et fichiers étant modifiés par leur
descripteur ; et elle **ne ferme rien sans le groupe du service qui lit** : une cible dont le compte ou le
groupe est introuvable est rapportée « refusé », avec son motif, au lieu d'être fermée par le seul mode. Un
chemin absent signifie un service non installé : il n'y a rien à faire.

| Chemin | Cible | Pourquoi |
|---|---|---|
| `/etc/ogs` | `0700 root:root`, fichiers `0600` | clé maître, déclarations de projets, identifiants |
| `/etc/letsencrypt` | `0711`, propriétaire root, groupe et fichiers **inchangés**, sans récursion | clés privées ; certbot les protège lui-même sous `archive/` et `live/` (0700), la racine n'a plus qu'à ne pas s'énumérer |
| `/etc/apache2`, `/etc/nginx` | `0750 root:www-data` | vhosts : domaines, chemins, authentifications |
| `/etc/php` | `0755` — **inchangé** | le PHP en ligne de commande, exécuté sous l'identité du projet par les crons, y lit son `php.ini` |
| `/etc/mysql` | `0750 root:mysql` | `debian.cnf` porte les identifiants de maintenance |
| `/etc/postgresql` | `0750`, groupe `postgres`, propriétaire **inchangé** | `pg_hba.conf` décrit qui peut se connecter |
| `/etc/rabbitmq` | `0750 root:rabbitmq`, fichiers `0640` | cookie Erlang, mot de passe d'administration |
| `/etc/telegraf` | `0750 root:telegraf`, fichiers `0640` | jetons d'écriture des métriques |
| `/etc/redis` | `0770`, groupe `redis`, propriétaire **inchangé**, fichiers `0640` | `requirepass` en clair |
| `/etc/opendkim` | `0750 root:opendkim`, fichiers `0640`, récursif | clés de signature et tables d'OpenDKIM |
| `/etc/ssl/private` | `0710 root:ssl-cert` | convention Debian ; les services concernés sont dans le groupe |
| `/etc/fail2ban` | `0750 root:root` | `jail.d` énumère les journaux du parc et les adresses jamais bannies |
| `/etc/cron.d` | `0750 root:root` | révèle les commandes et les identités de chaque projet |
| `/etc/logrotate.d` | `0750 root:root` | révèle l'emplacement des journaux du parc |
| `/etc/sudoers.d` | `0750 root:root` | révèle les délégations accordées |
| `/home` (`naming.homeRoot`) | `0711 root:root` | la racine des homes énumère le parc : chaque voisin, et le chemin de son code |

Le groupe est celui du service qui lit sa configuration sous sa propre identité, et il n'est pas
optionnel : un démon privé de la sienne ne le dit pas toujours.

- **MariaDB** lit ses fichiers d'options sous `mysql` et ignore **sans un mot** celui qu'il ne peut pas
  ouvrir : fermé en root:root, `/etc/mysql` le faisait repartir sur ses défauts, à l'écoute de toutes les
  interfaces.
- Les **workers d'Apache et de nginx** ouvrent à chaque requête, sous `www-data`, ce que désignent
  `AuthUserFile`, `auth_basic_user_file` et les `RewriteMap`, et mod_md écrit son magasin : fermés à
  `www-data`, un site protégé répond 500 alors qu'`apachectl -t` reste au vert.
- **Redis** réécrit sa configuration par renommage dans `/etc/redis` (`CONFIG REWRITE`, Sentinel) : le
  paquet le lui donne en `2770 redis:redis`, et le propriétaire reste le sien.
- **PostgreSQL** garde le propriétaire que lui donne le paquet — `pg_createcluster`, lancé sous `postgres`,
  y crée un répertoire par version. Contrepartie : `pg_wrapper` ne trouve plus le cluster pour un
  utilisateur de projet, et un cron qui vise un cluster hors du port 5432 doit préciser `-p` ou `PGPORT`.
- **RabbitMQ**, **Telegraf** et **OpenDKIM** lisent sous leur compte, `ssl-cert` sert les clés confiées
  aux démons non root.
- **certbot** écrit un certificat renouvelé en `0644 root:root`, et un démon qui ne tient que la clé par
  `ssl-cert` lit ce certificat par les bits des autres : rien n'est donc repris aux fichiers de
  `/etc/letsencrypt`, seule sa racine se ferme à l'énumération.

Un fichier d'authentification HTTP se range donc sous `/etc/apache2` ou `/etc/nginx`, jamais dans les
répertoires de surcharge des vhosts (`/etc/<moteur>/projects/…`), que `ogs` pose en `0750 root:root` : le
maître les lit, les workers non.

**Un parc durci par une version antérieure** n'est pas rouvert, `ogs` ne desserrant jamais. `/etc/mysql`,
`/etc/apache2` et `/etc/nginx` en root:root reçoivent leur groupe au prochain `ogs harden --apply` ; les
autres se réparent à la main :

```sh
chown redis /etc/redis && chmod 2770 /etc/redis          # CONFIG REWRITE et Sentinel
chmod 0711 /etc/letsencrypt                              # puis rendre le groupe ssl-cert à la clé confiée
```

**La racine des homes** (D60) se traverse sans s'énumérer, comme le home d'un projet (D51) : servir un
fichier n'exige que la traversée des répertoires et la lecture du fichier, et le serveur web n'a jamais
besoin de lister `/home`. Rien ne change donc pour les sites, ni pour un projet qui atteint son propre
home. Trois choses changent, et il faut les connaître avant d'appliquer :

- les globs non privilégiés cessent d'aboutir : `ls /home`, `du -sh /home/*`, la complétion d'un chemin.
  Sous `sudo`, tout fonctionne comme avant ;
- un agent de sauvegarde ou de supervision qui parcourrait `/home` **sous une identité non root** est à
  revoir. C'est le seul point qui peut réellement mordre ;
- le gain est **partiel**, et le savoir évite de s'en croire protégé : `/etc/passwd` est lisible par tous
  et énumère déjà tous les homes. La mesure gêne le parcours à l'aveugle, elle ne cloisonne rien.

Le traitement n'est jamais récursif : sous la racine vivent les répertoires des projets, dont `ogs` décide
ailleurs (`0711` le home, `0710` ses répertoires). Une racine montée depuis le réseau — `/home` sur NFS —
est rapportée « refusé » : son mode appartient au serveur, et un export en `root_squash` refuserait
`chmod` à root lui-même.

La règle « ne desserre jamais » vaut aussi ici, et elle mord : un `/home` en `0700` ne rend plus un seul
fichier et reste pourtant en place. C'est `ogs doctor` qui le signale — aucune vérification de
configuration ne le verrait, la configuration étant par ailleurs valide.

La table s'ajuste dans `/etc/ogs/config.yaml` — `skip` en écarte un chemin, `targets` remplace la table
**entière**, racine des homes comprise, qu'il faut alors y réinscrire :

```yaml
hardening:
  skip: [/etc/mysql]              # laisser un chemin tranquille
  targets:                        # ou remplacer la table entière
    - path: /etc/monagent
      owner: root
      group: monagent
      dirMode: 0750
      fileMode: 0640
```

**Ce que la commande ne fait pas.** Elle ne touche ni `/etc/systemd/system` — ses unités ne portent aucun
secret et plusieurs outils d'exploitation les lisent — ni `/etc/php` au-delà de son constat. Elle ne
descend pas non plus dans les fichiers d'un répertoire dont la cible ne fixe pas de `fileMode` : fermer un
répertoire suffit à rendre son contenu inatteignable, et modifier chaque fichier multiplierait les risques
sans rien apporter.

## Supervision : fail2ban et Telegraf

`ogs` n'installe ni fail2ban ni Telegraf. Quand ils sont là, il leur écrit ce qu'ils ne peuvent pas
deviner : quels journaux appartiennent à un projet, quels pools tournent sous quelle version, quels
moteurs de bases sont en service. Deux fichiers, recalculés en entier à chaque `apply`, et rien d'autre.

```sh
ogs apply                       # les deux fichiers suivent le parc
ogs secrets list @parc          # les comptes de supervision, sans leur valeur
ogs doctor                      # journaux disparus, droits du fragment, mod_status
```

| Fichier | Ce qu'ogs y met | Ce qu'il n'y met pas |
|---|---|---|
| `/etc/telegraf/telegraf.d/ogs.conf` | les entrées : moteur web, un pool PHP-FPM par pool, les processus de chaque projet, MySQL, PostgreSQL, RabbitMQ | **aucune destination** — la base de métriques, son organisation et son jeton restent dans votre `telegraf.conf` |
| `/etc/fail2ban/jail.d/ogs.conf` | les prisons, toutes préfixées `ogs-`, avec les journaux du parc | **aucun filtre** — ceux de la distribution sont employés tels quels ; votre `jail.local` n'est pas touché |

**Le fragment de Telegraf porte des mots de passe** — ceux des comptes de supervision — et il est donc écrit
en `0640`, groupe de l'agent. Si l'agent ne peut pas le lire, il démarre, ne collecte rien et **ne dit
rien** : `ogs doctor` le signale à sa place.

### L'état des pools passe par un vhost local

`ogs` pose un vhost `ogs-status` qui n'écoute que sur `127.0.0.1:8099` et relaie l'état du serveur web et
de chaque pool. Toute autre adresse d'écoute est refusée au chargement de la configuration.

La raison est simple : le seul autre moyen de donner l'état d'un pool à l'agent serait de lui donner le
socket FPM. Or parler à ce socket, c'est pouvoir y faire exécuter du code sous l'identité du projet — le
protocole FastCGI n'authentifie personne. Le serveur web détient déjà ces sockets ; il sert
d'intermédiaire, et l'agent n'obtient que ce que le pool rend sur son chemin de statut.

Sous Apache, `mod_status` doit être activé (`a2enmod status`) : sans lui, le vhost sert une page vide sans
que rien ne s'en plaigne.

### Les comptes de supervision

Ils appartiennent au parc, non à un projet, et ne peuvent atteindre aucune base de projet.

| Moteur | Privilèges | Authentification |
|---|---|---|
| MySQL / MariaDB | `PROCESS`, `REPLICATION CLIENT`, lecture du `performance_schema` | mot de passe scellé au keystore |
| PostgreSQL | `pg_monitor` | `peer` par le socket local, **sans mot de passe** |
| RabbitMQ | tag `monitoring` seul | mot de passe scellé au keystore |

Le rôle PostgreSQL porte le nom de l'utilisateur système de l'agent — `telegraf` par défaut —, parce que
l'authentification « peer » compare l'utilisateur du système. Changer `monitoring.telegraf.user` change donc
le nom du rôle.

### Un piège à connaître

**Une prison dont fail2ban ne trouve aucun journal l'empêche de démarrer, et il s'arrête alors tout
entier** : les prisons valides tombent avec elle. `ogs` n'écrit donc que des chemins qui existaient au
moment de l'`apply`, recalcule le fichier quand un projet est supprimé, et `ogs doctor` nomme les journaux
disparus — avant qu'un redémarrage ne les découvre.

Pour désactiver l'un ou l'autre, ou une prison précise :

```yaml
# /etc/ogs/config.yaml
monitoring:
  fail2ban:
    enabled: false          # ou : jails: { web-badbots: false }
  telegraf:
    inputs:
      rabbitmq: false
```

## Pare-feu : ufw

`ogs` n'installe pas ufw, et **sans `/etc/ogs/ufw.yaml` il n'y touche pas**. Pour lui confier le pare-feu :

```sh
sudo ogs firewall init        # ufw.yaml de départ, d'après les services installés
sudo ogs firewall profiles    # profils détectés : ports, disponibilité, sources
sudo ogs firewall plan -v     # les commandes ufw qui seraient passées
sudo ogs firewall apply
```

`init` fait de l'adresse de votre session SSH une source de confiance, qui a accès à tous les profils
disponibles. S'il ne la trouve pas, SSH reste ouvert à tous : c'est à restreindre.

### Confirmer la première activation

Quand ufw passe d'inactif à actif, `ogs` programme sa désactivation au bout de cinq minutes
(`rollbackAfter`). **Ouvrez une nouvelle session SSH** et lancez-y :

```sh
sudo ogs firewall confirm
```

C'est cette nouvelle session qui prouve que le pare-feu laisse passer l'administration. Une session ouverte
avant l'activation survit à des règles qui refuseraient la suivante. Sans confirmation, ufw se désactive de
lui-même, et le prochain `apply` le réactivera, de nouveau à confirmer ; un `confirm` tardif le dit — ufw
inactif — et rend le code 6.

**Un redémarrage de la machine dans le délai vaut retour** : le minuteur vit sous `/run` et ne survit pas,
mais l'activation laisse un témoin, `/var/lib/ogs/ufw-unconfirmed`, et un complément d'unité,
`/etc/systemd/system/ufw.service.d/ogs-rollback.conf` : au démarrage de la machine, tant que le témoin
existe, ufw reste inactif, et son journal (`journalctl -u ufw`) dit pourquoi. Relancer l'unité
(`systemctl restart ufw`) ne désactive rien. `ogs firewall status` et `ogs doctor` le signalent tant
qu'aucun `confirm` ne les a retirés ; `ogs firewall apply` réactive, à confirmer de nouveau. Un ufw
désactivé dans le délai n'a rien à confirmer : `confirm` le dit et rend le code 6.

### Les gardes contre la coupure

`ogs` refuse une déclaration qui couperait la session SSH en cours, bannirait son adresse, fermerait un port
où sshd écoute réellement — lu par `ss`, et sur une `ssh.socket` active — ou fermerait SSH à tous. Il juge ce
qui **restera ouvert** : les règles voulues, et les vôtres, qu'il ne retire jamais. Changer les ports d'un
profil qu'une règle emploie compte comme un retrait : `ufw app update` ferme l'ancien port.

La garde se lève par **`--allow-lockout`**, sur `ogs firewall plan`, `apply`, `ban` et `unban` seulement. Le
`--force` d'`ogs apply` — celui des fichiers retouchés — ne la lève jamais : on le donne pour un vhost, sans
penser au pare-feu, et il retirait la dernière règle ssh. Une déclaration refusée ne bloque pas le reste du
parc : `ogs apply` en fait une étape en échec, code 6, et seules les commandes `ogs firewall` rendent le
code 3.

Qui administre par un VPN : une règle à vous liée à une interface (`in on wg0`) garde SSH ouvert aux yeux de
la garde quand aucune session n'est à protéger — depuis la console. Pour une session, rien ne dit par quelle
interface elle est arrivée : la garde refuse alors, et le dit ; une règle vers l'adresse du serveur que la
session a jointe, elle, se vérifie.

### Ce qu'ogs touche, et ce qu'il laisse

| Élément | Géré par ogs |
|---|---|
| `/etc/ufw/applications.d/ogs` | oui : profils `ogs-ssh`, `ogs-sftp`, `ogs-mysql`… aux ports lus sur la machine |
| règles dont le commentaire commence par `ogs:` | oui : posées, corrigées, retirées |
| toute autre règle | **non** : laissée intacte, comptée par `ogs doctor` |
| profils des paquets (`OpenSSH`, `Apache Full`…) | non |
| politiques par défaut, journalisation | seulement si `ufw.yaml` les déclare, ou pour les défauts d'ufw (entrée `deny`, sortie `allow`) |

Pour savoir à qui appartient une adresse, et ce qu'elle a le droit de faire :

```sh
sudo ogs firewall whois 198.51.100.7
sudo ogs firewall ban 192.0.2.66 --comment "scan du 12/09"
sudo ogs firewall unban 192.0.2.66
```

`ban` et `unban` calculent d'abord le plan du document modifié : un bannissement que la garde refuse, ou
une confirmation déclinée, laissent `ufw.yaml` intact. Un problème que `ufw.yaml` portait déjà, sans
rapport avec le bannissement, ne l'empêche pas d'être écrit : il est signalé, et le pare-feu reste à
corriger (code 6 sous `--no-apply`, 3 sinon) ; sous `--no-apply`, de même d'un refus de garde déjà
présent — un refus que le bannissement fait naître n'écrit jamais rien. Sinon ils l'écrivent sans en perdre les
commentaires, puis appliquent. Les adresses de confiance sont aussi ajoutées à l'`ignoreip` des prisons
fail2ban d'ogs, au prochain `ogs apply` ; un `ufw.yaml` illisible ne les en retire pas. Sous `--dry-run`,
ils refusent l'option (code 2) plutôt que de réécrire `ufw.yaml`.

### Trois pièges

- **Docker publie les ports de ses conteneurs en amont d'ufw** : un port publié est ouvert quelles que soient
  les règles. `ogs doctor` le signale.
- **Apache et nginx installés tous deux, sans `web.engine`** : le profil web reste disponible — un serveur
  qu'on ne sait pas nommer n'est pas l'absence de serveur web —, et un `ban` ne retire plus l'accès public
  aux sites.
- **Un FTP sans plage passive connue est refusé** : la connexion passerait, et chaque transfert resterait
  bloqué. Déclarez `profiles.ftp.passive`, ou `profiles.ftp.available: false` si proftpd ne sert qu'au
  SFTP.

## Archives et rétention

Supprimer un projet produit une archive dans `/var/lib/ogs/archives` : déclaration, fichiers générés,
secrets chiffrés, arborescence et dump des bases. **Elle est conservée 14 jours**, puis purgée par une
étape du plan — annoncée par `ogs plan`, auditée, jamais silencieuse.

```sh
ogs project archives            # la colonne PURGE dit ce qu'il reste à chaque archive
```

Le délai se règle par `archives.retentionDays` dans `/etc/ogs/config.yaml`. La valeur `0` signifie
« conserver indéfiniment » : c'est un choix explicite, pas un défaut, car une archive contient des
données de clients.

## Sauvegarde

Trois choses, par ordre d'importance :

1. `/etc/ogs/master.key` — irremplaçable ;
2. `/etc/ogs/projects/` — les déclarations, qui décrivent le serveur ;
3. `/var/lib/ogs/state.db` — les mots de passe scellés des bases, d'AMQP et de supervision, les clés DKIM,
   les preuves de propriété des ressources partagées (D91, D93), les numéros attribués, les empreintes et
   l'audit. Les déclarations ne suffisent pas à la reconstruire.

**Une base d'état disparue n'est jamais recréée en silence** (D120). Tant que des projets sont déclarés,
les commandes qui en dépendent refusent en code 5 et `ogs doctor` le compte en échec ; seul le
renouvellement des certificats tourne sans elle — il ne réserve rien —, sur une base éphémère, et rend le
code 6 en la nommant, pour qu'aucun certificat ne périme pendant qu'on cherche la sauvegarde. Le remède :
restaurer `state.db` (arrêter le timer `ogs-renew` le temps de la copie) ; à défaut, `ogs init` la recrée
vide et le dit. Les numéros de ports et de pools écrits dans les fichiers sont alors repris au prochain apply
(D94) ; les mots de passe scellés ne le sont pas, et ce qu'ils prouvaient — bases, comptes, vhosts AMQP créés
par ogs — passe pour étranger : chaque refus le nomme.

**Reconstruire un serveur** à partir de la clé maître et des déclarations, sans `state.db` : installer le
paquet, déposer `/etc/ogs/master.key` et `/etc/ogs/projects/`, puis lancer `ogs init` à la main — le paquet
ne le fait pas quand il trouve des déclarations sans base, et toute commande qui écrit refuse en code 5
jusque-là. `init` dit qu'il recrée la base d'un parc existant, puis `ogs doctor` et `ogs plan` avant
`ogs apply`. Les bases et comptes SGBD d'origine, sans leurs mots de passe scellés, sont refusés comme
étrangers (D91), et chaque refus le nomme.

Les données des sites (homes, bases) relèvent de la politique de sauvegarde habituelle. `ogs db dump` et les
archives produites par `ogs project delete` ne s'y substituent pas.

Les copies que `--force` fait avant de reprendre un fichier retouché vont dans `/var/lib/ogs/backups`, sous
`<nom>.ogs-bak-<horodatage>` — suffixé `.1`, `.2`… quand une autre sauvegarde de la même seconde porte déjà
ce nom : un pool et un vhost s'appellent tous deux `<id>.conf`. Rien ne les purge.
