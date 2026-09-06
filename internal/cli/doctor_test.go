package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/provider/monitoring"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/store"
)

// écritSudoers pose une racine sudoers et son répertoire d'inclusion, comme les
// distributions le font, et rend le chemin de la racine.
func écritSudoers(t *testing.T, racine, dropin string) string {
	t.Helper()
	dir := t.TempDir()
	inclus := filepath.Join(dir, "sudoers.d")
	if err := os.MkdirAll(inclus, 0o755); err != nil {
		t.Fatal(err)
	}
	fichier := filepath.Join(dir, "sudoers")
	corps := racine + "\n@includedir " + inclus + "\n"
	if err := os.WriteFile(fichier, []byte(corps), 0o440); err != nil {
		t.Fatal(err)
	}
	if dropin != "" {
		if err := os.WriteFile(filepath.Join(inclus, "00-local"), []byte(dropin+"\n"), 0o440); err != nil {
			t.Fatal(err)
		}
	}
	return fichier
}

func TestEnvResetSuitLesFichiersInclus(t *testing.T) {
	// Le cas qui motive le contrôle : la racine est conforme, le drop-in défait.
	racine := écritSudoers(t, "Defaults env_reset", "Defaults !env_reset")
	status, detail, _ := envResetPolicy(racine)
	if status != "échec" {
		t.Errorf("un drop-in désactivant env_reset doit être vu : %s / %s", status, detail)
	}
	if !strings.Contains(detail, "00-local") {
		t.Errorf("le détail doit nommer le fichier fautif : %s", detail)
	}
}

func TestEnvResetIgnoreLesCommentaires(t *testing.T) {
	// Une ligne d'exemple commentée n'est pas une directive : la version qui
	// cherchait la chaîne dans le fichier entier rendait ici un échec.
	racine := écritSudoers(t, "# Defaults !env_reset\nDefaults env_reset", "")
	if status, detail, _ := envResetPolicy(racine); status != "ok" {
		t.Errorf("commentaire pris pour une directive : %s / %s", status, detail)
	}
}

