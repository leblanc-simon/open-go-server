package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deuxVhostsDeuxCertificats déclare deux vhosts, chacun désignant son
// certificat.
func deuxVhostsDeuxCertificats(t *testing.T, h *harness, refBlog string) {
	t.Helper()
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	decl := filepath.Join("etc", "ogs", "projects", "shopfr.yaml")
	avant := h.read(t, decl)
	apres := avant + `    - name: blog.shop.fr
      role: alias
  web:
    vhosts:
      - name: shopfr
        template: static
        domains: [shop.fr]
        tls: { enabled: true, certificate: principal }
      - name: blog
        template: static
        domains: [blog.shop.fr]
        tls: { enabled: true, certificate: ` + refBlog + ` }
  certificates:
    - name: principal
      domains: [shop.fr]
      challenge: selfsigned
    - name: blog
      domains: [blog.shop.fr]
      challenge: selfsigned
`
	if err := os.WriteFile(h.path(decl), []byte(apres), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Chaque vhost sert le certificat qu'il désigne (Q-30, D102). La référence était
// lue puis ignorée : les deux vhosts recevaient le premier certificat déclaré, et
// celui du blog, qui ne le couvrait pas, restait en HTTP seul — son propre
// certificat émis, renouvelé à vie, et servi par personne.
func TestChaqueVhostSertLeCertificatQuIlDésigne(t *testing.T) {
	h := newHarness(t)
	deuxVhostsDeuxCertificats(t, h, "blog")
	h.mustRun("apply", "shopfr", "--yes")

	site := filepath.Join("etc", "apache2", "sites-available")
	principal := h.read(t, filepath.Join(site, "shopfr_shopfr.conf"))
	blog := h.read(t, filepath.Join(site, "shopfr_blog.conf"))
	if !strings.Contains(principal, "<VirtualHost *:443>") || !strings.Contains(principal, "certificates/principal/fullchain.pem") {
		t.Errorf("le vhost principal doit servir le certificat « principal » :\n%s", principal)
	}
	if !strings.Contains(blog, "<VirtualHost *:443>") || !strings.Contains(blog, "certificates/blog/fullchain.pem") {
		t.Errorf("le vhost du blog doit servir le certificat « blog » :\n%s", blog)
	}
	if out, code := h.run("diff", "shopfr"); strings.Contains(out, "TLS") || strings.Contains(out, "certificat") {
		t.Errorf("le plan suivant ne doit plus rien annoncer de TLS (code %d) :\n%s", code, out)
	}
}

// Une référence que rien ne porte n'est ni refusée — elle l'était sans effet,
// le vhost servant le certificat par défaut, et le refuser couperait un HTTPS
// qui fonctionnait — ni tue : le plan le signale, en code 6.
func TestRéférenceDeCertificatInconnueSignalée(t *testing.T) {
	h := newHarness(t)
	deuxVhostsDeuxCertificats(t, h, "blgo")
	out, code := h.run("apply", "shopfr", "--yes")
	if code != CodePartial || !strings.Contains(out, "« blgo »") {
		t.Fatalf("une référence inconnue doit être signalée en code 6 (code %d) :\n%s", code, out)
	}
	blog := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr_blog.conf"))
	// Le choix par défaut prend le certificat dont la demande couvre le vhost.
	if !strings.Contains(blog, "certificates/blog/fullchain.pem") {
		t.Errorf("le vhost du blog doit servir le certificat qui le couvre :\n%s", blog)
	}
}

// ajouterAuSpec insère un bloc YAML sous « spec: ».
func ajouterAuSpec(t *testing.T, h *harness, id, bloc string) {
	t.Helper()
	decl := filepath.Join("etc", "ogs", "projects", id+".yaml")
	avant := h.read(t, decl)
	if err := os.WriteFile(h.path(decl), []byte(strings.Replace(avant, "spec:\n", "spec:\n"+bloc, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Relecture de D102 : spec.web.tls.certificate, jusque-là ignoré, s'appliquait
// à tous les vhosts dès que la référence existait — un certificat qui ne
// couvrait pas le site lui retirait son bloc 443, en code 0, HSTS actif. Une
// référence qui ne couvre aucun nom du vhost retombe sur le choix par défaut,
// et l'apply le signale en code 6.
func TestUneRéférenceQuiNeCouvrePasLeVhostNeLuiRetireRien(t *testing.T) {
	for _, ref := range []string{"autre", `"@wildcard-prep"`} {
		t.Run(ref, func(t *testing.T) {
			h := newHarness(t)
			h.mustRun("init")
			h.mustRun("project", "create", "shop.fr", "--kind", "static")
			ajouterAuSpec(t, h, "shopfr", "  certificates:\n"+
				"    - name: principal\n      domains: [shop.fr]\n      challenge: selfsigned\n"+
				"    - name: autre\n      domains: [autre.shop.fr]\n      challenge: selfsigned\n")
			h.mustRun("apply", "shopfr", "--yes")
			vhost := filepath.Join("etc", "apache2", "sites-available", "shopfr.conf")
			if c := h.read(t, vhost); !strings.Contains(c, "<VirtualHost *:443>") {
				t.Fatalf("premier apply : bloc 443 attendu :\n%s", c)
			}
			ajouterAuSpec(t, h, "shopfr", "  web:\n    tls: {enabled: true, certificate: "+ref+"}\n")
			out, code := h.run("apply", "shopfr", "--yes")
			if code != CodePartial || !strings.Contains(out, "ne couvre aucun de ses noms") {
				t.Errorf("la référence sans objet doit être signalée en code 6 (code %d) :\n%s", code, out)
			}
			c := h.read(t, vhost)
			if !strings.Contains(c, "<VirtualHost *:443>") || !strings.Contains(c, "certificates/principal/") {
				t.Errorf("le site doit garder son HTTPS et son certificat :\n%s", c)
			}
		})
	}
}

// Un vhost qui désigne un certificat pas encore émis garde celui qui le sert
// jusqu'à l'émission, puis bascule dans le même apply (D79, D102) : le désigné
// retenu d'emblée faisait passer le vhost en HTTP seul le temps de l'émission,
// et pour de bon si elle échouait.
func TestLaDésignationDUnCertificatNeufBasculeAprèsSonÉmission(t *testing.T) {
	h := newHarness(t)
	deuxVhostsDeuxCertificats(t, h, "blog")
	h.mustRun("apply", "shopfr", "--yes")
	decl := filepath.Join("etc", "ogs", "projects", "shopfr.yaml")
	avant := h.read(t, decl)
	apres := strings.Replace(avant, "certificate: principal }", "certificate: neuf }", 1)
	apres = strings.Replace(apres, "  certificates:\n", "  certificates:\n    - name: neuf\n      domains: [shop.fr]\n      challenge: selfsigned\n", 1)
	if err := os.WriteFile(h.path(decl), []byte(apres), 0o644); err != nil {
		t.Fatal(err)
	}
	vhost := filepath.Join("etc", "apache2", "sites-available", "shopfr_shopfr.conf")
	out, code := h.run("plan", "shopfr")
	if !strings.Contains(out, "bascule du vhost shopfr_shopfr sur le certificat neuf") {
		t.Errorf("le plan doit annoncer la bascule (code %d) :\n%s", code, out)
	}
	h.mustRun("apply", "shopfr", "--yes")
	if c := h.read(t, vhost); !strings.Contains(c, "certificates/neuf/fullchain.pem") {
		t.Errorf("le vhost doit servir le certificat neuf après son émission :\n%s", c)
	}
	if out, _ := h.run("diff", "shopfr"); strings.Contains(out, "TLS") || strings.Contains(out, "bascule") {
		t.Errorf("rien ne doit rester à basculer :\n%s", out)
	}
}

// Un certificat partagé que désigne un projet s'émet avec lui, et
// « cert renew <projet> » le renouvelle avec lui : seul « renew --all »
// l'atteignait (D102).
func TestLePartagéDésignéSeRenouvelleAvecLeProjet(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	cfg := filepath.Join("etc", "ogs", "config.yaml")
	c := h.read(t, cfg)
	if !strings.Contains(c, "\ncertificates:\n") {
		t.Fatalf("config.yaml sans section certificates :\n%s", c)
	}
	c = strings.Replace(c, "\ncertificates:\n", "\ncertificates:\n  commun:\n    domains: [shop.fr]\n    challenge: selfsigned\n", 1)
	if err := os.WriteFile(h.path(cfg), []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	ajouterAuSpec(t, h, "shopfr", "  web:\n    tls: {enabled: true, certificate: \"@commun\"}\n")
	h.mustRun("apply", "shopfr", "--yes")
	if v := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr.conf")); !strings.Contains(v, "_shared-commun/fullchain.pem") {
		t.Fatalf("le vhost doit servir @commun :\n%s", v)
	}
	out := h.mustRunPartial("cert", "renew", "shopfr", "--force")
	if !strings.Contains(out, "@commun renouvelé") {
		t.Errorf("le partagé désigné doit être renouvelé avec le projet :\n%s", out)
	}
}
