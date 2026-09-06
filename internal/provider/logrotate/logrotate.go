// Package logrotate génère la rotation des journaux de tous les projets, dans un
// fichier unique.
package logrotate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/template"
)

// Provider génère /etc/logrotate.d/ogs.
type Provider struct {
	Paths     config.Paths
	Templates *template.Set
	Config    *config.Config
	Now       func() time.Time
	// Force autorise l'écrasement d'un fichier qu'ogs n'a pas écrit, après
	// sauvegarde horodatée (§6.4). Sans lui, le refus n'aurait aucune porte de
	// sortie et le plan global resterait bloqué.
	Force bool
}

// refusStep produit l'étape qui refuse d'écraser un fichier qu'ogs n'a pas
// écrit. Le fichier est recalculé depuis le parc entier, mais une directive
// ajoutée à la main — une taille, un « postrotate » — n'en disparaîtrait pas
// moins sans un mot. Le message distingue les deux causes : un fichier « non
// géré » n'a jamais porté la marque d'ogs, un fichier « modifié hors ogs » a été
// retouché après avoir été généré.
func refusStep(resource, summary, path, backups string, state fs.State) *engine.Step {
	libelle, motif := "modifié hors ogs", "a été modifié en dehors d'ogs"
	if state == fs.StateForeign {
		libelle, motif = "non géré par ogs", "existe déjà sans porter la marque d'ogs"
	}
	s := engine.NewStep(resource, engine.Update, summary+" — fichier "+libelle)
	s.Apply = func(context.Context) error {
		return fmt.Errorf("%s %s : relancer avec --force pour l'écraser "+
			"(l'original sera sauvegardé dans %s)", path, motif, backups)
	}
	return s
}

// Name identifie le provider.
func (p *Provider) Name() string { return "logrotate" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Build calcule le contenu attendu du fichier de rotation pour l'ensemble des
// projets. Les chemins sont énumérés explicitement : un glob sur /home/*/logs/
// embarquerait les répertoires d'utilisateurs humains et permettrait de
// déclencher les scripts de rotation depuis un compte non géré.
func (p *Provider) Build(projects []*model.Project, webEngine, webUnit string) ([]byte, error) {
	var serverLogs, appLogs []string
	phpVersions := map[string]bool{}
	for _, proj := range projects {
		logDir := proj.Spec.Layout.LogDir
		if logDir == "" {
			continue
		}
		at, err := proj.AppType()
		if err != nil {
			return nil, err
		}
		if webEngine != "" {
			serverLogs = append(serverLogs,
				filepath.Join(logDir, webEngine+"-access.log"),
				filepath.Join(logDir, webEngine+"-error.log"))
		}
		if at.NeedsPHP {
			serverLogs = append(serverLogs,
				filepath.Join(logDir, "php-error.log"),
				filepath.Join(logDir, "php-slow.log"))
			for _, pool := range proj.Spec.PHP.Pools {
				phpVersions[pool.Version] = true
			}
		}
		// Les tâches réouvrent leur fichier à chaque exécution : aucun signal
		// n'est nécessaire après rotation.
		appLogs = append(appLogs, filepath.Join(logDir, "cron-*.log"))
	}
	sort.Strings(serverLogs)
	sort.Strings(appLogs)

	var units []string
	if webUnit != "" && len(serverLogs) > 0 {
		units = append(units, webUnit)
	}
	versions := make([]string, 0, len(phpVersions))
	for v := range phpVersions {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	for _, v := range versions {
		units = append(units, config.PHPUnit(v))
	}

	logs := p.Config.Logs
	if logs.Rotate == 0 {
		logs.Rotate = 30
	}
	if logs.MaxSize == "" {
		logs.MaxSize = "200M"
	}
	body, err := p.Templates.Render("logrotate/ogs.conf", map[string]any{
		"ServerLogs":  serverLogs,
		"AppLogs":     appLogs,
		"ReloadUnits": units,
		"Rotate":      logs.Rotate,
		"MaxSize":     logs.MaxSize,
		"Compress":    logs.Compress,
	})
	if err != nil {
		return nil, err
	}
	return fs.Render(fs.Header{
		Project:   fmt.Sprintf("%d projet(s)", len(projects)),
		Resource:  "logrotate/ogs",
		Generated: p.now(),
		Template:  p.Templates.OverridePath("logrotate/ogs.conf"),
	}, body, "#"), nil
}

// Plan renvoie l'étape de mise à jour du fichier de rotation, ou une étape sans
// effet s'il est déjà conforme.
func (p *Provider) Plan(projects []*model.Project, webEngine, webUnit string) (*engine.Step, error) {
	content, err := p.Build(projects, webEngine, webUnit)
	if err != nil {
		return nil, err
	}
	path := p.Paths.LogrotateFile()
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("rotation des journaux (%d projet(s))", len(projects))
	forced := false
	switch state {
	case fs.StateCurrent:
		return engine.NewStep("logrotate/ogs", engine.NoOp, "rotation des journaux"), nil
	case fs.StateHandEdited, fs.StateForeign:
		if !p.Force {
			return refusStep("logrotate/ogs", summary, path, p.Paths.BackupsDir(), state), nil
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
	s := engine.NewStep("logrotate/ogs", action, summary)
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
			// l'opération irréversible : la sauvegarde précède l'écriture. Elle
			// sort du répertoire : logrotate lit tout /etc/logrotate.d, et une
			// copie chargée comme seconde rotation des mêmes journaux arrêterait
			// celle de tout le parc.
			if _, err := fs.BackupTo(path, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
	return s, nil
}
