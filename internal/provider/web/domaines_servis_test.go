package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// ecrireConf pose un fichier de configuration sous le préfixe du moteur.
func ecrireConf(t *testing.T, chemin, corps string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(chemin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chemin, []byte(corps), 0o644); err != nil {
		t.Fatal(err)
	}
	return chemin
}

// confDir rend le répertoire de configurations générales que le moteur charge :
// conf-enabled pour Apache, conf.d pour nginx (Debian).
func confDir(prov *Provider) string {
	return filepath.Join(filepath.Dir(prov.Engine.AvailableDir()), map[string]string{
		"apache": "conf-enabled", "nginx": "conf.d"}[prov.Engine.Name()])
}

// R2-44 : la détection des domaines déjà servis ne lisait que sites-available.
// Un site servi depuis conf.d (nginx), conf-enabled (Apache) ou un fichier
// ordinaire de sites-enabled n'était pas vu : le vhost d'ogs était écrit, activé,
// et jamais atteint — le moteur retient la première déclaration d'un nom, et ces
// répertoires sont chargés avant sites-enabled ou à côté de lui.
func TestObserveVoitLesDomainesServisHorsDeSitesAvailable(t *testing.T) {
	for _, cas := range []struct {
		moteur, ou, corps string
	}{
		{"nginx", "conf.d/ancien.conf", "server {\n  listen 80;\n  server_name shop.fr;\n}\n"},
		{"nginx", "sites-enabled/ancien", "server {\n  server_name www.shop.fr;\n}\n"},
		{"apache", "conf-enabled/ancien.conf", "<VirtualHost *:80>\n  ServerName shop.fr\n</VirtualHost>\n"},
		{"apache", "sites-enabled/000-ancien.conf", "<VirtualHost *:80>\n  ServerAlias shop.fr\n</VirtualHost>\n"},
	} {
		prov, _ := fixture(t, cas.moteur)
		p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
		chemin := ecrireConf(t, filepath.Join(filepath.Dir(prov.Engine.AvailableDir()), cas.ou), cas.corps)
		obs, err := prov.Observe(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, path := range obs.Conflicts {
			found = found || path == chemin
		}
		if !found {
			t.Errorf("%s, %s : le domaine servi doit être vu : %+v", cas.moteur, cas.ou, obs.Conflicts)
			continue
		}
		if _, err := prov.Plan(context.Background(), p, obs); err == nil || !strings.Contains(err.Error(), chemin) {
			t.Errorf("%s, %s : le plan doit s'arrêter en nommant le fichier : %v", cas.moteur, cas.ou, err)
		}
		if len(obs.Stale) != 0 {
			t.Errorf("%s : rien hors de sites-available n'est jamais retiré : %v", cas.moteur, obs.Stale)
		}
	}
}

// R2-44, second volet : « ServerName shop.fr:80 » est une forme admise par
// Apache, et le nom gardait son port : le conflit n'était jamais vu.
func TestObserveVoitUnServerNameSuiviDUnPort(t *testing.T) {
	prov, _ := fixture(t, "apache")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	chemin := ecrireConf(t, filepath.Join(prov.Engine.AvailableDir(), "ancien.conf"),
		"<VirtualHost *:80>\n  ServerName shop.fr:80\n  ServerAlias www.shop.fr:80\n</VirtualHost>\n")
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Conflicts["shop.fr"] != chemin || obs.Conflicts["www.shop.fr"] != chemin {
		t.Errorf("le nom suivi d'un port doit être vu : %+v", obs.Conflicts)
	}
}

// Le risque de R2-44 est le faux conflit, qui refuserait un site qui marchait :
// les liens d'activation (le fichier visé est déjà jugé dans sites-available,
// celui du projet compris), ce que le moteur ne charge pas (Apache ne prend que
// *.conf, aucun moteur un fichier caché), et un fichier de sites-enabled écrit
// par ogs pour ce projet.
func TestObserveNeVoitPasDeConflitDansCeQuiNeLeSertPas(t *testing.T) {
	blocs := map[string]string{
		"apache": "<VirtualHost *:80>\n  ServerName shop.fr\n</VirtualHost>\n",
		"nginx":  "server {\n  server_name shop.fr;\n}\n",
	}
	for _, moteur := range []string{"apache", "nginx"} {
		prov, _ := fixture(t, moteur)
		p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
		vh := p.VHosts()[0]
		bloc := blocs[moteur]
		// Le vhost du projet, écrit et activé par ogs.
		poserVHost(t, prov, p.VHostFile(vh), vhostEcritPour(p.ID(), bloc), true)
		// Un lien d'activation sous un autre nom, vers le même fichier.
		if err := os.Symlink(filepath.Join(prov.Engine.AvailableDir(), p.VHostFile(vh)),
			filepath.Join(prov.Engine.EnabledDir(), "010-shop.conf")); err != nil {
			t.Fatal(err)
		}
		base := filepath.Dir(prov.Engine.AvailableDir())
		ecrireConf(t, filepath.Join(prov.Engine.EnabledDir(), ".cache"), bloc)
		if moteur == "apache" {
			// Apache n'inclut que sites-enabled/*.conf et conf-enabled/*.conf.
			ecrireConf(t, filepath.Join(prov.Engine.EnabledDir(), "ancien.disabled"), bloc)
			ecrireConf(t, filepath.Join(base, "conf-enabled", "ancien.txt"), bloc)
			// conf-available n'est pas chargé.
			ecrireConf(t, filepath.Join(base, "conf-available", "ancien.conf"), bloc)
		} else {
			ecrireConf(t, filepath.Join(base, "conf.d", "ancien.conf.dpkg-old"), bloc)
		}
		// Un fichier ordinaire de sites-enabled qu'ogs a écrit pour ce projet.
		ecrireConf(t, filepath.Join(prov.Engine.EnabledDir(), "copie.conf"), string(vhostEcritPour(p.ID(), bloc)))
		obs, err := prov.Observe(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if len(obs.Conflicts) != 0 {
			t.Errorf("%s : aucun conflit attendu : %+v", moteur, obs.Conflicts)
		}
		if len(obs.Stale) != 0 {
			t.Errorf("%s : rien hors de sites-available n'est retiré : %v", moteur, obs.Stale)
		}
	}
}

// Relecture de D118 (B2) : un « ServerName shop.fr » global dans conf-enabled —
// le remède courant à AH00558 — ne sert aucun site, et faisait refuser en dur le
// plan d'un site en place. Seuls comptent les noms d'un <VirtualHost> ou d'un
// bloc server, et sur les ports web quand le port se lit : un vhost étranger sur
// 8080, le backend d'un Varnish, ne détourne rien.
func TestSeulUnBlocQuiServeLesPortsWebEstUnConflit(t *testing.T) {
	for _, cas := range []struct {
		moteur, ou, corps string
		conflit           bool
	}{
		{"apache", "conf-enabled/fqdn.conf", "ServerName shop.fr\n", false},
		{"apache", "conf-enabled/varnish.conf", "<VirtualHost 127.0.0.1:8080>\n  ServerName shop.fr\n</VirtualHost>\n", false},
		{"apache", "conf-enabled/tout.conf", "<VirtualHost *>\n  ServerName shop.fr\n</VirtualHost>\n", true},
		{"apache", "conf-enabled/tls.conf", "<VirtualHost _default_:443 127.0.0.1:8443>\n  ServerAlias shop.fr\n</VirtualHost>\n", true},
		{"nginx", "conf.d/varnish.conf", "server {\n  listen 127.0.0.1:8080;\n  server_name shop.fr;\n}\n", false},
		{"nginx", "conf.d/socket.conf", "server {\n  listen unix:/run/x.sock;\n  server_name shop.fr;\n}\n", false},
		{"nginx", "conf.d/amont.conf", "upstream shop { server 127.0.0.1:80; }\nserver {\n  listen 8080;\n  location / { proxy_pass http://shop; }\n  server_name shop.fr;\n}\n", false},
		{"nginx", "conf.d/tls.conf", "server {\n  listen [::]:443 ssl;\n  server_name shop.fr;\n}\n", true},
		{"nginx", "conf.d/defaut.conf", "server {\n  server_name shop.fr;\n}\n", true},
	} {
		prov, _ := fixture(t, cas.moteur)
		p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
		ecrireConf(t, filepath.Join(filepath.Dir(prov.Engine.AvailableDir()), cas.ou), cas.corps)
		obs, err := prov.Observe(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(obs.Conflicts) > 0; got != cas.conflit {
			t.Errorf("%s, %s : conflit %v attendu, obtenu %+v", cas.moteur, cas.ou, cas.conflit, obs.Conflicts)
		}
	}
}

// Relecture de D118 (B2) : un conflit qui vise un vhost qu'ogs a déjà écrit et
// activé pour ce projet ne bloque plus — le site marche déjà, refuser le plan
// en dur privait le projet de tout le reste. C'est un avertissement non
// bloquant ; le refus en dur reste pour la création d'un vhost neuf. Le remède
// est celui du fichier : « ogs import » n'adopte que ce que sites-available
// porte.
func TestUnConflitSurUnVhostActifAvertitEtLeRemedeSuitLeFichier(t *testing.T) {
	ctx := context.Background()
	for _, moteur := range []string{"apache", "nginx"} {
		prov, _ := fixture(t, moteur)
		p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
		etranger := filepath.Join(filepath.Dir(prov.Engine.AvailableDir()),
			map[string]string{"apache": "conf-enabled", "nginx": "conf.d"}[moteur], "ancien.conf")
		ecrireConf(t, etranger, map[string]string{
			"apache": "<VirtualHost *:80>\n  ServerName shop.fr\n</VirtualHost>\n",
			"nginx":  "server {\n  server_name shop.fr;\n}\n"}[moteur])

		// Création : refus en dur, sans renvoi à « ogs import ».
		obs, err := prov.Observe(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = prov.Plan(ctx, p, obs)
		if err == nil || !strings.Contains(err.Error(), etranger) || strings.Contains(err.Error(), "ogs import") {
			t.Errorf("%s : la création doit être refusée en nommant %s, sans renvoyer à ogs import : %v", moteur, etranger, err)
		}

		// Le vhost du projet déjà écrit et activé : avertissement non bloquant.
		vh := p.VHosts()[0]
		poserVHost(t, prov, p.VHostFile(vh), vhostEcritPour(p.ID(), "x\n"), true)
		obs, err = prov.Observe(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		steps, err := prov.Plan(ctx, p, obs)
		if err != nil {
			t.Fatalf("%s : un conflit sur un vhost actif ne doit pas bloquer le plan : %v", moteur, err)
		}
		var averti error
		for _, s := range steps {
			if strings.HasSuffix(s.Resource, "/conflit") {
				if s.Critical {
					t.Errorf("%s : l'avertissement doit être non bloquant", moteur)
				}
				averti = s.Apply(ctx)
			}
		}
		if averti == nil || !strings.Contains(averti.Error(), etranger) || strings.Contains(averti.Error(), "ogs import") {
			t.Errorf("%s : l'avertissement doit nommer %s et son remède : %v", moteur, etranger, averti)
		}
	}

	// Sous sites-available, le remède reste l'adoption.
	prov, _ := fixture(t, "apache")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ecrireConf(t, filepath.Join(prov.Engine.AvailableDir(), "ancien.conf"),
		"<VirtualHost *:80>\n  ServerName shop.fr\n</VirtualHost>\n")
	obs, _ := prov.Observe(ctx, p)
	if _, err := prov.Plan(ctx, p, obs); err == nil || !strings.Contains(err.Error(), "ogs import") {
		t.Errorf("sous sites-available, le remède reste « ogs import » : %v", err)
	}
}

// Relecture de D118 (v3) : le code 6 ne vaut que si la déclaration étrangère
// est chargée avant le vhost d'ogs — c'est alors elle que le moteur sert. Chargée
// après, ou pas chargée du tout, elle est morte : une note du plan, sans effet
// sur le code de retour.
func TestUnConflitNAvertitQueSiLaDeclarationEtrangereEstServie(t *testing.T) {
	ctx := context.Background()
	bloc := "<VirtualHost *:80>\n  ServerName shop.fr\n</VirtualHost>\n"
	for _, cas := range []struct {
		nom           string
		poser         func(prov *Provider) string
		avertir, note bool
	}{
		{"sites-enabled avant", func(prov *Provider) string {
			return ecrireConf(t, filepath.Join(prov.Engine.EnabledDir(), "000-ancien.conf"), bloc)
		}, true, false},
		{"sites-enabled après", func(prov *Provider) string {
			return ecrireConf(t, filepath.Join(prov.Engine.EnabledDir(), "zz-redirect.conf"), bloc)
		}, false, true},
		{"sites-available activé après", func(prov *Provider) string {
			chemin, _ := poserVHost(t, prov, "zz-ancien.conf", []byte(bloc), true)
			return chemin
		}, false, true},
		{"sites-available activé avant", func(prov *Provider) string {
			chemin, _ := poserVHost(t, prov, "000-ancien.conf", []byte(bloc), true)
			return chemin
		}, true, false},
		{"sites-available sans lien", func(prov *Provider) string {
			chemin, _ := poserVHost(t, prov, "000-ancien.conf", []byte(bloc), false)
			return chemin
		}, false, true},
		{"conf-enabled", func(prov *Provider) string {
			return ecrireConf(t, filepath.Join(filepath.Dir(prov.Engine.AvailableDir()), "conf-enabled", "zz.conf"), bloc)
		}, true, false},
	} {
		prov, _ := fixture(t, "apache")
		p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
		vh := p.VHosts()[0]
		poserVHost(t, prov, p.VHostFile(vh), vhostEcritPour(p.ID(), "x\n"), true)
		etranger := cas.poser(prov)
		obs, err := prov.Observe(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		steps, err := prov.Plan(ctx, p, obs)
		if err != nil {
			t.Fatalf("%s : %v", cas.nom, err)
		}
		averti := false
		for _, s := range steps {
			averti = averti || strings.HasSuffix(s.Resource, "/conflit")
		}
		notes := prov.ConflictNotes(p, obs)
		if averti != cas.avertir || (len(notes) > 0) != cas.note {
			t.Errorf("%s : avertissement %v (attendu %v), notes %v (attendu %v)", cas.nom, averti, cas.avertir, notes, cas.note)
		}
		if cas.note && (len(notes) == 0 || !strings.Contains(notes[0], etranger)) {
			t.Errorf("%s : la note doit nommer %s : %v", cas.nom, etranger, notes)
		}
	}
}

// Un « # » entre guillemets n'ouvre pas de commentaire sous nginx, ni un « ; »
// ou une accolade ne ferment quoi que ce soit.
func TestNginxLitLesGuillemets(t *testing.T) {
	got := servedNames("nginx", "server { add_header X \"#fff\"; add_header Y 'a;{b}'; server_name shop.fr \"www.shop.fr\"; }\n")
	if strings.Join(got, " ") != "shop.fr www.shop.fr" {
		t.Errorf("noms servis : %q", got)
	}
}
