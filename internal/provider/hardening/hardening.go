// Package hardening restreint les droits des répertoires sensibles du système :
// ceux de configuration, et la racine des homes. Un serveur mutualisé porte des
// utilisateurs de projet qui ont un compte local : sans restriction, ils lisent
// /etc/nginx, /etc/mysql ou /etc/telegraf, y trouvent des chemins, des noms de
// bases, des jetons, et dressent la carte du serveur — y compris celle des sites
// de leurs voisins. Un simple « ls /home » la leur donne aussi (D60).
//
// Trois règles gouvernent ce paquet :
//
//   - on ne desserre jamais. Une cible plus permissive que l'état constaté est
//     ignorée : l'administrateur qui a mis 0700 là où la table dit 0750 avait
//     probablement une raison, et la lui retirer serait une régression de
//     sécurité commise par un outil de sécurité.
//   - on ne touche que ce qui existe, et jamais un lien symbolique : suivre un
//     lien reviendrait à modifier une cible inconnue, éventuellement hors d'/etc.
//   - on ne coupe aucun démon de sa configuration (D56). Le groupe est celui du
//     service qui lit, et rien n'est fermé sans lui : un compte introuvable fait
//     refuser la cible plutôt que fermer à l'aveugle.
package hardening

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
)

// Target décrit un chemin à restreindre.
type Target struct {
	// Path est le répertoire visé, hors préfixe.
	Path string `yaml:"path" json:"path"`
	// Owner et Group sont les propriétaires attendus. Un groupe vide laisse le
	// groupe en place : plusieurs services lisent leur configuration sous leur
	// propre identité, et leur retirer le groupe les casserait. Un propriétaire
	// vide laisse de même le propriétaire : Redis réécrit sa configuration dans
	// un répertoire que le paquet lui donne.
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Group string `yaml:"group,omitempty" json:"group,omitempty"`
	// DirMode est le mode des répertoires, FileMode celui des fichiers. Un
	// FileMode nul laisse les fichiers inchangés.
	DirMode  os.FileMode `yaml:"dirMode" json:"dirMode"`
	FileMode os.FileMode `yaml:"fileMode,omitempty" json:"fileMode,omitempty"`
	// Recursive étend le traitement à toute l'arborescence. Réservé aux endroits
	// dont chaque niveau est sensible, comme les clés privées.
	Recursive bool `yaml:"recursive,omitempty" json:"recursive,omitempty"`
	// Why explique la restriction, et Risk ce qu'elle peut casser. Les deux
	// figurent dans le rapport : appliquer sans comprendre est le meilleur moyen
	// d'interrompre un service.
	Why  string `yaml:"-" json:"why,omitempty"`
	Risk string `yaml:"-" json:"risk,omitempty"`
}

