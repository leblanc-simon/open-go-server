package db

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
)

var (
	reRevoqueConnect = regexp.MustCompile(`^REVOKE CONNECT ON DATABASE "([^"]+)" FROM PUBLIC`)
	reAccordeConnect = regexp.MustCompile(`^GRANT CONNECT ON DATABASE "([^"]+)" TO "([^"]+)"`)
)

// stubAcces est un cluster PostgreSQL qui tient aussi l'ACL des bases : qui la
// possède, si PUBLIC peut s'y connecter — ce que PostgreSQL accorde à toute
// base nouvelle —, et à quels rôles CONNECT a été accordé.
type stubAcces struct {
	*stubVivant
	public  map[string]bool
	connect map[string]bool
}

func (s *stubAcces) Exec(ctx context.Context, stmt string) error {
	if err := s.stubVivant.Exec(ctx, stmt); err != nil {
		return err
	}
	if m := reRevoqueConnect.FindStringSubmatch(stmt); m != nil {
		s.public[m[1]] = false
	}
	if m := reAccordeConnect.FindStringSubmatch(stmt); m != nil {
		s.connect[m[1]+"/"+m[2]] = true
	}
	return nil
}

func (s *stubAcces) DatabaseAccess(_ context.Context, database, role string) (string, bool, bool, error) {
	owner := ""
	for r, g := range s.grantsPar {
		if g[database] == "OWNER" {
			owner = r
		}
	}
	mine := s.users[role] && (s.public[database] || owner == role || s.connect[database+"/"+role])
	return owner, s.public[database], mine, nil
}

func clusterAcces(bases, rôles []string, propriétaires map[string]string, public map[string]bool) *stubAcces {
	grants := map[string]map[string]string{}
	for base, rôle := range propriétaires {
		if grants[rôle] == nil {
			grants[rôle] = map[string]string{}
		}
		grants[rôle][base] = "OWNER"
	}
	s := &stubAcces{stubVivant: vivant(bases, rôles, grants), public: public, connect: map[string]bool{}}
	s.postgres = true
	return s
}

func TestUneBaseDOgsRouverteAPublicEstCorrigee(t *testing.T) {
	// R1-45. D45 révoque CONNECT à PUBLIC, mais le jugement ne lisait que la
	// propriété de la base : un « GRANT CONNECT … TO PUBLIC » posé après coup —
	// ou une base créée avant D45 — restait un NoOp, et tout rôle du cluster
	// pouvait s'y connecter sans que rien ne le dise.
	prov, proj := fixture(t)
	proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "postgres")}
	sceller(t, prov, proj, "postgres", "shopfr")
	stub := clusterAcces([]string{"shopfr"}, []string{"shopfr"},
		map[string]string{"shopfr": "shopfr"}, map[string]bool{"shopfr": true})
	prov.clients = map[string]Client{"postgres": stub}

	steps := planifier(t, prov, proj)
	if len(steps) != 1 || steps[0].Action != engine.Update {
		t.Fatalf("la révocation doit être annoncée comme une correction : %v", steps)
	}
	if !strings.Contains(steps[0].Summary, "CONNECT") {
		t.Errorf("le plan doit dire pourquoi : %s", steps[0].Summary)
	}
	if !strings.Contains(décrire(t, steps[0]), `REVOKE CONNECT ON DATABASE "shopfr" FROM PUBLIC`) {
		t.Errorf("la révocation doit figurer au détail :\n%s", décrire(t, steps[0]))
	}
	if errs := appliquer(t, steps); len(errs) > 0 {
		t.Fatal(errs)
	}
	if stub.public["shopfr"] {
		t.Fatal("CONNECT doit être révoqué à PUBLIC")
	}
	for _, s := range planifier(t, prov, proj) {
		if s.Mutates() {
			t.Errorf("plan suivant : %s", s)
		}
	}
}

func TestUneBaseDOgsFermeeAPublicResteConforme(t *testing.T) {
	// Le parc en place, durci par D45 : lire l'ACL ne doit y faire apparaître
	// aucun écart.
	prov, proj := fixture(t)
	proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "postgres")}
	sceller(t, prov, proj, "postgres", "shopfr")
	prov.clients = map[string]Client{"postgres": clusterAcces([]string{"shopfr"}, []string{"shopfr"},
		map[string]string{"shopfr": "shopfr"}, map[string]bool{})}
	for _, s := range planifier(t, prov, proj) {
		if s.Mutates() {
			t.Errorf("aucun écart attendu : %s", s)
		}
	}
}

