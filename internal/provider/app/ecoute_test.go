package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/store"
)

func TestLectureDesEcoutesDuNoyau(t *testing.T) {
	tcp := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:5209 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 1 1 0 100 0 0 10 0\n" +
		"   1: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0 100 0 0 10 0\n" +
		// Une connexion établie n'est pas une écoute.
		"   2: 0100007F:5209 0100007F:D2F0 01 00000000:00000000 00:00000000 00000000  1001        0 3 1 0 100 0 0 10 0\n"
	tcp6 := "  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000000000000000000000000000:520A 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1002        0 4 1 0 100 0 0 10 0\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tcp"), []byte(tcp), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tcp6"), []byte(tcp6), 0o644); err != nil {
		t.Fatal(err)
	}
	ls, err := ReadListeners(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 3 {
		t.Fatalf("%d écoutes, 3 attendues : %+v", len(ls), ls)
	}
	if !ls[0].IP.Equal(net.IPv4(127, 0, 0, 1)) || ls[0].Port != 21001 || ls[0].UID != 1001 {
		t.Errorf("première écoute mal lue : %+v", ls[0])
	}
	if !ls[1].IP.IsUnspecified() || ls[1].Port != 80 || ls[1].UID != 0 {
		t.Errorf("deuxième écoute mal lue : %+v", ls[1])
	}
	if !ls[2].IP.IsUnspecified() || len(ls[2].IP) != net.IPv6len || ls[2].Port != 21002 {
		t.Errorf("écoute IPv6 mal lue : %+v", ls[2])
	}
}

// Sur la machine réelle, la lecture doit retrouver une écoute ouverte par le
// test lui-même, sous son propre uid.
func TestLectureDesEcoutesRetrouveLaSienne(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	ls, err := ReadListeners(ProcNet)
	if err != nil {
		t.Skip(err)
	}
	for _, x := range ls {
		if x.Port == port && x.IP.Equal(net.IPv4(127, 0, 0, 1)) && x.UID == os.Getuid() {
			return
		}
	}
	t.Fatalf("écoute 127.0.0.1:%d (uid %d) introuvable parmi %d", port, os.Getuid(), len(ls))
}

