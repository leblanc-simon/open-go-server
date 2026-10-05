package firewall

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
)

// L'unité transitoire du retour vit sous /run : un redémarrage dans le délai
// l'efface, et ufw repartait actif, sans filet — le témoin le rendait visible,
// sans rien défaire (R2-13). Un complément d'unité posé avec le témoin fait
// jouer le retour au démarrage : tant que le témoin existe, ufw ne s'active pas
// (D109).
func TestUnRedémarrageDansLeDélaiVautRetour(t *testing.T) {
	b := nouveauBanc(t, declarationType+"rollbackAfter: 30s\n", "ssh", "mariadb")
	b.write("/var/lib/ogs/.garde", "")
	b.prov.Exec = &ssReel{b.rec}
	b.prov.Admin = session("203.0.113.10", "")
	ctx := context.Background()
	complément := filepath.Join(b.root, "etc/systemd/system/ufw.service.d/ogs-rollback.conf")
	témoin := filepath.Join(b.root, "var/lib/ogs/ufw-unconfirmed")

	activer := func() *engine.Step {
		t.Helper()
		b.write("/etc/ufw/ufw.conf", "ENABLED=no\nLOGLEVEL=low\n")
		b.rec.Responses["systemctl is-active"] = exec.Result{Stdout: "inactive\n"}
		for _, s := range b.plan() {
			if s.Resource == "firewall/state" {
				if d, _ := s.Diff(); b.prov.Cfg.Rollback() > 0 && !strings.Contains(d, "ogs-rollback.conf") {
					t.Errorf("le plan doit annoncer le complément d'unité :\n%s", d)
				}
				if err := s.Apply(ctx); err != nil {
					t.Fatal(err)
				}
				return s
			}
		}
		t.Fatal("l'activation manque au plan")
		return nil
	}

	activer()
	data, err := os.ReadFile(complément)
	if err != nil {
		t.Fatalf("l'armement doit poser le complément d'unité : %v", err)
	}
	if !b.rec.Contains("systemctl daemon-reload") {
		t.Error("le complément posé, les unités doivent être relues")
	}
	if obs, _ := b.prov.Observe(ctx); !obs.BootGuard {
		t.Error("Observe doit voir le complément")
	}

	// Le complément, exécuté comme systemd l'exécuterait à chaque démarrage de
	// l'unité : au démarrage de la machine, /run est vide.
	marque := filepath.Join(b.root, "run/ogs-ufw-armed")
	if _, err := os.Stat(marque); err != nil {
		t.Fatalf("l'armement doit poser la marque de /run : %v", err)
	}
	lancer := func(machine bool) string {
		t.Helper()
		if machine {
			_ = os.Remove(marque)
		}
		if out, err := osexec.Command("/bin/sh", "-c", scriptDuComplément(t, string(data))).CombinedOutput(); err != nil {
			t.Fatalf("le complément ne doit jamais échouer : %v\n%s", err, out)
		}
		conf, _ := os.ReadFile(filepath.Join(b.root, "etc/ufw/ufw.conf"))
		return string(conf)
	}
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	// Relancer l'unité dans le délai ne désactive rien : seul le démarrage de la
	// machine vaut retour (B1 : « systemctl restart ufw » passait ufw inactif,
	// et confirm le déclarait confirmé).
	if conf := lancer(false); !strings.Contains(conf, "ENABLED=yes") {
		t.Errorf("une relance de l'unité dans le délai ne doit rien désactiver :\n%s", conf)
	}
	if conf := lancer(true); !strings.Contains(conf, "ENABLED=no") || !strings.Contains(conf, "LOGLEVEL=low") {
		t.Errorf("témoin présent : ufw doit rester inactif au démarrage :\n%s", conf)
	}
	if _, err := os.Stat(marque); err != nil {
		t.Errorf("le complément pose la marque après avoir joué : %v", err)
	}
	// Réactivé à la main après le démarrage, ufw n'est pas désactivé de nouveau
	// par une relance de l'unité.
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	if conf := lancer(false); !strings.Contains(conf, "ENABLED=yes") {
		t.Errorf("le complément ne joue qu'une fois par démarrage :\n%s", conf)
	}
	// Témoin disparu, complément resté : il ne désactive rien.
	if err := os.Remove(témoin); err != nil {
		t.Fatal(err)
	}
	if conf := lancer(true); !strings.Contains(conf, "ENABLED=yes") {
		t.Errorf("sans témoin, le complément ne doit rien désactiver :\n%s", conf)
	}

	// Un complément resté seul : doctor le nomme, confirm le retire.
	obs, _ := b.prov.Observe(ctx)
	if !constat(b.prov.Checks(obs, nil), "sans témoin") {
		t.Error("doctor doit nommer un complément resté sans témoin")
	}
	if confirmed, err := b.prov.Confirm(ctx, obs); confirmed || err != nil {
		t.Errorf("rien à confirmer : %v %v", confirmed, err)
	}
	if _, err := os.Stat(complément); !os.IsNotExist(err) {
		t.Errorf("confirm retire un complément resté seul : %v", err)
	}

	// La confirmation retire les deux.
	activer()
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	b.rec.Responses["systemctl is-active"] = exec.Result{Stdout: "active\n"}
	obs, _ = b.prov.Observe(ctx)
	if !constat(b.prov.Checks(obs, nil), "prochain démarrage") {
		t.Error("doctor doit dire qu'un redémarrage d'ici là désactive ufw")
	}
	if confirmed, err := b.prov.Confirm(ctx, obs); !confirmed || err != nil {
		t.Fatalf("confirm : %v %v", confirmed, err)
	}
	for _, f := range []string{témoin, complément} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("confirm retire %s : %v", f, err)
		}
	}

	// La compensation et la désactivation par ufw.yaml retirent les deux.
	state := activer()
	if err := state.Compensate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(complément); !os.IsNotExist(err) {
		t.Errorf("la compensation retire le complément : %v", err)
	}

	// Une activation sans retour (rollbackAfter: 0) retire ceux d'une
	// activation antérieure jamais confirmée : ils désactiveraient au démarrage
	// un pare-feu activé sans filet, délibérément.
	activer()
	b.prov.Cfg.RollbackAfter = "0"
	activer()
	if _, err := os.Stat(complément); !os.IsNotExist(err) {
		t.Errorf("rollbackAfter: 0 : le complément d'une activation antérieure doit partir : %v", err)
	}
	if _, err := os.Stat(témoin); !os.IsNotExist(err) {
		t.Errorf("rollbackAfter: 0 : le témoin d'une activation antérieure doit partir : %v", err)
	}
}

