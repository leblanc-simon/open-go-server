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
- **Configuration** de fail2ban et de l'agent de métriques Telegraf, dérivée du parc (D64) : prisons, entrées
  et comptes de supervision. Ni l'un ni l'autre n'est installé par ogs.
- **Signature DKIM** du courrier sortant des projets (D69) : clés par domaine d'envoi, tables d'OpenDKIM
  dérivées du parc, enregistrement DNS à publier. Ni OpenDKIM ni le MTA ne sont installés par ogs.
- **Pare-feu ufw** (D72), décrit par `/etc/ogs/ufw.yaml` : profils d'application dérivés des services
  installés, accès par profil, sources nommées, adresses de confiance, bannissements, politiques par
  défaut. ufw n'est pas installé par ogs, et sans `ufw.yaml` ogs n'y touche pas.

### 1.3 Hors périmètre

- Sauvegardes planifiées, politiques de rétention, restauration point-in-time. Seules des commandes de
  `dump`/`restore` ponctuelles existent, utilisées notamment par l'archivage à la suppression.
- Installation et mise à jour des paquets système (Apache, PHP, MySQL…) : ils sont supposés présents.
  `ogs doctor` se contente de diagnostiquer ce qui manque.
- DNS, messagerie, alerting. Le pare-feu et les métriques ne le sont plus tout à fait : ogs **écrit la
  configuration** de fail2ban et de Telegraf à partir du parc (D64), sans les installer, sans définir de
  destination de métriques (D65) et sans écrire le moindre filtre. Le pare-feu est entré à son tour
  (D72) : ogs pose les règles d'ufw que décrit `/etc/ogs/ufw.yaml`, et celles-là seulement — ni iptables
  ou nftables en direct, ni autre pare-feu qu'ufw. Rien d'autre — ni seuils d'alerte, ni tableaux de bord. La messagerie ne l'est plus tout à fait non
  plus : ogs **signe** le courrier des projets qui le demandent (D69), mais ne configure ni le MTA, ni ses
  relais, ni `opendkim.conf`, et n'écrit ni SPF ni DMARC. Le DNS reste hors périmètre : ogs émet
  l'enregistrement DKIM, l'exploitant le publie.
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
| D15 | Isolation | User dédié, ~~shell `nologin`~~ (**révisé par D62** : `/bin/bash`, réglable), **docroot simple**, home `/home/<host effectif>` en 0711 (D51) | Pas de mécanique `releases/` ; `/home` doit rester traversable |
| D16 | Échecs composites | **Rollback avec étapes optionnelles non bloquantes** | Chaque étape déclare sa criticité et sa compensation |
| D17 | Tests | Unitaires + **intégration en conteneurs systemd** | CI plus lourde mais garanties réelles |
| D18 | Binaire | **`ogs`**, statique, `.deb` via GoReleaser | CGO désactivé → SQLite en Go pur |
| D19 | Extensibilité | Templates surchargeables + **hooks** pre/post | Contrat de hook à documenter et figer |
| D20 | Adoption | **Import assisté** interactif, ressources non reconnues intactes | Notion de ressource « non gérée » dans le modèle |
| D21 | Accès | **Root strict** pour toute opération d'administration, avec une exception délimitée : `ogs my <verbe>` (D28) | Audit non nominatif (voir §14.3) |
| D22 | Sorties | `--output json\|yaml\|table`, codes de retour normalisés, complétion | Toute commande de lecture doit être sérialisable |
| D23 | Nommage | **Un identifiant unique** dérivé du host nomme user, groupe, bases, vhost AMQP, pool, vhost web, unités, cron | Contrainte de longueur commune à 32 car. ; suffixes de rôle pour les ressources additionnelles |
| D24 | Environnements | `prod` et `prep` ; la préproduction suffixe l'identifiant par **`prep`** (sans séparateur) et vit sur un domaine dédié : `<label de prod>.preprod-pp.net` (D58) | Réserve 4 car. sur les 32 disponibles ; le domaine est une clé de configuration globale |
| D25 | Accès du serveur web | **Groupe privé par projet**, `www-data` ajouté comme **membre secondaire** de chaque groupe | Isolation inter-sites préservée ; `.env` et `var/` restent hors de portée de `www-data`. Corollaire : le serveur web ne suit un lien du docroot que vers ce qui appartient à son auteur (D87) — sous Apache, une course subsiste, que seule une identité de service par projet fermerait (§20) |
| D26 | Certificats partagés | Un certificat peut être de **portée serveur** et référencé par `@<nom>` ; la préproduction utilise un wildcard `*.preprod-pp.net` unique | Impose DNS-01 et des credentials en écriture sur la zone ; supprime tout appel ACME à la création d'une préproduction |
| D27 | DNS | **Hors périmètre** : un wildcard `*.preprod-pp.net` est créé manuellement une fois ; `doctor` vérifie seulement la résolution | Un seul serveur de préproduction par domaine |
| D28 | Délégation | L'utilisateur du projet pilote ses services par **sudoers énuméré, sans métacaractère** ; `ogs my <verbe>` déduit le projet de **`SUDO_UID`** | Aucune cible à détourner ; fichier sudoers validé par `visudo -cf` avant activation ; dépend de `env_reset`, vérifié par `doctor` |
| D29 | Logs | Logs d'infrastructure dans **`/var/log/ogs-projects/<id>/`** (emplacement révisé par D71 ; `/home/<host>/logs/` à l'origine), répertoire **root** avec fichiers pré-créés ; rotation par un `/etc/logrotate.d/ogs` unique | Bloque l'escalade root par substitution de lien symbolique ; un seul reload par rotation |
| D30 | Interpréteur | Crons et unités invoquent **`/home/<host>/.local/bin/php`**, lien géré (emplacement révisé par D70) | Une bascule de version ne réécrit ni cron ni unité |
| D31 | Transport FPM | **Socket Unix par défaut**, numéroté selon la norme `1<version><incrément>` ; TCP possible mais explicite | Le numéro est un identifiant logique, pas un port ouvert |
| D32 | Layout | Code applicatif dans **`/home/<host>/www`** ; le docroot dépend du **type d'application** (`www` pour Prestashop, `www/public` pour Symfony) | Un type d'application devient une donnée de premier plan du modèle |
| D33 | Clé de déploiement | Paire **ed25519 sans passphrase** générée à la création, privée conservée sur le serveur, publique à déclarer en deploy key | Déploiement en **tirage** (le serveur clone) ; aucune clé privée à confier à un tiers. Le volet « aucune surface SSH entrante, `nologin` conservé » est **révisé par D62** : le shell est ouvert par défaut, ce qui ne change rien au sens du déploiement |
| D34 | Applications non-PHP | **Go, Node, Ruby, Python** supportés : processus supervisé par systemd + vhost en reverse-proxy | Élargit le périmètre à la supervision de runtimes applicatifs |
| D35 | Runtimes | `nvm`, `rbenv`, `pyenv`/`venv` installés **par utilisateur** dans son home, liens gérés dans `.local/bin/` (D70) | Une copie par site ; intégrité du runtime hors du contrôle de l'inventaire |
| D36 | Écoute des applications | **Socket Unix** `0660 www-data:<id>` quand le runtime le permet ; sinon port TCP `2<runtime><incrément>` sur **127.0.0.1 exclusivement** | Isolation équivalente à celle des pools PHP |
| D37 | Types livrés | **Prestashop** et **Symfony 3.4 → 8** (+ `static`, `php-plain` et les 4 runtimes) ; les autres frameworks s'ajoutent par `apptypes.yaml` sans code | Symfony 3.4 impose une variante distincte : docroot `web`, `app.php`, pas de Messenger |
| D38 | Compatibilité PHP | La contrainte `require.php` du **`composer.json` de l'application** fait autorité ; table indicative en secours seulement | Aucune matrice figée à maintenir ; `php switch` refuse une bascule incompatible sans `--force`. Le fichier est lu par descripteur, sans suivre de lien et borné à 1 Mio ; illisible ou refusé, c'est la table qui décide, annoncée comme telle (D82) |
| D39 | Versions PHP | **7.4 à 8.5**, y compris des versions en fin de support | `doctor` les signale sans jamais bloquer : un Prestashop ancien peut exiger 7.4 |
| D57 | Accès aux préproductions | **Aucune authentification** : elles sont accessibles par défaut, protégées seulement de l'indexation (`X-Robots-Tag`, `robots.txt`) | Une authentification à poser gêne le travail quotidien — recettes clients, tests d'intégration, webhooks de fournisseurs. La contrepartie est explicite : une préproduction est publique, donc sans donnée réelle |
| D58 | Host de préproduction | Le sous-domaine reprend le host canonique **privé de son extension** (`jardindeco.fr` → `jardindeco.preprod-pp.net`) ; un host **déjà posé** sur le domaine de préproduction est pris **tel quel**, et l'identifiant se dérive alors de son seul label | L'extension n'apporte rien à un nom qui vit sur un domaine dédié. Et le host construit est celui qu'on a sous les yeux : le saisir tel quel est le réflexe naturel, qui donnait `jardindecopreprodppnet.preprod-pp.net`, le home du même nom et l'utilisateur `jardindecopreprodppnetprep`. Deux contreparties assumées : deux hosts ne différant que par leur extension revendiquent le même sous-domaine — la collision est refusée par le contrôle d'unicité du host effectif, jamais silencieuse ; seul le **dernier** label est retiré, reconnaître `co.uk` ou `com.br` exigeant une liste des suffixes publics, hors périmètre |
| D59 | Unité du moteur web | ogs gère un complément d'unité `/etc/systemd/system/<moteur>.d/ogs.conf` qui **énumère** les répertoires de journaux des projets en `ReadWritePaths=-<chemin>` ; il est écrit **avant** les vhosts et déclenche un **redémarrage** du moteur, non un rechargement | Les distributions durcissent l'unité du serveur web — Ubuntu livre `apache2.service` avec `ProtectHome=read-only` — et un service ainsi protégé voit `/home` en lecture seule dans son seul espace de noms : le maître s'arrête en ouvrant le journal d'un vhost et emporte tous les sites, alors que `mount` affiche `rw` et que root écrit sans peine. `ReadWritePaths` remonte les chemins après application des protections, ce qui laisse le durcissement intact. Chaque répertoire est énuméré plutôt que la racine des homes : rien d'autre que les journaux n'a besoin de s'ouvrir. Le préfixe `-` est indispensable — un chemin absent fait échouer le service en 226/NAMESPACE — et le redémarrage l'est aussi : l'espace de noms est établi au démarrage, un `reload` ne le refait pas — **réduit par D71** à une seule entrée, `ReadWritePaths=-<logs.root>`, les journaux ayant quitté `/home` |
| D56 | Droits des répertoires de configuration | `ogs harden` **rapporte** par défaut et n'applique qu'avec `--apply` ; il **ne desserre jamais** et **ne suit aucun lien symbolique** ; `/etc/php` reste ouvert à dessein | Les utilisateurs de projet ont un compte local : sans restriction ils dressent la carte du parc. Mais fermer un répertoire qu'un démon lit sous sa propre identité l'interromprait : le groupe cible est celui du service (rabbitmq, telegraf, redis, postgres, opendkim — liste **révisée par D85**, qui y ajoute mysql, www-data et le propriétaire laissé au service qui réécrit sa configuration, et refuse une cible sans son groupe), et `/etc/php` reste lisible parce que le PHP en ligne de commande des crons y lit son `php.ini`. Un outil de sécurité qui desserrerait des droits déjà plus stricts serait une régression |
| D60 | Droits de la racine des homes | `ogs harden` ferme la racine des homes (`naming.homeRoot`) en **0711 root:root**, sans récursion ni `fileMode` | Un utilisateur de projet dresse la carte du parc d'un seul `ls /home` : le nom de chaque voisin, et le chemin de son code. La traversée reste ouverte à tous, donc rien ne change pour le serveur web — servir un fichier n'exige que la traversée des répertoires et la lecture du fichier (D51) — ni pour un projet qui atteint son propre home. Trois contreparties, toutes assumées : les globs non privilégiés (`ls /home`, `du -sh /home/*`) et la complétion cessent d'aboutir ; un agent de sauvegarde ou de supervision qui parcourrait `/home` sous une identité non root est à revoir ; et le gain est **partiel** — `/etc/passwd` est lisible par tous et énumère déjà les homes, ce qui fait de cette mesure une gêne, non une cloison. Le traitement n'est **jamais récursif** : sous la racine vivent les répertoires des projets, dont `model.defaultDirs` est le seul juge (D51), et un second juge ferait osciller le plan entre deux valeurs. La règle « ne desserre jamais » de D56 s'applique ici aussi, et elle mord : un `/home` en 0700 ne rend plus un seul fichier et reste pourtant en place — c'est `doctor` qui a la charge de le dire, aucune vérification de configuration ne le voyant. Une racine montée depuis le réseau (`/home` sur NFS) rend le verdict « refusé » plutôt qu'un échec de `chmod` : le mode y appartient au serveur, et un export en `root_squash` refuserait root lui-même |
| D51 | Droits de l'arborescence | Home en **0711**, répertoires du projet en **0710** | Le home se traverse sans s'énumérer : servir un fichier n'exige que la traversée du répertoire et la lecture du fichier. Personne — pas même le serveur web — ne peut lister le contenu, ce qui ôte tout moyen de se promener dans l'arborescence. Le bit de groupe du home est indispensable : Unix retient la première classe applicable sans repli, donc un home en 0701 refuserait la traversée à `www-data` **parce qu'il est membre du groupe** |
| D52 | Rétention des archives | **14 jours**, purgés par une étape du plan global ; zéro signifie « conserver indéfiniment » et doit être explicite | Une archive contient un dump de base, donc des données de clients : la garder sans terme n'est pas neutre. La purge est une étape annoncée, jamais un effet de bord |
| D53 | Fournisseur DNS | **OVH seul**, refusé à la déclaration comme à l'émission | La zone de préproduction y est hébergée. Un fournisseur non éprouvé donnerait une émission qui échoue au moment le moins opportun ; en ajouter un demande de le configurer et de l'éprouver |
| D54 | Versions de runtime | **Aucun refus** d'une version en fin de support | Même principe que pour PHP (D39) : `doctor` signale, il ne bloque pas. Un projet ancien peut exiger un runtime ancien |
| D55 | Mise à jour des runtimes | **Projet par projet**, aucune commande de parc | Conséquence assumée de D35 : une bascule globale imposerait un redémarrage échelonné de tous les services, dont le risque excède le confort |
| D50 | Réglages nginx de portée serveur | Un fragment géré **`/etc/nginx/conf.d/ogs.conf`**, écrit par le plan global, porte la correspondance `$http_upgrade → $connection_upgrade` | Les vhosts de proxy emploient cette variable, que nginx ne fournit pas : sans le fragment, **un seul site applicatif rendait toute la configuration invalide** et emportait tous les autres au rechargement suivant |
| D47 | Ordre des étapes d'unité | L'ordre suit le **cycle de vie**, non l'alphabet : cible, modèles d'unité, activation des instances ; en sens inverse pour un retrait | Trier par nom faisait démarrer chaque instance avant l'écriture de son modèle — systemd répondait « Unit not found » et aucun service ne démarrait au premier apply |
| D48 | Activation de la cible | La cible du projet est **liée à `multi-user.target`** | Sans ce lien, rien ne la réclame au démarrage de la machine : les services ne repartent pas après un redémarrage, alors que tout paraît en ordre tant qu'elle reste allumée |
| D49 | Chemins inscriptibles d'une unité | `ReadWritePaths` est **facultatif** (préfixe `-`) | `var/` est créé par l'application, non par ogs : un service déclaré avant le déploiement du code échouait en 226/NAMESPACE, avant même d'exécuter sa commande |
| D44 | Accès d'administration PostgreSQL | Le rôle d'administration porte **le nom de l'utilisateur système** (root en exploitation), non « postgres » ; `OGS_PG_ADMIN` permet d'en désigner un autre | L'authentification « peer » compare l'utilisateur du système : le rôle « postgres » est systématiquement refusé à root. Prérequis d'installation : `su - postgres -c 'createuser --superuser root'`, contrôlé par `ogs doctor` |
| D45 | Isolation des bases PostgreSQL | **`REVOKE CONNECT ... FROM PUBLIC`** sur chaque base, `GRANT CONNECT` au seul rôle du projet et à ses comptes additionnels | PostgreSQL accorde CONNECT à PUBLIC sur toute base nouvelle : sans révocation, le rôle d'un projet se connecte à la base d'un autre et y lit du catalogue. Les requêtes étant idempotentes, elles durcissent aussi les bases créées avant cette règle |
| D46 | Création de base PostgreSQL | **`TEMPLATE template0`** systématiquement | L'encodage étant toujours explicite, créer depuis `template1` échoue dès que celui-ci a un autre encodage — le cas de tout cluster initialisé sans locale |
| D41 | Droits de `/var/lib/ogs` | **0751** et non 0750 | Le serveur web doit traverser jusqu'à `acme-challenge` : en 0750 tout challenge HTTP-01 se voit refuser par un 403. Le bit d'exécution seul n'autorise pas l'énumération, et les contenus sensibles gardent leurs propres droits |
| D42 | Webroot du challenge | Le client ACME écrit dans `<webroot>/.well-known/acme-challenge/` : **Apache aliase ce répertoire, nginx prend le webroot** et y accole l'URI | Les deux directives n'ont pas la même sémantique ; une seule valeur de template menait fatalement l'un des deux moteurs à un 404 |
| D43 | TLS dans les templates | **Tous** les templates de vhost servent le HTTPS, non seulement ceux des sites PHP | Un site statique, une application derrière un proxy — donc tous les runtimes non-PHP — et un domaine redirigé ont autant besoin d'être chiffrés. Le bloc 443 n'est rendu que pour un nom que couvre le certificat présent (D79), et celui du projet couvre aussi ses redirections (D80) |
| D40 | Hooks | **Deux portées** : un fichier de `/etc/ogs/hooks` nommé d'après l'événement s'applique à tout le parc, `spec.hooks` le remplace pour un projet. Criticité déduite du préfixe : `pre` interrompt, `post` avertit | Un hook s'exécute en root : le script doit vivre sous `/etc/ogs/hooks`, appartenir à root et n'être inscriptible par personne d'autre, sans quoi il est refusé — c'est une escalade de privilèges autrement. Aucun hook en `--dry-run` : les commandes qui simulent s'arrêtent avant leurs hooks, les autres refusent l'option, et le `hook.Runner` lui-même n'exécute rien sous ce drapeau (D88) |
| D61 | Clés SSH d'administration | Si **`/opt/default_authorized_keys`** existe, `.ssh/authorized_keys` de chaque projet est un **lien symbolique** vers ce fichier, `lchown` à l'utilisateur du projet ; l'étape est **non critique**, elle n'existe pas sans le fichier commun, et un `authorized_keys` **régulier** n'est jamais remplacé | Les opérateurs qui interviennent sur les projets sont les mêmes pour tout le parc : un trousseau unique se met à jour en un endroit, là où une copie par projet exigerait de repasser sur chacun d'eux à chaque arrivée ou départ — et figerait le trousseau au jour de la création du site. Le fichier commun n'est **pas géré par ogs** : l'administrateur le pose et le tient à jour, ogs ne fait que le lier. Quatre points acquis de la mécanique d'OpenSSH : sshd **résout le lien** avant tout contrôle, donc ce sont les droits de la cible qui décident et le mode du lien — 0777 immuable sous Linux — n'a aucun effet, ce qui interdit d'en faire un critère de dérive ; sous `StrictModes`, un fichier qui n'appartient pas à root ou à l'utilisateur, ou inscriptible au-delà de son propriétaire, est ignoré **en silence** ; le fichier est lu sous l'identité de l'utilisateur du projet, donc il doit aussi **lui être lisible** — 0644 root:root ; et `lchown`, non `chown`, sans quoi le trousseau commun passerait à l'utilisateur du dernier projet appliqué. C'est `doctor` qui porte ces contrôles, aucun n'étant visible depuis le plan. Un `authorized_keys` régulier déjà présent — reprise d'un serveur existant, clé posée par l'utilisateur du projet — porte l'accès de quelqu'un : le remplacer par un lien le lui retirerait sans un mot. L'étape échoue donc sans rien détruire, ce qui rend un avertissement et le code 6, et l'exploitant tranche. Le shell des projets étant ouvert par défaut (D62), le trousseau est utile sans réglage supplémentaire ; un parc qui referme le shell garde le lien en place mais inerte, sauf accès restreint à SFTP. Dans les deux cas, poser le lien n'ouvre par lui-même aucune surface : c'est sshd qui décide, et rien ici ne touche à sa configuration |
| D62 | Shell des comptes de projet | **`/bin/bash`** par défaut, réglable pour tout le parc par **`user.shell`** dans `/etc/ogs/config.yaml` et projet par projet par `spec.user.shell` ; le shell déclaré est **réconcilié** (`usermod`), et un projet **adopté garde le sien** | Révise D15 et D33, qui fermaient le shell. La raison du renversement : un compte fermé rend inutilisables le trousseau de clés d'administration (D61), l'accès SFTP au docroot et tout dépannage à la main — trois usages quotidiens qu'il fallait rouvrir un projet à la fois. Ce que la décision ne change pas : ogs **ne touche pas à sshd**. Un shell ouvert n'ouvre par lui-même aucune connexion — il faut encore une clé autorisée et un sshd qui l'accepte — mais il faut le dire franchement, la surface d'attaque augmente : un compte de projet dont la clé fuite obtient un shell, là où `nologin` l'arrêtait net. D'où trois garde-fous. La valeur est **un réglage**, et la fermer se fait en une ligne pour tout le parc, `user.shell: /usr/sbin/nologin` ; le défaut est **de création**, donc le fichier d'un projet existant fait autorité ensuite — sans quoi un changement de configuration réécrirait le shell de tout le parc au prochain apply ; et un chemin **relatif** est refusé au chargement, `useradd` l'acceptant tel quel pour rendre un compte inutilisable sans un mot, quand `doctor` signale par ailleurs un shell absent du disque. La réconciliation est le corollaire nécessaire : sans elle `spec.user.shell` serait un état désiré que rien n'applique, et ouvrir ou fermer un projet existant demanderait un `usermod` à la main. Elle partage l'étape du compte avec la correction du home — deux étapes de même ressource se marcheraient dessus dans le plan comme dans l'audit. Un projet **adopté** est l'exception : son compte existait avant ogs, et l'import reprend son shell réel plutôt que d'imposer le défaut du parc, comme il garde ses autres noms |
| D63 | Surcharges de vhost | Chaque vhost inclut **deux répertoires facultatifs** de `/etc/<moteur>/projects/<nom du vhost>/` : `vhost/*.conf` dans le contexte du vhost (`server` de nginx, `<VirtualHost>` d'Apache) et `docroot/*.conf` dans celui du répertoire servi (`location /`, `<Directory>`). Les répertoires sont créés par l'apply en **0750 root:root** et **déduits du rendu**, non d'une table | Un `blocks` de `spec` vit dans le fichier du projet, donc sous le contrôle d'ogs : le modifier demande un apply, et il n'a pas de sens pour un réglage que l'exploitant ajuste à la main. Le répertoire répond à ce besoin sans ouvrir la moindre brèche dans le modèle : ogs ne lit jamais ces fichiers, il ne fait que les faire inclure, et l'empreinte du vhost généré ne dépend pas d'eux — une surcharge déposée ne fait donc jamais dériver le plan. **Le masque est ce qui rend l'inclusion facultative** : nginx et Apache traitent un joker sans correspondance comme un silence, là où un chemin littéral introuvable est fatal — d'où `IncludeOptional` et non `Include`, ce dernier échouant sur un joker vide depuis 2.4. Le nommage suit le **fichier de vhost** (`shopfr`, non `shop.fr`) : un projet porte plusieurs vhosts, et un domaine principal qui change laisserait un répertoire orphelin que rien ne relie plus. La liste des contextes est **déduite du vhost rendu** — les templates sont surchargeables, et une table finirait par mentir : ogs créerait un répertoire que le vhost n'inclut pas, où les fichiers déposés seraient ignorés en silence. Un vhost de **proxy** n'a donc que le contexte du vhost — il ne sert aucun répertoire, et nginx refuse un second `location /` dans le même `server` — et un vhost de **redirection** n'en a aucun : son unique `server` porte le challenge ACME, qu'un `rewrite` déposé là mettrait par terre. Trois points de sécurité et d'exploitation, tous portés par `doctor` : ces fichiers sont lus par le processus **maître, en root**, donc le régime des hooks s'applique — root:root, inscriptible par personne d'autre, sans quoi ce compte dicte la configuration du serveur ; un fichier `.conf` déposé hors des sous-répertoires de contexte n'est chargé par personne et **rien ne le dit** ; un répertoire qu'aucun vhost ne référence plus garde des fichiers que leur auteur croit actifs. La suppression d'un projet emporte les répertoires **vides** et laisse le reste, comme l'`authorized_keys` de D61 : ogs n'a pas écrit ces fichiers. Deux pièges de sémantique que la fonctionnalité expose et qu'aucune vérification de configuration ne voit : sur nginx un `add_header` dans le contexte du docroot **annule** tous ceux hérités du `server` — HSTS et `X-Robots-Tag` compris — et une directive **dupliquée** y est une erreur fatale, là où Apache retient la dernière ; un rechargement refusé arrête donc tout le parc, ce que la revalidation de `web.Provider.Reload` rattrape sans pouvoir l'empêcher |
| D64 | Supervision du parc | ogs **écrit la configuration** de fail2ban et de l'agent de métriques à partir de ce qu'il sait du parc — un fichier unique par outil, recalculé en entier à chaque apply, comme celui de logrotate. Il n'installe ni ne supervise rien lui-même, et se **tait** quand l'outil est absent : le critère est la présence de son répertoire de fragments, `monitoring.<outil>.enabled` tranchant explicitement | Révise §1.3, qui plaçait pare-feu et métriques hors périmètre. La raison du renversement tient en une phrase : ces deux outils ont besoin de ce qu'eux seuls ne peuvent pas deviner — quels journaux appartiennent à un projet, quels pools tournent sous quelle version, quels moteurs sont réellement en service —, et cette connaissance est exactement celle qu'ogs possède. Ce que la décision **ne fait pas** est aussi net : ogs n'installe aucun paquet (§1.3 reste), ne définit aucune destination de métriques (D65), n'écrit aucun filtre fail2ban — un filtre est une expression régulière appliquée à chaque ligne de journal, et en maintenir un jeu à côté de celui du projet amont serait s'engager à suivre chaque changement de format. Le tri-état de `enabled` est nécessaire : confondre « absent » et « faux » activerait la supervision sur une machine où rien n'est installé, ou l'inverse |
| D65 | Destination des métriques | ogs écrit les **entrées seules** dans `/etc/telegraf/telegraf.d/ogs.conf` ; la base, l'organisation et le jeton d'écriture restent dans `telegraf.conf`, à la main de l'administrateur | Un jeton d'écriture sur une base de métriques ne concerne aucun projet : ogs n'a pas à en être dépositaire, ni à gérer sa rotation. Le fragment porte en revanche les mots de passe des comptes de supervision, d'où **0640, groupe de l'agent** — le même régime que celui qu'`ogs harden` impose déjà à `/etc/telegraf` (D56). Deux conséquences à connaître : un fragment que l'agent ne peut pas lire ne produit **aucune erreur visible** — Telegraf démarre, ne collecte rien et se tait —, d'où le contrôle de `doctor` ; et il n'existe pas d'équivalent d'`apachectl -t` pour Telegraf, son option de test exécutant réellement la collecte, donc le rechargement se contente d'un **constat en aval** (`is-active`). L'enjeu est moindre qu'ailleurs : un agent mort ne fait tomber aucun site, il fait taire la supervision — encore faut-il le dire |
| D66 | Accès de l'agent à l'état des pools | Un **vhost de supervision** de portée serveur (`ogs-status`), écoutant sur la **boucle locale seule**, expose l'état du moteur web et celui de chaque pool PHP-FPM ; l'agent l'interroge en HTTP. Toute autre adresse d'écoute est **refusée au chargement de la configuration** | Le seul autre moyen de donner l'état d'un pool à l'agent est de lui donner le socket FPM. Or **parler au socket d'un pool, c'est pouvoir y faire exécuter du code sous l'identité du projet** : le protocole FastCGI n'a aucune authentification, et le pool exécute le script qu'on lui nomme. L'agent deviendrait un moyen d'exécution sur tous les projets à la fois. Le serveur web, lui, détient déjà ces sockets : il sert d'intermédiaire, et l'agent n'obtient que ce que le pool rend sur son chemin de statut. Le refus d'une adresse routable est au **chargement**, non à l'écriture du vhost : une erreur à l'apply laisserait le fichier écrit avant de se plaindre. Les pools exposent `pm.status_path` et `ping.path` sous des noms préfixés `ogs-`, inatteignables depuis un vhost de projet — ceux-ci ne routent vers FPM que les URI en `.php`. Deux pièges de moteur, tous deux invisibles à une vérification de configuration : Apache traite les sections `<Location>` **dans l'ordre du fichier** et non par spécificité, donc le refus par défaut doit venir en premier ; et `mod_proxy_fcgi` dérive `SCRIPT_NAME` de l'URI demandée et non du chemin de la cible `fcgi://`, d'où une réécriture explicite par `ProxyFCGISetEnvIf` — sans elle FPM ne reconnaît pas son chemin de statut, cherche un fichier et répond « Primary script unknown » |
| D67 | Comptes de supervision | Un compte par moteur, de **portée parc** (scope de keystore `@parc`, impossible à porter par un projet), aux privilèges bornés : MySQL `PROCESS, REPLICATION CLIENT` et la lecture du `performance_schema` ; PostgreSQL le rôle `pg_monitor` **sans mot de passe** ; RabbitMQ le seul tag `monitoring`. Chaque étape est **non critique** | Un agent de métriques compromis ne doit pas être un accès aux données de clients : aucun de ces comptes n'atteint la base d'un projet. Le rôle PostgreSQL porte le **nom de l'utilisateur système de l'agent**, non un nom choisi : la connexion locale est authentifiée « peer », qui compare l'utilisateur du système — c'est la règle de D44, et un rôle nommé autrement serait créé puis refusé à chaque collecte. Le tag `monitoring` de RabbitMQ suffit et se suffit : il donne la lecture de tous les vhosts par l'API d'administration, là où une permission par vhost donnerait le droit de lire les messages des projets. Deux pièges : un moteur **installé mais injoignable** produit un avertissement, un moteur **absent** ne produit rien — une machine sans MySQL n'a pas à voir un avertissement à chaque apply ; et **MariaDB rend « BINLOG MONITOR » là où MySQL rend « REPLICATION CLIENT »**, si bien que comparer au seul libellé de MySQL faisait annoncer éternellement une correction qu'on venait d'appliquer |
| D68 | Prisons fail2ban | Les prisons sont **préfixées `ogs-`**, leurs journaux **énumérés un à un** et filtrés par existence, et le fichier est **recalculé à la suppression d'un projet** | Trois conséquences d'un même comportement : **une prison dont fail2ban ne trouve aucun journal l'empêche de démarrer, et il s'arrête alors tout entier** — les prisons valides comprises. D'où le filtrage à l'écriture, le recalcul à la suppression — sans lui, retirer un projet cassait fail2ban au prochain redémarrage, sans un mot — et un contrôle de `doctor` qui nomme les journaux disparus avant qu'ils ne bloquent le démon. Le même raisonnement décide du **backend de la prison sshd** : une Debian récente n'installe plus rsyslog, `/var/log/auth.log` peut ne pas exister, mais le backend `systemd` exige les liaisons `python3-systemd` qui ne sont qu'une recommandation du paquet — on choisit donc d'après ce qui existe, en préférant le fichier, qui ne dépend d'aucune liaison. Le préfixe n'est pas décoratif : `jail.d` est un espace partagé avec l'administrateur, et une prison nommée `sshd` recouvrirait la sienne en silence. `doctor` exécute par ailleurs `fail2ban-client -t`, seul contrôle qui couvre filtres, expressions et backend d'un coup |
| D69 | Signature DKIM | ogs **génère et garde** une clé DKIM par domaine d'envoi d'un projet qui la demande (`spec.mail.dkim`, **opt-in**), écrit la `KeyTable` et la `SigningTable` d'**OpenDKIM** à partir du parc entier et **émet l'enregistrement à publier** ; il ne touche **ni au DNS, ni au MTA, ni à `opendkim.conf`**. Clé **RSA-2048** générée en Go, scellée dans le keystore (`<projet>/dkim/<domaine>/<sélecteur>`) et écrite en **0600 opendkim:opendkim** ; sélecteur daté (`ogs202609`) ; rotation en **deux temps** — `rotate` prépare, `activate` bascule et refuse tant que l'enregistrement publié ne correspond pas à la clé, sauf `--force`. Étapes **non critiques** ; la préproduction est **refusée à la validation** | Révise §1.3, qui plaçait la messagerie hors périmètre, pour la raison de D64 : OpenDKIM a besoin de ce qu'ogs seul sait — quels domaines appartiennent à quel projet, et quand un projet disparaît. Le reste de la messagerie demeure dehors : ni MTA, ni relais, ni SPF — qui dépend des relais empruntés, qu'ogs ignore —, ni DMARC. **Le DNS reste hors périmètre malgré D53** : les identifiants OVH ne couvrent que la zone de préproduction, les zones des clients vivent ailleurs, et écrire dans la zone d'un client n'engage pas la même confiance que dans la sienne ; `doctor` vérifie ce qui est publié. **Pourquoi deux temps** : passer d'une clé qui se vérifie à une clé dont la partie publique n'est pas publiée casse la signature de tout envoi — sous un DMARC `p=reject` sans SPF aligné, le courrier est rejeté. À la **création**, en revanche, on signe aussitôt : une signature invérifiable ne vaut pas moins qu'une absence de signature, DMARC les traitant de même, et faire dépendre l'apply d'une résolution DNS rendrait le plan instable d'une exécution à l'autre. **Pourquoi sceller** : une clé DKIM, contrairement à un mot de passe, ne se régénère pas sans action extérieure — la perdre oblige à republier chez le client ; le keystore est la source de vérité et le fichier en dérive, donc un fichier disparu est réécrit, jamais recréé avec une clé neuve. Le fichier de clé ne peut porter l'en-tête d'ogs : il est comparé au contenu scellé, et un écart est traité comme une modification à la main (`--force`). **Régime de `/etc/opendkim`** : ogs pose lui-même le répertoire et ses sous-répertoires en 0750 root:opendkim, et les tables en 0640 root:opendkim — exactement ce qu'`ogs harden` exige (D56). Deux juges en désaccord feraient osciller l'apply et harden, l'un rouvrant ce que l'autre ferme ; et harden, récursif sur `/etc/opendkim`, ne change jamais le propriétaire d'un fichier ni ne desserre un mode, si bien qu'une clé en 0600 opendkim:opendkim reste lisible au démon. **Pourquoi 0600** : sous `RequireSafeKeys`, défaut d'OpenDKIM, une clé lisible au-delà de son propriétaire est refusée — le régime 0640 de D65 ne convient pas ici. Et le refus n'est pas un silence : l'essai réel l'a montré, une clé refusée **ou absente** ne laisse pas partir le courrier sans signature, OpenDKIM rend une erreur temporaire et **le MTA garde en file tout le courrier du domaine**. D'où deux règles : une table ne désigne jamais une clé dont le fichier manque, et le fichier d'un domaine qui cesse d'être signé n'est retiré qu'**après** le rechargement des tables, OpenDKIM gardant en mémoire celles qu'il a chargées. **Pourquoi des tables de parc** : OpenDKIM n'en accepte qu'une de chaque, et recalculées depuis un sous-ensemble elles retireraient de la signature tous les autres projets — la leçon de logrotate et de tmpfiles ; elles vivent donc dans le plan global, et `doctor` vérifie qu'`opendkim.conf`, laissé à l'administrateur comme `telegraf.conf` (D65), les référence bien. **Pourquoi pas la préproduction** : elle ne doit jamais pouvoir signer au nom du domaine de production — un envoi de test parti vers de vrais clients serait authentifié. **Pourquoi l'opt-in** : beaucoup de projets envoient par un relais tiers qui signe de sa propre clé ; signer par défaut créerait des clés dont aucun courrier ne se sert. **Pourquoi RSA** : ed25519 (RFC 8463) est encore trop peu vérifié pour servir seul. Piège : une clé RSA-2048 dépasse les 255 octets d'une chaîne TXT, l'enregistrement émis est donc découpé et `doctor` recolle les morceaux avant de comparer. **Pourquoi OpenDKIM plutôt que rspamd** : ses tables sont des fichiers plats, recalculables comme logrotate ou fail2ban, là où rspamd est une tout autre pièce à supposer présente |
| D70 | Liens gérés | L'interpréteur PHP et les liens des runtimes (`node`, `python`, `ruby`, `bundle`, `app`) vivent dans **`~/.local/bin`**, qui appartient à **l'utilisateur du projet** (0700) ; `.bin/` disparaît, retiré à la migration une fois les unités relues. Root y écrit **par descripteurs ouverts sans suivre de lien** (`openat`, `O_NOFOLLOW`), et un fichier posé à la place d'un lien n'est remplacé que sous `--force` | Révise D30, D35 et §7.1.2. `~/.local/bin` est le répertoire que la convention XDG réserve aux exécutables de l'utilisateur, et que le `.profile` de Debian ajoute au PATH : le shell ouvert par D62 y trouve la version déclarée sans rien configurer. Il appartient à l'utilisateur parce que pip, pipx et bien d'autres y écrivent : le fermer les casserait. **Et le fermer ne protégerait rien** : l'essai l'a montré, le home appartenant à l'utilisateur (D51), un sous-répertoire root se **renomme** sans privilège, puis se remplace — `.bin/` n'a jamais garanti la version effective contre l'utilisateur, seulement contre l'accident. La garantie devient donc ce qu'elle était dans les faits : un **état réconcilié**, rétabli à chaque apply. Ce que la décision ajoute est la contrepartie d'écrire en root chez quelqu'un d'autre : chaque composant du chemin est ouvert sans suivre de lien, et le lien est posé relativement au descripteur — un `~/.local` remplacé par un lien vers un autre répertoire fait refuser l'étape au lieu d'y faire écrire root. Le même traitement vaut désormais pour `.ssh/authorized_keys` (D61), que root posait jusque-là par son nom dans un répertoire de l'utilisateur. La migration retire l'ancienne entrée `.bin` des déclarations à la normalisation — sous sa forme exacte seulement, un `.bin` déclaré autrement l'ayant été par l'exploitant — et l'ancien répertoire en **fin de plan**, après le vidage des rechargements : une unité réécrite mais pas encore relue par systemd désigne toujours l'ancien lien. Seuls les liens connus sont retirés, et le répertoire ne part que vide |
| D71 | Journaux hors du home | Les journaux d'Apache, de nginx, de PHP-FPM et des crons vivent dans **`/var/log/ogs-projects/<id>/`** — racine réglable par **`logs.root`** —, chaîne entièrement root : la racine en **0711 root:root**, le répertoire du projet en **0750 root:<id>**, tous les fichiers **pré-créés** en 0640 (`root:<id>` pour ce qu'écrit un maître root, `<id>:<id>` pour `php-error.log` et `cron-<nom>.log`, écrits sous l'identité du projet). Le home ne porte qu'un lien de commodité, **`~/logs → /var/log/ogs-projects/<id>`**, qu'aucun processus root n'emprunte. `spec.layout.logDir` est refusé hors de la racine, `spec.crons[].log` n'est plus qu'un nom de fichier | Révise D29, D59 et §7.1.2, et referme la faille constatée le 23/09/2026 : le home appartenant à l'utilisateur (D51), il **renommait** le `logs/` root sans privilège et en recréait un à lui, où `apache-error.log` devenait un lien vers un fichier système que le maître du serveur web ouvrait en root à son redémarrage suivant — au plus tard à la rotation quotidienne. D29 protégeait d'une substitution *dans* `logs/`, pas *de* `logs/`. Désormais plus aucun composant du chemin n'appartient à l'utilisateur : il n'a plus rien à renommer. Le lien du home ne rouvre rien — lui seul le suit —, et il rend la lecture triviale : `tail -f ~/logs/apache-error.log`, `zless` sur les archives, qui gardent le mode du fichier roté. **Deux défauts antérieurs se referment du même coup**, vus en le préparant : `php-error.log` n'était **jamais écrit** — les workers, qui ouvrent `php_admin_value[error_log]` sous l'identité du projet, ne pouvaient pas créer le fichier dans un répertoire root, et PHP se rabattait sur le journal global de FPM, où se mêlaient les erreurs de tous les projets — et les droits annoncés au §14.5 étaient faux, les journaux sortant en 0644 root:root et `php-slow.log` en 0600, illisible au projet ; la pré-création rend les droits vrais. **Logrotate** : aucun `su`, et il ne faut jamais en ajouter — un répertoire non inscriptible par un groupe non root passe le contrôle de sécurité du répertoire parent, et le rendre inscriptible au projet, pour laisser une application y créer ses fichiers, ferait refuser la rotation et rouvrirait la faille ; `create` reste sans mode ni propriétaire, chaque fichier gardant le sien ; jamais un chemin passant par le home, ni un `olddir` dans le home, logrotate suivant le lien en root. **Le complément d'unité** du serveur web se réduit à une entrée fixe, conservée par prudence envers un `ProtectSystem=strict` : son contenu ne change plus à la création d'un projet, qui **ne redémarre donc plus le serveur web**. **Contrepartie** : les journaux quittent `/home` pour `/var`, qui porte aussi `/var/lib/mysql` et `/var/lib/postgresql` — un journal d'accès qui s'emballe peut remplir la partition et arrêter les bases. D'où `logs.root`, dont `doctor` contrôle la chaîne de propriété, et l'avertissement de `doctor` sur l'espace libre ; `maxsize` borne déjà chaque fichier. `www-data`, membre du groupe de chaque projet (D25), lit tous les journaux, comme auparavant. Les journaux applicatifs (`var/log/`) restent hors périmètre, gérés par l'application. **Migration**, sans perte de lignes : le nouveau répertoire est créé et les fichiers courants y sont copiés ; vhosts, pool, cron et complément d'unité sont réécrits puis rechargés, les serveurs ouvrant alors les nouveaux fichiers ; les lignes écrites dans l'ancien entre la copie et le rechargement sont ajoutées au nouveau, les fichiers rotés déplacés sous leur nom — logrotate poursuit la numérotation —, puis l'ancien `~/logs` est retiré et le lien posé à sa place. L'ancien répertoire vivant dans le home, l'utilisateur a pu le substituer : il n'est lu que par descripteur ouvert sans suivre de lien, **s'il appartient à root**, et seuls ses fichiers réguliers appartenant à root ou au projet sont repris — sans quoi root recopierait un `apache-error.log → /etc/shadow` dans un répertoire que ce même utilisateur lit. À défaut, rien n'est lu, et `doctor` le signale |
| D72 | Pare-feu | ogs réconcilie **ufw** avec **`/etc/ogs/ufw.yaml`**, fichier distinct de `config.yaml` dont l'**absence** vaut « ogs ne touche pas à ufw ». Il écrit un seul fichier de profils, **`/etc/ufw/applications.d/ogs`**, aux profils préfixés **`ogs-`** et dérivés de ce qui est installé (unités systemd, `sshd_config`, `proftpd.conf`, `vsftpd.conf`, `redis.conf`…) ; toute règle passe par la **commande ufw**, jamais par `user.rules`. Une règle est à ogs si son commentaire commence par **`ogs:`** ; les autres sont laissées intactes et signalées. **Sources nommées** (propriétaire, adresses), adresses de **confiance** — une règle **par profil disponible**, et reprises dans l'`ignoreip` des prisons fail2ban —, **bannissements** placés en tête. Commentaire déclaré ou **engendré** d'après la source et son propriétaire. La **première activation** se défait d'elle-même au bout de `rollbackAfter` (5 min) faute de `ogs firewall confirm` ; une déclaration qui **couperait la session SSH** en cours, bannirait son adresse, fermerait un port où sshd écoute ou fermerait SSH à tous est refusée sauf `--allow-lockout` (**révisé par D86** : à l'origine `--force`, que le plan global transmettait, et qui ne lève plus cette garde). Une déclaration refusée ou invalide n'est **jamais tentée** : code 3 pour `ogs firewall`, avertissement (code 6) dans le plan global, **dont le reste s'applique**. Étapes **non critiques**, mais politiques et activation refusent de s'appliquer si une règle a échoué | Révise §1.3, qui plaçait les règles de pare-feu hors périmètre, pour la raison de D64 : ufw ne sait pas quels services tournent sur quels ports — un sshd sur 2200, le SFTP de proftpd sur 2222, la plage passive du FTP —, et ogs, lui, le lit. **Pourquoi un fichier à part** : il n'a pas le rythme de `config.yaml` — on bannit un soir de scan, on ouvre au poste d'un prestataire le temps d'une intervention —, et son absence est une décision que rien d'autre ne porte. **Pourquoi des profils ogs** : ceux des paquets (`OpenSSH`, `Apache Full`) figent des ports qu'ogs ne maîtrise pas, et `applications.d` leur appartient. **Pourquoi la commande et non le fichier** : `user.rules` est le reflet de ce qu'ufw a chargé, et le réécrire désaccorderait les deux jusqu'au prochain rechargement. **Pourquoi la marque** : sans elle, reconnaître ses règles demanderait un inventaire à tenir en plus d'ufw, qui se désaccorderait au premier `ufw delete` manuel ; avec elle, ce qui n'est pas marqué appartient à l'administrateur, sans exception. **Pourquoi une règle par profil pour la confiance**, plutôt qu'un `allow from` global : ce dernier ouvrirait aussi les ports qui ne doivent jamais sortir de la boucle locale — MariaDB, le vhost d'état (D66), les applications (D36) —, le jour où l'un d'eux écouterait ailleurs. **Pourquoi le retour automatique** : la session qui active un pare-feu mal écrit est celle qu'il coupe, et plus rien de ce qu'elle a lancé ne tourne pour réparer ; c'est donc systemd qui porte la désactivation (`systemd-run --on-active`). Seule la première activation l'arme (arbitrage du 24/09/2026) : une fois la confirmation obtenue depuis une nouvelle session, les apply suivants sont gardés par la vérification de la session. **Pourquoi le plan global ne s'arrête pas** : `project delete` recalcule par lui les prisons de fail2ban et la rotation des journaux (D68) ; une faute de frappe dans `ufw.yaml` ne doit pas priver le parc de ce recalcul — c'est la règle de `parcNonBloquante`. **Pourquoi en tête** : ufw retient la première règle qui correspond, et un refus posé après une autorisation plus large ne servirait jamais. Trois pièges d'ufw, tous invisibles à une lecture de sa sortie : il **refuse l'apostrophe** dans un commentaire — ogs la remplace par l'apostrophe typographique ; il **remplace en place** le commentaire d'une règle identique, mais `prepend` **ignore** une règle existante, si bien qu'un commentaire se corrige par la commande d'ajout ordinaire ; une règle d'application porte les ports du profil au moment où elle a été posée, d'où `ufw app update` quand un profil change — et ufw refuse une application inconnue **jusque dans `ufw delete`**, d'où le retrait des règles d'un profil **avant** de retirer le profil |
| D73 | Moteur web | Le moteur web est un choix **de machine** : celui qui est installé, ou `web.engine` de `/etc/ogs/config.yaml` si Apache et nginx le sont tous deux. `site create` n'a plus d'option `--engine`, et un `spec.web.engine` hérité est **relu sans erreur puis effacé** à la normalisation | Précise §7.6 et le §10.2, qui laissaient croire à un choix par projet. L'option écrivait `spec.web.engine` dans la déclaration, mais **aucun code ne lisait ce champ** : un projet déclaré en nginx recevait un vhost Apache sans un mot (constaté le 24/09/2026). Honorer le champ contredirait la règle d’un seul moteur piloté par serveur (§7.6) — vhosts, fragment de portée serveur, complément d'unité, vhost de statut et rechargements sont tous calculés pour un moteur. Le champ reste accepté à la lecture parce que le décodage est **strict** : le retirer du modèle ferait refuser toute déclaration écrite avec l'ancienne option. Au passage, une option inconnue rend désormais le code d'usage 2, et non 1 |
| D74 | Cycle de vie de PHP-FPM | Le rechargement d'une version PHP suit ses pools : **rechargé** si FPM tourne, **activé et démarré** (`reset-failed` puis `enable --now`) s'il est arrêté, **arrêté et désactivé** (`disable --now`) quand plus aucun `pool.d/*.conf` n'existe — d'ogs ou de l'administrateur | FPM refuse de tourner sans pool (« No pool defined »). Une installation qui retire le `www.conf` de la distribution — pour qu'aucun pool ne tourne sous `www-data` — laisse donc chaque version arrêtée tant qu'aucun projet ne l'emploie. ogs ne faisait que `systemctl reload` : au premier site, le rechargement échouait sur une unité inactive et le site répondait 503 sur un simple code 6 ; au départ du dernier, recharger un maître sans pool le tuait, et l'unité restée activée échouait à chaque démarrage. Démarrer ne suffit pas : sans `enable`, la version ne repartirait pas au redémarrage de la machine. Scénario `fpm-sans-pool` |
| D75 | Application `custom` | Un type d'application **`custom`** supervise une commande déclarée (**`spec.app.command`**, recopiée dans `ExecStart`), sans runtime ni lien géré, derrière un vhost de proxy **obligatoire**. Écoute sur un **port** par défaut (code 5, `25xxx`). La commande commence par un **chemin absolu** et tient sur une ligne ; `runtime` et `entrypoint` y sont refusés, `command` l'est pour les autres types. **`spec.app.envFile: true`** — admis pour toute application supervisée — branche **`/etc/ogs/env/<id>.env`**, emplacement **non réglable**, créé vide en **0600 root:root** (répertoire 0700) puis **jamais réécrit** ; un lien à sa place est refusé, des droits ouverts sont resserrés. Il est **scellé** dans l'archive d'un projet supprimé, puis retiré | Les types à runtime couvrent quatre écosystèmes ; tout le reste — un binaire Rust, un serveur Java, un outil livré compilé — n'avait d'autre issue qu'une unité écrite à la main, hors de la réconciliation. **Pourquoi un port** : un socket suppose que le code lise `SOCKET_PATH`, et ogs ne sait rien de ce qu'il lance. **Pourquoi un chemin absolu** : il écarte du même coup les préfixes d'`ExecStart` — `+` et `!` lanceraient la commande en root, au mépris de `User=` —, et un saut de ligne ferait écrire une directive de plus dans l'unité. Les variables, elles, sont développées par systemd (`${LISTEN_ADDR}`) : aucun gabarit à inventer. **Pourquoi un emplacement imposé** pour le fichier d'environnement : systemd le lit **en root, avant** le changement d'identité ; dans le home (D51), l'utilisateur le remplacerait par un lien vers le `.env` d'un voisin, et root injecterait les secrets de celui-ci dans son processus — la classe de faille que D70 et D71 ont refermée. **Pourquoi 0600 root:root** plutôt qu'un 0640 lisible au projet : le projet n'a pas à le lire, et c'est le régime qu'`ogs harden` impose sous `/etc/ogs` — deux juges en désaccord feraient osciller le plan. **Pourquoi jamais réécrit** : son contenu, des secrets le plus souvent, est l'affaire de l'exploitant ; ogs n'en porte ni l'empreinte ni le contrôle de dérive. Pas de tiret devant `EnvironmentFile=` : un fichier retiré doit faire échouer le démarrage, non priver l'application de ses secrets en silence. **Pourquoi un proxy obligatoire** : un démon sans HTTP a déjà sa place, `spec.services` ; l'admettre ici aurait demandé d'enseigner au plan web et à l'attente de disponibilité un type sans point d'écoute. Au passage, l'unité applicative `ogs-<id>-app@.service` est désormais retirée à la suppression du projet : elle survivait, invisible de `systemctl list-units` faute d'instance chargée ; et les affectations `Environment=` de toutes les unités sont **citées quand la valeur l'exige** — systemd découpant la ligne sur les blancs, une valeur à espaces arrivait tronquée au processus —, une valeur simple gardant son rendu pour ne réécrire ni redémarrer aucune unité existante |
| D76 | Appartenance des fichiers gérés | Un fichier n'appartient à un projet que si **l'en-tête d'ogs** le dit — le projet et la ressource de sa ligne `projet: … ressource: …`, cherchée sous la marque dans les trois premières lignes comme le fait `Inspect` (`fs.ManagedResource`) —, **jamais par son nom** ni par un préfixe. **Tout retrait passe par les cinq états** (`removal.File`, l'état relu à l'application) : un fichier intact part ; un fichier retouché ne part que sous `--force`, sauvegardé ; un fichier sans la marque ou marqué pour un autre projet ne part jamais, `--force` compris — refus non critique, code 6. Un projet n'**écrit** jamais le fichier d'un autre (`fs.CheckOwner`, code 4, `--force` compris) ; si ce dernier n'est plus déclaré, le message parle d'un reste à retirer à la main. Un vhost adopté désigne son fichier réel par **`spec.web.vhosts[].file`** : réservé aux adoptés, nom simple, `.conf` exigé sous Apache (code 3). La liste des vhosts d'un projet et le nom de leurs fichiers ont **une seule source**, `model.Project.VHosts`, `VHostName` et `VHostFile`, que lisent le provider web et la clé d'unicité « fichier de vhost » (§3.3.6). Une sauvegarde ne réutilise **jamais un nom** : `O_EXCL`, puis `.1`, `.2`… (`fs.writeBackup`). `project delete` garde un critère plus strict, `fs.ManagedProject` : la marque en première ligne | Révise §6.4, qui ne connaissait les cinq états qu'à l'écriture. L'attribution par préfixe faisait retirer au premier apply de `shop`, en code 0 et sans copie, le pool et le vhost de `shop_api` : `shop_api.conf` n'est pas un pool de `shop`, c'est le fichier principal de son voisin (R1-01). Le nom tronqué d'un vhost nginx adopté — `shop.fr` et `shop.de`, fichiers sans extension selon l'usage Debian — en faisait deux « obsolètes » du même projet, retirés avec le site voisin (R1-11). Un fichier cron ou sudoers retouché partait sans copie, alors que son écrasement était refusé (R1-34). **Pourquoi l'en-tête lu comme `Inspect`** : ce que l'un attribue, l'autre doit le juger de même — exiger la marque en tête laissait sans maître le fichier d'un voisin retouché au-dessus de son en-tête, que `--force` réécrivait alors au compte du mauvais projet. **Pourquoi `ManagedProject` reste strict** : il décide seul de ce que la suppression retire, sans passer par les cinq états, et ne pas reconnaître un fichier retouché le laisse en place — le choix prudent. **Pourquoi `file`** : l'adoption ne renomme jamais (§9), et sans lui le premier apply écrivait un second fichier servant les mêmes domaines. **Pourquoi `O_EXCL`** : pool et vhost s'appellent tous deux `<id>.conf`, cron et sudoers `ogs-<id>`, Telegraf et fail2ban `ogs.conf` ; un même apply forcé les sauvegarde dans la même seconde, et la seconde copie écrasait la première, que le plan annonçait sauvegardée. **Pourquoi une source unique** : recopiée du provider au contrôle d'unicité, la règle aurait divergé sans un mot, le contrôle approuvant des collisions que l'apply aurait trouvées. Corollaire : un fichier qui porte le nom exact d'un pool du projet dans une autre version sans qu'ogs l'ait écrit pour lui — le pool d'origine d'un projet adopté, resté après un `php switch` — est signalé (code 6) et jamais retiré |
| D77 | Garde-fous de la suppression | `project delete` prend le verrou global, puis prononce **tous ses refus avant la confirmation**, en un seul message où chacun porte son remède, avec le code du premier — ce que rien ne lève, puis ce que lève `--force`, puis `--no-dump`. **Rien ne lève** : une racine dangereuse par sa forme (« / », chemin relatif, emplacement système) ; une racine égale, ancêtre ou descendante de la racine ou du docroot d'un autre projet — une déclaration voisine illisible compte comme un refus ; une racine liée, qui n'est pas un répertoire ou qui a un autre propriétaire ; un **point de montage** sous la racine ou sous les journaux (`/proc/self/mountinfo`) ; un fichier laissé en place qu'ogs n'a pas écrit — sans son en-tête, illisible, ou lien vers un fichier hors du projet ; celui qu'ogs a écrit pour un autre projet est seulement laissé et nommé (remède proposé d'abord : `ogs apply <id> --force`). **`--force` lève** le compte sans la marque `ogs=` et les bases et comptes SGBD d'un projet adopté, nommés avant la confirmation. **`--no-dump` lève** une base inobservable ; un dump en échec abandonne avant tout retrait. Les refus sont **éprouvés de nouveau au point de non-retour**, après l'archive. Avant ce point, un refus, un dump en échec ou une **interruption** (code 9) retirent l'archive et relancent les services arrêtés ; après lui, le contexte est détaché et la suppression va à son terme, l'interruption étant seulement signalée. Seuls partent les fichiers dont l'en-tête désigne le projet (`fs.ManagedProject`) — hors le fichier d'environnement de D75, qui n'en porte pas et part toujours, scellé dans l'archive, le répertoire des clés DKIM du projet (`/etc/opendkim/keys/<id>`) et le répertoire `.wants` de sa cible, retirés en bloc. Sous `--keep-data`, rien n'est refusé, mais l'archive ne recopie pas une arborescence que la suppression n'aurait pas pu effacer. `validate.Uniqueness` refuse les **homes imbriqués** | Précise §7.1.6 et §13. Un refus prononcé après le point de non-retour ne sauvait plus que ce qu'il gardait : le compte non créé par ogs était refusé une fois tout le reste démonté, et le `--force` qu'il conseillait échouait faute de déclaration (R1-19). Un refus qui en masque un autre fait suivre un remède coûteux — retirer le vhost d'un site adopté, c'est couper le site — avant d'apprendre le suivant. Une racine englobante issue d'un import effaçait le home d'un voisin (R1-02) ; `os.RemoveAll` franchit les points de montage, et seule `mountinfo` montre un montage lié (R1-03) ; un dump en échec laissait partir `DROP DATABASE` sans sauvegarde (R1-12) ; un refus de taille laissait les services arrêtés (R1-72). **Pourquoi refuser un fichier qu'ogs n'a pas écrit** : il reste en place, et il désigne peut-être ce que la suppression efface — un vhost adopté qui journalise sous le home rend la configuration d'Apache invalide une fois le home parti, un pool partagé avec un site voisin le fait tomber en 502 (R1-24). **Pourquoi l'interruption** : `main` annule le contexte, il n'arrête pas le processus ; rien ne le consultait, et la suppression continuait, chaque commande échouant sur « context canceled », jusqu'à un home effacé sous un compte et des workers FPM vivants (R1-15). **Pourquoi aller à son terme passé le point de non-retour** : y renoncer laisserait le pire état — home effacé sous un compte vivant, prison fail2ban désignant des journaux disparus, qui empêche le démon de redémarrer. **Contreparties** : passé ce point, seul SIGKILL arrête le processus, le temps de la phase — brève, sauf l'effacement d'un gros home ; une interruption pendant l'archivage d'une arborescence attend la fin de celle-ci. Un parc écrit avant D77 peut porter deux homes imbriqués, laissés par un import : le refus vaut alors pour toute commande qui éprouve l'unicité, même au sujet d'un troisième projet, et le message dit que le conflit peut précéder la commande en cours |
| D78 | Bases à l'import | Une base n'est rattachée qu'au **seul** candidat que son nom désigne — son identifiant, ou le compte de son pool —, **aux séparateurs près, jamais par préfixe** (`shop_fr`, `shop-fr`, `shopfr` et `Shop.FR` se valent ; `shop` et `shopping`, non). Elle n'est déclarée qu'avec le **seul** compte du moteur qui porte ce nom, et sous son **hôte réel** (`host: '%'`) ; faute de quoi elle reste **non gérée**, et le scan comme l'adoption disent pourquoi — l'adoption affiche avant la confirmation les bases déclarées, leur compte et leur hôte. Le provider de bases **refuse de créer** un compte MySQL dont le nom existe déjà sous un autre hôte : étape non critique en échec, qui nomme le compte existant et la porte de sortie, rien n'étant exécuté ni scellé | Révise §9, qui corrélait par « similarité de nom ». La propriété d'une base n'est pas déductible, et ce que la déclaration engage ne se rattrape pas : son compte reçoit tous les droits à l'apply, et `project delete` supprime la base et le compte. Le préfixe donnait à `shop.fr` la base de `shopping.fr` et le compte de `shop.com`, le compte le plus court l'emportant, et le premier candidat dans l'ordre alphabétique prenait toutes les bases qui lui ressemblaient (R1-04). **Pourquoi l'hôte** : `'shop'@'%'` et `'shop'@'localhost'` sont deux comptes, et MySQL retient pour chaque connexion celui dont l'hôte est le plus précis ; déclarer `localhost` créait à l'apply un homonyme au mot de passe tiré, et l'application voyait son mot de passe refusé, par le socket comme par 127.0.0.1 (R1-09). Le refus du provider couvre les déclarations écrites avant cette règle, qui portent encore `host: localhost`. **Pourquoi non déclarée plutôt que déclarée sans compte** : tout le provider suppose un compte — création, `GRANT`, DSN, suppression. **Pourquoi pas un compte neuf** : il n'est pas neutre sur une base existante ; sur PostgreSQL, l'apply révoque `CONNECT` à `PUBLIC` et donne le schéma `public` au nouveau rôle, ce qui coupe le rôle réel. **Pourquoi deux hôtes font une ambiguïté** : retenir `localhost` reviendrait à deviner celui que l'application emploie. Le repli par nom du pool exige lui aussi l'égalité : un pool « shop » n'est plus rapproché de `shop.fr` par son seul nom, le socket restant une preuve. Contrepartie : une base nommée autrement que le site (`shop_prod`) reste non gérée, à déclarer à la main |
| D79 | Rendu TLS d'un vhost | Le bloc 443 d'un vhost est rendu dès que le certificat **présent et non expiré** couvre **l'un** de ses noms, et non tous. Son nom canonique — le `ServerName`, et la cible de la redirection du port 80 — est le premier nom couvert. Les noms en attente restent déclarés dans les deux blocs : le port 80 sert leur challenge avant toute redirection. La bascule sur le nouveau certificat suit l'émission, dont l'étape porte le rechargement du serveur web (`tls.Provider.WebReload`). `PlanTLS` réécrit, émission réussie ou non, tout vhost dont le rendu d'après le matériel présent diffère du fichier en place. Une redirection vise https si sa cible, quand c'est un nom du projet, est couverte, et vise une cible étrangère en https dès que le TLS est voulu. Un certificat **expiré n'est plus présenté**, qu'il soit de projet ou partagé, et `status` le dit « expiré » | Révise §6.2, étapes 6 à 9. Juger le vhost sur la demande le faisait redescendre en HTTP seul dès qu'un alias était ajouté ou le principal changé, puis le rechargeait ainsi avant même le challenge. Si l'émission échouait, il le restait : HSTS rendait le site injoignable à qui l'avait déjà visité, et les autres recevaient sur le port 443 le site d'un autre client (R1-06). **Pourquoi « l'un des noms » et non le seul principal** : quand le principal change en gardant l'ancien parmi les alias, c'est l'ancien que couvre le certificat. **Pourquoi comparer au fichier en place** : un repli peut couvrir la cible d'une redirection écrite en http quand rien n'était couvert, et le visiteur ferait sinon un saut de plus jusqu'au prochain apply. **Trois états transitoires**, que `plan` montre tous et que le prochain apply résorbe, `cert renew` ne réécrivant aucun vhost : (1) après une réémission réussie par le timer à la suite d'un changement de principal, le nom canonique reste celui de transition ; (2) un vhost de redirection resté en HTTP faute de couverture — en particulier celui qu'une version antérieure servait avec un certificat qui ne le couvrait pas — ne retrouve pas son bloc 443 après un renouvellement réussi par le seul timer ; (3) une redirection que le repli de D80 a retirée du certificat garde son bloc 443 jusqu'au prochain apply, ce que le message du repli annonce. Faire réécrire les vhosts par le timer lui imposerait le verrou global et l'écriture de configuration |
| D80 | Redirections et repli d'émission | Le certificat d'un projet couvre ses domaines principaux, ses alias **puis ses redirections**, hormis celles qu'aucune demande ACME ne peut porter (une adresse IP : son vhost reste en HTTP seul). Les noms du vhost principal sont **essentiels** (`Request.Essential`) ; dans un certificat déclaré, un joker l'est dès qu'il couvre un nom servi. Quand la demande complète échoue, l'émission se **replie par paliers**, du plus large au plus étroit : sans les redirections en attente, puis sans aucune redirection, puis ce que couvre le certificat en place, puis les noms essentiels qu'il couvre. Le premier palier qui aboutit est un succès partiel : code 6, « ✓ … (en partie) », rechargement retenu (`engine.PartialError`), jamais compensé. **Loin de l'échéance** (plus de 30 jours, sans `--force`), un palier n'est tenté que s'il **ajoute un nom sans retirer aucun de ceux que le certificat en place sert** ; celui qui en retirerait un est réservé à l'échéance, et le message donne sa date et son recours. Le timer ne réémet pour la seule couverture que si la déclaration a été appliquée telle quelle (`spec_hash`). `cert renew` recharge le serveur web dès qu'un certificat a changé, et signale (code 6) un projet dont la demande est refusée au lieu de le passer | Révise le lot L3, qui excluait les redirections du certificat, et précise D43 : un domaine redirigé veut être chiffré, et son vhost 443 présentait un certificat qui ne le couvrait pas (R1-32). **Pourquoi un repli** : une commande ACME échoue tout entière sur un seul nom refusé ; une redirection dont le DNS pointe ailleurs empêchait sinon le renouvellement du site, qui tombait à l'échéance. **Pourquoi des paliers et non les seuls noms déjà couverts** : un alias ajouté ou un nouveau principal ne pouvait jamais être couvert tant qu'une redirection était refusée. lego ne nomme le nom refusé que dans un texte qu'ogs ne lit pas, et pas du tout quand la tentative est suspendue : chaque palier écarte une classe de noms, la moins précieuse d'abord, en quatre demandes de plus au plus, chacune sous sa propre clé de suspension. **Pourquoi « ce que couvre le certificat en place » avant les seuls noms essentiels** : ce palier garde une redirection déjà servie, comme `www.shop.fr`, que le HSTS `includeSubDomains` oblige à joindre en HTTPS. **Pourquoi ne rien retirer loin de l'échéance** : c'est la règle de D79 appliquée au certificat — un nom servi en HTTPS ne redescend pas parce qu'une réémission est en attente ; un refus peut aussi être passager, et réémettre à l'identique consommerait les quotas de l'autorité. Près de l'échéance, le site passe avant tout. **Pourquoi écarter les adresses IP plutôt que les refuser** : la validation les admet et la redirection a du sens ; les garder faisait refuser la demande entière — plus d'apply, et le renouvellement du parc passait le projet en silence, le certificat allant à l'expiration. **Pourquoi signaler un projet non examiné** : le timer rendait 0 pendant qu'un certificat expirait. **Pourquoi recharger dès qu'un certificat change** : un échec ailleurs dans le parc privait sinon les certificats renouvelés de leur rechargement, et le serveur présentait l'ancien matériel jusqu'à son expiration (R1-64, R1-88). **Pourquoi pas un certificat distinct pour les redirections** : il ne protège pas d'un alias en attente, et il casse les changements de rôle — un alias devenu redirection passerait en HTTP le temps de l'émission. **Pourquoi la déclaration appliquée** : un alias écrit mais pas encore appliqué n'est servi par aucun vhost, et chaque passage du timer compterait un échec, jusqu'à suspendre l'apply qui suit. **Contreparties** : une redirection dont le DNS ne pointe jamais ici produit un code 6 à chaque apply et à chaque passage du timer — la retirer, ou déclarer `spec.certificates` ; une redirection déjà couverte puis refusée retarde, loin de l'échéance, la couverture d'un nom ajouté à côté jusqu'à la fenêtre de renouvellement, ce que `cert renew <id> --force` impose tout de suite |
| D81 | Archive et restauration sans suivre de lien | L'archivage parcourt le home **par descripteurs** (`fs.Dir` : `openat`, `O_NOFOLLOW`) ; chaque entrée est décrite par le descripteur ouvert, jamais par le constat qui l'a précédé, et une entrée changée entre les deux est écartée. Chaque en-tête de l'archive porte son uid et son gid, et le manifeste (`owners`, facultatif) le propriétaire de la racine de chaque arborescence. La restauration extrait **après l'apply**, par descripteurs, sans jamais traverser un lien, en création exclusive : ce que l'apply a posé dans le home neuf — trousseau (D61), interpréteur (D70), `~/logs` (D71) — est remplacé **en tant que lien** par ce que l'archive porte ; un répertoire existant n'est jamais remplacé. Chaque entrée reçoit son propriétaire **sur son descripteur** (un lien par un descripteur `O_PATH`) : au compte recréé ce qui était à l'utilisateur d'origine, **à root tout le reste**, dans le groupe du projet si c'était le sien, son mode d'origine gardé. Une archive qui ne porte pas de propriétaires est jugée sur sa déclaration **telle qu'archivée**, avant normalisation. Une entrée écartée rend le code 6 | Précise §13. Écrire par le nom faisait réécrire en root, par les liens que l'apply venait de poser, le trousseau commun de tout le parc ou le binaire PHP du système (R1-10). `filepath.Walk` et `os.Open` laissaient l'utilisateur faire archiver `/etc/ogs/master.key` en substituant un ancêtre ou un fichier entre le constat et l'ouverture (R1-59). Le `chown -R` de `www/` laissait à root `authorized_keys`, les sessions et le site d'un projet adopté hors de `www/` (R1-55). **Pourquoi pas tout rendre à l'utilisateur** : un lien dur vers `/etc/shadow`, qu'un `fs.protected_hardlinks=0` lui laisse poser dans son home, lui reviendrait lisible ; rendu à root avec son mode, il n'étend la lecture à personne. Ni refus ni masque de mode : un refus aurait perdu, à la restauration, les téléversements `www-data` d'un site adopté. **Pourquoi après l'apply** : les identifiants du compte recréé sont indispensables au remappage, et rien n'est jamais chowné après coup. **Pourquoi la déclaration archivée** : la normalisation retire l'entrée `logs` root ; l'ancien `~/logs` d'une archive antérieure à D71 revenait alors à l'utilisateur, et la migration le refusait à chaque apply comme substitué — il revient désormais à root, et l'apply suivant le migre, ce que la restauration annonce. **Pas de relèvement de `version`** : un ogs antérieur ignore `owners` et restaure comme avant ; relever la version aurait bloqué une restauration après retour arrière du paquet, sans rien protéger. Un bail `F_SETLEASE` fait échouer une ouverture non bloquante : l'ouverture retente le temps que le noyau le rompe (50 s au plus), puis échoue en le nommant. Limite : dans une archive sans propriétaires, rien ne distingue un lien dur ou un `~/logs` substitué. Scénario `restauration` |
| D82 | Root dans le home, hors apply | Les commandes qui lisent ou écrivent en root dans le home d'un projet **sans plan** — `user key show` et `rotate`, `site clone`, `php switch` — n'y passent **que par des descripteurs** ouverts sans suivre de lien (`fs.OpenDirAt`, `fs.DirAt`, `fs.CopyTreeAt`) : un composant lié fait refuser l'opération, jamais suivre. **Clé de déploiement** : `.ssh` doit être un vrai répertoire au compte du projet ; une clé qui n'est pas un fichier régulier du projet, qui dépasse **16 Kio** ou dont la partie publique n'est pas du texte imprimable sur une ligne n'est ni lue, ni affichée, ni sauvegardée ; la nouvelle paire est posée par `unlinkat` puis `openat` en `O_EXCL` et `O_NOFOLLOW`, mode et propriétaire sur le descripteur. **Copie du clone** : les liens sont recopiés comme liens ; seuls les fichiers de l'**uid source** sont repris ; fichiers spéciaux et répertoires au-delà de **1024 niveaux** sont écartés — tous comptés, les 50 premiers nommés et cités, code 6 ; les liens physiques internes sont reposés et les fichiers creux restent creux, root n'écrivant jamais plus de données que la source n'en occupait avant la copie ; propriétaire, mode et dates sont posés sur le descripteur ; ni ACL, ni attributs étendus, ni bits setuid et setgid des fichiers ne sont reportés. La copie suit l'**annulation** (SIGINT, SIGTERM) : code 9, projet cible créé avec un contenu partiel, base non copiée. **`php switch`** lit `composer.json` par descripteur, borné à **1 Mio** ; illisible ou refusé, c'est la table indicative qui décide, annoncée comme telle. La version d'un runtime se lit en lançant son lien **sous l'identité du projet** | Précise D38, §7.1.4 et §7.1.5. Le plan d'apply refuse un répertoire lié (`statDir`, étape critique), mais ces commandes le court-circuitent : elles tournent en root dans un répertoire que l'utilisateur possède (D51) et renomme sans privilège. `user key rotate` créait la paire dans la cible d'un `.ssh` lié et la donnait au projet — avec `/etc/apt/apt.conf.d` pour cible, une exécution en root au prochain `apt-get update` —, et `key show` lisait tout fichier (R1-13) ; `site clone` recopiait en root, par `cp -a www/.`, la cible d'un `www` lié, que `chown -R` donnait ensuite à la préproduction servie (R1-63). **Pourquoi l'uid source et non le compte de liens** : un lien physique vers `/etc/shadow` n'a plus qu'un lien dès que `useradd` — celui du clone même — réécrit le fichier par renommage. **Pourquoi des bornes** : taille, profondeur, nombre d'entrées et durée sont aux mains du locataire ; un fichier creux ne lui coûte aucun bloc, et `mkdir a; cd a` empile des milliers de niveaux pour quelques Mio, là où `cp -a` s'arrêtait de lui-même sur `PATH_MAX`. La mémoire de root reste linéaire : aucun chemin n'est retenu par niveau, alors qu'elle croissait comme le carré de la profondeur (800 Mio pour 8 000 niveaux). **Pourquoi l'annulation** : `cp -a` mourait avec son groupe de processus ; une copie faite dans le processus ne s'arrête que si elle consulte le contexte. **Pourquoi la table plutôt qu'un avertissement** pour un `composer.json` illisible : l'avertissement seul désarmait le garde-fou de D38, et l'utilisateur le déclenchait à volonté en posant un lien — ou bloquait la commande sur un tube, `/dev/zero` (S1R-30). **Pourquoi sous l'identité du projet** : exécuter en root ce que désigne un lien de l'utilisateur, c'est lui laisser choisir ce que root exécute (S1R-08). **Prix assumé** : ce qui appartient à root ou à `www-data` dans le `www` de production n'est pas recopié — sur un projet adopté, le rendre au compte de production avant le premier clone. SIGHUP n'est pas capté : une session ssh coupée pendant un long clone l'arrête net (§20) |
| D83 | Répertoires du projet et chemins désignés | **Sous le home, root n'atteint un répertoire que par descripteurs depuis la racine du projet**, à l'observation, à l'écriture comme à la compensation : `fs.EnsureDirAt`, `SetDirAt` et `LstatAt` ouvrent la racine en `O_NOFOLLOW`, puis chaque composant par `openat` relatif au précédent, et posent mode et propriétaire sur le descripteur ; la compensation d'une création passe par `fs.RemoveEmptyDirAt`. Un parent qui n'est pas un répertoire véritable fait échouer l'étape en le nommant (`DirState.LinkedParent`) ; un parent absent est créé au mode et au propriétaire de l'entrée. `fs.EnsureDir`, qui résout les parents par leur nom, ne sert plus qu'à la racine du projet, dont les parents appartiennent à root. **Hors du home, un chemin désigné en ligne de commande** (`db dump -O`, `db restore -i`) est parcouru depuis « / » par descripteurs (`fs.OpenParentDir`) : un lien n'y est suivi que si **seul root a pu le poser** — dans un répertoire à root, non inscriptible par le groupe ni par les autres, atteint par une chaîne entièrement de ce type, 40 liens au plus ; seul un fichier ordinaire ou un nom absent est admis, une sauvegarde étant recréée en `O_EXCL` et en 0600 ; `/dev/stdout`, `/dev/stderr`, `/dev/null` et `/dev/stdin` restent ouverts par leur nom | Généralise D70 aux répertoires de `model.defaultDirs`. `O_NOFOLLOW` ne protège que le **dernier** composant : root résolvait les parents par leur nom, et un parent jugé conforme au plan pouvait être remplacé par un lien avant l'étape de l'enfant — la confirmation d'« ogs apply » en laisse tout le temps. Un `~/.local` remplacé par un lien vers `/usr/local` faisait ainsi donner `/usr/local/bin` au projet, en tête de son propre PATH, et le plan annonçait « mode 0755 → 0700 » pour la cible du lien (S1R-03). `db dump -O` et `db restore -i` écrivaient et lisaient en root à travers un lien posé dans un home, et un tube nommé bloquait la commande (S1R-12). **Pourquoi « seul root a pu le poser » plutôt qu'aucun lien** : c'est le critère du §15 — aucun composant qu'un non-root contrôle —, et refuser tout lien casserait `project delete`, `project restore` et `site clone` sur une machine où `/var/lib/ogs`, `/home` ou `/var/run` sont des liens posés par l'administrateur. **Pourquoi les quatre noms de `/dev`** : `/dev/stdout`, `/dev/stderr` et `/dev/stdin` sont des liens « magiques » vers `/proc/self/fd`, que seul le noyau sait suivre, et `/dev/null` un périphérique caractère, que la règle du fichier ordinaire refuserait. Le refus d'une entrée de restauration tombe avant la confirmation, en code 2. Le propriétaire de l'entrée n'est pas contrôlé : un lien physique vers la sauvegarde d'un voisin appartient à root, qui est admis — seul `fs.protected_hardlinks` ferme ce cas —, et le refuser bloquerait un usage légitime |
| D84 | Clone sous garde | `site clone` ne **réécrit pas** la configuration applicative — ogs ne connaît ni `APP_SECRET`, ni les clés d'API, ni le relais de courrier — : il la **neutralise** et **dit ce qu'il n'a pas pu neutraliser**. Il ne recopie pas les fichiers que le type consacre à la configuration d'un environnement (`AppType.Config`), ni ses caches (`AppType.Cache`) ; il **masque à longueur égale**, dans la copie comme dans la déclaration, les secrets qu'ogs connaît de la source — les mots de passe qu'il a générés pour ses bases et son transport, les valeurs d'au moins 12 caractères de `/etc/ogs/env/<id>.env` (D75) ; il remplace la racine de la source par celle du clone dans la déclaration ; il reprend tâches et services **désactivés**. Il nomme, avec le **code 6**, chaque fichier retenu ou expurgé, chaque variable déclarée reprise, chaque lien dont la cible sort de la destination, chaque autre nom physique d'un fichier retenu déjà recopié, chaque secret qu'il n'a pas pu chercher et l'installeur exposé (`AppType.Installer`). La sortie dit, pour tout type, ce qui passe tel quel : les autres fichiers de `www/`, le reste des chaînes expurgées, le contenu de la base. `--dry-run` l'annonce | Précise §7.1.4. Recopié tel quel, `www/` faisait travailler la préproduction sur la base et la file de la production, `.env.local` et `parameters.php` compris (R4-05), et donnait ses secrets au compte de préproduction (S1R-10) ; les tâches de production partaient actives sur une copie des données (R1-98). **Pourquoi masquer plutôt que retirer** : une application privée de sa configuration offre son installeur — un WordPress sans `wp-config.php`, un Prestashop dont `install/` a survécu — à qui visite la préproduction, publique (D57). **Pourquoi à longueur égale** : elle préserve les fichiers creux, les chaînes PHP sérialisées et les liens physiques. **Pourquoi nommer les variables déclarées** : elles atteignent l'application par FPM ou systemd sans passer par sa configuration, et ogs n'en connaît pas le sens. **Pourquoi nommer les liens qui sortent** : la copie ne les suit pas, la préproduction les suivra — jusque dans le home de la production. **Pourquoi remplacer la racine** : la commande d'une application `custom` est absolue sous la racine du projet, et le clone lançait le programme de la production. **Écartés** : ne copier que le docroot (Symfony ne tournerait plus, et le docroot de Prestashop est `www`) ; les marqueurs `@ogs:` de `migrer.sh`, qui supposent un exploitant qui les a posés ; refuser le clone. **Limites** : `--with-data` recopie la base telle quelle — `ps_configuration` et `ps_shop_url` pour Prestashop —, ce qui est annoncé mais non réécrit ; les secrets qu'ogs ne connaît pas passent en clair dans les fichiers qu'aucun type ne consacre à la configuration, annoncés en bloc ; l'analyse des liens est lexicale |
| D85 | Durcissement : le groupe du lecteur | `ogs harden` **ne ferme rien sans le groupe du service qui lit** : `/etc/mysql` en root:**mysql**, `/etc/apache2` et `/etc/nginx` en root:**www-data**, `/etc/postgresql` au groupe postgres, **propriétaire laissé** ; `/etc/redis` en **0770**, groupe redis, propriétaire laissé, fichiers 0640 ; `/etc/letsencrypt` en **0711**, propriétaire root, groupe et fichiers inchangés, sans récursion. Une cible dont le compte ou le groupe est introuvable est **refusée**, avec son motif, au lieu d'être fermée par le seul mode. Le mode posé est l'**intersection** du constaté et de la cible — harden retire des droits, il n'en accorde aucun —, setgid et sticky conservés, le groupe posé avant le mode. Répertoire et fichiers sont ouverts sans suivre de lien et modifiés **par leur descripteur** | Révise D56, dont la table fermait des répertoires lus sous une identité non root sans leur donner le groupe du service. MariaDB (`User=mysql`) ignore **en silence** un fichier d'options illisible et repart sur ses défauts, à l'écoute de toutes les interfaces (R2-01). Les workers d'Apache et de nginx ouvrent à la requête `AuthUserFile`, `auth_basic_user_file`, les `RewriteMap` et le magasin de mod_md : fermés à `www-data`, le site protégé rendait une 500, `apachectl -t` restant au vert — là où le risque annoncé était « aucun » (R2-02). Redis réécrit sa configuration par renommage dans `/etc/redis`, que le paquet lui donne en 2770 (`CONFIG REWRITE`, Sentinel) (R2-03). Rendre à root la clé confiée au groupe ssl-cert, ou fermer aux autres le certificat renouvelé que certbot écrit en 0644 root:root, coupait à son redémarrage suivant tout démon non root qui les sert (R2-04). `pg_createcluster`, lancé sous postgres, crée des répertoires de version (R2-05). Une cible sans son groupe affichait « ✓ » sans rien faire, puis « à restreindre » à perpétuité (R2-06). **Pourquoi l'intersection** : avec `/etc/redis` en 0770, l'ancien calcul aurait donné l'écriture de groupe à un répertoire 0755 root:root. **Pourquoi par descripteur** : laisser `/etc/redis` à redis ouvre une course permanente — un lien posé entre le constat et l'écriture ferait poser par root le groupe redis et un mode lisible sur un autre fichier. **Contreparties** : `pg_wrapper` ne trouve plus le cluster pour un utilisateur de projet, donc un cron hors du port 5432 doit préciser `-p` ; un parc durci par une version antérieure n'est pas rouvert — `/etc/redis` en 0750 root:redis, `/etc/letsencrypt` en 0700 —, ogs ne desserrant jamais : la réparation est manuelle (INSTALLATION.md). `/etc/mysql`, `/etc/apache2` et `/etc/nginx` en root:root reçoivent en revanche leur groupe au prochain `--apply` |
| D86 | Gardes du pare-feu | Une déclaration qui couperait la session SSH en cours (adresse et port ensemble), bannirait son adresse, fermerait un port où sshd **écoute réellement** ou fermerait SSH à tous est refusée sauf **`--allow-lockout`**, option propre aux commandes `ogs firewall` (`plan`, `apply`, `ban`, `unban`) : le `--force` des fichiers retouchés (§6.4) ne la lève **jamais**, et le plan global jamais non plus. Les gardes jugent **ce qui restera ouvert** après le plan : les règles voulues de tout profil qui ouvre les ports visés, et les règles de l'administrateur qu'il laisse en place. **Changer les ports d'un profil qu'une règle emploie est un resserrement**, gardé comme un retrait. Sans session à protéger, une règle liée à une interface (hors `lo`) ou vers une adresse du serveur compte ; pour une session, la règle doit admettre son adresse et, si elle vise une adresse du serveur, celle que la session a jointe (3e champ de `SSH_CONNECTION`) — une règle liée à une interface ne la couvre pas, et le refus le dit. Le port de la session ne compte pas là où `ss` dit que sshd n'écoute plus. Deux moteurs sans `web.engine` ne rendent pas le profil web indisponible. La première activation pose un **témoin**, `/var/lib/ogs/ufw-unconfirmed` (0600 root), retiré par `confirm`, par la compensation et par `active: false` | Révise D72. **Pourquoi une option à part** : `--force` se donne pour un vhost ou une rotation retouchés, sans penser au pare-feu ; porté jusqu'au plan global, il levait la garde dans `ogs apply <id> --force`, `project delete --force` ou `mail dkim activate --force`, et retirait la dernière règle ssh qu'un apply sans lui refusait de retirer (R2-10). **Pourquoi les ports d'un profil** : `ufw app update` ferme l'ancien port ; un `Port 2222 # annoté` que la lecture de `sshd_config` prenait tel quel faisait retomber le profil à 22 et fermait le port où sshd écoutait, sans garde et en code 0 (R2-09). **Pourquoi compter les règles de l'administrateur** : ogs ne les retire jamais, donc l'accès qu'elles portent survit au plan ; les ignorer faisait refuser un `ufw limit OpenSSH` à chaque apply et chaque bannissement de qui gère ssh lui-même, et l'on prenait l'habitude de lever une garde qui emportait avec elle celle du bannissement de sa propre adresse. **Pourquoi deux questions** : sans session, la garde demande si le port restera ouvert du dehors — compter pour rien une règle `in on wg0` refusait, depuis la console, chaque bannissement de qui administre par un VPN ; pour une session, elle demande si la suivante passera par le même chemin, ce que rien ne dit d'une interface. **Pourquoi le port de la session ne compte pas partout** : sshd déplacé et redémarré, la session reste sur l'ancien port, et la garde refusait le plan même qui ouvrait le nouveau. **Pourquoi le profil web** : un profil tenu pour absent voyait ses règles retirées, et `firewall ban` retirait l'accès public de tous les sites, garde HTTP-01 comprise (R2-11). **Pourquoi un témoin** : l'unité transitoire du retour vit sous `/run` ; un redémarrage dans le délai l'efface, et ufw repart actif sans filet — `confirm` répondait « rien à confirmer », `status` et `doctor` se taisaient (R2-13). Le témoin distingue l'attente, le retour perdu et le retour joué — `confirm` rend alors 6 —, mais **ne réarme rien** : un redémarrage dans le délai ne vaut toujours pas retour (§20) |
| D87 | Docroots : liens et fichiers cachés | Le serveur web ne suit un lien symbolique du docroot que **vers ce qui appartient à son auteur** : Apache `Options -FollowSymLinks +SymLinksIfOwnerMatch`, posé sur la racine du projet comme sur le docroot ; nginx `disable_symlinks if_not_owner from=<racine du projet>`. Sous `AllowOverride All` (Prestashop), un `.htaccess` ne peut régler que `Indexes`, `MultiViews` et `SymLinksIfOwnerMatch` (`Options=`). Les fichiers et répertoires **cachés** — `.git`, `.env`, `.htaccess` — et `composer.json` et `composer.lock` ne sont servis par aucun vhost qui sert un répertoire (`php-fpm`, `static`), `/.well-known/` excepté : Apache par `RedirectMatch 403`, rendu dès la traduction de l'URL, avant tout contrôle d'accès, et pour `composer.*` par un `<If "%{REQUEST_FILENAME} …">` ; nginx par deux `location ~ … { deny all; }` en tête des expressions régulières. Sous nginx, le vhost PHP éprouve `$ogs_fichier` et non `$uri` dans `try_files` — les deux ne diffèrent que pour « / », qui devient le nom réservé `/.ogs-accueil/`, défini par le fragment de portée serveur (D50) —, et le vhost statique n'a plus de `try_files` | `www-data` est membre du groupe de chaque projet (D25) : suivre tout lien lui faisait servir publiquement les fichiers d'un voisin, désignés par un lien posé dans un docroot, que le pool du projet — sous son propre compte — ne pouvait même pas lire (S1R-01). Le contrôle part de la **racine du projet**, non du docroot : le home appartient à l'utilisateur (D51), qui remplace sans privilège le docroot lui-même par un lien, et Apache éprouve chaque composant avec les options du répertoire qui le contient ; `-FollowSymLinks` est explicite, Debian le posant sur `<Directory />`. Le déploiement documenté (`git pull`) place `.git` dans le docroot des sites `php-plain` et Prestashop (S1R-14). **Pourquoi `RedirectMatch` et `<If>`** : Apache fusionne les `<Location>` dans l'ordre du fichier, et un `<Location /> Require …` déposé en surcharge (D63) pour réserver le site à un réseau rouvrait un `<LocationMatch>` comme un `<FilesMatch>` ; MultiViews négocie « /composer » vers `composer.json` par une sous-requête sur le fichier, qui ne passe par aucun `RedirectMatch` — d'où un refus sur le nom du fichier, dans un `<If>`, qu'Apache fusionne après les `<Location>`. MultiViews ne s'exerce que dans un répertoire que `www-data` peut énumérer — le docroot en 0710 lui répond 403 (AH00686) —, soit un `vendor/*/composer.json` créé sous l'umask du projet. Le `RedirectMatch` n'est pas élargi à « /composer » sans extension, qui bloquerait des URL applicatives. **Pourquoi `$ogs_fichier`** : sous `disable_symlinks`, nginx ouvre un répertoire en lecture au lieu d'un `stat()`, et le docroot en 0710 ne se lit pas — l'accueil écrivait une ligne « crit », ou rendait 404. Une `location` dédiée à « / » l'aurait soustrait aux surcharges du docroot. **Changements visibles** : sous nginx, un site statique redirige un répertoire demandé sans barre finale (301) et répond 403 à un lien refusé ; un lien vers le fichier d'un autre compte ne répond plus ; un `.ogs-accueil/` posé par un projet Prestashop conduit son accueil à un 403. **Limite, portée au §20** : sous Apache, `SymLinksIfOwnerMatch` contrôle **le nom avant l'ouverture** — un lien permuté en boucle par un compte doté d'un shell (D62) a passé 126 fois sur 27 364 requêtes en dix secondes ; nginx compare sur le descripteur ouvert et n'y est pas sujet. La fermer demande de servir chaque projet sous une autre identité que `www-data`. Une surcharge ou un `.htaccess` qui poserait lui-même un `<If>` accordant l'accès rouvrirait la variante négociée : c'est un geste délibéré, et le `RedirectMatch` protège toujours l'URL directe |
| D88 | `--dry-run` tenu en un seul endroit | `--dry-run`, option globale, a **une conduite inscrite par commande** dans une table unique, `internal/cli/dryrun.go`, lue sur le chemin complet de la commande et jamais héritée d'un parent. **Simulation** : la commande décrit ce qu'elle ferait et s'arrête avant son hook « pre » et avant sa première écriture — `apply`, `site create`, `site clone`, `project create`, `import adopt`, `cert issue`, `php switch`, `cron add`, `db add`, `mail dkim enable` et `disable`, `amqp messenger`, `firewall apply`, `harden`. **Consultation** : l'option est sans objet, et l'audit d'une consultation (`secrets show`, `secrets env`, `my status`) est conservé. **Toute autre commande refuse l'option**, code 2, avant toute préparation — ni configuration, ni base, ni verrou —, y compris une commande ajoutée plus tard. Le `hook.Runner` lui-même résout le hook, contrôles de sécurité compris, et l'annonce « non exécuté » au lieu de le lancer | Précise D40 et §6.1. Une option globale est reçue par des commandes qui ne la lisent pas : `project delete --dry-run` supprimait pour de bon — archive, bases, vhost, compte, home, `preDelete` et `postDelete` en root — et rendait 0 ; `project restore` et `cert renew` de même (R3-20) ; `firewall ban --dry-run` réécrivait `ufw.yaml` (R2-14). **Pourquoi une table** : la conduite de chaque commande se relit d'un coup d'œil, et `TestDryRunChaqueCommandeSimuleOuRefuse` parcourt l'arbre des commandes en relevant le préfixe et les tables de la base d'état avant et après chacune. **Pourquoi refuser par défaut** : c'est le seul défaut sûr — une commande nouvelle ne peut pas ignorer l'option par oubli. **Pourquoi refuser plutôt que simuler** `project delete`, `project restore` et `cert renew` : une simulation fidèle rejouerait les remparts de D77, une demi-simulation serait trompeuse. **Pourquoi garder l'audit d'une consultation** : une divulgation non journalisée sous `--dry-run` serait une porte dérobée. **Pourquoi le `Runner`** : la règle de D40 tient ainsi même pour un appelant qui oublierait de s'arrêter, et la simulation dit ce que l'exécution rencontrerait — un hook inscriptible par tous reste refusé. Le verrou global n'est pas un état : `apply --dry-run` le prend pour calculer sur un système immobile. **Réserve** : comme `plan`, `diff` et `status`, `apply --dry-run` attribue dans la base d'état le numéro de pool ou le port d'un projet jamais appliqué — celui que l'apply lui donnera (§20). Les commandes qui éditent une déclaration (`cron remove`, `service add`, `db user add`…) refusent l'option : leur apprendre à simuler est une fonctionnalité à venir |
| D89 | Paquet et timer de renouvellement | Le paquet **ne tient pas l'état** du timer `ogs-renew`, qu'il ne livre pas : il le **note au retrait et le rend**. Le prerm n'arrête et ne désactive le timer qu'au seul `remove`, après avoir noté son état dans `/var/lib/ogs/ogs-renew.timer.avant-retrait` ; `upgrade`, `failed-upgrade` et `deconfigure` n'y touchent pas. Le postinst rend l'état noté — `config.yaml` présent ou non —, réactive un timer portant l'en-tête d'ogs quand `configure` reçoit une version inférieure ou égale à 0.0.3, et signale sur stderr un timer d'ogs laissé désactivé, sans passer outre. À une mise à jour, il lance `ogs init --quiet >/dev/null`, **sauf si la clé maître manque** : il le dit alors et laisse le choix à l'administrateur. Un postrm, qui n'agit qu'à la purge, retire les deux unités d'ogs reconnues à leur en-tête et le témoin, garde `/etc/ogs`, `/var/lib/ogs` et `/var/log/ogs` en le disant, et énumère les archives, que plus rien ne purge | Révise §17. Le prerm des versions jusqu'à 0.0.3 désactivait le timer sur `upgrade`, et rien ne le réactivait — apply ne demande l'activation qu'à l'écriture du fichier d'unité : après la première mise à jour, plus aucun certificat ne se renouvelait, sans qu'aucune commande ne le dise (R3-14). **Pourquoi réparer dans le postinst** : à une mise à jour, dpkg exécute le prerm de la version **en place** (`upgrade <nouvelle>`), puis le postinst de la nouvelle (`configure <ancienne>`) — un défaut de prerm ne se corrige que dans le postinst suivant. **Pourquoi un état noté plutôt qu'un drapeau** : un timer que l'administrateur a désactivé doit le rester à la réinstallation. **Pourquoi pas `deb-systemd-helper`** : le timer n'est pas une unité livrée, c'est le plan global d'`ogs apply` qui l'écrit. **Pourquoi un postrm** : sans postrm ni fichier de configuration, dpkg oubliait le paquet dès le retrait, et `apt purge` n'avait plus rien à faire (R3-17). **Pourquoi écarter la sortie standard d'`ogs init`** : apt donne à dpkg un pseudo-terminal, où toute commande d'ogs interroge le terminal sur sa couleur de fond ; sans répondant, comme sous unattended-upgrades, elle attend cinq secondes et laisse la requête dans le journal d'apt. **Pourquoi ne pas lancer `init` sans clé maître** : il en engendrerait une neuve sans un mot sous `--quiet`, et les secrets scellés ne se reliraient plus — c'est la neuve que l'administrateur sauvegarderait. **Dépendance au numéro** : une construction corrigée encore numérotée 0.0.3 réactiverait, à la mise à jour suivante, un timer que l'administrateur avait désactivé — d'où une étiquette au-delà de 0.0.3 avant toute diffusion (R3-19). Banc `packaging/scripts_test.go`, scénario `paquet` |
| D90 | Migration entre machines | La migration d'un ancien serveur vers une machine gérée par ogs est un **script livré à part**, `docs/migrer.sh`, piloté par un manifeste rempli hors du dépôt d'après `docs/migration.yaml`, exécuté en root sur la **cible**, qui tire fichiers et bases de la source par ssh ; rien n'est modifié sans `--executer`. (1) **Garde d'appartenance** : avant tout dépôt, un projet est refusé si la cible porte déjà ce projet sans que la migration l'ait déposé, si un autre projet déclare l'une de ses bases, son compte ou son home ou sert l'un de ses domaines — aux clés de `validate.Uniqueness`, host effectif d'une préproduction compris —, ou si son compte, son home, une base ou un utilisateur de base y existe déjà ; deux projets du manifeste ne partagent rien. (2) **La bascule est enregistrée** : engagée, elle fait refuser `synchroniser` — la cible fait foi — et une bascule reprise ne refait pas la copie finale ; `retour` est refusé sans bascule, met la cible en maintenance, rétablit tâches et workers sur la source, **attend le DNS** avant de rouvrir la source, et ne dépend pas d'un apply réussi sur la cible. (3) **Root n'écrit jamais par un chemin que fournit la source ou le compte du projet** : la source est recopiée dans un miroir du répertoire de travail, un sous-arbre par destination, jamais l'un sous l'autre, et le côté qui écrit dans le home tourne sous le compte du projet (`rsync -s`). (4) `ssh -n` partout où ssh ne porte pas un protocole ; socket de contrôle dans le répertoire privé de l'exécution, `ControlPersist` court, maître fermé en sortie ; valeurs de configuration et secrets passés par l'environnement du seul processus, jamais en argument. (5) `--oui` dispense de confirmer une intention, **jamais un fait que seul l'exploitant peut observer** — la propagation constatée sous `--sans-attente-dns` | Le script est antérieur à la revue du 25/09/2026, qui y a trouvé quatre critiques et un majeur : ssh consommait l'entrée standard des boucles `while read`, et seul le premier élément de chaque liste était traité (R3-01) ; `synchroniser` rejoué après la bascule supprimait la base de production et, par `rsync --delete`, les fichiers déposés depuis sur la cible (R3-02) ; sans garde, il supprimait la base d'un voisin ou reprenait un projet existant (R3-03) ; le socket de multiplexage, sous un nom stable de `/tmp`, laissait un compte local tenir le rôle de connexion maître pour les commandes root vers la source (S2R-04) ; les secrets passaient en argument de `python3`, lisibles de tous dans `/proc/<pid>/cmdline` (S2R-07). Root écrivait par le nom sous le home du projet, à rebours de D70 et D71, et un lien reçu de la source dans le miroir le menait hors du miroir (R3-07). **Pourquoi un script et non une commande d'ogs** : il pilote deux machines, dont une qu'ogs ne gère pas, pour un geste ponctuel ; il ne touche ogs que par ses commandes. **Pourquoi les clés de l'unicité** : une production et sa préproduction partagent `metadata.host` sans rien partager d'autre ; seule la comparaison qu'ogs lui-même fait ne se trompe ni dans un sens ni dans l'autre. **Pourquoi `--oui` ne suffit pas** : il vaut consentement, non constat ; un `retour` sous `--oui` rouvrait sinon la source sans que personne n'ait vu le DNS revenu. **Limites** : le script réplique `naming.EffectiveHost` en Python — aucune commande d'ogs ne rend un projet normalisé sans le déposer —, dérive qu'une telle commande supprimerait ; la lecture en root de la source suit un lien que le compte web de la source y aurait posé (`shared/uploads → /etc`), ce que la lecture sous son identité casserait sur les téléversements de `www-data`. Banc `test/migration` |
| D91 | Appartenance des bases et comptes SGBD | Un compte SGBD est **à ogs pour un projet** si son mot de passe est scellé dans la portée du projet — ogs ne le scelle qu'en créant le compte, ou en changeant le mot de passe d'un compte déjà sien ; une base l'est si elle existe et que ce compte y a ses droits (la propriété sous PostgreSQL, des privilèges sous MySQL). **Création** : hors projet adopté, une base ou un compte homonyme qui existe sans être à ogs n'est **pas repris** — étape non critique en échec, code 6, qui nomme chaque homonyme et les issues réelles (retirer ou renommer l'homonyme, déclarer d'autres noms ; l'import refuse un projet déjà déclaré) ; rien n'est exécuté, aucun mot de passe scellé. Le refus d'un **compte additionnel** ne vise que lui, dans une étape à part : la base et le compte principal restent réconciliés. Sous MySQL, l'ordre est **compte, GRANT, puis base** : le GRANT sur une base encore absente est accepté, et une création interrompue ne laisse jamais une base sans droits, qu'ogs tiendrait pour étrangère. **Compensation** : ne défait que ce que l'étape a créé — la base si elle était absente, les comptes absents à l'observation, et les mots de passe qu'elle a scellés. **Suppression** : hors projet adopté, ne part que ce qui est à ogs ; un homonyme reste en place, nommé, code 6. Un projet adopté emporte tout ce qu'il déclare, sous `--force` (D77). Le mot de passe d'un compte supprimé quitte le keystore — l'archive en garde la copie. Un compte réservé ne part **jamais**, `--force` compris. **Rotation** (`db user rotate`) : refusée sur un compte qui n'est pas à ogs, hors adoption, et sur un compte réservé. **Noms réservés** : `root`, `mysql`, `mysql.sys`, `mariadb.sys`, `debian-sys-maint`, `public`, `postgres`, tout rôle `pg_*`, `telegraf`, et les noms que la configuration désigne — compte et rôle de supervision (D67), rôle d'administration PostgreSQL (D44) —, casse ignorée. Refus de validation (code 3) **aux points d'écriture** d'une déclaration — `db add`, `db user add`, `project create`, `site create`, `project restore` —, l'import écartant la base avec une note ; **jamais au chargement** : une déclaration existante voit sa base refusée par le plan, étape non critique, rien d'exécuté. Un clone de préproduction ne reprend pas l'étiquette `adopted` | Révise §7.2, §7.3 et §13. La compensation d'une base neuve supprimait le compte qu'elle partageait avec une autre base du projet, celle de la production (R1-05) ; une base ou un compte homonyme était repris sans un mot, puis `DROP DATABASE`, `DROP USER` ou `DROP ROLE` à la suppression du projet (R2-39) ; `db user rotate shopfr root` changeait le mot de passe par lequel ogs administre MariaDB — `ALTER USER … IDENTIFIED BY` **remplace** l'authentification `unix_socket`, et ogs perdait l'accès au moteur pour tout le parc — et le scellait comme un secret du projet, que `secrets env` remettait à son application (S2R-05). **Pourquoi le mot de passe scellé** : c'est la seule trace qu'ogs laisse de chaque compte qu'il crée depuis toujours, donc elle reconnaît sans migration un parc antérieur ; elle voyage avec l'archive et la restauration, comme les clés DKIM (D69) ; et elle dit ce qui compte — ogs n'est maître que d'un compte dont il tient le mot de passe. **Pourquoi les droits pour la base** : une base n'a pas de mot de passe, et le seul compte scellé ne suffit pas — deux bases d'un projet peuvent partager un compte, et une base préexistante déclarée sous ce compte serait devenue la sienne. **Pourquoi pas au chargement** : `validate.Project` juge tout projet chargé, et un seul projet déclarant `root` rendait le code 3 à `plan`, à l'`apply` du parc et d'un projet sain, à `cert renew` — le timer ne renouvelait plus rien — et à la suppression du fautif comme d'un voisin, dont le plan global ne se recalculait plus (fail2ban, logrotate). **Pourquoi jamais `DROP USER` d'un compte réservé** : un projet d'avant D91 qui le déclarait en tient un mot de passe scellé, et passe donc pour en être le maître. **Contreparties** : un compte repris en silence avant D91 a reçu un mot de passe scellé et passe pour être à ogs — il part avec le projet ; l'éprouver en se connectant avec ce mot de passe n'est pas fiable (l'authentification locale de PostgreSQL est `peer`, un compte MySQL peut n'accepter que certains hôtes) et reste à faire. Une base dont on a retiré à la main les droits du compte est refusée comme étrangère. Un projet créé par ogs ne peut pas adopter une base isolée (§20) |
| D92 | Commandes SGBD : sockets, restauration, propriétaire PostgreSQL | (1) **Root ne se connecte qu'au socket du vrai moteur** : `/tmp` est retiré des emplacements cherchés, et un socket n'est admis que s'il appartient à root ou au compte du moteur (`mysql`, `postgres`), dans un répertoire qui leur appartient et qu'aucun autre ne peut écrire — sticky bit ou non —, le groupe du moteur admis (`/var/run/postgresql` en 2775). Un socket refusé rend le moteur **indisponible** en le disant, sans repli sur le répertoire par défaut. (2) **`db restore` est confiné au projet** : le script est chargé sous un compte de la base au jeu `all`, avec son mot de passe scellé passé par l'environnement : le **compte principal**, par un client lancé sous l'**utilisateur Linux du projet** (`mysql --no-defaults` par le socket admis, `psql --no-psqlrc` par TCP comme l'application) ; à défaut — principal `crud` ou `readonly`, un compte additionnel `all` portant les migrations —, ce compte additionnel, par un client que **systemd lance sous une identité jetable** (`systemd-run --pipe --wait -p DynamicUser=yes`, mot de passe dans un fichier d'environnement de root en 0600 sous `/run`), jamais sous l'utilisateur du projet, qui lirait ce mot de passe dans l'environnement du client. Sans systemd ni son bus, ou sans compte `all`, la restauration est refusée en nommant la sortie ; `project restore` compte une base non rechargée en succès partiel (code 6). Le mot de passe n'est présenté qu'au vrai moteur : un socket admis est exigé, et une connexion TCP exige que toute écoute qui peut la recevoir sur la boucle locale appartienne au moteur (`/proc/net/tcp*`) ; PostgreSQL y ajoute `PGREQUIREAUTH=scram-sha-256`, que la libpq antérieure à 16 ignore. Il en va de même pour `site clone` et `project restore`. Une base **adoptée** n'est restaurable qu'une fois son mot de passe repris par `ogs db user rotate <id> <compte>`, que le refus nomme. Deux adaptations du script : les clauses `DEFINER` de MySQL sont retirées en tête des seules lignes de définition, l'objet revenant au compte qui restaure ; le `COMMENT ON EXTENSION` que pg_dump pose sous son en-tête est retiré. `pg_dump` ne porte plus de propriétaire (`--no-owner`), et la sauvegarde d'un clone ni privilèges (`--no-privileges`). Un socket MySQL refusé n'est pas une absence : la supervision et l'import l'annoncent. (3) **PostgreSQL : le compte principal est propriétaire**, et un jeu `crud` ou `readonly` sur lui est **refusé** — par `db add` en code 2, par le plan en étape non critique qui ne change rien à la base en place ; le jeu borné se déclare sur un compte additionnel | (1) Tout compte local pose un socket dans `/tmp` au nom attendu : le moteur arrêté le temps d'une mise à jour, root s'y connectait et lui envoyait les mots de passe du parc, en clair dans les `CREATE USER` et `ALTER ROLE`, puis le contenu des bases d'un clone. (2) Chargé par le super-utilisateur du moteur sous root, le fichier restauré écrivait dans toutes les bases et lançait une commande shell par `\!` ; sous le compte et l'utilisateur du projet, le moteur ne lui ouvre que sa base, et ce que le client exécute de lui-même ne peut rien que l'utilisateur ne puisse depuis son shell (D62). Le mot de passe dans l'environnement n'est lisible que de l'identité du client : l'utilisateur du projet pour le compte principal, dont il détient déjà le mot de passe ; un uid jetable, qu'aucun compte n'atteint et dont systemd tue tout processus à l'arrêt, pour un compte additionnel. Le port d'un moteur arrêté est à quiconque s'y lie : sans ces gardes, la restauration livrait le mot de passe scellé puis le script à un faux serveur. (3) La propriété emporte tous les droits, qu'aucun `GRANT` ne retire : `readonly` était annoncé par le plan et sans effet. Un **rôle propriétaire distinct**, qui aurait rendu le jeu borné effectif, retirerait au compte de l'application le droit de créer des tables — ses migrations — au premier apply d'un site en place ; un **refus à la validation** bloquerait tout le parc, `loadProjects` rejetant le premier projet invalide |
| D93 | Ressources du broker et secrets d'administration du parc | (1) **Ce qu'ogs n'a pas créé sur un moteur, il ne le reprend pas et ne le supprime pas.** Le broker ne porte aucune marque : ce sont des **secrets scellés au keystore du projet** qui font foi, chacun pour sa ressource. Le **compte** AMQP est à ogs si son mot de passe `amqp/<user>` est scellé ; le **vhost**, si sa marque `amqp-vhost/<vhost>` l'est — ou, dans un projet qui ne porte **encore aucune** marque de vhost (parc antérieur à D93), si le compte d'ogs y a déjà ses permissions ; le premier plan qui admet un vhost ainsi en scelle la marque (étape de migration), et le critère s'éteint pour ce projet. Les deux secrets sont scellés **avant** la création de leur ressource, pour qu'une interruption ne laisse jamais une ressource d'ogs sans preuve ; la marque d'un vhost dont la création échoue est retirée. Un homonyme sans preuve fait de l'AMQP du projet une seule étape **non critique en échec** (code 6), sans rien déclarer dessus, pas même les permissions ; `project delete` juge vhost et compte chacun sur sa preuve, laisse l'homonyme et le dit, retire ce qui est à ogs **avec son secret** ; `amqp queue purge` refuse (code 4) et `amqp status` n'affiche pas ses files ; `RotatePassword` refuse. Même règle pour les **comptes de supervision** MySQL et RabbitMQ (`monitoring.telegraf.account`) : un homonyme sans secret scellé au keystore `@parc` n'est ni modifié (ni `ALTER USER`, ni tags) ni porté au fragment ; le secret d'un compte neuf est scellé avant `CREATE USER` ou la pose sur le broker, et le fragment ne porte l'entrée qu'une fois le compte **observé conforme ou posé**. (2) **Sur cette machine, les identifiants d'administration ne partent que vers le broker** : chaque connexion à l'API de management est vérifiée après établissement — le socket accepté, cherché dans `/proc/net/tcp{,6}` par ses deux adresses, doit appartenir à **root ou au compte du broker** (`brokerAccount` d'`amqp.yaml`, `--broker-account`, nom ou uid, défaut `rabbitmq`), quelle que soit l'adresse de l'URL ; introuvable, la connexion n'est admise que vers une adresse qui n'est pas celle de la machine, et refusée sinon (redirection DNAT) en nommant le remède ; refusée, la requête n'est pas envoyée et le broker est tenu pour indisponible. (3) **Les secrets d'administration du parc** — mot de passe du broker, secrets OVH — **se saisissent hors de la ligne de commande** : sans écho sur un terminal, sur l'entrée standard sinon (une ligne par secret, dans l'ordre de l'aide) ; une interruption pendant la saisie n'enregistre rien (code 9) ; les options porteuses de valeur restent acceptées mais sont signalées | Un vhost homonyme de l'administrateur était repris sans secret puis **détruit avec ses messages** par `project delete` (R2-40) ; le compte « telegraf » de l'administrateur recevait un mot de passe neuf et perdait ses tags (R2-41). Le port 15672 n'est pas privilégié : broker ou plugin arrêté, l'utilisateur d'un projet (qui a un shell, D62) l'ouvrait et recevait l'authentification Basic de l'administrateur et le mot de passe de chaque compte de projet (S1R-07) ; une option de ligne de commande reste dans le journal de sudo et l'historique de root (S1R-11). **Pourquoi une preuve par ressource** : le mot de passe prouve le compte, pas le vhost — un `spec.amqp.vhost` changé vers le vhost d'un autre, ce que le refus lui-même conseille, ouvrait ce vhost au compte du projet puis le supprimait. **Pourquoi des secrets et non une marque sur le broker** : ils existent pour tout compte qu'ogs a créé, et les permissions pour tout vhost — le parc existant ne voit au premier plan qu'une étape, la pose de la marque —, et une archive les restaure avec le projet ; une restauration sans ses secrets fait refuser, le sens prudent. **Pourquoi le critère des permissions s'éteint** : sans borne, il suffisait que l'administrateur accorde au compte du projet une permission, même restreinte, sur son vhost commun, puis que `spec.amqp.vhost` le désigne, pour que le plan y élargisse les droits et que la suppression l'emporte. **Pourquoi sceller avant** : scellé après, un `CREATE USER` passé suivi d'un `GRANT` en échec laissait un compte d'ogs sans preuve, refusé pour toujours. **Pourquoi l'autre bout de la connexion et non l'écoute** : relire l'écoute puis se connecter laisse une fenêtre, le broker s'arrêtant entre les deux ; le socket accepté hérite de l'uid du socket d'écoute. **Pourquoi pas rabbitmqctl** : sans API de management, ni arguments de file, ni exchanges, ni bindings. **Limites** : un broker distant n'est pas couvert — la liaison relève alors de l'URL (HTTPS) ; au premier plan d'un projet antérieur à D93, un vhost où son compte a reçu des permissions — de l'administrateur compris — est tenu pour le sien et marqué ; `project restore` réinjecte marque et mot de passe avec les autres secrets, si bien qu'un homonyme créé entre la suppression et la restauration, ou présent sur la machine où l'on restaure, serait adopté |
| D94 | Unicité, certificats et ports | (1) **L'unicité s'éprouve contre le parc entier**, jamais contre les seuls projets visés : `plan`, `apply` (un identifiant ou `-f`), `db add`, `amqp messenger`, `site clone`, `project restore`, `project create`, `site create`, `import adopt`, `applyProjects` sous le verrou global, et la TUI (application sous le verrou, assistant avant l'écriture) — la déclaration visée remplaçant celle de même identifiant. N'y comptent que les conflits où un projet visé est **détenteur ou demandeur** (`validate.UniquenessFor`) : une collision entre deux voisins est refusée à leur commande, non à celle d'un troisième. (2) **Le nom d'un certificat de projet est un nom de répertoire, unique dans le parc** : n'est refusé que ce qui sort de l'espace du projet — `/`, un `.` en tête (`.`, `..`), `@…` (référence d'un partagé), `_shared-…` (son répertoire) —, ainsi que deux certificats d'un projet sous un même nom (deux sans nom prennent tous deux l'identifiant), et seulement pour les projets visés par `plan`, `apply` et les créations (`validate.CertificateNames`), **jamais au chargement du parc** ; `tls.Provider.Requests` reprend la règle du nom, et le renouvellement nomme alors le projet sans arrêter le parc. `doctor` nomme les déclarations à corriger. La clé d'unicité « certificat » réserve à chaque projet son identifiant, nom de son certificat implicite, en plus de ceux qu'il déclare. (3) **Le disque fait foi quand la base d'état se tait** : avant la première attribution, les ports des unités applicatives (`Environment=PORT=`) et les numéros des pools écrits (`listen =`, port ou nom du socket) sont repris dans `state.db` (`Store.Claim`), le projet lu sur l'en-tête (D76), le runtime sur le code du port ; un numéro déjà attribué en base l'emporte, et un numéro tenu par une autre ressource n'est jamais repris. (4) **Un port neuf n'est jamais un port en écoute** : l'attribution d'une application et d'un pool `listen: tcp` écarte les ports que la machine tient déjà. (5) **Une écoute se juge à son compte** : la colonne uid de `/proc/net/tcp{,6}`, pour une adresse qui reçoit 127.0.0.1 (boucle locale ou joker) ; une écoute d'un autre compte que celui du projet **ou que root** fait de l'attente de disponibilité une **étape critique en échec**, qui arrête l'apply avant le vhost, `waitReady` la refuse au lieu de la tenir pour prête, et `doctor` la nomme — en relisant le port dans l'unité écrite, sans rien attribuer ni écrire dans la base d'état | Contrôler la seule liste visée comparait `apply blogfr` ou `plan -f` à lui-même : un domaine, une base, un home tenus par un voisin entraient sans un mot, et `db add --role api` sur `shop` déclarait `shop_api`, base du projet voisin, dont l'apply donnait les droits au compte de `shop` (R1-07). Le nom d'un certificat, libre, visait le wildcard partagé ou sortait du magasin — l'émission y écrit une clé privée, sa compensation y fait un `RemoveAll` —, et un projet qui nommait son certificat d'après l'identifiant d'un voisin écrasait le sien (R1-08). Une base d'état perdue ou recréée redistribuait ports et numéros dans l'ordre alphabétique : le premier projet calculé prenait le port en service chez un voisin, et chaque site était servi par l'application de l'autre (R2-18). Le port suivant d'une plage est prévisible : un compte local qui l'écoutait d'avance, ou le reprenait pendant les cinq secondes d'un redémarrage, passait la sonde — une connexion qui aboutit — et recevait les requêtes du vhost, cookies et en-têtes d'authentification compris (S2R-02). **Pourquoi le disque plutôt que le YAML** : écrire le numéro dans la déclaration ferait écrire à un `plan` le fichier du projet ; l'unité et le pool sont ce que le vhost d'un voisin sert déjà. **Pourquoi l'identifiant réservé** : le certificat implicite d'un vhost TLS porte ce nom sans être déclaré. **Pourquoi ne refuser un nom de certificat qu'au projet visé** : le chargement du parc précède `cert renew --all`, `status` et le recalcul du parc à la suppression d'un voisin (D68) ; une seule déclaration fautive les arrêtait tous en code 3, et un nom comme `*.cc.fr`, qui fonctionnait, devait continuer de fonctionner. **Pourquoi root est admis** : il n'est pas l'attaquant local de S2R-02, et c'est sous root que docker-proxy publie le port d'une application lancée par `docker run -p`. **Pourquoi `/proc/net` et non `ss`** : il donne l'uid sans outil ni analyse de sortie, et se lit en mode simulé comme un fichier de test. **Limites** (§20) : l'étape critique ne vaut qu'au plan — une écoute prise entre le plan et le démarrage fait échouer l'attente, non critique, et le vhost d'un projet neuf est écrit ; un pool FPM `listen: tcp` n'est pas éprouvé à son compte ; rien ne réserve un port au noyau |
| D95 | Home préexistant, ressources non gérées | **Un home tenu par un autre compte n'est jamais repris par la création d'un compte** : quand l'utilisateur d'un projet non adopté reste à créer, sa racine est refusée (code 4), avant toute étape, si elle appartient à un compte existant autre que root, ou si l'une de ses entrées de premier niveau appartient à un **compte ordinaire** existant — dans la plage `UID_MIN`–`UID_MAX` de `login.defs`, hors `www-data` ; le refus nomme les entrées et leur compte. Il est prononcé par `site create`, `site clone` et `project create` **avant le dépôt de la déclaration** (et donc sous `--dry-run`), puis par le plan. Une racine à root ou à un UID orphelin, dont les entrées ne sont à aucun compte ordinaire, n'est **reprise, et annoncée**, que vide, connue de la base d'état pour ce projet **actif** à ce chemin — une ligne archivée par `project delete` sans `--purge` ne compte pas —, ou réduite au **squelette d'ogs** — répertoires de `spec.layout.dirs`, `.local/bin` ne portant que des liens, `.nvm`, `.rbenv`, `.pyenv`, les anciens `.bin` et `logs`, le lien `~/logs`, et dans `.ssh` la seule clé de déploiement, `known_hosts` et le lien vers le trousseau commun ; tout autre contenu, et d'abord un `authorized_keys` régulier, fait refuser, nommé : l'étape dit « existant, repris : propriétaire → compte (créé par ce plan) ». `project delete` refuse, sans que `--force` le lève, une racine dont une entrée de premier niveau appartient à un compte ordinaire existant autre que celui du projet (même critère, `system.ForeignEntries`). **Aucun registre des ressources non gérées** : l'import n'en mémorise rien, `import unmanaged` inspecte le serveur au moment de la demande, et les conflits se jugent sur le système à chaque plan — compte sans la marque `ogs=`, home tenu par un autre compte, domaine servi par un vhost étranger, fichier sans en-tête, compte MySQL homonyme sous un autre hôte (D78) | Applique §7.1.7 et révise D20 et §9. `useradd --no-create-home` suivi de la pose de l'arborescence donnait au nouveau compte le home qu'un autre avait laissé à cet emplacement, sa racine chownée sous une étape « mode » ou « conforme » ; le contrôle du propriétaire de la racine à la suppression (D77) y voyait ensuite le compte du projet, et tout partait (R2-42). **Pourquoi le propriétaire et non le contenu** : un home non vide à un UID orphelin est ce que laissent un `userdel` à la main, une saga de création déroulée — runtime et lien de `~/.local/bin` ne sont pas compensés —, un rollback arrêté sur une compensation en échec, une suppression qui a retiré le compte sans le home ; l'apply les réparait, et refuser tout home non vide, comme la lettre du §7.1.7, l'en empêchait. **Pourquoi le squelette à un UID orphelin** : c'est aussi le propriétaire du home d'un compte supprimé sans `-r`, et useradd redonne volontiers l'UID libéré : le projet recevait les fichiers de l'ancien titulaire et son `authorized_keys` régulier — jamais remplacé (D61), accepté par `StrictModes` —, donc un shell sous son compte à la clé de l'ancien titulaire, puis `project delete` effaçait le tout. La base d'état reconnaît le home d'un projet déjà appliqué dont on a supprimé le compte à la main ; le squelette, celui d'une saga de création déroulée, qui n'a rien inscrit. **Pourquoi ignorer les comptes système** : un site adopté servi par mod_php a ses fichiers à `www-data`, et n'est pas pour autant le home de quelqu'un ; le refus à la suppression, que `--force` ne lève pas, le rendait insupprimable. **Pourquoi pas en mode préfixé** : useradd y est simulé, le compte n'existe jamais, et chaque apply serait une création sur l'arborescence posée par le précédent. **Pourquoi le premier niveau** : c'est là que se voit un home repris — la racine au projet, les fichiers de l'autre compte non —, et au-delà, ce qu'ogs ne pose pas est le contenu du projet. **Pourquoi retirer le registre plutôt que le relire** : écrit par la seule adoption, il ne portait ni les autres candidats, ni ce qui apparaît ou disparaît après l'import, ni rien sur un serveur jamais importé ; un conflit jugé sur lui aurait refusé à tort ce qui avait disparu et laissé passer ce qui était apparu, en se donnant pour une garantie. Chaque ressource qu'il nommait a sa garde sur l'état réel (R2-38). **Pourquoi une ligne active** : la ligne archivée garde le home, que le chemin a pu changer de mains depuis ; une restauration, un clone ou une déclaration redéposée reprenaient alors les fichiers de son nouveau titulaire. **Contreparties** : un home repris l'est avec le contenu de ses répertoires de projet (`www/`…), qui partira avec le projet — le plan l'annonce ; un projet dont le compte a été supprimé à la main après que la base d'état l'a perdu, et dont l'utilisateur a laissé des fichiers au premier niveau (`.bash_history`), est refusé — à rendre à root ou à reprendre par `ogs import` ; un home tenu par un compte système (hors root, pour la racine) ne retient rien ; un home repris avant D95 dont toutes les entrées sont au projet n'est plus reconnaissable. Le contenu de `.nvm`, `.rbenv` et `.pyenv` n'est pas jugé : un binaire de runtime qui s'y trouve fait passer l'installation pour faite, et l'unité l'exécuterait sous le compte du projet — reprendre un tel home suppose de vider ces répertoires. La table `unmanaged` de la base d'état n'est plus lue ni écrite |
| D96 | Environnement applicatif de la préproduction | `APP_ENV` (`SYMFONY_ENV` pour Symfony 3.4) vaut **`prod` dans tous les environnements**, préproduction comprise, dans le pool PHP comme dans les unités systemd ; le défaut `phpEnv: { APP_ENV: dev }` de l'environnement `prep` est retiré. Une valeur autre se déclare par projet (`spec.php.pools[].env`, `env.` des paramètres d'un service) | Révise §7.1.4. Une préproduction est publique (D57) : en `dev`, elle publiait le profiler et les pages d'erreur détaillées, et un code installé sans les dépendances de dev (`composer install --no-dev`, celui qu'on déploie) ne démarrait pas — le bundle du profiler est introuvable (R4-03). La préproduction sert à éprouver ce qui partira en production, donc sous la même configuration |

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
    shell: /bin/bash                # défaut de « user.shell » (D62) ; /usr/sbin/nologin pour fermer
    home: /home/shop.fr           # = /home/<host effectif>
    webAccess: group              # group | acl | none  (accès de www-data, §7.1)
    authorizedKeys: []            # optionnel

  layout:
    root: /home/shop.fr
    docroot: /home/shop.fr/www/public
    logDir: /var/log/ogs-projects/shopfr   # root:<id> 0750, hors du home (D71) ; ~/logs y renvoie
    mode: "0711"                  # racine : le propriétaire seul énumère, les autres traversent
    dirs:                         # créés avec propriétaire et mode explicites
      - { path: www,        mode: "0710" }   # le groupe traverse, il n'énumère pas
      - { path: www/public, mode: "0710" }   # selon le type d'application
      - { path: var,        mode: "0700" }
      - { path: logs,       mode: "0710", owner: root }
      - { path: .local,     mode: "0700" }
      - { path: .local/bin, mode: "0700" }   # liens gérés (D70), au compte du projet
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
      host: localhost             # hôte du compte ; celui du compte réel pour une base adoptée (D78)
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

  web:                            # le moteur est celui de la machine (D73)
    vhosts:
      - name: shopfr
        # file: shop.fr           # vhost adopté seulement : nom réel de son fichier (D76)
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

  mail:
    dkim:                         # opt-in, refusé en prep (D69, §7.13)
      enabled: true
      domains: [shop.fr]          # défaut : les domaines primary, jamais les redirect

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
| Host effectif | `shop.fr` (+ alias) | `shop.preprod-pp.net` |
| Home | `/home/shop.fr` | `/home/shop.preprod-pp.net` |
| Identifiant (tout le reste) | `shopfr` | `shopfrprep` |
| Certificat | propre au projet | wildcard partagé `*.preprod-pp.net` |

La préproduction vit sur un **domaine dédié**, configuré globalement et non par projet :

```yaml
# /etc/ogs/config.yaml
user:
  shell: /bin/bash                                   # défaut des comptes de projet (D62)
environments:
  prod:
    hostTemplate: "{{.Host}}"                        # host canonique tel que saisi
  prep:
    domain: preprod-pp.net
    hostTemplate: "{{.PrepLabel}}.{{.Domain}}"       # → shop.preprod-pp.net
    idSuffix: prep                                   # → shopfrprep
    certificate: "@wildcard-prep"                    # certificat partagé (§7.7.4)
    robotsNoindex: true
    presetDowngrade: true                            # medium → small
```

Points à noter :

- Le sous-domaine reprend le host **de production privé de son extension** (`shop.fr` → `shop`, D58) : le
  domaine `preprod-pp.net` porte déjà l'information d'environnement, et l'extension n'apporte rien à un nom
  qui vit sur un domaine dédié. Le sous-domaine ne porte donc pas l'identifiant de la préproduction :
  celui des ressources système reste `shopfrprep`, seul moyen de faire coexister les deux environnements
  sur un même serveur.
- Le home suit la règle invariante `/home/<host effectif>`, donc `/home/shop.preprod-pp.net`.
- Le host de préproduction n'est **jamais** soumis à la dérivation du §3.3.1 : il est construit, pas dérivé.
  L'identifiant reste calculé depuis le host canonique de production, ce qui évite toute circularité.
- Un host **déjà posé** sur le domaine de préproduction est pris tel quel (D58), et lui seul dérive alors
  l'identifiant : `jardindeco.preprod-pp.net --env prep` donne le host inchangé, le home
  `/home/jardindeco.preprod-pp.net` et l'identifiant `jardindecoprep`. C'est l'unique cas où l'identifiant
  ne vient pas d'un host de production, faute qu'il en existe un.
- Deux hosts ne différant que par leur extension (`shop.fr` et `shop.com`) revendiquent le même
  sous-domaine de préproduction. La collision est refusée par le contrôle d'unicité du host effectif
  (§3.3.6) ; l'un des deux doit alors employer `--host`.
- `--host` permet de forcer un host de préproduction hors convention (client exigeant son propre
  sous-domaine) ; le certificat partagé ne s'applique alors plus et un certificat propre est émis.

#### 3.3.4 Tableau de nommage

`<id>` désigne l'identifiant unique du projet (`shopfr`, ou `shopfrprep` en préproduction).

| Ressource | Nom | Exemple | Contrainte |
|---|---|---|---|
| Identifiant projet | dérivé du host | `shopfr` | `[a-z][a-z0-9]{1,27}`, ≤ 28 (+4 si `prep`) |
| Fichier projet | `<id>.yaml` | `/etc/ogs/projects/shopfr.yaml` | — |
| Home | `/home/<host effectif>` | `/home/shop.fr` | traversable, non énumérable, voir §7.1 (D51, D60) |
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
| Logs du projet | `<logs.root>/<id>/` | `/var/log/ogs-projects/shopfr/` | chaîne root, §14.5 ; lien `~/logs` (D71) |
| Lien interpréteur | `/home/<host effectif>/.local/bin/php` | `/home/shop.fr/.local/bin/php` | répertoire de l'utilisateur (D70) |
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
vhost AMQP, chaque nom de pool et de socket, chaque nom d'unité systemd, chaque **fichier de vhost** — le
vhost `b2b` du projet `shop` s'écrit dans `shop_b2b.conf`, fichier principal d'un projet `shop_b2b`, et le
nom réel d'un vhost adopté compte aussi (D76) —, chaque **nom de certificat**, l'identifiant de chaque projet
compris, nom de son certificat implicite (D94) ; aucun home n'est **sous** celui d'un autre projet (D77).

Le contrôle porte **toujours sur le parc entier** (D94), la déclaration en cours remplaçant celle de même
identifiant : `plan` et `apply`, qu'ils visent un identifiant ou un fichier (`-f`), `db add`, `site clone`,
`project restore`, `amqp messenger`, la TUI, et l'apply que ces commandes enchaînent, repris sous le verrou
global. Éprouver la seule liste visée comparait un projet à lui-même. N'y comptent que les conflits où un
projet visé est détenteur ou demandeur : une collision entre deux voisins arrête leurs commandes, pas celle
d'un troisième, ni la création d'un projet sans rapport (`project create`, `site create`, `import adopt`).

Deux collisions réelles, refusées en code 4 avec le nom du projet détenteur et la suggestion d'un `--id`
explicite :

- **Identifiants convergents** : les tirets étant supprimés, `mon-shop.fr` et `monshop.fr` produisent tous
  deux `monshopfr`. La troncature par hachage ne protège que les noms longs ; les collisions entre noms
  courts doivent être détectées et rejetées.
- **Empiètement sur le domaine de préproduction** : un projet de production dont le host canonique se
  termine par `.preprod-pp.net` est refusé, ce domaine étant réservé à l'espace de noms de préproduction
  (§3.3.3). En préproduction, le même host est au contraire pris tel quel (D58) : c'est le domaine, non
  l'environnement, qui décide, et le message de refus indique de déclarer le projet avec `--env prep`.
- **Collision entre extensions en préproduction** : `shop.fr` et `shop.com` déclarés tous deux en `prep`
  revendiquent `shop.preprod-pp.net`. La clé d'unicité « host effectif » les sépare. Elle est lue sur le
  domaine primaire de la déclaration et seulement calculée à défaut : un projet déclaré avant un changement
  de convention sert toujours le nom inscrit dans son fichier, et le recalculer inventerait des conflits
  entre des vhosts qui n'en ont aucun.
- **Homes imbriqués** : un import en produit sans y penser — un docroot `/var/www/boutique` devient un home,
  celui de `blog.boutique.fr` (`/var/www/boutique/blog`) un autre, dessous —, et `project delete` du premier
  effaçait le second, toujours déclaré et servi (D77). Un parc écrit avant ce contrôle peut en porter : le
  refus vaut alors pour toute commande qui éprouve l'unicité, même au sujet d'un troisième projet, et le
  message dit que le conflit peut précéder la commande en cours. Le remède est de corriger
  `spec.layout.root` de l'un des deux.

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
  backups/                                0700 — sauvegardes de --force, jamais à côté de l'original (§6.4)
  ufw-unconfirmed                         0600 — témoin d'une activation d'ufw jamais confirmée (D86)
  ogs-renew.timer.avant-retrait           état du timer noté par le prerm au retrait du paquet (D89)

/var/log/ogs/
  ogs.log                                 log applicatif structuré
  audit.log                               journal d'audit append-only

/run/ogs/                                 0755 root:root
  ogs.lock                                verrou global d'exécution
  <id>/                                   0710 <id>:<id> — répertoire d'exécution du projet
    <job>.lock                            verrous de crons
    app.sock                              socket de l'application non-PHP (§7.11)

/var/log/ogs-projects/                    racine des journaux des projets (logs.root), 0711 root:root (D71)
  <id>/                                   0750 root:<id> — apache, nginx, php, crons (§14.5)

/home/                                    racine des homes, 0711 root:root (D60)
/home/<host effectif>/                    racine projet, 0711 <id>:<id> (docroot simple)
  public/                                 docroot, 0710
  var/                                    0700, hors de portée de www-data
  logs → /var/log/ogs-projects/<id>       lien de commodité, au compte du projet (D71)
  .local/bin/php                          lien vers l'interpréteur de la version courante (D70)
```

Fichiers générés en dehors de l'arborescence `ogs` (toujours marqués par un en-tête de gestion) :

```
/etc/apache2/sites-available/<name>.conf   + lien dans sites-enabled
/etc/nginx/sites-available/<name>.conf     + lien dans sites-enabled
/etc/php/<ver>/fpm/pool.d/<pool>.conf
/etc/systemd/system/ogs-<id>-<svc>@.service
/etc/systemd/system/ogs-<id>.target
/etc/cron.d/ogs-<id>
/etc/opendkim/ogs/KeyTable                 clés DKIM de tout le parc (D69)
/etc/opendkim/ogs/SigningTable             domaines signés de tout le parc (D69)
/etc/ufw/applications.d/ogs                profils d'application ogs-* (D72)
```

Les règles d'ufw ne sont pas des fichiers d'ogs : elles passent par la commande ufw et se reconnaissent à
leur commentaire, qui commence par `ogs:` (§7.14).

Les clés privées DKIM (`/etc/opendkim/keys/<id>/<domaine>/<sélecteur>.private`, 0600 opendkim:opendkim)
sont écrites par ogs mais ne portent **pas** d'en-tête : un fichier PEM n'a pas de syntaxe de commentaire
sur laquelle compter. La dérive se mesure contre la clé scellée dans le keystore (§7.13).

Répertoires **créés** par ogs mais dont il n'écrit aucun fichier — les surcharges de vhost (D63), en
0750 root:root, dont le contenu appartient à l'exploitant :

```
/etc/<moteur>/projects/<name>/vhost/*.conf     contexte du vhost : server, <VirtualHost>
/etc/<moteur>/projects/<name>/docroot/*.conf   contexte du répertoire servi : location /, <Directory>
```

Le masque est ce qui rend l'inclusion facultative : un répertoire vide ou absent ne fait broncher aucun
des deux moteurs, là où un chemin littéral introuvable est fatal. Les contextes existants dépendent du
template — un vhost de proxy n'a que le premier, un vhost de redirection n'en a aucun — et sont **déduits
du vhost rendu**, ce qui laisse un template surchargé libre de n'en offrir aucun.

En-tête obligatoire de tout fichier généré :

```
# Généré par ogs — NE PAS MODIFIER À LA MAIN
# projet: shopfr  ressource: web/vhost/shopfr  empreinte: sha256:1f3a…  généré: 2026-08-22T14:30:11Z
# Pour personnaliser : /etc/ogs/templates/apache/php-fpm.conf.tmpl
```

L'empreinte permet de détecter une modification manuelle : `ogs diff` signale le fichier comme « modifié
hors ogs » et `apply` refuse de l'écraser sans `--force`. Le projet et la ressource de la ligne qui suit
la marque sont le **seul** critère d'appartenance d'un fichier (D76) : son nom ne prouve rien, et ogs ne
retire ni n'écrit un fichier que cette ligne attribue à un autre projet.

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

`--dry-run` s'arrête **avant le hook `preApply`** et avant la première écriture : ni fichier, ni compte, ni
hook. Ce cycle est celui des commandes qui savent simuler ; une commande qui ne le sait pas refuse l'option
en code 2 avant toute préparation, au lieu de l'ignorer (D88). Réserve : le calcul du plan attribue déjà,
dans la base d'état, le numéro de pool ou le port applicatif d'un projet jamais appliqué — celui que l'apply
lui donnera —, et `plan` n'est donc pas tout à fait « sans écriture » (§20).

### 6.2 Ordre des dépendances

Ordre de création (l'ordre de suppression est l'inverse exact) :

```
1. utilisateur système et groupe
2. arborescence et permissions
3. bases de données et utilisateurs
4. vhost AMQP, utilisateur, exchanges, queues, bindings, policies
5. pools PHP-FPM
6. vhosts web, rendus d'après le certificat présent (HTTP seul s'il n'y en a pas, D79)
7. reload web  ← prérequis du challenge HTTP-01
8. certificats TLS, repli par paliers si la demande complète échoue (D80) ; l'étape porte le reload web
9. bascule des vhosts dont le rendu d'après le matériel présent diffère du fichier en place, reload web
10. tâches cron
11. runtime applicatif (nvm, rbenv, pyenv, venv) pour les types non-PHP
12. unités systemd, daemon-reload, enable, start
13. attente de disponibilité du socket ou du port applicatif (§7.11.6)
14. vhost de proxy, reload web
15. hooks post-create
```

L'étape 7 est structurante : le challenge HTTP-01 exige que le vhost HTTP réponde déjà. Le vhost est donc
écrit en deux passes. La première ne retire jamais le HTTPS d'un vhost que le certificat présent sert déjà :
un alias ajouté ou un principal changé restent déclarés dans les deux blocs, servis par le port 80 pour leur
challenge, et le bloc 443 garde les noms déjà couverts (D79).

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

Une émission refusée pour un seul nom se replie par paliers (D80) : le site reste en HTTPS sur ce qui a pu
être couvert, l'étape s'annonce « ✓ … (en partie) » suivie de l'avertissement qui nomme ce qui manque, et
le code est 6 — un succès partiel n'est jamais compensé. Loin de l'échéance, aucun nom servi n'est retiré.

Si le rollback lui-même échoue, l'outil s'arrête, affiche l'état exact atteint et les commandes de
nettoyage manuel, et renvoie le code 1. Aucune tentative silencieuse.

### 6.4 Idempotence et détection de dérive

- Chaque fichier généré est comparé à son rendu attendu : identique → `NoOp`, différent → `Update` avec
  diff unifié affiché.
- L'empreinte SHA-256 du dernier contenu écrit est stockée en base. Si le fichier sur disque diverge à la
  fois du rendu attendu et de l'empreinte enregistrée, il a été **modifié à la main** : `apply` refuse et
  exige `--force` (qui sauvegarde l'original sous `<nom>.ogs-bak-<horodatage>`, suffixé `.1`, `.2`… si une
  autre sauvegarde de la même seconde porte déjà ce nom — D76).
- **Le retrait suit les mêmes cinq états que l'écriture** (D76, `removal.File`) : un fichier devenu inutile
  — pool, vhost et ses liens, vhost de statut, cron, sudoers, Telegraf, fail2ban, tables et clés DKIM —
  part s'il est intact ; retouché, il ne part que sous `--force`, après la même sauvegarde ; sans la marque
  d'ogs, ou marqué pour un autre projet, il ne part jamais. Un retrait refusé est une étape **non
  critique** en échec (code 6) : le fichier resté est en trop, pas en défaut, et rien n'est compensé pour
  lui. Un fichier qu'on ne peut pas lire avant de le retirer est une erreur ordinaire (`removal.InspectError`,
  code 1), ni un conflit ni une faute de déclaration.
- **`--force` reprend un fichier d'ogs retouché, ou jamais écrit par ogs ; il ne donne jamais à un projet
  le fichier d'un autre** (`fs.CheckOwner`, code 4), et il ne lève pas davantage les gardes du pare-feu
  contre la coupure d'accès, qui ont leur propre option (D86).
- La copie est déposée dans **`/var/lib/ogs/backups`** (0700 root), jamais à côté de l'original. Un
  répertoire de configuration est toujours relu en entier par quelqu'un, et la copie qu'on y laisse ment à
  ce lecteur : `/etc/logrotate.d` est chargé tout entier hors une liste de suffixes tabous où le nôtre ne
  figure pas — la sauvegarde y devient une seconde rotation portant sur les mêmes journaux, et deux
  directives pour un même chemin arrêtent la rotation de tout le parc ; `doctor` énumère `/etc/sudoers.d`
  sur le préfixe `ogs-` et accuserait sans fin la copie ; l'importateur lit `sites-available` et proposerait
  d'adopter une seconde fois un site déjà géré. Sortir du répertoire plutôt que choisir un suffixe ignoré :
  la liste des tabous appartient à chaque démon et peut changer, l'emplacement ne dépend de personne. Le
  répertoire commun est aussi ce qui rend l'écriture sûre — l'horodatage se devine à la seconde près, et une
  copie déposée dans un répertoire qu'un non-root possède (le `.ssh` d'un projet, à la rotation de la clé de
  déploiement) se ferait détourner par un lien posé d'avance à ce nom.
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
- **Le rechargement du serveur web revalide juste avant d'agir**, et pas seulement à l'écriture. Les deux
  moments sont séparés — les intentions sont groupées en fin d'apply — et ce qui était valide à l'écriture
  peut ne plus l'être : un fichier référencé a pu disparaître entre-temps. L'enjeu est disproportionné,
  `apachectl graceful` ne testant rien de lui-même : un maître Apache qui relit une configuration invalide
  **meurt**, emportant tous les sites du serveur, pas seulement celui qu'on installe. Un rechargement refusé
  laisse au contraire le serveur servir la configuration qu'il a déjà en mémoire.
- **L'état du serveur est constaté après le rechargement**, et le service relevé s'il s'est arrêté. Le
  compteur d'échecs est acquitté avant toute relance (`systemctl reset-failed`) : au-delà de
  `StartLimitBurst` — cinq démarrages en dix secondes par défaut — systemd refuse de relancer et rend
  « start request repeated too quickly », si bien qu'un serveur resterait mort alors que la cause vient
  d'être levée. Aucune
  vérification en amont ne couvre tous les cas : un service dont l'unité porte `ProtectHome` voit `/home` en
  lecture seule **dans son seul espace de noms**, où ogs ne peut pas se placer — `mount` affiche `rw`, et le
  serveur meurt pourtant sur l'ouverture d'un journal. Le fait est signalé dans tous les cas, même quand le
  redémarrage réussit : un serveur qui meurt à chaque rechargement est un incident, non un désagrément.
- **Un vhost dont le journal est hors d'atteinte du moteur n'est pas écrit** (§7.6, D59) : la vérification
  lit `ProtectHome` et `ReadWritePaths` effectifs de l'unité — ceux que systemd applique après l'unité et
  tous ses compléments — et non le fichier d'unité seul. Écrire depuis ogs ne prouve rien : root écrit là où
  le service ne peut pas.
- **Les journaux des vhosts sont éprouvés avant le même rechargement.** La validation syntaxique ne les
  ouvre pas, alors que le maître ouvre tous les journaux de tous les vhosts et s'arrête si un seul résiste :
  `AH00019: Unable to open logs, exiting`. Un unique projet dont le journal est inaccessible emporte donc le
  serveur entier — le cas s'est produit sur un `/home` remonté en lecture seule après une erreur disque, avec
  une configuration par ailleurs valide. Seuls les vhosts portant la marque d'ogs sont examinés, mais tous
  le sont, pas seulement ceux du projet en cours : un rechargement est global, et c'est un tiers qui en fait
  les frais. `doctor` porte le même contrôle, pour le découvrir avant le prochain rechargement.

---

## 7. Spécification des modules

### 7.1 Système : utilisateurs, groupes, arborescence, permissions

#### 7.1.1 Création

```
groupadd shopfr
useradd --gid shopfr --home-dir /home/shop.fr --shell /bin/bash \
        --no-create-home --comment "ogs=shopfr" shopfr
gpasswd -a www-data shopfr          # si webAccess = group (défaut)
install -d -o root -g shopfr -m 0750 /var/log/ogs-projects/shopfr
ln -sfn /var/log/ogs-projects/shopfr /home/shop.fr/logs   # posé sans suivre de lien (D70)
install -d -o shopfr -g shopfr -m 0700 /home/shop.fr/.local /home/shop.fr/.local/bin
ln -sfn /usr/bin/php8.5 /home/shop.fr/.local/bin/php   # posé sans suivre de lien (D70)
install -m 0440 -o root -g root <généré> /etc/sudoers.d/ogs-shopfr
```

Le groupe est **privé au projet** et porte le même nom que l'utilisateur (D23). L'arborescence est créée
explicitement, avec propriétaire et mode fixés à chaque niveau — jamais en dépendance du umask ambiant.
Le champ `--comment` marque le compte comme géré par `ogs`, ce qui permet de le distinguer d'un compte
adopté ou étranger.

#### 7.1.2 Permissions cibles

```
/home                        0755  root:root      traversable par tous (standard)
/home/shop.fr                0711  shopfr:shopfr  le propriétaire seul énumère ; les autres traversent
├── www/                     0710  shopfr:shopfr  code applicatif — le groupe traverse, il n'énumère pas
│   └── public/              0710  shopfr:shopfr  docroot (Symfony) ; pour Prestashop, www/ l'est
├── .ssh/                    0700  shopfr:shopfr  clé de déploiement (§7.1.5)
│   ├── id_ed25519           0600  shopfr:shopfr
│   ├── id_ed25519.pub       0644  shopfr:shopfr
│   └── authorized_keys      lien  shopfr:shopfr  → /opt/default_authorized_keys, s'il existe (D61)
├── var/                     0700  shopfr:shopfr  cache, sessions, logs applicatifs
│   ├── tmp/                 0700  shopfr:shopfr  upload_tmp_dir
│   └── sessions/            0700  shopfr:shopfr  session.save_path
├── logs → /var/log/ogs-projects/shopfr         lien de commodité (D71) — voir §14.5
├── .local/                  0700  shopfr:shopfr
│   └── bin/                 0700  shopfr:shopfr  répertoire des liens gérés (D70), sur le PATH
│       ├── php → /usr/bin/php8.5                lien géré par ogs (§7.5)
│       └── node, ruby, python, bundle, app      liens des autres runtimes (§7.11)
└── .env                     0600  shopfr:shopfr  lu par FPM (sous shopfr), pas par www-data
```

Le pool PHP-FPM tourne sous `shopfr:shopfr` : il accède donc à l'intégralité du home, `.env` et `var/`
compris. `www-data`, membre secondaire du groupe `shopfr`, obtient exactement ce qu'il lui faut : traverser
le home et `www/`, lire les fichiers que leurs propres droits lui ouvrent. Rien de plus — `var/` est en
0700 et `.env` en 0600.

**Traverser sans énumérer (D51).** Le home est en 0711 et les répertoires du projet en 0710 : le bit
d'exécution seul autorise le passage vers un chemin connu, le bit de lecture — absent — aurait autorisé
d'en dresser la liste. Servir un fichier n'a jamais besoin que du premier. Un membre du groupe, y compris
`www-data`, ne peut donc pas parcourir l'arborescence pour découvrir ce qu'elle contient : il doit déjà
savoir ce qu'il cherche. Le home est traversable par le groupe **et** par les autres, et cela n'est pas
une négligence : Unix retient la première classe applicable, sans repli. `www-data` étant membre du groupe
du projet, un home en 0701 lui appliquerait les droits de groupe — nuls — et lui refuserait la traversée que
les bits « autres » semblaient accorder. Ajouter `www-data` au groupe lui retirerait donc l'accès, et le
site cesserait d'être servi. Le bit « autres » sert, lui, au mode `webAccess: acl`, où le serveur web n'est
pas membre du groupe. Rien n'est lisible pour autant : les répertoires du niveau suivant sont fermés aux
autres, et aucun n'est énumérable.

**Pourquoi les journaux ne vivent plus dans le home (D71).** Apache, nginx et le maître PHP-FPM ouvrent
leurs fichiers de journal **en tant que root**, avant d'abandonner leurs privilèges. Si l'utilisateur pouvait
remplacer `apache-error.log` par un lien vers `/etc/shadow` ou `/etc/sudoers.d/`, il obtiendrait une écriture
root arbitraire, donc une escalade de privilèges complète — une classe de vulnérabilité connue et
régulièrement exploitée sur les panels d'hébergement. Un répertoire root avec des fichiers pré-créés
l'empêche **de substituer un fichier**, mais pas **de substituer le répertoire** quand celui-ci vit dans un
home qui lui appartient : il le renomme sans privilège et en recrée un à lui. D'où `/var/log/ogs-projects`,
dont aucun composant n'appartient à l'utilisateur (§14.5), et un simple lien `~/logs` pour la lecture,
qu'aucun processus root n'emprunte.

Même raisonnement pour les **liens gérés** (D70) : ils vivent dans `~/.local/bin`, au compte de
l'utilisateur. Un `.bin/` root ne garantissait pas la version effective, pour la même raison — le lien est
un état réconcilié à chaque apply, et root l'écrit sans jamais suivre de lien.

Règle générale, que ces deux cas illustrent : **un répertoire root dans le home ne protège rien contre
l'utilisateur**. Ce qu'un processus root ouvre en écriture vit hors du home, ou est écrit par des
descripteurs ouverts sans suivre de lien.

La règle vaut pour **chaque composant**, non pour le seul dernier (D83) : `O_NOFOLLOW` ne protège que la
feuille, et un parent jugé conforme au plan peut être remplacé par un lien avant l'étape de l'enfant — la
confirmation d'un apply en laisse tout le temps. Les répertoires de l'arborescence sont donc atteints
depuis la racine du projet, un composant après l'autre (`fs.EnsureDirAt`, `SetDirAt`, `LstatAt`), à
l'observation, à l'écriture comme à la compensation ; un parent qui n'est pas un répertoire véritable fait
échouer l'étape en le nommant, au lieu d'annoncer la correction de ce qu'on trouverait au bout du lien.
Seule la racine du projet, dont les parents appartiennent à root, est encore atteinte par son nom. Les
commandes qui travaillent dans le home hors de tout plan — clé de déploiement, clone, `php switch` —
suivent la même règle (D82), de même que l'archive et la restauration (D81).

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

Cette hypothèse, portée en suspens depuis le lot L1, est désormais **mesurée** sur les deux moteurs. Sur
une Debian 13 avec Apache 2.4 (`make integration`), un home en 0750 servi à `www-data` hors du groupe
renvoie **403** ; après `gpasswd -a` et un simple `apache2ctl graceful`, il renvoie **200**. Sur nginx
(`make integration-nginx`), le même essai renvoie **404** — ne pouvant traverser le home, nginx ne trouve
pas le fichier — puis **200** après un `reload`. Le scénario compare avec un `restart` et distingue
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
host effectif   shop.preprod-pp.net
home            /home/shop.preprod-pp.net
identifiant     shopfrprep
user + groupe   shopfrprep
base            shopfrprep      user de base   shopfrprep
vhost AMQP      shopfrprep      user AMQP      shopfrprep
pool PHP        shopfrprep      socket         /run/php/php8.5-fpm-18502-shopfrprep.sock
vhost web       shopfrprep.conf certificat     *.preprod-pp.net (partagé)
unités          ogs-shopfrprep-*.service        cron  /etc/cron.d/ogs-shopfrprep
logs            /var/log/ogs-projects/shopfrprep/     interpréteur  .local/bin/php
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
- **Aucune authentification** : une préproduction est accessible par défaut (D57). Elle n'est pas indexée,
  mais elle est publique — ce qui suppose de ne pas y déposer de données réelles, et de tenir son contenu
  pour lisible par quiconque connaît son adresse.
- **Aucun changement d'environnement applicatif** : `APP_ENV` (ou `SYMFONY_ENV`) vaut `prod` en
  préproduction comme en production, dans le pool comme dans les unités (D96).
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

`ogs site clone shop.fr --to prep` (lot L6) crée la préproduction, recopie `www/` sous garde (D82, D84),
puis restaure un dump de la base de production dans celle de préproduction.

La copie ne suit aucun lien : un lien est recopié comme lien, et un `www` source qui en est un fait refuser
la copie. Elle ne reprend que les fichiers du compte de production ; ceux de root ou de `www-data`, les
fichiers spéciaux et ce qui se trouve au-delà de 1024 niveaux sont écartés, nommés (50 au plus, cités) et
comptés, et la commande rend alors 6. Les liens physiques internes sont reposés et les fichiers creux
restent creux ; ni ACL, ni attributs étendus, ni bits setuid et setgid des fichiers ne sont reportés. Un
Ctrl-C ou un SIGTERM arrête la copie et rend le code 9 : la préproduction est créée avec une partie du
contenu, la base n'est pas copiée, et `ogs project delete <id>` la retire avant un nouveau clone.

La copie **neutralise ce qu'ogs connaît de la production, et nomme le reste** (D84). Les fichiers où le
type d'application range la configuration d'un environnement (`.env.local` de Symfony, `parameters.php`
de Prestashop, `.env` d'une application servie par un runtime) et ses caches compilés ne sont pas
recopiés ; les mots de passe qu'ogs a générés pour les bases et le transport de la production, et les
valeurs d'au moins 12 caractères de son fichier d'environnement, sont masqués à longueur égale dans la
copie comme dans la déclaration ; la racine de la production y est remplacée par celle de la
préproduction ; tâches et services sont repris désactivés. Chaque fichier retenu ou expurgé, chaque
variable déclarée reprise, chaque lien dont la cible sort de la préproduction, chaque secret qu'ogs n'a pas
pu chercher et un installeur Prestashop resté en place sont nommés, avec le code 6. Le reste — tout autre
fichier de `www/`, le reste d'une chaîne dont seul le mot de passe est masqué, le contenu de la base — passe
tel quel, et la sortie le dit. `--dry-run` annonce ce qui sera retenu, masqué et repris.

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
machine, sous la seule propriété de l'utilisateur du projet. Le sens du déploiement ne dépend pas du shell du
compte : il reste le tirage, que le shell soit ouvert (D62, le défaut) ou fermé.

```
$ ogs user key show shopfr
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI… ogs:shopfr@srv1
→ à déclarer en deploy key (lecture seule) sur le dépôt du projet
```

- `ogs user key show <id>` n'affiche que la clé **publique** ; la privée n'est jamais restituée par une
  commande, n'entre pas dans le keystore (elle vit sur le disque, protégée par ses permissions) et n'apparaît
  dans aucun log.
- `ogs user key rotate <id>` régénère la paire ; l'ancienne clé publique est affichée pour permettre son
  retrait chez l'hébergeur, l'ancienne clé privée sauvegardée dans `/var/lib/ogs/backups` sans jamais
  écraser une autre sauvegarde, et l'opération est journalisée.
- `show` et `rotate` tournent hors de tout plan (D82). Ils ouvrent `.ssh` par descripteur, sans suivre de
  lien, et refusent un `.ssh` lié ou qui n'appartient pas au projet — `ogs apply` le rétablit. Une clé qui
  n'est pas un fichier régulier du projet, qui dépasse 16 Kio ou, pour la clé publique, qui n'est pas du
  texte imprimable sur une ligne n'est ni lue, ni affichée, ni sauvegardée : un lien physique vers un
  fichier de root, une clé creuse de 1 Gio, une séquence d'échappement destinée au terminal de l'exploitant.
  La nouvelle paire est posée par `unlinkat` puis `openat` en `O_EXCL` et `O_NOFOLLOW`, avec mode et
  propriétaire posés sur le descripteur.
- `known_hosts` est pré-rempli par `ssh-keyscan` pour les hôtes déclarés dans
  `deployment.knownHosts` (par défaut `github.com`, `gitlab.com`), avec affichage des empreintes obtenues
  pour vérification. En l'absence de réseau, le fichier reste vide et un avertissement est émis :
  `StrictHostKeyChecking` n'est **jamais** désactivé, ni dans le `config` généré ni ailleurs.
- La clé est incluse dans l'archive de suppression (elle fait partie du home) et restaurée par
  `project restore`, ce qui évite de redéclarer une deploy key après une restauration.

**Clés d'administration entrantes (D61)** — si le fichier `/opt/default_authorized_keys` existe,
`.ssh/authorized_keys` du projet est un **lien symbolique** vers lui, posé juste après la clé de déploiement
et `lchown` à l'utilisateur du projet :

```
/home/shop.fr/.ssh/authorized_keys → /opt/default_authorized_keys
/opt/default_authorized_keys       0644  root:root   posé par l'administrateur, non géré par ogs
```

Un lien, non une copie : le trousseau se met à jour en un seul endroit, sans repasser sur chaque projet, là
où une copie serait figée au jour de la création du site. Le fichier commun n'est pas géré par ogs — aucun
en-tête, aucune empreinte : l'administrateur le pose et le tient à jour.

Trois comportements à connaître :

- **Sans le fichier commun, aucune étape.** La fonction est facultative, et la disparition du trousseau ne
  doit pas emporter un `authorized_keys` propre au projet.
- **Un `authorized_keys` régulier déjà présent n'est jamais remplacé** — reprise d'un serveur existant, clé
  posée par l'utilisateur du projet. Il porte l'accès de quelqu'un, que le lien lui retirerait sans un mot.
  L'étape échoue alors sans rien détruire : elle n'est pas critique, l'avertissement dit quoi faire (§13,
  code 6) et le reste du projet s'applique.
- **Un lien qui pointe ailleurs est ramené** sur le trousseau commun, l'ancienne cible restant sur le disque.

Ce sont les droits du **trousseau**, non ceux du lien, qui décident : sshd résout le lien avant tout
contrôle, et le mode d'un lien symbolique n'existe pas sous Linux — toujours 0777, immuable —, ce qui
interdit d'en faire un critère de dérive. `doctor` contrôle donc le trousseau : propriétaire root, non
inscriptible au-delà de root (sous `StrictModes`, sshd l'ignorerait **en silence**), lisible par les
utilisateurs des projets (sshd le lit sous leur identité), et un répertoire parent non inscriptible.
Le `lchown` du lien est cosmétique et ne l'est pas moins que nécessaire : un `chown` donnerait le trousseau
commun à l'utilisateur du dernier projet appliqué.

Le shell des projets est ouvert par défaut depuis D62, ce qui rend ce trousseau utile sans réglage
supplémentaire. Un parc qui referme le shell (`user.shell: /usr/sbin/nologin`) garde le lien en place mais
**inerte**, sauf accès restreint à SFTP côté sshd. Dans les deux cas, ogs ne touche pas à la configuration de
sshd : c'est elle, et les clés du trousseau, qui décident de ce qui entre.

#### 7.1.6 Suppression

`userdel` sans `--remove` (l'arborescence est traitée par la logique d'archivage), suppression du groupe,
retrait de `www-data` du groupe. Refus de supprimer un utilisateur non créé par `ogs` (absent de la base
d'état et sans marqueur `ogs=` dans le champ commentaire) sans `--force`. Le refus est prononcé à l'entrée
de la commande, avant la confirmation et dans le même message que tous les autres refus de la
suppression (D77), non au moment de retirer le compte, quand plus rien ne se relance. Le marqueur ne peut
contenir ni `:`, séparateur de champs de `/etc/passwd`, ni `,`, séparateur de sous-champs du GECOS.

Un `userdel` en échec ne fait pas abandonner l'arborescence : le home part quand même, et le compte restant
est signalé avec la liste des processus qui le tiennent — lue dans `/proc`, `userdel` ne nommant que le
premier PID — et la commande à passer pour finir. La suppression rend alors le code 6 (§13).

#### 7.1.7 Garde-fous

- Refus des noms réservés (`root`, `daemon`, `www-data`, `mysql`, `postgres`, `rabbitmq`, `nobody`…) et de
  tout identifiant déjà présent dans `/etc/passwd` ou `/etc/group` mais inconnu de la base d'état.
- Refus d'un home hors de `/home` (ou de la racine configurée), refus d'un home existant non vide lors
  d'une création, entendu comme **tenu par un autre compte** (D95) : quand le compte d'un projet non adopté
  reste à créer, sa racine est refusée si elle appartient à un compte existant autre que root, ou si une
  entrée de premier niveau appartient à un compte ordinaire existant (plage `UID_MIN`–`UID_MAX`, hors
  `www-data`). Le refus précède le dépôt de la déclaration. Une racine à root ou à un UID orphelin — compte
  supprimé à la main, saga de création déroulée, suppression qui a retiré le compte sans le home — n'est
  reprise, et annoncée, que vide, connue de la base d'état pour ce projet, ou réduite au squelette qu'ogs
  pose : le home d'un compte supprimé sans `-r` a le même propriétaire, useradd redonne son UID, et son
  `authorized_keys` régulier ouvrirait un shell sous le compte du projet. À la suppression, le même critère
  fait refuser l'effacement de l'arborescence, sans que `--force` le lève — la trace d'un home repris
  avant D95.
- Aucun processus ne doit tourner sous l'utilisateur au moment de sa suppression. Le rechargement des
  serveurs, ordonné avant elle (§13, étape 8), y suffit dans le cas courant : c'est lui qui libère les
  workers FPM du projet. Ce qui subsiste malgré tout est énuméré, jamais tué — ogs n'interrompt pas un
  processus qu'il n'a pas déclaré.
- Refus de générer un fichier sudoers pour un utilisateur qui n'est pas celui d'un projet géré, et refus
  d'écrire sous `/etc/sudoers.d/` un nom ne commençant pas par `ogs-`.
- UID laissé au système, plage configurable ; jamais de réutilisation d'un UID libéré tant que des fichiers
  lui appartiennent encore ailleurs sur le disque (contrôle sur `/home` uniquement, avec avertissement).

### 7.2 MySQL / MariaDB

**Connexion administrateur** : socket Unix (`/run/mysqld/mysqld.sock`, détecté), authentification
`auth_socket`/`unix_socket` en tant que root. Le socket n'est admis que s'il appartient à root ou à
`mysql`, dans un répertoire qu'aucun autre compte ne peut écrire ; `/tmp` n'est jamais cherché (D92). Repli sur `/etc/mysql/debian.cnf` s'il existe. Un DSN
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

**Un nom sous deux hôtes fait deux comptes** (D78). `'shop'@'%'` et `'shop'@'localhost'` coexistent, et
MySQL retient pour chaque connexion celui dont l'hôte est le plus précis : créer le second à côté du
premier ferait refuser le mot de passe de l'application, par le socket comme par 127.0.0.1. Quand le compte
déclaré — principal ou additionnel — est absent sous son hôte mais que son nom existe sous un autre,
l'observation le relève et le plan rend une étape non critique en échec, qui nomme le compte existant et
la porte de sortie (déclarer son hôte réel, ou retirer l'autre compte) : rien n'est exécuté, aucun mot de
passe n'est scellé. Les comptes additionnels partagent l'hôte de leur déclaration.

**Appartenance** (D91). Un compte est à ogs pour un projet si son mot de passe est scellé dans la portée du
projet — ogs ne le scelle qu'en le créant —, une base si ce compte y a des privilèges. Hors projet adopté,
un homonyme qui existe sans être à ogs n'est ni repris à la création — étape non critique en échec, qui le
nomme avec les issues réelles —, ni supprimé avec le projet ; une compensation ne retire que ce que son
étape a créé, jamais le compte qu'une autre base du projet emploie. Aucun projet ne déclare `root`,
`mysql`, les comptes internes du moteur ni celui de la supervision : `ALTER USER 'root'@'localhost'
IDENTIFIED BY` **remplace** sous MariaDB l'authentification `unix_socket` par laquelle ogs administre le
moteur, et `db user rotate` y aurait coupé ogs de tout le parc. Le refus vaut aux points d'écriture d'une
déclaration ; une déclaration existante voit sa base refusée par le plan, sans arrêter le parc, et un
compte réservé ne part jamais à la suppression. L'ordre des requêtes est **compte, GRANT, base** : MySQL
accepte le GRANT sur une base encore absente, et une création interrompue se reprend au plan suivant, là où
une base créée d'abord restait sans droits, tenue pour étrangère. Le refus d'un compte additionnel
étranger ne vise que lui.

**Contraintes** : identifiants échappés par backquotes doublées ; longueur d'utilisateur 32 caractères
(MySQL 8) contre 80 (MariaDB 10.6+), la limite la plus stricte est appliquée pour la portabilité ; détection
MariaDB par `SELECT VERSION()`.

**Dump/restore** : `mysqldump --single-transaction --routines --triggers --default-character-set=utf8mb4`,
compression zstd, sortie vers fichier ou stdout. Restauration avec confirmation si la base n'est pas vide.
Le chemin désigné par `db dump -O` ou `db restore -i` est parcouru depuis « / » par descripteurs (D83) :
un lien n'y est suivi que si seul root a pu le poser, et seul un fichier ordinaire — ou, pour une
sauvegarde, un nom absent — est admis ; un lien posé dans un home, un tube nommé, un périphérique sont
refusés — et laissés en place —, avant la confirmation et en code 2 pour une restauration. Un fichier
ordinaire déjà présent est délié, puis la sauvegarde recréée par le descripteur du répertoire en
`O_EXCL`, en 0600, à root. `/dev/stdout`, `/dev/stderr`, `/dev/null` et
`/dev/stdin` restent admis. Il en va de même pour PostgreSQL (§7.3).

La restauration est **confinée au projet** (D92) : `mysql --no-defaults --user=<compte>` par le socket
admis, sous l'utilisateur Linux du projet, le mot de passe scellé dans `MYSQL_PWD` ; un compte déclaré
sous `127.0.0.1` ou `::1` passe par TCP, après contrôle que le port 3306 est tenu par `mysql`. Le compte
est le principal s'il a le jeu `all`, lancé sous l'utilisateur du projet ; sinon le premier compte
additionnel `all`, lancé par `systemd-run -p DynamicUser=yes` sous une identité jetable — jamais sous
l'utilisateur du projet, qui lirait son mot de passe dans l'environnement du client. Sans compte `all`, ou
sans systemd et son bus, la restauration est refusée d'emblée en nommant la sortie ; `project restore`
compte une base non rechargée en succès partiel (code 6). Une base
**adoptée** ne se restaure qu'après `ogs db user rotate <id> <compte>` : ogs n'en connaît pas le mot de
passe, et le refus le nomme. Les clauses `DEFINER` sont retirées en tête des lignes de définition, jamais
dans les données : nommer un autre compte exige `SET_USER_ID`, et c'est le cas de tout clone. Un script qui
touche à une autre base échoue.

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

**Privilèges** : le compte principal est **propriétaire** de la base et de son schéma `public` — c'est
lui qui porte les migrations. Un jeu `crud` ou `readonly` sur lui est refusé (D92) : `db add` en code 2,
le plan par une étape non critique en échec, qui ne touche pas à la base en place. Le jeu borné se déclare
sur un compte additionnel, qui l'applique aux objets présents et à venir.

**Connexion** : le répertoire du socket n'est admis que s'il appartient à root ou à `postgres` sans être
inscriptible par un autre (le groupe `postgres` admis) ; un socket refusé rend le cluster indisponible,
sans repli sur `/var/run/postgresql` (D92).

**Dump/restore** : `pg_dump` en script SQL, rechargé par `psql`. Chemins désignés comme au §7.2 (D83). La
restauration est confinée comme au §7.2 : `psql --no-psqlrc --host=127.0.0.1 --username=<rôle>` sous
l'utilisateur Linux du projet, `PGPASSWORD` scellé, `PGREQUIREAUTH=scram-sha-256`, `ON_ERROR_STOP`. Elle
exige un socket admis — un cluster arrêté laisse son port à quiconque — et que le port soit tenu par
`postgres`. Le `COMMENT ON EXTENSION` que pg_dump pose sous son en-tête, qu'un non-propriétaire de
l'extension ne peut poser, est écarté. Aucune sauvegarde ne porte de propriétaire (`--no-owner`) : les
objets reviennent au rôle qui restaure. Celle d'un clone ne porte pas non plus les privilèges
(`--no-privileges`), qui nomment les rôles de la source. Une sauvegarde **antérieure** à D92 d'une base
adoptée dont des objets appartiennent à un autre rôle porte `ALTER … OWNER TO <autre>`, et sa
restauration s'arrête là ; de même un `ALTER DEFAULT PRIVILEGES FOR ROLE <autre>`.

**Appartenance** : celle du §7.2 (D91), la propriété de la base tenant lieu de privilèges. Sont réservés
`postgres`, `root` et le rôle d'administration d'ogs (D44), `public`, tout rôle `pg_*` — `pg_monitor`
existe déjà, et le déclarer en faisait le propriétaire de la base du projet — et le rôle de supervision
(D67), qu'un projet homonyme aurait reçu, `pg_monitor` compris.

**Base adoptée** : le durcissement de D45 s'applique aussi à une base préexistante reprise par l'import.
Quand le rôle déclaré ne la possède pas, l'apply exécute `REVOKE CONNECT … FROM PUBLIC`,
`REVOKE ALL ON SCHEMA public` et `ALTER SCHEMA public OWNER` : c'est une normalisation au sens du §9, que le
premier `diff` montre, mais elle peut couper d'autres rôles qui s'y connectaient. Point ouvert (§20).

### 7.4 RabbitMQ

**Pilotage** : API HTTP management (`http://127.0.0.1:15672/api/…`), authentification par un compte
administrateur dont les identifiants sont dans `/etc/ogs/credentials/amqp.yaml` (0600), enregistrés par
`ogs amqp credentials set --user <compte>` — le mot de passe est demandé sans écho, ou lu sur l'entrée
standard, jamais attendu en option (D93). Détection du plugin ; s'il est absent,
`ogs doctor` le signale et l'outil se replie sur `rabbitmqctl` en mode dégradé (vhosts, users et
permissions seulement, pas de déclaration fine des queues).

**Garde de l'autre bout** (D93) : le port de management n'est pas privilégié, et quand le broker est
arrêté tout compte local peut l'ouvrir. Chaque connexion est donc vérifiée une fois établie : l'autre bout,
cherché dans `/proc/net/tcp` et `/proc/net/tcp6`, doit appartenir à root ou au compte du broker
(`brokerAccount`, nom ou uid, défaut `rabbitmq`), que l'URL désigne la boucle locale, le nom ou une
adresse de la machine. Introuvable, il n'est admis que vers une adresse étrangère à la machine (broker
distant) ; vers la machine, c'est une redirection DNAT (Docker sans userland-proxy), refusée en invitant à
désigner le conteneur par son adresse. Refusé, rien n'est envoyé, et le broker est tenu pour
indisponible — étapes non critiques, comme pour un broker arrêté.

**Propriété** (D93) : le compte d'un projet n'est à ogs que si le keystore porte son mot de passe
(`amqp/<user>`), le vhost que s'il porte sa marque (`amqp-vhost/<vhost>`) — ou, dans un projet qui ne
porte encore aucune marque (parc antérieur à D93), si le compte d'ogs y a ses permissions ; ce vhost est
alors marqué au premier apply, et le critère ne vaut plus. Les deux sont scellés juste avant la création,
et la marque retirée si le vhost n'a pu être créé. `project restore` les réinjecte : un homonyme apparu
entre-temps, ou présent sur la machine de restauration, serait adopté. Des
homonymes sans preuve — l'administrateur, une application hors d'ogs — sont refusés en une étape non
critique : rien n'est déclaré dessus, `amqp queue purge` refuse, `amqp status` tait leurs files, et
`project delete` les laisse en place. Ce qui est à ogs part avec son secret.

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
sur le vhost et l'utilisateur du projet ; rien n'est écrit dans le répertoire des journaux du projet. La restriction est documentée
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
La base n'en est pas seule dépositaire (D94) : avant la première attribution, les numéros des pools écrits —
lus sur leur ligne `listen`, port ou nom du socket, le pool sur l'en-tête — y sont repris, si bien qu'une
`state.db` perdue ou recréée rend à chaque pool le sien au lieu de les redistribuer dans l'ordre
alphabétique. Un pool `listen: tcp` ne reçoit jamais un port que la machine écoute déjà.

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
php_admin_value[error_log] = /var/log/ogs-projects/shopfr/php-error.log
php_admin_flag[log_errors] = on
php_admin_value[upload_tmp_dir] = /home/shop.fr/var/tmp
php_admin_value[session.save_path] = /home/shop.fr/var/sessions
env[APP_ENV] = prod
slowlog = /var/log/ogs-projects/shopfr/php-slow.log
request_slowlog_timeout = 10s
request_terminate_timeout = 300s
catch_workers_output = yes
```

**Interpréteur CLI : le lien `/home/<host>/.local/bin/php`**

Les tâches cron et les unités systemd n'invoquent jamais `/usr/bin/php8.5` mais toujours
`/home/shop.fr/.local/bin/php`, lien symbolique géré par `ogs` (D70). Conséquence directe : une
bascule de version ne touche ni les fichiers cron ni les unités systemd, dont le contenu est indépendant de
la version. Le lien est également le point de vérité pour l'utilisateur, qui peut s'y référer dans ses
propres scripts — et le trouve sur son PATH, `~/.local/bin` y étant ajouté par le `.profile` de Debian — sans
jamais coder une version en dur.

Le répertoire appartenant à l'utilisateur, le lien est un **état réconcilié** : changé à la main, il est
rétabli au prochain apply ; un fichier régulier posé à sa place n'est remplacé que sous `--force`.

**Bascule de version** (`ogs php switch shopfr 8.5`) — séquence sans coupure :

1. Vérifier que la version cible est installée et que ses extensions couvrent celles de l'ancienne
   (comparaison de `php -m`), avertir sinon.
2. Allouer le nouveau numéro de pool dans la plage de la version cible.
3. Écrire le pool sur la nouvelle version, `php-fpm8.5 -t`.
4. `systemctl reload php8.5-fpm`, attendre l'apparition du socket (avec délai d'attente).
5. Réécrire les vhosts pointant sur l'ancien socket, valider, recharger le serveur web.
6. Mettre à jour le lien `.local/bin/php` (atomique, posé sans suivre de lien).
7. Redémarrer les services du projet via sa target, afin qu'ils chargent le nouvel interpréteur. Les crons
   basculent d'eux-mêmes à leur prochaine exécution ; aucun fichier cron n'est réécrit.
8. Supprimer l'ancien pool, recharger l'ancien FPM, libérer l'ancien numéro.

Toute erreur avant l'étape 5 annule proprement ; après l'étape 5, la compensation restaure l'ancien pool, le
vhost et le lien. `daemon-reload` n'est pas nécessaire, aucune unité n'ayant changé.

L'ancien pool part sous le régime des cinq états (D76) : retouché, il exige `--force` et une sauvegarde. Un
fichier qui porte le nom exact d'un pool du projet dans une autre version sans qu'ogs l'ait écrit pour lui —
le pool d'origine d'un projet adopté, resté actif après la bascule — est signalé et laissé en place, code 6.

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
    ErrorLog /var/log/ogs-projects/shopfr/apache-error.log
    CustomLog /var/log/ogs-projects/shopfr/apache-access.log combined
</VirtualHost>

<VirtualHost *:443>
    ServerName shop.fr
    ServerAlias www.shop.fr
    DocumentRoot /home/shop.fr/www/public
    <Directory /home/shop.fr>
        Options -FollowSymLinks +SymLinksIfOwnerMatch
    </Directory>
    <Directory /home/shop.fr/www/public>
        AllowOverride None
        Require all granted
        Options -Indexes -FollowSymLinks +SymLinksIfOwnerMatch
        FallbackResource /index.php
    </Directory>
    RedirectMatch 403 "/\.(?!well-known/)"
    RedirectMatch 403 "/composer\.(json|lock)$"
    <If "%{REQUEST_FILENAME} =~ m#/composer\.(json|lock)$#">
        Require all denied
    </If>
    <FilesMatch \.php$>
        SetHandler "proxy:unix:/run/php/php8.5-fpm-18501-shopfr.sock|fcgi://localhost"
    </FilesMatch>
    SSLEngine on
    SSLCertificateFile /var/lib/ogs/acme/certificates/shopfr/fullchain.pem
    SSLCertificateKeyFile /var/lib/ogs/acme/certificates/shopfr/privkey.pem
    Header always set Strict-Transport-Security "max-age=31536000; includeSubDomains"
    ErrorLog /var/log/ogs-projects/shopfr/apache-error.log
    CustomLog /var/log/ogs-projects/shopfr/apache-access.log combined
</VirtualHost>
```

**Nginx** : équivalent avec `fastcgi_pass unix:…`, `include snippets/fastcgi-php.conf`, `location ^~
/.well-known/acme-challenge/ { root /var/lib/ogs/acme-challenge; }`, `disable_symlinks if_not_owner
from=/home/shop.fr`, deux `location ~ /\.(?!well-known/)` et `location ~ /composer\.(json|lock)$` en
`deny all` en tête des expressions régulières, `try_files $ogs_fichier /index.php$is_args$args` —
`$ogs_fichier`, défini par le fragment de portée serveur, ne diffère de `$uri` que pour « / » (D87).

Sous `AllowOverride All` (Prestashop), la directive devient `AllowOverride All
Options=Indexes,MultiViews,SymLinksIfOwnerMatch` : un `.htaccess` règle ces trois options, jamais
`FollowSymLinks`, `ExecCGI` ni `Includes`, et une autre option y fait répondre 500.

**Modules requis** — vérifiés par `doctor` : Apache `proxy_fcgi`, `setenvif`, `rewrite`, `ssl`, `headers`,
`http2` ; Nginx compilé avec `http_ssl_module` et `http_v2_module`.

**Règles** :
- Le bloc `.well-known/acme-challenge` est présent dès la première passe HTTP, avant toute redirection.
- Un seul moteur web actif par serveur pour la v1 ; la coexistence Apache+Nginx sur des ports distincts
  n'est pas gérée (les deux peuvent être installés, un seul est piloté).
- Détection de conflit : refus d'écrire un vhost dont un domaine est déjà servi par une configuration
  existante, gérée ou non.
- Un vhost n'est à un projet que par son en-tête (D76) : un fichier posé à la main selon la convention de
  nommage n'est jamais pris pour un vhost obsolète du projet, et part moins encore ; s'il sert l'un de ses
  domaines, c'est un conflit (code 4). Le lien d'activation part avec le fichier, et seulement s'il le
  désigne.
- **Liens du docroot** (D87) : suivis seulement vers ce qui appartient à leur auteur, contrôle posé dès la
  racine du projet — `www-data`, membre de tous les groupes, servirait sinon les fichiers d'un voisin.
  Sous Apache, le contrôle précède l'ouverture et porte sur le nom : une course subsiste (§20) ; nginx
  compare sur le descripteur ouvert.
- **Fichiers cachés et métadonnées de dépendances** (D87) : `/.git`, `/.env`, `.htaccess`… et
  `composer.json`, `composer.lock` ne sont jamais servis, `/.well-known/` excepté — le challenge ACME et
  `security.txt` y vivent. Le refus d'Apache est rendu dès la traduction de l'URL, avant tout contrôle
  d'accès : aucun `Require` d'un `.htaccess` ou d'une surcharge ne le rouvre.

#### 7.6.1 Surcharges de vhost (D63)

Deux points de personnalisation par vhost, éditables à la main sans passer par un apply :

| Répertoire | Contexte Apache | Contexte nginx |
|---|---|---|
| `/etc/<moteur>/projects/<name>/vhost/*.conf` | `<VirtualHost>` du vhost servant | `server` du vhost servant |
| `/etc/<moteur>/projects/<name>/docroot/*.conf` | `<Directory <docroot>>` | `location /` |

`<name>` est le nom du **fichier de vhost** — `shopfr`, `shopfr_redirect` — et non le domaine : un projet
en porte plusieurs, et un domaine principal qui change laisserait un répertoire que rien ne relie plus.
Les fichiers sont lus dans l'ordre alphabétique du masque, d'où l'usage d'un préfixe numérique quand
l'ordre compte, et **après** les `blocks` de `spec` : la surcharge locale l'emporte sur ce que déclare le
projet. Le vhost généré nomme les deux répertoires en clair, ce qui les fait découvrir à qui le lit.

Ce que la mécanique ne fait **pas** : ogs ne lit jamais ces fichiers, n'en écrit aucun, et l'empreinte du
vhost généré ne dépend pas d'eux — un fichier déposé ne fait donc jamais dériver le plan. La liste des
contextes est déduite du vhost rendu (`web.Provider.includeDirs`), non d'une table associant un template à
ses contextes : les templates étant surchargeables, une table finirait par créer un répertoire qu'aucun
vhost n'inclut, où les fichiers déposés seraient ignorés en silence.

**Ce qui n'est pas couvert, et qui doit être dit** — les fichiers sont lus par le processus maître, donc
en root, et le moteur seul les valide :

- une directive **dupliquée** est fatale sur nginx (`is duplicate`), là où Apache retient la dernière :
  reposer un `client_max_body_size` déjà présent dans le template fait refuser tout le parc au
  rechargement suivant. La revalidation de `web.Provider.Reload` le rattrape — le serveur continue de
  servir sa configuration précédente — mais elle ne l'empêche pas ;
- sur nginx, un `add_header` dans le contexte du docroot **annule tous ceux hérités du `server`**, HSTS et
  `X-Robots-Tag` de préproduction compris : une surcharge qui pose un en-tête doit reposer les autres ;
- un `rewrite … last` déposé dans le contexte du vhost d'un site en clair est évalué **avant** le choix du
  `location`, donc avant le bloc du challenge ACME : le renouvellement du certificat cesse d'aboutir. Le
  vhost de redirection n'offre pour cette raison **aucun** point de surcharge ;
- sur Apache, un `<Location />` de surcharge — la restriction ordinaire d'un site à un réseau — rouvrirait
  un refus porté par `<LocationMatch>` ou `<FilesMatch>`, les sections fusionnant dans l'ordre du fichier :
  d'où le refus des fichiers cachés par `RedirectMatch`. `Options +MultiViews`, qu'une surcharge du
  docroot peut poser, négocie « /composer » vers `composer.json` par une sous-requête que le
  `RedirectMatch` ne voit pas : le refus porte donc aussi sur le nom du fichier, dans un `<If>`, fusionné
  après les `<Location>` (D87). La négociation exige de lister le répertoire : elle échoue sur le docroot,
  en 0710 (AH00686), et ne s'exerce que dans un sous-répertoire que `www-data` peut énumérer. Seule une
  surcharge qui poserait elle-même un `<If>` contraire rouvrirait la variante ;
- sur nginx, `/.ogs-accueil/` est un nom réservé : le vhost PHP y conduit « / » (D87), et un répertoire de
  ce nom posé dans le docroot d'un Prestashop fait rendre 403 à son accueil, sans rien servir.

**Contrôles de `doctor`** : propriétaire root et absence d'écriture par un tiers sur les répertoires comme
sur les fichiers — même régime que les hooks (D40), et pour la même raison : qui écrit là dicte la
configuration lue en root ; fichier `.conf` déposé hors des sous-répertoires de contexte, donc chargé par
personne ; répertoire qu'aucun vhost ne référence plus.

À la suppression d'un projet, les répertoires **vides** partent avec le vhost et le reste est conservé, en
étant annoncé : ogs n'a pas écrit ces fichiers.

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

`ogs cert dns-provider set ovh` guide la saisie — les secrets sont demandés sans écho, ou lus sur l'entrée
standard, une ligne chacun (D93) —, vérifie les credentials par un appel de lecture sur la zone
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

`ogs cert renew --all` renouvelle les certificats expirant sous 30 jours (seuil configurable), et ceux qui
ne couvrent plus les domaines demandés d'une déclaration appliquée telle quelle — c'est ce qui reprend seul
l'émission d'un alias dont le DNS ne pointait pas encore ici. Il lance le hook `postCertRenew` pour les
certificats qui ont changé, et recharge le serveur web **dès qu'un certificat a changé**, même si un autre
a échoué. Un projet dont la demande est refusée n'est pas passé en silence : il est nommé (« certificat non
examiné, déclaration refusée »), compté parmi les échecs, et le reste du parc est renouvelé (code 6).

**Ce que couvre le certificat d'un projet** (D80) : ses domaines principaux, ses alias, puis ses
redirections — hormis une adresse IP, qu'aucune demande ACME ne peut porter, et dont le vhost reste en HTTP
seul. Une commande ACME échouant tout entière sur un seul nom refusé, l'émission se replie par paliers :
sans les redirections en attente, sans aucune redirection, sur ce que couvre le certificat en place, puis
sur les seuls noms du vhost principal qu'il couvre. Loin de l'échéance et sans `--force`, un palier qui
retirerait un nom que le certificat en place sert est réservé à l'échéance ; le message en donne la date,
et `cert renew <id> --force` l'impose tout de suite.

**Nom d'un certificat de projet** (D94) : c'est le nom de son répertoire sous
`/var/lib/ogs/acme/certificates`, l'identifiant du projet à défaut. Il ne sort pas de l'espace des projets :
ni `/`, ni `.` en tête, ni `@…`, qui désigne un certificat partagé qu'un projet ne déclare pas, ni
`_shared-…`, répertoire de ces derniers — l'émission y écrit une clé privée, et sa compensation y fait un
`RemoveAll`. Tout autre nom, `*.cc.fr` compris, est accepté. Un nom refusé l'est au `plan` et à l'`apply` du
projet qui le porte et à son renouvellement, jamais au chargement du parc : une seule déclaration
arrêterait sinon le renouvellement de tous les certificats. Il est unique dans le parc (§3.3.6) ; deux
certificats d'un projet sous un même nom s'écraseraient l'un l'autre — chaque apply réémettant l'un sur
l'autre, jusqu'à épuiser le quota de l'autorité — et sont refusés de même, au seul projet qui les porte ;
`doctor` nomme les déclarations à corriger.

**Ce que présente un vhost** (D79) : le certificat présent, dès qu'il couvre l'un de ses noms — jamais la
demande, qui peut attendre une émission. Un certificat expiré n'est plus présenté, et `status` le dit
« expiré ».

**Garde-fous** :
- Vérification préalable de la résolution DNS du domaine vers une IP locale (avertissement, non bloquant
  car un reverse-proxy amont est possible).
- Respect des quotas Let's Encrypt : 5 échecs consécutifs par domaine déclenchent une pause de 1 heure
  enregistrée en base ; `--staging` bascule sur l'environnement de test.
- Clés privées en 0600 root, certificats en 0644.
- Révocation explicite, à la demande (`ogs cert revoke`) : aucune option ne la déclenche à la suppression
  d'un projet, qui ne révoque ni ne retire le certificat du projet — `ogs cert delete <id>` le retire ensuite (`ogs cert revoke <id>` avant, si la révocation est voulue)
  ensuite, le certificat n'étant plus référencé (§13 ; limite connue, §20).
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
- Un wildcard ne couvre qu'**un** niveau de sous-domaine : `shop.preprod-pp.net` est couvert,
  `a.shop.preprod-pp.net` ne l'est pas. Un host de préproduction à deux niveaux — atteignable depuis qu'un
  host peut être saisi tel quel (D58) — n'est pas refusé pour autant : le provider TLS constate que le
  wildcard ne couvre pas les domaines servis et émet un certificat propre au projet, comme pour un `--host`
  hors convention.
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
  /home/shop.fr/.local/bin/php /home/shop.fr/bin/console schedule:run \
  >> /var/log/ogs-projects/shopfr/cron-scheduler.log 2>&1
```

Règles :

- **Interpréteur** : toujours `/home/<host>/.local/bin/php`, jamais un binaire versionné (§7.5). Une bascule de
  version PHP ne réécrit donc aucun fichier cron.
- **Anti-recouvrement** : `flock -n` par défaut, désactivable par tâche. Les verrous vivent dans
  `/run/ogs/<id>/`, répertoire propre au projet (`0710 <id>:<id>`, §7.8) créé par `/etc/tmpfiles.d/ogs.conf`
  afin de survivre au redémarrage. C'est aussi là que vivent les sockets des applications non-PHP
  (§7.11), ce qui donne un seul répertoire d'exécution par projet, inscriptible par lui seul.
- **Identité** : exécution sous l'utilisateur du projet.
- **Journalisation** : sortie redirigée vers `cron-<nom>.log` du répertoire des journaux du projet
  (§14.5), fichier **pré-créé par `ogs`** avec propriétaire `<id>:<id>` et mode 0640 dans un répertoire
  appartenant à root — la tâche peut écrire dans le fichier, jamais le remplacer. `spec.crons[].log` n'est
  qu'un **nom de fichier** : un chemin ferait pré-créer par root un fichier au nom du projet n'importe où
  (D71).
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
ExecStart=/home/shop.fr/.local/bin/php bin/console messenger:consume async \
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

**Cohérence de version PHP** : les unités et les crons invoquent `/home/<host>/.local/bin/php`, jamais un binaire
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

`php switch` lit `composer.json` en root dans un répertoire que l'utilisateur possède : il l'ouvre par
descripteur, sans suivre de lien ni pour `www` ni pour le fichier, n'ouvre pas un fichier spécial et borne
la lecture à 1 Mio (D82). Un fichier illisible ou refusé décide **comme un fichier absent**, par la table
indicative, et la commande l'annonce comme tel (« ⚠ composer.json non lu ») plutôt que comme vérifié : le
traiter en simple avertissement désarmait le garde-fou, et l'utilisateur pouvait le faire à volonté en
posant un lien.

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

### 7.11 Applications non-PHP : Go, Node, Ruby, Python, custom

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
  → lien /home/shop.fr/.local/bin/node → …/v22.11.0/bin/node
  → liens npm, npx
```

Points d'implémentation :

- `nvm`, `rbenv` et `pyenv` sont des **scripts shell conçus pour un shell interactif** : ils sont
  inutilisables tels quels dans une unité systemd, qui n'en charge aucun profil. Les unités et les crons
  invoquent donc exclusivement les liens de `.local/bin/` (D30, D70), jamais `nvm use`.
- Les commandes d'installation sont exécutées par `su -s /bin/bash -c '…' <id>`, qui **force** le shell
  indépendamment de `/etc/passwd`. Le forçage reste nécessaire alors même que le shell par défaut est
  `/bin/bash` (D62) : un parc peut l'avoir refermé, et un compte adopté peut porter n'importe quoi.
- L'installation télécharge depuis Internet : l'étape est **non critique** (§6.3) et son échec n'annule pas la
  création du site, mais le service applicatif reste alors à l'arrêt avec un message explicite.
- Pour Python, `pyenv` fournit l'interpréteur et `ogs` crée le venv du projet (`python -m venv www/.venv`) ;
  `.local/bin/python` pointe vers le venv, pas vers l'interpréteur nu, afin que les dépendances suivent.
- Pour Go, aucun gestionnaire de version : `ogs` ne compile pas (§1.3). Le déploiement dépose un binaire et
  `.local/bin/app` pointe dessus — le remplacer puis redémarrer le service constitue une bascule atomique.

**Contrepartie assumée du choix « par utilisateur »** : chaque version est dupliquée par site (de l'ordre de
100 Mio pour une version de Node), les mises à jour de sécurité du runtime sont à faire autant de fois qu'il
y a de projets, et le lien `.local/bin/node` pointe vers un binaire situé dans un répertoire que
l'utilisateur contrôle : il peut donc en changer le contenu. Cela ne crée aucune escalade — il exécuterait ce
binaire sous sa propre identité de toute façon. La garantie « la version effective est celle de
l'inventaire » est, pour tous les liens, celle d'un état réconcilié à chaque apply (D70). `ogs status`
compare la version rapportée par `.local/bin/<runtime> --version` à celle déclarée et signale l'écart. Ce
binaire est lancé **sous l'identité du projet**, jamais en root (D82) : l'utilisateur choisit ce que
désigne le lien, et l'exécuter en root lui laisserait choisir ce que root exécute.

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
| `custom` | 5 | port (25xxx) | `LISTEN_ADDR`, `PORT`, développés par systemd dans `spec.app.command` |

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
- **Un port local se juge à son compte** (D94) : tout compte de la machine peut écouter sur un port libre, et
  le vhost relaie à qui répond. L'attribution écarte les ports déjà en écoute ; les ports des unités écrites
  (`Environment=PORT=`) sont repris en base avant toute attribution, pour qu'une `state.db` perdue ne donne
  pas à un projet le port d'un voisin ; et l'écoute d'un projet est relue avec son uid (`/proc/net/tcp`,
  `tcp6`) — une écoute d'un autre compte que celui du projet ou que root, sur 127.0.0.1 ou sur une adresse
  joker, est refusée par l'attente de disponibilité et nommée par `doctor`, qui relit le port dans l'unité
  écrite sans rien attribuer. Root est admis : docker-proxy publie sous root le port d'une application lancée
  par `docker run -p`.

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
ExecStart=/home/shop.fr/.local/bin/node server.js
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
et la commande de reprise, plutôt que de tout annuler. Une exception (D94) : un port déjà tenu par **un autre
compte** au moment du plan fait de l'attente une étape **critique**, en échec, qui arrête l'apply avant le
vhost — celui-ci relaierait le trafic du site à cet autre compte. Pendant l'attente, une écoute étrangère
n'est jamais tenue pour prête.

`ogs app restart|logs|status <id>` est un raccourci vers le service applicatif, et `sudo ogs my restart`
(§7.9.1) le redémarre au même titre que les autres services du projet.

#### 7.11.7 Application custom (D75)

```yaml
  app:
    kind: custom
    command: /home/outil.fr/www/bin/serveur --listen ${LISTEN_ADDR}
    envFile: true               # EnvironmentFile=/etc/ogs/env/outilfr.env
    listen: port                # défaut ; socket si la commande lit SOCKET_PATH
```

L'unité est celle du §7.11.4, `ExecStart` portant la commande telle quelle. Le plan omet l'installation du
runtime et les liens ; il pose le fichier d'environnement **avant** l'unité — systemd refuse de démarrer un
service dont l'`EnvironmentFile` manque — et le laisse ensuite à l'exploitant. Deux conséquences à connaître :
les affectations du fichier l'emportent sur les `Environment=` de l'unité, `PORT` compris ; et une
modification du fichier ne prend effet qu'au redémarrage du service, ogs n'en suivant pas le contenu.

---

### 7.12 Supervision : prisons et métriques

Deux fichiers de portée parc, recalculés en entier à chaque apply comme celui de logrotate, plus un vhost
d'état et les comptes que l'agent emploie (D64). ogs n'installe ni fail2ban ni Telegraf, et se tait quand
l'outil est absent — le critère est la présence de son répertoire de fragments.

#### Configuration

```yaml
# /etc/ogs/config.yaml
monitoring:
  status:
    enabled: true            # absent : ogs décide (actif dès que l'agent est là)
    listen: 127.0.0.1:8099   # boucle locale exigée, refusé au chargement sinon
    path: /ogs-status
  telegraf:
    enabled: true            # absent : actif si /etc/telegraf/telegraf.d existe
    user: telegraf           # compte de l'agent : groupe du fragment, et nom du rôle PostgreSQL
    account: telegraf        # nom des comptes MySQL et RabbitMQ
    interval: 30s
    inputs:                  # une famille absente de la table est activée
      rabbitmq: false
  fail2ban:
    enabled: true            # absent : actif si /etc/fail2ban/jail.d existe
    banTime: 1h
    findTime: 10m
    maxRetry: 5
    ignoreIP: [203.0.113.0/24]   # la boucle locale y est toujours ajoutée
    jails:
      web-badbots: false
```

#### Fichiers produits

| Fichier | Contenu | Droits |
|---|---|---|
| `/etc/telegraf/telegraf.d/ogs.conf` | entrées `apache`/`nginx`, `phpfpm` (un par pool), `procstat` (un par projet), `mysql`, `postgresql`, `rabbitmq` | 0640 root:*agent* |
| `/etc/fail2ban/jail.d/ogs.conf` | prisons `ogs-web-auth`, `ogs-web-noscript`, `ogs-web-badbots`, `ogs-sshd`, `ogs-recidive` | 0644 root:root |
| `/etc/<moteur>/sites-available/ogs-status.conf` | vhost d'état sur la boucle locale | 0644 root:root |

Aucune destination de métriques n'est écrite (D65). Aucun filtre fail2ban n'est écrit : ceux de la
distribution sont employés tels quels.

#### Le vhost d'état

Il expose `<path>/<moteur>` — `stub_status` pour nginx, `server-status` pour Apache — et, pour chaque pool,
`<path>/fpm/<pool>` et `<path>/ping/<pool>`, relayés vers le socket FPM. Il refuse tout le reste et
n'écoute que sur la boucle locale (D66). Les pools exposent `pm.status_path` et `ping.path` sous des noms
préfixés `ogs-`, que les vhosts de projet ne peuvent pas atteindre — ils ne routent vers FPM que les URI
en `.php`.

#### Comptes de supervision

Portée keystore `@parc`, jamais un projet (D67).

| Moteur | Compte | Privilèges | Authentification |
|---|---|---|---|
| MySQL / MariaDB | `monitoring.telegraf.account` | `PROCESS`, `REPLICATION CLIENT`, `SELECT` sur `performance_schema` | mot de passe scellé |
| PostgreSQL | nom de l'utilisateur système de l'agent | `pg_monitor` | `peer`, sans mot de passe |
| RabbitMQ | `monitoring.telegraf.account` | tag `monitoring` seul | mot de passe scellé |

`ogs secrets list @parc` et `ogs secrets show @parc mysql/telegraf` les consultent.

Un compte MySQL ou RabbitMQ homonyme qui existe **sans** mot de passe scellé au keystore est celui de
l'administrateur (D93) : ni son mot de passe, ni ses droits, ni ses tags ne sont touchés, l'étape échoue
sans être critique (code 6) en invitant à choisir un autre `monitoring.telegraf.account`, et le fragment
de l'agent ne porte pas d'entrée pour ce moteur. Le secret d'un compte neuf est scellé **avant** sa pose,
pour qu'une pose interrompue ne laisse pas un compte d'ogs sans preuve ; le fragment n'en porte l'entrée
qu'une fois le compte observé conforme ou posé.

#### Rechargements

`monitoring:fail2ban` valide (`fail2ban-client -t`), recharge, puis constate : un rechargement peut arrêter
le démon, et il est alors relevé — et signalé même si la relève réussit. `monitoring:telegraf` se contente
du constat en aval, faute d'un contrôle de configuration qui n'exécute pas la collecte (D65).

#### Ce que `doctor` porte

- les journaux référencés par les prisons existent-ils encore (D68) ;
- `fail2ban-client -t` accepte-t-il la configuration — le seul contrôle qui couvre filtres, expressions et
  backend d'un coup ;
- le fragment de l'agent est-il lisible par lui, et par lui seul ;
- `mod_status` est-il activé sous Apache, sans quoi le vhost d'état sert une page vide sans se plaindre.

### 7.13 Signature DKIM

ogs tient les clés DKIM des projets qui le demandent et les tables d'OpenDKIM qui les désignent (D69). Il
n'installe ni OpenDKIM ni le MTA, ne touche ni à `opendkim.conf` ni au branchement du milter, et n'écrit
aucun enregistrement DNS : il **émet** celui qu'il faut publier.

Le critère de présence est `/etc/opendkim.conf`, non un répertoire : c'est le fichier qu'OpenDKIM lit, et
le paquet Debian ne crée pas `/etc/opendkim`. Un parc où aucun projet ne demande la signature n'est pas
touché — ni tables vides posées d'office, ni reproche de `doctor` à un OpenDKIM que l'administrateur
tiendrait à la main.

#### Configuration

```yaml
# /etc/ogs/config.yaml
mail:
  dkim:
    enabled: true              # absent : actif si /etc/opendkim.conf existe
    user: opendkim             # compte d'OpenDKIM, propriétaire des clés
    keyBits: 2048              # RSA ; 1024 refusé au chargement
    selectorPrefix: ogs        # sélecteur = <préfixe><AAAAMM>
    retention: 168h            # durée de vie d'une clé remplacée par activate
```

```yaml
# /etc/ogs/projects/shopfr.yaml
spec:
  mail:
    dkim:
      enabled: true
      domains: [shop.fr]       # défaut : les domaines primary du projet
```

Un projet ne signe que s'il le déclare : beaucoup envoient par un relais tiers qui signe de sa propre clé.
La validation refuse `mail.dkim` en préproduction (code 3), un domaine qui n'appartient pas aux `domains` du
projet, et un domaine déjà signé par un autre projet du parc.

#### Fichiers produits

| Fichier | Contenu | Plan | Droits |
|---|---|---|---|
| `/etc/opendkim/keys/<id>/<domaine>/<sélecteur>.private` | clé privée RSA, PEM (PKCS#8) | projet | 0600 opendkim:opendkim, répertoires — `/etc/opendkim` compris — 0750 root:opendkim |
| `/etc/opendkim/ogs/KeyTable` | `<sélecteur>._domainkey.<domaine> <domaine>:<sélecteur>:<chemin>` | global | 0640 root:opendkim — le régime d'`ogs harden` |
| `/etc/opendkim/ogs/SigningTable` | `*@<domaine> <sélecteur>._domainkey.<domaine>` | global | 0640 root:opendkim |

La clé est écrite dans le plan de projet, les tables dans le plan global, qui s'exécute après : une table ne
référence jamais une clé pas encore posée — ni une clé dont le fichier manque, qu'elle ne désigne pas. Le
fichier d'un domaine qui cesse d'être signé est retiré à la fin du plan global, **après** le rechargement
des tables : OpenDKIM garde en mémoire celles qu'il a chargées, et retirer le fichier d'abord lui ferait
retenir le courrier du domaine. La clé, elle, reste scellée : réactiver la signature retrouve
l'enregistrement déjà publié. Les tables sont **recalculées en entier** depuis le parc, jamais
depuis les projets visés, et à la suppression d'un projet — comme `/etc/logrotate.d/ogs`. Elles ne
désignent que la clé **active** de chaque domaine : une clé préparée par `rotate` a son fichier, pas encore
sa ligne.

La clé privée est scellée dans le keystore sous `<projet>/dkim/<domaine>/<sélecteur>` ; le fichier en
dérive. Absent, il est réécrit depuis le keystore, **jamais régénéré** : une clé neuve exigerait de
republier l'enregistrement. Différent du contenu scellé, il est « modifié à la main » et `apply` refuse de
l'écraser sans `--force` (§6.4).

`opendkim.conf` reste à l'administrateur, qui y pointe les deux tables :

```
KeyTable      refile:/etc/opendkim/ogs/KeyTable
SigningTable  refile:/etc/opendkim/ogs/SigningTable
```

#### Enregistrement à publier

`ogs mail dkim records <projet>` émet, pour chaque domaine, l'enregistrement de la clé active et celui de
la clé préparée s'il y en a une, au format zone ou en JSON (`--output json`) :

```
ogs202609._domainkey.shop.fr. IN TXT ( "v=DKIM1; k=rsa; p=MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA…"
                                       "…IDAQAB" )
```

Une clé RSA-2048 dépasse les 255 octets d'une chaîne TXT : l'enregistrement est découpé en plusieurs
chaînes, et la comparaison de `doctor` les recolle avant de comparer.

#### Commandes

```
ogs mail dkim enable <projet> [--domain d]…   demande la signature (défaut : les domaines primaires)
ogs mail dkim disable <projet>                 cesse de signer ; les clés restent scellées
ogs mail dkim show <projet>                    clés et états, jamais leur partie privée
ogs mail dkim records <projet>                 enregistrements à publier (zone ou JSON)
ogs mail dkim rotate | activate <projet>       rotation en deux temps
```

#### Rotation

1. `ogs mail dkim rotate <projet> [--domain <d>]` génère une clé sous un nouveau sélecteur et l'écrit. La
   signature ne change pas.
2. L'exploitant publie l'enregistrement que `records` affiche.
3. `ogs mail dkim activate <projet> [--domain <d>]` vérifie que l'enregistrement publié correspond à la clé
   préparée, puis bascule les tables. Sans correspondance, il refuse (code 5) ; `--force` passe outre.
4. L'ancienne clé reste scellée et son fichier en place pendant `mail.dkim.retention` (défaut 7 jours) : son
   enregistrement doit survivre aux courriers encore en transit, et c'est à l'exploitant de le retirer.

Basculer vers une clé non publiée casserait la signature de tout envoi, et sous un DMARC `p=reject` sans
SPF aligné, rejetterait le courrier. À la **création**, en revanche, la signature est immédiate : une
signature invérifiable ne vaut pas moins qu'une absence de signature.

#### Rechargement

`mail:opendkim` est une intention groupée comme les autres : `opendkim -n -x /etc/opendkim.conf` valide la
configuration — il ouvre les tables, et en refuse une introuvable —, puis `systemctl reload-or-restart
opendkim` et le constat `is-active`, avec relève du démon s'il est tombé. Seules les tables la déclarent :
OpenDKIM relit le fichier d'une clé à chaque message, et recharger à l'écriture d'une clé ferait valider une
configuration qui désigne des tables pas encore écrites. Les étapes sont **non critiques** : une clé qui
n'est pas posée ne doit pas défaire un site par ailleurs fonctionnel.

#### Ce que `doctor` porte

- l'enregistrement publié correspond-il à la clé active de chaque domaine — comparé par ogs lui-même,
  chaînes recollées et blancs retirés, par le code qui garde `activate` : `opendkim-testkey` n'est que dans
  `opendkim-tools`, et deux vérifications distinctes pourraient se contredire ;
- les clés sont-elles en 0600 et au compte d'OpenDKIM, sans quoi `RequireSafeKeys` les refuse — et le
  courrier du domaine reste en file ;
- `opendkim.conf` référence-t-il les deux tables d'ogs — sinon rien n'est signé, et rien ne le dit ;
- une clé retirée des tables a-t-elle encore un enregistrement publié, à retirer à la main.

### 7.14 Pare-feu ufw

ogs réconcilie ufw avec `/etc/ogs/ufw.yaml` (D72). Sans ce fichier, il ne touche à rien : ni profil, ni
règle, ni politique. Il n'installe pas ufw ; déclaré sans ufw présent, le pare-feu produit un
avertissement (code 6) à chaque apply.

#### Déclaration

```yaml
# /etc/ogs/ufw.yaml
active: true                 # défaut ; false réconcilie règles et profils, ufw désactivé
policy:
  incoming: deny             # allow | deny | reject — défauts d'ufw : deny
  outgoing: allow            #                                      allow
  routed: deny               # tu : laissé à ufw
logging: low                 # off | on | low | medium | high | full ; tu : laissé tel quel
rollbackAfter: 5m            # retour automatique d'une première activation ; 0 s'en passe

sources:                     # à qui appartiennent les adresses
  bureau:
    owner: "Agence de Nantes"
    description: "sortie fibre et VPN"
    addresses: [203.0.113.10, 2001:db8:42::/48]
  acme:
    owner: "ACME SARL"
    addresses: [198.51.100.7]

trusted: [bureau]            # accès à tous les profils disponibles, une règle par profil

profiles:
  ssh:  {allow: []}          # trusted suffit
  web:  {allow: [any]}
  mysql:
    allow:
      - acme                 # source nommée
      - address: 192.0.2.10  # adresse, avec commentaire
        comment: "réplica de secours"
  redis: {ports: [6380/tcp]} # remplace les ports détectés
  ftp:   {passive: "40000:40100"}
  elastic:                   # hors catalogue : ports obligatoires
    ports: [9200/tcp]
    units: [elasticsearch]   # disponible si l'unité existe
    allow: [bureau]

ban:
  - 192.0.2.66
  - {address: 198.51.100.0/24, comment: "scan"}

outgoing:                    # lu seulement si policy.outgoing ≠ allow
  allow: [dns, https, http, ntp, smtp, submission, smtps, ssh, 5432/tcp]
```

Une entrée (`trusted`, `allow`, `ban`) s'écrit en chaîne — une adresse, un réseau, `any` ou le nom d'une
source — ou en table pour porter un commentaire. Refusé au chargement (code 3) : une source inconnue, un
réseau aux bits d'hôte non nuls (`10.0.0.5/24` : ufw le ramènerait à son réseau en IPv4 et non en IPv6,
et la relecture ne correspondrait plus), `any` en confiance ou en bannissement, une adresse bannie qui en
recouvre une autorisée, une liste ou une plage de ports sans protocole (ufw écarterait le profil entier
d'un simple avertissement), un profil hors catalogue sans `ports`, une politique sortante fermée dont les
sorties omettent `dns` ou `https`, une clé inconnue.

#### Profils

| Profil | Ports par défaut | Disponible si | Ports lus dans |
|---|---|---|---|
| `ssh` | 22/tcp | `ssh.service`, `sshd.service` | `sshd_config` et ses `Include` — chaque `Port` s'ajoute ; un commentaire de fin de ligne (`Port 2222 # …`) est ôté, comme le fait sshd, un « # » collé à un mot en faisant partie |
| `sftp` | 2222/tcp | `proftpd.service` et un contexte `SFTPEngine on` | le `Port` de ce contexte |
| `ftp` | 21/tcp + plage passive | proftpd (contexte hors SFTP, `Port` ≠ 0), vsftpd, pure-ftpd | `PassivePorts`, `pasv_min_port`/`pasv_max_port`, `PassivePortRange` |
| `web` | 80,443/tcp | le moteur retenu ; Apache et nginx installés sans `web.engine`, disponible quand même — un serveur qu'on ne sait pas nommer n'est pas l'absence de serveur web (D86) | — |
| `mysql` | 3306/tcp | mariadb, mysql, mysqld | — |
| `postgresql` | 5432/tcp | postgresql | — |
| `redis` | 6379/tcp | redis-server, redis | `redis.conf` ; `port 0` : indisponible |
| `rabbitmq` | 5671,5672/tcp | rabbitmq-server | — |
| `rabbitmq-management` | 15672/tcp | idem + greffon `rabbitmq_management` | — |
| `smtp` | 25/tcp | postfix, exim4 | — |
| `submission` | 465,587/tcp | postfix, exim4 | — |

Le critère est l'**unité installée**, non son état : un service arrêté reste un service de la machine, et
ses règles doivent exister le jour où on le redémarre. `profiles.<nom>.available` force la disponibilité
ou l'interdit. Le SFTP servi par sshd n'est pas un profil distinct : c'est le port de sshd. ogs **lit** la
configuration de proftpd, il ne l'écrit pas et ne crée aucun compte SFTP — l'arbitrage du 24/08/2026 sur
l'accès SFTP reste entier.

Le fichier de profils porte **tous** les profils, disponibles ou non : une définition ne coûte rien. Seul
un profil disponible reçoit des règles. `ogs firewall profiles` les énumère avec leurs ports, leurs
sources et la raison de leur disponibilité.

#### Règles et commentaires

| Famille | Commande | Commentaire engendré |
|---|---|---|
| accès | `ufw allow from <adresse> to any app ogs-<profil>` | `ogs: acme (ACME SARL)` ; `ogs: accès à mysql` ; `ogs: accès public à web` |
| confiance | idem, pour chaque profil disponible | `ogs: confiance — bureau (Agence de Nantes)` |
| bannissement | `ufw prepend deny from <adresse>` | `ogs: bannie` ; `ogs: bannie — scan` |
| sortie | `ufw allow out <port>[/<proto>]` | `ogs: sortie dns` |

Le commentaire dit ce qu'`ufw status` ne montre pas — le profil et l'adresse y sont déjà : à qui
appartient l'adresse, et à quel titre elle est là. Un commentaire déclaré le remplace, toujours précédé de
`ogs:`. Une adresse littérale couverte par une source en prend le propriétaire.

La réconciliation relit `user.rules` et `user6.rules`, dont les lignes `### tuple ###` sont la
représentation qu'ufw lui-même relit. Une règle voulue absente est posée ; présente sous un autre
commentaire, elle est corrigée en place ; un refus précédé d'une autorisation est retiré puis replacé en
tête. Une règle marquée qui n'est plus voulue est retirée — **après** les ajouts, pour qu'un changement
d'adresse n'ouvre jamais de fenêtre sans accès. Une règle marquée mais retouchée à la main (une interface
ajoutée, une journalisation) n'est ni reprise ni retirée : `doctor` la nomme.

#### Ordre et gardes

1. retrait des règles dont le profil disparaît, tant qu'ufw le connaît encore ;
2. fichier de profils, puis `ufw app update` des profils dont les ports changent et qu'une règle emploie ;
3. bannissements, accès, sorties ;
4. politiques qui s'ouvrent (vers `allow`) ;
5. retrait des règles devenues sans objet ;
6. journalisation, politiques qui se ferment ;
7. activation.

Les étapes sont non critiques — le pare-feu est une ressource de parc, et son échec ne doit pas dérouler
le reste du plan global —, mais tout ce qui retire un accès — retraits, politiques qui se ferment,
activation — **refuse de s'appliquer si une règle a échoué** : retirer l'ancienne adresse quand la
nouvelle n'a pas été autorisée déplacerait l'accès nulle part. Une politique qui s'ouvre passe avant les
retraits : retirer les sorties permises sous une sortie encore fermée couperait DNS et ACME le temps de
l'apply. Une règle dont le retrait échoue garde son profil défini, ufw refusant sinon à jamais de la
retirer. Une règle de l'administrateur de même définition qu'une règle voulue la satisfait, et reste la
sienne : ogs ne lui donne pas sa marque.

Refusé au plan : une adresse IPv6 alors qu'`IPV6=no`, un FTP autorisé dont la plage passive est
inconnue, un `web` fermé alors qu'un projet émet ses certificats par HTTP-01 — y compris par un profil web
indisponible ou écarté, dont les règles marquées partiraient.

Refusé sauf **`--allow-lockout`** (D86), option des seules commandes `ogs firewall` (`plan`, `apply`, `ban`,
`unban`) — le `--force` des fichiers retouchés (§6.4) ne la lève jamais, le plan global jamais non plus :
une politique entrante fermée sans aucune source SSH. Refusé de même, quand le plan **resserre** le
pare-feu — retrait de règle, refus nouveau, politique entrante, activation, ou **ports changés d'un profil
qu'une règle emploie**, `ufw app update` fermant l'ancien port :

- une session SSH en cours que rien ne laisserait plus passer, adresse et port ensemble ;
- une session dont l'adresse serait bannie ;
- un port où sshd **écoute réellement** (`ss -tlnp`, et les `ListenStream` d'une `ssh.socket` active) que
  plus rien n'ouvrirait — ni un profil, qu'il soit disponible, écarté ou non détecté, ni une règle de
  l'administrateur. Aucune lecture de `sshd_config` ne couvre toutes ses syntaxes, et c'est ce port-là que
  la politique par défaut coupe.

Les gardes jugent **ce qui restera ouvert** après le plan : les règles voulues de tout profil qui ouvre les
ports visés, et les règles de l'administrateur qu'il laisse en place. Une règle de l'administrateur compte
si c'est une autorisation entrante (`allow` ou `limit`, journalisée ou non) qui ne restreint pas le port
d'origine ; une règle de routage, de sortie, à port d'origine ou venant de la boucle locale ne compte pas.
Sans session à protéger, elle compte même liée à une interface autre que `lo`, ou visant une adresse du
serveur autre que la boucle locale. Pour une session, elle doit admettre son adresse et, si elle vise une adresse du serveur, celle que
la session a jointe — 3e champ de `SSH_CONNECTION`, l'adresse vue après translation, celle que filtre ufw ;
une règle liée à une interface ne la couvre pas, rien ne disant par où la session est arrivée, et le refus
le dit. Les ports visés pour la session sont ceux où sshd écoute ; à défaut, celui où elle est arrivée
(4e champ) ; à défaut, ceux du profil. Le port de la session ne compte pas quand `ss` dit que sshd n'y
écoute plus : il refusait le plan même qui ouvrait le port où sshd avait été déplacé. Une règle de
l'administrateur sur un profil `ogs-*` prend les nouveaux ports de ce profil.

Limite connue : sous tmux ou screen, un shell garde le `SSH_CONNECTION` de la connexion qui l'a créé ;
rattaché depuis une autre adresse, la garde contrôle l'ancienne. Un refus, comme un `ufw.yaml`
invalide, n'est jamais tenté : `ogs firewall plan|apply` rend le code 3 ; le plan global en fait une
étape non bloquante en échec — le reste du parc s'applique, et l'apply rend le code 6 en nommant le motif.
`ogs firewall confirm` ne lit pas `ufw.yaml` : un fichier cassé depuis l'activation ne doit pas empêcher de
garder un pare-feu qui fonctionne. L'adresse de la session est lue
dans `SSH_CONNECTION` ; sudo l'effaçant (`env_reset`), ogs la cherche dans l'environnement de ses
processus parents.

#### Première activation

Quand ufw passe d'inactif à actif, ogs programme d'abord `systemd-run --unit=ogs-ufw-rollback
--on-active=<rollbackAfter> ufw --force disable`, puis active. `ogs firewall confirm`, lancé depuis une
**nouvelle** session — c'est elle qui prouve que le pare-feu laisse passer l'administration —, arrête le
minuteur. Faute de confirmation, ufw se désactive de lui-même. Un minuteur qui ne peut pas être programmé
interdit l'activation ; `rollbackAfter: 0` s'en passe.

Avant le minuteur, ogs pose le **témoin** `/var/lib/ogs/ufw-unconfirmed` (0600 root, D86). Il est retiré par
`confirm`, par la compensation et par la désactivation que demande `active: false` ; si le minuteur ne
peut pas être programmé, le témoin est retiré et l'activation refusée. Il distingue trois états :

- **en attente** : minuteur actif ;
- **retour perdu** : ufw actif, plus de minuteur — ce que laisse un redémarrage dans le délai, l'unité
  transitoire vivant sous `/run`. `status` et `doctor` le signalent, et `confirm`, depuis une nouvelle
  session, confirme ;
- **retour joué** : ufw inactif. `confirm` rend 6 et renvoie à `ogs firewall apply`.

`confirm` sur un ufw inactif rend 6, sauf si `ufw.yaml` est absent ou déclare `active: false` ; il n'est lu
que pour cela, et cassé il ne décide rien. Le témoin rend l'état visible, il ne réarme rien : un redémarrage
dans le délai ne vaut pas retour (§20).

#### fail2ban

Les adresses de confiance rejoignent l'`ignoreip` de chaque prison `ogs-` (D68) : une adresse qui a accès
à tout est d'ordinaire celle d'où l'on administre, et quelques mots de passe mal tapés suffiraient à la
bannir. `ogs firewall apply` ne réconcilie que le pare-feu ; les prisons suivent au prochain `ogs apply`.

#### Commandes

```
ogs firewall init                      ufw.yaml de départ d'après ce qui est installé
ogs firewall status | profiles | rules | sources
ogs firewall whois <adresse>           à qui elle appartient, ce qui lui est permis
ogs firewall plan | apply [--allow-lockout]    le pare-feu seul
ogs firewall confirm                   confirme une première activation
ogs firewall ban <adresse|source> [--comment] [--no-apply] [--allow-lockout]
ogs firewall unban <adresse|source> [--no-apply] [--allow-lockout]
```

`ban` et `unban` écrivent `ufw.yaml` — il reste la source de vérité (D1) — en passant par l'arbre du
document : les commentaires de l'exploitant, qui disent souvent pourquoi une adresse est là, survivent. Le
document modifié est validé avant d'être écrit. Sous `--dry-run`, ils refusent l'option (code 2) au lieu de
réécrire `ufw.yaml` (D88). Un bannissement que la garde refuse reste en revanche écrit dans `ufw.yaml`, où
le prochain apply le retrouve (§20).

#### Ce que `doctor` porte

- ufw installé, actif si `ufw.yaml` le veut, activation en attente de confirmation — ou jamais confirmée
  et privée de son retour automatique, que le témoin révèle après un redémarrage ;
- pour chaque profil disponible qui a des règles, un port qui n'écoute nulle part — le service arrêté, ou
  un sshd déplacé que le profil n'ouvre pas — ou qui n'écoute que la boucle locale, où la règle n'ouvre
  rien ;
- Docker installé : il publie les ports de ses conteneurs par ses propres chaînes, examinées avant celles
  d'ufw ;
- les règles marquées mais retouchées, et le nombre de règles de l'administrateur, laissées intactes.

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

**Corrélation** : regroupement par nom — le même aux séparateurs près (`shop_fr`, `shop-fr`, `shopfr`),
**jamais un préfixe** (D78) —, par docroot commun, par utilisateur du pool, par socket FPM référencé dans un
vhost. Une base n'est rattachée qu'une fois tous les candidats connus, et au seul que son nom désigne :
son identifiant, ou le compte de son pool ; des candidats de même identifiant — un vhost doublé en
`-le-ssl` par certbot — comptent pour un seul projet. Un compte de base n'est rattaché que s'il est seul de
son nom, hôtes compris. L'identifiant proposé est celui que produirait la règle de dérivation
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
- Toute ressource non adoptée reste « non gérée » : aucune déclaration ne la porte, donc le moteur ne la
  planifie jamais. Rien n'en est mémorisé (D95) : `import unmanaged` inspecte le serveur au moment de la
  demande, et les conflits se jugent sur le système à chaque plan — compte sans la marque `ogs=` (§7.1),
  home préexistant (§7.1.7), domaine servi par un vhost étranger, fichier sans en-tête (§6.4) —, non sur un
  relevé fait à l'import, qui ne verrait ni ce qui apparaît ni ce qui disparaît ensuite.
- Après adoption, un `ogs diff <projet>` montre l'écart entre l'existant et ce que `ogs` produirait. Le
  premier `apply` normalise la configuration (et le montre avant d'agir).
- **Une base dont le nom désigne plusieurs projets, ou dont aucun compte — ou plus d'un, hôtes compris —
  ne porte le nom, n'est pas déclarée** (D78) : elle reste non gérée, chaque candidat concerné reçoit une
  note, et l'adoption affiche avant la confirmation les bases qu'elle déclare, avec leur compte et leur
  hôte. Une base déclarée l'est sous l'**hôte réel** de son compte (`host: '%'`). ogs ne crée, n'accorde ni
  ne supprime jamais un compte deviné ; une base qu'aucun nom ne rattache se déclare à la main dans
  `spec.databases`.
- **Un vhost adopté garde son fichier** : l'import porte son nom réel dans `spec.web.vhosts[].file` (D76),
  que l'apply reprend en place — sous `--force`, l'original étant sauvegardé — au lieu d'écrire un second
  fichier à côté. Le pool d'origine d'un projet adopté, laissé actif par un `php switch`, est signalé et
  jamais retiré.
- Un home adopté sous celui d'un autre projet est refusé par le contrôle d'unicité (§3.3.6, D77).

**Migration vers une autre machine** : quand les sites changent de serveur, l'import ne s'applique pas —
la reprise passe par `docs/migrer.sh` et son manifeste (D90), exécutés en root sur la nouvelle machine, qui
tire fichiers et bases de l'ancienne par ssh. Le script ne touche ogs que par ses commandes : il dépose
chaque déclaration, crons et services désactivés, puis synchronise, vérifie et bascule par vagues, et sait
revenir en arrière. Il refuse un projet dont un autre, sur la cible ou dans le manifeste, partage une base,
un compte, un home ou un domaine, et toute resynchronisation une fois la bascule engagée.

**Certificats certbot** : deux options à l'adoption — conserver les chemins certbot et laisser son timer
opérer, ou migrer vers le stockage `ogs` avec réémission (recommandé pour l'homogénéité, coûte une émission
ACME).

**Clés DKIM** : une `KeyTable` existante est **signalée**, non adoptée en v1. Reprendre une clé exigerait de
la sceller telle quelle et de garder son sélecteur — le seul moyen de ne pas republier chez le client —, et
de ne pas écraser les tables de l'administrateur, qu'OpenDKIM n'accepte qu'en un exemplaire (D69).

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
├── mail dkim enable | disable <projet>
├── mail dkim show | records <projet>  clés DKIM et enregistrements à publier (§7.13)
├── mail dkim rotate | activate <projet>
│
├── firewall init | status | profiles | rules | sources   pare-feu ufw (§7.14)
├── firewall whois <adresse>
├── firewall plan | apply | confirm   --allow-lockout lève les gardes contre la coupure (D86)
├── firewall ban | unban <adresse|source>
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
  --cron "* * * * *:bin/console schedule:run"
```

Le moteur web n'est pas une option de `site create` : il est celui de la machine (D73).

L'identifiant n'est pas saisi : il est dérivé du host (`shop.fr` → `shopfr`, §3.3). `--id` permet de le
forcer en cas de collision ou de troncature jugée illisible ; `--host` dissocie le host servi du host
canonique.

Variante préproduction — projet entièrement distinct, sans ressource partagée avec la production :

```
ogs site create shop.fr --env prep
  → host effectif  shop.preprod-pp.net
  → identifiant    shopfrprep
  → home           /home/shop.preprod-pp.net
  → certificat     *.preprod-pp.net (partagé, déjà émis) — aucun appel ACME
```

Comportement : construit le projet en mémoire, écrit `/etc/ogs/projects/shopfr.yaml`, affiche le plan, demande
confirmation, applique en saga, puis affiche le récapitulatif avec les chaînes de connexion générées.

Options transverses : `--dry-run`, `--yes`, `--non-interactive`, `--force`, `--output`, `--verbose`,
`--quiet`, `--no-hooks`, `--config`. `--dry-run` est **simulé** par les commandes qui le savent et
**refusé** — code 2, avant toute préparation — par les autres, jamais ignoré (D88) : la conduite de chaque
commande est inscrite dans `internal/cli/dryrun.go`. `--force` reprend un fichier retouché ou non géré, après
sauvegarde (§6.4) ; il ne donne jamais à un projet le fichier d'un autre et ne lève pas les gardes du
pare-feu, qui ont leur option (D86).

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
| 2 | erreur d'usage (arguments invalides, `--dry-run` demandé à une commande qui ne sait pas simuler, entrée de restauration refusée) |
| 3 | échec de validation (spécification incohérente) |
| 4 | conflit (ressource déjà existante ou possédée par un autre projet) |
| 5 | dépendance absente (paquet, module, plugin, service arrêté) |
| 6 | succès partiel (étapes non critiques en échec) |
| 7 | verrou indisponible (autre instance en cours) |
| 8 | privilèges insuffisants (non root) |
| 9 | opération annulée par l'utilisateur — refus à la confirmation, confirmation exigée en mode non interactif sans `--yes`, ou interruption (SIGINT, SIGTERM), notamment pendant le calcul d'un plan, d'une suppression avant son point de non-retour (D77) ou d'une copie de clone (D82) |

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
| `preDelete` | avant toute suppression, une fois les refus d'entrée levés, la confirmation donnée et la taille de l'archive admise ; les remparts étant éprouvés de nouveau après l'archive, une suppression peut encore être abandonnée après un `preDelete` réussi (D77) | oui |
| `postDelete` | après suppression, archivage et recalcul des fichiers de parc | non |
| `preApply` / `postApply` | encadrent tout apply | non |
| `postCertRenew` | après renouvellement réussi, même si un autre a échoué | non |
| `postPhpSwitch` | après bascule de version | non |

Contrat : le script reçoit le contexte JSON sur stdin (projet, action, ressources touchées, chemins), les
variables `OGS_PROJECT`, `OGS_ACTION`, `OGS_ROOT` dans l'environnement, s'exécute sous root avec un délai
maximal de 300 s (configurable), et son stdout/stderr est capturé dans les logs et l'audit. Un code de
retour non nul sur un hook critique interrompt l'opération et déclenche le rollback. `--no-hooks` désactive
l'ensemble.

Aucun hook ne s'exécute sous `--dry-run` (D40, D88) : une commande qui simule s'arrête avant son hook `pre`,
une commande qui ne sait pas simuler refuse l'option avant tout geste — `project delete --dry-run` ne lance
ni `preDelete` ni `postDelete` —, et le `hook.Runner` lui-même, sous ce drapeau, résout le script, contrôles
de sécurité compris, puis l'annonce « non exécuté (--dry-run) » : un hook qui serait refusé à l'exécution
l'est aussi en simulation.

---

## 13. Archivage et suppression

`ogs project delete shopfr` (mode par défaut, D6) :

1. **Verrou et remparts** (D77). Le verrou global est pris comme pour un apply, puis **tous les refus**
   sont prononcés, avant la confirmation, en un seul message où chacun porte son remède — le code est
   celui du premier : ce que rien ne lève (racine dangereuse par sa forme, partagée avec un autre projet
   ou l'englobant, liée, au mauvais propriétaire ; point de montage sous la racine ou sous les journaux ;
   fichier qu'ogs n'a pas écrit — sans son en-tête, illisible, ou lien vers un fichier hors du projet — et qu'il laisserait en place), puis ce que lève `--force`
   (compte sans la marque `ogs=`, bases et comptes SGBD d'un projet adopté, nommés avant la confirmation),
   puis ce que lève `--no-dump` (base inobservable). Viennent ensuite la confirmation, la taille de
   l'archive et l'espace disque.
2. Hook `preDelete`.
3. Arrêt des services du projet — sa cible systemd ; le cron et le pool restent actifs jusqu'aux étapes 6
   et 8.
4. Constitution de l'archive `/var/lib/ogs/archives/shopfr-20260822-1430.tar.zst` contenant : YAML du projet,
   configurations générées qui vont partir, dumps de bases (`mysqldump` / `pg_dump`), secrets **chiffrés**
   — fichier d'environnement de D75 compris —, arborescence `/home/shop.fr`, journaux du projet
   (`<logs.root>/<id>`, D71), et un manifeste JSON décrivant tout ce qui a été supprimé. L'arborescence et
   les journaux sont parcourus par descripteurs, sans suivre aucun lien (D81). Un dump en échec
   **abandonne** la suppression : une base ne part jamais sans sa sauvegarde.
5. **Point de non-retour.** Une interruption reçue jusqu'ici, ou un rempart que l'archive a laissé naître
   — un partage monté pendant ce temps, une déclaration voisine —, fait retirer l'archive et relancer les
   services : code 9 ou code du refus, rien n'est retiré. Au-delà, le contexte est détaché : la suppression
   va à son terme, et une interruption n'est que signalée.
6. Retrait des fichiers générés dont l'en-tête désigne le projet (`fs.ManagedProject`, D76) : unités, cron,
   vhosts et leurs liens, pool, sudoers ; les répertoires de surcharge (D63) s'ils sont vides. Un fichier
   qu'ogs a écrit pour un autre projet reste en place, nommé (code 6) ; celui qu'ogs n'a pas écrit a été
   refusé à l'étape 1, sauf sous `--keep-data`, où il est lui aussi laissé et nommé. Le fichier
   d'environnement de D75, lui, ne porte aucun en-tête — ogs n'en écrit jamais le contenu — et part **dans
   tous les cas**, `--keep-data` et `--purge` compris : il porte des secrets. Sa copie scellée reste dans
   l'archive ; sous `--purge`, il n'en reste rien. Le compte rendu nomme les enregistrements DKIM à retirer
   du DNS : ogs ne les a pas publiés (D69).
7. Suppression des ressources vivantes : bases et utilisateurs SGBD — ceux d'un projet adopté sous
   `--force` seulement ; hors adoption, ceux qui sont à ogs seulement (D91), un homonyme qu'il n'a pas
   créé restant en place, nommé, code 6 ; un compte réservé jamais —, vhost et utilisateur AMQP. Le mot
   de passe scellé d'un compte supprimé quitte le keystore ; l'archive en garde la copie. Le certificat du projet n'est ni retiré, ni révoqué, ni
   archivé (§20, limites connues).
8. **Rechargement des serveurs** — PHP-FPM et le moteur web — puis `daemon-reload`. Cette étape précède
   impérativement la suivante : les workers du pool tournent sous l'utilisateur du projet, et `userdel`
   refuse un compte dont un processus vit encore. Un serveur à l'arrêt n'a rien à recharger et ne produit
   donc aucun avertissement. Le rechargement du moteur web revalide la configuration au préalable (§6.5).
9. Suppression de l'utilisateur système, du groupe — `www-data` en est d'abord retiré (§7.1.6) — et de
   l'arborescence, dont les remparts sont éprouvés une dernière fois juste avant l'effacement ; puis des
   journaux sous `<logs.root>/<id>` (D71), sauf point de montage, et du répertoire d'exécution
   `/run/ogs/<id>`.
10. Le projet est marqué `archived` en base ; son YAML est déplacé dans `projects/archived/`.
11. Recalcul des fichiers de parc — prisons fail2ban, rotation, tmpfiles, Telegraf, tables d'OpenDKIM —,
    puis retrait des clés DKIM du projet, une fois les tables rechargées sans elles.
12. Hook `postDelete`.

Avant le point de non-retour, tout refus, tout dump en échec et toute interruption arrêtent le geste et
relancent ce qui a été arrêté (D77). Après lui, une étape en échec n'interrompt pas les suivantes : ce qui
peut être démonté l'est, et la commande rend le **code 6** en énumérant ce qui subsiste. Une suppression
annoncée comme un succès alors qu'elle laisse un compte et un home derrière elle est le pire des états
possibles.

Options : `--purge` supprime sans archiver ; sa confirmation exige la saisie de l'identifiant, et ni
`--yes` ni le mode non interactif ne la remplacent (code 9). `--keep-data` retire la configuration — vhost,
pool, cron, sudoers, unités, fichier d'environnement, clés DKIM — et sort le projet de l'inventaire, mais
garde le compte, les bases, le vhost AMQP, le home et les journaux. Aucun rempart ne la refuse : un fichier
qu'ogs n'a pas écrit pour ce projet est seulement laissé et nommé (code 6), l'archive ne recopie pas une
arborescence ni des journaux que la suppression n'aurait pas pu effacer, et un dump en échec n'y coûte
qu'un avertissement. `--no-dump` archive sans les bases. `--dry-run` est refusé (code 2, D88) : une
simulation fidèle rejouerait tous les remparts.

`ogs project restore <archive>` recrée le projet depuis une archive, dans cet ordre : déclaration rejouée
— refusée si le projet existe déjà (code 4) —, secrets réinjectés scellés, fichier d'environnement
réécrit, apply du projet, puis l'arborescence remise dans le home **après** l'apply, et enfin, sauf
`--with-data=false`, les dumps rechargés. Nécessite la même clé maître. L'extraction ne suit aucun lien et
rend chaque entrée à qui la possédait (D81) : ce que l'apply avait posé et que l'utilisateur avait remplacé
revient tel qu'il l'avait laissé — un `authorized_keys` régulier, un script à la place du lien `php`, un
répertoire à la place de `~/logs` —, et l'apply suivant le traite selon ses règles (D61, D70, D71), avec le
code 6 tant que ces fichiers restent en place. L'ancien `~/logs` d'une archive antérieure à D71 revient à
root, et c'est l'apply suivant qui le migre, ce que la restauration annonce. Une arborescence restaurée en
partie rend le code 6 ; `--dry-run` est refusé (code 2).

Garde-fous : si la taille cumulée à archiver dépasse un seuil configurable (défaut 5 Gio), confirmation
explicite avec l'estimation affichée ; vérification de l'espace disque disponible, avant que rien ne soit
arrêté — une arborescence que l'archive ne recopie pas n'y compte pas.

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

- `status` : vue consolidée par projet — services (actifs/attendus), expiration des certificats — un
  certificat expiré est dit « expiré », et n'est plus présenté (D79) —, profondeur des queues, pools FPM
  (processus actifs/max), dérive de configuration, dernière exécution des crons. Code de retour non nul si un problème est détecté, ce qui le rend utilisable en supervision.
- `doctor` : vérifie paquets et binaires attendus, modules Apache/Nginx, plugin RabbitMQ management,
  accessibilité des sockets SGBD, permissions de `/etc/ogs` et de la clé maître, cohérence entre la base
  d'état et le système, présence des unités `ogs-renew`, versions PHP utilisables, clés DKIM et
  enregistrements publiés (§7.13), chaîne de propriété et espace libre du répertoire des journaux (D71),
  ports écoutés des profils du pare-feu, Docker en amont d'ufw et activation jamais confirmée, que le
  témoin révèle après un redémarrage (§7.14, D86). Chaque anomalie est accompagnée de la commande de
  correction.

---

### 14.5 Logs des projets et rotation

Chaque projet dispose d'un unique répertoire de logs d'infrastructure, **`<logs.root>/<id>/`** —
`/var/log/ogs-projects/<id>/` par défaut —, hors du home (D71). Le home n'en porte qu'un lien, `~/logs`.

```
/var/log/ogs-projects/        0711  root:root     traversable, non énumérable : les voisins ne se voient pas
└── shopfr/                   0750  root:shopfr   le projet lit et liste ; seul root crée ou renomme
    ├── apache-access.log     0640  root:shopfr   écrit par le maître Apache (root)
    ├── apache-error.log      0640  root:shopfr
    ├── php-slow.log          0640  root:shopfr   écrit par le maître FPM (root)
    ├── php-error.log         0640  shopfr:shopfr écrit par les workers, sous l'identité du projet
    └── cron-scheduler.log    0640  shopfr:shopfr écrit par la tâche
/home/shop.fr/logs → /var/log/ogs-projects/shopfr  lien de commodité, au compte du projet
```

| Fichier | Producteur | Propriétaire |
|---|---|---|
| `apache-access.log`, `apache-error.log` | maître Apache (root) | `root:<id>` |
| `nginx-access.log`, `nginx-error.log` | maître nginx (root) | `root:<id>` |
| `php-slow.log` | maître PHP-FPM (root) | `root:<id>` |
| `php-error.log` | workers PHP-FPM (utilisateur du projet) | `<id>:<id>` |
| `cron-<nom>.log` | tâche cron (utilisateur du projet) | `<id>:<id>` |

Tous sont **pré-créés** par ogs : le répertoire n'étant inscriptible que par root, un worker ou une tâche ne
pourrait pas les créer — c'est ce qui faisait retomber les erreurs PHP dans le journal global de FPM, mêlées
à celles des autres projets —, et un maître root les créerait avec son umask, en 0644 ou 0600.

La chaîne de propriété est la garantie : aucun composant du chemin n'appartient à l'utilisateur, qui n'a
donc rien à renommer ni à substituer. Le lien `~/logs` ne rouvre rien — seuls l'utilisateur et ses propres
outils le suivent —, et il est posé comme les liens gérés (D70), sans suivre de lien, rétabli à chaque apply.
`spec.layout.logDir` est refusé hors de `logs.root`, et `logs.root` lui-même est contrôlé par `doctor`.

Les logs **applicatifs** (Symfony, Monolog) restent sous `var/log/`, entièrement gérés par l'application, et
ne sont pas rotés par `ogs`. Les logs RabbitMQ n'ont pas d'équivalent par projet (§7.4).

**Rotation** — un fichier unique `/etc/logrotate.d/ogs`, régénéré à chaque apply, listant explicitement les
chemins de tous les projets. Pas de glob sur la racine : l'énumération suit l'inventaire, comme les prisons
fail2ban, et un fichier n'y figure que si son projet existe.

```
# Généré par ogs — NE PAS MODIFIER À LA MAIN
# 12 projets  empreinte: sha256:…  généré: 2026-08-23T03:00:00Z

/var/log/ogs-projects/shopfr/apache-access.log
/var/log/ogs-projects/shopfr/apache-error.log
/var/log/ogs-projects/shopfr/php-error.log
/var/log/ogs-projects/shopfr/php-slow.log
/var/log/ogs-projects/autresitefr/apache-access.log
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

/var/log/ogs-projects/shopfr/cron-*.log
/var/log/ogs-projects/autresitefr/cron-*.log
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
- **Aucune directive `su`, et il ne faut jamais en ajouter.** Le répertoire n'étant inscriptible par aucun
  groupe non root, il passe le contrôle de sécurité que logrotate applique au répertoire parent ; le rendre
  inscriptible au projet — pour laisser une application y créer ses fichiers — ferait refuser la rotation,
  et rouvrirait la substitution que D71 ferme.
- **Jamais un chemin passant par le home**, ni un `olddir` dans le home : logrotate suivrait le lien `~/logs`
  en root, et l'utilisateur choisirait ce que root renomme et recrée.
- **Réouverture** : PHP rouvre `php-error.log` à chaque écriture et la tâche son `cron-<nom>.log` à chaque
  exécution ; `php-slow.log` et les journaux web le sont par les rechargements du `postrotate`. Les archives
  compressées gardent le mode du fichier roté, donc restent lisibles au projet.
- **Espace disque** : les journaux vivent sur `/var`, qui porte aussi les bases. `logs.root` permet de les
  poser sur une partition dédiée, et `doctor` avertit quand l'espace libre de la sienne baisse.
- Rétention configurable globalement (`logs.root`, `logs.rotate`, `logs.maxSize`, `logs.compress`) et surchargeable par
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
| Retrait du fichier d'un autre | Un fichier n'est à un projet que par son en-tête, jamais par son nom ni par un préfixe (D76) ; tout retrait passe par les cinq états — un fichier sans la marque ou marqué pour un autre projet ne part jamais, `--force` compris —, et un projet n'écrit jamais le fichier d'un autre. Une sauvegarde ne réutilise jamais un nom (`O_EXCL`). |
| Fuite de secrets | Masquage systématique dans les logs, l'audit et les messages d'erreur (filtre appliqué à la sortie, pas seulement au site d'appel) ; `secrets show` seul chemin de divulgation, audité. |
| Permissions trop larges | Modes explicites à chaque écriture (jamais dépendants du umask) ; `doctor` vérifie et propose la correction. |
| Escalade via hooks | Hooks refusés s'ils ne sont pas possédés par root et non inscriptibles par autrui ; chemin obligatoirement sous `/etc/ogs/hooks/`. Aucun hook sous `--dry-run`, le `hook.Runner` lui-même s'y refusant (D88). |
| Option ignorée | `--dry-run`, option globale, est simulé ou refusé (code 2) par chaque commande, jamais ignoré ; la conduite de chacune est inscrite dans une table unique, et une commande nouvelle refuse l'option par défaut (D88). |
| Abus de quota ACME | Compteur d'échecs par domaine, pause automatique, mode staging pour les tests. |
| Déni de service par erreur | Validation de configuration avant tout reload ; restauration automatique du fichier précédent en cas d'échec de validation. |
| Suppression accidentelle | Archivage par défaut, saisie de l'identifiant pour `--purge`, que `--yes` ne remplace pas, refus sur ressources non gérées. Tous les refus avant la confirmation, éprouvés de nouveau au point de non-retour : racine partagée ou englobante, point de montage, fichier laissé en place qu'ogs n'a pas écrit, base sans sauvegarde possible ; une interruption avant ce point est un abandon qui relance les services (D77). |
| Rattachement deviné à l'import | Une base n'est rattachée qu'au seul candidat que son nom désigne, aux séparateurs près, et déclarée qu'avec le seul compte de son nom, sous son hôte réel ; sinon elle reste non gérée. Aucun compte SGBD n'est créé à côté d'un homonyme sous un autre hôte (D78). |
| Fuite entre sites | Groupe privé par projet, jamais de groupe partagé entre utilisateurs de sites ; `.env` en 0600, `var/` en 0700 ; `open_basedir` limité au home ; le pool FPM tourne sous l'utilisateur du projet, `www-data` n'exécute aucun code applicatif. |
| Application exposée hors du proxy | Écoute imposée sur socket Unix `0770 <id>:<id>` ou sur `127.0.0.1` exclusivement ; `doctor` signale tout port applicatif en écoute sur `0.0.0.0` (§7.11.3). |
| Fuite de la clé de déploiement | Clé privée en 0600, propriété de l'utilisateur, jamais restituée par une commande ni écrite dans un log ; déploiement en tirage, donc aucune clé à confier à un tiers. |
| Runtime altéré | Les runtimes non-PHP vivant dans le home de l'utilisateur (D35), leur intégrité n'est pas garantie par ogs : `status` compare la version rapportée à celle déclarée et signale l'écart. Aucune escalade possible, le processus tournant déjà sous cette identité. |
| Privilège MySQL trop large | Échappement systématique de `_` en `\_` dans tout motif de `GRANT`, verrouillé par un test unitaire dédié (§3.3.5). |
| Collision prod / préproduction | Suffixe `prep` préservé lors de toute troncature ; unicité vérifiée sur l'identifiant, le host effectif **et** le home. |
| Escalade root par lien symbolique de log | Les journaux vivent hors du home, dans une chaîne de répertoires entièrement root, et sont pré-créés (D71) : l'utilisateur ne peut ni substituer un fichier, ni substituer le répertoire. Le lien `~/logs` n'est emprunté par aucun processus root. Aucun processus root n'ouvre en écriture un chemin dont un non-root contrôle un composant (§7.1.2). |
| Élargissement de la délégation sudo | Commandes énumérées sans métacaractère, jamais de `systemctl` nu ; `ogs my` ne prend aucun argument et déduit le projet de `SUDO_UID`, que sudo positionne sous `env_reset` ; validation `visudo -cf` avant activation, revalidation par `doctor` (§7.9.1). |
| Exécution croisée via un pool FPM | Socket Unix `0660 www-data:<id>` par défaut : seuls le serveur web et l'utilisateur du projet peuvent s'y connecter. Le mode TCP, dépourvu d'authentification, exige `--allow-tcp` et un avertissement explicite (§7.5). |
| Détournement de l'interpréteur | Le lien `php` vit dans `~/.local/bin`, au compte de l'utilisateur (D70) : il peut le changer, sans autre effet qu'une exécution sous sa propre identité, et l'apply suivant le rétablit. Root l'écrit sans jamais suivre de lien : un répertoire substitué fait refuser l'étape au lieu de faire écrire root ailleurs. La version d'un runtime se lit en lançant son lien sous l'identité du projet, jamais en root (D82). |
| Parent substitué dans le home | `O_NOFOLLOW` ne protège que la feuille : chaque répertoire du projet est atteint depuis sa racine par descripteurs, composant par composant, à l'observation comme à l'écriture et à la compensation ; un parent lié fait échouer l'étape en le nommant (D83). |
| Lecture ou copie root dans le home hors apply | `user key`, `site clone` et `php switch` n'y passent que par des descripteurs ouverts sans suivre de lien ; ils contrôlent le propriétaire sur le descripteur — un lien physique ne se voit pas comme un lien —, bornent taille, profondeur, données écrites et entrées nommées, citent les noms, et suivent l'annulation. Root ne recopie ni n'affiche pour autrui ce que le compte source ne possède pas (D82). |
| Archive et restauration par le nom | L'archivage et l'extraction parcourent le home par descripteurs, sans jamais traverser un lien ; chaque entrée restaurée reçoit son propriétaire sur son descripteur, et tout ce qui n'était pas à l'utilisateur revient à root (D81). |
| Chemin désigné en ligne de commande | `db dump -O` et `db restore -i` ne suivent un lien que si seul root a pu le poser, et n'admettent qu'un fichier ordinaire ; une sauvegarde est recréée en `O_EXCL`, en 0600 (D83). |
| Lien du docroot servi par `www-data` | Membre de tous les groupes (D25), le serveur web ne suit un lien que vers ce qui appartient à son auteur, contrôle posé dès la racine du projet (D87). Sous Apache, le contrôle précède l'ouverture : course résiduelle pour un compte doté d'un shell (§20). |
| Dépôt exposé par le docroot | Fichiers et répertoires cachés, `composer.json` et `composer.lock` jamais servis, `/.well-known/` excepté ; le refus d'Apache tient sous les surcharges D63 et sous MultiViews (D87). |
| Secrets de la production en préproduction | `site clone` ne recopie pas la configuration connue du type, masque les secrets qu'ogs connaît, dans la copie comme dans la déclaration, et nomme ce qu'il n'a pas pu neutraliser, code 6 (D84). |
| Démon coupé de sa configuration | `ogs harden` ne ferme rien sans le groupe du service qui lit, refuse une cible dont le groupe est introuvable, ne desserre jamais et écrit par descripteur (D85). |
| Coupure de l'accès d'administration | Les gardes du pare-feu jugent ce qui restera ouvert, ports où sshd écoute réellement compris, et ne se lèvent que par `--allow-lockout`, jamais par le `--force` des fichiers ; une première activation laisse un témoin qui survit au redémarrage (D86). |
| Migration entre machines | `docs/migrer.sh` n'écrit jamais en root par un chemin que fournit la source ou le compte du projet ; socket de contrôle ssh dans un répertoire privé de l'exécution ; secrets passés par l'environnement du seul processus, jamais en argument (D90). |

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
7. Cycle complet suppression avec archivage puis `project restore` → projet fonctionnel à l'identique
   (scénario `restauration`, détaillé en 37).
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
    `apache-error.log` par un lien vers `/etc/sudoers.d/x` → refus du système (répertoire root) ; tentative
    de renommer le répertoire des journaux, ou de faire pointer `~/logs` ailleurs, puis rotation et
    redémarrage du serveur web → aucune écriture root hors des fichiers attendus (D71).
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
28. **Application Go sur port** : binaire déposé, `.local/bin/app` mis à jour, redémarrage, réponse via le
    proxy ; puis test négatif — une application écoutant sur `0.0.0.0` est détectée et signalée par
    `doctor`.
29. **Échec d'installation de runtime** (réseau coupé) : le site est créé, le service reste à l'arrêt, code
    de retour 6, et la reprise après rétablissement du réseau aboutit.
30. **Clé de déploiement** : générée sans passphrase, permissions correctes, `user key show` n'expose que la
    partie publique, `rotate` remplace la paire au compte du projet et refuse un `.ssh` lié ; une clé liée
    physiquement à un fichier de root, creuse de 1 Gio ou non imprimable n'est ni affichée ni sauvegardée
    (D82) ; et la clé survit à un cycle archivage/restauration.
31. **Supervision** : le vhost d'état répond sur la boucle locale pour les **deux moteurs** — `stub_status`
    et `server-status` ne s'exposent pas de la même façon, et `SCRIPT_NAME` ne parvient pas à FPM par le
    même chemin —, le statut d'un pool traverse le serveur web jusqu'à FPM, et le vhost refuse tout le
    reste. Le fragment de l'agent est un TOML valide, lisible par lui seul, sans aucun socket FPM et sans
    destination de métriques. Le compte MySQL de supervision lit l'état du serveur et le
    `performance_schema` mais **pas** la base d'un projet ; le rôle PostgreSQL se connecte en « peer »,
    sans mot de passe, avec `pg_monitor`. `fail2ban-client -t` accepte le fichier de prisons. Un journal
    déplacé est nommé par `doctor` **et** refusé par fail2ban — le diagnostic est éprouvé contre le démon
    lui-même. Enfin la suppression d'un projet retire ses entrées de tous les fichiers de parc, et les deux
    configurations restent valides.
32. **Signature DKIM** : un vrai OpenDKIM branché sur un vrai Postfix, et un résolveur local tenant lieu de
    zone publiée. OpenDKIM accepte les tables d'ogs, en-tête compris, et le courrier sortant est signé ;
    l'enregistrement émis par `records` vérifie la signature produite. `doctor` signale un
    `opendkim.conf` qui ignore les tables, puis lit l'enregistrement publié, découpé en plusieurs chaînes.
    Une clé passée en 0644 **retient le courrier en file** — ce que `doctor` dit et que l'apply répare, le
    courrier repartant signé. Un fichier disparu est réécrit à l'identique. La rotation refuse de basculer
    (code 5) tant que la clé préparée n'est pas publiée. Un apply qui ne vise qu'un projet laisse les autres
    dans les tables ; une table retouchée est refusée puis sauvegardée sous `--force` ; un rechargement sur
    une configuration invalide laisse le démon debout ; la préproduction est refusée ; la suppression
    retire les clés, nomme les enregistrements à retirer et laisse le voisin signé. `ogs harden` trouve
    conforme ce qu'ogs a écrit dans `/etc/opendkim`, referme un répertoire et une table desserrés sans
    toucher aux clés, le courrier reste signé, et l'apply suivant ne rouvre rien.
33. **Journaux hors du home** (D71), sous systemd : chaîne `/var/log/ogs-projects` entièrement root,
    fichiers pré-créés au bon propriétaire, lien `~/logs` ; le projet lit ses journaux sans rien y créer et
    ne voit pas ceux de ses voisins ; `php-error.log` reçoit les erreurs PHP, `open_basedir` compris.
    L'attaque qui a motivé D71 — `~/logs` renommé et remplacé par un répertoire de l'utilisateur, où
    `apache-error.log` désigne un fichier système — échoue au redémarrage d'Apache comme à la rotation ;
    logrotate tourne sans `su`, garde les propriétaires, et l'archive reste lisible au projet. La migration
    d'un ancien `~/logs` reprend le contenu, journaux rotés compris, et Apache écrit ensuite au nouvel
    emplacement ; `doctor` signale une racine desserrée, que l'apply referme ; la suppression archive et
    retire les journaux.
34. **Pare-feu** (D72), sous systemd, avec un vrai ufw et des **connexions réelles** : deux espaces réseau
    reliés au conteneur par des paires veth tiennent lieu de clients, l'un de confiance, l'autre étranger,
    et le verdict est ce que le noyau laisse passer. Un témoin sans pare-feu établit d'abord que tout
    passe. ufw accepte le fichier de profils d'ogs, en-tête accentué compris, aux ports détectés — sshd sur
    deux ports, SFTP de proftpd sur 2222 dans son `VirtualHost`, plage passive du FTP. La garde refuse
    (code 3) une session SSH qu'aucune règle ne couvrirait. La première activation se défait d'elle-même
    faute de confirmation ; confirmée, elle tient. Le plan relu est vide ; un commentaire corrigé l'est en
    place, sans règle en double ; un port de profil changé entraîne ses règles, l'ancien port se fermant.
    Un bannissement placé en tête bloque une adresse que le web ouvre à tous ; relu après une
    autorisation, il est inopérant, et le plan le replace en tête. Une règle de l'administrateur survit,
    une règle d'ogs orpheline part ; `doctor` signale un service qui n'écoute que la boucle locale ;
    `active: false` désactive ufw. Les gardes de D86 : `confirm` après le retour automatique dit ufw
    inactif et rend 6 ; un retour perdu — son unité arrêtée comme un redémarrage l'efface — laisse le
    témoin, que `status`, `doctor` et `confirm` lisent ; un `Port` annoté garde son port ; des ports ssh qui
    fermeraient celui où sshd écoute sont refusés, `ogs apply --force` compris, de même qu'un profil ssh
    écarté, sauf si l'administrateur ouvre ssh lui-même, fût-ce par une seule interface — ce qui, pour une
    session, ne suffit pas, et le refus le dit ; sshd déplacé, le plan qui suit son nouveau port passe ;
    deux moteurs sans `web.engine`, bannir ne retire aucun accès au web.
35. **Appartenance par l'en-tête** (D76, scénario `nginx`) : un vhost posé à la main selon la convention de
    nommage du projet — `statictest_maintenance` — et son lien restent en place après un apply du projet
    qui aboutit.
36. **Remparts de la suppression** (D77, scénario `suppression`) : un partage monté sous le home et un
    fichier cron écrit à la main au nom du projet sont nommés ensemble dans un seul refus, sous `--force`,
    avant tout retrait ; le contenu du partage reste intact et le pool tourne toujours.
37. **Restauration** (D81, scénario `restauration`, sous systemd) : un home que l'utilisateur a modifié —
    `authorized_keys` régulier, script à la place du lien `php`, répertoire à la place de `~/logs`, lien dur
    vers un fichier du système — est restauré sans qu'aucun lien posé par l'apply de la restauration ne
    soit traversé : trousseau commun, interpréteur, fichier système et répertoire des journaux intacts,
    home rendu à l'utilisateur, lien dur à root, apply suivant en code 6. L'aller-retour ordinaire rétablit
    les liens gérés, le site est servi, apply et diff sont vides. L'ancien `~/logs` d'un projet antérieur à
    D71 revient à root, puis l'apply suivant le migre. `project restore --dry-run` rend 2 sans rien créer.
38. **Émission et repli** (D79, D80, scénario `pebble`) : un alias dont l'émission échoue ne retire pas le
    HTTPS du site ; un alias qui résout, à côté d'une redirection refusée, est couvert sans attendre et
    présenté sans rechargement manuel ; une redirection refusée ne coûte pas le renouvellement du site ;
    une redirection depuis une adresse IP ne bloque ni l'apply ni le renouvellement, son vhost restant en
    HTTP ; un renouvellement partiel recharge quand même le serveur, qui présente le nouveau numéro de
    série.
39. **Root dans le home hors apply** (D82, scénario `site-complet`) : la rotation de la clé pose la paire
    au compte du projet et refuse un `.ssh` lié ; `site clone` écarte et nomme un lien physique vers un
    fichier de root, repose les liens physiques internes, garde creux un fichier creux, borne la profondeur
    à 1024 niveaux en citant un nom forgé ; un SIGTERM pendant la copie rend 9 aussitôt, sans répertoire
    laissé à root.
40. **Clone sous garde** (D84, scénario `mysql`) : le mot de passe de la production, écrit dans la
    configuration comme le guide l'indique, est masqué dans la copie, ce qui reste ne s'authentifie plus, et
    la préproduction travaille sur sa propre base ; la sortie nomme le reste de `www/` et la base copiée.
41. **Hôte réel d'un compte adopté** (D78, scénario `mysql`) : à côté d'un `'vitrinefr'@'%'` créé à la
    main, aucun `'vitrinefr'@'localhost'` n'est créé et le refus est annoncé ; déclaré `host: '%'`, le compte
    est reconnu, son mot de passe fonctionne toujours et le diff suivant est vide.
42. **Durcissement** (D85, scénario `harden`) : après `harden --apply`, MariaDB lit toujours son
    `bind-address` sous l'identité `mysql`, `www-data` lit un fichier d'authentification rangé sous
    `/etc/nginx`, `/etc/postgresql` reste à postgres et le cluster redémarre.
43. **Docroots** (D87, scénarios `nginx` et `site-complet`) : `.git`, `.env` et `composer.json` répondent
    403, `/.well-known/` reste servi ; un lien vers le fichier d'un autre projet n'est pas suivi — le témoin
    établit que `www-data` le lit par son groupe —, un lien vers ses propres fichiers l'est, un docroot
    remplacé par un lien ne sert rien. Sous nginx, l'accueil reste servi sous `disable_symlinks` pour toute
    méthode, un répertoire sans barre finale est redirigé, et les surcharges du docroot valent pour « / » ;
    sous Apache, le refus tient sous le `<Location />` d'une surcharge et sous MultiViews, éprouvé dans un
    sous-répertoire que `www-data` peut lister.
44. **Répertoires et chemins désignés** (D83, scénarios `systemd` et `postgres`) : un `~/.local` remplacé
    par un lien vers `/usr/local` entre le plan et son exécution — par un hook `preApply` — fait échouer
    l'apply, `/usr/local/bin` intact ; `db dump -O` et `db restore -i` à travers un lien posé par le projet
    sont refusés, la cible du lien intacte, alors qu'un lien que seul root a pu poser (`/var/run`) reste
    suivi.
45. **`--dry-run` refusé** (D88, scénarios `hooks` et `restauration`) : `project delete --dry-run` rend 2,
    le site reste servi et aucun hook n'est lancé ; `project restore --dry-run` rend 2, sans déclaration,
    compte ni home.
46. **Paquet** (D89, scénario `paquet`) : le paquet construit est réétiqueté trois fois, dont une « 0.0.3 »
    qui porte le prerm d'origine et pas de postrm. Installation de 0.0.3, mise à jour depuis elle, puis entre
    deux versions corrigées : le timer reste actif, et aucun script ne signale quoi que ce soit (`ogs : …`)
    ni ne laisse de requête au terminal. Le retrait arrête le timer et garde le paquet en « rc », la
    réinstallation le rend actif, la purge retire les unités et dit ce qu'elle garde. Les scripts sont
    lancés par `apt-get -o DPkg::Path="$PATH"` : apt 3 fixe leur `PATH` sans `/usr/local`, où vivent la
    trace et le substitut de `systemctl`.

### 16.3 Bancs hors conteneur

Deux bancs éprouvent, dans `make test`, ce qui n'est pas du Go d'ogs :

- **`test/migration`** rejoue `docs/migrer.sh` (D90) contre un ancien serveur simulé : un répertoire local
  joint par un faux ssh qui lit l'entrée standard comme le vrai, des moteurs de base, une crontab,
  `supervisorctl`, `dig`, `curl` et `runuser` simulés ; `rsync`, `install`, `python3` et ogs — construit
  depuis le dépôt, en mode préfixé — sont les vrais, derrière des enveloppes qui notent chaque appel. Le
  banc ne tourne pas en root : ce qu'il vérifie, c'est l'identité que le script demande pour chaque
  écriture. Il saute ses tests si un outil manque (`python3-yaml`, `rsync`, `flock`…).
- **`packaging/scripts_test.go`** exécute les scripts de maintenance tels que dpkg les enchaîne — retrait,
  mise à jour depuis le prerm des versions jusqu'à 0.0.3, réinstallation, purge — dans un bac à sable où
  `systemctl` et `ogs` sont des doublures qui notent leurs appels et tiennent l'état d'activation des
  unités ; le vrai `dpkg` sert aux comparaisons de versions, et son absence fait sauter le banc. Il vérifie
  aussi que les dérogations lintian suivent les numéros de ligne des scripts.

Un test parcourt aussi l'arbre des commandes : `TestDryRunChaqueCommandeSimuleOuRefuse` relève le préfixe
et les tables de la base d'état avant et après chaque commande sous `--dry-run` (D88), l'aide et la
complétion que cobra ajoute à l'exécution comprises : une commande inscrite comme simulation ou
consultation doit avoir un cas qui l'éprouve, toute autre doit refuser l'option sans rien toucher.

### 16.4 Intégration continue

GitHub Actions : `go vet`, `staticcheck`, `golangci-lint`, `gofumpt` en vérification, tests unitaires avec
détecteur de concurrence, tests d'intégration par matrice (2 distributions × 2 serveurs web), `govulncheck`,
build multi-architecture. Les tests d'intégration sont obligatoires avant fusion sur la branche principale.

---

## 17. Packaging et distribution

- Binaire unique `ogs`, `CGO_ENABLED=0`, `-ldflags "-s -w -X …/version.Version=…"`, cibles
  `linux/amd64` et `linux/arm64`.
- GoReleaser : archives `.tar.gz`, paquet `.deb` (nfpm) et `.rpm` (préparé, non testé en v1), sommes de
  contrôle signées.
- Contenu du `.deb` : `/usr/bin/ogs`, complétions bash/zsh/fish, pages de manuel générées par Cobra,
  `changelog.gz`, `README`, le cahier des charges et les dérogations lintian ; les répertoires `/etc/ogs`
  (0700), `/etc/ogs/hooks` (0700), `/var/lib/ogs` (0751) et `/var/log/ogs` (0700). Il ne livre **ni** les
  unités `ogs-renew`, **ni** `tmpfiles.d/ogs.conf`, **ni** `logrotate.d/ogs` : c'est le plan global
  d'`ogs apply` qui les écrit sur la machine administrée, d'après son parc.
- Scripts de maintenance (D89) :
  - **postinst** : sans configuration, il annonce `ogs init` et n'engendre jamais la clé maître à l'insu de
    l'administrateur. À une mise à jour ou une réinstallation, il lance `ogs init --quiet >/dev/null`,
    idempotent, qui crée les répertoires nouveaux et migre la base d'état — un échec est affiché, jamais
    fatal —, sauf si la clé maître a disparu : il le dit alors, avec les deux remèdes, au lieu d'en laisser
    engendrer une neuve. Enfin il rend au timer de renouvellement l'état que le paquet lui a pris, et
    signale sur stderr un timer d'ogs laissé désactivé ;
  - **prerm** : au seul `remove`, il note l'état du timer puis l'arrête et le désactive ; `upgrade`,
    `failed-upgrade` et `deconfigure` n'y touchent pas ;
  - **postrm** : il n'agit qu'à la purge — retire les deux unités d'ogs, reconnues à leur en-tête, et le
    témoin du prerm ; garde `/etc/ogs`, `/var/lib/ogs` (état, comptes ACME, certificats que les vhosts
    servent encore, archives, dumps, sauvegardes de `--force`) et `/var/log/ogs`, et le dit ; énumère les
    archives, que plus rien ne purge. Sa seule présence compte : sans postrm ni fichier de configuration,
    dpkg oubliait le paquet dès le retrait, et `apt purge` n'avait plus rien à faire.
- Un problème que signale un script — migration en échec, clé maître absente, timer non activé ou dont
  l'état n'a pas pu être noté — l'est sur stderr, préfixé « `ogs : ` » : c'est ce que le scénario `paquet`
  surveille à l'installation, à la mise à jour et au retrait (§16.2, 46).
- Diffusion : poser une étiquette au-delà de 0.0.3 avant toute diffusion des scripts de D89. `make package`
  numérote d'après la dernière étiquette, et une construction corrigée encore numérotée 0.0.3 ferait
  réactiver, à la mise à jour suivante, un timer que l'administrateur avait désactivé — apt tiendrait
  d'ailleurs pour déjà installé un paquet de même numéro.
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
| **L1 — Web + PHP** | provider `system` (users, groupes, `logs/`, lien d'interpréteur, clé de déploiement), `php` (détection 7.4→8.5, pools, presets, numérotation, switch), types d'application Prestashop et Symfony 3.4→8 avec détection de variante et garde-fou de compatibilité, `web` (interface, Apache, Nginx), rotation des logs, reloads groupés, validation avant activation | Un Prestashop en 7.4 et un Symfony 8 en 8.5 servis simultanément, `php switch` sans coupure ni réécriture d'unité, bascule incompatible refusée |
| **L2 — Bases** | providers `mysql` et `postgres`, privilèges nommés, dump/restore, `secrets env` | Création complète, rotation de mot de passe, connexion applicative vérifiée |
| **L3 — TLS** | provider `acme` (lego), HTTP-01, DNS-01, timer de renouvellement, hooks de reload, staging, auto-signé | Émission et renouvellement réels contre pebble, échec non bloquant vérifié |
| **L4 — RabbitMQ** | provider `amqp` (API HTTP + fallback), exchanges/queues/bindings/policies, préréglage Messenger, détection d'arguments immuables | Transport Messenger fonctionnel de bout en bout |
| **L5 — Automatisation** | provider `cron`, provider `unit` (templates, instances, target, scale, logs), délégation sudoers et `ogs my`, tmpfiles | 3 workers Messenger supervisés, scale à chaud, cron exécuté sous le bon utilisateur |
| **L6 — Composition** | `site create` (y compris `--env prep`) et sa saga complète, `site clone`, `project delete` avec archivage, `project restore`, `status` | Scénarios d'intégration 1, 7 et 13 validés |
| **L7 — TUI** | inspecteur, panneau de détail, actions contextuelles, suivi de logs, wizard de création | Création complète d'un projet depuis la TUI, parité avec la CLI |
| **L8 — Import** | scan multi-sources, corrélation, adoption interactive, ressources non gérées | Adoption d'un serveur préconfiguré sans régression |
| **L9 — Applications non-PHP** | types d'application, providers `runtime` (nvm, rbenv, pyenv, venv), templates de service Go/Node/Python/Ruby, vhosts de proxy, attente de disponibilité | Scénarios d'intégration 22 à 25 validés |
| **L10 — Industrialisation** | hooks de parc et de projet, complétions dynamiques bash/zsh/fish, 96 pages de manuel engendrées, paquet `.deb` (nfpm), CI GitHub Actions, vingt scénarios d'intégration conteneurisés (Apache, PHP, nginx, hooks, MariaDB, PostgreSQL, RabbitMQ, ACME, systemd et cron, runtimes, durcissement, paquet, suppression, moteur web durci, supervision, signature DKIM, journaux, pare-feu, FPM sans pool, restauration), plus un scénario optionnel pour les runtimes qui compilent, documentation d'exploitation | Paquet installable, documentation publiée |

### Ce que les essais sur système réel ont corrigé

Les scénarios conteneurisés ont mis au jour trente-cinq défauts qu'aucun test unitaire ne
pouvait voir, parce que tous supposaient un interlocuteur simulé :

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
| Rôle d'administration PostgreSQL fixé à « postgres » | l'authentification « peer » le refusait toujours à root : **le provider PostgreSQL ne pouvait fonctionner sur aucune installation par défaut** (D44) |
| `CONNECT` laissé à PUBLIC sur les bases PostgreSQL | le rôle d'un projet **se connectait à la base d'un autre** (D45) |
| `CREATE DATABASE` depuis `template1` | échec sur tout cluster dont la base modèle n'est pas en UTF8 (D46) |
| Comptes additionnels PostgreSQL observés mais jamais créés | le plan les redemandait indéfiniment ; `ogs db user add` restait sans effet sur une base PostgreSQL |
| `/etc/ogs` créé en 0755 | les déclarations de projets — hôtes, utilisateurs, noms de bases — **dressaient la carte du parc pour tout utilisateur local**. Le répertoire, ses projets et ses hooks sont désormais en 0700 |
| Comptes additionnels non supprimés avec le projet, sur les deux moteurs | un compte **gardait la faculté de s'authentifier**, avec un mot de passe que le keystore ne conservait plus |
| Étapes d'unité triées par nom | les instances démarraient avant l'écriture de leur modèle : **aucun service ne démarrait au premier apply** (D47) |
| Cible du projet non liée à `multi-user.target` | **les services ne repartaient pas après un redémarrage** de la machine (D48) |
| `StartLimitBurst` et `StartLimitIntervalSec` placés dans `[Service]` | systemd les ignorait : la protection contre un service qui échoue en boucle **n'existait pas** |
| `ReadWritePaths` sur un `var/` inexistant | tout service déclaré avant le déploiement du code échouait en 226/NAMESPACE (D49) |
| Rechargements du plan global jamais exécutés | `systemd-tmpfiles` ne créait pas les répertoires d'exécution — **le verrou de toute tâche planifiée échouait** — et **le timer de renouvellement des certificats n'était jamais activé** |
| Intentions de rechargement calculées d'avance | un vidage anticipé consommait celle d'une étape à venir : la cible ne réclamait pas ses instances |
| `AsUser` recollait l'argv sans citation | tout argument contenant une espace était redécoupé par le shell : `ogs cron run` était inutilisable |
| `ogs cron run` sans répertoire de travail | la commande d'essai ne reproduisait pas ce que cron exécute, dans un sens comme dans l'autre |
| Unité applicative écrite mais jamais activée ni démarrée | **tout site non-PHP était mort-né** : l'attente de disponibilité échouait ensuite en accusant l'application, alors que rien n'avait été lancé |
| Attente de disponibilité placée avant le démarrage | elle ne pouvait qu'échouer ; les services et l'application démarrent désormais avant le serveur web, dont un vhost de proxy exposerait sinon une 502 |
| Modules Apache requis non contrôlés | `mod_proxy_http` absent donnait une 500 et un message d'Apache incompréhensible ; `doctor` énumère maintenant les six modules dont les vhosts dépendent |
| Mode de `logs/` imposé par deux providers | le plan oscillait entre deux valeurs, donc restait éternellement en écart |
| `$connection_upgrade` employé par les vhosts de proxy mais défini nulle part | **toute application Go, Node, Ruby ou Python rendait la configuration nginx entière invalide** : au rechargement suivant, tous les sites de la machine tombaient (D50) |
| Suppression d'un projet sans rechargement des serveurs | le maître PHP-FPM gardait le pool en mémoire : **ses workers continuaient de tourner et le site restait servi** |
| Intention de rechargement « web », sans moteur, émise à l'ajout de `www-data` au groupe d'un projet | routée nulle part depuis le lot L0 : le rechargement n'avait pas lieu, et **toute création de projet se soldait par le code 6**, sur un avertissement que rien n'affichait — les rechargements en échec n'étaient que comptés |
| Journaux dans le home, sous un répertoire root | l'utilisateur **renommait** le répertoire sans privilège et en recréait un à lui : un journal devenu lien vers un fichier système était ouvert en root par le serveur web au redémarrage suivant (D71) |
| `php-error.log` jamais pré-créé | les workers, sous l'identité du projet, ne pouvaient pas le créer dans un répertoire root : **aucune erreur PHP n'y arrivait**, toutes partant dans le journal global de FPM, mêlées à celles des autres projets |
| logrotate absent de l'image d'intégration | la rotation n'avait **jamais été éprouvée** sur un système réel, alors que le §16.2 l'exige |
| `systemd-tmpfiles --create` lancé sans argument | il traitait toute la configuration de la machine et rendait 65 sur une ligne en double d'un fichier de la distribution : un avertissement à chaque apply, longtemps compté sans être affiché |
| Migration des journaux sans rechargement quand aucun vhost ne changeait | Apache continuait d'écrire, par son descripteur, dans l'ancien fichier que la migration retirait |
| Clé DKIM retirée avant le rechargement des tables qui la désignaient | OpenDKIM, qui garde ses tables en mémoire, **retenait en file le courrier du domaine** jusqu'au rechargement : une clé désignée mais illisible ne laisse pas partir le courrier sans signature (D69) |

Les lots L0 à L2 constituent le socle minimal utilisable ; L3 à L6 apportent la valeur cible ; L7 à L10
finalisent le produit. L9 (applications non-PHP) est le seul lot réellement détachable : le produit est
complet sans lui pour un parc exclusivement PHP.

### Ce que la revue de code du 25/09/2026 a corrigé

Une revue fonctionnelle puis de sécurité, chaque constat soumis à une réfutation et à une reproduction
avant d'être retenu, a donné lieu à quinze lots de correction, chacun relu par un vérificateur
indépendant avant d'être intégré, l'ensemble éprouvé par les vingt scénarios. La plupart des constats critiques
avaient un point commun : **un nom, un préfixe ou un chemin pris pour une preuve** — d'appartenance, de
propriété, de ce qu'un lien désigne.

| Lot | Défaut principal | Conséquence en exploitation | Décision |
|---|---|---|---|
| A | fichiers obsolètes attribués par le préfixe `<id>_` ; retraits hors des cinq états | `apply shop` retirait le pool et le vhost de `shop_api`, en code 0 et sans copie | D76 |
| B | refus de la suppression prononcés après le point de non-retour, un à la fois ; `os.RemoveAll` à travers les montages | home d'un voisin effacé, partage NFS vidé, `DROP DATABASE` sans sauvegarde, services laissés arrêtés | D77 |
| C | bases et comptes rattachés par préfixe à l'import ; hôte du compte perdu | la base de `shopping.fr` accordée puis supprimée avec `shop.fr` ; mot de passe de l'application refusé | D78 |
| D | vhost rendu d'après la demande de certificat ; redirections hors du certificat | un alias ajouté faisait perdre le HTTPS du site ; domaine redirigé servi avec un certificat qui ne le couvre pas | D79, D80 |
| E | archive et restauration par le nom | trousseau commun et binaire PHP réécrits en root, `master.key` archivable par l'utilisateur | D81 |
| F | clé de déploiement, clone et `php switch` en root par le nom, hors de tout plan | paires créées dans `/etc/apt/apt.conf.d`, `www` lié recopié et donné à la préproduction | D82 |
| G | `harden` fermait des répertoires sans le groupe de leur lecteur | MariaDB repartait sur ses défauts, à l'écoute de toutes les interfaces ; sites protégés en 500 | D85 |
| H | port de sshd fermé sans garde ; `--force` des fichiers levant les gardes du pare-feu | une commande sans rapport coupait l'accès d'administration | D86 |
| I, I2 | boucles, rejeux, gardes d'appartenance, socket et secrets de `docs/migrer.sh` | seul le premier projet migré ; base de production supprimée par une resynchronisation | D90 |
| J | prerm désactivant le timer à chaque mise à jour du paquet | plus aucun certificat renouvelé après la première mise à jour, sans un mot | D89 |
| K | `--dry-run` ignoré par les commandes qui ne le lisaient pas | `project delete --dry-run` supprimait le projet, hooks compris, en code 0 | D88 |
| L | liens du docroot suivis sous `www-data` ; fichiers cachés servis | un projet servait les fichiers d'un voisin ; `.git` et `.env` publics | D87 |
| M | parents des répertoires du projet résolus par leur nom ; chemins de `db dump -O` / `restore -i` | `/usr/local/bin` donné au projet ; écriture root à travers un lien d'un home | D83 |
| N | `site clone` recopiait la configuration et les secrets de la production | la préproduction écrivait dans la base et la file de la production | D84 |

Les lots ont porté sur les constats critiques et sur les majeurs de même cause : la plupart des constats
majeurs et mineurs relevés par la revue restent à traiter, et le rapport de revue en tient la liste. Le §20
ne porte que les points que les correctifs ont laissés ouverts en connaissance de cause, et les majeurs de
sécurité non traités.

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
   configuration, les dumps de l'étape 4 du §13 disparaissent et `--no-dump` devient le comportement
   par défaut.
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

**Points arbitrés le 24/08/2026** :

- **Fournisseur DNS** : OVH, et lui seul (D53). Aucun autre n'est géré ; en ajouter un demandera de le
  configurer et de l'éprouver, non de le déclarer.
- **Rétention des archives** : 14 jours (D52).
- **Accès SFTP** : écarté, et non rouvert.
- **Versions de runtime** : aucun refus d'une version en fin de support (D54).
- **Mise à jour des runtimes** : projet par projet, sans commande de parc (D55).
- **Droits de l'arborescence** : home en 0711, répertoires en 0710 (D51), pour ôter tout moyen de se
  promener dans le home.

**Points arbitrés le 23/09/2026** (D69) :

- **Signataire DKIM** : OpenDKIM, et lui seul — ses tables sont des fichiers plats, recalculables comme
  logrotate ou fail2ban ; rspamd n'est pas géré.
- **Signature DKIM** : opt-in par projet, jamais par défaut.
- **Publication DNS de DKIM** : jamais par ogs, même quand la zone est chez OVH — D27 reste entier.

**Points arbitrés le 23/09/2026** (D71) :

- **Racine des journaux** (D71, implémentée) : `/var/log/ogs-projects`, distincte de `/var/log/ogs` — qu'il aurait sinon
  fallu rendre traversable alors qu'il porte le journal d'audit d'ogs.
- **Espace disque** : racine réglable par `logs.root`, dont `doctor` contrôle la chaîne de propriété et
  l'espace libre.
- **Historique à la migration** : **déplacé** dans le nouveau répertoire, fichiers rotés compris, pour que
  logrotate poursuive la numérotation (D71). ogs n'étant pas encore réellement en production, la migration
  n'a pas à ménager un parc existant, mais elle reste sans perte de lignes.

**Points arbitrés le 24/09/2026** (D72) :

- **Règles sans la marque d'ogs** : gardées et signalées, jamais purgées.
- **Adresses de confiance et fail2ban** : reprises dans l'`ignoreip` de chaque prison `ogs-`.
- **Retour automatique** : à la première activation seulement.
- **Confiance** : une règle par profil disponible, non un `allow from` global.
- **SFTP** : un profil distinct, servi par proftpd (mod_sftp), sur le port de son contexte
  `SFTPEngine on` — 2222 d'ordinaire. ogs ouvre le port, il ne gère pas l'accès SFTP.
- **Commentaires** : déclarés ou engendrés d'après la source nommée et son propriétaire.

**Points restant à arbitrer** :

- **Mode `webAccess: acl` par défaut ?** Le mode `group` est retenu ; si le serveur héberge des clients
  tiers non fiables, basculer le défaut sur `acl` restreindrait `www-data` au seul docroot. La question
  perd de son acuité depuis D51 : le groupe n'obtient plus qu'un droit de passage.
- **Redis / Memcached / Elasticsearch** : hors périmètre, candidats naturels à un futur système de
  plugins (D19 n'ouvre pour l'instant que templates et hooks).
- **Seuil d'archivage** (défaut 5 Gio) : au-delà, l'archivage est refusé faute de place ; faut-il le
  rendre configurable ?
- **`ogs db restore` sur une base non vide** : la sauvegarde étant un script SQL, la restauration échoue
  sur le premier objet existant. Faut-il une option `--clean` — donc destructive — ou l'exigence d'une
  base vide reste-t-elle la règle ?

**Points ouverts après la revue du 25/09/2026** — à arbitrer :

- **Adoption d'une base isolée** (D91). Seul `ogs import` confie à ogs une base ou un compte SGBD
  préexistant, et il adopte le projet entier. Un projet créé par ogs qui doit reprendre la base d'une
  application existante n'a donc d'autre issue que de la renommer ou de la recréer. Un champ
  `spec.databases[].adopted` ouvrirait l'adoption base par base — la suppression l'exigerait alors sous
  `--force`, comme D77 pour un projet adopté.
- **Comptes repris avant D91** (D91). Un homonyme repris en silence avant D91 porte un mot de passe scellé
  et passe pour être à ogs : `project delete` le supprime. Tenter la connexion avec ce mot de passe avant de
  le retirer le démasquerait sous MySQL, mais pas sous PostgreSQL, où l'authentification locale est `peer`.

- **Course de `SymLinksIfOwnerMatch` sous Apache** (D87, rattachée à D25). Apache contrôle le nom du lien
  avant d'ouvrir la cible : un lien permuté en boucle par un compte doté d'un shell (D62) a passé 126 fois
  sur 27 364 requêtes en dix secondes. nginx, qui compare sur le descripteur ouvert, n'y est pas sujet. La
  fermer demande de servir chaque projet sous une autre identité que `www-data` — une identité de service
  par projet, qui révise D25.
- **Un redémarrage avant confirmation d'ufw vaut-il retour ?** (D86, constat R2-13 traité en partie). Le
  témoin `/var/lib/ogs/ufw-unconfirmed` rend l'état visible, mais ne défait rien : l'unité transitoire du
  retour vit sous `/run`, et ufw repart actif sans filet. Piste : un complément d'unité
  `/etc/systemd/system/ufw.service.d/ogs-rollback.conf`
  (`ExecStartPre=-/bin/sed -i s/^ENABLED=.*/ENABLED=no/ /etc/ufw/ufw.conf`), posé avec le témoin et retiré
  avec lui, à éprouver par un redémarrage dans le délai. Écartés : `ENABLED=no` aussitôt — `ufw status`
  dirait inactif, et le plan réactiverait — et `systemctl disable ufw` — un `ufw enable` manuel laisserait
  ensuite le pare-feu éteint à chaque démarrage.
- **`CONNECT` révoqué à `PUBLIC` sur une base PostgreSQL adoptée** (D45, D78, §7.3). Quand le rôle déclaré
  ne possède pas la base, l'apply révoque `CONNECT` et les droits du schéma `public` à `PUBLIC` et donne le
  schéma au rôle : c'est la normalisation du §9, montrée au premier `diff`, mais elle coupe tout autre rôle
  qui s'y connectait. La garder, la réserver aux bases créées par ogs, ou l'exiger explicitement ?
- **Numéros attribués par le calcul du plan** (D88). `plan`, `diff`, `status`, l'aperçu de la TUI et
  `apply --dry-run` inscrivent dans la base d'état le numéro de pool ou le port applicatif d'un projet jamais
  appliqué — celui que l'apply lui donnera. Corriger demande de lire sans inscrire au calcul, d'inscrire
  dans l'étape sous le verrou, et de tenir en mémoire les réservations d'une exécution.
- **Transition entre deux certificats** (D79). Une préproduction servie par le wildcard qui reçoit un alias
  hors du wildcard bascule sur son propre certificat, et son vhost passe en HTTP seul le temps de
  l'émission. La corriger demande de distinguer la référence présentée de la référence demandée, ce qui
  touche la protection de `cert delete` pour le certificat partagé.
- **Points d'entrée PHP de Prestashop sous nginx** (D87). `rewrite ^/api/` envoie l'API webservice vers
  `/webservice/dispatcher.php`, que `location ~ \.php$ { return 404; }` refuse : `GET /api/…` rend 404, comme
  tout point d'entrée autre que `index.php`. Antérieur à la revue ; choix assumé ou défaut ?
- **Une commande qui rende un projet normalisé sans le déposer** (D90). `docs/migrer.sh` réplique en
  Python `naming.EffectiveHost`, fidèlement aujourd'hui, mais cette copie ne suivra pas une évolution du
  nommage ; un `ogs project export -f <fichier>` la supprimerait.
- **Jeu borné du compte principal PostgreSQL** (D92). Il est refusé plutôt qu'appliqué. L'appliquer
  demande un rôle propriétaire distinct (`NOLOGIN`) et un compte applicatif borné — mais alors plus aucun
  compte de connexion ne crée de tables, et les migrations exigent un compte dédié qui soit, lui,
  propriétaire des objets. À trancher avec un modèle « compte de migration / compte d'exécution ».
- **Restauration MySQL sous GTID** (D92). Un `mysqldump` de MySQL à GTID actif écrit
  `SET @@GLOBAL.GTID_PURGED` et `SET @@SESSION.SQL_LOG_BIN`, qu'un compte de projet ne peut pas exécuter.
  `--set-gtid-purged=OFF` n'existe pas chez MariaDB ; non traité.
- **Mot de passe PostgreSQL en md5** (D92). Un cluster mis à niveau garde les hachages md5 ;
  `PGREQUIREAUTH=scram-sha-256` fait alors refuser la restauration, qui renvoie vers
  `ogs db user rotate` — lequel repose le mot de passe selon `password_encryption`, `scram-sha-256` par
  défaut depuis PostgreSQL 14.
- **Libpq antérieure à 16** (D92). `PGREQUIREAUTH` y est ignorée : Debian 12 livre PostgreSQL 15, et la
  restauration n'y est alors protégée que par le contrôle du socket et de l'écoute TCP. Debian 13 (17) et
  l'image d'intégration l'appliquent.
- **Plan global exécuté sans être affiché** (constat R2-12). `ogs apply`, `site create`, `site clone` et
  `project delete` exécutent le plan global sans le montrer ni le confirmer — activation d'ufw, retraits
  d'accès, purge des archives (D52) —, et `apply --dry-run` l'omet de l'affichage, sans l'exécuter.

Limites connues, laissées en l'état :

- **Suppression** (D77) : passé le point de non-retour, seul SIGKILL arrête le processus ; une
  interruption pendant l'archivage d'une arborescence attend la fin de celle-ci, `archive.AddTree` ne
  consultant pas le contexte ; sous `--keep-data`, un partage monté sous le home écarte de l'archive toute
  l'arborescence, et non le seul sous-arbre monté ; le compte « N ressource(s) » compte deux fois les
  fichiers générés. Le **certificat du projet**, clé privée comprise, n'est ni archivé, ni retiré, ni
  révoqué : `doctor` en signale l'échéance puis l'expiration sans fin, jusqu'à un `ogs cert delete <id>` à la
  main (constat R1-22). Le **fichier d'environnement** de D75 part même sous `--keep-data`, et sans aucune
  copie sous `--purge` (constat R1-76). Le **cron et le pool** restent actifs pendant l'archivage — ils ne
  partent qu'après elle, avec les fichiers générés et le rechargement de FPM —, si bien qu'une écriture
  postérieure au dump manque à l'archive (constat R1-17). L'estimation de la taille de l'archive parcourt
  encore le home par ses noms (`filepath.Walk`), et non par descripteurs comme l'archive elle-même (D81) :
  elle ne lit aucun contenu, mais un répertoire remplacé par un lien pendant le parcours en fausse le total.
- **Compensation d'un apply** : « rollback effectué : le système est revenu à son état initial »
  s'affiche même quand la saga n'a pas tout défait — une base préexistante n'est jamais compensée, si bien
  qu'un `GRANT` ou un compte que l'étape venait d'y poser reste en place. Le message promet plus que la
  compensation.
- **Première émission d'un certificat** (D80) : sans certificat en place, les paliers de repli se réduisent
  aux noms essentiels, alias compris ; un alias dont le DNS ne pointe pas encore ici empêche donc tout
  certificat, et le site reste en HTTP seul jusqu'à la bascule de l'alias ou à son retrait.
- **Clone** (D82, D84) : SIGHUP n'est pas capté — une session ssh coupée pendant un long clone l'arrête net
  et laisse à root, en 0700, les répertoires en cours de copie : le lancer sous tmux ou screen ;
  `--with-data` recopie la base telle quelle — `ps_configuration` et `ps_shop_url` pour Prestashop —, ce
  qui est annoncé mais non réécrit, et un échec de cette copie ne compte pas dans le code 6 ; les chemins
  de la source dans les fichiers de `www/` ne sont pas réécrits ; l'analyse des liens est lexicale, et toute
  cible absolue, `/usr/share/javascript` compris, est nommée et rend 6 ; `site clone` n'éprouve pas
  l'unicité du parc entier. Une entrée de la source retirée entre l'énumération de son répertoire et son
  ouverture (`ENOENT` à `openat`) **arrête toute la copie**, là où le même cas relevé par `fstatat` est
  sauté : le clone d'un site vivant, dont l'application crée et retire des fichiers, peut échouer sans
  cause durable — le relancer. `--dry-run` ne prédit ni les liens qui sortent de `www/` — il annonce
  seulement que le code 6 viendra « si la copie rencontre » l'un d'eux — ni l'installeur resté exposé
  (`AppType.Installer`), que seule la copie constate.
- **Liens posés par root dans le home** : `fs.SymlinkAt` et la copie du clone posent encore le
  propriétaire d'un lien par `fchownat` sur un nom, là où `fs.Dir` passe par un descripteur `O_PATH` ; la
  fenêtre ne s'exploite que si `fs.protected_hardlinks` est désactivé.
- **Racine d'un projet adopté hors convention** (D83) : ses parents sont encore résolus par leur nom
  (`fs.EnsureDir`), à root sous la racine des homes, mais pas nécessairement ailleurs.
- **Import** (D78) : aucun rattachement par indice vérifiable — `GRANT` de l'utilisateur du pool,
  propriétaire `datdba` —, si bien qu'une base nommée autrement que le site reste non gérée ; les comptes
  additionnels d'une base adoptée sous `'%'` sont créés sous `'%'` ; un compte qui existe déjà sans secret
  scellé peut recevoir, au premier apply qui corrige ses privilèges, un mot de passe tiré que
  `CREATE USER IF NOT EXISTS` n'applique pas — `secrets env` le rend alors faux ; deux candidats de même
  identifiant (vhost doublé en `-le-ssl`) : `import adopt` retient le dernier ; les crons restent rattachés
  par sous-chaîne du compte, notés et jamais déclarés.
- **`db restore`** passe le script au client du super-utilisateur du moteur, sans le confiner à la base
  cible (constat R1-14).
- **Pare-feu** : un bannissement que la garde refuse reste écrit dans `ufw.yaml`, où le prochain apply le
  retrouve (constat R2-14, dont seul le volet `--dry-run` est traité).
- **Durcissement** (D85) : un parc durci par une version antérieure n'est pas rouvert ; `doctor` ne compte
  pas les cibles refusées faute de groupe, et ne relève ni les `AuthUserFile`, `auth_basic_user_file`,
  `RewriteMap` rangés sous `/etc`, ni un cluster PostgreSQL hors du port 5432 ; les répertoires de
  surcharge de D63, en 0750 root:root, ne se lisent pas par les workers — un fichier d'authentification n'y
  a pas sa place ; `hardening.targets` accepte des modes écrits en décimal (constat R2-07) ; les fichiers de
  `pool.d`, en 0644, portent les variables d'environnement des pools, que rien ne ferme (constat R2-08) ; le
  rapport affiche un 2770 comme un 0770.
- **Paquet et timer** (D89) : apply ne réactive le timer qu'en réécrivant son fichier, et ni `doctor` ni
  `status` ne le contrôlent, contrairement à ce que demande le §14.4 (constat R1-49) ; `ogs init --quiet`
  engendre sans un mot une clé maître absente — le postinst l'évite, pas un appel direct — et repose à
  chaque mise à jour les modes de `Paths.Dirs()`, `/etc/ogs/templates` en 0750 contre le 0700 récursif
  d'`ogs harden` : deux juges pour un même chemin ; sous `--quiet`, le journal du timer ne dit que
  « N renouvellement(s) en échec », le détail restant dans l'audit.
- **Migration** (D90) : la lecture en root sur la source suit un lien que le compte web de la source y
  aurait posé (`shared/uploads → /etc`) ; la lire sous son identité casserait sur les téléversements de
  `www-data`.
- **Ports des projets** (D94) : l'écoute étrangère ne rend l'attente critique qu'au plan — un compte qui
  prend le port entre le plan et le démarrage de l'instance fait échouer une attente non critique, et le
  vhost d'un projet neuf est écrit quand même (code 6, le refus nommé) ; un pool FPM `listen: tcp` n'est
  éprouvé qu'à l'attribution, pas à son compte — le maître l'ouvre en root —, `doctor` ne le relit pas ;
  rien ne réserve un port au noyau (`ip_local_reserved_ports` n'empêche pas un bind explicite), seules
  l'activation par socket de systemd ou le socket Unix fermeraient la fenêtre d'un redémarrage. La reprise
  depuis le disque ne vaut que pour une base **vide** : une `state.db` restaurée d'une sauvegarde ancienne
  garde ses numéros, même contredits par les unités écrites depuis. Une écoute de root sur le port d'un
  projet n'est pas signalée — docker-proxy y est légitime —, pas plus qu'un démon de root mal configuré.
- **Noms de certificat hérités** (D94) : un nom qui sort de l'espace du projet, ou deux certificats d'un
  projet sous un même nom, ne sont refusés qu'au `plan` et à l'`apply` de ce projet ; le renouvellement du
  parc, qui ne passe pas par eux, réémet encore l'un sur l'autre deux certificats de même nom tant que la
  déclaration n'est pas corrigée — `doctor` la nomme.
- **Complétion** : cobra crée la commande `completion` à l'exécution, si bien que l'annotation qui la
  dispenserait de root et sa traduction restent sans effet — `ogs completion bash` exige root et répond en
  anglais.
- **Majeurs de sécurité non traités** par les lots, reproduits par la revue :
  - `AllowOverride All` sur le docroot Prestashop fait appliquer par `www-data`, identité partagée par
    tous les projets, les directives qu'un `.htaccess` du projet choisit (constat S2R-01 ; D87 borne les
    `Options`, pas le reste). Une liste fermée ne suffit pas — les drapeaux `[H=…]` et `[P]` de
    `RewriteRule` atteignent `server-status` et le socket FPM d'un voisin — : la correction retenue est
    `AllowOverride None`, les règles de Prestashop 1.7, 8 et 9 portées par le template, en cours et non
    livrée ;
  - les secrets d'un projet autres que les mots de passe SGBD et AMQP (retirés depuis D91 et D93) restent
    au keystore après `project delete`, `--purge` et la purge des archives comprises, et un projet recréé
    sous le même identifiant en hérite (constat R1-21).
- **Prestashop sous nginx n'a pas de back-office** : le vhost n'exécute que le front controller racine
  (`location ~ ^/index.php … internal;`, puis `location ~ \.php$ { return 404; }`), si bien que
  `admin…/index.php`, `/api`, les `.php` des modules (`ajax.php`) et `index.php` en accès direct répondent
  404, là où Apache les sert. Relevé par la revue, non traité : il faut décider quels points d'entrée PHP
  ouvrir. Le même template ne sert pas les images produit dont l'identifiant a trois chiffres ou plus.

**Points ouverts** (D93) :

- **Reprendre un homonyme du broker** : aucun geste ne confie à ogs un vhost ou un compte AMQP qu'il n'a pas
  créés — ni `--force`, ni l'import, qui ne relève pas RabbitMQ. Le seul remède est de nommer autrement
  (`spec.amqp.vhost`, `spec.amqp.user`) ou de retirer l'homonyme. Une adoption explicite demanderait de
  sceller le mot de passe réel du compte, qu'ogs ne peut pas lire sur le broker.
- **`hidepid`** : la saisie hors ligne de commande ferme `/proc/<pid>/cmdline` pour les secrets d'ogs, pas
  pour les autres outils de la machine ; monter `/proc` en `hidepid=2` n'est ni exigé ni contrôlé par
  `doctor`.

