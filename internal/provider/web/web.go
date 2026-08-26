package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/fs"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/provider/php"
	"github.com/leviathan/ogs/internal/template"
)

// Provider réconcilie les vhosts d'un projet.
type Provider struct {
	Paths     config.Paths
	Exec      exec.Executor
	Templates *template.Set
	Engine    Engine
	PHP       *php.Provider
	// CertRef résout le certificat d'un projet et indique si son matériel est
	// présent. Le vhost n'est écrit en TLS que si le certificat existe : le
	// contraire empêcherait le serveur web de démarrer.
	CertRef func(*model.Project) (string, bool)
	// CertDir localise le matériel d'un certificat.
	CertDir func(ref string) string
	// ProxyTarget résout la cible d'un vhost de proxy : socket ou adresse locale.
	ProxyTarget func(*model.Project) (string, bool)
	Now         func() time.Time
}

// Name identifie le provider.
func (p *Provider) Name() string { return "web" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// VHostPlan est le rendu attendu d'un vhost.
type VHostPlan struct {
	Name        string
	Path        string
	EnabledPath string
	Content     []byte
	State       fs.State
	Enabled     bool
	ServerName  string
	Domains     []string
}

// Observed est l'état réel des vhosts.
type Observed struct {
	// Stale énumère les vhosts du projet devenus inutiles.
	Stale []string
	// Conflicts associe un domaine à la configuration étrangère qui le sert déjà.
	Conflicts map[string]string
}

// Observe inspecte les vhosts existants et cherche les conflits de domaine.
func (p *Provider) Observe(_ context.Context, proj *model.Project) (Observed, error) {
	obs := Observed{Conflicts: map[string]string{}}
	wanted := map[string]bool{}
	for _, vh := range p.vhosts(proj) {
		wanted[filepath.Join(p.Engine.AvailableDir(), p.Engine.FileName(p.vhostName(proj, vh)))] = true
	}
	entries, err := os.ReadDir(p.Engine.AvailableDir())
	if err != nil {
		if os.IsNotExist(err) {
			return obs, nil
		}
		return obs, err
	}
	ownDomains := map[string]bool{}
	for _, d := range proj.Spec.Domains {
		ownDomains[d.Name] = true
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(p.Engine.AvailableDir(), e.Name())
		if p.belongsTo(proj, e.Name()) {
			if !wanted[path] {
				obs.Stale = append(obs.Stale, path)
			}
			continue
		}
		// Configuration étrangère : on cherche seulement si elle sert l'un de nos
		// domaines, pour refuser de créer un vhost qui ne serait jamais atteint.
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, name := range serverNames(string(data)) {
			if ownDomains[name] {
				obs.Conflicts[name] = path
			}
		}
	}
	sort.Strings(obs.Stale)
	return obs, nil
}

// serverNames extrait les noms servis d'une configuration, quel que soit le
// moteur : ServerName/ServerAlias pour Apache, server_name pour Nginx.
func serverNames(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "servername "), strings.HasPrefix(lower, "serveralias "):
			out = append(out, strings.Fields(line)[1:]...)
		case strings.HasPrefix(lower, "server_name "):
			fields := strings.Fields(strings.TrimSuffix(line, ";"))
			out = append(out, fields[1:]...)
		}
	}
	return out
}

func (p *Provider) belongsTo(proj *model.Project, filename string) bool {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	// Un projet adopté conserve le nom réel de ses fichiers : la reconnaissance
	// s'appuie donc sur les noms attendus, et non sur le seul préfixe canonique.
	for _, name := range p.VHostNames(proj) {
		if base == name {
			return true
		}
	}
	id := proj.ID()
	return base == id || strings.HasPrefix(base, id+"_")
}

// vhosts renvoie les vhosts déclarés, ou celui déduit du projet. Un projet n'a
// pas à déclarer son vhost : le type d'application et les domaines suffisent.
func (p *Provider) vhosts(proj *model.Project) []model.VHost {
	if len(proj.Spec.Web.VHosts) > 0 {
		return proj.Spec.Web.VHosts
	}
	at, err := proj.AppType()
	if err != nil {
		return nil
	}
	var domains []string
	var redirects []string
	for _, d := range proj.Spec.Domains {
		switch d.Role {
		case "", "primary", "alias":
			domains = append(domains, d.Name)
		case "redirect":
			redirects = append(redirects, d.Name)
		}
	}
	if len(domains) == 0 {
		return nil
	}
	main := model.VHost{
		Name:     model.MainPoolName,
		Template: at.VHostTemplate,
		Domains:  domains,
		Docroot:  proj.Spec.Layout.Docroot,
	}
	if at.NeedsPHP && len(proj.Spec.PHP.Pools) > 0 {
		main.Pool = proj.Spec.PHP.Pools[0].Name
	}
	out := []model.VHost{main}
	if len(redirects) > 0 {
		out = append(out, model.VHost{
			Name:     "redirect",
			Template: "redirect",
			Domains:  redirects,
			Options:  map[string]any{"target": domains[0]},
		})
	}
	return out
}

