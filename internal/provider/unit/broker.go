package unit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/provider/removal"
)

// BrokerHookName est l'unité qui rattache les consommateurs du parc au broker
// local (D137) : son arrêt lance « ogs amqp stop », son démarrage « ogs amqp
// start ».
const BrokerHookName = "ogs-amqp.service"

// BrokerUnit est l'unité du broker RabbitMQ que livre la distribution.
const BrokerUnit = "rabbitmq-server.service"

// brokerHookResource est la ressource que porte l'en-tête de l'unité.
const brokerHookResource = "unit/broker-hook"

// unitDirs sont les répertoires où systemd cherche une unité installée.
var unitDirs = []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"}

// BrokerInstalled dit si le broker est installé sur cette machine. Le critère est
// le fichier d'unité, non l'état : un broker arrêté pour sa mise à jour reste
// celui de la machine. Un broker distant (amqp.yaml vers une autre machine)
// n'a pas d'unité ici, et rien ne s'y rattache.
func (p *Provider) BrokerInstalled() bool {
	for _, dir := range unitDirs {
		if _, err := os.Stat(filepath.Join(p.Paths.Rooted(dir), BrokerUnit)); err == nil {
			return true
		}
	}
	return false
}

func (p *Provider) brokerHookPath() string {
	return filepath.Join(p.Paths.SystemdDir(), BrokerHookName)
}

// brokerWantsLink est le lien d'activation que pose « systemctl enable ».
func (p *Provider) brokerWantsLink() string {
	return filepath.Join(p.Paths.SystemdDir(), BrokerUnit+".wants", BrokerHookName)
}

// BrokerHookPlan écrit l'unité ogs-amqp.service et l'active quand le broker est
// installé sur la machine, et la retire sinon (D137). C'est une ressource du
// parc, calculée par le plan global : elle ne dépend d'aucun projet — sans
// consommateur déclaré, « ogs amqp stop|start » ne fait rien.
//
// L'activation (enable --now) démarre l'unité, donc lance une fois « ogs amqp
// start » : une unité inactive ne jouerait pas son ExecStop à l'arrêt du broker.
// Une unité installée mais désactivée est signalée, jamais réactivée : c'est un
// choix de l'administrateur (D105) ; arrêtée, elle est laissée — « systemctl stop
// ogs-amqp » est un arrêt des consommateurs, que l'apply ne défait pas.
func (p *Provider) BrokerHookPlan(binary string) ([]*engine.Step, error) {
	path := p.brokerHookPath()
	if !p.BrokerInstalled() {
		step := removal.File{
			Resource: brokerHookResource,
			Summary:  "unité " + BrokerHookName + " — broker absent de la machine",
			Path:     path,
			Force:    p.Force,
			Backups:  p.Paths.BackupsDir(),
			Now:      p.now,
			Reload:   []string{"systemd:daemon-reload"},
			Links: func() []string {
				link := p.brokerWantsLink()
				if _, err := os.Lstat(link); err == nil {
					return []string{link}
				}
				return nil
			},
			Owns: func(resource string) bool { return resource == brokerHookResource },
		}
		// Rien d'ogs à retirer : un fichier absent, ou qui ne porte pas la marque
		// de cette unité — celui d'un administrateur reste, sans un mot.
		if _, resource, err := fs.ManagedResource(path); err != nil || resource != brokerHookResource {
			return nil, nil
		}
		s, err := step.Step()
		if err != nil || s == nil {
			return nil, err
		}
		if s.Action == engine.Delete {
			// Le répertoire .wants du broker part avec le lien, s'il est vide —
			// ce que ferait « systemctl disable ».
			remove := s.Apply
			s.Apply = func(ctx context.Context) error {
				if err := remove(ctx); err != nil {
					return err
				}
				_ = os.Remove(filepath.Dir(p.brokerWantsLink()))
				return nil
			}
		}
		return []*engine.Step{s}, nil
	}

	tmpl := "systemd/" + BrokerHookName
	body, err := p.Templates.Render(tmpl, map[string]any{"Binary": binary, "Broker": BrokerUnit})
	if err != nil {
		return nil, err
	}
	content := fs.Render(fs.Header{
		Project:   "*",
		Resource:  brokerHookResource,
		Generated: p.now(),
		Template:  p.Templates.OverridePath(tmpl),
	}, body, "#")
	s, err := p.fileStepFor("*", brokerHookResource, path, content, "unité "+BrokerHookName)
	if err != nil {
		return nil, err
	}
	if s != nil {
		if s.Mutates() && len(s.Reload) > 0 {
			// Une unité oneshot ne se redémarre pas pour prendre sa nouvelle
			// définition : daemon-reload suffit, et l'intention de cible d'un
			// projet n'a pas de sens pour une unité du parc.
			s.Reload = []string{"systemd:daemon-reload", "systemd:enable:" + BrokerHookName}
		}
		return []*engine.Step{s}, nil
	}
	if p.BrokerHookProblem(context.Background()) == "désactivée" {
		w := engine.NewStep(brokerHookResource+"/activation", engine.Update,
			"unité "+BrokerHookName+" désactivée — un arrêt du broker n'arrête pas les consommateurs")
		w.Critical = false
		w.Apply = func(context.Context) error {
			return fmt.Errorf("%s est désactivée et ogs ne la réactive pas : « systemctl enable --now %s » "+
				"si elle ne l'a pas été volontairement", BrokerHookName, BrokerHookName)
		}
		return []*engine.Step{w}, nil
	}
	return nil, nil
}

// BrokerHookProblem rend l'état d'une unité ogs-amqp.service installée qui ne
// jouera pas son rôle — « désactivée », ou « inactive » alors que le broker
// tourne —, vide sinon, si elle est absente ou si rien n'est observable. doctor
// le porte.
func (p *Provider) BrokerHookProblem(ctx context.Context) string {
	if p.Exec == nil || p.Exec.Simulated() {
		return ""
	}
	if _, err := os.Stat(p.brokerHookPath()); err != nil {
		return ""
	}
	quiet := func(verb, unit string) bool {
		_, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{verb, "--quiet", unit}})
		return err == nil
	}
	switch {
	case !quiet("is-enabled", BrokerHookName):
		return "désactivée"
	case systemdBooted() && quiet("is-active", BrokerUnit) && !quiet("is-active", BrokerHookName):
		return "inactive"
	}
	return ""
}

// systemdBooted dit si systemd est le gestionnaire du système (sd_booted).
var systemdBooted = func() bool {
	info, err := os.Stat("/run/systemd/system")
	return err == nil && info.IsDir()
}
