package importer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/db"
	"leblanc.io/open-go-server/internal/provider/web"
	"leblanc.io/open-go-server/internal/spec"
)

// legacyServer construit un serveur « configuré à la main », tel qu'on en
// reprend en exploitation.
func legacyServer(t *testing.T) (*Scanner, string) {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	eng, err := web.NewEngine("apache", prefix)
	if err != nil {
		t.Fatal(err)
	}
	mkdir := func(path string) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		mkdir(filepath.Dir(path))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir(eng.AvailableDir())
	mkdir(eng.EnabledDir())

	// Un site Symfony servi depuis /var/www, avec son pool et son compte.
	write(filepath.Join(eng.AvailableDir(), "boutique.conf"), `
<VirtualHost *:80>
    ServerName boutique.fr
    ServerAlias www.boutique.fr
    DocumentRoot /var/www/boutique/public
    <FilesMatch \.php$>
        SetHandler "proxy:unix:/run/php/php8.1-fpm-boutique.sock|fcgi://localhost"
    </FilesMatch>
</VirtualHost>
`)
	if err := os.Symlink(filepath.Join(eng.AvailableDir(), "boutique.conf"),
		filepath.Join(eng.EnabledDir(), "boutique.conf")); err != nil {
		t.Fatal(err)
	}
	// Un second site, statique et sans pool.
	write(filepath.Join(eng.AvailableDir(), "vitrine.conf"), `
<VirtualHost *:80>
    ServerName vitrine.fr
    DocumentRoot /var/www/vitrine
</VirtualHost>
`)
	// Un vhost déjà géré par ogs : il ne doit pas être proposé à l'adoption.
	write(filepath.Join(eng.AvailableDir(), "dejagere.conf"),
		"# Généré par ogs — NE PAS MODIFIER À LA MAIN\n<VirtualHost *:80>\n  ServerName dejagere.fr\n</VirtualHost>\n")

	// Pools PHP.
	for _, v := range []string{"8.1"} {
		mkdir(paths.PHPPoolDir(v))
		for _, bin := range []string{paths.PHPBin(v), paths.PHPFpmBin(v)} {
			write(bin, "#!/bin/sh\n")
		}
	}
	write(paths.PHPPoolFile("8.1", "boutique"), `
[boutique]
user = boutique
group = boutique
listen = /run/php/php8.1-fpm-boutique.sock
pm = dynamic
`)
	// Le pool de la distribution ne doit pas être adopté.
	write(paths.PHPPoolFile("8.1", "www"), "[www]\nuser = www-data\nlisten = /run/php/php8.1-fpm.sock\n")

	// Comptes système.
	write(filepath.Join(prefix, "etc", "passwd"), strings.Join([]string{
		"root:x:0:0:root:/root:/bin/bash",
		"www-data:x:33:33:www-data:/var/www:/usr/sbin/nologin",
		"boutique:x:1001:1001::/var/www/boutique:/bin/bash",
		"humain:x:1002:1002:Un humain:/home/humain:/bin/bash",
		"gere:x:1003:1003:ogs=gere:/home/gere.fr:/usr/sbin/nologin",
	}, "\n")+"\n")

	// Tâches et unités.
	write(filepath.Join(paths.CronDir(), "boutique-cron"),
		"0 3 * * * boutique /usr/bin/php8.1 /var/www/boutique/bin/console app:clean\n")
	write(filepath.Join(paths.SystemdDir(), "boutique-worker.service"),
		"[Service]\nUser=boutique\nExecStart=/usr/bin/php8.1 /var/www/boutique/bin/console messenger:consume\n")

	// Certificat certbot.
	mkdir(filepath.Join(prefix, "etc", "letsencrypt", "live", "boutique.fr"))

	return &Scanner{Paths: paths, Engine: eng, HomeRoots: []string{"/var/www", "/home"}}, prefix
}