// Defaults énumère les répertoires à restreindre, dans l'ordre où ils sont
// rapportés. La liste vise Debian et ses dérivées. homeRoot est la racine des
// homes (naming.homeRoot) ; vide, ou réduite à « / », elle est omise.
//
// Chaque ligne répond d'abord à une question : sous quelle identité ce répertoire
// est-il lu, ou réécrit (D56) ? Un démon qui lit sous la sienne perd sa
// configuration quand on la lui ferme, et il la perd sans un mot — MariaDB ignore
// un fichier d'options qu'il ne peut pas ouvrir et repart sur ses défauts, à
// l'écoute de toutes les interfaces. Le groupe est donc celui du service partout
// où un processus non root lit, workers du serveur web compris ; les projets n'en
// sont membres nulle part, c'est www-data qui est membre des leurs. Le
// propriétaire n'est laissé en place (Owner vide) que là où le paquet le donne au
// service pour qu'il réécrive ce qu'il contient : il ne dit rien aux projets, qui
// ne sont ni le propriétaire ni le groupe.
//
// Ce qui n'y figure pas est aussi un choix : /etc/php reste lisible parce que
// l'interpréteur en ligne de commande, exécuté sous l'identité du projet par les
// crons, y lit son php.ini — le restreindre changerait silencieusement le
// comportement de PHP. /etc/systemd/system reste lisible également : ses unités
// sont écrites par ogs et ne portent aucun secret, et plusieurs outils
// d'exploitation les lisent.
//
// pool.d, lui, n'est fermé par aucune cible, et ce n'est pas un choix : ses
// fichiers, en 0644, portent les variables d'environnement des pools, donc
// leurs mots de passe. C'est un point ouvert (§20 du cahier).
func Defaults(homeRoot string) []Target {
	targets := []Target{
		{
			Path: "/etc/ogs", Owner: "root", Group: "root", DirMode: 0o700, FileMode: 0o600,
			Recursive: true,
			Why:       "clé maître du keystore, déclarations de projets, identifiants d'administration",
			Risk:      "aucun : seul ogs, exécuté en root, y accède",
		},
		{
			// certbot tourne en root et range les clés sous archive/ et live/, qu'il
			// crée en 0700. Un service non root qui sert un certificat y accède, selon
			// la convention Debian, par le groupe ssl-cert posé sur ces deux
			// répertoires et sur la clé — groupe que certbot recopie d'une clé à la
			// suivante. Rien n'est donc repris aux fichiers, ni leur groupe ni leur
			// mode : rendre la clé à root, ou fermer aux autres le certificat
			// renouvelé, que certbot écrit en 0644 root:root, couperait ce service à
			// son prochain redémarrage — et le groupe perdu ne reviendrait plus, le
			// renouvellement recopiant celui de la clé précédente. La racine se ferme
			// à l'énumération et reste traversable par tous.
			Path: "/etc/letsencrypt", Owner: "root", DirMode: 0o711,
			Why: "clés privées des certificats ; certbot les protège lui-même sous archive/ " +
				"et live/ (0700), la racine n'a plus qu'à ne pas s'énumérer",
			Risk: "aucun pour un service qui suit la convention Debian : la traversée reste " +
				"ouverte, et ni le mode ni le groupe d'un fichier ne changent — une clé confiée " +
				"au groupe ssl-cert le reste",
		},
		{
			// Le groupe du serveur web (system.WebUser) : le maître lit la
			// configuration en root, mais un worker ouvre certains fichiers à la
			// requête, non au chargement.
			Path: "/etc/apache2", Owner: "root", Group: "www-data", DirMode: 0o750,
			Why: "vhosts : noms de domaine, chemins, fichiers d'authentification",
			Risk: "le maître lit la configuration en root, mais les workers tournent sous " +
				"www-data et ouvrent à la requête les fichiers d'authentification " +
				"(AuthUserFile, AuthGroupFile), les RewriteMap et le magasin de mod_md : " +
				"d'où le groupe. Fermés au serveur, ils rendraient une 500, une redirection " +
				"perdue ou un certificat jamais renouvelé, apachectl -t restant au vert",
		},
		{
			Path: "/etc/nginx", Owner: "root", Group: "www-data", DirMode: 0o750,
			Why: "idem Apache",
			Risk: "le maître lit en root, mais les workers tournent sous www-data et ouvrent " +
				"à chaque requête auth_basic_user_file, ou ce qu'un root et un alias " +
				"désignent ici : d'où le groupe",
		},
		{
			Path: "/etc/php", DirMode: 0o755,
			Why: "traversable à dessein : le PHP en ligne de commande, exécuté sous " +
				"l'identité du projet, lit son php.ini",
			Risk: "fermer ce répertoire ferait tourner les crons sans leur php.ini, " +
				"sans le moindre message",
		},
		{
			// L'unité du paquet porte User=mysql : mariadbd lit ses fichiers d'options
			// sous cette identité, seul debian-start tournant en root. debian.cnf, lui,
			// est en 0600 root:root, fermé au groupe comme aux autres.
			Path: "/etc/mysql", Owner: "root", Group: "mysql", DirMode: 0o750,
			Why: "debian.cnf porte les identifiants de maintenance ; conf.d révèle " +
				"la topologie du serveur",
			Risk: "le serveur lit sa configuration sous mysql, d'où le groupe : sans lui, il " +
				"ignorerait en silence chaque fichier d'options et repartirait sur ses " +
				"défauts, à l'écoute de toutes les interfaces. Les clients en ligne de " +
				"commande des projets perdent les réglages globaux du [client] ; une " +
				"connexion explicite (hôte, utilisateur, mot de passe) continue de fonctionner",
		},
		{
			// Le paquet donne /etc/postgresql à postgres, et pg_createcluster lancé sous
			// ce compte y crée le répertoire d'une version : le propriétaire est laissé
			// en place, le reprendre n'ajoutant rien à la fermeture que porte le mode.
			Path: "/etc/postgresql", Group: "postgres", DirMode: 0o750,
			Why: "pg_hba.conf décrit qui peut se connecter et comment",
			Risk: "le serveur et les outils du paquet tournent sous postgres, d'où le groupe. " +
				"pg_wrapper, lui, lit postgresql.conf sous l'identité de l'appelant pour " +
				"trouver le cluster : pour un utilisateur de projet, psql et pg_dump sans -p " +
				"visent alors le port 5432, et --cluster, PGCLUSTER ou ~/.postgresqlrc " +
				"échouent. Un cron de projet dont le cluster n'est pas sur 5432 doit " +
				"préciser -p, ou PGPORT",
		},
		{
			// Le broker lit sous rabbitmq ; rabbitmq-plugins, qui écrit enabled_plugins,
			// s'exécute sous l'identité de l'appelant — root — par le script d'enveloppe
			// du paquet. Le groupe suffit donc.
			Path: "/etc/rabbitmq", Owner: "root", Group: "rabbitmq", DirMode: 0o750, FileMode: 0o640,
			Why:  "cookie Erlang et mot de passe d'administration",
			Risk: "le broker tourne sous rabbitmq, d'où le groupe",
		},
		{
			Path: "/etc/telegraf", Owner: "root", Group: "telegraf", DirMode: 0o750, FileMode: 0o640,
			Why:  "telegraf.conf porte les jetons d'écriture de la base de métriques",
			Risk: "l'agent tourne sous telegraf, d'où le groupe",
		},
		{
			// Le régime du paquet (dpkg-statoverride) : 2770 redis:redis, redis.conf en
			// 0640. Redis réécrit sa configuration — CONFIG REWRITE, et Sentinel à
			// chaque changement d'état, bascule comprise — par un fichier temporaire
			// renommé dans le répertoire : il lui faut en rester propriétaire, ou garder
			// l'écriture par le groupe, et le setgid fait hériter du groupe redis le
			// fichier neuf. Rien de cela ne concerne les projets, qui ne sont ni l'un ni
			// l'autre : seuls les autres perdent l'accès.
			Path: "/etc/redis", Group: "redis", DirMode: 0o770, FileMode: 0o640,
			Why: "requirepass en clair",
			Risk: "le serveur tourne sous redis et réécrit lui-même sa configuration par un " +
				"fichier renommé dans le répertoire : propriétaire et écriture du groupe lui " +
				"sont laissés. Un /etc/redis déjà ramené en 0750 root:redis reste tel quel, " +
				"ogs ne desserrant jamais : « chown redis /etc/redis && chmod 2770 " +
				"/etc/redis » rend la réécriture",
		},
		{
			Path: "/etc/ssl/private", Owner: "root", Group: "ssl-cert", DirMode: 0o710,
			Why: "clés privées du système",
			Risk: "0710 et le groupe ssl-cert sont la convention Debian ; " +
				"les services qui lisent une clé y sont déjà membres",
		},
		{
			Path: "/etc/cron.d", Owner: "root", Group: "root", DirMode: 0o750,
			Why:  "les fichiers révèlent les commandes et les identités de chaque projet",
			Risk: "aucun : cron lit en root",
		},
		{
			Path: "/etc/fail2ban", Owner: "root", Group: "root", DirMode: 0o750,
			Why: "jail.d énumère les journaux de tous les projets, donc le parc, " +
				"et les adresses jamais bannies",
			Risk: "aucun : le démon tourne en root ; fail2ban-client est réservé " +
				"à root de toute façon",
		},
		{
			Path: "/etc/logrotate.d", Owner: "root", Group: "root", DirMode: 0o750,
			Why:  "révèle l'emplacement des journaux de tous les projets",
			Risk: "aucun : logrotate tourne en root",
		},
		{
			// Même régime que celui qu'ogs pose lui-même en écrivant les clés et
			// les tables DKIM (mail.OpenDKIMDirMode, mail.TableMode) : deux juges en
			// désaccord feraient osciller l'apply et harden. Récursif, parce que les
			// clés privées vivent sous keys/ ; le propriétaire des fichiers n'est
			// jamais changé et un mode n'est jamais desserré, si bien qu'une clé en
			// 0600 opendkim:opendkim reste telle quelle — la donner à root la
			// rendrait illisible au démon, qui retiendrait alors en file tout le
			// courrier du domaine (D69).
			Path: "/etc/opendkim", Owner: "root", Group: "opendkim", DirMode: 0o750, FileMode: 0o640,
			Recursive: true,
			Why:       "clés privées DKIM des projets, et tables qui désignent leurs domaines",
			Risk: "un fichier posé ici à la main passe en 0640 au groupe opendkim : lisible par le " +
				"démon, fermé aux utilisateurs des projets",
		},
		{
			Path: "/etc/sudoers.d", Owner: "root", Group: "root", DirMode: 0o750,
			Why:  "révèle les délégations accordées",
			Risk: "aucun : sudo lit en root",
		},
	}
	// La racine des homes (D60). Ni Recursive ni FileMode : sous /home vivent les
	// répertoires des projets, dont model.defaultDirs est le seul juge (0711 pour
	// le home, 0710 pour ses répertoires). Un durcissement qui les imposerait
	// aussi ferait osciller le plan entre deux valeurs.
	if root := filepath.Clean(homeRoot); root != "" && root != "." && root != "/" {
		targets = append(targets, Target{
			Path: root, Owner: "root", Group: "root", DirMode: 0o711,
			Why: "la racine des homes énumère le parc : un « ls /home » donne " +
				"le nom de chaque site voisin, et le chemin de son code",
			Risk: "la traversée reste ouverte à tous, donc le serveur web sert les " +
				"fichiers comme avant et chaque projet atteint son home ; ce qui " +
				"disparaît est l'énumération — les globs non privilégiés " +
				"(ls /home, du -sh /home/*), la complétion, et tout agent de " +
				"sauvegarde ou de supervision qui parcourrait /home sous une " +
				"identité non root. Le gain est partiel, et il faut le savoir : " +
				"/etc/passwd est lisible par tous et énumère déjà les homes",
		})
	}
	return targets
}

