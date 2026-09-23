package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"leblanc.io/open-go-server/internal/config"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// WebUser est le compte du serveur web, ajouté comme membre secondaire du groupe
// de chaque projet (D25). Il n'est jamais le groupe primaire d'un projet : cela
// rendrait chaque site lisible par tous les autres.
const WebUser = "www-data"

// groupdelNoSuchGroup est le code que rend groupdel quand le groupe n'existe pas.
const groupdelNoSuchGroup = 6

// Provider réconcilie l'utilisateur, le groupe, l'arborescence, la clé de
// déploiement et le lien d'interpréteur d'un projet.
type Provider struct {
	Paths    config.Paths
	Exec     exec.Executor
	Identity Identity
	// SkipWebUser désactive l'ajout de www-data au groupe, lorsque le compte
	// n'existe pas sur la machine (poste de développement, conteneur minimal).
	SkipWebUser bool
	// WebReload est l'intention de rechargement du serveur web retenu
	// (« web:apache », « web:nginx »). Le provider système ne choisit pas le
	// moteur : c'est l'assemblage qui le lui dit. L'intention nue « web », qu'il
	// émettait jusque-là, n'était routée nulle part — le rechargement n'avait pas
	// lieu, et chaque création de projet se soldait par un avertissement
	// invisible et le code 6.
	WebReload string
}

// Name identifie le provider.
func (p *Provider) Name() string { return "system" }

// Observed est l'état réel constaté.
type Observed struct {
	User      *UserInfo
	Group     *GroupInfo
	WebMember bool
	Dirs      map[string]DirState
}

// DirState est l'état d'un répertoire du projet.
type DirState struct {
	Exists bool
	// Symlink signale un lien symbolique posé à la place du répertoire. Il n'est
	// jamais suivi : sa cible a été choisie par l'utilisateur du projet.
	Symlink bool
	Mode    os.FileMode
	UID     int
	GID     int
}

// Observe inspecte le système pour un projet donné.
func (p *Provider) Observe(_ context.Context, proj *model.Project) (Observed, error) {
	obs := Observed{Dirs: map[string]DirState{}}
	u, err := p.Identity.LookupUser(proj.Spec.User.Name)
	if err != nil {
		return obs, fmt.Errorf("lecture du compte %s : %w", proj.Spec.User.Name, err)
	}
	obs.User = u
	g, err := p.Identity.LookupGroup(proj.Spec.User.Group)
	if err != nil {
		return obs, fmt.Errorf("lecture du groupe %s : %w", proj.Spec.User.Group, err)
	}
	obs.Group = g
	if g != nil {
		for _, m := range g.Members {
			if m == WebUser {
				obs.WebMember = true
			}
		}
	}
	for _, path := range p.paths(proj) {
		obs.Dirs[path] = statDir(path)
	}
	return obs, nil
}

// paths énumère la racine puis les sous-répertoires déclarés, dans l'ordre de
// création : un parent avant ses enfants.
func (p *Provider) paths(proj *model.Project) []string {
	out := []string{proj.Spec.Layout.Root}
	for _, d := range proj.Spec.Layout.Dirs {
		out = append(out, filepath.Join(proj.Spec.Layout.Root, d.Path))
	}
	return out
}

// statDir observe sans jamais suivre de lien. Les répertoires du projet vivent
// sous un home que son utilisateur écrit (0711, §7.1.2) : il peut retirer www/ ou
// var/ et poser à leur place un lien vers n'importe quel répertoire du système.
// os.Stat le résoudrait, et la correction qui suit poserait le mode et le
// propriétaire du projet sur la cible — une écriture root arbitraire, par le
// chemin même que la propriété root de logs/ ferme ailleurs. C'est la règle que
// « harden » applique déjà à /etc (D56).
func statDir(path string) DirState {
	info, err := os.Lstat(path)
	if err != nil {
		return DirState{}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return DirState{Exists: true, Symlink: true, UID: -1, GID: -1}
	}
	st := DirState{Exists: true, Mode: info.Mode().Perm(), UID: -1, GID: -1}
	if uid, gid, ok := OwnerOf(info); ok {
		st.UID, st.GID = uid, gid
	}
	return st
}

