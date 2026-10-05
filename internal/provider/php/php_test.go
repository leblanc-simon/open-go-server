package php

import (
	"context"
	"errors"
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
	// Preset par défaut de la machine : ondemand (D103).
	if !strings.Contains(content, "pm.max_children = 12") || !strings.Contains(content, "pm = ondemand") {
		t.Error("le preset par défaut, ondemand, doit fixer pm.max_children à 12")
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
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{"pm.max_children": model.Value("99")}
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

func TestBuildPrepResteEnProd(t *testing.T) {
	// D96 : la préproduction est publique, et un code installé sans les
	// dépendances de dev ne démarre pas en « dev ».
	prov, _, _ := fixture(t, "8.5")
	p := model.New("shop.fr", naming.EnvPrep, model.KindSymfony)
	p.Spec.App.Version = "6.4"
	if err := p.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	p.Spec.PHP.Pools[0].Version = "8.5"
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bp.Content), "env[APP_ENV] = prod") || strings.Contains(string(bp.Content), "= dev") {
		t.Errorf("la préproduction doit tourner en APP_ENV=prod :\n%s", bp.Content)
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

// poserPool écrit le pool qu'ogs aurait produit pour un projet, tel qu'un apply
// antérieur l'a laissé.
func poserPool(t *testing.T, prov *Provider, p *model.Project, pool model.Pool) string {
	t.Helper()
	bp, err := prov.Build(p, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteAtomic(bp.Path, bp.Content, 0o644, fs.KeepOwner); err != nil {
		t.Fatal(err)
	}
	return bp.Path
}

// planAvecObsoletes calcule le plan des pools, suppressions différées comprises.
func planAvecObsoletes(t *testing.T, prov *Provider, p *model.Project) (*engine.Plan, Observed) {
	t.Helper()
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
	// Les suppressions sont différées : elles s'appliquent après les vhosts.
	stale, err := prov.PlanStale(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stale {
		plan.Add(s)
	}
	return plan, obs
}

func TestPlanRemovesStalePool(t *testing.T) {
	prov, _, _ := fixture(t, "8.5", "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ctx := context.Background()

	// Le pool qu'ogs a écrit avant une bascule de version.
	stale := poserPool(t, prov, p, p.Spec.PHP.Pools[0])
	p.Spec.PHP.Pools[0].Version = "8.3"

	plan, obs := planAvecObsoletes(t, prov, p)
	if len(obs.Stale) != 1 || obs.Stale[0] != stale {
		t.Fatalf("pool obsolète non repéré : %+v", obs.Stale)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("le pool obsolète devrait avoir été supprimé : %v", err)
	}
}

// Le nom d'un pool ne dit pas à qui il appartient : « shop_api.conf » semble
// prolonger « shop » d'un rôle, et c'est le pool principal du projet shop_api.
// L'attribution par préfixe le faisait retirer par le premier apply de shop.
func TestObserveNAttribuePasLePoolDUnVoisinParSonNom(t *testing.T) {
	prov, _, _ := fixture(t, "8.5")
	shop := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	shop.Metadata.Name = "shop"
	voisin := project(t, "api.shop.fr", model.KindSymfony, "6.4", "8.5")
	voisin.Metadata.Name = "shop_api"
	chemin := poserPool(t, prov, voisin, voisin.Spec.PHP.Pools[0])
	if filepath.Base(chemin) != "shop_api.conf" {
		t.Fatalf("le pool du voisin est attendu en shop_api.conf, obtenu %s", chemin)
	}

	plan, obs := planAvecObsoletes(t, prov, shop)
	if len(obs.Stale) != 0 {
		t.Errorf("le pool du voisin ne doit pas être obsolète pour shop : %v", obs.Stale)
	}
	if _, err := engine.Execute(context.Background(), plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(chemin); err != nil {
		t.Errorf("le pool du voisin a été retiré par l'apply de shop : %v", err)
	}
}

// Le sens inverse : le pool « api » de shop s'écrit dans shop_api.conf, le pool
// principal du projet shop_api. Inspect, qui ignore le projet de l'en-tête, le
// tenait pour « à mettre à jour » : shop le réécrivait sous son utilisateur. Le
// fichier d'un autre projet est un conflit, --force compris.
func TestPlanRefuseDEcrireLePoolDUnAutreProjet(t *testing.T) {
	prov, _, _ := fixture(t, "8.5")
	prov.Force = true
	voisin := project(t, "api.shop.fr", model.KindSymfony, "6.4", "8.5")
	voisin.Metadata.Name = "shop_api"
	chemin := poserPool(t, prov, voisin, voisin.Spec.PHP.Pools[0])
	avant, _ := os.ReadFile(chemin)

	shop := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	shop.Metadata.Name = "shop"
	shop.Spec.PHP.Pools = append(shop.Spec.PHP.Pools, model.Pool{Name: "api", Version: "8.5"})
	_, err := prov.Plan(context.Background(), shop, Observed{})
	var owned *fs.OwnerError
	if !errors.As(err, &owned) || owned.Owner != "shop_api" || owned.Path != chemin {
		t.Fatalf("le pool du voisin doit être un conflit nommant son propriétaire : %v", err)
	}
	if got, _ := os.ReadFile(chemin); string(got) != string(avant) {
		t.Error("le pool du voisin a été touché")
	}
}

// Un pool posé à la main selon la convention — le back-office d'un projet — ne
// porte pas la marque d'ogs : il n'est jamais retiré, --force compris.
func TestPoolPoseALaMainSelonLaConventionResteEnPlace(t *testing.T) {
	prov, paths, _ := fixture(t, "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	manuel := paths.PHPPoolFile("8.3", "shopfr_admin")
	corps := []byte("[shopfr_admin]\nuser = shopfr\ngroup = shopfr\nlisten = /run/php/shopfr_admin.sock\n")
	if err := os.WriteFile(manuel, corps, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		prov.Force = force
		plan, obs := planAvecObsoletes(t, prov, p)
		if len(obs.Stale) != 0 {
			t.Errorf("--force=%v : un pool non géré ne doit pas être obsolète : %v", force, obs.Stale)
		}
		if _, err := engine.Execute(context.Background(), plan, engine.Options{}); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(manuel); err != nil || string(data) != string(corps) {
			t.Fatalf("--force=%v : le pool posé à la main a été touché : %v", force, err)
		}
	}
}

// Un pool d'ogs retouché à la main n'est retiré, comme il n'est écrasé, que sous
// --force et après sauvegarde (§6.4).
func TestRetraitDUnPoolRetoucheExigeForceEtSauvegarde(t *testing.T) {
	prov, paths, _ := fixture(t, "8.5", "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ctx := context.Background()
	stale := poserPool(t, prov, p, p.Spec.PHP.Pools[0])
	data, _ := os.ReadFile(stale)
	retouche := append(data, []byte("pm.max_children = 4\n")...)
	if err := os.WriteFile(stale, retouche, 0o644); err != nil {
		t.Fatal(err)
	}
	p.Spec.PHP.Pools[0].Version = "8.3"

	plan, _ := planAvecObsoletes(t, prov, p)
	var retrait *engine.Step
	for _, s := range plan.Mutating() {
		if s.Action == engine.Delete {
			retrait = s
		}
	}
	if retrait == nil || !strings.Contains(retrait.Summary, "modifié hors ogs") {
		t.Fatalf("le plan doit annoncer le refus du retrait :\n%s", plan.String())
	}
	report, err := engine.Execute(ctx, plan, engine.Options{})
	if err != nil {
		t.Fatalf("un retrait refusé ne doit pas défaire le reste de l'apply : %v", err)
	}
	if len(report.Warnings) == 0 || !strings.Contains(strings.Join(report.Warnings, "\n"), "--force") {
		t.Errorf("le refus doit nommer --force : %v", report.Warnings)
	}
	if got, _ := os.ReadFile(stale); string(got) != string(retouche) {
		t.Fatal("le pool retouché a été retiré sans --force")
	}

	prov.Force = true
	plan, _ = planAvecObsoletes(t, prov, p)
	if !strings.Contains(plan.String(), "retrait forcé") {
		t.Fatalf("le plan doit annoncer le retrait forcé :\n%s", plan.String())
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("le pool devait être retiré sous --force : %v", err)
	}
	baks, _ := filepath.Glob(filepath.Join(paths.BackupsDir(), filepath.Base(stale)+".ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue dans %s, %d trouvée(s)", paths.BackupsDir(), len(baks))
	}
	if saved, _ := os.ReadFile(baks[0]); string(saved) != string(retouche) {
		t.Errorf("la sauvegarde doit porter le pool retouché :\n%s", saved)
	}
}

// Le pool d'origine d'un projet adopté ne porte pas la marque d'ogs tant que
// --force ne l'a pas repris. Après un « php switch », il restait actif dans
// l'ancienne version, sous le compte du projet, sans étape ni avertissement : il
// n'est toujours pas retiré — ogs ne l'a pas écrit —, mais il est signalé.
func TestUnPoolAdopteLaisseDansLAncienneVersionEstSignale(t *testing.T) {
	prov, paths, _ := fixture(t, "8.5", "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	p.Metadata.Labels = map[string]string{"adopted": "true"}
	origine := paths.PHPPoolFile("8.5", p.ID())
	corps := []byte("[shopfr]\nuser = shopfr\ngroup = shopfr\nlisten = /run/php/php8.5-fpm-shopfr.sock\n")
	if err := os.WriteFile(origine, corps, 0o644); err != nil {
		t.Fatal(err)
	}
	// Un pool d'un autre nom, posé à la main, reste hors de propos.
	if err := os.WriteFile(paths.PHPPoolFile("8.5", "shopfr_admin"), corps, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		prov.Force = force
		plan, obs := planAvecObsoletes(t, prov, p)
		if len(obs.Stale) != 0 || len(obs.Leftovers) != 1 || obs.Leftovers[0] != origine {
			t.Fatalf("--force=%v : le pool d'origine doit être un reste, et lui seul : %+v", force, obs)
		}
		report, err := engine.Execute(context.Background(), plan, engine.Options{})
		if err != nil {
			t.Fatalf("--force=%v : un reste signalé ne doit pas défaire l'apply : %v", force, err)
		}
		if !strings.Contains(strings.Join(report.Warnings, "\n"), origine) {
			t.Errorf("--force=%v : le reste doit être signalé : %v", force, report.Warnings)
		}
		if data, err := os.ReadFile(origine); err != nil || string(data) != string(corps) {
			t.Fatalf("--force=%v : le pool d'origine a été touché : %v", force, err)
		}
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

func TestPoolExposeSonCheminDeStatut(t *testing.T) {
	prov, _, _ := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	content := string(bp.Content)
	for _, want := range []string{
		"pm.status_path = " + config.PoolStatusPath,
		"ping.path = " + config.PoolPingPath,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("le pool doit exposer son état au vhost de supervision : %q absent", want)
		}
	}
	// Le chemin ne doit pas ressembler à un script : les vhosts de projet ne
	// routent vers FPM que les URI en « .php », ce qui le rend inatteignable
	// depuis un site.
	if strings.HasSuffix(config.PoolStatusPath, ".php") {
		t.Error("un chemin de statut en .php serait servi par les vhosts de projet")
	}
}

func TestSurchargeDuStatutResteAuProjet(t *testing.T) {
	// Le chemin de statut est un réglage de base : un projet doit pouvoir le
	// déplacer, comme n'importe quel autre.
	prov, _, _ := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{"pm.status_path": model.Value("/ailleurs")}
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bp.Content), "pm.status_path = /ailleurs") {
		t.Error("la surcharge du projet doit primer sur le réglage de base")
	}
}

func TestPlanEcrasePoolAvecForceEtSauvegarde(t *testing.T) {
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

	poolFile := paths.PHPPoolFile("8.5", "shopfr")
	data, _ := os.ReadFile(poolFile)
	if err := os.WriteFile(poolFile, append(data, []byte("\npm.max_children = 4\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	// --force rend l'étape mutante : le refus n'est plus une impasse.
	prov.Force = true
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
	if len(mutating) != 1 || !strings.Contains(mutating[0].Summary, "écrasement forcé") {
		t.Fatalf("le plan doit annoncer l'écrasement forcé :\n%s", plan2.String())
	}
	if _, err := engine.Execute(ctx, plan2, engine.Options{}); err != nil {
		t.Fatalf("l'écrasement forcé doit réussir : %v", err)
	}
	after, _ := os.ReadFile(poolFile)
	if strings.Contains(string(after), "pm.max_children = 4") {
		t.Error("le pool doit avoir été réécrit")
	}
	// La copie sort du répertoire des pools : PHP-FPM n'inclut que les « .conf »,
	// mais ogs y réinventorie ses pools et l'exploitant y cherche les siens.
	if restes, _ := filepath.Glob(poolFile + ".ogs-bak-*"); len(restes) != 0 {
		t.Errorf("aucune sauvegarde ne doit rester dans %s : %v", filepath.Dir(poolFile), restes)
	}
	baks, _ := filepath.Glob(filepath.Join(paths.BackupsDir(), filepath.Base(poolFile)+".ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue dans %s, %d trouvée(s)", paths.BackupsDir(), len(baks))
	}
	saved, _ := os.ReadFile(baks[0])
	if !strings.Contains(string(saved), "pm.max_children = 4") {
		t.Errorf("la sauvegarde doit contenir l'original :\n%s", saved)
	}
}

func TestCompensationRendLePoolPrecedent(t *testing.T) {
	// La compensation d'une mise à jour supprimait le fichier : sur un écrasement
	// forcé, le rollback détruisait donc ce que la sauvegarde venait de préserver,
	// et sur une simple mise à jour il retirait un pool qui servait.
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
	poolFile := paths.PHPPoolFile("8.5", "shopfr")
	avant, _ := os.ReadFile(poolFile)

	// Un changement quelconque rend le pool obsolète.
	p.Spec.PHP.Pools[0].Preset = "large"
	obs, _ = prov.Observe(ctx, p)
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	var step *engine.Step
	for _, s := range steps {
		if s.Action == engine.Update {
			step = s
		}
	}
	if step == nil {
		t.Fatal("une mise à jour du pool était attendue")
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err := step.Compensate(ctx); err != nil {
		t.Fatal(err)
	}
	après, err := os.ReadFile(poolFile)
	if err != nil {
		t.Fatalf("la compensation ne doit pas supprimer un pool qui servait : %v", err)
	}
	if string(après) != string(avant) {
		t.Error("la compensation doit rendre le pool précédent")
	}
}

// fpmRéel se déclare non simulé : Reload court-circuite sur un exécuteur simulé,
// et c'est justement le dialogue avec systemd qu'il faut éprouver.
type fpmRéel struct{ *exec.Recorder }

func (f *fpmRéel) Simulated() bool { return false }

// systemctlAppelés rend les appels à systemctl, arguments recollés.
func systemctlAppelés(rec *exec.Recorder) []string {
	var out []string
	for _, c := range rec.Calls() {
		if c.Name == "systemctl" {
			out = append(out, strings.Join(c.Args, " "))
		}
	}
	return out
}

// reloadFPM prépare une version 8.5, active ou non, avec ou sans pool, et
// rend les appels à systemctl que produit son rechargement.
func reloadFPM(t *testing.T, active, pool bool) ([]string, error) {
	t.Helper()
	fenêtreCourte(t)
	prov, paths, _ := fixture(t, "8.5")
	rec := exec.NewRecorder()
	if !active {
		rec.Errors["systemctl is-active"] = errors.New("inactive")
	}
	prov.Exec = &fpmRéel{rec}
	if pool {
		if err := fs.WriteAtomic(paths.PHPPoolFile("8.5", "shopfr"), []byte("[shopfr]\n"), 0o644, fs.KeepOwner); err != nil {
			t.Fatal(err)
		}
	}
	// Un fichier hors du masque de php-fpm.conf n'est pas un pool.
	if err := fs.WriteAtomic(paths.PHPPoolFile("8.5", "www")+".dist", []byte("[www]\n"), 0o644, fs.KeepOwner); err != nil {
		t.Fatal(err)
	}
	err := prov.Reload(context.Background(), "8.5")
	return systemctlAppelés(rec), err
}

func TestReloadRechargeUnFPMActif(t *testing.T) {
	calls, err := reloadFPM(t, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(calls, " | "), "reload php8.5-fpm") {
		t.Errorf("un FPM actif se recharge : %v", calls)
	}
}

// Sans le pool www.conf de la distribution, FPM ne démarre pas tant qu'aucun
// projet ne l'emploie : le premier pool doit le démarrer, et un reload échouait.
func TestReloadDémarreUnFPMArrêtéAuPremierPool(t *testing.T) {
	calls, err := reloadFPM(t, false, true)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, " | ")
	if strings.Contains(joined, "reload php8.5-fpm") {
		t.Errorf("un FPM arrêté ne se recharge pas : %v", calls)
	}
	if !strings.Contains(joined, "reset-failed php8.5-fpm | enable --now php8.5-fpm") {
		t.Errorf("le compteur d'échecs est acquitté, puis l'unité activée et démarrée : %v", calls)
	}
}

// Recharger un maître sans pool le tue : il s'arrête proprement, et ne doit
// pas repartir au prochain démarrage de la machine.
func TestReloadArrêteUnFPMSansPool(t *testing.T) {
	calls, err := reloadFPM(t, true, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, " | ")
	if strings.Contains(joined, "reload php8.5-fpm") || !strings.Contains(joined, "disable --now php8.5-fpm") {
		t.Errorf("un FPM sans pool est arrêté et désactivé, jamais rechargé : %v", calls)
	}
}

func TestReloadLaisseArrêtéUnFPMSansPool(t *testing.T) {
	calls, err := reloadFPM(t, false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "is-active") {
			t.Errorf("un FPM arrêté sans pool n'a rien à faire : %v", calls)
		}
	}
}

func TestReloadNommeLaCauseDUnDémarrageRefusé(t *testing.T) {
	prov, paths, _ := fixture(t, "8.5")
	rec := exec.NewRecorder()
	rec.Errors["systemctl is-active"] = errors.New("inactive")
	rec.Errors["systemctl enable"] = errors.New("code 1")
	prov.Exec = &fpmRéel{rec}
	if err := fs.WriteAtomic(paths.PHPPoolFile("8.5", "shopfr"), []byte("[shopfr]\n"), 0o644, fs.KeepOwner); err != nil {
		t.Fatal(err)
	}
	err := prov.Reload(context.Background(), "8.5")
	if err == nil || !strings.Contains(err.Error(), "démarrage de php8.5-fpm") {
		t.Errorf("l'échec doit nommer le démarrage de l'unité : %v", err)
	}
}