// Un fichier de ce nom qu'ogs n'a pas écrit n'est jamais retiré, ni remplacé :
// l'activation se fait, avec son minuteur, et le dit en aboutissement partiel ;
// confirm n'en est pas empêché, et doctor le nomme.
func TestComplémentÉtrangerLaisséEnPlace(t *testing.T) {
	b := nouveauBanc(t, declarationType+"rollbackAfter: 30s\n", "ssh", "mariadb")
	b.write("/var/lib/ogs/.garde", "")
	b.prov.Exec = &ssReel{b.rec}
	b.prov.Admin = session("203.0.113.10", "")
	ctx := context.Background()
	const étranger = "[Service]\n# à moi\n"
	chemin := filepath.Join(b.root, "etc/systemd/system/ufw.service.d/ogs-rollback.conf")
	b.write("/etc/systemd/system/ufw.service.d/ogs-rollback.conf", étranger)
	b.write("/etc/ufw/ufw.conf", "ENABLED=no\nLOGLEVEL=low\n")
	b.rec.Responses["systemctl is-active"] = exec.Result{Stdout: "inactive\n"}
	var err error
	for _, s := range b.plan() {
		if s.Resource == "firewall/state" {
			err = s.Apply(ctx)
		}
	}
	var partial *engine.PartialError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "n'a pas été écrit par ogs") {
		t.Errorf("activation : aboutissement partiel qui nomme le complément étranger attendu, obtenu %v", err)
	}
	if !b.rec.Contains("systemd-run") || b.prov.Armed == 0 {
		t.Error("le minuteur doit être armé malgré le complément étranger")
	}
	if got, _ := os.ReadFile(chemin); string(got) != étranger {
		t.Errorf("le complément étranger ne doit pas être remplacé :\n%s", got)
	}
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	b.rec.Responses["systemctl is-active"] = exec.Result{Stdout: "active\n"}
	obs, _ := b.prov.Observe(ctx)
	if obs.BootGuard || !obs.BootGuardForeign {
		t.Errorf("Observe : un complément étranger n'est pas celui d'ogs (%+v)", obs)
	}
	if !constat(b.prov.Checks(obs, nil), "n'a pas été écrit par ogs") {
		t.Error("doctor doit nommer le complément étranger")
	}
	if ok, err := b.prov.Confirm(ctx, obs); !ok || err != nil {
		t.Errorf("confirm ne doit pas être empêché par un fichier étranger : %v %v", ok, err)
	}
	if got, _ := os.ReadFile(chemin); string(got) != étranger {
		t.Errorf("le complément étranger doit rester :\n%s", got)
	}

	// Un complément d'ogs retouché à la main n'est remplacé que sous --force.
	if err := os.Remove(chemin); err != nil {
		t.Fatal(err)
	}
	b.prov.Force = false
	if refused, err := b.prov.writeBootGuard(ctx); refused != nil || err != nil {
		t.Fatalf("écriture : %v %v", refused, err)
	}
	retouché, _ := os.ReadFile(chemin)
	retouché = append(retouché, "# retouché\n"...)
	if err := os.WriteFile(chemin, retouché, 0o644); err != nil {
		t.Fatal(err)
	}
	if refused, err := b.prov.writeBootGuard(ctx); refused == nil || err != nil {
		t.Errorf("complément retouché sans --force : refus attendu, obtenu %v %v", refused, err)
	}
	b.prov.Force = true
	if refused, err := b.prov.writeBootGuard(ctx); refused != nil || err != nil {
		t.Errorf("complément retouché sous --force : %v %v", refused, err)
	}
	if got, _ := os.ReadFile(chemin); strings.Contains(string(got), "# retouché") {
		t.Error("--force remplace le complément retouché")
	}
}

