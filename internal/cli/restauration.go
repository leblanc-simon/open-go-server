package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/db"
)

// restoreState est le témoin d'une restauration inachevée (D113). Il est posé
// avant la déclaration et retiré une fois le dernier apply passé, réussi ou
// partiel : entre les deux, la déclaration est en place et « project restore »
// refusait toute relance (« le projet existe déjà ») sans qu'aucune commande ne
// reprenne ce qui manquait (R1-54). Il dit de quelle archive vient le projet et
// les phases accomplies, qu'une reprise ne refait pas : la réinjection des
// secrets réécrirait le fichier d'environnement qu'elle a déjà posé — refusé,
// puisqu'un fichier en place n'est jamais écrasé (D104) —, et une base rechargée
// le serait deux fois. L'apply, lui, se rejoue toujours : il est idempotent.
type restoreState struct {
	Project   string    `json:"project"`
	Archive   string    `json:"archive"`
	CreatedAt time.Time `json:"archiveCreatedAt"`
	// Declaration est l'empreinte de la déclaration que la restauration a
	// écrite : la reprise exige qu'elle soit restée telle quelle. Une
	// déclaration retouchée entre-temps n'est plus celle de l'archive, et les
	// phases notées ne vaudraient plus pour elle.
	Declaration string `json:"declaration"`
	// Secrets : secrets réinjectés, fichier d'environnement et certificats
	// rendus.
	Secrets bool `json:"secrets"`
	// Tree : arborescence extraite.
	Tree bool `json:"tree"`
	// Dumps : sauvegardes déjà tentées, rechargées ou non — un échec est un
	// manque nommé, dont le remède est « db restore --from-archive » (D104).
	Dumps []string `json:"dumps,omitempty"`
	// Loading est la sauvegarde dont le rechargement a commencé sans que son
	// issue soit notée : interrompu, il a pu laisser la base à moitié chargée.
	Loading string `json:"loading,omitempty"`
	// PostCreate : le hook « postCreate » a été lancé ; une reprise ne le
	// relance jamais.
	PostCreate bool `json:"postCreate,omitempty"`
	// DumpsDone : toutes les sauvegardes tentées.
	DumpsDone bool `json:"dumpsDone,omitempty"`
	// Gaps : ce que les phases accomplies n'ont pas pu rendre, rapporté à la fin
	// de la reprise comme il l'aurait été sans interruption.
	Gaps []restoreGap `json:"gaps,omitempty"`
}

// matches dit si le témoin est celui d'une restauration depuis cette archive :
// le projet et l'instant de création du manifeste, non le chemin, que
// l'exploitant a pu déplacer.
func (s *restoreState) matches(m archive.Manifest) bool {
	return s != nil && s.Project == m.Project && s.CreatedAt.Equal(m.CreatedAt)
}

func (s *restoreState) dumped(entry string) bool {
	for _, d := range s.Dumps {
		if d == entry {
			return true
		}
	}
	return false
}

func (a *App) restoreMarker(id string) string {
	return filepath.Join(a.Paths.RestoresDir(), id+".json")
}

