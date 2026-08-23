package php

import (
	"fmt"
	"path/filepath"
	"sort"
)

// Preset est un dimensionnement nommé de pool.
type Preset struct {
	Name     string
	Settings map[string]string
}

// presets reprend les dimensionnements du cahier des charges. Le pool étant
// l'endroit où se joue la tenue en charge d'un site, ces valeurs sont explicites
// plutôt que déduites de la mémoire disponible : un dimensionnement implicite
// varie d'une machine à l'autre et rend les incidents difficiles à reproduire.
var presets = map[string]Preset{
	"micro": {Name: "micro", Settings: map[string]string{
		"pm":                            "ondemand",
		"pm.max_children":               "5",
		"pm.process_idle_timeout":       "10s",
		"php_admin_value[memory_limit]": "128M",
	}},
	"small": {Name: "small", Settings: map[string]string{
		"pm":                            "dynamic",
		"pm.max_children":               "10",
		"pm.start_servers":              "2",
		"pm.min_spare_servers":          "1",
		"pm.max_spare_servers":          "3",
		"php_admin_value[memory_limit]": "192M",
	}},
	"medium": {Name: "medium", Settings: map[string]string{
		"pm":                            "dynamic",
		"pm.max_children":               "20",
		"pm.start_servers":              "4",
		"pm.min_spare_servers":          "2",
		"pm.max_spare_servers":          "6",
		"php_admin_value[memory_limit]": "256M",
	}},
	"large": {Name: "large", Settings: map[string]string{
		"pm":                            "dynamic",
		"pm.max_children":               "50",
		"pm.start_servers":              "10",
		"pm.min_spare_servers":          "5",
		"pm.max_spare_servers":          "15",
		"php_admin_value[memory_limit]": "512M",
	}},
	"custom": {Name: "custom", Settings: map[string]string{}},
}

// PresetNames énumère les presets, triés.
func PresetNames() []string {
	out := make([]string, 0, len(presets))
	for k := range presets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LookupPreset renvoie un preset.
func LookupPreset(name string) (Preset, error) {
	if name == "" {
		name = "medium"
	}
	p, ok := presets[name]
	if !ok {
		return Preset{}, fmt.Errorf("preset %q inconnu (connus : %v)", name, PresetNames())
	}
	return p, nil
}

// baseSettings sont les réglages communs à tous les pools : journalisation,
// cloisonnement et garde-fous de durée. open_basedir limite l'accès au home du
// projet, ce qui contient les dégâts d'une inclusion de fichier malveillante.
func baseSettings(root, logDir string) map[string]string {
	return map[string]string{
		"pm.max_requests":                    "500",
		"php_admin_flag[log_errors]":         "on",
		"php_admin_value[error_log]":         filepath.Join(logDir, "php-error.log"),
		"php_admin_value[open_basedir]":      root + ":/tmp:/usr/share/php",
		"php_admin_value[upload_tmp_dir]":    filepath.Join(root, "var", "tmp"),
		"php_admin_value[session.save_path]": filepath.Join(root, "var", "sessions"),
		"php_admin_value[sys_temp_dir]":      filepath.Join(root, "var", "tmp"),
		"slowlog":                            filepath.Join(logDir, "php-slow.log"),
		"request_slowlog_timeout":            "10s",
		"request_terminate_timeout":          "300s",
		"catch_workers_output":               "yes",
		"decorate_workers_output":            "no",
	}
}

// mergeSettings applique, dans l'ordre : réglages communs, preset, réglages du
// type d'application, puis surcharges du projet. Chaque étage peut redéfinir le
// précédent, la déclaration du projet ayant toujours le dernier mot.
func mergeSettings(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, l := range layers {
		for k, v := range l {
			out[k] = v
		}
	}
	return out
}
