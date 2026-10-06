package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/spec"
)

// Lot Y17 (D123) : runtimes et clone.

// R1-40 : une application à l'écoute au moment du plan n'était jamais
// revérifiée, alors que l'apply la redémarrait — ici, une variable
// d'environnement changée réécrit l'unité. L'attente revient au plan.
func TestUneApplicationRedemarreeEstAttendueDeNouveau(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "node.fr", "--kind", "node", "--runtime", "22.11.0", "--tls=false", "--yes")

	// L'application répond sur son socket.
	sock := filepath.Join(config.NewPaths(h.prefix).ProjectRunDir("nodefr"), "app.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("socket Unix impossible ici : %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	if out := h.mustRun("plan", "nodefr"); strings.Contains(out, "attente de disponibilité") {
		t.Errorf("rien ne redémarre : l'application à l'écoute n'est pas attendue :\n%s", out)
	}
	h.modifierDéclaration(t, "nodefr", func(p *model.Project) {
		p.Spec.App.Env = map[string]string{"NODE_ENV": "production"}
	})
	out := h.mustRun("plan", "nodefr")
	if !strings.Contains(out, "unité applicative") || !strings.Contains(out, "après redémarrage") {
		t.Errorf("l'unité réécrite redémarre l'application : elle doit être attendue de nouveau :\n%s", out)
	}
}

// R1-98 : un projet qui signe en DKIM ne se clonait pas — la préproduction
// héritait de mail.dkim.enabled, que la validation refuse en préproduction, et
// le clone rendait 3 sans rien déposer. La signature n'est pas reprise, et le
// clone le dit (code 6).
func TestLeCloneDUnProjetSigneEnDKIMNeSignePasLaPreproduction(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("mail", "dkim", "enable", "shopfr")
	h.mustRunPartial("apply", "shopfr", "--yes")

	if out, _ := h.run("site", "clone", "shopfr", "--to", "prep", "--dry-run"); !strings.Contains(out,
		"le clone rendra le code 6 : signature DKIM non reprise") {
		t.Errorf("--dry-run doit annoncer le code 6 :\n%s", out)
	}
	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Fatalf("code %d, attendu %d :\n%s", code, CodePartial, out)
	}
	clone, err := spec.Load(h.path("etc", "ogs", "projects", "shopfrprep.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if clone.Spec.Mail != nil {
		t.Errorf("la préproduction ne doit pas signer : %+v", clone.Spec.Mail)
	}
	contientTout(t, out, "signature DKIM de shopfr non reprise")
	source, err := spec.Load(h.path("etc", "ogs", "projects", "shopfr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if source.Spec.Mail == nil || source.Spec.Mail.DKIM == nil || !source.Spec.Mail.DKIM.Enabled {
		t.Error("la production doit continuer de signer")
	}
}

// R4-32 : un numéro de pool imposé était recopié tel quel, et la préproduction
// réclamait celui de la production. Il redevient « auto » ; un preset déclaré,
// lui, est recopié tel quel (D103 : seul le preset hérité descend d'un cran).
func TestLeCloneNeRepritPasLeNumeroDePoolImpose(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "blog.fr", "--kind", "php-plain", "--php", "8.3", "--tls=false", "--yes")
	h.modifierDéclaration(t, "blogfr", func(p *model.Project) {
		p.Spec.PHP.Pools[0].Number = "7"
		p.Spec.PHP.Pools[0].Preset = "large"
	})
	h.mustRunPartial("apply", "blogfr", "--yes")

	out, code := h.run("site", "clone", "blogfr", "--to", "prep", "--yes")
	if code != CodeOK && code != CodePartial {
		t.Fatalf("code %d :\n%s", code, out)
	}
	clone, err := spec.Load(h.path("etc", "ogs", "projects", "blogfrprep.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if n := clone.Spec.PHP.Pools[0].Number; n != "auto" {
		t.Errorf("numéro de pool de la préproduction : %q, attendu auto", n)
	}
	if p := clone.Spec.PHP.Pools[0].Preset; p != "large" {
		t.Errorf("un preset déclaré est recopié tel quel (D103) : %q", p)
	}
	contientTout(t, out, "numéro de pool 7 de blogfr non repris")
	source, err := spec.Load(h.path("etc", "ogs", "projects", "blogfr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if source.Spec.PHP.Pools[0].Number != "7" {
		t.Errorf("la production garde son numéro : %q", source.Spec.PHP.Pools[0].Number)
	}
}

// R1-98 : « clone := *source » partageait avec la source tout ce que la
// déclaration pointe. Chaque pointeur et chaque table est dupliqué.
func TestLeCloneNePartageRienAvecLaSource(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "node")
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.App.Runtime = "22.11.0"
		p.Spec.App.Health = &model.Health{Timeout: "10s"}
		p.Spec.Web.TLS = &model.TLS{Enabled: true}
		p.Spec.Sudo = &model.Sudo{Style: "ogs", Verbs: []string{"restart"}}
		p.Spec.Hooks = map[string]string{"postCreate": "projets/post.sh"}
	})
	app := h.ouvrirApp(t, true)
	source, err := app.loadProject("shopfr")
	if err != nil {
		t.Fatal(err)
	}
	clone, err := app.cloneProject(source, naming.EnvPrep)
	if err != nil {
		t.Fatal(err)
	}
	clone.Spec.App.Health.Timeout = "1s"
	clone.Spec.Web.TLS.Enabled = false
	clone.Spec.Sudo.Verbs[0] = "status"
	clone.Spec.Hooks["postCreate"] = "autre.sh"
	if source.Spec.App.Health.Timeout != "10s" || !source.Spec.Web.TLS.Enabled ||
		source.Spec.Sudo.Verbs[0] != "restart" || source.Spec.Hooks["postCreate"] != "projets/post.sh" {
		t.Errorf("le clone partage la déclaration de la source : health=%+v tls=%+v sudo=%+v hooks=%v",
			source.Spec.App.Health, source.Spec.Web.TLS, source.Spec.Sudo, source.Spec.Hooks)
	}
}

