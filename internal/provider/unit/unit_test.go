package unit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
	"github.com/leviathan/ogs/internal/template"
)

func fixture(t *testing.T) (*Provider, *model.Project) {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	proj := model.New("shop.fr", naming.EnvProd, model.KindSymfony)
	proj.Spec.App.Version = "6.4"
	if err := proj.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	prov := &Provider{
		Paths: paths, Exec: exec.NewRecorder(),
		Templates: template.New(paths.TemplatesDir()),
		Now:       func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) },
	}
	return prov, proj
}

func apply(t *testing.T, prov *Provider, proj *model.Project) *engine.Plan {
	t.Helper()
	ctx := context.Background()
	obs, err := prov.Observe(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatalf("application : %v", err)
	}
	return plan
}

func TestBuildMessengerUnit(t *testing.T) {
	prov, proj := fixture(t)
	svc := model.Service{Name: "messenger-async", Template: TemplateMessenger,
		Instances: 3, Params: map[string]string{"transport": "async", "timeLimit": "3600"}}
	content, err := prov.Build(proj, svc)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, want := range []string{
		"User=shopfr",
		"Group=shopfr",
		"messenger:consume async",
		"--time-limit=3600",
		"Environment=APP_ENV=prod",
		// L'interpréteur passe par le lien : une bascule de version ne réécrit
		// aucune unité.
		filepath.Join(".bin", "php"),
		// Durcissement.
		"NoNewPrivileges=true",
		"ProtectSystem=full",
		"ProtectHome=read-only",
		// Un socket créé par le service doit rester joignable par www-data.
		"UMask=0007",
		// Messenger termine le message en cours sur SIGTERM.
		"KillSignal=SIGTERM",
		"PartOf=ogs-shopfr.target",
		"WantedBy=ogs-shopfr.target",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("unité incomplète, %q absent :\n%s", want, text)
		}
	}
	if strings.Contains(text, "/usr/bin/php8") {
		t.Error("aucune version de PHP ne doit apparaître en dur")
	}
	// Le broker étant absent de la déclaration, aucune dépendance ne doit être posée.
	if strings.Contains(text, "rabbitmq-server.service") {
		t.Error("la dépendance au broker ne doit apparaître que si un transport est déclaré")
	}
}

func TestBuildAddsBrokerDependencyWhenDeclared(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.AMQP = &model.AMQP{Vhost: "shopfr", User: "shopfr"}
	content, err := prov.Build(proj, model.Service{Name: "messenger-async", Template: TemplateMessenger})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "After=network.target rabbitmq-server.service") {
		t.Errorf("la dépendance au broker doit être posée :\n%s", content)
	}
}

func TestBuildRefusesMessengerOnSymfony34(t *testing.T) {
	// Le composant n'existe qu'à partir de la 4.1 : l'unité échouerait au
	// démarrage, et le diagnostic serait obscur.
	prov, proj := fixture(t)
	proj.Spec.App.Version = "3.4"
	_, err := prov.Build(proj, model.Service{Name: "messenger-async", Template: TemplateMessenger})
	if err == nil {
		t.Fatal("erreur attendue")
	}
	if !strings.Contains(err.Error(), TemplateConsumer) {
		t.Errorf("le message doit indiquer le template de remplacement : %v", err)
	}
	// Le template de remplacement, lui, doit fonctionner.
	content, err := prov.Build(proj, model.Service{Name: "consumer", Template: TemplateConsumer,
		Params: map[string]string{"transport": "async"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "rabbitmq:consumer async") {
		t.Errorf("consumer mal rendu :\n%s", content)
	}
	if !strings.Contains(string(content), "Environment=SYMFONY_ENV=prod") {
		t.Error("Symfony 3.4 emploie SYMFONY_ENV")
	}
}

func TestBuildWorkerRequiresCommand(t *testing.T) {
	prov, proj := fixture(t)
	if _, err := prov.Build(proj, model.Service{Name: "w", Template: TemplateWorker}); err == nil {
		t.Error("le template worker exige une commande")
	}
	content, err := prov.Build(proj, model.Service{Name: "w", Template: TemplateWorker,
		Params: map[string]string{"command": "/usr/bin/node worker.js"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "ExecStart=/usr/bin/node worker.js") {
		t.Errorf("commande mal rendue :\n%s", content)
	}
}

func TestBuildRejectsUnknownTemplate(t *testing.T) {
	prov, proj := fixture(t)
	_, err := prov.Build(proj, model.Service{Name: "x", Template: "supervisor"})
	if err == nil || !strings.Contains(err.Error(), "inconnu") {
		t.Errorf("template inconnu : %v", err)
	}
}

func TestApplyCreatesTargetUnitAndInstances(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "messenger-async", Template: TemplateMessenger, Instances: 3,
		Params: map[string]string{"transport": "async"},
	}}
	apply(t, prov, proj)

	for _, name := range []string{TargetName("shopfr"), ServiceName("shopfr", "messenger-async")} {
		if _, err := os.Stat(filepath.Join(prov.Paths.SystemdDir(), name)); err != nil {
			t.Errorf("%s absent : %v", name, err)
		}
	}
	// Trois instances activées, matérialisées par des liens dans .wants — ce que
	// fait « systemctl enable ».
	for i := 1; i <= 3; i++ {
		link := filepath.Join(prov.wantsDir("shopfr"), InstanceName("shopfr", "messenger-async", i))
		if _, err := os.Lstat(link); err != nil {
			t.Errorf("instance %d non activée : %v", i, err)
		}
	}
	// Idempotence.
	obs, _ := prov.Observe(context.Background(), proj)
	if obs.Instances["messenger-async"] != 3 {
		t.Errorf("instances observées : %d", obs.Instances["messenger-async"])
	}
	steps, _ := prov.Plan(context.Background(), proj, obs)
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if !plan.Empty() {
		t.Errorf("second plan non vide :\n%s", plan.String())
	}
}

func TestReducingInstancesDisablesSurplus(t *testing.T) {
	// Réduire l'échelle ne doit pas laisser de worker orphelin.
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "messenger-async", Template: TemplateMessenger, Instances: 3,
		Params: map[string]string{"transport": "async"},
	}}
	apply(t, prov, proj)
	proj.Spec.Services[0].Instances = 1
	plan := apply(t, prov, proj)
	if len(plan.Mutating()) != 2 {
		t.Errorf("deux désactivations attendues :\n%s", plan.String())
	}
	for i := 2; i <= 3; i++ {
		link := filepath.Join(prov.wantsDir("shopfr"), InstanceName("shopfr", "messenger-async", i))
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Errorf("l'instance %d devrait être désactivée : %v", i, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(prov.wantsDir("shopfr"),
		InstanceName("shopfr", "messenger-async", 1))); err != nil {
		t.Errorf("la première instance doit rester active : %v", err)
	}
}

func TestDisabledServiceStopsAllInstances(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "messenger-async", Template: TemplateMessenger, Instances: 2,
		Params: map[string]string{"transport": "async"},
	}}
	apply(t, prov, proj)
	disabled := false
	proj.Spec.Services[0].Enabled = &disabled
	apply(t, prov, proj)
	entries, err := os.ReadDir(prov.wantsDir("shopfr"))
	if err == nil && len(entries) != 0 {
		t.Errorf("aucune instance ne doit rester active : %v", entries)
	}
}

