package app

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
	runtimeprov "leblanc.io/open-go-server/internal/provider/runtime"
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
	// Le home existe avant tout plan applicatif : c'est le provider système qui le
	// crée, et les liens gérés refusent de fabriquer la racine d'un projet.
	if err := os.MkdirAll(proj.Spec.Layout.Root, 0o711); err != nil {
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
	cases := map[model.AppKind]struct{ attendu, lien string }{
		model.KindNode:   {"server.js", LinkName(RuntimeNode)},
		model.KindPython: {"gunicorn", LinkName(RuntimePython)},
		// Ruby n'invoque pas son interpréteur mais son lanceur : c'est « bundle »
		// qui est lié.
		model.KindRuby: {"puma", LinkBundle},
	}
	for kind, cas := range cases {
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
		if !strings.Contains(text, cas.attendu) {
			t.Errorf("%s : commande inattendue, %q absent :\n%s", kind, cas.attendu, text)
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
		if !strings.Contains(text, filepath.Join(".local", "bin", cas.lien)) {
			t.Errorf("%s : le lien géré doit être employé :\n%s", kind, text)
		}
		if strings.Contains(text, ".rbenv") || strings.Contains(text, ".nvm") {
			t.Errorf("%s : aucune version ne doit apparaître dans l'unité :\n%s", kind, text)
		}
	}
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
	if !strings.Contains(text, filepath.Join(".local", "bin", "app")) {
		t.Errorf("le binaire doit passer par .local/bin/app :\n%s", text)
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
	expected := []string{"app/rundir/shopfr", "app/runtime/node", "app/link/node",
		"app/unit/shopfr", "app/health/shopfr"}
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
	if err == nil || !strings.Contains(err.Error(), ".local/bin/app") {
		t.Errorf("le message doit expliquer le fonctionnement : %v", err)
	}
}

func TestInstallUsesForcedShell(t *testing.T) {
	// Le compte du projet peut avoir un shell fermé (D62) : bash est nommé, et la
	// commande passe sous l'identité du projet par AsUser — jamais par « su -c »,
	// qui la sortait du groupe de processus tué à l'annulation (D122).
	prefix := t.TempDir()
	rec := exec.NewRecorder()
	installer := &Installer{Exec: &realExecutor{rec}}
	proj := model.New("shop.fr", naming.EnvProd, model.KindNode)
	if err := proj.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	// Le téléchargement suit le mandataire d'ogs : seule l'installation le
	// reçoit (D122).
	t.Setenv("https_proxy", "http://mandataire:3128")
	if err := installer.Install(context.Background(), proj, RuntimeNode, "22.11.0"); err != nil {
		t.Fatal(err)
	}
	calls := rec.Calls()
	if len(calls) != 1 {
		t.Fatalf("%d appels : %+v", len(calls), calls)
	}
	if !slices.Contains(calls[0].Env, "https_proxy=http://mandataire:3128") {
		t.Errorf("le mandataire doit suivre l'installation : %q", calls[0].Env)
	}
	if calls[0].Name != "/bin/bash" || len(calls[0].Args) != 2 || calls[0].Args[0] != "-c" {
		t.Errorf("commande : %q %q", calls[0].Name, calls[0].Args)
	}
	if calls[0].AsUser != proj.Spec.User.Name || calls[0].Dir != proj.Spec.Layout.Root {
		t.Errorf("identité %q, répertoire %q", calls[0].AsUser, calls[0].Dir)
	}
	joined := strings.Join(calls[0].Args, " ")
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

func TestLaVersionDuRuntimeSInterrogeSousLIdentiteDuProjet(t *testing.T) {
	// Le lien vit dans ~/.local/bin, à l'utilisateur (D70) : lancé en root, le
	// binaire qu'il désigne serait celui que l'utilisateur a choisi.
	_, proj := fixture(t, model.KindNode)
	bin := filepath.Join(proj.Spec.Layout.Root, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "node")
	if err := os.Symlink(BinaryPath(proj.Spec.Layout.Root, RuntimeNode, "22.11.0"), link); err != nil {
		t.Fatal(err)
	}
	rec := exec.NewRecorder()
	rec.Responses[link+" --version"] = exec.Result{Stdout: "v22.11.0\n"}
	installer := &Installer{Exec: &realExecutor{rec}}
	got, err := installer.Version(context.Background(), proj, RuntimeNode)
	if err != nil || got != "22.11.0" {
		t.Fatalf("version %q, %v", got, err)
	}
	calls := rec.Calls()
	if len(calls) != 1 || calls[0].AsUser != proj.Spec.User.Name {
		t.Errorf("le binaire lié doit tourner sous l'identité %s : %+v", proj.Spec.User.Name, calls)
	}
}

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

// execStart extrait la ligne de commande d'une unité rendue.
func execStart(t *testing.T, unit string) string {
	t.Helper()
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			return strings.TrimPrefix(line, "ExecStart=")
		}
	}
	t.Fatalf("aucun ExecStart :\n%s", unit)
	return ""
}

func TestCommandeRubyInvoqueLeLanceurEtNonLInterpreteur(t *testing.T) {
	// « ruby -e bundle » évaluait le mot « bundle » comme un programme d'une
	// ligne : le service mourait à chaque démarrage sans que puma existe jamais,
	// et l'attente de disponibilité accusait ensuite le code applicatif.
	prov, proj := fixture(t, model.KindRuby)
	proj.Spec.App.Runtime = "3.3.6"
	endpoint, err := prov.Endpoint(proj)
	if err != nil {
		t.Fatal(err)
	}
	content, err := prov.Build(proj, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	commande := execStart(t, string(content))
	champs := strings.Fields(commande)
	if base := filepath.Base(champs[0]); base != LinkBundle && base != "puma" {
		t.Errorf("l'exécutable doit être un lanceur, obtenu %q :\n%s", base, commande)
	}
	if strings.Contains(commande, " -e ") {
		t.Errorf("« -e » prend son argument pour un programme :\n%s", commande)
	}
	if !strings.Contains(commande, "-b unix://"+endpoint.Socket) {
		t.Errorf("le point d'écoute doit être imposé par la ligne de commande :\n%s", commande)
	}
}

func TestEntrypointRubyDesigneLaConfigurationPuma(t *testing.T) {
	// L'entrypoint déclaré était ignoré sans un mot pour Ruby.
	prov, proj := fixture(t, model.KindRuby)
	proj.Spec.App.Runtime = "3.3.6"
	proj.Spec.App.Entrypoint = "config/puma.rb"
	endpoint, _ := prov.Endpoint(proj)
	content, err := prov.Build(proj, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	commande := execStart(t, string(content))
	if !strings.Contains(commande, "-C config/puma.rb") {
		t.Errorf("l'entrypoint doit devenir la configuration de puma :\n%s", commande)
	}
	// Le bind vient après : la ligne de commande l'emporte sur le fichier.
	if strings.Index(commande, "-C ") > strings.Index(commande, "-b ") {
		t.Errorf("le bind doit suivre la configuration :\n%s", commande)
	}
}

func TestPlanRubyPoseLeLienDuLanceur(t *testing.T) {
	// L'unité n'invoque que des liens gérés (D30) : le lanceur en exige un
	// second, à côté de celui de l'interpréteur.
	prov, proj := fixture(t, model.KindRuby)
	proj.Spec.App.Runtime = "3.3.6"
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
		t.Fatal(err)
	}
	cible, err := os.Readlink(filepath.Join(proj.Spec.Layout.Root, ".local", "bin", LinkBundle))
	if err != nil {
		t.Fatalf("lien du lanceur absent : %v", err)
	}
	if cible != BundlePath(proj.Spec.Layout.Root, "3.3.6") {
		t.Errorf("cible du lien : %q", cible)
	}
	// Le lanceur vit dans le même bin que l'interpréteur.
	if filepath.Dir(cible) != filepath.Dir(BinaryPath(proj.Spec.Layout.Root, RuntimeRuby, "3.3.6")) {
		t.Errorf("le lanceur doit être voisin de l'interpréteur : %q", cible)
	}
	// Idempotence : le runtime est réputé installé et l'application à l'écoute.
	binaire := BinaryPath(proj.Spec.Layout.Root, RuntimeRuby, "3.3.6")
	if err := os.MkdirAll(filepath.Dir(binaire), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaire, []byte("#!/bin/sh\n"), 0o755); err != nil {
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

func TestPlanCreeLeRepertoireDExecutionAvantLUnite(t *testing.T) {
	// Le fichier tmpfiles appartient au plan de parc, exécuté après tous les
	// projets : l'instance aurait démarré avant que le répertoire de son socket
	// existe, et l'attente de disponibilité aurait accusé le code applicatif.
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
	if steps[0].Resource != "app/rundir/"+proj.ID() {
		t.Fatalf("le répertoire d'exécution doit venir en premier : %s", steps[0].Resource)
	}
	for _, s := range steps {
		if s.Resource == "app/unit/"+proj.ID() {
			break
		}
		if s.Resource == "app/rundir/"+proj.ID() && s.Action == engine.NoOp {
			t.Fatal("le répertoire est absent : l'étape ne peut pas être un NoOp")
		}
	}
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	runDir := prov.Paths.ProjectRunDir(proj.ID())
	info, err := os.Stat(runDir)
	if err != nil {
		t.Fatalf("répertoire d'exécution absent : %v", err)
	}
	// Le mode est celui que déclare le fichier tmpfiles : deux endroits qui en
	// décideraient feraient osciller le plan.
	if info.Mode().Perm() != runtimeprov.ProjectRunMode.Perm() {
		t.Errorf("mode %04o, attendu %04o", info.Mode().Perm(), runtimeprov.ProjectRunMode.Perm())
	}
	// Idempotence : le répertoire posé, l'étape ne mute plus.
	obs, _ = prov.Observe(ctx, proj)
	steps, _ = prov.Plan(ctx, proj, obs)
	if steps[0].Action != engine.NoOp {
		t.Errorf("second plan : %s (%s)", steps[0].Summary, steps[0].Action)
	}
}

// ecritUnite applique la seule étape d'unité du plan, pour disposer d'un fichier
// portant la marque d'ogs.
func ecritUnite(t *testing.T, prov *Provider, proj *model.Project) string {
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
	for _, s := range steps {
		if s.Resource == "app/unit/"+proj.ID() {
			if err := s.Apply(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	return prov.unitPath(proj)
}

// planApresRetouche rejoue un plan sur une unité à laquelle un exploitant a
// ajouté une directive.
func planApresRetouche(t *testing.T, prov *Provider, proj *model.Project) ([]*engine.Step, string) {
	t.Helper()
	path := ecritUnite(t, prov, proj)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("MemoryMax=512M\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	obs, err := prov.Observe(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	return steps, path
}

func TestUniteApplicativeEditeeAMainEstRefuseeSansForce(t *testing.T) {
	// Une application supervisée est un service comme un autre : son unité doit
	// répondre au même geste que celle du provider d'unités, dans le même
	// répertoire.
	prov, proj := fixture(t, model.KindNode)
	steps, path := planApresRetouche(t, prov, proj)

	var refus *engine.Step
	for _, s := range steps {
		if s.Resource == "app/unit/"+proj.ID() {
			refus = s
		}
	}
	if refus == nil {
		t.Fatalf("aucune étape d'unité :\n%v", steps)
	}
	if !strings.Contains(refus.Summary, "modifié hors ogs") {
		t.Fatalf("la modification manuelle doit être signalée : %q", refus.Summary)
	}
	ctx := context.Background()
	err := refus.Apply(ctx)
	if err == nil {
		t.Fatal("l'écrasement doit être refusé sans --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("le refus doit nommer la porte de sortie : %v", err)
	}
	// Rien n'a été écrit : rien ne doit être rechargé ni redémarré.
	if len(refus.Reload) != 0 {
		t.Errorf("le refus ne doit demander aucun rechargement : %v", refus.Reload)
	}
	for _, s := range steps {
		if slices.Contains(s.Reload, RestartTargetIntent(proj.ID())) {
			t.Errorf("aucun redémarrage des instances ne doit être demandé : %s", s.Summary)
		}
	}
	if after, _ := os.ReadFile(path); !strings.Contains(string(after), "MemoryMax=512M") {
		t.Error("le refus ne doit rien écrire")
	}
}

func TestUniteApplicativeEcraseeAvecForceApresSauvegarde(t *testing.T) {
	prov, proj := fixture(t, model.KindNode)
	prov.Force = true
	steps, path := planApresRetouche(t, prov, proj)

	var forcée *engine.Step
	for _, s := range steps {
		if s.Resource == "app/unit/"+proj.ID() {
			forcée = s
		}
	}
	if forcée == nil || !strings.Contains(forcée.Summary, "écrasement forcé") {
		t.Fatalf("le plan doit annoncer l'écrasement forcé :\n%v", steps)
	}
	// L'unité change réellement : le redémarrage groupé reste dû, sans quoi les
	// instances continueraient de tourner avec l'ancienne définition.
	if !slices.Contains(forcée.Reload, RestartTargetIntent(proj.ID())) {
		t.Errorf("l'écrasement forcé doit demander le redémarrage groupé : %v", forcée.Reload)
	}
	ctx := context.Background()
	if err := forcée.Apply(ctx); err != nil {
		t.Fatalf("l'écrasement forcé doit réussir : %v", err)
	}
	if after, _ := os.ReadFile(path); strings.Contains(string(after), "MemoryMax=512M") {
		t.Error("l'unité doit avoir été réécrite")
	}
	// La copie sort de /etc/systemd/system : l'importateur y énumère les unités du
	// parc et adopterait la sauvegarde comme une unité de plus.
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
	// La compensation rend l'unité de l'exploitant, non le vide : une saga
	// annulée après l'écrasement l'aurait sinon effacée pour de bon.
	if err := forcée.Compensate(ctx); err != nil {
		t.Fatalf("compensation : %v", err)
	}
	if after, _ := os.ReadFile(path); !strings.Contains(string(after), "MemoryMax=512M") {
		t.Error("la compensation doit rendre l'unité d'origine")
	}
}

// customFixture prépare une application custom : ni runtime ni entrypoint, une
// commande déclarée.
func customFixture(t *testing.T) (*Provider, *model.Project) {
	t.Helper()
	prov, proj := fixture(t, model.KindCustom)
	proj.Spec.App.Runtime = ""
	proj.Spec.App.Command = "/home/shop.fr/www/bin/serveur --listen ${LISTEN_ADDR}"
	return prov, proj
}

func TestCustomRunsDeclaredCommandOnAPort(t *testing.T) {
	prov, proj := customFixture(t)
	if !Applies(proj) {
		t.Fatal("une application custom relève du provider applicatif")
	}
	endpoint, err := prov.Endpoint(proj)
	if err != nil {
		t.Fatal(err)
	}
	// ogs ne sait rien de la commande : il ne peut pas compter sur elle pour lire
	// SOCKET_PATH, et retient le port, dans la plage propre au type.
	if endpoint.Unix() || endpoint.Address != "127.0.0.1:25001" {
		t.Fatalf("point d'écoute : %+v, attendu 127.0.0.1:25001", endpoint)
	}
	body, err := prov.Build(proj, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	unit := string(body)
	for _, want := range []string{
		"ExecStart=/home/shop.fr/www/bin/serveur --listen ${LISTEN_ADDR}\n",
		"Environment=LISTEN_ADDR=127.0.0.1:25001", "Environment=PORT=25001",
		"User=shopfr", "UMask=0007", "NoNewPrivileges=true",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("%q absent de l'unité :\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "EnvironmentFile=") {
		t.Errorf("aucun fichier d'environnement n'a été demandé :\n%s", unit)
	}
	if strings.Contains(unit, model.LinkDir) {
		t.Errorf("une application custom ne passe par aucun lien géré :\n%s", unit)
	}
}

func TestCustomPlanHasNoRuntimeNorLink(t *testing.T) {
	prov, proj := customFixture(t)
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	var resources []string
	for _, s := range steps {
		resources = append(resources, s.Resource)
		if strings.HasPrefix(s.Resource, "app/runtime/") || strings.HasPrefix(s.Resource, "app/link/") {
			t.Errorf("étape inattendue pour une application custom : %s", s.Resource)
		}
	}
	if !slices.Contains(resources, "app/unit/shopfr") {
		t.Errorf("l'unité doit être écrite : %v", resources)
	}
	if err := prov.Installer.Install(context.Background(), proj, RuntimeCustom, "1"); err == nil ||
		!strings.Contains(err.Error(), "spec.app.command") {
		t.Errorf("l'installation d'un runtime custom doit être refusée et expliquée : %v", err)
	}
}

// envFilePlan renvoie l'étape du fichier d'environnement d'un plan.
func envFilePlan(t *testing.T, prov *Provider, proj *model.Project) *engine.Step {
	t.Helper()
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range steps {
		if s.Resource == "app/envfile/shopfr" {
			// Il précède l'unité : systemd refuse de démarrer un service dont
			// l'EnvironmentFile manque.
			if i+1 >= len(steps) || steps[i+1].Resource != "app/unit/shopfr" {
				t.Errorf("le fichier d'environnement doit précéder l'unité")
			}
			return s
		}
	}
	t.Fatal("aucune étape de fichier d'environnement")
	return nil
}

func TestEnvFileIsCreatedOnceAndNeverRewritten(t *testing.T) {
	prov, proj := customFixture(t)
	proj.Spec.App.EnvFile = true
	path := prov.Paths.EnvFile(proj.ID())

	s := envFilePlan(t, prov, proj)
	if s.Action != engine.Create {
		t.Fatalf("action %v, attendu une création", s.Action)
	}
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o, attendu 0600 : le projet n'a pas à lire ce que systemd lit pour lui", info.Mode().Perm())
	}
	if dir, _ := os.Stat(filepath.Dir(path)); dir.Mode().Perm() != 0o700 {
		t.Errorf("répertoire en %04o, attendu 0700 (régime d'ogs harden)", dir.Mode().Perm())
	}

	// Le contenu appartient à l'exploitant : ogs ne le touche plus.
	if err := os.WriteFile(path, []byte("SECRET=valeur\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := envFilePlan(t, prov, proj); s.Action != engine.NoOp {
		t.Errorf("un fichier conforme ne doit rien produire : %v %s", s.Action, s.Summary)
	}
	// Des droits ouverts sont resserrés, le contenu gardé.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	s = envFilePlan(t, prov, proj)
	if s.Action != engine.Update {
		t.Fatalf("des droits ouverts doivent être resserrés : %v", s.Action)
	}
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	info, _ = os.Stat(path)
	if string(data) != "SECRET=valeur\n" || info.Mode().Perm() != 0o600 {
		t.Errorf("après resserrement : %04o %q", info.Mode().Perm(), data)
	}

	body, err := prov.Build(proj, Endpoint{Address: "127.0.0.1:25001", Port: 25001})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "EnvironmentFile="+path+"\n") {
		t.Errorf("l'unité doit nommer le fichier, sans tiret — un fichier manquant doit se voir :\n%s", body)
	}
}

func TestEnvFileRefusesSymlink(t *testing.T) {
	// systemd lit le fichier en root en suivant le lien : un lien vers le .env
	// d'un voisin lui ferait injecter les secrets de celui-ci.
	prov, proj := customFixture(t)
	proj.Spec.App.EnvFile = true
	path := prov.Paths.EnvFile(proj.ID())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "voisin.env"), path); err != nil {
		t.Fatal(err)
	}
	s := envFilePlan(t, prov, proj)
	err := s.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pas un fichier ordinaire") {
		t.Fatalf("un lien doit être refusé, obtenu : %v", err)
	}
}
