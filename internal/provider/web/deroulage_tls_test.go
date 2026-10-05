package web

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	tlsprov "leblanc.io/open-go-server/internal/provider/tls"
)

// TestLeDeroulageNeLaissePasUnVhostSurUnCertificatRetire reproduit R1-35 : un
// site neuf reçoit son certificat, la bascule TLS réécrit son vhost avec le bloc
// 443, puis une étape critique échoue plus loin. La compensation de l'émission
// retirait le certificat, la bascule n'en avait aucune : le vhost désignait un
// certificat absent, et le rechargement suivant faisait tomber le serveur — le
// parc entier. Après le déroulage, le vhost en place ne doit jamais désigner un
// matériel qui n'est plus sur le disque (D110).
func TestLeDeroulageNeLaissePasUnVhostSurUnCertificatRetire(t *testing.T) {
	for _, moteur := range []string{"apache", "nginx"} {
		t.Run(moteur, func(t *testing.T) {
			prov, paths := fixture(t, moteur)
			now := func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) }
			store := tlsprov.Store{Paths: paths}
			certs := &tlsprov.Provider{
				Paths: paths, Cfg: config.Default(), Store: store, Now: now,
				Issuer:   tlsprov.SelfSigned{Validity: 90 * 24 * time.Hour, Now: now},
				Fallback: tlsprov.SelfSigned{Validity: 90 * 24 * time.Hour, Now: now},
			}
			prov.Certificate = certs.VHostCoverage
			prov.CertDir = store.Dir
			certs.ServedBy = prov.CertificateReferences
			p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
			ctx := context.Background()

			// Le vhost du site neuf, en HTTP seul : aucun certificat présent.
			bp := buildMain(t, prov, p)
			if _, port443 := blocs(t, moteur, string(bp.Content)); port443 != "" {
				t.Fatalf("sans certificat, le vhost ne doit pas servir le port 443 :\n%s", bp.Content)
			}
			if err := prov.writeVHost(ctx, bp); err != nil {
				t.Fatal(err)
			}

			plan := &engine.Plan{Project: p.ID()}
			steps, err := certs.Plan(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range steps {
				plan.Add(s)
			}
			for _, s := range prov.PlanTLS(ctx, p) {
				plan.Add(s)
			}
			échec := engine.NewStep("sudoers/shopfr", engine.Create, "délégation")
			échec.Apply = func(context.Context) error { return errors.New("visudo refuse la règle") }
			plan.Add(échec)

			report, err := engine.Execute(ctx, plan, engine.Options{})
			if err == nil || !report.RolledBack {
				t.Fatalf("le déroulage était attendu : %v", err)
			}
			data, err := os.ReadFile(bp.Path)
			if err != nil {
				t.Fatal(err)
			}
			_, présent, _ := store.Read("shopfr")
			if strings.Contains(string(data), store.Dir("shopfr")) && !présent {
				t.Fatalf("après le déroulage, le vhost désigne un certificat retiré :\n%s", data)
			}
		})
	}
}
