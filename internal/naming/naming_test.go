package naming

import "testing"

func TestBaseID(t *testing.T) {
	cases := []struct{ host, want string }{
		{"shop.fr", "shopfr"},
		{"www.shop.fr", "shopfr"},          // le préfixe www est ignoré
		{"SHOP.FR.", "shopfr"},             // casse et point final normalisés
		{"api.shop.fr", "apishopfr"},       // sous-domaine conservé
		{"ma-boutique.fr", "maboutiquefr"}, // tirets supprimés
		{"1001pneus.fr", "s1001pneusfr"},   // un identifiant ne peut pas commencer par un chiffre
	}
	for _, c := range cases {
		got, err := BaseID(c.host)
		if err != nil {
			t.Fatalf("BaseID(%q) : %v", c.host, err)
		}
		if got != c.want {
			t.Errorf("BaseID(%q) = %q, attendu %q", c.host, got, c.want)
		}
	}
}

func TestBaseIDTruncationIsDeterministicAndBounded(t *testing.T) {
	const host = "ma-boutique-de-vente-en-ligne-de-chaussures.fr" // 38 caractères utiles
	first, err := BaseID(host)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := BaseID(host)
	if first != second {
		t.Errorf("troncature non déterministe : %q puis %q", first, second)
	}
	if len(first) != MaxBase {
		t.Errorf("longueur %d, attendu %d (%q)", len(first), MaxBase, first)
	}
	// Le suffixe d'environnement doit encore tenir dans la limite dure.
	prep, err := ID(host, EnvPrep)
	if err != nil {
		t.Fatal(err)
	}
	if len(prep) > MaxIdentifier {
		t.Errorf("identifiant de préproduction trop long : %q (%d)", prep, len(prep))
	}
}

func TestBaseIDDistinguishesLongHosts(t *testing.T) {
	// Deux hosts longs partageant leurs 21 premiers caractères doivent rester
	// distincts : c'est le rôle du hachage.
	a, _ := BaseID("boutique-de-vente-en-ligne-alpha.fr")
	b, _ := BaseID("boutique-de-vente-en-ligne-beta.fr")
	if a == b {
		t.Errorf("collision entre deux hosts longs distincts : %q", a)
	}
}

func TestBaseIDCollisionOnShortHosts(t *testing.T) {
	// Collision assumée et documentée (§3.3.6) : les tirets étant supprimés,
	// mon-shop.fr et monshop.fr convergent. C'est à la validation d'unicité de
	// la refuser, pas à la dérivation de l'éviter.
	a, _ := BaseID("mon-shop.fr")
	b, _ := BaseID("monshop.fr")
	if a != b {
		t.Fatalf("le cas de collision documenté a changé : %q != %q", a, b)
	}
}

func TestIDPrepSharesBase(t *testing.T) {
	prod, err := ID("shop.fr", EnvProd)
	if err != nil {
		t.Fatal(err)
	}
	prep, err := ID("shop.fr", EnvPrep)
	if err != nil {
		t.Fatal(err)
	}
	if prod != "shopfr" || prep != "shopfrprep" {
		t.Fatalf("prod=%q prep=%q", prod, prep)
	}
	if prep != prod+SuffixPrep {
		t.Errorf("la base doit être partagée entre les environnements")
	}
}

func TestEffectiveHost(t *testing.T) {
	if h, err := EffectiveHost("shop.fr", EnvProd, "preprod-pp.net"); err != nil || h != "shop.fr" {
		t.Errorf("prod : %q, %v", h, err)
	}
	h, err := EffectiveHost("shop.fr", EnvPrep, "preprod-pp.net")
	if err != nil {
		t.Fatal(err)
	}
	if h != "shopfr.preprod-pp.net" {
		t.Errorf("prep : %q", h)
	}
	// Le sous-domaine reprend l'identifiant de production, pas celui de préproduction.
	if h == "shopfrprep.preprod-pp.net" {
		t.Errorf("le suffixe prep ne doit pas apparaître dans le host")
	}
	if _, err := EffectiveHost("shop.fr", EnvPrep, ""); err == nil {
		t.Errorf("un domaine de préproduction absent doit être une erreur")
	}
}

