package cli

import (
	"os"
	"strings"
	"testing"
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
