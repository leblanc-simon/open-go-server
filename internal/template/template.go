// Package template rend les fichiers de configuration. Les templates sont
// embarqués dans le binaire et surchargeables sans recompilation.
package template

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	texttemplate "text/template"
)

//go:embed all:templates
var embedded embed.FS

// Set donne accès aux templates, en privilégiant les surcharges de
// l'administrateur sur les versions embarquées.
type Set struct {
	// OverrideDir est /etc/ogs/templates. Un fichier de même chemin relatif y
	// remplace intégralement la version embarquée.
	OverrideDir string
}

// New construit un jeu de templates.
func New(overrideDir string) *Set { return &Set{OverrideDir: overrideDir} }

// Names énumère les templates embarqués, triés.
func (s *Set) Names() []string {
	var out []string
	_ = fs.WalkDir(embedded, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".tmpl"))
		return nil
	})
	sort.Strings(out)
	return out
}

// Source indique d'où vient un template : la surcharge si elle existe, sinon la
// version embarquée. Le chemin figure dans l'en-tête des fichiers générés, pour
// que l'administrateur sache quoi éditer.
func (s *Set) Source(name string) string {
	if p := s.overridePath(name); p != "" {
		return p
	}
	return "template embarqué " + name
}

// OverridePath renvoie le chemin de surcharge attendu, qu'il existe ou non.
func (s *Set) OverridePath(name string) string {
	if s.OverrideDir == "" {
		return ""
	}
	return filepath.Join(s.OverrideDir, filepath.FromSlash(name)+".tmpl")
}

func (s *Set) overridePath(name string) string {
	p := s.OverridePath(name)
	if p == "" {
		return ""
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// Read renvoie le contenu source d'un template.
func (s *Set) Read(name string) ([]byte, error) {
	if p := s.overridePath(name); p != "" {
		return os.ReadFile(p)
	}
	data, err := embedded.ReadFile(path.Join("templates", name+".tmpl"))
	if err != nil {
		return nil, fmt.Errorf("template %q inconnu", name)
	}
	return data, nil
}

// Render rend un template. Une surcharge syntaxiquement fautive est signalée
// avec son chemin : l'erreur doit désigner le fichier que l'administrateur a
// écrit, pas une abstraction interne.
func (s *Set) Render(name string, data any) ([]byte, error) {
	src, err := s.Read(name)
	if err != nil {
		return nil, err
	}
	t, err := texttemplate.New(path.Base(name)).Funcs(Funcs()).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s : %w", s.Source(name), err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("%s : %w", s.Source(name), err)
	}
	return buf.Bytes(), nil
}

// Extract écrit la version embarquée d'un template dans le répertoire de
// surcharge, pour servir de point de départ à une personnalisation.
func (s *Set) Extract(name string) (string, error) {
	dst := s.OverridePath(name)
	if dst == "" {
		return "", fmt.Errorf("aucun répertoire de surcharge configuré")
	}
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("%s existe déjà : le supprimer pour repartir de la version embarquée", dst)
	}
	src, err := embedded.ReadFile(path.Join("templates", name+".tmpl"))
	if err != nil {
		return "", fmt.Errorf("template %q inconnu", name)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, src, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

// Funcs expose les fonctions utilisables dans les templates.
func Funcs() texttemplate.FuncMap {
	return texttemplate.FuncMap{
		"snake":      func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "-", "_"), ".", "_") },
		"kebab":      func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "_", "-"), ".", "-") },
		"upper":      strings.ToUpper,
		"lower":      strings.ToLower,
		"join":       func(sep string, items []string) string { return strings.Join(items, sep) },
		"truncate":   truncate,
		"hash":       hash6,
		"indent":     indent,
		"quote":      func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` },
		"default":    func(fallback, v string) string { return orDefault(v, fallback) },
		"sorted":     sortedKeys,
		"hasPrefix":  strings.HasPrefix,
		"hasSuffix":  strings.HasSuffix,
		"contains":   strings.Contains,
		"systemdEnv": systemdEnv,
		"fpmValue":   fpmValue,
	}
}

// fpmValue rend la valeur d'une directive de pool PHP-FPM. L'analyseur d'ini de
// FPM lit « ; » comme l'ouverture d'un commentaire et développe « ${VAR} » depuis
// l'environnement du maître : une telle valeur est rendue entre apostrophes,
// qu'il lit littéralement. Les autres restent nues et gardent leur rendu — un
// guillemet compris : « "/usr/sbin/sendmail -t -i" » est une citation de
// l'exploitant, que des apostrophes changeraient de sens. Une valeur à citer qui
// porte une apostrophe ou un guillemet est refusée en amont
// (validate.PoolValueProblem, dont c'est la règle).
func fpmValue(value string) string {
	if strings.Contains(value, ";") || strings.Contains(value, "${") {
		return "'" + value + "'"
	}
	return value
}

// systemdEnv rend l'affectation d'une directive Environment=. systemd découpe
// la ligne sur les blancs : sans guillemets, « K=deux mots » définit K=deux et
// écarte « mots » comme une affectation invalide, sans autre signe qu'une ligne
// de journal. La citation n'est posée que si la valeur l'exige, de sorte qu'une
// unité aux valeurs simples garde son rendu — et ne soit ni réécrite ni
// redémarrée. Les « % » restent des spécificateurs : « INSTANCE=%i » est un
// usage légitime.
func systemdEnv(key, value string) string {
	assignment := key + "=" + value
	if !strings.ContainsAny(value, " \t\"'\\") {
		return assignment
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(assignment) + `"`
}

func truncate(n int, s string) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

func hash6(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:6]
}

func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// sortedKeys rend l'itération sur une table déterministe : sans cela, deux
// rendus successifs différeraient et provoqueraient une réécriture inutile.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
