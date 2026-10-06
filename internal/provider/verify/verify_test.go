package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/exec"
)

// outil répond à chaque appel par la réponse suivante de sa liste.
type outil struct {
	réponses []exec.Result
	appels   []exec.Command
}

func (o *outil) Simulated() bool { return false }
func (o *outil) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	o.appels = append(o.appels, c)
	if len(o.réponses) == 0 {
		return exec.Result{}, nil
	}
	r := o.réponses[0]
	o.réponses = o.réponses[1:]
	if r.ExitCode != 0 {
		return r, errors.New("code de sortie non nul")
	}
	return r, nil
}

const unite = "/etc/systemd/system/ogs-shopfr-app@.service"

// Seules les lignes localisées dans le fichier d'ogs font refuser : celles des
// unités tierces et le binaire pas encore déployé, non.
func TestSystemdNeRetientQueLesLignesDuFichierDOgs(t *testing.T) {
	tiers := exec.Result{ExitCode: 1, Stderr: "/lib/systemd/system/apache2.service:12: Unknown key 'X'\n" +
		"ogs-shopfr-app@test_instance.service: Command /home/shopfr/www/bin/console is not executable: No such file or directory\n"}
	if err := SystemdUnit(context.Background(), &outil{réponses: []exec.Result{tiers}}, unite); err != nil {
		t.Fatalf("une plainte hors du fichier d'ogs ne doit pas le faire refuser : %v", err)
	}
	propre := exec.Result{ExitCode: 1, Stderr: unite + ":9: Unknown key 'StartLimitIntervalSec' in section [Service], ignoring.\n"}
	o := &outil{réponses: []exec.Result{propre}}
	err := SystemdUnit(context.Background(), o, unite)
	if !IsRefused(err) || !strings.Contains(err.Error(), "StartLimitIntervalSec") {
		t.Fatalf("une clé ignorée de l'unité d'ogs doit la faire refuser : %v", err)
	}
	if got := o.appels[0].String(); !strings.Contains(got, "systemd-analyze verify") || !strings.Contains(got, unite) {
		t.Errorf("commande = %s", got)
	}
}

func TestLogrotateNeRetientQueLesErreursDuFichier(t *testing.T) {
	path := "/etc/logrotate.d/ogs"
	absent := exec.Result{ExitCode: 1, Stderr: "error: stat of /var/log/ogs-projects/x/a.log failed: No such file or directory\n"}
	if err := Logrotate(context.Background(), &outil{réponses: []exec.Result{absent}}, path); err != nil {
		t.Fatalf("un journal absent ne doit pas faire refuser le fichier : %v", err)
	}
	inconnue := exec.Result{Stderr: "warning: logrotate in debug mode does nothing\nwarning: " + path + ":6 unknown option 'dayly' -- ignoring line\n"}
	if err := Logrotate(context.Background(), &outil{réponses: []exec.Result{inconnue}}, path); !IsRefused(err) {
		t.Fatalf("une option ignorée du fichier d'ogs doit le faire refuser, même en code 0 : %v", err)
	}
	doublon := exec.Result{ExitCode: 1, Stderr: "error: " + path + ":12 duplicate log entry for /var/log/x\n"}
	if err := Logrotate(context.Background(), &outil{réponses: []exec.Result{doublon}}, path); !IsRefused(err) {
		t.Fatalf("une entrée en double doit faire refuser le fichier : %v", err)
	}
}

// fail2ban-client -t éprouve toute la machine : le refus n'est imputé au
// fichier d'ogs que s'il disparaît une fois l'ancien fichier rendu.
func TestFail2banImputeLeRefusParDifference(t *testing.T) {
	ko := exec.Result{ExitCode: 1, Stderr: "ERROR  Failed during configuration: Have not found any log file for ogs-x jail\n"}
	var rendu, remis int
	restore := func() error { rendu++; return nil }
	rewrite := func() error { remis++; return nil }

	err := Fail2ban(context.Background(), &outil{réponses: []exec.Result{ko, {}}}, "/etc/fail2ban/jail.d/ogs.conf", restore, rewrite)
	if !IsRefused(err) || rendu != 1 || remis != 0 {
		t.Fatalf("refus imputable : err=%v rendu=%d remis=%d", err, rendu, remis)
	}
	rendu, remis = 0, 0
	err = Fail2ban(context.Background(), &outil{réponses: []exec.Result{ko, ko}}, "/etc/fail2ban/jail.d/ogs.conf", restore, rewrite)
	if err != nil || rendu != 1 || remis != 1 {
		t.Fatalf("défaut antérieur au fichier : err=%v rendu=%d remis=%d", err, rendu, remis)
	}
}

func TestRestoreRendLeFichierPrecedentOuLeRetire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	refus := &Refused{Tool: "outil", Path: path, Detail: "non"}
	if err := os.WriteFile(path, []byte("neuf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Restore(refus, path, []byte("ancien"), 0o644); !IsRefused(err) {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "ancien" {
		t.Errorf("contenu = %q", data)
	}
	if err := Restore(refus, path, nil, 0o644); !IsRefused(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("sans fichier précédent, le nouveau doit être retiré : %v", err)
	}
	if err := Restore(nil, path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOutilAbsentOuSimulationNeValidentRien(t *testing.T) {
	if err := SystemdUnit(context.Background(), exec.NewRecorder(), unite); err != nil {
		t.Fatal(err)
	}
}

// systemd 249 (Ubuntu 22.04) ne connaît pas --recursive-errors : la
// vérification est relancée sans l'option, au lieu de ne rien éprouver.
func TestSystemd249RelanceSansLOption(t *testing.T) {
	inconnue := exec.Result{ExitCode: 1, Stderr: "systemd-analyze: unrecognized option '--recursive-errors=no'\n"}
	propre := exec.Result{ExitCode: 1, Stderr: unite + ":4: Unknown key 'Restartt' in section [Service], ignoring.\n"}
	o := &outil{réponses: []exec.Result{inconnue, propre}}
	if err := SystemdUnit(context.Background(), o, unite); !IsRefused(err) {
		t.Fatalf("refus attendu après la relance : %v", err)
	}
	if len(o.appels) != 2 || strings.Contains(o.appels[1].String(), "recursive-errors") {
		t.Errorf("appels = %v", o.appels)
	}
}

func TestRestoreRendLeModeDOrigine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("ancien"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	previous, mode := ReadPrevious(path)
	if err := os.WriteFile(path, []byte("neuf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = Restore(&Refused{Tool: "t", Path: path, Detail: "d"}, path, previous, mode)
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode rendu %04o, attendu 0600", info.Mode().Perm())
	}
}

// Réserve de la relecture v2 : un fichier refusé qui n'a pas pu être rendu reste
// en place ; l'erreur ne doit plus passer pour un refus, sans quoi l'étape est
// tolérée alors que le fichier refusé est servi.
func TestUnRenduImpossibleNEstPasUnRefus(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root écrit dans un répertoire en lecture seule")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("neuf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	err := Restore(&Refused{Tool: "t", Path: path, Detail: "d"}, path, []byte("ancien"), 0o644)
	if err == nil || IsRefused(err) {
		t.Fatalf("un rendu impossible ne doit pas rester un refus : %v", err)
	}
	if Tolerate(err, []byte("ancien")) != err {
		t.Error("un rendu impossible ne doit pas être toléré")
	}
}
