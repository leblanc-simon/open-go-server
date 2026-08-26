package app

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/template"
)

// ServiceName est le nom du service applicatif d'un projet.
const ServiceName = "app"

// DefaultTimeout est le délai d'attente de disponibilité.
const DefaultTimeout = 30 * time.Second

// Provider réconcilie l'application non-PHP d'un projet.
type Provider struct {
	Paths     config.Paths
	Exec      exec.Executor
	Templates *template.Set
	Store     *store.Store
	Installer *Installer
	Now       func() time.Time
}

// Name identifie le provider.
func (p *Provider) Name() string { return "app" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Endpoint décrit le point d'écoute d'une application.
type Endpoint struct {
	// Socket est le chemin du socket Unix, s'il est employé.
	Socket string
	// Address est l'adresse TCP, sinon.
	Address string
	// Port est le numéro attribué, y compris en mode socket : il identifie
	// l'application dans l'inventaire.
	Port int
}

// Unix indique si l'écoute passe par un socket.
func (e Endpoint) Unix() bool { return e.Socket != "" }

// Target renvoie la cible à employer dans une configuration de proxy.
func (e Endpoint) Target() string {
	if e.Unix() {
		return e.Socket
	}
	return e.Address
}

// Applies indique si le projet relève de ce provider.
func Applies(proj *model.Project) bool {
	at, err := proj.AppType()
	if err != nil {
		return false
	}
	return at.Runtime != ""
}

// Endpoint calcule le point d'écoute. Le socket Unix est le défaut : ses
// permissions restreignent l'accès au serveur web et au projet, alors qu'un port
// TCP local est joignable par tout utilisateur de la machine (D36).
func (p *Provider) Endpoint(proj *model.Project) (Endpoint, error) {
	at, err := proj.AppType()
	if err != nil {
		return Endpoint{}, err
	}
	if at.Runtime == "" {
		return Endpoint{}, fmt.Errorf("le projet %s n'a pas d'application supervisée", proj.ID())
	}
	port, err := p.port(proj, at.Runtime)
	if err != nil {
		return Endpoint{}, err
	}
	listen := proj.Spec.App.Listen
	if listen == "" {
		// Un binaire Go écoute sur un port : il n'a pas de convention de socket, et
		// l'imposer supposerait une coopération du code applicatif.
		if at.Runtime == RuntimeGo {
			listen = "port"
		} else {
			listen = "socket"
		}
	}
	switch listen {
	case "socket":
		return Endpoint{
			Socket: filepath.Join(p.Paths.ProjectRunDir(proj.ID()), "app.sock"),
			Port:   port,
		}, nil
	case "port":
		return Endpoint{Address: fmt.Sprintf("127.0.0.1:%d", port), Port: port}, nil
	}
	return Endpoint{}, fmt.Errorf("mode d'écoute %q inconnu (socket ou port)", listen)
}

// port attribue le numéro de l'application, selon la norme 2<runtime><incrément>.
func (p *Provider) port(proj *model.Project, runtime string) (int, error) {
	code, ok := naming.RuntimeCode[runtime]
	if !ok {
		return 0, fmt.Errorf("runtime %q sans code de numérotation", runtime)
	}
	min, err := naming.AppPort(runtime, 1)
	if err != nil {
		return 0, err
	}
	max, err := naming.AppPort(runtime, 999)
	if err != nil {
		return 0, err
	}
	_ = code
	if p.Store == nil {
		return min, nil
	}
	return p.Store.Allocate("app-"+runtime, proj.ID(), ServiceName, min, max)
}

// Observed est l'état réel de l'application.
type Observed struct {
	RuntimeInstalled bool
	LinkTarget       string
	UnitPresent      bool
	Listening        bool
	Endpoint         Endpoint
}

// Observe inspecte le runtime, le lien et le point d'écoute.
func (p *Provider) Observe(_ context.Context, proj *model.Project) (Observed, error) {
	obs := Observed{}
	at, err := proj.AppType()
	if err != nil || at.Runtime == "" {
		return obs, nil
	}
	endpoint, err := p.Endpoint(proj)
	if err != nil {
		return obs, err
	}
	obs.Endpoint = endpoint
	obs.RuntimeInstalled = at.Runtime == RuntimeGo ||
		Installed(proj.Spec.Layout.Root, at.Runtime, proj.Spec.App.Runtime)
	if target, err := os.Readlink(LinkPath(proj.Spec.Layout.Root, at.Runtime)); err == nil {
		obs.LinkTarget = target
	}
	if _, err := os.Stat(p.unitPath(proj)); err == nil {
		obs.UnitPresent = true
	}
	obs.Listening = p.listening(endpoint)
	return obs, nil
}

func (p *Provider) unitPath(proj *model.Project) string {
	return filepath.Join(p.Paths.SystemdDir(),
		fmt.Sprintf("ogs-%s-%s@.service", proj.ID(), ServiceName))
}

// listening vérifie que le point d'écoute répond.
func (p *Provider) listening(e Endpoint) bool {
	if e.Unix() {
		if _, err := os.Stat(e.Socket); err != nil {
			return false
		}
		conn, err := net.DialTimeout("unix", e.Socket, time.Second)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	conn, err := net.DialTimeout("tcp", e.Address, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Build calcule le contenu de l'unité applicative.
func (p *Provider) Build(proj *model.Project, endpoint Endpoint) ([]byte, error) {
	at, err := proj.AppType()
	if err != nil {
		return nil, err
	}
	command, err := p.command(proj, at.Runtime, endpoint)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for k, v := range proj.Spec.App.Env {
		env[k] = v
	}
	// Le point d'écoute est transmis par l'environnement : c'est le code
	// applicatif qui décide, et ogs ne peut pas le lui imposer (§7.11.3).
	if endpoint.Unix() {
		env["SOCKET_PATH"] = endpoint.Socket
	} else {
		env["HOST"] = "127.0.0.1"
		env["PORT"] = strconv.Itoa(endpoint.Port)
		env["LISTEN_ADDR"] = endpoint.Address
	}
	body, err := p.Templates.Render("systemd/app.service", map[string]any{
		"Description": fmt.Sprintf("ogs %s — application %s", proj.ID(), at.Runtime),
		"Target":      fmt.Sprintf("ogs-%s.target", proj.ID()),
		"Identifier":  fmt.Sprintf("ogs-%s-%s", proj.ID(), ServiceName),
		"User":        proj.Spec.User.Name,
		"Group":       proj.Spec.User.Group,
		"Workdir":     filepath.Join(proj.Spec.Layout.Root, "www"),
		"Command":     command,
		"Environment": env,
		"RestartSec":  "5",
		"StopTimeout": "30",
		"RunDir":      p.Paths.ProjectRunDir(proj.ID()),
	})
	if err != nil {
		return nil, err
	}
	return fs.Render(fs.Header{
		Project:   proj.ID(),
		Resource:  "app/" + ServiceName,
		Generated: p.now(),
		Template:  p.Templates.OverridePath("systemd/app.service"),
	}, body, "#"), nil
}

// command construit la ligne d'exécution selon le runtime.
func (p *Provider) command(proj *model.Project, runtime string, endpoint Endpoint) (string, error) {
	link := LinkPath(proj.Spec.Layout.Root, runtime)
	entry := proj.Spec.App.Entrypoint
	switch runtime {
	case RuntimeGo:
		// Le lien pointe vers le binaire déployé : le remplacer puis redémarrer
		// constitue une bascule atomique.
		return link, nil
	case RuntimeNode:
		if entry == "" {
			entry = "server.js"
		}
		return link + " " + entry, nil
	case RuntimePython:
		if entry == "" {
			entry = "app:app"
		}
		bind := "127.0.0.1:" + strconv.Itoa(endpoint.Port)
		if endpoint.Unix() {
			bind = "unix:" + endpoint.Socket
		}
		// La ligne de commande maîtrise le point d'écoute : contrairement à Node,
		// il n'y a pas à espérer que le code applicatif lise une variable.
		return fmt.Sprintf("%s -m gunicorn --bind %s --workers 2 %s", link, bind, entry), nil
	case RuntimeRuby:
		bind := "tcp://127.0.0.1:" + strconv.Itoa(endpoint.Port)
		if endpoint.Unix() {
			bind = "unix://" + endpoint.Socket
		}
		return fmt.Sprintf("%s -e bundle exec puma -b %s", link, bind), nil
	}
	return "", fmt.Errorf("runtime %q inconnu", runtime)
}

// Plan calcule les étapes : runtime, lien, unité, puis attente de disponibilité.
func (p *Provider) Plan(_ context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	at, err := proj.AppType()
	if err != nil || at.Runtime == "" {
		return nil, nil
	}
	endpoint := obs.Endpoint
	var steps []*engine.Step

	// 1. Le runtime. Son installation télécharge depuis Internet : l'étape est non
	// critique, et son échec laisse le site en place avec le service arrêté.
	if !obs.RuntimeInstalled {
		version := proj.Spec.App.Runtime
		if version == "" {
			return nil, fmt.Errorf("le type %s exige une version de runtime (spec.app.runtime)", at.Kind)
		}
		s := engine.NewStep("app/runtime/"+at.Runtime, engine.Create,
			fmt.Sprintf("runtime %s %s dans le home du projet", at.Runtime, version))
		s.Critical = false
		s.Apply = func(ctx context.Context) error {
			return p.Installer.Install(ctx, proj, at.Runtime, version)
		}
		steps = append(steps, s)
	} else {
		steps = append(steps, engine.NewStep("app/runtime/"+at.Runtime, engine.NoOp,
			fmt.Sprintf("runtime %s", at.Runtime)))
	}

	// 2. Le lien géré, qui rend la version effective indépendante de l'unité.
	target := BinaryPath(proj.Spec.Layout.Root, at.Runtime, proj.Spec.App.Runtime)
	link := LinkPath(proj.Spec.Layout.Root, at.Runtime)
	if obs.LinkTarget != target {
		s := engine.NewStep("app/link/"+at.Runtime, engine.Create,
			fmt.Sprintf("lien .bin/%s → %s", LinkName(at.Runtime), target))
		if obs.LinkTarget != "" {
			s.Action = engine.Update
			previous := obs.LinkTarget
			s.Compensate = func(context.Context) error { return replaceSymlink(link, previous) }
		}
		s.Apply = func(context.Context) error { return replaceSymlink(link, target) }
		steps = append(steps, s)
	} else {
		steps = append(steps, engine.NewStep("app/link/"+at.Runtime, engine.NoOp,
			fmt.Sprintf("lien .bin/%s", LinkName(at.Runtime))))
	}

	// 3. L'unité.
	content, err := p.Build(proj, endpoint)
	if err != nil {
		return nil, err
	}
	path := p.unitPath(proj)
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	if state != fs.StateCurrent {
		action := engine.Create
		if state != fs.StateAbsent {
			action = engine.Update
		}
		s := engine.NewStep("app/unit/"+proj.ID(), action,
			fmt.Sprintf("unité applicative (%s, écoute %s)", at.Runtime, describeEndpoint(endpoint)))
		s.Diff = func() (string, error) {
			current, err := os.ReadFile(path)
			if err != nil {
				return "nouveau fichier " + path, nil
			}
			return fs.UnifiedDiff(string(current), string(content), path), nil
		}
		s.Apply = func(context.Context) error {
			if err := fs.EnsureDir(filepath.Dir(path), 0o755, fs.KeepOwner); err != nil {
				return err
			}
			return fs.WriteAtomic(path, content, 0o644, fs.KeepOwner)
		}
		s.Compensate = func(context.Context) error { return os.Remove(path) }
		s.Reload = []string{"systemd:daemon-reload"}
		if action == engine.Update {
			// Relire la définition ne relance rien : sans ce redémarrage, changer le
			// mode d'écoute ou la commande resterait écrit sans prendre effet.
			s.Reload = append(s.Reload, RestartTargetIntent(proj.ID()))
		}
		steps = append(steps, s)
	} else {
		steps = append(steps, engine.NewStep("app/unit/"+proj.ID(), engine.NoOp, "unité applicative"))
	}

	return steps, nil
}

// RestartTargetIntent est l'intention de redémarrage groupé des unités d'un
// projet. Elle est nommée ici plutôt qu'importée du provider d'unités : la
// chaîne est un contrat entre les providers et l'orchestrateur, pas une
// dépendance de code.
func RestartTargetIntent(id string) string { return "systemd:restart-target:" + id }

// PlanHealth produit l'attente de disponibilité. Elle est séparée du reste parce
// qu'elle doit venir après le démarrage des instances, dont le provider d'unités
// a la charge : attendre avant d'avoir lancé quoi que ce soit ne pourrait
// qu'échouer, en accusant l'application à tort.
func (p *Provider) PlanHealth(_ context.Context, proj *model.Project, obs Observed) (*engine.Step, error) {
	at, err := proj.AppType()
	if err != nil || at.Runtime == "" {
		return nil, nil
	}
	endpoint, err := p.Endpoint(proj)
	if err != nil {
		return nil, err
	}
	if obs.Listening {
		return engine.NewStep("app/health/"+proj.ID(), engine.NoOp, "application à l'écoute"), nil
	}
	s := engine.NewStep("app/health/"+proj.ID(), engine.Create,
		fmt.Sprintf("attente de disponibilité sur %s", describeEndpoint(endpoint)))
	// Sans elle, le premier rechargement du serveur web exposerait une 502 (§7.11.6).
	s.Critical = false
	// Les redémarrages en attente doivent avoir eu lieu : autrement on attendrait
	// une application qui tourne encore avec son ancienne configuration.
	s.FlushReloads = true
	s.Apply = func(ctx context.Context) error { return p.waitReady(ctx, proj, endpoint) }
	return s, nil
}

// waitReady attend que l'application réponde, dans la limite du délai imparti.
func (p *Provider) waitReady(ctx context.Context, proj *model.Project, endpoint Endpoint) error {
	timeout := DefaultTimeout
	if proj.Spec.App.Health != nil && proj.Spec.App.Health.Timeout != "" {
		if d, err := time.ParseDuration(proj.Spec.App.Health.Timeout); err == nil {
			timeout = d
		}
	}
	if p.Exec != nil && p.Exec.Simulated() {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.listening(endpoint) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if endpoint.Unix() {
		// Le cas le plus fréquent : l'application ignore la variable et écoute
		// ailleurs. Le message doit le dire, sinon le diagnostic est long.
		return fmt.Errorf("aucune écoute sur %s après %s : l'application lit-elle bien "+
			"SOCKET_PATH ? Sinon, déclarer « listen: port » et lire PORT",
			endpoint.Socket, timeout)
	}
	return fmt.Errorf("aucune écoute sur %s après %s : l'application écoute-t-elle sur "+
		"127.0.0.1 et le port fourni par PORT ?", endpoint.Address, timeout)
}

func describeEndpoint(e Endpoint) string {
	if e.Unix() {
		return "socket " + e.Socket
	}
	return e.Address
}

// replaceSymlink remplace un lien de façon atomique.
func replaceSymlink(link, target string) error {
	if err := fs.EnsureDir(filepath.Dir(link), 0o750, fs.KeepOwner); err != nil {
		return err
	}
	tmp := link + ".ogs-tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ExposedPorts cherche les applications qui écoutent au-delà de la boucle locale.
// Une application joignable depuis l'extérieur contourne le vhost, donc le TLS,
// les en-têtes de sécurité et l'éventuelle authentification (§7.11.3).
func ExposedPorts(listeners []string) []string {
	var out []string
	for _, l := range listeners {
		host, port, err := net.SplitHostPort(strings.TrimSpace(l))
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 21000 || n > 24999 {
			continue
		}
		if host == "127.0.0.1" || host == "::1" || host == "localhost" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// ProxyTarget résout la cible de proxy d'un projet, pour le provider web.
func (p *Provider) ProxyTarget(proj *model.Project) (string, bool) {
	if !Applies(proj) {
		return "", false
	}
	endpoint, err := p.Endpoint(proj)
	if err != nil {
		return "", false
	}
	return endpoint.Target(), endpoint.Unix()
}
