package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/spec"
)

// Lot Y19 (D121) : services et réglages ignorés.

// projetNode déclare une application supervisée : son unité est
// ogs-<id>-app@.service.
func (h *harness) projetNode(t *testing.T) {
	t.Helper()
	h.mustRun("project", "create", "app.fr", "--kind", "node")
	h.modifierDéclaration(t, "appfr", func(p *model.Project) {
		p.Spec.App.Runtime = "22.11.0"
		p.Spec.App.Entrypoint = "server.js"
	})
}

// R1-33 : un service « app » écrivait le fichier d'unité de l'application
// supervisée ; les deux providers se le disputaient à chaque apply.
func TestServiceAddRefuseLeNomDeLApplicationSupervisée(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.projetNode(t)
	out, code := h.run("service", "add", "appfr", "app", "--template", "worker", "--command", "/bin/true")
	if code != CodeValidation {
		t.Fatalf("« service add … app » doit être refusé en code 3, obtenu %d :\n%s", code, out)
	}
	p, err := spec.Load(h.path("etc", "ogs", "projects", "appfr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Spec.Services) != 0 {
		t.Errorf("rien ne doit être déclaré : %+v", p.Spec.Services)
	}
	// Réservé aussi sur un projet sans application supervisée : il pourrait le
	// devenir.
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4")
	if out, code := h.run("service", "add", "shopfr", "app", "--template", "worker",
		"--command", "/bin/true"); code != CodeValidation {
		t.Errorf("le nom doit être réservé à toute déclaration neuve, obtenu %d :\n%s", code, out)
	}
}

func TestUnServiceAppEstRefuséAuPlanJamaisAuChargement(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.projetNode(t)
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4")
	service := model.Service{Name: "app", Template: "worker", Params: map[string]string{"command": "/bin/true"}}
	h.modifierDéclaration(t, "appfr", func(p *model.Project) { p.Spec.Services = []model.Service{service} })
	// Un projet PHP n'a pas d'application supervisée : son service « app »
	// fonctionne, et le refuser l'arrêterait.
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) { p.Spec.Services = []model.Service{service} })

	out, code := h.run("plan", "appfr")
	if code != CodeValidation || !strings.Contains(out, "spec.services[0].name") {
		t.Errorf("le plan doit refuser le service « app » d'une application supervisée (code 3), "+
			"obtenu %d :\n%s", code, out)
	}
	if out, code := h.run("apply", "appfr", "--yes"); code != CodeValidation {
		t.Errorf("l'apply aussi, obtenu %d :\n%s", code, out)
	}
	// Le reste du parc continue de se charger.
	if out, code := h.run("plan", "shopfr"); code != CodeOK {
		t.Errorf("le voisin, et un projet PHP qui déclare « app », ne doivent pas être bloqués (%d) :\n%s",
			code, out)
	}
	if out, code := h.run("status"); code == CodeValidation {
		t.Errorf("le chargement ne doit pas refuser la déclaration :\n%s", out)
	}
}

// R2-31 : sudo.style et sudo.verbs de config.yaml n'avaient aucun effet. Ils
// sont un défaut de création (D121, comme user.shell en D62).
func TestLaDélégationDeConfigYamlEstUnDéfautDeCréation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "ancien.fr", "--kind", "php-symfony", "--app-version", "6.4")
	h.mustRun("service", "add", "ancienfr", "veille", "--template", "worker", "--command", "/bin/true")

	cfg := h.path("etc", "ogs", "config.yaml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	modifié := strings.Replace(string(data), "sudo:\n  style: ogs\n", "sudo:\n  style: systemctl\n", 1)
	if modifié == string(data) {
		t.Fatalf("la configuration générée doit porter sudo.style :\n%s", data)
	}
	if err := os.WriteFile(cfg, []byte(modifié), 0o600); err != nil {
		t.Fatal(err)
	}

	h.mustRun("project", "create", "neuf.fr", "--kind", "php-symfony", "--app-version", "6.4")
	h.mustRun("service", "add", "neuffr", "veille", "--template", "worker", "--command", "/bin/true")
	neuf, err := spec.Load(h.path("etc", "ogs", "projects", "neuffr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if neuf.Spec.Sudo == nil || neuf.Spec.Sudo.Style != "systemctl" ||
		!slices.Equal(neuf.Spec.Sudo.Verbs, []string{"start", "stop", "restart", "status"}) {
		t.Fatalf("la déclaration d'un projet neuf doit porter la délégation configurée : %+v", neuf.Spec.Sudo)
	}
	h.mustRun("apply", "neuffr", "--yes")
	if s := h.read(t, filepath.Join("etc", "sudoers.d", "ogs-neuffr")); !strings.Contains(s, "/usr/bin/systemctl restart ogs-neuffr-veille@1.service") {
		t.Errorf("la délégation du projet neuf doit suivre le style configuré :\n%s", s)
	}

	// Le projet existant garde la délégation qu'il avait : changer config.yaml
	// n'en retire ni n'en ajoute.
	ancien, err := spec.Load(h.path("etc", "ogs", "projects", "ancienfr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if ancien.Spec.Sudo != nil {
		t.Errorf("la déclaration existante ne doit pas être touchée : %+v", ancien.Spec.Sudo)
	}
	h.mustRun("apply", "ancienfr", "--yes")
	if s := h.read(t, filepath.Join("etc", "sudoers.d", "ogs-ancienfr")); !strings.Contains(s, "/usr/bin/ogs my restart") ||
		strings.Contains(s, "systemctl") {
		t.Errorf("le projet existant garde sa délégation historique :\n%s", s)
	}
}

// R1-66 : « amqp messenger » remplaçait toute la déclaration AMQP. Un second
// transport effaçait le premier — et l'apply suivant retirait du broker ce qui
// quittait la déclaration (D100).
func TestAMQPMessengerComplèteSansRetirer(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "3.4", "--php", "7.4")
	h.mustRun("amqp", "messenger", "shopfr", "--transport", "async", "--failed")
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.AMQP.Queues = append(p.Spec.AMQP.Queues, model.Queue{Name: "rapports", Type: "classic", Durable: true})
		p.Spec.AMQP.Policies = []model.Policy{{Name: "borne", Pattern: "^rapports$",
			Definition: map[string]any{"max-length": 1000}}}
		p.Spec.AMQP.Permissions["configure"] = "^(async|rapports).*"
	})
	h.mustRun("amqp", "messenger", "shopfr", "--transport", "mails", "--failed")
	p, err := spec.Load(h.path("etc", "ogs", "projects", "shopfr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, q := range p.Spec.AMQP.Queues {
		files = append(files, q.Name)
	}
	for _, want := range []string{"async", "async_failed", "rapports", "mails", "mails_failed"} {
		if !slices.Contains(files, want) {
			t.Errorf("la file %s doit être déclarée : %v", want, files)
		}
	}
	if len(p.Spec.AMQP.Policies) != 1 || p.Spec.AMQP.Permissions["configure"] != "^(async|rapports).*" {
		t.Errorf("policies et permissions déclarées doivent rester : %+v", p.Spec.AMQP)
	}
	if !slices.Contains(p.Spec.AMQP.Bindings, model.Binding{Exchange: "messages", Queue: "async", RoutingKey: "async"}) {
		t.Errorf("le binding du premier transport doit rester : %+v", p.Spec.AMQP.Bindings)
	}
}

// Une file du préréglage déjà déclarée autrement est gardée telle quelle, et le
// code 6 le dit.
func TestAMQPMessengerGardeUneFileDéclaréeAutrement(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "3.4", "--php", "7.4")
	h.mustRun("amqp", "messenger", "shopfr", "--transport", "async", "--failed")
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		for i := range p.Spec.AMQP.Queues {
			if p.Spec.AMQP.Queues[i].Name == "async" {
				p.Spec.AMQP.Queues[i].Type = "quorum"
			}
		}
	})
	out, code := h.run("amqp", "messenger", "shopfr", "--transport", "async", "--failed")
	if code != CodePartial || !strings.Contains(out, "file async") {
		t.Errorf("code 6 et la file nommée attendus, obtenu %d :\n%s", code, out)
	}
	p, err := spec.Load(h.path("etc", "ogs", "projects", "shopfr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range p.Spec.AMQP.Queues {
		if q.Name == "async" && q.Type != "quorum" {
			t.Errorf("la file déclarée doit rester en quorum : %+v", q)
		}
	}
}

