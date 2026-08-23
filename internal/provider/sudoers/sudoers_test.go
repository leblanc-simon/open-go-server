package sudoers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
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
