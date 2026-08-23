// Package unit génère et pilote les services systemd d'un projet.
package unit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/fs"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/template"
)

// Templates connus. Le nom est celui déclaré dans le YAML du projet.
const (
	TemplateMessenger = "symfony-messenger"
	TemplateConsumer  = "rabbitmq-consumer"
	TemplateWorker    = "worker"
)

// Templates énumère les templates de service disponibles.
func Templates() []string {
	return []string{TemplateMessenger, TemplateConsumer, TemplateWorker}
}

// Provider réconcilie les unités systemd d'un projet.
type Provider struct {
	Paths     config.Paths
	Exec      exec.Executor
	Templates *template.Set
	Now       func() time.Time
}

// Name identifie le provider.
func (p *Provider) Name() string { return "unit" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// TargetName renvoie le nom de la cible d'un projet.
func TargetName(id string) string { return "ogs-" + id + ".target" }

// ServiceName renvoie le nom du modèle d'unité d'un service.
func ServiceName(id, service string) string {
	return fmt.Sprintf("ogs-%s-%s@.service", id, service)
}

// InstanceName renvoie le nom d'une instance.
func InstanceName(id, service string, index int) string {
	return fmt.Sprintf("ogs-%s-%s@%d.service", id, service, index)
}

// wantsDir est le répertoire où systemd enregistre les unités activées d'une
// cible. Lire ce répertoire suffit à connaître les instances actives, sans
// interroger systemd — ce qui rend l'observation possible hors ligne.
func (p *Provider) wantsDir(id string) string {
	return filepath.Join(p.Paths.SystemdDir(), TargetName(id)+".wants")
}

// Observed est l'état réel des unités d'un projet.
type Observed struct {
	// Instances associe un service au nombre d'instances activées.
	Instances map[string]int
	// Enabled associe un nom d'instance à sa présence dans la cible.
	Enabled map[string]bool
}

// Observe lit les unités activées.
func (p *Provider) Observe(_ context.Context, proj *model.Project) (Observed, error) {
	obs := Observed{Instances: map[string]int{}, Enabled: map[string]bool{}}
	entries, err := os.ReadDir(p.wantsDir(proj.ID()))
	if err != nil {
		if os.IsNotExist(err) {
			return obs, nil
		}
		return obs, err
	}
	prefix := "ogs-" + proj.ID() + "-"
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".service") {
			continue
		}
		obs.Enabled[name] = true
		// ogs-<id>-<service>@<n>.service
		body := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".service")
		at := strings.LastIndex(body, "@")
		if at < 0 {
			continue
		}
		service := body[:at]
		if n, err := strconv.Atoi(body[at+1:]); err == nil && n > obs.Instances[service] {
			obs.Instances[service] = n
		}
	}
	return obs, nil
}

// unitContext assemble le contexte de rendu d'une unité.
func (p *Provider) unitContext(proj *model.Project, svc model.Service) (map[string]any, error) {
	params := func(key, fallback string) string {
		if svc.Params == nil {
			return fallback
		}
		if v, ok := svc.Params[key]; ok && v != "" {
			return v
		}
		return fallback
	}
	transport := params("transport", svc.Name)
	interpreter := filepath.Join(proj.Spec.Layout.Root, ".bin", "php")
	env := map[string]string{}
	at, err := proj.AppType()
	if err != nil {
		return nil, err
	}
	if at.EnvVar != "" {
		value := "prod"
		if proj.IsPrep() {
			value = "dev"
		}
		env[at.EnvVar] = value
	}
	for k, v := range svc.Params {
		if strings.HasPrefix(k, "env.") {
			env[strings.TrimPrefix(k, "env.")] = v
		}
	}
	memory := params("memoryLimit", "128M")
	return map[string]any{
		"Description":    fmt.Sprintf("ogs %s — %s", proj.ID(), svc.Name),
		"Target":         TargetName(proj.ID()),
		"Identifier":     fmt.Sprintf("ogs-%s-%s", proj.ID(), svc.Name),
		"User":           proj.Spec.User.Name,
		"Group":          proj.Spec.User.Group,
		"Workdir":        params("workdir", filepath.Join(proj.Spec.Layout.Root, "www")),
		"Interpreter":    interpreter,
		"Console":        params("console", "bin/console"),
		"Transport":      transport,
		"TimeLimit":      params("timeLimit", "3600"),
		"MemoryLimit":    memory,
		"MemoryLimitMiB": strings.TrimSuffix(strings.TrimSuffix(memory, "M"), "MiB"),
		"Limit":          params("limit", "500"),
		"RestartSec":     params("restartSec", "5"),
		"StopTimeout":    params("stopTimeout", "60"),
		"Command":        params("command", ""),
		"Environment":    env,
		"RunDir":         p.Paths.ProjectRunDir(proj.ID()),
		// Un socket Unix est créé en 0777 & ~umask : sans ce réglage il sortirait
		// en 0755, dépourvu du bit d'écriture pour le groupe, et www-data ne
		// pourrait pas s'y connecter (§7.11.3).
		"UMask":       params("umask", "0007"),
		"AfterBroker": proj.Spec.AMQP != nil,
	}, nil
}

