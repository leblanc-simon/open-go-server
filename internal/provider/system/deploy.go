package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/spec"
)

// KeyName est le nom de la clé de déploiement.
const KeyName = "id_ed25519"

// sshDir est le répertoire des clés, relatif au home du projet.
const sshDir = ".ssh"

// maxKeySize borne ce que root lit d'un fichier de clé. .ssh appartient au
// projet, qui donne à ses fichiers la taille qu'il veut : une clé creuse de
// plusieurs Gio ne lui coûte rien, et la rotation la lisait en mémoire puis la
// réécrivait pleine dans les sauvegardes, sur /var. Une paire ed25519 tient en
// quelques centaines d'octets ; seize Kio couvrent tout ce qu'écrit ssh-keygen.
const maxKeySize = 16 << 10

// KeyPath renvoie le chemin de la clé privée d'un projet.
func KeyPath(root string) string { return filepath.Join(root, sshDir, KeyName) }

// keyStep génère la paire ed25519 de déploiement. La clé privée reste sur le
// serveur : le déploiement se fait en tirage, la clé publique étant déclarée en
// deploy key chez l'hébergeur du dépôt (D33).
//
// L'étape est non critique : ssh-keygen peut manquer sur une image minimale, et
// son absence ne doit pas empêcher la création du site.
func (p *Provider) keyStep(proj *model.Project) *engine.Step {
	path := KeyPath(proj.Spec.Layout.Root)
	resource := "system/deploy-key/" + proj.ID()
	// os.Lstat, non os.Stat : .ssh appartient à l'utilisateur du projet, qui en
	// choisit les entrées. Un os.Stat conclurait « clé absente » sur un lien
	// pendant, et ssh-keygen — lancé en root, sans O_NOFOLLOW — écrirait la clé
	// privée dans la cible du lien, que le chown suivant lui donnerait. C'est
	// l'invariant du §7.1.2 : aucun processus root n'ouvre en écriture un chemin
	// dont un non-root contrôle le répertoire.
	for _, f := range []string{path, path + ".pub"} {
		info, err := os.Lstat(f)
		if err != nil || info.Mode().IsRegular() {
			continue
		}
		// Un lien, un répertoire, un socket : rien n'est détruit et rien n'est
		// généré par-dessus. L'étape reste non critique, le reste du projet
		// s'applique et l'avertissement dit quoi faire.
		s := engine.NewStep(resource, engine.Update, "clé de déploiement à régénérer")
		s.Critical = false
		s.Apply = func(context.Context) error {
			return fmt.Errorf("%s existe et n'est pas un fichier régulier : le retirer "+
				"avant de générer la clé de déploiement", f)
		}
		return s
	}
	if _, err := os.Lstat(path); err == nil {
		return engine.NewStep(resource, engine.NoOp, "clé de déploiement")
	}
	s := engine.NewStep(resource, engine.Create,
		fmt.Sprintf("clé de déploiement ed25519 (%s)", path))
	s.Critical = false
	s.Apply = func(ctx context.Context) error { return p.generateKey(ctx, proj) }
	// La compensation vient après coup, quand l'utilisateur a eu tout le temps
	// de remplacer .ssh par un lien : un os.Remove par le nom supprimerait alors
	// la clé du répertoire visé — celle de root comprise.
	root := proj.Spec.Layout.Root
	s.Compensate = func(context.Context) error {
		_ = fs.RemoveFileAt(root, sshDir, KeyName)
		_ = fs.RemoveFileAt(root, sshDir, KeyName+".pub")
		return nil
	}
	return s
}

