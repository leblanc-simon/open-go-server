package validate

import (
	"strings"
	"testing"

	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
)

func newProject(t *testing.T, host string, env naming.Env, kind model.AppKind) *model.Project {
	t.Helper()
	p := model.New(host, env, kind)
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectAcceptsNormalizedStatic(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("projet normalisé refusé : %v", err)
	}
}

func TestNormalizeProvidesDefaultPool(t *testing.T) {
	// La normalisation déclare le pool principal : un type PHP n'a qu'une réponse
	// sensée sur ce point.
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindPrestashop)
	if len(p.Spec.PHP.Pools) != 1 || p.Spec.PHP.Pools[0].Name != model.MainPoolName {
		t.Fatalf("pool par défaut absent : %+v", p.Spec.PHP.Pools)
	}
	if p.Spec.PHP.Pools[0].Version != model.DefaultPHPVersion {
		t.Errorf("version par défaut %q", p.Spec.PHP.Pools[0].Version)
	}
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("projet normalisé refusé : %v", err)
	}
}

func TestNormalizeDowngradesPresetForPrep(t *testing.T) {
	prod := newProject(t, "shop.fr", naming.EnvProd, model.KindSymfony)
	prep := newProject(t, "shop.fr", naming.EnvPrep, model.KindSymfony)
	if prod.Spec.PHP.Pools[0].Preset != "medium" || prep.Spec.PHP.Pools[0].Preset != "small" {
		t.Errorf("presets : prod=%q prep=%q", prod.Spec.PHP.Pools[0].Preset, prep.Spec.PHP.Pools[0].Preset)
	}
}

func TestProjectRequiresPoolForPHPKinds(t *testing.T) {
	// La règle reste un garde-fou pour un document construit sans normalisation.
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindPrestashop)
	p.Spec.PHP.Pools = nil
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "requiert un pool PHP") {
		t.Fatalf("un type PHP sans pool doit être refusé, obtenu : %v", err)
	}
}

func TestProjectRejectsUnsupportedPHP(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindPrestashop)
	p.Spec.PHP.Pools = []model.Pool{{Name: "web", Version: "7.2"}}
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "non gérée") {
		t.Fatalf("PHP 7.2 devrait être refusé, obtenu : %v", err)
	}
}

func TestProjectAcceptsEOLPHP(t *testing.T) {
	// PHP 7.4 est en fin de support mais reste utilisable : un Prestashop ancien
	// peut l'exiger (D39).
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindPrestashop)
	p.Spec.PHP.Pools = []model.Pool{{Name: "web", Version: "7.4"}}
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("PHP 7.4 doit être accepté : %v", err)
	}
	if _, eol := EOLDate("7.4"); !eol {
		t.Error("PHP 7.4 devrait être signalé en fin de support")
	}
	if _, eol := EOLDate("8.4"); eol {
		t.Error("PHP 8.4 ne devrait pas être signalé en fin de support")
	}
}

func TestProjectRejectsHomeOutsideRoot(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Layout.Root = "/srv/shop.fr"
	p.Spec.User.Home = "/srv/shop.fr"
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "doit être sous /home") {
		t.Fatalf("un home hors racine doit être refusé, obtenu : %v", err)
	}
}

func TestProjectRejectsBadDatabaseIdentifier(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Databases = []model.Database{{Engine: "mysql", Name: "shop`; DROP", User: "shopfr"}}
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "nom de base") {
		t.Fatalf("un nom de base non conforme doit être refusé, obtenu : %v", err)
	}
}

func TestProjectRejectsAMQPVhostWithSlash(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.AMQP = &model.AMQP{Vhost: "/shopfr", User: "shopfr"}
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "%2F") {
		t.Fatalf("un vhost préfixé de / doit être refusé, obtenu : %v", err)
	}
}

func TestProjectRejectsDanglingBinding(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.AMQP = &model.AMQP{
		Vhost: "shopfr", User: "shopfr",
		Queues:   []model.Queue{{Name: "async"}},
		Bindings: []model.Binding{{Exchange: "messages", Queue: "async"}},
	}
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "non déclaré") {
		t.Fatalf("un binding vers un exchange absent doit être refusé, obtenu : %v", err)
	}
}

func TestCronExpression(t *testing.T) {
	valid := []string{"* * * * *", "*/5 * * * *", "0 3 * * 1", "15,45 2-4 1 1 *", "0 0 1 1 0"}
	for _, e := range valid {
		if err := CronExpression(e); err != nil {
			t.Errorf("expression valide refusée %q : %v", e, err)
		}
	}
	invalid := []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "a * * * *", "*/0 * * * *"}
	for _, e := range invalid {
		if err := CronExpression(e); err == nil {
			t.Errorf("expression invalide acceptée : %q", e)
		}
	}
}

func TestUniquenessDetectsHomeCollision(t *testing.T) {
	// Deux identifiants distincts peuvent réclamer le même home : c'est le cas
	// documenté au §3.3.6, à refuser explicitement.
	a := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	b := newProject(t, "autre.fr", naming.EnvProd, model.KindStatic)
	b.Spec.Layout.Root = a.Spec.Layout.Root
	err := Uniqueness([]*model.Project{a, b}, "preprod-pp.net")
	if err == nil || !strings.Contains(err.Error(), "home") {
		t.Fatalf("collision de home non détectée : %v", err)
	}
}

func TestUniquenessDetectsIdentifierCollision(t *testing.T) {
	// mon-shop.fr et monshop.fr convergent vers le même identifiant.
	a := newProject(t, "mon-shop.fr", naming.EnvProd, model.KindStatic)
	b := newProject(t, "monshop.fr", naming.EnvProd, model.KindStatic)
	err := Uniqueness([]*model.Project{a, b}, "preprod-pp.net")
	if err == nil || !strings.Contains(err.Error(), "identifiant") {
		t.Fatalf("collision d'identifiant non détectée : %v", err)
	}
}

func TestUniquenessAllowsProdAndPrep(t *testing.T) {
	prod := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	prep := newProject(t, "shop.fr", naming.EnvPrep, model.KindStatic)
	if err := Uniqueness([]*model.Project{prod, prep}, "preprod-pp.net"); err != nil {
		t.Fatalf("production et préproduction doivent cohabiter : %v", err)
	}
	if prod.Spec.Layout.Root == prep.Spec.Layout.Root {
		t.Error("les deux environnements doivent avoir des homes distincts")
	}
}

func TestUniquenessDetectsDomainCollision(t *testing.T) {
	a := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	b := newProject(t, "autre.fr", naming.EnvProd, model.KindStatic)
	b.Spec.Domains = append(b.Spec.Domains, model.Domain{Name: "www.shop.fr", Role: "alias"})
	if err := Uniqueness([]*model.Project{a, b}, "preprod-pp.net"); err == nil {
		t.Fatal("un domaine déjà servi doit être refusé")
	}
}
