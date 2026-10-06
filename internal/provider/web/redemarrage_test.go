package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

// complementFixture prépare un complément d'unité à écrire sur un moteur actif,
// avec un validateur de configuration présent sous le préfixe : validate ne
// l'appelle que s'il existe.
func complementFixture(t *testing.T, sys *systemeReel) (*Provider, []*engine.Step) {
	t.Helper()
	p, _ := fixture(t, "apache")
	bin, _ := p.Engine.ValidateArgs()
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p.Exec = sys
	proj := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	if err := proj.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	steps, err := p.PlanDropIn(context.Background(), []*model.Project{proj})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[1].Resource != "web/dropin/restart" {
		t.Fatalf("écriture puis redémarrage attendus : %+v", steps)
	}
	return p, steps
}

// R1-36 : le redémarrage qui suit le complément d'unité ne validait rien. Une
// configuration devenue invalide entre-temps — un fichier qu'un vhost inclut,
// retiré à la main — était chargée par un « restart » : le maître refusait de
// repartir et tous les sites tombaient, là où un rechargement aurait été refusé.
// Le redémarrage est refusé, et le complément rendu, pour que le plan suivant le
// rejoue.
func TestLeRedemarrageDuComplementValideLaConfiguration(t *testing.T) {
	sys := &systemeReel{vivant: -1, etat: "active", echecs: map[string]error{}}
	p, steps := complementFixture(t, sys)
	ctx := context.Background()
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	bin, _ := p.Engine.ValidateArgs()
	sys.echecs[bin+" -t"] = errors.New("AH00526: Syntax error")

	err := steps[1].Apply(ctx)
	if err == nil || !strings.Contains(err.Error(), "redémarrage refusé") {
		t.Fatalf("une configuration invalide doit faire refuser le redémarrage : %v", err)
	}
	if sys.a("systemctl restart") {
		t.Error("aucun redémarrage ne doit avoir été tenté sur une configuration invalide")
	}
	// Le complément est rendu : le plan suivant le réécrit et rejoue le
	// redémarrage, sans quoi l'espace de noms du service ne serait jamais refait.
	if _, err := os.Stat(p.DropInPath()); !os.IsNotExist(err) {
		t.Errorf("le complément doit être rendu à l'état d'avant (absent) : %v", err)
	}
	proj := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	_ = proj.Normalize("/home", "preprod-pp.net")
	again, err := p.PlanDropIn(ctx, []*model.Project{proj})
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Action == engine.NoOp || len(again) != 2 {
		t.Errorf("le plan suivant doit rejouer l'écriture et le redémarrage : %+v", again)
	}
}

// R1-36 : « apachectl -t » ne voit pas un journal inaccessible, que le maître
// ouvre au démarrage et sur lequel il s'arrête (AH00019). Rechargement et
// redémarrage portent le même contrôle.
func TestLeRedemarrageDuComplementEprouveLesJournaux(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root écrit partout : le refus ne peut pas être éprouvé")
	}
	sys := &systemeReel{vivant: -1, etat: "active"}
	p, steps := complementFixture(t, sys)
	logDir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(logDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(logDir, 0o755) })
	vhost := "# Généré par ogs — NE PAS MODIFIER À LA MAIN\n<VirtualHost *:80>\n    ErrorLog " +
		filepath.Join(logDir, "apache-error.log") + "\n</VirtualHost>\n"
	if err := os.WriteFile(filepath.Join(p.Engine.EnabledDir(), "shopfr.conf"), []byte(vhost), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	err := steps[1].Apply(ctx)
	if err == nil || !strings.Contains(err.Error(), "apache-error.log") {
		t.Fatalf("un journal inaccessible doit faire refuser le redémarrage en le nommant : %v", err)
	}
	if sys.a("systemctl restart") {
		t.Error("aucun redémarrage ne doit avoir été tenté")
	}
}

// R1-36 : un redémarrage qui rend 0 ne dit pas que le moteur vit. Il est observé
// comme un rechargement, relevé s'il meurt, et le fait est signalé.
func TestUnMoteurMortApresLeRedemarrageDuComplementEstReleveEtSignale(t *testing.T) {
	old, oldPoll := reloadWatch, reloadPoll
	reloadWatch, reloadPoll = 300*time.Millisecond, 20*time.Millisecond
	defer func() { reloadWatch, reloadPoll = old, oldPoll }()

	sys := &systemeReel{vivant: 0, etat: "active", relevable: true}
	_, steps := complementFixture(t, sys)
	ctx := context.Background()
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	err := steps[1].Apply(ctx)
	if err == nil || !strings.Contains(err.Error(), "relevé") {
		t.Fatalf("la mort du moteur après le redémarrage doit être vue et signalée : %v", err)
	}
	if !sys.a("systemctl start") {
		t.Error("le moteur mort doit être relevé")
	}

	// Un moteur qui survit n'est pas signalé.
	sys = &systemeReel{vivant: -1, etat: "active"}
	_, steps = complementFixture(t, sys)
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err := steps[1].Apply(ctx); err != nil {
		t.Errorf("un redémarrage réussi ne doit rien signaler : %v", err)
	}
	if !sys.a("systemctl restart apache2") {
		t.Error("le redémarrage doit avoir eu lieu")
	}
}
