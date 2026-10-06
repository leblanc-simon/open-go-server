package db

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/exec"
)

// fluxSize est la taille des sauvegardes de ces essais : assez pour qu'une copie
// en mémoire se voie nettement.
const fluxSize = 64 << 20

// fluxExec simule les clients d'un moteur sur une base de fluxSize octets :
// mysqldump écrit dans le flux qu'on lui donne — comme exec.Local —, ou rend sa
// sortie dans Result, que Local tient alors en mémoire ; mysql lit son entrée.
type fluxExec struct {
	*exec.Recorder
	lus int64
}

func (f *fluxExec) Run(ctx context.Context, cmd exec.Command) (exec.Result, error) {
	switch filepath.Base(cmd.Name) {
	case "mysqldump":
		if cmd.Stdout == nil {
			return exec.Result{Stdout: strings.Repeat("x", fluxSize)}, nil
		}
		n, err := io.CopyN(cmd.Stdout, motif{}, fluxSize)
		f.lus = n
		return exec.Result{}, err
	case "mysql":
		n, err := io.Copy(io.Discard, entréeDe(cmd))
		f.lus = n
		return exec.Result{}, err
	}
	return f.Recorder.Run(ctx, cmd)
}

// motif est une source inépuisable de lignes.
type motif struct{}

func (motif) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = "INSERT;\n"[i%8]
	}
	return len(p), nil
}

// allouéPendant mesure ce que f alloue.
func allouéPendant(t *testing.T, f func()) uint64 {
	t.Helper()
	runtime.GC()
	var avant, après runtime.MemStats
	runtime.ReadMemStats(&avant)
	f()
	runtime.ReadMemStats(&après)
	return après.TotalAlloc - avant.TotalAlloc
}

// R1-57 : la sortie de mysqldump était tenue en mémoire par exec, recopiée en
// chaîne, puis compressée d'un bloc — trois fois la base.
func TestLaSauvegardeVaAuFilDeLEau(t *testing.T) {
	prov, proj, decl := dumpFixture(t)
	fe := &fluxExec{Recorder: prov.Exec.(*exec.Recorder)}
	prov.Exec = fe
	for _, nom := range []string{"shopfr.sql", "shopfr.sql.gz"} {
		sortie := filepath.Join(t.TempDir(), nom)
		var err error
		alloué := allouéPendant(t, func() { _, err = prov.Dump(context.Background(), proj, decl, sortie) })
		if err != nil {
			t.Fatal(err)
		}
		if alloué > fluxSize/4 {
			t.Errorf("%s : %d Mo alloués pour une base de %d Mo — la sauvegarde passe par la mémoire",
				nom, alloué>>20, fluxSize>>20)
		}
		if info, err := os.Stat(sortie); err != nil || info.Size() == 0 {
			t.Errorf("%s : sauvegarde absente (%v)", nom, err)
		}
	}
}

// R1-57 : la restauration lisait la sauvegarde entière (io.ReadAll), la
// recopiait pour la confiner, puis la passait au client par un tampon.
func TestLaRestaurationVaAuFilDeLEau(t *testing.T) {
	prov, proj, decl := dumpFixture(t)
	mysql := filepath.Join(prov.Paths.Prefix(), "usr", "bin", "mysql")
	if err := os.WriteFile(mysql, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	fe := &fluxExec{Recorder: prov.Exec.(*exec.Recorder)}
	prov.Exec = fe
	entrée := filepath.Join(t.TempDir(), "shopfr.sql")
	f, err := os.Create(entrée)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(f, motif{}, fluxSize); err != nil {
		t.Fatal(err)
	}
	f.Close()
	alloué := allouéPendant(t, func() { err = prov.Restore(context.Background(), proj, decl, entrée) })
	if err != nil {
		t.Fatal(err)
	}
	if fe.lus != fluxSize {
		t.Errorf("le client a lu %d octets, %d attendus", fe.lus, fluxSize)
	}
	if alloué > fluxSize/4 {
		t.Errorf("%d Mo alloués pour une sauvegarde de %d Mo — elle passe par la mémoire", alloué>>20, fluxSize>>20)
	}
}

// Une sauvegarde qui échoue en chemin laisse la précédente en place : écrite au
// fil de l'eau, elle ne l'est plus à sa place, mais sous un nom temporaire.
func TestUneSauvegardeEnEchecGardeLaPrecedente(t *testing.T) {
	prov, proj, decl := dumpFixture(t)
	prov.Exec = échecExec{prov.Exec.(*exec.Recorder)}
	dir := t.TempDir()
	sortie := filepath.Join(dir, "shopfr.sql")
	if err := os.WriteFile(sortie, []byte("hier"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Dump(context.Background(), proj, decl, sortie); err == nil {
		t.Fatal("l'échec du client doit remonter")
	}
	if data, err := os.ReadFile(sortie); err != nil || string(data) != "hier" {
		t.Errorf("la sauvegarde précédente est perdue : %q, %v", data, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("un temporaire est resté : %v", entries)
	}
}

// échecExec fait échouer mysqldump après une sortie partielle.
type échecExec struct{ *exec.Recorder }

func (e échecExec) Run(ctx context.Context, cmd exec.Command) (exec.Result, error) {
	if filepath.Base(cmd.Name) == "mysqldump" {
		if cmd.Stdout != nil {
			_, _ = io.WriteString(cmd.Stdout, "-- début\n")
		}
		return exec.Result{ExitCode: 2}, io.ErrUnexpectedEOF
	}
	return e.Recorder.Run(ctx, cmd)
}

// Une ligne plus longue que le tampon garde son en-tête confiné : la clause est
// en tête, loin de la coupure ; la suite passe telle quelle.
func TestLeConfinementMySQLDUneLongueLigne(t *testing.T) {
	queue := strings.Repeat("x", 3*lineHead)
	ligne := "CREATE DEFINER=`src`@`%` PROCEDURE p() SELECT '" + queue + "';\n"
	got := string(confineMySQLDump([]byte(ligne + "-- fin\n")))
	want := "CREATE  PROCEDURE p() SELECT '" + queue + "';\n-- fin\n"
	if got != want {
		t.Errorf("longue ligne mal confinée (%d octets, %d attendus)", len(got), len(want))
	}
}
