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
	"leblanc.io/open-go-server/internal/provider/removal"
	"leblanc.io/open-go-server/internal/provider/verify"
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

	// TrustedIP porte les adresses de confiance du pare-feu (D72), ajoutées à
	// l'« ignoreip » de chaque prison : une adresse qui a accès à tous les
	// profils ne doit pas être bannie par une autre voie — c'est d'ordinaire
	// celle d'où l'on administre, et quelques mots de passe mal tapés suffiraient.
	TrustedIP []string
	// KeepTrusted dit que les adresses de confiance ne se lisent pas — ufw.yaml
	// présent mais invalide : celles de l'« ignoreip » du fichier de prisons en
	// place sont gardées (D109). Recalculer sans elles rendait bannissable
	// l'adresse d'administration, alors que le pare-feu, lui, restait en l'état.
	KeepTrusted bool

	// Force autorise l'écrasement d'un fichier modifié à la main (§6.4). Sans
	// elle, l'apply refuse — les deux fichiers de supervision sont recalculés en
	// entier, et écraser sans le dire perdrait le travail de l'exploitant.
	Force bool

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
//
// Un outil désactivé ne laisse pas derrière lui le fichier d'un apply précédent :
// il continuerait de s'appliquer — prisons comprises — et ne serait plus jamais
// recalculé, pas même à la suppression d'un projet, alors même que D68 fait de ce
// recalcul un invariant. C'est la symétrie du vhost d'état, qui sait déjà se
// retirer.
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
	} else {
		step, err := p.removeStep("monitoring/telegraf",
			"retrait des entrées Telegraf (supervision désactivée)",
			p.Paths.TelegrafFile(), "monitoring:telegraf")
		if err != nil {
			return nil, err
		}
		if step != nil {
			steps = append(steps, step)
		}
	}
	if obs.Fail2banPresent {
		step, err := p.planFail2ban(projects)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	} else {
		step, err := p.removeStep("monitoring/fail2ban",
			"retrait des prisons fail2ban (supervision désactivée)",
			p.Paths.Fail2banJailFile(), "monitoring:fail2ban")
		if err != nil {
			return nil, err
		}
		if step != nil {
			steps = append(steps, step)
		}
	}
	return steps, nil
}

// removeStep retire un fichier de supervision devenu sans objet. Seul le fichier
// d'ogs est retiré, jamais le répertoire : il appartient à la distribution, et
// les fichiers voisins à l'administrateur. Il suit le régime de son écriture :
// retouché — une prison ajoutée, un seuil changé —, il ne part que sous --force
// et sauvegardé ; sans la marque d'ogs, il n'est pas à lui. Mode et propriétaire
// sont relevés au retrait et rendus tels quels par la compensation : le fragment
// Telegraf est en 0640 groupe de l'agent, et le restaurer en root:root le
// rendrait illisible à celui qui doit le lire.
func (p *Provider) removeStep(resource, summary, path, reload string) (*engine.Step, error) {
	return removal.File{
		Resource: resource,
		Summary:  summary,
		Path:     path,
		Force:    p.Force,
		Backups:  p.backupDir(),
		Now:      p.now,
		Reload:   []string{reload},
		// Non critique, comme le reste de la supervision : un fichier qu'on
		// n'arrive pas à retirer ne doit pas faire échouer un déploiement.
		NonCritical: true,
	}.Step()
}

// managedFile décrit un fichier de supervision géré par ogs.
type managedFile struct {
	resource string
	summary  string
	path     string
	content  []byte
	mode     os.FileMode
	owner    fs.Owner
	reload   string

	// refresh recalcule le corps au moment de l'application, lorsqu'il dépend de
	// ce que les étapes précédentes ont fait. Le fragment de l'agent est dans ce
	// cas : il porte les mots de passe des comptes de supervision, qui ne sont
	// scellés qu'à l'application de leur étape. Absent, le corps calculé au plan
	// est écrit tel quel.
	refresh func() ([]byte, error)

	// validate éprouve le fichier écrit avec l'outil qui le lit (D117). Un refus
	// fait rendre le fichier précédent par l'étape elle-même : le moteur ne
	// compense jamais l'étape qui échoue. restore rend l'ancien contenu, rewrite
	// remet le nouveau quand le défaut n'est pas le sien.
	validate func(ctx context.Context, restore, rewrite func() error) error
}

