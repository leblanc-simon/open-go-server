package naming

import "testing"

func TestValidateGroup(t *testing.T) {
	// Le nom d'un groupe entre dans un chemin et préfixe les bases (D135) : ni
	// souligné, ni point, ni barre, ni compte système.
	for _, ok := range []string{"acme", "betrancourt", "a1", "abcdefghijklmnop"} {
		if err := ValidateGroup(ok); err != nil {
			t.Errorf("%q refusé : %v", ok, err)
		}
	}
	for _, ko := range []string{"", "a", "1acme", "Acme", "ac_me", "ac-me", "ac.me", "..", "a/b",
		"abcdefghijklmnopq", "root", "mysql", "ogs"} {
		if err := ValidateGroup(ko); err == nil {
			t.Errorf("%q accepté", ko)
		}
	}
}

func TestGroupedName(t *testing.T) {
	if got := GroupedName("", "shopfr"); got != "shopfr" {
		t.Errorf("sans groupe : %q", got)
	}
	if got := GroupedName("acme", "shopfr_analytics"); got != "acme_shopfr_analytics" {
		t.Errorf("avec groupe : %q", got)
	}
}
