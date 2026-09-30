package cli

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/spec"
)

// chaîneDeConnexion rend la valeur de la variable name que « ogs secrets env id »
// émet, et le mot de passe qu'elle porte.
func (h *harness) chaîneDeConnexion(t *testing.T, id, name string) (dsn, password string) {
	t.Helper()
	out := h.mustRun("secrets", "env", id)
	for _, line := range strings.Split(out, "\n") {
		value, ok := strings.CutPrefix(line, name+"=")
		if !ok {
			continue
		}
		dsn, err := strconv.Unquote(value)
		if err != nil {
			t.Fatalf("%s illisible : %q", name, line)
		}
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		password, _ = u.User.Password()
		if len(password) < minGuardedSecret {
			t.Fatalf("mot de passe de %s trop court : %q", id, password)
		}
		return dsn, password
	}
	t.Fatalf("%s absent de « ogs secrets env %s » :\n%s", name, id, out)
	return "", ""
}

// siteSymfonyAvecTransport crée shop.fr, un Symfony dont le transport AMQP est
// réellement créé — par un faux broker —, si bien qu'ogs en a généré et scellé
// le mot de passe comme sur un vrai serveur.
func siteSymfonyAvecTransport(t *testing.T, extra ...string) *harness {
	t.Helper()
	h := newHarness(t)
	h.mustRun("init")
	h.fakeBroker(t, newBrokerState())
	h.mustRunPartial(append([]string{"site", "create", "shop.fr", "--kind", "php-symfony",
		"--app-version", "6.4", "--php", "8.5", "--messenger", "async", "--yes"}, extra...)...)
	return h
}

// modifierDéclaration applique change à la déclaration du projet id.
func (h *harness) modifierDéclaration(t *testing.T, id string, change func(p *model.Project)) {
	t.Helper()
	decl := h.path("etc", "ogs", "projects", id+".yaml")
	p, err := spec.Load(decl)
	if err != nil {
		t.Fatal(err)
	}
	change(p)
	if err := spec.Save(decl, p); err != nil {
		t.Fatal(err)
	}
}

