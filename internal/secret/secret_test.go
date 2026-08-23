package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newKeystore(t *testing.T) (*Keystore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := GenerateKey(path); err != nil {
		t.Fatal(err)
	}
	ks, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return ks, path
}

func TestGenerateKeyPermissions(t *testing.T) {
	_, path := newKeystore(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Errorf("mode %04o, attendu 0400", info.Mode().Perm())
	}
	if err := GenerateKey(path); err == nil {
		t.Error("régénérer par-dessus une clé existante doit être refusé")
	}
}

func TestOpenRejectsLoosePermissions(t *testing.T) {
	_, path := newKeystore(t)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "lisible") {
		t.Errorf("une clé lisible par tous doit être refusée, obtenu : %v", err)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	ks, _ := newKeystore(t)
	secret := []byte("mot-de-passe-mysql")
	nonce, ct, err := ks.Seal("shopfr", "mysql/shopfr", secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ct), "mysql") {
		t.Error("le texte chiffré ne doit pas laisser filtrer le clair")
	}
	got, err := ks.Open("shopfr", "mysql/shopfr", nonce, ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(secret) {
		t.Errorf("secret restitué %q", got)
	}
}

func TestOpenRejectsMovedSecret(t *testing.T) {
	// Les données associées interdisent de réutiliser le texte chiffré d'un
	// projet pour un autre.
	ks, _ := newKeystore(t)
	nonce, ct, _ := ks.Seal("shopfr", "mysql/shopfr", []byte("secret"))
	if _, err := ks.Open("autrefr", "mysql/shopfr", nonce, ct); err == nil {
		t.Error("un secret déplacé vers un autre projet doit être rejeté")
	}
	if _, err := ks.Open("shopfr", "amqp/shopfr", nonce, ct); err == nil {
		t.Error("un secret déplacé vers un autre usage doit être rejeté")
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	ks, _ := newKeystore(t)
	nonce, ct, _ := ks.Seal("shopfr", "mysql/shopfr", []byte("secret"))
	ct[0] ^= 0xff
	if _, err := ks.Open("shopfr", "mysql/shopfr", nonce, ct); err == nil {
		t.Error("un texte chiffré altéré doit être rejeté")
	}
}

func TestOpenRejectsForeignKey(t *testing.T) {
	ks, _ := newKeystore(t)
	other, _ := newKeystore(t)
	nonce, ct, _ := ks.Seal("shopfr", "mysql/shopfr", []byte("secret"))
	if _, err := other.Open("shopfr", "mysql/shopfr", nonce, ct); err == nil {
		t.Error("une autre clé maître ne doit pas pouvoir ouvrir le secret")
	}
}

func TestPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pw, err := Password(0)
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != DefaultPasswordLength {
			t.Fatalf("longueur %d, attendue %d", len(pw), DefaultPasswordLength)
		}
		if strings.ContainsAny(pw, "'\"`\\$ \n\t/O0l1") {
			t.Errorf("caractère problématique dans %q", pw)
		}
		if seen[pw] {
			t.Fatalf("mot de passe répété : %q", pw)
		}
		seen[pw] = true
	}
}
