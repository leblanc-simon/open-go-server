package php

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
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/template"
)

// fixture prépare une arborescence PHP simulée sous un préfixe temporaire.
func fixture(t *testing.T, versions ...string) (*Provider, config.Paths, string) {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	for _, v := range versions {
		if err := os.MkdirAll(paths.PHPPoolDir(v), 0o755); err != nil {
			t.Fatal(err)
		}
		// Binaires factices : la détection vérifie leur présence, pas leur contenu.
		for _, bin := range []string{paths.PHPBin(v), paths.PHPFpmBin(v)} {
			if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(paths.PHPRunDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	prov := &Provider{
		Paths:     paths,
		Exec:      exec.NewRecorder(),
		Templates: template.New(paths.TemplatesDir()),
		Store:     st,
		Now:       func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) },
	}
	return prov, paths, prefix
}

func project(t *testing.T, host string, kind model.AppKind, version, php string) *model.Project {
	t.Helper()
	p := model.New(host, naming.EnvProd, kind)
	p.Spec.App.Version = version
	if err := p.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if php != "" {
		p.Spec.PHP.Pools[0].Version = php
	}
	return p
}

func TestDetect(t *testing.T) {
	_, paths, _ := fixture(t, "7.4", "8.3", "8.5")
	versions := Detect(paths)
	if len(versions) != 3 {
		t.Fatalf("%d versions détectées, 3 attendues : %+v", len(versions), versions)
	}
	if versions[0].Version != "7.4" || versions[2].Version != "8.5" {
		t.Errorf("tri instable : %+v", versions)
	}
	for _, v := range versions {
		if !v.Usable {
			t.Errorf("%s devrait être utilisable : %v", v.Version, v.Missing)
		}
	}
	// La fin de support est signalée, sans rendre la version inutilisable.
	if versions[0].EOL == "" {
		t.Error("PHP 7.4 devrait porter une date de fin de support")
	}
	if versions[2].EOL != "" {
		t.Errorf("PHP 8.5 ne devrait pas être en fin de support : %q", versions[2].EOL)
	}
}

func TestDetectReportsMissingBinaries(t *testing.T) {
	_, paths, _ := fixture(t, "8.3")
	if err := os.Remove(paths.PHPFpmBin("8.3")); err != nil {
		t.Fatal(err)
	}
	v, ok := Find(Detect(paths), "8.3")
	if !ok {
		t.Fatal("version non détectée")
	}
	if v.Usable {
		t.Error("une version sans binaire FPM ne doit pas être utilisable")
	}
	if len(v.Missing) != 1 || !strings.Contains(v.Missing[0], "php-fpm8.3") {
		t.Errorf("le manque doit être explicité : %v", v.Missing)
	}
}

func TestBuildPoolNumbering(t *testing.T) {
	prov, _, _ := fixture(t, "8.5", "7.4")
	// Trois projets sur PHP 8.5 : 18501, 18502, 18503.
	for i, host := range []string{"a.fr", "b.fr", "c.fr"} {
		p := project(t, host, model.KindSymfony, "6.4", "8.5")
		bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
		if err != nil {
			t.Fatal(err)
		}
		if want := 18501 + i; bp.Number != want {
			t.Errorf("%s : numéro %d, attendu %d", host, bp.Number, want)
		}
	}
	// La plage de PHP 7.4 est indépendante.
	p := project(t, "vieux.fr", model.KindPrestashop, "", "7.4")
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	if bp.Number != 17401 {
		t.Errorf("PHP 7.4 : numéro %d, attendu 17401", bp.Number)
	}
}

func TestBuildSocketPathCarriesNumber(t *testing.T) {
	prov, _, prefix := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(prefix, "run", "php", "php8.5-fpm-18501-shopfr.sock")
	if bp.Listen != want {
		t.Errorf("point d'écoute %q, attendu %q", bp.Listen, want)
	}
	if !bp.Unix {
		t.Error("le socket Unix est le défaut")
	}
	if !strings.Contains(string(bp.Content), "listen.mode = 0660") {
		t.Error("le socket doit être en 0660 pour que www-data puisse s'y connecter")
	}
	if !strings.Contains(string(bp.Content), "listen.group = shopfr") {
		t.Error("le groupe du socket doit être celui du projet")
	}
}

func TestBuildTCPListen(t *testing.T) {
	prov, _, _ := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	p.Spec.PHP.Pools[0].Listen = "tcp"
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	if bp.Listen != "127.0.0.1:18501" {
		t.Errorf("point d'écoute %q", bp.Listen)
	}
	if !strings.Contains(string(bp.Content), "listen.allowed_clients = 127.0.0.1") {
		t.Error("l'écoute TCP doit être restreinte à la boucle locale")
	}
	// L'absence d'authentification doit être écrite noir sur blanc.
	if !strings.Contains(string(bp.Content), "aucune authentification") {
		t.Error("le fichier généré doit documenter le risque de l'écoute TCP")
	}
}

func TestBuildAppliesPresetAndAppSettings(t *testing.T) {
	prov, _, _ := fixture(t, "7.4")
	p := project(t, "boutique.fr", model.KindPrestashop, "", "7.4")
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	content := string(bp.Content)
	// Preset medium.
	if !strings.Contains(content, "pm.max_children = 20") {
		t.Error("le preset medium doit fixer pm.max_children à 20")
	}
	// Réglages propres à Prestashop, qui redéfinissent la mémoire du preset.
	for _, want := range []string{
		"php_admin_value[memory_limit] = 512M",
		"php_admin_value[max_input_vars] = 5000",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("réglage Prestashop absent : %s", want)
		}
	}
	// Cloisonnement systématique.
	if !strings.Contains(content, "php_admin_value[open_basedir] = "+p.Spec.Layout.Root) {
		t.Error("open_basedir doit être limité au home du projet")
	}
}

