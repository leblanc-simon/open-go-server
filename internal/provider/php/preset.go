package php

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/validate"
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
// config.yaml les redéfinit clé par clé, ou en ajoute (« php.pool.presets »).
var presets = map[string]Preset{
	// ondemand est le preset par défaut (D103) : un pool qui ne sert rien ne
	// tient aucun worker, et la mémoire d'une machine qui porte des dizaines de
	// sites n'est plus réservée par ceux qui dorment.
	"ondemand": {Name: "ondemand", Settings: map[string]string{
		"pm":                            "ondemand",
		"pm.max_children":               "12",
		"pm.process_idle_timeout":       "30s",
		"php_admin_value[memory_limit]": "256M",
	}},
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

// downgrades est la descente d'un cran que l'environnement applique au preset
// par défaut (« presetDowngrade », D103). Un preset ajouté par config.yaml n'y
// figure pas : il reste tel quel, comme custom.
var downgrades = map[string]string{
	"large":    "medium",
	"medium":   "small",
	"small":    "micro",
	"ondemand": "micro",
}

// KnownPresets énumère les presets livrés et ceux de la configuration, triés.
func KnownPresets(cfg *config.Config) []string {
	seen := map[string]bool{}
	for k := range presets {
		seen[k] = true
	}
	for k := range poolConfig(cfg).Presets {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
		// Seuls les fichiers .php sont exécutés (D103). Le défaut de FPM admet
		// aussi .php3 à .php7 : un fichier téléversé sous l'un de ces noms, que le
		// vhost d'ogs ne route pas vers FPM, le serait par une autre configuration
		// — un vhost adopté, une surcharge (D63). Les points d'état ne sont pas des
		// scripts : FPM les sert avant ce contrôle.
		"security.limit_extensions": ".php",
		// Points d'état du pool, lus par le vhost de supervision (D66). Ils ne sont
		// atteignables depuis aucun vhost de projet : ceux-ci ne routent vers FPM
		// que les URI se terminant par « .php ». Le nom est préfixé « ogs- » pour
		// qu'un chemin applicatif ne puisse pas le recouvrir par accident.
		"pm.status_path": config.PoolStatusPath,
		"ping.path":      config.PoolPingPath,
	}
}

// DisableFunctionsKey est la directive où se rend la liste de disable_functions.
// php_admin_value : le script ne peut pas la relever par ini_set.
const DisableFunctionsKey = "php_admin_value[disable_functions]"

// Problem est un réglage qui n'a pas pu être rendu dans un pool.
type Problem struct {
	// Field situe le réglage : « spec.php.pools[web].settings.user », ou
	// « config.yaml php.pool.defaults.slowlog ».
	Field   string
	Message string
	// Blocking vaut vrai pour ce que le plan du projet refuse — une déclaration
	// du projet qui injecterait une directive, un preset déclaré inconnu. Le
	// reste — clé verrouillée, réglage de la machine illisible — est écarté du
	// rendu et nommé par une étape non critique (code 6).
	Blocking bool
}

func (p Problem) String() string { return p.Field + " : " + p.Message }

// Resolved est le calcul d'un pool, hors numéro et point d'écoute.
type Resolved struct {
	// Preset est le preset retenu ; Inherited dit qu'il vient de la machine (le
	// pool n'en déclare pas), Downgraded qu'il a été descendu par l'environnement.
	Preset     string
	Inherited  bool
	Downgraded bool
	Settings   map[string]string
	Env        map[string]string
	Problems   []Problem
}

// Blocking rend les problèmes que le plan du projet refuse.
func (r Resolved) Blocking() []Problem {
	var out []Problem
	for _, pb := range r.Problems {
		if pb.Blocking {
			out = append(out, pb)
		}
	}
	return out
}

// Warnings rend les problèmes écartés du rendu.
func (r Resolved) Warnings() []Problem {
	var out []Problem
	for _, pb := range r.Problems {
		if !pb.Blocking {
			out = append(out, pb)
		}
	}
	return out
}

// DescribePreset rend le preset retenu tel qu'un affichage le nomme : déclaré,
// hérité de la machine, ou hérité puis descendu par l'environnement.
func (r Resolved) DescribePreset() string {
	switch {
	case r.Downgraded:
		return r.Preset + " (défaut de la machine, descendu d'un cran)"
	case r.Inherited:
		return r.Preset + " (défaut de la machine)"
	}
	return r.Preset
}

func poolConfig(cfg *config.Config) config.PHPPool {
	if cfg == nil {
		return config.PHPPool{}
	}
	return cfg.PHP.Pool
}

// Resolve calcule les réglages et l'environnement d'un pool, couche par couche
// (D103) : socle d'ogs → défauts de la machine → preset → type d'application
// (disable_functions compris) → environnement (la descente du preset par
// défaut) → déclaration du projet, puis les fonctions rouvertes. Une valeur
// nulle retire la clé héritée ; une clé verrouillée ne se surcharge à aucun
// étage. cfg nil vaut la configuration livrée.
//
// Resolve n'échoue jamais sur un réglage : il rend ce qu'il a pu calculer et la
// liste de ce qu'il a écarté. Le vhost de supervision, calculé pour tout le parc,
// passe par lui — un pool mal déclaré ne doit pas arrêter le plan global.
func Resolve(cfg *config.Config, proj *model.Project, pool model.Pool, at model.AppType) Resolved {
	pc := poolConfig(cfg)
	r := Resolved{Settings: baseSettings(proj.Spec.Layout.Root, proj.Spec.Layout.LogDir)}
	projectField := fmt.Sprintf("spec.php.pools[%s]", pool.Name)

	apply := func(layer model.PoolSettings, field string, fromProject bool) {
		r.Problems = append(r.Problems, applyLayer(r.Settings, layer, field, fromProject)...)
	}

	apply(pc.Defaults, "config.yaml php.pool.defaults", false)

	// Preset : déclaré, il doit exister ; hérité de la machine, il est descendu
	// d'un cran si l'environnement le demande, et un défaut inconnu retombe sur
	// le preset livré.
	name := pool.Preset
	if name == "" {
		r.Inherited = true
		name = pc.PresetName()
		if !presetExists(pc, name) {
			r.Problems = append(r.Problems, Problem{Field: "config.yaml php.pool.defaultPreset",
				Message: fmt.Sprintf("preset %q inconnu (connus : %s) — %s retenu",
					name, strings.Join(KnownPresets(cfg), ", "), config.DefaultPoolPreset)})
			name = config.DefaultPoolPreset
		}
		if cfg != nil && cfg.Environments[string(proj.Env())].PresetDowngrade {
			if down, ok := downgrades[name]; ok {
				name, r.Downgraded = down, true
			}
		}
	}
	r.Preset = name
	if !presetExists(pc, name) {
		r.Problems = append(r.Problems, Problem{Field: projectField + ".preset",
			Message:  fmt.Sprintf("preset %q inconnu (connus : %s)", name, strings.Join(KnownPresets(cfg), ", ")),
			Blocking: true})
	} else {
		for k, v := range presets[name].Settings {
			r.Settings[k] = v
		}
		apply(pc.Presets[name], "config.yaml php.pool.presets."+name, false)
	}

	for k, v := range at.PHPSettings {
		r.Settings[k] = v
	}
	if at.DisableFunctions {
		var list []string
		for _, fn := range pc.DisabledFunctions() {
			if msg := validate.FunctionNameProblem(fn); msg != "" {
				r.Problems = append(r.Problems, Problem{Field: "config.yaml php.pool.disableFunctions",
					Message: msg + " — écarté"})
				continue
			}
			list = append(list, fn)
		}
		if len(list) > 0 {
			r.Settings[DisableFunctionsKey] = strings.Join(list, ",")
		}
	}

	apply(pool.Settings, projectField+".settings", true)

	// Les fonctions rouvertes s'ôtent de la liste rendue, d'où qu'elle vienne :
	// du type, des défauts de la machine ou de la déclaration elle-même.
	if len(pool.AllowFunctions) > 0 {
		allow := map[string]bool{}
		for _, fn := range pool.AllowFunctions {
			if msg := validate.FunctionNameProblem(fn); msg != "" {
				r.Problems = append(r.Problems, Problem{Field: projectField + ".allowFunctions",
					Message: msg, Blocking: true})
				continue
			}
			// PHP compare les noms de fonctions sans tenir compte de la casse.
			allow[strings.ToLower(fn)] = true
		}
		closed := map[string]bool{}
		if current, ok := r.Settings[DisableFunctionsKey]; ok {
			var kept []string
			for _, fn := range strings.Split(current, ",") {
				fn = strings.TrimSpace(fn)
				closed[strings.ToLower(fn)] = true
				if fn != "" && !allow[strings.ToLower(fn)] {
					kept = append(kept, fn)
				}
			}
			if len(kept) == 0 {
				delete(r.Settings, DisableFunctionsKey)
			} else {
				r.Settings[DisableFunctionsKey] = strings.Join(kept, ",")
			}
		}
		// Une fonction rouverte que le pool ne fermait pas ne rouvre rien : le
		// dire, plutôt que de laisser croire à un effet.
		for _, fn := range pool.AllowFunctions {
			if validate.FunctionNameProblem(fn) == "" && !closed[strings.ToLower(fn)] {
				r.Problems = append(r.Problems, Problem{Field: projectField + ".allowFunctions",
					Message: fmt.Sprintf("%s n'est pas dans le disable_functions du pool : rien à rouvrir", fn)})
			}
		}
	}

	r.Env = map[string]string{}
	if at.EnvVar != "" {
		// « prod » en préproduction aussi (D96) : le site y est public, et un code
		// installé sans les dépendances de dev ne démarre pas en « dev ».
		r.Env[at.EnvVar] = "prod"
	}
	envKeys := make([]string, 0, len(pool.Env))
	for k := range pool.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		v := pool.Env[k]
		if msg := validate.PoolEnvProblem(k, v); msg != "" {
			r.Problems = append(r.Problems, Problem{Field: projectField + ".env." + fmt.Sprintf("%q", k),
				Message: msg + " — écartée", Blocking: true})
			continue
		}
		r.Env[k] = v
	}
	return r
}

