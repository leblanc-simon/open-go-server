package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/provider/firewall"
)

// runSplit exécute une commande et rend séparément ses deux sorties : ce qui
// doit rester un document machine sur la sortie standard, et ce qui passe sur
// la sortie d'erreur.
func (h *harness) runSplit(args ...string) (stdout, stderr string, code int) {
	h.t.Helper()
	var out, errBuf bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(h.stdin))
	h.stdin = ""
	root.SetArgs(append([]string{"--prefix", h.prefix}, args...))
	err := root.Execute()
	if err != nil {
		errBuf.WriteString("ogs : " + err.Error() + "\n")
	}
	return out.String(), errBuf.String(), ExitCode(err)
}

// Q-110 : un avertissement d'apply n'est jamais tu. Sous --quiet comme sous
// -o json, il passe sur la sortie d'erreur, et la sortie standard reste vide ou
// reste un document valide.
func TestLesAvertissementsNeSontJamaisTus(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	// Le mode préfixé n'a pas de moteur MySQL : la base produit un avertissement.
	h.mustRunPartial("project", "create", "shop.fr", "--db", "mysql")
	for _, opts := range [][]string{{"-q"}, {"-o", "json"}, {"-o", "yaml"}} {
		stdout, stderr, code := h.runSplit(append([]string{"apply", "shopfr", "--yes"}, opts...)...)
		if code != CodePartial {
			t.Fatalf("%v : code %d, 6 attendu\n%s%s", opts, code, stdout, stderr)
		}
		if !strings.Contains(stderr, "⚠") {
			t.Errorf("%v : l'avertissement doit atteindre la sortie d'erreur :\n%s", opts, stderr)
		}
		if strings.Contains(stdout, "⚠") {
			t.Errorf("%v : l'avertissement ne doit pas polluer la sortie standard :\n%s", opts, stdout)
		}
		if opts[0] == "-o" && opts[1] == "json" && strings.TrimSpace(stdout) != "" && !json.Valid([]byte(stdout)) {
			t.Errorf("sortie json invalide :\n%s", stdout)
		}
	}
	// En mode table, il reste dans le fil de la sortie.
	stdout, _, _ := h.runSplit("apply", "shopfr", "--yes")
	if !strings.Contains(stdout, "⚠") {
		t.Errorf("en mode table, l'avertissement suit la sortie :\n%s", stdout)
	}
}

// Q-110 : l'annonce du retour automatique d'ufw est la seule chose de l'apply
// que l'exploitant ne peut pas manquer.
func TestLAnnonceDuRetourAutomatiqueDUfwNEstJamaisTue(t *testing.T) {
	for _, app := range []*App{{Quiet: true, Format: "table"}, {Format: "json"}} {
		var out, errBuf bytes.Buffer
		app.Out, app.Err = &out, &errBuf
		app.announceRollback(&firewall.Provider{Armed: 5 * time.Minute})
		if !strings.Contains(errBuf.String(), "ogs firewall confirm") {
			t.Errorf("quiet=%v format=%s : annonce tue (stdout %q, stderr %q)", app.Quiet, app.Format,
				out.String(), errBuf.String())
		}
		if out.Len() != 0 {
			t.Errorf("format=%s : la sortie standard doit rester au document : %q", app.Format, out.String())
		}
	}
}

// Q-69 : une sous-commande inconnue est une erreur d'usage (code 2), avec une
// suggestion, jamais l'aide et le code 0 — « ogs db dmup shopfr > dump.sql »
// écrivait l'aide dans la sauvegarde et rendait 0.
func TestUneSousCommandeInconnueEstUneErreurDUsage(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr")
	cas := map[string]string{
		"project delet shopfr --yes": "delete",
		"service restrat shopfr":     "restart",
		"db dmup shopfr":             "dump",
		"cert renw":                  "renew",
		"frobnicate":                 "",
		"user key shw shopfr":        "show",
	}
	for ligne, suggestion := range cas {
		stdout, stderr, code := h.runSplit(strings.Fields(ligne)...)
		if code != CodeUsage {
			t.Errorf("« ogs %s » : code %d, 2 attendu\n%s%s", ligne, code, stdout, stderr)
			continue
		}
		if strings.Contains(stdout, "Utilisation") {
			t.Errorf("« ogs %s » : l'aide ne doit pas tenir lieu de résultat :\n%s", ligne, stdout)
		}
		if !strings.Contains(stderr, "inconnue") || (suggestion != "" && !strings.Contains(stderr, suggestion)) {
			t.Errorf("« ogs %s » : message attendu avec « %s » :\n%s", ligne, suggestion, stderr)
		}
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfr.yaml")); err != nil {
		t.Errorf("la déclaration doit rester : %v", err)
	}
	// Un groupe sans argument affiche toujours son aide, en code 0.
	for _, args := range [][]string{{"project"}, {"project", "--dry-run"}} {
		if out, code := h.run(args...); code != CodeOK || !strings.Contains(out, "Utilisation") {
			t.Errorf("« ogs %s » : aide attendue en code 0, obtenu %d :\n%s", strings.Join(args, " "), code, out)
		}
	}
}

