package monitoring

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/verify"
)

// fail2banJuge refuse la configuration tant que le fichier d'ogs porte marque.
type fail2banJuge struct {
	path, marque string
}

func (f *fail2banJuge) Simulated() bool { return false }
func (f *fail2banJuge) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	if c.Name == "fail2ban-client" && len(c.Args) > 0 && c.Args[0] == "-t" {
		if data, _ := os.ReadFile(f.path); strings.Contains(string(data), f.marque) {
			return exec.Result{ExitCode: 255, Stderr: "ERROR  Failed during configuration: prison refusée\n"},
				errors.New("code 255")
		}
	}
	return exec.Result{}, nil
}

// R3-37 : fail2ban n'était éprouvé qu'au rechargement, le fichier écrit et
// laissé en place. Refusé, il restait, passait pour conforme au plan suivant, et
// fail2ban ne redémarrait plus au démarrage suivant de la machine — prisons de
// l'administrateur comprises. Il est désormais éprouvé à l'écriture, et rendu.
func TestDesPrisonsRefuseesSontRendues(t *testing.T) {
	p, paths := fixture(t)
	shop := project(t, "shop.fr", model.KindSymfony, "8.3")
	withLogs(t, shop, "nginx")
	obs := Observed{Fail2banPresent: true, TelegrafGroup: -1}
	appliquer(t, p, []*model.Project{shop}, obs)
	path := paths.Fail2banJailFile()
	ancien, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Un second projet ajoute ses prisons ; le juge refuse le fichier qui les porte.
	blog := project(t, "blog.fr", model.KindSymfony, "8.3")
	withLogs(t, blog, "nginx")
	p.Exec = &fail2banJuge{path: path, marque: blog.Spec.Layout.LogDir}
	steps, err := p.Plan(context.Background(), []*model.Project{shop, blog}, obs)
	if err != nil {
		t.Fatal(err)
	}
	s := etape(t, steps, "monitoring/fail2ban")
	err = s.Apply(context.Background())
	if !errors.As(err, new(*engine.ToleratedError)) {
		t.Errorf("des prisons refusées ne doivent pas arrêter le plan global : %v", err)
	}
	if !verify.IsRefused(err) {
		t.Fatalf("les prisons doivent être refusées : %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != string(ancien) {
		t.Errorf("le fichier refusé doit être rendu :\n%s", data)
	}
}