// Build calcule le contenu attendu d'une unité de service.
func (p *Provider) Build(proj *model.Project, svc model.Service) ([]byte, error) {
	tmpl := svc.Template
	if tmpl == "" {
		tmpl = TemplateMessenger
	}
	known := false
	for _, t := range Templates() {
		if t == tmpl {
			known = true
		}
	}
	if !known {
		return nil, fmt.Errorf("template de service %q inconnu (connus : %s)",
			tmpl, strings.Join(Templates(), ", "))
	}
	// Messenger n'existe qu'à partir de Symfony 4.1 : proposer ce template sur
	// une 3.4 produirait une unité qui échoue au démarrage.
	if tmpl == TemplateMessenger && proj.Spec.App.Kind == model.KindSymfony &&
		!model.SupportsMessenger(proj.Spec.App.Kind, proj.Spec.App.Version) {
		return nil, fmt.Errorf("le template %s exige Symfony 4.1 ou plus (projet en %s) : "+
			"employer %s", TemplateMessenger, proj.Spec.App.Version, TemplateConsumer)
	}
	ctx, err := p.unitContext(proj, svc)
	if err != nil {
		return nil, err
	}
	if tmpl == TemplateWorker && ctx["Command"] == "" {
		return nil, fmt.Errorf("service %s : le template worker exige un paramètre « command »", svc.Name)
	}
	body, err := p.Templates.Render("systemd/"+tmpl+".service", ctx)
	if err != nil {
		return nil, err
	}
	return fs.Render(fs.Header{
		Project:   proj.ID(),
		Resource:  "unit/" + svc.Name,
		Generated: p.now(),
		Template:  p.Templates.OverridePath("systemd/" + tmpl + ".service"),
	}, body, "#"), nil
}

// buildTarget calcule le contenu de la cible du projet.
func (p *Provider) buildTarget(proj *model.Project) ([]byte, error) {
	body, err := p.Templates.Render("systemd/target", map[string]any{
		"Description": fmt.Sprintf("ogs %s — tous les services", proj.ID()),
	})
	if err != nil {
		return nil, err
	}
	return fs.Render(fs.Header{
		Project:   proj.ID(),
		Resource:  "unit/target",
		Generated: p.now(),
	}, body, "#"), nil
}

