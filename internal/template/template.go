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
	"regexp"
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
		"fpmEnv":     fpmEnv,
	}
}

// fpmValue rend la valeur d'une directive de pool PHP-FPM (D115) : fpmValue
// <clé> <valeur>, ou fpmValue <valeur> pour un template surchargé écrit avant
// D115, qui est alors jugée comme un réglage d'ini ordinaire.
func fpmValue(args ...string) (string, error) {
	switch len(args) {
	case 1:
		return FPMValue("", args[0]), nil
	case 2:
		return FPMValue(args[0], args[1]), nil
	}
	return "", fmt.Errorf("fpmValue attend une clé et une valeur, ou une valeur")
}

// fpmEnv rend la valeur d'une variable env[] du pool.
func fpmEnv(value string) string { return FPMValue("env", value) }

var (
	// fpmQuotedForm est une valeur écrite par l'exploitant dans la syntaxe d'ini
	// de FPM : une suite de chaînes entre guillemets (échappements « \" », « \\ »,
	// « \$ ») ou entre apostrophes, que l'analyseur colle bout à bout.
	fpmQuotedForm = regexp.MustCompile(`^\s*(?:"(?:[^"\\]|\\.)*"|'[^']*')(?:\s*(?:"(?:[^"\\]|\\.)*"|'[^']*'))*\s*$`)
	// fpmBare est une valeur que l'analyseur d'ini rend telle qu'il la lit, hors
	// mots-clés et constantes : chemins, tailles, durées, listes.
	fpmBare = regexp.MustCompile(`^[A-Za-z0-9_./:,@%+-]+$`)
	// fpmConstant est un nom de constante, que l'analyseur remplace par sa
	// valeur s'il est défini.
	fpmConstant = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	// fpmExpression est une expression de constantes d'ini — « E_ALL &
	// ~E_DEPRECATED » —, que l'analyseur évalue : error_reporting attend le
	// nombre, et PHP lit une chaîne par atoi : 0, plus aucune erreur rapportée.
	fpmExpression = regexp.MustCompile(`^[A-Z0-9_ \t&|^~!()]*[A-Z_][A-Z0-9_ \t&|^~!()]*$`)
	// fpmEscaper cite une valeur entre guillemets. « ${ » s'y coupe en deux
	// chaînes collées, « $""{ » : avant PHP 8.1, « \${ » en tête de chaîne garde
	// la barre et développe la variable.
	fpmEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `${`, `$""{`)
)

// fpmKeywords sont les mots que l'analyseur d'ini remplace par « 1 » ou « ».
var fpmKeywords = map[string]bool{"true": true, "false": true, "on": true, "off": true, "yes": true,
	"no": true, "none": true, "null": true}

// FPMQuotedForm dit si une valeur est déjà citée par l'exploitant dans la
// syntaxe d'ini (« "/usr/sbin/sendmail -t -i" », D103) : elle est rendue telle
// quelle, et FPM en retire les guillemets.
func FPMQuotedForm(value string) bool { return fpmQuotedForm.MatchString(value) }

// FPMRead rend ce que FPM lit d'une valeur rendue par FPMValue, hors mots-clés
// et constantes : la valeur elle-même, ou, citée par l'exploitant, ses chaînes
// mises bout à bout, échappements « \" », « \\ » et « \$ » résolus.
func FPMRead(value string) string {
	if !FPMQuotedForm(value) {
		return value
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\'':
			j := strings.IndexByte(value[i+1:], '\'')
			b.WriteString(value[i+1 : i+1+j])
			i += j + 1
		case '"':
			for i++; value[i] != '"'; i++ {
				if value[i] == '\\' && i+1 < len(value) && strings.IndexByte(`"\\$`, value[i+1]) >= 0 {
					i++
				}
				b.WriteByte(value[i])
			}
		}
	}
	return b.String()
}

