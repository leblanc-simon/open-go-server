package monitoring

import (
	"fmt"
	"path"
	"sort"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// PoolInput est un pool PHP-FPM et l'URL où lire son état.
type PoolInput struct {
	Project string
	Name    string
	Version string
	URL     string
}

// ProcInput est un projet et l'utilisateur dont les processus sont comptés.
// Interroger par utilisateur plutôt que par unité systemd est un choix : il
// couvre du même geste les workers PHP-FPM du projet, ses services déclarés et
// son application supervisée, sans avoir à énumérer des instances dont le nombre
// change.
type ProcInput struct {
	Project string
	User    string
}

// WebInput décrit l'entrée du moteur web.
type WebInput struct {
	Kind string // apache | nginx
	URL  string
}

// TelegrafData est le contexte du fragment d'entrées.
type TelegrafData struct {
	Interval string
	Web      *WebInput
	Pools    []PoolInput
	Procs    []ProcInput
	MySQL    *MySQLAccess
	Postgres []PostgresAccess
	Rabbit   *RabbitAccess
}

// BuildTelegraf compose le contexte du fragment à partir du parc.
func (p *Provider) BuildTelegraf(projects []*model.Project, creds Credentials) (TelegrafData, error) {
	mon := p.Cfg.Monitoring
	data := TelegrafData{Interval: mon.TelegrafInterval()}

	base := "http://" + mon.StatusListen() + mon.StatusPath()
	if mon.InputEnabled("web") && p.WebEngine != "" {
		url := base + "/" + p.WebEngine
		if p.WebEngine == "apache" {
			// mod_status ne rend un format lisible par une machine qu'avec « ?auto » :
			// sans lui, l'agent reçoit une page HTML et n'en tire rien.
			url += "?auto"
		}
		data.Web = &WebInput{Kind: p.WebEngine, URL: url}
	}

	for _, proj := range projects {
		if mon.InputEnabled("procstat") && proj.Spec.User.Name != "" {
			data.Procs = append(data.Procs, ProcInput{Project: proj.ID(), User: proj.Spec.User.Name})
		}
		if !mon.InputEnabled("phpfpm") || p.PHP == nil {
			continue
		}
		at, err := proj.AppType()
		if err != nil {
			return data, err
		}
		if !at.NeedsPHP {
			continue
		}
		for _, pool := range proj.Spec.PHP.Pools {
			bp, err := p.PHP.Build(proj, pool)
			if err != nil {
				// Un pool qu'ogs ne sait pas calculer est déjà signalé par le plan du
				// projet : la supervision n'a pas à le redire, ni à échouer pour lui.
				continue
			}
			data.Pools = append(data.Pools, PoolInput{
				Project: proj.ID(), Name: bp.Name, Version: bp.Version,
				URL: base + "/fpm/" + bp.Name,
			})
		}
	}
	sort.Slice(data.Pools, func(i, j int) bool { return data.Pools[i].Name < data.Pools[j].Name })
	sort.Slice(data.Procs, func(i, j int) bool { return data.Procs[i].Project < data.Procs[j].Project })

	data.MySQL = creds.MySQL
	data.Rabbit = creds.RabbitMQ
	for _, pg := range creds.Postgres {
		if pg.Socket == "" {
			continue
		}
		data.Postgres = append(data.Postgres, pg)
	}
	return data, nil
}

// planTelegraf produit l'étape d'écriture du fragment d'entrées.
//
// Le fichier porte le mot de passe du compte MySQL et celui du broker : il est
// écrit en 0640, groupe de l'agent. C'est le même régime que celui qu'« ogs
// harden » impose à /etc/telegraf (D56), et pour la même raison.
func (p *Provider) planTelegraf(projects []*model.Project, creds Credentials, obs Observed) (*engine.Step, error) {
	data, err := p.BuildTelegraf(projects, creds)
	if err != nil {
		return nil, err
	}
	body, err := p.Templates.Render("telegraf/ogs.conf", map[string]any{
		"Interval": data.Interval,
		"Web":      data.Web,
		"Pools":    data.Pools,
		"Procs":    data.Procs,
		"MySQL":    data.MySQL,
		"Postgres": data.Postgres,
		"Rabbit":   data.Rabbit,
	})
	if err != nil {
		return nil, err
	}
	content := fs.Render(p.header("monitoring/telegraf", "telegraf/ogs.conf", len(projects)), body, "#")

	owner := fs.KeepOwner
	if obs.TelegrafGroupOK {
		owner = fs.Owner{UID: 0, GID: obs.TelegrafGroup}
	}
	summary := fmt.Sprintf("entrées Telegraf (%d pool(s), %d projet(s))", len(data.Pools), len(projects))
	return p.fileStep("monitoring/telegraf", summary, p.Paths.TelegrafFile(),
		content, 0o640, owner, "monitoring:telegraf")
}

// PoolStatusPath expose le chemin de statut attendu dans un pool, pour que le
// provider PHP et le vhost de supervision s'accordent sans se connaître.
func PoolStatusPath() string { return config.PoolStatusPath }

// StatusURLFor compose l'URL de statut d'un pool. Elle sert aux contrôles de
// « doctor », qui doit interroger exactement ce que l'agent interroge.
func StatusURLFor(cfg *config.Config, pool string) string {
	return "http://" + cfg.Monitoring.StatusListen() + path.Join(cfg.Monitoring.StatusPath(), "fpm", pool)
}
