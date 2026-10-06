package php

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/template"
)

// DefaultSessionLifetime est le session.gc_maxlifetime de PHP, en secondes,
// quand ni le pool ni le php.ini de FPM n'en fixent un.
const DefaultSessionLifetime = 1440

// SessionLifetime rend l'âge, en secondes, au-delà duquel les sessions d'un
// projet se purgent (D115) — zéro si aucun pool ne les range dans le répertoire
// qu'ogs lui donne —, et faux pour un type sans PHP.
//
// Debian règle session.gc_probability à 0 et confie la purge à sessionclean,
// qui ne lit que le php.ini et le save_path qu'il désigne : déplacé dans le
// pool, le répertoire d'un projet n'était jamais purgé. L'âge est le
// gc_maxlifetime effectif — celui du pool, sinon celui du php.ini de FPM de sa
// version et de son conf.d, sinon le défaut de PHP —, le plus long des pools qui partagent le
// répertoire : une session n'y est jamais retirée avant que son pool ne la tienne
// pour expirée. Une valeur que l'application relève par ini_set n'est pas vue,
// comme sous sessionclean : elle se déclare dans le pool.
func SessionLifetime(cfg *config.Config, paths config.Paths, proj *model.Project) (age int, php bool) {
	at, err := proj.AppType()
	if err != nil || !at.NeedsPHP {
		return 0, false
	}
	dir := paths.ProjectSessionsDir(proj.ID())
	for _, pool := range proj.Spec.PHP.Pools {
		settings := Resolve(cfg, paths, proj, pool, at).Settings
		if handler, ok := iniSetting(settings, "session.save_handler"); ok && handler != "files" {
			continue
		}
		if path, ok := iniSetting(settings, "session.save_path"); !ok || filepath.Clean(path) != dir {
			continue
		}
		lifetime := DefaultSessionLifetime
		raw, ok := iniSetting(settings, "session.gc_maxlifetime")
		if !ok {
			raw, ok = fpmIniValue(paths, pool.Version, "session.gc_maxlifetime")
		}
		if ok {
			if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n > 0 {
				lifetime = n
			}
		}
		age = max(age, lifetime)
	}
	return age, true
}

// iniSetting rend ce que FPM lit d'une directive d'ini du pool, la valeur
// d'administration l'emportant : php_value ne la relève pas.
func iniSetting(settings map[string]string, name string) (string, bool) {
	for _, key := range []string{"php_admin_value[" + name + "]", "php_value[" + name + "]"} {
		if v, ok := settings[key]; ok {
			return template.FPMRead(v), true
		}
	}
	return "", false
}

// fpmIniValue rend la valeur d'une directive pour le FPM d'une version, comme
// PHP la retient : php.ini, puis les fichiers de conf.d dans l'ordre de leur nom,
// le dernier l'emportant.
func fpmIniValue(paths config.Paths, version, name string) (string, bool) {
	dir := filepath.Join(paths.PHPRoot(), version, "fpm")
	files := []string{filepath.Join(dir, "php.ini")}
	extra, _ := filepath.Glob(filepath.Join(dir, "conf.d", "*.ini"))
	sort.Strings(extra)
	value, found := "", false
	for _, f := range append(files, extra...) {
		if v, ok := iniFileValue(f, name); ok {
			value, found = v, true
		}
	}
	return value, found
}

// iniFileValue rend la dernière valeur d'une directive d'un fichier php.ini,
// commentaire et guillemets ôtés.
func iniFileValue(path, name string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	value, found := "", false
	for _, line := range strings.Split(string(data), "\n") {
		key, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		if i := strings.Index(v, ";"); i >= 0 {
			v = v[:i]
		}
		value, found = strings.Trim(strings.TrimSpace(v), `"'`), true
	}
	return value, found
}

// SessionsRootMode est le mode de la racine des sessions : traversable, pour
// que chaque pool atteigne son répertoire, sans être énumérable.
const SessionsRootMode os.FileMode = 0o711

// SessionsDirMode est le mode du répertoire des sessions d'un projet : à son
// compte seul. Il est nommé ici, une seule fois : la ligne tmpfiles et la
// création immédiate en décident ensemble, sans quoi le mode oscillerait.
const SessionsDirMode os.FileMode = 0o700

// EnsureSessionsDir pose le répertoire des sessions d'un projet, avec le mode et
// le propriétaire que déclare la ligne tmpfiles : le pool qui le désigne doit le
// trouver dès son premier rechargement, avant le plan global.
func EnsureSessionsDir(paths config.Paths, proj *model.Project) error {
	if err := fs.EnsureDir(paths.SessionsRoot(), SessionsRootMode, fs.KeepOwner); err != nil {
		return err
	}
	return fs.EnsureDir(paths.ProjectSessionsDir(proj.ID()), SessionsDirMode,
		fs.LookupOwner(proj.Spec.User.Name, proj.Spec.User.Group))
}
