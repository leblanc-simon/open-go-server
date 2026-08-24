# Cahier des charges technique — `ogs` (open-go-server)

**Version** : 1.0 — 22 août 2026
**Objet** : utilitaire CLI/TUI d'administration de serveurs web mutualisés, écrit en Go.
**Statut** : spécification validée, prête pour la réalisation.

---

## 1. Objet et périmètre

### 1.1 Problème traité

L'exploitation d'un serveur hébergeant plusieurs applications web (typiquement Symfony) impose de créer et
maintenir, pour chaque projet, un ensemble cohérent de ressources hétérogènes : utilisateur système, bases
de données MySQL et/ou PostgreSQL, vhost AMQP et queues RabbitMQ, pool PHP-FPM d'une version donnée, vhost
Apache ou Nginx, certificat TLS, tâches planifiées et services systemd de workers. Ces opérations sont
aujourd'hui manuelles, non reproductibles, sans trace, et leur suppression laisse systématiquement des
résidus.

`ogs` industrialise ce cycle de vie : un projet est décrit par un fichier YAML, et un moteur de
réconciliation idempotent amène le serveur dans l'état décrit.

### 1.2 Dans le périmètre

- Utilisateurs et groupes système, arborescence applicative, permissions.
- Bases MySQL/MariaDB et PostgreSQL : bases, utilisateurs, privilèges, extensions.
- RabbitMQ : vhosts, utilisateurs, permissions, exchanges, queues, bindings, policies.
- PHP-FPM multi-version (7.4 → 8.5) : pools, presets de dimensionnement, bascule de version contrôlée.
- Types d'application Prestashop et Symfony (3.4 → 8), docroot et vhost adaptés à la variante détectée.
- Apache 2.4 et Nginx : vhosts, alias, redirections, blocs de configuration additionnels.
- TLS : émission, renouvellement et révocation de certificats Let's Encrypt (ACME), certificats
  auto-signés pour le développement.
- Tâches planifiées via `/etc/cron.d`.
- Services systemd générés depuis des templates, avec instances multiples (workers Messenger).
- Applications **Go, Node.js, Ruby et Python** : runtime, processus supervisé, reverse-proxy.
- Clé de déploiement ed25519 par projet.
- Commandes composites de haut niveau (`site create`) orchestrant l'ensemble.
- Import assisté d'un serveur existant.

### 1.3 Hors périmètre

- Sauvegardes planifiées, politiques de rétention, restauration point-in-time. Seules des commandes de
  `dump`/`restore` ponctuelles existent, utilisées notamment par l'archivage à la suppression.
- Installation et mise à jour des paquets système (Apache, PHP, MySQL…) : ils sont supposés présents.
  `ogs doctor` se contente de diagnostiquer ce qui manque.
- DNS, messagerie, pare-feu, monitoring, métriques, alerting.
- **Quotas disque** : hors périmètre, y compris à terme.
- Compilation et build applicatif (`go build`, `npm run build`, `bundle install`) : relève du déploiement. `ogs` installe le runtime et supervise le processus, il ne construit pas l'application.
- Déploiement applicatif (git, composer, assets) : relève d'un outil de déploiement dédié.
- Conteneurs et orchestration.

### 1.4 Environnement cible

| Élément | Valeur |
|---|---|
| Distributions | Debian 12/13, Ubuntu 22.04 / 24.04 / 26.04 |
| Architectures | amd64 |
| Go | 1.26+ |
| Exécution | locale, sur la machine administrée, **root strict** |
| PHP | 7.4, 8.0, 8.1, 8.2, 8.3, 8.4, 8.5 (dépôt Sury), multi-version simultanée |
| Bases | MySQL 8.x, PostgreSQL 14+ (multi-cluster) |
| RabbitMQ | 3.12+ avec plugin `rabbitmq_management` |

---

## 2. Décisions d'architecture

Décisions arbitrées et figées pour la v1. Toute remise en cause est une évolution majeure.

| # | Décision | Choix retenu | Conséquence principale |
|---|---|---|---|
| D1 | Modèle d'application | **Hybride déclaratif** : les commandes écrivent un YAML, un moteur idempotent l'applique | Impose `plan`/`apply`/`diff` et un état désiré persistant |
| D2 | Source de vérité | **YAML** (état désiré, versionnable) **+ SQLite** (état observé, secrets, audit) | Deux stockages à garder cohérents ; réconciliation explicite |
| D3 | Secrets | **Keystore chiffré local**, relecture possible via commande auditée | Clé maître à protéger ; modèle de menace documenté |
| D4 | Portée | **Locale**, derrière une abstraction `Executor` | Backend SSH ajoutable en v2 sans refonte |
| D5 | Entité racine | **Projet** (1 user, N domaines, N ressources) | `site create` est une façade sur `project` |
| D6 | Suppression | **Archivage par défaut**, purge explicite | Répertoire d'archives à gérer |
| D7 | Serveurs web | **Apache et Nginx**, interface commune, un moteur par projet | Deux jeux de templates, une seule abstraction |
| D8 | Bases | MySQL/MariaDB + PostgreSQL, **connexion admin par socket Unix** | Aucun mot de passe root stocké |
| D9 | RabbitMQ | **Un vhost par projet**, pilotage par **API HTTP management** | Plugin management requis ; fallback `rabbitmqctl` dégradé |
| D10 | PHP | **Multi-version complet**, presets, `php switch` | Détection dynamique des versions installées |
| D11 | TLS | **lego embarqué**, HTTP-01 + DNS-01 | Aucune dépendance à certbot ; timer systemd propre à `ogs` |
| D12 | Crons | **`/etc/cron.d/ogs-<id>`** généré, `flock` systématique | Fichier entièrement piloté par l'outil |
| D13 | systemd | **Unités templatisées** `@.service` + `.target`, instances multiples | `daemon-reload` groupé en fin d'apply |
| D14 | TUI | **Inspecteur + actions + wizard** (Bubble Tea) | Composant majeur, livré en lot dédié |
| D15 | Isolation | User dédié, shell `nologin`, **docroot simple**, home `/home/<host effectif>` en 0750 | Pas de mécanique `releases/` ; `/home` doit rester traversable |
| D16 | Échecs composites | **Rollback avec étapes optionnelles non bloquantes** | Chaque étape déclare sa criticité et sa compensation |
| D17 | Tests | Unitaires + **intégration en conteneurs systemd** | CI plus lourde mais garanties réelles |
| D18 | Binaire | **`ogs`**, statique, `.deb` via GoReleaser | CGO désactivé → SQLite en Go pur |
| D19 | Extensibilité | Templates surchargeables + **hooks** pre/post | Contrat de hook à documenter et figer |
| D20 | Adoption | **Import assisté** interactif, ressources non reconnues intactes | Notion de ressource « non gérée » dans le modèle |
| D21 | Accès | **Root strict** pour toute opération d'administration, avec une exception délimitée : `ogs my <verbe>` (D28) | Audit non nominatif (voir §14.3) |
| D22 | Sorties | `--output json\|yaml\|table`, codes de retour normalisés, complétion | Toute commande de lecture doit être sérialisable |
| D23 | Nommage | **Un identifiant unique** dérivé du host nomme user, groupe, bases, vhost AMQP, pool, vhost web, unités, cron | Contrainte de longueur commune à 32 car. ; suffixes de rôle pour les ressources additionnelles |
| D24 | Environnements | `prod` et `prep` ; la préproduction suffixe l'identifiant par **`prep`** (sans séparateur) et vit sur un domaine dédié : `<id de prod>.preprod-pp.net` | Réserve 4 car. sur les 32 disponibles ; le domaine est une clé de configuration globale |
| D25 | Accès du serveur web | **Groupe privé par projet**, `www-data` ajouté comme **membre secondaire** de chaque groupe | Isolation inter-sites préservée ; `.env` et `var/` restent hors de portée de `www-data` |
| D26 | Certificats partagés | Un certificat peut être de **portée serveur** et référencé par `@<nom>` ; la préproduction utilise un wildcard `*.preprod-pp.net` unique | Impose DNS-01 et des credentials en écriture sur la zone ; supprime tout appel ACME à la création d'une préproduction |
| D27 | DNS | **Hors périmètre** : un wildcard `*.preprod-pp.net` est créé manuellement une fois ; `doctor` vérifie seulement la résolution | Un seul serveur de préproduction par domaine |
| D28 | Délégation | L'utilisateur du projet pilote ses services par **sudoers énuméré, sans métacaractère** ; `ogs my <verbe>` déduit le projet de **`SUDO_UID`** | Aucune cible à détourner ; fichier sudoers validé par `visudo -cf` avant activation ; dépend de `env_reset`, vérifié par `doctor` |
| D29 | Logs | Logs d'infrastructure dans **`/home/<host>/logs/`**, répertoire **root** avec fichiers pré-créés ; rotation par un `/etc/logrotate.d/ogs` unique | Bloque l'escalade root par substitution de lien symbolique ; un seul reload par rotation |
| D30 | Interpréteur | Crons et unités invoquent **`/home/<host>/.bin/php`**, lien géré | Une bascule de version ne réécrit ni cron ni unité |
| D31 | Transport FPM | **Socket Unix par défaut**, numéroté selon la norme `1<version><incrément>` ; TCP possible mais explicite | Le numéro est un identifiant logique, pas un port ouvert |
| D32 | Layout | Code applicatif dans **`/home/<host>/www`** ; le docroot dépend du **type d'application** (`www` pour Prestashop, `www/public` pour Symfony) | Un type d'application devient une donnée de premier plan du modèle |
| D33 | Clé de déploiement | Paire **ed25519 sans passphrase** générée à la création, privée conservée sur le serveur, publique à déclarer en deploy key | Déploiement en **tirage** (le serveur clone) ; aucune surface SSH entrante, `nologin` conservé |
| D34 | Applications non-PHP | **Go, Node, Ruby, Python** supportés : processus supervisé par systemd + vhost en reverse-proxy | Élargit le périmètre à la supervision de runtimes applicatifs |
| D35 | Runtimes | `nvm`, `rbenv`, `pyenv`/`venv` installés **par utilisateur** dans son home, liens gérés dans `.bin/` | Une copie par site ; intégrité du runtime hors du contrôle de l'inventaire |
| D36 | Écoute des applications | **Socket Unix** `0660 www-data:<id>` quand le runtime le permet ; sinon port TCP `2<runtime><incrément>` sur **127.0.0.1 exclusivement** | Isolation équivalente à celle des pools PHP |
| D37 | Types livrés | **Prestashop** et **Symfony 3.4 → 8** (+ `static`, `php-plain` et les 4 runtimes) ; les autres frameworks s'ajoutent par `apptypes.yaml` sans code | Symfony 3.4 impose une variante distincte : docroot `web`, `app.php`, pas de Messenger |
| D38 | Compatibilité PHP | La contrainte `require.php` du **`composer.json` de l'application** fait autorité ; table indicative en secours seulement | Aucune matrice figée à maintenir ; `php switch` refuse une bascule incompatible sans `--force` |
| D39 | Versions PHP | **7.4 à 8.5**, y compris des versions en fin de support | `doctor` les signale sans jamais bloquer : un Prestashop ancien peut exiger 7.4 |
| D41 | Droits de `/var/lib/ogs` | **0751** et non 0750 | Le serveur web doit traverser jusqu'à `acme-challenge` : en 0750 tout challenge HTTP-01 se voit refuser par un 403. Le bit d'exécution seul n'autorise pas l'énumération, et les contenus sensibles gardent leurs propres droits |
| D42 | Webroot du challenge | Le client ACME écrit dans `<webroot>/.well-known/acme-challenge/` : **Apache aliase ce répertoire, nginx prend le webroot** et y accole l'URI | Les deux directives n'ont pas la même sémantique ; une seule valeur de template menait fatalement l'un des deux moteurs à un 404 |
| D43 | TLS dans les templates | **Tous** les templates de vhost servent le HTTPS, non seulement ceux des sites PHP | Un site statique, une application derrière un proxy — donc tous les runtimes non-PHP — et un domaine redirigé ont autant besoin d'être chiffrés |
| D40 | Hooks | **Deux portées** : un fichier de `/etc/ogs/hooks` nommé d'après l'événement s'applique à tout le parc, `spec.hooks` le remplace pour un projet. Criticité déduite du préfixe : `pre` interrompt, `post` avertit | Un hook s'exécute en root : le script doit vivre sous `/etc/ogs/hooks`, appartenir à root et n'être inscriptible par personne d'autre, sans quoi il est refusé — c'est une escalade de privilèges autrement. Aucun hook en `--dry-run` |

---

## 3. Modèle de domaine

### 3.1 Entités

```
Project (racine)
├── App               1    type (kind), docroot déduit, runtime et version éventuels
├── SystemUser        1    user + groupe Linux, home, shell, clé de déploiement
├── Domain            N    nom de domaine, rôle (principal, alias, redirection)
├── Database          N    moteur, nom, utilisateur, privilèges, extensions
├── AMQP              0..1 vhost, utilisateur, exchanges, queues, bindings, policies
├── PHPPool           N    version, preset, réglages surchargés
├── VirtualHost       N    moteur web, domaines servis, docroot, pool, TLS, blocs
├── Certificate       N    domaines couverts, challenge, provider DNS, état
├── CronJob           N    expression, commande, user, verrou, log
└── Service           N    template, instances, paramètres, état systemd
```

Règles d'intégrité :

- Un projet possède exactement un `SystemUser`. Deux projets ne partagent jamais d'utilisateur.
- Un domaine appartient à un seul projet ; l'unicité est globale sur le serveur.
- Un `VirtualHost` référence un `PHPPool` du même projet, ou aucun (site statique, reverse-proxy).
- Un `Certificate` de projet ne couvre que des domaines du projet. Un certificat peut aussi être de
  **portée serveur** (déclaré dans la configuration globale, référencé par `@<nom>`) et être partagé par
  plusieurs projets : c'est le cas du wildcard de préproduction (§7.7).
- La suppression d'un projet supprime ses ressources filles en cascade, dans l'ordre inverse des
  dépendances.

### 3.2 Format du fichier projet

`/etc/ogs/projects/<id>.yaml`

```yaml
apiVersion: ogs/v1
kind: Project
metadata:
  name: shopfr                    # identifiant unique, dérivé du host (§3.3)
  host: shop.fr                   # host canonique, source de l'identifiant
  env: prod                       # prod | prep  → prep suffixe l'identifiant par "prep"
  description: "Boutique en ligne"
  labels: { client: acme }

spec:
  user:
    name: shopfr                  # = metadata.name, jamais saisi séparément
    group: shopfr                 # groupe privé du projet (§7.1)
    uid: auto
    shell: /usr/sbin/nologin
    home: /home/shop.fr           # = /home/<host effectif>
    webAccess: group              # group | acl | none  (accès de www-data, §7.1)
    authorizedKeys: []            # optionnel

  layout:
    root: /home/shop.fr
    docroot: /home/shop.fr/www/public
    logDir: /home/shop.fr/logs      # root:<id> 0750, non inscriptible par l'utilisateur
    mode: "0750"                  # racine : rwx utilisateur, r-x groupe, rien pour les autres
    dirs:                         # créés avec propriétaire et mode explicites
      - { path: www,        mode: "0750" }
      - { path: www/public, mode: "0750" }   # selon le type d'application
      - { path: var,        mode: "0700" }
      - { path: logs,       mode: "0750", owner: root }
      - { path: .bin,       mode: "0750", owner: root }
      - { path: .ssh,       mode: "0700" }
      - { path: var/tmp,      mode: "0700" }
      - { path: var/sessions, mode: "0700" }

  app:
    kind: php-symfony             # voir §7.10 pour la liste
    version: auto                 # auto → lu dans composer.lock ; ou "3.4", "6.4", "8.0"
    # docroot déduit du couple kind/version, surchargeable par layout.docroot

  domains:
    - { name: shop.fr, role: primary }
    - { name: www.shop.fr, role: redirect, target: shop.fr }
    - { name: api.shop.fr, role: primary }

  databases:
    - engine: mysql               # mysql | mariadb | postgres
      name: shopfr
      user: shopfr
      host: localhost             # hôte autorisé pour le grant
      charset: utf8mb4
      collation: utf8mb4_unicode_ci
      privileges: all             # all | crud | readonly | [liste]
      extraUsers:
        - { user: shopfr_ro, privileges: readonly }
    - engine: postgres
      cluster: "17/main"
      name: shopfr_analytics
      user: shopfr_pg
      extensions: [uuid-ossp, pg_trgm]

  amqp:
    vhost: shopfr
    user: shopfr
    permissions: { configure: ".*", write: ".*", read: ".*" }
    exchanges:
      - { name: messages, type: direct, durable: true }
      - { name: dlx, type: direct, durable: true }
    queues:
      - name: async
        type: quorum              # classic | quorum
        durable: true
        arguments:
          x-dead-letter-exchange: dlx
          x-message-ttl: 86400000
      - { name: failed, type: classic, durable: true }
    bindings:
      - { exchange: messages, queue: async, routingKey: async }
      - { exchange: dlx, queue: failed, routingKey: async }
    policies:
      - { name: ha, pattern: "^.*", definition: { max-length: 100000 } }

  sudo:
    style: ogs                    # ogs | systemctl | none
    verbs: [start, stop, restart, status]

  php:
    pools:
      - name: web                 # → pool php nommé shopfr
        version: "8.5"
        listen: unix              # unix (défaut) | tcp
        number: auto              # 18501 — norme 1<version><incrément>, §7.5
        preset: medium            # micro | small | medium | large | custom
        settings:                 # surcharges optionnelles
          pm: dynamic
          pm.max_children: 20
          php_admin_value[memory_limit]: 256M
          php_admin_value[open_basedir]: /home/shop.fr:/tmp
        env:
          APP_ENV: prod

  web:
    engine: apache                # apache | nginx
    vhosts:
      - name: shopfr
        template: php-fpm         # static | php-fpm | proxy | redirect
        domains: [shop.fr, www.shop.fr]
        docroot: /home/shop.fr/www/public
        pool: web
        tls: { enabled: true, certificate: shopfr }
        options:
          hsts: true
          gzip: true
          clientMaxBodySize: 32M
          indexes: false
        blocks:                   # fragments bruts injectés
          - name: cache-assets
            content: |
              <FilesMatch "\.(js|css|woff2)$">
                Header set Cache-Control "max-age=31536000, immutable"
              </FilesMatch>

  certificates:
    - name: shopfr
      domains: [shop.fr, www.shop.fr]
      challenge: http-01          # http-01 | dns-01 | selfsigned
      # dnsProvider: ovh          # requis si dns-01
      keyType: ec256
      staging: false

  crons:
    - name: scheduler
      schedule: "* * * * *"
      command: "bin/console schedule:run"
      php: "8.4"                  # utilise le binaire CLI de cette version
      workdir: /home/shop.fr
      lock: true
      log: sched.log
      enabled: true

  services:
    - name: messenger-async
      template: symfony-messenger
      instances: 3
      params:
        transport: async
        timeLimit: 3600
        memoryLimit: 128M
        limit: 500                # messages avant arrêt propre
      enabled: true

  hooks:
    postCreate: /etc/ogs/hooks/provision-app.sh
```