// State est l'état constaté d'une cible.
type State struct {
	Target Target `json:"target" yaml:"target"`
	// Present vaut faux quand le chemin n'existe pas : le service n'est pas
	// installé, il n'y a rien à faire.
	Present bool `json:"present" yaml:"present"`
	// Symlink signale un lien symbolique, que l'on refuse de suivre.
	Symlink bool `json:"symlink" yaml:"symlink"`
	// Reason motive un refus. Un rapport qui dit « refusé » sans dire pourquoi
	// laisse l'administrateur soupçonner un défaut là où il y a une décision.
	Reason string      `json:"reason,omitempty" yaml:"reason,omitempty"`
	Mode   os.FileMode `json:"mode" yaml:"mode"`
	Owner  string      `json:"owner" yaml:"owner"`
	Group  string      `json:"group" yaml:"group"`
	// Verdict résume la comparaison : conforme, à restreindre, déjà plus strict,
	// absent, ou refusé.
	Verdict string `json:"verdict" yaml:"verdict"`
	// Loose énumère les fichiers à reprendre sous le chemin : ceux dont le mode
	// est plus permissif que la cible, et ceux qui n'en portent pas le groupe
	// (cf. looseFiles). Un fichier plus strict que la cible peut donc y figurer —
	// c'est alors son seul groupe qui est en cause, et son mode restera tel quel :
	// le nommer « trop permissif » enverrait chercher une permissivité qui
	// n'existe pas.
	Loose []string `json:"loose,omitempty" yaml:"loose,omitempty"`
}

