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
	runtimeprov "leblanc.io/open-go-server/internal/provider/runtime"
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
	// Force autorise l'écrasement de l'unité applicative qu'ogs n'a pas écrite,
	// après sauvegarde horodatée (§6.4). Sans lui, le refus n'aurait aucune porte
	// de sortie et l'apply du projet resterait bloqué.
	Force bool
	// Owner résout le compte du projet, à qui appartiennent les liens gérés : ils
	// vivent dans un répertoire de l'utilisateur (D70). Nil, ou en mode simulé,
	// le propriétaire n'est pas changé.
	Owner func(*model.Project) (uid, gid int, err error)
}

// linkOwner est le propriétaire des liens gérés d'un projet.
func (p *Provider) linkOwner(proj *model.Project) fs.Owner {
	if p.Owner == nil || (p.Exec != nil && p.Exec.Simulated()) {
		return fs.KeepOwner
	}
	uid, gid, err := p.Owner(proj)
	if err != nil {
		return fs.KeepOwner
	}
	return fs.Owner{UID: uid, GID: gid}
}

// setLink pose un lien géré dans le répertoire des liens, sans jamais suivre de
// lien : ce répertoire appartient à l'utilisateur, qui pourrait sinon y
// substituer un lien et faire écrire root où il veut.
func (p *Provider) setLink(proj *model.Project, name, target string) error {
	return fs.SymlinkAt(proj.Spec.Layout.Root, model.LinkDir, name, target, p.linkOwner(proj), p.Force)
}

// readLink observe un lien géré par le même chemin sûr.
func readLink(proj *model.Project, name string) string {
	kind, target, err := fs.LinkStateAt(proj.Spec.Layout.Root, model.LinkDir, name)
	if err != nil || kind != fs.EntrySymlink {
		return ""
	}
	return target
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
	// BundleLinkTarget est la cible du lien bundle, propre à Ruby : l'unité y
	// invoque le lanceur, jamais l'interpréteur.
	BundleLinkTarget string
	// RunDirMode est le mode du répertoire d'exécution, ou zéro s'il est absent.
	RunDirMode  os.FileMode
	UnitPresent bool
	Listening   bool
	Endpoint    Endpoint
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
	obs.LinkTarget = readLink(proj, LinkName(at.Runtime))
	if at.Runtime == RuntimeRuby {
		obs.BundleLinkTarget = readLink(proj, LinkBundle)
	}
	if info, err := os.Stat(p.Paths.ProjectRunDir(proj.ID())); err == nil && info.IsDir() {
		obs.RunDirMode = info.Mode().Perm()
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
		// « bundle » est un exécutable, non un programme d'une ligne : « ruby -e »
		// évaluerait ce mot comme du code (NameError) et puma ne démarrerait
		// jamais. Contrairement au « -m » de Python, « -e » n'est pas un
		// commutateur de module. D'où un second lien géré, à côté du lien ruby.
		cmd := bundleLinkPath(proj) + " exec puma"
		if entry != "" {
			// L'entrypoint d'un projet Ruby est le fichier de configuration de puma :
			// l'ignorer sans un mot, comme on le faisait, laissait croire qu'il
			// s'appliquait.
			cmd += " -C " + entry
		}
		// Le bind vient après : une option de ligne de commande l'emporte sur le
		// fichier de configuration, et c'est ogs qui décide du point d'écoute.
		return cmd + " -b " + bind, nil
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

	// 0. Le répertoire d'exécution, qui porte le socket. Le fichier tmpfiles le
	// déclare aussi — il le recrée après un redémarrage, /run étant un tmpfs vidé
	// — mais il appartient au plan de parc, exécuté après tous les projets :
	// l'instance aurait démarré avant que le répertoire de son socket existe, et
	// l'attente de disponibilité aurait accusé le code applicatif d'un échec qui
	// n'est pas le sien.
	steps = append(steps, p.runDirStep(proj, obs))

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
	name := LinkName(at.Runtime)
	if obs.LinkTarget != target {
		s := engine.NewStep("app/link/"+at.Runtime, engine.Create,
			fmt.Sprintf("lien %s/%s → %s", model.LinkDir, name, target))
		if obs.LinkTarget != "" {
			s.Action = engine.Update
			previous := obs.LinkTarget
			s.Compensate = func(context.Context) error { return p.setLink(proj, name, previous) }
		}
		s.Apply = func(context.Context) error { return p.setLink(proj, name, target) }
		steps = append(steps, s)
	} else {
		steps = append(steps, engine.NewStep("app/link/"+at.Runtime, engine.NoOp,
			fmt.Sprintf("lien %s/%s", model.LinkDir, name)))
	}

	// 2 bis. Le lanceur de Ruby. L'unité invoque « bundle exec puma » : sans ce
	// second lien, elle nommerait le bin d'une version, ce que le lien existe
	// justement pour éviter (D30).
	if at.Runtime == RuntimeRuby {
		steps = append(steps, p.bundleLinkStep(proj, obs))
	}

	// 3. L'unité.
	s, err := p.unitStep(proj, at, endpoint)
	if err != nil {
		return nil, err
	}
	steps = append(steps, s)

	return steps, nil
}

// unitStep produit l'étape d'écriture de l'unité applicative. Elle suit le même
// régime que celle du provider d'unités — cinq états, refus nommant --force,
// sauvegarde avant écrasement : les deux fichiers vivent dans le même
// répertoire, et le même geste d'exploitation doit y recevoir la même réponse.
func (p *Provider) unitStep(proj *model.Project, at model.AppType, endpoint Endpoint) (*engine.Step, error) {
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
		summary := fmt.Sprintf("unité applicative (%s, écoute %s)", at.Runtime, describeEndpoint(endpoint))
		forced := false
		switch state {
		case fs.StateHandEdited, fs.StateForeign:
			if !p.Force {
				// Une application supervisée est un service comme un autre : son unité
				// éditée à la main porte une intention d'exploitation — une limite
				// mémoire, une variable — que l'écrasement perdrait sans un mot, en
				// redémarrant les instances dans la foulée (§6.4). Le service voisin
				// écrit par le provider d'unités refuse déjà ; les deux fichiers du même
				// répertoire ne peuvent pas répondre différemment au même geste.
				return refusStep("app/unit/"+proj.ID(), summary, path, p.Paths.BackupsDir(), state), nil
			}
			// --force : l'étape redevient une mise à jour ordinaire, annoncée comme
			// telle par « plan » avant qu'« apply » ne l'exécute. La sauvegarde n'a
			// lieu qu'à l'application : « plan » et « diff » n'écrivent rien.
			forced = true
			state = fs.StateOutdated
			summary += " — écrasement forcé (sauvegarde dans " + p.Paths.BackupsDir() + ")"
		}
		action := engine.Create
		if state != fs.StateAbsent {
			action = engine.Update
		}
		s := engine.NewStep("app/unit/"+proj.ID(), action, summary)
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
			// Compenser en retirant le fichier effacerait l'unité qui existait avant
			// l'étape — sous --force, celle de l'exploitant.
			if previous == nil {
				return os.Remove(path)
			}
			return fs.WriteAtomic(path, previous, 0o644, fs.KeepOwner)
		}
		s.Reload = []string{"systemd:daemon-reload"}
		if action == engine.Update {
			// Relire la définition ne relance rien : sans ce redémarrage, changer le
			// mode d'écoute ou la commande resterait écrit sans prendre effet.
			s.Reload = append(s.Reload, RestartTargetIntent(proj.ID()))
		}
		return s, nil
	}
	return engine.NewStep("app/unit/"+proj.ID(), engine.NoOp, "unité applicative"), nil
}

