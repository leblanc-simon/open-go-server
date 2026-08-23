package web

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
	Now     func() time.Time
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
// principal porte le nom nu du projet.
func (p *Provider) vhostName(proj *model.Project, vh model.VHost) string {
	if vh.Name == "" || vh.Name == model.MainPoolName {
		return proj.ID()
	}
	return proj.ID() + "_" + vh.Name
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
	wantTLS := vh.TLS == nil || vh.TLS.Enabled
	certRef, certReady := "", false
	if wantTLS && p.CertRef != nil {
		certRef, certReady = p.CertRef(proj)
	}
	certDir := filepath.Join(p.Paths.ACMEDir(), "certificates", name)
	if certRef != "" && p.CertDir != nil {
		certDir = p.CertDir(certRef)
	}

	ctx := map[string]any{
		"Project":           proj.ID(),
		"Name":              name,
		"AppKind":           string(proj.Spec.App.Kind),
		"ServerName":        vh.Domains[0],
		"Aliases":           vh.Domains[1:],
		"Docroot":           docroot,
		"LogDir":            proj.Spec.Layout.LogDir,
		"FrontController":   frontController,
		"AllowOverride":     at.AllowOverride,
		"ACMEChallenge":     p.Paths.ACMEChallenge(),
		"TLS":               wantTLS && certReady,
		"HSTS":              optionBool(vh.Options, "hsts", true),
		"CertDir":           certDir,
		"Noindex":           proj.IsPrep(),
		"BasicAuthFile":     "",
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
		if vh.TLS != nil && !vh.TLS.Enabled {
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

// Reload recharge le serveur web. Un rechargement suffit à prendre en compte une
// nouvelle appartenance de groupe : les workers rappellent initgroups() au fork.
func (p *Provider) Reload(ctx context.Context) error {
	if p.Exec.Simulated() {
		return nil
	}
	_, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"reload", p.Engine.Unit()}})
	return err
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
