package unit

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

// R1-48 : « active » voulait dire « lien présent ». Une instance morte, ou dont
// le démarrage avait échoué, n'était jamais relancée (D121).

func planDe(t *testing.T, prov *Provider, proj *model.Project) []*engine.Step {
	t.Helper()
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

func étape(steps []*engine.Step, resource string) *engine.Step {
	for _, s := range steps {
		if s.Resource == resource {
			return s
		}
	}
	return nil
}

func projetVeille(t *testing.T) (*Provider, *model.Project) {
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "veille", Template: TemplateWorker, Instances: 2,
		Params: map[string]string{"command": "/bin/true"},
	}}
	apply(t, prov, proj)
	return prov, proj
}

func TestUneInstanceEnÉchecEstRelancée(t *testing.T) {
	prov, proj := projetVeille(t)
	sd := &systemdFactice{unités: "ogs-shopfr.target loaded active active cible\n" +
		"● ogs-shopfr-veille@1.service loaded failed failed veille\n" +
		"ogs-shopfr-veille@2.service loaded active running veille\n"}
	prov.Exec = sd
	steps := planDe(t, prov, proj)
	s := étape(steps, "unit/instance/ogs-shopfr-veille@1.service")
	if s == nil || s.Action == engine.NoOp {
		t.Fatalf("une instance en échec doit être relancée : %v", s)
	}
	if s.Critical {
		t.Error("la relance ne doit pas être critique : un worker qui retombe ne défait pas le site")
	}
	if s2 := étape(steps, "unit/instance/ogs-shopfr-veille@2.service"); s2 == nil || s2.Action != engine.NoOp {
		t.Errorf("l'instance qui tourne est conforme : %v", s2)
	}
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sd.appel("reset-failed"), "ogs-shopfr-veille@1.service") {
		t.Errorf("le compteur d'échecs doit être acquitté avant la relance : %v", sd.appels)
	}
	if !slices.Contains(sd.appel("start"), "ogs-shopfr-veille@1.service") {
		t.Errorf("l'instance doit être démarrée : %v", sd.appels)
	}
}

// Un service arrêté par l'exploitant est « inactive » : l'apply le laisse
// arrêté (D114).
func TestUneInstanceArrêtéeParLExploitantResteArrêtée(t *testing.T) {
	prov, proj := projetVeille(t)
	prov.Exec = &systemdFactice{unités: "ogs-shopfr.target loaded inactive dead cible\n" +
		"ogs-shopfr-veille@1.service loaded inactive dead veille\n" +
		"ogs-shopfr-veille@2.service loaded inactive dead veille\n"}
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range planDe(t, prov, proj) {
		plan.Add(s)
	}
	if !plan.Empty() {
		t.Errorf("un service arrêté volontairement ne doit pas être relancé :\n%s", plan.String())
	}
}

// systemd injoignable : la lecture en échec n'est pas un échec des services.
func TestSystemdMuetNeFaitRienRelancer(t *testing.T) {
	prov, proj := projetVeille(t)
	prov.Exec = &systemdEnPanne{}
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range planDe(t, prov, proj) {
		plan.Add(s)
	}
	if !plan.Empty() {
		t.Errorf("plan non vide sur une lecture en échec :\n%s", plan.String())
	}
}

type systemdEnPanne struct{ systemdFactice }

func (s *systemdEnPanne) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	return exec.Result{}, os.ErrPermission
}

// Le lien posé avant le démarrage, une interruption entre les deux laissait une
// instance activée que rien ne lançait, tenue pour conforme à jamais.
func TestUnDémarrageInterrompuNActiveRien(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "veille", Template: TemplateWorker, Instances: 1,
		Params: map[string]string{"command": "/bin/true"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	prov.Exec = &systemdInterrompu{cancel: cancel}
	s := prov.enableStep(proj, "ogs-shopfr-veille@1.service")
	if err := s.Apply(ctx); err == nil {
		t.Fatal("un démarrage interrompu doit échouer")
	}
	link := filepath.Join(prov.wantsDir("shopfr"), "ogs-shopfr-veille@1.service")
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("l'instance ne doit pas être activée tant qu'elle n'a pas démarré : %v", err)
	}
}

type systemdInterrompu struct {
	systemdFactice
	cancel context.CancelFunc
}

