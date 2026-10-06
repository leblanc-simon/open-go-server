package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/exec"
)

// systemeReel est un exécuteur qui se dit réel — Reload ne court-circuite pas —
// et répond d'après un script : « is-active » vrai pour les premiers appels,
// faux ensuite, comme un maître Apache qui meurt après le retour de graceful.
type systemeReel struct {
	mu        sync.Mutex
	calls     []string
	vivant    int // nombre d'« is-active » encore vrais ; -1 : toujours
	relevable bool
	// etat est ce que rend « systemctl show -p ActiveState --value », vide par
	// défaut (substitut sans init).
	etat string
	// echecs fait échouer toute commande dont la ligne commence par la clé.
	echecs map[string]error
}

func (s *systemeReel) Simulated() bool { return false }
func (s *systemeReel) Run(_ context.Context, cmd exec.Command) (exec.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ligne := strings.TrimSpace(cmd.Name + " " + strings.Join(cmd.Args, " "))
	s.calls = append(s.calls, ligne)
	for prefixe, err := range s.echecs {
		if strings.HasPrefix(ligne, prefixe) {
			return exec.Result{ExitCode: 1}, err
		}
	}
	if len(cmd.Args) > 0 && cmd.Name == "systemctl" {
		switch cmd.Args[0] {
		case "is-active":
			if s.vivant == 0 {
				return exec.Result{ExitCode: 3}, errors.New("exit status 3")
			}
			if s.vivant > 0 {
				s.vivant--
			}
		case "start":
			if s.relevable {
				s.vivant = -1
			}
		case "show":
			if cmd.Args[len(cmd.Args)-1] == "--value" {
				return exec.Result{Stdout: s.etat + "\n"}, nil
			}
		}
	}
	return exec.Result{}, nil
}

func (s *systemeReel) a(prefixe string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if strings.HasPrefix(c, prefixe) {
			return true
		}
	}
	return false
}

// paireFixture pose un vhost activé qui désigne le certificat shopfr du magasin,
// et branche un contrôle de paire scripté.
func paireFixture(t *testing.T, moteur string, apparie, reparable bool) (*Provider, *bool) {
	t.Helper()
	p, paths := fixture(t, moteur)
	root := filepath.Join(paths.ACMEDir(), "certificates")
	p.CertDir = func(ref string) string { return filepath.Join(root, ref) }
	vhost := "ssl_certificate " + filepath.Join(root, "shopfr", "fullchain.pem") + ";\n" +
		"ssl_certificate_key " + filepath.Join(root, "shopfr", "privkey.pem") + ";\n"
	if err := os.WriteFile(filepath.Join(p.Engine.EnabledDir(), "voisin.conf"), []byte(vhost), 0o644); err != nil {
		t.Fatal(err)
	}
	repare := false
	p.CertificatePair = func(_ context.Context, dir string, repair bool) (PairCheck, error) {
		if dir != filepath.Join(root, "shopfr") {
			t.Errorf("répertoire inattendu : %s", dir)
		}
		if !apparie && reparable && repair {
			repare = true
			return PairCheck{Name: "shopfr", Paired: true, Repaired: true}, nil
		}
		return PairCheck{Name: "shopfr", Paired: apparie}, nil
	}
	return p, &repare
}

// B2 de la relecture de D110 : une clé recopiée à la main sur un site, puis
// l'apply d'un projet sans rapport. « apachectl -t » répondait « Syntax OK », le
// rechargement était annoncé, et le maître Apache mourait en chargeant la paire :
// tous les sites tombaient, code 0. Le rechargement est désormais refusé, en le
// nommant, sous les deux moteurs.
func TestUnRechargementEstRefuseSurUnePaireDepareillee(t *testing.T) {
	for _, moteur := range []string{"apache", "nginx"} {
		p, _ := paireFixture(t, moteur, false, false)
		sys := &systemeReel{vivant: -1}
		p.Exec = sys
		err := p.Reload(context.Background())
		var unpaired *UnpairedError
		if !errors.As(err, &unpaired) || !strings.Contains(err.Error(), "continue de servir sa configuration précédente") ||
			!strings.Contains(err.Error(), "ogs cert renew shopfr") || !strings.Contains(err.Error(), "voisin.conf") {
			t.Errorf("%s : le rechargement doit être refusé en nommant la paire et le vhost : %v", moteur, err)
		}
		if sys.a("systemctl reload") {
			t.Errorf("%s : aucun rechargement ne doit avoir été tenté", moteur)
		}
	}
}

// Une écriture interrompue s'achève avant le rechargement, qui a lieu.
func TestUneEcritureInterrompueSAcheveAvantLeRechargement(t *testing.T) {
	old := reloadWatch
	reloadWatch = 0
	defer func() { reloadWatch = old }()
	p, repare := paireFixture(t, "apache", false, true)
	sys := &systemeReel{vivant: -1}
	p.Exec = sys
	if err := p.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !*repare || !sys.a("systemctl reload") {
		t.Errorf("l'écriture doit être achevée puis le serveur rechargé (réparé : %v)", *repare)
	}
	// Doctor n'écrit rien : la paire est nommée.
	*repare = false
	if err := p.CheckCertificates(context.Background(), false); err == nil || *repare {
		t.Errorf("doctor doit nommer la paire sans l'achever : %v", err)
	}
}

// B2, second volet : « is-active » relu aussitôt après « graceful » voyait
// vivant un maître qui allait mourir. Le moteur est observé quelques instants,
// relevé s'il meurt, et le fait est signalé.
func TestUneMortAsynchroneApresRechargementEstVueEtRelevee(t *testing.T) {
	old, oldPoll := reloadWatch, reloadPoll
	reloadWatch, reloadPoll = 500*time.Millisecond, 20*time.Millisecond
	defer func() { reloadWatch, reloadPoll = old, oldPoll }()
	p, _ := fixture(t, "apache")
	// Vivant aux deux premières relectures, mort ensuite.
	sys := &systemeReel{vivant: 2, relevable: true}
	p.Exec = sys
	err := p.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "a arrêté") || !strings.Contains(err.Error(), "relevé") {
		t.Fatalf("la mort après le retour du rechargement doit être vue et signalée : %v", err)
	}
	if !sys.a("systemctl reset-failed") || !sys.a("systemctl start") {
		t.Error("le serveur mort doit être relevé, compteur d'échecs acquitté")
	}

	// Un serveur qui survit à la fenêtre n'est pas signalé.
	sys = &systemeReel{vivant: -1}
	p.Exec = sys
	if err := p.Reload(context.Background()); err != nil {
		t.Errorf("un serveur vivant ne doit rien signaler : %v", err)
	}
}
