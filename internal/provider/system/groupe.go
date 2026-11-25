package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// GroupDirMode est le mode du répertoire d'un groupe de projets (D135). Il
// appartient à root:root, et www-data, qui n'est pas membre de root, relève des
// « autres » : la traversée lui suffit pour servir un docroot. Le 0711 du home
// (D51) tient à ce que www-data est membre du groupe du projet ; ici, personne
// d'autre que root n'énumère le groupe.
const GroupDirMode os.FileMode = 0o701

// groupDir rend le répertoire du groupe d'un projet, découpé en son parent — la
// racine des homes, à root — et son nom. ok est faux pour un projet sans
// groupe. Le répertoire se lit sur le home : validate.GroupPlacement a garanti
// au plan du projet visé qu'il est <racine des homes>/<groupe> ; une
// déclaration qui le contredirait est refusée ici aussi, plutôt que de créer
// un répertoire au mauvais endroit.
func groupDir(proj *model.Project) (parent, name string, ok bool, err error) {
	group := proj.Spec.Group
	if group == "" {
		return "", "", false, nil
	}
	dir := filepath.Dir(filepath.Clean(proj.Spec.Layout.Root))
	if filepath.Base(dir) != group {
		return "", "", false, fmt.Errorf("le home %s n'est pas sous le répertoire du groupe %q : le groupe "+
			"se fixe à la création (D135)", proj.Spec.Layout.Root, group)
	}
	return filepath.Dir(dir), group, true, nil
}

// observeGroupDir décrit le répertoire du groupe sans le suivre ni suivre de
// lien en chemin. Son parent, la racine des homes, est à root.
func observeGroupDir(proj *model.Project) (DirState, error) {
	parent, name, ok, err := groupDir(proj)
	if err != nil || !ok {
		return DirState{}, err
	}
	info, err := fs.LstatAt(parent, name)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return DirState{}, nil
	case errors.Is(err, fs.ErrNotDirectory):
		return DirState{Exists: true, LinkedParent: true, UID: -1, GID: -1}, nil
	case err != nil:
		return DirState{}, fmt.Errorf("lecture du répertoire du groupe %s : %w", filepath.Join(parent, name), err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return DirState{Exists: true, Symlink: true, UID: -1, GID: -1}, nil
	}
	if !info.IsDir() {
		return DirState{}, fmt.Errorf("%s existe et n'est pas un répertoire : il ne peut pas recevoir le "+
			"groupe %q (D135)", filepath.Join(parent, name), name)
	}
	st := DirState{Exists: true, Mode: info.Mode().Perm(), UID: -1, GID: -1}
	if uid, gid, ok := OwnerOf(info); ok {
		st.UID, st.GID = uid, gid
	}
	return st, nil
}

// PreexistingGroupDir refuse un répertoire de groupe que root ne possède pas
// (D135) : qui possède un répertoire en renomme les entrées sans privilège, et
// substituerait au home d'un membre du groupe un répertoire à lui. Aucune
// reprise n'est possible : le donner à root retirerait à son propriétaire ce
// qu'il y a posé, le laisser lui laisserait les homes. Sous préfixe, tout
// appartient à qui lance les tests : le contrôle se tait.
func (p *Provider) PreexistingGroupDir(proj *model.Project, obs Observed) error {
	if p.Exec.Simulated() || !obs.GroupDir.Exists || obs.GroupDir.Symlink || obs.GroupDir.LinkedParent {
		return nil
	}
	if obs.GroupDir.UID == 0 {
		return nil
	}
	parent, name, _, _ := groupDir(proj)
	dir := filepath.Join(parent, name)
	nameOf := p.AccountOfUID
	if nameOf == nil {
		nameOf = AccountOfUID
	}
	tenu := fmt.Sprintf("l'UID %d", obs.GroupDir.UID)
	if owner, err := nameOf(obs.GroupDir.UID); err == nil && owner != "" {
		tenu = fmt.Sprintf("le compte %s (UID %d)", owner, obs.GroupDir.UID)
	}
	return fmt.Errorf("le répertoire du groupe %q, %s, existe déjà et appartient à %s : un répertoire de groupe "+
		"est à root, sans quoi son propriétaire pourrait substituer le home d'un membre (D135) ; le rendre à "+
		"root:root s'il ne porte que des homes de projets, ou choisir un autre nom de groupe", name, dir, tenu)
}