func presetExists(pc config.PHPPool, name string) bool {
	if _, ok := presets[name]; ok {
		return true
	}
	_, ok := pc.Presets[name]
	return ok
}

// ConfigProblems énumère ce que la section « php.pool » de config.yaml porte
// et que les pools ne rendront pas : clés verrouillées ou hors grammaire,
// valeurs sur plusieurs lignes, preset par défaut inconnu, nom de fonction
// invalide. La configuration se charge malgré eux (D103) ; « doctor » les nomme,
// et le plan de chaque pool touché aussi.
func ConfigProblems(cfg *config.Config) []Problem {
	pc := poolConfig(cfg)
	var out []Problem
	// Les couches sont éprouvées comme le rendu les traite, sur le socle d'un
	// projet quelconque : c'est le même jugement, au mot près.
	layer := func(settings model.PoolSettings, field string) {
		out = append(out, applyLayer(baseSettings("/", "/"), settings, field, false)...)
	}
	layer(pc.Defaults, "config.yaml php.pool.defaults")
	names := make([]string, 0, len(pc.Presets))
	for name := range pc.Presets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !validate.PresetName(name) {
			out = append(out, Problem{Field: "config.yaml php.pool.presets",
				Message: fmt.Sprintf("nom de preset %q invalide : aucun projet ne peut le déclarer", name)})
		}
		layer(pc.Presets[name], "config.yaml php.pool.presets."+name)
	}
	if name := pc.PresetName(); !presetExists(pc, name) {
		out = append(out, Problem{Field: "config.yaml php.pool.defaultPreset",
			Message: fmt.Sprintf("preset %q inconnu (connus : %s) — %s retenu",
				name, strings.Join(KnownPresets(cfg), ", "), config.DefaultPoolPreset)})
	}
	for _, fn := range pc.DisabledFunctions() {
		if msg := validate.FunctionNameProblem(fn); msg != "" {
			out = append(out, Problem{Field: "config.yaml php.pool.disableFunctions", Message: msg})
		}
	}
	return out
}