// vhostName traduit le nom logique d'un vhost en nom de fichier : le vhost
// principal porte le nom nu du projet, les autres un suffixe de rôle. Un projet
// adopté conserve en revanche le nom réel de ses fichiers : les renommer
// reviendrait à recréer le vhost, et son domaine apparaîtrait en conflit avec
// lui-même.
func (p *Provider) vhostName(proj *model.Project, vh model.VHost) string {
	if vh.Name == "" || vh.Name == model.MainPoolName {
		return proj.ID()
	}
	if proj.Metadata.Labels["adopted"] == "true" {
		return vh.Name
	}
	return proj.ID() + "_" + vh.Name
}

// VHostNames énumère les noms de fichiers de vhost d'un projet, pour que la
// suppression sache quoi retirer.
func (p *Provider) VHostNames(proj *model.Project) []string {
	vhosts := p.vhosts(proj)
	out := make([]string, 0, len(vhosts))
	for _, vh := range vhosts {
		out = append(out, p.vhostName(proj, vh))
	}
	return out
}

// Build calcule le rendu attendu d'un vhost.
func (p *Provider) Build(proj *model.Project, vh model.VHost) (*VHostPlan, error) {
	at, err := proj.AppType()
	if err != nil {
		return nil, err
	}
	name := p.vhostName(proj, vh)
	tmpl := vh.Template
	if tmpl == "" {
		tmpl = at.VHostTemplate
	}
	if len(vh.Domains) == 0 {
		return nil, fmt.Errorf("vhost %s : aucun domaine", name)
	}

	docroot := vh.Docroot
	if docroot == "" {
		docroot = proj.Spec.Layout.Docroot
	}
	frontController := at.FrontController
	if frontController == "" {
		frontController = "index.php"
	}

	// Le TLS n'est activé dans le rendu que si le certificat est réellement
	// disponible. Sinon le vhost est produit en HTTP seul : c'est aussi ce qui
	// permet au challenge HTTP-01 d'aboutir avant la première émission.
	wantTLS := !proj.Spec.Web.TLSDisabled() && (vh.TLS == nil || vh.TLS.Enabled)
	certRef, certReady := "", false
	if wantTLS && p.CertRef != nil {
		certRef, certReady = p.CertRef(proj)
	}
	certDir := filepath.Join(p.Paths.ACMEDir(), "certificates", name)
	if certRef != "" && p.CertDir != nil {
		certDir = p.CertDir(certRef)
	}

	ctx := map[string]any{
		"Project":         proj.ID(),
		"Name":            name,
		"AppKind":         string(proj.Spec.App.Kind),
		"ServerName":      vh.Domains[0],
		"Aliases":         vh.Domains[1:],
		"Docroot":         docroot,
		"LogDir":          proj.Spec.Layout.LogDir,
		"FrontController": frontController,
		"AllowOverride":   at.AllowOverride,
		// Deux valeurs distinctes : Apache aliase le répertoire des jetons, nginx
		// prend le webroot et y accole l'URI.
		"ACMEChallenge":     p.Paths.ACMEChallenge(),
		"ACMEChallengeDir":  p.Paths.ACMEChallengeTokens(),
		"RobotsNoindexFile": p.Paths.RobotsNoindexFile(),
		"TLS":               wantTLS && certReady,
		"HSTS":              optionBool(vh.Options, "hsts", true),
		"CertDir":           certDir,
		"Noindex":           proj.IsPrep(),
		"ClientMaxBodySize": optionString(vh.Options, "clientMaxBodySize", "32M"),
		"Blocks":            vh.Blocks,
		"RedirectTarget":    optionString(vh.Options, "target", vh.Domains[0]),
		"ProxyTarget":       optionString(vh.Options, "proxyTarget", ""),
		"FPMHandler":        "",
		"FPMPass":           "",
	}

	if at.NeedsPHP {
		pool, err := p.poolFor(proj, vh)
		if err != nil {
			return nil, err
		}
		ref := p.Engine.FPMReference(pool.Listen, pool.Unix)
		ctx["FPMHandler"] = ref
		ctx["FPMPass"] = ref
	}
	if tmpl == "proxy" {
		target := optionString(vh.Options, "proxyTarget", "")
		if target == "" && p.ProxyTarget != nil {
			resolved, unix := p.ProxyTarget(proj)
			if resolved == "" {
				return nil, fmt.Errorf("vhost %s : aucune cible de proxy résolue", name)
			}
			target = p.Engine.ProxyReference(resolved, unix)
		}
		if target == "" {
			return nil, fmt.Errorf("vhost %s : préciser la cible de proxy (options.proxyTarget)", name)
		}
		ctx["ProxyTarget"] = target
	}

	body, err := p.Templates.Render(p.Engine.TemplateName(tmpl), ctx)
	if err != nil {
		return nil, err
	}
	content := fs.Render(fs.Header{
		Project:   proj.ID(),
		Resource:  "web/vhost/" + name,
		Generated: p.now(),
		Template:  p.Templates.OverridePath(p.Engine.TemplateName(tmpl)),
	}, body, p.Engine.CommentPrefix())

	path := filepath.Join(p.Engine.AvailableDir(), p.Engine.FileName(name))
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	enabledPath := filepath.Join(p.Engine.EnabledDir(), p.Engine.FileName(name))
	_, enabledErr := os.Lstat(enabledPath)
	return &VHostPlan{
		Name: name, Path: path, EnabledPath: enabledPath, Content: content,
		State: state, Enabled: enabledErr == nil, ServerName: vh.Domains[0], Domains: vh.Domains,
	}, nil
}

