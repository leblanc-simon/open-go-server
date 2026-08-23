package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/goccy/go-yaml"

	"github.com/leviathan/ogs/internal/fs"
	"github.com/leviathan/ogs/internal/secret"
	"github.com/leviathan/ogs/internal/store"
)

func newInitCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Préparer /etc/ogs, la base d'état et la clé maître",
		Long: "Crée l'arborescence de l'outil, la base d'état SQLite, la configuration par\n" +
			"défaut et la clé maître du keystore. L'opération est idempotente : elle peut\n" +
			"être rejouée sans effet de bord, ce qui permet de l'appeler depuis un postinst.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var created, existing int
			for _, d := range app.Paths.Dirs() {
				if _, err := os.Stat(d.Path); err == nil {
					existing++
				} else {
					created++
				}
				if err := fs.EnsureDir(d.Path, d.Mode, fs.KeepOwner); err != nil {
					return wrap(CodeError, err)
				}
			}

			// Configuration : écrite seulement si absente, pour ne jamais écraser
			// les réglages de l'administrateur.
			cfgPath := app.Paths.ConfigFile()
			if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
				data, err := yaml.Marshal(app.Cfg)
				if err != nil {
					return wrap(CodeError, err)
				}
				header := []byte("# Configuration d'ogs. Voir « ogs doctor » pour vérifier la cohérence.\n")
				if err := fs.WriteAtomic(cfgPath, append(header, data...), 0o644, fs.KeepOwner); err != nil {
					return wrap(CodeError, err)
				}
				app.printf("✓ configuration écrite : %s\n", cfgPath)
			} else {
				app.printf("= configuration déjà présente : %s\n", cfgPath)
			}

			// Clé maître : jamais régénérée, sa perte rendrait tous les secrets
			// existants illisibles.
			keyPath := app.Paths.MasterKey()
			if _, err := os.Stat(keyPath); os.IsNotExist(err) {
				if err := secret.GenerateKey(keyPath); err != nil {
					return wrap(CodeError, err)
				}
				app.printf("✓ clé maître générée : %s (mode 0400)\n", keyPath)
				app.printf("  ⚠ sauvegarder ce fichier : sans lui, les secrets chiffrés sont irrécupérables.\n")
			} else {
				app.printf("= clé maître déjà présente : %s\n", keyPath)
			}

			st, err := store.Open(app.Paths.StateDB())
			if err != nil {
				return wrap(CodeError, err)
			}
			defer st.Close()
			v, err := st.SchemaVersion()
			if err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ base d'état : %s (schéma v%d)\n", st.Path(), v)
			app.printf("✓ %d répertoire(s) créé(s), %d déjà présent(s)\n", created, existing)
			if app.Format != "table" {
				return app.emit("InitResult", map[string]any{
					"config": cfgPath, "masterKey": keyPath, "stateDB": st.Path(), "schema": v,
				}, nil, nil)
			}
			fmt.Fprintln(app.Out, "\nogs est prêt. Créer un premier projet :\n  ogs project create shop.fr --kind php-symfony")
			return nil
		},
	}
}