// groupDirStep crée ou corrige le répertoire du groupe, root:root 0701, avant
// le home qu'il porte. L'apply en est le seul juge : « ogs harden » n'y touche
// pas, et sa règle « ne desserre jamais » ferait sinon osciller le plan avec un
// répertoire plus fermé. Un groupe différent de root se corrige aussi : un
// groupe dont www-data serait membre lui appliquerait les droits de groupe —
// nuls — et lui refuserait la traversée.
func (p *Provider) groupDirStep(proj *model.Project, obs Observed) (*engine.Step, error) {
	parent, name, ok, err := groupDir(proj)
	if err != nil || !ok {
		return nil, err
	}
	dir := filepath.Join(parent, name)
	id := "system/group-dir/" + name
	owner := fs.Owner{UID: 0, GID: 0}
	label := "root:root"
	if p.Exec.Simulated() {
		owner = fs.KeepOwner
	}
	state := obs.GroupDir
	switch {
	case state.Symlink || state.LinkedParent:
		s := engine.NewStep(id, engine.Update, fmt.Sprintf("répertoire du groupe %s : lien symbolique", dir))
		s.Apply = func(context.Context) error {
			return fmt.Errorf("%s n'est pas un répertoire véritable (lien symbolique ?) : ogs ne le suit pas ; "+
				"le retirer avant de réappliquer", dir)
		}
		return s, nil
	case !state.Exists:
		s := engine.NewStep(id, engine.Create, fmt.Sprintf("répertoire du groupe %s (%04o %s)", dir,
			GroupDirMode, label))
		s.Apply = func(context.Context) error {
			// La racine des homes existe sur toute machine ; sous préfixe, le premier
			// projet la crée, comme le faisait la création du home par son nom. Au
			// régime que lui donnerait « ogs harden » (D60), et jamais retouchée.
			if _, err := os.Lstat(parent); errors.Is(err, os.ErrNotExist) {
				if err := os.MkdirAll(parent, 0o711); err != nil {
					return err
				}
			}
			return fs.EnsureDirAt(parent, name, GroupDirMode, owner)
		}
		s.Compensate = func(context.Context) error {
			// Seulement vide : un autre membre a pu y poser son home entre-temps.
			_, _ = fs.RemoveEmptyDirAt(parent, name)
			return nil
		}
		return s, nil
	}
	ownerDrift := !p.Exec.Simulated() && state.GID > 0
	if state.Mode == GroupDirMode && !ownerDrift {
		return engine.NewStep(id, engine.NoOp, "répertoire du groupe "+dir), nil
	}
	var quoi string
	if state.Mode != GroupDirMode {
		quoi = fmt.Sprintf("mode %04o → %04o", state.Mode, GroupDirMode)
	}
	if ownerDrift {
		if quoi != "" {
			quoi += ", "
		}
		quoi += fmt.Sprintf("propriétaire %d:%d → %s", state.UID, state.GID, label)
	}
	previous, prevUID, prevGID := state.Mode, state.UID, state.GID
	s := engine.NewStep(id, engine.Update, fmt.Sprintf("répertoire du groupe %s : %s", dir, quoi))
	s.Apply = func(context.Context) error { return fs.SetDirAt(parent, name, GroupDirMode, owner) }
	s.Compensate = func(context.Context) error { return restoreDir(parent, name, previous, prevUID, prevGID) }
	return s, nil
}