// Une archive d'avant D121 qui porte un service « app » à côté de
// l'application supervisée : refus avant tout dépôt, remède propre à l'archive.
func TestLaRestaurationDUnServiceAppEstRefuséeAvantToutDépôt(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.projetNode(t)
	h.mustRunPartial("apply", "appfr", "--yes")
	h.modifierDéclaration(t, "appfr", func(p *model.Project) {
		p.Spec.Services = []model.Service{{Name: "app", Template: "worker", Params: map[string]string{"command": "/bin/true"}}}
	})
	if out, code := h.run("project", "delete", "appfr", "--yes"); code != CodeOK && code != CodePartial {
		t.Fatalf("suppression : %d\n%s", code, out)
	}
	archives, _ := filepath.Glob(h.path("var", "lib", "ogs", "archives", "appfr-*"))
	if len(archives) != 1 {
		t.Fatalf("une archive attendue : %v", archives)
	}
	out, code := h.run("project", "restore", archives[0], "--yes")
	if code != CodeValidation || !strings.Contains(out, "tar --zstd") || !strings.Contains(out, "renommer le service") {
		t.Errorf("refus en code 3 avec un remède applicable à l'archive attendu, obtenu %d :\n%s", code, out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "appfr.yaml")); err == nil {
		t.Error("rien ne doit être déposé")
	}
}

// Transition d'un projet d'avant D121 : application supervisée et service
// « app » dont le modèle est marqué « unit/app ». Le remède nommé par le refus
// — « service remove … app » puis apply — ne doit jamais retirer l'unité
// applicative, ni au premier apply, ni au suivant.
func TestLeRemèdeDuServiceAppGardeLUnitéApplicative(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.projetNode(t)
	h.mustRunPartial("apply", "appfr", "--yes")
	unite := h.path("etc", "systemd", "system", "ogs-appfr-app@.service")
	data, err := os.ReadFile(unite)
	if err != nil {
		t.Fatal(err)
	}
	marque := strings.Replace(string(data), "ressource: app/app", "ressource: unit/app", 1)
	if marque == string(data) {
		t.Fatalf("en-tête inattendu :\n%s", data)
	}
	if err := os.WriteFile(unite, []byte(marque), 0o644); err != nil {
		t.Fatal(err)
	}
	h.modifierDéclaration(t, "appfr", func(p *model.Project) {
		p.Spec.Services = []model.Service{{Name: "app", Template: "worker", Params: map[string]string{"command": "/bin/true"}}}
	})
	out, code := h.run("plan", "appfr")
	if code != CodeValidation || !strings.Contains(out, "ogs service remove appfr app") {
		t.Fatalf("le refus doit nommer le remède, obtenu %d :\n%s", code, out)
	}
	h.mustRun("service", "remove", "appfr", "app")
	for i := 1; i <= 2; i++ {
		h.mustRunPartial("apply", "appfr", "--yes")
		if _, err := os.Stat(unite); err != nil {
			t.Fatalf("apply %d : l'unité de l'application supervisée a été retirée : %v", i, err)
		}
	}
}
