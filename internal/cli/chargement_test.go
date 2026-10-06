package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// parcDeDeux déclare et applique deux sites statiques.
func parcDeDeux(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("project", "create", "blog.fr", "--kind", "static")
	h.mustRunPartial("apply", "--yes")
	return h
}

// modifierConfig réécrit config.yaml.
func (h *harness) modifierConfig(t *testing.T, modifier func(string) string) {
	t.Helper()
	path := h.path("etc", "ogs", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	modifié := modifier(string(data))
	if modifié == string(data) {
		t.Fatalf("config.yaml n'a pas changé :\n%s", data)
	}
	if err := os.WriteFile(path, []byte(modifié), 0o644); err != nil {
		t.Fatal(err)
	}
}

// R1-41 : une seule déclaration refusée au chargement arrêtait le renouvellement
// de tous les certificats du parc, en code 3, et doctor s'interrompait sur elle.
// Le renouvellement la saute en la nommant (code 6) ; le plan et l'apply du parc
// la refusent toujours, et doctor la nomme sans taire le reste.
func TestUneDeclarationRefuseeNArretePasLeRenouvellementDuParc(t *testing.T) {
	h := parcDeDeux(t)
	h.modifierDeclaration(t, "blogfr", func(s string) string {
		return strings.Replace(s, "webAccess: group", "webAccess: nimporte", 1)
	})
	// Un fichier illisible aussi : il n'arrête pas davantage le parc.
	déposer(t, h.path("etc", "ogs", "projects", "casse.yaml"), "metadata: [\n")

	for _, args := range [][]string{{"cert", "renew", "--all"}, {"cert", "renew"}} {
		out, code := h.run(args...)
		if code != CodePartial {
			t.Errorf("%v : code %d, attendu %d (projet sauté, nommé) :\n%s", args, code, CodePartial, out)
		}
		for _, nom := range []string{"blogfr", "casse.yaml"} {
			if !strings.Contains(out, nom) {
				t.Errorf("%v : %s doit être nommé :\n%s", args, nom, out)
			}
		}
	}

	// L'apply du parc relit tout le parc : il refuse, et nomme les deux fautes
	// d'un coup, non la première seule.
	out, code := h.run("plan")
	if code != CodeValidation {
		t.Errorf("plan du parc : code %d, attendu %d :\n%s", code, CodeValidation, out)
	}
	if !strings.Contains(out, "blogfr") || !strings.Contains(out, "casse.yaml") {
		t.Errorf("le refus doit nommer toutes les déclarations fautives :\n%s", out)
	}

	out, _ = h.run("doctor")
	if !strings.Contains(out, "blogfr") || !strings.Contains(out, "casse.yaml") {
		t.Errorf("doctor doit nommer les déclarations refusées :\n%s", out)
	}
	// Les contrôles qui suivent le chargement ont tourné sur le reste du parc.
	if !strings.Contains(out, "unicité du parc") {
		t.Errorf("doctor s'est arrêté sur la déclaration refusée :\n%s", out)
	}
}

// R2-28 : changer logs.root ou naming.homeRoot alors que des projets existent
// rendait toutes les déclarations invalides au chargement — le renouvellement
// des certificats s'arrêtait en code 3 avec le reste. Le changement est refusé
// au plan et à l'apply (code 3), doctor le nomme avec l'ancienne valeur à
// remettre, et le renouvellement continue.
func TestChangerUneRacineDuParcEstRefuseSansArreterLeRenouvellement(t *testing.T) {
	cas := []struct {
		nom, clé string
		modifier func(string) string
		ancienne string
	}{
		{"logs.root", "logs.root", func(s string) string {
			return strings.Replace(s, "root: /var/log/ogs-projects", "root: /srv/journaux", 1)
		}, "/var/log/ogs-projects"},
		{"naming.homeRoot", "naming.homeRoot", func(s string) string {
			return strings.Replace(s, "homeRoot: /home", "homeRoot: /srv/www", 1)
		}, "/home"},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			h := parcDeDeux(t)
			h.modifierConfig(t, c.modifier)

			if out, code := h.run("cert", "renew", "--all"); code == CodeValidation || code == CodeError {
				t.Errorf("cert renew --all : code %d, le renouvellement ne doit pas s'arrêter :\n%s", code, out)
			}
			for _, args := range [][]string{{"plan", "shopfr"}, {"apply", "--yes"}, {"plan"}} {
				out, code := h.run(args...)
				if code != CodeValidation {
					t.Errorf("%v : code %d, attendu %d :\n%s", args, code, CodeValidation, out)
				}
				if !strings.Contains(out, c.clé) || !strings.Contains(out, c.ancienne) {
					t.Errorf("%v : le refus doit nommer %s et l'ancienne valeur %s :\n%s", args, c.clé, c.ancienne, out)
				}
			}
			// Un projet neuf se poserait sous la nouvelle racine, le parc sous
			// l'ancienne : la création est refusée aussi, avant toute écriture.
			out, code := h.run("project", "create", "neuf.fr", "--kind", "static")
			if code != CodeValidation {
				t.Errorf("project create : code %d, attendu %d :\n%s", code, CodeValidation, out)
			}
			if _, err := os.Stat(h.path("etc", "ogs", "projects", "neuffr.yaml")); err == nil {
				t.Error("la déclaration du projet neuf ne doit pas être écrite")
			}
			// La suppression chercherait home et journaux sous la nouvelle racine :
			// refusée avant la confirmation, --keep-data compris.
			for _, args := range [][]string{{"project", "delete", "shopfr", "--yes"},
				{"project", "delete", "shopfr", "--yes", "--keep-data"}} {
				out, code := h.run(args...)
				if code != CodeValidation || !strings.Contains(out, c.ancienne) {
					t.Errorf("%v : code %d, attendu %d avec l'ancienne valeur :\n%s", args, code, CodeValidation, out)
				}
			}
			if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfr.yaml")); err != nil {
				t.Errorf("la déclaration de shopfr doit rester : %v", err)
			}
			out, _ = h.run("doctor")
			if !strings.Contains(out, c.clé) || !strings.Contains(out, c.ancienne) {
				t.Errorf("doctor doit nommer %s et l'ancienne valeur %s :\n%s", c.clé, c.ancienne, out)
			}
		})
	}
}