// Verdicts possibles.
const (
	VerdictOK       = "conforme"
	VerdictRestrict = "à restreindre"
	VerdictStricter = "déjà plus strict"
	VerdictAbsent   = "absent"
	VerdictRefused  = "refusé"
)

// Provider observe et restreint les droits.
type Provider struct {
	// Prefix décale les chemins, pour les essais.
	Prefix string
	// HomeRoot est la racine des homes, qui entre dans la table par défaut (D60).
	// Sans effet lorsque Targets remplace la table : celle-ci est alors prise
	// telle quelle.
	HomeRoot string
	// Targets remplace la table par défaut lorsqu'elle est renseignée.
	Targets []Target
	// Skip énumère les chemins à laisser tranquilles.
	Skip []string
	// LookupIDs résout un couple utilisateur/groupe. Isolé pour les tests.
	LookupIDs func(owner, group string) (int, int, error)
}

// targets applique les remplacements et les exclusions.
func (p *Provider) targets() []Target {
	list := p.Targets
	if len(list) == 0 {
		list = Defaults(p.HomeRoot)
	}
	skip := map[string]bool{}
	for _, s := range p.Skip {
		skip[filepath.Clean(s)] = true
	}
	out := make([]Target, 0, len(list))
	for _, t := range list {
		if !skip[filepath.Clean(t.Path)] {
			out = append(out, t)
		}
	}
	return out
}

