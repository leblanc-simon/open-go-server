package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copierFixturePrestashop dépose dans le docroot de boutique.fr l'arbre des
// .htaccess livrés par une version de Prestashop (fixtures réelles du provider
// web), son .htaccess racine engendré par Tools::generateHtaccess pour la
// variante donnée, et le fichier où la version se lit.
func copierFixturePrestashop(t *testing.T, h *harness, version, variante string) string {
	t.Helper()
	src := filepath.Join("..", "provider", "web", "testdata", "prestashop", version)
	docroot := h.path("home", "boutique.fr", "www")
	err := filepath.WalkDir(filepath.Join(src, "arbre"), func(chemin string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(filepath.Join(src, "arbre"), chemin)
		return copierFichier(chemin, filepath.Join(docroot, rel))
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := copierFichier(filepath.Join(src, "racine-"+variante+".htaccess"), filepath.Join(docroot, ".htaccess")); err != nil {
		t.Fatal(err)
	}
	ecrireVersionPrestashop(t, docroot, version)
	return docroot
}

func ecrireVersionPrestashop(t *testing.T, docroot, version string) {
	t.Helper()
	fichier, ligne := filepath.Join("src", "Core", "Version.php"), "    public const VERSION = '"+version+"';\n"
	if strings.HasPrefix(version, "1.7.") {
		fichier, ligne = filepath.Join("app", "AppKernel.php"), "    const VERSION = '"+version+"';\n"
	}
	for rel, corps := range map[string]string{"index.php": "<?php\n", fichier: "<?php\nclass X\n{\n" + ligne} {
		f := filepath.Join(docroot, rel)
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(corps), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func copierFichier(de, vers string) error {
	c, err := os.ReadFile(de)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(vers), 0o755); err != nil {
		return err
	}
	return os.WriteFile(vers, c, 0o644)
}

// lignesDoctor rend les lignes de la sortie de doctor qui nomment ce contrôle.
func lignesDoctor(out, nom string) string {
	var garde []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, nom) {
			garde = append(garde, l)
		}
	}
	return strings.Join(garde, "\n")
}

// doctor taisait les .htaccess livrés par une 8 seulement : toute 1.7 ordinaire
// recevait « directives propres dans admin…, img, js », toute 9 « admin-api,
// admin… », et rendait le code 6 à chaque passage. Les fixtures sont les
// .htaccess réels de chaque version (D99).
func TestDoctorTaitLesHtaccessLivresParChaqueVersionDePrestashop(t *testing.T) {
	for version, branche := range map[string]string{"1.7.8.11": "1.7", "8.1.7": "8", "9.0.0": "9"} {
		t.Run(version, func(t *testing.T) {
			h := newHarness(t)
			h.mustRun("init")
			h.mustRun("project", "create", "boutique.fr", "--kind", "php-prestashop", "--php", "8.4")
			copierFixturePrestashop(t, h, version, "ordinaire")
			h.run("apply", "--yes")
			out, _ := h.run("doctor")
			if l := lignesDoctor(out, "htaccess boutiquefr"); l != "" {
				t.Errorf("une boutique %s ordinaire ne doit rien signaler de ses .htaccess :\n%s", version, l)
			}
			if l := lignesDoctor(out, "prestashop boutiquefr"); !strings.Contains(l, "vhost aux règles de la "+branche) {
				t.Errorf("doctor doit dire la branche servie (%s) :\n%s", branche, out)
			}
		})
	}
}

// Ce que le template ne reprend pas est rappelé à chaque passage : boutique en
// sous-répertoire, URI virtuelle, version changée depuis le dernier apply,
// version indéterminée.
func TestDoctorRappelleCeQueLeVhostPrestashopNePortePas(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "boutique.fr", "--kind", "php-prestashop", "--php", "8.4")
	docroot := copierFixturePrestashop(t, h, "8.1.7", "sub")
	h.run("apply", "--yes")
	out, _ := h.run("doctor")
	if l := lignesDoctor(out, "htaccess boutiquefr"); !strings.Contains(l, "sous-répertoire") || !strings.Contains(l, "/sub/") {
		t.Errorf("la boutique en sous-répertoire doit être rappelée :\n%s", out)
	}
	// La même, installée dans www/sub/ : doctor nomme le répertoire.
	if err := copierFichier(filepath.Join(docroot, ".htaccess"), filepath.Join(docroot, "sub", ".htaccess")); err != nil {
		t.Fatal(err)
	}
	out, _ = h.run("doctor")
	if l := lignesDoctor(out, "htaccess boutiquefr"); !strings.Contains(l, "sub/ : boutique en sous-répertoire") {
		t.Errorf("la boutique installée dans sub/ doit être nommée :\n%s", out)
	}
	if err := os.Remove(filepath.Join(docroot, "sub", ".htaccess")); err != nil {
		t.Fatal(err)
	}
	if err := copierFichier(filepath.Join("..", "provider", "web", "testdata", "prestashop", "8.1.7",
		"racine-virtuelle.htaccess"), filepath.Join(docroot, ".htaccess")); err != nil {
		t.Fatal(err)
	}
	out, _ = h.run("doctor")
	if l := lignesDoctor(out, "htaccess boutiquefr"); !strings.Contains(l, "/shop2/") {
		t.Errorf("l'URI virtuelle doit être rappelée :\n%s", out)
	}
	// Mise à jour vers la 9 faite sans ogs : le vhost porte encore les règles de la 8.
	ecrireVersionPrestashop(t, docroot, "9.0.0")
	out, _ = h.run("doctor")
	if l := lignesDoctor(out, "prestashop boutiquefr"); !strings.Contains(l, "ne porte pas les règles de la 9") ||
		!strings.Contains(l, "ogs apply boutiquefr") {
		t.Errorf("une version changée depuis le rendu du vhost doit être signalée :\n%s", out)
	}
	// Plus aucune version lisible.
	if err := os.Remove(filepath.Join(docroot, "src", "Core", "Version.php")); err != nil {
		t.Fatal(err)
	}
	out, _ = h.run("doctor")
	if l := lignesDoctor(out, "prestashop boutiquefr"); !strings.Contains(l, "indéterminée") {
		t.Errorf("une version indéterminée doit être signalée :\n%s", out)
	}
}
