package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

func TestUneBaseNonRestaureeNEstPasUnSucces(t *testing.T) {
	// restoreDumps ne faisait qu'afficher : « project restore » rendait 0 et
	// « ✓ restauré » sur une base vide. Il doit rendre l'échec, que la commande
	// compte en succès partiel (D92).
	prefix := t.TempDir()
	var out bytes.Buffer
	a := &App{Cfg: config.Default(), Paths: config.NewPaths(prefix), Exec: exec.NewRecorder(),
		Prefix: prefix, Out: &out, Format: "table"}
	p := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	if err := p.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	// Un compte principal borné, sans compte « all » : la restauration est
	// refusée d'emblée.
	p.Spec.Databases = []model.Database{{Engine: "mysql", Name: "shopfr", User: "shopfr", Privileges: "readonly"}}
	path := filepath.Join(prefix, "archive.tar.zst")
	w, err := archive.Create(path, archive.Manifest{Project: p.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("dump", "dumps/mysql/shopfr.sql", "", []byte("-- sauvegarde\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if missing, err := a.restoreDumps(cmd, path, p, manifest, nil, nil); len(missing) != 1 || err != nil {
		t.Errorf("DÉFAUT : une restauration refusée compte comme réussie :\n%s", out.String())
	}
	if !strings.Contains(out.String(), "« all »") {
		t.Errorf("le refus doit être affiché :\n%s", out.String())
	}
}

// appAnnulee construit une App sous un préfixe jetable, et une commande dont le
// contexte est déjà annulé — un Ctrl+C reçu.
func appAnnulee(t *testing.T) (*App, *cobra.Command, *bytes.Buffer) {
	t.Helper()
	prefix := t.TempDir()
	var out bytes.Buffer
	a := &App{Cfg: config.Default(), Paths: config.NewPaths(prefix), Exec: exec.NewRecorder(),
		Prefix: prefix, Out: &out, Format: "table"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	return a, cmd, &out
}

func projetAvecBase(t *testing.T, a *App, env naming.Env) *model.Project {
	t.Helper()
	p := model.New("shop.fr", env, model.KindStatic)
	if err := p.Normalize(filepath.Join(a.Prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	p.Spec.Databases = []model.Database{{Engine: "mysql", Name: p.ID(), User: p.ID(), Privileges: "all"}}
	return p
}

func TestUneRestaurationDArchiveInterrompueRendLeCode9(t *testing.T) {
	a, cmd, out := appAnnulee(t)
	p := projetAvecBase(t, a, naming.EnvProd)
	p.Spec.Databases = append(p.Spec.Databases, model.Database{Engine: "mysql", Name: "shopfr_z", User: p.ID(),
		Privileges: "all"})
	path := filepath.Join(a.Prefix, "archive.tar.zst")
	w, err := archive.Create(path, archive.Manifest{Project: p.ID()})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dumps/mysql/shopfr.sql", "dumps/mysql/shopfr_z.sql"} {
		if err := w.AddBytes("dump", name, "", []byte("-- sauvegarde\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.restoreDumps(cmd, path, p, manifest, nil, nil)
	if ExitCode(err) != CodeCancelled {
		t.Errorf("DÉFAUT : une interruption doit remonter en code %d, obtenu %v (%v) :\n%s",
			CodeCancelled, ExitCode(err), err, out)
	}
	if strings.Contains(out.String(), "shopfr_z") {
		t.Errorf("les bases suivantes ne doivent pas être tentées :\n%s", out)
	}
}

func TestUneCopieDesDonneesInterrompueRendLeCode9(t *testing.T) {
	a, cmd, _ := appAnnulee(t)
	source := projetAvecBase(t, a, naming.EnvProd)
	target := projetAvecBase(t, a, naming.EnvPrep)
	if err := a.cloneData(cmd, source, target); ExitCode(err) != CodeCancelled {
		t.Errorf("DÉFAUT : une interruption de la copie des données doit rendre le code %d : %v",
			CodeCancelled, err)
	}
}

// archiveDeSauvegardes écrit une archive qui ne porte que ces sauvegardes.
func archiveDeSauvegardes(t *testing.T, a *App, id string, noms ...string) (string, archive.Manifest) {
	t.Helper()
	path := filepath.Join(a.Prefix, "archive.tar.zst")
	w, err := archive.Create(path, archive.Manifest{Project: id})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range noms {
		if err := w.AddBytes("dump", name, "", []byte("-- sauvegarde\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return path, manifest
}

// Une archive antérieure à D113 range ses sauvegardes sous « dumps/<nom>.sql » :
// elle reste relisible tant que ce nom ne désigne qu'une base.
func TestUneArchiveAnterieureAD113ResteRelisible(t *testing.T) {
	a, _, out := appAnnulee(t)
	p := projetAvecBase(t, a, naming.EnvProd)
	// Un compte principal borné : la restauration est refusée avant tout accès
	// au moteur, ce qui suffit à dire à quelle base la sauvegarde est rapportée.
	p.Spec.Databases[0].Privileges = "readonly"
	path, manifest := archiveDeSauvegardes(t, a, p.ID(), "dumps/shopfr.sql")
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	missing, err := a.restoreDumps(cmd, path, p, manifest, nil, nil)
	if err != nil || len(missing) != 1 || !strings.Contains(missing[0].What, "base mysql shopfr non rechargée") {
		t.Errorf("DÉFAUT : la sauvegarde sans moteur doit être rapportée à l'unique base de ce nom : %v %v\n%s",
			missing, err, out)
	}
}

// Deux bases homonymes et une sauvegarde sans moteur : ogs ne choisit pas, il
// nomme la sauvegarde et le moyen de la recharger — l'ancien code la rechargeait
// dans la dernière base de ce nom déclarée, PostgreSQL pour une sauvegarde MySQL.
func TestUneSauvegardeAncienneAmbigueNEstRechargeeNullePart(t *testing.T) {
	a, _, out := appAnnulee(t)
	p := projetAvecBase(t, a, naming.EnvProd)
	p.Spec.Databases = append(p.Spec.Databases, model.Database{Engine: "postgres", Name: p.ID(), User: p.ID(),
		Privileges: "all"})
	path, manifest := archiveDeSauvegardes(t, a, p.ID(), "dumps/shopfr.sql", "dumps/shopfr.sql")
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	missing, err := a.restoreDumps(cmd, path, p, manifest, nil, nil)
	if err != nil || len(missing) != 1 || !strings.Contains(missing[0].What, "antérieure à D113") ||
		!strings.Contains(missing[0].Remedy, "ogs db restore shopfr <moteur>:shopfr -i") {
		t.Errorf("DÉFAUT : une sauvegarde ancienne ambiguë doit être nommée une fois, avec son remède : %v %v\n%s",
			missing, err, out)
	}
	if strings.Contains(out.String(), "restaurée") {
		t.Errorf("rien ne doit être rechargé :\n%s", out)
	}
}

// Une reprise ne recharge pas dans PostgreSQL une sauvegarde dont le
// rechargement a été interrompu : pg_dump ne retire rien, et le script
// s'arrêterait sur le premier objet déjà créé. La base est nommée.
func TestUneRepriseNommeLaBasePostgresAMoitieChargee(t *testing.T) {
	a, _, out := appAnnulee(t)
	p := projetAvecBase(t, a, naming.EnvProd)
	p.Spec.Databases[0].Engine = "postgres"
	path, manifest := archiveDeSauvegardes(t, a, p.ID(), "dumps/postgres/shopfr.sql")
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	state := &restoreState{Project: p.ID(), Loading: "dumps/postgres/shopfr.sql"}
	missing, err := a.restoreDumps(cmd, path, p, manifest, state, nil)
	if err != nil || len(missing) != 1 || !strings.Contains(missing[0].What, "chargée à moitié") {
		t.Errorf("DÉFAUT : la base interrompue doit être nommée : %v %v\n%s", missing, err, out)
	}
	if state.Loading != "" || !state.dumped("dumps/postgres/shopfr.sql") {
		t.Errorf("la sauvegarde doit être notée tentée : %+v", state)
	}
	// Déjà tentée, une seconde reprise la laisse.
	if missing, _ := a.restoreDumps(cmd, path, p, manifest, state, nil); len(missing) != 0 {
		t.Errorf("une sauvegarde déjà tentée ne se reprend pas : %v", missing)
	}
}