// undeletable énumère les clés qu'une valeur nulle ne retire pas : les points
// d'état que le vhost de supervision relaie (D66). Retirées, FPM ne les servirait
// plus et le vhost relaierait vers un chemin mort ; une autre valeur reste admise,
// et le vhost la suit.
var undeletable = map[string]bool{"pm.status_path": true, "ping.path": true}

// applyLayer pose une couche de réglages sur dst et rend ce qu'elle a écarté.
//
// FPM compare les noms de directives **sans tenir compte de la casse** : « User =
// root » après « user = alice » est accepté par « php-fpm -t » et fait tourner le
// pool en root. Chaque clé est donc ramenée à sa forme canonique — minuscules hors
// crochets, le nom entre crochets étant celui d'une directive d'ini ou d'une
// variable, sensibles à la casse — avant tout jugement, et une directive n'est
// rendue qu'une fois, la dernière couche l'emportant.
func applyLayer(dst map[string]string, layer model.PoolSettings, field string, fromProject bool) []Problem {
	var out []Problem
	keys := make([]string, 0, len(layer))
	for k := range layer {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seen := map[string]string{}
	for _, k := range keys {
		v := layer[k]
		if msg := validate.PoolKeyProblem(k); msg != "" {
			out = append(out, Problem{Field: field + "." + fmt.Sprintf("%q", k),
				Message: msg + " — écartée", Blocking: fromProject})
			continue
		}
		ck := validate.CanonicalPoolKey(k)
		if prev, dup := seen[ck]; dup {
			out = append(out, Problem{Field: field + "." + k,
				Message: fmt.Sprintf("même directive que %s à la casse près (FPM ne distingue pas) — %s retenue", prev, k)})
		}
		seen[ck] = k
		if why, locked := validate.LockedPoolKey(ck); locked {
			out = append(out, Problem{Field: field + "." + k,
				Message: "clé verrouillée (" + why + ") — écartée, la valeur d'ogs est rendue"})
			continue
		}
		if v == nil {
			if undeletable[ck] {
				out = append(out, Problem{Field: field + "." + k,
					Message: "ne se retire pas : le vhost de supervision la relaie (D66) — valeur héritée rendue"})
				continue
			}
			if _, inherited := dst[ck]; !inherited && fromProject {
				// Avant D103, « clé: » sans valeur valait une chaîne vide.
				out = append(out, Problem{Field: field + "." + k,
					Message: "valeur nulle sans clé héritée : rien à retirer (une chaîne vide s'écrit \"\")"})
			}
			delete(dst, ck)
			continue
		}
		if msg := validate.PoolValueProblem(*v); msg != "" {
			out = append(out, Problem{Field: field + "." + k,
				Message: msg + " — écartée", Blocking: fromProject})
			continue
		}
		dst[ck] = *v
	}
	return out
}
