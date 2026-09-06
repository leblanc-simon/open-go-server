package sudoers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
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
	prov := &Provider{Paths: paths, Exec: exec.NewRecorder(),
		Now: func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) }}
	return prov, proj
}

func TestBuildOGSStyleHasNoArgument(t *testing.T) {
	prov, proj := fixture(t)
	content, err := prov.Build(proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, verb := range DefaultVerbs {
		want := "shopfr ALL=(root) NOPASSWD: /usr/bin/ogs my " + verb
		if !strings.Contains(text, want) {
			t.Errorf("règle absente : %s\n%s", want, text)
		}
	}
	// Aucun argument de projet : il n'y a rien à détourner.
	if strings.Contains(text, "shopfr my") || strings.Contains(text, "--project") {
		t.Errorf("la commande ne doit prendre aucune cible :\n%s", text)
	}
}

func TestBuildRejectsMetacharacters(t *testing.T) {
	// Un « * » dans une spécification sudoers laisserait passer des arguments
	// imprévus : le générateur ne doit jamais en produire.
	prov, proj := fixture(t)
	proj.Spec.Sudo = &model.Sudo{Style: StyleOGS, Verbs: []string{"restart", "st*"}}
	if _, err := prov.Build(proj, nil); err == nil {
		t.Error("un verbe contenant un métacaractère doit être refusé")
	}
	proj.Spec.Sudo.Verbs = []string{"restart; rm -rf /"}
	if _, err := prov.Build(proj, nil); err == nil {
		t.Error("un verbe contenant des espaces doit être refusé")
	}
}

func TestValidateRefusesDangerousRules(t *testing.T) {
	cases := map[string]string{
		"métacaractère":        "shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl restart ogs-shopfr-*\n",
		"systemctl nu":         "shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl\n",
		"systemctl sans unité": "shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl daemon-reload\n",
		"délégation totale":    "shopfr ALL=(ALL) NOPASSWD: /usr/bin/ogs my restart\n",
		"chemin relatif":       "shopfr ALL=(root) NOPASSWD: ogs my restart\n",
		// La virgule sépare deux commandes autorisées, le dièse commente la fin de
		// la ligne : la grammaire reste correcte et visudo n'y voit rien.
		"seconde commande": "shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl start ogs-shopfr-w@1.service, /bin/bash\n",
		"fin commentée":    "shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl start ogs-shopfr-w@1.service #\n",
	}
	for name, rule := range cases {
		if err := Validate([]byte(rule)); err == nil {
			t.Errorf("%s : règle acceptée à tort (%q)", name, strings.TrimSpace(rule))
		}
	}
	// Une règle correcte passe.
	ok := "# commentaire\nshopfr ALL=(root) NOPASSWD: /usr/bin/ogs my restart\n"
	if err := Validate([]byte(ok)); err != nil {
		t.Errorf("règle correcte refusée : %v", err)
	}
}

func TestBuildSystemctlStyleEnumeratesInstances(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Sudo = &model.Sudo{Style: StyleSystemctl, Verbs: []string{"restart"}}
	proj.Spec.Services = []model.Service{{Name: "messenger-async", Instances: 3}}
	content, err := prov.Build(proj, map[string]int{"messenger-async": 3})
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	// La cible et chaque instance, nommément : c'est le coût de l'énumération.
	if !strings.Contains(text, "/usr/bin/systemctl restart ogs-shopfr.target") {
		t.Errorf("la cible doit être autorisée :\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		want := "/usr/bin/systemctl restart ogs-shopfr-messenger-async@" + string(rune('0'+i)) + ".service"
		if !strings.Contains(text, want) {
			t.Errorf("instance %d absente :\n%s", i, text)
		}
	}
	// Et surtout : aucun motif générique.
	if strings.Contains(text, "@*") || strings.Contains(text, "-*") {
		t.Errorf("aucun motif ne doit apparaître :\n%s", text)
	}
}

func TestStyleNoneProducesNothing(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Sudo = &model.Sudo{Style: StyleNone}
	content, err := prov.Build(proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Errorf("aucun fichier attendu :\n%s", content)
	}
}

func TestUnknownStyle(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Sudo = &model.Sudo{Style: "polkit"}
	if _, err := prov.Build(proj, nil); err == nil {
		t.Error("style inconnu accepté")
	}
}

func TestApplyWritesRestrictedFile(t *testing.T) {
	prov, proj := fixture(t)
	step, err := prov.Plan(context.Background(), proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := prov.Path("shopfr")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// sudo refuse un fichier de sudoers.d inscriptible : 0440 est la seule valeur
	// acceptable.
	if info.Mode().Perm() != 0o440 {
		t.Errorf("mode %04o, attendu 0440", info.Mode().Perm())
	}
	// Aucun temporaire ne doit subsister : un fichier invalide dans sudoers.d
	// casserait sudo pour tout le système.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ogs-sudoers-") {
			t.Errorf("temporaire laissé en place : %s", e.Name())
		}
	}
	// Idempotence.
	step, err = prov.Plan(context.Background(), proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if step.Mutates() {
		t.Errorf("second plan non vide : %s", step.Summary)
	}
}

func TestVisudoRejectionAbortsWrite(t *testing.T) {
	// Si visudo refuse le fichier, rien ne doit être installé.
	prov, proj := fixture(t)
	prov.Exec = &refusingExecutor{}
	visudo := filepath.Join(prov.Paths.Prefix(), "usr", "sbin", "visudo")
	if err := os.MkdirAll(filepath.Dir(visudo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(visudo, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	step, err := prov.Plan(context.Background(), proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(context.Background()); err == nil {
		t.Fatal("l'écriture devait être refusée")
	}
	if _, err := os.Stat(prov.Path("shopfr")); !os.IsNotExist(err) {
		t.Error("aucun fichier ne doit avoir été installé")
	}
}

// refusingExecutor se déclare non simulé et refuse toute commande : il permet
// d'éprouver le comportement quand visudo rejette le fichier.
type refusingExecutor struct{}

func (r *refusingExecutor) Simulated() bool { return false }

func (r *refusingExecutor) Run(context.Context, exec.Command) (exec.Result, error) {
	return exec.Result{ExitCode: 1}, errRefused
}

var errRefused = &refusedError{}

type refusedError struct{}

func (e *refusedError) Error() string { return "syntaxe invalide" }

func TestAuditDetectsForeignAndLooseFiles(t *testing.T) {
	prov, proj := fixture(t)
	step, _ := prov.Plan(context.Background(), proj, nil)
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	problems, err := prov.Audit()
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Errorf("aucun problème attendu : %v", problems)
	}

	// Un fichier au préfixe ogs- mais non généré par ogs : il pourrait avoir été
	// déposé pour élargir des droits.
	foreign := filepath.Join(prov.Paths.SudoersDir(), "ogs-pirate")
	if err := os.WriteFile(foreign, []byte("pirate ALL=(ALL) NOPASSWD: ALL\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	problems, _ = prov.Audit()
	if len(problems) == 0 || !strings.Contains(problems[0], "pirate") {
		t.Errorf("le fichier étranger doit être signalé : %v", problems)
	}

	// Un fichier généré mais élargi à la main.
	path := prov.Path("shopfr")
	data, _ := os.ReadFile(path)
	// Le fichier est en lecture seule : le rendre inscriptible reproduit ce que
	// ferait un administrateur qui l'édite à la main.
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data,
		[]byte("shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl restart ogs-*\n")...), 0o640); err != nil {
		t.Fatal(err)
	}
	problems, _ = prov.Audit()
	found := false
	for _, p := range problems {
		if strings.Contains(p, "métacaractère") {
			found = true
		}
	}
	if !found {
		t.Errorf("l'élargissement manuel doit être détecté : %v", problems)
	}
}

func TestBuildRefuseUnNomDeServiceDangereux(t *testing.T) {
	// Le nom du service atterrit dans une spécification de commande : une virgule
	// y ouvre une seconde commande et un « # » masque la fin de la ligne. La règle
	// produite serait syntaxiquement irréprochable, donc acceptée par visudo.
	prov, proj := fixture(t)
	proj.Spec.Sudo = &model.Sudo{Style: StyleSystemctl, Verbs: []string{"start"}}
	for _, nom := range []string{
		"w@1.service, /bin/bash #",
		"worker,/bin/sh",
		"worker #",
		"../../cron.d/pwn",
		"",
	} {
		proj.Spec.Services = []model.Service{{Name: nom, Instances: 1}}
		if _, err := prov.Build(proj, nil); err == nil {
			t.Errorf("nom de service accepté à tort : %q", nom)
		}
	}
	// Un nom ordinaire passe.
	proj.Spec.Services = []model.Service{{Name: "messenger_async-2", Instances: 1}}
	if _, err := prov.Build(proj, nil); err != nil {
		t.Errorf("nom de service légitime refusé : %v", err)
	}
}

func TestBuildSuitLaSpecificationEtNonLObserve(t *testing.T) {
	// La carte observée décrit l'état d'AVANT le plan d'unités : s'y fier laissait
	// la délégation d'un apply en retard, sans droit sur les instances qui
	// viennent d'être démarrées.
	prov, proj := fixture(t)
	proj.Spec.Sudo = &model.Sudo{Style: StyleSystemctl, Verbs: []string{"restart"}}
	proj.Spec.Services = []model.Service{{Name: "messenger-async", Instances: 3}}
	content, err := prov.Build(proj, map[string]int{"messenger-async": 1})
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for i := 1; i <= 3; i++ {
		want := "/usr/bin/systemctl restart ogs-shopfr-messenger-async@" +
			string(rune('0'+i)) + ".service"
		if !strings.Contains(text, want) {
			t.Errorf("instance %d absente alors qu'elle est déclarée :\n%s", i, text)
		}
	}
}

func TestBuildUnitLObserveTantQueLaDesactivationNAPasEuLieu(t *testing.T) {
	// À la descente en charge, l'instance retirée peut tourner encore : son
	// propriétaire doit pouvoir l'arrêter jusqu'à ce qu'elle ait disparu.
	prov, proj := fixture(t)
	proj.Spec.Sudo = &model.Sudo{Style: StyleSystemctl, Verbs: []string{"restart"}}
	proj.Spec.Services = []model.Service{{Name: "messenger-async", Instances: 1}}
	content, err := prov.Build(proj, map[string]int{"messenger-async": 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "ogs-shopfr-messenger-async@3.service") {
		t.Errorf("l'instance encore active doit rester pilotable :\n%s", content)
	}
	// Une fois la désactivation faite, la délégation suit la spécification.
	content, err = prov.Build(proj, map[string]int{"messenger-async": 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "messenger-async@2.service") {
		t.Errorf("l'instance retirée ne doit plus figurer :\n%s", content)
	}
}

func TestBuildNeDelegueRienSurUnServiceDesactive(t *testing.T) {
	// Un service que l'exploitant a coupé ne se pilote plus : l'unité étant un
	// modèle, « systemctl start » ne relancerait pas l'instance, il la recréerait.
	prov, proj := fixture(t)
	disabled := false
	proj.Spec.Sudo = &model.Sudo{Style: StyleSystemctl, Verbs: []string{"start"}}
	proj.Spec.Services = []model.Service{
		{Name: "messenger-async", Instances: 2, Enabled: &disabled},
	}
	content, err := prov.Build(proj, map[string]int{"messenger-async": 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "messenger-async@") {
		t.Errorf("aucune instance d'un service désactivé ne doit être déléguée :\n%s", content)
	}
	// La cible du projet reste pilotable : elle ne dépend pas d'un service.
	if !strings.Contains(string(content), "/usr/bin/systemctl start ogs-shopfr.target") {
		t.Errorf("la cible doit rester autorisée :\n%s", content)
	}
}

func TestDelegationModifieeALaMainNEstPasEcraseeEnSilence(t *testing.T) {
	// Une règle sudoers retouchée à la main porte une délégation que quelqu'un a
	// voulue : l'écraser sans un mot la retirerait (§6.4).
	prov, proj := fixture(t)
	ctx := context.Background()
	step, err := prov.Plan(ctx, proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	path := prov.Path(proj.ID())
	data, _ := os.ReadFile(path)
	const ajout = "# commentaire ajouté à la main\n"
	// Le fichier est en 0440 : l'exploitant le rouvre avant d'y toucher.
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte(ajout)...), 0o640); err != nil {
		t.Fatal(err)
	}

	step, err = prov.Plan(ctx, proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(step.Summary, "modifié hors ogs") {
		t.Fatalf("la modification manuelle doit être signalée : %s", step.Summary)
	}
	if err := step.Apply(ctx); err == nil {
		t.Error("l'écrasement doit être refusé sans --force")
	}
	if intact, _ := os.ReadFile(path); !strings.Contains(string(intact), "à la main") {
		t.Error("le refus ne doit rien écrire")
	}

	// --force : écrasement, précédé de la sauvegarde.
	prov.Force = true
	step, err = prov.Plan(ctx, proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(step.Summary, "écrasement forcé") {
		t.Fatalf("le plan doit annoncer l'écrasement forcé : %s", step.Summary)
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatalf("l'écrasement forcé doit réussir : %v", err)
	}
	if after, _ := os.ReadFile(path); strings.Contains(string(after), "à la main") {
		t.Error("le fichier doit avoir été réécrit")
	}
	if restes, _ := filepath.Glob(path + ".ogs-bak-*"); len(restes) != 0 {
		t.Errorf("aucune sauvegarde ne doit rester dans %s : %v", prov.Paths.SudoersDir(), restes)
	}
	baks, _ := filepath.Glob(filepath.Join(prov.Paths.BackupsDir(), filepath.Base(path)+".ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue dans %s, %d trouvée(s)", prov.Paths.BackupsDir(), len(baks))
	}
	if saved, _ := os.ReadFile(baks[0]); !strings.Contains(string(saved), "à la main") {
		t.Error("la sauvegarde doit contenir l'original")
	}
}

func TestLAuditIgnoreLaSauvegardeDUnEcrasementForce(t *testing.T) {
	// La sauvegarde d'une délégation forcée ne doit pas retomber sous les yeux
	// d'Audit : elle porte le préfixe « ogs- », donc le filtre du répertoire la
	// ramasserait, et son contenu est justement celui qu'on vient d'écraser —
	// l'élargissement à la main que Validate refuse, sous le mode 0640 auquel
	// l'exploitant a rouvert le fichier. « doctor » accuserait indéfiniment un
	// fichier qu'ogs a écrit lui-même, avec pour remède de supprimer la seule
	// copie de la configuration écrasée.
	prov, proj := fixture(t)
	ctx := context.Background()
	step, err := prov.Plan(ctx, proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	path := prov.Path(proj.ID())
	data, _ := os.ReadFile(path)
	// Le fichier est en 0440 : l'exploitant le rouvre avant d'y toucher, et la
	// sauvegarde héritera de ce mode.
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	élargie := append(data, []byte("shopfr ALL=(root) NOPASSWD: /usr/bin/systemctl restart ogs-shopfr-*\n")...)
	if err := os.WriteFile(path, élargie, 0o640); err != nil {
		t.Fatal(err)
	}
	if problems, err := prov.Audit(); err != nil || len(problems) == 0 {
		t.Fatalf("l'élargissement manuel doit être signalé avant l'écrasement : %v (%v)", problems, err)
	}

	prov.Force = true
	step, err = prov.Plan(ctx, proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatalf("l'écrasement forcé doit réussir : %v", err)
	}
	problems, err := prov.Audit()
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Errorf("l'audit ne doit plus rien signaler après l'écrasement : %v", problems)
	}
}

func TestCompensationRendLaDelegationPrecedente(t *testing.T) {
	// La compensation supprimait le fichier : sur une mise à jour, le rollback
	// retirait donc à l'utilisateur du projet un accès qu'il avait déjà.
	prov, proj := fixture(t)
	ctx := context.Background()
	step, err := prov.Plan(ctx, proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	path := prov.Path(proj.ID())
	avant, _ := os.ReadFile(path)

	proj.Spec.Sudo = &model.Sudo{Style: StyleOGS, Verbs: []string{"restart"}}
	step, err = prov.Plan(ctx, proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err := step.Compensate(ctx); err != nil {
		t.Fatal(err)
	}
	après, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("la compensation ne doit pas retirer une délégation existante : %v", err)
	}
	if string(après) != string(avant) {
		t.Error("la compensation doit rendre la délégation précédente")
	}
}