// poolFor retrouve le pool associé à un vhost, afin d'en tirer le point d'écoute.
func (p *Provider) poolFor(proj *model.Project, vh model.VHost) (*php.PoolPlan, error) {
	if p.PHP == nil {
		return nil, fmt.Errorf("provider PHP indisponible : impossible de résoudre le pool du vhost")
	}
	want := vh.Pool
	if want == "" {
		want = model.MainPoolName
	}
	for _, pool := range proj.Spec.PHP.Pools {
		if pool.Name == want {
			return p.PHP.Build(proj, pool)
		}
	}
	return nil, fmt.Errorf("vhost %s : pool %q non déclaré", p.vhostName(proj, vh), want)
}

// Plan calcule les étapes relatives aux vhosts.
func (p *Provider) Plan(_ context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	if len(obs.Conflicts) > 0 {
		for domain, path := range obs.Conflicts {
			return nil, fmt.Errorf("le domaine %s est déjà servi par %s : "+
				"retirer cette configuration ou adopter le site avec « ogs import »", domain, path)
		}
	}
	var steps []*engine.Step
	for _, vh := range p.vhosts(proj) {
		bp, err := p.Build(proj, vh)
		if err != nil {
			return nil, err
		}
		steps = append(steps, p.vhostStep(proj, bp)...)
	}
	for _, stale := range obs.Stale {
		steps = append(steps, p.removeStep(stale))
	}
	return steps, nil
}

func (p *Provider) vhostStep(proj *model.Project, bp *VHostPlan) []*engine.Step {
	resource := "web/vhost/" + bp.Name
	summary := fmt.Sprintf("vhost %s %s (%s)", p.Engine.Name(), bp.Name, strings.Join(bp.Domains, ", "))
	var steps []*engine.Step

	switch bp.State {
	case fs.StateCurrent:
		steps = append(steps, engine.NewStep(resource, engine.NoOp, summary))
	case fs.StateHandEdited, fs.StateForeign:
		s := engine.NewStep(resource, engine.Update, summary+" — fichier modifié hors ogs")
		path := bp.Path
		s.Apply = func(context.Context) error {
			return fmt.Errorf("%s a été modifié en dehors d'ogs : relancer avec --force pour l'écraser "+
				"(une sauvegarde horodatée sera créée)", path)
		}
		steps = append(steps, s)
	default:
		action := engine.Create
		if bp.State == fs.StateOutdated {
			action = engine.Update
		}
		s := engine.NewStep(resource, action, summary)
		s.Diff = func() (string, error) { return diffText(bp.Path, bp.Content) }
		s.Apply = func(ctx context.Context) error { return p.writeVHost(ctx, bp) }
		s.Compensate = func(context.Context) error { return os.Remove(bp.Path) }
		s.Reload = []string{"web:" + p.Engine.Name()}
		steps = append(steps, s)
	}

	if !bp.Enabled {
		s := engine.NewStep(resource+"/enabled", engine.Create,
			fmt.Sprintf("activation du vhost %s", bp.Name))
		s.Apply = func(ctx context.Context) error { return p.enable(ctx, bp) }
		s.Compensate = func(context.Context) error { return os.Remove(bp.EnabledPath) }
		s.Reload = []string{"web:" + p.Engine.Name()}
		steps = append(steps, s)
	} else {
		steps = append(steps, engine.NewStep(resource+"/enabled", engine.NoOp,
			fmt.Sprintf("vhost %s actif", bp.Name)))
	}
	return steps
}