// aucunFichierNePorte exige qu'aucun fichier sous racine ne contienne secret.
func aucunFichierNePorte(t *testing.T, racine, secret string) {
	t.Helper()
	err := filepath.Walk(racine, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			if data, _ := os.ReadFile(p); strings.Contains(string(data), secret) {
				t.Errorf("%s porte un secret de la production", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// pasDeCopieAnnonceeFaite exige que la sortie n'annonce pas une copie fidèle.
func pasDeCopieAnnonceeFaite(t *testing.T, out string) {
	t.Helper()
	for _, ligne := range strings.Split(out, "\n") {
		if strings.HasPrefix(ligne, "✓ contenu copié") {
			t.Errorf("une copie expurgée ne doit pas s'annoncer faite : %q", ligne)
		}
	}
}

// sortieHonnête exige que la sortie d'un clone n'annonce jamais une
// neutralisation complète : ogs ne retire que ce qu'il connaît, et la sortie
// doit le dire.
func sortieHonnête(t *testing.T, out string) {
	t.Helper()
	for _, faux := range []string{"ni les identifiants ni les secrets", "sans la configuration ni les secrets",
		"Rien de ce qui désigne"} {
		if strings.Contains(out, faux) {
			t.Errorf("la sortie annonce une neutralisation que le clone ne fait pas (%q) :\n%s", faux, out)
		}
	}
}

// contientTout exige chacune des chaînes want dans la sortie.
func contientTout(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("%q absent de la sortie :\n%s", w, out)
		}
	}
}

func TestLeCloneNeLaissePasLaPreproductionDesignerLaProduction(t *testing.T) {
	// Le .env.local de la production, écrit depuis « ogs secrets env » comme le
	// guide l'indique, était recopié tel quel : la préproduction consommait les
	// files de la production avec ses identifiants, et son compte recevait en
	// clair APP_SECRET et la clé du coffre. La sortie ne disait que « ✓ contenu
	// copié », code 0.
	h := siteSymfonyAvecTransport(t)
	dsn, password := h.chaîneDeConnexion(t, "shopfr", "MESSENGER_TRANSPORT_DSN")
	www := h.path("home", "shop.fr", "www")
	for rel, contenu := range map[string]string{
		".env":       "APP_ENV=dev\nMESSENGER_TRANSPORT_DSN=doctrine://default\n",
		".env.local": "MESSENGER_TRANSPORT_DSN=\"" + dsn + "\"\nAPP_SECRET=secret-de-production\n",
		// Versionné, et ne devrait porter que des valeurs par défaut : il est
		// recopié, le mot de passe masqué.
		".env.prod": "MESSENGER_TRANSPORT_DSN=\"" + dsn + "\"\n",
		"config/secrets/prod/prod.decrypt.private.php": "<?php return 'clé-du-coffre-de-production';",
		"var/cache/prod/Container.php":                 "<?php return ['secret' => 'secret-de-production'];",
		"public/index.php":                             "<?php echo 'production';",
	} {
		déposer(t, filepath.Join(www, filepath.FromSlash(rel)), contenu)
	}

	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("une préproduction à configurer est un succès partiel : code %d, attendu %d\n%s", code, CodePartial, out)
	}
	prep := h.path("home", "shop.preprod-pp.net", "www")
	for _, rel := range []string{".env.local", "config/secrets/prod/prod.decrypt.private.php", "var/cache"} {
		if _, err := os.Lstat(filepath.Join(prep, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s a été recopié dans la préproduction : %v", rel, err)
		}
		if !strings.Contains(out, filepath.FromSlash(rel)+`" : `) {
			t.Errorf("%s doit être nommé comme non recopié :\n%s", rel, out)
		}
	}
	intact(t, filepath.Join(prep, ".env"), "APP_ENV=dev\nMESSENGER_TRANSPORT_DSN=doctrine://default\n")
	intact(t, filepath.Join(prep, "public", "index.php"), "<?php echo 'production';")
	masqué := strings.ReplaceAll(dsn, password, string(fs.MaskOf(len(password))))
	intact(t, filepath.Join(prep, ".env.prod"), "MESSENGER_TRANSPORT_DSN=\""+masqué+"\"\n")
	aucunFichierNePorte(t, prep, password)
	aucunFichierNePorte(t, prep, "secret-de-production")
	aucunFichierNePorte(t, prep, "clé-du-coffre-de-production")

	contientTout(t, out, "⚠ non recopié", "fichier de configuration d'une application php-symfony",
		"⚠ secret retiré de la copie", ".env.prod", "mot de passe de shopfr (vhost AMQP shopfr)",
		"ogs n'a retiré de la copie que les fichiers de configuration et les caches connus d'une application "+
			"php-symfony et les mots de passe du transport de shopfr",
		"shopfrprep reprend tel quel de shopfr", "chaînes expurgées",
		"configuration de shopfrprep à écrire", "ogs secrets env shopfrprep",
		"shopfrprep à revoir avant usage : 3 entrée(s) de configuration non recopiée(s), 1 fichier(s) expurgé(s)")
	pasDeCopieAnnonceeFaite(t, out)
	sortieHonnête(t, out)
	// La préproduction a les siens, qu'ogs lui a générés.
	prepDSN, prepPassword := h.chaîneDeConnexion(t, "shopfrprep", "MESSENGER_TRANSPORT_DSN")
	if prepPassword == password || !strings.Contains(prepDSN, "/shopfrprep") {
		t.Errorf("la préproduction doit avoir son propre transport : %s", prepDSN)
	}
}

func TestLeCloneDitCeQuIlRepriseTelQuel(t *testing.T) {
	// Le .env.local retenu, la sortie affirmait qu'aucun identifiant ni secret
	// de la production n'était recopié — alors que le .env versionné portait le
	// relais de courrier, un fichier de config/ la clé de paiement, et le pool
	// une autre : tous trois passaient à la préproduction, octet pour octet.
	// Le clone dit ce qu'il a retiré, que le reste passe tel quel, et nomme la
	// variable du pool.
	h := siteSymfonyAvecTransport(t)
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.PHP.Pools[0].Env = map[string]string{"STRIPE_SECRET_KEY": "sk_live_pool"}
	})
	www := h.path("home", "shop.fr", "www")
	mailer := "MAILER_DSN=smtp://relais:mdp-relais-prod@smtp.example.net:587\n"
	stripe := "parameters:\n  stripe: sk_live_fichier\n"
	for rel, contenu := range map[string]string{
		".env":                             mailer,
		".env.local":                       "APP_SECRET=secret-de-production\n",
		"config/packages/prod/stripe.yaml": stripe,
	} {
		déposer(t, filepath.Join(www, filepath.FromSlash(rel)), contenu)
	}

	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d\n%s", code, CodePartial, out)
	}
	prep := h.path("home", "shop.preprod-pp.net", "www")
	// Ce qu'ogs ne connaît pas est bien là : la sortie doit le dire.
	intact(t, filepath.Join(prep, ".env"), mailer)
	intact(t, filepath.Join(prep, "config", "packages", "prod", "stripe.yaml"), stripe)
	sortieHonnête(t, out)
	contientTout(t, out,
		"⚠ repris de shopfr tel quel : spec.php.pools[web].env.STRIPE_SECRET_KEY",
		"shopfrprep reprend tel quel de shopfr :",
		"tout autre fichier de www (un .env versionné, un fichier de config/…) : un secret qu'ogs ne connaît pas y est en clair",
		"déclaration            1 champ(s) à revoir",
		"1 champ(s) de la déclaration à revoir")
	clone, err := spec.Load(h.path("etc", "ogs", "projects", "shopfrprep.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := clone.Spec.PHP.Pools[0].Env["STRIPE_SECRET_KEY"]; got != "sk_live_pool" {
		t.Errorf("une variable reprise telle quelle doit l'être vraiment : %q", got)
	}
}

