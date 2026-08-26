// Commande docgen produit les pages de manuel, les scripts de complétion et la
// documentation Markdown des commandes. Elle est appelée à la construction du
// paquet, de sorte que la documentation livrée ne puisse pas diverger du code.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"leblanc.io/open-go-server/internal/cli"
)

// placeholder reconnaît un argument encadré par des chevrons.
var placeholder = regexp.MustCompile(`<([^<>]+)>`)

// upperPlaceholders réécrit récursivement les lignes d'usage.
func upperPlaceholders(cmd *cobra.Command) {
	cmd.Use = placeholder.ReplaceAllStringFunc(cmd.Use, func(m string) string {
		return strings.ToUpper(strings.Trim(m, "<>"))
	})
	for _, c := range cmd.Commands() {
		upperPlaceholders(c)
	}
}

func main() {
	out := "dist/doc"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	// La date de référence est fournie par l'environnement quand la construction
	// se veut reproductible.
	date := time.Now()
	if v := os.Getenv("OGS_DOC_DATE"); v != "" {
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			log.Fatalf("OGS_DOC_DATE : %v", err)
		}
		date = d
	}

	root := cli.NewRootCommand()
	root.DisableAutoGenTag = true
	// Les marques-places sont écrits « <id> » dans l'aide en ligne, mais le
	// convertisseur Markdown vers roff y voit une balise HTML et les efface. On
	// les passe donc en majuscules, comme le veut l'usage des pages de manuel.
	upperPlaceholders(root)

	manDir := filepath.Join(out, "man")
	mdDir := filepath.Join(out, "md")
	compDir := filepath.Join(out, "completion")
	for _, d := range []string{manDir, mdDir, compDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatal(err)
		}
	}

	header := &doc.GenManHeader{
		Title:   "OGS",
		Section: "8", // administration système
		Date:    &date,
		Source:  "ogs",
		Manual:  "Manuel de l'administrateur",
	}
	if err := doc.GenManTree(root, header, manDir); err != nil {
		log.Fatal(err)
	}
	if err := doc.GenMarkdownTree(root, mdDir); err != nil {
		log.Fatal(err)
	}

	for name, gen := range map[string]func(io.Writer) error{
		"ogs.bash": func(w io.Writer) error { return root.GenBashCompletionV2(w, true) },
		"ogs.zsh":  root.GenZshCompletion,
		"ogs.fish": func(w io.Writer) error { return root.GenFishCompletion(w, true) },
	} {
		f, err := os.Create(filepath.Join(compDir, name))
		if err != nil {
			log.Fatal(err)
		}
		if err := gen(f); err != nil {
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
	}

	entries, _ := os.ReadDir(manDir)
	pages := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".8" {
			pages++
		}
	}
	fmt.Printf("documentation écrite dans %s : %d page(s) de manuel, 3 scripts de complétion\n", out, pages)
}