// Plan calcule les étapes nécessaires. L'ordre suit celui des dépendances :
// groupe, utilisateur, appartenance du serveur web, racine, sous-répertoires.
func (p *Provider) Plan(_ context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	var steps []*engine.Step
	id := proj.ID()
	spec := proj.Spec.User

	// 1. Groupe privé du projet.
	if obs.Group == nil {
		s := engine.NewStep("system/group/"+spec.Group, engine.Create,
			fmt.Sprintf("groupe %s", spec.Group))
		s.Apply = func(ctx context.Context) error {
			_, err := p.Exec.Run(ctx, exec.Command{Name: "groupadd", Args: []string{spec.Group}})
			return err
		}
		s.Compensate = func(ctx context.Context) error {
			// Le groupe a pu partir avec l'utilisateur, la compensation précédente
			// l'ayant emporté : userdel supprime le groupe primaire dès qu'il n'a plus
			// d'autre membre. Le code 6 de groupdel — « le groupe n'existe pas » — est
			// donc le résultat attendu, et non un échec : le rapporter ferait passer un
			// rollback réussi pour un rollback en échec, et enverrait vérifier un
			// système intact.
			res, err := p.Exec.Run(ctx, exec.Command{Name: "groupdel", Args: []string{spec.Group}})
			if err != nil && res.ExitCode == groupdelNoSuchGroup {
				return nil
			}
			return err
		}
		steps = append(steps, s)
	} else {
		steps = append(steps, engine.NewStep("system/group/"+spec.Group, engine.NoOp,
			fmt.Sprintf("groupe %s", spec.Group)))
	}

	// 2. Utilisateur du projet.
	switch {
	case obs.User == nil:
		s := engine.NewStep("system/user/"+spec.Name, engine.Create,
			fmt.Sprintf("utilisateur %s (home %s, shell %s)", spec.Name, spec.Home, spec.Shell))
		s.Apply = func(ctx context.Context) error {
			args := []string{
				"--gid", spec.Group,
				"--home-dir", spec.Home,
				"--shell", spec.Shell,
				"--no-create-home",
				"--comment", managedMarker + id,
			}
			if spec.UID != "" && spec.UID != "auto" {
				args = append(args, "--uid", spec.UID)
			}
			args = append(args, spec.Name)
			_, err := p.Exec.Run(ctx, exec.Command{Name: "useradd", Args: args})
			return err
		}
		s.Compensate = func(ctx context.Context) error {
			_, err := p.Exec.Run(ctx, exec.Command{Name: "userdel", Args: []string{spec.Name}})
			return err
		}
		steps = append(steps, s)
	case !obs.User.Managed && proj.Metadata.Labels["adopted"] != "true":
		// Le compte existe mais n'a pas été créé par ogs : on ne le modifie pas
		// en silence. L'adoption est une opération explicite (import), et un projet
		// qui en est issu reprend légitimement un compte non marqué.
		return nil, fmt.Errorf("l'utilisateur %s existe déjà et n'est pas géré par ogs "+
			"(champ commentaire sans marque %q) : l'adopter avec « ogs import » ou choisir un autre identifiant",
			spec.Name, managedMarker)
	default:
		// Home et shell sont corrigés par une seule étape : deux étapes portant la
		// même ressource se marcheraient dessus dans le plan comme dans l'audit, et
		// un seul « usermod » suffit à les poser toutes deux.
		var drifts []string
		var args, revert []string
		if obs.User.Home != spec.Home && obs.User.Home != "" {
			drifts = append(drifts, fmt.Sprintf("home %s → %s", obs.User.Home, spec.Home))
			args = append(args, "--home", spec.Home)
			revert = append(revert, "--home", obs.User.Home)
		}
		// Le shell déclaré est réconcilié : sans cela, ouvrir ou fermer l'accès d'un
		// projet existant demanderait un usermod à la main, et « spec.user.shell »
		// serait un état désiré que rien n'applique (D62).
		if obs.User.Shell != spec.Shell && obs.User.Shell != "" {
			drifts = append(drifts, fmt.Sprintf("shell %s → %s", obs.User.Shell, spec.Shell))
			args = append(args, "--shell", spec.Shell)
			revert = append(revert, "--shell", obs.User.Shell)
		}
		if len(drifts) == 0 {
			steps = append(steps, engine.NewStep("system/user/"+spec.Name, engine.NoOp,
				fmt.Sprintf("utilisateur %s", spec.Name)))
			break
		}
		s := engine.NewStep("system/user/"+spec.Name, engine.Update,
			fmt.Sprintf("utilisateur %s : %s", spec.Name, strings.Join(drifts, ", ")))
		s.Apply = func(ctx context.Context) error {
			_, err := p.Exec.Run(ctx, exec.Command{Name: "usermod",
				Args: append(append([]string{}, args...), spec.Name)})
			return err
		}
		s.Compensate = func(ctx context.Context) error {
			_, err := p.Exec.Run(ctx, exec.Command{Name: "usermod",
				Args: append(append([]string{}, revert...), spec.Name)})
			return err
		}
		steps = append(steps, s)
	}

	// 3. Appartenance du serveur web au groupe du projet.
	if spec.WebAccess == "group" && !p.SkipWebUser {
		if !obs.WebMember {
			s := engine.NewStep("system/group-member/"+spec.Group, engine.Create,
				fmt.Sprintf("%s membre du groupe %s", WebUser, spec.Group))
			s.Apply = func(ctx context.Context) error {
				_, err := p.Exec.Run(ctx, exec.Command{Name: "gpasswd",
					Args: []string{"-a", WebUser, spec.Group}})
				return err
			}
			s.Compensate = func(ctx context.Context) error {
				_, err := p.Exec.Run(ctx, exec.Command{Name: "gpasswd",
					Args: []string{"-d", WebUser, spec.Group}})
				return err
			}
			// Les workers rappellent initgroups() à chaque fork : un rechargement
			// suffit à prendre en compte la nouvelle appartenance.
			if p.WebReload != "" {
				s.Reload = []string{p.WebReload}
			}
			steps = append(steps, s)
		} else {
			steps = append(steps, engine.NewStep("system/group-member/"+spec.Group, engine.NoOp,
				fmt.Sprintf("%s membre du groupe %s", WebUser, spec.Group)))
		}
	}

	// 4. Arborescence.
	dirSteps, err := p.dirSteps(proj, obs)
	if err != nil {
		return nil, err
	}
	steps = append(steps, dirSteps...)

	// 5. Clé de déploiement et lien d'interpréteur, une fois l'arborescence en place.
	steps = append(steps, p.keyStep(proj))
	// 6. Clés autorisées, après la clé de déploiement : les deux vivent dans .ssh,
	// et l'ordre rend l'apply lisible plutôt qu'il ne crée une dépendance.
	if s := p.authorizedKeysStep(proj); s != nil {
		steps = append(steps, s)
	}
	if s := p.binStep(proj); s != nil {
		steps = append(steps, s)
	}
	return steps, nil
}