// pyvenvDe pose dans dir un venv créé par l'interpréteur pyenv de root.
func pyvenvDe(t *testing.T, dir, root, version string) {
	t.Helper()
	déposer(t, filepath.Join(dir, "bin", "python"), "#!/bin/sh\n")
	déposer(t, filepath.Join(dir, "pyvenv.cfg"), "home = "+filepath.Join(root, ".pyenv", "versions", version, "bin")+
		"\ninclude-system-site-packages = false\nversion = "+version+"\n")
}

// R4-25 : le clone recopiait www/.venv : la préproduction exécutait
// l'interpréteur logé dans le home de la production, et cassait quand celle-ci
// changeait de version. L'ancien venv n'est pas recopié — les nouveaux vivent
// hors de www (D123) —, et le clone dit où installer les dépendances.
func TestLeCloneNeRecopiePasLeVenvDeLaProduction(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "py.fr", "--kind", "python", "--runtime", "3.12.7", "--tls=false", "--yes")
	root := h.path("home", "py.fr")
	pyvenvDe(t, filepath.Join(root, "www", ".venv"), root, "3.12.7")
	déposer(t, filepath.Join(root, "www", "app.py"), "app = None\n")

	out, code := h.run("site", "clone", "pyfr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d :\n%s", code, CodePartial, out)
	}
	prep := h.path("home", "py.preprod-pp.net")
	if _, err := os.Lstat(filepath.Join(prep, "www", ".venv")); !os.IsNotExist(err) {
		t.Errorf("le venv de la production ne doit pas être recopié : %v", err)
	}
	if _, err := os.Lstat(filepath.Join(prep, "www", "app.py")); err != nil {
		t.Errorf("le code est recopié : %v", err)
	}
	link, err := os.Readlink(filepath.Join(prep, ".local", "bin", "python"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(prep, ".venv-3.12.7", "bin", "python"); link != want {
		t.Errorf("la préproduction emploie son propre venv : %q, attendu %q", link, want)
	}
	contientTout(t, out, "venv de pyfr, dont l'interpréteur vit dans son home", filepath.Join(prep, ".venv-3.12.7"))
}

// R4-23 : un venv quitté reste en place, pour un retour arrière ; doctor le
// nomme.
func TestDoctorNommeLesVenvsQuittes(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "py.fr", "--kind", "python", "--runtime", "3.12.7", "--tls=false", "--yes")
	root := h.path("home", "py.fr")
	pyvenvDe(t, filepath.Join(root, "www", ".venv"), root, "3.11.9")
	pyvenvDe(t, filepath.Join(root, ".venv-3.12.7"), root, "3.12.7")
	out, _ := h.run("doctor")
	contientTout(t, out, filepath.Join(root, "www", ".venv"))
	if strings.Contains(out, filepath.Join(root, ".venv-3.12.7")+" ") {
		t.Errorf("le venv en service n'est pas un venv quitté :\n%s", out)
	}
}

// Une section mail sans signature est retirée elle aussi : le clone le dit,
// sans code 6.
func TestLeCloneDitLaSectionMailRetireeMemeDesactivee(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.Mail = &model.Mail{DKIM: &model.DKIM{Enabled: false}}
	})
	h.mustRunPartial("apply", "shopfr", "--yes")
	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodeOK {
		t.Errorf("code %d, attendu %d :\n%s", code, CodeOK, out)
	}
	contientTout(t, out, "section mail de shopfr non reprise (signature DKIM désactivée)")
}
