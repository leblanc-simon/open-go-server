package logrotate

import (
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// postrotate rend les lignes du script de fin de rotation.
func postrotate(t *testing.T, body string) []string {
	t.Helper()
	_, after, ok := strings.Cut(body, "postrotate")
	if !ok {
		t.Fatalf("aucun postrotate :\n%s", body)
	}
	script, _, _ := strings.Cut(after, "endscript")
	var out []string
	for _, l := range strings.Split(script, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// R3N-01 : chaque nuit, le postrotate rechargeait FPM — un USR2, ré-exécution
// complète du maître — sans « php-fpm -t » et en jetant le résultat : les
// requêtes en cours étaient coupées et, sur une configuration devenue invalide,
// FPM mourait pour toute la version. PHP rouvre ses journaux de lui-même.
func TestRotationNeRechargePasFPM(t *testing.T) {
	prov, projects := fixture(t)
	projects[0].Spec.PHP.Pools = []model.Pool{{Version: "8.4"}}
	body, err := prov.Build(projects, "apache", "apache2.service")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "fpm") {
		t.Errorf("la rotation ne doit adresser aucun signal à FPM :\n%s", body)
	}
	script := postrotate(t, string(body))
	if len(script) != 1 || !strings.HasPrefix(script[0], "systemctl reload apache2.service ") {
		t.Fatalf("seul le serveur web est rechargé : %q", script)
	}
	// Un échec ne bloque pas logrotate, mais il n'est plus jeté.
	if !strings.Contains(script[0], "|| logger -t ogs-logrotate") || !strings.HasSuffix(script[0], "|| true") {
		t.Errorf("l'échec doit être consigné, sans faire échouer logrotate : %q", script[0])
	}
}

// R3N-13 : un nom choisi par « log: » n'était couvert par aucune rotation, et
// les journaux des tâches n'avaient aucune borne de taille (D71).
func TestRotationDesJournauxDeTâches(t *testing.T) {
	prov, projects := fixture(t)
	projects[0].Spec.Crons = []model.CronJob{
		{Name: "scheduler", Schedule: "* * * * *", Command: "bin/console sched", Log: "sched.log"},
		{Name: "purge", Schedule: "@daily", Command: "bin/console purge"},
		// Un nom que le bloc des serveurs tient déjà n'est pas répété : logrotate
		// refuse une seconde entrée pour un même fichier.
		{Name: "bavarde", Schedule: "@hourly", Command: "true", Log: "apache-error.log"},
	}
	body, err := prov.Build(projects, "apache", "apache2.service")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	logDir := projects[0].Spec.Layout.LogDir
	_, tâches, ok := strings.Cut(text, "endscript")
	if !ok {
		t.Fatalf("bloc des serveurs attendu :\n%s", text)
	}
	for _, want := range []string{logDir + "/sched.log", logDir + "/cron-purge.log", "maxsize 200M"} {
		if !strings.Contains(tâches, want) {
			t.Errorf("le bloc des tâches doit porter %q :\n%s", want, tâches)
		}
	}
	if strings.Contains(tâches, "apache-error.log") {
		t.Errorf("un journal déjà roté par le bloc des serveurs ne doit pas l'être deux fois :\n%s", tâches)
	}
	if n := strings.Count(text, logDir+"/apache-error.log"); n != 1 {
		t.Errorf("%d entrées pour apache-error.log, 1 attendue", n)
	}
	if strings.Contains(text, "cron-*.log") {
		t.Errorf("les journaux sont énumérés, plus désignés par un motif :\n%s", text)
	}
}

// La borne suit logs.maxSize, comme celle des serveurs.
func TestBorneDesTâchesSuitLaConfiguration(t *testing.T) {
	prov, projects := fixture(t)
	prov.Config.Logs.MaxSize = "50M"
	projects[0].Spec.Crons = []model.CronJob{{Name: "purge", Schedule: "@daily", Command: "true"}}
	body, err := prov.Build(projects, "apache", "apache2.service")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), "maxsize 50M"); n != 2 {
		t.Errorf("les deux blocs portent la borne réglée, %d trouvé(s) :\n%s", n, body)
	}
}