// positionnels compte les arguments qu'annonce la ligne d'utilisation d'une
// commande, hors options et valeurs d'options, et dit s'ils sont en nombre
// libre (« [id...] »).
func positionnels(use string) (n int, variadique bool) {
	mots := strings.Fields(use)
	for i := 1; i < len(mots); i++ {
		m := mots[i]
		if strings.HasPrefix(m, "-") || strings.HasPrefix(m, "[-") {
			// La valeur d'une option la suit : « --user <compte> ».
			if i+1 < len(mots) && strings.HasPrefix(mots[i+1], "<") && !strings.Contains(m, "=") {
				i++
			}
			continue
		}
		n++
		if strings.Contains(m, "...") {
			variadique = true
		}
	}
	return n, variadique
}

// Q-69 : sur l'arbre entier, toute commande feuille refuse en code 2 un
// argument de plus que n'en annonce sa ligne d'utilisation — « ogs project list
// extra » listait tout le parc, « ogs harden extra » traitait toutes les cibles.
// Une commande ajoutée plus tard sans validateur tombe sous la règle.
func TestChaqueCommandeRefuseLesArgumentsEnTrop(t *testing.T) {
	root := NewRootCommand()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	for _, c := range walk(root) {
		if !c.Runnable() || c.HasSubCommands() || c.Hidden || c.Name() == "help" {
			continue
		}
		if c.Args == nil {
			t.Errorf("« %s » n'a pas de validateur d'arguments", c.CommandPath())
			continue
		}
		n, variadique := positionnels(c.Use)
		if variadique {
			continue
		}
		trop := make([]string, n+1)
		for i := range trop {
			trop[i] = "x"
		}
		err := c.ValidateArgs(trop)
		if ExitCode(err) != CodeUsage {
			t.Errorf("« %s » (« %s ») : %d argument(s) doivent être refusés en code 2, obtenu %v (code %d)",
				c.CommandPath(), c.Use, n+1, err, ExitCode(err))
		}
	}
}

// Q-69 : de bout en bout, un argument en trop arrête la commande avant tout
// effet, et le message est en français.
func TestUnArgumentEnTropArreteLaCommande(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr")
	for _, ligne := range []string{"project list extra", "project show shopfr extra", "harden extra"} {
		stdout, stderr, code := h.runSplit(strings.Fields(ligne)...)
		if code != CodeUsage {
			t.Errorf("« ogs %s » : code %d, 2 attendu\n%s%s", ligne, code, stdout, stderr)
		}
		if strings.Contains(stderr, "accepts") || strings.Contains(stderr, "unknown") {
			t.Errorf("« ogs %s » : message anglais :\n%s", ligne, stderr)
		}
	}
}

// Q-66 : « service logs » va jusqu'à journalctl — sous --prefix, au Recorder —,
// ne vise qu'une unité du projet, et une interruption rend le code 9.
func TestServiceLogsNeViseQueLesUnitesDuProjet(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--service", "symfony-messenger:async:1", "--yes")
	if out, code := h.run("service", "logs", "shopfr", "messenger-async"); code != CodeOK {
		t.Errorf("service déclaré : code %d, 0 attendu\n%s", code, out)
	}
	for _, nom := range []string{"inconnu", "*", "messenger-async@1", "messenger-*"} {
		if out, code := h.run("service", "logs", "shopfr", nom); code != CodeUsage {
			t.Errorf("« %s » : code %d, 2 attendu\n%s", nom, code, out)
		}
	}
	if code := ExitCode(logsErr(fmt.Errorf("journalctl : %w", context.Canceled))); code != CodeCancelled {
		t.Errorf("Ctrl-C : code %d, 9 attendu", code)
	}
}