// Plan calcule les étapes des unités, de la cible et des instances.
func (p *Provider) Plan(_ context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	if len(proj.Spec.Services) == 0 {
		return p.cleanupSteps(proj, obs), nil
	}
	var steps []*engine.Step

	// 1. La cible, qui permet le pilotage groupé.
	targetPath := filepath.Join(p.Paths.SystemdDir(), TargetName(proj.ID()))
	targetContent, err := p.buildTarget(proj)
	if err != nil {
		return nil, err
	}
	if s := p.fileStep("unit/target/"+proj.ID(), targetPath, targetContent,
		"cible "+TargetName(proj.ID())); s != nil {
		steps = append(steps, s)
	}

	// 2. Les modèles d'unité.
	wanted := map[string]bool{}
	for _, svc := range proj.Spec.Services {
		svc := svc
		content, err := p.Build(proj, svc)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(p.Paths.SystemdDir(), ServiceName(proj.ID(), svc.Name))
		if s := p.fileStep("unit/service/"+svc.Name, path, content,
			fmt.Sprintf("unité %s", ServiceName(proj.ID(), svc.Name))); s != nil {
			steps = append(steps, s)
		}

		// 3. Les instances : activer celles qui manquent, désactiver le surplus.
		instances := svc.Instances
		if instances <= 0 {
			instances = 1
		}
		enabled := svc.Enabled == nil || *svc.Enabled
		if !enabled {
			instances = 0
		}
		for i := 1; i <= instances; i++ {
			name := InstanceName(proj.ID(), svc.Name, i)
			wanted[name] = true
			if obs.Enabled[name] {
				steps = append(steps, engine.NewStep("unit/instance/"+name, engine.NoOp,
					fmt.Sprintf("instance %s active", name)))
				continue
			}
			steps = append(steps, p.enableStep(proj, name))
		}
	}
	// Le surplus d'instances est désactivé : réduire l'échelle ne doit pas laisser
	// de worker orphelin.
	for name := range obs.Enabled {
		if !wanted[name] {
			steps = append(steps, p.disableStep(proj, name))
		}
	}
	sortSteps(steps)
	return steps, nil
}

// cleanupSteps retire les unités d'un projet qui n'en déclare plus.
func (p *Provider) cleanupSteps(proj *model.Project, obs Observed) []*engine.Step {
	var steps []*engine.Step
	for name := range obs.Enabled {
		steps = append(steps, p.disableStep(proj, name))
	}
	sortSteps(steps)
	return steps
}

// fileStep produit l'étape d'écriture d'un fichier d'unité, ou rien s'il est
// déjà conforme.
func (p *Provider) fileStep(resource, path string, content []byte, summary string) *engine.Step {
	state, err := fs.Inspect(path, content)
	if err != nil || state == fs.StateCurrent {
		return nil
	}
	action := engine.Create
	if state != fs.StateAbsent {
		action = engine.Update
	}
	s := engine.NewStep(resource, action, summary)
	s.Diff = func() (string, error) {
		current, err := os.ReadFile(path)
		if err != nil {
			return "nouveau fichier " + path, nil
		}
		return fs.UnifiedDiff(string(current), string(content), path), nil
	}
	var previous []byte
	s.Apply = func(context.Context) error {
		if data, err := os.ReadFile(path); err == nil {
			previous = data
		}
		if err := fs.EnsureDir(filepath.Dir(path), 0o755, fs.KeepOwner); err != nil {
			return err
		}
		return fs.WriteAtomic(path, content, 0o644, fs.KeepOwner)
	}
	s.Compensate = func(context.Context) error {
		if previous == nil {
			return os.Remove(path)
		}
		return fs.WriteAtomic(path, previous, 0o644, fs.KeepOwner)
	}
	// Un rechargement du gestionnaire suffit : il est groupé pour tout le plan.
	s.Reload = []string{"systemd:daemon-reload"}
	return s
}

// enableStep active une instance : le lien dans le répertoire .wants de la cible
// est exactement ce que crée « systemctl enable ».
func (p *Provider) enableStep(proj *model.Project, name string) *engine.Step {
	s := engine.NewStep("unit/instance/"+name, engine.Create,
		fmt.Sprintf("activation et démarrage de %s", name))
	link := filepath.Join(p.wantsDir(proj.ID()), name)
	// Non critique : un service qui refuse de démarrer ne doit pas annuler le
	// déploiement du site.
	s.Critical = false
	s.Apply = func(ctx context.Context) error {
		if err := fs.EnsureDir(filepath.Dir(link), 0o755, fs.KeepOwner); err != nil {
			return err
		}
		target := filepath.Join(p.Paths.SystemdDir(), templateOf(name))
		if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
			return err
		}
		return p.systemctl(ctx, "start", name)
	}
	s.Compensate = func(ctx context.Context) error {
		_ = p.systemctl(ctx, "stop", name)
		return os.Remove(link)
	}
	return s
}