// Observe constate l'état de chaque cible.
func (p *Provider) Observe() []State {
	var out []State
	for _, t := range p.targets() {
		out = append(out, p.observeOne(t))
	}
	return out
}

func (p *Provider) observeOne(t Target) State {
	st := State{Target: t}
	path := p.resolve(t.Path)
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		st.Verdict = VerdictAbsent
		return st
	case err != nil:
		st.Verdict = VerdictRefused
		return st
	}
	st.Present = true
	if info.Mode()&os.ModeSymlink != 0 {
		st.Symlink = true
		st.Verdict = VerdictRefused
		st.Reason = "lien symbolique : la cible est inconnue"
		return st
	}
	if !info.IsDir() {
		st.Verdict = VerdictRefused
		st.Reason = "n'est pas un répertoire"
		return st
	}
	st.Mode = info.Mode().Perm()
	st.Owner, st.Group = ownerNames(info)

	// Un chemin monté depuis le réseau — /home sur NFS est le cas courant —
	// n'appartient pas à cette machine : le mode vient du serveur, et root_squash
	// fait échouer le chmod. Le dire « refusé » vaut mieux qu'un avertissement
	// d'échec à chaque apply, qui finirait par ne plus être lu.
	if nom, distant := remoteFS(path); distant {
		st.Verdict = VerdictRefused
		st.Reason = "monté en " + nom + " : le mode ne nous appartient pas"
		return st
	}

	// Le groupe du service est la condition de la fermeture, non son complément :
	// le démon qui lisait par les bits des autres ne lit plus, ensuite, que par
	// ceux du groupe. Un compte introuvable — service absent, ou installé sous un
	// autre nom — laisse donc le répertoire tel quel, et le dit. Fermer quand même
	// coupait ce démon, puis annonçait « ✓ » sans avoir posé le groupe, et le
	// rapport réclamait la même restriction à chaque passage.
	if t.Owner != "" || t.Group != "" {
		if _, _, err := p.lookup(t.Owner, t.Group); err != nil {
			st.Verdict = VerdictRefused
			st.Reason = p.missingAccount(t) + " introuvable : fermer sans lui couperait " +
				"le service qui y lit"
			return st
		}
	}

	switch {
	case looser(st.Mode, t.DirMode) || !ownerMatches(t, st):
		st.Verdict = VerdictRestrict
	case st.Mode != t.DirMode:
		st.Verdict = VerdictStricter
	default:
		st.Verdict = VerdictOK
	}
	// Les fichiers ne comptent que si la cible en fixe le mode.
	if t.FileMode != 0 {
		st.Loose = p.looseFiles(t, path)
		if len(st.Loose) > 0 && st.Verdict == VerdictOK {
			st.Verdict = VerdictRestrict
		}
	}
	return st
}

