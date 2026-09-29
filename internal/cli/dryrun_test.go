package cli

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/hook"
)

// empreinte relève ce que porte le préfixe : chaque entrée avec son type, son
// mode et son contenu — ou sa cible pour un lien —, puis les lignes de chaque
// table de la base d'état. Le verrou global en est écarté : ce n'est pas un état,
// et « apply --dry-run » le prend pour que son calcul lise un système qui ne
// bouge pas. Le journal d'audit n'y entre que sur demande : une consultation
// (« secrets show ») le tient même sous --dry-run, et doit le tenir.
func (h *harness) empreinte(t *testing.T, audit bool) map[string]string {
	t.Helper()
	paths := config.NewPaths(h.prefix)
	// La base est relevée par ses tables, non par ses octets : le journal WAL
	// qu'une connexion laisse ouvert n'est pas un état.
	écartés := map[string]bool{
		paths.GlobalLock():       true,
		paths.StateDB():          true,
		paths.StateDB() + "-wal": true,
		paths.StateDB() + "-shm": true,
	}
	if !audit {
		écartés[paths.AuditLog()] = true
	}
	out := map[string]string{}
	err := filepath.WalkDir(h.prefix, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if écartés[path] {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(h.prefix, path)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "lien → " + target
		case info.IsDir():
			out[rel] = fmt.Sprintf("répertoire %v", info.Mode())
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("fichier %v %x", info.Mode(), sha256.Sum256(data))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("relevé du préfixe : %v", err)
	}
	for table, lignes := range h.tablesDeLaBase(t, audit) {
		out["base:"+table] = lignes
	}
	return out
}

// tablesDeLaBase rend le contenu de chaque table de la base d'état, lignes
// triées : l'ordre de lecture n'est pas un état.
func (h *harness) tablesDeLaBase(t *testing.T, audit bool) map[string]string {
	t.Helper()
	path := config.NewPaths(h.prefix).StateDB()
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("ouverture de %s : %v", path, err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatalf("tables de %s : %v", path, err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	out := map[string]string{}
	for _, table := range tables {
		query := `SELECT * FROM "` + table + `"`
		if !audit {
			switch table {
			case "audit_log":
				continue
			case "sqlite_sequence":
				// Le compteur de l'audit avance avec lui ; ceux des autres tables
				// restent relevés.
				query += ` WHERE name != 'audit_log'`
			}
		}
		r, err := db.Query(query)
		if err != nil {
			t.Fatalf("lecture de %s : %v", table, err)
		}
		cols, _ := r.Columns()
		var lignes []string
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			lignes = append(lignes, fmt.Sprintf("%v", vals))
		}
		r.Close()
		sort.Strings(lignes)
		out[table] = strings.Join(lignes, "\n")
	}
	return out
}

// écarts énumère ce qui diffère entre deux relevés.
func écarts(avant, après map[string]string) []string {
	var out []string
	for k, v := range avant {
		if w, ok := après[k]; !ok {
			out = append(out, "disparu : "+k)
		} else if w != v {
			out = append(out, "modifié : "+k)
		}
	}
	for k := range après {
		if _, ok := avant[k]; !ok {
			out = append(out, "apparu : "+k)
		}
	}
	sort.Strings(out)
	return out
}

// déposerHooks pose un hook de parc pour chaque événement : sa seule présence
// vaut déclaration, et « ↪ hook » dans la sortie dit qu'il a été lancé.
func (h *harness) déposerHooks(t *testing.T) {
	t.Helper()
	dir := h.path("etc", "ogs", "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ev := range hook.Events() {
		if err := os.WriteFile(filepath.Join(dir, ev), []byte("#!/bin/sh\necho "+ev+"\n"), 0o750); err != nil {
			t.Fatal(err)
		}
	}
}

// rienNATouché exige qu'une commande lancée sous --dry-run n'ait rien changé au
// préfixe ni à la base d'état, et n'ait lancé aucun hook.
func (h *harness) rienNATouché(t *testing.T, commande string, avant map[string]string, audit bool, sortie string) {
	t.Helper()
	if diff := écarts(avant, h.empreinte(t, audit)); len(diff) > 0 {
		t.Errorf("« %s » a modifié le système sous --dry-run :\n  %s\n%s",
			commande, strings.Join(diff, "\n  "), sortie)
	}
	// Le Runner ne lance rien sous --dry-run, mais l'annonce : une commande qui
	// simule doit s'arrêter avant d'en arriver là.
	if strings.Contains(sortie, "↪ hook") {
		t.Errorf("« %s » est allée jusqu'à un hook sous --dry-run (D40) :\n%s", commande, sortie)
	}
}

func TestDryRunNeSupprimeNiNeRestaureNiNeRenouvelleRien(t *testing.T) {
	// R3-20 : « project delete --dry-run » supprimait réellement le projet —
	// archive, vhost, home, déclaration, hooks preDelete et postDelete compris —
	// et rendait 0 en affichant « ✓ supprimé ». « project restore » réécrivait
	// la déclaration, appliquait le projet et lançait postCreate ; « cert renew »
	// réémettait le certificat et lançait postCertRenew. Aucune des trois ne
	// sait simuler : elles refusent l'option, avant d'avoir rien touché.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	h.mustRun("project", "create", "cert.fr", "--kind", "static")
	h.mustRun("apply", "certfr", "--yes")
	// Un projet supprimé pour de bon, dont l'archive servira à la restauration.
	h.mustRun("site", "create", "vieux.fr", "--kind", "static", "--tls=false", "--yes")
	h.mustRun("project", "delete", "vieuxfr", "--yes")
	archives, err := os.ReadDir(h.path("var", "lib", "ogs", "archives"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("une archive attendue après la suppression : %v %v", archives, err)
	}
	archive := h.path("var", "lib", "ogs", "archives", archives[0].Name())
	h.déposerHooks(t)

	refuse := func(args ...string) {
		t.Helper()
		commande := strings.Join(args, " ")
		avant := h.empreinte(t, true)
		out, code := h.run(append(args, "--dry-run", "--yes")...)
		if code != CodeUsage {
			t.Errorf("« %s --dry-run » doit refuser l'option (code %d), obtenu %d :\n%s",
				commande, CodeUsage, code, out)
		}
		if !strings.Contains(out, "ne sait pas simuler son effet : --dry-run est refusé") {
			t.Errorf("le refus de « %s --dry-run » doit dire pourquoi :\n%s", commande, out)
		}
		h.rienNATouché(t, commande, avant, true, out)
	}

	refuse("project", "delete", "shopfr")
	refuse("project", "delete", "shopfr", "--purge")
	refuse("project", "delete", "shopfr", "--keep-data")
	for _, f := range []string{
		filepath.Join("etc", "ogs", "projects", "shopfr.yaml"),
		filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"),
		filepath.Join("home", "shop.fr"),
	} {
		if _, err := os.Stat(h.path(f)); err != nil {
			t.Errorf("%s doit survivre à « project delete --dry-run » : %v", f, err)
		}
	}

	refuse("cert", "renew", "certfr", "--force")
	refuse("cert", "renew", "--all", "--force")

	refuse("project", "restore", archive)
	refuse("project", "restore", archive, "--with-data=false")
	for _, f := range []string{
		filepath.Join("etc", "ogs", "projects", "vieuxfr.yaml"),
		filepath.Join("home", "vieux.fr"),
	} {
		if _, err := os.Stat(h.path(f)); !os.IsNotExist(err) {
			t.Errorf("« project restore --dry-run » ne doit pas recréer %s", f)
		}
	}
}

// casDryRun est, pour une commande qui accepte --dry-run, une invocation qui la
// mène jusqu'à sa simulation — non jusqu'à une erreur d'usage, qui ne
// prouverait rien de ce qu'elle aurait écrit.
type casDryRun struct {
	args []string
	// annonce est la preuve, dans la sortie, que la commande qui simule est
	// allée jusqu'au bout de sa simulation.
	annonce string
}

// casDryRunParCommande couvre chaque commande inscrite dans dryRunConducts. Les
// commandes qui refusent l'option n'y figurent pas : leurs arguments importent
// peu, le refus précède tout.
func casDryRunParCommande(secret string) map[string]casDryRun {
	return map[string]casDryRun{
		// Simulations.
		"ogs apply":          {[]string{"apply", "--yes"}, "--dry-run : rien n'a été appliqué"},
		"ogs amqp messenger": {[]string{"amqp", "messenger", "shopfr"}, "vhost: shopfr"},
		"ogs cert issue":     {[]string{"cert", "issue", "certfr"}, "--dry-run : certificat certfr"},
		"ogs cron add": {[]string{"cron", "add", "shopfr", "nettoyage", "--schedule", "0 4 * * *",
			"--command", "bin/console app:nettoyage"}, "--dry-run : tâche nettoyage"},
		"ogs db add":            {[]string{"db", "add", "certfr", "--engine", "postgres"}, "--dry-run : la base postgres"},
		"ogs firewall apply":    {[]string{"firewall", "apply", "--yes"}, "--dry-run : rien n'a été appliqué"},
		"ogs harden":            {[]string{"harden", "--apply", "--yes"}, "--dry-run : rien n'a été restreint"},
		"ogs import adopt":      {[]string{"import", "adopt", "boutiquefr", "--yes"}, "boutiquefr.yaml serait écrit"},
		"ogs mail dkim enable":  {[]string{"mail", "dkim", "enable", "shopfr"}, "--dry-run : la signature DKIM serait demandée"},
		"ogs mail dkim disable": {[]string{"mail", "dkim", "disable", "certfr"}, "--dry-run : la signature DKIM de certfr serait retirée"},
		"ogs php switch":        {[]string{"php", "switch", "shopfr", "8.4"}, "--dry-run : le pool web passerait de PHP 8.5 à PHP 8.4"},
		"ogs project create":    {[]string{"project", "create", "neuf.fr", "--kind", "static"}, "neuffr.yaml serait écrit"},
		"ogs site create": {[]string{"site", "create", "neuf.fr", "--kind", "static", "--tls=false", "--yes"},
			"neuffr.yaml serait écrit"},
		"ogs site clone": {[]string{"site", "clone", "shopfr", "--to", "prep", "--yes"}, "--dry-run : rien n'a été écrit"},

		// Consultations.
		"ogs amqp status":           {args: []string{"amqp", "status"}},
		"ogs app types":             {args: []string{"app", "types"}},
		"ogs cert list":             {args: []string{"cert", "list"}},
		"ogs completion bash":       {args: []string{"completion", "bash"}},
		"ogs completion fish":       {args: []string{"completion", "fish"}},
		"ogs completion powershell": {args: []string{"completion", "powershell"}},
		"ogs completion zsh":        {args: []string{"completion", "zsh"}},
		"ogs cron list":             {args: []string{"cron", "list"}},
		"ogs db list":               {args: []string{"db", "list"}},
		"ogs diff":                  {args: []string{"diff"}},
		"ogs doctor":                {args: []string{"doctor"}},
		"ogs firewall plan":         {args: []string{"firewall", "plan"}},
		"ogs firewall profiles":     {args: []string{"firewall", "profiles"}},
		"ogs firewall rules":        {args: []string{"firewall", "rules"}},
		"ogs firewall sources":      {args: []string{"firewall", "sources"}},
		"ogs firewall status":       {args: []string{"firewall", "status"}},
		"ogs firewall whois":        {args: []string{"firewall", "whois", "203.0.113.10"}},
		"ogs help":                  {args: []string{"help", "project", "delete"}},
		"ogs import scan":           {args: []string{"import", "scan"}},
		"ogs import unmanaged":      {args: []string{"import", "unmanaged"}},
		"ogs mail dkim records":     {args: []string{"mail", "dkim", "records", "certfr"}},
		"ogs mail dkim show":        {args: []string{"mail", "dkim", "show", "certfr"}},
		"ogs my status":             {args: []string{"my", "status"}},
		"ogs php list":              {args: []string{"php", "list"}},
		"ogs php ports":             {args: []string{"php", "ports"}},
		"ogs plan":                  {args: []string{"plan"}},
		"ogs project archives":      {args: []string{"project", "archives"}},
		"ogs project export":        {args: []string{"project", "export", "shopfr"}},
		"ogs project list":          {args: []string{"project", "list"}},
		"ogs project show":          {args: []string{"project", "show", "shopfr"}},
		"ogs runtime list":          {args: []string{"runtime", "list"}},
		"ogs secrets env":           {args: []string{"secrets", "env", "shopfr"}},
		"ogs secrets list":          {args: []string{"secrets", "list", "certfr"}},
		"ogs secrets show":          {args: []string{"secrets", "show", "certfr", secret}},
		"ogs service list":          {args: []string{"service", "list"}},
		"ogs service logs":          {args: []string{"service", "logs", "shopfr", "messenger-async"}},
		"ogs status":                {args: []string{"status"}},
		"ogs template list":         {args: []string{"template", "list"}},
		"ogs template show":         {args: []string{"template", "show", "logrotate/ogs.conf"}},
		"ogs user key show":         {args: []string{"user", "key", "show", "shopfr"}},
		"ogs version":               {args: []string{"version"}},
		"ogs web engine":            {args: []string{"web", "engine"}},
		"ogs web test":              {args: []string{"web", "test"}},
	}
}

// argumentsFactices rend le plus petit nombre d'arguments que la commande
// accepte : cobra les valide avant le refus de --dry-run, qui doit pourtant être
// atteint quelle que soit la commande.
func argumentsFactices(cmd *cobra.Command) []string {
	for n := 0; n <= 4; n++ {
		args := make([]string, n)
		for i := range args {
			args[i] = "shopfr"
		}
		if cmd.ValidateArgs(args) == nil {
			return args
		}
	}
	return nil
}

func TestDryRunChaqueCommandeSimuleOuRefuse(t *testing.T) {
	// La règle de --dry-run, commande par commande, sur l'arbre entier : une
	// commande qui sait simuler va jusqu'au bout de sa simulation sans rien
	// toucher — ni fichier, ni base d'état, ni hook ; une consultation ne change
	// rien ; toute autre commande refuse l'option (code 2), avant d'avoir rien
	// touché. Une commande ajoutée plus tard tombe d'elle-même dans la dernière
	// catégorie, jusqu'à ce qu'on lui apprenne à simuler.
	// Les projets du parc d'essai sont appliqués : le calcul du plan d'un projet
	// qui ne l'a jamais été lui attribue ses numéros dans la base d'état — la
	// réserve que nomme dryRunConducts.
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--service", "symfony-messenger:async:1", "--cron", "0 3 * * *:bin/console app:purge",
		"--tls=false", "--yes")
	h.mustRunPartial("site", "create", "cert.fr", "--kind", "static", "--yes")
	h.mustRun("mail", "dkim", "enable", "certfr")
	h.mustRunPartial("apply", "certfr", "--yes")
	// Après les créations, pour qu'aucune ne les applique : ufw.yaml, et un site
	// tenu à la main que l'import peut adopter.
	h.ecrireUFW(ufwEssai)
	h.legacyServer(t)
	h.déposerHooks(t)
	secret := ""
	for _, line := range strings.Split(h.mustRun("secrets", "list", "certfr"), "\n") {
		if strings.HasPrefix(line, "dkim/") {
			secret = strings.TrimSpace(line)
		}
	}
	if secret == "" {
		t.Fatal("la clé DKIM de certfr doit être scellée : « secrets show » n'aurait rien à lire")
	}

	root := NewRootCommand()
	// L'aide et la complétion sont ajoutées par cobra à l'exécution : elles
	// n'échappent pas pour autant à la règle.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	commandes := map[string]*cobra.Command{}
	for _, c := range walk(root) {
		if c.Runnable() {
			commandes[c.CommandPath()] = c
		}
	}
	for path := range dryRunConducts {
		if commandes[path] == nil {
			t.Errorf("dryRunConducts inscrit « %s », qui n'est pas une commande", path)
		}
	}
	cas := casDryRunParCommande(secret)
	for path := range cas {
		if cmd := commandes[path]; cmd == nil || dryRunConductOf(cmd) == dryRunRefused {
			t.Errorf("« %s » a un cas d'essai sans être inscrit dans dryRunConducts", path)
		}
	}

	chemins := make([]string, 0, len(commandes))
	for path := range commandes {
		chemins = append(chemins, path)
	}
	sort.Strings(chemins)
	for _, path := range chemins {
		cmd := commandes[path]
		conduite := dryRunConductOf(cmd)
		c, ok := cas[path]
		if conduite != dryRunRefused && !ok {
			t.Errorf("« %s » accepte --dry-run sans cas qui l'éprouve : l'ajouter à casDryRunParCommande", path)
			continue
		}
		t.Run(path, func(t *testing.T) {
			args := c.args
			if conduite == dryRunRefused {
				args = append(strings.Fields(path)[1:], argumentsFactices(cmd)...)
				// --yes : une commande qui passerait outre le refus ne s'arrêterait
				// pas non plus à sa confirmation.
				args = append(args, "--yes")
			}
			avant := h.empreinte(t, conduite != dryRunReadOnly)
			out, code := h.run(append(args, "--dry-run")...)
			switch conduite {
			case dryRunRefused:
				if code != CodeUsage || !strings.Contains(out, "--dry-run est refusé") {
					t.Errorf("« %s » ne sait pas simuler : --dry-run doit être refusé (code %d), obtenu %d :\n%s",
						path, CodeUsage, code, out)
				}
			case dryRunSimulated:
				if code != CodeOK || !strings.Contains(out, c.annonce) {
					t.Errorf("« %s --dry-run » doit aller jusqu'à sa simulation (code 0, « %s »), obtenu %d :\n%s",
						strings.Join(args, " "), c.annonce, code, out)
				}
			case dryRunReadOnly:
				if strings.Contains(out, "--dry-run est refusé") {
					t.Errorf("« %s » est une consultation : --dry-run y est sans objet :\n%s", path, out)
				}
			}
			h.rienNATouché(t, strings.Join(args, " "), avant, conduite != dryRunReadOnly, out)
		})
	}
}
