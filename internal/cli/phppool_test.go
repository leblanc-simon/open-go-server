package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/spec"
)

func deuxSitesPHP(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-plain", "--php", "8.3", "--tls=false", "--yes")
	h.mustRunPartial("site", "create", "blog.fr", "--kind", "php-plain", "--php", "8.3", "--tls=false", "--yes")
	return h
}

func TestUnPoolInjecteEstRefuseAuProjetViseSansArreterLeParc(t *testing.T) {
	// Une valeur sur deux lignes écrivait une seconde directive dans le pool,
	// que FPM lit en root. Le refus porte sur le projet visé : au chargement, il
	// arrêterait tout le parc, « cert renew » du minuteur compris.
	h := deuxSitesPHP(t)
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.PHP.Pools[0].Settings = model.PoolSettings{"pm.max_children": model.Value("5\nuser = root")}
	})
	if out, code := h.run("plan", "blogfr"); code != CodeOK {
		t.Errorf("le voisin doit rester planifiable (code %d) :\n%s", code, out)
	}
	for _, args := range [][]string{{"plan", "shopfr"}, {"apply", "shopfr", "--yes"},
		{"plan", "-f", h.path("etc", "ogs", "projects", "shopfr.yaml")}} {
		out, code := h.run(args...)
		if code != CodeValidation || !strings.Contains(out, "spec.php.pools[web].settings.pm.max_children") {
			t.Errorf("« ogs %s » : code %d, attendu %d et la clé nommée :\n%s",
				strings.Join(args, " "), code, CodeValidation, out)
		}
	}
	if pool := h.read(t, filepath.Join("etc", "php", "8.3", "fpm", "pool.d", "shopfr.conf")); strings.Contains(pool, "user = root") {
		t.Errorf("directive injectée dans le pool :\n%s", pool)
	}
	out, _ := h.run("doctor")
	if !strings.Contains(out, "pools php") || !strings.Contains(out, "projet shopfr") {
		t.Errorf("doctor doit nommer la déclaration refusée :\n%s", out)
	}
}

func TestUneCleVerrouilleeEstEcarteeEnCode6(t *testing.T) {
	h := deuxSitesPHP(t)
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.PHP.Pools[0].Settings = model.PoolSettings{
			"user":            model.Value("root"),
			"pm.max_children": model.Value("7"),
		}
	})
	out, code := h.run("apply", "shopfr", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d :\n%s", code, CodePartial, out)
	}
	if !strings.Contains(out, "settings.user") {
		t.Errorf("la clé écartée doit être nommée :\n%s", out)
	}
	pool := h.read(t, filepath.Join("etc", "php", "8.3", "fpm", "pool.d", "shopfr.conf"))
	if strings.Contains(pool, "user = root") || !strings.Contains(pool, "user = shopfr") ||
		!strings.Contains(pool, "pm.max_children = 7") {
		t.Errorf("le pool doit être rendu sans la clé verrouillée, et avec le reste :\n%s", pool)
	}
}

