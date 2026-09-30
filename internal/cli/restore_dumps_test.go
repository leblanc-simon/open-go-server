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
	if err := w.AddBytes("dump", "dumps/shopfr.sql", "", []byte("-- sauvegarde\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if ok, err := a.restoreDumps(cmd, path, p, manifest); ok || err != nil {
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
	path := filepath.Join(a.Prefix, "archive.tar.zst")
	w, err := archive.Create(path, archive.Manifest{Project: p.ID()})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dumps/shopfr.sql", "dumps/shopfr_z.sql"} {
		if err := w.AddBytes("dump", name, "", []byte("-- sauvegarde\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	ok, err := a.restoreDumps(cmd, path, p, manifest)
	if ok || ExitCode(err) != CodeCancelled {
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
