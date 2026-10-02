package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// saisieSecrets lit les secrets d'administration du parc — le compte
// d'administration du broker, les clés d'OVH — hors de la ligne de commande
// (D93). Passée en option, une valeur figure dans /proc/<pid>/cmdline, lisible
// de tout compte local tant que la commande tourne, dans la ligne COMMAND= que
// sudo journalise et dans l'historique du shell de root ; les deux derniers
// durent. L'utilisateur d'un projet a un shell (D62).
//
// Sur un terminal, la valeur est demandée sans écho. Sinon, elle est lue sur
// l'entrée standard, une ligne par secret manquant, dans l'ordre que l'aide de
// la commande donne : un script écrit « printf '%s\n' "$mdp" | ogs … », ou
// redirige un fichier en 0600. Les options porteuses de valeur restent acceptées,
// pour ne casser aucun script, mais chacune est signalée.
type saisieSecrets struct {
	app    *App
	cmd    *cobra.Command
	lignes *bufio.Reader
}

func (a *App) saisieSecrets(cmd *cobra.Command) *saisieSecrets {
	return &saisieSecrets{app: a, cmd: cmd}
}

// valeur rend le secret que porte l'option nommée, ou le demande.
func (s *saisieSecrets) valeur(option, valeur, libellé string) (string, error) {
	if valeur != "" {
		s.app.warnf("⚠ --%s : la valeur figure dans la ligne de commande — journal de sudo, historique "+
			"du shell, /proc —, la faire tourner si d'autres ont accès à la machine ; l'omettre pour "+
			"la saisir sans écho ou la passer sur l'entrée standard\n", option)
		return valeur, nil
	}
	in := s.cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if s.app.NonInteractive {
			return "", exitf(CodeUsage, "%s requis : le passer sur l'entrée standard en mode non interactif",
				libellé)
		}
		fmt.Fprintf(s.cmd.ErrOrStderr(), "%s (sans écho) : ", libellé)
		data, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(s.cmd.ErrOrStderr())
		// Ctrl+C à l'invite est capté par ogs (arrêt propre des commandes), si
		// bien que la lecture continue jusqu'à Entrée : la valeur partielle serait
		// enregistrée. L'annulation se lit donc après coup.
		if err := s.annulée(); err != nil {
			return "", err
		}
		if err != nil {
			return "", wrap(CodeError, err)
		}
		return nonVide(string(data), libellé)
	}
	if s.lignes == nil {
		s.lignes = bufio.NewReader(in)
	}
	ligne, err := s.lignes.ReadString('\n')
	if err := s.annulée(); err != nil {
		return "", err
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", wrap(CodeError, err)
	}
	if ligne == "" {
		return "", exitf(CodeUsage, "%s requis : le saisir sur un terminal, ou le passer sur l'entrée "+
			"standard, une ligne par secret", libellé)
	}
	return nonVide(strings.TrimRight(ligne, "\r\n"), libellé)
}

// annulée rend le code 9 si la commande a été interrompue pendant la saisie.
func (s *saisieSecrets) annulée() error {
	if ctx := s.cmd.Context(); ctx != nil && ctx.Err() != nil {
		return exitf(CodeCancelled, "saisie interrompue : rien n'est enregistré")
	}
	return nil
}

func nonVide(v, libellé string) (string, error) {
	if v == "" {
		return "", exitf(CodeUsage, "%s vide", libellé)
	}
	return v, nil
}
