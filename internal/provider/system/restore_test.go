package system

import (
	"testing"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/spec"
)

// compteRecree déclare le compte du projet tel que l'apply de la restauration l'a
// recréé, sous des identifiants qui ne sont pas ceux d'origine.
func compteRecree(t *testing.T, prov *Provider, ident *FakeIdentity) (user, root fs.Owner) {
	t.Helper()
	ident.Users["shopfr"] = &UserInfo{Name: "shopfr", UID: 1004, GID: 1004, Managed: true}
	ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: 1004}
	prov.Exec = &exec.Local{}
	return fs.Owner{UID: 1004, GID: 1004}, fs.Owner{UID: 0, GID: 1004}
}

func TestRestoredOwnerRendToutLeHomeALUtilisateurHorsDesRepertoiresRoot(t *testing.T) {
	// La restauration ne rendait que www/ : authorized_keys, sessions, caches, et
	// tout le site d'un projet adopté servi hors de www/ restaient à root. Une
	// archive qui ne porte aucun propriétaire rend tout à l'utilisateur, sauf ce
	// que la déclaration donne à root — et un répertoire de l'utilisateur déclaré
	// sous celui-ci lui reste.
	p, prov, _, ident := fixture(t)
	p.Spec.Layout.Dirs = append(p.Spec.Layout.Dirs,
		model.Dir{Path: "outils", Mode: "0710", Owner: "root"},
		model.Dir{Path: "outils/cache", Mode: "0700"})
	user, root := compteRecree(t, prov, ident)

	owner, err := prov.RestoredOwner(p, p.Spec.Layout.Dirs, nil)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]fs.Owner{
		"www/public/index.php":       user,
		".ssh/authorized_keys":       user,
		"var/sessions/sess_abc":      user,
		".config/composer/auth.json": user,
		"public_html/index.php":      user,
		".local/bin/php":             user,
		"logs":                       user,
		"logs/depose.txt":            user,
		"outils":                     root,
		"outils/script.sh":           root,
		"outils/cache":               user,
		"outils/cache/x":             user,
	} {
		if got := owner(rel, fs.Owner{}); got != want {
			t.Errorf("%s : propriétaire %+v, attendu %+v", rel, got, want)
		}
	}
}