// looseFiles énumère les fichiers à reprendre : ceux plus permissifs que la
// cible, et ceux qui n'en portent pas le groupe. Le groupe compte autant que le
// mode — un fichier déjà en 0640 mais resté root:root est illisible à l'agent qui
// le lit sous sa propre identité (D56) —, et le juger sur le seul mode le dirait
// « conforme » à jamais : un parc déjà durci ne serait jamais réparé.
func (p *Provider) looseFiles(t Target, root string) []string {
	gid := p.targetGID(t)
	var out []string
	walk := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				continue
			}
			info, err := e.Info()
			// Un tube ou un socket n'a rien d'un fichier de configuration, et apply,
			// qui n'ouvre que des fichiers réguliers, le laisserait tel quel : le
			// relever ferait réclamer à chaque passage ce qui ne sera jamais fait.
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if looser(info.Mode().Perm(), fileTarget(info.Mode().Perm(), t.FileMode)) ||
				(gid >= 0 && fileGID(info) != gid) {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	if !t.Recursive {
		walk(root)
		sort.Strings(out)
		return out
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		walk(path)
		return nil
	})
	sort.Strings(out)
	return out
}

// Plan produit les étapes de restriction. Aucune n'est critique : durcir /etc ne
// doit pas interrompre une opération plus large.
func (p *Provider) Plan(states []State) []*engine.Step {
	var steps []*engine.Step
	for _, st := range states {
		st := st
		resource := "hardening" + st.Target.Path
		if st.Verdict != VerdictRestrict {
			steps = append(steps, engine.NewStep(resource, engine.NoOp,
				fmt.Sprintf("%s : %s", st.Target.Path, st.Verdict)))
			continue
		}
		s := engine.NewStep(resource, engine.Update, p.summary(st))
		s.Critical = false
		s.Apply = func(context.Context) error { return p.apply(st) }
		steps = append(steps, s)
	}
	return steps
}

func (p *Provider) summary(st State) string {
	var quoi []string
	if looser(st.Mode, st.Target.DirMode) {
		quoi = append(quoi, fmt.Sprintf("mode %04o → %04o", st.Mode, dirTarget(st.Mode, st.Target.DirMode)))
	}
	if !ownerMatches(st.Target, st) {
		quoi = append(quoi, fmt.Sprintf("propriétaire %s:%s → %s", st.Owner, st.Group,
			OwnerLabel(st.Target)))
	}
	if n := len(st.Loose); n > 0 {
		detail := fmt.Sprintf("%d fichier(s) → %04o", n, st.Target.FileMode)
		if st.Target.Group != "" {
			detail += " :" + st.Target.Group
		}
		quoi = append(quoi, detail)
	}
	return st.Target.Path + " : " + strings.Join(quoi, ", ")
}

