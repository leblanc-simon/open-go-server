package tls

import (
	"context"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/naming"
)

func matérielDEssai(t *testing.T, staging bool, domains ...string) Material {
	t.Helper()
	m, err := stagingIssuer{now: time.Now}.Issue(context.Background(), Request{Domains: domains, Staging: staging})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func full(m Material) []byte { return append(append([]byte{}, m.Certificate...), m.Chain...) }

func TestParseImportSepareLaFeuilleDeLaChaineEtNeGardeQueLaCle(t *testing.T) {
	m := matérielDEssai(t, false, "shop.fr")
	// OpenSSL écrit parfois les paramètres de la courbe devant la clé.
	key := append([]byte("-----BEGIN EC PARAMETERS-----\nBggqhkjOPQMBBw==\n-----END EC PARAMETERS-----\n"),
		m.PrivateKey...)
	imp, err := ParseImport(full(m), key, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(imp.Material.Certificate) != string(m.Certificate) || string(imp.Material.Chain) != string(m.Chain) {
		t.Error("la feuille et la chaîne doivent être séparées telles quelles")
	}
	if strings.Contains(string(imp.Material.PrivateKey), "EC PARAMETERS") {
		t.Error("seule la clé est gardée")
	}
	if imp.Info.Domains[0] != "shop.fr" {
		t.Errorf("domaines lus : %v", imp.Info.Domains)
	}
}

func TestParseImportRefuse(t *testing.T) {
	prod := matérielDEssai(t, false, "shop.fr")
	staging := matérielDEssai(t, true, "shop.fr")
	autre := matérielDEssai(t, false, "shop.fr")
	chiffrée := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Headers: map[string]string{
		"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00"}, Bytes: []byte{1}})
	for _, c := range []struct {
		nom        string
		chain, key []byte
		motif      string
	}{
		{"clé dépareillée", full(prod), autre.PrivateKey, "ne correspond pas"},
		{"environnement de test", full(staging), staging.PrivateKey, "environnement de test"},
		{"autorité en tête", prod.Chain, prod.PrivateKey, "autorité"},
		{"clé dans le certificat", append(full(prod), prod.PrivateKey...), prod.PrivateKey, "porte une clé privée"},
		{"clé chiffrée à l'ancienne", full(prod), chiffrée, "chiffrée"},
		{"aucun certificat", []byte("pas du PEM"), prod.PrivateKey, "aucun certificat"},
	} {
		t.Run(c.nom, func(t *testing.T) {
			if _, err := ParseImport(c.chain, c.key, time.Now(), nil); err == nil || !strings.Contains(err.Error(), c.motif) {
				t.Errorf("refus attendu avec « %s », obtenu %v", c.motif, err)
			}
		})
	}
}

func TestParseImportAvertitDUneEcheanceProche(t *testing.T) {
	m := matérielDEssai(t, false, "shop.fr")
	// Le matériel d'essai vaut 90 jours : 70 jours plus tard, il entre dans le
	// seuil de renouvellement.
	imp, err := ParseImport(full(m), m.PrivateKey, time.Now().Add(70*24*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(imp.Warnings, "\n"), "expire dans") {
		t.Errorf("avertissement d'échéance attendu : %v", imp.Warnings)
	}
}

func TestImportTargetChoisitLaDemandeEtNommeLesNomsNonCouverts(t *testing.T) {
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	m := matérielDEssai(t, false, "shop.fr")
	imp, err := ParseImport(full(m), m.PrivateKey, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req, missing, err := p.ImportTarget(proj, "", imp.Info)
	if err != nil {
		t.Fatal(err)
	}
	if req.Name != proj.ID() {
		t.Errorf("le certificat implicite du projet est visé : %s", req.Name)
	}
	if len(missing) != len(req.Domains)-1 {
		t.Errorf("noms non couverts : %v (demande %v)", missing, req.Domains)
	}
	if _, _, err := p.ImportTarget(proj, "autre", imp.Info); err == nil {
		t.Error("un nom de certificat que le projet ne déclare pas est refusé")
	}
}
