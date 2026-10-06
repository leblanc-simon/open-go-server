// Package removal retire les fichiers gérés sous le régime des cinq états
// (§6.4), que seule leur écriture connaissait jusque-là.
//
// Chaque provider retirait ses fichiers devenus inutiles par un simple
// os.Remove : un pool ou un vhost posé à la main selon la convention de nommage,
// un fichier cron ou sudoers retouché par l'exploitant partaient sans --force ni
// copie, alors que ces mêmes fichiers étaient refusés à l'écriture. Le retrait
// passe désormais par la même porte que l'écrasement, et un fichier qu'ogs n'a
// pas écrit pour ce projet n'est jamais retiré.
package removal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/provider/system"
)

// File décrit un fichier qu'une étape doit retirer.
type File struct {
	// Resource et Summary nomment l'étape, comme pour une écriture.
	Resource string
	Summary  string
	Path     string
	// Project est l'identifiant que l'en-tête d'ogs doit porter pour que le
	// fichier se retire ; vide pour un fichier de parc (voir fs.InspectRemoval).
	Project string
	// Inspect remplace fs.InspectRemoval pour un fichier qui ne peut pas porter
	// l'en-tête d'ogs : une clé DKIM se juge contre la clé scellée.
	Inspect func() (fs.State, error)
	// Force autorise le retrait d'un fichier modifié à la main, après
	// sauvegarde dans Backups. Il ne vaut jamais pour un fichier qu'ogs n'a pas
	// écrit : --force reprend un fichier d'ogs retouché, il n'en fait pas un.
	Force   bool
	Backups string
	Now     func() time.Time
	// Links rend, au moment du retrait, les liens qui désignent le fichier : ils
	// partent avec lui. Un lien d'activation laissé derrière pointerait dans le
	// vide, ce que nginx tient pour fatal — tout le parc avec lui.
	Links func() []string
	// Reload et FlushReloads ne sont portés que par l'étape qui retire : un refus
	// n'a rien changé, rien ne doit être rechargé en son nom.
	Reload       []string
	FlushReloads bool
	// NonCritical rend non critique le retrait lui-même, pour un provider dont
	// aucune étape ne doit faire compenser un déploiement (supervision, DKIM).
	NonCritical bool
	// Owns, s'il est donné, juge la ressource que porte l'en-tête (D76, D100) :
	// le fichier n'est retiré que si elle relève encore de l'appelant. Elle est
	// relue au plan et de nouveau au retrait — un autre provider a pu réécrire le
	// fichier entre-temps, sous sa propre ressource, dans le même apply (D121) :
	// l'unité applicative réécrite par-dessus un modèle de service homonyme
	// partait sur la foi de l'en-tête lu au plan, liens d'activation compris.
	Owns func(resource string) bool
}

// foreignResource dit si l'en-tête désigne désormais une ressource que
// l'appelant ne tient pas : le fichier n'est alors plus le sien à retirer.
func (f File) foreignResource() bool {
	if f.Owns == nil {
		return false
	}
	_, resource, err := fs.ManagedResource(f.Path)
	return err == nil && !f.Owns(resource)
}

// InspectError signale un fichier dont l'état n'a pas pu être lu au plan. Il
// n'est ni un conflit ni une faute de déclaration : l'appelant qui classe les
// erreurs de son plan le rend comme une erreur ordinaire.
type InspectError struct {
	Path string
	Err  error
}

func (e *InspectError) Error() string {
	return fmt.Sprintf("examen de %s avant retrait : %v", e.Path, e.Err)
}

func (e *InspectError) Unwrap() error { return e.Err }

func (f File) inspect() (fs.State, error) {
	if f.Inspect != nil {
		return f.Inspect()
	}
	return fs.InspectRemoval(f.Path, f.Project)
}

func (f File) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Step rend l'étape qui retire le fichier, celle qui refuse de le faire, ou
// nil s'il est déjà absent.
//
// Un refus n'est jamais critique : le fichier resté en place est en trop, pas
// en défaut — le site, le pool ou la tâche déclarés sont servis. Faire
// compenser tout le déploiement pour un fichier qu'on a seulement renoncé à
// retirer défairait ce qui marche, sans rien retirer de plus. Le code 6 et le
// message suffisent à l'exploitant pour trancher.
func (f File) Step() (*engine.Step, error) {
	state, err := f.inspect()
	if err != nil {
		return nil, &InspectError{Path: f.Path, Err: err}
	}
	if state == fs.StateAbsent || f.foreignResource() {
		return nil, nil
	}
	if refus := f.refusal(state); refus != nil {
		// Rien ne dirait autrement qu'un fichier est resté : la tâche cron ou la
		// délégation qu'il porte continuerait de valoir alors que la déclaration
		// n'en veut plus.
		s := engine.NewStep(f.Resource, engine.Delete, f.Summary+" — "+f.label(state))
		s.Critical = false
		s.Apply = func(context.Context) error { return refus }
		return s, nil
	}
	summary := f.Summary
	if state == fs.StateHandEdited {
		// --force : l'étape redevient un retrait ordinaire, annoncé comme tel par
		// « plan ». La sauvegarde n'a lieu qu'à l'application : « plan » et
		// « diff » n'écrivent rien.
		summary += " — retrait forcé (sauvegarde dans " + f.Backups + ")"
	}
	return f.removeStep(summary), nil
}

