package archive

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// R1-57 : AddFile lisait le fichier en entier — la sauvegarde d'une base,
// archivée à la suppression d'un projet, passait toute par la mémoire.
func TestAddFileRecopieEnFlux(t *testing.T) {
	const taille = 64 << 20
	dir := t.TempDir()
	dump := filepath.Join(dir, "shopfr.sql")
	f, err := os.Create(dump)
	if err != nil {
		t.Fatal(err)
	}
	// Un fichier creux : rien n'est écrit sur le disque, tout serait lu.
	if err := f.Truncate(taille); err != nil {
		t.Fatal(err)
	}
	f.Close()
	w, err := Create(filepath.Join(dir, "a.tar.zst"), Manifest{Project: "shopfr"})
	if err != nil {
		t.Fatal(err)
	}
	// Les tampons du compresseur s'allouent à la première écriture, quelle que
	// soit la taille : une petite entrée les met en place.
	if err := w.AddBytes("spec", "project.yaml", "", bytes.Repeat([]byte("x"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var avant, après runtime.MemStats
	runtime.ReadMemStats(&avant)
	err = w.AddFile("dump", "dumps/mysql/shopfr.sql", dump)
	runtime.ReadMemStats(&après)
	if err != nil {
		t.Fatal(err)
	}
	if alloué := après.TotalAlloc - avant.TotalAlloc; alloué > taille/4 {
		t.Errorf("%d Mo alloués pour un fichier de %d Mo : il passe par la mémoire", alloué>>20, taille>>20)
	}
	m, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	trouvée := false
	for _, e := range m.Entries {
		trouvée = trouvée || (e.Name == "dumps/mysql/shopfr.sql" && e.Bytes == taille)
	}
	if !trouvée || m.Bytes != taille+1<<20 {
		t.Errorf("manifeste : %+v", m)
	}
	var compte countWriter
	if _, err := CopyFile(w.Path(), "dumps/mysql/shopfr.sql", &compte); err != nil {
		t.Fatal(err)
	}
	if compte.n != taille {
		t.Errorf("%d octets relus, %d attendus", compte.n, taille)
	}
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

var _ io.Writer = (*countWriter)(nil)
