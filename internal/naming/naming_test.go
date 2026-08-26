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
	prep, err := ID(host, EnvPrep, "preprod-pp.net")
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
	prod, err := ID("shop.fr", EnvProd, "preprod-pp.net")
	if err != nil {
		t.Fatal(err)
	}
	prep, err := ID("shop.fr", EnvPrep, "preprod-pp.net")
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
	// L'extension du host de production ne participe pas au sous-domaine (D58) :
	// elle n'apporte rien à un nom qui vit sur un domaine dédié.
	if h != "shop.preprod-pp.net" {
		t.Errorf("prep : %q", h)
	}
	// Le sous-domaine reprend l'identifiant de production, pas celui de préproduction.
	if h == "shopprep.preprod-pp.net" {
		t.Errorf("le suffixe prep ne doit pas apparaître dans le host")
	}
	if _, err := EffectiveHost("shop.fr", EnvPrep, ""); err == nil {
		t.Errorf("un domaine de préproduction absent doit être une erreur")
	}
}

func TestPrepLabelIgnoresExtension(t *testing.T) {
	for host, want := range map[string]string{
		"jardindeco.fr":  "jardindeco",
		"api.shop.fr":    "apishop",
		"mon-shop.co.uk": "monshopco",
		"1001pneus.fr":   "s1001pneus",
	} {
		got, err := PrepLabel(host)
		if err != nil {
			t.Fatalf("%s : %v", host, err)
		}
		if got != want {
			t.Errorf("PrepLabel(%q) = %q, attendu %q", host, got, want)
		}
	}
	// Le label reste un label DNS valide, donc borné comme un identifiant.
	long, err := PrepLabel("ma-boutique-de-vente-en-ligne-de-chaussures.fr")
	if err != nil {
		t.Fatal(err)
	}
	if len(long) > MaxBase {
		t.Errorf("label trop long : %q (%d)", long, len(long))
	}
}

// Un host déjà posé sur le domaine de préproduction est pris tel quel, et
// l'identifiant se dérive du seul label : le construire une seconde fois donnait
// jardindecopreprodppnet.preprod-pp.net et l'utilisateur jardindecopreprodppnetprep.
func TestPrepHostAlreadyOnPrepDomain(t *testing.T) {
	const host = "jardindeco.preprod-pp.net"
	h, err := EffectiveHost(host, EnvPrep, "preprod-pp.net")
	if err != nil {
		t.Fatal(err)
	}
	if h != host {
		t.Errorf("host effectif %q, attendu %q", h, host)
	}
	id, err := ID(host, EnvPrep, "preprod-pp.net")
	if err != nil {
		t.Fatal(err)
	}
	if id != "jardindecoprep" {
		t.Errorf("identifiant %q, attendu jardindecoprep", id)
	}
	// Le domaine de préproduction nu ne laisse rien à dériver : le host entier
	// fait alors office de base, plutôt qu'une erreur sur un cas inoffensif.
	if id, err := ID("preprod-pp.net", EnvPrep, "preprod-pp.net"); err != nil || id != "preprodppnetprep" {
		t.Errorf("domaine nu : %q, %v", id, err)
	}
}

// Un projet de production sur le domaine de préproduction est refusé (§3.3.6) :
// ce domaine est réservé à l'espace de noms de la préproduction.
func TestProdHostOnPrepDomainRejected(t *testing.T) {
	if _, err := EffectiveHost("jardindeco.preprod-pp.net", EnvProd, "preprod-pp.net"); err == nil {
		t.Error("un host de production sur le domaine de préproduction doit être refusé")
	}
	if _, err := EffectiveHost("preprod-pp.net", EnvProd, "preprod-pp.net"); err == nil {
		t.Error("le domaine de préproduction lui-même doit être refusé en production")
	}
	// Un host qui n'en est pas un sous-domaine reste accepté : la comparaison
	// porte sur les labels, pas sur les caractères.
	if _, err := EffectiveHost("nonpreprod-pp.net", EnvProd, "preprod-pp.net"); err != nil {
		t.Errorf("host voisin refusé à tort : %v", err)
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

func TestUnescapeMySQLPatternIsTheInverseOfEscape(t *testing.T) {
	// La relecture des privilèges rend le motif tel qu'il est stocké. Sans cette
	// conversion, une base dont le nom contient un souligné — le cas de toute base
	// additionnelle, « shopfr_stats » — ne serait jamais vue comme conforme, et le
	// GRANT serait rejoué à chaque apply.
	for _, nom := range []string{"shopfr", "shopfr_stats", "a_b_c", "cent%pour_cent"} {
		if got := UnescapeMySQLPattern(EscapeMySQLPattern(nom)); got != nom {
			t.Errorf("aller-retour de %q = %q", nom, got)
		}
	}
	if got := UnescapeMySQLPattern(`shopfr\_stats`); got != "shopfr_stats" {
		t.Errorf("motif stocké mal interprété : %q", got)
	}
}
