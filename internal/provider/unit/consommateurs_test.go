package unit

import (
	"context"
	"slices"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// projetAConsommateurs déclare un service Messenger (template implicite), un
// consumer du bundle et un worker libre, puis applique : les liens .wants
// existent.
func projetAConsommateurs(t *testing.T) (*Provider, *model.Project) {
	t.Helper()
	prov, proj := fixture(t)
	proj.Spec.Services = []model.Service{
		{Name: "async"},
		{Name: "commandes", Template: TemplateConsumer},
		{Name: "async-veille", Template: TemplateWorker, Params: map[string]string{"command": "/bin/true"}},
	}
	apply(t, prov, proj)
	return prov, proj
}

func TestLesConsommateursDuBrokerSontMessengerEtRabbitmqConsumer(t *testing.T) {
	_, proj := projetAConsommateurs(t)
	if got := BrokerServices(proj); !slices.Equal(got, []string{"async", "commandes"}) {
		t.Errorf("services consommateurs : %v", got)
	}
}

// « amqp stop » n'arrête que les instances actives des consommateurs : ni la
// cible, ni le worker libre — dont le nom commence pourtant comme « async » —,
// ni une instance déjà inactive. L'arrêt est acquitté (D121).
func TestStopServicesNArrêteQueLesConsommateursActifs(t *testing.T) {
	prov, proj := projetAConsommateurs(t)
	sd := &systemdFactice{unités: "ogs-shopfr.target loaded active active Projet shopfr\n" +
		"ogs-shopfr-async@1.service loaded active running async\n" +
		"ogs-shopfr-async@2.service loaded inactive dead async\n" +
		"● ogs-shopfr-commandes@1.service loaded activating auto-restart commandes\n" +
		"ogs-shopfr-async-veille@1.service loaded active running veille\n"}
	prov.Exec = sd
	arrêtées, err := prov.StopServices(context.Background(), proj.ID(), BrokerServices(proj))
	if err != nil {
		t.Fatal(err)
	}
	attendu := []string{"ogs-shopfr-async@1.service", "ogs-shopfr-commandes@1.service"}
	if !slices.Equal(arrêtées, attendu) {
		t.Errorf("unités arrêtées : %v, attendu %v", arrêtées, attendu)
	}
	if got := sd.appel("stop"); !slices.Equal(got, attendu) {
		t.Errorf("systemctl stop %v", got)
	}
	if got := sd.appel("reset-failed"); !slices.Equal(got, attendu) {
		t.Errorf("l'arrêt doit être acquitté : systemctl reset-failed %v", got)
	}
}

// « amqp start » démarre les instances activées des consommateurs, puis la
// cible sans ses dépendances : démarrée ordinairement, elle relancerait le
// worker libre que l'exploitant a peut-être arrêté.
func TestStartServicesDémarreLesConsommateursSansTirerLaCible(t *testing.T) {
	prov, proj := projetAConsommateurs(t)
	sd := &systemdFactice{}
	prov.Exec = sd
	démarrées, err := prov.StartServices(context.Background(), proj.ID(), BrokerServices(proj))
	if err != nil {
		t.Fatal(err)
	}
	attendu := []string{"ogs-shopfr-async@1.service", "ogs-shopfr-commandes@1.service"}
	if !slices.Equal(démarrées, attendu) {
		t.Errorf("unités démarrées : %v, attendu %v", démarrées, attendu)
	}
	var starts [][]string
	for _, a := range sd.appels {
		if len(a) > 1 && a[1] == "start" {
			starts = append(starts, a[2:])
		}
	}
	if len(starts) != 2 || !slices.Equal(starts[0], attendu) ||
		!slices.Equal(starts[1], []string{"--job-mode=ignore-requirements", TargetName(proj.ID())}) {
		t.Errorf("démarrages : %v", starts)
	}
}