// FPMValue rend la valeur d'une directive de pool de sorte que FPM lise celle
// que la déclaration porte (D115). L'analyseur d'ini de FPM lit « ; » comme un
// commentaire, développe « ${VAR} » depuis l'environnement du maître, évalue
// « & | ^ ~ ! ( ) » et les constantes, change true, on, yes… en « 1 » et
// false, off, no, none, null en chaîne vide, et refuse « = » ou une apostrophe
// seule : une valeur nue n'arrive intacte que si elle n'en porte aucun. Toute
// autre valeur est rendue entre guillemets, « \ » et « " » échappés, « ${ »
// coupé. Restent nues :
//   - une valeur déjà citée par l'exploitant (FPMQuotedForm) ;
//   - un mot-clé booléen dans un contexte booléen — directive de FPM,
//     php_flag, php_admin_flag — où il veut dire ce que l'analyseur en fait ;
//   - une expression de constantes en capitales, hors env[] : error_reporting
//     attend le nombre qu'elle vaut.
//
// key est la directive (forme canonique), ou « env » pour une variable.
func FPMValue(key, value string) string {
	env := key == "env" || strings.HasPrefix(key, "env[")
	switch {
	case FPMQuotedForm(value):
		return value
	case value == "":
		return `""`
	case fpmKeywords[strings.ToLower(value)]:
		if !env && fpmBooleanKey(key) {
			return value
		}
	case !env && fpmExpression.MatchString(value):
		return value
	case fpmBare.MatchString(value):
		// Un nom de constante en capitales serait remplacé par sa valeur : nu
		// hors env[] seulement, comme une expression.
		if !env || !fpmConstant.MatchString(value) {
			return value
		}
	}
	return `"` + fpmEscaper.Replace(value) + `"`
}

// fpmBooleanKey dit si une directive attend un booléen : celles de FPM lui-même
// (sans crochets) et les php_flag.
func fpmBooleanKey(key string) bool {
	if key == "" {
		return false
	}
	return !strings.Contains(key, "[") || strings.HasPrefix(key, "php_flag[") ||
		strings.HasPrefix(key, "php_admin_flag[")
}

// systemdEnv rend l'affectation d'une directive Environment=. systemd découpe
// la ligne sur les blancs : sans guillemets, « K=deux mots » définit K=deux et
// écarte « mots » comme une affectation invalide, sans autre signe qu'une ligne
// de journal. La citation n'est posée que si la valeur l'exige, de sorte qu'une
// unité aux valeurs simples garde son rendu — et ne soit ni réécrite ni
// redémarrée. Un « % » qui introduit un spécificateur reste tel quel :
// « INSTANCE=%i » est un usage légitime. Tout autre « % » est doublé (D117) :
// systemd refusait l'affectation entière (« Invalid slot ») — une DATABASE_URL
// à mot de passe encodé (« p%40ss »), le vhost AMQP par défaut (« %2f ») —, la
// variable manquait sans autre signe qu'une ligne de journal, et
// « systemd-analyze verify » refuse désormais l'unité.
func systemdEnv(key, value string) string {
	assignment := key + "=" + escapePercent(value)
	if !strings.ContainsAny(value, " \t\"'\\") {
		return assignment
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(assignment) + `"`
}

// systemdSpecifiers énumère les lettres que systemd résout après « % » (relevé
// sur systemd 257, systemd.unit « Specifiers », « %% » compris), moins « D » :
// systemd 252 (Debian 12) ne le connaît pas, et un mot de passe encodé en URL
// (« %D0… ») y faisait refuser l'unité. « %D » est donc toujours doublé, littéral
// partout.
const systemdSpecifiers = "abcdfghijlmnopqrstuvwyABCEGHIJLMNPRSTUVWY%"

// escapePercent double tout « % » qui n'introduit pas un spécificateur systemd.
func escapePercent(v string) string {
	if !strings.Contains(v, "%") {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '%' {
			b.WriteByte(v[i])
			continue
		}
		if i+1 < len(v) && strings.IndexByte(systemdSpecifiers, v[i+1]) >= 0 {
			b.WriteByte('%')
			b.WriteByte(v[i+1])
			i++
			continue
		}
		b.WriteString("%%")
	}
	return b.String()
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