// refusal rend le refus que vaut state, ou nil si le fichier peut partir : un
// fichier qu'ogs n'a pas écrit pour ce projet ne part jamais, un fichier retouché
// seulement sous --force.
func (f File) refusal(state fs.State) error {
	switch {
	case state == fs.StateForeign:
		return fmt.Errorf("%s : ogs ne retire jamais un fichier qu'il n'a pas écrit pour ce projet, "+
			"--force compris — le retirer à la main s'il est sans objet", f.foreignMotive())
	case state == fs.StateHandEdited && !f.Force:
		return fmt.Errorf("%s a été modifié en dehors d'ogs : relancer avec --force pour le retirer "+
			"(l'original sera sauvegardé dans %s)", f.Path, f.Backups)
	}
	return nil
}

// label est le complément de résumé d'un retrait refusé.
func (f File) label(state fs.State) string {
	if state == fs.StateHandEdited {
		return "fichier modifié hors ogs"
	}
	if owner := f.otherOwner(); owner != "" {
		return "fichier géré pour le projet " + owner + ", laissé en place"
	}
	return "fichier non géré par ogs, laissé en place"
}

func (f File) foreignMotive() string {
	if owner := f.otherOwner(); owner != "" {
		return fmt.Sprintf("%s appartient au projet %s", f.Path, owner)
	}
	if f.Inspect != nil {
		// Un fichier jugé sans en-tête — une clé DKIM — n'est étranger que s'il
		// n'est pas un fichier ordinaire.
		return fmt.Sprintf("%s n'est pas un fichier ordinaire", f.Path)
	}
	return fmt.Sprintf("%s ne porte pas la marque d'ogs", f.Path)
}

// otherOwner rend le projet pour lequel ogs a écrit le fichier, s'il en nomme un
// autre que celui du retrait.
func (f File) otherOwner() string {
	if f.Project == "" || f.Inspect != nil {
		return ""
	}
	// Lu comme InspectRemoval lit la marque : un fichier qu'il juge d'un autre
	// projet est nommé comme tel, jamais « sans la marque d'ogs ».
	if owner, _, _ := fs.ManagedResource(f.Path); owner != f.Project {
		return owner
	}
	return ""
}

// lien est un lien retiré avec le fichier, et sa cible, pour la compensation.
type lien struct{ path, cible string }

func (f File) removeStep(summary string) *engine.Step {
	s := engine.NewStep(f.Resource, engine.Delete, summary)
	s.Critical = !f.NonCritical
	s.Diff = func() (string, error) { return "suppression de " + f.Path, nil }
	s.Reload = f.Reload
	s.FlushReloads = f.FlushReloads

	// Le contenu, le mode et le propriétaire sont relevés à l'application et non
	// au plan : l'état du disque a pu changer entre les deux.
	var previous []byte
	var removed bool
	var mode os.FileMode
	owner := fs.KeepOwner
	var liens []lien
	s.Apply = func(context.Context) error {
		// L'état est relu : un fichier retouché ou remplacé entre le plan et
		// l'application relève du refus, non du retrait.
		state, err := f.inspect()
		if err != nil {
			return err
		}
		if state == fs.StateAbsent || f.foreignResource() {
			return nil
		}
		if refus := f.refusal(state); refus != nil {
			// Et le refus reste ce qu'il est au plan : non critique. Critique, il
			// faisait compenser tout le déploiement pour un fichier qu'on renonce
			// seulement à retirer. Le moteur lit Critical au retour d'Apply.
			s.Critical = false
			return refus
		}
		info, err := os.Lstat(f.Path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(f.Path)
		if err != nil {
			return err
		}
		if state == fs.StateHandEdited {
			// Retirer le travail d'un exploitant sans en garder trace rend
			// l'opération irréversible : la sauvegarde précède le retrait.
			if _, err := fs.BackupTo(f.Path, f.Backups, f.now()); err != nil {
				return err
			}
		}
		previous, mode = data, info.Mode().Perm()
		if uid, gid, ok := system.OwnerOf(info); ok {
			owner = fs.Owner{UID: uid, GID: gid}
		}
		// Les liens partent avant le fichier, pour qu'aucun ne pointe jamais dans
		// le vide ; un retrait qui échoue ensuite les rétablit aussitôt, sans quoi
		// l'étape en échec — que rien ne compense — laisserait le fichier hors
		// ligne.
		liens = nil
		if f.Links != nil {
			for _, l := range f.Links() {
				cible, err := os.Readlink(l)
				if err != nil {
					continue
				}
				if err := os.Remove(l); err != nil && !os.IsNotExist(err) {
					return f.rétablir(err, liens)
				}
				liens = append(liens, lien{l, cible})
			}
		}
		if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			return f.rétablir(err, liens)
		}
		removed = true
		return nil
	}
	// La compensation rend le fichier tel qu'il était, puis ses liens : rendre le
	// fichier sans eux annoncerait un retour à l'état initial en laissant le
	// vhost hors ligne — ni Apache ni nginx ne chargent sites-available.
	s.Compensate = func(context.Context) error {
		if !removed {
			return nil
		}
		if err := fs.WriteAtomic(f.Path, previous, mode, owner); err != nil {
			return f.perdu(err, previous, mode, liens)
		}
		if err := relier(liens); err != nil {
			return f.horsLigne(err)
		}
		return nil
	}
	return s
}

