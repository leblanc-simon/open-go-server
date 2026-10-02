package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// retiredKeys énumère, par motif de chemin (« * » vaut un nom quelconque), les
// clés qu'une version antérieure d'« ogs init » écrivait et qui ont été retirées
// depuis. Une configuration écrite alors les porte encore sans que
// l'administrateur y ait touché : elles sont nommées, jamais refusées.
var retiredKeys = []string{
	"environments.*.basicAuth", // retirée par b49b1d0 : les préproductions sont publiques (D57)
}

// UnknownKeys rend, par chemin (« monitoring.fail2ban.maxretry »), les clés du
// document que Config ne connaît pas, et à part celles qui ont été retirées
// (retiredKeys). Le décodage de config.yaml n'est pas strict : une clé mal
// orthographiée y était acceptée et son réglage perdu en silence (Q-34). La
// rendre fatale au chargement arrêterait tout le parc, le renouvellement des
// certificats compris : c'est l'appelant qui décide du sort de chaque commande
// (D101).
//
// Le parcours suit les étiquettes yaml de Config, dans les tables et les listes
// comme dans les structures ; il s'arrête sur tout type qu'il ne sait pas
// décrire, sans rien y signaler.
func UnknownKeys(data []byte) (unknown, retired []string, err error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	walkKeys(doc, reflect.TypeOf(Config{}), "", func(path string) {
		if isRetired(path) {
			retired = append(retired, path)
			return
		}
		unknown = append(unknown, path)
	})
	sort.Strings(unknown)
	sort.Strings(retired)
	return unknown, retired, nil
}

// UnknownKeysError rend l'erreur qui nomme les clés inconnues d'un fichier, ou
// nil s'il n'en porte aucune.
func UnknownKeysError(path string, unknown []string) error {
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("%s : clé(s) inconnue(s), dont le réglage serait ignoré : %s",
		path, strings.Join(unknown, ", "))
}

func isRetired(path string) bool {
	parts := strings.Split(path, ".")
	for _, pattern := range retiredKeys {
		p := strings.Split(pattern, ".")
		if len(p) != len(parts) {
			continue
		}
		match := true
		for i := range p {
			if p[i] != "*" && p[i] != parts[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func walkKeys(node any, t reflect.Type, path string, report func(string)) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	join := func(key string) string {
		if path == "" {
			return key
		}
		return path + "." + key
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		fields := yamlFields(t)
		for key, value := range m {
			ft, known := fields[key]
			if !known {
				// Une faute de casse — « maxretry » pour « maxRetry » — se corrige
				// d'un mot : le dire.
				if name := foldMatch(fields, key); name != "" {
					report(join(key) + " (« " + name + " » ?)")
				} else {
					report(join(key))
				}
				continue
			}
			walkKeys(value, ft, join(key), report)
		}
	case reflect.Map:
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		for key, value := range m {
			walkKeys(value, t.Elem(), join(key), report)
		}
	case reflect.Slice, reflect.Array:
		list, ok := node.([]any)
		if !ok {
			return
		}
		for i, value := range list {
			walkKeys(value, t.Elem(), fmt.Sprintf("%s[%d]", path, i), report)
		}
	}
}

// foldMatch rend le champ qui ne diffère de key que par la casse, vide sinon.
func foldMatch(fields map[string]reflect.Type, key string) string {
	for name := range fields {
		if strings.EqualFold(name, key) {
			return name
		}
	}
	return ""
}

// yamlFields rend les champs d'une structure par leur clé yaml, comme le
// décodeur les nomme : l'étiquette, sinon le nom du champ en minuscules.
func yamlFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if strings.Contains(opts, "inline") {
			for k, v := range yamlFields(f.Type) {
				out[k] = v
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f.Type
	}
	return out
}