func TestNormalizeHostRejects(t *testing.T) {
	for _, h := range []string{"", "shop", "sh op.fr", "shop..fr", "-shop.fr", "shop.fr-"} {
		if _, err := NormalizeHost(h); err == nil {
			t.Errorf("host %q accepté à tort", h)
		}
	}
}

func TestValidateIdentifier(t *testing.T) {
	for _, id := range []string{"shopfr", "shopfrprep", "s1001pneusfr"} {
		if err := ValidateIdentifier(id); err != nil {
			t.Errorf("%q refusé : %v", id, err)
		}
	}
	for _, id := range []string{"root", "www-data", "mysql", "ogs", "1shop", "Shop", "a", "shop.fr", ""} {
		if err := ValidateIdentifier(id); err == nil {
			t.Errorf("%q accepté à tort", id)
		}
	}
}

func TestEscapeMySQLPattern(t *testing.T) {
	if got := EscapeMySQLPattern("shopfr_analytics"); got != `shopfr\_analytics` {
		t.Errorf("underscore non échappé : %q", got)
	}
	if got := EscapeMySQLPattern("shopfr"); got != "shopfr" {
		t.Errorf("nom sans joker modifié : %q", got)
	}
	if got := EscapeMySQLPattern("a%b"); got != `a\%b` {
		t.Errorf("pourcent non échappé : %q", got)
	}
}

func TestPoolNumber(t *testing.T) {
	cases := []struct {
		version   string
		increment int
		want      int
	}{
		{"8.5", 1, 18501},
		{"8.4", 3, 18403},
		{"7.4", 1, 17401},
		{"8.1", 10, 18110},
	}
	for _, c := range cases {
		got, err := PoolNumber(c.version, c.increment)
		if err != nil {
			t.Fatalf("PoolNumber(%s, %d) : %v", c.version, c.increment, err)
		}
		if got != c.want {
			t.Errorf("PoolNumber(%s, %d) = %d, attendu %d", c.version, c.increment, got, c.want)
		}
	}
	if _, err := PoolNumber("8.5", 100); err == nil {
		t.Error("un centième pool sur une version doit être refusé")
	}
	// Limite connue de la norme : au-delà de PHP 9.9 le numéro déborde.
	if _, err := PoolNumber("10.0", 1); err == nil {
		t.Error("PHP 10.0 devrait être refusé (numéro hors plage)")
	}
}

func TestAppPort(t *testing.T) {
	cases := []struct {
		runtime   string
		increment int
		want      int
	}{
		{"go", 1, 21001},
		{"node", 1, 22001},
		{"python", 3, 23003},
		{"ruby", 999, 24999},
	}
	for _, c := range cases {
		got, err := AppPort(c.runtime, c.increment)
		if err != nil {
			t.Fatalf("AppPort(%s, %d) : %v", c.runtime, c.increment, err)
		}
		if got != c.want {
			t.Errorf("AppPort(%s, %d) = %d, attendu %d", c.runtime, c.increment, got, c.want)
		}
	}
	if _, err := AppPort("perl", 1); err == nil {
		t.Error("runtime inconnu accepté à tort")
	}
}

func TestParseEnv(t *testing.T) {
	if e, err := ParseEnv(""); err != nil || e != EnvProd {
		t.Errorf("défaut = %q, %v", e, err)
	}
	if e, _ := ParseEnv("PREP"); e != EnvPrep {
		t.Errorf("casse non normalisée : %q", e)
	}
	if _, err := ParseEnv("staging"); err == nil {
		t.Error("environnement inconnu accepté")
	}
}
