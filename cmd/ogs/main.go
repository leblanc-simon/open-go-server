// Commande ogs : administration de serveurs web.
package main

import (
	"context"
	"fmt"
	"os"

	"leblanc.io/open-go-server/internal/cli"
)

func main() {
	// Une interruption doit laisser le moteur dérouler ses compensations : le
	// contexte est annulé, il n'y a pas d'arrêt brutal (signaux.go, D122).
	ctx, stop := watchSignals(context.Background(), os.Stderr)

	root := cli.NewRootCommand()
	// Les signaux restent captés jusqu'à la sortie : le message d'erreur peut
	// partir vers un terminal fermé, et SIGPIPE ne doit pas l'emporter sur le
	// code de retour.
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ogs : %v\n", err)
		os.Exit(cli.ExitCode(err))
	}
	stop()
}