Tous les blocs sont optionnels : un projet peut se limiter à `user` + `layout`.

### 3.3 Nom unique et conventions de nommage

**Principe** : un projet possède **un seul identifiant**, dérivé de son host, et cet identifiant nomme
absolument tout — utilisateur Linux et son groupe, base de données, utilisateur de base, vhost et
utilisateur RabbitMQ, pool PHP-FPM, fichier de vhost web, unités systemd, fichier cron, fichier YAML du
projet. Le seul chemin qui garde le host lisible est le home.

#### 3.3.1 Dérivation host → identifiant

```
host canonique            shop.fr
  ├─ retrait du préfixe   www. (ignoré : www.shop.fr et shop.fr donnent le même identifiant)
  ├─ minuscules, retrait des points et de tout caractère hors [a-z0-9-]
  ├─ retrait des tirets    (ma-boutique.fr → maboutiquefr)
  ├─ préfixe « s » si le résultat commence par un chiffre : `useradd` refuse par
  │                       défaut un nom ne débutant pas par une lettre
  │                       (1001pneus.fr → s1001pneusfr)
  └─ identifiant           shopfr
```

Exemples :

| Host | Identifiant | Home |
|---|---|---|
| `shop.fr` | `shopfr` | `/home/shop.fr` |
| `www.shop.fr` | `shopfr` | `/home/shop.fr` |
| `api.shop.fr` | `apishopfr` | `/home/api.shop.fr` |
| `ma-boutique.fr` | `maboutiquefr` | `/home/ma-boutique.fr` |
| `1001pneus.fr` | `s1001pneusfr` | `/home/1001pneus.fr` |
| `ma-boutique-de-vente-en-ligne-de-chaussures.fr` | `maboutiquedeventeenli_e20de4` | `/home/ma-boutique-de-vente-en-ligne-de-chaussures.fr` |

#### 3.3.2 Longueurs

