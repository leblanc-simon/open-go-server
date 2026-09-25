# Guide d'utilisation

Ce guide décrit `ogs` du point de vue de l'exploitant : comment déclarer un site, l'appliquer et le faire
vivre. L'installation est traitée dans [`INSTALLATION.md`](INSTALLATION.md), les hooks dans
[`HOOKS.md`](HOOKS.md), les raisons des choix dans le [cahier des charges](CAHIER-DES-CHARGES.md).

## Le principe

Un projet est **un fichier YAML** dans `/etc/ogs/projects/<id>.yaml`. Il décrit ce que doit être le site :
son type d'application, ses domaines, ses bases, son PHP, ses tâches, ses services. `ogs` compare cette
description au système réel et calcule les étapes qui l'y amènent.

Tout ce qui n'est pas écrit dans le fichier est **déduit** : l'utilisateur Linux, le home, le docroot, le pool
PHP, le vhost, le certificat. Un fichier de dix lignes suffit à un site complet ; `ogs project export <id>`
montre la forme développée.

Chaque projet reçoit un **identifiant** dérivé de son host (`shop.fr` → `shopfr`), qui nomme tout ce qui lui
appartient : utilisateur et groupe, base, pool PHP, vhost, unités systemd, fichier cron.

## Le cycle de travail

```sh
sudo ogs init                  # une fois par machine : /etc/ogs, base d'état, clé maître
sudo ogs doctor                # prérequis, droits, cohérence du parc

sudo ogs project create shop.fr --kind php-symfony   # écrit la déclaration, n'applique rien
sudo ogs plan shopfr           # ce qui changerait
sudo ogs apply shopfr          # l'applique, après confirmation
sudo ogs diff shopfr           # l'écart restant : « conforme » après un apply réussi
sudo ogs status                # vue d'ensemble du parc
```

`ogs site create` fait les trois premiers temps d'un coup : déclaration, plan, application. Le fichier du
projet reste ensuite la référence : on l'édite à la main (ou par les commandes `ogs cron add`,
`ogs service add`…), puis on relance `ogs apply`. Rejouer un `apply` sans rien changer ne fait rien.