// readRestoreState rend le témoin du projet, ou nil s'il n'y en a pas. Un
// témoin illisible est une erreur : le taire ferait passer une restauration
// inachevée pour un projet ordinaire.
func (a *App) readRestoreState(id string) (*restoreState, error) {
	data, err := os.ReadFile(a.restoreMarker(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s restoreState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("témoin de restauration %s illisible : %w", a.restoreMarker(id), err)
	}
	return &s, nil
}

// saveRestoreState écrit le témoin, atomiquement, dans un répertoire à root.
func (a *App) saveRestoreState(s *restoreState) error {
	if err := fs.EnsureDir(a.Paths.RestoresDir(), 0o700, fs.KeepOwner); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fs.WriteAtomic(a.restoreMarker(s.Project), append(data, '\n'), 0o600, fs.KeepOwner)
}

// clearRestoreState retire le témoin : la restauration est achevée, ou le
// projet supprimé.
func (a *App) clearRestoreState(id string) {
	if err := os.Remove(a.restoreMarker(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.warnf("⚠ témoin de restauration %s non retiré : %v\n", a.restoreMarker(id), err)
	}
}

// unfinishedRestore joint à l'erreur qui interrompt une restauration ce qui
// reste en place et les deux issues : la reprendre une fois la cause levée, ou y
// renoncer. Le code de l'erreur est gardé. Quand un moteur de base est
// injoignable, la suppression refuserait de partir sans sauvegarder des bases
// qu'elle ne peut pas lire : le remède dit « --no-dump », l'archive de la
// restauration gardant les sauvegardes.
func (a *App) unfinishedRestore(err error, id, archivePath string, p *model.Project) error {
	abandon := exec.ShellLine("ogs", "project", "delete", id)
	if p != nil && len(p.Spec.Databases) > 0 && (ExitCode(err) == CodeDependency || errors.Is(err, db.ErrUnavailable)) {
		abandon = exec.ShellLine("ogs", "project", "delete", id, "--no-dump") + " (moteur de base injoignable : " +
			"l'archive " + archivePath + " garde les sauvegardes)"
	}
	return &ExitError{Code: ExitCode(err), Err: fmt.Errorf("%w\nrestauration de %s inachevée : la déclaration et "+
		"les secrets restent en place, avec ce que l'apply n'a pas défait, notés dans %s — la reprendre une fois "+
		"la cause levée : %s ; ou y renoncer : %s", err, id, a.restoreMarker(id),
		exec.ShellLine("ogs", "project", "restore", "--resume", archivePath), abandon)}
}

// declarationDigest rend l'empreinte du fichier de déclaration du projet.
func (a *App) declarationDigest(id string) (string, error) {
	data, err := os.ReadFile(a.Paths.ProjectFile(id))
	if err != nil {
		return "", err
	}
	return digestOf(data), nil
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// addGaps note des manques, une fois chacun : une reprise retrouve ceux qu'une
// phase interrompue avait déjà nommés.
func (s *restoreState) addGaps(gaps ...restoreGap) {
	for _, g := range gaps {
		known := false
		for _, k := range s.Gaps {
			if k.What == g.What {
				known = true
				break
			}
		}
		if !known {
			s.Gaps = append(s.Gaps, g)
		}
	}
}

// unfinishedRestores rend les témoins de restauration présents, par projet.
// Un témoin qu'on ne lit pas est rendu vide : il n'en dit pas moins qu'une
// restauration est restée en plan.
func (a *App) unfinishedRestores() map[string]*restoreState {
	entries, err := os.ReadDir(a.Paths.RestoresDir())
	if err != nil {
		return nil
	}
	out := map[string]*restoreState{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		s, err := a.readRestoreState(id)
		if err != nil || s == nil {
			s = &restoreState{Project: id}
		}
		out[id] = s
	}
	return out
}

// restoreRemedy dit comment achever ou abandonner une restauration inachevée.
func (a *App) restoreRemedy(s *restoreState) string {
	if s.Archive == "" {
		return fmt.Sprintf("le témoin %s est illisible : achever par « ogs project restore --resume <archive> », "+
			"ou y renoncer par %s", a.restoreMarker(s.Project), exec.ShellLine("ogs", "project", "delete", s.Project))
	}
	return fmt.Sprintf("l'achever : %s ; ou y renoncer : %s",
		exec.ShellLine("ogs", "project", "restore", "--resume", s.Archive),
		exec.ShellLine("ogs", "project", "delete", s.Project))
}

// restoreChecks nomme, pour doctor, chaque restauration inachevée : son témoin
// n'est relu par rien d'autre, et une restauration abandonnée laisse un projet
// dont le home et les bases n'ont peut-être jamais été rendus. Un témoin sans
// déclaration est un reste, qu'on peut retirer.
func (a *App) restoreChecks(add func(name, status, detail, fix string)) {
	for id, s := range a.unfinishedRestores() {
		if _, err := os.Stat(a.Paths.ProjectFile(id)); err != nil {
			add("restauration de "+id, "avertissement",
				"témoin d'une restauration sans déclaration en place", "le retirer : rm "+
					exec.ShellLine(a.restoreMarker(id)))
			continue
		}
		add("restauration de "+id, "avertissement",
			"restauration inachevée : contenu ou bases peut-être non rendus, services peut-être non démarrés",
			a.restoreRemedy(s))
	}
}

// warnUnfinishedRestores avertit qu'un apply ordinaire porte sur un projet dont
// la restauration est inachevée : il en démarre les services sur un contenu que
// la restauration n'a peut-être pas rendu. Ce n'est pas un refus — l'apply est
// parfois le remède —, et la reprise reste possible ensuite.
func (a *App) warnUnfinishedRestores(projects []*model.Project) {
	for _, p := range projects {
		s, err := a.readRestoreState(p.ID())
		if err != nil || s == nil {
			continue
		}
		a.warnf("⚠ %s : restauration inachevée — cet apply démarre ses services sur un contenu qu'elle n'a "+
			"peut-être pas rendu ; %s\n", p.ID(), a.restoreRemedy(s))
	}
}
