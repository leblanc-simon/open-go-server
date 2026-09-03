package web

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// StatusVHostName est le nom du vhost de supervision. Il ne porte pas
// d'identifiant de projet : il est de portée serveur, comme le fragment de
// réglages.
const StatusVHostName = "ogs-status"

// StatusPool est un pool exposé par le vhost de supervision.
type StatusPool struct {
	Name    string
	Project string
	// Pass est la référence du point d'écoute FPM, dans la syntaxe du moteur.
	Pass string
}

// PlanStatus produit le vhost de supervision : un point d'écoute local qui
// expose l'état du moteur web et celui de chaque pool PHP-FPM du parc.
//
// Ce vhost existe pour une raison précise. L'agent de métriques a besoin de
// l'état des pools ; le seul autre moyen de l'obtenir est de lui donner accès
// aux sockets FPM. Or **parler au socket d'un pool, c'est pouvoir y faire
// exécuter du code sous l'identité du projet** : le protocole FastCGI n'a aucune
// authentification, et le pool exécute le script qu'on lui nomme. Donner ces
// sockets à l'agent ferait de lui un moyen d'exécution sur tous les projets à la
// fois. Le serveur web, lui, les a déjà : il sert d'intermédiaire, et l'agent
// n'obtient que ce que le pool veut bien rendre sur son chemin de statut.
//
// Le vhost n'écoute que sur la boucle locale — la configuration refuse toute
// autre adresse — et refuse tout ce qui n'est pas un chemin de statut.
func (p *Provider) PlanStatus(ctx context.Context, projects []*model.Project,
	listen, prefix string, enabled bool) ([]*engine.Step, error) {

	name := StatusVHostName
	filePath := path.Join(p.Engine.AvailableDir(), p.Engine.FileName(name))
	enabledPath := path.Join(p.Engine.EnabledDir(), p.Engine.FileName(name))

	if !enabled {
		if _, err := os.Stat(filePath); err != nil {
			return nil, nil
		}
		s := engine.NewStep("web/status", engine.Delete,
			"retrait du vhost de supervision "+name)
		s.Apply = func(context.Context) error {
			_ = os.Remove(enabledPath)
			if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		s.Reload = []string{"web:" + p.Engine.Name()}
		return []*engine.Step{s}, nil
	}

	pools, err := p.statusPools(projects)
	if err != nil {
		return nil, err
	}
	tmplName := p.Engine.TemplateName("status")
	body, err := p.Templates.Render(tmplName, map[string]any{
		"Listen":     listen,
		"Prefix":     prefix,
		"Engine":     p.Engine.Name(),
		"Pools":      pools,
		"PoolStatus": config.PoolStatusPath,
		"PoolPing":   config.PoolPingPath,
	})
	if err != nil {
		return nil, err
	}
	content := fs.Render(fs.Header{
		Project: "parc", Resource: "web/status", Generated: p.now(),
		Template: p.Templates.OverridePath(tmplName),
	}, body, p.Engine.CommentPrefix())

	state, err := fs.Inspect(filePath, content)
	if err != nil {
		return nil, err
	}
	_, linkErr := os.Lstat(enabledPath)
	isEnabled := linkErr == nil

	summary := fmt.Sprintf("vhost de supervision %s sur %s (%d pool(s))", name, listen, len(pools))
	var steps []*engine.Step
	if state == fs.StateCurrent {
		steps = append(steps, engine.NewStep("web/status", engine.NoOp, summary))
	} else {
		action := engine.Create
		if state != fs.StateAbsent {
			action = engine.Update
		}
		s := engine.NewStep("web/status", action, summary)
		s.Diff = func() (string, error) { return diffText(filePath, content) }
		s.Apply = func(ctx context.Context) error {
			previous, hadPrevious := os.ReadFile(filePath)
			if err := fs.WriteAtomic(filePath, content, 0o644, fs.KeepOwner); err != nil {
				return err
			}
			// Même précaution que pour un vhost de projet : une configuration
			// invalide écrite ici arrête tout le serveur au rechargement suivant.
			if err := validate(ctx, p.Engine, p.Exec); err != nil {
				if hadPrevious == nil {
					_ = fs.WriteAtomic(filePath, previous, 0o644, fs.KeepOwner)
				} else {
					_ = os.Remove(filePath)
				}
				return err
			}
			return nil
		}
		s.Compensate = func(context.Context) error { return os.Remove(filePath) }
		s.Reload = []string{"web:" + p.Engine.Name()}
		steps = append(steps, s)
	}

	if !isEnabled {
		s := engine.NewStep("web/status/enabled", engine.Create,
			"activation du vhost de supervision")
		s.Apply = func(ctx context.Context) error {
			return p.enable(ctx, &VHostPlan{Path: filePath, EnabledPath: enabledPath})
		}
		s.Compensate = func(context.Context) error { return os.Remove(enabledPath) }
		s.Reload = []string{"web:" + p.Engine.Name()}
		steps = append(steps, s)
	} else {
		steps = append(steps, engine.NewStep("web/status/enabled", engine.NoOp,
			"vhost de supervision actif"))
	}
	return steps, nil
}

// statusPools énumère les pools du parc et la référence de leur point d'écoute.
func (p *Provider) statusPools(projects []*model.Project) ([]StatusPool, error) {
	var pools []StatusPool
	if p.PHP == nil {
		return nil, nil
	}
	for _, proj := range projects {
		at, err := proj.AppType()
		if err != nil {
			return nil, err
		}
		if !at.NeedsPHP {
			continue
		}
		for _, pool := range proj.Spec.PHP.Pools {
			bp, err := p.PHP.Build(proj, pool)
			if err != nil {
				// Un pool qu'ogs ne sait pas calculer est déjà signalé par le plan du
				// projet : le vhost de supervision n'a pas à échouer pour lui.
				continue
			}
			pools = append(pools, StatusPool{
				Name: bp.Name, Project: proj.ID(),
				Pass: p.Engine.FPMReference(bp.Listen, bp.Unix),
			})
		}
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })
	return pools, nil
}
