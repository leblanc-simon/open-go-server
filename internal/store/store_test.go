package store

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenAppliesMigrationsAndRestrictsMode(t *testing.T) {
	s := open(t)
	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != len(migrations) {
		t.Errorf("version de schéma %d, attendue %d", v, len(migrations))
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o, attendu 0600 (la base contient les secrets)", info.Mode().Perm())
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("réouverture impossible : %v", err)
	}
	defer s2.Close()
	v, _ := s2.SchemaVersion()
	if v != len(migrations) {
		t.Errorf("les migrations ont été rejouées : version %d", v)
	}
}

func row(id string) ProjectRow {
	return ProjectRow{ID: id, Host: id + ".fr", Env: "prod", EffectiveHost: id + ".fr",
		Home: "/home/" + id + ".fr", AppKind: "static"}
}

func TestProjectCRUD(t *testing.T) {
	s := open(t)
	if err := s.UpsertProject(row("shopfr")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Project("shopfr")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" || got.Home != "/home/shopfr.fr" {
		t.Errorf("projet lu : %+v", got)
	}
	// Upsert : pas de doublon, mise à jour en place.
	r := row("shopfr")
	r.AppKind = "php-symfony"
	if err := s.UpsertProject(r); err != nil {
		t.Fatal(err)
	}
	list, err := s.Projects()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].AppKind != "php-symfony" {
		t.Errorf("liste : %+v", list)
	}
	if err := s.SetProjectStatus("shopfr", "archived"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Project("shopfr")
	if got.Status != "archived" {
		t.Errorf("statut %q", got.Status)
	}
	if err := s.SetProjectStatus("inconnu", "archived"); !errors.Is(err, ErrNotFound) {
		t.Errorf("statut d'un projet inconnu : %v", err)
	}
	if err := s.DeleteProject("shopfr"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Project("shopfr"); !errors.Is(err, ErrNotFound) {
		t.Errorf("après suppression : %v", err)
	}
}

func TestManagedFiles(t *testing.T) {
	s := open(t)
	if err := s.UpsertProject(row("shopfr")); err != nil {
		t.Fatal(err)
	}
	path := "/etc/apache2/sites-available/shopfr.conf"
	if err := s.RecordFile(path, "shopfr", "vhost/shopfr", "abc123", "0644"); err != nil {
		t.Fatal(err)
	}
	fp, err := s.FileFingerprint(path)
	if err != nil || fp != "abc123" {
		t.Fatalf("empreinte %q, %v", fp, err)
	}
	if err := s.RecordFile(path, "shopfr", "vhost/shopfr", "def456", "0644"); err != nil {
		t.Fatal(err)
	}
	fp, _ = s.FileFingerprint(path)
	if fp != "def456" {
		t.Errorf("empreinte non mise à jour : %q", fp)
	}
	if err := s.ForgetFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileFingerprint(path); !errors.Is(err, ErrNotFound) {
		t.Errorf("après oubli : %v", err)
	}
}

func TestManagedFilesCascadeOnProjectDelete(t *testing.T) {
	s := open(t)
	if err := s.UpsertProject(row("shopfr")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFile("/etc/cron.d/ogs-shopfr", "shopfr", "cron/shopfr", "x", "0644"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject("shopfr"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileFingerprint("/etc/cron.d/ogs-shopfr"); !errors.Is(err, ErrNotFound) {
		t.Errorf("les fichiers doivent disparaître avec le projet : %v", err)
	}
}

func TestAllocateIsStableAndFillsGaps(t *testing.T) {
	s := open(t)
	// Trois pools sur PHP 8.5 : 18501, 18502, 18503.
	for i, name := range []string{"a", "b", "c"} {
		n, err := s.Allocate("php-8.5", name, "web", 18501, 18599)
		if err != nil {
			t.Fatal(err)
		}
		if want := 18501 + i; n != want {
			t.Fatalf("pool %s : numéro %d, attendu %d", name, n, want)
		}
	}
	// Réallocation : le même numéro est rendu, sans consommer de place.
	again, err := s.Allocate("php-8.5", "b", "web", 18501, 18599)
	if err != nil || again != 18502 {
		t.Fatalf("attribution instable : %d, %v", again, err)
	}
	// Libération puis nouvelle demande : le trou est réutilisé.
	if err := s.Release("php-8.5", "b", "web"); err != nil {
		t.Fatal(err)
	}
	n, err := s.Allocate("php-8.5", "d", "web", 18501, 18599)
	if err != nil || n != 18502 {
		t.Fatalf("le numéro libéré doit être réutilisé : %d, %v", n, err)
	}
	// Les plages sont indépendantes d'une version à l'autre.
	n, err = s.Allocate("php-7.4", "a", "web", 17401, 17499)
	if err != nil || n != 17401 {
		t.Fatalf("plage 7.4 : %d, %v", n, err)
	}
}

func TestAllocateExhaustion(t *testing.T) {
	s := open(t)
	if _, err := s.Allocate("php-8.5", "a", "web", 18501, 18501); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Allocate("php-8.5", "b", "web", 18501, 18501); err == nil {
		t.Error("une plage saturée doit produire une erreur explicite")
	}
}

func TestSecrets(t *testing.T) {
	s := open(t)
	if err := s.PutSecret("shopfr", "mysql/shopfr", []byte("nonce"), []byte("chiffré")); err != nil {
		t.Fatal(err)
	}
	nonce, ct, err := s.GetSecret("shopfr", "mysql/shopfr")
	if err != nil {
		t.Fatal(err)
	}
	if string(nonce) != "nonce" || string(ct) != "chiffré" {
		t.Errorf("secret restitué : %q, %q", nonce, ct)
	}
	names, err := s.SecretNames("shopfr")
	if err != nil || len(names) != 1 || names[0] != "mysql/shopfr" {
		t.Errorf("noms : %v, %v", names, err)
	}
	if _, _, err := s.GetSecret("shopfr", "absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("secret absent : %v", err)
	}
}

func TestAudit(t *testing.T) {
	s := open(t)
	base := time.Now().Add(-2 * time.Hour)
	for i, action := range []string{"project.create", "user.create", "apply"} {
		if err := s.AppendAudit(AuditEntry{
			RunID: "run1", At: base.Add(time.Duration(i) * time.Minute),
			Project: "shopfr", Action: action, Outcome: "ok",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendAudit(AuditEntry{RunID: "run2", Project: "autrefr", Action: "apply", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Audit("", time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("%d entrées, 4 attendues", len(all))
	}
	if all[0].Project != "autrefr" {
		t.Errorf("le journal doit être rendu du plus récent au plus ancien, obtenu %q", all[0].Project)
	}
	filtered, err := s.Audit("shopfr", time.Time{}, 10)
	if err != nil || len(filtered) != 3 {
		t.Errorf("filtre par projet : %d entrées, %v", len(filtered), err)
	}
	recent, err := s.Audit("", base.Add(90*time.Second), 10)
	if err != nil || len(recent) != 2 {
		t.Errorf("filtre temporel : %d entrées, %v", len(recent), err)
	}
}

func TestACMEFailureBackoff(t *testing.T) {
	s := open(t)
	key := "shop.fr"
	// Sous le seuil, aucune suspension : un échec ponctuel (DNS non propagé) ne
	// doit pas bloquer les tentatives suivantes.
	for i := 1; i < ACMEFailureThreshold; i++ {
		count, until, err := s.RecordACMEFailure(key, "challenge échoué")
		if err != nil {
			t.Fatal(err)
		}
		if count != i {
			t.Errorf("compteur %d, attendu %d", count, i)
		}
		if !until.IsZero() {
			t.Errorf("suspension prématurée au %d-ème échec", i)
		}
	}
	// Au seuil, les tentatives sont suspendues : insister ferait perdre le droit
	// d'émettre pour le domaine.
	count, until, err := s.RecordACMEFailure(key, "challenge échoué")
	if err != nil {
		t.Fatal(err)
	}
	if count != ACMEFailureThreshold || until.IsZero() {
		t.Fatalf("seuil non appliqué : compteur %d, jusqu'à %v", count, until)
	}
	got, paused, err := s.ACMEPause(key)
	if err != nil {
		t.Fatal(err)
	}
	if !paused || got.Before(time.Now()) {
		t.Errorf("suspension non active : %v, %v", got, paused)
	}
	// Un autre domaine n'est pas affecté.
	if _, paused, _ := s.ACMEPause("autre.fr"); paused {
		t.Error("la suspension doit être propre à la cible")
	}
	// Un succès remet le compteur à zéro.
	if err := s.ClearACMEFailures(key); err != nil {
		t.Fatal(err)
	}
	if _, paused, _ := s.ACMEPause(key); paused {
		t.Error("le succès doit lever la suspension")
	}
}

// TestOuvertureRestreintLeJournal vérifie que le journal WAL et son index ne sont
// pas lisibles par tous : ils vivent dans /var/lib/ogs, traversable à dessein
// (0751, pour le challenge ACME), et portent les pages récemment écrites —
// inventaire du parc et journal d'audit compris. SQLite calque leur mode sur
// celui de la base, qu'il faut donc poser avant la première écriture.
//
// L'umask est fixé explicitement, et le test ne tourne pas en parallèle : il est
// global au processus, et un verdict qui en dépendrait ne vaudrait rien.
func TestOuvertureRestreintLeJournal(t *testing.T) {
	ancien := syscall.Umask(0o022)
	defer syscall.Umask(ancien)

	chemin := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(chemin)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Une écriture, pour que le journal existe réellement.
	if err := s.RecordFile("/etc/apache2/sites-available/ogs-shopfr.conf",
		"", "web/vhost", "abc", "0644"); err != nil {
		t.Fatal(err)
	}
	vu := false
	for _, suffixe := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(chemin + suffixe)
		if err != nil {
			continue
		}
		if suffixe != "" {
			vu = true
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s : mode %04o, 0600 attendu", filepath.Base(chemin+suffixe), info.Mode().Perm())
		}
	}
	if !vu {
		t.Fatal("aucun fichier annexe créé : le test n'éprouve rien")
	}
}

// TestOuvertureRattrapeUnJournalAbandonne : un processus tué laisse son journal
// derrière lui, avec le mode qu'il avait à sa création ; SQLite le rouvre tel
// quel et ne le corrige jamais. Le journal est ici recopié pendant que la base
// est ouverte, puis remis en place après la fermeture : c'est l'état que laisse
// un arrêt brutal.
//
// Le mode du journal fabriqué est posé après l'écriture et vérifié avant
// l'ouverture : celui que demande os.WriteFile est rogné par l'umask ambiant, et
// sous un umask 077 — poste durci, unité systemd portant UMask=0077 — le journal
// naîtrait en 0600, où le test passerait sans rien éprouver.
func TestOuvertureRattrapeUnJournalAbandonne(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(chemin)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFile("/x", "", "r", "f", "0644"); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(chemin + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if len(journal) == 0 {
		t.Fatal("journal vide : le test n'éprouve rien")
	}
	s.Close()
	if err := os.WriteFile(chemin+"-wal", journal, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(chemin+"-wal", 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(chemin + "-wal"); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o644 {
		t.Fatalf("journal fabriqué en %04o : le test n'éprouve rien", info.Mode().Perm())
	}

	s2, err := Open(chemin)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	info, err := os.Stat(chemin + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("journal abandonné : mode %04o, 0600 attendu", info.Mode().Perm())
	}
}

// Claim reprend dans la base un numéro que le disque atteste — l'unité ou le
// pool écrits — sans jamais le prendre à une autre ressource : c'est ainsi
// qu'une base d'état perdue retrouve les ports en service au lieu de les
// redistribuer dans l'ordre alphabétique (R2-18).
func TestClaimReprendUnNumeroSansLeVolerAPersonne(t *testing.T) {
	s := open(t)
	ok, err := s.Claim("app-go", "bbfr", "app", 21001)
	if err != nil || !ok {
		t.Fatalf("un numéro libre doit être repris : %v %v", ok, err)
	}
	// Déjà tenu par une autre ressource : refusé, la base reste inchangée.
	if ok, err := s.Claim("app-go", "aafr", "app", 21001); err != nil || ok {
		t.Fatalf("un numéro tenu ne doit pas être repris : %v %v", ok, err)
	}
	// La ressource a déjà son numéro : il n'est pas remplacé.
	if ok, err := s.Claim("app-go", "bbfr", "app", 21005); err != nil || ok {
		t.Fatalf("un numéro attribué ne doit pas être remplacé : %v %v", ok, err)
	}
	n, err := s.Allocate("app-go", "aafr", "app", 21001, 21999)
	if err != nil || n != 21002 {
		t.Fatalf("l'attribution doit contourner le numéro repris : %d %v", n, err)
	}
	if n, err := s.Allocate("app-go", "bbfr", "app", 21001, 21999); err != nil || n != 21001 {
		t.Fatalf("le numéro repris doit être rendu : %d %v", n, err)
	}
}

// Un numéro déjà en écoute sur la machine n'est jamais attribué : le port
// prévisible d'un projet à venir, pris d'avance par un autre compte, lui
// aurait livré tout le trafic du site (S2R-02).
func TestAllocateEviteUnNumeroOccupe(t *testing.T) {
	s := open(t)
	occupé := func(n int) bool { return n == 21001 || n == 21002 }
	n, err := s.Allocate("app-go", "aafr", "app", 21001, 21999, occupé)
	if err != nil || n != 21003 {
		t.Fatalf("attribué %d, attendu 21003 : %v", n, err)
	}
	// Un numéro déjà attribué reste celui de la ressource, occupé ou non : c'est
	// sa propre application qui l'écoute.
	n, err = s.Allocate("app-go", "aafr", "app", 21001, 21999, func(int) bool { return true })
	if err != nil || n != 21003 {
		t.Fatalf("attribution instable : %d %v", n, err)
	}
}

// D100 : l'aperçu n'inscrit rien mais ne promet pas deux fois le même numéro ;
// un numéro imposé remplace celui de la ressource sans jamais reprendre celui
// d'une autre ; l'annulation retire ce qu'un plan non appliqué a réservé.
func TestAperçuRéservationEtAnnulation(t *testing.T) {
	s := open(t)
	if _, err := s.Allocate("php-8.5", "a", "web", 18501, 18599); err != nil {
		t.Fatal(err)
	}
	s.PreviewAllocations(true)
	b, _ := s.Allocate("php-8.5", "b", "web", 18501, 18599)
	c, _ := s.Allocate("php-8.5", "c", "web", 18501, 18599)
	again, _ := s.Allocate("php-8.5", "b", "web", 18501, 18599)
	if b != 18502 || c != 18503 || again != b {
		t.Errorf("aperçu : b=%d c=%d b encore=%d", b, c, again)
	}
	if h, err := s.Reserve("php-8.5", "d", "web", 18540); err != nil || h != nil {
		t.Fatalf("réservation en aperçu : %v %v", h, err)
	}
	if err := s.Release("php-8.5", "a", "web"); err != nil {
		t.Fatal(err)
	}
	s.PreviewAllocations(false)
	if list, _ := s.Allocations("php-8.5"); len(list) != 1 || list[0].Project != "a" {
		t.Fatalf("l'aperçu ne doit rien écrire : %v", list)
	}

	if n, _ := s.Allocate("php-8.5", "b", "web", 18501, 18599); n != 18502 {
		t.Fatalf("b=%d", n)
	}
	mark := s.AllocationMark()
	if h, err := s.Reserve("php-8.5", "b", "web", 18501); err != nil || h == nil || h.Project != "a" {
		t.Fatalf("un numéro tenu par une autre ressource n'est jamais repris : %v %v", h, err)
	}
	if h, err := s.Reserve("php-8.5", "b", "web", 18550); err != nil || h != nil {
		t.Fatalf("réservation : %v %v", h, err)
	}
	if list, _ := s.Allocations("php-8.5"); len(list) != 2 || list[1].Number != 18550 {
		t.Fatalf("le numéro imposé remplace l'ancien : %v", list)
	}
	if n, _ := s.Allocate("php-8.5", "e", "web", 18501, 18599); n != 18502 {
		t.Fatalf("e=%d", n)
	}
	if err := s.CancelAllocations(mark); err != nil {
		t.Fatal(err)
	}
	// L'annulation retire e et rend à b le numéro que la réservation avait
	// remplacé.
	if list, _ := s.Allocations("php-8.5"); len(list) != 2 || list[1].Project != "b" || list[1].Number != 18502 {
		t.Errorf("l'annulation doit retirer e et rendre 18502 à b : %v", list)
	}
}

// La base en mémoire du renouvellement sans state.db (D120) est migrée comme
// l'autre, ne laisse aucun fichier et se dit éphémère.
func TestOpenMemoryNeCreeAucunFichier(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Ephemeral() {
		t.Error("la base en mémoire doit se dire éphémère")
	}
	if v, err := s.SchemaVersion(); err != nil || v != len(migrations) {
		t.Errorf("schéma %d (%v), attendu %d", v, err, len(migrations))
	}
	if err := s.UpsertProject(ProjectRow{ID: "shopfr", Host: "shop.fr", Env: "prod", EffectiveHost: "shop.fr",
		Home: "/home/shopfr", AppKind: "static"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Project("shopfr"); err != nil {
		t.Errorf("la base en mémoire doit relire ce qu'elle a reçu : %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("aucun fichier ne doit être créé, trouvé %d entrée(s)", len(entries))
	}
}