func TestScanFindsResources(t *testing.T) {
	scanner, _ := legacyServer(t)
	resources, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	counts := Summary(resources)
	if counts[KindVHost] != 3 {
		t.Errorf("%d vhosts, 3 attendus", counts[KindVHost])
	}
	if counts[KindPool] != 2 {
		t.Errorf("%d pools, 2 attendus", counts[KindPool])
	}
	if counts[KindUser] < 2 {
		t.Errorf("%d comptes, au moins 2 attendus", counts[KindUser])
	}
	if counts[KindCron] != 1 || counts[KindUnit] != 1 || counts[KindCertificate] != 1 {
		t.Errorf("tâches %d, unités %d, certificats %d",
			counts[KindCron], counts[KindUnit], counts[KindCertificate])
	}
	// Le vhost déjà géré est repéré comme tel.
	found := false
	for _, r := range resources {
		if r.Kind == KindVHost && r.Name == "dejagere" {
			found = Managed(r)
		}
	}
	if !found {
		t.Error("un vhost généré par ogs doit être marqué comme géré")
	}
	// Les attributs exploitables sont extraits.
	for _, r := range resources {
		if r.Kind == KindVHost && r.Name == "boutique" {
			if r.Attr("docroot") != "/var/www/boutique/public" {
				t.Errorf("docroot : %q", r.Attr("docroot"))
			}
			if r.Attr("fpm") != "/run/php/php8.1-fpm-boutique.sock" {
				t.Errorf("socket FPM : %q", r.Attr("fpm"))
			}
			if r.Attr("domains") != "boutique.fr,www.boutique.fr" {
				t.Errorf("domaines : %q", r.Attr("domains"))
			}
			if r.Attr("enabled") != "true" {
				t.Error("le vhost lié dans sites-enabled doit être marqué actif")
			}
		}
	}
}

func TestScanIsReadOnly(t *testing.T) {
	// Une inspection doit pouvoir être lancée sur un serveur de production sans
	// rien y modifier.
	scanner, prefix := legacyServer(t)
	before := snapshotTree(t, prefix)
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := snapshotTree(t, prefix)
	if before != after {
		t.Error("le scan a modifié le système")
	}
}

