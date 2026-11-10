package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// DeployLink et DeployConf sont, à la racine du home, le lien vers le script de
// déploiement commun et sa configuration (D132). Le script déduit le projet, sa
// configuration et ses hooks de l'emplacement du lien, qu'il ne résout pas : le
// lien doit donc vivre au premier niveau du home, à côté de deploy.conf.
const (
	DeployLink = "deploy.sh"
	DeployConf = "deploy.conf"
)

// maxDeployConf borne ce que root lit de deploy.conf : le fichier est au projet,
// qui lui donne la taille qu'il veut. Ce qu'ogs y écrit tient en quelques lignes.
const maxDeployConf = 4 << 10

// DeployFramework rend la valeur de FRAMEWORK que deploy.conf porte pour un type
// d'application, parmi celles que le script accepte (symfony, laravel,
// prestashop, composer, none, auto). Un php-plain peut être un Laravel, un
// projet Composer ou de simples fichiers : le script le détecte (auto). Les
// types sans PHP n'ont pas d'étape de construction dans le script — none —, il
// leur reste la mise à jour du dépôt, l'arrêt et le redémarrage, et les hooks.
func DeployFramework(kind model.AppKind) string {
	switch kind {
	case model.KindSymfony:
		return "symfony"
	case model.KindPrestashop:
		return "prestashop"
	case model.KindPHPPlain:
		return "auto"
	}
	return "none"
}

// DeployBranch rend la branche déployée : main en production, preprod en
// préproduction.
func DeployBranch(proj *model.Project) string {
	if proj.IsPrep() {
		return "preprod"
	}
	return "main"
}

// DeployConfContent rend le deploy.conf qu'ogs pose pour un projet. L'environnement
// applicatif est toujours prod : la préproduction se distingue par sa branche,
// non par APP_ENV. PROJECT_DIR n'est écrit que si le script ne le déduirait pas
// juste — il le compose en /home/<nom du répertoire du lien>/www.
func DeployConfContent(proj *model.Project) []byte {
	var b strings.Builder
	b.WriteString("# Configuration de ~/" + DeployLink + " (script de déploiement commun).\n")
	b.WriteString("# Posée par ogs à la création du projet, jamais réécrite : elle est au projet,\n")
	b.WriteString("# qui peut y ajouter ses réglages et ses hooks (hook_<étape>).\n")
	fmt.Fprintf(&b, "FRAMEWORK=%q\n", DeployFramework(proj.Spec.App.Kind))
	fmt.Fprintf(&b, "BRANCH=%q\n", DeployBranch(proj))
	b.WriteString("ENVIRONNEMENT=\"prod\"\n")
	root := filepath.Clean(proj.Spec.Layout.Root)
	if code := filepath.Join(root, "www"); code != filepath.Join("/home", filepath.Base(root), "www") {
		fmt.Fprintf(&b, "PROJECT_DIR=%q\n", code)
	}
	return []byte(b.String())
}

// deployLinkStep lie ~/deploy.sh au script de déploiement commun (D132).
//
// Aucune étape si le script est absent : la fonction est facultative. Un
// deploy.sh déjà présent qui n'est pas ce lien — un script propre au projet, un
// lien vers un autre — est laissé en place sans un mot : il est au projet, et
// rien n'y est à protéger, à l'inverse de l'authorized_keys de D61.
//
// L'étape est non critique : un outil de déploiement qui ne se pose pas ne doit
// pas empêcher la création du site.
func (p *Provider) deployLinkStep(proj *model.Project) *engine.Step {
	source := p.Paths.DeployScript()
	if _, err := os.Stat(source); err != nil {
		return nil
	}
	root := proj.Spec.Layout.Root
	resource := "system/deploy-script/" + proj.ID()
	path := filepath.Join(root, DeployLink)
	kind, current, err := fs.LinkStateAt(root, "", DeployLink)
	switch {
	case err != nil:
		s := engine.NewStep(resource, engine.Update, fmt.Sprintf("lien %s → %s — chemin refusé", path, source))
		s.Critical = false
		s.Apply = func(context.Context) error { return err }
		return s
	case kind == fs.EntrySymlink && current == source:
		return engine.NewStep(resource, engine.NoOp, fmt.Sprintf("lien %s → %s", path, source))
	case kind != fs.EntryAbsent:
		return engine.NewStep(resource, engine.NoOp, fmt.Sprintf("%s propre au projet, laissé en place", path))
	}
	s := engine.NewStep(resource, engine.Create, fmt.Sprintf("lien %s → %s", path, source))
	s.Critical = false
	// replace à faux : un fichier posé entre le plan et l'application est au
	// projet, et l'étape échoue plutôt que de le remplacer.
	s.Apply = func(context.Context) error {
		return fs.SymlinkAt(root, "", DeployLink, source, p.linkOwner(proj), false)
	}
	// RemoveLinkAt ne retire qu'un lien, par un descripteur du home.
	s.Compensate = func(context.Context) error {
		_ = fs.RemoveLinkAt(root, "", DeployLink)
		return nil
	}
	return s
}