// generateKey produit la paire dans un répertoire temporaire appartenant à root,
// puis l'installe dans .ssh. ssh-keygen ouvre son fichier de sortie en
// O_WRONLY|O_CREAT|O_TRUNC, sans O_NOFOLLOW : le laisser écrire directement dans
// un répertoire que l'utilisateur du projet possède lui donnerait une écriture
// root sur le chemin de son choix, et le chown qui suivait la cible du lien la
// lui remettait.
func (p *Provider) generateKey(ctx context.Context, proj *model.Project) error {
	dir, err := os.MkdirTemp("", "ogs-keygen-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	gen := filepath.Join(dir, KeyName)
	comment := fmt.Sprintf("ogs=%s@%s", proj.ID(), hostname())
	if _, err := p.Exec.Run(ctx, exec.Command{
		Name: "ssh-keygen",
		// Sans passphrase : la clé est destinée à des déploiements non
		// interactifs, et sa protection repose sur les permissions du fichier.
		Args: []string{"-t", "ed25519", "-N", "", "-C", comment, "-f", gen},
	}); err != nil {
		return err
	}
	if p.Exec.Simulated() {
		return nil
	}
	uid, gid, err := p.ResolveOwner(proj, "")
	if err != nil {
		return err
	}
	owner := fs.Owner{UID: uid, GID: gid}
	// Le plan a vérifié .ssh, mais à l'observation : entre elle et cette étape,
	// l'utilisateur a pu le remplacer par un lien. Le répertoire est donc rouvert
	// et contrôlé ici, et c'est par ce descripteur que la paire est posée.
	ssh, err := openSSHDir(proj, owner)
	if err != nil {
		return err
	}
	defer ssh.Close()
	return installKeyPair(gen, ssh, owner)
}

// installKeyPair pose la paire générée à sa place définitive, par le descripteur
// de .ssh — clé publique comprise : elle est tout aussi exposée, un lien à sa
// place faisant tronquer puis donner au projet le fichier visé. DirAt.WriteFile
// délie le nom puis le recrée en O_CREAT|O_EXCL|O_NOFOLLOW, mode et propriétaire
// posés sur le descripteur.
func installKeyPair(gen string, ssh *fs.DirAt, owner fs.Owner) error {
	for _, f := range []struct {
		src, name string
		mode      os.FileMode
	}{
		{gen, KeyName, 0o600},
		{gen + ".pub", KeyName + ".pub", 0o644},
	} {
		data, err := os.ReadFile(f.src)
		if err != nil {
			return err
		}
		if err := ssh.WriteFile(f.name, data, f.mode, owner); err != nil {
			return err
		}
	}
	return nil
}

// keyOwner est le compte auquel reviennent le .ssh d'un projet et ses clés. En
// mode simulé, rien n'est ni contrôlé ni changé : sous préfixe, l'arborescence
// appartient à qui fait tourner l'essai, et les comptes n'existent pas.
func keyOwner(ex exec.Executor, proj *model.Project, ident Identity) (fs.Owner, error) {
	if ex.Simulated() {
		return fs.KeepOwner, nil
	}
	u, err := ident.LookupUser(proj.Spec.User.Name)
	if err != nil || u == nil {
		return fs.KeepOwner, fmt.Errorf("utilisateur %s introuvable", proj.Spec.User.Name)
	}
	g, err := ident.LookupGroup(proj.Spec.User.Group)
	if err != nil || g == nil {
		return fs.KeepOwner, fmt.Errorf("groupe %s introuvable", proj.Spec.User.Group)
	}
	return fs.Owner{UID: u.UID, GID: g.GID}, nil
}

// openSSHDir ouvre le .ssh d'un projet sans suivre de lien, ni en chemin ni au
// dernier composant, et refuse un répertoire qui n'appartient pas à owner.
//
// Le home appartient à l'utilisateur (D51) : il renomme son .ssh sans privilège
// et pose à la place un lien vers le répertoire de son choix — /root/.ssh,
// /etc/apt/apt.conf.d. Un contrôle du seul dernier composant (os.Lstat sur la
// clé) laisse ce lien être résolu, et root y supprimait puis créait la paire,
// qu'il donnait ensuite au projet. Un vrai répertoire qui ne lui appartient pas
// est refusé de même : l'utilisateur peut aussi renommer en .ssh un répertoire
// de root resté dans son home. Toutes les opérations qui suivent — contrôle,
// sauvegarde, remplacement, relecture — passent par le descripteur rendu, donc
// par le répertoire constaté ici.
func openSSHDir(proj *model.Project, owner fs.Owner) (*fs.DirAt, error) {
	root := proj.Spec.Layout.Root
	path := filepath.Join(root, sshDir)
	ssh, err := fs.OpenDirAt(root, sshDir)
	switch {
	case errors.Is(err, fs.ErrNotDirectory):
		return nil, fmt.Errorf("%s n'est pas un répertoire véritable (lien symbolique ?) : ogs n'y lit ni "+
			"n'y écrit la clé de déploiement — le rétablir, puis « ogs apply %s »", path, proj.ID())
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("%s absent : « ogs apply %s » le crée (%w)", path, proj.ID(), err)
	case err != nil:
		return nil, err
	}
	if owner.UID >= 0 {
		uid, _, err := ssh.Owner()
		if err != nil {
			ssh.Close()
			return nil, err
		}
		if int(uid) != owner.UID {
			// L'apply rend .ssh au projet (dirSteps) : c'est lui qui répare, non
			// cette commande, qui n'a pas de plan pour le dire.
			ssh.Close()
			return nil, fmt.Errorf("%s appartient à l'uid %d, non à %s (uid %d) : ogs n'y lit ni n'y écrit "+
				"la clé de déploiement — le rendre au projet (« ogs apply %s » le fait), puis relancer",
				path, uid, proj.Spec.User.Name, owner.UID, proj.ID())
		}
	}
	return ssh, nil
}

// AuthorizedKeysPath renvoie le chemin des clés autorisées d'un projet.
func AuthorizedKeysPath(root string) string {
	return filepath.Join(root, ".ssh", "authorized_keys")
}

// authorizedKeysStep lie .ssh/authorized_keys au trousseau commun du parc
// lorsqu'il existe (D61) : les clés d'administration sont ainsi déclarées en un
// seul endroit, et l'arrivée d'un opérateur ne demande pas de repasser sur chaque
// projet. Un lien, non une copie — un fichier recopié figerait le trousseau au
// jour de la création du projet.
//
// Aucune étape si le trousseau est absent : la fonction est facultative, et la
// disparition du fichier commun ne doit pas emporter un authorized_keys propre au
// projet.
//
// L'étape est non critique, comme la génération de clé : un accès d'administration
// qui ne se pose pas ne doit pas empêcher la création du site.
func (p *Provider) authorizedKeysStep(proj *model.Project) *engine.Step {
	source := p.Paths.DefaultAuthorizedKeys()
	if _, err := os.Stat(source); err != nil {
		return nil
	}
	link := AuthorizedKeysPath(proj.Spec.Layout.Root)
	resource := "system/authorized-keys/" + proj.ID()

	info, err := os.Lstat(link)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink == 0:
		// Un fichier régulier, posé à la main ou hérité d'un serveur repris : le
		// remplacer par un lien retirerait sans un mot l'accès de qui l'y avait
		// inscrit. L'étape échoue donc sans rien détruire — l'avertissement dit
		// quoi faire, et le reste du projet s'applique.
		s := engine.NewStep(resource, engine.Update,
			"clés autorisées à lier au trousseau commun")
		s.Critical = false
		s.Apply = func(context.Context) error {
			return fmt.Errorf("%s existe et n'est pas un lien : le déplacer, "+
				"ou reporter ses clés dans %s", link, source)
		}
		return s
	case err == nil:
		current, rerr := os.Readlink(link)
		if rerr == nil && current == source {
			// Le propriétaire du lien n'entre pas dans ce constat : sshd résout le
			// lien avant de vérifier quoi que ce soit, et un lchown manqué ferait
			// alors osciller le plan sans rien changer pour personne.
			return engine.NewStep(resource, engine.NoOp, "clés autorisées liées au trousseau commun")
		}
		s := engine.NewStep(resource, engine.Update,
			fmt.Sprintf("clés autorisées : %s → %s", current, source))
		s.Critical = false
		s.Apply = func(context.Context) error { return p.linkAuthorizedKeys(proj, link, source) }
		s.Compensate = func(context.Context) error {
			rel, err := filepath.Rel(proj.Spec.Layout.Root, filepath.Dir(link))
			if err != nil {
				return err
			}
			return fs.SymlinkAt(proj.Spec.Layout.Root, rel, filepath.Base(link), current, p.linkOwner(proj), false)
		}
		return s
	}
	s := engine.NewStep(resource, engine.Create,
		fmt.Sprintf("clés autorisées liées à %s", source))
	s.Critical = false
	s.Apply = func(context.Context) error { return p.linkAuthorizedKeys(proj, link, source) }
	// Ni os.Remove par le nom, qui suivrait un .ssh remplacé par un lien d'ici
	// au rollback et retirerait l'authorized_keys du répertoire visé — celui de
	// root compris —, ni la suppression d'autre chose qu'un lien : c'est un lien
	// que l'étape a posé.
	s.Compensate = func(context.Context) error {
		_ = fs.RemoveLinkAt(proj.Spec.Layout.Root, sshDir, filepath.Base(link))
		return nil
	}
	return s
}

