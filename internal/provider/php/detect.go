// Package php porte le provider PHP-FPM : détection des versions installées,
// presets de dimensionnement et génération des pools.
package php

import (
	"os"
	"path/filepath"
	"sort"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/validate"
)

// Version décrit une version PHP observée sur la machine.
type Version struct {
	Version string `json:"version" yaml:"version"`
	// Usable vaut vrai si la configuration FPM et les binaires sont présents.
	Usable  bool   `json:"usable" yaml:"usable"`
	PoolDir string `json:"poolDir" yaml:"poolDir"`
	CLI     string `json:"cli" yaml:"cli"`
	FPM     string `json:"fpm" yaml:"fpm"`
	Unit    string `json:"unit" yaml:"unit"`
	Pools   int    `json:"pools" yaml:"pools"`
	// EOL porte la date de fin de support si elle est passée.
	EOL string `json:"eol,omitempty" yaml:"eol,omitempty"`
	// Missing détaille ce qui manque quand la version n'est pas utilisable.
	Missing []string `json:"missing,omitempty" yaml:"missing,omitempty"`
}

// Detect inspecte /etc/php et les binaires associés. Une version est utilisable
// dès que sa configuration FPM et ses binaires existent ; l'état du service est
// du ressort de systemd, donc hors de cette détection.
func Detect(paths config.Paths) []Version {
	entries, err := os.ReadDir(paths.PHPRoot())
	if err != nil {
		return nil
	}
	var out []Version
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v := Version{
			Version: e.Name(),
			PoolDir: paths.PHPPoolDir(e.Name()),
			CLI:     paths.PHPBin(e.Name()),
			FPM:     paths.PHPFpmBin(e.Name()),
			Unit:    config.PHPUnit(e.Name()),
		}
		if _, err := os.Stat(v.PoolDir); err != nil {
			v.Missing = append(v.Missing, "configuration FPM ("+v.PoolDir+")")
		}
		if _, err := os.Stat(v.CLI); err != nil {
			v.Missing = append(v.Missing, "interpréteur "+v.CLI)
		}
		if _, err := os.Stat(v.FPM); err != nil {
			v.Missing = append(v.Missing, "binaire "+v.FPM)
		}
		v.Usable = len(v.Missing) == 0
		if date, eol := validate.EOLDate(v.Version); eol {
			v.EOL = date
		}
		if pools, err := os.ReadDir(v.PoolDir); err == nil {
			for _, p := range pools {
				if filepath.Ext(p.Name()) == ".conf" {
					v.Pools++
				}
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// Find renvoie une version détectée.
func Find(versions []Version, want string) (Version, bool) {
	for _, v := range versions {
		if v.Version == want {
			return v, true
		}
	}
	return Version{}, false
}

// Managed rend les versions qu'un projet peut déclarer (« php.versions » de
// config.yaml, D125). cfg nil vaut la configuration livrée.
func Managed(cfg *config.Config) []string {
	if cfg == nil {
		return config.DefaultPHPVersions
	}
	return cfg.PHP.PHPVersions()
}

// IsManaged indique si une version fait partie des versions déclarables.
func IsManaged(cfg *config.Config, version string) bool {
	for _, v := range Managed(cfg) {
		if v == version {
			return true
		}
	}
	return false
}