func (s *systemdInterrompu) Run(ctx context.Context, c exec.Command) (exec.Result, error) {
	if c.Name == "systemctl" && len(c.Args) > 0 && c.Args[0] == "start" {
		s.cancel()
		return exec.Result{}, ctx.Err()
	}
	return s.systemdFactice.Run(ctx, c)
}

// Activer une instance neuve démarrait la cible ordinairement, qui tirait ses
// Wants= : un worker arrêté par l'exploitant repartait au passage.
func TestActiverUneInstanceNeRelancePasSesSœurs(t *testing.T) {
	prov, proj := projetVeille(t)
	sd := &systemdFactice{}
	prov.Exec = sd
	proj.Spec.Services[0].Instances = 3
	apply(t, prov, proj)
	for _, a := range sd.appels {
		if len(a) > 1 && a[0] == "systemctl" && a[1] == "start" && slices.Contains(a, TargetName("shopfr")) &&
			!slices.Contains(a, "--job-mode=ignore-requirements") {
			t.Errorf("la cible doit démarrer sans tirer ses Wants= : %v", a)
		}
	}
	if !slices.Contains(sd.appel("start"), "ogs-shopfr-veille@3.service") {
		t.Errorf("l'instance neuve doit démarrer : %v", sd.appels)
	}
}

// R1-33, transition : un service « app » d'avant D121 a laissé le modèle marqué
// « unit/app » sous le nom de l'unité applicative. Le remède — « service remove
// … app » puis apply — ne doit pas retirer l'unité de l'application supervisée.
func TestLeRetraitDuServiceAppÉpargneLUnitéApplicative(t *testing.T) {
	prov, _ := fixture(t)
	proj := model.New("app.fr", naming.EnvProd, model.KindNode)
	proj.Spec.App.Runtime = "22.11.0"
	proj.Spec.App.Entrypoint = "server.js"
	if err := proj.Normalize(filepath.Join(filepath.Dir(prov.Paths.SystemdDir()), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	svc := model.Service{Name: "app", Template: TemplateWorker, Params: map[string]string{"command": "/bin/true"}}
	content, err := prov.Build(proj, svc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(prov.Paths.SystemdDir(), ServiceName(proj.ID(), "app"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, s := range planDe(t, prov, proj) {
		if s.Action == engine.Delete && strings.Contains(s.Summary, ServiceName(proj.ID(), "app")) {
			t.Errorf("l'unité de l'application supervisée ne doit pas être retirée : %s", s.Summary)
		}
	}
}

// Un worker qui rend 1 sur SIGTERM sort de « service stop » en échec : l'arrêt
// voulu est acquitté, sans quoi l'apply le relançait (D121).
func TestLArrêtAcquitteLÉchecDUnWorkerQuiSortEnErreur(t *testing.T) {
	prov, _ := fixture(t)
	sd := &systemdFactice{unités: "ogs-shopfr-veille@1.service loaded active running veille\n"}
	prov.Exec = sd
	if _, err := prov.Stop(context.Background(), "shopfr"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sd.appel("reset-failed"), "ogs-shopfr-veille@1.service") {
		t.Errorf("l'arrêt doit acquitter l'échec éventuel des unités arrêtées : %v", sd.appels)
	}
	sd.appels = nil
	if err := prov.ControlID(context.Background(), "shopfr", "start"); err != nil {
		t.Fatal(err)
	}
	if len(sd.appels) == 0 || sd.appels[0][1] != "reset-failed" {
		t.Errorf("« start » acquitte les échecs avant de démarrer : %v", sd.appels)
	}
}

// Désactiver une instance acquitte son échec éventuel : un worker sorti en
// erreur sur SIGTERM restait « not-found failed » après le retrait de son modèle.
func TestLaDésactivationAcquitteLÉchec(t *testing.T) {
	prov, proj := projetVeille(t)
	sd := &systemdFactice{}
	prov.Exec = sd
	proj.Spec.Services[0].Instances = 1
	apply(t, prov, proj)
	if !slices.Contains(sd.appel("reset-failed"), "ogs-shopfr-veille@2.service") {
		t.Errorf("l'instance désactivée doit être acquittée : %v", sd.appels)
	}
}