La contrainte la plus stricte est **32 caractères** (nom d'utilisateur Linux et utilisateur MySQL 8). Le
suffixe d'environnement `prep` en consomme 4, donc :

- longueur utile de l'identifiant de base : **28 caractères** ;
- au-delà, troncature déterministe : **21 premiers caractères + `_` + 6 hexadécimaux** du SHA-256 du host
  canonique complet, soit 28 caractères — deux hosts longs partageant leurs 21 premiers caractères restent
  ainsi distincts ;
- l'identifiant de base est identique en production et en préproduction : la troncature porte toujours sur
  le host, jamais sur le suffixe d'environnement.

Les ressources additionnelles (§3.3.4) peuvent dépasser 32 caractères pour les objets qui l'autorisent
(base MySQL : 64, rôle PostgreSQL : 63). Pour un objet limité à 32 (utilisateur), la même troncature est
appliquée au nom complet et un avertissement explicite est émis lors du `plan`.

#### 3.3.3 Environnements

Deux environnements : `prod` (défaut) et `prep`.

| | Production | Préproduction |
|---|---|---|
| Host effectif | `shop.fr` (+ alias) | `shopfr.preprod-pp.net` |
| Home | `/home/shop.fr` | `/home/shopfr.preprod-pp.net` |
| Identifiant (tout le reste) | `shopfr` | `shopfrprep` |
| Certificat | propre au projet | wildcard partagé `*.preprod-pp.net` |

La préproduction vit sur un **domaine dédié**, configuré globalement et non par projet :

```yaml
# /etc/ogs/config.yaml
environments:
  prod:
    hostTemplate: "{{.Host}}"                        # host canonique tel que saisi
  prep:
    domain: preprod-pp.net
    hostTemplate: "{{.Id}}.{{.Domain}}"              # → shopfr.preprod-pp.net
    idSuffix: prep                                   # → shopfrprep
    certificate: "@wildcard-prep"                    # certificat partagé (§7.7.4)
    robotsNoindex: true
    basicAuth: false                                 # activable par projet
    phpEnv: { APP_ENV: dev }
    presetDowngrade: true                            # medium → small
```

Points à noter :

- Le sous-domaine reprend l'identifiant **de production** (`shopfr`), pas celui de la préproduction : le
  domaine `preprod-pp.net` porte déjà l'information d'environnement, la répéter dans le sous-domaine serait
  redondant. L'identifiant des ressources système reste `shopfrprep`, seul moyen de faire coexister les deux
  environnements sur un même serveur.
- Le home suit la règle invariante `/home/<host effectif>`, donc `/home/shopfr.preprod-pp.net`.
- Le host de préproduction n'est **jamais** soumis à la dérivation du §3.3.1 : il est construit, pas dérivé.
  L'identifiant reste calculé depuis le host canonique de production, ce qui évite toute circularité.
- `--host` permet de forcer un host de préproduction hors convention (client exigeant son propre
  sous-domaine) ; le certificat partagé ne s'applique alors plus et un certificat propre est émis.

#### 3.3.4 Tableau de nommage

`<id>` désigne l'identifiant unique du projet (`shopfr`, ou `shopfrprep` en préproduction).

| Ressource | Nom | Exemple | Contrainte |
|---|---|---|---|
| Identifiant projet | dérivé du host | `shopfr` | `[a-z][a-z0-9]{1,27}`, ≤ 28 (+4 si `prep`) |
| Fichier projet | `<id>.yaml` | `/etc/ogs/projects/shopfr.yaml` | — |
| Home | `/home/<host effectif>` | `/home/shop.fr` | traversable, voir §7.1 |
| User + groupe Linux | `<id>` | `shopfr` | ≤ 32 car. |
| Base principale | `<id>` | `shopfr` | ≤ 64 car. (MySQL), 63 (PG) |
| Base additionnelle | `<id>_<rôle>` | `shopfr_analytics` | idem |
| User de base principal | `<id>` | `shopfr` | ≤ 32 car. (MySQL 8) |
| User de base additionnel | `<id>_<rôle>` | `shopfr_ro` | idem |
| Vhost AMQP | `<id>` | `shopfr` | sans `/` initial : pas d'encodage `%2F` dans les DSN |
| User AMQP | `<id>` | `shopfr` | — |
| Pool PHP principal | `<id>` | `shopfr` | fichier `<id>.conf` |
| Pool PHP additionnel | `<id>_<rôle>` | `shopfr_worker` | — |
| Numéro de pool | `1<version><incrément 2 ch.>` | `18501` (php 8.5, 1er pool) | 100 pools par version |
| Socket FPM | `/run/php/php<ver>-fpm-<numéro>-<pool>.sock` | `/run/php/php8.5-fpm-18501-shopfr.sock` | ≤ 107 car. (`sun_path`) |
| Vhost web principal | `<id>.conf` | `shopfr.conf` | — |
| Vhost web additionnel | `<id>_<rôle>.conf` | `shopfr_api.conf` | — |
| Certificat | `<id>` ou `<id>_<rôle>` | `shopfr` | répertoire sous `acme/certificates/` |
| Unité systemd | `ogs-<id>-<service>@.service` | `ogs-shopfr-messenger-async@.service` | — |
| Cible systemd | `ogs-<id>.target` | `ogs-shopfr.target` | — |
| Fichier cron | `/etc/cron.d/ogs-<id>` | `/etc/cron.d/ogs-shopfr` | pas de `.` dans le nom |
| Logs du projet | `/home/<host effectif>/logs/` | `/home/shop.fr/logs/` | répertoire root, §7.1.2 |
| Lien interpréteur | `/home/<host effectif>/.bin/php` | `/home/shop.fr/.bin/php` | répertoire root |
| Sudoers | `/etc/sudoers.d/ogs-<id>` | `/etc/sudoers.d/ogs-shopfr` | 0440 root:root, §7.9.1 |

Les modèles sont surchargeables dans `/etc/ogs/config.yaml`, mais le principe du nom unique est structurant :
le changer revient à changer la convention de tout le parc.

#### 3.3.5 Conséquence impérative sur les GRANT MySQL

L'underscore est un **joker** dans les motifs de privilèges MySQL. Un nom contenant `_` — cas des
ressources additionnelles (`shopfr_analytics`) et des identifiants tronqués (`maboutiquedeventeenli_e20de4`) —
doit donc être échappé, sans quoi le privilège porte sur bien plus que la base visée :

```sql
-- FAUX : donne aussi accès à shopfrXanalytics, shopfr0analytics…
GRANT ALL PRIVILEGES ON `shopfr_analytics`.* TO 'shopfr'@'localhost';
-- CORRECT
GRANT ALL PRIVILEGES ON `shopfr\_analytics`.* TO 'shopfr'@'localhost';
```

L'échappement est appliqué systématiquement par le provider MySQL, y compris sur les noms sans underscore,
et un test unitaire dédié le verrouille.

#### 3.3.6 Unicité vérifiée à la validation

Sont uniques sur le serveur, et vérifiés avant tout apply : l'identifiant de projet, le host effectif, le
home, chaque domaine servi, le nom d'utilisateur Linux, chaque nom de base et d'utilisateur de base, le
vhost AMQP, chaque nom de pool et de socket, chaque nom d'unité systemd.

Deux collisions réelles, refusées en code 4 avec le nom du projet détenteur et la suggestion d'un `--id`
explicite :

- **Identifiants convergents** : les tirets étant supprimés, `mon-shop.fr` et `monshop.fr` produisent tous
  deux `monshopfr`. La troncature par hachage ne protège que les noms longs ; les collisions entre noms
  courts doivent être détectées et rejetées.
- **Empiètement sur le domaine de préproduction** : un projet de production dont le host canonique se
  termine par `.preprod-pp.net` est refusé, ce domaine étant réservé à l'espace de noms de préproduction
  (§3.3.3).

---

## 4. Arborescence système

```
/usr/bin/ogs                              binaire
/etc/ogs/
  config.yaml                             configuration globale
  master.key                              clé de chiffrement du keystore (0400 root)
  projects/<id>.yaml                    état désiré (versionnable dans git)
  templates/                              surcharges utilisateur des templates embarqués
    apache/php-fpm.conf.tmpl
    nginx/php-fpm.conf.tmpl
    php/pool.conf.tmpl
    systemd/symfony-messenger.service.tmpl
    cron/job.tmpl
  hooks/                                  scripts utilisateur
  credentials/dns/<provider>.yaml         credentials DNS-01 (chiffrés, 0600)
  htpasswd/<id>                           authentification basique (préproduction)
/etc/sudoers.d/ogs-<id>                   délégation à l'utilisateur du projet (0440)
/etc/logrotate.d/ogs                      rotation des logs de tous les projets (généré)
/etc/tmpfiles.d/ogs.conf                  répertoires /run/ogs/<id> (généré)

/var/lib/ogs/
  state.db                                SQLite : état observé, secrets, audit
  acme/                                   compte ACME, clés, certificats
    accounts/<ca>/<email>/
    certificates/<id>/{cert.pem,chain.pem,fullchain.pem,privkey.pem}      certificat de projet
    certificates/_wildcard.preprod-pp.net/{…}                             certificat partagé
  acme-challenge/                         webroot partagé HTTP-01
  archives/<id>-<horodatage>.tar.zst    archives de suppression

/var/log/ogs/
  ogs.log                                 log applicatif structuré
  audit.log                               journal d'audit append-only

/run/ogs/                                 0755 root:root
  ogs.lock                                verrou global d'exécution
  <id>/                                   0750 <id>:<id> — répertoire d'exécution du projet
    <job>.lock                            verrous de crons
    app.sock                              socket de l'application non-PHP (§7.11)

/home/<host effectif>/                    racine projet, 0750 <id>:<id> (docroot simple)
  public/                                 docroot, 0750
  var/                                    0700, hors de portée de www-data
  logs/                                   0750 root:<id> — apache, nginx, php, crons
  .bin/php                                lien vers l'interpréteur de la version courante
```

Fichiers générés en dehors de l'arborescence `ogs` (toujours marqués par un en-tête de gestion) :

```
/etc/apache2/sites-available/<name>.conf   + lien dans sites-enabled
/etc/nginx/sites-available/<name>.conf     + lien dans sites-enabled
/etc/php/<ver>/fpm/pool.d/<pool>.conf
/etc/systemd/system/ogs-<id>-<svc>@.service
/etc/systemd/system/ogs-<id>.target
/etc/cron.d/ogs-<id>
```

En-tête obligatoire de tout fichier généré :

```
# Généré par ogs — NE PAS MODIFIER À LA MAIN
# projet: shopfr  ressource: vhost/shopfr  empreinte: sha256:1f3a…  généré: 2026-08-22T14:30:11Z
# Pour personnaliser : /etc/ogs/templates/apache/php-fpm.conf.tmpl
```

L'empreinte permet de détecter une modification manuelle : `ogs diff` signale le fichier comme « modifié
hors ogs » et `apply` refuse de l'écraser sans `--force`.

---

## 5. Architecture logicielle

### 5.1 Découpage en paquets

```
cmd/ogs/                      point d'entrée
internal/
  cli/                        commandes Cobra, parsing, sorties
    root.go project.go site.go db.go amqp.go php.go web.go cert.go
    cron.go service.go user.go import.go doctor.go secrets.go completion.go
  tui/                        Bubble Tea : modèles, vues, wizard
  config/                     config globale, chargement, valeurs par défaut
  model/                      structures du domaine, validation, normalisation
  spec/                       lecture/écriture YAML, migration de schéma
  engine/                     moteur de réconciliation
    plan.go apply.go diff.go step.go saga.go lock.go
  provider/                   une implémentation par domaine technique
    system/                   users, groupes, répertoires, permissions
    mysql/  postgres/         SGBD
    amqp/                     RabbitMQ (API HTTP + fallback CLI)
    php/                      détection versions, pools
    web/                      interface WebServer
      apache/  nginx/
    acme/                     lego, challenges, renouvellement
    cron/                     génération /etc/cron.d
    unit/                     systemd (unités, target, dbus/systemctl)
  store/                      SQLite : schéma, migrations, requêtes
  secret/                     keystore chiffré
  exec/                       abstraction Executor (local, dry-run, futur SSH)
  fs/                         écriture atomique, sauvegarde, empreintes
  template/                   embed.FS + surcharges, fonctions de template
  audit/                      journal append-only
  hook/                       exécution des hooks utilisateur
  importer/                   scan et adoption de l'existant
  validate/                   règles transverses (unicité, longueurs, conflits)
  version/                    version, build info
```

### 5.2 Interfaces structurantes

```go
// exec : tout accès au système passe par là (local, dry-run, SSH en v2).
type Executor interface {
    Run(ctx context.Context, cmd Command) (Result, error)
    ReadFile(ctx context.Context, path string) ([]byte, error)
    WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode, owner Owner) error
    Remove(ctx context.Context, path string) error
    Stat(ctx context.Context, path string) (FileInfo, error)
}

// provider : contrat commun à tous les domaines techniques.
type Provider interface {
    Name() string
    Detect(ctx context.Context) (Availability, error)      // installé ? version ? actif ?
    Observe(ctx context.Context, p *model.Project) (Observed, error)
    Plan(ctx context.Context, p *model.Project, o Observed) ([]engine.Step, error)
}

// engine.Step : unité atomique, compensable, avec criticité.
type Step struct {
    ID          string
    Resource    string            // "mysql/db/shopfr"
    Action      Action            // Create | Update | Delete | NoOp
    Critical    bool              // false → échec non bloquant (D16)
    Describe    func() string     // ligne affichée dans le plan
    Diff        func() (string, error)
    Apply       func(context.Context) error
    Compensate  func(context.Context) error
    Reload      []ReloadIntent    // reloads à grouper en fin d'apply
}

// web : abstraction Apache/Nginx.
type WebServer interface {
    Engine() string
    Render(vh *model.VirtualHost, p *model.Project) ([]byte, error)
    Validate(ctx context.Context) error            // apachectl -t / nginx -t
    Enable(ctx context.Context, name string) error
    Disable(ctx context.Context, name string) error
    Reload(ctx context.Context) error
    Observe(ctx context.Context) ([]ObservedVHost, error)
}

// database : abstraction MySQL/MariaDB/PostgreSQL.
type Database interface {
    Engine() string
    EnsureDatabase(ctx context.Context, spec model.Database) error
    EnsureUser(ctx context.Context, spec model.DBUser, password string) error
    Grant(ctx context.Context, spec model.Database, user model.DBUser) error
    Drop(ctx context.Context, spec model.Database) error
    Dump(ctx context.Context, spec model.Database, w io.Writer) error
    Restore(ctx context.Context, spec model.Database, r io.Reader) error
    Observe(ctx context.Context) (ObservedDBs, error)
}
```

### 5.3 Dépendances externes

| Usage | Bibliothèque | Justification |
|---|---|---|
| CLI | `spf13/cobra` + `spf13/viper` | standard de fait, complétion incluse |
| TUI | `charmbracelet/bubbletea`, `bubbles`, `lipgloss` | écosystème mature |
| YAML | `goccy/go-yaml` | préservation de l'ordre, erreurs localisées |
| SQLite | `modernc.org/sqlite` | pur Go, compatible `CGO_ENABLED=0` |
| ACME | `go-acme/lego/v4` | client complet, nombreux providers DNS |
| MySQL | `go-sql-driver/mysql` | socket Unix natif |
| PostgreSQL | `jackc/pgx/v5` | socket Unix natif |
| systemd | `coreos/go-systemd/v22` (dbus) | plus fiable que le parsing de `systemctl` |
| Chiffrement | `golang.org/x/crypto` (AES-GCM, argon2) | standard |
| Logs | `log/slog` (stdlib) | structuré, sans dépendance |
| Tests | `stretchr/testify`, `testcontainers-go` | assertions et intégration |

Contrainte : aucune dépendance C, `CGO_ENABLED=0`, binaire statique.

---

## 6. Moteur de réconciliation

### 6.1 Cycle

```
   Charger le YAML (état désiré)
            │
            ▼
   Valider (schéma, unicité, longueurs, cohérence inter-ressources)
            │
            ▼
   Observer (chaque provider inspecte le système réel)
            │
            ▼
   Planifier (diff désiré/observé → liste ordonnée de Steps)
            │
            ▼
   ┌────────┴────────┐
   │  --dry-run ?    │──oui──▶ afficher le plan + les diffs, sortir (code 0)
   └────────┬────────┘
            │ non
            ▼
   Confirmer (sauf --yes / --non-interactive)
            │
            ▼
   Exécuter en saga (ordre des dépendances, compensations empilées)
            │
            ▼
   Reloads groupés (daemon-reload, php-fpm, apache/nginx, une fois chacun)
            │
            ▼
   Mettre à jour l'état observé, écrire l'audit, exécuter les hooks post
```

### 6.2 Ordre des dépendances

Ordre de création (l'ordre de suppression est l'inverse exact) :

```
1. utilisateur système et groupe
2. arborescence et permissions
3. bases de données et utilisateurs
4. vhost AMQP, utilisateur, exchanges, queues, bindings, policies
5. pools PHP-FPM
6. vhosts web (HTTP seul dans un premier temps)
7. reload web  ← prérequis du challenge HTTP-01
8. certificats TLS
9. réécriture des vhosts avec TLS, reload web
10. tâches cron
11. runtime applicatif (nvm, rbenv, pyenv, venv) pour les types non-PHP
12. unités systemd, daemon-reload, enable, start
13. attente de disponibilité du socket ou du port applicatif (§7.11.6)
14. vhost de proxy, reload web
15. hooks post-create
```

L'étape 7 est structurante : le challenge HTTP-01 exige que le vhost HTTP réponde déjà. Le vhost est donc
écrit en deux passes.

**Exception, environnement `prep`** : le certificat wildcard partagé existant déjà, les étapes 7 à 9 se
réduisent à une seule passe — le vhost est écrit directement en HTTP + HTTPS, suivi d'un unique reload.
Une création de préproduction ne comporte donc aucune étape non critique, aucun appel réseau ACME et
aucune attente de propagation DNS.

### 6.3 Gestion des échecs (D16)

Chaque `Step` porte un booléen `Critical` :

- **Critique** (défaut) : utilisateur, arborescence, bases, pool, vhost. Un échec déclenche le rollback de
  toutes les étapes déjà appliquées, dans l'ordre inverse, via leur `Compensate`.
- **Non critique** : certificats, démarrage des services, hooks post. Un échec est journalisé comme
  avertissement, l'apply continue, le projet reste fonctionnel dans un état dégradé documenté, et une
  commande de reprise est proposée.

```
✓ user linux       shopfr
✓ mysql db+user    shopfr / shopfr
✓ amqp vhost       shopfr  (+2 queues, 2 bindings)
✓ php pool 8.4     shopfr
✓ vhost apache     shopfr.conf (HTTP)
⚠ cert le          shop.fr : challenge HTTP-01 échoué (DNS ne pointe pas ici)
  → site actif en HTTP, TLS en attente
  → reprise : ogs cert issue shopfr
✓ cron             scheduler
✓ service          ogs-shopfr-messenger-async@{1,2,3} (running)

Terminé avec 1 avertissement (code 6). Audit : 4f2a9c.
```

Si le rollback lui-même échoue, l'outil s'arrête, affiche l'état exact atteint et les commandes de
nettoyage manuel, et renvoie le code 1. Aucune tentative silencieuse.

### 6.4 Idempotence et détection de dérive

- Chaque fichier généré est comparé à son rendu attendu : identique → `NoOp`, différent → `Update` avec
  diff unifié affiché.
- L'empreinte SHA-256 du dernier contenu écrit est stockée en base. Si le fichier sur disque diverge à la
  fois du rendu attendu et de l'empreinte enregistrée, il a été **modifié à la main** : `apply` refuse et
  exige `--force` (qui sauvegarde l'original en `.ogs-bak-<horodatage>`).
- Les ressources vivantes (bases, queues, users) sont comparées par leurs attributs observés, pas par une
  simple présence.

### 6.5 Concurrence et atomicité

- Verrou exclusif `flock` sur `/run/ogs/ogs.lock` pendant tout `apply`, `import` et toute commande
  mutante. Un second processus échoue immédiatement en code 7 avec le PID détenteur.
- Écriture de fichier : temporaire dans le même répertoire → `fsync` → `chmod`/`chown` → `rename` atomique.
- Validation avant activation : `apachectl -t` / `nginx -t` / `php-fpm -t` sont exécutés **après** écriture
  et **avant** reload. En cas d'échec, le fichier précédent est restauré et l'étape échoue.
- Reload groupé : les intentions de reload sont dédupliquées et exécutées une seule fois en fin d'apply
  (un `systemctl daemon-reload`, un reload par version de PHP-FPM concernée, un reload du serveur web).

---

## 7. Spécification des modules

### 7.1 Système : utilisateurs, groupes, arborescence, permissions

#### 7.1.1 Création

```
groupadd shopfr
useradd --gid shopfr --home-dir /home/shop.fr --shell /usr/sbin/nologin \
        --no-create-home --comment "ogs=shopfr" shopfr
gpasswd -a www-data shopfr          # si webAccess = group (défaut)
install -d -o root -g shopfr -m 0750 /home/shop.fr/logs /home/shop.fr/.bin
ln -sfn /usr/bin/php8.5 /home/shop.fr/.bin/php
install -m 0440 -o root -g root <généré> /etc/sudoers.d/ogs-shopfr
```

Le groupe est **privé au projet** et porte le même nom que l'utilisateur (D23). L'arborescence est créée
explicitement, avec propriétaire et mode fixés à chaque niveau — jamais en dépendance du umask ambiant.
Le champ `--comment` marque le compte comme géré par `ogs`, ce qui permet de le distinguer d'un compte
adopté ou étranger.

#### 7.1.2 Permissions cibles

```
/home                        0755  root:root      traversable par tous (standard)
/home/shop.fr                0750  shopfr:shopfr  rwx utilisateur, r-x groupe, --- autres
├── www/                     0750  shopfr:shopfr  code applicatif
│   └── public/              0750  shopfr:shopfr  docroot (Symfony) ; pour Prestashop, www/ l'est
├── .ssh/                    0700  shopfr:shopfr  clé de déploiement (§7.1.7)
│   ├── id_ed25519           0600  shopfr:shopfr
│   └── id_ed25519.pub       0644  shopfr:shopfr
├── var/                     0700  shopfr:shopfr  cache, sessions, logs applicatifs
│   ├── tmp/                 0700  shopfr:shopfr  upload_tmp_dir
│   └── sessions/            0700  shopfr:shopfr  session.save_path
├── logs/                    0750  root:shopfr    ← répertoire NON inscriptible par l'utilisateur
│   ├── apache-access.log    0640  root:shopfr    écrit par le master Apache (root)
│   ├── apache-error.log     0640  root:shopfr
│   ├── php-error.log        0640  root:shopfr    écrit par le master FPM (root)
│   ├── php-slow.log         0640  root:shopfr
│   └── cron-scheduler.log   0640  shopfr:shopfr  pré-créé par ogs, écrit par la tâche
├── .bin/                    0750  root:shopfr    ← répertoire NON inscriptible par l'utilisateur
│   ├── php → /usr/bin/php8.5                    lien géré par ogs (§7.5)
│   └── node, ruby, python, app                  liens des autres runtimes (§7.11)
└── .env                     0600  shopfr:shopfr  lu par FPM (sous shopfr), pas par www-data
```

Le pool PHP-FPM tourne sous `shopfr:shopfr` : il accède donc à l'intégralité du home, `.env` et `var/`
compris. `www-data`, membre secondaire du groupe `shopfr`, obtient exactement la traversée du home et la
lecture de `public/` — et rien de plus, puisque `var/` est en 0700 et `.env` en 0600.

**Pourquoi `logs/` et `.bin/` appartiennent à root.** Ces deux répertoires sont lisibles et traversables par
l'utilisateur (groupe `shopfr`, `r-x`) mais **il ne peut pas y créer, renommer ni supprimer d'entrée**. Ce
détail est une exigence de sécurité, pas une préférence :

- `logs/` : Apache, Nginx et le master PHP-FPM ouvrent leurs fichiers de log **en tant que root**, avant
  d'abandonner leurs privilèges. Si l'utilisateur pouvait remplacer `apache-error.log` par un lien
  symbolique vers `/etc/shadow` ou `/etc/sudoers.d/`, il obtiendrait une écriture root arbitraire, donc une
  escalade de privilèges complète. C'est une classe de vulnérabilité connue et régulièrement exploitée sur
  les panels d'hébergement. Un répertoire root avec des fichiers pré-créés la supprime : l'utilisateur peut
  écrire *dans* les fichiers qui lui appartiennent (logs de crons), jamais en substituer un.
- `.bin/` : le lien `php` étant référencé par les tâches cron et les unités systemd, un utilisateur qui
  pourrait le remplacer n'obtiendrait certes qu'une exécution sous sa propre identité — sans escalade — mais
  cela contournerait silencieusement le pilotage de version par `ogs`. Le répertoire root garantit que la
  version effective est bien celle que l'inventaire déclare.

Corollaire pratique : `logrotate` n'a pas besoin de la directive `su`, précisément parce que le répertoire
n'est pas inscriptible par un non-root (§14.5).

#### 7.1.3 Accès du serveur web : arbitrage

Le mode retenu (D25) est `webAccess: group`.

**Pourquoi pas `www-data` comme groupe primaire des utilisateurs de site.** Cette option, plus simple en
apparence, annule le bénéfice principal de l'isolation par utilisateur. Avec `alpha:www-data` et
`beta:www-data`, tout fichier du site `beta` portant le bit de lecture groupe est lisible par le site
`alpha`, y compris son `.env`. Or le pool FPM du site `alpha` tourne sous `alpha:www-data` : une inclusion
de fichier, un plugin compromis ou un webshell sur `alpha` suffit à récupérer les identifiants de base de
`beta` et à exfiltrer ses données. Chaque site ajouté élargit la surface de tous les autres, et les
fichiers créés par les applications héritent automatiquement du groupe partagé, rendant la fuite
silencieuse. Cette configuration n'est acceptable que si tous les sites appartiennent au même propriétaire
de confiance ; ce n'est pas l'hypothèse de conception retenue.

**Pourquoi le groupe privé + membre secondaire fonctionne.** L'asymétrie est exactement celle recherchée :
`www-data` appartient à tous les groupes de sites, mais aucun utilisateur de site n'appartient au groupe
d'un autre. Le site `alpha` n'a strictement aucun droit sur `/home/beta.fr` (bits « autres » à zéro).
Et `www-data` n'exécute jamais de code applicatif : le PHP est traité par FPM sous l'utilisateur du site,
le serveur web ne servant que le statique.

**Prise en compte de la nouvelle appartenance — vérifié.** Les workers Apache et Nginx appellent
`initgroups()` à chaque fork : un `reload`/`graceful` suffit, sans redémarrage complet. Le processus maître
conserve ses propres credentials, sans conséquence puisqu'il est root.

Cette hypothèse, portée en suspens depuis le lot L1, est désormais **mesurée** par le scénario
d'intégration `test/integration/scenarios/reload-group.sh` (lot L10, `make integration`) : sur une Debian 13
avec Apache 2.4, un home en 0750 servi à `www-data` hors du groupe renvoie **403** ; après `gpasswd -a` et
un simple `apache2ctl graceful`, il renvoie **200**. Le scénario compare avec un `restart` et distingue
explicitement les trois issues, de sorte qu'une régression sur une version future ne passerait pas
inaperçue : il rendrait le code 2 et son message indiquerait qu'un restart — donc une coupure — devient
nécessaire.

La conception est donc confirmée : **la création d'un site n'impose aucune coupure du serveur web.**

**Modes alternatifs prévus par le champ `webAccess`** :

| Mode | Mécanisme | Quand l'utiliser |
|---|---|---|
| `group` (défaut) | `www-data` membre du groupe du projet | cas général ; standard, lisible, aucune dépendance |
| `acl` | aucune appartenance ; `setfacl -m u:www-data:--x` sur le home, `r-X` récursif **et par défaut** sur `public/` | isolation plus fine : `www-data` n'atteint jamais le reste du home ; exige les ACL sur le montage |
| `none` | aucun accès accordé | site entièrement servi par un proxy vers un applicatif (template `proxy`), sans statique |

En mode `group`, une ACL par défaut peut être ajoutée sur `public/` (`setfacl -d -m g:shopfr:rX`) pour
garantir le bit de lecture groupe sur les fichiers créés par l'application, indépendamment de son umask —
utile pour les répertoires d'upload. C'est le comportement appliqué si `layout.enforceUploadAcl` est activé.

`ogs doctor` vérifie : traversabilité de `/home`, présence de `www-data` dans les groupes attendus, absence
d'options de montage bloquantes sur `/home` (`noexec` empêcherait l'exécution des binaires du projet —
composer, node — sans gêner l'interprétation PHP ; `nosuid` et `nodev` sont au contraire souhaitables),
support des ACL si `webAccess: acl` est utilisé, et cohérence des modes de l'arborescence.

#### 7.1.4 Environnement de préproduction

`ogs site create shop.fr --env prep` produit un projet distinct et complet :

```
host effectif   shopfr.preprod-pp.net
home            /home/shopfr.preprod-pp.net
identifiant     shopfrprep
user + groupe   shopfrprep
base            shopfrprep      user de base   shopfrprep
vhost AMQP      shopfrprep      user AMQP      shopfrprep
pool PHP        shopfrprep      socket         /run/php/php8.5-fpm-18502-shopfrprep.sock
vhost web       shopfrprep.conf certificat     *.preprod-pp.net (partagé)
unités          ogs-shopfrprep-*.service        cron  /etc/cron.d/ogs-shopfrprep
logs            /home/shopfr.preprod-pp.net/logs/     interpréteur  .bin/php
sudoers         /etc/sudoers.d/ogs-shopfrprep
```

Production et préproduction ne partagent **aucune** ressource : ni utilisateur, ni groupe, ni base, ni
queue, ni socket, ni unité. Seul le certificat est mutualisé, et il est de portée serveur, pas de portée
projet.

Différences appliquées automatiquement en `prep` :

- **Certificat déjà disponible** : le vhost référence le wildcard partagé `*.preprod-pp.net`. Aucune
  émission ACME n'est déclenchée à la création, donc aucune attente, aucune dépendance à la propagation DNS
  et aucun risque de quota. C'est la simplification la plus notable : la saga de création d'une
  préproduction n'a plus d'étape non critique susceptible d'échouer (§6.2).
- **Interdiction d'indexation** : en-tête `X-Robots-Tag: noindex, nofollow` sur toutes les réponses, et
  `robots.txt` servi avec `Disallow: /` en priorité sur celui du projet.
- **Authentification HTTP basique** activable par `--basic-auth` : `ogs` génère le couple identifiant/mot de
  passe, le stocke dans le keystore et écrit le fichier `htpasswd` (bcrypt) sous `/etc/ogs/htpasswd/`.
- `APP_ENV=dev` par défaut dans l'environnement du pool, surchargeable.
- Preset de pool réduit d'un cran (`medium` → `small`), sauf preset explicite.

**Prérequis DNS** — hors périmètre de l'outil, à faire une fois : un enregistrement wildcard
`*.preprod-pp.net A <ip du serveur>` (et `AAAA` le cas échéant). Toute préproduction résout alors sans
intervention. `ogs doctor` vérifie que le wildcard résout bien vers une adresse locale et le signale sinon ;
`ogs site create --env prep` émet un avertissement non bloquant si le host construit ne résout pas.

Limite assumée de ce choix : un enregistrement wildcard ne désigne qu'une seule adresse, donc un seul
serveur de préproduction par domaine. Un parc multi-serveurs impose soit un domaine (ou sous-domaine)
de préproduction par serveur — `*.srv1.preprod-pp.net`, en changeant `environments.prep.domain` sur chaque
machine — soit l'activation de la création automatique d'enregistrements DNS, prévue comme évolution et non
livrée en v1.

`ogs site clone shop.fr --to prep` (lot L6) crée la préproduction puis copie le docroot et restaure un dump
de la base de production dans celle de préproduction.

#### 7.1.5 Clé de déploiement

Une paire ed25519 sans passphrase est générée à la création du projet :

```
ssh-keygen -t ed25519 -N "" -C "ogs:shopfr@srv1" -f /home/shop.fr/.ssh/id_ed25519
```

```
/home/shop.fr/.ssh/       0700  shopfr:shopfr
├── id_ed25519            0600  shopfr:shopfr   ne quitte jamais le serveur
├── id_ed25519.pub        0644  shopfr:shopfr
├── known_hosts           0644  shopfr:shopfr   pré-rempli si le réseau est disponible
└── config                0644  shopfr:shopfr   IdentityFile, IdentitiesOnly=yes
```

**Sens du déploiement** : le serveur **tire** le code (`git clone`, `git pull`). La clé publique est destinée
à être déclarée en *deploy key* en lecture seule chez l'hébergeur du dépôt ; la clé privée reste sur la
machine, sous la seule propriété de l'utilisateur du projet. Aucun accès SSH entrant n'est ouvert et le shell
`nologin` est conservé (D15, D33).

```
$ ogs user key show shopfr
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI… ogs:shopfr@srv1
→ à déclarer en deploy key (lecture seule) sur le dépôt du projet
```

- `ogs user key show <id>` n'affiche que la clé **publique** ; la privée n'est jamais restituée par une
  commande, n'entre pas dans le keystore (elle vit sur le disque, protégée par ses permissions) et n'apparaît
  dans aucun log.
- `ogs user key rotate <id>` régénère la paire ; l'ancienne clé publique est affichée pour permettre son
  retrait chez l'hébergeur, et l'opération est journalisée.
- `known_hosts` est pré-rempli par `ssh-keyscan` pour les hôtes déclarés dans
  `deployment.knownHosts` (par défaut `github.com`, `gitlab.com`), avec affichage des empreintes obtenues
  pour vérification. En l'absence de réseau, le fichier reste vide et un avertissement est émis :
  `StrictHostKeyChecking` n'est **jamais** désactivé, ni dans le `config` généré ni ailleurs.
- La clé est incluse dans l'archive de suppression (elle fait partie du home) et restaurée par
  `project restore`, ce qui évite de redéclarer une deploy key après une restauration.

#### 7.1.6 Suppression

`userdel` sans `--remove` (l'arborescence est traitée par la logique d'archivage), suppression du groupe,
retrait de `www-data` du groupe. Refus de supprimer un utilisateur non créé par `ogs` (absent de la base
d'état et sans marqueur `ogs=` dans le champ commentaire) sans `--force`. Le marqueur ne peut contenir ni `:`,
séparateur de champs de `/etc/passwd`, ni `,`, séparateur de sous-champs du GECOS.

#### 7.1.7 Garde-fous

- Refus des noms réservés (`root`, `daemon`, `www-data`, `mysql`, `postgres`, `rabbitmq`, `nobody`…) et de
  tout identifiant déjà présent dans `/etc/passwd` ou `/etc/group` mais inconnu de la base d'état.
- Refus d'un home hors de `/home` (ou de la racine configurée), refus d'un home existant non vide lors
  d'une création.
- Vérification qu'aucun processus ne tourne sous l'utilisateur avant suppression.
- Refus de générer un fichier sudoers pour un utilisateur qui n'est pas celui d'un projet géré, et refus
  d'écrire sous `/etc/sudoers.d/` un nom ne commençant pas par `ogs-`.
- UID laissé au système, plage configurable ; jamais de réutilisation d'un UID libéré tant que des fichiers
  lui appartiennent encore ailleurs sur le disque (contrôle sur `/home` uniquement, avec avertissement).

### 7.2 MySQL / MariaDB

**Connexion administrateur** : socket Unix (`/run/mysqld/mysqld.sock`, détecté), authentification
`auth_socket`/`unix_socket` en tant que root. Repli sur `/etc/mysql/debian.cnf` s'il existe. Un DSN
explicite reste configurable pour un serveur distant.

**Opérations** :

```sql
CREATE DATABASE IF NOT EXISTS `shopfr` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS 'shopfr'@'localhost' IDENTIFIED BY '…';
ALTER USER 'shopfr'@'localhost' IDENTIFIED BY '…';        -- rotation
GRANT ALL PRIVILEGES ON `shopfr`.* TO 'shopfr'@'localhost';
-- ressource additionnelle : underscore échappé, sinon le motif est un joker (§3.3.5)
GRANT SELECT, SHOW VIEW ON `shopfr\_analytics`.* TO 'shopfr_ro'@'localhost';
FLUSH PRIVILEGES;
```

Jeux de privilèges nommés :

| Nom | Privilèges |
|---|---|
| `all` | `ALL PRIVILEGES` sur la base |
| `crud` | `SELECT, INSERT, UPDATE, DELETE` |
| `readonly` | `SELECT, SHOW VIEW` |
| liste | privilèges bruts fournis, validés contre une liste blanche |

**Plugin d'authentification** : MySQL 8 utilise `caching_sha2_password` par défaut. Le driver `mysqlnd` le
prend en charge à partir de PHP 7.4, donc toutes les versions gérées ici (§7.10.3) sont compatibles. Le champ
`authPlugin` reste exposé (`auto` par défaut) pour les cas où un client tiers plus ancien accède à la même
base : il bascule alors explicitement sur `mysql_native_password`, choix consigné dans l'état du projet.

**Observation** : `information_schema.SCHEMATA`, `mysql.user`, `SHOW GRANTS`. Les privilèges observés sont
normalisés puis comparés à l'état désiré ; un écart produit un `Update` (révocation ciblée + grant).

**Contraintes** : identifiants échappés par backquotes doublées ; longueur d'utilisateur 32 caractères
(MySQL 8) contre 80 (MariaDB 10.6+), la limite la plus stricte est appliquée pour la portabilité ; détection
MariaDB par `SELECT VERSION()`.

**Dump/restore** : `mysqldump --single-transaction --routines --triggers --default-character-set=utf8mb4`,
compression zstd, sortie vers fichier ou stdout. Restauration avec confirmation si la base n'est pas vide.

### 7.3 PostgreSQL

**Connexion** : socket Unix (`/var/run/postgresql/.s.PGSQL.<port>`), authentification `peer` en tant
qu'utilisateur `postgres`. Multi-cluster : les clusters sont découverts via `pg_lsclusters` ou l'inspection
de `/etc/postgresql/*/*/postgresql.conf`, et adressés par `<version>/<nom>`.

**Opérations** :

```sql
CREATE ROLE shopfr_pg LOGIN PASSWORD '…';
ALTER ROLE shopfr_pg WITH PASSWORD '…';
CREATE DATABASE shopfr_analytics OWNER shopfr_pg ENCODING 'UTF8' LC_COLLATE 'fr_FR.UTF-8' TEMPLATE template0;
\c shopfr_analytics
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
REVOKE ALL ON SCHEMA public FROM PUBLIC;          -- durcissement PG 15+
GRANT ALL ON SCHEMA public TO shopfr_pg;
```

**Spécificités** : `CREATE DATABASE` n'est pas transactionnel (exécution hors transaction) ; les extensions
requièrent une connexion à la base cible ; le comportement de `public` a changé en PostgreSQL 15, le
durcissement est appliqué systématiquement. Suppression : `DROP DATABASE … WITH (FORCE)` sur PG 13+, sinon
terminaison préalable des connexions via `pg_terminate_backend`.

**Dump/restore** : `pg_dump -Fc` (format custom) et `pg_restore`.

### 7.4 RabbitMQ

**Pilotage** : API HTTP management (`http://127.0.0.1:15672/api/…`), authentification par un compte
administrateur dont les identifiants sont dans le keystore. Détection du plugin ; s'il est absent,
`ogs doctor` le signale et l'outil se replie sur `rabbitmqctl` en mode dégradé (vhosts, users et
permissions seulement, pas de déclaration fine des queues).

**Ressources gérées** (toutes idempotentes, l'API management étant en PUT) :

| Ressource | Endpoint |
|---|---|
| vhost | `PUT /api/vhosts/{vhost}` |
| utilisateur | `PUT /api/users/{user}` (`password_hash`, tags) |
| permissions | `PUT /api/permissions/{vhost}/{user}` |
| permissions topic | `PUT /api/topic-permissions/{vhost}/{user}` |
| exchange | `PUT /api/exchanges/{vhost}/{name}` |
| queue | `PUT /api/queues/{vhost}/{name}` |
| binding | `POST /api/bindings/{vhost}/e/{exchange}/q/{queue}` |
| policy | `PUT /api/policies/{vhost}/{name}` |

**Point d'attention majeur** : les arguments d'une queue (`x-message-ttl`, `x-dead-letter-exchange`, type
quorum/classic) sont **immuables** après création. Une modification détectée ne peut pas être appliquée
telle quelle : le plan affiche l'écart et propose explicitement une recréation (`--recreate-queues`), qui
n'est jamais automatique car elle détruit les messages en attente. Le nombre de messages présents est
affiché avant confirmation.

**Préréglage Symfony Messenger** : une option `--messenger <transport>[,<transport>…]` génère pour chaque
transport l'exchange (type `direct`), la queue durable, le binding par routing key, plus une queue
`<transport>_failed` et un DLX si demandé.

**Observation** : `GET /api/queues/{vhost}` fournit également la profondeur des queues et le nombre de
consumers, exploités par la TUI et `ogs status`.

**Logs** : RabbitMQ journalise au niveau du broker (`/var/log/rabbitmq/rabbit@<hôte>.log`) et n'offre
aucune granularité par vhost. Il est donc impossible de dédier un fichier de log à un projet, et un lien
symbolique vers le log global serait une fuite d'information entre projets : il exposerait l'activité de
tous les vhosts à l'utilisateur d'un seul. `ogs logs <id> --amqp` lit donc le log du broker et le filtre
sur le vhost et l'utilisateur du projet ; rien n'est écrit dans `logs/`. La restriction est documentée
comme telle plutôt que contournée.

### 7.5 PHP-FPM

**Détection** : parcours de `/etc/php/*/fpm/`, corrélation avec les unités `php<ver>-fpm.service` et les
binaires `/usr/bin/php<ver>`. Une version est utilisable si le service existe ; son état actif est reporté.
Les versions gérées vont de 7.4 à 8.5 (§7.10.3) et peuvent coexister : le parc réel impose de faire tourner
un Prestashop en 7.4 à côté d'un Symfony 8 en 8.5 sur la même machine.

**Presets de pool** :

| Preset | pm | max_children | start | min/max spare | memory_limit | usage visé |
|---|---|---|---|---|---|---|
| `micro` | ondemand | 5 | — | — | 128M | site vitrine, faible trafic |
| `small` | dynamic | 10 | 2 | 1 / 3 | 192M | petit site dynamique |
| `medium` | dynamic | 20 | 4 | 2 / 6 | 256M | application standard |
| `large` | dynamic | 50 | 10 | 5 / 15 | 512M | trafic élevé |
| `custom` | — | — | — | — | — | tout fourni par `settings` |

Tout paramètre du preset est surchargeable individuellement par `settings`.

**Numéro de pool** — norme `1{version sans point}{incrément sur 2 chiffres}` :

```
php 8.5, premier pool créé   → 18501
php 8.4, troisième pool      → 18403
php 8.1, dixième pool        → 18110
```

L'incrément est attribué **par pool** (et non par projet : un projet ayant un pool `web` et un pool
`worker` consomme deux numéros), à la première place libre de la plage de sa version. Le numéro est
enregistré en base, reste stable pendant toute la vie du pool, et n'est réattribué qu'après suppression.

Il sert d'identifiant logique — il apparaît dans le nom du socket, l'inventaire, la TUI et les diagnostics —
et devient un **port TCP réel** uniquement pour un pool déclaré `listen: tcp` :

```yaml
php:
  pools:
    - name: web
      version: "8.5"
      listen: unix        # unix (défaut) | tcp
```

| Mode | Écoute | Contrôle d'accès |
|---|---|---|
| `unix` (défaut) | `/run/php/php8.5-fpm-18501-shopfr.sock`, `0660 www-data:shopfr` | permissions du fichier : seuls `www-data` et l'utilisateur du projet peuvent s'y connecter |
| `tcp` | `127.0.0.1:18501` | **aucun** : tout utilisateur local peut interroger le pool et faire exécuter du code sous l'identité de celui-ci |

Le mode `tcp` n'est justifié que par une contrainte externe : serveur web sur une autre machine, pool dans un
conteneur, ou chroot empêchant l'accès au socket. Il est refusé par défaut et exige `--allow-tcp` à la
création, la conséquence ci-dessus étant inscrite dans l'avertissement. Limite du schéma de numérotation à
signaler : 100 pools par version (`1<ver>00`–`1<ver>99`), et le format cesse d'être valide au-delà de
PHP 9.9 (`10.0` produirait un numéro supérieur à 65535) — la validation le détectera le jour venu.

**Pool généré** :

```ini
[shopfr]
user = shopfr
group = shopfr
listen = /run/php/php8.5-fpm-18501-shopfr.sock
listen.owner = www-data
listen.group = shopfr
listen.mode = 0660
pm = dynamic
pm.max_children = 20
pm.start_servers = 4
pm.min_spare_servers = 2
pm.max_spare_servers = 6
pm.max_requests = 500
php_admin_value[memory_limit] = 256M
php_admin_value[open_basedir] = /home/shop.fr:/tmp:/usr/share/php
php_admin_value[error_log] = /home/shop.fr/logs/php-error.log
php_admin_flag[log_errors] = on
php_admin_value[upload_tmp_dir] = /home/shop.fr/var/tmp
php_admin_value[session.save_path] = /home/shop.fr/var/sessions
env[APP_ENV] = prod
slowlog = /home/shop.fr/logs/php-slow.log
request_slowlog_timeout = 10s
request_terminate_timeout = 300s
catch_workers_output = yes
```

**Interpréteur CLI : le lien `/home/<host>/.bin/php`**

Les tâches cron et les unités systemd n'invoquent jamais `/usr/bin/php8.5` mais toujours
`/home/shop.fr/.bin/php`, lien symbolique géré par `ogs` (répertoire root, §7.1.2). Conséquence directe : une
bascule de version ne touche ni les fichiers cron ni les unités systemd, dont le contenu est indépendant de
la version. Le lien est également le point de vérité pour l'utilisateur, qui peut s'y référer dans ses
propres scripts (`.bin/php bin/console …`) sans jamais coder une version en dur.

`.bin/` est prévu pour accueillir d'autres liens gérés selon le même principe (`composer` notamment), sans
que cela relève du périmètre v1.

**Bascule de version** (`ogs php switch shopfr 8.5`) — séquence sans coupure :

1. Vérifier que la version cible est installée et que ses extensions couvrent celles de l'ancienne
   (comparaison de `php -m`), avertir sinon.
2. Allouer le nouveau numéro de pool dans la plage de la version cible.
3. Écrire le pool sur la nouvelle version, `php-fpm8.5 -t`.
4. `systemctl reload php8.5-fpm`, attendre l'apparition du socket (avec délai d'attente).
5. Réécrire les vhosts pointant sur l'ancien socket, valider, recharger le serveur web.
6. Mettre à jour le lien `.bin/php` (`ln -sfn`, atomique).
7. Redémarrer les services du projet via sa target, afin qu'ils chargent le nouvel interpréteur. Les crons
   basculent d'eux-mêmes à leur prochaine exécution ; aucun fichier cron n'est réécrit.
8. Supprimer l'ancien pool, recharger l'ancien FPM, libérer l'ancien numéro.

Toute erreur avant l'étape 5 annule proprement ; après l'étape 5, la compensation restaure l'ancien pool, le
vhost et le lien. `daemon-reload` n'est pas nécessaire, aucune unité n'ayant changé.

### 7.6 Serveurs web

Modèle unifié : un `VirtualHost` décrit domaines, docroot, pool, TLS et options ; chaque moteur en produit
sa configuration.

**Templates fournis** :

| Template | Description |
|---|---|
| `static` | fichiers statiques, pas de PHP |
| `php-fpm` | PHP via socket FPM (`mod_proxy_fcgi` ou `fastcgi_pass`) |
| `proxy` | reverse-proxy vers un port ou un socket local (Node, Go…) |
| `redirect` | redirection 301 vers un autre domaine |

**Apache (extrait du template `php-fpm`)** :

```apache
<VirtualHost *:80>
    ServerName shop.fr
    ServerAlias www.shop.fr
    DocumentRoot /home/shop.fr/www/public
    Alias /.well-known/acme-challenge /var/lib/ogs/acme-challenge
    <Directory /var/lib/ogs/acme-challenge>
        Require all granted
    </Directory>
    RewriteEngine On
    RewriteCond %{REQUEST_URI} !^/\.well-known/acme-challenge/
    RewriteRule ^(.*)$ https://shop.fr$1 [R=301,L]
    ErrorLog /home/shop.fr/logs/apache-error.log
    CustomLog /home/shop.fr/logs/apache-access.log combined
</VirtualHost>

<VirtualHost *:443>
    ServerName shop.fr
    ServerAlias www.shop.fr
    DocumentRoot /home/shop.fr/www/public
    <Directory /home/shop.fr/www/public>
        AllowOverride All
        Require all granted
        Options -Indexes +FollowSymLinks
    </Directory>
    <FilesMatch \.php$>
        SetHandler "proxy:unix:/run/php/php8.5-fpm-18501-shopfr.sock|fcgi://localhost"
    </FilesMatch>
    SSLEngine on
    SSLCertificateFile /var/lib/ogs/acme/certificates/shopfr/fullchain.pem
    SSLCertificateKeyFile /var/lib/ogs/acme/certificates/shopfr/privkey.pem
    Header always set Strict-Transport-Security "max-age=31536000; includeSubDomains"
    ErrorLog /home/shop.fr/logs/apache-error.log
    CustomLog /home/shop.fr/logs/apache-access.log combined
</VirtualHost>
```

**Nginx** : équivalent avec `fastcgi_pass unix:…`, `include snippets/fastcgi-php.conf`, `location ^~
/.well-known/acme-challenge/ { root /var/lib/ogs/acme-challenge; }`, `try_files $uri /index.php$is_args$args`.

**Modules requis** — vérifiés par `doctor` : Apache `proxy_fcgi`, `setenvif`, `rewrite`, `ssl`, `headers`,
`http2` ; Nginx compilé avec `http_ssl_module` et `http_v2_module`.

**Règles** :
- Le bloc `.well-known/acme-challenge` est présent dès la première passe HTTP, avant toute redirection.
- Un seul moteur web actif par serveur pour la v1 ; la coexistence Apache+Nginx sur des ports distincts
  n'est pas gérée (les deux peuvent être installés, un seul est piloté).
- Détection de conflit : refus d'écrire un vhost dont un domaine est déjà servi par une configuration
  existante, gérée ou non.

### 7.7 TLS / ACME

**Client** : `go-acme/lego/v4` embarqué. Un compte ACME par couple (CA, e-mail), stocké sous
`/var/lib/ogs/acme/accounts/`.

**Challenges** :
- `http-01` (défaut) : webroot partagé `/var/lib/ogs/acme-challenge`, servi par tous les vhosts. Aucune
  interruption du serveur web, pas de mode standalone.
- `dns-01` : provider **OVH par défaut**, credentials chiffrés dans
  `/etc/ogs/credentials/dns/ovh.yaml`. Seule voie possible pour un wildcard, donc chemin obligatoire pour le
  certificat de préproduction (§7.7 « Certificats partagés »). Les autres providers lego (Cloudflare, Gandi,
  Route53, Scaleway, DigitalOcean…) restent sélectionnables par `dnsProvider`, sans garantie de test.

```yaml
# /etc/ogs/credentials/dns/ovh.yaml  (0600 root:root, valeurs chiffrées au repos)
endpoint: ovh-eu
applicationKey: "…"
applicationSecret: "…"
consumerKey: "…"
# variante OAuth2 (API OVH récente) : clientId / clientSecret à la place des trois clés ci-dessus
```

`ogs cert dns-provider set ovh` guide la saisie, vérifie les credentials par un appel de lecture sur la zone
avant de les enregistrer, et signale si les droits d'écriture sur les enregistrements TXT manquent — cette
vérification évite de découvrir le problème au premier renouvellement.
- `selfsigned` : certificat auto-signé pour le développement, généré sans ACME.

**Renouvellement** : unité et timer installés par `ogs` —

```ini
# ogs-renew.timer
[Timer]
OnCalendar=*-*-* 03,15:00:00
RandomizedDelaySec=3600
Persistent=true
```

`ogs cert renew --all` renouvelle les certificats expirant sous 30 jours (seuil configurable), puis
déclenche les reloads groupés et les hooks `postCertRenew`.

**Garde-fous** :
- Vérification préalable de la résolution DNS du domaine vers une IP locale (avertissement, non bloquant
  car un reverse-proxy amont est possible).
- Respect des quotas Let's Encrypt : 5 échecs consécutifs par domaine déclenchent une pause de 1 heure
  enregistrée en base ; `--staging` bascule sur l'environnement de test.
- Clés privées en 0600 root, certificats en 0644.
- Révocation explicite (`ogs cert revoke`) avant suppression d'un projet si l'option est activée.
- Un certificat partagé n'est jamais révoqué ni supprimé par la suppression d'un projet (§7.7.4).

**Certificats partagés (portée serveur)**

Un certificat peut appartenir au serveur plutôt qu'à un projet. C'est le cas du wildcard de préproduction,
et le mécanisme est générique : tout certificat déclaré dans la configuration globale est référencé par les
vhosts au moyen de la syntaxe `@<nom>`.

```yaml
# /etc/ogs/config.yaml
certificates:
  wildcard-prep:
    domains: ["*.preprod-pp.net", "preprod-pp.net"]
    challenge: dns-01
    dnsProvider: ovh
    keyType: ec256
```

```
/var/lib/ogs/acme/certificates/
  _wildcard.preprod-pp.net/{cert.pem,chain.pem,fullchain.pem,privkey.pem}   ← partagé
  shopfr/{…}                                                               ← projet
```

Règles :

- Un certificat partagé est créé et renouvelé indépendamment de tout projet : `ogs cert issue @wildcard-prep`,
  et il est inclus dans `ogs cert renew --all`.
- Les vhosts qui le référencent sont recensés en base ; son renouvellement déclenche un reload du serveur
  web, une fois, quel que soit le nombre de vhosts concernés.
- La suppression d'un projet qui l'utilise ne le touche pas. Inversement, `ogs cert delete @wildcard-prep`
  est refusé tant qu'un vhost le référence, avec la liste des projets concernés.
- Un seul appel ACME couvre l'ensemble des préproductions : le quota Let's Encrypt (50 nouveaux certificats
  par semaine et par domaine enregistré) cesse d'être une contrainte, quel que soit le nombre de sites.
- Le wildcard impose le challenge DNS-01, donc des credentials en écriture sur la zone `preprod-pp.net`,
  chiffrés dans `/etc/ogs/credentials/dns/`. C'est la seule contrepartie du dispositif.
- Un wildcard ne couvre qu'**un** niveau de sous-domaine : `shopfr.preprod-pp.net` est couvert,
  `a.shopfr.preprod-pp.net` ne l'est pas. La validation refuse un host de préproduction à deux niveaux.
- `ogs doctor` alerte à 21 jours de l'expiration d'un certificat partagé, avec une sévérité supérieure à
  celle d'un certificat de projet : son expiration casserait toutes les préproductions à la fois.


### 7.8 Tâches planifiées

Un unique fichier par projet, entièrement régénéré à chaque apply :

```
# Généré par ogs — NE PAS MODIFIER À LA MAIN
# projet: shopfr  empreinte: sha256:…  généré: 2026-08-23T14:30:11Z
SHELL=/bin/sh
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
MAILTO=""

# scheduler
* * * * * shopfr /usr/bin/flock -n /run/ogs/shopfr/scheduler.lock \
  /home/shop.fr/.bin/php /home/shop.fr/bin/console schedule:run \
  >> /home/shop.fr/logs/cron-scheduler.log 2>&1
```

Règles :

- **Interpréteur** : toujours `/home/<host>/.bin/php`, jamais un binaire versionné (§7.5). Une bascule de
  version PHP ne réécrit donc aucun fichier cron.
- **Anti-recouvrement** : `flock -n` par défaut, désactivable par tâche. Les verrous vivent dans
  `/run/ogs/<id>/`, répertoire propre au projet (`0750 <id>:<id>`) créé par `/etc/tmpfiles.d/ogs.conf`
  afin de survivre au redémarrage. C'est aussi là que vivent les sockets des applications non-PHP
  (§7.11), ce qui donne un seul répertoire d'exécution par projet, inscriptible par lui seul.
- **Identité** : exécution sous l'utilisateur du projet.
- **Journalisation** : sortie redirigée vers `logs/cron-<nom>.log`, fichier **pré-créé par `ogs`** avec
  propriétaire `<id>:<id>` et mode 0640 dans un répertoire appartenant à root (§7.1.2) — la tâche peut
  écrire dans le fichier, jamais le remplacer.
- **Validation** : l'expression cron est vérifiée à la saisie, avec affichage des trois prochaines
  occurrences pour confirmation.
- **Désactivation** : `enabled: false` commente la ligne sans la supprimer, ce qui conserve l'historique et
  permet une réactivation immédiate.
- `ogs cron run <id> <nom>` exécute la tâche immédiatement, sous la bonne identité et avec le même verrou,
  pour tester sans attendre l'échéance.

### 7.9 Services systemd

**Unité templatisée générée** :

```ini
# /etc/systemd/system/ogs-shopfr-messenger-async@.service
[Unit]
Description=ogs shopfr — messenger async (instance %i)
PartOf=ogs-shopfr.target
After=network.target rabbitmq-server.service

[Service]
Type=simple
User=shopfr
Group=shopfr
WorkingDirectory=/home/shop.fr
ExecStart=/home/shop.fr/.bin/php bin/console messenger:consume async \
  --time-limit=3600 --memory-limit=128M --limit=500 -v
Restart=always
RestartSec=5
StartLimitBurst=10
StartLimitIntervalSec=600
KillSignal=SIGTERM
TimeoutStopSec=60
StandardOutput=journal
StandardError=journal
SyslogIdentifier=ogs-shopfr-messenger-async

# durcissement
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=read-only
ReadWritePaths=/home/shop.fr/var

[Install]
WantedBy=ogs-shopfr.target
```

```ini
# /etc/systemd/system/ogs-shopfr.target
[Unit]
Description=ogs shopfr — tous les services
[Install]
WantedBy=multi-user.target
```

**Templates fournis** : `symfony-messenger`, `worker` (commande arbitraire en boucle supervisée),
`oneshot-timer` (commande longue déclenchée par timer). Tous surchargeables.

**Opérations** : `service add/remove/scale/start/stop/restart/status/logs`. Le `scale` ajuste le nombre
d'instances actives (`systemctl enable --now ogs-shopfr-messenger-async@4`, `disable --now` pour réduire).
`logs` s'appuie sur `journalctl -u 'ogs-shopfr-messenger-async@*'` avec option `-f`.

**Redémarrage groupé** : `ogs service restart shopfr --all` redémarre via la target, ce qui permet un arrêt
propre (SIGTERM, Messenger termine le message en cours) puis un redémarrage ordonné.

**Cohérence de version PHP** : les unités et les crons invoquent `/home/<host>/.bin/php`, jamais un binaire
versionné. Une bascule de version ne réécrit donc **aucune** unité ni aucun fichier cron : elle met à jour
le lien, puis redémarre les services du projet pour qu'ils chargent le nouvel interpréteur. Les tâches cron
prennent la nouvelle version à leur exécution suivante, sans intervention. `daemon-reload` n'est plus requis
dans ce cas (§7.5).

#### 7.9.1 Délégation du pilotage à l'utilisateur du projet

L'utilisateur du projet doit pouvoir arrêter, démarrer et redémarrer ses propres services — typiquement
depuis un script de déploiement, une commande applicative ou un accès SSH restreint. `ogs` génère pour cela
un fichier sudoers dédié, en **énumérant les commandes exactes, sans aucun métacaractère**.

**Mode `ogs` (défaut)** — une surface d'appel minimale et non paramétrable :

```
# /etc/sudoers.d/ogs-shopfr   (0440 root:root, généré, validé par visudo -cf)
shopfr ALL=(root) NOPASSWD: /usr/bin/ogs my start
shopfr ALL=(root) NOPASSWD: /usr/bin/ogs my stop
shopfr ALL=(root) NOPASSWD: /usr/bin/ogs my restart
shopfr ALL=(root) NOPASSWD: /usr/bin/ogs my status
shopfr ALL=(root) NOPASSWD: /usr/bin/ogs my reload-php
```

La sous-commande `ogs my <verbe>` **ne prend aucun argument de projet** : `ogs` détermine le projet à
partir de l'identité de l'appelant, puis restreint l'action à ce seul projet. Il devient structurellement
impossible de cibler un autre projet : il n'y a pas de paramètre à détourner, donc pas de wildcard sudoers à
contourner. Cette surface est la seule exception à la règle root-strict (D21, amendée en D28) ; toute autre
commande refuse de s'exécuter hors root.

**Comment l'appelant est identifié.** `getuid()` ne convient pas : `sudo` appelle `setresuid(0, 0, 0)`, donc
l'UID réel vaut 0 dans le processus délégué. La source retenue est **`SUDO_UID`**, que `sudo` positionne
lui-même après avoir purgé l'environnement — comportement de la directive `env_reset`, active par défaut sur
Debian et Ubuntu. `ogs doctor` vérifie que `env_reset` est bien en vigueur : sans elle, l'appelant contrôle
son environnement et pourrait se présenter comme un autre projet. En l'absence de `SUDO_UID` (invocation
directe en root), `ogs my` refuse et renvoie vers `ogs service <verbe> <id>`, qui est explicite.