// apply restreint le chemin : le groupe du répertoire, puis son mode, puis les
// fichiers relevés. Le mode n'est jamais desserré, et rien n'est modifié par son
// nom : le répertoire est ouvert sans suivre de lien, chaque fichier aussi,
// composant par composant sous la cible (fs.OpenFileAt), et l'un comme l'autre
// sont modifiés par leur descripteur. Là où le propriétaire reste au service —
// /etc/redis, 2770 redis:redis —, celui-ci peut remplacer un fichier ou un
// sous-répertoire par un lien entre le constat et l'écriture : suivre le nom, ce
// serait le laisser choisir sur quoi root pose le groupe du service et un mode
// qui le lui ouvre.
func (p *Provider) apply(st State) error {
	path := p.resolve(st.Target.Path)
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return err
	}
	// Le groupe avant le mode : fermer d'abord laisserait le service, le temps
	// d'un appel, hors de sa propre configuration. Hors root, le propriétaire ne
	// peut pas être changé : c'est le cas du mode préfixé, où les comptes système
	// n'existent pas non plus. Le mode, lui, est restreint quand même — le dire en
	// échec serait faux.
	if !ownerMatches(st.Target, st) && os.Geteuid() == 0 {
		uid, gid, err := p.lookup(st.Target.Owner, st.Target.Group)
		if err != nil {
			// observeOne refuse une cible dont le compte manque : il a disparu
			// depuis le constat. Fermer sans lui couperait le service, et l'annoncer
			// fait serait faux.
			return fmt.Errorf("%s introuvable : répertoire laissé en l'état", p.missingAccount(st.Target))
		}
		if err := dir.Chown(uid, gid); err != nil {
			return err
		}
	}
	if looser(info.Mode(), st.Target.DirMode) {
		// Les bits spéciaux sont conservés : le setgid de /etc/redis fait hériter
		// du groupe redis le fichier que Redis crée en réécrivant sa configuration,
		// et ni lui ni le sticky n'accordent le moindre droit.
		special := info.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
		if err := dir.Chmod(dirTarget(info.Mode(), st.Target.DirMode) | special); err != nil {
			return err
		}
	}
	gid := p.targetGID(st.Target)
	for _, f := range st.Loose {
		if err := restrictFile(path, f, gid, st.Target.FileMode); err != nil {
			return err
		}
	}
	return nil
}

// restrictFile reprend un fichier relevé par looseFiles. Il est ouvert sans
// suivre de lien, ni en chemin ni à l'entrée : disparu, remplacé par un lien ou
// par autre chose qu'un fichier régulier depuis le constat, ce n'est plus celui
// que l'on a relevé, et il est laissé tel quel — comme un lien l'a toujours été.
func restrictFile(root, path string, gid int, mode os.FileMode) error {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return nil
	}
	f, _, err := fs.OpenFileAt(root, rel, filepath.Base(path))
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	// Le groupe avant le mode. Fermer un fichier à 0640 sans lui donner le groupe
	// du service le rend illisible au démon qui le lit sous sa propre identité :
	// c'est tout l'équilibre de /etc/telegraf, /etc/rabbitmq et /etc/redis (D56), et
	// telegraf, dans ce cas, démarre, ne collecte rien et se tait. Le propriétaire,
	// lui, est laissé en place : le prendre à un service lui retirerait le droit de
	// réécrire sa propre configuration — redis.conf après un CONFIG REWRITE. Le
	// chown efface au passage le bit setgid d'un fichier exécutable par son groupe,
	// et ogs ne le repose pas : fileTarget part d'un Perm() masqué à 0o777, où ni
	// setuid ni setgid ne figurent, et le mode visé ne les porte pas davantage.
	// C'est voulu — ces bits n'ont rien à faire sur un fichier de configuration —,
	// et les rendre reviendrait à desserrer.
	if gid >= 0 && fileGID(info) != gid {
		if err := f.Chown(-1, gid); err != nil && os.Geteuid() == 0 {
			return err
		}
	}
	// On ne desserre jamais, ici comme pour les répertoires : un fichier déjà plus
	// strict que la cible reste tel quel. Sans ce contrôle, un fichier retenu pour
	// son seul groupe se verrait rouvrir son mode.
	want := fileTarget(info.Mode().Perm(), mode)
	if !looser(info.Mode().Perm(), want) {
		return nil
	}
	return f.Chmod(want)
}

