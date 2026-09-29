package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// projetReel pose une première fois l'arborescence en mode simulé, puis passe en
// mode réel : ensureDir résout alors le propriétaire et le pose. Le projet a
// l'uid de l'appelant et, s'il en a un, un groupe secondaire distinct de son
// groupe primaire — le seul moyen de voir un fchown sans être root ; à défaut,
// le groupe primaire, et seul le mode témoigne.
func projetReel(t *testing.T) (*model.Project, *Provider, int) {
	t.Helper()
	p, prov, _, ident := fixture(t)
	prov.SkipWebUser = true
	apply(t, prov, p)

	uid, gid := os.Getuid(), os.Getgid()
	if groups, err := os.Getgroups(); err == nil {
		for _, g := range groups {
			if g != gid {
				gid = g
				break
			}
		}
	}
	ident.Users[p.Spec.User.Name] = &UserInfo{Name: p.Spec.User.Name, UID: uid, GID: gid,
		Home: p.Spec.Layout.Root, Managed: true}
	ident.Groups[p.Spec.User.Group] = &GroupInfo{Name: p.Spec.User.Group, GID: gid}
	prov.Exec = &exec.Local{}
	// Une première exécution réelle pose le groupe : l'arborescence est ensuite
	// conforme, et seuls les écarts du test restent à planifier.
	plan, _ := planDesRepertoires(t, prov, p)
	if _, err := engine.Execute(context.Background(), plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	return p, prov, gid
}

// planDesRepertoires observe puis planifie la seule arborescence.
func planDesRepertoires(t *testing.T, prov *Provider, p *model.Project) (*engine.Plan, []*engine.Step) {
	t.Helper()
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.dirSteps(p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	return plan, steps
}

func TestUnParentSubstitueEntreLePlanEtLApplyNEstPasSuivi(t *testing.T) {
	// L'utilisateur retire ~/.local/bin et ~/var/tmp : au plan, .local est un vrai
	// répertoire (NoOp), .local/bin et var/tmp sont à créer. Pendant l'apply —
	// la confirmation d'« ogs apply » lui en laisse tout le temps, l'étape de
	// var/tmp sert ici de signal —, il remplace .local par un lien : « mv .local
	// .x && ln -s /usr/local .local ». Root résolvait de nouveau .local/bin par
	// son nom, et posait mode et propriétaire du projet sur /usr/local/bin, en
	// tête de son propre PATH — ou créait un bin/ au projet ailleurs, /etc/bin.
	p, prov, gid := projetReel(t)
	ctx := context.Background()
	root := p.Spec.Layout.Root
	local := filepath.Join(root, ".local")
	bin := filepath.Join(root, model.LinkDir)
	vartmp := filepath.Join(root, "var", "tmp")

	usrLocal := t.TempDir()
	usrLocalBin := filepath.Join(usrLocal, "bin")
	if err := os.Mkdir(usrLocalBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(usrLocalBin, 0o755); err != nil {
		t.Fatal(err)
	}
	avant, err := os.Lstat(usrLocalBin)
	if err != nil {
		t.Fatal(err)
	}
	_, gidAvant, _ := OwnerOf(avant)

	for _, cas := range []struct{ nom, cible string }{{"usr-local", usrLocal}, {"etc", t.TempDir()}} {
		t.Run(cas.nom, func(t *testing.T) {
			for _, d := range []string{vartmp, bin} {
				if err := os.RemoveAll(d); err != nil {
					t.Fatal(err)
				}
			}
			if info, err := os.Lstat(local); err == nil && info.Mode()&os.ModeSymlink != 0 {
				if err := os.Remove(local); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(local, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(local, os.Getuid(), gid); err != nil {
				t.Fatal(err)
			}

			plan, steps := planDesRepertoires(t, prov, p)
			actions := map[string]engine.Action{}
			for _, s := range steps {
				actions[s.Resource] = s.Action
			}
			if actions["system/dir/"+local] != engine.NoOp || actions["system/dir/"+bin] != engine.Create ||
				actions["system/dir/"+vartmp] != engine.Create {
				t.Fatalf("préparation inattendue : %v", actions)
			}
			for _, s := range plan.Mutating() {
				if s.Resource != "system/dir/"+vartmp {
					continue
				}
				orig, cible := s.Apply, cas.cible
				s.Apply = func(ctx context.Context) error {
					if err := orig(ctx); err != nil {
						return err
					}
					if err := os.Rename(local, filepath.Join(root, ".x-"+filepath.Base(cible))); err != nil {
						return err
					}
					return os.Symlink(cible, local)
				}
			}
			report, err := engine.Execute(ctx, plan, engine.Options{})
			if err == nil {
				t.Error("l'étape de .local/bin doit échouer : son parent est devenu un lien")
			}
			var refus error
			for _, o := range report.Outcomes {
				if o.Step.Resource == "system/dir/"+bin {
					refus = o.Err
				}
			}
			if !errors.Is(refus, fs.ErrNotDirectory) {
				t.Errorf("le refus doit nommer le composant substitué : %v", refus)
			}

			cibleBin := filepath.Join(cas.cible, "bin")
			info, err := os.Lstat(cibleBin)
			if cas.cible != usrLocal {
				if err == nil {
					t.Errorf("DÉFAUT : root a créé %s hors du home, par le parent substitué", cibleBin)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o755 {
				t.Errorf("DÉFAUT : %s, hors du home, est passé en %04o par le parent substitué", cibleBin, got)
			}
			if _, g, _ := OwnerOf(info); g != gidAvant {
				t.Errorf("DÉFAUT : %s, hors du home, a reçu le groupe %d du projet (%d avant)", cibleBin, g, gidAvant)
			}
		})
	}
}

func TestLePlanNeDecritPasCeQuiEstAuBoutDUnParentLie(t *testing.T) {
	// .local est déjà un lien au plan : son étape échoue (R3N-10), et celle de
	// .local/bin ne doit pas annoncer « mode 0755 → 0700 » — la description du
	// bin/ de la cible, que l'os.Lstat du chemin complet rendait.
	p, prov, _ := projetReel(t)
	root := p.Spec.Layout.Root
	local := filepath.Join(root, ".local")
	bin := filepath.Join(root, model.LinkDir)
	if err := os.RemoveAll(local); err != nil {
		t.Fatal(err)
	}
	cible := t.TempDir()
	if err := os.Mkdir(filepath.Join(cible, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(cible, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cible, local); err != nil {
		t.Fatal(err)
	}
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if st := obs.Dirs[bin]; !st.LinkedParent || st.Mode != 0 {
		t.Errorf("le parent lié doit être constaté, sans décrire sa cible : %+v", st)
	}
	plan, _ := planDesRepertoires(t, prov, p)
	for _, s := range plan.Mutating() {
		if s.Resource == "system/dir/"+bin && !strings.Contains(s.Summary, "parent") {
			t.Errorf("le plan doit annoncer le parent lié, non une correction : %q", s.Summary)
		}
	}
	if _, err := engine.Execute(context.Background(), plan, engine.Options{}); err == nil {
		t.Error("un parent lié doit faire échouer le plan")
	}
	if info, _ := os.Lstat(filepath.Join(cible, "bin")); info.Mode().Perm() != 0o755 {
		t.Errorf("la cible a été corrigée : %04o", info.Mode().Perm())
	}
}

func TestLesCompensationsNeSuiventPasUnParentSubstitue(t *testing.T) {
	// Un rollback vient après coup : entre l'apply et lui, l'utilisateur a pu
	// remplacer var/ par un lien. La correction de var/tmp rendrait alors son
	// ancien mode — et son ancien propriétaire — au tmp/ de la cible, et le
	// retrait de la création un répertoire vide qui n'est pas le sien.
	p, prov, _ := projetReel(t)
	root := p.Spec.Layout.Root
	varDir := filepath.Join(root, "var")
	vartmp := filepath.Join(varDir, "tmp")
	sessions := filepath.Join(varDir, "sessions")
	if err := os.Chmod(vartmp, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sessions); err != nil {
		t.Fatal(err)
	}
	_, steps := planDesRepertoires(t, prov, p)
	var corrige, cree *engine.Step
	for _, s := range steps {
		switch s.Resource {
		case "system/dir/" + vartmp:
			corrige = s
		case "system/dir/" + sessions:
			cree = s
		}
	}
	if corrige == nil || corrige.Action != engine.Update || cree == nil || cree.Action != engine.Create {
		t.Fatalf("préparation inattendue : %v %v", corrige, cree)
	}
	for _, s := range []*engine.Step{corrige, cree} {
		if err := s.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	cible := t.TempDir()
	for _, d := range []string{"tmp", "sessions"} {
		if err := os.Mkdir(filepath.Join(cible, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(cible, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(varDir, filepath.Join(root, ".var-x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cible, varDir); err != nil {
		t.Fatal(err)
	}
	if err := corrige.Compensate(context.Background()); err == nil {
		t.Error("la compensation doit refuser un parent devenu lien")
	}
	_ = cree.Compensate(context.Background())
	if info, err := os.Lstat(filepath.Join(cible, "tmp")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("DÉFAUT : la compensation a traversé le parent substitué : %v %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(cible, "sessions")); err != nil {
		t.Errorf("DÉFAUT : la compensation a retiré le répertoire de la cible : %v", err)
	}
}