// PlanTLS produit la bascule en TLS des vhosts dont le certificat n'était pas
// encore disponible au moment du calcul. Ces étapes sont appliquées après
// l'émission : le rendu est donc recalculé au moment de l'exécution, pas au plan.
func (p *Provider) PlanTLS(_ context.Context, proj *model.Project) []*engine.Step {
	if p.CertRef == nil {
		return nil
	}
	if _, ready := p.CertRef(proj); ready {
		// Le certificat était déjà là : le vhost a été écrit en TLS d'emblée.
		return nil
	}
	var steps []*engine.Step
	for _, vh := range p.vhosts(proj) {
		if proj.Spec.Web.TLSDisabled() || (vh.TLS != nil && !vh.TLS.Enabled) {
			continue
		}
		vhCopy := vh
		name := p.vhostName(proj, vh)
		s := engine.NewStep("web/vhost/"+name+"/tls", engine.Update,
			fmt.Sprintf("activation TLS du vhost %s", name))
		// Non critique : si l'émission a échoué, le site reste servi en HTTP.
		s.Critical = false
		s.Apply = func(ctx context.Context) error {
			ref, ready := p.CertRef(proj)
			if !ready {
				return fmt.Errorf("certificat %s indisponible : le vhost reste en HTTP "+
					"(reprendre avec « ogs cert issue %s » puis « ogs apply %s »)", ref, proj.ID(), proj.ID())
			}
			bp, err := p.Build(proj, vhCopy)
			if err != nil {
				return err
			}
			if bp.State == fs.StateCurrent {
				return nil
			}
			return p.writeVHost(ctx, bp)
		}
		s.Reload = []string{"web:" + p.Engine.Name()}
		steps = append(steps, s)
	}
	return steps
}

func (p *Provider) removeStep(path string) *engine.Step {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	s := engine.NewStep("web/vhost/"+name, engine.Delete, "vhost obsolète "+path)
	enabled := filepath.Join(p.Engine.EnabledDir(), filepath.Base(path))
	var backup []byte
	s.Apply = func(context.Context) error {
		if data, err := os.ReadFile(path); err == nil {
			backup = data
		}
		_ = os.Remove(enabled)
		return os.Remove(path)
	}
	s.Compensate = func(context.Context) error {
		if backup == nil {
			return nil
		}
		return fs.WriteAtomic(path, backup, 0o644, fs.KeepOwner)
	}
	s.Reload = []string{"web:" + p.Engine.Name()}
	return s
}

// writeVHost écrit puis valide. La validation intervient avant l'activation :
// une configuration refusée par le moteur ne doit jamais devenir active.
func (p *Provider) writeVHost(ctx context.Context, bp *VHostPlan) error {
	// Refus avant écriture : un vhost dont le journal est hors d'atteinte du moteur
	// ne casse pas ce seul site, il arrête le serveur au rechargement suivant.
	for _, logPath := range logPaths(string(bp.Content)) {
		if err := p.reachable(ctx, logPath); err != nil {
			return fmt.Errorf("journal %s : %w", logPath, err)
		}
	}
	previous, hadPrevious := os.ReadFile(bp.Path)
	if err := fs.WriteAtomic(bp.Path, bp.Content, 0o644, fs.KeepOwner); err != nil {
		return err
	}
	if err := validate(ctx, p.Engine, p.Exec); err != nil {
		if hadPrevious == nil {
			_ = fs.WriteAtomic(bp.Path, previous, 0o644, fs.KeepOwner)
		} else {
			_ = os.Remove(bp.Path)
		}
		return err
	}
	return nil
}