func TestRestoredOwnerRendARootLAncienLogsDUneArchiveAnterieureAD71(t *testing.T) {
	// Une archive antérieure à D71, et aux propriétaires portés : son ~/logs est le
	// répertoire root des journaux, et sa déclaration le dit (logs, 0710, root).
	// La normalisation retire cette entrée — c'est ce qui déclenche la migration —,
	// et la restauration, qui décidait d'après la déclaration normalisée, rendait
	// ~/logs et ses journaux à l'utilisateur : la migration refusait ensuite ce
	// répertoire à chaque apply comme substitué, et les journaux n'atteignaient
	// jamais logs.root. C'est la déclaration archivée qui décide.
	ancien, prov, _, ident := fixture(t)
	ancien.Spec.Layout.LogDir = model.LegacyLogDir(ancien.Spec.Layout.Root)
	ancien.Spec.Layout.Dirs = append(ancien.Spec.Layout.Dirs, model.Dir{Path: "logs", Mode: "0710", Owner: "root"})
	archivee, err := spec.Encode(ancien)
	if err != nil {
		t.Fatal(err)
	}

	// Ce que fait la restauration : la déclaration lue dans l'archive, puis
	// normalisée avant l'apply.
	p, err := spec.Decode(archivee)
	if err != nil {
		t.Fatal(err)
	}
	archived := append([]model.Dir(nil), p.Spec.Layout.Dirs...)
	if err := p.Normalize(t.TempDir(), "preprod-pp.net", model.WithLogsRoot(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	for _, d := range p.Spec.Layout.Dirs {
		if d.Path == "logs" {
			t.Fatalf("la normalisation devait retirer l'ancienne entrée logs : %+v", p.Spec.Layout.Dirs)
		}
	}
	user, root := compteRecree(t, prov, ident)

	owner, err := prov.RestoredOwner(p, archived, nil)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]fs.Owner{
		"logs":                   root,
		"logs/apache-access.log": root,
		"logs/php-error.log.1":   root,
		"www/index.php":          user,
		".ssh/authorized_keys":   user,
	} {
		if got := owner(rel, fs.Owner{}); got != want {
			t.Errorf("%s : propriétaire %+v, attendu %+v", rel, got, want)
		}
	}
}

func TestRestoredOwnerSuitLesProprietairesPortesParLArchive(t *testing.T) {
	// Une archive récente porte le propriétaire de chaque entrée, et celui de la
	// racine — le home, à l'utilisateur d'origine (2001:2001). Ce qui était à lui
	// revient au compte recréé (1004:1004) ; le reste revient à root, dans le
	// groupe du projet seulement si c'était le sien. Le rendre à l'utilisateur lui
	// aurait donné, par un lien dur posé dans son home, un fichier du système.
	p, prov, _, ident := fixture(t)
	p.Spec.Layout.Dirs = append(p.Spec.Layout.Dirs, model.Dir{Path: "logs", Mode: "0710", Owner: "root"})
	user, root := compteRecree(t, prov, ident)

	owner, err := prov.RestoredOwner(p, p.Spec.Layout.Dirs, &fs.Owner{UID: 2001, GID: 2001})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		nom, rel string
		orig     fs.Owner
		want     fs.Owner
	}{
		{"fichier de l'utilisateur", "www/index.php", fs.Owner{UID: 2001, GID: 2001}, user},
		{"fichier de l'utilisateur dans un autre de ses groupes", ".config/x", fs.Owner{UID: 2001, GID: 50}, user},
		{"ancien journal root du projet", "logs/apache-access.log", fs.Owner{UID: 0, GID: 2001}, root},
		{"lien dur vers /etc/shadow", "www/shadow", fs.Owner{UID: 0, GID: 42}, fs.Owner{UID: 0, GID: 0}},
		{"fichier de www-data d'un site adopté", "www/uploads/a.jpg", fs.Owner{UID: 33, GID: 33}, fs.Owner{UID: 0, GID: 0}},
		{"fichier d'un voisin dans le groupe du projet", "www/v", fs.Owner{UID: 1005, GID: 2001}, root},
		// Le propriétaire porté l'emporte sur la déclaration : un ~/logs que
		// l'utilisateur a substitué revient à lui, et la migration le refuse à
		// bon droit au lieu de reprendre son contenu comme celui de root.
		{"répertoire déclaré root, mais substitué par l'utilisateur", "logs/depose.txt", fs.Owner{UID: 2001, GID: 2001}, user},
	} {
		if got := owner(c.rel, c.orig); got != c.want {
			t.Errorf("%s (%s, %+v) : propriétaire %+v, attendu %+v", c.nom, c.rel, c.orig, got, c.want)
		}
	}

	// Une racine archivée au compte de root ne dit pas ce qui était au projet :
	// la déclaration décide, comme pour une archive qui ne porte rien.
	owner, err = prov.RestoredOwner(p, p.Spec.Layout.Dirs, &fs.Owner{UID: 0, GID: 0})
	if err != nil {
		t.Fatal(err)
	}
	if got := owner("www/index.php", fs.Owner{UID: 0, GID: 0}); got != user {
		t.Errorf("racine à root : www/index.php à %+v, attendu %+v d'après la déclaration", got, user)
	}
}

func TestRestoredOwnerNeChangeRienEnModeSimule(t *testing.T) {
	// Sous préfixe, l'essai tourne sans privilèges : aucun propriétaire n'est posé,
	// et le compte n'existe pas.
	p, prov, _, _ := fixture(t)
	owner, err := prov.RestoredOwner(p, p.Spec.Layout.Dirs, &fs.Owner{UID: 2001, GID: 2001})
	if err != nil {
		t.Fatal(err)
	}
	if got := owner("www/index.php", fs.Owner{UID: 2001, GID: 2001}); got != fs.KeepOwner {
		t.Errorf("mode simulé : propriétaire %+v, attendu fs.KeepOwner", got)
	}
}
