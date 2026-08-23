# ogs

Utilitaire CLI (et à terme TUI) d'administration de serveurs web mutualisés, écrit en Go.

`ogs` industrialise le cycle de vie d'un site hébergé : utilisateur système, arborescence, bases MySQL /
PostgreSQL, vhost et files RabbitMQ, pool PHP-FPM, vhost Apache ou Nginx, certificat TLS, tâches planifiées
et services systemd. Un projet est décrit par un fichier YAML ; un moteur de réconciliation idempotent amène
le serveur dans l'état décrit.

Le cahier des charges technique complet — 39 décisions d'architecture arbitrées, spécification par module,
plan de tests, découpage en lots — est dans [`docs/CAHIER-DES-CHARGES.md`](docs/CAHIER-DES-CHARGES.md).
C'est la référence : toute contribution s'y adosse.

## État d'avancement

| Lot | Contenu | État |
|---|---|---|
| **L0** | Socle : nommage, modèle, validation, YAML, écriture atomique, base d'état, keystore, audit, moteur (plan/apply/diff/saga/verrou), provider système, CLI | **livré** |
| L1 | Web + PHP : pools multi-version 7.4→8.5, vhosts Apache et Nginx, types Prestashop et Symfony, rotation des logs | à venir |
| L2 | Bases MySQL/MariaDB et PostgreSQL | à venir |
| L3 | TLS : ACME embarqué, HTTP-01 et DNS-01 (OVH), wildcard partagé | à venir |
| L4 | RabbitMQ : vhosts, queues, bindings, préréglage Messenger | à venir |
| L5 | Crons, services systemd templatisés, délégation `ogs my` | à venir |
| L6 | Composition : `site create`, archivage, restauration, `status` | à venir |
| L7 | TUI (Bubble Tea) | à venir |
| L8 | Import d'un serveur existant | à venir |
| L9 | Applications Go, Node, Ruby, Python | à venir |
| L10 | Industrialisation : hooks, paquet .deb, CI, documentation | à venir |

## Compilation

Aucune dépendance C : le binaire est statique et se déploie seul.

```sh
make build        # produit ./ogs
make test         # tests unitaires et d'intégration
make cover        # couverture
make all          # fmt, vet, test, build
```

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

## Conventions structurantes

- **Un identifiant unique par projet**, dérivé du host (`shop.fr` → `shopfr`), qui nomme l'utilisateur
  Linux, son groupe, les bases, le vhost RabbitMQ, le pool PHP, le vhost web, les unités systemd et le cron.
- **Home** en `/home/<host effectif>`, en 0750 ; le code applicatif dans `www/`, le docroot dépendant du
  type d'application (`www` pour Prestashop, `www/public` pour Symfony 4+, `www/web` pour Symfony 3.4).
- **`logs/` et `.bin/` appartiennent à root** : l'utilisateur y lit mais n'y crée rien. Cela interdit de
  substituer un fichier de log par un lien symbolique et d'obtenir une écriture root arbitraire.
- **Isolation** : chaque projet a son groupe privé, et c'est `www-data` qui en devient membre secondaire —
  jamais l'inverse, qui rendrait chaque site lisible par tous les autres.
- **Préproduction** : `--env prep` produit un projet distinct sur un domaine dédié
  (`<identifiant>.preprod-pp.net`), sans partager la moindre ressource avec la production.

## Codes de retour

| Code | Signification | | Code | Signification |
|---|---|---|---|---|
| 0 | succès | | 5 | dépendance absente |
| 1 | erreur d'exécution | | 6 | succès partiel, ou écart détecté par `diff` |
| 2 | erreur d'usage | | 7 | verrou indisponible |
| 3 | échec de validation | | 8 | privilèges insuffisants |
| 4 | conflit | | 9 | opération annulée |

## Licence

WTFPL — voir [`LICENSE`](LICENSE). La WTFPL ne comportant aucune clause d'exclusion de garantie, celle-ci
est portée par [`NOTICE`](NOTICE) : cet outil s'exécute en root et peut détruire des données, lisez cet
avertissement avant de l'employer en production.