// Q-34 : une clé inconnue de config.yaml n'est jamais perdue en silence. Une
// commande qui écrit la refuse (code 3) ; une consultation et le renouvellement
// des certificats — le timer — l'annoncent sur la sortie d'erreur et
// continuent ; doctor la compte en échec. Une clé retirée n'arrête rien.
func TestUneCleInconnueDeLaConfigurationNEstJamaisTue(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr")
	cfg := h.path("etc", "ogs", "config.yaml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// La clé mal orthographiée prend place dans une section existante.
	text := strings.Replace(string(data), "archives:\n", "archives:\n  retentionDay: 3\n", 1)
	if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ligne := range []string{"apply shopfr --yes", "project create blog.fr", "harden --apply --yes"} {
		_, stderr, code := h.runSplit(strings.Fields(ligne)...)
		if code != CodeValidation || !strings.Contains(stderr, "archives.retentionDay") {
			t.Errorf("« ogs %s » : code %d, 3 attendu avec la clé nommée :\n%s", ligne, code, stderr)
		}
	}
	for _, ligne := range []string{"project list", "cert renew --all", "plan shopfr"} {
		_, stderr, code := h.runSplit(strings.Fields(ligne)...)
		if code == CodeValidation || !strings.Contains(stderr, "archives.retentionDay") {
			t.Errorf("« ogs %s » : la clé doit être annoncée sans arrêter la commande (code %d) :\n%s",
				ligne, code, stderr)
		}
	}
	out, _ := h.run("doctor")
	if !strings.Contains(out, "archives.retentionDay") {
		t.Errorf("doctor doit nommer la clé inconnue :\n%s", out)
	}
	// Une clé qu'une version antérieure d'« ogs init » écrivait n'arrête rien.
	text = strings.Replace(string(data), "  prep:\n", "  prep:\n    basicAuth: false\n", 1)
	if text == string(data) {
		t.Fatalf("section prep introuvable :\n%s", data)
	}
	if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := h.runSplit("apply", "shopfr", "--yes"); code == CodeValidation {
		t.Errorf("une clé retirée ne doit pas refuser l'apply :\n%s", stderr)
	}
}

// Q-34 : une clé inconnue du fichier d'identifiants RabbitMQ est nommée, sans
// priver les commandes du broker.
func TestUneCleInconnueDesIdentifiantsAMQPEstNommee(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	dir := h.path("etc", "ogs", "credentials")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := "url: http://127.0.0.1:15672\nuser: admin\npassword: x\namqpport: 5673\n"
	if err := os.WriteFile(h.path("etc", "ogs", "credentials", "amqp.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	app := newApp()
	var errBuf bytes.Buffer
	app.Err, app.Paths = &errBuf, config.NewPaths(h.prefix)
	creds, err := app.amqpCredentials()
	if err != nil || creds.User != "admin" {
		t.Fatalf("le fichier doit rester lu : %+v %v", creds, err)
	}
	if !strings.Contains(errBuf.String(), "amqpport") {
		t.Errorf("la clé inconnue doit être nommée : %q", errBuf.String())
	}
	// Un jeu DNS dont « zones » est mal orthographié ne serait choisi pour
	// aucune zone (D97) : doctor le nomme, sans l'écarter.
	if err := os.MkdirAll(h.path("etc", "ogs", "credentials", "dns"), 0o700); err != nil {
		t.Fatal(err)
	}
	jeu := "provider: ovh\nzone: [exemple.fr]\nclientId: a\nclientSecret: b\n"
	if err := os.WriteFile(h.path("etc", "ogs", "credentials", "dns", "client-a.yaml"), []byte(jeu), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := h.run("doctor")
	for _, cle := range []string{"clé inconnue « amqpport »", "clé inconnue « zone »"} {
		if !strings.Contains(out, cle) {
			t.Errorf("doctor doit nommer « %s » :\n%s", cle, out)
		}
	}
}

// D101 : un nom nu résolu par compatibilité dans le répertoire de travail est
// signalé par l'apply (code 6) et par doctor, qui propose « ./<nom> ».
func TestUnNomNuDeCronResoluParCompatibiliteEstSignale(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	if err := os.WriteFile(h.path("home", "shop.fr", "www", "backup.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.mustRun("cron", "add", "shopfr", "sauv", "--schedule", "0 2 * * *", "--command", "backup.sh --dest /srv/x")
	out, code := h.run("apply", "shopfr", "--yes")
	if code != CodePartial || strings.Count(out, "écrire « ./backup.sh »") != 1 {
		t.Errorf("apply : code %d, 6 attendu avec une seule fois « ./backup.sh » :\n%s", code, out)
	}
	if out, _ := h.run("doctor"); !strings.Contains(out, "./backup.sh") {
		t.Errorf("doctor doit proposer « ./backup.sh » :\n%s", out)
	}
}

// Une étape non critique en échec ne s'annonce qu'une fois, par « ⚠ » ; une
// étape critique, par « ✗ » ; une étape aboutie en partie, par « ✓ … (en partie) ».
func TestUneEtapeNonCritiqueNeSAnnonceQuUneFois(t *testing.T) {
	var out bytes.Buffer
	app := &App{Out: &out, Err: &out, Format: "table"}
	s := engine.NewStep("x", engine.Update, "étape")
	s.Critical = false
	app.stepf("  ", s, errors.New("souci"))
	if out.Len() != 0 {
		t.Errorf("non critique : rien avant OnWarning, obtenu %q", out.String())
	}
	s.Critical = true
	app.stepf("  ", s, errors.New("souci"))
	if !strings.Contains(out.String(), "✗ étape : souci") {
		t.Errorf("critique : %q", out.String())
	}
}