```
$ sudo ogs my restart              # sous l'utilisateur shopfr
  ↻ ogs-shopfr.target
    messenger-async@1  restarting → running
    messenger-async@2  restarting → running
    messenger-async@3  restarting → running
  3 instances actives.
```

**Mode `systemctl`** (option `sudo.style: systemctl`) — pour les scripts existants et les habitudes
d'exploitation, `ogs` énumère les unités réelles du projet, une ligne par unité et par verbe :

```
shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl start ogs-shopfr.target
shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl stop ogs-shopfr.target
shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl restart ogs-shopfr.target
shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl status ogs-shopfr.target
shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl restart ogs-shopfr-messenger-async@1.service
shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl restart ogs-shopfr-messenger-async@2.service
shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl restart ogs-shopfr-messenger-async@3.service
```

Le fichier est régénéré à chaque changement du nombre d'instances : c'est le coût de l'énumération, et le
moteur de réconciliation s'en charge comme de n'importe quel autre fichier généré.

**Ce que le générateur n'écrit jamais**, et qui est verrouillé par des tests :

- aucun `*` ni `?` dans une spécification de commande — `systemctl restart ogs-shopfr-*` autoriserait
  `ogs-shopfrprep-…` et donc le pilotage de la préproduction depuis la production ;
