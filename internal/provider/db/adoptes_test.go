package db

import (
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

func TestUnCompteAdopteNeRecoitPasUnMotDePasseJamaisPose(t *testing.T) {
	// R2-21 (reste : adoptés). Le compte d'un projet adopté existe, ogs n'en tient
	// pas le mot de passe (D78). Une correction de privilèges faisait tirer et
	// sceller un mot de passe que « CREATE USER IF NOT EXISTS » ne pose pas :
	// le keystore publiait alors, par « ogs db env » ou « secrets show », un mot
	// de passe que le moteur ignore — et faisait du compte, à tort, celui d'ogs.
	t.Run("mysql", func(t *testing.T) {
		prov, proj := fixture(t)
		decl := DefaultDatabase(proj, "mysql")
		decl.Name, decl.User, decl.Host, decl.Privileges = "shop", "shop", "%", "readonly"
		proj.Spec.Databases = []model.Database{decl}
		adopter(proj)
		stub := vivant([]string{"shop"}, []string{"shop@%"},
			map[string]map[string]string{"shop": {"shop": tousDroits}})
		prov.clients = map[string]Client{"mysql": stub}

		steps := planifier(t, prov, proj)
		if len(steps) == 0 || !steps[0].Mutates() {
			t.Fatalf("la réduction des privilèges doit être annoncée : %v", steps)
		}
		if strings.Contains(décrire(t, steps[0]), "CREATE USER") {
			t.Errorf("aucun CREATE USER sur un compte adopté existant :\n%s", décrire(t, steps[0]))
		}
		if errs := appliquer(t, steps); len(errs) > 0 {
			t.Fatal(errs)
		}
		if prov.sealed(proj.ID(), "mysql", "shop") {
			t.Error("aucun mot de passe ne doit être scellé : ogs ne l'a pas posé")
		}
		for _, q := range stub.exécutées {
			if strings.Contains(q, "IDENTIFIED") || strings.HasPrefix(q, "CREATE USER") {
				t.Errorf("le compte adopté ne doit être ni créé ni modifié : %s", q)
			}
		}
	})
	t.Run("postgres", func(t *testing.T) {
		prov, proj := fixture(t)
		decl := DefaultDatabase(proj, "postgres")
		decl.Name, decl.User, decl.Extensions = "shop", "shop", []string{"pg_trgm"}
		proj.Spec.Databases = []model.Database{decl}
		adopter(proj)
		stub := vivant([]string{"shop"}, []string{"shop"},
			map[string]map[string]string{"shop": {"shop": "OWNER"}})
		stub.postgres = true
		prov.clients = map[string]Client{"postgres": stub}

		if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
			t.Fatal(errs)
		}
		if prov.sealed(proj.ID(), "postgres", "shop") {
			t.Error("aucun mot de passe ne doit être scellé : ogs ne l'a pas posé")
		}
	})
}

func TestLeCompteDisparuDUnAdopteEstRecreeEtAnnonce(t *testing.T) {
	// R1-60. Un projet adopté supprimé sous --force, puis restauré : son compte
	// SGBD est recréé avec un mot de passe neuf — l'archive n'en portait aucun,
	// ogs ne l'ayant jamais tenu. L'application, configurée avec l'ancien, perd
	// sa base sans que la restauration le dise. Arbitrage D111 : recréé, scellé,
	// annoncé en code 6, la sortie disant de reconfigurer l'application.
	for _, engineName := range []string{"mysql", "postgres"} {
		t.Run(engineName, func(t *testing.T) {
			prov, proj := fixture(t)
			decl := DefaultDatabase(proj, engineName)
			decl.Name, decl.User = "shop", "shop"
			if engineName == "mysql" {
				decl.Host = "%"
			}
			proj.Spec.Databases = []model.Database{decl}
			adopter(proj)
			stub := vivant(nil, nil, map[string]map[string]string{})
			stub.postgres = engineName == "postgres"
			prov.clients = map[string]Client{engineName: stub}

			steps := planifier(t, prov, proj)
			var annonce bool
			for _, s := range steps {
				if !s.Critical && strings.Contains(s.Summary, "mot de passe neuf") {
					annonce = true
				}
			}
			if !annonce {
				t.Fatalf("le plan doit annoncer le mot de passe neuf : %v", steps)
			}
			errs := appliquer(t, steps)
			if !prov.sealed(proj.ID(), engineName, "shop") {
				t.Fatal("le mot de passe posé doit être scellé")
			}
			pw, _ := prov.Password(proj.ID(), engineName, "shop")
			posé := false
			for _, q := range stub.exécutées {
				if strings.Contains(q, pw) {
					posé = true
				}
			}
			if !posé {
				t.Errorf("le mot de passe scellé doit être celui posé : %v", stub.exécutées)
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), "ogs secrets show "+proj.ID()+" "+engineName+"/shop") {
				t.Errorf("un avertissement (code 6) doit nommer la reconfiguration : %v", errs)
			}
		})
	}
}

func TestUnCompteCreeParOgsNEstPasAnnonceCommeRecree(t *testing.T) {
	// Hors adoption, un compte neuf est l'ordinaire : rien à annoncer.
	prov, proj := fixture(t)
	proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "mysql")}
	prov.clients = map[string]Client{"mysql": vivant(nil, []string{"root@localhost"}, map[string]map[string]string{})}
	steps := planifier(t, prov, proj)
	for _, s := range steps {
		if strings.Contains(s.Summary, "mot de passe neuf") {
			t.Errorf("annonce inattendue : %s", s)
		}
	}
	if errs := appliquer(t, steps); len(errs) > 0 {
		t.Error(errs)
	}
}

func TestLeCompteDUnAdopteRecreeAvecUnSecretAncienEstAnnonce(t *testing.T) {
	// Parc antérieur à D111 : l'apply d'alors scellait pour le compte adopté un
	// mot de passe jamais posé (R2-21). Après « project delete --force », la
	// restauration réinjecte ce secret, et le compte était recréé avec lui en
	// code 0 : l'application perdait sa base sans un mot.
	for _, engineName := range []string{"mysql", "postgres"} {
		t.Run(engineName, func(t *testing.T) {
			prov, proj := fixture(t)
			decl := DefaultDatabase(proj, engineName)
			decl.Name, decl.User = "shop", "shop"
			if engineName == "mysql" {
				decl.Host = "%"
			}
			proj.Spec.Databases = []model.Database{decl}
			adopter(proj)
			sceller(t, prov, proj, engineName, "shop")
			ancien, _ := prov.Password(proj.ID(), engineName, "shop")
			stub := vivant(nil, nil, map[string]map[string]string{})
			stub.postgres = engineName == "postgres"
			prov.clients = map[string]Client{engineName: stub}

			errs := appliquer(t, planifier(t, prov, proj))
			if len(errs) != 1 {
				t.Fatalf("un avertissement (code 6) est attendu : %v", errs)
			}
			for _, want := range []string{"n'a peut-être jamais été celui de l'application",
				"ogs secrets show " + proj.ID() + " " + engineName + "/shop", "fichier d'environnement", "FPM"} {
				if !strings.Contains(errs[0].Error(), want) {
					t.Errorf("l'avertissement doit dire %q : %v", want, errs[0])
				}
			}
			if pw, _ := prov.Password(proj.ID(), engineName, "shop"); pw != ancien {
				t.Error("le secret scellé est celui que porte le compte recréé")
			}
		})
	}
}
