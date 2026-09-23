package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

func TestSignatureDKIMDeBoutEnBout(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("project", "create", "blog.fr", "--kind", "static")
	h.mustRun("mail", "dkim", "enable", "shopfr")
	h.mustRun("mail", "dkim", "enable", "blogfr")

	// « plan » n'engendre rien : une clé tirée au plan serait perdue si l'on
	// renonçait à l'apply.
	if out := h.mustRun("plan", "shopfr"); !strings.Contains(out, "clé DKIM de shop.fr") {
		t.Errorf("le plan doit annoncer la clé :\n%s", out)
	}
	if out := h.mustRun("mail", "dkim", "show", "shopfr"); !strings.Contains(out, "aucune clé") {
		t.Errorf("« plan » ne doit rien sceller :\n%s", out)
	}

	// « apply shopfr » ne vise qu'un projet, mais les tables portent tout le parc :
	// les établir sur le seul projet visé retirerait blog.fr de la signature.
	h.mustRun("apply", "blogfr", "--yes")
	h.mustRun("apply", "shopfr", "--yes")
	signing := h.read(t, "etc/opendkim/ogs/SigningTable")
	for _, want := range []string{"*@shop.fr ogs", "*@blog.fr ogs"} {
		if !strings.Contains(signing, want) {
			t.Errorf("SigningTable sans %q :\n%s", want, signing)
		}
	}

	out := h.mustRun("--output", "json", "mail", "dkim", "records", "shopfr")
	var env struct {
		Items []struct {
			Name, Value, State string
			Strings            []string
		}
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("sortie JSON invalide : %v\n%s", err, out)
	}
	if len(env.Items) != 1 || !strings.HasSuffix(env.Items[0].Name, "._domainkey.shop.fr.") ||
		!strings.HasPrefix(env.Items[0].Value, "v=DKIM1; k=rsa; p=") || len(env.Items[0].Strings) < 2 {
		t.Fatalf("enregistrement inattendu : %+v", env.Items)
	}

	// Rotation : la clé préparée n'est pas désignée tant que la bascule n'est pas
	// faite, et la bascule refuse un enregistrement qu'elle n'a pas pu vérifier.
	h.mustRun("mail", "dkim", "rotate", "shopfr")
	if _, code := h.run("mail", "dkim", "rotate", "shopfr"); code != CodeConflict {
		t.Errorf("une seconde rotation doit rendre le code %d, obtenu %d", CodeConflict, code)
	}
	if got := h.read(t, "etc/opendkim/ogs/SigningTable"); got != signing {
		t.Errorf("la rotation ne doit pas toucher aux tables :\n%s", got)
	}
	if out, code := h.run("mail", "dkim", "activate", "shopfr"); code != CodeDependency {
		t.Errorf("une bascule non vérifiée doit rendre le code %d, obtenu %d\n%s", CodeDependency, code, out)
	}
	h.mustRun("mail", "dkim", "activate", "shopfr", "--force", "--yes")
	after := h.read(t, "etc/opendkim/ogs/SigningTable")
	if !strings.Contains(after, "*@shop.fr ogs202") || !strings.Contains(after, "*@blog.fr ogs") ||
		after == signing {
		t.Errorf("la bascule doit désigner la nouvelle clé, sans retirer le voisin :\n%s", after)
	}

	// Suppression : les fichiers de clés partent, les tables sont recalculées, et
	// l'enregistrement à retirer est nommé — ogs ne l'a pas publié.
	out = h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if !strings.Contains(out, "enregistrement DKIM à retirer du DNS") {
		t.Errorf("la suppression doit nommer les enregistrements à retirer :\n%s", out)
	}
	if _, err := os.Stat(h.path("etc", "opendkim", "keys", "shopfr")); !os.IsNotExist(err) {
		t.Error("les clés du projet supprimé doivent disparaître")
	}
	final := h.read(t, "etc/opendkim/ogs/SigningTable")
	if strings.Contains(final, "shop.fr") || !strings.Contains(final, "blog.fr") {
		t.Errorf("les tables doivent oublier le projet supprimé, et lui seul :\n%s", final)
	}
}

func TestSignatureDKIMRefuseeEnPreproductionParLaCLI(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static", "--env", "prep")
	if out, code := h.run("mail", "dkim", "enable", "shopfrprep"); code != CodeValidation {
		t.Errorf("code %d attendu en préproduction, obtenu %d\n%s", CodeValidation, code, out)
	}
}

// Toute intention de rechargement émise par un plan doit être routée : une cible
// inconnue n'est qu'un avertissement, et le rechargement n'a pas lieu. L'intention
// nue « web » du provider système est passée ainsi inaperçue depuis le premier
// lot — chaque création de projet se soldait par le code 6.
func TestToutesLesIntentionsDeRechargementSontRoutees(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4")
	h.mustRun("mail", "dkim", "enable", "shopfr")
	h.mustRun("cron", "add", "shopfr", "purge", "--schedule", "0 3 * * *", "--command", "bin/console app:purge")

	app := newApp()
	app.Prefix, app.Quiet, app.Out, app.Err = h.prefix, true, io.Discard, io.Discard
	if err := app.setup(); err != nil {
		t.Fatal(err)
	}
	if err := app.openStore(); err != nil {
		t.Fatal(err)
	}
	defer app.close()
	provs, err := app.buildProviders()
	if err != nil {
		t.Fatal(err)
	}
	defer provs.close()
	// Le mode préfixé tait l'appartenance de www-data au groupe du projet : c'est
	// précisément l'étape dont l'intention était perdue.
	provs.System.SkipWebUser = false

	p, err := app.loadProject("shopfr")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plan, err := app.buildPlan(ctx, provs, p)
	if err != nil {
		t.Fatal(err)
	}
	global, err := app.buildGlobalPlan(ctx, provs, []*model.Project{p})
	if err != nil {
		t.Fatal(err)
	}
	reload := app.reloadFunc(provs)
	seen := map[string]bool{}
	for _, s := range append(plan.Mutating(), global.Mutating()...) {
		for _, target := range s.Reload {
			if seen[target] {
				continue
			}
			seen[target] = true
			if err := reload(ctx, target); err != nil && strings.Contains(err.Error(), "inconnue") {
				t.Errorf("%s : intention %q non routée", s.Resource, target)
			}
		}
	}
	for _, want := range []string{"web:apache", "mail:opendkim", "systemd:tmpfiles"} {
		if !seen[want] {
			t.Errorf("intention %q attendue parmi %v", want, seen)
		}
	}
}
