package cli

import (
	"github.com/spf13/cobra"
)

// Conduite des commandes sous --dry-run.
//
// --dry-run est une option globale : toute commande la reçoit, qu'elle sache
// simuler ou non, et une commande qui ne la lit pas agit comme si elle était
// absente. « project delete --dry-run » supprimait ainsi réellement le projet —
// archive, bases, vhost, compte, home, hooks preDelete et postDelete compris —,
// puis rendait 0 ; « project restore » et « cert renew » en faisaient autant
// (R3-20). L'exploitant qui passe l'option veut justement voir sans risque :
// l'ignorer, c'est agir à sa place au moment précis où il a demandé de ne pas le
// faire.
//
// La règle est donc tenue ici, en un seul endroit, et non par chaque commande :
// une commande absente de la table ci-dessous refuse l'option (code 2) avant
// d'avoir rien lu ni touché. Une commande ajoutée plus tard est refusée par
// défaut, jusqu'à ce qu'on l'y inscrive — ce qu'on ne fait qu'après lui avoir
// appris à simuler. TestDryRunChaqueCommandeSimuleOuRefuse parcourt l'arbre des
// commandes et éprouve chacune sur un préfixe : ce qui simule ne touche à rien,
// ce qui refuse le fait avant tout geste.

// dryRunConduct est la conduite d'une commande sous --dry-run.
type dryRunConduct int

const (
	// dryRunRefused : la commande agit et ne sait pas simuler. Elle refuse
	// l'option plutôt que de l'ignorer.
	dryRunRefused dryRunConduct = iota
	// dryRunSimulated : la commande décrit ce qu'elle ferait et s'arrête avant le
	// premier geste — ni fichier, ni compte, ni hook, ni écriture dans la base
	// d'état.
	dryRunSimulated
	// dryRunReadOnly : la commande ne modifie rien ; l'option est sans objet. Elle
	// garde la trace d'audit d'une consultation (« secrets show ») : une
	// divulgation qui passerait sous --dry-run sans être journalisée serait une
	// porte dérobée.
	dryRunReadOnly
)

// dryRunConducts inscrit les commandes qui acceptent --dry-run, par chemin
// complet. Le verrou global que prennent « apply » et « firewall apply » n'est
// pas un état du système : il est pris pour que le calcul lise un système qui ne
// bouge pas sous lui. Une réserve, que « apply --dry-run » partage avec « plan »,
// « diff » et « status » parce qu'elle tient au calcul du plan lui-même : le
// numéro du pool PHP ou le port applicatif d'un projet jamais appliqué y est
// attribué, donc inscrit dans la base d'état — celui-là même que l'apply lui
// donnera.
var dryRunConducts = map[string]dryRunConduct{
	// Commandes qui savent simuler : chacune s'arrête, sous --dry-run, avant son
	// hook « pre » et avant sa première écriture.
	"ogs apply":             dryRunSimulated, // plan de chaque projet affiché, rien d'exécuté
	"ogs amqp messenger":    dryRunSimulated, // déclaration affichée, non écrite
	"ogs cert issue":        dryRunSimulated, // demande affichée, aucune émission
	"ogs cron add":          dryRunSimulated,
	"ogs db add":            dryRunSimulated,
	"ogs firewall apply":    dryRunSimulated, // plan du pare-feu affiché, rien d'exécuté
	"ogs harden":            dryRunSimulated, // rapport, et sous --apply les restrictions prévues
	"ogs import adopt":      dryRunSimulated, // déclaration affichée, non écrite
	"ogs mail dkim enable":  dryRunSimulated,
	"ogs mail dkim disable": dryRunSimulated,
	"ogs php switch":        dryRunSimulated, // compatibilité vérifiée, bascule non déclarée
	"ogs project create":    dryRunSimulated, // déclaration affichée, non écrite
	"ogs site create":       dryRunSimulated, // déclaration affichée, non écrite ni appliquée
	"ogs site clone":        dryRunSimulated, // récapitulatif, rien d'écrit

	// Commandes de consultation.
	"ogs amqp status":            dryRunReadOnly,
	"ogs app types":              dryRunReadOnly,
	"ogs cert list":              dryRunReadOnly,
	"ogs cert dns-provider list": dryRunReadOnly,
	"ogs completion bash":        dryRunReadOnly,
	"ogs completion fish":        dryRunReadOnly,
	"ogs completion powershell":  dryRunReadOnly,
	"ogs completion zsh":         dryRunReadOnly,
	"ogs cron list":              dryRunReadOnly,
	"ogs db list":                dryRunReadOnly,
	"ogs diff":                   dryRunReadOnly,
	"ogs doctor":                 dryRunReadOnly,
	"ogs firewall plan":          dryRunReadOnly,
	"ogs firewall profiles":      dryRunReadOnly,
	"ogs firewall rules":         dryRunReadOnly,
	"ogs firewall sources":       dryRunReadOnly,
	"ogs firewall status":        dryRunReadOnly,
	"ogs firewall whois":         dryRunReadOnly,
	"ogs help":                   dryRunReadOnly,
	"ogs import scan":            dryRunReadOnly,
	"ogs import unmanaged":       dryRunReadOnly,
	"ogs mail dkim records":      dryRunReadOnly,
	"ogs mail dkim show":         dryRunReadOnly,
	"ogs my status":              dryRunReadOnly,
	"ogs php list":               dryRunReadOnly,
	"ogs php ports":              dryRunReadOnly,
	"ogs plan":                   dryRunReadOnly,
	"ogs project archives":       dryRunReadOnly,
	"ogs project export":         dryRunReadOnly,
	"ogs project list":           dryRunReadOnly,
	"ogs project show":           dryRunReadOnly,
	"ogs runtime list":           dryRunReadOnly,
	"ogs secrets env":            dryRunReadOnly,
	"ogs secrets list":           dryRunReadOnly,
	"ogs secrets show":           dryRunReadOnly,
	"ogs service list":           dryRunReadOnly,
	"ogs service logs":           dryRunReadOnly,
	"ogs status":                 dryRunReadOnly,
	"ogs template list":          dryRunReadOnly,
	"ogs template show":          dryRunReadOnly,
	"ogs user key show":          dryRunReadOnly,
	"ogs version":                dryRunReadOnly,
	"ogs web engine":             dryRunReadOnly,
	"ogs web test":               dryRunReadOnly,
}

// dryRunConductOf renvoie la conduite d'une commande. Le chemin complet, et non
// le nom : « show » est une consultation sous « project », il n'en serait pas
// forcément une ailleurs. Aucune conduite ne s'hérite d'un parent, pour la même
// raison.
func dryRunConductOf(cmd *cobra.Command) dryRunConduct {
	return dryRunConducts[cmd.CommandPath()]
}

// refuseDryRun rend l'erreur d'usage d'une commande qui ne sait pas simuler.
// Elle est appelée avant toute préparation : rien n'est lu, rien n'est ouvert.
func refuseDryRun(cmd *cobra.Command) error {
	if dryRunConductOf(cmd) != dryRunRefused {
		return nil
	}
	return exitf(CodeUsage, "« %s » ne sait pas simuler son effet : --dry-run est refusé "+
		"plutôt qu'ignoré, et rien n'a été fait", cmd.CommandPath())
}