// disableStep désactive une instance devenue inutile.
func (p *Provider) disableStep(proj *model.Project, name string) *engine.Step {
	s := engine.NewStep("unit/instance/"+name, engine.Delete,
		fmt.Sprintf("arrêt et désactivation de %s", name))
	link := filepath.Join(p.wantsDir(proj.ID()), name)
	s.Apply = func(ctx context.Context) error {
		if err := p.systemctl(ctx, "stop", name); err != nil {
			return err
		}
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return s
}

// templateOf retrouve le modèle d'une instance : ogs-x-y@3.service → ogs-x-y@.service.
func templateOf(instance string) string {
	at := strings.LastIndex(instance, "@")
	if at < 0 {
		return instance
	}
	return instance[:at+1] + ".service"
}

// systemctl pilote systemd. En mode simulé, rien n'est exécuté : les liens
// d'activation suffisent à rendre l'état observable.
func (p *Provider) systemctl(ctx context.Context, args ...string) error {
	if p.Exec == nil || p.Exec.Simulated() {
		return nil
	}
	_, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: args})
	return err
}

// Control applique un verbe à la cible d'un projet, donc à tous ses services.
func (p *Provider) Control(ctx context.Context, proj *model.Project, verb string) error {
	switch verb {
	case "start", "stop", "restart", "status":
	default:
		return fmt.Errorf("verbe %q inconnu (start, stop, restart ou status)", verb)
	}
	return p.systemctl(ctx, verb, TargetName(proj.ID()))
}

// Scale ajuste le nombre d'instances d'un service en base, sans passer par le
// fichier de projet : c'est une opération d'exploitation, pas une déclaration.
func (p *Provider) Scale(ctx context.Context, proj *model.Project, service string, count int) ([]string, error) {
	if count < 0 {
		return nil, fmt.Errorf("nombre d'instances négatif")
	}
	obs, err := p.Observe(ctx, proj)
	if err != nil {
		return nil, err
	}
	current := obs.Instances[service]
	var changed []string
	for i := current + 1; i <= count; i++ {
		name := InstanceName(proj.ID(), service, i)
		link := filepath.Join(p.wantsDir(proj.ID()), name)
		if err := fs.EnsureDir(filepath.Dir(link), 0o755, fs.KeepOwner); err != nil {
			return changed, err
		}
		target := filepath.Join(p.Paths.SystemdDir(), ServiceName(proj.ID(), service))
		if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
			return changed, err
		}
		if err := p.systemctl(ctx, "start", name); err != nil {
			return changed, err
		}
		changed = append(changed, "+"+name)
	}
	for i := current; i > count; i-- {
		name := InstanceName(proj.ID(), service, i)
		if err := p.systemctl(ctx, "stop", name); err != nil {
			return changed, err
		}
		if err := os.Remove(filepath.Join(p.wantsDir(proj.ID()), name)); err != nil && !os.IsNotExist(err) {
			return changed, err
		}
		changed = append(changed, "-"+name)
	}
	return changed, nil
}

// Logs renvoie la commande de consultation des journaux d'un service.
func (p *Provider) Logs(ctx context.Context, proj *model.Project, service string, follow bool, lines int) error {
	pattern := fmt.Sprintf("ogs-%s-%s@*", proj.ID(), service)
	args := []string{"-u", pattern, "-n", strconv.Itoa(lines)}
	if follow {
		args = append(args, "-f")
	}
	if p.Exec == nil || p.Exec.Simulated() {
		return fmt.Errorf("mode simulé : la commande aurait été « journalctl %s »", strings.Join(args, " "))
	}
	_, err := p.Exec.Run(ctx, exec.Command{Name: "journalctl", Args: args})
	return err
}

// sortSteps rend l'ordre des étapes déterministe : sans cela, l'itération sur une
// table produirait un plan différent à chaque appel.
func sortSteps(steps []*engine.Step) {
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Resource < steps[j].Resource })
}