func TestBuildProjectSettingsWinOverPreset(t *testing.T) {
	prov, _, _ := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	p.Spec.PHP.Pools[0].Settings = map[string]string{"pm.max_children": "99"}
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bp.Content), "pm.max_children = 99") {
		t.Error("la surcharge du projet doit primer sur le preset")
	}
}

func TestBuildSetsEnvVarPerVariant(t *testing.T) {
	prov, _, _ := fixture(t, "8.5", "7.4")
	modern := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	bp, err := prov.Build(modern, modern.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bp.Content), "env[APP_ENV] = prod") {
		t.Errorf("Symfony 4+ utilise APP_ENV :\n%s", bp.Content)
	}
	legacy := project(t, "ancien.fr", model.KindSymfony, "3.4", "7.4")
	bp, err = prov.Build(legacy, legacy.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bp.Content), "env[SYMFONY_ENV] = prod") {
		t.Errorf("Symfony 3.4 utilise SYMFONY_ENV :\n%s", bp.Content)
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	// Deux rendus successifs doivent être identiques, sans quoi chaque apply
	// réécrirait le fichier et rechargerait FPM pour rien.
	prov, _, _ := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	first, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	second, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Content) != string(second.Content) {
		t.Error("le rendu du pool n'est pas reproductible")
	}
}

func TestBuildRejectsUnsupportedVersion(t *testing.T) {
	prov, _, _ := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	p.Spec.PHP.Pools[0].Version = "7.2"
	if _, err := prov.Build(p, p.Spec.PHP.Pools[0]); err == nil {
		t.Error("PHP 7.2 est hors périmètre")
	}
}

