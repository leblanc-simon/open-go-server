package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/template"
)

func fixture(t *testing.T, kind model.AppKind) (*Provider, *model.Project) {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	st, err := store.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	proj := model.New("shop.fr", naming.EnvProd, kind)
	proj.Spec.App.Runtime = "22.11.0"
	if err := proj.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	rec := exec.NewRecorder()
	prov := &Provider{
		Paths: paths, Exec: rec, Templates: template.New(paths.TemplatesDir()),
		Store: st, Installer: &Installer{Exec: rec},
		Now: func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) },
	}
	return prov, proj
}

func TestAppliesOnlyToRuntimeKinds(t *testing.T) {
	for _, kind := range []model.AppKind{model.KindGo, model.KindNode, model.KindPython, model.KindRuby} {
		_, proj := fixture(t, kind)
		if !Applies(proj) {
			t.Errorf("%s doit relever du provider applicatif", kind)
		}
	}
	for _, kind := range []model.AppKind{model.KindSymfony, model.KindPrestashop, model.KindStatic} {
		_, proj := fixture(t, kind)
		if Applies(proj) {
			t.Errorf("%s ne relève pas du provider applicatif", kind)
		}
	}
}

func TestEndpointDefaultsToSocketExceptGo(t *testing.T) {
	// Le socket Unix est le défaut : ses permissions restreignent l'accès, alors
	// qu'un port local est joignable par tout utilisateur de la machine.
	prov, proj := fixture(t, model.KindNode)
	endpoint, err := prov.Endpoint(proj)
	if err != nil {
		t.Fatal(err)
	}
	if !endpoint.Unix() {
		t.Errorf("Node doit écouter sur un socket : %+v", endpoint)
	}
	if !strings.HasSuffix(endpoint.Socket, filepath.Join("shopfr", "app.sock")) {
		t.Errorf("chemin du socket : %q", endpoint.Socket)
	}
	// Le numéro est attribué même en mode socket : il identifie l'application.
	if endpoint.Port != 22001 {
		t.Errorf("numéro attribué : %d, attendu 22001", endpoint.Port)
	}

	// Un binaire Go n'a pas de convention de socket : il écoute sur un port.
	prov, proj = fixture(t, model.KindGo)
	endpoint, err = prov.Endpoint(proj)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Unix() {
		t.Errorf("Go doit écouter sur un port : %+v", endpoint)
	}
	if endpoint.Address != "127.0.0.1:21001" {
		t.Errorf("adresse : %q", endpoint.Address)
	}
}

func TestPortNumbering(t *testing.T) {
	cases := map[model.AppKind]int{
		model.KindGo: 21001, model.KindNode: 22001,
		model.KindPython: 23001, model.KindRuby: 24001,
	}
	for kind, want := range cases {
		prov, proj := fixture(t, kind)
		endpoint, err := prov.Endpoint(proj)
		if err != nil {
			t.Fatal(err)
		}
		if endpoint.Port != want {
			t.Errorf("%s : port %d, attendu %d", kind, endpoint.Port, want)
		}
	}
}

func TestPortIsStableAndIncrements(t *testing.T) {
	prov, first := fixture(t, model.KindNode)
	e1, err := prov.Endpoint(first)
	if err != nil {
		t.Fatal(err)
	}
	// Le même projet garde son numéro.
	again, _ := prov.Endpoint(first)
	if again.Port != e1.Port {
		t.Errorf("numéro instable : %d puis %d", e1.Port, again.Port)
	}
	// Un second projet en reçoit un autre.
	second := model.New("autre.fr", naming.EnvProd, model.KindNode)
	second.Spec.App.Runtime = "22.11.0"
	if err := second.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	e2, err := prov.Endpoint(second)
	if err != nil {
		t.Fatal(err)
	}
	if e2.Port == e1.Port {
		t.Errorf("deux projets partagent le port %d", e1.Port)
	}
}

func TestListenPortCanBeForced(t *testing.T) {
	prov, proj := fixture(t, model.KindNode)
	proj.Spec.App.Listen = "port"
	endpoint, err := prov.Endpoint(proj)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Unix() {
		t.Error("le mode port doit être respecté")
	}
	// L'écoute reste bornée à la boucle locale : autrement l'application serait
	// joignable depuis Internet, contournant le vhost et le TLS.
	if !strings.HasPrefix(endpoint.Address, "127.0.0.1:") {
		t.Errorf("adresse : %q", endpoint.Address)
	}
}