// linkAuthorizedKeys pose le lien, puis lui donne le propriétaire du projet.
//
// Lchown et non Chown : suivre le lien donnerait le trousseau commun — root — au
// dernier projet appliqué. Un lien symbolique n'a pas de mode propre sous Linux
// (toujours 0777, et immuable) : les droits qui décident sont ceux de la cible et
// des répertoires traversés. sshd résout le lien, puis exige sous StrictModes que
// le fichier appartienne à root ou à l'utilisateur et ne soit inscriptible que par
// lui — et il le lit sous l'identité de cet utilisateur, donc il doit aussi lui
// être lisible. C'est au trousseau commun de porter ces droits, non au lien ;
// « doctor » les contrôle, faute de quoi sshd l'ignore en silence.
//
// Le lien est posé par un descripteur de .ssh ouvert sans suivre de lien :
// .ssh appartient à l'utilisateur, qui pourrait sinon le remplacer par un lien
// vers un répertoire de son choix et y faire créer — puis lchown — un fichier
// par root.
func (p *Provider) linkAuthorizedKeys(proj *model.Project, link, source string) error {
	rel, err := filepath.Rel(proj.Spec.Layout.Root, filepath.Dir(link))
	if err != nil {
		return err
	}
	return fs.SymlinkAt(proj.Spec.Layout.Root, rel, filepath.Base(link), source, p.linkOwner(proj), false)
}

