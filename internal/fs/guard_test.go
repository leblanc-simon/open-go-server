package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// déposerDans écrit rel sous racine, en créant ses répertoires.
func déposerDans(t *testing.T, racine, rel string, contenu []byte) {
	t.Helper()
	p := filepath.Join(racine, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, contenu, 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestLaGardeRetientLesCheminsDesignes(t *testing.T) {
	// Un .env.local recopié donnait à la préproduction les identifiants de la
	// production, et à son compte ses secrets. Il n'est plus recopié, pas plus
	// que ce que désignent les autres motifs — un lien de ce nom compris —, et
	// celui que la destination porte déjà, le sien, reste en place.
	src, www := arbreSource(t)
	for rel, contenu := range map[string]string{
		".env":            "APP_ENV=dev\n",
		".env.local":      "DATABASE_URL=mysql://shopfr:prod@127.0.0.1/shopfr\n",
		".env.prod.local": "APP_SECRET=prod\n",
		"config/secrets/prod/prod.decrypt.private.php": "<?php return 'clé';",
		"config/secrets/prod/prod.encrypt.public.php":  "<?php return 'publique';",
		"var/cache/prod/Container.php":                 "<?php // compilé",
		"var/log/prod.log":                             "journal\n",
		"sous/.env.local":                              "hors motif : un niveau de trop\n",
	} {
		déposerDans(t, www, rel, []byte(contenu))
	}
	if err := os.Symlink("/etc/secrets/shop.env", filepath.Join(www, ".env.local.php")); err != nil {
		t.Fatal(err)
	}
	dst, out := arbreDestination(t)
	déposerDans(t, out, ".env.local", []byte("DATABASE_URL=mysql://shopfrprep:prep@127.0.0.1/shopfrprep\n"))

	motif := func(p, raison string) GuardPath { return GuardPath{Pattern: p, Reason: raison} }
	g := Guard{Paths: []GuardPath{
		motif(".env.local", "configuration"), motif(".env.local.php", "configuration"),
		motif(".env.*.local", "configuration"), motif("config/secrets/*/*.decrypt.private.php", "clé du coffre"),
		motif("var/cache", "cache"),
	}}
	skipped, err := CopyTreeGuardedAt(context.Background(), src, "www", dst, "www", -1, KeepOwner, g)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Withheld != 5 || len(skipped.WithheldNamed) != 5 || skipped.Count != 0 || skipped.Masked != 0 {
		t.Errorf("cinq entrées retenues, aucune écartée ni expurgée, obtenu %+v", skipped)
	}
	nommées := strings.Join(skipped.WithheldNamed, "\n")
	for _, want := range []string{`/www/.env.local" : configuration`, `.env.local.php" : configuration`,
		`.env.prod.local" : configuration`, `prod.decrypt.private.php" : clé du coffre`, `/www/var/cache" : cache`} {
		if !strings.Contains(nommées, want) {
			t.Errorf("%q absent des entrées retenues :\n%s", want, nommées)
		}
	}
	for _, rel := range []string{".env.local.php", ".env.prod.local", "config/secrets/prod/prod.decrypt.private.php",
		"var/cache"} {
		if _, err := os.Lstat(filepath.Join(out, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s a été recopié : %v", rel, err)
		}
	}
	intactDans(t, out, ".env.local", "DATABASE_URL=mysql://shopfrprep:prep@127.0.0.1/shopfrprep\n")
	intactDans(t, out, ".env", "APP_ENV=dev\n")
	intactDans(t, out, "config/secrets/prod/prod.encrypt.public.php", "<?php return 'publique';")
	intactDans(t, out, "var/log/prod.log", "journal\n")
	intactDans(t, out, "sous/.env.local", "hors motif : un niveau de trop\n")
}

// intactDans exige que rel, sous racine, porte exactement contenu.
func intactDans(t *testing.T, racine, rel, contenu string) {
	t.Helper()
	if got, err := os.ReadFile(filepath.Join(racine, filepath.FromSlash(rel))); err != nil || string(got) != contenu {
		t.Errorf("%s : %q (%v), attendu %q", rel, got, err, contenu)
	}
}

func TestLaGardeMasqueLesSecretsDansLaCopie(t *testing.T) {
	// Un wp-config.php, un cache compilé, un journal portent le mot de passe de
	// la production : la copie les garde — une application privée de sa
	// configuration peut offrir son assistant d'installation au premier venu —,
	// le secret remplacé par un masque de même longueur. Rien ne se décale, et
	// une occurrence à cheval sur deux blocs de la copie est masquée des deux
	// côtés.
	const secret = "Prod+Mot=de-passe.0123456789abcd"
	src, www := arbreSource(t)
	déposerDans(t, www, "wp-config.php", []byte("<?php define('DB_PASSWORD', '"+secret+"');\n"))
	grand := bytes.Repeat([]byte("."), 300<<10)
	for _, off := range []int{128<<10 - 10, 200 << 10, len(grand) - len(secret)} {
		copy(grand[off:], secret)
	}
	déposerDans(t, www, "var/grand.bin", grand)
	déposerDans(t, www, "propre.txt", []byte("rien à masquer\n"))
	encodé := "Prod%2BMot%3Dde-passe.0123456789abcd"
	déposerDans(t, www, "app.ini", []byte("dsn = amqp://shopfr:"+encodé+"@127.0.0.1/shopfr\n"))
	// Un fichier lié deux fois n'est lu qu'une fois, et ses deux noms désignent
	// la copie expurgée.
	if err := os.Link(filepath.Join(www, "wp-config.php"), filepath.Join(www, "wp-config.lien.php")); err != nil {
		t.Fatal(err)
	}
	dst, out := arbreDestination(t)

	g := Guard{Values: []GuardValue{
		{Value: []byte(secret), Reason: "mot de passe de shopfr"},
		{Value: []byte(encodé), Reason: "mot de passe de shopfr"},
	}}
	skipped, err := CopyTreeGuardedAt(context.Background(), src, "www", dst, "www", -1, KeepOwner, g)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Masked != 3 || skipped.Withheld != 0 || skipped.Count != 0 {
		t.Errorf("trois fichiers expurgés, obtenu %+v", skipped)
	}
	nommés := strings.Join(skipped.MaskedNamed, "\n")
	for _, want := range []string{`wp-config.lien.php" : mot de passe de shopfr`, `grand.bin" : mot de passe de shopfr`,
		`app.ini" : mot de passe de shopfr`} {
		if !strings.Contains(nommés, want) {
			t.Errorf("%q absent des fichiers expurgés :\n%s", want, nommés)
		}
	}
	if strings.Contains(nommés, "propre.txt") {
		t.Errorf("un fichier sans secret ne doit pas être dit expurgé :\n%s", nommés)
	}
	masque := string(MaskOf(len(secret)))
	intactDans(t, out, "wp-config.php", "<?php define('DB_PASSWORD', '"+masque+"');\n")
	intactDans(t, out, "wp-config.lien.php", "<?php define('DB_PASSWORD', '"+masque+"');\n")
	intactDans(t, out, "app.ini", "dsn = amqp://shopfr:"+string(MaskOf(len(encodé)))+"@127.0.0.1/shopfr\n")
	intactDans(t, out, "propre.txt", "rien à masquer\n")
	copie, err := os.ReadFile(filepath.Join(out, "var", "grand.bin"))
	if err != nil {
		t.Fatal(err)
	}
	attendu := bytes.ReplaceAll(grand, []byte(secret), []byte(masque))
	if !bytes.Equal(copie, attendu) || bytes.Contains(copie, []byte(secret)) {
		t.Errorf("le secret doit être masqué partout, bloc à cheval compris : %d occurrence(s) restante(s)",
			bytes.Count(copie, []byte(secret)))
	}
	var a, b unix.Stat_t
	if unix.Stat(filepath.Join(out, "wp-config.php"), &a) != nil || unix.Stat(filepath.Join(out, "wp-config.lien.php"), &b) != nil ||
		a.Ino != b.Ino {
		t.Error("les deux noms du fichier lié doivent désigner la même copie")
	}
}

func TestLaGardeMasqueUnSecretAChevalSurDeuxBlocs(t *testing.T) {
	// Toutes les coupures possibles d'un secret par la frontière des blocs de la
	// copie (128 Kio) : la part déjà écrite est réécrite à sa position.
	const secret = "secret-de-la-production-0123456"
	src, www := arbreSource(t)
	const bloc = 128 << 10
	for coupe := 1; coupe < len(secret); coupe++ {
		contenu := bytes.Repeat([]byte("-"), 2*bloc)
		copy(contenu[bloc-coupe:], secret)
		déposerDans(t, www, fmt.Sprintf("f%03d", coupe), contenu)
	}
	dst, out := arbreDestination(t)
	g := Guard{Values: []GuardValue{{Value: []byte(secret), Reason: "mot de passe"}}}
	skipped, err := CopyTreeGuardedAt(context.Background(), src, "www", dst, "www", -1, KeepOwner, g)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Masked != len(secret)-1 {
		t.Errorf("%d fichiers expurgés, attendu %d", skipped.Masked, len(secret)-1)
	}
	entrées, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entrées {
		data, err := os.ReadFile(filepath.Join(out, e.Name()))
		if err != nil || bytes.Contains(data, []byte(secret)) || !bytes.Contains(data, MaskOf(len(secret))) ||
			len(data) != 2*bloc {
			t.Errorf("%s : secret resté ou masque absent (%d octets, %v)", e.Name(), len(data), err)
		}
	}
}

func TestLaGardeMasqueUnSecretApresUnTrou(t *testing.T) {
	// Les étendues d'un fichier creux se lisent séparément : la fin de l'une ne
	// prolonge pas le début de la suivante, et le fichier reste creux.
	const secret = "secret-de-la-production-0123456"
	src, www := arbreSource(t)
	const taille = 64 << 20
	f, err := os.Create(filepath.Join(www, "creux.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("début "+secret[:10]), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(secret), taille/2); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(taille); err != nil {
		t.Fatal(err)
	}
	f.Close()
	dst, out := arbreDestination(t)

	g := Guard{Values: []GuardValue{{Value: []byte(secret), Reason: "mot de passe"}}}
	skipped, err := CopyTreeGuardedAt(context.Background(), src, "www", dst, "www", -1, KeepOwner, g)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Masked != 1 {
		t.Errorf("le fichier creux doit être dit expurgé : %+v", skipped)
	}
	c, err := os.Open(filepath.Join(out, "creux.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for off, want := range map[int64]string{0: "début " + secret[:10], taille / 2: string(MaskOf(len(secret)))} {
		buf := make([]byte, len(want))
		if _, err := c.ReadAt(buf, off); err != nil || string(buf) != want {
			t.Errorf("à %d : %q, attendu %q (%v)", off, buf, want, err)
		}
	}
	if info, err := c.Stat(); err != nil || info.Size() != taille {
		t.Errorf("taille de la copie : %v, %v", info, err)
	}
}

func TestLaGardeRefuseUnMotifInvalide(t *testing.T) {
	// Une garde qui ne retiendrait rien sans le dire laisserait passer ce
	// qu'elle devait arrêter : rien n'est recopié.
	src, www := arbreSource(t)
	déposerDans(t, www, ".env.local", []byte("secret\n"))
	dst, out := arbreDestination(t)
	for _, g := range []Guard{
		{Paths: []GuardPath{{Pattern: "[.env"}}},
		{Paths: []GuardPath{{Pattern: "../.env"}}},
		{Values: []GuardValue{{Value: nil, Reason: "vide"}}},
	} {
		_, err := CopyTreeGuardedAt(context.Background(), src, "www", dst, "www", -1, KeepOwner, g)
		var root *RootError
		if !errors.As(err, &root) {
			t.Errorf("%+v : une garde invalide doit refuser la copie, obtenu %v", g, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(out, ".env.local")); !os.IsNotExist(err) {
		t.Errorf("une garde invalide a laissé passer la copie : %v", err)
	}
}

func TestLaCopieNommeLesLiensQuiSortentDeLaDestination(t *testing.T) {
	// Un lien est recopié comme lien, jamais suivi : mais le projet de
	// destination le suivra. Un wp-config.php lié à un répertoire partagé du
	// projet source lui donnait la configuration de la production, avec
	// « ✓ contenu copié » pour tout message. Une cible absolue ou qui remonte
	// au-delà de la racine de destination est nommée ; une cible relative qui y
	// reste désigne les fichiers de la destination, et ne l'est pas.
	src, www := arbreSource(t)
	partagé := filepath.Join(src, "shared", "wp-config.php")
	déposerDans(t, src, "shared/wp-config.php", []byte("<?php // production\n"))
	if err := os.MkdirAll(filepath.Join(www, "sous"), 0o755); err != nil {
		t.Fatal(err)
	}
	for nom, cible := range map[string]string{
		"wp-config.php": partagé,
		"jquery":        "/usr/share/javascript/jquery",
		"uploads":       "../shared/uploads",
		"voisin":        "../../autre.fr/shared",
		"sous/public":   "../public",
		"sous/remonte":  "../../../etc",
		"sous/détour":   "a/../../../../x",
	} {
		if err := os.Symlink(cible, filepath.Join(www, filepath.FromSlash(nom))); err != nil {
			t.Fatal(err)
		}
	}
	dst, out := arbreDestination(t)

	skipped, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Outside != 5 || len(skipped.OutsideNamed) != 5 || skipped.Count != 0 {
		t.Errorf("cinq liens hors de la destination, aucune entrée écartée : %+v", skipped)
	}
	nommés := strings.Join(skipped.OutsideNamed, "\n")
	for _, want := range []string{
		`wp-config.php" → "` + partagé + `" : cible absolue, dans le projet source "` + src + `"`,
		`jquery" → "/usr/share/javascript/jquery" : cible absolue, hors de la copie`,
		`voisin" → "../../autre.fr/shared" : cible relative qui sort de "` + dst + `"`,
		`remonte" → "../../../etc" : cible relative`,
		`détour" → "a/../../../../x" : cible relative`,
	} {
		if !strings.Contains(nommés, want) {
			t.Errorf("%q absent des liens nommés :\n%s", want, nommés)
		}
	}
	for _, nom := range []string{`uploads"`, `sous/public"`} {
		if strings.Contains(nommés, nom) {
			t.Errorf("un lien qui reste dans la destination ne doit pas être nommé : %s\n%s", nom, nommés)
		}
	}
	if got, err := os.Readlink(filepath.Join(out, "wp-config.php")); err != nil || got != partagé {
		t.Errorf("le lien est recopié tel quel : %q, %v", got, err)
	}
}

func TestLaGardeRetientLesAutresNomsDUnFichierRetenu(t *testing.T) {
	// Un .env.local lié physiquement ailleurs dans www passait entier sous
	// l'autre nom. Rencontré après le nom retenu, l'autre nom est retenu aussi ;
	// rencontré avant — les noms se visitent dans l'ordre —, il est nommé.
	src, www := arbreSource(t)
	déposerDans(t, www, ".env.local", []byte("APP_SECRET=production\n"))
	for _, nom := range []string{".env.backup", "config/copie.env"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(www, nom)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(www, ".env.local"), filepath.Join(www, filepath.FromSlash(nom))); err != nil {
			t.Fatal(err)
		}
	}
	dst, out := arbreDestination(t)

	g := Guard{Paths: []GuardPath{{Pattern: ".env.local", Reason: "configuration"}}}
	skipped, err := CopyTreeGuardedAt(context.Background(), src, "www", dst, "www", -1, KeepOwner, g)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Withheld != 2 || skipped.Exposed != 1 || len(skipped.ExposedNamed) != 1 {
		t.Fatalf("deux noms retenus, un nommé comme recopié avant le nom retenu : %+v", skipped)
	}
	if !strings.Contains(skipped.ExposedNamed[0], `.env.backup" : même fichier que "`+filepath.Join(www, ".env.local")) {
		t.Errorf("le nom recopié avant le nom retenu doit être nommé : %s", skipped.ExposedNamed[0])
	}
	if !strings.Contains(strings.Join(skipped.WithheldNamed, "\n"), `copie.env" : autre nom de`) {
		t.Errorf("l'autre nom rencontré après doit être retenu : %v", skipped.WithheldNamed)
	}
	for _, rel := range []string{".env.local", "config/copie.env"} {
		if _, err := os.Lstat(filepath.Join(out, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s a été recopié : %v", rel, err)
		}
	}
}

func TestLeMasqueALaLongueurDuSecret(t *testing.T) {
	for _, n := range []int{1, 12, len(guardMask), 32, 64} {
		m := MaskOf(n)
		if len(m) != n || !strings.HasPrefix(guardMask, string(m[:min(n, len(guardMask))])) {
			t.Errorf("masque de %d octets : %q", n, m)
		}
	}
}
