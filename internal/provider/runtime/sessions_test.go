package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/provider/php"
)

// R4-28 (D115) : Debian règle session.gc_probability à 0 et ne purge, par
// sessionclean, que le save_path du php.ini. Celui d'un pool d'ogs n'était
// jamais purgé : les sessions s'y accumulaient sans fin.
func TestLesSessionsSontPurgeesSelonLeurAge(t *testing.T) {
	prov, node := fixture(t)
	home := filepath.Dir(node.Spec.Layout.Root)
	shop := model.New("boutique.fr", naming.EnvProd, model.KindSymfony)
	shop.Spec.App.Version = "6.4"
	blog := model.New("blog.fr", naming.EnvProd, model.KindPHPPlain)
	for _, p := range []*model.Project{shop, blog} {
		if err := p.Normalize(home, "preprod-pp.net"); err != nil {
			t.Fatal(err)
		}
	}
	// Un pool qui garde ses sessions une journée, un autre au défaut : le
	// répertoire commun suit le plus long.
	shop.Spec.PHP.Pools[0].Settings = model.PoolSettings{"php_value[session.gc_maxlifetime]": model.Value("86400")}
	shop.Spec.PHP.Pools = append(shop.Spec.PHP.Pools, model.Pool{Name: "api", Version: shop.Spec.PHP.Pools[0].Version})
	// Le php.ini de FPM fixe la valeur d'un pool qui n'en déclare pas.
	ini := filepath.Join(prov.Paths.PHPRoot(), blog.Spec.PHP.Pools[0].Version, "fpm", "php.ini")
	if err := os.MkdirAll(filepath.Dir(ini), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ini, []byte(";session.gc_maxlifetime = 1\nsession.gc_maxlifetime = 3600 ; une heure\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// conf.d l'emporte sur php.ini, le dernier fichier par son nom.
	confd := filepath.Join(filepath.Dir(ini), "conf.d")
	if err := os.MkdirAll(confd, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"10-a.ini": "60", "20-b.ini": "3600"} {
		if err := os.WriteFile(filepath.Join(confd, name), []byte("session.gc_maxlifetime = "+v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ini, []byte("session.gc_maxlifetime = 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	content := string(prov.Build([]*model.Project{node, shop, blog}))
	for _, want := range []string{
		"d " + prov.Paths.SessionsRoot() + " 0711 root root -",
		"d " + prov.Paths.ProjectSessionsDir("boutiquefr") + " 0700 boutiquefr boutiquefr 86400s",
		"d " + prov.Paths.ProjectSessionsDir("blogfr") + " 0700 blogfr blogfr 3600s",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("ligne %q absente :\n%s", want, content)
		}
	}
	// Un projet sans PHP n'a pas de sessions.
	if strings.Contains(content, prov.Paths.ProjectSessionsDir(node.ID())) {
		t.Errorf("un projet sans PHP n'a pas de ligne de sessions :\n%s", content)
	}
	// Sans réglage ni php.ini : le défaut de PHP.
	if err := os.RemoveAll(filepath.Dir(ini)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prov.Build([]*model.Project{blog})), "0700 blogfr blogfr 1440s") {
		t.Errorf("le défaut de PHP (1440 s) est attendu :\n%s", prov.Build([]*model.Project{blog}))
	}
	// Un pool qui range ses sessions ailleurs ne fixe pas l'âge : rien à purger.
	blog.Spec.PHP.Pools[0].Settings = model.PoolSettings{"php_admin_value[session.save_path]": model.Value("/srv/x")}
	if !strings.Contains(string(prov.Build([]*model.Project{blog})), "0700 blogfr blogfr -") {
		t.Errorf("sans pool qui l'emploie, le répertoire n'est pas purgé :\n%s", prov.Build([]*model.Project{blog}))
	}

	// Le répertoire est créé aussitôt, au mode de la ligne.
	step, err := prov.Plan([]*model.Project{shop})
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	for dir, mode := range map[string]os.FileMode{prov.Paths.SessionsRoot(): php.SessionsRootMode,
		prov.Paths.ProjectSessionsDir("boutiquefr"): php.SessionsDirMode} {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s : %v %v, mode attendu %04o", dir, info, err, mode)
		}
	}
}