// binStep maintient le lien php vers l'interpréteur de la version courante, dans
// le répertoire des liens gérés (D70). Les tâches cron et les unités systemd
// référencent ce lien, jamais un binaire versionné : une bascule de version ne
// réécrit donc aucun de ces fichiers (D30).
//
// Le répertoire appartient à l'utilisateur : le lien est un état réconcilié,
// rétabli à chaque apply s'il a été changé. Un fichier régulier posé à sa place
// porte en revanche le travail de quelqu'un, et n'est remplacé que sous --force.
func (p *Provider) binStep(proj *model.Project) *engine.Step {
	if len(proj.Spec.PHP.Pools) == 0 {
		return nil
	}
	version := proj.Spec.PHP.Pools[0].Version
	target := p.Paths.PHPBin(version)
	root := proj.Spec.Layout.Root
	label := model.LinkDir + "/php"
	kind, current, err := fs.LinkStateAt(root, model.LinkDir, "php")
	switch {
	case err != nil:
		// Un composant remplacé par un lien : root n'écrira pas là où
		// l'utilisateur l'envoie. L'arborescence le signale et le corrige.
		s := engine.NewStep("system/bin/php", engine.Update, "lien "+label+" — chemin refusé")
		s.Critical = false
		s.Apply = func(context.Context) error { return err }
		return s
	case kind == fs.EntrySymlink && current == target:
		return engine.NewStep("system/bin/php", engine.NoOp,
			fmt.Sprintf("lien %s → php %s", label, version))
	case kind == fs.EntryOther && !p.Force:
		s := engine.NewStep("system/bin/php", engine.Update,
			fmt.Sprintf("lien %s → %s — un fichier occupe déjà ce nom", label, target))
		s.Critical = false
		s.Apply = func(context.Context) error {
			return fmt.Errorf("%s existe et n'est pas un lien : le déplacer, ou relancer avec "+
				"--force pour le remplacer par le lien géré", filepath.Join(model.BinDir(root), "php"))
		}
		return s
	}
	action, summary := engine.Create, fmt.Sprintf("lien %s → %s", label, target)
	if kind != fs.EntryAbsent {
		action, summary = engine.Update, fmt.Sprintf("lien %s : %s → %s", label, orValue(current, "fichier"), target)
	}
	s := engine.NewStep("system/bin/php", action, summary)
	s.Apply = func(context.Context) error {
		return fs.SymlinkAt(root, model.LinkDir, "php", target, p.linkOwner(proj), p.Force)
	}
	s.Compensate = func(context.Context) error {
		if kind == fs.EntrySymlink {
			return fs.SymlinkAt(root, model.LinkDir, "php", current, p.linkOwner(proj), false)
		}
		return fs.RemoveLinkAt(root, model.LinkDir, "php")
	}
	return s
}

// linkOwner est le propriétaire des liens posés dans un répertoire de
// l'utilisateur : lui-même. En mode simulé, rien n'est changé.
func (p *Provider) linkOwner(proj *model.Project) fs.Owner {
	if p.Exec.Simulated() {
		return fs.KeepOwner
	}
	uid, gid, err := p.ResolveOwner(proj, "")
	if err != nil {
		return fs.KeepOwner
	}
	return fs.Owner{UID: uid, GID: gid}
}

// LegacyLinks sont les liens que l'ancien .bin/ portait : ceux-là seuls sont
// retirés à la migration (D70).
var LegacyLinks = model.LegacyLinkNames

// legacyBin est le constat porté sur l'ancien ~/.bin d'un projet (D116).
type legacyBin struct {
	// present : un ~/.bin existe, sous une forme ou une autre.
	present bool
	// ours : c'est l'ancien répertoire d'ogs — répertoire véritable à root
	// (constaté hors mode simulé), en 0710 ou 0750, dont chaque lien connu
	// désigne une cible qu'ogs y posait. reason dit sinon pourquoi.
	ours   bool
	reason string
	// links : les liens connus et leur cible, que la migration retire.
	links map[string]string
}

// ogsLinkTarget dit si target est une cible qu'ogs posait pour le lien name de
// l'ancien .bin : l'interpréteur d'une version de PHP, ou le binaire d'un
// runtime installé dans le home (BinaryPath, BundlePath au moment de D70).
func (p *Provider) ogsLinkTarget(root, name, target string) bool {
	match := func(pattern string) bool {
		ok, _ := filepath.Match(pattern, target)
		return ok
	}
	switch name {
	case "php":
		v, ok := strings.CutPrefix(target, p.Paths.PHPBin(""))
		return ok && phpVersion.MatchString(v)
	case "node":
		return match(filepath.Join(root, ".nvm", "versions", "node", "v*", "bin", "node"))
	case "ruby":
		return match(filepath.Join(root, ".rbenv", "versions", "*", "bin", "ruby"))
	case "bundle":
		return match(filepath.Join(root, ".rbenv", "versions", "*", "bin", "bundle"))
	case "python":
		return target == filepath.Join(root, "www", ".venv", "bin", "python")
	case "app":
		return target == filepath.Join(root, "www", "app")
	}
	return false
}

var phpVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// inspectLegacyBin ouvre ~/.bin par descripteur, sans suivre de lien (D83), et
// juge s'il est l'ancien répertoire d'ogs. Le critère est celui de l'empreinte,
// jamais du nom : un ~/.bin repris d'un home préexistant, ou posé par
// l'utilisateur après D70, porte son travail — ses liens php ou node vers ses
// propres binaires, que sa crontab et son .profile emploient.
func (p *Provider) inspectLegacyBin(dir *fs.DirAt, root string) legacyBin {
	st := legacyBin{present: true, links: map[string]string{}}
	if !p.Exec.Simulated() {
		uid, _, err := dir.Owner()
		if err != nil {
			st.reason = err.Error()
			return st
		}
		if uid != 0 {
			// Seul root peut avoir posé un répertoire à root dans le home ; un
			// répertoire à quelqu'un d'autre n'a jamais été celui d'ogs.
			st.reason = fmt.Sprintf("il appartient à l'uid %d, non à root", uid)
			return st
		}
	}
	perm, err := dir.Perm()
	if err != nil {
		st.reason = err.Error()
		return st
	}
	if perm != 0o710 && perm != 0o750 {
		st.reason = fmt.Sprintf("il est en %04o, jamais un mode qu'ogs lui a donné", perm)
		return st
	}
	for _, name := range LegacyLinks {
		target, isLink, present, err := dir.Readlink(name)
		switch {
		case err != nil:
			st.reason = err.Error()
			return st
		case !present || !isLink:
			continue
		case !p.ogsLinkTarget(root, name, target):
			st.reason = fmt.Sprintf("son lien %s désigne %s, qu'ogs n'y a jamais posé", name, target)
			return st
		}
		st.links[name] = target
	}
	// Une entrée qu'ogs n'a pas posée — un script, un lien d'un autre nom — fait
	// laisser le répertoire entier : jugée après coup, elle laissait partir les
	// liens d'ogs, puis retenait le répertoire, et chaque apply rendait 6 sans
	// rien pouvoir y faire (D116).
	names, err := dir.Names()
	if err != nil {
		st.reason = err.Error()
		return st
	}
	var foreign []string
	for _, n := range names {
		if _, ok := st.links[n]; !ok {
			foreign = append(foreign, strconv.Quote(n))
		}
	}
	if len(foreign) > 0 {
		st.reason = fmt.Sprintf("il contient %s, qu'ogs n'y a pas posé", strings.Join(foreign, ", "))
		return st
	}
	st.ours = true
	return st
}

// observeLegacyBin constate l'ancien ~/.bin d'un projet. Un .bin que la
// déclaration porte encore après normalisation est celui de l'exploitant
// (model.legacyDirs) : il n'est pas jugé.
func (p *Provider) observeLegacyBin(proj *model.Project) (legacyBin, error) {
	if proj.DeclaresDir(model.LegacyLinkDir) {
		return legacyBin{present: true, reason: "spec.layout.dirs le déclare"}, nil
	}
	dir, err := fs.OpenDirAt(proj.Spec.Layout.Root, model.LegacyLinkDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return legacyBin{}, nil
	case errors.Is(err, fs.ErrNotDirectory):
		return legacyBin{present: true, reason: "ce n'est pas un répertoire véritable"}, nil
	case err != nil:
		return legacyBin{}, err
	}
	defer dir.Close()
	return p.inspectLegacyBin(dir, proj.Spec.Layout.Root), nil
}

// PlanLegacyCommands réécrit les commandes déclarées qui désignent un lien de
// l'ancien .bin vers ~/.local/bin (D116). L'ancien §7.5 invitait à passer par ce
// lien dans ses propres scripts : une tâche ou un worker qui l'invoque en chemin
// absolu échouait, la migration une fois passée, sans que rien ne le dise.
//
// proj n'est jamais modifié : la fonction rend la copie sur laquelle le reste du
// plan se calcule — crons et unités suivent ainsi dans l'apply même —, et un
// plan recalculé sur le même projet (la TUI affiche puis applique) retrouve les
// mêmes références, donc la même étape. L'étape rendue porte la réécriture de la
// déclaration — à placer en fin de plan, une fois les liens posés —, qui n'a
// lieu qu'à l'application — jamais au chargement — et ne
// touche que ces valeurs, le reste du fichier restant octet pour octet celui de
// l'exploitant (spec.RewriteValues).
//
// Rien n'est réécrit quand ~/.bin est encore là sans être l'ancien répertoire
// d'ogs : les liens qu'il porte sont ceux de l'exploitant, et ses commandes les
// désignent à dessein.
func (p *Provider) PlanLegacyCommands(proj *model.Project) (*model.Project, *engine.Step) {
	// Une commande n'est réécrite que si chaque lien qu'elle désignera est déjà
	// dans ~/.local/bin : un runtime dont l'installation échoue ne pose pas son
	// lien, et la tâche, réécrite, perdrait l'ancien qui fonctionnait. Celle-ci
	// garde .bin (la déclaration le désigne encore) et attend l'apply suivant.
	// Le lien php est l'exception : binStep le pose dans ce même plan, étape
	// critique qui précède tâches et unités — un projet d'avant D70 n'a pas
	// encore de ~/.local/bin à son premier apply.
	var edits []model.LegacyBinRef
	for _, r := range proj.LegacyBinRefs() {
		if r.Rewritten == "" {
			continue
		}
		missing := 0
		for _, n := range r.Links {
			if n == "php" && len(proj.Spec.PHP.Pools) > 0 {
				continue
			}
			missing += len(missingManagedLinks(proj.Spec.Layout.Root, []string{n}))
		}
		if missing == 0 {
			edits = append(edits, r)
		}
	}
	if len(edits) == 0 {
		return proj, nil
	}
	legacy, err := p.observeLegacyBin(proj)
	if err != nil || (legacy.present && !legacy.ours) {
		return proj, nil
	}
	path := p.Paths.ProjectFile(proj.ID())
	fields := make([]string, 0, len(edits))
	for _, r := range edits {
		fields = append(fields, r.Field)
	}
	s := engine.NewStep("system/declaration/legacy-bin", engine.Update,
		fmt.Sprintf("déclaration %s : %s → %s dans %s", path, model.LegacyLinkDir+"/",
			model.LinkDir+"/", strings.Join(fields, ", ")))
	// Non critique : crons et unités sont déjà calculés sur les nouveaux chemins ;
	// tant que la déclaration reste en l'état, l'ancien .bin est retenu
	// (PlanLegacyBin), et le prochain apply la reprend.
	s.Critical = false
	var before []byte
	var written []byte
	s.Apply = func(context.Context) error {
		for _, r := range edits {
			if missing := missingManagedLinks(proj.Spec.Layout.Root, r.Links); len(missing) > 0 {
				return fmt.Errorf("%s non réécrite : %s absent — l'ancien %s est gardé", r.Field,
					strings.Join(missing, ", "), model.LegacyLinkDir)
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// « apply -f » d'une déclaration qui n'est pas déposée : rien à
				// réécrire sous /etc/ogs/projects, le fichier passé reste à reprendre.
				return fmt.Errorf("%s absent : reporter à la main dans la déclaration appliquée %s", path,
					strings.Join(fields, ", "))
			}
			return err
		}
		spEdits := make([]spec.Edit, 0, len(edits))
		for _, r := range edits {
			spEdits = append(spEdits, spec.Edit{Path: r.Path, Old: r.Value, New: r.Rewritten})
		}
		out, err := spec.RewriteValues(data, spEdits)
		if err != nil {
			return fmt.Errorf("%s : %w — remplacer %s par %s dans %s ; l'ancien %s est gardé d'ici là", path, err,
				"<racine>/"+model.LegacyLinkDir+"/", "<racine>/"+model.LinkDir+"/", strings.Join(fields, ", "),
				model.LegacyLinkDir)
		}
		if err := spec.WriteFileAtomic(path, out); err != nil {
			return err
		}
		before, written = data, out
		return nil
	}
	// Le déroulage rend le texte d'origine, s'il est encore celui qu'on a écrit :
	// une retouche de l'exploitant entre-temps l'emporte.
	s.Compensate = func(context.Context) error {
		if written == nil {
			return nil
		}
		if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, written) {
			return nil
		}
		return spec.WriteFileAtomic(path, before)
	}
	return proj.WithLegacyBinRefs(edits), s
}

