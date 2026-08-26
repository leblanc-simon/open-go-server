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
survivent. Seul le timer de renouvellement est arrêté.

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
validé à chaque lecture ; une valeur inconnue est refusée plutôt qu'ignorée.

Les identifiants d'administration ne s'y écrivent pas en clair :

```sh
ogs db credentials set mysql        # demande le mot de passe et le chiffre
ogs amqp credentials set
ogs cert dns-provider set ovh
```

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

Les migrations de schéma de la base d'état sont appliquées au premier lancement. Les déclarations de projet
sont lues telles quelles : une version plus récente d'`ogs` n'exige jamais de réécrire les fichiers
existants. Après une mise à jour, `ogs diff` sur l'ensemble du parc indique si les templates livrés ont
évolué — et donc quels fichiers un `apply` réécrirait.

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

Deux règles gouvernent la commande : elle **ne desserre jamais** — un répertoire déjà plus strict que la
cible est laissé tel quel — et elle **ne suit aucun lien symbolique**. Un chemin absent signifie un service
non installé : il n'y a rien à faire.

| Chemin | Cible | Pourquoi |
|---|---|---|
| `/etc/ogs` | `0700 root:root`, fichiers `0600` | clé maître, déclarations de projets, identifiants |
| `/etc/letsencrypt` | `0700 root:root`, fichiers `0600`, récursif | clés privées |
| `/etc/apache2`, `/etc/nginx` | `0750 root:root` | vhosts : domaines, chemins, authentifications |
| `/etc/php` | `0755` — **inchangé** | le PHP en ligne de commande, exécuté sous l'identité du projet par les crons, y lit son `php.ini` |
| `/etc/mysql` | `0750 root:root` | `debian.cnf` porte les identifiants de maintenance |
| `/etc/postgresql` | `0750 root:postgres` | `pg_hba.conf` décrit qui peut se connecter |
| `/etc/rabbitmq` | `0750 root:rabbitmq`, fichiers `0640` | cookie Erlang, mot de passe d'administration |
| `/etc/telegraf` | `0750 root:telegraf`, fichiers `0640` | jetons d'écriture des métriques |
| `/etc/redis` | `0750 root:redis`, fichiers `0640` | `requirepass` en clair |
| `/etc/ssl/private` | `0710 root:ssl-cert` | convention Debian ; les services concernés sont dans le groupe |
| `/etc/cron.d` | `0750 root:root` | révèle les commandes et les identités de chaque projet |
| `/etc/logrotate.d` | `0750 root:root` | révèle l'emplacement des journaux du parc |
| `/etc/sudoers.d` | `0750 root:root` | révèle les délégations accordées |
| `/home` (`naming.homeRoot`) | `0711 root:root` | la racine des homes énumère le parc : chaque voisin, et le chemin de son code |

Le groupe est celui du service quand le démon lit sa configuration sous sa propre identité — RabbitMQ,
Telegraf, Redis, PostgreSQL. Pour Apache et Nginx, le processus maître lit en root et les workers héritent
des descripteurs déjà ouverts : `root:root` suffit.

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
2. `/etc/ogs/projects/` — les déclarations, qui suffisent à reconstruire le serveur ;
3. `/var/lib/ogs/state.db` — l'historique et les empreintes ; utile, non indispensable.

Les données des sites (homes, bases) relèvent de la politique de sauvegarde habituelle. `ogs db dump` et les
archives produites par `ogs project delete` ne s'y substituent pas.
