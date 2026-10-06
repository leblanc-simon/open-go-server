package web

import (
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/app"
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
	// Une écoute nouvelle — vhost absent, inactif, ou qui change d'adresse — exige
	// un port libre (R2-37, D118) : « apachectl -t » ne voit pas un port pris, et
	// le maître Apache meurt au rechargement suivant en emportant tous les sites ;
	// nginx garde sa configuration précédente sans un mot. Seul ce vhost est
	// refusé, d'une étape non critique : le plan global continue, et rien du plan
	// d'un projet n'en dépend.
	if (state != fs.StateCurrent || !isEnabled) && !(isEnabled && statusListens(filePath, listen)) {
		if holder, err := p.portHolder(listen); err != nil || holder != "" {
			return []*engine.Step{statusPortRefused(summary, listen, holder, err)}, nil
		}
	}
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
			if err := p.validateConfig(ctx); err != nil {
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

// statusListens dit si le vhost de supervision en place écoute déjà sur listen :
// le port est alors celui que le moteur tient pour lui.
func statusListens(path, listen string) bool {
	data, err := readBounded(path, maxConfigRead)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), ";"))
		if len(fields) >= 2 && strings.EqualFold(fields[0], "listen") && sameAddress(fields[1], listen) {
			return true
		}
	}
	return false
}

// sameAddress compare deux « adresse:port » après analyse, jamais comme chaînes.
func sameAddress(a, b string) bool {
	ha, pa, err := net.SplitHostPort(a)
	if err != nil {
		return false
	}
	hb, pb, err := net.SplitHostPort(b)
	if err != nil || pa != pb {
		return false
	}
	ia, ib := net.ParseIP(ha), net.ParseIP(hb)
	return ia != nil && ia.Equal(ib)
}

// portHolder nomme l'écoute qui occupe déjà l'adresse du vhost de supervision,
// vide s'il n'y en a pas. Une écoute joker du même port l'occupe aussi : bind()
// sur 127.0.0.1 échoue quand 0.0.0.0 est tenu, et « :: » est compté pour l'IPv4
// comme pour l'IPv6 — IPV6_V6ONLY ne se lit pas dans /proc/net, et un refus à
// tort ne coûte que la supervision, là où un port pris coûte le parc.
func (p *Provider) portHolder(listen string) (string, error) {
	host, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	port, err := strconv.Atoi(portStr)
	if ip == nil || err != nil {
		return "", fmt.Errorf("adresse d'écoute %q illisible", listen)
	}
	listeners, err := p.listeners()
	if err != nil {
		return "", err
	}
	for _, l := range listeners {
		if l.Port != port {
			continue
		}
		occupe := l.IP.Equal(ip)
		if l.IP.IsUnspecified() {
			v4 := l.IP.To4() != nil
			occupe = !v4 || ip.To4() != nil
		}
		if occupe {
			return fmt.Sprintf("%s (uid %d)", net.JoinHostPort(l.IP.String(), portStr), l.UID), nil
		}
	}
	return "", nil
}

// listeners rend les écoutes TCP de la machine, ou rien en mode simulé : un plan
// calculé sous --prefix ne dépend pas de ce qui tourne sur le poste.
func (p *Provider) listeners() ([]app.Listener, error) {
	if p.Listeners != nil {
		return p.Listeners()
	}
	if p.Exec == nil || p.Exec.Simulated() {
		return nil, nil
	}
	return app.ReadListeners(app.ProcNet)
}

// statusPortRefused est l'étape qui refuse le vhost de supervision sur un port
// déjà pris, ou que /proc/net n'a pas permis de juger.
func statusPortRefused(summary, listen, holder string, cause error) *engine.Step {
	s := engine.NewStep("web/status", engine.Update, summary+" — port déjà pris")
	s.Critical = false
	s.Apply = func(context.Context) error {
		if cause != nil {
			return fmt.Errorf("vhost de supervision non écrit : l'occupation de %s n'a pas pu être lue (%w) — "+
				"un port pris arrêterait le maître Apache au rechargement", listen, cause)
		}
		return fmt.Errorf("vhost de supervision non écrit : %s est déjà tenu par l'écoute %s — sur un port pris, "+
			"« Listen » arrête le maître Apache au rechargement suivant, et nginx garde sa configuration "+
			"précédente ; choisir un port libre (monitoring.status.listen dans /etc/ogs/config.yaml) ou "+
			"libérer celui-ci (D118)", listen, holder)
	}
	return s
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
