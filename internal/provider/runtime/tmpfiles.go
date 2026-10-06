// Package runtime génère les répertoires d'exécution des projets.
package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/php"
)

// ProjectRunMode est le mode du répertoire d'exécution d'un projet : le projet y
// écrit ses verrous et ses sockets, le groupe traverse jusqu'au socket qu'il
// connaît sans pouvoir énumérer le reste, personne d'autre n'entre (D51). Il est
// nommé ici, une seule fois : la ligne tmpfiles et la création immédiate ont
// divergé, et un répertoire dont deux endroits décident du mode oscille.
const ProjectRunMode os.FileMode = 0o710

// RunRootMode est le mode de /run/ogs : traversable par tous, sans quoi personne
// n'atteindrait le répertoire d'un projet.
const RunRootMode os.FileMode = 0o755

// ProjectRunOwner résout le propriétaire du répertoire d'exécution
// (fs.LookupOwner).
func ProjectRunOwner(proj *model.Project) fs.Owner {
	return fs.LookupOwner(proj.Spec.User.Name, proj.Spec.User.Group)
}

// EnsureProjectRunDir pose /run/ogs/<id> avec le mode et le propriétaire que
// déclare le fichier tmpfiles. Le laisser à root:root, comme le faisait la
// création immédiate, interdit au projet d'y lier son socket et d'y poser les
// verrous de ses tâches.
func EnsureProjectRunDir(path string, proj *model.Project) error {
	// La racine d'abord, et à son mode à elle : MkdirAll créerait sinon /run/ogs
	// en 0710 root:root, où plus personne ne passerait.
	if err := fs.EnsureDir(filepath.Dir(path), RunRootMode, fs.KeepOwner); err != nil {
		return err
	}
	return fs.EnsureDir(path, ProjectRunMode, ProjectRunOwner(proj))
}

// Provider génère /etc/tmpfiles.d/ogs.conf : les répertoires de /run disparaissent
// au redémarrage, et c'est systemd-tmpfiles qui les recrée.
type Provider struct {
	Paths config.Paths
	// Config donne le gc_maxlifetime des pools, d'où l'âge de la purge des
	// sessions (D115). nil vaut la configuration livrée.
	Config *config.Config
	Now    func() time.Time
}

// Name identifie le provider.
func (p *Provider) Name() string { return "runtime" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Build calcule le contenu attendu du fichier tmpfiles.
func (p *Provider) Build(projects []*model.Project) []byte {
	var b strings.Builder
	b.WriteString("# Répertoires d'exécution : verrous de tâches et sockets applicatifs.\n")
	b.WriteString("# Type Chemin Mode Propriétaire Groupe Âge\n")
	fmt.Fprintf(&b, "d %s %04o root root -\n", p.Paths.RunDir(), RunRootMode.Perm())
	ids := make([]string, 0, len(projects))
	owners := map[string]string{}
	for _, proj := range projects {
		ids = append(ids, proj.ID())
		owners[proj.ID()] = proj.Spec.User.Name + " " + proj.Spec.User.Group
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(&b, "d %s %04o %s -\n", p.Paths.ProjectRunDir(id), ProjectRunMode.Perm(), owners[id])
	}
	// Sessions PHP (D115), purgées selon leur âge : Debian n'en confie la purge
	// qu'à sessionclean, qui ne connaît que le save_path du php.ini. La chaîne
	// est entièrement root jusqu'au répertoire du projet : systemd-tmpfiles
	// résout les parents par leur nom, et sous le home, l'utilisateur lui
	// désignait le répertoire « sessions » de son choix.
	sessions := p.sessionLines(projects)
	if len(sessions) > 0 {
		b.WriteString("# Sessions PHP, purgées au-delà du session.gc_maxlifetime effectif des pools.\n")
		fmt.Fprintf(&b, "d %s %04o root root -\n", p.Paths.SessionsRoot(), php.SessionsRootMode.Perm())
		for _, line := range sessions {
			b.WriteString(line)
		}
	}
	return fs.Render(fs.Header{
		Project:   fmt.Sprintf("%d projet(s)", len(projects)),
		Resource:  "runtime/tmpfiles",
		Generated: p.now(),
	}, []byte(b.String()), "#")
}

// sessionLines rend la ligne de sessions de chaque projet PHP, triée.
func (p *Provider) sessionLines(projects []*model.Project) []string {
	var out []string
	for _, proj := range projects {
		age, isPHP := php.SessionLifetime(p.Config, p.Paths, proj)
		if !isPHP {
			continue
		}
		purge := "-"
		if age > 0 {
			purge = fmt.Sprintf("%ds", age)
		}
		out = append(out, fmt.Sprintf("d %s %04o %s %s %s\n", p.Paths.ProjectSessionsDir(proj.ID()),
			php.SessionsDirMode.Perm(), proj.Spec.User.Name, proj.Spec.User.Group, purge))
	}
	sort.Strings(out)
	return out
}

// Plan renvoie l'étape de mise à jour du fichier tmpfiles.
func (p *Provider) Plan(projects []*model.Project) (*engine.Step, error) {
	content := p.Build(projects)
	path := p.Paths.TmpfilesFile()
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	if state == fs.StateCurrent {
		return engine.NewStep("runtime/tmpfiles", engine.NoOp, "répertoires d'exécution"), nil
	}
	action := engine.Create
	if state != fs.StateAbsent {
		action = engine.Update
	}
	s := engine.NewStep("runtime/tmpfiles", action,
		fmt.Sprintf("répertoires d'exécution (%d projet(s))", len(projects)))
	var previous []byte
	s.Apply = func(context.Context) error {
		if data, err := os.ReadFile(path); err == nil {
			previous = data
		}
		if err := fs.EnsureDir(filepath.Dir(path), 0o755, fs.KeepOwner); err != nil {
			return err
		}
		if err := fs.WriteAtomic(path, content, 0o644, fs.KeepOwner); err != nil {
			return err
		}
		// Les répertoires sont créés immédiatement, sans attendre le prochain
		// démarrage : les tâches et les services en ont besoin maintenant.
		return p.ensureNow(projects)
	}
	s.Compensate = func(context.Context) error {
		if previous == nil {
			return os.Remove(path)
		}
		return fs.WriteAtomic(path, previous, 0o644, fs.KeepOwner)
	}
	s.Reload = []string{"systemd:tmpfiles"}
	return s, nil
}

// ensureNow crée les répertoires sans passer par systemd-tmpfiles.
func (p *Provider) ensureNow(projects []*model.Project) error {
	if err := fs.EnsureDir(p.Paths.RunDir(), RunRootMode, fs.KeepOwner); err != nil {
		return err
	}
	for _, proj := range projects {
		if err := EnsureProjectRunDir(p.Paths.ProjectRunDir(proj.ID()), proj); err != nil {
			return err
		}
		if _, isPHP := php.SessionLifetime(p.Config, p.Paths, proj); isPHP {
			if err := php.EnsureSessionsDir(p.Paths, proj); err != nil {
				return err
			}
		}
	}
	return nil
}