// ufw désactivé dans le délai — à la main, ou par une relance de l'unité avant
// D109 v2 : confirm le déclarait confirmé, code 0, ufw inactif (B1). Il n'y a
// rien à confirmer ; status et doctor le disent.
func TestConfirmUFWInactifDansLeDélai(t *testing.T) {
	b := nouveauBanc(t, declarationType+"rollbackAfter: 30s\n", "ssh", "mariadb")
	b.prov.Exec = &ssReel{b.rec}
	ctx := context.Background()
	b.write("/var/lib/ogs/ufw-unconfirmed", "# témoin\n")
	b.write("/etc/ufw/ufw.conf", "ENABLED=no\nLOGLEVEL=low\n")
	b.rec.Responses["systemctl is-active"] = exec.Result{Stdout: "active\n"}
	obs, _ := b.prov.Observe(ctx)
	if !obs.DisabledWhilePending() {
		t.Fatalf("montage : retour en attente, ufw inactif (%+v)", obs)
	}
	if ok, err := b.prov.Confirm(ctx, obs); ok || err != nil {
		t.Errorf("rien à confirmer, ufw inactif : %v %v", ok, err)
	}
	if b.rec.Contains("systemctl stop") {
		t.Error("le minuteur ne doit pas être arrêté : rien n'est confirmé")
	}
	if _, err := os.Stat(filepath.Join(b.root, "var/lib/ogs/ufw-unconfirmed")); err != nil {
		t.Errorf("le témoin doit rester : %v", err)
	}
	if !constat(b.prov.Checks(obs, nil), "désactivé dans le délai") {
		t.Error("doctor doit dire qu'ufw a été désactivé dans le délai")
	}
}

// sed -i remplaçait un ufw.conf lié par un fichier ordinaire : le complément
// écrit dans la cible du lien.
func TestComplémentSuitUnUFWConfLié(t *testing.T) {
	b := nouveauBanc(t, declarationType+"rollbackAfter: 30s\n", "ssh")
	b.write("/etc/ufw/ufw.conf.reel", "ENABLED=yes\nLOGLEVEL=low\n")
	conf := filepath.Join(b.root, "etc/ufw/ufw.conf")
	_ = os.Remove(conf)
	if err := os.Symlink("ufw.conf.reel", conf); err != nil {
		t.Fatal(err)
	}
	b.write("/var/lib/ogs/ufw-unconfirmed", "# témoin\n")
	if err := os.MkdirAll(filepath.Join(b.root, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := scriptDuComplément(t, string(b.prov.bootGuardContent()))
	if out, err := osexec.Command("/bin/sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if fi, err := os.Lstat(conf); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("ufw.conf doit rester un lien : %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(b.root, "etc/ufw/ufw.conf.reel")); !strings.Contains(string(got), "ENABLED=no") {
		t.Errorf("la cible du lien doit être désactivée :\n%s", got)
	}
}

// scriptDuComplément extrait le script de la ligne ExecStartPre.
func scriptDuComplément(t *testing.T, unit string) string {
	t.Helper()
	for _, line := range strings.Split(unit, "\n") {
		if rest, ok := strings.CutPrefix(line, "ExecStartPre=-/bin/sh -c '"); ok && strings.HasSuffix(rest, "'") {
			script := strings.TrimSuffix(rest, "'")
			if strings.Contains(script, "'") || strings.ContainsAny(script, "$%\\") {
				t.Fatalf("le script ne doit porter ni apostrophe droite, ni $, %%, \\ — systemd les interprète :\n%s", script)
			}
			return script
		}
	}
	t.Fatalf("aucune ligne ExecStartPre :\n%s", unit)
	return ""
}
