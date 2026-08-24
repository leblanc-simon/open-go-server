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

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/fs"
	"github.com/leviathan/ogs/internal/model"
)

// Provider génère /etc/tmpfiles.d/ogs.conf : les répertoires de /run disparaissent
// au redémarrage, et c'est systemd-tmpfiles qui les recrée.
type Provider struct {
	Paths config.Paths
	Now   func() time.Time
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
	fmt.Fprintf(&b, "d %s 0755 root root -\n", p.Paths.RunDir())
	ids := make([]string, 0, len(projects))
	owners := map[string]string{}
	for _, proj := range projects {
		ids = append(ids, proj.ID())
		owners[proj.ID()] = proj.Spec.User.Name + " " + proj.Spec.User.Group
	}
	sort.Strings(ids)
	for _, id := range ids {
		// 0710 : le projet écrit ses verrous et ses sockets, le groupe traverse
		// jusqu'au socket qu'il connaît sans pouvoir énumérer le reste, personne
		// d'autre n'entre (D51).
		fmt.Fprintf(&b, "d %s 0710 %s -\n", p.Paths.ProjectRunDir(id), owners[id])
	}
	return fs.Render(fs.Header{
		Project:   fmt.Sprintf("%d projet(s)", len(projects)),
		Resource:  "runtime/tmpfiles",
		Generated: p.now(),
	}, []byte(b.String()), "#")
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
	if err := fs.EnsureDir(p.Paths.RunDir(), 0o755, fs.KeepOwner); err != nil {
		return err
	}
	for _, proj := range projects {
		if err := fs.EnsureDir(p.Paths.ProjectRunDir(proj.ID()), 0o750, fs.KeepOwner); err != nil {
			return err
		}
	}
	return nil
}
