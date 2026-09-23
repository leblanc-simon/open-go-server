package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// KeyName est le nom de la clé de déploiement.
const KeyName = "id_ed25519"

// KeyPath renvoie le chemin de la clé privée d'un projet.
func KeyPath(root string) string { return filepath.Join(root, ".ssh", KeyName) }

// PublicKeyPath renvoie le chemin de la clé publique.
func PublicKeyPath(root string) string { return KeyPath(root) + ".pub" }

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
	s.Apply = func(ctx context.Context) error { return p.generateKey(ctx, proj, path) }
	s.Compensate = func(context.Context) error {
		_ = os.Remove(path)
		_ = os.Remove(path + ".pub")
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
func (p *Provider) generateKey(ctx context.Context, proj *model.Project, path string) error {
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
	return installKeyPair(gen, path, uid, gid)
}

// installKeyPair pose la paire générée à sa place définitive, clé publique
// comprise : elle est tout aussi exposée, un lien à sa place faisant tronquer
// puis donner au projet le fichier visé.
func installKeyPair(gen, path string, uid, gid int) error {
	for _, f := range []struct {
		src, dst string
		mode     os.FileMode
	}{
		{gen, path, 0o600},
		{gen + ".pub", path + ".pub", 0o644},
	} {
		data, err := os.ReadFile(f.src)
		if err != nil {
			return err
		}
		if err := installKeyFile(f.dst, data, f.mode, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// installKeyFile écrit un fichier de clé sans jamais suivre de lien. Le chemin
// est délié d'abord, puis créé en O_CREAT|O_EXCL|O_NOFOLLOW : l'ouverture échoue
// si quoi que ce soit occupe le chemin entre les deux — un lien reposé par
// l'utilisateur du projet compris, .ssh étant à lui. Le mode et le propriétaire
// sont posés sur le descripteur, non sur le chemin, qui peut changer entre deux
// appels.
func installKeyFile(path string, data []byte, mode os.FileMode, uid, gid int) error {
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return fmt.Errorf("écriture de %s : %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if err := f.Chown(uid, gid); err != nil {
		return err
	}
	return f.Close()
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
	s.Compensate = func(context.Context) error {
		_ = os.Remove(link)
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
var LegacyLinks = []string{"php", "node", "python", "ruby", "bundle", "app"}

// PlanLegacyBin retire l'ancien répertoire des liens, .bin/, une fois les crons
// et les unités passés au nouvel emplacement. L'étape vient en fin de plan et
// vide d'abord les rechargements en attente : une unité réécrite mais pas
// encore relue par systemd désigne toujours l'ancien lien, et le retirer avant
// ferait échouer le premier redémarrage du service.
//
// Seuls les liens connus sont retirés, sans jamais suivre de lien — le home
// appartient à l'utilisateur, qui a pu remplacer .bin/ —, et le répertoire ne
// part que s'il est vide : ce qui reste n'a pas été posé par ogs.
func (p *Provider) PlanLegacyBin(proj *model.Project) *engine.Step {
	root := proj.Spec.Layout.Root
	legacy := filepath.Join(root, model.LegacyLinkDir)
	info, err := os.Lstat(legacy)
	if err != nil || !info.IsDir() {
		return nil
	}
	s := engine.NewStep("system/legacy-bin", engine.Delete,
		fmt.Sprintf("retrait de l'ancien %s (liens désormais dans %s)", legacy, model.BinDir(root)))
	s.Critical = false
	s.FlushReloads = true
	s.Apply = func(context.Context) error {
		for _, name := range LegacyLinks {
			if err := fs.RemoveLinkAt(root, model.LegacyLinkDir, name); err != nil {
				return err
			}
		}
		remaining, err := fs.RemoveEmptyDirAt(root, model.LegacyLinkDir)
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			return fmt.Errorf("%s conservé : il contient encore %s, qu'ogs n'a pas posé",
				legacy, strings.Join(remaining, ", "))
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

// PublicKey lit la clé publique de déploiement d'un projet.
func PublicKey(root string) (string, error) {
	data, err := os.ReadFile(PublicKeyPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("aucune clé de déploiement : lancer « ogs apply » pour la générer")
		}
		return "", err
	}
	return string(data), nil
}

// RotateKey régénère la paire de déploiement en conservant l'ancienne clé
// publique, afin qu'elle puisse être retirée chez l'hébergeur du dépôt.
//
// backupDir est le répertoire où atterrit l'ancienne clé privée — jamais le .ssh
// du projet : c'est lui qui le possède, et le nom horodaté de la copie se devine
// à la seconde près, si bien qu'un lien posé d'avance à ce nom ferait écrire par
// root le contenu de la clé là où le projet l'aurait décidé.
func RotateKey(ctx context.Context, ex exec.Executor, proj *model.Project, ident Identity, backupDir string) (old string, err error) {
	root := proj.Spec.Layout.Root
	path := KeyPath(root)
	// Le contrôle vient avant toute lecture : la sauvegarde lit par os.ReadFile,
	// donc à travers un lien, et recopierait le contenu d'un fichier de root dans
	// un répertoire que le projet lit. Même invariant qu'à la génération (§7.1.2).
	for _, f := range []string{path, path + ".pub"} {
		if info, lerr := os.Lstat(f); lerr == nil && !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s existe et n'est pas un fichier régulier : le retirer "+
				"avant de renouveler la clé de déploiement", f)
		}
	}
	old, _ = PublicKey(root)
	if _, err := fs.BackupTo(path, backupDir, nowFunc()); err != nil && !os.IsNotExist(err) {
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
	u, err := ident.LookupUser(proj.Spec.User.Name)
	if err != nil || u == nil {
		return old, fmt.Errorf("utilisateur %s introuvable", proj.Spec.User.Name)
	}
	g, err := ident.LookupGroup(proj.Spec.User.Group)
	if err != nil || g == nil {
		return old, fmt.Errorf("groupe %s introuvable", proj.Spec.User.Group)
	}
	return old, installKeyPair(gen, path, u.UID, g.GID)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "serveur"
	}
	return h
}