func (p *Provider) dirSteps(proj *model.Project, obs Observed) ([]*engine.Step, error) {
	rootMode, err := fs.ParseMode(proj.Spec.Layout.Mode)
	if err != nil {
		return nil, fmt.Errorf("spec.layout.mode : %w", err)
	}
	type target struct {
		path  string
		mode  os.FileMode
		owner string
	}
	targets := []target{{proj.Spec.Layout.Root, rootMode, ""}}
	for _, d := range proj.Spec.Layout.Dirs {
		mode, err := fs.ParseMode(d.Mode)
		if err != nil {
			return nil, fmt.Errorf("spec.layout.dirs[%s].mode : %w", d.Path, err)
		}
		targets = append(targets, target{filepath.Join(proj.Spec.Layout.Root, d.Path), mode, d.Owner})
	}

	// Le propriétaire attendu se lit dans l'état déjà observé, jamais par une
	// nouvelle résolution : sur une création de projet, le compte et le groupe
	// n'existent pas encore et ResolveOwner ferait échouer tout le plan, alors que
	// ce sont les premières étapes qui les créent. Sous préfixe, ensureDir laisse
	// le propriétaire en place (fs.KeepOwner) : comparer y ferait osciller le plan
	// sur un écart qu'aucune étape ne corrigerait.
	checkOwner := !p.Exec.Simulated() && obs.Group != nil

	var steps []*engine.Step
	for _, t := range targets {
		t := t
		state := obs.Dirs[t.path]
		// logs/ et .bin/ appartiennent à root : l'utilisateur ne doit pas pouvoir
		// y créer d'entrée, ce qui interdirait de substituer un fichier de log par
		// un lien symbolique et d'obtenir une écriture root arbitraire (§7.1.2).
		ownerLabel := proj.Spec.User.Name + ":" + proj.Spec.User.Group
		if t.owner == "root" {
			ownerLabel = "root:" + proj.Spec.User.Group
		}
		desired := fmt.Sprintf("%s (%04o %s)", t.path, t.mode.Perm(), ownerLabel)

		// Le propriétaire est du désiré au même titre que le mode : c'est lui, non
		// le 0710, qui interdit à l'utilisateur de créer une entrée dans logs/ et
		// .bin/. Un « chown -R » d'exploitation laisse le mode intact et rouvre la
		// substitution d'un journal par un lien symbolique, que le maître Apache
		// ouvre ensuite en root (§7.1.2) : sans cette comparaison, la dérive reste
		// « conforme » indéfiniment.
		wantUID, wantGID := -1, -1
		if checkOwner {
			wantGID = obs.Group.GID
			switch {
			case t.owner == "root":
				wantUID = 0
			case obs.User != nil:
				wantUID = obs.User.UID
			}
		}
		ownerDrift := state.Exists && !state.Symlink && wantUID >= 0 && state.UID >= 0 &&
			(state.UID != wantUID || state.GID != wantGID)

		switch {
		case state.Symlink:
			// Étape critique, et sans correction automatique : la suite du plan écrit
			// dans ce répertoire — docroot, sessions, clé de déploiement —, et la
			// cible du lien a été choisie par l'utilisateur du projet. Poser le mode
			// ou le propriétaire dessus, ou simplement poursuivre, reviendrait à lui
			// donner un répertoire du système.
			s := engine.NewStep("system/dir/"+t.path, engine.Update,
				fmt.Sprintf("répertoire %s : lien symbolique", t.path))
			s.Apply = func(context.Context) error {
				return fmt.Errorf("%s est un lien symbolique : ogs ne le suit pas et ne "+
					"corrige ni son mode ni son propriétaire ; le retirer avant de réappliquer",
					t.path)
			}
			steps = append(steps, s)
		case !state.Exists:
			s := engine.NewStep("system/dir/"+t.path, engine.Create, "répertoire "+desired)
			s.Apply = func(ctx context.Context) error { return p.ensureDir(proj, t.path, t.mode, t.owner) }
			s.Compensate = func(context.Context) error {
				// os.Remove échoue sur un répertoire non vide, et c'est voulu : un
				// chemin de compensation ne détruit jamais de données. Seuls les
				// répertoires restés vides sont retirés.
				_ = os.Remove(t.path)
				return nil
			}
			steps = append(steps, s)
		case state.Mode != t.mode.Perm() || ownerDrift:
			previous := state.Mode
			prevUID, prevGID := state.UID, state.GID
			var quoi []string
			if state.Mode != t.mode.Perm() {
				quoi = append(quoi, fmt.Sprintf("mode %04o → %04o", previous, t.mode.Perm()))
			}
			if ownerDrift {
				quoi = append(quoi, fmt.Sprintf("propriétaire %d:%d → %s (%d:%d)",
					prevUID, prevGID, ownerLabel, wantUID, wantGID))
			}
			s := engine.NewStep("system/dir/"+t.path, engine.Update,
				fmt.Sprintf("répertoire %s : %s", t.path, strings.Join(quoi, ", ")))
			s.Apply = func(ctx context.Context) error { return p.ensureDir(proj, t.path, t.mode, t.owner) }
			// Le propriétaire est reposé lui aussi : un rollback qui ne rendrait que
			// le mode laisserait le chown en place, donc la dérive corrigée à moitié.
			s.Compensate = func(context.Context) error {
				return restoreDir(t.path, previous, prevUID, prevGID)
			}
			steps = append(steps, s)
		default:
			steps = append(steps, engine.NewStep("system/dir/"+t.path, engine.NoOp, "répertoire "+t.path))
		}
	}
	return steps, nil
}

