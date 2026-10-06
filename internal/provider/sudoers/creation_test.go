package sudoers

import (
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// Le style systemctl recopié de config.yaml (D121) ne délègue rien à un projet
// qui n'a rien à piloter : pas de fichier, comme pour le style ogs.
func TestStyleSystemctlSansServiceNÉcritRien(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Sudo = &model.Sudo{Style: StyleSystemctl, Verbs: DefaultVerbs}
	content, err := prov.Build(proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Errorf("aucune délégation attendue sans service :\n%s", content)
	}
	proj.Spec.Services = []model.Service{{Name: "veille", Instances: 1}}
	if content, err := prov.Build(proj, nil); err != nil || content == nil {
		t.Errorf("avec un service, la délégation est écrite : %v", err)
	}
}

func TestCreationSettingsNÉcritQueCeQuiDiffèreDeLHistorique(t *testing.T) {
	if s := CreationSettings("ogs", []string{"start", "stop", "restart", "status"}); s != nil {
		t.Errorf("la délégation historique n'écrit rien : %+v", s)
	}
	if s := CreationSettings("", nil); s != nil {
		t.Errorf("une configuration vide n'écrit rien : %+v", s)
	}
	if s := CreationSettings("none", []string{"start"}); s == nil || s.Style != "none" || s.Verbs != nil {
		t.Errorf("style none sans verbes attendu : %+v", s)
	}
	if s := CreationSettings("ogs", []string{"restart"}); s == nil || len(s.Verbs) != 1 {
		t.Errorf("des verbes différents sont recopiés : %+v", s)
	}
}