func TestLesDefautsDeLaMachineSontVivants(t *testing.T) {
	// Contrairement au shell (D62), un changement de php.pool réécrit les pools
	// au prochain apply ; et un preset écrit dans la déclaration ne bascule pas.
	h := deuxSitesPHP(t)
	h.modifierDéclaration(t, "blogfr", func(p *model.Project) { p.Spec.PHP.Pools[0].Preset = "medium" })
	if decl := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfr.yaml")); strings.Contains(decl, "preset:") {
		t.Errorf("la création ne doit plus écrire de preset :\n%s", decl)
	}
	cfg := h.path("etc", "ogs", "config.yaml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	extra := "\nphp:\n  pool:\n    defaultPreset: small\n    defaults:\n      pm.max_requests: \"1000\"\n"
	if err := os.WriteFile(cfg, append(data, extra...), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustRunPartial("apply", "--yes")
	shop := h.read(t, filepath.Join("etc", "php", "8.3", "fpm", "pool.d", "shopfr.conf"))
	blog := h.read(t, filepath.Join("etc", "php", "8.3", "fpm", "pool.d", "blogfr.conf"))
	for nom, pool := range map[string]string{"shopfr": shop, "blogfr": blog} {
		if !strings.Contains(pool, "pm.max_requests = 1000") {
			t.Errorf("%s : le défaut de la machine n'a pas été appliqué :\n%s", nom, pool)
		}
	}
	if !strings.Contains(shop, "pm.max_children = 10") {
		t.Errorf("shopfr, sans preset déclaré, suit defaultPreset small :\n%s", shop)
	}
	if !strings.Contains(blog, "pm.max_children = 20") {
		t.Errorf("blogfr garde son preset déclaré, medium :\n%s", blog)
	}
	if out := h.mustRun("project", "show", "shopfr"); !strings.Contains(out, "small (défaut de la machine)") {
		t.Errorf("project show doit dire d'où vient le preset :\n%s", out)
	}
}

func TestLeCloneRebaseLesReglagesSansToucherLaSource(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-plain", "--php", "8.3", "--tls=false", "--yes")
	racine := h.path("home", "shop.fr")
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.PHP.Pools[0].Settings = model.PoolSettings{
			"php_admin_value[include_path]": model.Value(racine + "/lib"),
			"request_terminate_timeout":     nil,
		}
		p.Spec.PHP.Pools[0].AllowFunctions = []string{"exec"}
	})
	h.mustRunPartial("site", "clone", "shopfr", "--to", "prep", "--yes")
	clone, err := spec.Load(h.path("etc", "ogs", "projects", "shopfrprep.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	prepRacine := h.path("home", "shop.preprod-pp.net")
	s := clone.Spec.PHP.Pools[0].Settings
	if v, _ := s.Get("php_admin_value[include_path]"); v != prepRacine+"/lib" {
		t.Errorf("réglage du clone : %q, attendu sous %s", v, prepRacine)
	}
	if v, present := s["request_terminate_timeout"]; !present || v != nil {
		t.Error("la valeur nulle doit être reprise nulle par le clone")
	}
	source, err := spec.Load(h.path("etc", "ogs", "projects", "shopfr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := source.Spec.PHP.Pools[0].Settings.Get("php_admin_value[include_path]"); v != racine+"/lib" {
		t.Errorf("la déclaration de la source a été modifiée : %q", v)
	}
}

func TestDoctorCompareLaCapaciteDesPoolsALaMemoire(t *testing.T) {
	h := deuxSitesPHP(t)
	// Sans meminfo simulé, le contrôle se tait.
	if out, _ := h.run("doctor"); strings.Contains(out, "capacité php") {
		t.Errorf("sans meminfo, rien à comparer :\n%s", out)
	}
	if err := os.MkdirAll(h.path("proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Deux pools ondemand : 2 × 12 × 256 Mio = 6 Gio, pour 2 Gio (300 %).
	if err := os.WriteFile(h.path("proc", "meminfo"), []byte("MemTotal:        2097152 kB\nMemFree: 1 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := h.run("doctor")
	if !strings.Contains(out, "capacité php 8.3") || !strings.Contains(out, "6,0 Gio") {
		t.Errorf("la capacité doit être comparée à la mémoire :\n%s", out)
	}
	if !regexp.MustCompile(`⚠\s+capacité php 8.3`).MatchString(out) {
		t.Errorf("au-delà de la mémoire, un avertissement :\n%s", out)
	}
}

func TestCapaciteDesPools(t *testing.T) {
	loads := []poolLoad{
		{Project: "a", Pool: "web", Version: "8.3", Settings: map[string]string{
			"pm.max_children": "10", "php_admin_value[memory_limit]": "256M"}},
		{Project: "b", Pool: "web", Version: "8.3", Settings: map[string]string{
			"pm.max_children": "4", "php_value[memory_limit]": "1G"}},
		{Project: "c", Pool: "web", Version: "8.3", Settings: map[string]string{
			"pm.max_children": "4", "php_admin_value[memory_limit]": "-1"}},
		{Project: "d", Pool: "web", Version: "7.4", Settings: map[string]string{"pm.max_children": "2"}},
		{Project: "e", Pool: "web", Version: "7.4", Settings: map[string]string{
			"pm.max_children": "9", "php_admin_value[memory_limit]": "0"}},
	}
	got := capacityChecks(loads, 8<<30, 200)
	var joined []string
	for _, f := range got {
		joined = append(joined, f.name+"|"+f.status+"|"+f.detail)
	}
	all := strings.Join(joined, "\n")
	for _, want := range []string{
		"capacité php 8.3|ok|2 pool(s) : au pire 6,5 Gio", // 2,5 + 4
		"capacité php 8.3|avertissement|memory_limit illimité (-1), capacité non bornée : c/web",
		"capacité php 7.4|ok|1 pool(s) : au pire 0,2 Gio", // 2 × 128M, défaut de PHP
		"capacité php|ok|toutes versions : au pire 6,8 Gio",
		"capacité php 7.4|avertissement|non comptés : e/web (memory_limit 0, que PHP refuse)",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("%q absent :\n%s", want, all)
		}
	}
	// Seuil : information jusqu'à 200 % de la mémoire, avertissement au-delà ;
	// réglable, et 0 ou moins ne l'avertit jamais.
	one := []poolLoad{{Project: "a", Pool: "web", Version: "8.3", Settings: map[string]string{
		"pm.max_children": "1", "php_admin_value[memory_limit]": "1990M"}}}
	statut := func(memMiB int64, seuil int) string {
		t.Helper()
		return capacityChecks(one, memMiB<<20, seuil)[0].status
	}
	if got := statut(1000, 200); got != "ok" {
		t.Errorf("199 %% : information attendue, obtenu %s", got)
	}
	one[0].Settings["php_admin_value[memory_limit]"] = "2010M"
	if got := statut(1000, 200); got != "avertissement" {
		t.Errorf("201 %% : avertissement attendu, obtenu %s", got)
	}
	if got := statut(1000, 250); got != "ok" {
		t.Errorf("seuil réglé à 250 %% : information attendue, obtenu %s", got)
	}
	if got := statut(1000, 0); got != "ok" {
		t.Errorf("seuil à 0 : jamais d'avertissement, obtenu %s", got)
	}
	for in, want := range map[string]int64{"128M": 128 << 20, "2g": 2 << 30, "512K": 512 << 10, "1024": 1024} {
		if n, unl, ok := parseMemoryLimit(in); !ok || unl || n != want {
			t.Errorf("%s → %d %v %v", in, n, unl, ok)
		}
	}
	if _, unl, ok := parseMemoryLimit("-1"); !ok || !unl {
		t.Error("-1 est illimité")
	}
	if _, _, ok := parseMemoryLimit("beaucoup"); ok {
		t.Error("une valeur illisible n'est pas comptée")
	}
}

// D101 × D103 : les réglages écartés ou sans effet d'un pool — clé verrouillée,
// allowFunctions qui ne rouvre rien, valeur nulle sans rien à retirer — sont
// annoncés une fois, par warnf : sur la sortie d'erreur sous -q et -o json, dans
// le fil de la sortie en mode table, jamais en double.
func TestLesReglagesEcartesDUnPoolSAnnoncentUneFois(t *testing.T) {
	for _, opts := range [][]string{{}, {"-q"}, {"-o", "json"}} {
		h := deuxSitesPHP(t)
		h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
			p.Spec.PHP.Pools[0].Settings = model.PoolSettings{
				"user":                  model.Value("root"),
				"php_value[inexistant]": nil,
			}
			p.Spec.PHP.Pools[0].AllowFunctions = []string{"strlen"}
		})
		stdout, stderr, code := h.runSplit(append([]string{"apply", "shopfr", "--yes"}, opts...)...)
		if code != CodePartial {
			t.Fatalf("%v : code %d, attendu %d\n%s%s", opts, code, CodePartial, stdout, stderr)
		}
		visible, tue := stderr, stdout
		if len(opts) == 0 {
			visible, tue = stdout, ""
		}
		// Le plan, affiché en mode table, la liste aussi : seule compte l'annonce
		// de l'étape en échec.
		if n := len(regexp.MustCompile(`(?m)^\s*⚠ pool php .*réglage\(s\) à revoir`).FindAllString(visible, -1)); n != 1 {
			t.Errorf("%v : l'étape doit s'annoncer une fois, %d fois :\n%s", opts, n, visible)
		}
		for _, mot := range []string{"settings.user", "rien à rouvrir", "rien à retirer"} {
			if !strings.Contains(visible, mot) {
				t.Errorf("%v : « %s » doit être nommé :\n%s", opts, mot, visible)
			}
		}
		if strings.Contains(tue, "⚠") {
			t.Errorf("%v : l'avertissement ne doit pas polluer la sortie standard :\n%s", opts, tue)
		}
	}
}
