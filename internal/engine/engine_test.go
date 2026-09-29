package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tracker enregistre l'ordre des applications et des compensations.
type tracker struct{ events []string }

func (tr *tracker) step(name string, action Action, critical bool, fail bool) *Step {
	s := NewStep("test/"+name, action, name)
	s.Critical = critical
	s.Apply = func(context.Context) error {
		if fail {
			return fmt.Errorf("échec simulé de %s", name)
		}
		tr.events = append(tr.events, "apply:"+name)
		return nil
	}
	s.Compensate = func(context.Context) error {
		tr.events = append(tr.events, "compensate:"+name)
		return nil
	}
	return s
}

func TestExecuteAppliesInOrder(t *testing.T) {
	tr := &tracker{}
	plan := &Plan{Project: "shopfr"}
	plan.Add(tr.step("user", Create, true, false))
	plan.Add(tr.step("dirs", Create, true, false))
	report, err := Execute(context.Background(), plan, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied() != 2 || report.RolledBack {
		t.Fatalf("rapport inattendu : %+v", report)
	}
	if got := fmt.Sprint(tr.events); got != "[apply:user apply:dirs]" {
		t.Errorf("ordre d'application : %s", got)
	}
}

func TestExecuteRollsBackInReverseOrder(t *testing.T) {
	tr := &tracker{}
	plan := &Plan{Project: "shopfr"}
	plan.Add(tr.step("user", Create, true, false))
	plan.Add(tr.step("dirs", Create, true, false))
	plan.Add(tr.step("database", Create, true, true)) // échec critique
	plan.Add(tr.step("vhost", Create, true, false))   // ne doit pas être atteinte

	report, err := Execute(context.Background(), plan, Options{})
	if err == nil {
		t.Fatal("un échec critique doit remonter une erreur")
	}
	if !report.RolledBack {
		t.Error("le rollback doit être signalé")
	}
	if report.RollbackErr != nil {
		t.Errorf("compensation en échec : %v", report.RollbackErr)
	}
	want := "[apply:user apply:dirs compensate:dirs compensate:user]"
	if got := fmt.Sprint(tr.events); got != want {
		t.Errorf("séquence :\n  obtenue %s\n  attendue %s", got, want)
	}
	if f := report.Failed(); f == nil || f.Step.Resource != "test/database" {
		t.Errorf("étape en échec mal identifiée : %+v", f)
	}
}

func TestExecuteContinuesOnNonCriticalFailure(t *testing.T) {
	tr := &tracker{}
	plan := &Plan{Project: "shopfr"}
	plan.Add(tr.step("user", Create, true, false))
	plan.Add(tr.step("certificat", Create, false, true)) // non critique, échoue
	plan.Add(tr.step("cron", Create, true, false))

	var warned []string
	report, err := Execute(context.Background(), plan, Options{
		OnWarning: func(s *Step, err error) { warned = append(warned, s.Resource) },
	})
	if err != nil {
		t.Fatalf("une étape non critique ne doit pas faire échouer l'apply : %v", err)
	}
	if report.RolledBack {
		t.Error("aucun rollback ne doit avoir lieu")
	}
	if len(report.Warnings) != 1 || len(warned) != 1 || warned[0] != "test/certificat" {
		t.Errorf("avertissements : %v / %v", report.Warnings, warned)
	}
	if got := fmt.Sprint(tr.events); got != "[apply:user apply:cron]" {
		t.Errorf("les étapes suivantes doivent être appliquées : %s", got)
	}
	if report.Failed() != nil {
		t.Error("un échec non critique ne doit pas être rapporté comme échec")
	}
}

func TestExecuteStopsRollbackOnCompensationFailure(t *testing.T) {
	tr := &tracker{}
	plan := &Plan{Project: "shopfr"}
	first := tr.step("user", Create, true, false)
	second := tr.step("dirs", Create, true, false)
	second.Compensate = func(context.Context) error { return errors.New("compensation impossible") }
	plan.Add(first)
	plan.Add(second)
	plan.Add(tr.step("database", Create, true, true))

	report, err := Execute(context.Background(), plan, Options{})
	if err == nil {
		t.Fatal("erreur attendue")
	}
	if report.RollbackErr == nil {
		t.Fatal("l'échec de compensation doit être remonté")
	}
	// La compensation de « user » n'est pas tentée : on s'arrête et on rapporte.
	if got := fmt.Sprint(tr.events); got != "[apply:user apply:dirs]" {
		t.Errorf("le déroulage doit s'arrêter à la première compensation en échec : %s", got)
	}
}

func TestExecuteDryRunAppliesNothing(t *testing.T) {
	tr := &tracker{}
	plan := &Plan{Project: "shopfr"}
	plan.Add(tr.step("user", Create, true, false))
	report, err := Execute(context.Background(), plan, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.events) != 0 {
		t.Errorf("aucune action ne doit être exécutée : %v", tr.events)
	}
	if len(report.Outcomes) != 1 || !report.Outcomes[0].Skipped {
		t.Errorf("les étapes doivent être marquées non exécutées : %+v", report.Outcomes)
	}
}

func TestPlanSkipsNoOpAndDeduplicatesReloads(t *testing.T) {
	plan := &Plan{Project: "shopfr"}
	a := NewStep("web/vhost/shopfr", Update, "vhost shopfr")
	a.Reload = []string{"apache2", "php8.3-fpm"}
	b := NewStep("web/vhost/autre", Create, "vhost autre")
	b.Reload = []string{"apache2"}
	c := NewStep("system/user/shopfr", NoOp, "utilisateur shopfr")
	c.Reload = []string{"ne-doit-pas-apparaitre"}
	plan.Add(a)
	plan.Add(b)
	plan.Add(c)

	if len(plan.Mutating()) != 2 {
		t.Errorf("%d étapes mutantes, 2 attendues", len(plan.Mutating()))
	}
	if plan.Empty() {
		t.Error("le plan n'est pas vide")
	}
	got := fmt.Sprint(plan.Reloads())
	if got != "[apache2 php8.3-fpm]" {
		t.Errorf("rechargements : %s", got)
	}
}

func TestPlanEmptyWhenAllNoOp(t *testing.T) {
	plan := &Plan{Project: "shopfr"}
	plan.Add(NewStep("system/user/shopfr", NoOp, "utilisateur"))
	if !plan.Empty() {
		t.Error("un plan entièrement inchangé doit être vide")
	}
	if plan.String() != "  aucun changement\n" {
		t.Errorf("rendu : %q", plan.String())
	}
}

func TestExecuteReloadsOnce(t *testing.T) {
	plan := &Plan{Project: "shopfr"}
	for _, n := range []string{"a", "b"} {
		s := NewStep("web/vhost/"+n, Create, "vhost "+n)
		s.Apply = func(context.Context) error { return nil }
		s.Reload = []string{"apache2"}
		plan.Add(s)
	}
	var reloads []string
	report, err := Execute(context.Background(), plan, Options{
		Reload: func(_ context.Context, target string) error {
			reloads = append(reloads, target)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reloads) != 1 || reloads[0] != "apache2" {
		t.Errorf("un seul rechargement attendu, obtenu %v", reloads)
	}
	if len(report.Reloaded) != 1 {
		t.Errorf("rapport de rechargement : %v", report.Reloaded)
	}
}

func TestUneEtapePartielleRetientSonRechargementSansEtreCompensee(t *testing.T) {
	// Un certificat émis pour une partie de ses noms remplace le matériel que le
	// serveur garde en mémoire : l'avertissement ne doit pas coûter le
	// rechargement, faute de quoi l'ancien certificat resterait présenté jusqu'à
	// son expiration. Il ne doit pas non plus être défait par une étape critique
	// en échec plus loin : d'autres étapes ont pu le désigner entre-temps.
	tr := &tracker{}
	plan := &Plan{Project: "shopfr"}
	cert := tr.step("certificat", Create, false, false)
	cert.Apply = func(context.Context) error {
		return Partial(errors.New("émis sans ancien-shop.fr"))
	}
	cert.Reload = []string{"web:apache"}
	plan.Add(cert)
	plan.Add(tr.step("vhost", Update, false, true))
	plan.Add(tr.step("cron", Create, true, true))

	var reloads []string
	report, err := Execute(context.Background(), plan, Options{
		Reload: func(_ context.Context, target string) error {
			reloads = append(reloads, target)
			return nil
		},
	})
	if err == nil {
		t.Fatal("l'échec critique doit remonter")
	}
	if len(report.Warnings) != 2 || !strings.Contains(report.Warnings[0], "sans ancien-shop.fr") {
		t.Errorf("l'aboutissement partiel est un avertissement : %v", report.Warnings)
	}
	if fmt.Sprint(tr.events) != "[]" {
		t.Errorf("l'étape partielle ne doit pas être compensée : %v", tr.events)
	}

	// Sans échec critique, le rechargement suit, une fois.
	plan = &Plan{Project: "shopfr"}
	plan.Add(cert)
	plan.Add(tr.step("vhost", Update, false, true))
	reloads = nil
	if _, err := Execute(context.Background(), plan, Options{
		Reload: func(_ context.Context, target string) error {
			reloads = append(reloads, target)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(reloads) != "[web:apache]" {
		t.Errorf("l'étape partielle doit porter son rechargement : %v", reloads)
	}
}

func TestLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ogs.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	// Dans le même processus, flock est réentrant : on vérifie donc surtout que
	// le verrou s'acquiert, se libère et se réacquiert proprement.
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("le verrou libéré doit être réacquérable : %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestFlushReloadsRunsPendingReloadsBeforeTheStep(t *testing.T) {
	// Le challenge HTTP-01 a besoin que le serveur web ait relu son vhost : un
	// fichier écrit ne suffit pas. Une étape peut donc exiger que les
	// rechargements en attente soient exécutés avant elle — et ils ne doivent pas
	// l'être deux fois.
	var journal []string

	plan := &Plan{Project: "shopfr"}
	ecriture := NewStep("web/vhost/shopfr", Create, "vhost")
	ecriture.Apply = func(context.Context) error { journal = append(journal, "écriture"); return nil }
	ecriture.Reload = []string{"web:apache"}
	plan.Add(ecriture)

	emission := NewStep("tls/certificate/shopfr", Create, "certificat")
	emission.FlushReloads = true
	emission.Apply = func(context.Context) error { journal = append(journal, "émission"); return nil }
	plan.Add(emission)

	report, err := Execute(context.Background(), plan, Options{
		Reload: func(_ context.Context, target string) error {
			journal = append(journal, "rechargement "+target)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	attendu := "écriture, rechargement web:apache, émission"
	if got := strings.Join(journal, ", "); got != attendu {
		t.Errorf("ordre d'exécution = %q, attendu %q", got, attendu)
	}
	if len(report.Reloaded) != 1 {
		t.Errorf("le rechargement doit être exécuté une seule fois : %v", report.Reloaded)
	}
}

func TestReloadIntentDeclaredAfterAFlushIsStillHonoured(t *testing.T) {
	// Une étape qui vide la file ne doit pas consommer l'intention d'une étape qui
	// n'a pas encore eu lieu. Le cas réel : le démarrage d'une instance systemd
	// exige un rechargement préalable, puis crée un lien d'activation qui en exige
	// un second — sans quoi la cible ne réclame pas l'instance au démarrage.
	var journal []string
	plan := &Plan{Project: "shopfr"}

	unite := NewStep("unit/service/veille", Create, "unité")
	unite.Apply = func(context.Context) error { journal = append(journal, "écriture"); return nil }
	unite.Reload = []string{"systemd:daemon-reload"}
	plan.Add(unite)

	instance := NewStep("unit/instance/veille@1", Create, "instance")
	instance.FlushReloads = true
	instance.Apply = func(context.Context) error { journal = append(journal, "démarrage"); return nil }
	instance.Reload = []string{"systemd:daemon-reload"}
	plan.Add(instance)

	report, err := Execute(context.Background(), plan, Options{
		Reload: func(_ context.Context, target string) error {
			journal = append(journal, "rechargement")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	attendu := "écriture, rechargement, démarrage, rechargement"
	if got := strings.Join(journal, ", "); got != attendu {
		t.Errorf("séquence = %q, attendu %q", got, attendu)
	}
	if len(report.Reloaded) != 2 {
		t.Errorf("deux rechargements attendus : %v", report.Reloaded)
	}
}

// Une annulation survenue *pendant* une étape doit compenser tout autant qu'une
// annulation vue entre deux : Ctrl-C tombe rarement dans l'intervalle. Le
// contexte étant alors déjà mort, une compensation qui lance une commande
// (groupdel, userdel, suppression d'une base) échouerait sans avoir rien fait.
func TestExecuteCompenseMalgreUneAnnulationPendantUneEtape(t *testing.T) {
	ctx, annuler := context.WithCancel(context.Background())
	defer annuler()

	var vivant, compense bool
	premiere := NewStep("test/user", Create, "user")
	premiere.Apply = func(context.Context) error { return nil }
	premiere.Compensate = func(ctx context.Context) error {
		compense = true
		vivant = ctx.Err() == nil
		return ctx.Err()
	}
	seconde := NewStep("test/database", Create, "database")
	seconde.Apply = func(ctx context.Context) error {
		annuler()
		return ctx.Err()
	}

	plan := &Plan{Project: "shopfr", Steps: []*Step{premiere, seconde}}
	report, err := Execute(ctx, plan, Options{})
	if err == nil {
		t.Fatal("une étape critique annulée doit remonter une erreur")
	}
	if !compense {
		t.Fatal("la compensation de la première étape n'a pas eu lieu")
	}
	if !vivant {
		t.Error("la compensation a reçu un contexte déjà annulé")
	}
	if !report.RolledBack {
		t.Error("le rapport n'annonce pas de déroulage")
	}
	if report.RollbackErr != nil {
		t.Errorf("déroulage en échec : %v", report.RollbackErr)
	}
}

// Chaque compensation reçoit son propre délai. Une borne unique pour tout le
// déroulage expirerait en cours de route sur une saga longue : les compensations
// lentes — l'arrêt des instances systemd, qui consomme jusqu'à TimeoutStopSec par
// instance — appartiennent à des étapes appliquées tard, donc compensées tôt, et
// mangeraient le budget de groupdel, userdel et de la suppression des bases,
// dernières compensées. Compte, groupe et base resteraient en place alors que le
// rapport annonce un déroulage.
func TestChaqueCompensationRecoitSonProprePlafondDeTemps(t *testing.T) {
	échéances := map[string]time.Time{}
	compenser := func(nom string) func(context.Context) error {
		return func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				t.Errorf("compensation de %s sur un contexte mort : %v", nom, err)
				return err
			}
			échéance, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("compensation de %s : aucune échéance, la borne a disparu", nom)
			}
			échéances[nom] = échéance
			// Sans quoi les deux échéances pourraient tomber sur la même valeur
			// d'horloge et le test ne distinguerait plus les deux budgets.
			time.Sleep(2 * time.Millisecond)
			return nil
		}
	}

	groupe := NewStep("system/group", Create, "group")
	groupe.Apply = func(context.Context) error { return nil }
	groupe.Compensate = compenser("groupe")
	instance := NewStep("unit/instance", Create, "instance")
	instance.Apply = func(context.Context) error { return nil }
	instance.Compensate = compenser("instance")
	sudoers := NewStep("sudoers/file", Create, "sudoers")
	sudoers.Apply = func(context.Context) error { return errors.New("visudo refuse la règle") }

	plan := &Plan{Project: "shopfr", Steps: []*Step{groupe, instance, sudoers}}
	report, err := Execute(context.Background(), plan, Options{})
	if err == nil {
		t.Fatal("une étape critique en échec doit remonter une erreur")
	}
	if report.RollbackErr != nil {
		t.Fatalf("déroulage en échec : %v", report.RollbackErr)
	}
	if len(échéances) != 2 {
		t.Fatalf("%d compensation(s) déroulée(s), deux attendues", len(échéances))
	}
	// L'instance est compensée la première : son échéance doit être antérieure à
	// celle du groupe, faute de quoi les deux partagent le même budget.
	if !échéances["groupe"].After(échéances["instance"]) {
		t.Errorf("les deux compensations partagent une seule échéance (%v)", échéances)
	}
	// Et ce budget est bien celui d'une compensation, non le reliquat de celui du
	// déroulage entier.
	if reste := time.Until(échéances["groupe"]); reste > délaiCompensation {
		t.Errorf("la dernière compensation dispose de %v, %v au plus attendus",
			reste, délaiCompensation)
	}
}
