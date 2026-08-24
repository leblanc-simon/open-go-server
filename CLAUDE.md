# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Langue

Le projet est **entièrement en français** : code, commentaires, messages d'erreur, documentation, noms de
tests, sorties CLI. S'y conformer, accents compris. Les identifiants techniques (noms de types Go, clés
YAML, directives de configuration) restent dans leur forme d'origine.

Les commits ne mentionnent jamais l'assistance d'un outil d'IA.

## La référence

[`docs/CAHIER-DES-CHARGES.md`](docs/CAHIER-DES-CHARGES.md) fait autorité : 50 décisions d'architecture
arbitrées (D1–D50, tableau §2), spécification par module, plan de tests, découpage en lots. **Consulter le
tableau des décisions avant de proposer une architecture** — la plupart des questions « pourquoi pas
plutôt… ? » y sont déjà tranchées, avec leur raison. Toute décision nouvelle s'y ajoute.

## Commandes

```sh
make build            # ./ogs, statique (CGO_ENABLED=0), aucune dépendance C
make test             # tests unitaires
make check            # fmt + vet + test
make cover            # couverture (62,5 % actuellement)
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

Dix scénarios conteneurisés éprouvent ce que les tests unitaires ne peuvent pas voir. Ils ont mis au jour
vingt-et-un défauts invisibles autrement (§18 du cahier des charges) : **y recourir avant de déclarer un
provider fonctionnel**.

```sh
make integration-all       # les dix, à la suite
make integration-nginx     # un seul (voir SCENARIOS dans le Makefile)
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
  `choisir_moteur apache|nginx` (`scenarios/lib.sh`) après `ogs init`.

## Architecture

### Le modèle : état désiré, réconciliation

Un projet est décrit par un YAML dans `/etc/ogs/projects/<id>.yaml` (`internal/model`, chargé par
`internal/spec`). Chaque provider **observe** l'état réel, le **compare** au désiré et produit des
**étapes** ; le moteur (`internal/engine`) les exécute en saga — une étape critique en échec fait compenser
les précédentes en ordre inverse, une étape non critique produit un avertissement et l'exécution continue
(un certificat qui échoue ne doit pas détruire un site par ailleurs fonctionnel).

`plan`, `diff` et `apply` partagent le même calcul : `apply.go:buildPlan` assemble les providers dans un
ordre qui est une décision, non un détail — système → bases → AMQP → applicatif → PHP → web → TLS → bascule
TLS du vhost → PHP obsolète → cron → unités → sudoers. `buildGlobalPlan` porte ce qui vaut pour tout le
parc (logrotate, fragment nginx, tmpfiles, unités de renouvellement).

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

- **`/var/lib/ogs` en 0751**, pas 0750 : le serveur web doit traverser jusqu'au répertoire du challenge ACME.
- Le client ACME écrit sous **`<webroot>/.well-known/acme-challenge/`** : Apache aliase ce répertoire,
  nginx prend le webroot et y accole l'URI. Les deux directives n'ont pas la même sémantique.
- Les vhosts de proxy nginx emploient `$connection_upgrade`, fourni par le fragment géré
  `/etc/nginx/conf.d/ogs.conf`. Toute variable employée par un vhost doit être définie quelque part — un
  test unitaire le vérifie.
- `StartLimitBurst` et `StartLimitIntervalSec` appartiennent à **`[Unit]`** ; dans `[Service]`, systemd les
  ignore en silence.
- `ReadWritePaths` d'une unité est **facultatif** (préfixe `-`) : `var/` est créé par l'application, et un
  chemin absent fait échouer le service en 226/NAMESPACE avant toute exécution.
- Les étapes d'unité s'ordonnent par **cycle de vie**, pas par nom : `unit/instance` précède
  `unit/service` dans l'alphabet, ce qui ferait démarrer une instance avant l'écriture de son modèle.
- La cible d'un projet doit être liée à `multi-user.target`, sinon les services ne repartent pas après un
  redémarrage.
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
