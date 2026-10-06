package php

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

// ligne rend la valeur d'une directive du pool rendu, et faux si elle n'y est pas.
func ligne(t *testing.T, content, key string) (string, bool) {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + ` = (.*)$`)
	m := re.FindAllStringSubmatch(content, -1)
	if len(m) > 1 {
		t.Fatalf("%s rendu %d fois :\n%s", key, len(m), content)
	}
	if m == nil {
		return "", false
	}
	return m[0][1], true
}

func cfgAvec(edit func(*config.Config)) *config.Config {
	c := config.Default()
	edit(c)
	return c
}

func TestLesCouchesDuPoolSEmpilentDansLOrdre(t *testing.T) {
	prov, _, _ := fixture(t, "8.3")
	prov.Config = cfgAvec(func(c *config.Config) {
		c.PHP.Pool.Defaults = model.PoolSettings{
			"pm.max_requests":               model.Value("1000"), // socle : 500
			"request_slowlog_timeout":       model.Value("5s"),   // socle : 10s
			"pm.max_children":               model.Value("3"),    // redéfini par le preset
			"php_admin_value[sys_temp_dir]": nil,                 // retiré du socle
		}
	})
	p := project(t, "boutique.fr", model.KindPrestashop, "", "8.3")
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{
		"request_terminate_timeout":       nil,                 // retiré : socle
		"php_admin_value[max_input_vars]": model.Value("9000"), // type : 5000
		"php_value[date.timezone]":        model.Value(""),     // chaîne vide : une valeur
	}
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	c := string(bp.Content)
	for key, want := range map[string]string{
		"pm.max_requests":                 "1000", // machine sur socle
		"request_slowlog_timeout":         "5s",
		"pm.max_children":                 "12", // preset sur machine
		"pm":                              "ondemand",
		"php_admin_value[memory_limit]":   "512M", // type sur preset
		"php_admin_value[max_input_vars]": "9000", // projet sur type
		"php_value[date.timezone]":        `""`,
		"security.limit_extensions":       ".php",
	} {
		if got, ok := ligne(t, c, key); !ok || got != want {
			t.Errorf("%s = %q (présent : %v), attendu %q", key, got, ok, want)
		}
	}
	for _, key := range []string{"php_admin_value[sys_temp_dir]", "request_terminate_timeout"} {
		if _, ok := ligne(t, c, key); ok {
			t.Errorf("%s, retiré par une valeur nulle, est encore rendu :\n%s", key, c)
		}
	}
}