func TestBuildUnitPerRuntime(t *testing.T) {
	cases := map[model.AppKind]string{
		model.KindNode:   "server.js",
		model.KindPython: "gunicorn",
		model.KindRuby:   "puma",
	}
	for kind, want := range cases {
		prov, proj := fixture(t, kind)
		endpoint, err := prov.Endpoint(proj)
		if err != nil {
			t.Fatal(err)
		}
		content, err := prov.Build(proj, endpoint)
		if err != nil {
			t.Fatal(err)
		}
		text := string(content)
		if !strings.Contains(text, want) {
			t.Errorf("%s : commande inattendue, %q absent :\n%s", kind, want, text)
		}
		// Le durcissement et le umask sont systématiques.
		for _, required := range []string{"UMask=0007", "NoNewPrivileges=true", "ProtectSystem=full"} {
			if !strings.Contains(text, required) {
				t.Errorf("%s : %q absent", kind, required)
			}
		}
		// Le point d'écoute est transmis par l'environnement.
		if !strings.Contains(text, "SOCKET_PATH=") {
			t.Errorf("%s : le chemin du socket doit être transmis :\n%s", kind, text)
		}
		// Le lien géré est employé, jamais un binaire versionné.
		if !strings.Contains(text, filepath.Join(".bin", LinkName(mustRuntime(t, proj)))) {
			t.Errorf("%s : le lien géré doit être employé :\n%s", kind, text)
		}
	}
}

func mustRuntime(t *testing.T, proj *model.Project) string {
	t.Helper()
	at, err := proj.AppType()
	if err != nil {
		t.Fatal(err)
	}
	return at.Runtime
}