func TestEnvResetSuitLaFormeHistoriqueDeLInclusion(t *testing.T) {
	// « #includedir » n'est pas un commentaire mais l'ancienne écriture de la
	// directive : écarter toute ligne commençant par « # » ferait perdre
	// l'inclusion qu'on cherche justement à suivre.
	dir := t.TempDir()
	inclus := filepath.Join(dir, "sudoers.d")
	if err := os.MkdirAll(inclus, 0o755); err != nil {
		t.Fatal(err)
	}
	racine := filepath.Join(dir, "sudoers")
	if err := os.WriteFile(racine, []byte("#includedir "+inclus+"\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inclus, "10-legacy"), []byte("Defaults !env_reset\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := envResetPolicy(racine); status != "échec" {
		t.Errorf("#includedir doit être suivi, statut %q", status)
	}
}

func TestEnvResetIgnoreLesFichiersQueSudoIgnore(t *testing.T) {
	// sudo n'ouvre pas les entrées contenant un point ni celles terminées par
	// « ~ » : une sauvegarde d'éditeur ne décide de rien.
	dir := t.TempDir()
	inclus := filepath.Join(dir, "sudoers.d")
	if err := os.MkdirAll(inclus, 0o755); err != nil {
		t.Fatal(err)
	}
	racine := filepath.Join(dir, "sudoers")
	if err := os.WriteFile(racine, []byte("@includedir "+inclus+"\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	for _, nom := range []string{"00-local.dpkg-dist", "00-local~"} {
		if err := os.WriteFile(filepath.Join(inclus, nom), []byte("Defaults !env_reset\n"), 0o440); err != nil {
			t.Fatal(err)
		}
	}
	if status, detail, _ := envResetPolicy(racine); status != "ok" {
		t.Errorf("fichier ignoré par sudo pris en compte : %s / %s", status, detail)
	}
}

func TestEnvResetNeConclutPasSurUneSourceIllisible(t *testing.T) {
	// Affirmer « active » sans avoir tout lu est précisément ce qu'on corrige.
	status, detail, _ := envResetPolicy(filepath.Join(t.TempDir(), "absent"))
	if status != "avertissement" {
		t.Errorf("source illisible : statut %q, détail %q", status, detail)
	}
}

func TestEnvResetLaDerniereAffectationLEmporte(t *testing.T) {
	// Un drop-in qui rétablit env_reset après l'avoir vue désactivée ne laisse
	// aucune anomalie : c'est l'état final qui compte.
	racine := écritSudoers(t, "Defaults !env_reset", "Defaults:%admin env_reset")
	if status, detail, _ := envResetPolicy(racine); status != "ok" {
		t.Errorf("l'état final est actif : %s / %s", status, detail)
	}
}

// fragmentTelegraf pose sous le préfixe un fragment d'entrées portant les deux
// comptes qui ont un mot de passe.
func fragmentTelegraf(t *testing.T, h *harness) {
	t.Helper()
	dir := h.path("etc", "telegraf", "telegraf.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	corps := `[[inputs.mysql]]
  servers = ["telegraf:motdepasse@unix(/run/mysqld/mysqld.sock)/"]
[[inputs.postgresql]]
  address = "host=/run/postgresql port=5432 user=telegraf dbname=postgres sslmode=disable"
[[inputs.rabbitmq]]
  url = "http://127.0.0.1:15672"
  username = "telegraf"
  password = "motdepasse"
`
	if err := os.WriteFile(filepath.Join(dir, "ogs.conf"), []byte(corps), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorSignaleUnCompteDeSupervisionNonScellé(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	fragmentTelegraf(t, h)

	out, _ := h.run("doctor")
	if !strings.Contains(out, "comptes de supervision") {
		t.Fatalf("le contrôle des comptes de supervision doit paraître :\n%s", out)
	}
	for _, attendu := range []string{"mysql/telegraf", "rabbitmq/telegraf"} {
		if !strings.Contains(out, "aucun mot de passe scellé pour "+attendu) {
			t.Errorf("un secret référencé par le fragment et absent du keystore doit être "+
				"signalé (%s) :\n%s", attendu, out)
		}
	}
	// Le compte PostgreSQL est authentifié « peer » et n'a pas de mot de passe
	// (D67) : l'exiger ferait échouer un parc sain.
	if strings.Contains(out, "postgres/") {
		t.Errorf("aucun secret n'est attendu pour PostgreSQL :\n%s", out)
	}
}

func TestDoctorAccepteUnCompteDeSupervisionScellé(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	fragmentTelegraf(t, h)

	st, err := store.Open(h.path("var", "lib", "ogs", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := secret.Open(h.path("etc", "ogs", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, nom := range []string{"mysql/telegraf", "rabbitmq/telegraf"} {
		nonce, ciphertext, err := ks.Seal(monitoring.SecretScope, nom, []byte("motdepasse"))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutSecret(monitoring.SecretScope, nom, nonce, ciphertext); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out, _ := h.run("doctor")
	if strings.Contains(out, "aucun mot de passe scellé") {
		t.Errorf("les deux secrets sont scellés :\n%s", out)
	}
	if !strings.Contains(out, "mysql/telegraf scellé et déchiffrable") {
		t.Errorf("le contrôle doit rendre compte du secret vérifié :\n%s", out)
	}
}

func TestDoctorSignaleUnSecretIndéchiffrable(t *testing.T) {
	// Une base d'état restaurée sans la clé maître qui l'a scellée : le secret est
	// là, l'agent porte le mot de passe, et personne ne peut plus le relire.
	h := newHarness(t)
	h.mustRun("init")
	fragmentTelegraf(t, h)

	autre := filepath.Join(t.TempDir(), "master.key")
	if err := secret.GenerateKey(autre); err != nil {
		t.Fatal(err)
	}
	ks, err := secret.Open(autre)
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := ks.Seal(monitoring.SecretScope, "mysql/telegraf", []byte("motdepasse"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(h.path("var", "lib", "ogs", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSecret(monitoring.SecretScope, "mysql/telegraf", nonce, ciphertext); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out, _ := h.run("doctor")
	if !strings.Contains(out, "déchiffrement refusé") {
		t.Errorf("un secret scellé par une autre clé doit être signalé :\n%s", out)
	}
}

func TestDoctorSignaleUnJournalDeBaseDEtatTropOuvert(t *testing.T) {
	// SQLite calque le mode de « -wal » et de « -shm » sur celui de la base au
	// moment où il les crée, et ne recrée jamais un journal abandonné : celui d'un
	// init interrompu, ou d'une base héritée en 0644, reste lisible sous
	// /var/lib/ogs, que le challenge ACME rend traversable (0751).
	h := newHarness(t)
	h.mustRun("init")
	journal := h.path("var", "lib", "ogs", "state.db-wal")
	if err := os.WriteFile(journal, []byte("journal abandonné"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _ := h.run("doctor")
	if !strings.Contains(out, "journal de la base d'état") {
		t.Fatalf("le contrôle du journal de la base d'état doit paraître :\n%s", out)
	}
	if !strings.Contains(out, "lisible au-delà de son propriétaire") {
		t.Errorf("un journal en 0644 doit être signalé :\n%s", out)
	}
	// « doctor » ouvre la base pour ses contrôles de secrets, et cette ouverture
	// referme les journaux : le rapport doit dire ce qui a été trouvé ET ce que
	// l'exécution a corrigé, sans réclamer un « chmod » devenu sans effet ni sortir
	// en échec pour une anomalie levée.
	if !strings.Contains(out, "remis en 0600") {
		t.Errorf("la réparation faite en passant doit être dite :\n%s", out)
	}
	if strings.Contains(out, "chmod 0600 "+journal) {
		t.Errorf("le remède proposé serait sans effet, le journal étant déjà refermé :\n%s", out)
	}
	if info, err := os.Stat(journal); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("le journal devait être refermé par l'ouverture de la base, mode %04o", perm)
	}
	if !strings.Contains(ligneDe(out, journal), "⚠") {
		t.Errorf("un journal déjà refermé est un avertissement, pas un échec :\n%s", out)
	}
}

// ligneDe rend la première ligne de la sortie qui cite le motif, pour juger du
// symbole d'état sans dépendre du remplissage du tabwriter.
func ligneDe(out, motif string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, motif) {
			return l
		}
	}
	return ""
}

func TestDoctorSignaleUnJournalQuIlNAPasRefermé(t *testing.T) {
	// Sans base à ouvrir, rien ne referme le journal : l'anomalie est toujours là
	// au moment du rapport, et c'est alors un échec assorti du remède qui vaut.
	h := newHarness(t)
	h.mustRun("init")
	journal := h.path("var", "lib", "ogs", "state.db-wal")
	if err := os.WriteFile(journal, []byte("journal abandonné"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.path("var", "lib", "ogs", "state.db")); err != nil {
		t.Fatal(err)
	}

	out, _ := h.run("doctor")
	if !strings.Contains(out, "chmod 0600 "+journal) {
		t.Errorf("un journal encore ouvert doit être corrigé par l'exploitant :\n%s", out)
	}
	if !strings.Contains(ligneDe(out, journal), "✗") {
		t.Errorf("un journal encore ouvert est un échec :\n%s", out)
	}
}

func TestDoctorAccepteUnJournalDeBaseDEtatFerme(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	journal := h.path("var", "lib", "ogs", "state.db-shm")
	if err := os.WriteFile(journal, []byte("journal"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, _ := h.run("doctor")
	if strings.Contains(out, "lisible au-delà de son propriétaire") {
		t.Errorf("un journal en 0600 n'a rien d'anormal :\n%s", out)
	}
}

func TestPrisonDeRecidiveAnnonceeMaisAbsente(t *testing.T) {
	// La prison n'est pas écrite quand le journal de fail2ban n'existe pas (D68),
	// et rien ne le dit : le fichier reste correct, « fail2ban-client -t » n'a
	// aucune prison à reprocher, et la protection annoncée est absente.
	absent := filepath.Join(t.TempDir(), "fail2ban.log")
	status, detail, fix := recidiveJailStatus("[ogs-sshd]\nenabled = true\n", absent)
	if status != "avertissement" {
		t.Fatalf("statut %q, détail %q", status, detail)
	}
	if !strings.Contains(detail, absent) {
		t.Errorf("le détail doit nommer le journal manquant : %s", detail)
	}
	if fix == "" {
		t.Error("le contrôle doit porter un remède actionnable")
	}
}

func TestPrisonDeRecidiveEcriteEstConforme(t *testing.T) {
	status, _, _ := recidiveJailStatus("[ogs-sshd]\n[ogs-recidive]\n", "/var/log/fail2ban.log")
	if status != "ok" {
		t.Errorf("une prison écrite ne doit rien signaler, statut %q", status)
	}
}

func TestPrisonDeRecidiveNeDitRienSansAucunePrison(t *testing.T) {
	// La récidive lit le journal que les autres prisons alimentent : sans elles,
	// son absence n'est pas une anomalie, et l'annoncer serait un faux positif.
	if status, _, _ := recidiveJailStatus("# aucune prison\n", "/var/log/fail2ban.log"); status != "" {
		t.Errorf("aucun contrôle attendu, statut %q", status)
	}
}

func TestDoctorSignaleUnFichierDeSupervisionOrphelin(t *testing.T) {
	// L'apply retire le fichier d'un outil désactivé, mais un parc qu'on n'a pas
	// appliqué depuis garde une supervision active que la configuration dit
	// éteinte : le fichier s'applique toujours, et plus rien ne le recalcule.
	h := newHarness(t)
	h.mustRun("init")
	cfg := h.path("etc", "ogs", "config.yaml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	désactivé := "monitoring:\n  telegraf:\n    enabled: false\n  fail2ban:\n    enabled: false\n"
	updated := regexp.MustCompile(`(?s)monitoring:.*$`).ReplaceAllString(string(data), désactivé)
	if updated == string(data) {
		t.Fatalf("la section monitoring est attendue dans la configuration produite :\n%s", data)
	}
	if err := os.WriteFile(cfg, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	// Les deux fichiers d'un apply antérieur, restés en place.
	fragments := map[string]string{
		filepath.Join("etc", "telegraf", "telegraf.d", "ogs.conf"): "[[inputs.mysql]]\n",
		filepath.Join("etc", "fail2ban", "jail.d", "ogs.conf"):     "[ogs-sshd]\nenabled = true\n",
	}
	for rel, corps := range fragments {
		if err := os.MkdirAll(filepath.Dir(h.path(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.path(rel), []byte(corps), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	out, _ := h.run("doctor")
	if !strings.Contains(out, "supervision désactivée") {
		t.Fatalf("un fichier de supervision orphelin doit être signalé :\n%s", out)
	}
	for rel := range fragments {
		if !strings.Contains(out, h.path(rel)) {
			t.Errorf("le contrôle doit nommer %s :\n%s", rel, out)
		}
	}
}

func TestDoctorNeSignaleRienQuandLaSupervisionEstActive(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	fragmentTelegraf(t, h)

	out, _ := h.run("doctor")
	if strings.Contains(out, "supervision désactivée") {
		t.Errorf("la supervision n'est pas désactivée en configuration :\n%s", out)
	}
}