func TestUneCleVerrouilleeNeSeSurchargeJamais(t *testing.T) {
	// Avant D103, « user: root » rendait une seconde ligne user après celle du
	// template : FPM retient la dernière, et le pool tournait en root.
	prov, _, _ := fixture(t, "8.3")
	prov.Config = cfgAvec(func(c *config.Config) {
		c.PHP.Pool.Defaults = model.PoolSettings{"slowlog": model.Value("/etc/cron.d/x")}
	})
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{
		"user":                       model.Value("root"),
		"listen.mode":                model.Value("0666"),
		"access.log":                 model.Value("/etc/passwd"),
		"php_admin_value[error_log]": nil,
		"pm.max_children":            model.Value("7"),
	}
	steps, err := prov.Plan(context.Background(), p, Observed{})
	if err != nil {
		t.Fatalf("une clé verrouillée ne doit pas refuser le plan : %v", err)
	}
	bp, _ := prov.Build(p, p.Spec.PHP.Pools[0])
	c := string(bp.Content)
	if got, _ := ligne(t, c, "user"); got != p.Spec.User.Name {
		t.Errorf("user = %q, attendu %q", got, p.Spec.User.Name)
	}
	if got, _ := ligne(t, c, "listen.mode"); got != "0660" {
		t.Errorf("listen.mode = %q, attendu 0660", got)
	}
	if _, ok := ligne(t, c, "access.log"); ok {
		t.Error("access.log déclaré par le projet a été rendu")
	}
	if got, _ := ligne(t, c, "slowlog"); !strings.HasPrefix(got, p.Spec.Layout.LogDir) {
		t.Errorf("slowlog = %q, attendu sous %s", got, p.Spec.Layout.LogDir)
	}
	if got, _ := ligne(t, c, "php_admin_value[error_log]"); !strings.HasPrefix(got, p.Spec.Layout.LogDir) {
		t.Errorf("error_log retiré ou déplacé : %q", got)
	}
	if got, _ := ligne(t, c, "pm.max_children"); got != "7" {
		t.Errorf("le reste du pool doit être rendu : pm.max_children = %q", got)
	}
	var avert *struct{}
	for _, s := range steps {
		if strings.HasSuffix(s.Resource, "/settings") {
			if s.Critical {
				t.Error("l'étape qui nomme les clés écartées doit être non critique")
			}
			for _, want := range []string{"settings.user", "settings.listen.mode", "settings.access.log",
				"php.pool.defaults.slowlog", "settings.php_admin_value[error_log]"} {
				if !strings.Contains(s.Summary, want) {
					t.Errorf("l'étape doit nommer %s : %s", want, s.Summary)
				}
			}
			if s.Apply(context.Background()) == nil {
				t.Error("l'étape doit échouer à l'application, pour compter dans le code 6")
			}
			avert = &struct{}{}
		}
	}
	if avert == nil {
		t.Error("aucune étape ne nomme les clés écartées")
	}
}

func TestUneValeurSurPlusieursLignesEstRefuseeAuPlan(t *testing.T) {
	// Avant D103, la valeur était rendue telle quelle : la seconde ligne devenait
	// une directive du pool, que FPM lit en root.
	cas := map[string]func(*model.Pool){
		"valeur": func(pool *model.Pool) {
			pool.Settings = model.PoolSettings{"pm.max_children": model.Value("5\nuser = root")}
		},
		"clé": func(pool *model.Pool) {
			pool.Settings = model.PoolSettings{"php_value[x]\nuser": model.Value("root")}
		},
		"clé hors grammaire": func(pool *model.Pool) {
			pool.Settings = model.PoolSettings{"php_value[x] = 1 ; ": model.Value("2")}
		},
		"env":      func(pool *model.Pool) { pool.Env = map[string]string{"A": "b\r\nuser = root"} },
		"fonction": func(pool *model.Pool) { pool.AllowFunctions = []string{"exec,system"} },
	}
	for nom, edit := range cas {
		t.Run(nom, func(t *testing.T) {
			prov, _, _ := fixture(t, "8.3")
			p := project(t, "shop.fr", model.KindPHPPlain, "", "8.3")
			edit(&p.Spec.PHP.Pools[0])
			_, err := prov.Plan(context.Background(), p, Observed{})
			var refus *DeclarationError
			if !errors.As(err, &refus) {
				t.Fatalf("le plan doit refuser la déclaration, obtenu %v", err)
			}
			// Le calcul, lui, ne l'est pas : le vhost de supervision passe par lui
			// pour tout le parc. Et rien d'injecté n'atteint le rendu.
			bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
			if err != nil {
				t.Fatal(err)
			}
			if regexp.MustCompile(`(?m)^user = root`).Match(bp.Content) {
				t.Errorf("directive injectée dans le pool :\n%s", bp.Content)
			}
		})
	}
}

