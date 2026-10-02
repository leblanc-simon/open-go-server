package cli

import (
	"os"
	"slices"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// D100 (revue du lot T) : une archive porte les preuves des ressources que la suppression
// a ensuite détruites ; restaurées, elles font d'un homonyme créé depuis par
// l'administrateur une ressource d'ogs, que l'apply de la restauration retire.
func TestRestaurationNeRetirePasUnHomonymeDeLAdministrateur(t *testing.T) {
	h := newHarness(t)
	state := newBrokerState()
	h.projetMessenger(t, state)
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.AMQP.Vhost, p.Spec.AMQP.User = "shopfr2", "shopfr2"
	})
	h.run("project", "delete", "shopfr", "--yes")
	if state.users["shopfr"] {
		t.Fatal("prémisse : l'ancien compte doit être parti")
	}
	// L'administrateur crée depuis un compte homonyme, pour autre chose.
	state.users["shopfr"] = true
	state.writes = nil
	out, code := h.run("project", "restore", h.derniereArchive(t), "--yes")
	t.Logf("code %d\n%s", code, out)
	if slices.Contains(state.writes, "DELETE /api/users/shopfr") {
		t.Errorf("la restauration a supprimé le compte amqp de l'administrateur : %v", state.writes)
	}
}

// D100 (revue du lot T) : un service nommé « app » d'un projet PHP porte le nom de fichier
// de l'unité applicative ; PlanStale le retire (attribution par le projet seul,
// pas par la ressource de l'en-tête).
func TestServiceNomméAppNEstPasLUnitéApplicative(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("service", "add", "shopfr", "app", "--transport", "async")
	out, code := h.run("apply", "shopfr", "--yes")
	t.Logf("code %d\n%s", code, out)
	path := h.path("etc", "systemd", "system", "ogs-shopfr-app@.service")
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("l'unité du service déclaré « app » a été retirée : %v", err)
	}
	out, code = h.run("apply", "shopfr", "--yes")
	t.Logf("apply 2 code %d", code)
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("au second apply, l'unité du service déclaré « app » a été retirée : %v", err)
	}
	out, code = h.run("apply", "shopfr", "--yes")
	t.Logf("apply 3 code %d", code)
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("au troisième apply, toujours absente : %v", err)
	}
}

// D100 (revue du lot T) : spec.amqp retiré, puis le broker désinstallé (accès retiré) :
// chaque apply du projet rend-il désormais le code 6, sans issue ?
func TestAMQPRetiréPuisBrokerDésinstalléNeRendPas6(t *testing.T) {
	h := newHarness(t)
	state := newBrokerState()
	h.projetMessenger(t, state)
	// Le vhost garde une file pleine : l'apply ne peut pas le retirer.
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) { p.Spec.AMQP = nil })
	if err := os.Remove(h.path("etc", "ogs", "credentials", "amqp.yaml")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		out, code := h.run("apply", "shopfr", "--yes")
		t.Logf("apply %d : code %d", i, code)
		if code != 0 {
			t.Errorf("apply sans broker ni spec.amqp : code %d\n%s", code, out)
		}
	}
	out, code := h.run("diff", "shopfr")
	t.Logf("diff code %d\n%s", code, out)
}

// L'autre sens : la ressource que la déclaration restaurée nomme reste
// reconnue à ogs — sa preuve est réinjectée, rien n'est refusé comme étranger.
func TestRestaurationGardeLesPreuvesDeCeQueLaDéclarationNomme(t *testing.T) {
	h := newHarness(t)
	state := newBrokerState()
	h.projetMessenger(t, state)
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.AMQP.Vhost, p.Spec.AMQP.User = "shopfr2", "shopfr2"
	})
	h.run("project", "delete", "shopfr", "--yes")
	state.writes = nil
	out := h.mustRunPartial("project", "restore", h.derniereArchive(t), "--yes")
	if !slices.Contains(state.writes, "PUT /api/users/shopfr2") || !state.vhosts["shopfr2"] {
		t.Errorf("le vhost et le compte déclarés doivent être recréés comme ceux d'ogs : %v\n%s", state.writes, out)
	}
	names := h.secretsDe(t, "shopfr")
	for _, want := range []string{"amqp/shopfr2", "amqp-vhost/shopfr2"} {
		if !slices.Contains(names, want) {
			t.Errorf("la preuve de %s doit revenir : %v", want, names)
		}
	}
	for _, gone := range []string{"amqp/shopfr", "amqp-vhost/shopfr"} {
		if slices.Contains(names, gone) {
			t.Errorf("la preuve d'une ressource que la suppression a retirée ne revient pas : %s", gone)
		}
	}
}

// Même règle pour les comptes SGBD : seuls reviennent les mots de passe des
// comptes déclarés, jamais la marque d'un compte déplacé.
func TestRestaurationNeRéinjecteQueLesComptesDéclarés(t *testing.T) {
	p := &model.Project{}
	p.Spec.Databases = []model.Database{{Engine: "mariadb", Name: "shopfr", User: "shopfr",
		ExtraUsers: []model.DBUser{{User: "lecture"}}}}
	for name, want := range map[string]bool{
		"mysql/shopfr": true, "mariadb/lecture": true, "mysql/ancien": false, "postgres/shopfr": false,
		"mysql-moved/shopfr": false, "dkim/shop.fr/ogs": true, "amqp/shopfr": false,
	} {
		if got := restorableSecret(p, name); got != want {
			t.Errorf("%s : réinjecté %v, attendu %v", name, got, want)
		}
	}
}