// deployConfStep pose ~/deploy.conf s'il est absent (D132), au compte du projet :
// le script le lit sous cette identité, et le projet le complète — réglages,
// hooks. Le fichier n'a donc pas d'en-tête d'ogs et n'est jamais réécrit : comme
// le fichier d'environnement de D75, ogs en pose le modèle, l'exploitant en tient
// le contenu. Une branche ou un type changés après coup ne le touchent pas.
//
// Aucune étape sans script commun, comme pour le lien. L'étape est non critique.
func (p *Provider) deployConfStep(proj *model.Project) *engine.Step {
	if _, err := os.Stat(p.Paths.DeployScript()); err != nil {
		return nil
	}
	root := proj.Spec.Layout.Root
	resource := "system/deploy-conf/" + proj.ID()
	path := filepath.Join(root, DeployConf)
	kind, _, err := fs.LinkStateAt(root, "", DeployConf)
	switch {
	case err != nil:
		s := engine.NewStep(resource, engine.Create, path+" — chemin refusé")
		s.Critical = false
		s.Apply = func(context.Context) error { return err }
		return s
	case kind != fs.EntryAbsent:
		return engine.NewStep(resource, engine.NoOp, path+" présent, tenu par le projet")
	}
	content := DeployConfContent(proj)
	s := engine.NewStep(resource, engine.Create, fmt.Sprintf("%s (FRAMEWORK=%s, BRANCH=%s)",
		path, DeployFramework(proj.Spec.App.Kind), DeployBranch(proj)))
	s.Critical = false
	s.Apply = func(context.Context) error {
		dir, err := fs.OpenDir(root)
		if err != nil {
			return err
		}
		defer dir.Close()
		// Création exclusive, sans suivre de lien : ce qui occupe le nom depuis le
		// plan est au projet et le reste.
		f, err := dir.Create(DeployConf, 0o600, p.linkOwner(proj))
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				return nil
			}
			return err
		}
		if _, err := f.Write(content); err != nil {
			f.Close()
			_ = dir.Remove(DeployConf)
			return err
		}
		if err := f.Close(); err != nil {
			_ = dir.Remove(DeployConf)
			return err
		}
		return nil
	}
	// La compensation ne retire que le fichier tel qu'ogs l'a écrit : retouché
	// entre-temps, il est au projet.
	s.Compensate = func(context.Context) error {
		dir, err := fs.OpenDir(root)
		if err != nil {
			return nil
		}
		defer dir.Close()
		if same, _ := sameDeployConf(dir, content); same {
			_ = dir.Remove(DeployConf)
		}
		return nil
	}
	return s
}

// sameDeployConf dit si le deploy.conf du répertoire ouvert porte exactement want.
// Le fichier est ouvert sans suivre de lien, sa taille bornée.
func sameDeployConf(dir *fs.Dir, want []byte) (bool, error) {
	f, err := dir.OpenFile(DeployConf)
	if err != nil {
		return false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxDeployConf+1))
	if err != nil {
		return false, err
	}
	return bytes.Equal(data, want), nil
}

// deployConfIsOurs dit si le deploy.conf de la racine ouverte rootFD est celui
// qu'ogs pose pour proj, octet pour octet : sans quoi il porte peut-être les hooks
// d'un ancien titulaire du home, que le nouveau compte exécuterait à son premier
// déploiement (D95).
func deployConfIsOurs(rootFD int, proj *model.Project) bool {
	fd, err := unix.Openat(rootFD, DeployConf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	f := os.NewFile(uintptr(fd), DeployConf)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Size > maxDeployConf {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxDeployConf+1))
	return err == nil && bytes.Equal(data, DeployConfContent(proj))
}