- aucun `systemctl` nu, sans unité : cela ouvrirait `systemctl link`, `--host=`, `daemon-reexec`, `isolate`,
  `mask` et l'arrêt de la machine ;
- aucune règle `(ALL)` ni `ALL=(ALL)` ;
- aucun chemin relatif : `secure_path` de sudo est supposé, mais les chemins sont écrits en absolu.

**Garde-fous d'écriture** : le fichier est produit dans un temporaire du même répertoire, validé par
`visudo -cf <temporaire>`, puis renommé atomiquement. Un fichier sudoers syntaxiquement invalide rendant
`sudo` inutilisable pour l'ensemble du système, l'étape de validation est **critique** au sens du §6.3 : son
échec annule l'apply et le fichier précédent reste en place. `ogs doctor` revalide l'ensemble de
`/etc/sudoers.d/ogs-*` et signale tout fichier étranger portant ce préfixe.

**Traçabilité** : chaque invocation de `ogs my` est écrite au journal d'audit avec l'UID appelant, le projet
déduit et le verbe, au même titre qu'une opération d'administration.

### 7.10 Types d'application et docroot

Le type d'application est une donnée de premier plan : il détermine le docroot, le front controller, le
template de vhost, le runtime et le service à superviser. Le mapping vit dans `/etc/ogs/apptypes.yaml`,
surchargeable, et se consulte par `ogs app types`.

**Types livrés en v1** :

| `kind` | Variante | Docroot | Front controller | Template de vhost |
|---|---|---|---|---|
| `php-prestashop` | 1.7, 8, 9 | `www` | `index.php` | `php-fpm` + règles Prestashop |
| `php-symfony` | 3.4 | `www/web` | `app.php` | `php-fpm` + front controller |
| `php-symfony` | 4.x → 8.x | `www/public` | `index.php` | `php-fpm` + front controller |
| `php-plain` | — | `www` | — | `php-fpm` |
| `static` | — | `www` | — | `static` |
| `go`, `node`, `python`, `ruby` | — | — (proxy) | — | `proxy` |

WordPress, Laravel et les autres frameworks ne sont pas livrés : le mécanisme les accepte par simple ajout
dans `apptypes.yaml`, sans code ni recompilation (D19).

#### 7.10.1 Symfony 3.4 et Symfony 4 → 8

L'écart entre Symfony 3.4 et les versions suivantes n'est pas cosmétique et interdit un type unique :

| | Symfony 3.4 | Symfony 4.x → 8.x |
|---|---|---|
| Docroot | `www/web` | `www/public` |
| Front controller | `app.php` (et `app_dev.php`) | `index.php` |
| Console | `bin/console` | `bin/console` |
| Variable d'environnement | `SYMFONY_ENV` | `APP_ENV` |
| Configuration | `app/config/parameters.yml` | `.env` |
| Messenger | **absent** (composant introduit en 4.1) | disponible |
| PHP maximal | 7.4 | selon la mineure (§7.10.3) |

**Détection de la variante** : `app.version: auto` (défaut) lit la contrainte de
`symfony/framework-bundle` dans `www/composer.lock`, puis `composer.json` ; à défaut, l'arborescence tranche
(`web/app.php` → 3.4, `public/index.php` → 4+). La valeur peut toujours être forcée. La variante détectée est
enregistrée dans l'état, et un changement détecté après une montée de version applicative apparaît dans
`ogs diff` : le docroot et le vhost à réécrire y sont explicités, ce qui fait de la migration 3.4 → 4 une
opération accompagnée plutôt que subie.

**Conséquence sur les workers** : le template `symfony-messenger` (§7.9) est refusé pour un projet en 3.4 —
le composant n'existe pas. Le template `rabbitmq-consumer` prend le relais pour les sites 3.4 s'appuyant sur
`oldsound/rabbitmq-bundle` (`bin/console rabbitmq:consumer <nom>`), avec les mêmes garde-fous de
redémarrage, de limite mémoire et de durée de vie.

#### 7.10.2 Prestashop

- Docroot `www`, `.htaccess` généré par Prestashop lui-même : `AllowOverride All` côté Apache. Côté **Nginx,
  il n'y a pas de `.htaccess`** — les règles de réécriture doivent être fournies par le template (routage
  `/index.php`, `/img/`, `/c/`, images webp, `/api/`, blocage de `/app/config`, `/var/logs`, `/translations`,
  `/vendor`). C'est le point où une configuration générique produit un site à moitié fonctionnel.