func TestDisableFunctionsAuSeulTypePHPPlain(t *testing.T) {
	prov, _, _ := fixture(t, "8.3")
	plain := project(t, "plain.fr", model.KindPHPPlain, "", "8.3")
	bp, _ := prov.Build(plain, plain.Spec.PHP.Pools[0])
	got, ok := ligne(t, string(bp.Content), DisableFunctionsKey)
	if !ok || got != "exec,passthru,shell_exec,system,proc_open,popen,pcntl_exec" {
		t.Errorf("php-plain : disable_functions = %q (%v)", got, ok)
	}
	sf := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	bp, _ = prov.Build(sf, sf.Spec.PHP.Pools[0])
	if _, ok := ligne(t, string(bp.Content), DisableFunctionsKey); ok {
		t.Error("Symfony ne reçoit pas disable_functions par défaut")
	}

	// allowFunctions rouvre sans recopier la liste ; la casse ne compte pas.
	plain.Spec.PHP.Pools[0].AllowFunctions = []string{"Exec", "popen"}
	bp, _ = prov.Build(plain, plain.Spec.PHP.Pools[0])
	if got, _ := ligne(t, string(bp.Content), DisableFunctionsKey); got != "passthru,shell_exec,system,proc_open,pcntl_exec" {
		t.Errorf("après allowFunctions : %q", got)
	}
	// Tout rouvert : la directive disparaît.
	plain.Spec.PHP.Pools[0].AllowFunctions = config.DefaultDisableFunctions
	bp, _ = prov.Build(plain, plain.Spec.PHP.Pools[0])
	if _, ok := ligne(t, string(bp.Content), DisableFunctionsKey); ok {
		t.Error("une liste entièrement rouverte ne doit pas être rendue vide")
	}

	// La machine fixe la liste ; vide, rien n'est posé.
	plain.Spec.PHP.Pools[0].AllowFunctions = nil
	liste := []string{"exec"}
	prov.Config = cfgAvec(func(c *config.Config) { c.PHP.Pool.DisableFunctions = &liste })
	bp, _ = prov.Build(plain, plain.Spec.PHP.Pools[0])
	if got, _ := ligne(t, string(bp.Content), DisableFunctionsKey); got != "exec" {
		t.Errorf("liste de la machine : %q", got)
	}
	vide := []string{}
	prov.Config = cfgAvec(func(c *config.Config) { c.PHP.Pool.DisableFunctions = &vide })
	bp, _ = prov.Build(plain, plain.Spec.PHP.Pools[0])
	if _, ok := ligne(t, string(bp.Content), DisableFunctionsKey); ok {
		t.Error("une liste vide ne pose rien")
	}

	// Les défauts de la machine l'étendent à tout type, et allowFunctions s'y
	// applique aussi.
	prov.Config = cfgAvec(func(c *config.Config) {
		c.PHP.Pool.Defaults = model.PoolSettings{DisableFunctionsKey: model.Value("exec,system")}
	})
	sf.Spec.PHP.Pools[0].AllowFunctions = []string{"system"}
	bp, _ = prov.Build(sf, sf.Spec.PHP.Pools[0])
	if got, _ := ligne(t, string(bp.Content), DisableFunctionsKey); got != "exec" {
		t.Errorf("défaut de la machine, puis allowFunctions : %q", got)
	}
}

