package spec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

const sample = `
apiVersion: ogs/v1
kind: Project
metadata:
  name: shopfr
  host: shop.fr
  env: prod
spec:
  app:
    kind: php-symfony
    version: "6.4"
  user:
    name: shopfr
    group: shopfr
    home: /home/shop.fr
  layout:
    root: /home/shop.fr
    docroot: /home/shop.fr/www/public
    logDir: /home/shop.fr/logs
    mode: "0750"
  domains:
    - name: shop.fr
      role: primary
  databases:
    - engine: mysql
      name: shopfr
      user: shopfr
  php:
    pools:
      - name: web
        version: "8.3"
        preset: medium
`

func TestDecodeAndEncodeRoundTrip(t *testing.T) {
	p, err := Decode([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if p.Metadata.Name != "shopfr" || p.Spec.App.Kind != model.KindSymfony {
		t.Fatalf("décodage inattendu : %+v", p.Metadata)
	}
	data, err := Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Decode(data)
	if err != nil {
		t.Fatalf("le YAML produit n'est pas relisible : %v\n%s", err, data)
	}
	if again.Metadata.Name != p.Metadata.Name || again.Metadata.Host != p.Metadata.Host ||
		again.Metadata.Env != p.Metadata.Env {
		t.Errorf("aller-retour non conservatif :\n%+v\n%+v", p.Metadata, again.Metadata)
	}
	if len(again.Spec.PHP.Pools) != 1 || again.Spec.PHP.Pools[0].Version != "8.3" {
		t.Errorf("pools perdus à l'aller-retour : %+v", again.Spec.PHP.Pools)
	}
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	// Le mode strict évite qu'une faute de frappe passe inaperçue et laisse une
	// ressource silencieusement non gérée.
	_, err := Decode([]byte("apiVersion: ogs/v1\nkind: Project\nmetadata:\n  nam: shopfr\n"))
	if err == nil {
		t.Fatal("un champ inconnu doit être refusé")
	}
	if !strings.Contains(err.Error(), "nam") {
		t.Errorf("l'erreur devrait nommer le champ fautif : %v", err)
	}
}

func TestDecodeRejectsUnknownAPIVersion(t *testing.T) {
	if _, err := Decode([]byte("apiVersion: ogs/v2\nkind: Project\n")); err == nil {
		t.Fatal("une apiVersion inconnue doit être refusée")
	}
}

func TestSaveLoadAll(t *testing.T) {
	dir := t.TempDir()
	for _, host := range []string{"shop.fr", "autre.fr"} {
		p := model.New(host, naming.EnvProd, model.KindStatic)
		if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
			t.Fatal(err)
		}
		if err := Save(filepath.Join(dir, p.ID()+".yaml"), p); err != nil {
			t.Fatal(err)
		}
	}
	list, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("%d projets chargés, 2 attendus", len(list))
	}
	if list[0].ID() != "autrefr" || list[1].ID() != "shopfr" {
		t.Errorf("tri instable : %s, %s", list[0].ID(), list[1].ID())
	}
}

func TestLoadAllOnMissingDir(t *testing.T) {
	list, err := LoadAll(filepath.Join(t.TempDir(), "absent"))
	if err != nil || list != nil {
		t.Errorf("un répertoire absent doit donner une liste vide sans erreur : %v", err)
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("un projet absent doit être signalé comme tel : %v", err)
	}
}

func TestLoadAllStopsOnInvalidFile(t *testing.T) {
	// Un fichier illisible ne doit pas être ignoré en silence : la commande
	// travaillerait alors sur un parc incomplet.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "casse.yaml"), []byte("apiVersion: ogs/v1\nkind: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAll(dir); err == nil {
		t.Fatal("un YAML invalide doit interrompre le chargement")
	}
}

func TestLoadAllIgnoresNonYAMLAndArchived(t *testing.T) {
	dir := t.TempDir()
	p := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if err := Save(filepath.Join(dir, "shopfr.yaml"), p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("rien"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "archived"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Save(filepath.Join(dir, "archived", "vieuxfr.yaml"), p); err != nil {
		t.Fatal(err)
	}
	list, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("%d projets chargés, 1 attendu (les archives et les autres fichiers sont ignorés)", len(list))
	}
}