// dirTarget calcule le mode d'un répertoire à restreindre : l'intersection du
// mode constaté et de la cible. harden retire des droits, il n'en accorde aucun,
// pas même ceux que la cible admet : /etc/redis admet l'écriture du groupe, que le
// paquet donne à Redis pour réécrire sa configuration, mais un répertoire qui ne
// la portait pas ne la reçoit pas d'un outil de durcissement.
func dirTarget(current, target os.FileMode) os.FileMode {
	return current.Perm() & target.Perm()
}

// missingAccount nomme ce qui manque pour poser le propriétaire d'une cible : le
// groupe d'abord, puisque c'est par lui que le service lit, puis le compte.
func (p *Provider) missingAccount(t Target) string {
	if t.Group != "" {
		if _, _, err := p.lookup("", t.Group); err != nil {
			return "groupe " + t.Group
		}
	}
	return "compte " + t.Owner
}

// fileTarget calcule le mode d'un fichier en préservant ses bits d'exécution.
// Un durcissement ne doit pas rendre un fichier inexécutable : les hooks vivent
// sous /etc/ogs, et les priver de ce bit les ferait refuser par ogs lui-même.
func fileTarget(current, target os.FileMode) os.FileMode {
	return target | (current & 0o110)
}

// targetGID résout le groupe de la cible, ou rend -1. Un groupe vide veut dire
// « laisser en place » (cf. Target.Group) : il n'y a rien à poser. Un groupe
// introuvable fait refuser la cible dès le constat (observeOne) ; -1 n'est plus
// alors qu'une garde, pour un compte retiré entre le constat et l'écriture.
func (p *Provider) targetGID(t Target) int {
	if t.Group == "" {
		return -1
	}
	_, gid, err := p.lookup("", t.Group)
	if err != nil {
		return -1
	}
	return gid
}

func (p *Provider) lookup(owner, group string) (int, int, error) {
	if p.LookupIDs != nil {
		return p.LookupIDs(owner, group)
	}
	uid := -1
	if owner != "" {
		u, err := user.Lookup(owner)
		if err != nil {
			return -1, -1, err
		}
		uid, _ = strconv.Atoi(u.Uid)
	}
	gid := -1
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return -1, -1, err
		}
		gid, _ = strconv.Atoi(g.Gid)
	}
	return uid, gid, nil
}

func (p *Provider) resolve(path string) string {
	if p.Prefix == "" {
		return path
	}
	return filepath.Join(p.Prefix, path)
}

// looser indique que le mode constaté accorde des droits que la cible refuse.
// C'est la seule comparaison qui déclenche une action : un mode plus strict est
// laissé tel quel.
func looser(current, target os.FileMode) bool {
	return current.Perm()&^target.Perm() != 0
}

func ownerMatches(t Target, st State) bool {
	if t.Owner != "" && st.Owner != t.Owner {
		return false
	}
	if t.Group != "" && st.Group != t.Group {
		return false
	}
	return true
}

// OwnerLabel rend le propriétaire attendu sous forme lisible.
func OwnerLabel(t Target) string {
	switch {
	case t.Owner != "" && t.Group != "":
		return t.Owner + ":" + t.Group
	case t.Owner != "":
		return t.Owner
	case t.Group != "":
		return ":" + t.Group
	}
	return "inchangé"
}

// Restrict compte les cibles à restreindre.
func Restrict(states []State) int {
	return count(states, VerdictRestrict)
}

// Refused compte les cibles laissées en l'état pour un motif — lien, système de
// fichiers distant, compte du service introuvable. Il n'y a rien à y appliquer,
// mais les dire « conformes » serait faux.
func Refused(states []State) int {
	return count(states, VerdictRefused)
}

func count(states []State, verdict string) int {
	n := 0
	for _, st := range states {
		if st.Verdict == verdict {
			n++
		}
	}
	return n
}
