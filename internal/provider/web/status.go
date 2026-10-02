package web

import (
	"context"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/removal"
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
	// Status et Ping sont les chemins d'état *effectifs* de ce pool. Ils ne sont
	// pas ceux du parc : « pm.status_path » et « ping.path » sont des réglages de
	// base, qu'un projet surcharge par spec.php.pools[].settings. Or FPM compare
	// SCRIPT_NAME à la valeur qu'il a retenue, lui — le vhost force cette variable
	// (D66), il doit donc forcer le bon chemin, sans quoi le pool cherche un
	// fichier et répond « Primary script unknown » à chaque collecte.
	Status string
	Ping   string
}

// reCheminEtat reconnaît un chemin d'état écrivable tel quel dans un vhost.
// Le contrôle n'est pas une coquetterie : la valeur vient du fichier d'un
// projet, et le vhost de supervision est de portée serveur. Un point-virgule,
// une espace ou un saut de ligne y ajouterait une directive — et sur nginx une
// directive dupliquée est fatale, donc un seul projet abattrait le moteur.
var reCheminEtat = regexp.MustCompile(`^/[A-Za-z0-9._~/-]{1,64}$`)

// cheminEtat rend le chemin d'état retenu par le pool, ou le défaut du parc.
//
// Une valeur qu'on ne saurait pas écrire sans risque retombe sur le défaut :
// une métrique manquante pour ce pool — l'état d'avant ce contrôle — vaut mieux
// qu'une directive arbitraire dans un vhost que le moteur charge en root.
func cheminEtat(settings map[string]string, clef, defaut string) string {
	v := settings[clef]
	if v == "" || !reCheminEtat.MatchString(v) {
		return defaut
	}
	return v
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
		// Même régime que les vhosts d'un projet : retouché, il n'est retiré que
		// sous --force et sauvegardé ; sans la marque d'ogs, il n'est pas à lui.
		s, err := removal.File{
			Resource: "web/status",
			Summary:  "retrait du vhost de supervision " + name,
			Path:     filePath,
			Project:  "parc",
			Force:    p.Force,
			Backups:  p.Paths.BackupsDir(),
			Now:      p.now,
			Links:    func() []string { return p.linksTo(filePath) },
			Reload:   []string{"web:" + p.Engine.Name()},
		}.Step()
		if err != nil || s == nil {
			return nil, err
		}
		return []*engine.Step{s}, nil
	}

	pools, err := p.statusPools(projects)
	if err != nil {
		return nil, err
	}
	tmplName := p.Engine.TemplateName("status")
	body, err := p.Templates.Render(tmplName, map[string]any{
		"Listen": listen,
		"Prefix": prefix,
		"Engine": p.Engine.Name(),
		"Pools":  pools,
		// Les chemins du parc restent offerts au contexte alors que les templates
		// livrés emploient désormais ceux du pool : les templates sont
		// surchargeables, et une surcharge écrite avant eux les emploie encore.
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
		// Corps précédent, relevé à l'application : l'état du disque a pu changer
		// depuis le plan.
		var previous []byte
		var hadPrevious bool
		s.Apply = func(ctx context.Context) error {
			if data, err := os.ReadFile(filePath); err == nil {
				previous, hadPrevious = data, true
			}
			if err := fs.WriteAtomic(filePath, content, 0o644, fs.KeepOwner); err != nil {
				return err
			}
			// Même précaution que pour un vhost de projet : une configuration
			// invalide écrite ici arrête tout le serveur au rechargement suivant.
			if err := validate(ctx, p.Engine, p.Exec); err != nil {
				if hadPrevious {
					_ = fs.WriteAtomic(filePath, previous, 0o644, fs.KeepOwner)
				} else {
					_ = os.Remove(filePath)
				}
				return err
			}
			return nil
		}
		// Comme pour un vhost de projet : la compensation d'une mise à jour rend le
		// contenu antérieur. Le supprimer laisserait le lien d'activation — déjà
		// posé, donc jamais compensé — pointer dans le vide, et ce vhost-ci est de
		// portée serveur : nginx refuserait alors de démarrer pour tout le parc.
		s.Compensate = func(context.Context) error {
			if hadPrevious {
				return fs.WriteAtomic(filePath, previous, 0o644, fs.KeepOwner)
			}
			// Un fichier déjà absent n'est pas une erreur : une compensation en
			// échec arrête tout le déroulage.
			if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
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
				Pass:   p.Engine.FPMReference(bp.Listen, bp.Unix),
				Status: cheminEtat(bp.Resolved.Settings, "pm.status_path", config.PoolStatusPath),
				Ping:   cheminEtat(bp.Resolved.Settings, "ping.path", config.PoolPingPath),
			})
		}
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })
	return pools, nil
}
