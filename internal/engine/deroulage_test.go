package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// R1-37 : un vidage anticipé (FlushReloads) a fait charger aux démons ce que
// les étapes précédentes avaient écrit. Le déroulage défaisait les fichiers sans
// rien rejouer : FPM gardait le pool défait, ses workers tournaient sous le
// compte que la compensation suivante supprimait, et userdel échouait. Le
// rechargement doit être rejoué après la compensation du pool, avant celle du
// compte.
func TestDéroulageRejoueLesRechargementsDéjàExécutés(t *testing.T) {
	var journal []string
	note := func(s string) { journal = append(journal, s) }
	plan := &Plan{Project: "shopfr"}

	compte := NewStep("system/user/shopfr", Create, "compte")
	compte.Apply = func(context.Context) error { note("useradd"); return nil }
	compte.Compensate = func(context.Context) error { note("userdel"); return nil }
	plan.Add(compte)

	pool := NewStep("php/pool/shopfr", Create, "pool")
	pool.Apply = func(context.Context) error { note("pool écrit"); return nil }
	pool.Compensate = func(context.Context) error { note("pool retiré"); return nil }
	pool.Reload = []string{"php:8.4"}
	plan.Add(pool)

	vhost := NewStep("web/vhost/shopfr", Create, "vhost")
	vhost.Apply = func(context.Context) error { note("vhost écrit"); return nil }
	vhost.Compensate = func(context.Context) error { note("vhost retiré"); return nil }
	vhost.Reload = []string{"web:apache"}
	plan.Add(vhost)

	// Le démarrage d'une instance vide la file : FPM et le serveur web ont
	// chargé le pool et le vhost.
	instance := NewStep("unit/instance/veille@1", Create, "instance")
	instance.FlushReloads = true
	instance.Apply = func(context.Context) error { note("démarrage"); return nil }
	instance.Compensate = func(context.Context) error { note("arrêt"); return nil }
	plan.Add(instance)

	// Écrit après le vidage, jamais chargé : son intention n'est pas rejouée.
	tardif := NewStep("web/vhost/autre", Create, "second vhost")
	tardif.Apply = func(context.Context) error { note("second vhost écrit"); return nil }
	tardif.Compensate = func(context.Context) error { note("second vhost retiré"); return nil }
	tardif.Reload = []string{"systemd:daemon-reload"}
	plan.Add(tardif)

	echec := NewStep("tls/certificate/shopfr", Create, "certificat")
	echec.Apply = func(context.Context) error { return errors.New("refusé") }
	plan.Add(echec)

	report, err := Execute(context.Background(), plan, Options{
		Reload: func(_ context.Context, target string) error { note("↻ " + target); return nil },
	})
	if err == nil {
		t.Fatal("l'échec critique doit remonter")
	}
	got := strings.Join(journal, ", ")
	attendu := "useradd, pool écrit, vhost écrit, ↻ php:8.4, ↻ web:apache, démarrage, second vhost écrit, " +
		"second vhost retiré, arrêt, vhost retiré, ↻ web:apache, pool retiré, ↻ php:8.4, userdel"
	if got != attendu {
		t.Errorf("séquence :\n  %s\nattendu :\n  %s", got, attendu)
	}
	if strings.Join(report.Replayed, ",") != "web:apache,php:8.4" {
		t.Errorf("rechargements rejoués : %v", report.Replayed)
	}
}

// Un déroulage arrêté en route rejoue malgré tout ce qu'il a défait.
func TestDéroulageInterrompuRejoueCeQuIlADéfait(t *testing.T) {
	var journal []string
	note := func(s string) { journal = append(journal, s) }
	plan := &Plan{Project: "shopfr"}

	premier := NewStep("php/pool/a", Create, "pool a")
	premier.Apply = func(context.Context) error { return nil }
	premier.Compensate = func(context.Context) error { note("a : compensation en échec"); return errors.New("disque") }
	premier.Reload = []string{"php:8.4"}
	plan.Add(premier)

	second := NewStep("php/pool/b", Create, "pool b")
	second.Apply = func(context.Context) error { return nil }
	second.Compensate = func(context.Context) error { note("b retiré"); return nil }
	second.Reload = []string{"php:8.4"}
	plan.Add(second)

	vidage := NewStep("unit/instance/x@1", Create, "instance")
	vidage.FlushReloads = true
	vidage.Apply = func(context.Context) error { return errors.New("refusé") }
	plan.Add(vidage)

	_, err := Execute(context.Background(), plan, Options{
		Reload: func(_ context.Context, target string) error { note("↻ " + target); return nil },
	})
	if err == nil {
		t.Fatal("l'échec critique doit remonter")
	}
	attendu := "↻ php:8.4, b retiré, a : compensation en échec, ↻ php:8.4"
	if got := strings.Join(journal, ", "); got != attendu {
		t.Errorf("séquence = %q, attendu %q", got, attendu)
	}
}
