package store

import (
	"errors"
	"os"
	"path/filepath"
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
