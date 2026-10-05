package mail

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/store"
)

// Engendrer une clé RSA coûte : un petit réservoir, tiré une fois pour tous les
// tests, suffit à distinguer une clé réécrite d'une clé régénérée.
var (
	poolOnce sync.Once
	pool     []string
)

func keyPool(t *testing.T) []string {
	t.Helper()
	poolOnce.Do(func() {
		for range 4 {
			k, err := GenerateKey(2048)
			if err != nil {
				panic(err)
			}
			pool = append(pool, k)
		}
	})
	return pool
}

type fixture struct {
	p        *Provider
	paths    config.Paths
	now      time.Time
	generate int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	st, err := store.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keyPath := filepath.Join(prefix, "master.key")
	if err := secret.GenerateKey(keyPath); err != nil {
		t.Fatal(err)
	}
	ks, err := secret.Open(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	keys := keyPool(t)
	f := &fixture{paths: paths, now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	f.p = &Provider{
		Paths: paths, Cfg: config.Default(), Exec: exec.NewRecorder(),
		Store: st, Keys: ks,
		Now: func() time.Time { return f.now },
		Generate: func(int) (string, error) {
			k := keys[f.generate%len(keys)]
			f.generate++
			return k, nil
		},
	}
	return f
}

func project(t *testing.T, host string, dkim *model.DKIM, extra ...model.Domain) *model.Project {
	t.Helper()
	p := model.New(host, naming.EnvProd, model.KindStatic)
	p.Spec.Domains = append([]model.Domain{{Name: host, Role: "primary"}}, extra...)
	if dkim != nil {
		p.Spec.Mail = &model.Mail{DKIM: dkim}
	}
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fixture) observe(t *testing.T) Observed {
	t.Helper()
	obs, err := f.p.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

func (f *fixture) plan(t *testing.T, p *model.Project) []*engine.Step {
	t.Helper()
	steps, err := f.p.Plan(context.Background(), p, f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

func run(t *testing.T, steps []*engine.Step) {
	t.Helper()
	for _, s := range steps {
		if !s.Mutates() {
			continue
		}
		if err := s.Apply(context.Background()); err != nil {
			t.Fatalf("%s : %v", s.Summary, err)
		}
	}
}

func mutating(steps []*engine.Step) []*engine.Step {
	var out []*engine.Step
	for _, s := range steps {
		if s.Mutates() {
			out = append(out, s)
		}
	}
	return out
}

func TestUneCleEstCreeeScelleeEtEcriteEn0600(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	steps := mutating(f.plan(t, p))
	if len(steps) != 1 || steps[0].Action != engine.Create {
		t.Fatalf("une création attendue, obtenu %v", steps)
	}
	if steps[0].Critical {
		t.Error("une clé qui n'est pas posée ne doit pas défaire le site : étape non critique attendue")
	}
	if f.generate != 0 {
		t.Fatal("« plan » ne doit rien engendrer : la clé serait perdue si l'on renonçait à l'apply")
	}
	run(t, steps)

	keys, err := f.p.ProjectKeys(p)
	if err != nil || len(keys) != 1 {
		t.Fatalf("une clé scellée attendue, obtenu %v (%v)", keys, err)
	}
	k := keys[0]
	if k.State != StateActive || k.Selector != "ogs202609" {
		t.Errorf("clé active sous ogs202609 attendue, obtenu %s sous %s", k.State, k.Selector)
	}
	path := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", k.Selector)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("RequireSafeKeys refuse une clé ouverte : 0600 attendu, obtenu %04o", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != k.PEM {
		t.Error("le fichier doit porter la clé scellée, à l'octet près")
	}
	if len(mutating(f.plan(t, p))) != 0 {
		t.Error("un second plan doit être vide")
	}
}

func TestLesDomainesSignesParDefautSontLesPrimaires(t *testing.T) {
	p := project(t, "shop.fr", &model.DKIM{Enabled: true},
		model.Domain{Name: "www.shop.fr", Role: "redirect", Target: "shop.fr"},
		model.Domain{Name: "api.shop.fr", Role: "primary"})
	got := strings.Join(p.DKIMDomains(), ",")
	if got != "shop.fr,api.shop.fr" {
		t.Errorf("domaines primaires attendus, sans la redirection ; obtenu %s", got)
	}
}

func TestUnFichierDisparuEstReecritJamaisRegenere(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	keys, _ := f.p.ProjectKeys(p)
	path := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", keys[0].Selector)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	steps := mutating(f.plan(t, p))
	if len(steps) != 1 || !strings.Contains(steps[0].Summary, "réécrite depuis le keystore") {
		t.Fatalf("réécriture attendue, obtenu %v", steps)
	}
	run(t, steps)
	if f.generate != 1 {
		t.Errorf("une clé neuve exigerait de republier chez le client : %d génération(s)", f.generate)
	}
	data, _ := os.ReadFile(path)
	if string(data) != keys[0].PEM {
		t.Error("le fichier réécrit doit être la clé scellée")
	}
}

func TestUnFichierModifieALaMainEstRefuseSansForce(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	keys, _ := f.p.ProjectKeys(p)
	path := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", keys[0].Selector)
	if err := os.WriteFile(path, []byte("autre chose\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	steps := mutating(f.plan(t, p))
	if len(steps) != 1 {
		t.Fatalf("une étape attendue, obtenu %v", steps)
	}
	if err := steps[0].Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("refus renvoyant à --force attendu, obtenu %v", err)
	}

	f.p.Force = true
	run(t, f.plan(t, p))
	data, _ := os.ReadFile(path)
	if string(data) != keys[0].PEM {
		t.Error("--force doit réécrire la clé scellée")
	}
	backups, _ := filepath.Glob(filepath.Join(f.paths.BackupsDir(), "*.private.ogs-bak-*"))
	if len(backups) != 1 {
		t.Errorf("une sauvegarde attendue hors du répertoire des clés, obtenu %v", backups)
	}
}

func TestDesDroitsElargisSontResserres(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	keys, _ := f.p.ProjectKeys(p)
	path := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", keys[0].Selector)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	steps := mutating(f.plan(t, p))
	if len(steps) != 1 || !strings.Contains(steps[0].Summary, "0644") {
		t.Fatalf("correction des droits attendue, obtenu %v", steps)
	}
	run(t, steps)
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("0600 attendu après correction, obtenu %04o", info.Mode().Perm())
	}
}

func TestOpenDKIMAbsentNeProduitRien(t *testing.T) {
	f := newFixture(t)
	off := false
	f.p.Cfg.Mail.DKIM.Enabled = &off
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	if steps := f.plan(t, p); len(steps) != 0 {
		t.Errorf("aucune étape attendue quand la signature est désactivée, obtenu %v", steps)
	}
}

func TestLaCreationSeCompense(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	steps := mutating(f.plan(t, p))
	run(t, steps)
	if err := steps[0].Compensate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if keys, _ := f.p.ProjectKeys(p); len(keys) != 0 {
		t.Error("la compensation doit retirer la clé scellée")
	}
	if _, err := os.Stat(f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")); !os.IsNotExist(err) {
		t.Error("la compensation doit retirer le fichier")
	}
}

func tables(t *testing.T, f *fixture, projects ...*model.Project) (string, string) {
	t.Helper()
	steps, err := f.p.PlanTables(projects, f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	run(t, steps)
	kt, _ := os.ReadFile(f.paths.OpenDKIMKeyTable())
	st, _ := os.ReadFile(f.paths.OpenDKIMSigningTable())
	return string(kt), string(st)
}

// deletions rend les retraits de clés du plan global : expirations et domaines
// qui ne sont plus signés (D108).
func deletions(t *testing.T, f *fixture, projects ...*model.Project) []*engine.Step {
	t.Helper()
	steps, err := f.p.PlanTables(projects, f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	var out []*engine.Step
	for _, s := range steps {
		if s.Action == engine.Delete && strings.HasPrefix(s.Resource, "mail/dkim/") {
			out = append(out, s)
		}
	}
	return out
}

func TestLesTablesPortentToutLeParc(t *testing.T) {
	f := newFixture(t)
	shop := project(t, "shop.fr", &model.DKIM{Enabled: true})
	blog := project(t, "blog.fr", &model.DKIM{Enabled: true})
	quiet := project(t, "calme.fr", nil)
	run(t, f.plan(t, shop))
	run(t, f.plan(t, blog))

	kt, st := tables(t, f, blog, quiet, shop)
	for _, want := range []string{
		"ogs202609._domainkey.shop.fr shop.fr:ogs202609:" + f.paths.OpenDKIMKeyFile("shopfr", "shop.fr", "ogs202609"),
		"ogs202609._domainkey.blog.fr blog.fr:ogs202609:",
	} {
		if !strings.Contains(kt, want) {
			t.Errorf("KeyTable sans %q :\n%s", want, kt)
		}
	}
	// La SigningTable est lue en expressions : une table littérale ne
	// reconnaîtrait aucun expéditeur, et rien ne serait signé sans un mot.
	if !strings.Contains(st, "*@shop.fr ogs202609._domainkey.shop.fr") ||
		!strings.Contains(st, "*@blog.fr ogs202609._domainkey.blog.fr") {
		t.Errorf("SigningTable incomplète :\n%s", st)
	}
	if strings.Contains(kt, "calme.fr") {
		t.Error("un projet qui ne demande pas de signature n'a rien à faire dans les tables")
	}

	// Recalculées sans le projet supprimé, elles l'oublient — et lui seul.
	kt, _ = tables(t, f, blog, quiet)
	if strings.Contains(kt, "shop.fr") || !strings.Contains(kt, "blog.fr") {
		t.Errorf("la suppression d'un projet doit le retirer, lui seul :\n%s", kt)
	}
}

func TestUneTableNeDesigneJamaisUneCleAbsente(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	if err := os.Remove(f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")); err != nil {
		t.Fatal(err)
	}
	// Une clé désignée mais introuvable fait rendre à OpenDKIM une erreur
	// temporaire : le courrier resterait en file, là où une absence de signature
	// l'aurait laissé partir.
	kt, _ := tables(t, f, p)
	if strings.Contains(kt, "shop.fr:") {
		t.Errorf("la KeyTable désigne une clé absente :\n%s", kt)
	}
}

func TestUneTableModifieeALaMainEstRefusee(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	if err := os.WriteFile(f.paths.OpenDKIMSigningTable(), []byte("*@autre.fr x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	steps, err := f.p.PlanTables([]*model.Project{p}, f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	var refus error
	for _, s := range mutating(steps) {
		if err := s.Apply(context.Background()); err != nil {
			refus = err
		}
	}
	if refus == nil || !strings.Contains(refus.Error(), "--force") {
		t.Errorf("refus renvoyant à --force attendu, obtenu %v", refus)
	}
}

// OpenDKIM désactivé, ogs retire ses tables — les siennes seulement, et comme
// il les a écrites : une table retouchée ne part que sous --force, une table
// sans la marque d'ogs est celle de l'administrateur.
func TestOpenDKIMDesactiveNeRetireQueSesTablesIntactes(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	keyTable, signingTable := f.paths.OpenDKIMKeyTable(), f.paths.OpenDKIMSigningTable()
	data, _ := os.ReadFile(keyTable)
	retouchee := append(data, []byte("maison._domainkey.autre.fr autre.fr:maison:/etc/opendkim/maison.key\n")...)
	if err := os.WriteFile(keyTable, retouchee, 0o640); err != nil {
		t.Fatal(err)
	}

	steps, err := f.p.PlanTables([]*model.Project{p}, Observed{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range mutating(steps) {
		err := s.Apply(context.Background())
		if s.Resource == "mail/dkim-keytable" && (err == nil || !strings.Contains(err.Error(), "--force")) {
			t.Errorf("la table retouchée doit être refusée, en nommant --force : %v", err)
		}
		if s.Resource == "mail/dkim-signingtable" && err != nil {
			t.Errorf("la table intacte doit partir : %v", err)
		}
	}
	if got, _ := os.ReadFile(keyTable); string(got) != string(retouchee) {
		t.Error("la table retouchée a été retirée sans --force")
	}
	if _, err := os.Stat(signingTable); !os.IsNotExist(err) {
		t.Errorf("la table intacte devait être retirée : %v", err)
	}

	// Une table posée par l'administrateur n'est jamais retirée, --force compris.
	if err := os.WriteFile(signingTable, []byte("*@autre.fr maison._domainkey.autre.fr\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	f.p.Force = true
	steps, err = f.p.PlanTables([]*model.Project{p}, Observed{})
	if err != nil {
		t.Fatal(err)
	}
	run(t, mutating(steps)[:1])
	if _, err := os.Stat(keyTable); !os.IsNotExist(err) {
		t.Errorf("la table retouchée devait partir sous --force : %v", err)
	}
	baks, _ := filepath.Glob(filepath.Join(f.paths.BackupsDir(), filepath.Base(keyTable)+".ogs-bak-*"))
	if len(baks) != 1 {
		t.Errorf("une sauvegarde de la table attendue, %d trouvée(s)", len(baks))
	}
	for _, s := range mutating(steps)[1:] {
		if err := s.Apply(context.Background()); err == nil {
			t.Errorf("%s : la table de l'administrateur doit être laissée en le disant", s.Resource)
		}
	}
	if _, err := os.Stat(signingTable); err != nil {
		t.Errorf("la table de l'administrateur a été retirée : %v", err)
	}
}

func TestUneTableConformeResteConformeLaSecondeSuivante(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	// L'en-tête porte un horodatage : sans comparaison d'empreintes, chaque apply
	// réécrirait les tables et rechargerait OpenDKIM.
	f.now = f.now.Add(time.Minute)
	steps, err := f.p.PlanTables([]*model.Project{p}, f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	if m := mutating(steps); len(m) != 0 {
		t.Errorf("tables conformes attendues, obtenu %v", m)
	}
}

func TestLaRotationSeFaitEnDeuxTemps(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	obs := f.observe(t)

	f.now = f.now.Add(24 * time.Hour)
	steps, err := f.p.Rotate(context.Background(), p, "", obs)
	if err != nil {
		t.Fatal(err)
	}
	run(t, steps)
	if _, err := f.p.Rotate(context.Background(), p, "", obs); !errors.Is(err, ErrAlreadyPending) {
		t.Errorf("une seconde rotation doit être refusée, obtenu %v", err)
	}
	// Préparée, la clé n'est pas encore désignée : basculer vers une clé non
	// publiée casserait la signature de tout envoi.
	_, st := tables(t, f, p)
	if !strings.Contains(st, "ogs202609._domainkey") || strings.Contains(st, "ogs202609b") {
		t.Fatalf("la SigningTable doit désigner l'ancienne clé tant que la bascule n'est pas faite :\n%s", st)
	}
	if len(mutating(f.plan(t, p))) != 0 {
		t.Error("une clé préparée ne doit rien changer au plan du projet")
	}

	pending, err := f.p.Pending(p, "")
	if err != nil || len(pending) != 1 || pending[0].Selector != "ogs202609b" {
		t.Fatalf("clé préparée ogs202609b attendue, obtenu %v (%v)", pending, err)
	}
	activation := f.p.Activate(p, pending)
	if err := activation.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, st = tables(t, f, p)
	if !strings.Contains(st, "ogs202609b._domainkey") {
		t.Errorf("la SigningTable doit désigner la nouvelle clé :\n%s", st)
	}
	keys, _ := f.p.ProjectKeys(p)
	states := map[string]string{}
	for _, k := range keys {
		states[k.Selector] = k.State
	}
	if states["ogs202609"] != StateRetired || states["ogs202609b"] != StateActive {
		t.Errorf("ancienne retirée et nouvelle active attendues, obtenu %v", states)
	}

	// La bascule se compense : l'ancienne clé redevient active.
	if err := activation.Compensate(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys, _ = f.p.ProjectKeys(p)
	for _, k := range keys {
		states[k.Selector] = k.State
	}
	if states["ogs202609"] != StateActive || states["ogs202609b"] != StatePending {
		t.Errorf("la compensation doit rétablir les états, obtenu %v", states)
	}
}

func TestUneCleRemplaceeDisparaitApresLaRetention(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	steps, err := f.p.Rotate(context.Background(), p, "", f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	run(t, steps)
	pending, _ := f.p.Pending(p, "")
	if err := f.p.Activate(p, pending).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")

	f.now = f.now.Add(6 * 24 * time.Hour)
	if len(deletions(t, f, p)) != 0 {
		t.Fatal("avant la fin de la rétention, la clé remplacée reste en place")
	}
	f.now = f.now.Add(2 * 24 * time.Hour)
	// L'expiration se fait au plan global, après les tables (D108), jamais au
	// plan du projet.
	if len(mutating(f.plan(t, p))) != 0 {
		t.Fatal("le plan du projet ne retire aucune clé")
	}
	steps = deletions(t, f, p)
	if len(steps) != 1 || steps[0].Action != engine.Delete ||
		!strings.Contains(steps[0].Summary, "ogs202609._domainkey.shop.fr") {
		t.Fatalf("retrait nommant l'enregistrement à retirer attendu, obtenu %v", steps)
	}
	run(t, steps)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("le fichier de la clé remplacée doit disparaître")
	}
	keys, _ := f.p.ProjectKeys(p)
	if len(keys) != 1 || keys[0].Selector != "ogs202609b" {
		t.Errorf("seule la clé active doit rester scellée, obtenu %v", keys)
	}
}

func TestUnDomaineQuiNEstPlusSigneGardeSaCle(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	p.Spec.Mail = nil
	if steps := mutating(f.plan(t, p)); len(steps) != 0 {
		t.Fatalf("le plan du projet ne doit pas retirer une clé que les tables désignent encore : %v", steps)
	}
	steps, err := f.p.PlanTables([]*model.Project{p}, f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	steps = mutating(steps)
	last := steps[len(steps)-1]
	if last.Action != engine.Delete || !last.FlushReloads {
		t.Fatalf("retrait du fichier après les tables rechargées attendu, obtenu %v", steps)
	}
	run(t, steps)
	if _, err := os.Stat(f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")); !os.IsNotExist(err) {
		t.Error("le fichier d'un domaine qui n'est plus signé doit être retiré")
	}
	if keys, _ := f.p.ProjectKeys(p); len(keys) != 1 {
		t.Error("la clé doit rester scellée : réactiver la signature retrouve l'enregistrement publié")
	}
	p.Spec.Mail = &model.Mail{DKIM: &model.DKIM{Enabled: true}}
	run(t, f.plan(t, p))
	if f.generate != 1 {
		t.Errorf("la réactivation doit reprendre la clé scellée, non en engendrer une : %d génération(s)", f.generate)
	}
}

// Le fichier d'une clé qui ne correspond plus à la clé scellée porte une clé
// privée que le keystore ne connaît pas : ni le domaine qui cesse d'être signé,
// ni l'expiration d'une clé remplacée ne l'emportent sans --force, copie faite.
func TestUneCleRetoucheeNEstRetireeQueSousForce(t *testing.T) {
	autreCle := keyPool(t)[3]
	retoucher := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(autreCle), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	verifier := func(t *testing.T, f *fixture, step *engine.Step, path string) {
		t.Helper()
		if !strings.Contains(step.Summary, "modifié hors ogs") {
			t.Fatalf("le retrait doit être refusé : %s", step.Summary)
		}
		if err := step.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "--force") {
			t.Errorf("le refus doit nommer --force : %v", err)
		}
		if data, _ := os.ReadFile(path); string(data) != autreCle {
			t.Fatal("la clé retouchée a été retirée sans --force")
		}
	}
	sauvegarde := func(t *testing.T, f *fixture, path string) {
		t.Helper()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("la clé devait être retirée sous --force : %v", err)
		}
		baks, _ := filepath.Glob(filepath.Join(f.paths.BackupsDir(), filepath.Base(path)+".ogs-bak-*"))
		if len(baks) != 1 {
			t.Fatalf("une sauvegarde attendue, %d trouvée(s)", len(baks))
		}
		if data, _ := os.ReadFile(baks[0]); string(data) != autreCle {
			t.Error("la sauvegarde doit porter la clé retouchée")
		}
	}

	t.Run("domaine qui n'est plus signé", func(t *testing.T) {
		f := newFixture(t)
		p := project(t, "shop.fr", &model.DKIM{Enabled: true})
		run(t, f.plan(t, p))
		tables(t, f, p)
		path := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")
		retoucher(t, path)
		p.Spec.Mail = nil
		retrait := func() []*engine.Step {
			steps, err := f.p.PlanTables([]*model.Project{p}, f.observe(t))
			if err != nil {
				t.Fatal(err)
			}
			return mutating(steps)
		}
		steps := retrait()
		verifier(t, f, steps[len(steps)-1], path)
		f.p.Force = true
		steps = retrait()
		if s := steps[len(steps)-1]; !strings.Contains(s.Summary, "retrait forcé") {
			t.Fatalf("le plan doit annoncer le retrait forcé : %s", s.Summary)
		}
		// Les tables d'abord : le fichier ne part qu'une fois qu'elles ne le
		// désignent plus (D108).
		run(t, steps)
		sauvegarde(t, f, path)
	})

	t.Run("clé remplacée dont la rétention est écoulée", func(t *testing.T) {
		f := newFixture(t)
		p := project(t, "shop.fr", &model.DKIM{Enabled: true})
		run(t, f.plan(t, p))
		steps, err := f.p.Rotate(context.Background(), p, "", f.observe(t))
		if err != nil {
			t.Fatal(err)
		}
		run(t, steps)
		pending, _ := f.p.Pending(p, "")
		if err := f.p.Activate(p, pending).Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
		path := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")
		retoucher(t, path)
		f.now = f.now.Add(8 * 24 * time.Hour)
		expiration := func() *engine.Step {
			if d := deletions(t, f, p); len(d) > 0 {
				return d[0]
			}
			t.Fatal("expiration attendue")
			return nil
		}
		verifier(t, f, expiration(), path)
		if keys, _ := f.p.ProjectKeys(p); len(keys) != 2 {
			t.Errorf("un refus ne défait rien à moitié : la clé remplacée reste scellée, obtenu %v", keys)
		}
		f.p.Force = true
		s := expiration()
		if !strings.Contains(s.Summary, "retrait forcé") {
			t.Fatalf("le plan doit annoncer le retrait forcé : %s", s.Summary)
		}
		if err := s.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
		sauvegarde(t, f, path)
	})
}

// L'expiration relit la clé au moment de la retirer, comme tout retrait : une
// clé intacte au plan, remplacée avant la confirmation, partait sans copie — et
// son secret avec elle.
func TestLExpirationRelitLaCleAvantDeLaRetirer(t *testing.T) {
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	steps, err := f.p.Rotate(context.Background(), p, "", f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	run(t, steps)
	pending, _ := f.p.Pending(p, "")
	if err := f.p.Activate(p, pending).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(8 * 24 * time.Hour)
	var expiration *engine.Step
	for _, s := range deletions(t, f, p) {
		expiration = s
	}
	if expiration == nil || strings.Contains(expiration.Summary, "hors ogs") {
		t.Fatalf("une expiration ordinaire est attendue : %+v", expiration)
	}

	path := f.paths.OpenDKIMKeyFile(p.ID(), "shop.fr", "ogs202609")
	autreCle := keyPool(t)[3]
	if err := os.WriteFile(path, []byte(autreCle), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := expiration.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("la clé remplacée après le plan doit être refusée, en nommant --force : %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != autreCle {
		t.Error("la clé remplacée après le plan a été retirée")
	}
	if keys, _ := f.p.ProjectKeys(p); len(keys) != 2 {
		t.Errorf("la clé remplacée doit rester scellée, obtenu %v", keys)
	}
}

func TestLeSelecteurSuivantDuMemeMoisRecoitUneLettre(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	if s := nextSelector("ogs", now, nil, nil); s != "ogs202609" {
		t.Errorf("ogs202609 attendu, obtenu %s", s)
	}
	if s := nextSelector("ogs", now, []Key{{Selector: "ogs202609"}, {Selector: "ogs202609b"}}, nil); s != "ogs202609c" {
		t.Errorf("ogs202609c attendu, obtenu %s", s)
	}
}

func TestLEnregistrementEstDecoupeEnChainesDe255Octets(t *testing.T) {
	k := Key{Domain: "shop.fr", Selector: "ogs202609", State: StateActive, PEM: keyPool(t)[0]}
	r, err := NewRecord(k)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Strings) < 2 {
		t.Fatalf("une clé RSA-2048 dépasse une chaîne TXT : découpage attendu, obtenu %d chaîne(s)", len(r.Strings))
	}
	for _, s := range r.Strings {
		if len(s) > 255 {
			t.Errorf("chaîne de %d octets", len(s))
		}
	}
	if strings.Join(r.Strings, "") != r.Value {
		t.Error("les chaînes recollées doivent rendre la valeur")
	}
	if !strings.HasPrefix(r.Value, "v=DKIM1; k=rsa; p=MIIB") {
		t.Errorf("valeur inattendue : %.40s…", r.Value)
	}
	zone := r.Zone()
	if !strings.HasPrefix(zone, "ogs202609._domainkey.shop.fr. IN TXT (") || !strings.HasSuffix(zone, ")") {
		t.Errorf("format de zone inattendu :\n%s", zone)
	}
}

func TestLaVerificationDNS(t *testing.T) {
	f := newFixture(t)
	k := Key{Domain: "shop.fr", Selector: "ogs202609", PEM: keyPool(t)[0]}
	other := Key{Domain: "shop.fr", Selector: "ogs202609", PEM: keyPool(t)[1]}
	r, _ := NewRecord(k)
	wrong, _ := NewRecord(other)

	cases := []struct {
		nom     string
		txt     []string
		err     error
		verdict string
	}{
		// Un exploitant qui colle la valeur en plusieurs morceaux y laisse des blancs.
		{"publié, recollé avec des blancs", []string{strings.Join(r.Strings, " \n ")}, nil, DNSMatch},
		{"publié parmi d'autres", []string{"google-site-verification=x", r.Value}, nil, DNSMatch},
		{"autre clé", []string{wrong.Value}, nil, DNSMismatch},
		{"aucun enregistrement DKIM", []string{"v=spf1 -all"}, nil, DNSAbsent},
		{"nom inexistant", nil, &net.DNSError{Err: "no such host", IsNotFound: true}, DNSAbsent},
		{"résolveur muet", nil, &net.DNSError{Err: "timeout", IsTimeout: true}, DNSUnreachable},
	}
	for _, c := range cases {
		t.Run(c.nom, func(t *testing.T) {
			f.p.LookupTXT = func(_ context.Context, name string) ([]string, error) {
				if name != "ogs202609._domainkey.shop.fr" {
					t.Errorf("nom interrogé inattendu : %s", name)
				}
				return c.txt, c.err
			}
			if v, detail := f.p.Verify(context.Background(), k); v != c.verdict {
				t.Errorf("%s attendu, obtenu %s (%s)", c.verdict, v, detail)
			}
		})
	}
}

func TestOpendkimConfDoitDesignerLesTablesDOgs(t *testing.T) {
	f := newFixture(t)
	conf := f.paths.OpenDKIMConf()
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		nom, contenu, status, extrait string
	}{
		{"conforme", "KeyTable " + f.paths.OpenDKIMKeyTable() + "\nsigningtable refile:" +
			f.paths.OpenDKIMSigningTable() + " # commentaire\n", statusOK, ""},
		{"lue littéralement", "KeyTable file:" + f.paths.OpenDKIMKeyTable() + "\nSigningTable " +
			f.paths.OpenDKIMSigningTable() + "\n", statusFailure, "lue littéralement"},
		{"absentes", "Syslog yes\n# KeyTable " + f.paths.OpenDKIMKeyTable() + "\n", statusFailure, "(absente)"},
	}
	for _, c := range cases {
		t.Run(c.nom, func(t *testing.T) {
			if err := os.WriteFile(conf, []byte(c.contenu), 0o644); err != nil {
				t.Fatal(err)
			}
			status, detail, _ := f.p.confCheck()
			if status != c.status || !strings.Contains(detail, c.extrait) {
				t.Errorf("%s « …%s… » attendu, obtenu %s « %s »", c.status, c.extrait, status, detail)
			}
		})
	}
}

func TestLesTablesDeLAdministrateurSontSignalees(t *testing.T) {
	f := newFixture(t)
	conf := f.paths.OpenDKIMConf()
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("KeyTable refile:/etc/opendkim/key.table\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if table, foreign := f.p.ForeignTables(); !foreign || table != "refile:/etc/opendkim/key.table" {
		t.Errorf("KeyTable étrangère attendue, obtenu %q %v", table, foreign)
	}
	if err := os.WriteFile(conf, []byte("KeyTable "+f.paths.OpenDKIMKeyTable()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, foreign := f.p.ForeignTables(); foreign {
		t.Error("les tables d'ogs ne sont pas étrangères")
	}
}

func TestLaCleEstLaPartiePubliqueDuPEM(t *testing.T) {
	pub, err := PublicKey(keyPool(t)[0])
	if err != nil {
		t.Fatal(err)
	}
	// SubjectPublicKeyInfo d'une clé RSA-2048 : 294 octets, 392 caractères en base64.
	if len(pub) != 392 || !strings.HasPrefix(pub, "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A") {
		t.Errorf("clé publique inattendue (%d caractères) : %.40s…", len(pub), pub)
	}
	if _, err := PublicKey("pas une clé"); err == nil {
		t.Error("un PEM illisible doit être refusé")
	}
}

func TestSansProjetSigneAucuneTableNEstPosee(t *testing.T) {
	// OpenDKIM peut servir à l'administrateur par ailleurs : des tables vides
	// posées d'office feraient recharger sa configuration à chaque apply.
	f := newFixture(t)
	quiet := project(t, "calme.fr", nil)
	steps, err := f.p.PlanTables([]*model.Project{quiet}, f.observe(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Errorf("aucune étape attendue, obtenu %v", steps)
	}
	if findings := f.p.Checks(context.Background(), []*model.Project{quiet}, f.observe(t)); len(findings) != 0 {
		t.Errorf("« doctor » n'a rien à dire d'un OpenDKIM que personne n'emploie par ogs : %v", findings)
	}

	// Une fois écrites, elles restent recalculées — jusqu'à vides : sans quoi le
	// dernier projet signé ne quitterait jamais les tables.
	shop := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, shop))
	tables(t, f, shop)
	kt, _ := tables(t, f, quiet)
	if strings.Contains(kt, "shop.fr") {
		t.Errorf("le dernier projet signé doit quitter les tables :\n%s", kt)
	}
}

func TestSansCleMaitreUnProjetNonSigneSePlanifie(t *testing.T) {
	// Un keystore absent ne doit pas faire échouer le plan d'un projet qui ne
	// demande rien : la clé maître n'est exigée que s'il y a une clé à ouvrir.
	f := newFixture(t)
	f.p.Keys = nil
	quiet := project(t, "calme.fr", nil)
	if steps := f.plan(t, quiet); len(steps) != 0 {
		t.Errorf("aucune étape attendue, obtenu %v", steps)
	}
	if _, err := f.p.PlanTables([]*model.Project{quiet}, f.observe(t)); err != nil {
		t.Errorf("les tables d'un parc sans signature ne doivent pas exiger le keystore : %v", err)
	}
}

func TestLesTablesSontAuRegimeDeHarden(t *testing.T) {
	// « ogs harden » ferme /etc/opendkim en 0750 et ses fichiers en 0640 : écrire
	// les tables en 0644, ou le répertoire en 0755, ferait rouvrir à chaque apply
	// ce que harden referme.
	f := newFixture(t)
	p := project(t, "shop.fr", &model.DKIM{Enabled: true})
	run(t, f.plan(t, p))
	tables(t, f, p)
	for path, want := range map[string]os.FileMode{
		f.paths.OpenDKIMKeyTable():              0o640,
		f.paths.OpenDKIMSigningTable():          0o640,
		f.paths.OpenDKIMTablesDir():             0o750,
		filepath.Dir(f.paths.OpenDKIMKeysDir()): 0o750,
		f.paths.OpenDKIMKeysDir():               0o750,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s : %04o attendu, obtenu %v (%v)", path, want, info.Mode().Perm(), err)
		}
	}
}