func TestLeCloneNommeLaDeclarationQuIlReprend(t *testing.T) {
	// Une variable déclarée dans le projet parvient à l'application sans passer
	// par sa configuration — par FPM, par systemd, par la ligne cron. Le clone la
	// reprenait sans un mot, code 0 et « ✓ contenu copié » : la préproduction
	// atteignait la base externe de la production avec une clé de paiement
	// réelle. Chaque variable reprise est nommée — le secret d'ogs masqué —, et
	// compte dans le code 6 ; la racine de la production, dans une commande ou
	// un répertoire de travail, est remplacée par celle du clone.
	h := siteSymfonyAvecTransport(t, "--cron", "0 3 * * *:bin/console app:purge",
		"--service", "symfony-messenger:async:1")
	dsn, password := h.chaîneDeConnexion(t, "shopfr", "MESSENGER_TRANSPORT_DSN")
	racine := h.path("home", "shop.fr")
	externe := "mysql://app:mdp-externe-prod@db.prod.interne:3306/shop"
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.PHP.Pools[0].Env = map[string]string{"MESSENGER_TRANSPORT_DSN": dsn, "DATABASE_URL": externe,
			"STRIPE_KEY": "sk_live_pool"}
		p.Spec.Crons[0].Command = "bin/console app:purge --dsn=" + dsn
		p.Spec.Crons[0].Workdir = filepath.Join(racine, "www")
		p.Spec.Services[0].Params["env.WORKER_TOKEN"] = "jeton-de-production"
	})
	déposer(t, filepath.Join(racine, "www", "public", "index.php"), "<?php echo 'production';")

	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d\n%s", code, CodePartial, out)
	}
	prepRacine := h.path("home", "shop.preprod-pp.net")
	contientTout(t, out,
		"⚠ repris de shopfr, secret masqué : spec.php.pools[web].env.MESSENGER_TRANSPORT_DSN (mot de passe de shopfr (vhost AMQP shopfr))",
		"⚠ repris de shopfr tel quel : spec.php.pools[web].env.DATABASE_URL",
		"⚠ repris de shopfr tel quel : spec.php.pools[web].env.STRIPE_KEY",
		"⚠ repris de shopfr tel quel : spec.services[messenger-async].params.env.WORKER_TOKEN",
		"⚠ repris de shopfr, secret masqué : spec.crons[app-purge].command",
		"⚠ repris de shopfr, racine "+racine+" remplacée par "+prepRacine+" : spec.crons[app-purge].workdir",
		// Le contenu, lui, n'a rien porté : la copie est dite faite, le code 6
		// vient de la déclaration.
		"✓ contenu copié",
		"shopfrprep à revoir avant usage : 5 champ(s) de la déclaration à revoir")
	if strings.Contains(out, "params.transport") {
		t.Errorf("un réglage du service sans secret ne doit pas être nommé :\n%s", out)
	}
	clone, err := spec.Load(h.path("etc", "ogs", "projects", "shopfrprep.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	masqué := strings.ReplaceAll(dsn, password, string(fs.MaskOf(len(password))))
	if got := clone.Spec.PHP.Pools[0].Env["MESSENGER_TRANSPORT_DSN"]; got != masqué {
		t.Errorf("variable du pool de la préproduction : %q, attendu %q", got, masqué)
	}
	if got := clone.Spec.PHP.Pools[0].Env["DATABASE_URL"]; got != externe {
		t.Errorf("une variable sans secret d'ogs est reprise telle quelle, et dite telle : %q", got)
	}
	if got := clone.Spec.Crons[0].Workdir; got != filepath.Join(prepRacine, "www") {
		t.Errorf("répertoire de travail de la tâche du clone : %q", got)
	}
	aucunFichierNePorte(t, h.path("etc", "php"), password)
	aucunFichierNePorte(t, h.path("etc", "cron.d"), password)
	aucunFichierNePorte(t, h.path("etc", "ogs", "projects", "shopfrprep.yaml"), password)
	// La source, elle, garde sa déclaration : le clone n'a touché aucune de ses
	// tables.
	source, err := spec.Load(h.path("etc", "ogs", "projects", "shopfr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if source.Spec.PHP.Pools[0].Env["MESSENGER_TRANSPORT_DSN"] != dsn || !strings.Contains(source.Spec.Crons[0].Command, dsn) ||
		source.Spec.Crons[0].Workdir != filepath.Join(racine, "www") {
		t.Errorf("la déclaration de la production a été modifiée : %+v", source.Spec)
	}
}

func TestLeCloneRepartTachesEtServicesDesactives(t *testing.T) {
	// Sur une copie des données de la production, une tâche de relance écrit
	// aux vrais clients, et un worker démarre avant que la préproduction ait sa
	// configuration. Le clone les reprend désactivés, et dit comment les
	// réactiver.
	h := siteSymfonyAvecTransport(t, "--service", "symfony-messenger:async:2",
		"--cron", "* * * * *:bin/console schedule:run")

	out, _ := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	clone, err := spec.Load(h.path("etc", "ogs", "projects", "shopfrprep.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range clone.Spec.Crons {
		if isEnabled(c.Enabled) {
			t.Errorf("la tâche %s de la préproduction doit être désactivée", c.Name)
		}
	}
	for _, s := range clone.Spec.Services {
		if isEnabled(s.Enabled) {
			t.Errorf("le service %s de la préproduction doit être désactivé", s.Name)
		}
	}
	source, err := spec.Load(h.path("etc", "ogs", "projects", "shopfr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !isEnabled(source.Spec.Crons[0].Enabled) || !isEnabled(source.Spec.Services[0].Enabled) {
		t.Error("les tâches et services de la production doivent rester actifs")
	}
	if cron := h.read(t, filepath.Join("etc", "cron.d", "ogs-shopfrprep")); strings.Contains(cron, "\n* * * * *") {
		t.Errorf("la tâche de la préproduction ne doit pas être planifiée :\n%s", cron)
	}
	contientTout(t, out, "tâche        schedule-run (* * * * *, désactivée)",
		"service      messenger-async (symfony-messenger, 2 instance(s), désactivé)",
		"repris désactivés", "ogs cron enable shopfrprep schedule-run",
		"service messenger-async : retirer « enabled: false »")
}

func TestLeCloneNeSeContreditPasSurUnSecretQuIlNAPasCherche(t *testing.T) {
	// Un php-plain dont le mot de passe du transport n'est pas dans le keystore
	// — un projet adopté, ou comme ici un transport jamais créé : la sortie
	// disait la copie non vérifiée, puis qu'aucun identifiant ni secret n'était
	// recopié, puis qu'un secret l'était. Elle dit désormais qu'ogs n'a rien
	// cherché, et que la copie peut porter ce secret en clair.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "blog.fr", "--kind", "php-plain", "--php", "8.3",
		"--messenger", "async", "--yes")
	www := h.path("home", "blog.fr", "www")
	déposer(t, filepath.Join(www, "wp-config.php"), "<?php define('AMQP_PASSWORD', 'mot-de-passe-de-production');\n")

	out, code := h.run("site", "clone", "blogfr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d\n%s", code, CodePartial, out)
	}
	sortieHonnête(t, out)
	pasDeCopieAnnonceeFaite(t, out)
	contientTout(t, out,
		"⚠ copie non vérifiée : mot de passe de blogfr (vhost AMQP blogfr)",
		"ogs n'a rien retiré de la copie : il ne connaît pas les fichiers de configuration d'une application "+
			"php-plain, et n'a cherché aucun secret de blogfr",
		"tous les fichiers de www : un secret qu'ogs ne connaît pas y est en clair",
		"secrets non cherchés   1, nommé(s) plus haut : la copie peut les porter en clair",
		"1 secret(s) non cherché(s)")
	intact(t, filepath.Join(h.path("home", "blog.preprod-pp.net", "www"), "wp-config.php"),
		"<?php define('AMQP_PASSWORD', 'mot-de-passe-de-production');\n")
}

func TestLeCloneDePrestashopNeRecopiePasSesParametres(t *testing.T) {
	// parameters.php retenu, un install/ resté en production renverrait tout
	// visiteur de la préproduction, publique, vers l'installeur : le clone le
	// dit.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "boutique.fr", "--kind", "php-prestashop", "--php", "8.1", "--yes")
	www := h.path("home", "boutique.fr", "www")
	déposer(t, filepath.Join(www, "app", "config", "parameters.php"),
		"<?php return ['parameters' => ['database_name' => 'boutiquefr', 'cookie_key' => 'clé']];\n")
	déposer(t, filepath.Join(www, "config", "settings.inc.php"), "<?php define('_COOKIE_KEY_', 'clé');\n")
	déposer(t, filepath.Join(www, "index.php"), "<?php // boutique\n")
	déposer(t, filepath.Join(www, "install", "index.php"), "<?php // installeur\n")

	out, code := h.run("site", "clone", "boutiquefr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d\n%s", code, CodePartial, out)
	}
	prep := h.path("home", "boutique.preprod-pp.net", "www")
	for _, rel := range []string{"app/config/parameters.php", "config/settings.inc.php"} {
		if _, err := os.Lstat(filepath.Join(prep, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s a été recopié : %v", rel, err)
		}
	}
	intact(t, filepath.Join(prep, "index.php"), "<?php // boutique\n")
	pasDeCopieAnnonceeFaite(t, out)
	sortieHonnête(t, out)
	contientTout(t, out, "⚠ installeur exposé : "+filepath.Join(prep, "install")+"/ est recopié",
		"une préproduction est publique (D57)", "installeur exposé")
}

func TestLeCloneNeDitPasLInstalleurExposeQuandLaConfigurationEstLa(t *testing.T) {
	// Un hook postCreate qui écrit le parameters.php de la préproduction le voit
	// conservé : l'installeur n'est plus offert, et le clone ne le dit pas.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "boutique.fr", "--kind", "php-prestashop", "--php", "8.1", "--yes")
	www := h.path("home", "boutique.fr", "www")
	déposer(t, filepath.Join(www, "install-dev", "index.php"), "<?php // installeur\n")
	déposer(t, filepath.Join(h.path("home", "boutique.preprod-pp.net", "www"), "app", "config", "parameters.php"),
		"<?php // préproduction\n")

	out, _ := h.run("site", "clone", "boutiquefr", "--to", "prep", "--yes")
	if strings.Contains(out, "installeur exposé") {
		t.Errorf("une configuration présente ferme l'installeur :\n%s", out)
	}
}

func TestLeCloneDUnSitePHPQuelconqueMasqueCeQuIlReconnait(t *testing.T) {
	// ogs ne connaît pas les fichiers d'une application php-plain : il n'en
	// retient aucun — un WordPress privé de wp-config.php offrirait son
	// assistant d'installation sur une préproduction publique —, en masque les
	// mots de passe qu'il a générés, et dit que le reste passe tel quel. Un lien
	// vers la configuration rangée hors de www, dans le home de la production,
	// passait sans un mot : il est nommé.
	h := newHarness(t)
	h.mustRun("init")
	h.fakeBroker(t, newBrokerState())
	h.mustRunPartial("site", "create", "blog.fr", "--kind", "php-plain", "--php", "8.3",
		"--messenger", "async", "--yes")
	_, password := h.chaîneDeConnexion(t, "blogfr", "MESSENGER_TRANSPORT_DSN")
	racine := h.path("home", "blog.fr")
	www := filepath.Join(racine, "www")
	déposer(t, filepath.Join(www, "wp-config.php"),
		"<?php\ndefine('AMQP_PASSWORD', '"+password+"');\ndefine('AUTH_KEY', 'sel-de-production');\n")
	partagé := filepath.Join(racine, "shared", "secrets.php")
	déposer(t, partagé, "<?php define('AMQP_PASSWORD', '"+password+"');\n")
	if err := os.Symlink(partagé, filepath.Join(www, "secrets.php")); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("site", "clone", "blogfr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d\n%s", code, CodePartial, out)
	}
	prep := h.path("home", "blog.preprod-pp.net", "www")
	intact(t, filepath.Join(prep, "wp-config.php"), "<?php\ndefine('AMQP_PASSWORD', '"+
		string(fs.MaskOf(len(password)))+"');\ndefine('AUTH_KEY', 'sel-de-production');\n")
	sortieHonnête(t, out)
	contientTout(t, out, "secret retiré de la copie", "wp-config.php",
		"⚠ lien recopié tel quel : \""+filepath.Join(www, "secrets.php")+"\" → \""+partagé+
			"\" : cible absolue, dans le projet source \""+racine+"\"",
		"ogs n'a retiré de la copie que les mots de passe du transport de blogfr — il ne connaît pas les "+
			"fichiers de configuration d'une application "+string(model.KindPHPPlain),
		"tous les fichiers de www : un secret qu'ogs ne connaît pas y est en clair",
		"1 lien(s) qui sortent de la destination")
}

func TestLeCloneMasqueLesValeursDuFichierDEnvironnement(t *testing.T) {
	// Le fichier d'environnement de la production (D75) n'est pas recopié, mais
	// l'exploitant en reporte souvent les valeurs dans un fichier de www : ogs
	// les connaît, et les masque comme ses mots de passe. La commande d'une
	// application custom, absolue sous la racine du projet, faisait lancer par la
	// préproduction le programme de la production : sa racine est remplacée.
	h := newHarness(t)
	h.mustRun("init")
	racine := h.path("home", "api.fr")
	h.mustRunPartial("site", "create", "api.fr", "--kind", "custom",
		"--command", filepath.Join(racine, "www", "bin", "serveur")+" --port ${PORT}", "--env-file", "--yes")
	envFile := h.path("etc", "ogs", "env", "apifr.env")
	if _, err := os.Stat(envFile); err != nil {
		t.Fatalf("le fichier d'environnement doit avoir été créé : %v", err)
	}
	f, err := os.OpenFile(envFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("STRIPE_KEY=\"sk_live_0123456789abcdef\"\nPORT=3000\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	www := filepath.Join(racine, "www")
	déposer(t, filepath.Join(www, "config", "app.json"), `{"stripe": "sk_live_0123456789abcdef", "port": 3000}`)

	out, code := h.run("site", "clone", "apifr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d\n%s", code, CodePartial, out)
	}
	prep := h.path("home", "api.preprod-pp.net")
	intact(t, filepath.Join(prep, "www", "config", "app.json"),
		`{"stripe": "`+string(fs.MaskOf(len("sk_live_0123456789abcdef")))+`", "port": 3000}`)
	contientTout(t, out, "secret retiré de la copie", "valeur de STRIPE_KEY ("+envFile+")",
		"les valeurs de "+envFile+" d'au moins 12 caractères",
		"⚠ repris de apifr, racine "+racine+" remplacée par "+prep+" : spec.app.command",
		"environnement          "+h.path("etc", "ogs", "env", "apifrprep.env")+", créé vide")
	clone, err := spec.Load(h.path("etc", "ogs", "projects", "apifrprep.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(prep, "www", "bin", "serveur") + " --port ${PORT}"; clone.Spec.App.Command != want {
		t.Errorf("commande du clone : %q, attendu %q", clone.Spec.App.Command, want)
	}
	if data, err := os.ReadFile(h.path("etc", "ogs", "env", "apifrprep.env")); err != nil ||
		strings.Contains(string(data), "sk_live") {
		t.Errorf("le fichier d'environnement du clone doit être créé vide : %q, %v", data, err)
	}
}

func TestLeCloneRefuseNeDitAucunFichierRepris(t *testing.T) {
	// Une copie refusée n'a rien recopié : la sortie ne dit ni que les fichiers
	// de www sont passés, ni qu'un secret n'y a pas été cherché.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "blog.fr", "--kind", "php-plain", "--php", "8.3",
		"--messenger", "async", "--yes")
	www := h.path("home", "blog.fr", "www")
	if err := os.Rename(www, www+".orig"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(www+".orig", www); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("site", "clone", "blogfr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d\n%s", code, CodePartial, out)
	}
	contientTout(t, out, "rien n'a été recopié")
	for _, faux := range []string{"tous les fichiers de www", "copie non vérifiée", "ogs n'a rien retiré"} {
		if strings.Contains(out, faux) {
			t.Errorf("une copie refusée ne doit pas dire %q :\n%s", faux, out)
		}
	}
}

func TestLeCloneDitQueLaBaseCopieeEstRepriseTelleQuelle(t *testing.T) {
	// --with-data recopie ce que l'application range en base — relais de
	// courrier, clés des modules de paiement, domaine du site : la sortie le
	// dit. Le mode préfixé n'a pas de moteur pour copier réellement la base.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-prestashop", "--php", "8.1", "--yes")
	h.mustRunPartial("site", "clone", "shopfr", "--to", "prep", "--yes")
	app := newApp()
	var out strings.Builder
	app.Out, app.Paths = &out, config.NewPaths(h.prefix)
	source, err := spec.Load(app.Paths.ProjectFile("shopfr"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := spec.Load(app.Paths.ProjectFile("shopfrprep"))
	if err != nil {
		t.Fatal(err)
	}
	app.adviseCloneConfig(source, target, cloneGuard{}, fs.Skipped{}, declaredReview{}, true, true)
	contientTout(t, out.String(), "base                   copie de celle de shopfr : ce que l'application y range")
	out.Reset()
	app.adviseCloneConfig(source, target, cloneGuard{}, fs.Skipped{}, declaredReview{}, true, false)
	if strings.Contains(out.String(), "copie de celle de shopfr") {
		t.Errorf("une base non copiée ne doit pas être dite reprise :\n%s", out.String())
	}
}

func TestLaRacineDeLaSourceNEstRemplaceeQuEntiere(t *testing.T) {
	for _, c := range []struct{ avant, après string }{
		{"/home/shop.fr", "/home/prep"},
		{"/home/shop.fr/www/bin/serveur --port ${PORT}", "/home/prep/www/bin/serveur --port ${PORT}"},
		{"cd /home/shop.fr && ./x /home/shop.fr/var", "cd /home/prep && ./x /home/prep/var"},
		{"ROOT=/home/shop.fr;", "ROOT=/home/prep;"},
		{"/home/shop.fr.old/www", "/home/shop.fr.old/www"},
		{"/home/shop.frx", "/home/shop.frx"},
		{"/srv/home/shop.fr/www", "/srv/home/shop.fr/www"},
		{"sans chemin", "sans chemin"},
	} {
		got, changed := rebase(c.avant, "/home/shop.fr", "/home/prep")
		if got != c.après || changed != (c.avant != c.après) {
			t.Errorf("rebase(%q) = %q, %v ; attendu %q", c.avant, got, changed, c.après)
		}
	}
}

func TestLeFichierDEnvironnementSeRelitCommeSystemdLeLit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shopfr.env")
	if err := os.WriteFile(path, []byte("# commentaire\n; autre\n\nA=un\n B = \"deux mots\" \nC='trois'\nsans-égal\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	vars, err := readEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []envVar{{"A", "un"}, {"B", "deux mots"}, {"C", "trois"}}
	if len(vars) != len(want) {
		t.Fatalf("affectations : %+v, attendu %+v", vars, want)
	}
	for i := range want {
		if vars[i] != want[i] {
			t.Errorf("affectation %d : %+v, attendu %+v", i, vars[i], want[i])
		}
	}
	// Un lien à sa place n'est pas suivi : systemd le suivrait en root.
	lien := filepath.Join(dir, "lien.env")
	if err := os.Symlink(path, lien); err != nil {
		t.Fatal(err)
	}
	if _, err := readEnvFile(lien); err == nil || !strings.Contains(err.Error(), "lien refusé") {
		t.Errorf("un lien doit être refusé : %v", err)
	}
	if _, err := readEnvFile(filepath.Join(dir, "absent.env")); !os.IsNotExist(err) {
		t.Errorf("un fichier absent se dit absent : %v", err)
	}
}

func TestLeCloneSimuleAnnonceSaGarde(t *testing.T) {
	// --dry-run n'annonçait que la désactivation des tâches : ni les fichiers
	// qui ne seraient pas recopiés, ni les secrets masqués, ni le code 6.
	h := siteSymfonyAvecTransport(t)
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.PHP.Pools[0].Env = map[string]string{"STRIPE_KEY": "sk_live_pool"}
	})

	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes", "--dry-run")
	if code != CodeOK {
		t.Errorf("code %d :\n%s", code, out)
	}
	contientTout(t, out, "copie de www :",
		"non recopiés           .env.local, .env.local.php, .env.*.local, config/secrets/*/*.decrypt.private.php",
		"masqués partout        les mots de passe du transport de shopfr",
		"déclaration            repris de shopfr tel quel : spec.php.pools[web].env.STRIPE_KEY",
		"le clone rendra le code 6 : 1 champ(s) de la déclaration à revoir",
		"--dry-run : rien n'a été écrit")
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfrprep.yaml")); !os.IsNotExist(err) {
		t.Errorf("--dry-run ne doit rien écrire : %v", err)
	}
}

func TestLeCloneDUnProjetAdopteNEstPasAdopte(t *testing.T) {
	// La préproduction héritait de l'étiquette adopted : elle échappait au refus
	// des bases et comptes homonymes, et sa suppression sous --force les emportait
	// (D91). Elle est créée par ogs, pas reprise.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.modifierDeclaration(t, "shopfr", adopter)
	h.mustRunPartial("site", "clone", "shopfr", "--to", "prep", "--yes")
	data, err := os.ReadFile(h.path("etc", "ogs", "projects", "shopfrprep.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "adopted") {
		t.Errorf("la préproduction ne doit pas être adoptée :\n%s", data)
	}
	source, err := os.ReadFile(h.path("etc", "ogs", "projects", "shopfr.yaml"))
	if err != nil || !strings.Contains(string(source), "adopted") {
		t.Errorf("la source doit rester adoptée : %v\n%s", err, source)
	}
}