// missingManagedLinks nomme, parmi names, les liens absents de ~/.local/bin,
// lus par descripteur sans suivre de lien (D83).
func missingManagedLinks(root string, names []string) []string {
	var out []string
	for _, n := range names {
		if kind, _, err := fs.LinkStateAt(root, model.LinkDir, n); err != nil || kind == fs.EntryAbsent {
			out = append(out, filepath.Join(model.BinDir(root), n))
		}
	}
	return out
}

// declaredLegacyRefs relit la déclaration déposée et nomme les valeurs qui
// désignent encore l'ancien .bin. C'est elle que « ogs cron run », un apply
// suivant et l'exploitant lisent : tant qu'elle désigne .bin, .bin reste — la
// copie du plan n'en dit rien. Une déclaration absente (« apply -f » d'un
// fichier non déposé) ou illisible retient aussi.
func (p *Provider) declaredLegacyRefs(proj *model.Project) ([]string, error) {
	path := p.Paths.ProjectFile(proj.ID())
	raw, err := spec.Load(path)
	if err != nil {
		return nil, err
	}
	raw.Spec.Layout.Root = proj.Spec.Layout.Root
	raw.Spec.User.Home = proj.Spec.User.Home
	var fields []string
	for _, r := range raw.LegacyBinRefs() {
		fields = append(fields, r.Field)
	}
	return fields, nil
}

