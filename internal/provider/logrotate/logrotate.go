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

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/fs"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/template"
)

// Provider génère /etc/logrotate.d/ogs.
type Provider struct {
	Paths     config.Paths
	Templates *template.Set
	Config    *config.Config
	Now       func() time.Time
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
	if state == fs.StateCurrent {
		return engine.NewStep("logrotate/ogs", engine.NoOp, "rotation des journaux"), nil
	}
	action := engine.Create
	if state != fs.StateAbsent {
		action = engine.Update
	}
	s := engine.NewStep("logrotate/ogs", action,
		fmt.Sprintf("rotation des journaux (%d projet(s))", len(projects)))
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