func TestPlanAppliesAndIsIdempotent(t *testing.T) {
	prov, paths, _ := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ctx := context.Background()

	obs, err := prov.Observe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if len(plan.Mutating()) != 1 {
		t.Fatalf("une seule étape attendue :\n%s", plan.String())
	}
	if got := plan.Reloads(); len(got) != 1 || got[0] != "php:8.5" {
		t.Errorf("rechargements : %v", got)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	poolFile := paths.PHPPoolFile("8.5", "shopfr")
	if _, err := os.Stat(poolFile); err != nil {
		t.Fatalf("pool non écrit : %v", err)
	}

	// Second passage : plus rien à faire.
	obs, _ = prov.Observe(ctx, p)
	steps, err = prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan2 := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan2.Add(s)
	}
	if !plan2.Empty() {
		t.Errorf("le second plan doit être vide :\n%s", plan2.String())
	}
}

func TestPlanDetectsHandEditedPool(t *testing.T) {
	prov, paths, _ := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ctx := context.Background()
	obs, _ := prov.Observe(ctx, p)
	steps, _ := prov.Plan(ctx, p, obs)
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}

	// Modification manuelle du fichier généré.
	poolFile := paths.PHPPoolFile("8.5", "shopfr")
	data, _ := os.ReadFile(poolFile)
	if err := os.WriteFile(poolFile, append(data, []byte("\npm.max_children = 4\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	obs, _ = prov.Observe(ctx, p)
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan2 := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan2.Add(s)
	}
	mutating := plan2.Mutating()
	if len(mutating) != 1 || !strings.Contains(mutating[0].Summary, "modifié hors ogs") {
		t.Fatalf("la modification manuelle doit être signalée :\n%s", plan2.String())
	}
	// Et l'application doit refuser d'écraser sans --force.
	if _, err := engine.Execute(ctx, plan2, engine.Options{}); err == nil {
		t.Error("l'écrasement d'un fichier modifié à la main doit être refusé")
	}
}

func TestPlanRemovesStalePool(t *testing.T) {
	prov, paths, _ := fixture(t, "8.5", "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	ctx := context.Background()

	// Un pool laissé par une version antérieure.
	stale := paths.PHPPoolFile("8.5", "shopfr")
	if err := fs.WriteAtomic(stale, []byte("; ancien pool\n"), 0o644, fs.KeepOwner); err != nil {
		t.Fatal(err)
	}
	obs, err := prov.Observe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Stale) != 1 || obs.Stale[0] != stale {
		t.Fatalf("pool obsolète non repéré : %+v", obs.Stale)
	}
	steps, _ := prov.Plan(ctx, p, obs)
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	// Les suppressions sont différées : elles s'appliquent après les vhosts.
	for _, s := range prov.PlanStale(ctx, p, obs) {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("le pool obsolète devrait avoir été supprimé : %v", err)
	}
}

func TestObserveIgnoresOtherProjectsPools(t *testing.T) {
	prov, paths, _ := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	other := paths.PHPPoolFile("8.5", "autreprojet")
	if err := fs.WriteAtomic(other, []byte("; pool d'un autre projet\n"), 0o644, fs.KeepOwner); err != nil {
		t.Fatal(err)
	}
	// Le pool www.conf de la distribution ne doit pas non plus être touché.
	distro := paths.PHPPoolFile("8.5", "www")
	if err := fs.WriteAtomic(distro, []byte("[www]\n"), 0o644, fs.KeepOwner); err != nil {
		t.Fatal(err)
	}
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Stale) != 0 {
		t.Errorf("aucun pool étranger ne doit être considéré comme obsolète : %v", obs.Stale)
	}
}

func TestPlanRejectsUninstalledVersion(t *testing.T) {
	prov, _, _ := fixture(t, "8.5")
	prov.Exec = &exec.Local{} // exécuteur réel : la version doit exister
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.1")
	obs, _ := prov.Observe(context.Background(), p)
	_, err := prov.Plan(context.Background(), p, obs)
	if err == nil || !strings.Contains(err.Error(), "n'est pas installé") {
		t.Fatalf("une version absente doit être refusée : %v", err)
	}
}
