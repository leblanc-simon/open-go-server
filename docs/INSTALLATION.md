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

## Mise à jour

```sh
apt install ./ogs_1.1.0_amd64.deb
```

Les migrations de schéma de la base d'état sont appliquées au premier lancement. Les déclarations de projet
sont lues telles quelles : une version plus récente d'`ogs` n'exige jamais de réécrire les fichiers
existants. Après une mise à jour, `ogs diff` sur l'ensemble du parc indique si les templates livrés ont
évolué — et donc quels fichiers un `apply` réécrirait.

## Sauvegarde

Trois choses, par ordre d'importance :

1. `/etc/ogs/master.key` — irremplaçable ;
2. `/etc/ogs/projects/` — les déclarations, qui suffisent à reconstruire le serveur ;
3. `/var/lib/ogs/state.db` — l'historique et les empreintes ; utile, non indispensable.

Les données des sites (homes, bases) relèvent de la politique de sauvegarde habituelle. `ogs db dump` et les
archives produites par `ogs project delete` ne s'y substituent pas.
