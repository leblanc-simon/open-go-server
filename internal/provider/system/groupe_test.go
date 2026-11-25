package system

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

// fixtureGroupe prépare un projet du groupe acme sous une racine des homes
// temporaire (D135).
func fixtureGroupe(t *testing.T, homeRoot, host string) (*model.Project, *Provider) {
	t.Helper()
	p := model.New(host, naming.EnvProd, model.KindStatic)
	p.Spec.Group = "acme"
	if err := p.Normalize(homeRoot, "preprod-pp.net",
		model.WithLogsRoot(filepath.Join(t.TempDir(), "ogs-projects"))); err != nil {
		t.Fatal(err)
	}
	prov := &Provider{Paths: config.NewPaths(t.TempDir()), Exec: exec.NewRecorder(), Identity: NewFakeIdentity()}
	return p, prov
}

func planSysteme(t *testing.T, prov *Provider, p *model.Project) []*engine.Step {
	t.Helper()
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), p, obs)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

func etape(steps []*engine.Step, id string) (int, *engine.Step) {
	for i, s := range steps {
		if s.Resource == id {
			return i, s
		}
	}
	return -1, nil
}

func TestLeRepertoireDuGroupePrecedeLeHomeEtSeCreeEn0701(t *testing.T) {
	homeRoot := t.TempDir()
	p, prov := fixtureGroupe(t, homeRoot, "shop.fr")
	steps := planSysteme(t, prov, p)
	ig, g := etape(steps, "system/group-dir/acme")
	ih, _ := etape(steps, "system/dir/"+p.Spec.Layout.Root)
	if g == nil || ih < 0 || ig > ih {
		t.Fatalf("l'étape du groupe doit précéder celle du home (%d, %d)", ig, ih)
	}
	if g.Action != engine.Create {
		t.Errorf("action : %v", g.Action)
	}
	if r := apply(t, prov, p); r.RolledBack {
		t.Fatal("aucun rollback attendu")
	}
	info, err := os.Lstat(filepath.Join(homeRoot, "acme"))
	if err != nil || info.Mode().Perm() != GroupDirMode {
		t.Fatalf("répertoire du groupe : %v, %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(homeRoot, "acme", "shop.fr")); err != nil {
		t.Errorf("home absent : %v", err)
	}
	// Un second membre trouve le répertoire conforme.
	q, prov2 := fixtureGroupe(t, homeRoot, "blog.fr")
	if _, s := etape(planSysteme(t, prov2, q), "system/group-dir/acme"); s == nil || s.Action != engine.NoOp {
		t.Errorf("répertoire du groupe d'un second membre : %+v", s)
	}
}

func TestLeModeDuRepertoireDuGroupeEstRetabli(t *testing.T) {
	homeRoot := t.TempDir()
	p, prov := fixtureGroupe(t, homeRoot, "shop.fr")
	apply(t, prov, p)
	dir := filepath.Join(homeRoot, "acme")
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, s := etape(planSysteme(t, prov, p), "system/group-dir/acme")
	if s == nil || s.Action != engine.Update || !strings.Contains(s.Summary, "0755 → 0701") {
		t.Fatalf("écart de mode : %+v", s)
	}
	apply(t, prov, p)
	if info, _ := os.Lstat(dir); info.Mode().Perm() != GroupDirMode {
		t.Errorf("mode après apply : %04o", info.Mode().Perm())
	}
}

func TestUnLienALaPlaceDuRepertoireDuGroupeNEstPasSuivi(t *testing.T) {
	homeRoot := t.TempDir()
	cible := t.TempDir()
	if err := os.Symlink(cible, filepath.Join(homeRoot, "acme")); err != nil {
		t.Fatal(err)
	}
	p, prov := fixtureGroupe(t, homeRoot, "shop.fr")
	_, s := etape(planSysteme(t, prov, p), "system/group-dir/acme")
	if s == nil || s.Apply == nil {
		t.Fatalf("étape attendue : %+v", s)
	}
	if err := s.Apply(context.Background()); err == nil {
		t.Error("un lien à la place du répertoire du groupe doit faire échouer l'étape")
	}
	if info, _ := os.Stat(cible); info.Mode().Perm() == GroupDirMode {
		t.Error("le mode a été posé sur la cible du lien")
	}
}

func TestLaCompensationNeRetireQuUnRepertoireDeGroupeVide(t *testing.T) {
	homeRoot := t.TempDir()
	p, prov := fixtureGroupe(t, homeRoot, "shop.fr")
	_, s := etape(planSysteme(t, prov, p), "system/group-dir/acme")
	ctx := context.Background()
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(homeRoot, "acme")
	if err := os.Mkdir(filepath.Join(dir, "blog.fr"), 0o711); err != nil {
		t.Fatal(err)
	}
	_ = s.Compensate(ctx)
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("un répertoire de groupe non vide a été retiré : %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "blog.fr")); err != nil {
		t.Fatal(err)
	}
	_ = s.Compensate(ctx)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("le répertoire vide doit partir : %v", err)
	}
}

func TestUnRepertoireDeGroupeQueRootNePossedePasEstRefuse(t *testing.T) {
	// Les tests ne tournent pas en root : le répertoire appartient à leur compte,
	// ce qui est justement le cas à refuser hors du mode simulé.
	homeRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(homeRoot, "acme"), 0o701); err != nil {
		t.Fatal(err)
	}
	p, prov := fixtureGroupe(t, homeRoot, "shop.fr")
	if os.Getuid() == 0 {
		t.Skip("le répertoire serait à root")
	}
	prov.Exec = &realExecutor{exec.NewRecorder()}
	prov.AccountOfUID = func(int) (string, error) { return "intrus", nil }
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	err = prov.PreexistingGroupDir(p, obs)
	if err == nil || !strings.Contains(err.Error(), "intrus") {
		t.Fatalf("répertoire de groupe à un compte ordinaire accepté : %v", err)
	}
	if _, err := prov.Plan(context.Background(), p, obs); err == nil {
		t.Error("le plan doit refuser lui aussi")
	}
}

func TestUnHomeHorsDuRepertoireDeSonGroupeEstRefuse(t *testing.T) {
	homeRoot := t.TempDir()
	p, prov := fixtureGroupe(t, homeRoot, "shop.fr")
	p.Spec.Layout.Root = filepath.Join(homeRoot, "shop.fr")
	if _, err := prov.Observe(context.Background(), p); err == nil {
		t.Error("un home hors du répertoire de son groupe doit être refusé")
	}
}

// realExecutor se déclare non simulé : les contrôles de propriétaire s'y
// exercent, les commandes restant enregistrées.
type realExecutor struct{ *exec.Recorder }

func (r *realExecutor) Simulated() bool { return false }
