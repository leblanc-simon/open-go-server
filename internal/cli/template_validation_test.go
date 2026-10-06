package cli

import (
	"strings"
	"testing"
)

// R3-37 : « ogs template extract » promettait pour tout template que « le rendu
// est validé avant activation » — faux pour les unités, le cron, la rotation et
// Telegraf. La promesse dit désormais ce qui éprouve chaque rendu (D117).
func TestTemplateExtractNePrometQueLaValidationQuiExiste(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	cas := map[string]string{
		"telegraf/ogs.conf":       "aucun outil",
		"cron/project":            "aucun outil",
		"systemd/worker.service":  "systemd-analyze verify",
		"logrotate/ogs.conf":      "logrotate -d",
		"fail2ban/ogs.conf":       "fail2ban-client -t",
		"systemd/ogs-renew.timer": "aucun outil",
	}
	for nom, attendu := range cas {
		out := h.mustRun("template", "extract", nom)
		if !strings.Contains(out, attendu) {
			t.Errorf("%s : %q attendu dans :\n%s", nom, attendu, out)
		}
		if strings.Contains(out, "le rendu est validé avant activation") {
			t.Errorf("%s : promesse générale encore faite :\n%s", nom, out)
		}
	}
}