func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		b.WriteString(path)
		b.WriteString(info.Mode().String())
		if info.Mode().IsRegular() {
			b.WriteString(string(rune(info.Size())))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestCorrelateGroupsBySocketAndUser(t *testing.T) {
	scanner, _ := legacyServer(t)
	resources, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	candidates, unmanaged := Correlate(resources, "/home", "preprod-pp.net")
	if len(candidates) != 2 {
		t.Fatalf("%d candidats, 2 attendus : %+v", len(candidates), candidates)
	}
	var boutique *Candidate
	for i := range candidates {
		if candidates[i].Host == "boutique.fr" {
			boutique = &candidates[i]
		}
	}
	if boutique == nil {
		t.Fatalf("candidat boutique.fr absent : %+v", candidates)
	}
	// Le socket FPM désigne le pool sans ambiguïté, et le pool donne l'utilisateur.
	if boutique.PHPVersion != "8.1" {
		t.Errorf("version PHP : %q", boutique.PHPVersion)
	}
	if boutique.User != "boutique" {
		t.Errorf("utilisateur : %q", boutique.User)
	}
	if boutique.Home != "/var/www/boutique" {
		t.Errorf("racine : %q", boutique.Home)
	}
	if boutique.ID != "boutiquefr" {
		t.Errorf("identifiant proposé : %q", boutique.ID)
	}
	if boutique.Confidence != ConfidenceHigh {
		t.Errorf("confiance : %q", boutique.Confidence)
	}
	if boutique.Kind != model.KindSymfony {
		t.Errorf("type deviné : %q", boutique.Kind)
	}
	// Les tâches, unités et certificats de l'utilisateur sont rattachés.
	kinds := map[Kind]int{}
	for _, r := range boutique.Resources {
		kinds[r.Kind]++
	}
	for _, kind := range []Kind{KindVHost, KindPool, KindUser, KindCron, KindUnit, KindCertificate} {
		if kinds[kind] == 0 {
			t.Errorf("ressource %s non rattachée : %+v", kind, boutique.Resources)
		}
	}
	// Le certificat certbot est signalé comme un point à trancher.
	if !containsSubstring(boutique.Uncertain, "certbot") {
		t.Errorf("le certificat certbot doit être signalé : %v", boutique.Uncertain)
	}

	// Le compte humain et le pool de la distribution restent non gérés.
	var names []string
	for _, r := range unmanaged {
		names = append(names, r.Name)
	}
	for _, want := range []string{"humain", "www"} {
		if !contains(names, want) {
			t.Errorf("%s devrait rester non géré : %v", want, names)
		}
	}
	// Et rien de ce qui est déjà géré ne réapparaît.
	if contains(names, "dejagere") || contains(names, "gere") {
		t.Errorf("une ressource déjà gérée ne doit pas figurer parmi les non gérées : %v", names)
	}
}

func TestCorrelateStaticSiteHasLowerConfidence(t *testing.T) {
	scanner, _ := legacyServer(t)
	resources, _ := scanner.Scan(context.Background())
	candidates, _ := Correlate(resources, "/home", "preprod-pp.net")
	for _, c := range candidates {
		if c.Host != "vitrine.fr" {
			continue
		}
		// Sans pool ni compte, la déduction est fragile et doit le dire.
		if c.Confidence == ConfidenceHigh {
			t.Errorf("un site sans pool ni compte ne peut pas être « élevé » : %+v", c)
		}
		if len(c.Uncertain) == 0 {
			t.Error("les incertitudes doivent être énoncées")
		}
		if c.Kind != model.KindStatic {
			t.Errorf("sans pool PHP, le site doit être statique : %q", c.Kind)
		}
		return
	}
	t.Fatal("candidat vitrine.fr absent")
}

func TestAdoptKeepsRealNames(t *testing.T) {
	// L'adoption décrit l'existant : renommer une base ou un compte exigerait de
	// les recréer, ce qui n'est pas une opération d'import.
	scanner, _ := legacyServer(t)
	resources, _ := scanner.Scan(context.Background())
	candidates, _ := Correlate(resources, "/home", "preprod-pp.net")
	var boutique Candidate
	for _, c := range candidates {
		if c.Host == "boutique.fr" {
			boutique = c
		}
	}
	p, notes, err := Adopt(boutique, "/home", "preprod-pp.net")
	if err != nil {
		t.Fatal(err)
	}
	if p.Spec.User.Name != "boutique" {
		t.Errorf("l'utilisateur réel doit être conservé : %q", p.Spec.User.Name)
	}
	if p.Spec.Layout.Root != "/var/www/boutique" {
		t.Errorf("la racine réelle doit être conservée : %q", p.Spec.Layout.Root)
	}
	if p.Spec.Layout.Docroot != "/var/www/boutique/public" {
		t.Errorf("le docroot réel doit être conservé : %q", p.Spec.Layout.Docroot)
	}
	if p.ID() != "boutiquefr" {
		t.Errorf("identifiant : %q", p.ID())
	}
	if len(p.Spec.Domains) != 2 || p.Spec.Domains[1].Role != "alias" {
		t.Errorf("domaines : %+v", p.Spec.Domains)
	}
	if p.Metadata.Labels["adopted"] != "true" {
		t.Error("un projet adopté doit être marqué comme tel")
	}
	// Les notes disent ce qui reste à faire, sans rien tenter d'office.
	if !containsSubstring(notes, "n'est ni deviné ni réinitialisé") && len(p.Spec.Databases) > 0 {
		t.Errorf("le sort du mot de passe doit être explicité : %v", notes)
	}
	if !containsSubstring(notes, "cron add") {
		t.Errorf("les tâches non reprises doivent être signalées : %v", notes)
	}
}

func TestDivergesAnnouncesNormalisation(t *testing.T) {
	// Adopter puis appliquer normaliserait la configuration : l'écart doit être
	// annoncé avant, pas découvert après.
	scanner, _ := legacyServer(t)
	resources, _ := scanner.Scan(context.Background())
	candidates, _ := Correlate(resources, "/home", "preprod-pp.net")
	var boutique Candidate
	for _, c := range candidates {
		if c.Host == "boutique.fr" {
			boutique = c
		}
	}
	p, _, err := Adopt(boutique, "/home", "preprod-pp.net")
	if err != nil {
		t.Fatal(err)
	}
	diffs := Diverges(p, "/home", "preprod-pp.net")
	if len(diffs) == 0 {
		t.Fatal("des écarts à la convention sont attendus")
	}
	if !containsSubstring(diffs, "convention") {
		t.Errorf("les écarts doivent nommer la convention : %v", diffs)
	}
	// L'utilisateur et la racine diffèrent de ce qu'ogs aurait choisi.
	joined := strings.Join(diffs, "\n")
	if !strings.Contains(joined, "boutique") || !strings.Contains(joined, "/home/boutique.fr") {
		t.Errorf("écarts attendus sur l'utilisateur et la racine :\n%s", joined)
	}
}

func TestSimilarIgnoresSeparators(t *testing.T) {
	// Les séparateurs, et rien d'autre : un préfixe rapprochait « shop » de
	// « shopping » et de « shop_old », et le premier candidat venu prenait la base
	// et le compte du voisin — qu'ogs accordait à l'apply puis supprimait avec le
	// projet.
	cases := map[[2]string]bool{
		{"shop_fr", "shopfr"}:      true,
		{"shop-fr", "shopfr"}:      true,
		{"shop_fr", "shop-fr"}:     true,
		{"Shop.FR", "shopfr"}:      true,
		{"shopfr", "shopfrprep"}:   false,
		{"boutique", "boutiquefr"}: false,
		{"shop", "shopping"}:       false,
		{"shop_old", "shop"}:       false,
		{"shop", "autre"}:          false,
		{"", "shopfr"}:             false,
		{"", ""}:                   false,
	}
	for pair, want := range cases {
		if got := similar(pair[0], pair[1]); got != want {
			t.Errorf("similar(%q, %q) = %v, attendu %v", pair[0], pair[1], got, want)
		}
	}
}

func TestDeduceHome(t *testing.T) {
	cases := map[string]string{
		"/var/www/shop/public":     "/var/www/shop",
		"/home/shop.fr/www/public": "/home/shop.fr",
		"/home/shop.fr/www":        "/home/shop.fr",
		"/var/www/vitrine":         "/var/www/vitrine",
	}
	for docroot, want := range cases {
		if got := deduceHome(docroot); got != want {
			t.Errorf("deduceHome(%q) = %q, attendu %q", docroot, got, want)
		}
	}
}

func contains(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}

func containsSubstring(items []string, want string) bool {
	for _, s := range items {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func TestCorrelateNeRevendiquePasDeuxFoisLeMemeCompte(t *testing.T) {
	// Deux sites servis sous le home d'un même compte : ce home ne désigne
	// personne. Le premier vhost rencontré s'appropriait le compte et la racine,
	// et l'apply chownait le répertoire parent des deux sites à un seul projet.
	resources := []Resource{
		{Kind: KindVHost, Name: "a.fr", Attributes: map[string]string{
			"domains": "a.fr", "docroot": "/var/www/a"}},
		{Kind: KindVHost, Name: "b.fr", Attributes: map[string]string{
			"domains": "b.fr", "docroot": "/var/www/b"}},
		{Kind: KindUser, Name: "www", Attributes: map[string]string{
			"home": "/var/www", "shell": "/bin/bash"}},
	}
	candidates, _ := Correlate(resources, "/home", "preprod-pp.net")
	if len(candidates) != 2 {
		t.Fatalf("%d candidats, 2 attendus : %+v", len(candidates), candidates)
	}
	porteurs := 0
	for _, c := range candidates {
		if c.User == "www" {
			porteurs++
			if c.Home != "/var/www" {
				t.Errorf("le porteur du compte doit garder son home : %q", c.Home)
			}
		} else if c.Home == "/var/www" {
			t.Errorf("%s hérite de la racine partagée sans porter le compte", c.Host)
		}
	}
	if porteurs != 1 {
		t.Errorf("%d candidats revendiquent le compte www, 1 au plus attendu", porteurs)
	}
}

func TestCorrelateExigeUneFrontiereDeChemin(t *testing.T) {
	// « /var/www » n'est pas un ancêtre de « /var/www2/site » : le préfixe brut
	// les rapprochait, et le candidat héritait d'un compte étranger.
	resources := []Resource{
		{Kind: KindVHost, Name: "site.fr", Attributes: map[string]string{
			"domains": "site.fr", "docroot": "/var/www2/site/public"}},
		{Kind: KindUser, Name: "www", Attributes: map[string]string{
			"home": "/var/www", "shell": "/bin/bash"}},
	}
	candidates, unmanaged := Correlate(resources, "/home", "preprod-pp.net")
	if len(candidates) != 1 {
		t.Fatalf("%d candidats, 1 attendu : %+v", len(candidates), candidates)
	}
	if candidates[0].User != "" {
		t.Errorf("compte %q rapproché à tort par un faux préfixe", candidates[0].User)
	}
	var noms []string
	for _, r := range unmanaged {
		noms = append(noms, r.Name)
	}
	if !contains(noms, "www") {
		t.Errorf("le compte non rapproché doit rester non géré : %v", noms)
	}
}

func TestCorrelateCompteDevineParLeHomeResteMoyen(t *testing.T) {
	// La confiance élevée est réservée aux trois sources concordantes — vhost,
	// pool désigné par son socket, compte système. Un compte deviné par son home
	// est un seul indice, et il doit s'annoncer comme une supposition.
	resources := []Resource{
		{Kind: KindVHost, Name: "vitrine.fr", Attributes: map[string]string{
			"domains": "vitrine.fr", "docroot": "/var/www/vitrine/public"}},
		{Kind: KindUser, Name: "vitrine", Attributes: map[string]string{
			"home": "/var/www/vitrine", "shell": "/bin/bash"}},
	}
	candidates, _ := Correlate(resources, "/home", "preprod-pp.net")
	if len(candidates) != 1 {
		t.Fatalf("%d candidats, 1 attendu : %+v", len(candidates), candidates)
	}
	c := candidates[0]
	if c.Confidence != ConfidenceMedium {
		t.Errorf("confiance %q, « moyenne » attendue", c.Confidence)
	}
	annonce := false
	for _, u := range c.Uncertain {
		if strings.Contains(u, "rapproché par le home") {
			annonce = true
		}
	}
	if !annonce {
		t.Errorf("le rapprochement par le home doit être annoncé : %v", c.Uncertain)
	}
}

func TestScanIgnoreLesSauvegardesDeVhost(t *testing.T) {
	// Un serveur repris porte l'héritage des versions antérieures d'ogs, qui
	// déposaient la copie d'un écrasement forcé à côté du vhost écrasé. Elle en
	// garde le ServerName : lue comme un vhost, elle propose d'adopter une
	// seconde fois un site déjà géré.
	scanner, _ := legacyServer(t)
	copie := filepath.Join(scanner.Engine.AvailableDir(),
		"dejagere.conf.ogs-bak-20260905-141203")
	if err := os.WriteFile(copie,
		[]byte("<VirtualHost *:80>\n  ServerName dejagere.fr\n</VirtualHost>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resources, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resources {
		if r.Path == copie {
			t.Errorf("la sauvegarde est prise pour une ressource : %s", r)
		}
	}
	candidates, unmanaged := Correlate(resources, "/home", "preprod-pp.net")
	for _, c := range candidates {
		if c.Host == "dejagere.fr" {
			t.Errorf("un site déjà géré est proposé à l'adoption : %+v", c)
		}
	}
	for _, r := range unmanaged {
		if r.Path == copie {
			t.Errorf("la sauvegarde figure parmi les ressources non gérées : %s", r)
		}
	}
}

func TestCorrelateRetientLeCompteAuHomeLePlusProfond(t *testing.T) {
	// Deux comptes englobent le docroot : celui du site et celui qui possède la
	// racine commune. Retenir le premier venu — l'ordre est alphabétique —
	// donnait au projet le compte du voisin et sa racine, que l'apply aurait
	// chownée.
	resources := []Resource{
		{Kind: KindVHost, Name: "zoe.fr", Attributes: map[string]string{
			"domains": "zoe.fr", "docroot": "/var/www/zoe/public"}},
		{Kind: KindUser, Name: "hebergeur", Attributes: map[string]string{
			"home": "/var/www", "shell": "/usr/sbin/nologin"}},
		{Kind: KindUser, Name: "zoe", Attributes: map[string]string{
			"home": "/var/www/zoe", "shell": "/bin/bash"}},
	}
	candidates, _ := Correlate(resources, "/home", "preprod-pp.net")
	if len(candidates) != 1 {
		t.Fatalf("%d candidats, 1 attendu : %+v", len(candidates), candidates)
	}
	c := candidates[0]
	if c.User != "zoe" {
		t.Errorf("compte retenu %q, « zoe » attendu", c.User)
	}
	if c.Home != "/var/www/zoe" {
		t.Errorf("racine retenue %q, « /var/www/zoe » attendue", c.Home)
	}
	if c.Shell != "/bin/bash" {
		t.Errorf("shell retenu %q : c'est celui du compte du voisin", c.Shell)
	}
}

// clientFige joue le moteur MySQL d'un serveur repris : des bases et des comptes,
// ceux-ci au format du vrai client — « user@host », tel que CONCAT le rend.
type clientFige struct {
	bases, comptes []string
}

func ensemble(noms []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range noms {
		out[n] = true
	}
	return out
}

func (c *clientFige) Endpoint(context.Context) (db.Endpoint, error) { return db.Endpoint{}, nil }
func (c *clientFige) Databases(context.Context) (map[string]bool, error) {
	return ensemble(c.bases), nil
}
func (c *clientFige) Users(context.Context) (map[string]bool, error) {
	return ensemble(c.comptes), nil
}
func (c *clientFige) Grants(context.Context, string, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (c *clientFige) Extensions(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (c *clientFige) Exec(context.Context, string) error {
	return fmt.Errorf("le scan est en lecture seule")
}
func (c *clientFige) ExecIn(context.Context, string, string) error {
	return fmt.Errorf("le scan est en lecture seule")
}
func (c *clientFige) Close() error { return nil }

// serveurAvecBases construit un serveur repris dont chaque site a son vhost, son
// pool désigné par le socket et son compte système — le cas de confiance élevée —,
// et dont le moteur MySQL porte les bases et les comptes donnés.
func serveurAvecBases(t *testing.T, sites map[string]string, bases, comptes []string) *Scanner {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	eng, err := web.NewEngine("apache", prefix)
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(eng.EnabledDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	write(paths.PHPBin("8.1"), "#!/bin/sh\n")
	write(paths.PHPFpmBin("8.1"), "#!/bin/sh\n")
	hosts := make([]string, 0, len(sites))
	for host := range sites {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	var passwd []string
	for i, host := range hosts {
		user := sites[host]
		socket := "/run/php/php8.1-fpm-" + user + ".sock"
		write(filepath.Join(eng.AvailableDir(), host+".conf"), fmt.Sprintf(
			"<VirtualHost *:80>\n    ServerName %s\n    DocumentRoot /var/www/%s/public\n"+
				"    <FilesMatch \\.php$>\n        SetHandler \"proxy:unix:%s|fcgi://localhost\"\n"+
				"    </FilesMatch>\n</VirtualHost>\n", host, user, socket))
		write(paths.PHPPoolFile("8.1", user), fmt.Sprintf("[%s]\nuser = %s\nlisten = %s\n", user, user, socket))
		passwd = append(passwd, fmt.Sprintf("%s:x:%d:%d::/var/www/%s:/bin/bash", user, 1001+i, 1001+i, user))
	}
	write(filepath.Join(prefix, "etc", "passwd"), strings.Join(passwd, "\n")+"\n")
	return &Scanner{Paths: paths, Engine: eng, HomeRoots: []string{"/var/www"},
		DBClients: map[string]db.Client{"mysql": &clientFige{bases: bases, comptes: comptes}}}
}

// inspecter enchaîne le scan et la corrélation, comme « ogs import ».
func inspecter(t *testing.T, scanner *Scanner) ([]Candidate, []Resource) {
	t.Helper()
	resources, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return Correlate(resources, "/home", "preprod-pp.net")
}

func candidat(t *testing.T, candidates []Candidate, id string) Candidate {
	t.Helper()
	for _, c := range candidates {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("candidat %s absent : %+v", id, candidates)
	return Candidate{}
}

func basesDe(c Candidate) []string {
	var out []string
	for _, r := range c.Resources {
		if r.Kind == KindDatabase {
			out = append(out, r.Name)
		}
	}
	return out
}

func nonGeree(unmanaged []Resource, kind Kind, name string) bool {
	for _, r := range unmanaged {
		if r.Kind == kind && r.Name == name {
			return true
		}
	}
	return false
}

func TestScanNeRattacheQueLesComptesDeMemeNom(t *testing.T) {
	// Un préfixe suffisait : le compte de shop.com était rapproché de la base de
	// shop.fr, et de la base shop_old d'un tout autre site. Le même nom,
	// séparateurs mis à part, et chaque hôte compté pour un compte.
	scanner := serveurAvecBases(t, nil,
		[]string{"mysql", "shop", "shopfr", "shop_old", "boutique_fr"},
		[]string{"root@localhost", "shop@localhost", "shop@%", "shopfr@localhost", "boutiquefr@%"})
	resources, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	attendus := map[string]string{
		"shop":        "shop@%,shop@localhost",
		"shopfr":      "shopfr@localhost",
		"shop_old":    "",
		"boutique_fr": "boutiquefr@%",
	}
	vues := 0
	for _, r := range resources {
		if r.Kind != KindDatabase {
			continue
		}
		vues++
		if want, ok := attendus[r.Name]; !ok {
			t.Errorf("base système relevée : %s", r.Name)
		} else if r.Attr("users") != want {
			t.Errorf("base %s : comptes %q, %q attendus", r.Name, r.Attr("users"), want)
		}
	}
	if vues != len(attendus) {
		t.Errorf("%d bases relevées, %d attendues", vues, len(attendus))
	}
}

func TestCorrelateRattacheChaqueBaseAuSeulProjetQuiLaDesigne(t *testing.T) {
	// shop.fr (pool shop) et shopping.fr (pool shopping) : le premier candidat
	// revendiquait aussi la base shopping, sous le compte shop, et le second n'en
	// recevait aucune. L'apply de shopfr accordait shopping à shop, et
	// « project delete shopfr » la supprimait.
	scanner := serveurAvecBases(t,
		map[string]string{"shop.fr": "shop", "shopping.fr": "shopping"},
		[]string{"shop", "shopping"},
		[]string{"shop@localhost", "shopping@localhost"})
	candidates, _ := inspecter(t, scanner)
	for id, want := range map[string]string{"shopfr": "shop", "shoppingfr": "shopping"} {
		c := candidat(t, candidates, id)
		if got := basesDe(c); len(got) != 1 || got[0] != want {
			t.Errorf("%s : bases %v, [%s] attendue", id, got, want)
		}
		p, _, err := Adopt(c, "/home", "preprod-pp.net")
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Spec.Databases) != 1 || p.Spec.Databases[0].Name != want ||
			p.Spec.Databases[0].User != want {
			t.Errorf("%s : déclaration %+v, base et compte %s attendus", id, p.Spec.Databases, want)
		}
	}
}

func TestAdoptNeRetientPasLeCompteDUnVoisin(t *testing.T) {
	// shop.com (base shop, compte shop) et shop.fr (base shopfr, compte shopfr).
	// Le compte de shop.com, plus court, passait premier dans l'ordre
	// alphabétique : la base de shop.fr était déclarée sous le compte shop, que
	// « project delete shopfr » supprimait — shop.com, resté hors d'ogs, perdait
	// l'accès à sa base.
	scanner := serveurAvecBases(t,
		map[string]string{"shop.com": "shopcom", "shop.fr": "shopfr"},
		[]string{"shop", "shopfr"},
		[]string{"shop@localhost", "shopfr@localhost"})
	candidates, unmanaged := inspecter(t, scanner)
	for _, c := range candidates {
		p, _, err := Adopt(c, "/home", "preprod-pp.net")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range p.Spec.Databases {
			if d.Name == "shop" || d.User == "shop" {
				t.Errorf("%s déclare la base ou le compte de shop.com : %+v", c.ID, d)
			}
		}
		if c.ID == "shopfr" && (len(p.Spec.Databases) != 1 || p.Spec.Databases[0].Name != "shopfr" ||
			p.Spec.Databases[0].User != "shopfr" || p.Spec.Databases[0].Host != "localhost") {
			t.Errorf("shopfr : déclaration %+v, shopfr sous shopfr@localhost attendue", p.Spec.Databases)
		}
	}
	// La base que rien ne désigne reste non gérée : elle ne sera jamais touchée.
	if !nonGeree(unmanaged, KindDatabase, "shop") {
		t.Errorf("la base shop doit rester non gérée : %v", unmanaged)
	}
}

func TestCorrelateLaisseHorsGestionUneBaseAmbigue(t *testing.T) {
	// Un pool servant deux sites donne le même compte à deux candidats : la base
	// de ce nom les désigne tous deux. Le premier dans l'ordre alphabétique la
	// prenait ; aucun ne doit l'avoir, et chacun doit dire pourquoi.
	socket := "/run/php/php8.1-fpm-shop.sock"
	resources := []Resource{
		{Kind: KindVHost, Name: "shop.com", Attributes: map[string]string{
			"domains": "shop.com", "docroot": "/var/www/shop/public", "fpm": socket}},
		{Kind: KindVHost, Name: "shop.fr", Attributes: map[string]string{
			"domains": "shop.fr", "docroot": "/var/www/shop/public", "fpm": socket}},
		{Kind: KindPool, Name: "shop", Attributes: map[string]string{
			"version": "8.1", "user": "shop", "listen": socket}},
		{Kind: KindDatabase, Name: "shop", Attributes: map[string]string{
			"engine": "mysql", "users": "shop@localhost"}},
	}
	candidates, unmanaged := Correlate(resources, "/home", "preprod-pp.net")
	if len(candidates) != 2 {
		t.Fatalf("%d candidats, 2 attendus : %+v", len(candidates), candidates)
	}
	for _, c := range candidates {
		if got := basesDe(c); len(got) > 0 {
			t.Errorf("%s revendique %v, que son voisin désigne aussi", c.ID, got)
		}
		if !containsSubstring(c.Uncertain, "base shop laissée hors gestion : son nom désigne aussi") {
			t.Errorf("%s doit dire pourquoi la base est écartée : %v", c.ID, c.Uncertain)
		}
	}
	if !nonGeree(unmanaged, KindDatabase, "shop") {
		t.Errorf("la base ambiguë doit rester non gérée : %v", unmanaged)
	}

	// Deux vhosts d'un même site — certbot double le vhost d'un « -le-ssl » au
	// même ServerName — ne sont pas deux prétendants : la base leur revient.
	resources = []Resource{
		{Kind: KindVHost, Name: "shop.fr", Attributes: map[string]string{
			"domains": "shop.fr", "docroot": "/var/www/shop/public", "fpm": socket}},
		{Kind: KindVHost, Name: "shop.fr-le-ssl", Attributes: map[string]string{
			"domains": "shop.fr", "docroot": "/var/www/shop/public", "fpm": socket}},
		{Kind: KindPool, Name: "shop", Attributes: map[string]string{
			"version": "8.1", "user": "shop", "listen": socket}},
		{Kind: KindDatabase, Name: "shopfr", Attributes: map[string]string{
			"engine": "mysql", "users": "shopfr@localhost"}},
	}
	candidates, _ = Correlate(resources, "/home", "preprod-pp.net")
	for _, c := range candidates {
		if got := basesDe(c); len(got) != 1 || got[0] != "shopfr" {
			t.Errorf("%s (%s) : bases %v, [shopfr] attendue", c.ID, c.Resources[0].Name, got)
		}
		if containsSubstring(c.Uncertain, "désigne aussi") {
			t.Errorf("un site n'est pas en concurrence avec lui-même : %v", c.Uncertain)
		}
	}
}

func TestCorrelateLaisseHorsGestionUneBaseSansCompteSur(t *testing.T) {
	// Deux comptes du même nom sous deux hôtes sont deux comptes, et aucun compte
	// n'en désigne aucun : déclarer la base ferait créer, accorder, puis supprimer
	// avec le projet un compte que rien ne lui relie. Elle reste non gérée.
	resources := []Resource{
		{Kind: KindVHost, Name: "shop.fr", Attributes: map[string]string{
			"domains": "shop.fr", "docroot": "/var/www/shop/public"}},
		{Kind: KindVHost, Name: "vitrine.fr", Attributes: map[string]string{
			"domains": "vitrine.fr", "docroot": "/var/www/vitrine"}},
		{Kind: KindDatabase, Name: "shop_fr", Attributes: map[string]string{
			"engine": "mysql", "users": "shopfr@%,shopfr@localhost"}},
		{Kind: KindDatabase, Name: "vitrinefr", Attributes: map[string]string{
			"engine": "mysql"}},
	}
	candidates, unmanaged := Correlate(resources, "/home", "preprod-pp.net")
	for id, motif := range map[string]string{
		"shopfr": "base shop_fr laissée hors gestion : plusieurs comptes portent son nom " +
			"(shopfr@%, shopfr@localhost)",
		"vitrinefr": "base vitrinefr laissée hors gestion : aucun compte ne porte son nom",
	} {
		c := candidat(t, candidates, id)
		if got := basesDe(c); len(got) > 0 {
			t.Errorf("%s revendique %v sans compte sûr", id, got)
		}
		if !containsSubstring(c.Uncertain, motif) {
			t.Errorf("%s doit dire %q : %v", id, motif, c.Uncertain)
		}
		p, _, err := Adopt(c, "/home", "preprod-pp.net")
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Spec.Databases) > 0 {
			t.Errorf("%s déclare %+v sans compte sûr", id, p.Spec.Databases)
		}
	}
	for _, base := range []string{"shop_fr", "vitrinefr"} {
		if !nonGeree(unmanaged, KindDatabase, base) {
			t.Errorf("la base %s doit rester non gérée : %v", base, unmanaged)
		}
	}
}

func TestAdoptNeDeclarePasUneBaseSansCompteSur(t *testing.T) {
	// La garde d'Adopt tient pour un candidat composé ailleurs que par Correlate :
	// le premier compte de la liste n'est jamais retenu d'office.
	c := Candidate{ID: "shopfr", Host: "shop.fr", Domains: []string{"shop.fr"},
		Resources: []Resource{{Kind: KindDatabase, Name: "shop", Attributes: map[string]string{
			"engine": "mysql", "users": "shop@%,shop@localhost"}}}}
	p, notes, err := Adopt(c, "/home", "preprod-pp.net")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Spec.Databases) > 0 {
		t.Errorf("aucune base ne doit être déclarée sans compte sûr : %+v", p.Spec.Databases)
	}
	if !containsSubstring(notes, "base shop non déclarée : plusieurs comptes portent son nom") {
		t.Errorf("la base écartée doit être annoncée : %v", notes)
	}
}

func TestAdoptGardeLHoteReelDuCompteMySQL(t *testing.T) {
	// Un compte 'shop'@'%' était déclaré 'shop'@'localhost' : l'observation ne le
	// trouvait pas, et l'apply créait à côté un compte au mot de passe tiré que
	// MySQL préfère pour toute connexion locale — l'application perdait l'accès à
	// sa base au premier apply suivant l'import.
	scanner := serveurAvecBases(t, map[string]string{"shop.fr": "shop"},
		[]string{"shop"}, []string{"root@localhost", "shop@%"})
	candidates, _ := inspecter(t, scanner)
	p, notes, err := Adopt(candidat(t, candidates, "shopfr"), "/home", "preprod-pp.net")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Spec.Databases) != 1 {
		t.Fatalf("une base attendue : %+v", p.Spec.Databases)
	}
	d := p.Spec.Databases[0]
	if d.Name != "shop" || d.User != "shop" || d.Host != "%" {
		t.Errorf("déclaration %s sous %s@%s, shop sous shop@%% attendue", d.Name, d.User, d.Host)
	}
	if !containsSubstring(notes, "le mot de passe de shop@% est inconnu d'ogs") {
		t.Errorf("la note doit nommer le compte réel : %v", notes)
	}
	// L'hôte survit à l'écriture de la déclaration : c'est elle que l'apply relit.
	data, err := spec.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	relu, err := spec.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := relu.Spec.Databases[0].Host; got != "%" {
		t.Errorf("hôte relu %q, %% attendu :\n%s", got, data)
	}
}