func TestScale(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "messenger-async", Template: TemplateMessenger, Instances: 1,
		Params: map[string]string{"transport": "async"},
	}}
	apply(t, prov, proj)
	ctx := context.Background()
	changed, err := prov.Scale(ctx, proj, "messenger-async", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 3 {
		t.Errorf("trois ajouts attendus : %v", changed)
	}
	obs, _ := prov.Observe(ctx, proj)
	if obs.Instances["messenger-async"] != 4 {
		t.Errorf("instances : %d", obs.Instances["messenger-async"])
	}
	// Puis réduction.
	changed, err = prov.Scale(ctx, proj, "messenger-async", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 || !strings.HasPrefix(changed[0], "-") {
		t.Errorf("deux retraits attendus : %v", changed)
	}
	obs, _ = prov.Observe(ctx, proj)
	if obs.Instances["messenger-async"] != 2 {
		t.Errorf("instances après réduction : %d", obs.Instances["messenger-async"])
	}
}

func TestInstanceStepsAreNonCritical(t *testing.T) {
	// Un service qui refuse de démarrer ne doit pas annuler le déploiement.
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "messenger-async", Template: TemplateMessenger, Instances: 1,
		Params: map[string]string{"transport": "async"},
	}}
	obs, _ := prov.Observe(context.Background(), proj)
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if strings.HasPrefix(s.Resource, "unit/instance/") && s.Critical {
			t.Errorf("l'étape %s doit être non critique", s.Resource)
		}
	}
}

func TestRemovingServicesCleansUp(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "messenger-async", Template: TemplateMessenger, Instances: 2,
		Params: map[string]string{"transport": "async"},
	}}
	apply(t, prov, proj)
	proj.Spec.Services = nil
	plan := apply(t, prov, proj)
	if len(plan.Mutating()) != 2 {
		t.Errorf("les deux instances doivent être désactivées :\n%s", plan.String())
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	// L'itération sur une table produirait un ordre variable, donc un plan
	// différent à chaque appel.
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{
		{Name: "a", Template: TemplateWorker, Instances: 2, Params: map[string]string{"command": "/bin/true"}},
		{Name: "b", Template: TemplateWorker, Instances: 2, Params: map[string]string{"command": "/bin/true"}},
	}
	ctx := context.Background()
	obs, _ := prov.Observe(ctx, proj)
	first, err := prov.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		next, err := prov.Plan(ctx, proj, obs)
		if err != nil {
			t.Fatal(err)
		}
		for j := range first {
			if first[j].Resource != next[j].Resource {
				t.Fatalf("ordre instable en position %d : %s puis %s",
					j, first[j].Resource, next[j].Resource)
			}
		}
	}
}

func TestTemplateOf(t *testing.T) {
	if got := templateOf("ogs-shopfr-messenger-async@3.service"); got != "ogs-shopfr-messenger-async@.service" {
		t.Errorf("templateOf : %q", got)
	}
}
