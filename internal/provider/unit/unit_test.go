package unit

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/template"
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
		filepath.Join(".local", "bin", "php"),
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

func TestBuildPrepResteEnProd(t *testing.T) {
	// D96 : une préproduction tourne en APP_ENV=prod, comme son pool PHP.
	prov, _ := fixture(t)
	proj := model.New("shop.fr", naming.EnvPrep, model.KindSymfony)
	proj.Spec.App.Version = "6.4"
	if err := proj.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	svc := model.Service{Name: "messenger-async", Template: TemplateMessenger, Instances: 1}
	content, err := prov.Build(proj, svc)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(content); !strings.Contains(text, "Environment=APP_ENV=prod") || strings.Contains(text, "APP_ENV=dev") {
		t.Errorf("la préproduction doit tourner en APP_ENV=prod :\n%s", text)
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
	// Les deux instances, plus le détachement de la cible : un projet sans service
	// ne doit plus être réclamé au démarrage.
	if len(plan.Mutating()) != 3 {
		t.Errorf("deux instances et la cible doivent être désactivées :\n%s", plan.String())
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

func TestStartLimitDirectivesLiveInUnitSection(t *testing.T) {
	// StartLimitBurst et StartLimitIntervalSec appartiennent à [Unit] depuis
	// systemd 229. Placés dans [Service], systemd les ignore avec un simple
	// avertissement au chargement : la protection contre un service qui échoue en
	// boucle n'existe alors pas, sans que rien ne le signale.
	set := template.New(t.TempDir())
	for _, nom := range []string{"symfony-messenger", "worker", "rabbitmq-consumer", "app"} {
		t.Run(nom, func(t *testing.T) {
			src, err := set.Read("systemd/" + nom + ".service")
			if err != nil {
				t.Fatal(err)
			}
			contenu := string(src)
			if !strings.Contains(contenu, "StartLimitBurst") {
				t.Skip("ce template ne borne pas les redémarrages")
			}
			// L'en-tête est cherché en début de ligne : le mot figure aussi dans un
			// commentaire, plus haut.
			unit := contenu[:strings.Index(contenu, "\n[Service]")]
			for _, directive := range []string{"StartLimitBurst", "StartLimitIntervalSec"} {
				if !strings.Contains(unit, directive) {
					t.Errorf("%s doit figurer dans [Unit] :\n%s", directive, contenu)
				}
			}
		})
	}
}

func TestPlanOrdersUnitFilesBeforeInstances(t *testing.T) {
	// L'ordre suit le cycle de vie, non l'alphabet : « unit/instance » précède
	// « unit/service » par ordre lexical, de sorte qu'un tri par nom faisait
	// démarrer chaque instance avant l'écriture de son modèle — systemd répondait
	// « Unit not found » et aucun service ne démarrait au premier apply.
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "veille", Template: "worker", Instances: 2,
		Params: map[string]string{"command": "/usr/bin/true"},
	}}
	steps, err := prov.Plan(context.Background(), proj, Observed{Enabled: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	var ordre []string
	for _, s := range steps {
		ordre = append(ordre, s.Resource)
	}
	position := func(prefixe string) int {
		for i, r := range ordre {
			if strings.HasPrefix(r, prefixe) {
				return i
			}
		}
		return -1
	}
	cible, modele, instance := position("unit/target/"), position("unit/service/"), position("unit/instance/")
	if cible < 0 || modele < 0 || instance < 0 {
		t.Fatalf("étapes attendues absentes : %v", ordre)
	}
	if !(cible < modele && modele < instance) {
		t.Errorf("ordre incorrect (cible %d, modèle %d, instance %d) : %v",
			cible, modele, instance, ordre)
	}
}

func TestCleanupStopsInstancesBeforeRemovingFiles(t *testing.T) {
	// Dans l'autre sens : retirer le fichier d'unité avant d'arrêter ses instances
	// laisserait des processus sans unité pour les piloter.
	prov, proj := fixture(t)
	proj.Spec.Services = nil
	obs := Observed{Enabled: map[string]bool{
		InstanceName(proj.ID(), "veille", 1): true,
	}}
	steps := prov.cleanupSteps(proj, obs)
	var arret, detachement = -1, -1
	for i, s := range steps {
		if strings.HasPrefix(s.Resource, "unit/instance/") && arret < 0 {
			arret = i
		}
		if strings.HasPrefix(s.Resource, "unit/target-enable/") && detachement < 0 {
			detachement = i
		}
	}
	if arret < 0 {
		t.Fatalf("aucun arrêt d'instance planifié : %v", steps)
	}
	if detachement >= 0 && arret > detachement {
		t.Errorf("les instances doivent être arrêtées avant le détachement de la cible : %v", steps)
	}
}

func TestReadWritePathsAreOptional(t *testing.T) {
	// « var » est créé par l'application, pas par ogs : sans le tiret qui rend le
	// chemin facultatif, systemd refuse d'installer l'espace de noms et le service
	// échoue en 226/NAMESPACE — avant même d'exécuter la commande.
	// Le contrôle porte sur l'unité rendue : c'est elle que systemd lit.
	prov, proj := fixture(t)
	for _, svc := range []model.Service{
		{Name: "async", Template: "symfony-messenger", Params: map[string]string{"transport": "async"}},
		{Name: "veille", Template: "worker", Params: map[string]string{"command": "/usr/bin/true"}},
		{Name: "conso", Template: "rabbitmq-consumer", Params: map[string]string{"queue": "async"}},
	} {
		contenu, err := prov.Build(proj, svc)
		if err != nil {
			t.Fatalf("%s : %v", svc.Name, err)
		}
		for _, ligne := range strings.Split(string(contenu), "\n") {
			if !strings.HasPrefix(ligne, "ReadWritePaths=") {
				continue
			}
			for _, chemin := range strings.Fields(strings.TrimPrefix(ligne, "ReadWritePaths=")) {
				if !strings.HasPrefix(chemin, "-") {
					t.Errorf("%s : %q doit être facultatif (préfixe « - »)", svc.Name, chemin)
				}
			}
		}
	}
}

func TestApplicationInstancesAreEnabledAndStarted(t *testing.T) {
	// L'unité d'une application supervisée est écrite par le provider applicatif,
	// mais rien ne la lançait : l'attente de disponibilité échouait alors en
	// accusant l'application, et tout site non-PHP restait mort-né.
	prov, proj := fixture(t)
	proj.Spec.App.Kind = model.KindGo
	proj.Spec.App.Instances = 2
	proj.Spec.Services = nil

	steps, err := prov.Plan(context.Background(), proj, Observed{Enabled: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	var resumes []string
	for _, s := range steps {
		resumes = append(resumes, s.Resource)
	}
	for i := 1; i <= 2; i++ {
		attendu := "unit/instance/" + InstanceName(proj.ID(), AppServiceName, i)
		if !slices.Contains(resumes, attendu) {
			t.Errorf("l'instance %d de l'application doit être activée : %v", i, resumes)
		}
	}
	// La cible doit exister aussi : l'unité applicative la réclame par PartOf.
	if !slices.Contains(resumes, "unit/target/"+proj.ID()) {
		t.Errorf("la cible du projet doit être écrite : %v", resumes)
	}

	// Un projet PHP sans service ne produit rien : il n'a pas d'application
	// supervisée.
	php, projPHP := fixture(t)
	projPHP.Spec.Services = nil
	steps, err = php.Plan(context.Background(), projPHP, Observed{Enabled: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Mutates() {
			t.Errorf("aucune étape attendue pour un projet sans service ni application : %s", s)
		}
	}
}

func TestChangedUnitRequestsRestart(t *testing.T) {
	// Relire la définition ne relance rien : sans redémarrage, un changement
	// d'unité — commande, variable, limite — resterait écrit sans prendre effet.
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "veille", Template: "worker",
		Params: map[string]string{"command": "/usr/bin/true"},
	}}
	apply(t, prov, proj)

	// Le même projet, avec une commande différente.
	proj.Spec.Services[0].Params["command"] = "/usr/bin/sleep 60"
	steps, err := prov.Plan(context.Background(), proj, Observed{
		Enabled: map[string]bool{InstanceName(proj.ID(), "veille", 1): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var trouve bool
	for _, s := range steps {
		if s.Action != engine.Update {
			continue
		}
		if slices.Contains(s.Reload, RestartTargetIntent(proj.ID())) {
			trouve = true
		}
		if !slices.Contains(s.Reload, "systemd:daemon-reload") {
			t.Errorf("le daemon-reload doit rester demandé : %v", s.Reload)
		}
	}
	if !trouve {
		t.Errorf("une unité modifiée doit demander le redémarrage groupé :\n%v", steps)
	}

	// À la création, en revanche, c'est l'activation qui démarre : pas de
	// redémarrage à demander.
	neuf, projNeuf := fixture(t)
	projNeuf.Spec.Services = proj.Spec.Services
	steps, err = neuf.Plan(context.Background(), projNeuf, Observed{Enabled: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if slices.Contains(s.Reload, RestartTargetIntent(projNeuf.ID())) {
			t.Errorf("aucun redémarrage ne doit être demandé à la création : %s", s)
		}
	}
}

func TestUniteModifieeALaMainNEstPasEcraseeEnSilence(t *testing.T) {
	// Une unité éditée à la main porte une intention d'exploitation — une limite
	// mémoire posée après un incident. L'écraser la perdait sans un mot, et le
	// redémarrage groupé des instances suivait dans la foulée (§6.4).
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "veille", Template: "worker",
		Params: map[string]string{"command": "/usr/bin/true"},
	}}
	apply(t, prov, proj)

	path := filepath.Join(prov.Paths.SystemdDir(), ServiceName(proj.ID(), "veille"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("MemoryMax=512M\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	obs := Observed{Enabled: map[string]bool{InstanceName(proj.ID(), "veille", 1): true}}
	steps, err := prov.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	var refus *engine.Step
	for _, s := range steps {
		if strings.Contains(s.Summary, "modifié hors ogs") {
			refus = s
		}
	}
	if refus == nil {
		t.Fatalf("la modification manuelle doit être signalée :\n%v", steps)
	}
	if err := refus.Apply(ctx); err == nil {
		t.Error("l'écrasement doit être refusé sans --force")
	}
	// Rien n'a été écrit : rien ne doit être rechargé ni redémarré.
	if len(refus.Reload) != 0 {
		t.Errorf("le refus ne doit demander aucun rechargement : %v", refus.Reload)
	}
	for _, s := range steps {
		if slices.Contains(s.Reload, RestartTargetIntent(proj.ID())) {
			t.Errorf("aucun redémarrage des services ne doit être demandé : %s", s.Summary)
		}
	}
	if after, _ := os.ReadFile(path); !strings.Contains(string(after), "MemoryMax=512M") {
		t.Error("le refus ne doit rien écrire")
	}
}

func TestUniteEcraseeAvecForceApresSauvegarde(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "veille", Template: "worker",
		Params: map[string]string{"command": "/usr/bin/true"},
	}}
	apply(t, prov, proj)

	path := filepath.Join(prov.Paths.SystemdDir(), ServiceName(proj.ID(), "veille"))
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(data, []byte("MemoryMax=512M\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	prov.Force = true
	ctx := context.Background()
	obs := Observed{Enabled: map[string]bool{InstanceName(proj.ID(), "veille", 1): true}}
	steps, err := prov.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	var forcée *engine.Step
	for _, s := range steps {
		if strings.Contains(s.Summary, "écrasement forcé") {
			forcée = s
		}
	}
	if forcée == nil {
		t.Fatalf("le plan doit annoncer l'écrasement forcé :\n%v", steps)
	}
	// L'unité change réellement : le redémarrage groupé reste dû, sans quoi les
	// instances continueraient de tourner avec l'ancienne définition.
	if !slices.Contains(forcée.Reload, RestartTargetIntent(proj.ID())) {
		t.Errorf("l'écrasement forcé doit demander le redémarrage groupé : %v", forcée.Reload)
	}
	if err := forcée.Apply(ctx); err != nil {
		t.Fatalf("l'écrasement forcé doit réussir : %v", err)
	}
	if after, _ := os.ReadFile(path); strings.Contains(string(after), "MemoryMax=512M") {
		t.Error("l'unité doit avoir été réécrite")
	}
	// La copie sort de /etc/systemd/system : systemd n'y charge que les suffixes
	// d'unité qu'il connaît, mais l'importateur y énumère les unités du parc.
	if restes, _ := filepath.Glob(path + ".ogs-bak-*"); len(restes) != 0 {
		t.Errorf("aucune sauvegarde ne doit rester dans %s : %v", prov.Paths.SystemdDir(), restes)
	}
	baks, _ := filepath.Glob(filepath.Join(prov.Paths.BackupsDir(), filepath.Base(path)+".ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue dans %s, %d trouvée(s)", prov.Paths.BackupsDir(), len(baks))
	}
	if saved, _ := os.ReadFile(baks[0]); !strings.Contains(string(saved), "MemoryMax=512M") {
		t.Error("la sauvegarde doit contenir l'original")
	}
}

func TestPlanRemonteUneUniteIllisible(t *testing.T) {
	// Confondre « illisible » et « rien à faire » faisait annoncer un succès sans
	// que rien n'ait été vérifié ni écrit.
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{{
		Name: "veille", Template: "worker",
		Params: map[string]string{"command": "/usr/bin/true"},
	}}
	path := filepath.Join(prov.Paths.SystemdDir(), ServiceName(proj.ID(), "veille"))
	// Un répertoire à la place du fichier : fs.Inspect échoue à la lecture.
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, Observed{Enabled: map[string]bool{}})
	if err == nil {
		t.Fatalf("une unité illisible doit faire échouer le plan, %d étapes rendues", len(steps))
	}
}

// Q-66 : les journaux d'un service atteignent la sortie, sans pager, et seule
// une unité du projet peut être visée.
func TestLogsEcritLesJournauxDansLaSortie(t *testing.T) {
	prov, proj := fixture(t)
	rec := prov.Exec.(*exec.Recorder)
	rec.Responses["journalctl -u"] = exec.Result{Stdout: "messenger démarré\n"}
	var out, errOut strings.Builder
	if err := prov.Logs(context.Background(), proj, "async", false, 20, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.String() != "messenger démarré\n" {
		t.Errorf("les journaux doivent atteindre la sortie : %q", out.String())
	}
	calls := rec.Calls()
	if len(calls) != 1 || !strings.Contains(calls[0].String(), "journalctl -u ogs-shopfr-async@* -n 20 --no-pager") {
		t.Errorf("commande : %v", calls)
	}
	for _, nom := range []string{"*", "a*", "x@y", "a?", "[ab]", "", "-f"} {
		if err := prov.Logs(context.Background(), proj, nom, false, 20, &out, &errOut); err == nil {
			t.Errorf("le nom de service %q ne doit pas être accepté", nom)
		}
	}
}
