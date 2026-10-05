package monitoring

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/fs"
)

// Les adresses gardées quand ufw.yaml ne se lit pas sont celles des prisons
// d'ogs, jamais celles d'une prison de l'administrateur qui partagerait le
// fichier (D109).
func TestPreviousIgnoreIPNeLitQueLesPrisonsDOgs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ogs.conf")
	body := "# Généré par ogs\n" +
		"[ogs-sshd]\nenabled = true\nignoreip = 127.0.0.1/8 ::1 203.0.113.10\n\n" +
		"[ogs-recidive]\nignoreip = 127.0.0.1/8 ::1 203.0.113.10 198.51.100.0/24\n\n" +
		"[sshd-maison]\nignoreip = 192.0.2.1\n"
	content := fs.Render(fs.Header{Project: "1 projet(s)", Resource: "monitoring/fail2ban", Generated: time.Now()},
		[]byte(body), "#")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	want := []string{"127.0.0.1/8", "::1", "203.0.113.10", "198.51.100.0/24"}
	if got, err := PreviousIgnoreIP(path); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("adresses reprises : %v (%v), attendu %v", got, err, want)
	}
	if got, err := PreviousIgnoreIP(filepath.Join(t.TempDir(), "absent")); got != nil || err != nil {
		t.Errorf("fichier absent : rien à reprendre, obtenu %v %v", got, err)
	}

	// Retouché à la main, le fichier porterait des adresses que personne n'a
	// déclarées de confiance : rien n'est repris, et l'erreur le nomme.
	retouché := strings.Replace(string(content), "203.0.113.10 198.51.100.0/24", "203.0.113.10 0.0.0.0/0", 1)
	if err := os.WriteFile(path, []byte(retouché), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := PreviousIgnoreIP(path); got != nil || err == nil || !strings.Contains(err.Error(), "modifié à la main") {
		t.Errorf("fichier retouché : rien à reprendre, erreur nommée attendue, obtenu %v %v", got, err)
	}
	// Non écrit par ogs : de même.
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := PreviousIgnoreIP(path); got != nil || err == nil {
		t.Errorf("fichier non géré : rien à reprendre, erreur attendue, obtenu %v %v", got, err)
	}
}
