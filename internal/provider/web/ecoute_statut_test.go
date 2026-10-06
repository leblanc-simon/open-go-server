package web

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
)

// appliquerStatut applique les étapes du vhost de supervision et rend les
// erreurs des étapes non critiques, comme le moteur les relèverait.
func appliquerStatut(t *testing.T, steps []*engine.Step) []error {
	t.Helper()
	var errs []error
	for _, s := range steps {
		if s.Apply == nil {
			continue
		}
		if err := s.Apply(context.Background()); err != nil {
			if s.Critical {
				t.Fatalf("%s : étape critique en échec : %v", s.Resource, err)
			}
			errs = append(errs, err)
		}
	}
	return errs
}

// R2-37 : le vhost de supervision ajoutait « Listen 127.0.0.1:8099 » sans
// regarder qui tenait déjà ce port. « apachectl -t » ne voit pas un port pris :
// le maître Apache mourait au rechargement suivant en emportant tous les sites,
// et nginx gardait sa configuration précédente en silence. Le vhost est refusé,
// d'une étape non critique qui nomme le port : le plan global continue, et
// l'apply des projets n'en dépend pas.
func TestLeVhostDeSupervisionRefuseUnPortDejaPris(t *testing.T) {
	for _, moteur := range []string{"apache", "nginx"} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skip("aucune écoute locale possible : ", err)
		}
		listen := l.Addr().String()
		prov, _ := fixture(t, moteur)
		prov.Exec = &systemeReel{vivant: -1}
		shop := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
		steps, err := prov.PlanStatus(context.Background(), []*model.Project{shop}, listen, "/ogs-status", true)
		if err != nil {
			t.Fatal(err)
		}
		errs := appliquerStatut(t, steps)
		path := filepath.Join(prov.Engine.AvailableDir(), prov.Engine.FileName(StatusVHostName))
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s : le vhost ne doit pas être écrit sur un port déjà pris : %v", moteur, err)
		}
		_, port, _ := net.SplitHostPort(listen)
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), port) {
			t.Errorf("%s : un refus non critique nommant le port %s est attendu : %v", moteur, port, errs)
		}
		if _, err := os.Lstat(filepath.Join(prov.Engine.EnabledDir(), prov.Engine.FileName(StatusVHostName))); err == nil {
			t.Errorf("%s : aucun lien d'activation ne doit être posé", moteur)
		}
		l.Close()
	}
}

// Le port que tient le moteur pour le vhost de supervision déjà actif n'est pas
// « pris » : un pool ajouté réécrit le vhost sans changer d'écoute.
func TestLeVhostDeSupervisionGardeLePortQuIlTientDeja(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("aucune écoute locale possible : ", err)
	}
	defer l.Close()
	listen := l.Addr().String()
	prov, _ := fixture(t, "apache")
	shop := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	// Première écriture en mode simulé : le vhost est posé et activé.
	steps, err := prov.PlanStatus(context.Background(), nil, listen, "/ogs-status", true)
	if err != nil {
		t.Fatal(err)
	}
	if errs := appliquerStatut(t, steps); len(errs) != 0 {
		t.Fatal(errs)
	}
	// Le moteur tient désormais le port ; un pool s'ajoute.
	prov.Exec = &systemeReel{vivant: -1}
	steps, err = prov.PlanStatus(context.Background(), []*model.Project{shop}, listen, "/ogs-status", true)
	if err != nil {
		t.Fatal(err)
	}
	if errs := appliquerStatut(t, steps); len(errs) != 0 {
		t.Errorf("le port du vhost déjà actif ne doit pas être refusé : %v", errs)
	}
	path := filepath.Join(prov.Engine.AvailableDir(), prov.Engine.FileName(StatusVHostName))
	if c, _ := os.ReadFile(path); !strings.Contains(string(c), "/ogs-status/fpm/shopfr") {
		t.Errorf("le vhost doit être réécrit avec le pool :\n%s", c)
	}
}