// enable crée le lien symbolique d'activation, comme le font a2ensite et le
// répertoire sites-enabled de Nginx, puis revalide.
func (p *Provider) enable(ctx context.Context, bp *VHostPlan) error {
	if err := os.MkdirAll(p.Engine.EnabledDir(), 0o755); err != nil {
		return err
	}
	rel, err := filepath.Rel(p.Engine.EnabledDir(), bp.Path)
	if err != nil {
		rel = bp.Path
	}
	if err := os.Symlink(rel, bp.EnabledPath); err != nil && !os.IsExist(err) {
		return err
	}
	if err := validate(ctx, p.Engine, p.Exec); err != nil {
		_ = os.Remove(bp.EnabledPath)
		return err
	}
	return nil
}

// DropInPath est le chemin du complément d'unité que gère ogs.
func (p *Provider) DropInPath() string {
	return filepath.Join(p.Paths.SystemdDir(), p.Engine.Unit()+".service.d", "ogs.conf")
}

// PlanDropIn produit l'étape du complément d'unité du moteur web : il ouvre en
// écriture les répertoires de journaux des projets.
//
// Les unités des serveurs web sont durcies par leur distribution — Ubuntu livre
// « apache2.service » avec ProtectHome=read-only — et un service ainsi protégé voit
// /home en lecture seule **dans son seul espace de noms** : « mount » affiche rw,
// root écrit sans peine, et le maître meurt pourtant sur l'ouverture du journal d'un
// vhost, emportant tous les sites (« AH00019: Unable to open logs, exiting »). Sans
// ce complément, ogs est inutilisable sur une telle machine.
//
// ReadWritePaths remonte les chemins en écriture après application de ProtectHome
// et de ProtectSystem, ce qui laisse le reste du durcissement intact. Chaque
// répertoire est énuméré plutôt que la racine des homes : rien d'autre que les
// journaux n'a besoin de s'ouvrir. Le préfixe « - » est indispensable — un
// ReadWritePaths pointant sur un chemin absent fait échouer le service en
// 226/NAMESPACE avant toute exécution, et un projet supprimé laisse son répertoire
// dans la liste jusqu'au prochain apply.
func (p *Provider) PlanDropIn(ctx context.Context, projects []*model.Project) ([]*engine.Step, error) {
	var body []byte
	body = append(body, "[Service]\n"...)
	seen := map[string]bool{}
	dirs := make([]string, 0, len(projects))
	for _, proj := range projects {
		dir := proj.Spec.Layout.LogDir
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		body = append(body, "ReadWritePaths=-"+dir+"\n"...)
	}
	content := fs.Render(fs.Header{
		Project: "parc", Resource: "web/dropin", Generated: p.now(),
	}, body, "#")

	path := p.DropInPath()
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	summary := "journaux ouverts à " + p.Engine.Unit() + " (" + filepath.Base(path) + ")"
	if state == fs.StateCurrent {
		return []*engine.Step{engine.NewStep("web/dropin", engine.NoOp, summary)}, nil
	}
	action := engine.Create
	if state != fs.StateAbsent {
		action = engine.Update
	}
	s := engine.NewStep("web/dropin", action, summary)
	s.Apply = func(ctx context.Context) error {
		if err := fs.EnsureDir(filepath.Dir(path), 0o755, fs.KeepOwner); err != nil {
			return err
		}
		if err := fs.WriteAtomic(path, content, 0o644, fs.KeepOwner); err != nil {
			return err
		}
		if p.Exec.Simulated() {
			return nil
		}
		_, err := p.Exec.Run(ctx, exec.Command{
			Name: "systemctl", Args: []string{"daemon-reload"}})
		return err
	}
	s.Compensate = func(ctx context.Context) error {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		if p.Exec.Simulated() {
			return nil
		}
		_, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"daemon-reload"}})
		return err
	}
	steps := []*engine.Step{s}

	// Le redémarrage est une étape distincte, et non critique : l'espace de noms
	// d'un service est établi à son démarrage — un « reload » ne le refait pas — mais
	// un moteur qui refuse de repartir pour une raison étrangère au projet (un port
	// déjà pris, une configuration tierce) ne doit pas faire échouer l'apply et
	// défaire par rollback un site par ailleurs correct.
	//
	// systemd distingue deux arrêts, et ogs doit les distinguer aussi : un moteur
	// « inactive » a été arrêté proprement — l'exploitant l'a peut-être voulu, et le
	// complément prendra effet à son prochain démarrage — tandis qu'un moteur
	// « failed » est mort sur une erreur dont la cause vient précisément d'être
	// levée. Le laisser mort alors qu'on peut le relever serait absurde.
	verb := ""
	switch p.unitState(ctx) {
	case "active", "reloading":
		verb = "restart"
	case "failed":
		verb = "start"
	}
	if verb != "" {
		summary := "redémarrage de " + p.Engine.Unit() + " (espace de noms des journaux)"
		if verb == "start" {
			summary = "relance de " + p.Engine.Unit() + ", arrêté sur erreur"
		}
		r := engine.NewStep("web/dropin/restart", engine.Update, summary)
		r.Critical = false
		r.Apply = func(ctx context.Context) error {
			if p.Exec.Simulated() {
				return nil
			}
			return p.restartUnit(ctx, verb)
		}
		steps = append(steps, r)
	}
	return steps, nil
}