- Le répertoire d'administration porte un suffixe aléatoire (`admin1a2b3c/`) : il est enregistré dans l'état
  du projet afin que les blocs de restriction (authentification, filtrage d'adresses, `noindex`) puissent le
  cibler sans que l'administrateur ait à le retrouver.
- Réglages PHP relevés par défaut : `memory_limit 512M`, `max_input_vars 5000`, `upload_max_filesize 64M`,
  `max_execution_time 300` — valeurs surchargeables, alignées sur les prérequis Prestashop.
- Les répertoires inscriptibles attendus (`var/cache`, `var/logs`, `img`, `upload`, `download`,
  `translations`, `config`, `themes`) appartiennent à l'utilisateur du projet, donc inscriptibles sans
  réglage particulier ; `img` et `upload` restent lisibles par `www-data` via le groupe, puisqu'ils sont
  servis en statique.

#### 7.10.3 Compatibilité PHP × application

Les versions PHP gérées vont de **7.4 à 8.5** (dépôt Sury), soit `7.4`, `8.0`, `8.1`, `8.2`, `8.3`, `8.4`,
`8.5`. La numérotation des pools s'y applique sans changement : PHP 7.4 premier pool → `17401`, PHP 8.5
premier pool → `18501`.

**La source d'autorité est le `composer.json` de l'application**, pas une table figée dans `ogs` : la
contrainte `require.php` est lue et confrontée à la version du pool. Un écart est signalé au `plan` — de
manière bloquante pour une incompatibilité déclarée, sous forme d'avertissement si la contrainte est absente
ou illisible. Ce choix évite d'embarquer une matrice qui vieillit à chaque sortie.

Table indicative, utilisée seulement en l'absence de `composer.json` exploitable :

| Application | PHP compatible |
|---|---|
| Symfony 3.4 | 7.4 maximum — **ne fonctionne pas sur PHP 8** |
| Symfony 4.4 | 7.4 → 8.1 |
| Symfony 5.4 | 7.4 → 8.3 |
| Symfony 6.4 | 8.1 → 8.4 |
| Symfony 7.x | 8.2 et au-delà |
| Symfony 8.x | 8.3 ou 8.4 selon la mineure — valeur à lire dans `composer.json`, non figée |
| Prestashop 1.7 | 7.1 → 8.1 selon la mineure |
| Prestashop 8 | 7.4 → 8.1 |
| Prestashop 9 | 8.1 et au-delà |

`ogs php switch` refuse par défaut une bascule vers une version incompatible avec l'application détectée et
exige `--force` accompagné du motif, journalisé. C'est le garde-fou le plus utile de tout le module PHP : une
bascule de 7.4 vers 8.x sur un Symfony 3.4 rend le site inaccessible instantanément.

**Fin de support** : `doctor` marque les versions dont le support est terminé (7.4, 8.0 et 8.1 le sont à ce
jour) à partir d'une table datée embarquée, sans jamais bloquer — un Prestashop ancien peut exiger 7.4, et
c'est une réalité d'exploitation, pas une erreur de configuration. L'information est remontée dans `status` et
dans la TUI, avec la date de fin de support.

### 7.11 Applications non-PHP : Go, Node, Ruby, Python

Le dispositif réutilise deux briques existantes : un **service systemd** (§7.9) supervise le processus, un
**vhost en reverse-proxy** (§7.6) l'expose. Ce qui s'ajoute est le runtime, l'écoute et l'attente de
disponibilité.

#### 7.11.1 Déclaration

```yaml
  app:
    kind: node
    runtime: "22.11.0"          # version installée pour ce projet
    entrypoint: "server.js"     # relatif à www/
    listen: socket              # socket (défaut) | port
    instances: 1
    env:
      NODE_ENV: production
    health:
      path: /health             # optionnel : requête HTTP de validation
      timeout: 30s
```

#### 7.11.2 Runtimes, installés par utilisateur

Conformément à D35, chaque projet possède sa propre installation dans son home :

```
/home/shop.fr/.nvm/versions/node/v22.11.0/bin/node
/home/shop.fr/.rbenv/versions/3.3.6/bin/ruby
/home/shop.fr/.pyenv/versions/3.12.7/bin/python3
/home/shop.fr/www/.venv/bin/python          ← venv du projet (Python)
```

```
$ ogs runtime install shopfr node 22.11.0
  → installe nvm dans /home/shop.fr/.nvm (si absent)
  → nvm install 22.11.0
  → lien /home/shop.fr/.bin/node → …/v22.11.0/bin/node
  → liens npm, npx
```

Points d'implémentation :

- `nvm`, `rbenv` et `pyenv` sont des **scripts shell conçus pour un shell interactif** : ils sont
  inutilisables tels quels dans une unité systemd, qui n'en charge aucun profil. Les unités et les crons
  invoquent donc exclusivement les liens de `.bin/` (D30), jamais `nvm use`.
- L'utilisateur ayant un shell `nologin`, les commandes d'installation sont exécutées par
  `su -s /bin/bash -c '…' <id>`, qui force le shell indépendamment de `/etc/passwd`.
- L'installation télécharge depuis Internet : l'étape est **non critique** (§6.3) et son échec n'annule pas la
  création du site, mais le service applicatif reste alors à l'arrêt avec un message explicite.
- Pour Python, `pyenv` fournit l'interpréteur et `ogs` crée le venv du projet (`python -m venv www/.venv`) ;
  `.bin/python` pointe vers le venv, pas vers l'interpréteur nu, afin que les dépendances suivent.
- Pour Go, aucun gestionnaire de version : `ogs` ne compile pas (§1.3). Le déploiement dépose un binaire et
  `.bin/app` pointe dessus — le remplacer puis redémarrer le service constitue une bascule atomique.

**Contrepartie assumée du choix « par utilisateur »** : chaque version est dupliquée par site (de l'ordre de
100 Mio pour une version de Node), les mises à jour de sécurité du runtime sont à faire autant de fois qu'il
y a de projets, et le lien `.bin/node`, bien qu'appartenant à root, pointe vers un binaire situé dans un
répertoire que l'utilisateur contrôle : il peut donc en changer le contenu. Cela ne crée aucune escalade — il
exécuterait ce binaire sous sa propre identité de toute façon — mais la garantie « la version effective est
celle de l'inventaire » ne vaut, pour ces runtimes, que pour PHP. `ogs status` compare la version rapportée
par `.bin/<runtime> --version` à celle déclarée et signale l'écart.

#### 7.11.3 Écoute : socket Unix par défaut

```
listen: socket  →  /run/ogs/shopfr/app.sock   0770 shopfr:shopfr
listen: port    →  127.0.0.1:22001            norme 2<runtime><incrément>
```

| Runtime | Code | Écoute recommandée | Fourniture du point d'écoute |
|---|---|---|---|
| `go` | 1 | port (21xxx) | `LISTEN_ADDR=127.0.0.1:21001` |
| `node` | 2 | socket, si l'application le gère | `SOCKET_PATH=/run/ogs/<id>/app.sock` |
| `python` | 3 | socket (gunicorn/uvicorn `--bind unix:…`) | ligne de commande du template |
| `ruby` | 4 | socket (puma `-b unix://…`) | ligne de commande du template |

- Le socket vit dans `/run/ogs/<id>/`, répertoire propre au projet créé par `tmpfiles.d` (§7.8). Ses
  permissions `0770 <id>:<id>` autorisent la connexion du processus et de `www-data` (membre du groupe), et
  la refusent à tout autre utilisateur — l'isolation est celle des pools PHP en socket Unix.
- Pour l'obtenir, l'unité fixe **`UMask=0007`** : un socket Unix est créé en `0777 & ~umask`, donc le umask
  par défaut (0022) produirait `0755`, dépourvu du bit d'écriture pour le groupe — et `www-data` ne pourrait
  pas s'y connecter. C'est une cause de panne peu évidente à diagnostiquer.
- **`ogs` ne peut pas forcer une application à écouter sur un socket** : pour Node, c'est le code applicatif
  qui appelle `server.listen()`. Le chemin est fourni par variable d'environnement et, si l'application
  l'ignore, l'échec est détecté par l'attente de disponibilité avec un message qui nomme précisément la
  cause. Pour Python et Ruby, la ligne de commande du template (gunicorn, uvicorn, puma) garantit le socket.
- En mode port, l'adresse d'écoute impose **127.0.0.1** et jamais `0.0.0.0` : une application écoutant sur
  toutes les interfaces est joignable directement depuis Internet, contournant le vhost, le TLS, les en-têtes
  de sécurité et l'éventuelle authentification. `ogs doctor` inspecte les sockets en écoute et signale tout
  port applicatif exposé hors de la boucle locale.

#### 7.11.4 Unité générée (exemple Node)

```ini
# /etc/systemd/system/ogs-shopfr-app@.service
[Unit]
Description=ogs shopfr — application node (instance %i)
PartOf=ogs-shopfr.target
After=network.target

[Service]
Type=simple
User=shopfr
Group=shopfr
WorkingDirectory=/home/shop.fr/www
Environment=NODE_ENV=production
Environment=SOCKET_PATH=/run/ogs/shopfr/app.sock
ExecStart=/home/shop.fr/.bin/node server.js
Restart=always
RestartSec=5
UMask=0007
KillSignal=SIGTERM
TimeoutStopSec=30
StandardOutput=journal
StandardError=journal
SyslogIdentifier=ogs-shopfr-app

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=read-only
ReadWritePaths=/home/shop.fr/var /run/ogs/shopfr

[Install]
WantedBy=ogs-shopfr.target
```

#### 7.11.5 Vhost en reverse-proxy

```apache
# Apache — socket Unix (mod_proxy_http, unix: supporté depuis 2.4.7)
ProxyPreserveHost On
ProxyPass        /.well-known/acme-challenge !
ProxyPass        / unix:/run/ogs/shopfr/app.sock|http://localhost/
ProxyPassReverse / unix:/run/ogs/shopfr/app.sock|http://localhost/
RequestHeader set X-Forwarded-Proto "https"
```

```nginx
# Nginx — socket Unix
upstream ogs_shopfr { server unix:/run/ogs/shopfr/app.sock; }
location / {
    proxy_pass http://ogs_shopfr;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;      # WebSocket
    proxy_set_header Connection $connection_upgrade;
}
```

Le service statique peut rester à la charge du serveur web (`location /assets/ { root /home/shop.fr/www/public; }`)
plutôt que d'être proxifié, ce qui décharge l'application : c'est le comportement par défaut lorsque le type
d'application déclare un répertoire d'actifs.

#### 7.11.6 Ordre et attente de disponibilité

L'ordre des dépendances (§6.2) s'enrichit pour ces types : le runtime est installé, puis le service est
démarré, puis **`ogs` attend que le socket ou le port réponde** — et seulement ensuite écrit et active le
vhost de proxy. Sans cette attente, le premier reload exposerait un `502 Bad Gateway`.

L'attente est bornée (`health.timeout`, 30 s par défaut) et vérifie, dans l'ordre : existence du socket ou
ouverture du port, puis, si `health.path` est renseigné, une requête HTTP dont la réponse doit être inférieure
à 500. L'échec est **non critique** : le site est laissé en place avec son service à l'arrêt, un avertissement
et la commande de reprise, plutôt que de tout annuler.

`ogs app restart|logs|status <id>` est un raccourci vers le service applicatif, et `sudo ogs my restart`
(§7.9.1) le redémarre au même titre que les autres services du projet.

---

## 8. Secrets et keystore

### 8.1 Conception

- **Clé maître** : 32 octets aléatoires dans `/etc/ogs/master.key`, mode 0400, propriétaire root. Générée
  à l'initialisation (`ogs init`). Sauvegarde par l'administrateur explicitement recommandée : sans elle,
  les secrets sont irrécupérables.
- **Chiffrement** : AES-256-GCM, nonce aléatoire de 12 octets par secret, données associées =
  `<projet>/<type>/<identifiant>` (interdit la substitution d'un secret par un autre).
- **Stockage** : table `secrets` de `state.db` (`scope`, `key`, `nonce`, `ciphertext`, `created_at`,
  `rotated_at`, `version`).
- **Génération** : 32 caractères tirés d'un alphabet excluant les caractères ambigus et problématiques en
  shell (`'`, `"`, `` ` ``, `\`, `$`, espace). Entropie ≥ 160 bits. Longueur et alphabet configurables.

### 8.2 Manipulation

```
ogs secrets list [projet]              # noms uniquement, jamais les valeurs
ogs secrets show shopfr/mysql/shopfr   # affiche la valeur, entrée d'audit obligatoire
ogs secrets rotate shopfr/mysql/shopfr # régénère, applique au SGBD, met à jour les fichiers dépendants
ogs secrets env shopfr                   # émet les DATABASE_URL / MESSENGER_TRANSPORT_DSN
```

`secrets env` produit les chaînes de connexion prêtes à coller dans un `.env` :

```
DATABASE_URL="mysql:/shopfr_usr:xxx@127.0.0.1:3306shopfr_db?serverVersion=8.0&charset=utf8mb4"
MESSENGER_TRANSPORT_DSN="amqp:/shopfr_amqp:xxx@127.0.0.1:5672shopfrfr"
```

Un hook `postCreate` peut consommer cette sortie pour écrire directement le `.env` applicatif.

### 8.3 Modèle de menace assumé

Le keystore protège contre : la lecture d'une sauvegarde du fichier `state.db` sans la clé maître, la
divulgation par recopie de la base, la lecture par un utilisateur non-root. Il ne protège **pas** contre un
compromis root de la machine, qui donne accès à la clé maître et donc à tous les secrets. Cette limite est
acceptée : `ogs` étant root-strict, elle est intrinsèque.

---

## 9. Import d'un serveur existant

`ogs import scan` inspecte le système et propose une adoption interactive.

**Sources inspectées** : vhosts Apache/Nginx (`sites-available`, `sites-enabled`), pools PHP-FPM, comptes
système d'UID ≥ 1000 avec home sous `/home` ou `/var/www`, bases MySQL/PostgreSQL et leurs utilisateurs,
vhosts et queues RabbitMQ, crontabs utilisateur et `/etc/cron.d`, unités systemd hors paquets, certificats
existants (`/etc/letsencrypt/live`, `/etc/ssl`).

**Corrélation** : regroupement par similarité de nom, par docroot commun, par utilisateur du pool, par
socket FPM référencé dans un vhost. L'identifiant proposé est celui que produirait la règle de dérivation
(§3.3) appliquée au `ServerName` du vhost ; il est modifiable à l'adoption, et l'écart entre le nommage
existant et le nommage canonique est affiché. **L'adoption ne renomme jamais** : les ressources gardent
leurs noms réels, l'identifiant `ogs` n'étant qu'une clé de regroupement. Un renommage vers la convention
canonique est une opération distincte et explicite (`ogs project rename`), signalée comme destructrice
puisqu'elle implique la recréation de l'utilisateur, la migration des bases et l'arrêt des services. Chaque grappe candidate est présentée avec son niveau de confiance et
les éléments incertains explicitement signalés (typiquement la propriété d'une base, non déductible).

```
$ ogs import scan

Projet candidat : shopfr  (confiance élevée)
  vhost apache    shop.fr, www.shop.fr → /home/shop.fr/www/public
  pool php        8.3shopfr  (user shopfr)
  user système    shopfr (uid 1004, home /home/shop.fr)
  base mysql      shopfr  utilisateurs : shopfr@localhost   [propriété à confirmer]
  cert certbot    /etc/letsencrypt/live/shop.fr  expire 12/10/2026
  cron            2 tâches dans la crontab de shopfr
  Adopter ? [o]ui / [n]on / [d]étail / [m]odifier / [i]gnorer

Non géré (laissé strictement intact) :
  vhost old-blog.fr, base wordpress_old, user backup-svc
```

**Garanties** :
- Le scan est en lecture seule. Rien n'est modifié.
- L'adoption produit les YAML et les enregistrements de base, sans exécuter d'apply.
- Les secrets existants étant inconnus, les mots de passe des utilisateurs adoptés sont marqués
  `unknown` : ils ne sont ni devinés ni réinitialisés. `secrets rotate` est proposé si l'administrateur
  souhaite reprendre la main.
- Toute ressource non adoptée est enregistrée comme « non gérée » : le moteur ne la planifiera jamais, et
  les détections de conflit la prennent en compte.
- Après adoption, un `ogs diff <projet>` montre l'écart entre l'existant et ce que `ogs` produirait. Le
  premier `apply` normalise la configuration (et le montre avant d'agir).

**Certificats certbot** : deux options à l'adoption — conserver les chemins certbot et laisser son timer
opérer, ou migrer vers le stockage `ogs` avec réémission (recommandé pour l'homogénéité, coûte une émission
ACME).

---

## 10. Interface en ligne de commande

### 10.1 Arborescence des commandes

```
ogs
├── init                             initialise /etc/ogs, la base, la clé maître
├── doctor                           diagnostic : paquets, modules, plugins, permissions, versions
├── version
│
├── site create <domaine>            façade composite (voir §10.2)
├── site clone <domaine> --to prep    duplique un site vers un autre environnement
├── site list | show | delete
│
├── project create <id>              projet nu, sans ressources
├── project list | show | edit | delete | rename
├── project export <id>              YAML sur stdout
├── project import <fichier>         crée/met à jour depuis un YAML
│
├── plan [projet]                    calcule et affiche le plan (aucune écriture)
├── apply [projet] [-f fichier]      applique (--dry-run, --yes, --force)
├── diff [projet]                    écart état désiré / système réel
├── status [projet]                  état de santé consolidé
│
├── user create | delete | passwd | show
├── user key show | rotate            clé de déploiement ed25519 (§7.1.5)
│
├── db list | show
├── db create <projet> --engine --name --user
├── db drop | grant | revoke
├── db dump <projet> <base> [-o fichier]
├── db restore <projet> <base> -i fichier
├── db user add | remove | rotate
│
├── amqp vhost create | delete | list
├── amqp user create | delete | perms
├── amqp queue add | remove | list | purge
├── amqp exchange add | remove
├── amqp binding add | remove
├── amqp messenger <projet> --transport async,failed
│
├── app types                        types d'application connus et docroot associé
├── app set <id> --kind <kind>       change le type (docroot, vhost, service)
├── app start | stop | restart | status | logs
├── runtime list                     runtimes installés, par projet
├── runtime install <id> <runtime> <version>
├── runtime remove <id> <runtime> <version>
├── php list                         versions installées et pools
├── php ports                        numéros attribués, par version et par pool
├── php pool add | remove | show | tune
├── php switch <projet> <version>
├── php reload [version]
│
├── web vhost add | remove | enable | disable | show
├── web engine                       moteur détecté et actif
├── web test                         validation de configuration
├── web reload
│
├── cert list | show
├── cert issue <projet|nom>
├── cert renew [--all] [--force]
├── cert revoke
├── cert dns-provider set | list
│
├── cron add | remove | list | enable | disable | run
│
├── service add | remove | list | show
├── service start | stop | restart | scale | logs
│
├── my start|stop|restart|status         surface déléguée à l'utilisateur du projet (§7.9.1)
├── logs <id> [--web|--php|--cron|--service|--amqp] [-f]
├── secrets list | show | rotate | env
│
├── import scan | adopt | unmanaged
│
├── template list | show | extract    extrait un template embarqué pour surcharge
├── audit log [--projet] [--since]
└── completion bash|zsh|fish
```

### 10.2 Commande composite `site create`

```
ogs site create shop.fr \
  --alias www.shop.fr \
  --php 8.4 --preset medium \
  --mysql \
  --amqp --messenger async,failed \
  --tls \
  --service symfony-messenger:async:3 \
  --cron "* * * * *:bin/console schedule:run" \
  --engine apache
```

L'identifiant n'est pas saisi : il est dérivé du host (`shop.fr` → `shopfr`, §3.3). `--id` permet de le
forcer en cas de collision ou de troncature jugée illisible ; `--host` dissocie le host servi du host
canonique.

Variante préproduction — projet entièrement distinct, sans ressource partagée avec la production :

```
ogs site create shop.fr --env prep --basic-auth
  → host effectif  shopfr.preprod-pp.net
  → identifiant    shopfrprep
  → home           /home/shopfr.preprod-pp.net
  → certificat     *.preprod-pp.net (partagé, déjà émis) — aucun appel ACME
```

Comportement : construit le projet en mémoire, écrit `/etc/ogs/projects/shopfr.yaml`, affiche le plan, demande
confirmation, applique en saga, puis affiche le récapitulatif avec les chaînes de connexion générées.

Options transverses de toutes les commandes mutantes : `--dry-run`, `--yes`, `--non-interactive`, `--force`,
`--output`, `--verbose`, `--quiet`, `--no-hooks`, `--config`.

**`--prefix <répertoire>`** décale l'ensemble des chemins système sous un répertoire et simule les commandes
système. C'est la seule dérogation au root-strict (D21) : sans elle, aucun test automatisé ne serait
possible, puisque toute opération exigerait les privilèges du superutilisateur et modifierait la machine de
développement. Ce mode est réservé aux essais et au développement ; il l'annonce sur la sortie d'erreur et
n'exécute aucune commande réelle.

### 10.3 Codes de retour

| Code | Signification |
|---|---|
| 0 | succès |
| 1 | erreur d'exécution |
| 2 | erreur d'usage (arguments invalides) |
| 3 | échec de validation (spécification incohérente) |
| 4 | conflit (ressource déjà existante ou possédée par un autre projet) |
| 5 | dépendance absente (paquet, module, plugin, service arrêté) |
| 6 | succès partiel (étapes non critiques en échec) |
| 7 | verrou indisponible (autre instance en cours) |
| 8 | privilèges insuffisants (non root) |
| 9 | opération annulée par l'utilisateur |

### 10.4 Sorties machine

`--output json|yaml` sur toutes les commandes de lecture, avec une enveloppe stable :

```json
{
  "apiVersion": "ogs/v1",
  "kind": "ProjectList",
  "items": [ { "name": "shopfr", "domains": ["shop.fr"], "php": "8.4", "status": "healthy" } ],
  "warnings": []
}
```

Le plan est également sérialisable (`ogs plan --output json`), afin d'être exploitable par un pipeline CI
qui décide d'appliquer ou non.

---

## 11. Interface TUI

Bubble Tea. Lancée par `ogs` sans argument ou par `ogs tui`.

**Écran principal** — liste maître/détail :

```
┌─ ogs ────────────────────────────────────────────────────────────────────┐
│  Projets (13)             │  shopfr                          prod        │
│  ───────────────────────  │  ────────────────────────────────────────    │
│ ▸ shopfr          ● sain  │  Host       shop.fr, www.shop.fr             │
│   shopfrprep      ● sain  │  Home       /home/shop.fr  0750              │
│   blogexemplefr   ▲ 9 j   │  User       shopfr:shopfr (uid 1004)         │
│   apipartnersfr   ✕ 1 KO  │  Accès web  www-data ∈ groupe shopfr         │
│   monsitefr       ⚠ dérive│  PHP        8.4 · pool shopfr (medium)       │
│   …                       │  Web        apache · TLS ✓ 12/11/2026        │
│                           │  MySQL      shopfr · user shopfr             │
│                           │  AMQP       shopfr · async 1 240 · failed 0  │
│                           │  Services   messenger-async 3/3 ●            │
│                           │  Crons      scheduler (il y a 12 s)          │
│                           │  Dérive     aucune                           │
├──────────────────────────────────────────────────────────────────────────┤
│ ↑↓ naviguer  ⏎ détail  n nouveau  e éditer  a apply  d diff  l logs      │
│ r restart  c cert  s secrets  i import  / filtrer  ? aide  q quitter     │
└──────────────────────────────────────────────────────────────────────────┘
```

**Fonctions** :
- Navigation et filtrage des projets, vue détaillée par ressource avec descente dans les sous-objets.
- Indicateurs d'attention : certificat proche de l'expiration, unité `failed`, dérive de configuration,
  queue anormalement profonde, pool FPM saturé.
- Actions contextuelles : apply, diff, restart de service, suivi de logs (journalctl et logs de crons),
  renouvellement de certificat, affichage/rotation d'un secret (avec confirmation).
- **Wizard de création** : suite d'étapes (identité → domaines → PHP → bases → AMQP → web/TLS → services →
  crons → récapitulatif) produisant le YAML, affichant le plan, puis appliquant. Sortie possible à toute
  étape avec sauvegarde du YAML sans application.
- Toute action mutante affiche le plan et demande confirmation, exactement comme la CLI : la TUI n'est
  qu'un pilote du même moteur, sans chemin de code privilégié.

**Contraintes** : rendu correct à partir de 80×24, dégradation propre sans couleur (`NO_COLOR`,
`TERM=dumb`), aucune opération bloquante sur le thread de rendu (observation en arrière-plan avec
indicateur de chargement), rafraîchissement des états sur demande et non en boucle serrée.

---

## 12. Templates et hooks

### 12.1 Templates

Tous les templates sont embarqués dans le binaire (`embed.FS`) et surchargeables : si
`/etc/ogs/templates/<chemin>` existe, il remplace la version embarquée. `ogs template extract
apache/php-fpm` écrit la version embarquée dans `/etc/ogs/templates/` pour servir de point de départ.

Moteur : `text/template` avec fonctions ajoutées — `snake`, `kebab`, `truncate`, `hash`, `quote`,
`indent`, `default`, `join`, `sockPath`, `phpBin`.

Contexte fourni : le projet complet, la ressource courante, la version PHP résolue, les chemins de la
distribution détectée, la version d'`ogs`, l'horodatage.

Un template surchargé est validé au chargement (parsing) et son rendu passe par la validation du moteur
concerné (`apachectl -t`…) avant activation : une surcharge cassée ne peut pas casser le serveur.

### 12.2 Hooks

Scripts exécutés par `ogs` aux points suivants :

| Hook | Moment | Critique |
|---|---|---|
| `preCreate` | avant la première étape d'un projet neuf | oui |
| `postCreate` | après la dernière étape réussie | non |
| `preDelete` | avant toute suppression | oui |
| `postDelete` | après suppression et archivage | non |
| `preApply` / `postApply` | encadrent tout apply | non |
| `postCertRenew` | après renouvellement réussi | non |
| `postPhpSwitch` | après bascule de version | non |

Contrat : le script reçoit le contexte JSON sur stdin (projet, action, ressources touchées, chemins), les
variables `OGS_PROJECT`, `OGS_ACTION`, `OGS_ROOT` dans l'environnement, s'exécute sous root avec un délai
maximal de 300 s (configurable), et son stdout/stderr est capturé dans les logs et l'audit. Un code de
retour non nul sur un hook critique interrompt l'opération et déclenche le rollback. `--no-hooks` désactive
l'ensemble.

---

## 13. Archivage et suppression

`ogs project delete shopfr` (mode par défaut, D6) :

1. Hook `preDelete`.
2. Arrêt et désactivation des services, suppression des unités.
3. Suppression du fichier cron.
4. Désactivation du vhost, reload web.
5. Dump des bases du projet (`mysqldump` / `pg_dump`).
6. Constitution de l'archive `/var/lib/ogs/archives/shopfr-20260822-1430.tar.zst` contenant : YAML du projet,
   configurations générées (vhost, pool, unités, cron), dumps de bases, secrets **chiffrés**, arborescence
   `/home/shop.fr`, et un manifeste JSON décrivant tout ce qui a été supprimé.
7. Suppression des ressources vivantes : bases et utilisateurs SGBD, vhost et utilisateur AMQP, pool FPM,
   fichiers de configuration, certificats (révocation si configurée), utilisateur système, arborescence.
8. Le projet est marqué `archived` en base ; son YAML est déplacé dans `projects/archived/`.
9. Hook `postDelete`.

Options : `--purge` supprime sans archiver (double confirmation, saisie de l'identifiant exigée) ; `--keep-data`
conserve bases et fichiers en ne désactivant que le service ; `--no-dump` saute l'étape 5.

`ogs project restore <archive>` recrée le projet depuis une archive : YAML rejoué, dumps restaurés, secrets
déchiffrés et réinjectés. Nécessite la même clé maître.

Garde-fous : si la taille cumulée à archiver dépasse un seuil configurable (défaut 5 Gio), confirmation
explicite avec l'estimation affichée ; vérification de l'espace disque disponible avant de commencer.

---

## 14. Observabilité

### 14.1 Logs applicatifs

`log/slog` en JSON vers `/var/log/ogs/ogs.log`, plus une sortie humaine sur le terminal. Champs communs :
`ts`, `level`, `msg`, `project`, `resource`, `action`, `step_id`, `run_id`, `duration_ms`, `error`.
Niveaux pilotés par `-v` (info → debug → trace, où trace journalise toutes les commandes système exécutées
et les requêtes SQL, secrets masqués).

### 14.2 Journal d'audit

Fichier append-only `/var/log/ogs/audit.log` **et** table `audit_log`. Une entrée par étape appliquée :
identifiant de run, horodatage, projet, ressource, action, résultat, empreinte avant/après pour les
fichiers, et pour les secrets consultés l'identifiant du secret (jamais la valeur).

`ogs audit log --projet shopfr --since 7d` restitue l'historique, exportable en JSON.

### 14.3 Identité de l'appelant

L'outil est root-strict (D21) et n'implémente aucun contrôle d'accès. `SUDO_USER`, `SSH_CONNECTION` et
l'UID d'appel effectif sont néanmoins enregistrés dans l'audit **à titre informatif** : ce sont des
indications de traçabilité, sans valeur de preuve puisque falsifiables par quiconque est déjà root.

### 14.4 `ogs status` et `ogs doctor`

- `status` : vue consolidée par projet — services (actifs/attendus), expiration des certificats,
  profondeur des queues, pools FPM (processus actifs/max), dérive de configuration, dernière exécution des
  crons. Code de retour non nul si un problème est détecté, ce qui le rend utilisable en supervision.
- `doctor` : vérifie paquets et binaires attendus, modules Apache/Nginx, plugin RabbitMQ management,
  accessibilité des sockets SGBD, permissions de `/etc/ogs` et de la clé maître, cohérence entre la base
  d'état et le système, présence des unités `ogs-renew`, versions PHP utilisables. Chaque anomalie est
  accompagnée de la commande de correction.

---

### 14.5 Logs des projets et rotation

Chaque projet dispose d'un unique répertoire de logs d'infrastructure, `/home/<host effectif>/logs/`, dont
la propriété root et les implications de sécurité sont détaillées au §7.1.2.

| Fichier | Producteur | Propriétaire |
|---|---|---|
| `apache-access.log`, `apache-error.log` | master Apache (root) | `root:<id>` |
| `nginx-access.log`, `nginx-error.log` | master Nginx (root) | `root:<id>` |
| `php-error.log`, `php-slow.log` | master PHP-FPM (root) | `root:<id>` |
| `cron-<nom>.log` | tâche cron (utilisateur du projet) | `<id>:<id>` |

Les logs **applicatifs** (Symfony, Monolog) restent sous `var/log/`, entièrement gérés par l'application, et
ne sont pas rotés par `ogs`. Les logs RabbitMQ n'ont pas d'équivalent par projet (§7.4).

**Rotation** — un fichier unique `/etc/logrotate.d/ogs`, régénéré à chaque apply, listant explicitement les
chemins de tous les projets. Pas de glob sur `/home/*/logs/`, qui embarquerait les répertoires
d'utilisateurs humains et permettrait de déclencher les scripts de rotation depuis un compte non géré.

```
# Généré par ogs — NE PAS MODIFIER À LA MAIN
# 12 projets  empreinte: sha256:…  généré: 2026-08-23T03:00:00Z

/home/shop.fr/logs/apache-access.log
/home/shop.fr/logs/apache-error.log
/home/shop.fr/logs/php-error.log
/home/shop.fr/logs/php-slow.log
/home/autresite.fr/logs/apache-access.log
{
    daily
    rotate 30
    maxsize 200M
    missingok
    notifempty
    compress
    delaycompress
    create                       ← sans mode ni propriétaire : hérités du fichier d'origine
    sharedscripts
    postrotate
        systemctl reload apache2 php8.5-fpm php8.4-fpm > /dev/null 2>&1 || true
    endscript
}

/home/shop.fr/logs/cron-*.log
/home/autresite.fr/logs/cron-*.log
{
    weekly
    rotate 8
    missingok
    notifempty
    compress
    create                       ← les crons rouvrent leur fichier à chaque exécution : pas de signal
}
```

Points de conception :

- `create` est écrit **sans mode ni propriétaire** : logrotate reprend alors ceux du fichier d'origine, ce
  qui permet à un seul bloc de couvrir des projets dont les logs appartiennent à des utilisateurs
  différents. C'est ce qui rend le fichier unique possible.
- `sharedscripts` garantit **un seul** reload par rotation, quel que soit le nombre de projets — sans cela,
  cent projets provoqueraient cent rechargements du serveur web.
- Le bloc de reload n'énumère que les moteurs et versions PHP réellement utilisés, recalculés à chaque
  génération.
- Aucune directive `su` n'est nécessaire, les répertoires `logs/` n'étant pas inscriptibles par un non-root.
- Rétention configurable globalement (`logs.rotate`, `logs.maxSize`, `logs.compress`) et surchargeable par
  projet.
- `ogs logs <id>` agrège la consultation : `--web`, `--php`, `--cron <nom>`, `--service <nom>` (journalctl),
  `--amqp` (log du broker filtré), avec `-f` pour le suivi et `--since`.

## 15. Sécurité

| Risque | Mesure |
|---|---|
| Injection SQL dans les identifiants | Liste blanche stricte `[a-zA-Z0-9_]` sur tout nom de base, rôle, utilisateur ; refus sinon. Les identifiants n'étant pas paramétrables en SQL, la validation est la seule défense et elle est centralisée dans `validate/`. |
| Injection shell | Aucun `sh -c` : `exec.CommandContext` avec arguments séparés. Les rares cas nécessitant un shell (lignes de cron) passent par une génération contrôlée et un échappement explicite. |
| Traversée de chemin | Tout chemin issu de la spécification est nettoyé et vérifié comme appartenant à une racine autorisée (`/home`, `/etc/ogs`, `/var/lib/ogs`, `/var/log/ogs`). |
| Écrasement de configuration non gérée | En-tête et empreinte obligatoires ; refus d'écrire sur un fichier sans en-tête `ogs` sans `--force`, avec sauvegarde préalable. |
| Fuite de secrets | Masquage systématique dans les logs, l'audit et les messages d'erreur (filtre appliqué à la sortie, pas seulement au site d'appel) ; `secrets show` seul chemin de divulgation, audité. |
| Permissions trop larges | Modes explicites à chaque écriture (jamais dépendants du umask) ; `doctor` vérifie et propose la correction. |
| Escalade via hooks | Hooks refusés s'ils ne sont pas possédés par root et non inscriptibles par autrui ; chemin obligatoirement sous `/etc/ogs/hooks/`. |
| Abus de quota ACME | Compteur d'échecs par domaine, pause automatique, mode staging pour les tests. |
| Déni de service par erreur | Validation de configuration avant tout reload ; restauration automatique du fichier précédent en cas d'échec de validation. |
| Suppression accidentelle | Archivage par défaut, double confirmation et saisie de l'identifiant pour `--purge`, refus sur ressources non gérées. |
| Fuite entre sites | Groupe privé par projet, jamais de groupe partagé entre utilisateurs de sites ; `.env` en 0600, `var/` en 0700 ; `open_basedir` limité au home ; le pool FPM tourne sous l'utilisateur du projet, `www-data` n'exécute aucun code applicatif. |
| Application exposée hors du proxy | Écoute imposée sur socket Unix `0770 <id>:<id>` ou sur `127.0.0.1` exclusivement ; `doctor` signale tout port applicatif en écoute sur `0.0.0.0` (§7.11.3). |
| Fuite de la clé de déploiement | Clé privée en 0600, propriété de l'utilisateur, jamais restituée par une commande ni écrite dans un log ; déploiement en tirage, donc aucune clé à confier à un tiers. |
| Runtime altéré | Les runtimes non-PHP vivant dans le home de l'utilisateur (D35), leur intégrité n'est pas garantie par ogs : `status` compare la version rapportée à celle déclarée et signale l'écart. Aucune escalade possible, le processus tournant déjà sous cette identité. |
| Privilège MySQL trop large | Échappement systématique de `_` en `\_` dans tout motif de `GRANT`, verrouillé par un test unitaire dédié (§3.3.5). |
| Collision prod / préproduction | Suffixe `prep` préservé lors de toute troncature ; unicité vérifiée sur l'identifiant, le host effectif **et** le home. |
| Escalade root par lien symbolique de log | `logs/` appartient à root et n'est pas inscriptible par l'utilisateur ; les fichiers sont pré-créés. Aucun processus root n'ouvre en écriture un chemin dont un non-root contrôle le répertoire (§7.1.2). |
| Élargissement de la délégation sudo | Commandes énumérées sans métacaractère, jamais de `systemctl` nu ; `ogs my` ne prend aucun argument et déduit le projet de `SUDO_UID`, que sudo positionne sous `env_reset` ; validation `visudo -cf` avant activation, revalidation par `doctor` (§7.9.1). |
| Exécution croisée via un pool FPM | Socket Unix `0660 www-data:<id>` par défaut : seuls le serveur web et l'utilisateur du projet peuvent s'y connecter. Le mode TCP, dépourvu d'authentification, exige `--allow-tcp` et un avertissement explicite (§7.5). |
| Détournement de l'interpréteur | `.bin/` appartient à root : l'utilisateur ne peut pas substituer le lien `php` et faire diverger la version effective de celle que déclare l'inventaire. |

---

## 16. Tests

### 16.1 Tests unitaires

Cible : > 80 % de couverture sur `model`, `spec`, `validate`, `engine`, `template`, `secret`, `store`, et
sur la logique de planification de chaque provider (avec `Executor` simulé et observations injectées).

Cas systématiquement couverts : idempotence (un plan sur un état déjà conforme ne produit que des `NoOp`),
troncature déterministe des noms, validation des expressions cron, ordre des dépendances, rollback complet
d'une saga, distinction critique/non critique, rendu des templates comparé à des fichiers de référence
(golden files), chiffrement/déchiffrement avec mauvaise clé et données associées altérées.

### 16.2 Tests d'intégration

Conteneurs Debian 13 et Ubuntu 24.04 dotés de systemd (Podman ou Docker avec `systemd` comme PID 1),
orchestrés par `testcontainers-go`, avec Apache, Nginx, MySQL, MariaDB, PostgreSQL, RabbitMQ, PHP 8.2/8.3/8.4
préinstallés dans l'image de test.

Scénarios exigés :

1. `site create` complet, puis vérification effective : la page répond en HTTPS, PHP s'exécute, la connexion
   applicative aux bases fonctionne, la queue AMQP existe, le worker consomme un message publié de bout en
   bout, la tâche cron s'exécute.
2. `apply` rejoué sans changement → plan intégralement `NoOp`.
3. Modification manuelle d'un vhost → détection par `diff`, refus d'`apply`, correction avec `--force`.
4. `php switch` de 8.3 vers 8.4 sans erreur HTTP pendant la bascule (sollicitation continue en parallèle).
5. Échec de certificat simulé (DNS non résolvant) → site créé en HTTP, code de retour 6, reprise réussie
   après correction.
6. Échec critique injecté au milieu d'un `apply` → rollback vérifié ressource par ressource, système
   identique à l'état initial.
7. Cycle complet suppression avec archivage puis `project restore` → projet fonctionnel à l'identique.
8. `import scan` sur un conteneur préconfiguré « à la main » → adoption correcte, ressources non gérées
   laissées intactes, premier `diff` cohérent.
9. Deux `apply` simultanés → le second échoue en code 7 sans effet de bord.
10. ACME contre **pebble** (serveur ACME de test local) pour émission et renouvellement réels sans toucher
    Let's Encrypt.
11. **Isolation inter-sites** : deux sites créés, tentative de lecture du `.env` et du `var/` du second
    depuis un processus lancé sous l'utilisateur du premier → refus systématique. Test symétrique, et
    répété pour les trois modes de `webAccess`.
12. **Prise en compte du groupe `www-data`** : création d'un site alors que le serveur web tourne déjà,
    puis `reload` seul (sans `restart`) → le statique du nouveau site est servi. Ce test détermine si
    l'apply peut se contenter d'un reload (§7.1.3).
13. **Prod et préproduction du même host** sur le même serveur : les deux projets coexistent, aucune
    ressource partagée hormis le certificat, et la suppression de l'un n'affecte ni l'autre ni le
    certificat partagé.
14. **Troncature** : host de plus de 28 caractères en `prod` et en `prep` → identifiants distincts, tous
    deux ≤ 32 caractères, base commune identique, reproductibles d'une exécution à l'autre.
15. **Certificat partagé** : wildcard émis une fois contre pebble (DNS-01 simulé), puis trois
    préproductions créées → aucune des trois ne déclenche d'appel ACME, les trois répondent en HTTPS, et
    un renouvellement du wildcard provoque un seul reload du serveur web.
16. **Refus de suppression** d'un certificat partagé encore référencé, avec la liste des projets
    concernés ; suppression acceptée une fois le dernier vhost retiré.
17. **Délégation sudo** : sous l'utilisateur du projet, `sudo ogs my restart` redémarre bien ses services ;
    toute tentative de cibler un autre projet échoue, y compris avec un `SUDO_USER` falsifié ; aucune
    autre commande `ogs` n'est autorisée par sudo. Le fichier généré est vérifié par `visudo -cf` dans le
    test lui-même.
18. **Escalade par lien symbolique** : sous l'utilisateur du projet, tentative de remplacer
    `logs/apache-error.log` par un lien vers `/etc/sudoers.d/x` → refus du système (répertoire root), et
    vérification qu'aucune écriture root n'a eu lieu hors du fichier attendu.
19. **Rotation** : trois projets, un `logrotate -f /etc/logrotate.d/ogs` → tous les fichiers rotés, chacun
    recréé avec son propriétaire d'origine, **un seul** reload du serveur web, et le service web comme les
    crons continuent d'écrire dans les nouveaux fichiers.
20. **Bascule de version via le lien** : `php switch` de 8.4 vers 8.5 → ni le fichier cron ni les unités
    systemd ne sont modifiés (empreintes inchangées), le lien pointe sur la nouvelle version, les workers
    tournent avec le nouvel interpréteur après redémarrage, et la tâche cron l'utilise à son exécution
    suivante.
21. **Numérotation des pools** : création puis suppression puis recréation de pools sur deux versions →
    numéros attribués selon la norme, stables, sans collision, et réattribués uniquement après libération.
22. **Docroot par type d'application** : un projet `php-prestashop` sert bien `www/`, un `php-symfony` en
    4+ sert `www/public/`, un `php-symfony` en 3.4 sert `www/web/` avec `app.php`, et un `app set` bascule
    d'un type à l'autre en réécrivant le vhost sans perte de service.
23. **Détection de variante Symfony** : à partir d'un `composer.lock` de Symfony 3.4 puis d'un de Symfony
    6.4, la variante et le docroot sont déduits correctement ; sans composer, l'arborescence tranche.
24. **Garde-fou de compatibilité** : `php switch` de 7.4 vers 8.3 sur un Symfony 3.4 est refusé avec le
    motif lu dans `composer.json` ; l'opération n'aboutit qu'avec `--force`, et est journalisée.
25. **Cohabitation de versions** : un Prestashop en PHP 7.4 (pool 17401) et un Symfony 8 en PHP 8.5 (pool
    18501) servis simultanément, chacun avec son socket, sans interférence lors du reload de l'un.
26. **Worker Symfony 3.4** : le template `symfony-messenger` est refusé, le template `rabbitmq-consumer`
    accepté et fonctionnel de bout en bout.
27. **Application Node de bout en bout** : `runtime install` puis service démarré sur socket Unix, attente
    de disponibilité respectée, réponse HTTPS via le proxy, `www-data` capable de se connecter au socket
    (vérifie `UMask=0007`), et refus de connexion depuis un utilisateur tiers.
28. **Application Go sur port** : binaire déposé, `.bin/app` mis à jour, redémarrage, réponse via le
    proxy ; puis test négatif — une application écoutant sur `0.0.0.0` est détectée et signalée par
    `doctor`.
29. **Échec d'installation de runtime** (réseau coupé) : le site est créé, le service reste à l'arrêt, code
    de retour 6, et la reprise après rétablissement du réseau aboutit.
30. **Clé de déploiement** : générée sans passphrase, permissions correctes, `user key show` n'expose que la
    partie publique, `rotate` remplace la paire, et la clé survit à un cycle archivage/restauration.

### 16.3 Intégration continue

GitHub Actions : `go vet`, `staticcheck`, `golangci-lint`, `gofumpt` en vérification, tests unitaires avec
détecteur de concurrence, tests d'intégration par matrice (2 distributions × 2 serveurs web), `govulncheck`,
build multi-architecture. Les tests d'intégration sont obligatoires avant fusion sur la branche principale.

---

## 17. Packaging et distribution

- Binaire unique `ogs`, `CGO_ENABLED=0`, `-ldflags "-s -w -X …/version.Version=…"`, cibles
  `linux/amd64` et `linux/arm64`.
- GoReleaser : archives `.tar.gz`, paquet `.deb` (nfpm) et `.rpm` (préparé, non testé en v1), sommes de
  contrôle signées.
- Contenu du `.deb` : `/usr/bin/ogs`, unités `ogs-renew.service` et `ogs-renew.timer`, `tmpfiles.d/ogs.conf`,
  `logrotate.d/ogs`, complétions bash/zsh/fish, pages de manuel générées par Cobra, `postinst` exécutant
  `ogs init --quiet` de façon idempotente.
- Documentation : `README` (installation, prise en main), `docs/` (référence des commandes générée depuis
  Cobra, référence du format YAML, guide des templates et hooks, guide de migration/import).
- Licence : **WTFPL** (fichier `LICENSE`). La WTFPL ne comportant **aucune clause d'exclusion de
  garantie** — contrairement à MIT, BSD ou Apache-2.0 — un fichier `NOTICE` distinct porte une clause de
  non-garantie et de non-responsabilité, reprise dans le `README` et dans la sortie de `ogs version
  --license`. Pour un outil s'exécutant en root et capable de supprimer des données, laisser ce point
  vide serait une exposition inutile ; la permissivité voulue est intégralement préservée.
- Versionnage sémantique. Le champ `apiVersion` du YAML permet des migrations de schéma : `ogs` migre
  automatiquement les fichiers d'une version antérieure, avec sauvegarde préalable.

---

## 18. Lots de livraison

| Lot | Contenu | Critère d'acceptation |
|---|---|---|
| **L0 — Socle** | `exec`, `fs` (écriture atomique, empreintes), `store` + migrations, `secret`, `config`, `model` + `spec` + `validate` (dérivation du nommage, troncature, unicité), `template`, `audit`, `engine` (plan/apply/diff/saga/verrou), CLI de base, `init`, `doctor` | `plan`/`apply` sur un projet ne contenant qu'un utilisateur et une arborescence ; idempotence et rollback démontrés par les tests |
| **L1 — Web + PHP** | provider `system` (users, groupes, `logs/`, `.bin/php`, clé de déploiement), `php` (détection 7.4→8.5, pools, presets, numérotation, switch), types d'application Prestashop et Symfony 3.4→8 avec détection de variante et garde-fou de compatibilité, `web` (interface, Apache, Nginx), rotation des logs, reloads groupés, validation avant activation | Un Prestashop en 7.4 et un Symfony 8 en 8.5 servis simultanément, `php switch` sans coupure ni réécriture d'unité, bascule incompatible refusée |
| **L2 — Bases** | providers `mysql` et `postgres`, privilèges nommés, dump/restore, `secrets env` | Création complète, rotation de mot de passe, connexion applicative vérifiée |
| **L3 — TLS** | provider `acme` (lego), HTTP-01, DNS-01, timer de renouvellement, hooks de reload, staging, auto-signé | Émission et renouvellement réels contre pebble, échec non bloquant vérifié |
| **L4 — RabbitMQ** | provider `amqp` (API HTTP + fallback), exchanges/queues/bindings/policies, préréglage Messenger, détection d'arguments immuables | Transport Messenger fonctionnel de bout en bout |
| **L5 — Automatisation** | provider `cron`, provider `unit` (templates, instances, target, scale, logs), délégation sudoers et `ogs my`, tmpfiles | 3 workers Messenger supervisés, scale à chaud, cron exécuté sous le bon utilisateur |
| **L6 — Composition** | `site create` (y compris `--env prep`) et sa saga complète, `site clone`, `project delete` avec archivage, `project restore`, `status` | Scénarios d'intégration 1, 7 et 13 validés |
| **L7 — TUI** | inspecteur, panneau de détail, actions contextuelles, suivi de logs, wizard de création | Création complète d'un projet depuis la TUI, parité avec la CLI |
| **L8 — Import** | scan multi-sources, corrélation, adoption interactive, ressources non gérées | Adoption d'un serveur préconfiguré sans régression |
| **L9 — Applications non-PHP** | types d'application, providers `runtime` (nvm, rbenv, pyenv, venv), templates de service Go/Node/Python/Ruby, vhosts de proxy, attente de disponibilité | Scénarios d'intégration 22 à 25 validés |
| **L10 — Industrialisation** | hooks de parc et de projet, complétions dynamiques bash/zsh/fish, 96 pages de manuel engendrées, paquet `.deb` (nfpm), CI GitHub Actions, sept scénarios d'intégration conteneurisés (serveur web, PHP, hooks, MariaDB, RabbitMQ, ACME, paquet), documentation d'exploitation | Paquet installable, documentation publiée |

### Ce que les essais sur système réel ont corrigé

Les scénarios conteneurisés du lot L10 ont mis au jour sept défauts qu'aucun test unitaire ne pouvait
voir, parce que tous supposaient un interlocuteur simulé :

| Défaut | Conséquence en exploitation |
|---|---|
| `/var/lib/ogs` en 0750 | le serveur web ne pouvait pas traverser : **tout challenge HTTP-01 échouait en 403** (D41) |
| Webroot du challenge mal aliasé | le jeton était écrit là où aucun des deux moteurs ne le cherchait : **404** (D42) |
| Rechargement du serveur web groupé en fin d'apply | le vhost n'était pas encore chargé quand l'autorité venait valider : **la première émission d'un site neuf échouait toujours**. Une étape peut désormais exiger que les rechargements en attente soient exécutés avant elle |
| TLS absent des templates `static`, `proxy` et `redirect` | un certificat était émis puis **jamais présenté** — sites statiques et toutes les applications non-PHP restaient en clair (D43) |
| Comparaison octet à octet incluant l'horodatage de l'en-tête | **tout fichier était déclaré en écart la seconde suivant son écriture**, et chaque apply le réécrivait en rechargeant le serveur |
| Motif de `GRANT` non déséchappé à la relecture | une base dont le nom contient un souligné — donc **toute base additionnelle** — n'était jamais reconnue conforme |
| `robots.txt` de préproduction aliasé vers un fichier jamais créé | 404 sur `/robots.txt`, donc **aucune protection contre l'indexation** |
| `--tls=false` sans effet, et dump dans le répertoire courant | un site voulu en HTTP consommait un quota ACME ; un dump de production pouvait atterrir à la racine du système |

Les lots L0 à L2 constituent le socle minimal utilisable ; L3 à L6 apportent la valeur cible ; L7 à L10
finalisent le produit. L9 (applications non-PHP) est le seul lot réellement détachable : le produit est
complet sans lui pour un parc exclusivement PHP.

---

## 19. Exigences non fonctionnelles

| Exigence | Cible |
|---|---|
| Démarrage à froid | < 100 ms pour une commande de lecture simple |
| `plan` sur 50 projets | < 3 s (observation parallélisée par provider) |
| `apply` d'un site complet | < 20 s hors émission ACME |
| Empreinte mémoire | < 60 Mio en usage CLI, < 120 Mio en TUI |
| Taille du binaire | < 40 Mio |
| Interruption de service | nulle pour toute opération sur un projet existant, hors reload (< 1 s) |
| Reprise après interruption | tout `apply` interrompu (SIGINT, coupure) laisse un état cohérent ou déclenche le rollback ; le verrou est libéré, la reprise est possible sans nettoyage manuel |
| Messages d'erreur | toujours : ce qui a échoué, pourquoi, quelle commande corrige |
| Langue | interface et documentation en français, identifiants et code en anglais |

---

## 20. Hypothèses et points à arbitrer

**Hypothèses retenues faute de précision** :

1. Les sauvegardes sont hors périmètre, mais l'archivage à la suppression réalise malgré tout un dump SQL
   ponctuel (via `mysqldump`/`pg_dump`) : sans lui, une archive serait inexploitable. Aucune planification
   ni rétention n'est fournie pour autant. Si tu préfères une archive strictement limitée aux fichiers de
   configuration, l'étape 5 du §13 disparaît et `--no-dump` devient le comportement par défaut.
2. Un seul moteur web est piloté par serveur ; les deux peuvent être installés, mais la topologie
   Nginx-frontal-vers-Apache-backend n'est pas gérée en v1.
3. Les paquets (Apache, Nginx, PHP, SGBD, RabbitMQ) sont installés et maintenus hors `ogs` ; `doctor`
   diagnostique sans jamais installer.
4. Le dépôt Sury est la source supposée du multi-PHP sur Debian/Ubuntu.
5. Un seul serveur RabbitMQ, joignable en local ou par DSN configuré.
6. Les sites Symfony suivent l'arborescence standard de leur version (`web/` en 3.4, `public/` ensuite) et
   embarquent un `composer.json` exploitable ; à défaut, la variante et la compatibilité PHP sont déduites
   de l'arborescence, avec avertissement.
7. `/home` est monté sans `noexec` : sinon les binaires du projet (composer, node) ne peuvent pas
   s'exécuter, ce qui n'empêche pas PHP de fonctionner mais casse les chaînes de build. `doctor` le
   signale.
8. La production et la préproduction d'un même site peuvent cohabiter sur le même serveur ; c'est le cas
   d'usage visé par `--env prep`.
9. Le domaine de préproduction (`preprod-pp.net`) est contrôlé par l'exploitant, son wildcard DNS est en
   place, et les credentials de sa zone sont fournis à `ogs` pour le challenge DNS-01 du wildcard TLS.
10. Un seul serveur de préproduction par domaine de préproduction (conséquence du wildcard DNS unique,
   D27). Un parc multi-serveurs impose un sous-domaine par machine.

**Points restant à arbitrer** :

- **Provider DNS de `preprod-pp.net`** : à préciser, car il conditionne le challenge DNS-01 du wildcard,
  qui n'est plus optionnel mais requis dès la première préproduction. Plus largement, lego propose plus
  de cent providers ; lesquels documenter et tester réellement ? (proposition : celui de `preprod-pp.net`
  en priorité, puis OVH, Cloudflare, Gandi — les autres restent utilisables sans garantie).
- **Seuil d'archivage** (défaut 5 Gio) et **durée de rétention** des archives : purge automatique ou
  jamais ?
- **Accès SFTP** : écarté en v1 (D15, docroot simple, shell `nologin`). À rouvrir si des tiers doivent
  déposer des fichiers. Le home en 0750 avec groupe privé s'y prête bien : un chroot SFTP exigerait en
  revanche que le répertoire de chroot appartienne à root, donc un niveau intermédiaire
  (`/home/shop.fr` root:root 0755 + `/home/shop.fr/app` en 0750), ce qui modifierait le layout.
- **Mode `webAccess: acl` par défaut ?** Le mode `group` est retenu pour la v1 ; si tu héberges des
  clients tiers non fiables, basculer le défaut sur `acl` restreint `www-data` au seul docroot.
- **Redis / Memcached / Elasticsearch** : hors périmètre v1, candidats naturels à un futur système de
  plugins (D19 n'ouvre pour l'instant que templates et hooks).
- **Versions de runtime par défaut** : quelles versions de Node, Ruby et Python proposer, et faut-il que
  `ogs` refuse une version en fin de support ?
- **Mise à jour des runtimes** : conséquence directe de D35, elle est à faire projet par projet. Une
  commande `runtime upgrade --all --to <version>` serait-elle utile, avec redémarrage échelonné des
  services ?
