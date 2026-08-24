// Package engine porte le moteur de réconciliation : plan, application en saga,
// diff et verrou d'exécution.
package engine

import (
	"context"
	"fmt"
)

// Action est la nature d'une étape.
type Action int

const (
	NoOp Action = iota
	Create
	Update
	Delete
)

func (a Action) String() string {
	switch a {
	case Create:
		return "créer"
	case Update:
		return "mettre à jour"
	case Delete:
		return "supprimer"
	}
	return "inchangé"
}

// Symbol est le préfixe affiché dans le plan.
func (a Action) Symbol() string {
	switch a {
	case Create:
		return "+"
	case Update:
		return "~"
	case Delete:
		return "-"
	}
	return "="
}

// Step est une unité de travail atomique et compensable.
type Step struct {
	// Resource identifie la ressource visée, par exemple « system/user/shopfr ».
	Resource string
	// Action est la nature du changement.
	Action Action
	// Summary est la ligne affichée dans le plan.
	Summary string
	// Critical vaut faux pour les étapes dont l'échec ne doit pas annuler
	// l'opération : émission de certificat, démarrage de service, hooks (D16).
	Critical bool
	// Diff détaille le changement (contenu de fichier, requêtes SQL). Optionnel.
	Diff func() (string, error)
	// Apply réalise le changement.
	Apply func(context.Context) error
	// Compensate annule un Apply réussi, lors du déroulage d'une saga.
	Compensate func(context.Context) error
	// Reload énumère les intentions de rechargement, dédupliquées puis exécutées
	// une seule fois en fin d'application.
	Reload []string
	// FlushReloads exige que les rechargements en attente soient exécutés avant
	// cette étape. Certaines étapes ne se contentent pas d'un fichier écrit : le
	// challenge HTTP-01 a besoin que le serveur web ait relu son vhost, faute de
	// quoi l'autorité reçoit un 404.
	FlushReloads bool
}

// NewStep construit une étape critique par défaut : le cas dangereux est celui
// qu'on oublie de déclarer, donc c'est lui qui doit être le comportement implicite.
func NewStep(resource string, action Action, summary string) *Step {
	return &Step{Resource: resource, Action: action, Summary: summary, Critical: true}
}

// String rend l'étape lisible dans le plan.
func (s *Step) String() string {
	if s.Summary != "" {
		return fmt.Sprintf("%s %s", s.Action.Symbol(), s.Summary)
	}
	return fmt.Sprintf("%s %s %s", s.Action.Symbol(), s.Action, s.Resource)
}

// Mutates indique si l'étape change quelque chose.
func (s *Step) Mutates() bool { return s.Action != NoOp }
