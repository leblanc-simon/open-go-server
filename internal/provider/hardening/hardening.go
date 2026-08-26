// Package hardening restreint les droits des répertoires sensibles du système :
// ceux de configuration, et la racine des homes. Un serveur mutualisé porte des
// utilisateurs de projet qui ont un compte local : sans restriction, ils lisent
// /etc/nginx, /etc/mysql ou /etc/telegraf, y trouvent des chemins, des noms de
// bases, des jetons, et dressent la carte du serveur — y compris celle des sites
// de leurs voisins. Un simple « ls /home » la leur donne aussi (D60).
//
// Deux règles gouvernent ce paquet :
//
//   - on ne desserre jamais. Une cible plus permissive que l'état constaté est
//     ignorée : l'administrateur qui a mis 0700 là où la table dit 0750 avait
//     probablement une raison, et la lui retirer serait une régression de
//     sécurité commise par un outil de sécurité.
//   - on ne touche que ce qui existe, et jamais un lien symbolique : suivre un
//     lien reviendrait à modifier une cible inconnue, éventuellement hors d'/etc.
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

	"leblanc.io/open-go-server/internal/engine"
)

// Target décrit un chemin à restreindre.
type Target struct {
	// Path est le répertoire visé, hors préfixe.
	Path string `yaml:"path" json:"path"`
	// Owner et Group sont les propriétaires attendus. Un groupe vide laisse le
	// groupe en place : plusieurs services lisent leur configuration sous leur
	// propre identité, et leur retirer le groupe les casserait.
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
// Ce qui n'y figure pas est aussi un choix : /etc/php reste lisible parce que
// l'interpréteur en ligne de commande, exécuté sous l'identité du projet par les
// crons, y lit son php.ini — le restreindre changerait silencieusement le
// comportement de PHP. Seul pool.d, qui porte les variables d'environnement donc
// les mots de passe, est fermé. /etc/systemd/system reste lisible également : ses
// unités sont écrites par ogs et ne portent aucun secret, et plusieurs outils
// d'exploitation les lisent.
func Defaults(homeRoot string) []Target {
	targets := []Target{
		{
			Path: "/etc/ogs", Owner: "root", Group: "root", DirMode: 0o700, FileMode: 0o600,
			Recursive: true,
			Why:       "clé maître du keystore, déclarations de projets, identifiants d'administration",
			Risk:      "aucun : seul ogs, exécuté en root, y accède",
		},
		{
			Path: "/etc/letsencrypt", Owner: "root", Group: "root", DirMode: 0o700, FileMode: 0o600,
			Recursive: true,
			Why:       "clés privées des certificats",
			Risk: "un service non-root qui lirait un certificat ici perdrait l'accès ; " +
				"ogs range les siens dans /var/lib/ogs/acme",
		},
		{
			Path: "/etc/apache2", Owner: "root", Group: "root", DirMode: 0o750,
			Why: "vhosts : noms de domaine, chemins, fichiers d'authentification",
			Risk: "aucun : le processus maître lit la configuration en root, " +
				"les workers héritent des descripteurs ouverts",
		},
		{
			Path: "/etc/nginx", Owner: "root", Group: "root", DirMode: 0o750,
			Why:  "idem Apache",
			Risk: "aucun : le maître lit en root",
		},
		{
			Path: "/etc/php", DirMode: 0o755,
			Why: "traversable à dessein : le PHP en ligne de commande, exécuté sous " +
				"l'identité du projet, lit son php.ini",
			Risk: "fermer ce répertoire ferait tourner les crons sans leur php.ini, " +
				"sans le moindre message",
		},
		{
			Path: "/etc/mysql", Owner: "root", Group: "root", DirMode: 0o750,
			Why: "debian.cnf porte les identifiants de maintenance ; conf.d révèle " +
				"la topologie du serveur",
			Risk: "les clients en ligne de commande perdent les réglages globaux du " +
				"[client] ; une connexion explicite (hôte, utilisateur, mot de passe) " +
				"continue de fonctionner",
		},
		{
			Path: "/etc/postgresql", Owner: "root", Group: "postgres", DirMode: 0o750,
			Why:  "pg_hba.conf décrit qui peut se connecter et comment",
			Risk: "les outils du paquet tournent sous postgres, d'où le groupe",
		},
		{
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
			Path: "/etc/redis", Owner: "root", Group: "redis", DirMode: 0o750, FileMode: 0o640,
			Why:  "requirepass en clair",
			Risk: "le serveur tourne sous redis, d'où le groupe",
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
			Path: "/etc/logrotate.d", Owner: "root", Group: "root", DirMode: 0o750,
			Why:  "révèle l'emplacement des journaux de tous les projets",
			Risk: "aucun : logrotate tourne en root",
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
	// Loose énumère les fichiers trop permissifs trouvés sous le chemin.
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
	if fs, distant := remoteFS(path); distant {
		st.Verdict = VerdictRefused
		st.Reason = "monté en " + fs + " : le mode ne nous appartient pas"
		return st
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

// looseFiles énumère les fichiers plus permissifs que la cible.
func (p *Provider) looseFiles(t Target, root string) []string {
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
			if err != nil {
				continue
			}
			if looser(info.Mode().Perm(), fileTarget(info.Mode().Perm(), t.FileMode)) {
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
		quoi = append(quoi, fmt.Sprintf("mode %04o → %04o", st.Mode, st.Target.DirMode))
	}
	if !ownerMatches(st.Target, st) {
		quoi = append(quoi, fmt.Sprintf("propriétaire %s:%s → %s", st.Owner, st.Group,
			OwnerLabel(st.Target)))
	}
	if n := len(st.Loose); n > 0 {
		quoi = append(quoi, fmt.Sprintf("%d fichier(s) → %04o", n, st.Target.FileMode))
	}
	return st.Target.Path + " : " + strings.Join(quoi, ", ")
}

// apply restreint le chemin. Le mode n'est jamais desserré, et le propriétaire
// n'est changé que si les comptes visés existent.
func (p *Provider) apply(st State) error {
	path := p.resolve(st.Target.Path)
	if looser(st.Mode, st.Target.DirMode) {
		if err := os.Chmod(path, st.Target.DirMode); err != nil {
			return err
		}
	}
	for _, f := range st.Loose {
		info, err := os.Lstat(f)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := os.Chmod(f, fileTarget(info.Mode().Perm(), st.Target.FileMode)); err != nil {
			return err
		}
	}
	if ownerMatches(st.Target, st) {
		return nil
	}
	// Hors root, le propriétaire ne peut pas être changé : c'est le cas du mode
	// préfixé, où les comptes système n'existent pas non plus. Le mode, lui, a
	// bien été restreint — l'essentiel est fait, et le dire en échec serait faux.
	if os.Geteuid() != 0 {
		return nil
	}
	uid, gid, err := p.lookup(st.Target.Owner, st.Target.Group)
	if err != nil {
		// Un groupe absent signifie un service non installé : la restriction de
		// mode a eu lieu, c'est l'essentiel.
		return nil
	}
	return os.Chown(path, uid, gid)
}

// fileTarget calcule le mode d'un fichier en préservant ses bits d'exécution.
// Un durcissement ne doit pas rendre un fichier inexécutable : les hooks vivent
// sous /etc/ogs, et les priver de ce bit les ferait refuser par ogs lui-même.
func fileTarget(current, target os.FileMode) os.FileMode {
	return target | (current & 0o110)
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
	n := 0
	for _, st := range states {
		if st.Verdict == VerdictRestrict {
			n++
		}
	}
	return n
}
