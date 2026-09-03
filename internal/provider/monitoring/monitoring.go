// Package monitoring écrit la configuration des outils de supervision du parc :
// les entrées de l'agent de métriques et les prisons de fail2ban (D64).
//
// ogs n'installe rien et ne supervise rien lui-même. Il sait en revanche ce
// qu'aucun des deux outils ne peut deviner : quels journaux appartiennent à un
// projet, quels pools tournent sous quelle version, quels moteurs de bases sont
// réellement en service. Ces fichiers-là sont donc dérivés du parc, comme celui
// de logrotate, et sur le même principe : un fichier unique par outil, recalculé
// en entier à chaque apply.
package monitoring

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/amqp"
	"leblanc.io/open-go-server/internal/provider/db"
	"leblanc.io/open-go-server/internal/provider/php"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/template"
)

// SecretScope est la portée des secrets qui n'appartiennent à aucun projet. Le
// caractère « @ » est refusé aux identifiants de projet : aucune collision n'est
// possible avec le scope d'un projet réel, présent ou à venir.
const SecretScope = "@parc"

// Provider écrit les fichiers de supervision du parc.
type Provider struct {
	Paths     config.Paths
	Cfg       *config.Config
	Exec      exec.Executor
	Templates *template.Set
	Store     *store.Store
	Keys      *secret.Keystore

	// DB ouvre les clients des moteurs de bases ; AMQP porte celui du broker.
	// Tous deux peuvent être absents : la supervision d'un moteur injoignable
	// produit un avertissement, jamais un échec.
	DB   *db.Provider
	AMQP *amqp.Provider
	PHP  *php.Provider

	// WebEngine et WebUnit décrivent le moteur retenu, pour nommer les journaux
	// des projets et l'entrée de statut.
	WebEngine string
	WebUnit   string

	Now func() time.Time
}

// Name identifie le provider.
func (p *Provider) Name() string { return "monitoring" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Observed est ce qu'ogs a trouvé sur la machine.
type Observed struct {
	// TelegrafPresent et Fail2banPresent disent si l'outil est installé. Le
	// critère est son répertoire de fragments : c'est lui qu'ogs écrit, et un
	// binaire présent sans répertoire de fragments ne mènerait qu'à un fichier
	// que personne ne lit.
	TelegrafPresent bool
	Fail2banPresent bool
	// TelegrafGroup est le groupe propriétaire du fragment, résolu depuis le
	// compte de l'agent. Absent, le fragment garde son propriétaire : le rendre
	// illisible à l'agent serait pire que de laisser des droits trop larges — et
	// « doctor » le dit.
	TelegrafGroup   int
	TelegrafGroupOK bool
}

// Observe inspecte la machine. En mode simulé, les deux outils sont réputés
// présents : le mode d'essai doit produire un parc complet sur une machine où
// rien n'est installé.
func (p *Provider) Observe(context.Context) (Observed, error) {
	obs := Observed{TelegrafGroup: -1}
	simulated := p.Exec != nil && p.Exec.Simulated()

	obs.TelegrafPresent = p.enabled(p.Cfg.Monitoring.TelegrafEnabled, p.Paths.TelegrafDir(), simulated)
	obs.Fail2banPresent = p.enabled(p.Cfg.Monitoring.Fail2banEnabled, p.Paths.Fail2banJailDir(), simulated)

	if obs.TelegrafPresent {
		if g, err := user.Lookup(p.Cfg.Monitoring.TelegrafUser()); err == nil {
			if gid, err := strconv.Atoi(g.Gid); err == nil {
				obs.TelegrafGroup, obs.TelegrafGroupOK = gid, true
			}
		}
	}
	return obs, nil
}

// enabled tranche le tri-état : une valeur explicite l'emporte, l'absence s'en
// remet à la présence du répertoire.
func (p *Provider) enabled(decide func() (bool, bool), dir string, simulated bool) bool {
	if v, set := decide(); set {
		return v
	}
	if simulated {
		return true
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// Plan calcule les étapes de supervision : d'abord les comptes, dont les mots de
// passe alimentent le fragment, puis les deux fichiers.
func (p *Provider) Plan(ctx context.Context, projects []*model.Project, obs Observed) ([]*engine.Step, error) {
	var steps []*engine.Step
	if obs.TelegrafPresent {
		accounts, creds := p.planAccounts(ctx)
		steps = append(steps, accounts...)

		step, err := p.planTelegraf(projects, creds, obs)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	if obs.Fail2banPresent {
		step, err := p.planFail2ban(projects)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// fileStep construit l'étape d'écriture d'un fichier géré : même mécanique que
// pour logrotate — inspection par empreinte, diff unifié, compensation par
// restauration de l'ancien contenu.
func (p *Provider) fileStep(resource, summary, path string, content []byte,
	mode os.FileMode, owner fs.Owner, reload string) (*engine.Step, error) {

	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	if state == fs.StateCurrent {
		return engine.NewStep(resource, engine.NoOp, summary), nil
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
		return fs.WriteAtomic(path, content, mode, owner)
	}
	s.Compensate = func(context.Context) error {
		if previous == nil {
			return os.Remove(path)
		}
		return fs.WriteAtomic(path, previous, mode, owner)
	}
	s.Reload = []string{reload}
	return s, nil
}

// header compose l'en-tête des fichiers générés.
func (p *Provider) header(resource, tmpl string, count int) fs.Header {
	return fs.Header{
		Project:   fmt.Sprintf("%d projet(s)", count),
		Resource:  resource,
		Generated: p.now(),
		Template:  p.Templates.OverridePath(tmpl),
	}
}
