// Package cli porte l'interface en ligne de commande.
package cli

import (
	"errors"
	"fmt"
)

// Codes de retour normalisés (§10.3). Ils font partie du contrat de l'outil :
// un script d'exploitation doit pouvoir distinguer un conflit d'une dépendance
// absente sans analyser les messages.
const (
	CodeOK         = 0
	CodeError      = 1
	CodeUsage      = 2
	CodeValidation = 3
	CodeConflict   = 4
	CodeDependency = 5
	CodePartial    = 6
	CodeLocked     = 7
	CodePermission = 8
	CodeCancelled  = 9
)

// ExitError porte un code de retour explicite.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("code %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// exitf construit une erreur de sortie.
func exitf(code int, format string, args ...any) error {
	return &ExitError{Code: code, Err: fmt.Errorf(format, args...)}
}

// wrap associe un code à une erreur existante.
func wrap(code int, err error) error {
	if err == nil {
		return nil
	}
	return &ExitError{Code: code, Err: err}
}

// ExitCode extrait le code de retour d'une erreur.
func ExitCode(err error) int {
	if err == nil {
		return CodeOK
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return CodeError
}