// R2-20 : une base d'état disparue était recréée vide en silence par la première
// commande venue, doctor n'en disait rien. Tant que des projets existent, les
// commandes qui en dépendent refusent en code 5 jusqu'à « ogs init » ; le
// renouvellement tourne sans elle, sans la recréer, et le dit (code 6).
func TestUneBaseDEtatDisparueEstRefuseeJusquAInit(t *testing.T) {
	h := parcDeDeux(t)
	base := h.path("var", "lib", "ogs", "state.db")
	for _, f := range []string{base, base + "-wal", base + "-shm"} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	absente := func(quand string) {
		t.Helper()
		if _, err := os.Stat(base); err == nil {
			t.Fatalf("%s : la base d'état a été recréée", quand)
		}
	}

	out, code := h.run("plan", "shopfr")
	if code != CodeDependency || !strings.Contains(out, "ogs init") {
		t.Errorf("plan : code %d, attendu %d avec le remède :\n%s", code, CodeDependency, out)
	}
	absente("plan")
	if out, code := h.run("apply", "--yes"); code != CodeDependency {
		t.Errorf("apply : code %d, attendu %d :\n%s", code, CodeDependency, out)
	}
	absente("apply")

	out, code = h.run("cert", "renew", "--all")
	if code != CodePartial || !strings.Contains(out, "state.db") {
		t.Errorf("cert renew : code %d, attendu %d en nommant la base :\n%s", code, CodePartial, out)
	}
	absente("cert renew")

	out, _ = h.run("doctor")
	if !strings.Contains(out, "base d'état") || !strings.Contains(out, "ogs init") {
		t.Errorf("doctor doit nommer la base disparue :\n%s", out)
	}
	absente("doctor")

	out = h.mustRun("init")
	if !strings.Contains(out, "recréée") {
		t.Errorf("init doit dire qu'il recrée la base d'un parc existant :\n%s", out)
	}
	if _, err := os.Stat(filepath.Clean(base)); err != nil {
		t.Fatalf("init doit recréer la base : %v", err)
	}
	if out, code := h.run("plan", "shopfr"); code != CodeOK && code != CodePartial {
		t.Errorf("après init, plan : code %d :\n%s", code, out)
	}
}