// PlanSnippet produit l'étape du fragment de portée serveur. Elle est globale :
// le fichier ne dépend d'aucun projet, mais tous les vhosts de proxy en
// dépendent.
func (p *Provider) PlanSnippet() (*engine.Step, error) {
	path := p.Engine.SnippetPath()
	if path == "" {
		return nil, nil
	}
	body, err := p.Templates.Render(p.Engine.TemplateName("http-snippet"), map[string]any{})
	if err != nil {
		return nil, err
	}
	content := fs.Render(fs.Header{
		Project: "parc", Resource: "web/snippet", Generated: p.now(),
		Template: p.Templates.OverridePath(p.Engine.TemplateName("http-snippet")),
	}, body, p.Engine.CommentPrefix())

	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	if state == fs.StateCurrent {
		return engine.NewStep("web/snippet", engine.NoOp, "réglages "+p.Engine.Name()), nil
	}
	action := engine.Create
	if state != fs.StateAbsent {
		action = engine.Update
	}
	s := engine.NewStep("web/snippet", action, "réglages "+p.Engine.Name()+" ("+filepath.Base(path)+")")
	s.Apply = func(ctx context.Context) error {
		if err := fs.EnsureDir(filepath.Dir(path), 0o755, fs.KeepOwner); err != nil {
			return err
		}
		if err := fs.WriteAtomic(path, content, 0o644, fs.KeepOwner); err != nil {
			return err
		}
		// La configuration est revalidée : un fragment fautif casserait tous les
		// sites, pas seulement celui qu'on installe.
		if err := validate(ctx, p.Engine, p.Exec); err != nil {
			_ = os.Remove(path)
			return err
		}
		return nil
	}
	s.Reload = []string{"web:" + p.Engine.Name()}
	return s, nil
}

// Reload recharge le serveur web. Un rechargement suffit à prendre en compte une
// nouvelle appartenance de groupe : les workers rappellent initgroups() au fork.
//
// La configuration est revalidée juste avant, et non seulement à l'écriture :
// « apachectl graceful » ne teste rien de lui-même, et un maître Apache qui relit
// une configuration devenue invalide **meurt** — le serveur entier, pas le seul
// vhost fautif. Or l'écriture et le rechargement sont séparés dans le temps : les
// intentions sont groupées en fin d'application, et une suppression intervenue
// entre-temps peut avoir emporté un fichier qu'un vhost référence. Refuser le
// rechargement laisse le serveur tourner sur la configuration qu'il a déjà en
// mémoire, ce qui est toujours préférable à un serveur mort.
func (p *Provider) Reload(ctx context.Context) error {
	if p.Exec.Simulated() {
		return nil
	}
	if err := validate(ctx, p.Engine, p.Exec); err != nil {
		return fmt.Errorf("%w\n  rechargement refusé : %s continue de servir sa configuration "+
			"précédente", err, p.Engine.Unit())
	}
	if err := p.CheckLogs(ctx); err != nil {
		return fmt.Errorf("%w\n  rechargement refusé : %s continue de servir sa configuration "+
			"précédente", err, p.Engine.Unit())
	}
	if _, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl",
		Args: []string{"reload", p.Engine.Unit()}}); err != nil {
		return err
	}
	// Un rechargement qui rend 0 ne dit pas que le serveur vit encore : systemd
	// journalise « Reloaded » puis, une ligne plus bas, la mort du maître. Aucune
	// vérification en amont ne couvre tous les cas — un service dont l'unité porte
	// ProtectHome voit /home en lecture seule dans son seul espace de noms, où ogs
	// ne peut pas se placer. Reste à constater le résultat, et à relever le serveur
	// plutôt qu'à laisser un parc entier hors ligne jusqu'à ce que quelqu'un s'en
	// aperçoive. Le fait est signalé dans tous les cas : un serveur qui meurt à
	// chaque rechargement est un incident, même relevé aussitôt.
	if p.unitActive(ctx) {
		return nil
	}
	if err := p.restartUnit(ctx, "start"); err != nil {
		return fmt.Errorf("le rechargement a arrêté %s, qui refuse de redémarrer : %w\n"+
			"  les sites ne sont plus servis — voir « journalctl -u %s »",
			p.Engine.Unit(), err, p.Engine.Unit())
	}
	return fmt.Errorf("le rechargement a arrêté %s ; le service a été redémarré, "+
		"mais la cause reste à traiter (voir « journalctl -u %s »)",
		p.Engine.Unit(), p.Engine.Unit())
}