func TestUneBaseAdopteeNEstJamaisRefermee(t *testing.T) {
	// Arbitrage D111 : CONNECT n'est révoqué à PUBLIC que sur les bases qu'ogs a
	// créées. Une base adoptée qui l'accorde est signalée, jamais modifiée — la
	// refermer coupait tout autre rôle qui s'y connectait.
	for _, cas := range []struct {
		nom          string
		propriétaire string
	}{
		{"au rôle déclaré", "shop"},
		// R1-46 : le rôle déclaré d'après le nom n'est pas le propriétaire. Le
		// jugement exigeait la propriété : l'étape repassait à chaque apply,
		// révoquant CONNECT et donnant le schéma public à un rôle qui ne
		// possédait pas la base, sans jamais devenir conforme.
		{"à un autre rôle", "shop_owner"},
	} {
		t.Run(cas.nom, func(t *testing.T) {
			prov, proj := fixture(t)
			decl := DefaultDatabase(proj, "postgres")
			decl.Name, decl.User = "shop", "shop"
			proj.Spec.Databases = []model.Database{decl}
			adopter(proj)
			stub := clusterAcces([]string{"shop"}, []string{"shop", "shop_owner"},
				map[string]string{"shop": cas.propriétaire}, map[string]bool{"shop": true})
			prov.clients = map[string]Client{"postgres": stub}

			for _, s := range planifier(t, prov, proj) {
				if s.Mutates() {
					t.Errorf("une base adoptée ne doit faire apparaître aucun écart : %s", s)
				}
			}
		})
	}
}

func TestUneCorrectionSurUneBaseAdopteeNeLaRefermePas(t *testing.T) {
	// Une extension déclarée manque : l'étape tourne, mais ne touche ni à
	// CONNECT ni au schéma public d'une base qu'ogs n'a pas créée.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "postgres")
	decl.Name, decl.User, decl.Extensions = "shop", "shop", []string{"pg_trgm"}
	proj.Spec.Databases = []model.Database{decl}
	adopter(proj)
	stub := clusterAcces([]string{"shop"}, []string{"shop", "shop_owner"},
		map[string]string{"shop": "shop_owner"}, map[string]bool{"shop": true})
	prov.clients = map[string]Client{"postgres": stub}

	steps := planifier(t, prov, proj)
	if len(steps) != 1 || !steps[0].Mutates() {
		t.Fatalf("l'extension manquante doit être annoncée : %v", steps)
	}
	if !strings.Contains(steps[0].Summary, "CONNECT") {
		t.Errorf("le plan doit signaler CONNECT laissé à PUBLIC : %s", steps[0].Summary)
	}
	if errs := appliquer(t, steps); len(errs) > 0 {
		t.Fatal(errs)
	}
	if !exécutée(stub.stubVivant, `[shop] CREATE EXTENSION IF NOT EXISTS "pg_trgm"`) {
		t.Errorf("l'extension doit être créée : %v", stub.dansBases)
	}
	for _, q := range append(append([]string(nil), stub.exécutées...), stub.dansBases...) {
		if strings.Contains(q, "PUBLIC") || strings.Contains(q, "SCHEMA public OWNER") ||
			strings.Contains(q, "OWNER TO") {
			t.Errorf("rien de la base adoptée ne doit être refermé ni repris : %s", q)
		}
	}
}

func TestUneBaseAdopteeFermeeDonneCONNECTAuRoleDeclare(t *testing.T) {
	// L'administrateur a déjà révoqué CONNECT à PUBLIC, et le rôle déclaré n'en
	// est pas propriétaire : sans GRANT, il ne peut pas se connecter. La base
	// était jugée conforme, alors que D111 promet CONNECT au rôle déclaré.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "postgres")
	decl.Name, decl.User = "shop", "shop"
	proj.Spec.Databases = []model.Database{decl}
	adopter(proj)
	stub := clusterAcces([]string{"shop"}, []string{"shop", "admin"},
		map[string]string{"shop": "admin"}, map[string]bool{"shop": false})
	prov.clients = map[string]Client{"postgres": stub}

	steps := planifier(t, prov, proj)
	if len(steps) != 1 || !steps[0].Mutates() || !strings.Contains(steps[0].Summary, "CONNECT accordé au rôle shop") {
		t.Fatalf("CONNECT doit être accordé au rôle déclaré : %v", steps)
	}
	if errs := appliquer(t, steps); len(errs) > 0 {
		t.Fatal(errs)
	}
	for _, q := range append(append([]string(nil), stub.exécutées...), stub.dansBases...) {
		if strings.Contains(q, "PUBLIC") || strings.Contains(q, "OWNER") {
			t.Errorf("rien d'autre ne doit être touché sur une base adoptée : %s", q)
		}
	}
	for _, s := range planifier(t, prov, proj) {
		if s.Mutates() {
			t.Errorf("plan suivant : %s", s)
		}
	}
}
