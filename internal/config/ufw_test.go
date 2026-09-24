package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func chargerUFW(t *testing.T, contenu string) (*UFW, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ufw.yaml")
	if err := os.WriteFile(path, []byte(contenu), 0o600); err != nil {
		t.Fatal(err)
	}
	u, present, err := LoadUFW(path)
	if !present {
		t.Fatal("le fichier écrit doit être vu comme présent")
	}
	return u, err
}

func TestUFWAbsentNeGereRien(t *testing.T) {
	u, present, err := LoadUFW(filepath.Join(t.TempDir(), "ufw.yaml"))
	if err != nil || present || u != nil {
		t.Fatalf("un ufw.yaml absent veut dire « non géré » : %v %v %v", u, present, err)
	}
}

func TestUFWFormesCourtesDesEntrees(t *testing.T) {
	u, err := chargerUFW(t, `
sources:
  bureau:
    owner: Agence
    addresses: [203.0.113.10]
trusted: [bureau, 198.51.100.7]
profiles:
  web:
    allow: [any]
  mysql:
    allow:
      - address: 192.0.2.10
        comment: réplica
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Trusted[0]; got.Source != "bureau" || got.Address != "" {
		t.Errorf("« bureau » doit désigner une source : %+v", got)
	}
	if got := u.Trusted[1]; got.Address != "198.51.100.7" {
		t.Errorf("une adresse écrite en chaîne doit rester une adresse : %+v", got)
	}
	if got := u.Profiles["web"].Allow[0]; got.Address != UFWAny {
		t.Errorf("« any » doit désigner toute adresse : %+v", got)
	}
	if got := u.Profiles["mysql"].Allow[0]; got.Address != "192.0.2.10" || got.Comment != "réplica" {
		t.Errorf("forme longue mal lue : %+v", got)
	}
}

func TestUFWDefautsDesPolitiques(t *testing.T) {
	u, err := chargerUFW(t, "logging: low\n")
	if err != nil {
		t.Fatal(err)
	}
	if u.IncomingPolicy() != "deny" || u.OutgoingPolicy() != "allow" || u.RoutedPolicy() != "" {
		t.Errorf("défauts d'ufw attendus, routage non géré : %s %s %q",
			u.IncomingPolicy(), u.OutgoingPolicy(), u.RoutedPolicy())
	}
	if !u.IsActive() {
		t.Error("déclarer un pare-feu, c'est vouloir qu'il filtre")
	}
	if u.Rollback() != DefaultUFWRollbackAfter {
		t.Errorf("délai de retour %s", u.Rollback())
	}
}

func TestUFWRefus(t *testing.T) {
	cas := []struct {
		nom, yaml, attendu string
	}{
		{"source inconnue", "trusted: [bureau]\n", "source « bureau » inconnue"},
		{"bits d'hôte", "trusted: [10.0.0.5/24]\n", "écrire 10.0.0.0/24"},
		{"any en confiance", "trusted: [any]\n", "« any » n'a pas de sens ici"},
		{"any banni", "ban: [any]\n", "« any » n'a pas de sens ici"},
		{"bannie et autorisée", "trusted: [10.0.0.0/24]\nban: [10.0.0.9]\n", "recouvre 10.0.0.0/24"},
		{"liste sans protocole", "profiles:\n  mysql:\n    ports: [\"3306,3307\"]\n", "exige son protocole"},
		{"plage sans protocole", "profiles:\n  ftp:\n    ports: [\"21:22\"]\n", "exige son protocole"},
		{"profil propre sans ports", "profiles:\n  elastic:\n    allow: [any]\n", "hors catalogue sans « ports »"},
		{"passif hors ftp", "profiles:\n  sftp:\n    passive: \"40000:40100\"\n", "seul le profil ftp"},
		{"passif inversé", "profiles:\n  ftp:\n    passive: \"40100:40000\"\n", "bornes croissantes"},
		{"sortie sans dns", "policy:\n  outgoing: deny\noutgoing:\n  allow: [https]\n", "« dns »"},
		{"sortie inconnue", "policy:\n  outgoing: deny\noutgoing:\n  allow: [dns, https, gopher]\n", "gopher"},
		{"sorties inutiles", "outgoing:\n  allow: [dns]\n", "n'a d'effet que"},
		{"délai trop court", "rollbackAfter: 10s\n", "trop court"},
		{"politique inconnue", "policy:\n  incoming: drop\n", "policy.incoming"},
		{"clé inconnue", "polcy:\n  incoming: deny\n", "polcy"},
		{"source sans adresse", "sources:\n  vide:\n    owner: x\n    addresses: []\n", "aucune adresse"},
		{"commentaire sur deux lignes", "trusted:\n  - address: 10.0.0.1\n    comment: \"a\\nb\"\n", "une ligne"},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			_, err := chargerUFW(t, c.yaml)
			if err == nil {
				t.Fatalf("déclaration acceptée, refus attendu (%s)", c.attendu)
			}
			if !strings.Contains(err.Error(), c.attendu) {
				t.Errorf("message %q, attendu %q", err, c.attendu)
			}
		})
	}
}

func TestUFWRefusDedoublonne(t *testing.T) {
	// Une source autorisée sur deux profils ne doit pas faire dire deux fois le
	// même chevauchement.
	_, err := chargerUFW(t, `
sources:
  acme: {addresses: [198.51.100.7]}
profiles:
  mysql: {allow: [acme]}
  sftp: {allow: [acme]}
ban: [198.51.100.0/24]
`)
	if err == nil {
		t.Fatal("chevauchement accepté")
	}
	if n := strings.Count(err.Error(), "recouvre"); n != 1 {
		t.Errorf("%d messages pour un même chevauchement : %v", n, err)
	}
}

func TestUFWAdresseLitteraleReprendSaSource(t *testing.T) {
	u := &UFW{Sources: map[string]UFWSource{"bureau": {Owner: "Agence", Addresses: []string{"203.0.113.0/24"}}}}
	addrs, err := u.Resolve(UFWEntry{Address: "203.0.113.10"})
	if err != nil {
		t.Fatal(err)
	}
	if addrs[0].Source != "bureau" || addrs[0].Owner != "Agence" {
		t.Errorf("une adresse couverte par une source doit en porter le propriétaire : %+v", addrs[0])
	}
}

func TestUFWApostropheRemplacee(t *testing.T) {
	// ufw refuse une apostrophe droite dans un commentaire ; le français en est
	// plein.
	if got := SanitizeUFWComment("l'agence  de\tNantes"); got != "l’agence de Nantes" {
		t.Errorf("commentaire %q", got)
	}
}

func TestUFWAdressesNormalisees(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.10":     "203.0.113.10",
		"203.0.113.10/32":  "203.0.113.10",
		"10.0.0.0/8":       "10.0.0.0/8",
		"2001:db8::1":      "2001:db8::1",
		"2001:db8:42::/48": "2001:db8:42::/48",
		"::ffff:192.0.2.1": "192.0.2.1",
	} {
		p, err := ParseUFWAddress(in)
		if err != nil {
			t.Errorf("%s : %v", in, err)
			continue
		}
		if got := FormatUFWPrefix(p); got != want {
			t.Errorf("%s rendu %s, attendu %s", in, got, want)
		}
	}
	if _, err := ParseUFWAddress("0.0.0.0/0"); err == nil {
		t.Error("0.0.0.0/0 doit s'écrire « any »")
	}
}
