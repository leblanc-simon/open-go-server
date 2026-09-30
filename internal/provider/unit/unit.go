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

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/template"
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
	// Force autorise l'écrasement d'un fichier qu'ogs n'a pas écrit, après
	// sauvegarde horodatée (§6.4). Sans lui, le refus n'aurait aucune porte de
	// sortie et l'apply du projet resterait bloqué.
	Force bool
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
	interpreter := filepath.Join(model.BinDir(proj.Spec.Layout.Root), "php")
	env := map[string]string{}
	at, err := proj.AppType()
	if err != nil {
		return nil, err
	}
	if at.EnvVar != "" {
		// « prod » en préproduction aussi (D96), comme le pool PHP.
		env[at.EnvVar] = "prod"
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
	// Une application supervisée est un service comme un autre du point de vue de
	// systemd : son fichier d'unité est écrit par le provider applicatif, mais la
	// cible, l'activation et le démarrage de ses instances relèvent d'ici — sans
	// quoi l'unité existerait sans que rien ne la lance, et l'attente de
	// disponibilité échouerait en accusant l'application.
	appInstances := appServiceInstances(proj)
	if len(proj.Spec.Services) == 0 && appInstances == 0 {
		return p.cleanupSteps(proj, obs), nil
	}
	var steps []*engine.Step

	// 1. La cible, qui permet le pilotage groupé.
	targetPath := filepath.Join(p.Paths.SystemdDir(), TargetName(proj.ID()))
	targetContent, err := p.buildTarget(proj)
	if err != nil {
		return nil, err
	}
	s, err := p.fileStepFor(proj.ID(), "unit/target/"+proj.ID(), targetPath, targetContent,
		"cible "+TargetName(proj.ID()))
	if err != nil {
		return nil, err
	}
	if s != nil {
		steps = append(steps, s)
	}
	steps = append(steps, p.enableTargetStep(proj))

	// 2. Les modèles d'unité.
	wanted := map[string]bool{}
	for _, svc := range proj.Spec.Services {
		svc := svc
		content, err := p.Build(proj, svc)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(p.Paths.SystemdDir(), ServiceName(proj.ID(), svc.Name))
		s, err := p.fileStepFor(proj.ID(), "unit/service/"+svc.Name, path, content,
			fmt.Sprintf("unité %s", ServiceName(proj.ID(), svc.Name)))
		if err != nil {
			return nil, err
		}
		if s != nil {
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
	for i := 1; i <= appInstances; i++ {
		name := InstanceName(proj.ID(), AppServiceName, i)
		wanted[name] = true
		if obs.Enabled[name] {
			steps = append(steps, engine.NewStep("unit/instance/"+name, engine.NoOp,
				fmt.Sprintf("instance %s active", name)))
			continue
		}
		steps = append(steps, p.enableStep(proj, name))
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

// AppServiceName est le nom logique du service d'une application supervisée. Le
// provider applicatif écrit « ogs-<id>-app@.service » ; ce nom fait le lien.
const AppServiceName = "app"

// appServiceInstances renvoie le nombre d'instances de l'application supervisée,
// ou zéro si le projet n'en a pas.
func appServiceInstances(proj *model.Project) int {
	at, err := proj.AppType()
	if err != nil || at.Runtime == "" {
		return 0
	}
	if n := proj.Spec.App.Instances; n > 0 {
		return n
	}
	return 1
}

// cleanupSteps retire les unités d'un projet qui n'en déclare plus.
func (p *Provider) cleanupSteps(proj *model.Project, obs Observed) []*engine.Step {
	var steps []*engine.Step
	for name := range obs.Enabled {
		steps = append(steps, p.disableStep(proj, name))
	}
	if s := p.disableTargetStep(proj); s != nil {
		steps = append(steps, s)
	}
	sortSteps(steps)
	return steps
}

// fileStep produit l'étape d'écriture d'un fichier d'unité, ou rien s'il est
// déjà conforme.
// fileStepFor écrit un fichier d'unité. Le projet est nécessaire pour demander,
// le cas échéant, le redémarrage groupé de ses unités.
func (p *Provider) fileStepFor(project, resource, path string, content []byte, summary string) (*engine.Step, error) {
	state, err := fs.Inspect(path, content)
	// Une unité illisible — chemin occupé par un répertoire, droits ôtés — n'est
	// pas « rien à faire » : confondre les deux faisait annoncer un succès sans
	// que rien n'ait été vérifié ni écrit.
	if err != nil {
		return nil, err
	}
	forced := false
	switch state {
	case fs.StateCurrent:
		return nil, nil
	case fs.StateHandEdited, fs.StateForeign:
		if !p.Force {
			// Une unité éditée à la main porte une intention d'exploitation — une
			// limite mémoire, une variable — que l'écrasement perdrait sans un mot,
			// en redémarrant les services du projet dans la foulée (§6.4). La
			// surcharge supportée par systemd reste le drop-in <unité>.d/.
			return refusStep(resource, summary, path, p.Paths.BackupsDir(), state), nil
		}
		// --force : l'étape redevient une mise à jour ordinaire, annoncée comme
		// telle par « plan » avant qu'« apply » ne l'exécute. La sauvegarde, elle,
		// n'a lieu qu'à l'application : « plan » et « diff » n'écrivent rien.
		forced = true
		state = fs.StateOutdated
		summary += " — écrasement forcé (sauvegarde dans " + p.Paths.BackupsDir() + ")"
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
		if forced {
			// Écraser le travail d'un exploitant sans en garder trace rend
			// l'opération irréversible : la sauvegarde précède l'écriture.
			if _, err := fs.BackupTo(path, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
				return err
			}
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
	s.Reload = []string{"systemd:daemon-reload"}
	if action == engine.Update {
		// Relire la définition ne relance rien : un service en cours continue de
		// tourner avec l'ancienne. Sans ce redémarrage, tout changement d'unité —
		// nouvelle commande, nouvelle variable, nouvelle limite — resterait écrit
		// sans jamais prendre effet. La cible les redémarre tous, dans l'ordre.
		s.Reload = append(s.Reload, RestartTargetIntent(project))
	}
	return s, nil
}

// refusStep produit l'étape qui refuse d'écraser un fichier qu'ogs n'a pas
// écrit. Le message nomme la porte de sortie — elle existe désormais — et
// distingue les deux causes : un fichier « non géré » n'a jamais porté la marque
// d'ogs ; un fichier « modifié hors ogs » a été retouché après avoir été généré.
// L'étape ne porte aucune intention de rechargement : rien ne doit être
// redémarré au nom d'une écriture qui n'a pas eu lieu.
func refusStep(resource, summary, path, backups string, state fs.State) *engine.Step {
	libelle, motif := "modifié hors ogs", "a été modifié en dehors d'ogs"
	if state == fs.StateForeign {
		libelle, motif = "non géré par ogs", "existe déjà sans porter la marque d'ogs"
	}
	s := engine.NewStep(resource, engine.Update, summary+" — fichier "+libelle)
	s.Apply = func(context.Context) error {
		return fmt.Errorf("%s %s : relancer avec --force pour l'écraser "+
			"(l'original sera sauvegardé dans %s ; "+
			"la surcharge supportée par systemd est un drop-in %s.d/)",
			path, motif, backups, filepath.Base(path))
	}
	return s
}

// RestartTargetIntent est l'intention de redémarrage groupé des unités d'un
// projet, par sa cible.
func RestartTargetIntent(id string) string { return "systemd:restart-target:" + id }

// enableTargetStep rattache la cible du projet à multi-user.target. Sans ce lien,
// rien ne réclame la cible au démarrage de la machine : les services du projet
// ne repartent pas après un redémarrage, alors que tout paraît en ordre tant que
// la machine reste allumée.
func (p *Provider) enableTargetStep(proj *model.Project) *engine.Step {
	name := TargetName(proj.ID())
	link := filepath.Join(p.Paths.SystemdDir(), "multi-user.target.wants", name)
	if _, err := os.Lstat(link); err == nil {
		return engine.NewStep("unit/target-enable/"+proj.ID(), engine.NoOp,
			fmt.Sprintf("cible %s activée au démarrage", name))
	}
	s := engine.NewStep("unit/target-enable/"+proj.ID(), engine.Create,
		fmt.Sprintf("activation de %s au démarrage", name))
	s.Critical = false
	s.Apply = func(context.Context) error {
		if err := fs.EnsureDir(filepath.Dir(link), 0o755, fs.KeepOwner); err != nil {
			return err
		}
		target := filepath.Join(p.Paths.SystemdDir(), name)
		if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
			return err
		}
		return nil
	}
	s.Compensate = func(context.Context) error { return os.Remove(link) }
	return s
}

// disableTargetStep détache la cible : un projet sans service, ou supprimé, ne
// doit plus être réclamé au démarrage.
func (p *Provider) disableTargetStep(proj *model.Project) *engine.Step {
	name := TargetName(proj.ID())
	link := filepath.Join(p.Paths.SystemdDir(), "multi-user.target.wants", name)
	if _, err := os.Lstat(link); err != nil {
		return nil
	}
	s := engine.NewStep("unit/target-enable/"+proj.ID(), engine.Delete,
		fmt.Sprintf("désactivation de %s au démarrage", name))
	s.Apply = func(context.Context) error {
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
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
	// Le gestionnaire doit avoir relu ses unités avant qu'on démarre une instance :
	// autrement systemd répond « Unit not found » sur une unité qu'ogs vient
	// pourtant d'écrire. Le daemon-reload est groupé, il est donc vidé ici.
	s.FlushReloads = true
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
	// Le lien vient d'être créé : sans un nouveau rechargement, la cible ne le
	// connaît pas et n'entraînerait pas l'instance au démarrage de la machine.
	s.Reload = []string{"systemd:daemon-reload"}
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
// sortSteps ordonne les étapes selon le cycle de vie, non par nom : une instance
// ne peut pas démarrer avant que son modèle d'unité existe, et un fichier
// d'unité ne peut pas être retiré avant que ses instances soient arrêtées. Le
// nom ne sert qu'à départager, pour que le plan reste reproductible.
func sortSteps(steps []*engine.Step) {
	rang := func(s *engine.Step) int {
		suppression := s.Action == engine.Delete
		switch {
		case strings.HasPrefix(s.Resource, "unit/target-enable/"):
			if suppression {
				return 5
			}
			return 1 // après l'écriture de la cible, avant les instances
		case strings.HasPrefix(s.Resource, "unit/target/"):
			if suppression {
				return 5 // la cible part en dernier
			}
			return 0 // et arrive en premier
		case strings.HasPrefix(s.Resource, "unit/service/"):
			if suppression {
				return 4
			}
			return 1
		default: // unit/instance/
			if suppression {
				return 3 // arrêter avant de retirer les fichiers
			}
			return 2 // démarrer après les avoir écrits
		}
	}
	sort.SliceStable(steps, func(i, j int) bool {
		if ri, rj := rang(steps[i]), rang(steps[j]); ri != rj {
			return ri < rj
		}
		return steps[i].Resource < steps[j].Resource
	})
}