// Les consultations qui ne recalculent aucun fichier du parc ne s'arrêtent pas
// sur un voisin refusé : elles le nomment et rendent le reste (D120). « ogs my »
// ne montre pas à l'appelant le détail de la déclaration d'un autre.
func TestUnVoisinRefuseNArretePasLesConsultations(t *testing.T) {
	h := parcDeDeux(t)
	h.modifierDeclaration(t, "blogfr", func(s string) string {
		return strings.Replace(s, "webAccess: group", "webAccess: nimporte", 1)
	})
	out, code := h.run("project", "list")
	if code != CodePartial || !strings.Contains(out, "shopfr") {
		t.Errorf("project list : code %d, attendu %d avec shopfr :\n%s", code, CodePartial, out)
	}
	out, code = h.run("status")
	if code == CodeValidation || !strings.Contains(out, "shopfr") {
		t.Errorf("status : code %d, le parc lisible doit être rendu :\n%s", code, out)
	}
	t.Setenv("SUDO_UID", strconv.Itoa(os.Getuid()))
	out, _ = h.run("my", "restart")
	if strings.Contains(out, "nimporte") || strings.Contains(out, "webAccess") {
		t.Errorf("ogs my ne doit pas montrer la déclaration d'un voisin :\n%s", out)
	}
}

// Le plan global est le point commun de tout recalcul du parc — « mail dkim
// activate » y passe sans checkFleetUniqueness : des journaux hors de logs.root
// n'y entrent jamais, la rotation et les prisons les décriraient (D71, D120).
func TestLePlanGlobalRefuseDesJournauxHorsDeLaRacine(t *testing.T) {
	h := parcDeDeux(t)
	h.modifierDeclaration(t, "blogfr", func(s string) string {
		return strings.Replace(s, "logDir: "+h.path("var", "log", "ogs-projects", "blogfr"),
			"logDir: "+h.path("srv", "journaux", "blogfr"), 1)
	})
	app := h.ouvrirApp(t, true)
	provs, err := app.buildProviders()
	if err != nil {
		t.Fatal(err)
	}
	defer provs.close()
	_, err = app.buildGlobalPlan(context.Background(), provs, nil)
	if ExitCode(err) != CodeValidation || !strings.Contains(fmt.Sprint(err), "logs.root") {
		t.Errorf("le plan global doit refuser en code %d : %v", CodeValidation, err)
	}
}

// Une machine jamais initialisée — sans clé maître — n'a pas « perdu » sa base :
// la première commande la crée, comme avant (D120).
func TestUneMachineJamaisInitialiseeNEstPasUneBasePerdue(t *testing.T) {
	h := newHarness(t)
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	if out, code := h.run("apply", "--yes"); code == CodeDependency || strings.Contains(out, "sauvegarde") {
		t.Errorf("machine jamais initialisée : code %d :\n%s", code, out)
	}
}

// Sans base, une commande qui écrit refuse avant sa première écriture : « site
// create » déposait sa déclaration, puis l'apply rendait 5 (D120).
func TestSansBaseRienNEstDeclare(t *testing.T) {
	h := parcDeDeux(t)
	base := h.path("var", "lib", "ogs", "state.db")
	for _, f := range []string{base, base + "-wal", base + "-shm"} {
		_ = os.Remove(f)
	}
	for _, args := range [][]string{{"site", "create", "neuf.fr", "--kind", "static", "--yes"},
		{"project", "create", "neuf.fr", "--kind", "static"}} {
		out, code := h.run(args...)
		if code != CodeDependency {
			t.Errorf("%v : code %d, attendu %d :\n%s", args, code, CodeDependency, out)
		}
		if _, err := os.Stat(h.path("etc", "ogs", "projects", "neuffr.yaml")); err == nil {
			t.Fatalf("%v : la déclaration a été écrite sans base d'état", args)
		}
	}
	// Les commandes d'incident tournent sans la base (D120).
	for _, args := range [][]string{{"web", "reload"}, {"template", "extract", "apache/static.conf"},
		{"firewall", "status"}, {"harden", "--dry-run"}} {
		if out, code := h.run(args...); code == CodeDependency {
			t.Errorf("%v : refusée sans base d'état :\n%s", args, out)
		}
	}
	out := h.mustRun("init")
	if strings.Contains(out, "premier projet") {
		t.Errorf("init après recréation ne doit pas inviter à créer un premier projet :\n%s", out)
	}
}

// Un voisin illisible fait échouer le recalcul du parc après une suppression :
// la sortie nomme ce qui désigne encore le projet supprimé, et le remède.
func TestSuppressionAvecUnVoisinRefuseNommeLeRecalculManque(t *testing.T) {
	h := parcDeDeux(t)
	déposer(t, h.path("etc", "ogs", "projects", "casse.yaml"), "metadata: [\n")
	out, code := h.run("project", "delete", "shopfr", "--yes", "--keep-data")
	if code != CodePartial || !strings.Contains(out, "relancer « ogs apply »") {
		t.Errorf("project delete : code %d, attendu %d avec le remède :\n%s", code, CodePartial, out)
	}
}
