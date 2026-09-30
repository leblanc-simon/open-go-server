package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestSaisieInterrompueNEnregistreRien(t *testing.T) {
	// Ctrl+C à l'invite est capté par ogs : la lecture continuait jusqu'à Entrée,
	// et la valeur partielle était enregistrée.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetIn(strings.NewReader("mot-de-pa\n"))
	app := newApp()
	_, err := app.saisieSecrets(cmd).valeur("password", "", "mot de passe")
	if ExitCode(err) != CodeCancelled {
		t.Errorf("code %d attendu %d : %v", ExitCode(err), CodeCancelled, err)
	}
}
