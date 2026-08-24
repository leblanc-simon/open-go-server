package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/provider/php"
	"github.com/leviathan/ogs/internal/spec"
)

// installCompletions dote l'arbre de commandes de complétions dynamiques.
//
// L'affectation se déduit de la ligne d'usage plutôt que d'une liste tenue à la
// main : une commande déclarant « <id> » complète des identifiants de projet, et
// le reste suit. Ainsi une commande ajoutée plus tard hérite de la complétion
// sans qu'on ait à y penser.
func installCompletions(root *cobra.Command, app *App) {
	for _, cmd := range walk(root) {
		tokens := argTokens(cmd.Use)
		if len(tokens) == 0 || cmd.ValidArgsFunction != nil {
			continue
		}
		c, toks := cmd, tokens
		c.ValidArgsFunction = func(cmd *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
			return completeToken(app, c, toks, args, prefix)
		}
	}
	// Les valeurs de ces options sont closes : les proposer évite une faute de frappe.
	for _, cmd := range walk(root) {
		if f := cmd.Flags().Lookup("php"); f != nil {
			_ = cmd.RegisterFlagCompletionFunc("php", staticComplete(php.Supported))
		}
		if f := cmd.Flags().Lookup("kind"); f != nil {
			_ = cmd.RegisterFlagCompletionFunc("kind", staticComplete(kinds()))
		}
		if f := cmd.Flags().Lookup("env"); f != nil {
			_ = cmd.RegisterFlagCompletionFunc("env", staticComplete([]string{"prod", "staging"}))
		}
	}
}

// completeToken complète l'argument à la position courante.
func completeToken(app *App, cmd *cobra.Command, tokens []string, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
	tok := tokenAt(tokens, len(args))
	if tok == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	switch {
	case strings.HasPrefix(tok, "id"):
		names := projectIDs(app)
		if strings.Contains(tok, "@") {
			names = append(names, sharedCerts(app)...)
		}
		return filterPrefix(names, prefix), cobra.ShellCompDirectiveNoFileComp
	case tok == "service":
		return filterPrefix(serviceNames(app, first(args)), prefix), cobra.ShellCompDirectiveNoFileComp
	case tok == "version":
		if cmd.Parent() != nil && cmd.Parent().Name() == "php" {
			return filterPrefix(php.Supported, prefix), cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	case tok == "base":
		return filterPrefix(databaseNames(app, first(args)), prefix), cobra.ShellCompDirectiveNoFileComp
	case tok == "file":
		return filterPrefix(projectQueues(app, first(args)), prefix), cobra.ShellCompDirectiveNoFileComp
	case tok == "archive":
		return nil, cobra.ShellCompDirectiveDefault // un chemin : le shell complète mieux
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// argTokens extrait les arguments positionnels d'une ligne d'usage, dans
// l'ordre. « apply [id...] » donne [id...], « logs <id> <service> » donne
// [id, service].
func argTokens(use string) []string {
	var out []string
	for _, f := range strings.Fields(use)[min(1, len(strings.Fields(use))):] {
		if strings.HasPrefix(f, "--") {
			break
		}
		if len(f) < 3 {
			continue
		}
		switch f[0] {
		case '<':
			out = append(out, strings.Trim(f, "<>"))
		case '[':
			out = append(out, strings.Trim(f, "[]"))
		}
	}
	return out
}

// tokenAt donne le jeton attendu à la position n, en tenant compte des jetons
// répétables : « [id...] » complète des projets à toutes les positions.
func tokenAt(tokens []string, n int) string {
	for i, t := range tokens {
		repeat := strings.HasSuffix(t, "...")
		t = strings.TrimSuffix(t, "...")
		if i == n || (repeat && n >= i) {
			return t
		}
	}
	return ""
}

// projectIDs liste les projets déclarés. La lecture est volontairement
// superficielle : un fichier invalide ne doit pas priver l'utilisateur de la
// complétion des autres.
func projectIDs(app *App) []string {
	entries, err := os.ReadDir(paths(app).ProjectsDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(out)
	return out
}

// loadForCompletion charge un projet sans le valider : la complétion doit
// fonctionner même sur une déclaration en cours d'écriture.
func loadForCompletion(app *App, id string) *model.Project {
	if id == "" {
		return nil
	}
	p, err := spec.Load(paths(app).ProjectFile(id))
	if err != nil {
		return nil
	}
	return p
}

func serviceNames(app *App, id string) []string {
	p := loadForCompletion(app, id)
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Spec.Services))
	for _, s := range p.Spec.Services {
		out = append(out, s.Name)
	}
	return out
}

func databaseNames(app *App, id string) []string {
	p := loadForCompletion(app, id)
	if p == nil {
		return nil
	}
	var out []string
	for _, d := range p.Spec.Databases {
		out = append(out, d.Name)
	}
	return out
}

func projectQueues(app *App, id string) []string {
	p := loadForCompletion(app, id)
	if p == nil || p.Spec.AMQP == nil {
		return nil
	}
	return queueNames(p.Spec.AMQP)
}

// sharedCerts liste les certificats partagés, désignés par « @nom ».
func sharedCerts(app *App) []string {
	if err := app.loadConfig(); err != nil {
		return nil
	}
	var out []string
	for name := range app.Cfg.Certificates {
		out = append(out, "@"+name)
	}
	sort.Strings(out)
	return out
}

// paths reconstruit les chemins depuis le préfixe : la complétion s'exécute
// avant que setup n'ait eu lieu.
func paths(app *App) config.Paths { return config.NewPaths(app.Prefix) }

func kinds() []string {
	return []string{
		string(model.KindStatic), string(model.KindPHPPlain),
		string(model.KindPrestashop), string(model.KindSymfony),
		string(model.KindGo), string(model.KindNode),
		string(model.KindPython), string(model.KindRuby),
	}
}

func staticComplete(values []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
		return filterPrefix(values, prefix), cobra.ShellCompDirectiveNoFileComp
	}
}

func filterPrefix(values []string, prefix string) []string {
	var out []string
	for _, v := range values {
		if strings.HasPrefix(v, prefix) {
			out = append(out, v)
		}
	}
	return out
}

// walk parcourt l'arbre des commandes, racine comprise.
func walk(cmd *cobra.Command) []*cobra.Command {
	out := []*cobra.Command{cmd}
	for _, c := range cmd.Commands() {
		out = append(out, walk(c)...)
	}
	return out
}

func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
