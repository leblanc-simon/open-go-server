// Commande ogs : administration de serveurs web.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/leviathan/ogs/internal/cli"
)

func main() {
	// Une interruption doit laisser le moteur dérouler ses compensations : le
	// contexte est annulé, il n'y a pas d'arrêt brutal.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := cli.NewRootCommand()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ogs : %v\n", err)
		os.Exit(cli.ExitCode(err))
	}
}