// refusStep produit l'étape qui refuse d'écraser un fichier qu'ogs n'a pas
// écrit, et distingue les deux causes : un fichier « non géré » n'a jamais porté
// la marque d'ogs ; un fichier « modifié hors ogs » a été retouché après avoir
// été généré. L'étape ne porte aucune intention de rechargement : rien ne doit
// être redémarré au nom d'une écriture qui n'a pas eu lieu. Le message est celui
// du provider d'unités, recopié plutôt qu'importé pour la même raison que
// RestartTargetIntent — les deux paquets restent découplés.
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

// runDirStep pose /run/ogs/<id>. Le mode et le propriétaire sont ceux que le
// provider runtime déclare dans /etc/tmpfiles.d : deux endroits qui décideraient
// du mode d'un même répertoire feraient osciller le plan.
func (p *Provider) runDirStep(proj *model.Project, obs Observed) *engine.Step {
	path := p.Paths.ProjectRunDir(proj.ID())
	if obs.RunDirMode == runtimeprov.ProjectRunMode.Perm() {
		return engine.NewStep("app/rundir/"+proj.ID(), engine.NoOp, "répertoire d'exécution "+path)
	}
	action := engine.Create
	if obs.RunDirMode != 0 {
		action = engine.Update
	}
	s := engine.NewStep("app/rundir/"+proj.ID(), action,
		fmt.Sprintf("répertoire d'exécution %s (%04o %s:%s)", path,
			runtimeprov.ProjectRunMode.Perm(), proj.Spec.User.Name, proj.Spec.User.Group))
	s.Apply = func(context.Context) error {
		return runtimeprov.EnsureProjectRunDir(path, proj)
	}
	s.Compensate = func(context.Context) error {
		// Seul un répertoire resté vide est retiré : un socket ou un verrou encore
		// présent appartient à un service qui tourne.
		_ = os.Remove(path)
		return nil
	}
	return s
}

// bundleLinkPath renvoie le chemin du lien vers le lanceur de Ruby.
func bundleLinkPath(proj *model.Project) string {
	return filepath.Join(model.BinDir(proj.Spec.Layout.Root), LinkBundle)
}

// bundleLinkStep pose le lien bundle à côté du lien ruby.
func (p *Provider) bundleLinkStep(proj *model.Project, obs Observed) *engine.Step {
	target := BundlePath(proj.Spec.Layout.Root, proj.Spec.App.Runtime)
	if obs.BundleLinkTarget == target {
		return engine.NewStep("app/link/"+LinkBundle, engine.NoOp, "lien "+model.LinkDir+"/"+LinkBundle)
	}
	s := engine.NewStep("app/link/"+LinkBundle, engine.Create,
		fmt.Sprintf("lien %s/%s → %s", model.LinkDir, LinkBundle, target))
	if obs.BundleLinkTarget != "" {
		s.Action = engine.Update
		previous := obs.BundleLinkTarget
		s.Compensate = func(context.Context) error { return p.setLink(proj, LinkBundle, previous) }
	}
	s.Apply = func(context.Context) error { return p.setLink(proj, LinkBundle, target) }
	return s
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
