package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
)

// Tests de D108 : clés, tables et rotation DKIM.

// perdreLaBase simule la perte de state.db : les clés scellées disparaissent,
// les fichiers restent sur le disque — et l'enregistrement dans le DNS.
func perdreLaBase(t *testing.T, f *fixture, p *model.Project) {
	t.Helper()
	keys, err := f.p.ProjectKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if err := f.p.Store.DeleteSecret(p.ID(), SecretName(k.Domain, k.Selector)); err != nil {
			t.Fatal(err)
		}
	}
}

// executer joue un plan comme le moteur, rechargements compris : c'est l'ordre
// réel des écritures, des rechargements et des retraits qui est en cause.
func executer(t *testing.T, f *fixture, steps []*engine.Step) *engine.Report {
	t.Helper()
	plan := &engine.Plan{}
	for _, s := range steps {
		plan.Add(s)
	}
	report, err := engine.Execute(context.Background(), plan, engine.Options{
		Reload: func(ctx context.Context, target string) error {
			if target != ReloadTarget {
				return nil
			}
			return f.p.Reload(ctx)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// R2-19 : après la perte de la base d'état, la clé en place est celle que le DNS
// publie. La régénérer — sous le même sélecteur, le même mois — écrasait le
// fichier publié sans copie, et les tables désignaient une clé que personne
// n'avait publiée.
func TestApresPerteDeLaBaseLaCleEnPlaceEstRescellee(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	path := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")
	publiee, _ := os.ReadFile(path)
	perdreLaBase(t, f, p)

	steps := mutating(f.plan(t, p))
	if len(steps) != 1 || !strings.Contains(steps[0].Summary, "reprise du fichier en place") {
		t.Fatalf("reprise de la clé en place attendue, obtenu %v", steps)
	}
	run(t, steps)
	if f.generate != 1 {
		t.Errorf("aucune clé ne doit être engendrée : %d génération(s)", f.generate)
	}
	if got, _ := os.ReadFile(path); string(got) != string(publiee) {
		t.Error("le fichier publié a été réécrit")
	}
	keys, _ := f.p.ProjectKeys(p)
	if len(keys) != 1 || keys[0].PEM != string(publiee) || keys[0].State != StateActive ||
		keys[0].Selector != "ogs202609" {
		t.Fatalf("la clé en place doit être rescellée active, obtenu %+v", keys)
	}
	if kt, _ := tables(t, f, p); !strings.Contains(kt, "ogs202609._domainkey.shop.fr") {
		t.Errorf("les tables doivent désigner la clé rescellée :\n%s", kt)
	}
	if m := mutating(f.plan(t, p)); len(m) != 0 {
		t.Errorf("rescellée, la clé est conforme : %v", m)
	}
}

// Un fichier que l'on ne peut pas tenir pour la clé d'ogs — trop ouvert, lié —
// n'est ni rescellé ni écrasé : la clé n'est pas régénérée non plus, et l'étape
// le dit.
func TestUnFichierEnPlaceDouteuxNEstNiRescelleNiEcrase(t *testing.T) {
	for _, cas := range []struct {
		nom   string
		gater func(t *testing.T, path string)
		motif string
	}{
		{"mode", func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "0644"},
		{"lien", func(t *testing.T, path string) {
			cible := filepath.Join(t.TempDir(), "ailleurs.private")
			data, _ := os.ReadFile(path)
			if err := os.WriteFile(cible, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(cible, path); err != nil {
				t.Fatal(err)
			}
		}, "fichier ordinaire"},
		{"illisible", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("pas une clé\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "PEM"},
	} {
		t.Run(cas.nom, func(t *testing.T) {
			f := newFixture(t)
			p := project(t, "shop.fr", &model.DKIM{Enabled: true})
			run(t, f.plan(t, p))
			path := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")
			perdreLaBase(t, f, p)
			cas.gater(t, path)
			avant, _ := os.Lstat(path)

			steps := mutating(f.plan(t, p))
			if len(steps) != 1 {
				t.Fatalf("une étape de refus attendue, obtenu %v", steps)
			}
			err := steps[0].Apply(context.Background())
			if err == nil || !strings.Contains(err.Error(), cas.motif) || steps[0].Critical {
				t.Errorf("refus non critique nommant %q attendu, obtenu %v", cas.motif, err)
			}
			if f.generate != 1 {
				t.Errorf("aucune clé ne doit être engendrée : %d génération(s)", f.generate)
			}
			if apres, _ := os.Lstat(path); apres == nil || apres.Mode() != avant.Mode() {
				t.Error("le fichier en place a été touché")
			}
			if keys, _ := f.p.ProjectKeys(p); len(keys) != 0 {
				t.Errorf("rien ne doit être scellé : %v", keys)
			}
		})
	}
}

// Une clé neuve ne prend jamais le sélecteur d'un fichier en place que le
// keystore ne connaît pas : elle l'écraserait sans copie.
func TestUnSelecteurNeufEviteLesFichiersEnPlace(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	// Un fichier inconnu du keystore, au sélecteur du mois suivant.
	inconnu := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202610")
	if err := os.WriteFile(inconnu, []byte(keyPool(t)[3]), 0o600); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.AddDate(0, 1, 0)
	steps, err := f.p.Rotate(context.Background(), p, "", f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || strings.Contains(steps[0].Summary, "sélecteur ogs202610)") {
		t.Fatalf("le sélecteur d'un fichier en place ne doit pas être repris : %v", steps)
	}
	run(t, steps)
	if got, _ := os.ReadFile(inconnu); string(got) != keyPool(t)[3] {
		t.Error("le fichier en place a été écrasé")
	}
}

// R2-22 : un keystore indisponible ne fait plus échouer le plan global de tout
// apply. Une étape en échec nomme la cause, et aucune table n'est retirée ni
// réécrite : recalculées sans les clés, elles cesseraient de signer.
func TestUnKeystoreIndisponibleNArretePasLePlanGlobal(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	quiet := project(t, "calme.fr", nil)
	run(t, f.plan(t, p))
	ktAvant, stAvant := tables(t, f, p, quiet)
	f.p.Keys = nil

	steps, err := f.p.PlanTables([]*model.Project{p, quiet}, f.observe(t))
	if err != nil {
		t.Fatalf("le plan global ne doit pas échouer : %v", err)
	}
	var cause error
	for _, s := range mutating(steps) {
		if s.Critical {
			t.Errorf("%s : aucune étape DKIM n'est critique", s.Summary)
		}
		if strings.Contains(s.Resource, "table") {
			t.Errorf("aucune table ne doit être touchée : %s", s.Summary)
		}
		if err := s.Apply(context.Background()); err != nil {
			cause = err
		}
	}
	if cause == nil || !strings.Contains(cause.Error(), "keystore") {
		t.Errorf("une étape en échec nommant le keystore attendue, obtenu %v", cause)
	}
	kt, _ := os.ReadFile(f.paths.OpenDKIMKeyTable())
	st, _ := os.ReadFile(f.paths.OpenDKIMSigningTable())
	if string(kt) != ktAvant || string(st) != stAvant {
		t.Error("les tables ont changé")
	}

	// Le plan du projet non plus ne doit pas échouer : le site se déploie, la
	// signature attend.
	projSteps, err := f.p.Plan(context.Background(), p, f.observe(t))
	if err != nil {
		t.Fatalf("le plan du projet ne doit pas échouer : %v", err)
	}
	if m := mutating(projSteps); len(m) != 1 || m[0].Critical || m[0].Apply(context.Background()) == nil {
		t.Errorf("une étape non critique en échec attendue, obtenu %v", m)
	}
}

// R2-30 : mail.dkim.enabled: false alors qu'opendkim.conf désigne les tables
// d'ogs. Les retirer empêchait OpenDKIM de redémarrer ; elles sont vidées, plus
// rien n'est signé, et « doctor » dit qu'opendkim.conf les désigne encore.
func TestDKIMDesactiveVideLesTablesQueDesigneOpendkimConf(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	conf := "KeyTable " + f.paths.OpenDKIMKeyTable() + "\nSigningTable refile:" + f.paths.OpenDKIMSigningTable() + "\n"
	if err := os.MkdirAll(filepath.Dir(f.paths.OpenDKIMConf()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.paths.OpenDKIMConf(), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	disabled := false
	f.p.Cfg.Mail.DKIM.Enabled = &disabled
	obs := f.observe(t)
	if obs.Present {
		t.Fatal("mail.dkim.enabled: false doit valoir OpenDKIM absent")
	}
	steps, err := f.p.PlanTables([]*model.Project{p}, obs)
	if err != nil {
		t.Fatal(err)
	}
	run(t, steps)
	for _, path := range []string{f.paths.OpenDKIMKeyTable(), f.paths.OpenDKIMSigningTable()} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s doit rester en place, opendkim.conf le désigne : %v", path, err)
		}
		if strings.Contains(string(data), "shop.fr") {
			t.Errorf("%s doit être vidée :\n%s", path, data)
		}
	}
	found := false
	for _, fd := range f.p.Checks(context.Background(), []*model.Project{p}, obs) {
		if strings.Contains(fd.Detail, "opendkim.conf") || strings.Contains(fd.Detail, f.paths.OpenDKIMConf()) {
			found = fd.Status == statusWarning
		}
	}
	if !found {
		t.Error("« doctor » doit dire qu'opendkim.conf désigne encore les tables d'ogs")
	}
}

// executeur non simulé : OpenDKIM est « réellement » rechargé, et le
// rechargement peut échouer.
type executeur struct{ *exec.Recorder }

func (executeur) Simulated() bool { return false }

// R4-11 : le fichier d'un domaine qui n'est plus signé ne part que si les tables
// ne le désignent plus et qu'OpenDKIM les a relues. Sinon OpenDKIM rend une
// erreur temporaire et le MTA garde en file tout le courrier du domaine.
func TestUneCleEncoreDesigneeNePartJamais(t *testing.T) {
	prepare := func(t *testing.T) (*fixture, *model.Project, string) {
		f := newFixture(t)
		p := project(t, "shop.fr", &model.DKIM{Enabled: true})
		run(t, f.plan(t, p))
		tables(t, f, p)
		p.Spec.Mail = nil
		return f, p, f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")
	}
	t.Run("table refusée", func(t *testing.T) {
		f, p, path := prepare(t)
		st := f.paths.OpenDKIMSigningTable()
		data, _ := os.ReadFile(st)
		if err := os.WriteFile(st, append(data, "# retouche\n"...), 0o640); err != nil {
			t.Fatal(err)
		}
		steps, err := f.p.PlanTables([]*model.Project{p}, f.observe(t))
		if err != nil {
			t.Fatal(err)
		}
		executer(t, f, steps)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("la SigningTable désigne encore la clé : son fichier devait rester (%v)", err)
		}
	})
	t.Run("rechargement en échec", func(t *testing.T) {
		f, p, path := prepare(t)
		rec := exec.NewRecorder()
		rec.Errors["opendkim -n"] = errors.New("configuration refusée")
		f.p.Exec = executeur{rec}
		obs := Observed{Present: true, UID: -1, GID: -1}
		steps, err := f.p.PlanTables([]*model.Project{p}, obs)
		if err != nil {
			t.Fatal(err)
		}
		report := executer(t, f, steps)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("OpenDKIM n'a pas relu les tables : le fichier devait rester (%v)", err)
		}
		if len(report.Warnings) == 0 {
			t.Error("le retrait différé doit être signalé")
		}
	})
	t.Run("tables et rechargement réussis", func(t *testing.T) {
		f, p, path := prepare(t)
		steps, err := f.p.PlanTables([]*model.Project{p}, f.observe(t))
		if err != nil {
			t.Fatal(err)
		}
		executer(t, f, steps)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("le fichier doit partir : %v", err)
		}
	})
}

// R4-12 : l'expiration d'une clé remplacée se faisait au plan du projet, avant
// les tables : si la bascule n'avait pas été suivie de leur recalcul, la clé
// partait alors que les tables la désignaient encore.
func TestLExpirationAttendQueLesTablesNeDesignentPlusLaCle(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	steps, err := f.p.Rotate(context.Background(), p, "", f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	run(t, steps)
	pending, _ := f.p.Pending(p, "")
	if err := f.p.Activate(p, pending).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	// La bascule est scellée, mais les tables n'ont pas été recalculées : elles
	// désignent toujours l'ancienne clé, et l'une d'elles a été retouchée.
	st := f.paths.OpenDKIMSigningTable()
	data, _ := os.ReadFile(st)
	if err := os.WriteFile(st, append(data, "# retouche\n"...), 0o640); err != nil {
		t.Fatal(err)
	}
	old := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")
	f.now = f.now.Add(8 * 24 * time.Hour)

	if m := mutating(f.plan(t, p)); len(m) != 0 {
		t.Fatalf("le plan du projet ne retire plus de clé : %v", m)
	}
	global, err := f.p.PlanTables([]*model.Project{p}, f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	executer(t, f, global)
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("la SigningTable désigne encore l'ancienne clé : elle devait rester (%v)", err)
	}
	if keys, _ := f.p.ProjectKeys(p); len(keys) != 2 {
		t.Errorf("la clé remplacée doit rester scellée tant qu'elle n'est pas retirée : %v", keys)
	}

	// La table rétablie, l'expiration a lieu, après les tables.
	if err := os.WriteFile(st, data, 0o640); err != nil {
		t.Fatal(err)
	}
	global, err = f.p.PlanTables([]*model.Project{p}, f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	executer(t, f, global)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("la clé remplacée doit partir une fois les tables à jour : %v", err)
	}
	if keys, _ := f.p.ProjectKeys(p); len(keys) != 1 || keys[0].Selector != "ogs202609b" {
		t.Errorf("seule la clé active doit rester scellée, obtenu %v", keys)
	}
}

// R4-14 : les domaines signés sont canonisés à la lecture. La validation
// comparait des formes normalisées, mais ogs employait la chaîne brute : « Shop.FR. »
// donnait une clé sous un autre répertoire et des lignes de table que
// l'enregistrement publié ne reconnaissait pas.
func TestLesDomainesSignesSontCanonises(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true, Domains: []string{" Shop.FR. "}})
	if got := p.DKIMDomains(); len(got) != 1 || got[0] != "shop.fr" {
		t.Fatalf("domaine canonisé attendu, obtenu %q", got)
	}
	run(t, f.plan(t, p))
	kt, st := tables(t, f, p)
	if !strings.Contains(st, "*@shop.fr ogs202609._domainkey.shop.fr\n") {
		t.Errorf("SigningTable non canonisée :\n%s", st)
	}
	if !strings.Contains(kt, f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")) {
		t.Errorf("KeyTable non canonisée :\n%s", kt)
	}
}

// Une clé déjà scellée sous une forme brute — « Shop.fr », accepté jusque-là —
// n'est pas régénérée : elle est publiée.
func TestUneCleScelleeSousUneFormeBruteEstReprise(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", nil)
	k := Key{Domain: "Shop.fr", Selector: "ogs202601", State: StateActive,
		Created: f.now.Add(-time.Hour), Activated: f.now.Add(-time.Hour), PEM: keyPool(t)[2]}
	if err := f.p.sealKey(p.ID(), k); err != nil {
		t.Fatal(err)
	}
	if err := f.p.writeKey(f.paths.OpenDKIMKeyFile(p.ID(), "Shop.fr", "ogs202601"), k, f.observe(t)); err != nil {
		t.Fatal(err)
	}
	p.Spec.Mail = &model.Mail{DKIM: &model.DKIM{Enabled: true, Domains: []string{"Shop.fr"}}}
	if m := mutating(f.plan(t, p)); len(m) != 0 {
		t.Fatalf("la clé scellée doit être reconnue, obtenu %v", m)
	}
	kt, st := tables(t, f, p)
	if !strings.Contains(st, "*@shop.fr ogs202601._domainkey.shop.fr") ||
		!strings.Contains(kt, f.paths.OpenDKIMKeyFile(p.ID(), "Shop.fr", "ogs202601")) {
		t.Errorf("les tables doivent désigner la clé en place :\n%s\n%s", kt, st)
	}
	if f.generate != 0 {
		t.Errorf("aucune clé ne doit être engendrée : %d", f.generate)
	}
}

// Plusieurs fichiers en place : la KeyTable dit lequel OpenDKIM emploie. Sans
// elle, ogs ne choisit pas — et n'engendre rien.
func TestPlusieursFichiersEnPlaceLaKeyTableTranche(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	autre := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202608")
	if err := os.WriteFile(autre, []byte(keyPool(t)[3]), 0o600); err != nil {
		t.Fatal(err)
	}
	perdreLaBase(t, f, p)
	steps := mutating(f.plan(t, p))
	if len(steps) != 1 || !strings.Contains(steps[0].Summary, "sélecteur ogs202609") ||
		!strings.Contains(steps[0].Summary, "reprise") {
		t.Fatalf("la clé que désigne la KeyTable doit être reprise, obtenu %v", steps)
	}

	if err := os.Remove(f.paths.OpenDKIMKeyTable()); err != nil {
		t.Fatal(err)
	}
	steps = mutating(f.plan(t, p))
	if len(steps) != 1 || steps[0].Critical {
		t.Fatalf("un refus non critique attendu, obtenu %v", steps)
	}
	if err := steps[0].Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "ogs202608") {
		t.Errorf("le refus doit nommer les fichiers en place : %v", err)
	}
	if f.generate != 1 {
		t.Errorf("aucune clé ne doit être engendrée : %d génération(s)", f.generate)
	}
}

// La suppression d'un projet retire ses clés après le recalcul des tables ; un
// recalcul en échec n'y est qu'un avertissement, et les fichiers restent alors.
func TestLaSuppressionNeRetirePasUneCleEncoreDesignee(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	dir := filepath.Join(f.paths.OpenDKIMKeysDir(), p.ID())
	if err := f.p.RemoveProject(p.ID()); err == nil || !strings.Contains(err.Error(), "KeyTable") {
		t.Errorf("refus nommant la KeyTable attendu, obtenu %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("les clés devaient rester : %v", err)
	}
	tables(t, f)
	if err := f.p.RemoveProject(p.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("les clés devaient partir : %v", err)
	}
}

// Relecture (1) : les deux tables s'écrivent ensemble ou pas du tout. Une
// SigningTable retouchée refusée pendant que la KeyTable était réécrite sans le
// domaine laissait la première désigner un nom absent de la seconde : OpenDKIM
// rendait une erreur temporaire, et le courrier restait en file.
func TestLesDeuxTablesSEcriventEnsembleOuPasDuTout(t *testing.T) {
	f := newFixture(t)
	shop := project(t, "shop.fr", &model.DKIM{Enabled: true})
	blog := project(t, "blog.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, shop))
	run(t, f.plan(t, blog))
	tables(t, f, shop, blog)
	kt := f.paths.OpenDKIMKeyTable()
	avant, _ := os.ReadFile(kt)
	st := f.paths.OpenDKIMSigningTable()
	data, _ := os.ReadFile(st)
	if err := os.WriteFile(st, append(data, "# retouche\n"...), 0o640); err != nil {
		t.Fatal(err)
	}
	shop.Spec.Mail = nil
	steps, err := f.p.PlanTables([]*model.Project{shop, blog}, f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	report := executer(t, f, steps)
	if apres, _ := os.ReadFile(kt); string(apres) != string(avant) {
		t.Errorf("la SigningTable refusée, la KeyTable devait rester en l'état :\n%s", apres)
	}
	signing, _ := os.ReadFile(st)
	keyTable, _ := os.ReadFile(kt)
	for _, line := range strings.Split(string(signing), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && !strings.HasPrefix(fields[0], "#") &&
			!strings.Contains(string(keyTable), fields[1]+" ") {
			t.Errorf("la SigningTable désigne %s, absent de la KeyTable", fields[1])
		}
	}
	if _, err := os.Stat(f.paths.OpenDKIMKeyFile(shop.ID(), "shop.fr", "ogs202609")); err != nil {
		t.Errorf("la clé encore désignée devait rester : %v", err)
	}
	dit := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "ne s'écrivent qu'ensemble") {
			dit = true
		}
	}
	if !dit {
		t.Errorf("l'étape de la KeyTable doit dire pourquoi elle reste en l'état : %v", report.Warnings)
	}
}

// Relecture (2) : une clé scellée sous une forme brute vit sous keys/<id>/Shop.fr/.
// Après la perte de la base, ne la chercher que sous la forme canonique faisait
// engendrer une clé neuve, et les tables désignaient une clé non publiée.
func TestApresPerteDeLaBaseLaCleSousUneFormeBruteEstRescellee(t *testing.T) {
	for _, avecTables := range []bool{true, false} {
		t.Run(map[bool]string{true: "KeyTable en place", false: "sans KeyTable"}[avecTables], func(t *testing.T) {
			f := newFixture(t)
			p := project(t, "shop.fr", nil)
			k := Key{Domain: "Shop.fr", Selector: "ogs202601", State: StateActive,
				Created: f.now.Add(-time.Hour), Activated: f.now.Add(-time.Hour), PEM: keyPool(t)[2]}
			if err := f.p.sealKey(p.ID(), k); err != nil {
				t.Fatal(err)
			}
			path := f.paths.OpenDKIMKeyFile(p.ID(), "Shop.fr", "ogs202601")
			if err := f.p.writeKey(path, k, f.observe(t)); err != nil {
				t.Fatal(err)
			}
			p.Spec.Mail = &model.Mail{DKIM: &model.DKIM{Enabled: true, Domains: []string{"Shop.fr"}}}
			if avecTables {
				tables(t, f, p)
			}
			perdreLaBase(t, f, p)

			steps := mutating(f.plan(t, p))
			if len(steps) != 1 || !strings.Contains(steps[0].Summary, "reprise du fichier en place") {
				t.Fatalf("reprise de la clé publiée attendue, obtenu %v", steps)
			}
			run(t, steps)
			if f.generate != 0 {
				t.Errorf("aucune clé ne doit être engendrée : %d", f.generate)
			}
			kt, _ := tables(t, f, p)
			if !strings.Contains(kt, path) {
				t.Errorf("les tables doivent désigner la clé publiée :\n%s", kt)
			}
			if m := mutating(f.plan(t, p)); len(m) != 0 {
				t.Errorf("rescellée, la clé est conforme : %v", m)
			}
		})
	}
}

// Relecture (3) : un lien physique ne se voit pas comme un lien. La clé de
// blogfr liée par root dans le répertoire de newsfr aurait été rescellée pour
// newsfr.
func TestUnLienPhysiqueNEstPasRescelle(t *testing.T) {
	f := newFixture(t)
	blog := project(t, "blog.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, blog))
	news := project(t, "news.fr", &model.DKIM{Enabled: true})
	dir := filepath.Dir(f.paths.OpenDKIMKeyFile(news.ID(), "news.fr", "x"))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	lien := f.paths.OpenDKIMKeyFile(news.ID(), "news.fr", "ogs202609")
	if err := os.Link(f.paths.OpenDKIMKeyFile(blog.ID(), "blog.fr", "ogs202609"), lien); err != nil {
		t.Fatal(err)
	}
	steps := mutating(f.plan(t, news))
	if len(steps) != 1 {
		t.Fatalf("une étape de refus attendue, obtenu %v", steps)
	}
	if err := steps[0].Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "lien physique") {
		t.Errorf("refus nommant le lien physique attendu, obtenu %v", err)
	}
	if keys, _ := f.p.ProjectKeys(news); len(keys) != 0 {
		t.Errorf("rien ne doit être scellé pour newsfr : %v", keys)
	}
}

// Relecture (4) : un fichier en place refusé au rescellement — une clé 0400,
// qu'OpenDKIM accepte — ne fait pas cesser de signer le domaine : la ligne que
// la KeyTable lui consacre est gardée, comme les tables d'un keystore illisible.
func TestUnFichierRefuseAuRescellementGardeSaLigne(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	path := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")
	perdreLaBase(t, f, p)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	steps := mutating(f.plan(t, p))
	if len(steps) != 1 {
		t.Fatalf("une étape de refus attendue, obtenu %v", steps)
	}
	err := steps[0].Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ligne") {
		t.Errorf("le refus doit dire que la ligne est gardée : %v", err)
	}
	kt, st := tables(t, f, p)
	if !strings.Contains(kt, path) || !strings.Contains(st, "*@shop.fr ogs202609._domainkey.shop.fr") {
		t.Errorf("le domaine doit rester signé par la clé en place :\n%s\n%s", kt, st)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if kt, _ := tables(t, f, p); strings.Contains(kt, path) {
		t.Errorf("une ligne gardée ne désigne jamais un fichier absent :\n%s", kt)
	}
}