// fileStep construit l'étape d'écriture d'un fichier géré : même mécanique que
// pour logrotate — inspection par empreinte, diff unifié, compensation par
// restauration de l'ancien contenu, et refus d'écraser ce qu'un exploitant a
// retouché (§6.4).
func (p *Provider) fileStep(f managedFile) (*engine.Step, error) {
	state, err := fs.Inspect(f.path, f.content)
	if err != nil {
		return nil, err
	}
	summary, forced := f.summary, false
	switch state {
	case fs.StateCurrent:
		return engine.NewStep(f.resource, engine.NoOp, summary), nil
	case fs.StateHandEdited, fs.StateForeign:
		// Ces deux fichiers sont recalculés en entier à chaque apply : une ligne
		// ajoutée à la main y disparaîtrait sans un mot, et sans copie. C'est
		// d'autant moins acceptable pour les prisons de fail2ban, où la retouche
		// porte sur ce qui bannit des adresses.
		if !p.Force {
			return refusStep(f.resource, summary, f.path, p.backupPattern(f.path), state), nil
		}
		// --force : l'étape redevient une mise à jour ordinaire, annoncée comme
		// telle par « plan ». La sauvegarde n'a lieu qu'à l'application : « plan »
		// et « diff » n'écrivent rien.
		forced = true
		state = fs.StateOutdated
		summary += " — écrasement forcé (sauvegarde " + p.backupPattern(f.path) + ")"
	}
	action := engine.Create
	if state != fs.StateAbsent {
		action = engine.Update
	}
	s := engine.NewStep(f.resource, action, summary)
	s.Diff = func() (string, error) {
		current, err := os.ReadFile(f.path)
		if err != nil {
			return "nouveau fichier " + f.path, nil
		}
		return fs.UnifiedDiff(string(current), string(f.content), f.path), nil
	}
	var previous []byte
	s.Apply = func(ctx context.Context) error {
		previous = nil
		content := f.content
		if f.refresh != nil {
			refreshed, err := f.refresh()
			if err != nil {
				return err
			}
			content = refreshed
		}
		if data, err := os.ReadFile(f.path); err == nil {
			previous = data
		}
		if forced {
			// Écraser le travail d'un exploitant sans en garder trace rend
			// l'opération irréversible : la sauvegarde précède l'écriture, et sort du
			// répertoire (voir backupDir).
			if _, err := fs.BackupTo(f.path, p.backupDir(), p.now()); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := fs.EnsureDir(filepath.Dir(f.path), 0o755, fs.KeepOwner); err != nil {
			return err
		}
		if err := fs.WriteAtomic(f.path, content, f.mode, f.owner); err != nil {
			return err
		}
		if f.validate == nil {
			return nil
		}
		restore := func() error {
			if previous == nil {
				if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
					return err
				}
				return nil
			}
			return fs.WriteAtomic(f.path, previous, f.mode, f.owner)
		}
		// Des prisons refusées sont rendues, et le refus est toléré (D117) : le
		// reste du plan global suit son cours. Une vraie erreur d'écriture reste
		// critique.
		err := f.validate(ctx, restore, func() error { return fs.WriteAtomic(f.path, content, f.mode, f.owner) })
		if verify.IsRefused(err) {
			return engine.Tolerated(err)
		}
		return err
	}
	s.Compensate = func(context.Context) error {
		if previous == nil {
			return os.Remove(f.path)
		}
		return fs.WriteAtomic(f.path, previous, f.mode, f.owner)
	}
	s.Reload = []string{f.reload}
	return s, nil
}

// refusStep refuse d'écraser un fichier qu'ogs n'a pas écrit lui-même, et dit
// comment passer outre : §6.4 veut le refus, pas la surprise.
func refusStep(resource, summary, path, sauvegarde string, state fs.State) *engine.Step {
	libelle, motif := "modifié hors ogs", "a été modifié en dehors d'ogs"
	if state == fs.StateForeign {
		libelle, motif = "non géré par ogs", "existe déjà sans porter la marque d'ogs"
	}
	s := engine.NewStep(resource, engine.Update, summary+" — fichier "+libelle)
	// Non critique, comme le reste de la supervision : un fragment qu'on refuse
	// d'écraser ne doit pas faire compenser le déploiement d'un site.
	s.Critical = false
	s.Apply = func(context.Context) error {
		return fmt.Errorf("%s %s : relancer avec --force pour l'écraser "+
			"(l'original sera sauvegardé en %s)", path, motif, sauvegarde)
	}
	return s
}

// backupDir est le répertoire où atterrit la sauvegarde d'un écrasement forcé.
// Elle ne peut pas rester à côté de l'original : Telegraf et fail2ban lisent
// leur répertoire de fragments en entier, et la liste des suffixes que chacun
// ignore lui appartient — elle peut changer sous nos pieds, l'emplacement ne
// dépend de personne. Une copie chargée serait une seconde configuration, active
// et invisible : des prisons en double, ou une entrée de collecte de plus.
func (p *Provider) backupDir() string { return filepath.Join(p.Paths.Lib(), "backups") }

// backupPattern nomme l'emplacement exact de la copie, horodatage excepté :
// l'exploitant doit retrouver son fichier sans avoir à deviner où il est parti.
func (p *Provider) backupPattern(path string) string {
	return filepath.Join(p.backupDir(), filepath.Base(path)) + ".ogs-bak-<horodatage>"
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