// rétablir rend l'échec d'un retrait, joint à celui du rétablissement des liens
// déjà retirés s'il a échoué : jeté, il laissait le fichier en place mais
// désactivé — hors ligne au prochain rechargement du serveur —, sous une erreur
// qui ne parlait que du retrait. L'étape en échec n'est pas compensée : rien
// d'autre ne rétablira ces liens.
func (f File) rétablir(cause error, liens []lien) error {
	if err := relier(liens); err != nil {
		return engine.Residual(errors.Join(cause, f.horsLigne(err)))
	}
	return cause
}

// horsLigne dit d'un rétablissement de liens en échec ce qu'il laisse : le
// fichier présent mais plus activé, et comment le rétablir.
func (f File) horsLigne(err error) error {
	var remèdes []string
	var echec *liensNonRétablis
	if errors.As(err, &echec) {
		for _, l := range echec.liens {
			remèdes = append(remèdes, exec.ShellLine("ln", "-s", "--", l.cible, l.path))
		}
	}
	return fmt.Errorf("%w — %s est en place mais n'est plus activé : hors ligne au prochain rechargement du "+
		"serveur ; le rétablir : %s", err, f.Path, strings.Join(remèdes, " ; "))
}

// perdu dit d'une compensation qui n'a pas pu réécrire le fichier ce qu'elle
// laisse : fichier et liens retirés, donc hors ligne. Les liens ne sont pas
// rétablis — ils pointeraient dans le vide, ce que nginx tient pour fatal, tout
// le parc avec lui —, et le contenu relevé à l'application est gardé dans les
// sauvegardes, faute de quoi rien ne permettrait de le rendre.
func (f File) perdu(cause error, previous []byte, mode os.FileMode, liens []lien) error {
	copie, err := fs.BackupDataTo(filepath.Base(f.Path), previous, mode, f.Backups, f.now())
	var remède string
	if err != nil {
		remède = fmt.Sprintf("son contenu n'a pas pu être gardé (%v)", err)
	} else {
		remède = "le rétablir : " + exec.ShellLine("cp", "--", copie, f.Path)
	}
	for _, l := range liens {
		remède += " ; " + exec.ShellLine("ln", "-s", "--", l.cible, l.path)
	}
	return fmt.Errorf("%s non réécrit : %w — le fichier et ses liens d'activation restent retirés, la "+
		"ressource est hors ligne ; %s", f.Path, cause, remède)
}

// relier rétablit des liens retirés. Une compensation en échec arrête tout le
// déroulage : un lien déjà en place n'est pas une erreur.
func relier(liens []lien) error {
	echec := &liensNonRétablis{}
	for _, l := range liens {
		if err := os.Symlink(l.cible, l.path); err != nil && !os.IsExist(err) {
			echec.liens = append(echec.liens, l)
			echec.errs = append(echec.errs, err.Error())
		}
	}
	if len(echec.liens) > 0 {
		return echec
	}
	return nil
}

// liensNonRétablis porte les liens que relier n'a pas pu rétablir, pour que le
// message dise lesquels et comment.
type liensNonRétablis struct {
	liens []lien
	errs  []string
}

func (e *liensNonRétablis) Error() string {
	return "liens non rétablis : " + strings.Join(e.errs, " ; ")
}
