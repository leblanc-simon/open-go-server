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
