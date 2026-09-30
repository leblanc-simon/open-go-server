package cli

import (
	"os"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

func TestAdoptionRefuseUnConflitAvecLeParc(t *testing.T) {
	// « site create » et « project create » contrôlent l'unicité ; l'adoption est
	// la troisième porte d'entrée du parc, et elle ne le faisait pas. Deux
	// déclarations revendiquant le même domaine étaient écrites toutes les deux,
	// et le conflit n'apparaissait qu'au premier apply portant sur tout le parc.
	h := newHarness(t)
	h.mustRun("init")
	// Le même host, sous un autre identifiant : le fichier du candidat n'existe
	// donc pas, et seul le contrôle d'unicité peut voir la collision. Le site est
	// déclaré avant que la configuration « faite à la main » n'apparaisse — sinon
	// c'est le vhost existant qui ferait échouer la création.
	h.mustRun("site", "create", "boutique.fr", "--id", "ancienne", "--kind", "static",
		"--tls=false", "--yes")
	h.legacyServer(t)

	out, code := h.run("import", "adopt", "boutiquefr", "--yes")
	if code != CodeConflict {
		t.Fatalf("une adoption en conflit avec le parc doit rendre %d, obtenu %d :\n%s",
			CodeConflict, code, out)
	}
	if !strings.Contains(out, "conflit avec le parc existant") {
		t.Errorf("le conflit doit être nommé :\n%s", out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "boutiquefr.yaml")); !os.IsNotExist(err) {
		t.Error("aucune déclaration ne doit être écrite en cas de conflit")
	}
}

func TestAdoptionSansConflitResteEcrite(t *testing.T) {
	// Le contrôle ne doit pas fermer la porte au cas courant : un serveur repris
	// dont rien n'est encore déclaré. C'est cette adoption-ci qui éprouve le cas
	// limite du contrôle d'unicité — le candidat ne doit pas entrer en conflit avec
	// lui-même : le contrôle porte sur le parc augmenté du candidat, jamais compté
	// deux fois, sans quoi la collision porterait sur son propre host.
	h := newHarness(t)
	h.mustRun("init")
	h.legacyServer(t)

	h.mustRun("import", "adopt", "boutiquefr", "--yes")
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "boutiquefr.yaml")); err != nil {
		t.Errorf("l'adoption d'un candidat sans conflit doit être écrite : %v", err)
	}

	// Réadopter le même candidat bute sur la déclaration déjà écrite, et c'est ce
	// garde-là qui doit parler : le contrôle d'unicité, placé après lui, accuserait
	// le parc — « conflit avec le parc existant » — de ce qui n'est que le projet
	// lui-même, et l'exploitant chercherait un voisin qui n'existe pas.
	out, code := h.run("import", "adopt", "boutiquefr", "--yes")
	if code != CodeConflict {
		t.Fatalf("réadopter un projet déjà déclaré doit rendre %d, obtenu %d :\n%s",
			CodeConflict, code, out)
	}
	if !strings.Contains(out, "déjà déclaré") {
		t.Errorf("la déclaration existante doit être nommée :\n%s", out)
	}
	if strings.Contains(out, "conflit avec le parc existant") {
		t.Errorf("un projet ne doit pas être mis en conflit avec lui-même :\n%s", out)
	}
}

func TestAdoptionAfficheCeQuiResteAConfirmer(t *testing.T) {
	// La confirmation portait sur une déclaration dont l'exploitant ne voyait ni
	// les bases retenues ni ce qui restait à confirmer : seul le scan le disait,
	// et une base écartée faute de compte sûr — ou prise au voisin — passait sans
	// un mot au moment d'écrire.
	h := newHarness(t)
	h.mustRun("init")
	h.legacyServer(t)
	if err := os.MkdirAll(h.path("var", "www", "vitrine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path("etc", "apache2", "sites-available", "vitrine.conf"),
		[]byte("<VirtualHost *:80>\n    ServerName vitrine.fr\n    DocumentRoot /var/www/vitrine\n</VirtualHost>\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("import", "adopt", "vitrinefr", "--yes")
	if !strings.Contains(out, "⚠ aucun utilisateur système repéré") {
		t.Errorf("les incertitudes du candidat doivent être affichées avant d'écrire :\n%s", out)
	}
}

func TestImportUnmanagedListeCeQueLeServeurPorteMaintenant(t *testing.T) {
	// Le registre des ressources non gérées n'était écrit qu'à l'adoption, sans
	// les autres candidats, et jamais relu : « import unmanaged » répondait
	// « aucune ressource » sur un serveur jamais adopté, et la promesse que les
	// conflits en tiendraient compte n'était tenue nulle part (R2-38, D95). La
	// liste est désormais celle de l'inspection, au moment de la demande.
	h := newHarness(t)
	h.mustRun("init")
	h.legacyServer(t)

	out := h.mustRun("import", "unmanaged")
	if !strings.Contains(out, "humain") {
		t.Errorf("le compte « humain », rattaché à aucun candidat, doit être listé :\n%s", out)
	}
	if strings.Contains(out, "boutique.fr") {
		t.Errorf("le vhost d'un candidat n'est pas hors gestion :\n%s", out)
	}

	// Ce qui disparaît du serveur disparaît de la liste : un relevé figé à
	// l'import continuait de le nommer.
	if err := os.WriteFile(h.path("etc", "passwd"),
		[]byte("boutique:x:1001:1001::/var/www/boutique:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := h.mustRun("import", "unmanaged"); strings.Contains(out, "humain") {
		t.Errorf("un compte supprimé ne doit plus être listé :\n%s", out)
	}
}

func TestAdoptionEcarteLesBasesAuCompteReserve(t *testing.T) {
	// Une application ancienne qui se connecte en root faisait adopter root :
	// l'import conseillait ensuite « db user rotate », qui changeait le mot de
	// passe d'administration du moteur (D91).
	p := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	if err := p.Normalize(t.TempDir(), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	p.Spec.Databases = []model.Database{
		{Engine: "mariadb", Name: "shop", User: "root"},
		{Engine: "mariadb", Name: "shop_api", User: "shop_api"},
		{Engine: "postgres", Name: "stats", User: "metriques"},
	}
	notes := []string{
		"le mot de passe de root@localhost est inconnu d'ogs : Employer « ogs db user rotate shopfr root » pour en reprendre la main",
		"le mot de passe de shop_api@localhost est inconnu d'ogs : Employer « ogs db user rotate shopfr shop_api » pour en reprendre la main",
	}
	notes = withoutReservedDatabases(p, notes, []string{"metriques"})
	if len(p.Spec.Databases) != 1 || p.Spec.Databases[0].Name != "shop_api" {
		t.Fatalf("seule shop_api doit rester déclarée : %+v", p.Spec.Databases)
	}
	joined := strings.Join(notes, "\n")
	if strings.Contains(joined, "rotate shopfr root") {
		t.Errorf("le conseil de rotation de root doit tomber :\n%s", joined)
	}
	for _, want := range []string{"rotate shopfr shop_api", "base shop non déclarée", "base stats non déclarée"} {
		if !strings.Contains(joined, want) {
			t.Errorf("note absente : %s\n%s", want, joined)
		}
	}
}
