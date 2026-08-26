package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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
	if _, err := os.Stat(path); err == nil {
		return engine.NewStep("system/deploy-key/"+proj.ID(), engine.NoOp, "clé de déploiement")
	}
	s := engine.NewStep("system/deploy-key/"+proj.ID(), engine.Create,
		fmt.Sprintf("clé de déploiement ed25519 (%s)", path))
	s.Critical = false
	s.Apply = func(ctx context.Context) error {
		comment := fmt.Sprintf("ogs=%s@%s", proj.ID(), hostname())
		if _, err := p.Exec.Run(ctx, exec.Command{
			Name: "ssh-keygen",
			// Sans passphrase : la clé est destinée à des déploiements non
			// interactifs, et sa protection repose sur les permissions du fichier.
			Args: []string{"-t", "ed25519", "-N", "", "-C", comment, "-f", path},
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
		for file, mode := range map[string]os.FileMode{path: 0o600, path + ".pub": 0o644} {
			if err := os.Chmod(file, mode); err != nil {
				return err
			}
			if err := os.Chown(file, uid, gid); err != nil {
				return err
			}
		}
		return nil
	}
	s.Compensate = func(context.Context) error {
		_ = os.Remove(path)
		_ = os.Remove(path + ".pub")
		return nil
	}
	return s
}

// binStep maintient le lien .bin/php vers l'interpréteur de la version courante.
// Les tâches cron et les unités systemd référencent ce lien, jamais un binaire
// versionné : une bascule de version ne réécrit donc aucun de ces fichiers (D30).
func (p *Provider) binStep(proj *model.Project) *engine.Step {
	if len(proj.Spec.PHP.Pools) == 0 {
		return nil
	}
	version := proj.Spec.PHP.Pools[0].Version
	target := p.Paths.PHPBin(version)
	link := filepath.Join(proj.Spec.Layout.Root, ".bin", "php")

	current, err := os.Readlink(link)
	switch {
	case err == nil && current == target:
		return engine.NewStep("system/bin/php", engine.NoOp,
			fmt.Sprintf("lien .bin/php → php %s", version))
	case err == nil:
		s := engine.NewStep("system/bin/php", engine.Update,
			fmt.Sprintf("lien .bin/php : %s → %s", current, target))
		previous := current
		s.Apply = func(context.Context) error { return replaceSymlink(link, target) }
		s.Compensate = func(context.Context) error { return replaceSymlink(link, previous) }
		return s
	}
	s := engine.NewStep("system/bin/php", engine.Create,
		fmt.Sprintf("lien .bin/php → %s", target))
	s.Apply = func(context.Context) error { return replaceSymlink(link, target) }
	s.Compensate = func(context.Context) error { return os.Remove(link) }
	return s
}

// replaceSymlink remplace un lien de façon atomique : le lien temporaire est créé
// puis renommé, de sorte qu'aucun appelant ne voit de chemin manquant.
func replaceSymlink(link, target string) error {
	// Le mode de .bin/ relève de l'arborescence : ce n'est ici qu'un filet.
	if err := os.MkdirAll(filepath.Dir(link), 0o710); err != nil {
		return err
	}
	tmp := link + ".ogs-tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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
func RotateKey(ctx context.Context, ex exec.Executor, proj *model.Project, ident Identity) (old string, err error) {
	root := proj.Spec.Layout.Root
	old, _ = PublicKey(root)
	path := KeyPath(root)
	if _, err := fs.Backup(path, nowFunc()); err != nil && !os.IsNotExist(err) {
		return old, err
	}
	_ = os.Remove(path)
	_ = os.Remove(path + ".pub")
	comment := fmt.Sprintf("ogs=%s@%s", proj.ID(), hostname())
	if _, err := ex.Run(ctx, exec.Command{
		Name: "ssh-keygen",
		Args: []string{"-t", "ed25519", "-N", "", "-C", comment, "-f", path},
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
	for file, mode := range map[string]os.FileMode{path: 0o600, path + ".pub": 0o644} {
		if err := os.Chmod(file, mode); err != nil {
			return old, err
		}
		if err := os.Chown(file, u.UID, g.GID); err != nil {
			return old, err
		}
	}
	return old, nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "serveur"
	}
	return h
}