// restartUnit redémarre le moteur. Le compteur d'échecs est acquitté au préalable :
// systemd refuse de relancer un service qui a échoué plus de StartLimitBurst fois
// dans l'intervalle (cinq fois en dix secondes par défaut) et rend « start request
// repeated too quickly » — un serveur mort resterait mort, alors que la cause vient
// d'être levée. « reset-failed » est sans effet sur un service sain.
func (p *Provider) restartUnit(ctx context.Context, verb string) error {
	_, _ = p.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{"reset-failed", p.Engine.Unit()}})
	_, err := p.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{verb, p.Engine.Unit()}})
	return err
}

// unitActive dit si l'unité du moteur est active.
func (p *Provider) unitActive(ctx context.Context) bool {
	_, err := p.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{"is-active", "--quiet", p.Engine.Unit()},
	})
	return err == nil
}

// unitState renvoie l'état systemd de l'unité du moteur : « active », « inactive »,
// « failed »… En mode simulé, l'unité est réputée active — c'est ce qui permet aux
// tests d'observer les commandes qui auraient été passées.
func (p *Provider) unitState(ctx context.Context) string {
	if p.Exec.Simulated() {
		return "active"
	}
	res, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{
		"show", p.Engine.Unit(), "-p", "ActiveState", "--value"}})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// CheckLogs vérifie que les journaux des vhosts gérés sont ouvrables en écriture.
//
// C'est le trou que la validation syntaxique laisse : « apachectl -t » répond
// « Syntax OK » sans jamais ouvrir un fichier de log, alors que le maître, lui,
// les ouvre tous au (re)démarrage et **s'arrête** si l'un d'eux résiste —
// « AH00019: Unable to open logs, exiting ». Un seul projet dont le journal est
// inaccessible emporte donc tous les sites du serveur. Le cas s'est produit sur un
// /home remonté en lecture seule après une erreur disque : la configuration était
// valide, l'écriture impossible.
//
// Tous les vhosts activés sont examinés, pas seulement ceux du projet en cours :
// un rechargement est global, et c'est un tiers qui en fait les frais.
func (p *Provider) CheckLogs(ctx context.Context) error {
	entries, err := os.ReadDir(p.Engine.EnabledDir())
	if err != nil {
		// Répertoire absent : il n'y a aucun vhost à servir, donc rien à vérifier.
		return nil
	}
	seen := map[string]bool{}
	for _, e := range entries {
		path := filepath.Join(p.Engine.EnabledDir(), e.Name())
		content, err := os.ReadFile(path)
		if err != nil || !strings.Contains(firstLines(content), fs.Marker) {
			// Un vhost qu'ogs n'a pas écrit ne relève pas de lui.
			continue
		}
		for _, logPath := range logPaths(string(content)) {
			if seen[logPath] {
				continue
			}
			seen[logPath] = true
			if err := writable(logPath); err != nil {
				return fmt.Errorf("journal %s inaccessible en écriture (%s) : %w",
					logPath, e.Name(), err)
			}
			if err := p.reachable(ctx, logPath); err != nil {
				return fmt.Errorf("journal %s (%s) : %w", logPath, e.Name(), err)
			}
		}
	}
	return nil
}