func goProject(t *testing.T, root, host string) *model.Project {
	t.Helper()
	proj := model.New(host, naming.EnvProd, model.KindGo)
	if err := proj.Normalize(filepath.Join(root, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	return proj
}

// Une base d'état perdue ou recréée redistribuait les ports dans l'ordre
// alphabétique : aa.fr, calculé le premier, prenait 21001 — le port que l'unité
// de bb.fr écoute et vers lequel son vhost relaie —, et chacun des deux sites
// était servi par l'application de l'autre (R2-18).
func TestUneBaseDEtatPerdueRendLesPortsEnService(t *testing.T) {
	prov, _ := fixture(t, model.KindGo)
	root := filepath.Dir(prov.Paths.StateDB())
	a, b := goProject(t, root, "aa.fr"), goProject(t, root, "bb.fr")
	// bb.fr a été créé le premier : 21001, puis aa.fr : 21002.
	for _, proj := range []*model.Project{b, a} {
		e, err := prov.Endpoint(proj)
		if err != nil {
			t.Fatal(err)
		}
		unit, err := prov.Build(proj, e)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(prov.unitPath(proj)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(prov.unitPath(proj), unit, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// La base est perdue : une neuve, vide, et un nouveau provider.
	fresh, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	again := &Provider{Paths: prov.Paths, Exec: prov.Exec, Templates: prov.Templates, Store: fresh, Now: prov.Now}
	for _, c := range []struct {
		proj *model.Project
		want string
	}{{a, "127.0.0.1:21002"}, {b, "127.0.0.1:21001"}} {
		e, err := again.Endpoint(c.proj)
		if err != nil {
			t.Fatal(err)
		}
		if e.Address != c.want {
			t.Errorf("%s : %s après la perte de la base, attendu %s (le port de son unité)",
				c.proj.ID(), e.Address, c.want)
		}
	}
	// Un projet nouveau ne reçoit aucun des ports en service.
	e, err := again.Endpoint(goProject(t, root, "cc.fr"))
	if err != nil || e.Address != "127.0.0.1:21003" {
		t.Errorf("nouveau projet : %s, attendu 127.0.0.1:21003 : %v", e.Address, err)
	}
}

// Le port suivant d'une plage est prévisible : un compte qui l'écoute d'avance
// recevait tout le trafic du prochain projet. L'attribution l'écarte, et la
// sonde de disponibilité ne tient plus pour prête une écoute d'un autre compte
// (S2R-02).
func TestUnPortTenuParUnAutreCompteNEstNiAttribueNiTenuPourPret(t *testing.T) {
	prov, proj := fixture(t, model.KindGo)
	étranger := []Listener{{IP: net.IPv4(127, 0, 0, 1), Port: 21001, UID: 1001}}
	prov.Listeners = func() ([]Listener, error) { return étranger, nil }
	prov.Owner = func(*model.Project) (int, int, error) { return 2000, 2000, nil }

	e, err := prov.Endpoint(proj)
	if err != nil {
		t.Fatal(err)
	}
	if e.Port != 21002 {
		t.Fatalf("port attribué %d : 21001, déjà en écoute, doit être écarté", e.Port)
	}

	// Le port est pris après l'attribution, par un autre compte, sur toutes les
	// interfaces : il reçoit aussi ce qui vise 127.0.0.1.
	étranger = []Listener{{IP: net.IPv4zero, Port: 21002, UID: 1001}}
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Foreign == "" {
		t.Fatal("l'écoute étrangère n'est pas relevée")
	}
	step, err := prov.PlanHealth(context.Background(), proj, obs, false)
	if err != nil {
		t.Fatal(err)
	}
	if step.Action == engine.NoOp || !step.Critical {
		t.Fatalf("une écoute étrangère doit arrêter l'apply avant le vhost : %+v", step)
	}
	if err := step.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "1001") {
		t.Errorf("le refus doit nommer le compte qui tient le port : %v", err)
	}

	// La sonde elle-même, sur une vraie écoute : prête pour le compte du projet,
	// refusée pour un autre.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	e = Endpoint{Address: l.Addr().String(), Port: port}
	uid := 1001
	reel := &Provider{Paths: prov.Paths, Exec: &realExecutor{exec.NewRecorder()}, Owner: prov.Owner,
		Listeners: func() ([]Listener, error) {
			return []Listener{{IP: net.IPv4(127, 0, 0, 1), Port: port, UID: uid}}, nil
		}}
	proj.Spec.App.Health = &model.Health{Timeout: "1s"}
	if err := reel.waitReady(context.Background(), proj, e); err == nil {
		t.Error("waitReady tient pour prête l'écoute d'un autre compte")
	}
	uid = 2000
	if err := reel.waitReady(context.Background(), proj, e); err != nil {
		t.Errorf("l'écoute du projet doit être tenue pour prête : %v", err)
	}
}

// Le compte du projet qui écoute sur son port n'est pas un étranger.
func TestLEcouteDuProjetNEstPasEtrangere(t *testing.T) {
	prov, proj := fixture(t, model.KindGo)
	prov.Owner = func(*model.Project) (int, int, error) { return 2000, 2000, nil }
	prov.Listeners = func() ([]Listener, error) {
		return []Listener{
			{IP: net.IPv4(127, 0, 0, 1), Port: 21001, UID: 2000},
			// Une adresse qui ne reçoit pas le trafic de la boucle locale.
			{IP: net.IPv4(10, 0, 0, 5), Port: 21001, UID: 1001},
		}, nil
	}
	e := Endpoint{Address: "127.0.0.1:21001", Port: 21001}
	foreign, err := prov.foreignListener(proj, e)
	if err != nil || foreign != "" {
		t.Fatalf("l'écoute du projet est tenue pour étrangère : %q %v", foreign, err)
	}
	// Root non plus : docker-proxy publie sous root le port d'une application
	// lancée par « docker run -p ».
	prov.Listeners = func() ([]Listener, error) {
		return []Listener{{IP: net.IPv4zero, Port: 21001, UID: 0}}, nil
	}
	if foreign, err := prov.foreignListener(proj, e); err != nil || foreign != "" {
		t.Fatalf("une écoute de root est tenue pour étrangère : %q %v", foreign, err)
	}
}