Une étape **critique** en échec (compte, pool, vhost) défait les précédentes. Une étape **non critique**
(base injoignable, certificat, démarrage d'un service) laisse le site en place avec un avertissement et le
code de retour 6.

> Pour essayer sans risque, tout fonctionne sans root avec `--prefix ./dev` : les chemins système sont
> décalés sous `./dev` et les commandes système simulées. `make dev-demo` en donne un exemple.

## Types d'application

| `--kind`         | Docroot            | Servi par                                          |
|------------------|--------------------|----------------------------------------------------|
| `static`         | `www`              | le serveur web seul                                |
| `php-plain`      | `www`              | PHP-FPM                                            |
| `php-prestashop` | `www`              | PHP-FPM, réglages PHP adaptés, `.htaccess` actifs  |
| `php-symfony`    | `www/public`       | PHP-FPM (`www/web` pour Symfony 3.4)               |
| `node`, `python`, `ruby`, `go` | —    | un service supervisé, derrière un reverse-proxy   |
| `custom`         | —                  | la commande de votre choix, supervisée de même    |

Le code applicatif se dépose dans `/home/<host>/www`. Les journaux sont sous
`/var/log/ogs-projects/<id>/`, accessibles depuis le home par le lien `~/logs`.

Le moteur web (Apache ou nginx) est un choix **de machine**, non de projet : `ogs` emploie celui qui est
installé, et `web.engine` dans `/etc/ogs/config.yaml` tranche si les deux le sont.

## Exemples, du plus simple au plus complet

### 1. Un site statique

```sh
sudo ogs site create vitrine.fr --kind static
```

Résultat : un utilisateur `vitrinefr`, un home `/home/vitrine.fr` et son `www/`, un vhost, un certificat
Let's Encrypt et la redirection HTTP → HTTPS. La déclaration écrite tient en quelques lignes :

```yaml
apiVersion: ogs/v1
kind: Project
metadata:
  name: vitrinefr
  host: vitrine.fr
spec:
  app:
    kind: static
```

`--tls=false` s'en passe (site interne, essai) ; `--self-signed` pose un certificat auto-signé.

### 2. Un site PHP avec une base

```sh
sudo ogs site create blog.fr --kind php-plain --php 8.3 --db mysql
sudo ogs secrets env blogfr          # DATABASE_URL, prête à coller dans la configuration
```

S'ajoutent un pool PHP-FPM dédié qui tourne sous l'identité du projet, le lien `~/.local/bin/php` vers
l'interpréteur choisi, une base `blogfr` et son utilisateur. Le mot de passe est généré une fois, scellé
dans le keystore, et n'est plus jamais changé sans `ogs db user rotate`.

Changer de version de PHP plus tard :

```sh
sudo ogs php switch blogfr 8.4       # refusé si le composer.json ne l'accepte pas
```

### 3. Un Symfony avec alias, redirection, Messenger et tâche planifiée

```sh
sudo ogs site create shop.fr \
  --kind php-symfony --app-version 7.2 --php 8.4 \
  --alias www.shop.fr --redirect ancien-shop.fr \
  --db mysql --messenger async \
  --service symfony-messenger:async:3 \
  --cron "* * * * *:bin/console schedule:run"
```

- `www.shop.fr` est servi par le même vhost et couvert par le même certificat ;
- `ancien-shop.fr` reçoit un vhost de redirection permanente vers `shop.fr` ;
- `--messenger async` crée dans RabbitMQ un vhost, un utilisateur, l'exchange, la file `async` et sa file
  d'échec ; `ogs secrets env shopfr` donne alors aussi `MESSENGER_TRANSPORT_DSN` ;
- `--service` démarre trois consommateurs de la file (voir [Les services](#les-services)) ;
- `--cron` pose une tâche chaque minute, sous l'utilisateur du projet, enveloppée dans `flock` pour ne
  jamais se chevaucher, journalisée dans `~/logs/`.

### 4. Une application Node

```sh
sudo ogs site create api.fr --kind node --runtime 22.14.0
```

Node est installé **pour ce projet** (par nvm, dans son home), l'application est lancée par une unité
systemd et le vhost devient un reverse-proxy. L'application écoute sur le socket Unix dont le chemin lui est
transmis par `SOCKET_PATH` — l'accès en est ainsi réservé au serveur web — et doit répondre avant que le
vhost ne soit écrit, pour ne jamais exposer une 502. Même logique pour `python`, `ruby` et `go` (un binaire
Go écoute sur un port local, borné à `127.0.0.1`).

```yaml
spec:
  app:
    kind: node
    runtime: 22.14.0
    entrypoint: server.js
    instances: 2
    env:
      NODE_ENV: production
    health:
      path: /healthz
      timeout: 30s
```

### 5. Un service quelconque

Quand aucun type ne convient — un binaire Rust, un serveur Java, un outil livré tout compilé —, `custom`
supervise la commande que vous écrivez, sans installer de runtime ni poser de lien :

```sh
sudo ogs site create outil.fr --kind custom --env-file \
    --command '/home/outil.fr/www/bin/serveur --listen ${LISTEN_ADDR}'
```

```yaml
spec:
  app:
    kind: custom
    command: /home/outil.fr/www/bin/serveur --listen ${LISTEN_ADDR}
    envFile: true            # branche /etc/ogs/env/outilfr.env
    instances: 1
    env:
      RUST_LOG: info
```

- **L'écoute** est un port local par défaut (`127.0.0.1:25xxx`) : ogs ne sait rien de la commande et ne
  peut pas compter sur elle pour lire `SOCKET_PATH`. `listen: socket` reste possible si elle le fait.
  L'adresse est transmise par `PORT`, `HOST` et `LISTEN_ADDR` (ou `SOCKET_PATH`), que systemd développe
  dans la commande — `${PORT}`, `${LISTEN_ADDR}`. Un `%` y est un spécificateur systemd (`%i` : numéro
  d'instance) ; un `%` littéral s'écrit `%%`.
- **La commande** commence par un chemin absolu, sur une seule ligne. `runtime` et `entrypoint` sont
  refusés : tout tient dans `command`.
- **Le fichier d'environnement** vit dans `/etc/ogs/env/<id>.env`, et nulle part ailleurs : systemd le lit
  en root avant de lancer le service, et un fichier placé dans le home pourrait être remplacé par un lien
  vers les secrets d'un voisin. ogs le crée vide en `0600 root:root` et **ne le réécrit jamais** ; une
  modification prend effet au redémarrage (`sudo ogs service restart outilfr`). Ses valeurs l'emportent
  sur celles de `env` — y compris sur `PORT` : ne l'y redéfinissez pas. Il est archivé chiffré et supprimé
  avec le projet. `envFile` vaut aussi pour `node`, `python`, `ruby` et `go`.
- Un démon **sans HTTP** (consommateur de file, worker) ne relève pas de ce type, qui est toujours exposé
  par le vhost : déclarez-le dans `spec.services`.

### 6. Un projet complet, écrit à la main

Pour un projet riche, le plus lisible est d'écrire le fichier directement, puis de le vérifier avant de le
déposer :

```sh
sudo ogs plan -f boutique.yaml       # calcule le plan sans rien écrire
sudo cp boutique.yaml /etc/ogs/projects/boutiquefr.yaml
sudo ogs amqp messenger boutiquefr --transport async --transport mails
sudo ogs apply boutiquefr
```

```yaml
apiVersion: ogs/v1
kind: Project
metadata:
  name: boutiquefr
  host: boutique.fr
  description: Boutique Symfony, statistiques et workers
spec:
  app:
    kind: php-symfony
    version: "7.2"

  domains:
    - name: boutique.fr
    - name: www.boutique.fr
      role: alias
    - name: ancienne-boutique.fr
      role: redirect
      target: boutique.fr

  php:
    pools:
      - name: web
        version: "8.4"
        preset: large                    # micro, small, medium, large ou custom
        settings:
          php_admin_value[upload_max_filesize]: 64M

  databases:
    - engine: mysql
      name: boutiquefr
      user: boutiquefr
    - engine: postgres
      name: boutiquefr_stats
      user: boutiquefr_stats
      extensions: [pg_trgm]

  # Le bloc amqp (exchanges, files, bindings) est complété par « ogs amqp messenger ».
  amqp:
    vhost: boutiquefr
    user: boutiquefr

  crons:
    - name: planificateur
      schedule: "* * * * *"
      command: bin/console schedule:run
    - name: purge-nocturne
      schedule: "30 3 * * *"
      command: bin/console app:purge --no-interaction

  services:
    - name: messenger-async
      template: symfony-messenger
      instances: 3
      params:
        transport: async
        memoryLimit: 256M
    - name: messenger-mails
      template: symfony-messenger
      params:
        transport: mails
    - name: export
      template: worker
      params:
        command: /home/boutique.fr/.local/bin/php bin/console app:export --watch

  mail:
    dkim:
      enabled: true                      # clé DKIM par domaine, à publier dans le DNS

  sudo:
    verbs: [start, stop, restart, status]  # « sudo ogs my restart » depuis le compte du projet
```

Ce qui est absent reste déduit : utilisateur, home, docroot `www/public`, vhosts principal et de
redirection, certificat couvrant `boutique.fr` et `www.boutique.fr`.

### 7. Une préproduction

```sh
sudo ogs site clone shopfr --to prep          # copie aussi la base ; --with-data=false sinon
```

La préproduction reçoit l'identifiant `shopfrprep`, un host sous le domaine des préproductions
(`shop.<domaine>`, le domaine étant `environments.prep.domain` de `/etc/ogs/config.yaml`), son propre
utilisateur, ses propres bases et un pool PHP plus modeste. Elle est servie par le certificat wildcard partagé des
préproductions : aucun appel à l'autorité de certification.

## Les services

Un service est un processus long — consommateur de file, worker — que systemd lance, supervise et relance
sous l'identité du projet. Chaque service est une **unité templatisée** : `ogs-<id>-<service>@.service`,
dont on démarre une ou plusieurs instances (`@1`, `@2`…). Toutes les unités d'un projet sont rattachées à
une cible `ogs-<id>.target`, activée au démarrage de la machine : les services repartent seuls après un
redémarrage.

Trois templates :

| Template            | Lance                                                    | Paramètre principal   |
|---------------------|----------------------------------------------------------|-----------------------|
| `symfony-messenger` | `bin/console messenger:consume <transport>` (Symfony ≥ 4.1) | `transport`        |
| `rabbitmq-consumer` | `bin/console rabbitmq:consumer <transport>` (Symfony 3.4) | `transport`          |
| `worker`            | une commande quelconque                                  | `command`             |

Les consommateurs Symfony s'arrêtent d'eux-mêmes au bout de `limit` messages (500), `timeLimit` secondes
(3600) ou `memoryLimit` de mémoire (128M), et systemd les relance aussitôt : c'est ce qui évite les fuites de
mémoire d'un processus PHP qui tourne des jours. Autres paramètres : `workdir`, `restartSec`, `stopTimeout`,
et `env.<VARIABLE>` pour passer une variable d'environnement.

**Un service :**

```sh
sudo ogs service add shopfr messenger-async --transport async --instances 2
sudo ogs apply shopfr
```

**Plusieurs services** se déclarent l'un après l'autre, chacun avec son nombre d'instances :

```sh
sudo ogs service add shopfr messenger-async --transport async --instances 3
sudo ogs service add shopfr messenger-mails --transport mails
sudo ogs service add shopfr export --template worker \
  --command "/home/shop.fr/.local/bin/php bin/console app:export --watch"
sudo ogs apply shopfr
```

```
PROJET  SERVICE          TEMPLATE           DÉCLARÉES  ACTIVES
shopfr  messenger-async  symfony-messenger  3          3
shopfr  messenger-mails  symfony-messenger  1          1
shopfr  export           worker             1          1
```

L'`apply` écrit les unités, active la cible et démarre les instances **avant** d'écrire le vhost, de sorte
que le site n'est jamais exposé avec une application qui n'a pas encore démarré. Une unité modifiée
(nouvelle limite mémoire, nouvelle commande) est redémarrée d'elle-même.

**Au quotidien :**

```sh
sudo ogs service list shopfr
sudo ogs service restart shopfr                  # tous les services du projet (start, stop aussi)
sudo ogs service scale shopfr messenger-async 5  # immédiat, sans toucher à la déclaration
sudo ogs service logs shopfr messenger-async -f
sudo ogs service remove shopfr export
```

`scale` agit tout de suite mais ne modifie pas le fichier : le prochain `apply` rétablit le nombre déclaré.
Pour un changement durable, modifier `instances` dans le YAML.

Depuis le compte du projet (après un déploiement, par exemple), l'utilisateur pilote ses propres services
sans être root, par une règle sudo restreinte à ces seules commandes :

```sh
sudo ogs my restart        # start, stop, restart, status
```

Les unités sont durcies : `/home` y est en lecture seule, hormis `www/var` du projet et son répertoire
d'exécution `/run/ogs/<id>`. Un worker qui doit écrire ailleurs échouera en « Read-only file system » —
écrire sous `www/var`.

## Migrer depuis un ancien serveur

Deux situations, deux chemins :

- les sites **restent sur la machine** et `ogs` doit les prendre en main : `ogs import scan`, puis
  `ogs import adopt <id>`. Un projet adopté garde ses noms réels (compte, bases, pool) ;
- les sites **changent de machine** : c'est une migration. Elle est décrite par un manifeste,
  rempli d'après le modèle [`migration.yaml`](migration.yaml), et exécutée par [`migrer.sh`](migrer.sh),
  sur la **nouvelle** machine et en root. La nouvelle machine tire fichiers et bases de l'ancienne par ssh : l'ancienne n'a besoin
  d'aucun accès à la nouvelle.

### Le manifeste

Copier le modèle **hors du dépôt** avant de le remplir — rempli, il porte les adresses des serveurs et les
noms des clients : `migrer.sh` exige donc son chemin (`-m`). Un bloc par site : où sont son code et ses données sur l'ancien serveur, ses bases, ses files RabbitMQ, ses
tâches et workers à arrêter, les fichiers de configuration à réécrire, la mise en maintenance, les
vérifications à rejouer — et sa **déclaration ogs complète**, déposée telle quelle dans
`/etc/ogs/projects/`. Les consignes de remplissage sont en tête du fichier.

Trois règles :

- **aucun secret** dans le manifeste. Les bases de l'ancien serveur sont lues en root par son socket local ;
  celles du nouveau sont créées par `ogs`, qui en génère les mots de passe. Une valeur `@ogs:DATABASE_URL`
  (ou `@ogs:DATABASE_URL#password`, `#name`, `#user`…) est remplacée par ce que rend `ogs secrets env` ;
- une valeur inconnue s'écrit `~ # À VÉRIFIER` : le script refuse le projet tant qu'il en reste une ;
- `ogs import scan -o yaml`, lancé sur l'**ancien** serveur (le binaire est statique), inventorie vhosts,
  pools, comptes, bases et crons sans rien modifier : un bon point de départ pour remplir chaque bloc.

### Le déroulé

```sh
M=/root/migration.yaml                     # la copie remplie du modèle
docs/migrer.sh -m $M controle vague:1                 # manifeste complet et cohérent
docs/migrer.sh -m $M prealables tous                  # outils, doctor, accès ssh, bases, TTL
docs/migrer.sh -m $M --executer declarer vague:1      # compte, pool, bases vides, vhost
docs/migrer.sh -m $M --executer synchroniser vague:1  # fichiers, bases, configuration — répétable
docs/migrer.sh -m $M verifier vague:1                 # vérifications, forcées vers la nouvelle IP
docs/migrer.sh -m $M --executer basculer vague:1      # dans la fenêtre de maintenance
docs/migrer.sh -m $M etat
```

**Sans `--executer`, rien n'est modifié** : chaque commande qui changerait quelque chose est affichée, et
seules les lectures sont faites. Les sites sont groupés par **vague**, une vague partageant une fenêtre de
maintenance ; commencer par une vague d'un petit site sans enjeu.

- **Déclarer** dépose la déclaration **crons et services désactivés** : une tâche ne doit jamais tourner sur
  les deux serveurs à la fois. Le certificat échoue tant que le DNS désigne l'ancien serveur — code 6,
  attendu. Une zone chez OVH peut l'obtenir d'avance par `challenge: dns-01`.
- **Synchroniser** ne coupe rien : l'ancien serveur continue de servir. La rejouer la veille de la bascule
  réduit la dernière copie à presque rien.
- **Basculer** met l'ancien site en maintenance, attend que ses files RabbitMQ soient vides, arrête ses
  tâches et workers, fait la dernière copie, puis vérifie le nouveau site **avant** de toucher au DNS : en
  cas d'échec, l'ancien serveur est rétabli et les visiteurs n'ont rien vu. Le script attend ensuite que le
  DNS désigne la nouvelle machine — le changement d'enregistrement reste manuel —, réactive crons et
  services, et `ogs apply` émet le certificat.
- **Retour** (`migrer.sh -m $M --executer retour <id>`) désactive le projet sur la nouvelle machine et rétablit
  tâches, workers et site sur l'ancienne. Les données écrites sur la nouvelle machine depuis la bascule ne
  sont **pas** reportées.

Ce que le script ne fait pas : modifier le DNS, corriger les chemins absolus écrits en dur dans le code ou
la base (`chemins_en_dur`, listés par `controle`), et publier la nouvelle clé DKIM — `ogs` en génère une
neuve, à publier par `ogs mail dkim records <id>` **avant** la bascule, sous un sélecteur différent de
l'ancien. Abaisser le TTL des zones 48 h à l'avance, et prévenir les tiers qui filtrent sur l'adresse de
l'ancien serveur (`dependances_externes`).

## Autres opérations courantes

```sh
sudo ogs cert list                          # certificats et échéances (renouvellement automatique)
sudo ogs db dump shopfr                     # sauvegarde de la base
sudo ogs cron run shopfr planificateur      # exécuter une tâche tout de suite
sudo ogs project delete shopfr              # archive complète, puis suppression
sudo ogs project restore /var/lib/ogs/archives/shopfr-20260823-143000.tar.zst
sudo ogs import scan                        # reprendre un serveur existant, en lecture seule
sudo ogs                                    # interface en mode texte
```

Toutes les commandes acceptent `-o json` ou `-o yaml`, et `man ogs-<commande>` détaille chacune.

## Codes de retour

| Code | Sens                                     | Code | Sens                          |
|------|------------------------------------------|------|-------------------------------|
| 0    | succès                                   | 5    | dépendance absente            |
| 1    | erreur                                   | 6    | succès partiel, ou écart détecté |
| 2    | usage                                    | 7    | verrou indisponible           |
| 3    | validation                               | 8    | privilèges insuffisants       |
| 4    | conflit                                  | 9    | annulé                        |