// reachable dit si le moteur web peut atteindre un chemin en écriture depuis son
// propre espace de noms. Écrire depuis ogs ne prouve rien : root écrit sans peine là
// où ProtectHome=read-only interdit au service de le faire, et c'est le service qui
// ouvre les journaux.
func (p *Provider) reachable(ctx context.Context, path string) error {
	props, err := p.unitProtection(ctx)
	if err != nil || props == nil {
		// Sans réponse de systemd, rien ne peut être affirmé : mieux vaut ne rien
		// dire que refuser un vhost sur une supposition.
		return nil
	}
	if !props.protectsHome || !underHome(path) {
		return nil
	}
	for _, rw := range props.readWrite {
		if path == rw || strings.HasPrefix(path, strings.TrimSuffix(rw, "/")+"/") {
			return nil
		}
	}
	return fmt.Errorf("hors d'atteinte de %s : son unité porte ProtectHome=%s, "+
		"qui lui montre /home en lecture seule — le maître s'arrêterait en ouvrant "+
		"ce journal (« Unable to open logs, exiting »). Appliquer le projet écrit le "+
		"complément d'unité %s, qui ouvre ce répertoire",
		p.Engine.Unit(), props.protectHome, p.DropInPath())
}

// unitProtection porte ce que systemd applique à l'unité du moteur.
type unitProtection struct {
	protectHome  string
	protectsHome bool
	readWrite    []string
}

// unitProtection lit les propriétés effectives de l'unité — celles que systemd
// applique après l'unité et tous ses compléments, et non celles du seul fichier.
func (p *Provider) unitProtection(ctx context.Context) (*unitProtection, error) {
	if p.Exec.Simulated() {
		return nil, nil
	}
	res, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{
		"show", p.Engine.Unit(), "-p", "ProtectHome", "-p", "ReadWritePaths"}})
	if err != nil {
		return nil, err
	}
	out := &unitProtection{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ProtectHome":
			out.protectHome = value
			// « tmpfs » et « yes » masquent /home, « read-only » l'interdit en
			// écriture : les trois empêchent d'y écrire un journal.
			out.protectsHome = value == "yes" || value == "read-only" || value == "tmpfs"
		case "ReadWritePaths":
			// systemd rend la liste sur une ligne, séparée par des espaces, chaque
			// chemin pouvant porter le préfixe « - » qui le rend facultatif.
			for _, f := range strings.Fields(value) {
				out.readWrite = append(out.readWrite, strings.TrimPrefix(f, "-"))
			}
		}
	}
	if out.protectHome == "" {
		return nil, nil
	}
	return out, nil
}

// underHome dit si un chemin relève d'un répertoire personnel, seul territoire que
// ProtectHome couvre : /home, /root et /run/user.
func underHome(path string) bool {
	for _, root := range []string{"/home", "/root", "/run/user"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

// reLogDirective reconnaît les directives de journalisation des deux moteurs. Le
// chemin doit être absolu : les formats de log nommés d'Apache (« CustomLog …
// combined ») ne sont pas des chemins, et nginx accepte « access_log off ».
var reLogDirective = regexp.MustCompile(`(?m)^\s*(?:ErrorLog|CustomLog|error_log|access_log)\s+(/\S+)`)

// logPaths extrait les journaux référencés par un vhost.
func logPaths(content string) []string {
	var out []string
	for _, m := range reLogDirective.FindAllStringSubmatch(content, -1) {
		out = append(out, strings.TrimSuffix(m[1], ";"))
	}
	return out
}

// writable dit si un journal peut être ouvert en écriture, sans rien laisser
// derrière : un fichier existant est ouvert en ajout — le test le plus fidèle à ce
// que fera le serveur — et pour un journal encore absent c'est son répertoire qui
// est éprouvé, par un temporaire aussitôt retiré. Créer le journal serait plus
// simple mais le laisserait à root, alors que son propriétaire est le projet.
func writable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o640)
	if err == nil {
		return f.Close()
	}
	if !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(path)
	probe, err := os.CreateTemp(dir, ".ogs-log-probe-*")
	if err != nil {
		return err
	}
	_ = probe.Close()
	return os.Remove(probe.Name())
}

// firstLines borne la recherche de la marque à l'en-tête : un vhost entier peut
// être volumineux, et la marque est sur ses trois premières lignes.
func firstLines(data []byte) string {
	if len(data) > 512 {
		data = data[:512]
	}
	return string(data)
}

func optionString(options map[string]any, key, fallback string) string {
	if options == nil {
		return fallback
	}
	if v, ok := options[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return fallback
}

func optionBool(options map[string]any, key string, fallback bool) bool {
	if options == nil {
		return fallback
	}
	if v, ok := options[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return fallback
}

func diffText(path string, expected []byte) (string, error) {
	current, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "nouveau fichier " + path, nil
		}
		return "", err
	}
	return fs.UnifiedDiff(string(current), string(expected), path), nil
}