func TestBuildGoUsesManagedBinaryLink(t *testing.T) {
	// Remplacer le lien puis redémarrer constitue une bascule atomique.
	prov, proj := fixture(t, model.KindGo)
	endpoint, _ := prov.Endpoint(proj)
	content, err := prov.Build(proj, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.Contains(text, filepath.Join(".bin", "app")) {
		t.Errorf("le binaire doit passer par .bin/app :\n%s", text)
	}
	if !strings.Contains(text, "PORT=21001") || !strings.Contains(text, "HOST=127.0.0.1") {
		t.Errorf("le port doit être transmis :\n%s", text)
	}
}

func TestPlanInstallsRuntimeThenLinksThenWaits(t *testing.T) {
	prov, proj := fixture(t, model.KindNode)
	ctx := context.Background()
	obs, err := prov.Observe(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	// L'attente de disponibilité est rendue à part : elle doit venir après le
	// démarrage des instances, dont le provider d'unités a la charge.
	health, err := prov.PlanHealth(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if health == nil {
		t.Fatal("aucune attente de disponibilité")
	}
	steps = append(steps, health)
	var order []string
	for _, s := range steps {
		order = append(order, s.Resource)
	}
	expected := []string{"app/runtime/node", "app/link/node", "app/unit/shopfr", "app/health/shopfr"}
	if strings.Join(order, ",") != strings.Join(expected, ",") {
		t.Errorf("ordre des étapes :\n  %v\nattendu :\n  %v", order, expected)
	}
	// L'installation du runtime et l'attente sont non critiques : le site reste
	// déployé même si l'application ne démarre pas.
	for _, s := range steps {
		if strings.HasPrefix(s.Resource, "app/runtime/") || strings.HasPrefix(s.Resource, "app/health/") {
			if s.Critical {
				t.Errorf("%s doit être non critique", s.Resource)
			}
		}
	}
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	// Le lien et l'unité sont en place.
	target, err := os.Readlink(LinkPath(proj.Spec.Layout.Root, RuntimeNode))
	if err != nil {
		t.Fatalf("lien absent : %v", err)
	}
	if !strings.Contains(target, "v22.11.0") {
		t.Errorf("cible du lien : %q", target)
	}
	if _, err := os.Stat(prov.unitPath(proj)); err != nil {
		t.Errorf("unité absente : %v", err)
	}
}

func TestPlanRequiresRuntimeVersion(t *testing.T) {
	prov, proj := fixture(t, model.KindNode)
	proj.Spec.App.Runtime = ""
	obs, _ := prov.Observe(context.Background(), proj)
	_, err := prov.Plan(context.Background(), proj, obs)
	if err == nil || !strings.Contains(err.Error(), "version de runtime") {
		t.Errorf("une version est indispensable : %v", err)
	}
}

func TestPlanIsIdempotent(t *testing.T) {
	prov, proj := fixture(t, model.KindNode)
	ctx := context.Background()
	obs, _ := prov.Observe(ctx, proj)
	steps, _ := prov.Plan(ctx, proj, obs)
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	// Le runtime est réputé installé et l'application à l'écoute.
	binary := BinaryPath(proj.Spec.Layout.Root, RuntimeNode, "22.11.0")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	obs, _ = prov.Observe(ctx, proj)
	obs.Listening = true
	steps, _ = prov.Plan(ctx, proj, obs)
	plan2 := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan2.Add(s)
	}
	if !plan2.Empty() {
		t.Errorf("second plan non vide :\n%s", plan2.String())
	}
}

func TestGoRuntimeHasNoVersionManager(t *testing.T) {
	prov, proj := fixture(t, model.KindGo)
	err := prov.Installer.Install(context.Background(), proj, RuntimeGo, "1.23")
	if err == nil || !strings.Contains(err.Error(), ".bin/app") {
		t.Errorf("le message doit expliquer le fonctionnement : %v", err)
	}
}

func TestInstallUsesForcedShell(t *testing.T) {
	// L'utilisateur du projet a nologin : « su -s » force le shell, sans quoi
	// l'installation échouerait.
	prefix := t.TempDir()
	rec := exec.NewRecorder()
	installer := &Installer{Exec: &realExecutor{rec}}
	proj := model.New("shop.fr", naming.EnvProd, model.KindNode)
	if err := proj.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if err := installer.Install(context.Background(), proj, RuntimeNode, "22.11.0"); err != nil {
		t.Fatal(err)
	}
	calls := rec.Calls()
	if len(calls) != 1 {
		t.Fatalf("%d appels : %+v", len(calls), calls)
	}
	if calls[0].Name != "su" {
		t.Errorf("commande : %q", calls[0].Name)
	}
	joined := strings.Join(calls[0].Args, " ")
	if !strings.Contains(joined, "-s /bin/bash") {
		t.Errorf("le shell doit être forcé : %q", joined)
	}
	if !strings.Contains(joined, "nvm install 22.11.0") {
		t.Errorf("commande d'installation : %q", joined)
	}
	if !strings.Contains(joined, ".nvm") {
		t.Errorf("le gestionnaire doit vivre dans le home : %q", joined)
	}
}

// realExecutor se déclare non simulé afin que les commandes soient enregistrées.
type realExecutor struct{ *exec.Recorder }

func (r *realExecutor) Simulated() bool { return false }

func TestExposedPortsDetectsPublicListeners(t *testing.T) {
	// Une application joignable depuis l'extérieur contourne le vhost, donc le
	// TLS et l'authentification.
	listeners := []string{
		"127.0.0.1:22001", // correct
		"0.0.0.0:22002",   // exposé
		"[::]:21003",      // exposé
		"0.0.0.0:8080",    // hors de nos plages : ce n'est pas notre affaire
		"127.0.0.1:3306",  // hors plages
	}
	exposed := ExposedPorts(listeners)
	if len(exposed) != 2 {
		t.Fatalf("%d exposés, 2 attendus : %v", len(exposed), exposed)
	}
	for _, want := range []string{"0.0.0.0:22002", "[::]:21003"} {
		found := false
		for _, e := range exposed {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s devrait être signalé : %v", want, exposed)
		}
	}
}

func TestWaitReadyMessageNamesLikelyCause(t *testing.T) {
	// Le cas le plus fréquent est que l'application ignore la variable : le
	// message doit le dire, sinon le diagnostic est long.
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	prov := &Provider{Paths: paths, Exec: &realExecutor{exec.NewRecorder()}}
	proj := model.New("shop.fr", naming.EnvProd, model.KindNode)
	proj.Spec.App.Health = &model.Health{Timeout: "10ms"}
	if err := proj.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	err := prov.waitReady(context.Background(), proj,
		Endpoint{Socket: filepath.Join(prefix, "absent.sock")})
	if err == nil {
		t.Fatal("erreur attendue")
	}
	if !strings.Contains(err.Error(), "SOCKET_PATH") || !strings.Contains(err.Error(), "listen: port") {
		t.Errorf("le message doit orienter le diagnostic : %v", err)
	}
}