// restoreDir repose le mode et le propriétaire d'un répertoire sans jamais
// suivre de lien. Une compensation intervient après coup, et le plan a beau
// refuser un chemin lié, l'utilisateur du projet — qui écrit dans son home
// (0711, §7.1.2) — a pu substituer un lien entre l'apply et le rollback :
// os.Chmod et os.Chown le suivraient, et rendraient à la cible le mode et le
// propriétaire d'origine du répertoire, c'est-à-dire ceux du projet. Le mode et
// le propriétaire sont donc posés sur le descripteur, ouvert en O_NOFOLLOW.
func restoreDir(path string, mode os.FileMode, uid, gid int) error {
	d, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("ouverture de %s : %w", path, err)
	}
	defer d.Close()
	if err := d.Chmod(mode); err != nil {
		return err
	}
	if uid >= 0 && gid >= 0 {
		return d.Chown(uid, gid)
	}
	return nil
}

// ensureDir crée ou corrige un répertoire avec le propriétaire attendu.
func (p *Provider) ensureDir(proj *model.Project, path string, mode os.FileMode, owner string) error {
	target := fs.KeepOwner
	if !p.Exec.Simulated() {
		uid, gid, err := p.ResolveOwner(proj, owner)
		if err != nil {
			return err
		}
		target = fs.Owner{UID: uid, GID: gid}
	}
	return fs.EnsureDir(path, mode, target)
}

// ResolveOwner traduit le propriétaire attendu en identifiants numériques. Les
// autres providers en ont besoin pour créer des fichiers appartenant au projet.
func (p *Provider) ResolveOwner(proj *model.Project, owner string) (uid, gid int, err error) {
	g, err := p.Identity.LookupGroup(proj.Spec.User.Group)
	if err != nil {
		return -1, -1, err
	}
	if g == nil {
		return -1, -1, fmt.Errorf("groupe %s introuvable après création", proj.Spec.User.Group)
	}
	if owner == "root" {
		return 0, g.GID, nil
	}
	u, err := p.Identity.LookupUser(proj.Spec.User.Name)
	if err != nil {
		return -1, -1, err
	}
	if u == nil {
		return -1, -1, fmt.Errorf("utilisateur %s introuvable après création", proj.Spec.User.Name)
	}
	return u.UID, g.GID, nil
}

// nowFunc est isolé pour rester substituable en test.
var nowFunc = time.Now

// UID renvoie l'identifiant numérique d'un utilisateur, sous forme de chaîne.
func UID(u *UserInfo) string {
	if u == nil {
		return ""
	}
	return strconv.Itoa(u.UID)
}
