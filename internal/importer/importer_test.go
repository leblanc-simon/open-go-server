package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/web"
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
	cases := map[[2]string]bool{
		{"shop_fr", "shopfr"}:      true,
		{"shop-fr", "shopfr"}:      true,
		{"shopfr", "shopfrprep"}:   true,
		{"boutique", "boutiquefr"}: true,
		{"shop", "autre"}:          false,
		{"", "shopfr"}:             false,
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