func TestPresetDeclareOuHerite(t *testing.T) {
	prov, _, _ := fixture(t, "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	children := func(p *model.Project) string {
		t.Helper()
		bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
		if err != nil {
			t.Fatal(err)
		}
		v, _ := ligne(t, string(bp.Content), "pm.max_children")
		return v
	}
	if got := children(p); got != "12" {
		t.Errorf("hérité, livré : ondemand attendu (12), obtenu %s", got)
	}
	// Un preset écrit dans la déclaration — celui des déclarations antérieures
	// à D103 — est gardé.
	p.Spec.PHP.Pools[0].Preset = "medium"
	if got := children(p); got != "20" {
		t.Errorf("déclaré medium : 20 attendu, obtenu %s", got)
	}
	// La machine change le défaut : seul le pool hérité le suit.
	prov.Config = cfgAvec(func(c *config.Config) { c.PHP.Pool.DefaultPreset = "large" })
	if got := children(p); got != "20" {
		t.Errorf("le preset déclaré ne suit pas la machine : %s", got)
	}
	p.Spec.PHP.Pools[0].Preset = ""
	if got := children(p); got != "50" {
		t.Errorf("hérité, machine large : 50 attendu, obtenu %s", got)
	}

	// En préproduction, le défaut hérité descend d'un cran ; le déclaré jamais.
	prep := model.New("shop.fr", naming.EnvPrep, model.KindSymfony)
	prep.Spec.App.Version = "6.4"
	if err := prep.Normalize(t.TempDir(), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	prov.Config = config.Default()
	if got := children(prep); got != "5" {
		t.Errorf("préproduction, ondemand descendu en micro : 5 attendu, obtenu %s", got)
	}
	prep.Spec.PHP.Pools[0].Preset = "medium"
	if got := children(prep); got != "20" {
		t.Errorf("préproduction, preset déclaré : 20 attendu, obtenu %s", got)
	}
	prep.Spec.PHP.Pools[0].Preset = ""
	prov.Config = cfgAvec(func(c *config.Config) {
		e := c.Environments["prep"]
		e.PresetDowngrade = false
		c.Environments["prep"] = e
	})
	if got := children(prep); got != "12" {
		t.Errorf("presetDowngrade: false : 12 attendu, obtenu %s", got)
	}
}

func TestPresetsDeLaMachine(t *testing.T) {
	prov, _, _ := fixture(t, "8.3")
	prov.Config = cfgAvec(func(c *config.Config) {
		c.PHP.Pool.Presets = map[string]model.PoolSettings{
			// Redéfini clé par clé : le reste du preset livré demeure.
			"medium": {"pm.max_children": model.Value("30"), "pm.start_servers": nil},
			"maison": {"pm": model.Value("static"), "pm.max_children": model.Value("4")},
		}
		c.PHP.Pool.DefaultPreset = "maison"
	})
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	bp, _ := prov.Build(p, p.Spec.PHP.Pools[0])
	if got, _ := ligne(t, string(bp.Content), "pm"); got != "static" {
		t.Errorf("preset ajouté par la machine, défaut : pm = %q", got)
	}
	p.Spec.PHP.Pools[0].Preset = "medium"
	bp, _ = prov.Build(p, p.Spec.PHP.Pools[0])
	c := string(bp.Content)
	if got, _ := ligne(t, c, "pm.max_children"); got != "30" {
		t.Errorf("medium redéfini : pm.max_children = %q", got)
	}
	if got, _ := ligne(t, c, "pm.min_spare_servers"); got != "2" {
		t.Errorf("le reste de medium doit demeurer : pm.min_spare_servers = %q", got)
	}
	if _, ok := ligne(t, c, "pm.start_servers"); ok {
		t.Error("pm.start_servers, retiré par la machine, est encore rendu")
	}
}

func TestPresetInconnu(t *testing.T) {
	prov, _, _ := fixture(t, "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	// Déclaré : le plan du projet le refuse.
	p.Spec.PHP.Pools[0].Preset = "introuvable"
	var refus *DeclarationError
	if _, err := prov.Plan(context.Background(), p, Observed{}); !errors.As(err, &refus) ||
		!strings.Contains(err.Error(), "introuvable") {
		t.Errorf("un preset déclaré inconnu doit être refusé au plan : %v", err)
	}
	// Défaut de la machine inconnu : ondemand retenu, étape non critique.
	p.Spec.PHP.Pools[0].Preset = ""
	prov.Config = cfgAvec(func(c *config.Config) { c.PHP.Pool.DefaultPreset = "introuvable" })
	steps, err := prov.Plan(context.Background(), p, Observed{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range steps {
		if strings.HasSuffix(s.Resource, "/settings") && strings.Contains(s.Summary, "defaultPreset") && !s.Critical {
			found = true
		}
	}
	if !found {
		t.Error("un defaultPreset inconnu doit être nommé par une étape non critique")
	}
	bp, _ := prov.Build(p, p.Spec.PHP.Pools[0])
	if got, _ := ligne(t, string(bp.Content), "pm.max_children"); got != "12" {
		t.Errorf("repli sur ondemand attendu : %q", got)
	}
}

func TestConfigProblems(t *testing.T) {
	if pbs := ConfigProblems(config.Default()); len(pbs) != 0 {
		t.Errorf("la configuration livrée ne porte aucun problème : %v", pbs)
	}
	bad := []string{"exec", "a b"}
	c := cfgAvec(func(c *config.Config) {
		c.PHP.Pool.Defaults = model.PoolSettings{"user": model.Value("x"), "a\nb": model.Value("1"),
			"pm.max_requests": model.Value("1\n2")}
		c.PHP.Pool.Presets = map[string]model.PoolSettings{"Mal Nommé": {}}
		c.PHP.Pool.DefaultPreset = "absent"
		c.PHP.Pool.DisableFunctions = &bad
	})
	var all []string
	for _, pb := range ConfigProblems(c) {
		all = append(all, pb.String())
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{"defaults.user", `"a\nb"`, "defaults.pm.max_requests", "Mal Nommé",
		"defaultPreset", `"a b"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s non nommé :\n%s", want, joined)
		}
	}
}

func TestLeVerrouNeSeContournePasParLaCasse(t *testing.T) {
	// FPM compare les noms de directives sans casse : « User = root » après
	// « user = alice » passe « php-fpm -t » et fait tourner le pool en root.
	prov, _, _ := fixture(t, "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{
		"User":                       model.Value("root"),
		"Group":                      model.Value("root"),
		"user":                       model.Value("root"),
		"Listen.Owner":               model.Value("nobody"),
		"LISTEN.owner":               model.Value("nobody"),
		"PM.max_children":            model.Value("7"),
		"pm.max_children":            model.Value("40"),
		"PHP_ADMIN_VALUE[error_log]": model.Value("/etc/x"),
		"Include":                    model.Value("/tmp/evil.conf"),
		"prefix":                     model.Value("/tmp"),
		"listen.backlog":             model.Value("64"),
	}
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	c := string(bp.Content)
	for _, re := range []string{`(?mi)^user = root`, `(?mi)^group = root`, `(?mi)^listen\.owner = nobody`,
		`(?mi)^include`, `(?mi)^prefix`, `(?mi)^php_admin_value\[error_log\] = /etc/x`} {
		if regexp.MustCompile(re).MatchString(c) {
			t.Errorf("%s rendu malgré le verrou :\n%s", re, c)
		}
	}
	if n := len(regexp.MustCompile(`(?mi)^pm\.max_children = `).FindAllString(c, -1)); n != 1 {
		t.Errorf("pm.max_children rendu %d fois, attendu une :\n%s", n, c)
	}
	if got, _ := ligne(t, c, "listen.backlog"); got != "64" {
		t.Errorf("listen.backlog reste libre : %q", got)
	}
	joined := ""
	for _, pb := range bp.Resolved.Warnings() {
		joined += pb.String() + "\n"
	}
	for _, want := range []string{"settings.User", "settings.Listen.Owner", "settings.Include", "settings.prefix",
		"à la casse près"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q non nommé :\n%s", want, joined)
		}
	}
}

func TestLesPointsDEtatNeSeRetirentPas(t *testing.T) {
	prov, _, _ := fixture(t, "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{"pm.status_path": nil, "ping.path": model.Value("/autre-ping")}
	bp, _ := prov.Build(p, p.Spec.PHP.Pools[0])
	c := string(bp.Content)
	if got, _ := ligne(t, c, "pm.status_path"); got != config.PoolStatusPath {
		t.Errorf("pm.status_path retiré : %q", got)
	}
	if got, _ := ligne(t, c, "ping.path"); got != "/autre-ping" {
		t.Errorf("une autre valeur reste admise : %q", got)
	}
	if len(bp.Resolved.Warnings()) != 1 {
		t.Errorf("le retrait refusé doit être nommé : %v", bp.Resolved.Warnings())
	}
}

func TestLesValeursSontCiteesQuandLAnalyseurLesLiraitAutrement(t *testing.T) {
	prov, _, _ := fixture(t, "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{
		"php_value[include_path]":    model.Value(".:/a;/b"),
		"php_value[user_agent]":      model.Value("${HOME}x"),
		"php_value[error_reporting]": model.Value("E_ALL & ~E_DEPRECATED"),
	}
	p.Spec.PHP.Pools[0].Env = map[string]string{"DSN": "a;b"}
	bp, _ := prov.Build(p, p.Spec.PHP.Pools[0])
	c := string(bp.Content)
	for _, want := range []string{`php_value[include_path] = ".:/a;/b"`, `php_value[user_agent] = "$""{HOME}x"`,
		"php_value[error_reporting] = E_ALL & ~E_DEPRECATED", `env[DSN] = "a;b"`} {
		if !strings.Contains(c, want) {
			t.Errorf("%q absent :\n%s", want, c)
		}
	}
	// Une valeur déjà citée par l'exploitant garde son rendu (D103) ; toute autre
	// valeur que l'analyseur lirait autrement est citée, et parvient telle
	// quelle à FPM (D115) — main en refusait, ou la corrompait en silence.
	cases := []struct{ value, render string }{
		{`"/usr/sbin/sendmail -t -i"`, `"/usr/sbin/sendmail -t -i"`},
		{`""`, `""`},
		{`'x'`, `'x'`},
		{`"a" "b"`, `"a" "b"`}, // FPM colle : ab
		{`"a\"b"`, `"a\"b"`},   // FPM lit a"b
		{`"a;b"`, `"a;b"`},
		{`a"`, `"a\""`},
		{`.:/a;/b`, `".:/a;/b"`},
		{`a";b`, `"a\";b"`},
		{`"x" ${HOME}`, `"\"x\" $""{HOME}"`},
		{`l'a;b`, `"l'a;b"`},
		{`l'a`, `"l'a"`},
		{`a & b`, `"a & b"`},
		{`a=b`, `"a=b"`},
		{`no`, `"no"`},
		{`$HOME`, `"$HOME"`},
		{``, `""`},
	}
	for _, tc := range cases {
		p.Spec.PHP.Pools[0].Settings = model.PoolSettings{"php_value[user_agent]": model.Value(tc.value)}
		p.Spec.PHP.Pools[0].Env = nil
		if _, err := prov.Plan(context.Background(), p, Observed{}); err != nil {
			t.Errorf("%s : refusée à tort : %v", tc.value, err)
		}
		bp, _ = prov.Build(p, p.Spec.PHP.Pools[0])
		got, _ := ligne(t, string(bp.Content), "php_value[user_agent]")
		if got != tc.render {
			t.Errorf("%s : rendu %s, attendu %s", tc.value, got, tc.render)
		}
	}
	// Un mot-clé reste nu là où il est un booléen.
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{"php_flag[display_errors]": model.Value("off"),
		"catch_workers_output": model.Value("no")}
	bp, _ = prov.Build(p, p.Spec.PHP.Pools[0])
	for _, want := range []string{"php_flag[display_errors] = off", "catch_workers_output = no"} {
		if !strings.Contains(string(bp.Content), want) {
			t.Errorf("%q absent :\n%s", want, bp.Content)
		}
	}
	// Une variable est toujours citée : « no » arrivait vide — FPM refusait même
	// le pool —, « E_ALL » valait 32767.
	p.Spec.PHP.Pools[0].Settings = nil
	p.Spec.PHP.Pools[0].Env = map[string]string{"A": "no", "B": "E_ALL", "C": "l'a", "D": "x$y"}
	bp, _ = prov.Build(p, p.Spec.PHP.Pools[0])
	for _, want := range []string{"env[APP_ENV] = prod", `env[A] = "no"`, `env[B] = "E_ALL"`, `env[C] = "l'a"`, `env[D] = "x$y"`} {
		if !strings.Contains(string(bp.Content), want) {
			t.Errorf("%q absent :\n%s", want, bp.Content)
		}
	}
	// « $… » en tête d'une variable est la syntaxe de FPM pour relayer celle de
	// l'environnement de son maître (www.conf de Debian : env[HOSTNAME] =
	// $HOSTNAME) : elle passe le plan et garde ce sens, citée ou non (D115).
	for _, v := range []string{`$HOSTNAME`, `"$x"`} {
		p.Spec.PHP.Pools[0].Env = map[string]string{"HN": v}
		if _, err := prov.Plan(context.Background(), p, Observed{}); err != nil {
			t.Errorf("env %q refusée : %v", v, err)
		}
	}
	p.Spec.PHP.Pools[0].Env = map[string]string{"HN": "$HOSTNAME"}
	bp, _ = prov.Build(p, p.Spec.PHP.Pools[0])
	if !strings.Contains(string(bp.Content), `env[HN] = "$HOSTNAME"`) {
		t.Errorf("env[HN] = $HOSTNAME doit être rendue pour FPM :\n%s", bp.Content)
	}
	p.Spec.PHP.Pools[0].Env = nil
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{"env[PASS]": model.Value("$x")}
	if _, err := prov.Plan(context.Background(), p, Observed{}); err != nil {
		t.Errorf("env[] des réglages commençant par « $ » refusée : %v", err)
	}
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{"php_value[x]": model.Value("$x")}
	if _, err := prov.Plan(context.Background(), p, Observed{}); err != nil {
		t.Errorf("« $ » en tête d'une valeur d'ini se cite : %v", err)
	}
}

func TestAllowFunctionsSansEffetEstNomme(t *testing.T) {
	prov, paths, _ := fixture(t, "8.3")
	ini := paths.PHPRoot() + "/8.3/fpm/php.ini"
	if err := os.MkdirAll(filepath.Dir(ini), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ini, []byte("disable_functions = phpinfo,getmypid ; Debian\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sf := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	sf.Spec.PHP.Pools[0].AllowFunctions = []string{"exec", "getmypid"}
	bp, _ := prov.Build(sf, sf.Spec.PHP.Pools[0])
	joined := ""
	for _, pb := range bp.Resolved.Warnings() {
		joined += pb.String() + "\n"
	}
	for _, want := range []string{"exec n'est pas dans le disable_functions du pool", "getmypid est fermée par le php.ini"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q non nommé :\n%s", want, joined)
		}
	}
}

func TestUneValeurNulleSansCleHeriteeEstNommee(t *testing.T) {
	// Avant D103, « php_value[x]: » sans valeur valait une chaîne vide ; c'est
	// désormais un retrait, et le plan le dit quand il n'y a rien à retirer.
	prov, _, _ := fixture(t, "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{"php_value[x]": nil, "request_terminate_timeout": nil}
	bp, _ := prov.Build(p, p.Spec.PHP.Pools[0])
	w := bp.Resolved.Warnings()
	if len(w) != 1 || !strings.Contains(w[0].String(), "php_value[x]") {
		t.Errorf("seule la clé sans héritage doit être nommée : %v", w)
	}
}

// D115 : les sessions quittent le home pour une chaîne root, que
// systemd-tmpfiles purge sans suivre de lien. Un open_basedir déclaré, écrit
// avant, ne connaît pas ce répertoire : il y est ajouté, sans quoi PHP refusait
// d'ouvrir la moindre session.
func TestLesSessionsViventHorsDuHomeEtRestentAtteignables(t *testing.T) {
	prov, paths, _ := fixture(t, "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	dir := paths.ProjectSessionsDir("shopfr")
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	c := string(bp.Content)
	if got, _ := ligne(t, c, "php_admin_value[session.save_path]"); got != dir {
		t.Errorf("save_path %q, attendu %q", got, dir)
	}
	if got, _ := ligne(t, c, "php_admin_value[open_basedir]"); !strings.HasSuffix(got, ":"+dir) {
		t.Errorf("open_basedir %q ne couvre pas %s", got, dir)
	}
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{
		"php_admin_value[open_basedir]": model.Value(p.Spec.Layout.Root + ":/usr/share/fonts")}
	bp, _ = prov.Build(p, p.Spec.PHP.Pools[0])
	if got, _ := ligne(t, string(bp.Content), "php_admin_value[open_basedir]"); got !=
		`"`+p.Spec.Layout.Root+":/usr/share/fonts:"+dir+`"` && got != p.Spec.Layout.Root+":/usr/share/fonts:"+dir {
		t.Errorf("open_basedir déclaré : %q", got)
	}
	// Cité par l'exploitant, il est lu comme FPM le lit, puis complété.
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{
		"php_admin_value[open_basedir]": model.Value(`"` + p.Spec.Layout.Root + `:/opt/lib"`)}
	bp, _ = prov.Build(p, p.Spec.PHP.Pools[0])
	if got, _ := ligne(t, string(bp.Content), "php_admin_value[open_basedir]"); got != p.Spec.Layout.Root+":/opt/lib:"+dir {
		t.Errorf("open_basedir cité : %q", got)
	}
	// Un save_path déclaré ailleurs n'appelle rien.
	p.Spec.PHP.Pools[0].Settings["php_admin_value[session.save_path]"] = model.Value("/srv/sessions")
	bp, _ = prov.Build(p, p.Spec.PHP.Pools[0])
	if got, _ := ligne(t, string(bp.Content), "php_admin_value[open_basedir]"); strings.Contains(got, dir) {
		t.Errorf("open_basedir modifié sans objet : %q", got)
	}
}

// Un php_value que le php_admin_value du socle rend sans effet est noté (D115),
// pour doctor : php_value[session.save_path] laissait les sessions dans le
// répertoire d'ogs sans un mot.
func TestUnPHPValueMasqueParLAdministrationEstNomme(t *testing.T) {
	prov, _, _ := fixture(t, "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	p.Spec.PHP.Pools[0].Settings = model.PoolSettings{"php_value[session.save_path]": model.Value("/srv/s")}
	bp, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pb := range bp.Resolved.Notes {
		found = found || strings.Contains(pb.String(), "php_admin_value[session.save_path] l'emporte")
	}
	// Une note, jamais un problème : une déclaration ancienne rendrait sinon le
	// code 6 à chaque apply.
	if !found || len(bp.Resolved.Problems) > 0 {
		t.Errorf("php_value masqué : notes %v, problèmes %v", bp.Resolved.Notes, bp.Resolved.Problems)
	}
}

// Un répertoire de sessions retiré à la main est recréé à l'apply, pools
// conformes ou non (D115).
func TestLeRepertoireDesSessionsManquantEstRecree(t *testing.T) {
	prov, paths, _ := fixture(t, "8.3")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.3")
	// Pool écrit puis répertoire retiré à la main : le pool est conforme.
	steps, err := prov.Plan(context.Background(), p, Observed{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Apply != nil {
			if err := s.Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.RemoveAll(paths.ProjectSessionsDir("shopfr")); err != nil {
		t.Fatal(err)
	}
	steps, err = prov.Plan(context.Background(), p, Observed{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range steps {
		if s.Resource == "php/sessions" {
			found = true
			if err := s.Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if info, err := os.Stat(paths.ProjectSessionsDir("shopfr")); !found || err != nil || info.Mode().Perm() != SessionsDirMode {
		t.Errorf("répertoire des sessions non recréé (étape %v) : %v %v", found, info, err)
	}
	steps, _ = prov.Plan(context.Background(), p, Observed{})
	for _, s := range steps {
		if s.Resource == "php/sessions" {
			t.Error("répertoire présent : aucune étape attendue")
		}
	}
}