// PlanLegacyBin retire l'ancien répertoire des liens, .bin/, une fois les crons
// et les unités passés au nouvel emplacement. L'étape vient en fin de plan et
// vide d'abord les rechargements en attente : une unité réécrite mais pas
// encore relue par systemd désigne toujours l'ancien lien, et le retirer avant
// ferait échouer le premier redémarrage du service.
//
// Seul l'ancien répertoire d'ogs est retiré (D116) : à root, en 0710 ou 0750,
// ne portant que des liens connus, chacun désignant une cible qu'ogs y posait. Un .bin à l'utilisateur,
// déclaré par l'exploitant ou dont un lien désigne autre chose n'est pas touché,
// et ne produit aucune étape. Tout passe par un descripteur ouvert sans suivre de
// lien (D83) ; seuls les liens reconnus partent, et le répertoire seulement vide.
//
// Tant qu'une déclaration désigne encore .bin — sous une forme qu'ogs ne réécrit
// pas, un chemin relatif, ou parce que la réécriture du fichier a été refusée —,
// rien n'est retiré : l'étape échoue en la nommant. Le fichier déposé est relu
// à l'application, après l'étape qui le réécrit.
func (p *Provider) PlanLegacyBin(proj *model.Project) *engine.Step {
	root := proj.Spec.Layout.Root
	legacy := filepath.Join(root, model.LegacyLinkDir)
	st, err := p.observeLegacyBin(proj)
	if err != nil || !st.present || !st.ours {
		return nil
	}
	s := engine.NewStep("system/legacy-bin", engine.Delete,
		fmt.Sprintf("retrait de l'ancien %s (liens désormais dans %s)", legacy, model.BinDir(root)))
	s.Critical = false
	s.FlushReloads = true
	var held []string
	for _, r := range proj.LegacyBinRefs() {
		held = append(held, r.Field)
	}
	if len(held) > 0 {
		s.Action = engine.Update
		s.Summary = fmt.Sprintf("ancien %s conservé : la déclaration le désigne encore", legacy)
		s.FlushReloads = false
		s.Apply = func(context.Context) error {
			return fmt.Errorf("%s conservé : %s le désigne encore sous une forme qu'ogs ne réécrit pas — "+
				"y écrire %s, puis relancer « ogs apply %s »", legacy, strings.Join(held, ", "),
				filepath.Join(model.BinDir(root), "<lien>"), proj.ID())
		}
		return s
	}
	s.Apply = func(context.Context) error {
		// Rouvert et rejugé ici : entre le plan et l'étape, l'utilisateur a pu
		// renommer l'ancien répertoire et poser le sien sous ce nom.
		dir, err := fs.OpenDirAt(root, model.LegacyLinkDir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer dir.Close()
		// La déclaration déposée d'abord : une réécriture refusée (valeur repliée,
		// fusion YAML…) la laisse désigner .bin, et « ogs cron run » reconstruit
		// la commande depuis elle.
		fields, err := p.declaredLegacyRefs(proj)
		if err != nil {
			return fmt.Errorf("%s conservé : déclaration illisible (%w)", legacy, err)
		}
		if len(fields) > 0 {
			return fmt.Errorf("%s conservé : %s désigne encore %s dans %s — y écrire %s, puis relancer "+
				"« ogs apply %s »", legacy, p.Paths.ProjectFile(proj.ID()), model.LegacyLinkDir+"/",
				strings.Join(fields, ", "), model.LinkDir+"/", proj.ID())
		}
		now := p.inspectLegacyBin(dir, root)
		if !now.ours {
			return fmt.Errorf("%s laissé en place : %s", legacy, now.reason)
		}
		// Chaque lien ne part que si son remplaçant est en place : un runtime
		// dont l'installation a échoué n'a pas posé le sien, et ce qui passait
		// encore par l'ancien — un script, un .profile — cesserait de marcher.
		names := make([]string, 0, len(now.links))
		for name := range now.links {
			names = append(names, name)
		}
		sort.Strings(names)
		if missing := missingManagedLinks(root, names); len(missing) > 0 {
			return fmt.Errorf("%s conservé : %s absent — l'apply suivant le retire une fois le lien posé",
				legacy, strings.Join(missing, ", "))
		}
		for name, target := range now.links {
			if err := dir.RemoveLink(name, target); err != nil {
				return err
			}
		}
		// Le répertoire, vide, part par son nom depuis un descripteur de la
		// racine : si l'utilisateur lui en a substitué un autre entre-temps,
		// seul un répertoire vide peut partir — rien ne s'y perd.
		left, err := fs.RemoveEmptyDirAt(root, model.LegacyLinkDir)
		if err != nil {
			return err
		}
		if len(left) > 0 {
			return fmt.Errorf("%s conservé : il contient encore %s, qu'ogs n'a pas posé",
				legacy, strings.Join(left, ", "))
		}
		return nil
	}
	return s
}

func orValue(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// errNoDeployKey dit l'absence de clé, et ce qui la crée.
var errNoDeployKey = errors.New("aucune clé de déploiement : lancer « ogs apply » pour la générer")

// PublicKey lit la clé publique de déploiement d'un projet, par le descripteur de
// son .ssh (openSSHDir) et sans suivre de lien : « user key show » tourne en
// root, et un os.ReadFile par le nom affichait à l'exploitant le contenu de tout
// fichier que root peut lire, pour peu que .ssh ou la clé soit un lien — ou un
// lien physique, d'où le refus d'une clé qui n'appartient pas au projet. La
// lecture est bornée (maxKeySize) : une clé creuse démesurée ne remplit ni la
// mémoire ni le terminal. Et une clé qui n'est pas du texte imprimable n'est pas
// affichée (keyText).
func PublicKey(ex exec.Executor, proj *model.Project, ident Identity) (string, error) {
	owner, err := keyOwner(ex, proj, ident)
	if err != nil {
		// Un projet déclaré mais jamais appliqué n'a ni compte ni clé : c'est
		// l'absence de la clé qu'il faut dire, et ce qui la crée. Le constat se
		// fait sans rien lire ni suivre.
		if kind, _, lerr := fs.LinkStateAt(proj.Spec.Layout.Root, sshDir, KeyName+".pub"); lerr == nil &&
			kind == fs.EntryAbsent {
			return "", errNoDeployKey
		}
		return "", err
	}
	ssh, err := openSSHDir(proj, owner)
	if err == nil {
		defer ssh.Close()
		var data []byte
		if data, _, err = ssh.ReadFile(KeyName+".pub", owner.UID, maxKeySize); err == nil {
			if !keyText(data) {
				return "", fmt.Errorf("%s n'est pas une clé publique affichable (caractère de contrôle ou "+
					"octet invalide) : ogs ne l'affiche pas — la retirer, puis « ogs apply %s » en génère une",
					filepath.Join(ssh.Path(), KeyName+".pub"), proj.ID())
			}
			return string(data), nil
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return "", errNoDeployKey
	}
	return "", err
}

// keyText dit si une clé publique lue dans .ssh peut s'afficher telle quelle :
// « user key show » et « user key rotate » la rendent brute, pour qu'elle se
// copie. Le fichier appartient au projet, qui y écrit ce qu'il veut, et un saut
// de ligne ou une séquence d'échappement forgeraient des lignes dans le terminal
// de l'exploitant. Une clé publique est du texte imprimable, sur une ligne ; un
// octet qui n'est pas de l'UTF-8 compte comme un contrôle, un terminal en 8 bits
// lisant 0x9b comme l'introducteur d'une séquence.
func keyText(data []byte) bool {
	s := strings.TrimSuffix(string(data), "\n")
	return utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// RotateKey régénère la paire de déploiement en conservant l'ancienne clé
// publique, afin qu'elle puisse être retirée chez l'hébergeur du dépôt.
//
// La commande tourne hors de tout plan : aucun contrôle de l'arborescence ne la
// précède. Tout passe donc par le descripteur de .ssh qu'ouvre openSSHDir, qui
// refuse un lien et un répertoire étranger au projet.
//
// backupDir est le répertoire où atterrit l'ancienne clé privée — jamais le .ssh
// du projet : c'est lui qui le possède, et le nom horodaté de la copie se devine
// à la seconde près, si bien qu'un lien posé d'avance à ce nom ferait écrire par
// root le contenu de la clé là où le projet l'aurait décidé.
func RotateKey(ctx context.Context, ex exec.Executor, proj *model.Project, ident Identity, backupDir string) (old string, err error) {
	owner, err := keyOwner(ex, proj, ident)
	if err != nil {
		return "", err
	}
	ssh, err := openSSHDir(proj, owner)
	if err != nil {
		return "", err
	}
	defer ssh.Close()
	// Le contrôle vient avant toute lecture : une clé remplacée par un lien, un
	// répertoire ou une FIFO n'est ni lue, ni sauvegardée, ni remplacée. Même
	// invariant qu'à la génération (§7.1.2). Une clé qui n'appartient pas au
	// projet ne l'est pas davantage : ce peut être un lien physique vers celle
	// d'un autre compte, dont l'ancienne clé publique serait affichée. Ni une clé
	// démesurée, que root lirait en mémoire puis réécrirait dans les sauvegardes.
	for _, name := range []string{KeyName, KeyName + ".pub"} {
		e, present, err := ssh.Entry(name)
		if err != nil {
			return "", err
		}
		path := filepath.Join(ssh.Path(), name)
		switch {
		case !present:
		case !e.Regular:
			return "", fmt.Errorf("%s existe et n'est pas un fichier régulier : le retirer "+
				"avant de renouveler la clé de déploiement", path)
		case owner.UID >= 0 && int(e.UID) != owner.UID:
			return "", fmt.Errorf("%s appartient à l'uid %d, non à %s : le retirer, ou le rendre au projet, "+
				"avant de renouveler la clé de déploiement", path, e.UID, proj.Spec.User.Name)
		case e.Size > maxKeySize:
			return "", fmt.Errorf("%s fait %d octets, bien plus qu'une clé : le retirer avant de renouveler "+
				"la clé de déploiement", path, e.Size)
		}
	}
	// Relues par le descripteur, avec les mêmes contrôles : entre l'examen et la
	// lecture, l'utilisateur a pu mettre autre chose sous le même nom.
	switch data, _, err := ssh.ReadFile(KeyName+".pub", owner.UID, maxKeySize); {
	case err == nil && !keyText(data):
		return "", fmt.Errorf("%s n'est pas une clé publique affichable (caractère de contrôle ou octet "+
			"invalide) : le retirer avant de renouveler la clé de déploiement", filepath.Join(ssh.Path(), KeyName+".pub"))
	case err == nil:
		old = string(data)
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	switch data, perm, err := ssh.ReadFile(KeyName, owner.UID, maxKeySize); {
	case err == nil:
		if _, err := fs.BackupDataTo(KeyName, data, perm, backupDir, nowFunc()); err != nil {
			return old, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return old, err
	}
	dir, err := os.MkdirTemp("", "ogs-keygen-")
	if err != nil {
		return old, err
	}
	defer os.RemoveAll(dir)
	gen := filepath.Join(dir, KeyName)
	comment := fmt.Sprintf("ogs=%s@%s", proj.ID(), hostname())
	if _, err := ex.Run(ctx, exec.Command{
		Name: "ssh-keygen",
		Args: []string{"-t", "ed25519", "-N", "", "-C", comment, "-f", gen},
	}); err != nil {
		return old, err
	}
	if ex.Simulated() {
		return old, nil
	}
	return old, installKeyPair(gen, ssh, owner)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "serveur"
	}
	return h
}
