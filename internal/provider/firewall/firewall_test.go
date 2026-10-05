package firewall

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
)

// banc prépare une machine sous préfixe : ufw installé, inactif, IPv6 actif.
type banc struct {
	t    *testing.T
	root string
	rec  *exec.Recorder
	prov *Provider
}

func nouveauBanc(t *testing.T, declaration string, units ...string) *banc {
	t.Helper()
	root := t.TempDir()
	b := &banc{t: t, root: root, rec: exec.NewRecorder()}
	b.write("/etc/ufw/applications.d/.garde", "")
	b.write("/etc/ufw/ufw.conf", "ENABLED=no\nLOGLEVEL=low\n")
	b.write("/etc/default/ufw", "IPV6=yes\nDEFAULT_INPUT_POLICY=\"DROP\"\nDEFAULT_OUTPUT_POLICY=\"ACCEPT\"\nDEFAULT_FORWARD_POLICY=\"DROP\"\n")
	for _, u := range units {
		b.write("/lib/systemd/system/"+u+".service", "")
	}
	cfg := &config.UFW{}
	if err := yaml.UnmarshalWithOptions([]byte(declaration), cfg, yaml.Strict()); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	b.prov = &Provider{Paths: config.NewPaths(root), Exec: b.rec, Cfg: cfg, WebUnit: "apache2",
		Now: func() time.Time { return time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC) }}
	return b
}

func (b *banc) write(path, content string) {
	b.t.Helper()
	full := filepath.Join(b.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *banc) plan(projects ...*model.Project) []*engine.Step {
	b.t.Helper()
	steps, err := b.planErr(projects...)
	if err != nil {
		b.t.Fatal(err)
	}
	return steps
}

func (b *banc) planErr(projects ...*model.Project) ([]*engine.Step, error) {
	obs, err := b.prov.Observe(context.Background())
	if err != nil {
		b.t.Fatal(err)
	}
	return b.prov.Plan(context.Background(), projects, obs)
}

// tuple écrit une ligne de user.rules telle qu'ufw la range.
func tuple(fields, comment string) string {
	line := "### tuple ### " + fields
	if comment != "" {
		line += " comment=" + hex.EncodeToString([]byte(comment))
	}
	return line + "\n-A ufw-user-input -j ACCEPT\n\n"
}

// rulesFile compose un user.rules à partir des règles voulues : c'est l'état
// d'un pare-feu déjà réconcilié.
func rulesFile(rules []Rule, family int) string {
	var b strings.Builder
	b.WriteString("*filter\n")
	anyAddr := "0.0.0.0/0"
	if family == 6 {
		anyAddr = "::/0"
	}
	for _, r := range rules {
		fams := r.Families(true)
		ok := false
		for _, f := range fams {
			ok = ok || f == family
		}
		if !ok {
			continue
		}
		src := r.From
		if src == config.UFWAny {
			src = anyAddr
		}
		switch {
		case r.App != "":
			b.WriteString(tuple(fmt.Sprintf("%s any 22 %s any %s %s - in", r.Action, anyAddr, src, r.App), r.Comment))
		case r.Direction == "in":
			b.WriteString(tuple(fmt.Sprintf("%s any any %s any %s in", r.Action, anyAddr, src), r.Comment))
		default:
			b.WriteString(tuple(fmt.Sprintf("%s %s %s %s any %s out", r.Action, r.Proto, r.Port, anyAddr, src), r.Comment))
		}
	}
	return b.String() + "COMMIT\n"
}

func resources(steps []*engine.Step) []string {
	var out []string
	for _, s := range steps {
		if s.Mutates() {
			out = append(out, s.Resource)
		}
	}
	return out
}

func diffs(t *testing.T, steps []*engine.Step) string {
	var b strings.Builder
	for _, s := range steps {
		if s.Diff == nil {
			continue
		}
		d, err := s.Diff()
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(d + "\n")
	}
	return b.String()
}

const declarationType = `
sources:
  bureau:
    owner: "Agence de Nantes"
    addresses: [203.0.113.10]
  acme:
    owner: "ACME SARL"
    addresses: [198.51.100.7]
trusted: [bureau]
profiles:
  web: {allow: [any]}
  mysql: {allow: [acme]}
ban: [192.0.2.66]
`

func TestParseRulesReconnaitLesTroisFormes(t *testing.T) {
	data := "*filter\n" +
		tuple("allow any 3306 0.0.0.0/0 any 198.51.100.7 ogs-mysql - in", "ogs: acme") +
		tuple("deny any any 0.0.0.0/0 any 192.0.2.66 in", "ogs: bannie") +
		tuple("allow any 53 0.0.0.0/0 any 0.0.0.0/0 out", "ogs: sortie dns") +
		tuple("limit tcp 22 0.0.0.0/0 any 0.0.0.0/0 in", "") +
		tuple("allow tcp 8080 0.0.0.0/0 any 0.0.0.0/0 in_eth0", "ogs: retouchée")
	rules := parseRules([]byte(data), 4)
	if len(rules) != 5 {
		t.Fatalf("%d règles lues, 5 attendues", len(rules))
	}
	if r := rules[0]; !r.Recognized || !r.Managed || r.App != "ogs-mysql" || r.From != "198.51.100.7" {
		t.Errorf("accès mal lu : %+v", r)
	}
	if r := rules[1]; !r.Recognized || r.Action != "deny" || r.From != "192.0.2.66" {
		t.Errorf("refus mal lu : %+v", r)
	}
	if r := rules[2]; !r.Recognized || r.Direction != "out" || r.Port != "53" || r.From != config.UFWAny {
		t.Errorf("sortie mal lue : %+v", r)
	}
	if r := rules[3]; r.Recognized || r.Managed || r.Verb != "limit" {
		t.Errorf("une règle de l'administrateur n'est ni d'ogs ni reconnue : %+v", r)
	}
	if r := rules[4]; r.Recognized || !r.Managed {
		t.Errorf("une règle marquée mais retouchée reste méconnaissable : %+v", r)
	}
}

func TestDetectionSSHEtProftpd(t *testing.T) {
	b := nouveauBanc(t, "{}", "ssh", "proftpd")
	b.write("/etc/ssh/sshd_config", "Include /etc/ssh/sshd_config.d/*.conf\nPort 22\nMatch User x\n  Port 9999\n")
	b.write("/etc/ssh/sshd_config.d/ogs.conf", "Port 2200\n")
	b.write("/etc/proftpd/proftpd.conf", "Port 21\nPassivePorts 49152 49200\nInclude /etc/proftpd/conf.d/\n")
	b.write("/etc/proftpd/conf.d/sftp.conf",
		"<IfModule mod_sftp.c>\n<VirtualHost 0.0.0.0>\n  SFTPEngine on\n  Port 2222\n</VirtualHost>\n</IfModule>\n")
	got := map[string]Profile{}
	for _, p := range b.prov.Profiles() {
		got[p.Name] = p
	}
	if p := got["ssh"]; !p.Available || p.PortsSpec() != "2200/tcp|22/tcp" {
		t.Errorf("ssh : %+v — chaque Port s'ajoute, ceux d'un Match ne comptent pas", p)
	}
	if p := got["sftp"]; !p.Available || p.PortsSpec() != "2222/tcp" {
		t.Errorf("sftp : %+v", p)
	}
	if p := got["ftp"]; !p.Available || p.PortsSpec() != "21/tcp|49152:49200/tcp" || p.PassiveUnknown {
		t.Errorf("ftp : %+v", p)
	}
	if p := got["mysql"]; p.Available {
		t.Errorf("mysql n'est pas installé : %+v", p)
	}
	if p := got["web"]; !p.Available {
		t.Errorf("web suit le moteur retenu : %+v", p)
	}
}

func TestDetectionProftpdSFTPSeul(t *testing.T) {
	b := nouveauBanc(t, "{}", "proftpd")
	b.write("/etc/proftpd/proftpd.conf", "Port 0\n<VirtualHost 0.0.0.0>\nSFTPEngine on\nPort 2222\n</VirtualHost>\n")
	for _, p := range b.prov.Profiles() {
		if p.Name == "ftp" && p.Available {
			t.Errorf("« Port 0 » éteint le FTP du serveur principal : %+v", p)
		}
		if p.Name == "sftp" && !p.Available {
			t.Errorf("sftp attendu : %+v", p)
		}
	}
}

func TestDetectionVsftpdEtRedis(t *testing.T) {
	b := nouveauBanc(t, "{}", "vsftpd", "redis-server")
	b.write("/etc/vsftpd.conf", "listen=YES\npasv_min_port=40000\npasv_max_port=40100\n")
	b.write("/etc/redis/redis.conf", "bind 127.0.0.1\nport 0\nunixsocket /run/redis/redis.sock\n")
	for _, p := range b.prov.Profiles() {
		switch p.Name {
		case "ftp":
			if p.PortsSpec() != "21/tcp|40000:40100/tcp" {
				t.Errorf("ftp : %+v", p)
			}
		case "redis":
			if p.Available {
				t.Errorf("« port 0 » : redis n'écoute pas en TCP : %+v", p)
			}
		}
	}
}

func TestPlanNeufPuisIdempotent(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	steps := b.plan()
	d := diffs(t, steps)
	for _, want := range []string{
		"ufw prepend deny from 192.0.2.66 comment 'ogs: bannie'",
		"ufw allow from 203.0.113.10 to any app ogs-ssh comment 'ogs: confiance — bureau (Agence de Nantes)'",
		"ufw allow from 198.51.100.7 to any app ogs-mysql comment 'ogs: acme (ACME SARL)'",
		"ufw allow from any to any app ogs-web comment 'ogs: accès public à web'",
		"ufw --force enable",
		"--on-active=300s",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("plan sans %q :\n%s", want, d)
		}
	}
	if res := resources(steps); res[0] != "firewall/profiles" || res[len(res)-1] != "firewall/state" {
		t.Errorf("ordre : profils d'abord, activation en dernier : %v", res)
	}

	// Pare-feu réconcilié : fichier de profils écrit, règles posées, ufw actif.
	if _, err := engine.Execute(context.Background(), &engine.Plan{Steps: steps[:1]}, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	desired, err := b.prov.Desired()
	if err != nil {
		t.Fatal(err)
	}
	b.write("/etc/ufw/user.rules", rulesFile(desired, 4))
	b.write("/etc/ufw/user6.rules", rulesFile(desired, 6))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	if res := resources(b.plan()); len(res) != 0 {
		t.Errorf("un pare-feu conforme ne doit produire aucune étape : %v", res)
	}
}

func TestCommentaireCorrigeEnPlace(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	desired, _ := b.prov.Desired()
	for i := range desired {
		if desired[i].App == "ogs-mysql" && desired[i].From == "198.51.100.7" {
			desired[i].Comment = "ogs: ancien commentaire"
		}
	}
	b.write("/etc/ufw/user.rules", rulesFile(desired, 4))
	b.write("/etc/ufw/user6.rules", rulesFile(desired, 6))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	steps := b.plan()
	var mutating []*engine.Step
	for _, s := range steps {
		if s.Mutates() && s.Resource != "firewall/profiles" {
			mutating = append(mutating, s)
		}
	}
	if len(mutating) != 1 || mutating[0].Action != engine.Update {
		t.Fatalf("une mise à jour de commentaire attendue : %v", resources(mutating))
	}
	d := diffs(t, mutating)
	if strings.Contains(d, "delete") || strings.Contains(d, "prepend") {
		t.Errorf("ufw remplace un commentaire en place : ni retrait ni insertion :\n%s", d)
	}
}

func TestRegleOrphelineRetireeEtRegleEtrangereIntacte(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	desired, _ := b.prov.Desired()
	extra := "*filter\n" +
		tuple("allow any 3306 0.0.0.0/0 any 192.0.2.200 ogs-mysql - in", "ogs: ancien prestataire") +
		tuple("allow any 3306 0.0.0.0/0 any 192.0.2.201 ogs-mysql - in", "à moi")
	b.write("/etc/ufw/user.rules", strings.Replace(rulesFile(desired, 4), "*filter\n", extra, 1))
	b.write("/etc/ufw/user6.rules", rulesFile(desired, 6))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	steps := b.plan()
	d := diffs(t, steps)
	if !strings.Contains(d, "ufw delete allow from 192.0.2.200 to any app ogs-mysql") {
		t.Errorf("la règle d'ogs devenue sans objet doit être retirée :\n%s", d)
	}
	if strings.Contains(d, "192.0.2.201") {
		t.Errorf("une règle sans la marque d'ogs ne se touche pas :\n%s", d)
	}
}

func TestRefusReplaceEnTete(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	desired, _ := b.prov.Desired()
	var bans, others []Rule
	for _, r := range desired {
		if r.Kind == KindBan {
			bans = append(bans, r)
		} else {
			others = append(others, r)
		}
	}
	// Le refus relu après une autorisation : ufw retiendrait l'autorisation.
	b.write("/etc/ufw/user.rules", rulesFile(append(others, bans...), 4))
	b.write("/etc/ufw/user6.rules", rulesFile(desired, 6))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	d := diffs(t, b.plan())
	if !strings.Contains(d, "ufw delete deny from 192.0.2.66\nufw prepend deny from 192.0.2.66") {
		t.Errorf("un refus placé après une autorisation doit être replacé en tête :\n%s", d)
	}
}

func TestProfilDisparuRetireSesReglesAvantLeFichier(t *testing.T) {
	// ufw refuse une application qu'il ne connaît pas, jusque dans « ufw
	// delete » : la règle d'un profil retiré doit partir avant son profil.
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	b.write("/etc/ufw/applications.d/ogs", "[ogs-elastic]\ntitle=x\ndescription=x\nports=9200/tcp\n")
	b.write("/etc/ufw/user.rules", "*filter\n"+tuple("allow any 9200 0.0.0.0/0 any 10.1.1.1 ogs-elastic - in", "ogs: x"))
	res := resources(b.plan())
	if len(res) < 2 || !strings.Contains(res[0], "ogs-elastic") || res[1] != "firewall/profiles" {
		t.Errorf("retrait de la règle, puis fichier de profils : %v", res)
	}
}

func TestProfilEmployeParUneRegleEtrangereGarde(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	b.write("/etc/ufw/applications.d/ogs", "[ogs-elastic]\ntitle=x\ndescription=x\nports=9200/tcp\n")
	b.write("/etc/ufw/user.rules", "*filter\n"+tuple("allow any 9200 0.0.0.0/0 any 10.1.1.1 ogs-elastic - in", "à moi"))
	// Le fichier de l'essai n'a pas l'en-tête d'ogs : sans --force, il serait
	// refusé comme étranger — ce qui est le comportement voulu, non l'objet ici.
	b.prov.Force = true
	var body string
	for _, s := range b.plan() {
		if s.Resource == "firewall/profiles" {
			body, _ = s.Diff()
		}
	}
	if !strings.Contains(body, "+description=Profil retiré d’ufw.yaml, gardé tant qu’une règle l’emploie") {
		t.Errorf("un profil qu'une règle de l'administrateur emploie reste défini :\n%s", body)
	}
}

// session fixe la session d'administration du banc.
func session(addr, port string) func() (Session, bool) {
	return func() (Session, bool) { return Session{Addr: netip.MustParseAddr(addr), Port: port}, true }
}

func TestGardeSessionAdministration(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	b.prov.Admin = session("192.0.2.50", "")
	_, err := b.planErr()
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "192.0.2.50") {
		t.Fatalf("une activation qui couperait la session doit être refusée : %v", err)
	}
	b.prov.AllowLockout = true
	if _, err := b.planErr(); err != nil {
		t.Errorf("--allow-lockout passe outre la garde : %v", err)
	}

	b.prov.AllowLockout = false
	b.prov.Admin = session("203.0.113.10", "")
	if _, err := b.planErr(); err != nil {
		t.Errorf("une session couverte par la confiance est acceptée : %v", err)
	}
	b.prov.Admin = session("192.0.2.66", "")
	if _, err := b.planErr(); err == nil || !strings.Contains(err.Error(), "bannirait") {
		t.Errorf("bannir l'adresse de la session doit être refusé : %v", err)
	}
}

// Le --force des fichiers retouchés (§6.4) se donne pour un vhost ou une
// rotation, sans penser au pare-feu : il levait pourtant les gardes contre la
// coupure dans toute commande qui exécute le plan global. Ce sont deux
// décisions distinctes.
func TestForceNeLevePasLesGardesContreLaCoupure(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	b.prov.Force = true
	b.prov.Admin = session("192.0.2.50", "")
	if _, err := b.planErr(); err == nil || !strings.Contains(err.Error(), "192.0.2.50") {
		t.Errorf("--force ne doit pas lever la garde de la session : %v", err)
	}
	b.prov.Admin = session("192.0.2.66", "")
	if _, err := b.planErr(); err == nil || !strings.Contains(err.Error(), "bannirait") {
		t.Errorf("--force ne doit pas lever la garde du bannissement : %v", err)
	}
	b.prov.Admin = nil
	b.prov.Cfg.Trusted = nil
	_, err := b.planErr()
	if err == nil || !strings.Contains(err.Error(), "ssh : aucune source") {
		t.Fatalf("--force ne doit pas lever la garde de SSH fermé à tous : %v", err)
	}
	if !strings.Contains(err.Error(), "--allow-lockout") || strings.Contains(err.Error(), "--force") {
		t.Errorf("le refus doit nommer la seule option qui le lève :\n%v", err)
	}
}

func TestGardeSSHSansSource(t *testing.T) {
	b := nouveauBanc(t, "profiles:\n  web: {allow: [any]}\n", "ssh")
	if _, err := b.planErr(); err == nil || !strings.Contains(err.Error(), "ssh : aucune source") {
		t.Errorf("une politique entrante fermée sans accès SSH doit être refusée : %v", err)
	}
}

func TestGardeHTTP01(t *testing.T) {
	b := nouveauBanc(t, declarationType+"", "ssh", "mariadb")
	b.prov.Cfg.Profiles["web"] = config.UFWProfile{Allow: []config.UFWEntry{{Source: "bureau"}}}
	proj := &model.Project{}
	proj.Spec.Certificates = []model.Certificate{{Name: "shop", Domains: []string{"shop.fr"}}}
	if _, err := b.planErr(proj); err == nil || !strings.Contains(err.Error(), "HTTP-01") {
		t.Errorf("un projet en HTTP-01 derrière un web fermé doit être refusé : %v", err)
	}
}

func TestGardeFTPSansPlagePassive(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb", "proftpd")
	b.write("/etc/proftpd/proftpd.conf", "Port 21\n")
	if _, err := b.planErr(); err == nil || !strings.Contains(err.Error(), "ports passifs") {
		t.Errorf("un FTP ouvert sans plage passive doit être refusé : %v", err)
	}
	b.prov.Cfg.Profiles["ftp"] = config.UFWProfile{Passive: "40000:40100"}
	if _, err := b.planErr(); err != nil {
		t.Errorf("la plage déclarée lève le refus : %v", err)
	}
}

func TestActivationRefuseeApresUneRegleEnEchec(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	b.rec.Errors["ufw allow"] = errors.New("échec simulé")
	steps := b.plan()
	report, err := engine.Execute(context.Background(), &engine.Plan{Steps: steps}, engine.Options{})
	if err != nil {
		t.Fatalf("les étapes du pare-feu sont non critiques : %v", err)
	}
	for _, o := range report.Outcomes {
		if o.Step.Resource == "firewall/state" {
			if o.Err == nil || !strings.Contains(o.Err.Error(), "n'ont pas pu être posées") {
				t.Errorf("l'activation doit refuser après une règle en échec : %v", o.Err)
			}
		}
	}
	if b.rec.Contains("--force enable") {
		t.Error("ufw ne doit pas avoir été activé")
	}
}

func TestSortiesSousPolitiqueFermee(t *testing.T) {
	b := nouveauBanc(t, declarationType+"policy:\n  outgoing: deny\noutgoing:\n  allow: [dns, https, 5432/tcp]\n", "ssh", "mariadb")
	d := diffs(t, b.plan())
	for _, want := range []string{"ufw allow out 53 comment", "ufw allow out 443/tcp comment", "ufw allow out 5432/tcp",
		"ufw default deny outgoing"} {
		if !strings.Contains(d, want) {
			t.Errorf("plan sans %q :\n%s", want, d)
		}
	}
}

func TestIgnoreIPReprendLaConfiance(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh")
	if got, err := b.prov.IgnoreIP(); err != nil || len(got) != 1 || got[0] != "203.0.113.10" {
		t.Errorf("adresses de confiance pour fail2ban : %v %v", got, err)
	}
	// Une déclaration qui ne se lit pas ne rend pas une liste vide : l'appelant
	// garderait sinon l'ignoreip sans l'adresse d'administration (D109).
	b.prov.Cfg, b.prov.LoadErr = nil, errors.New("ufw.yaml invalide")
	if got, err := b.prov.IgnoreIP(); err == nil {
		t.Errorf("ufw.yaml invalide : une erreur est attendue, obtenu %v", got)
	}
}

func TestSansDeclarationRienNestFait(t *testing.T) {
	p := &Provider{Paths: config.NewPaths(t.TempDir()), Exec: exec.NewRecorder()}
	steps, err := p.Plan(context.Background(), nil, Observed{Installed: true})
	if err != nil || len(steps) != 0 {
		t.Errorf("sans ufw.yaml, ogs ne touche pas à ufw : %v %v", steps, err)
	}
}

func TestUFWAbsentAvertit(t *testing.T) {
	b := nouveauBanc(t, declarationType)
	steps, err := b.prov.Plan(context.Background(), nil, Observed{})
	if err != nil || len(steps) != 1 || steps[0].Critical {
		t.Fatalf("un avertissement non bloquant attendu : %v %v", steps, err)
	}
	if err := steps[0].Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "ufw est absent") {
		t.Errorf("message : %v", err)
	}
}

func TestPortsEcoutes(t *testing.T) {
	listeners := []string{"0.0.0.0:22", "127.0.0.1:3306", "[::1]:6379", "*:80"}
	if pub, _ := listening([]string{"22/tcp"}, listeners); !pub {
		t.Error("22 écoute publiquement")
	}
	if pub, lo := listening([]string{"3306/tcp"}, listeners); pub || !lo {
		t.Error("3306 n'écoute que la boucle locale")
	}
	if pub, lo := listening([]string{"80,443/tcp"}, listeners); !pub || lo {
		t.Error("80 écoute publiquement")
	}
	if pub, lo := listening([]string{"2222/tcp"}, listeners); pub || lo {
		t.Error("rien n'écoute sur 2222")
	}
}

func TestDoctorNeSignaleLEcouteQueDesProfilsOuverts(t *testing.T) {
	// La confiance ouvre chaque profil disponible : MariaDB n'écoutant que la
	// boucle locale n'est pas une anomalie tant que rien ne l'ouvre explicitement.
	b := nouveauBanc(t, "sources:\n  bureau: {addresses: [203.0.113.10]}\ntrusted: [bureau]\n", "ssh", "mariadb")
	obs := Observed{Installed: true, Enabled: true}
	listeners := []string{"127.0.0.1:3306"}
	var got []string
	for _, f := range b.prov.Checks(obs, listeners) {
		got = append(got, f.Name+" : "+f.Detail)
	}
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "pare-feu : mysql") {
		t.Errorf("mysql ouvert par la seule confiance ne doit rien signaler :\n%s", joined)
	}
	if !strings.Contains(joined, "pare-feu : ssh : rien n'écoute sur 22/tcp") {
		t.Errorf("ssh est toujours éprouvé — un sshd déplacé coupe l'accès :\n%s", joined)
	}

	b.prov.Cfg.Profiles = map[string]config.UFWProfile{"mysql": {Allow: []config.UFWEntry{{Address: "198.51.100.7"}}}}
	got = nil
	for _, f := range b.prov.Checks(obs, listeners) {
		got = append(got, f.Name+" : "+f.Detail)
	}
	if joined := strings.Join(got, "\n"); !strings.Contains(joined, "pare-feu : mysql : 3306/tcp n'écoute que sur la boucle locale") {
		t.Errorf("mysql ouvert explicitement sur un service en boucle locale doit être signalé :\n%s", joined)
	}
}

func TestRetraitRefuseSiLaRegleDeRemplacementEchoue(t *testing.T) {
	// ssh passait à tous par une règle d'ogs ; la déclaration le restreint à une
	// adresse. Si la nouvelle règle échoue, l'ancienne ne doit pas partir : le
	// changement déplacerait l'accès nulle part.
	b := nouveauBanc(t, "profiles:\n  ssh: {allow: [192.0.2.77]}\n", "ssh")
	b.write("/etc/ufw/user.rules", "*filter\n"+
		tuple("allow any 22 0.0.0.0/0 any 0.0.0.0/0 ogs-ssh - in", "ogs: accès public à ssh"))
	b.write("/etc/ufw/user6.rules", "*filter\n"+
		tuple("allow any 22 ::/0 any ::/0 ogs-ssh - in", "ogs: accès public à ssh"))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	b.rec.Errors["ufw allow"] = errors.New("échec simulé")
	if _, err := engine.Execute(context.Background(), &engine.Plan{Steps: b.plan()}, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	if b.rec.Contains("ufw delete allow from any") {
		t.Error("l'accès existant ne doit pas être retiré quand son remplaçant a échoué")
	}
}

func TestRegleEtrangereDeMemeDefinitionReste(t *testing.T) {
	// L'administrateur bannit déjà 192.0.2.66, sans commentaire. ufw.yaml le
	// demande aussi : la règle suffit, et reste la sienne — lui donner la marque
	// d'ogs la ferait retirer le jour où ufw.yaml cesserait de la demander.
	b := nouveauBanc(t, "ban: [192.0.2.66]\npolicy: {incoming: allow}\n")
	b.write("/etc/ufw/user.rules", "*filter\n"+tuple("deny any any 0.0.0.0/0 any 192.0.2.66 in", ""))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	b.write("/etc/default/ufw", "IPV6=yes\nDEFAULT_INPUT_POLICY=\"ACCEPT\"\nDEFAULT_OUTPUT_POLICY=\"ACCEPT\"\n")
	for _, s := range b.plan() {
		if strings.HasPrefix(s.Resource, "firewall/rule/") && s.Mutates() {
			t.Errorf("la règle de l'administrateur ne doit pas être reprise : %s", s.Summary)
		}
	}
}

func TestProfilGardeSiLeRetraitDeSaRegleEchoue(t *testing.T) {
	// Sans cela, le profil disparaît du fichier alors que la règle reste, et ufw
	// refuse ensuite de la retirer : l'accès d'un ancien prestataire resterait
	// ouvert pour toujours.
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	b.write("/etc/ufw/applications.d/ogs", "[ogs-elastic]\ntitle=x\ndescription=x\nports=9200/tcp\n")
	b.write("/etc/ufw/user.rules", "*filter\n"+tuple("allow any 9200 0.0.0.0/0 any 10.1.1.1 ogs-elastic - in", "ogs: x"))
	b.prov.Force = true
	b.rec.Errors["ufw delete"] = errors.New("échec simulé")
	if _, err := engine.Execute(context.Background(), &engine.Plan{Steps: b.plan()}, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(b.root, "etc/ufw/applications.d/ogs"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[ogs-elastic]") {
		t.Errorf("le profil doit rester défini tant que sa règle n'est pas retirée :\n%s", data)
	}
}

func TestPolitiqueQuiSOuvrePasseAvantLesRetraits(t *testing.T) {
	// Passer la sortie de deny à allow retire les sorties permises : les retirer
	// avant d'ouvrir couperait DNS et ACME le temps de l'apply.
	b := nouveauBanc(t, "policy: {incoming: allow, outgoing: allow}\n")
	b.write("/etc/ufw/user.rules", "*filter\n"+tuple("allow any 53 0.0.0.0/0 any 0.0.0.0/0 out", "ogs: sortie dns"))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	b.write("/etc/default/ufw", "IPV6=no\nDEFAULT_INPUT_POLICY=\"ACCEPT\"\nDEFAULT_OUTPUT_POLICY=\"DROP\"\n")
	res := resources(b.plan())
	pol, del := -1, -1
	for i, r := range res {
		switch {
		case r == "firewall/policy/outgoing":
			pol = i
		case strings.HasPrefix(r, "firewall/rule/allow|out"):
			del = i
		}
	}
	if pol < 0 || del < 0 || pol > del {
		t.Errorf("la politique qui s'ouvre doit précéder le retrait : %v", res)
	}
}

func TestPortsSSHSyntaxesAdmises(t *testing.T) {
	b := nouveauBanc(t, "{}", "ssh")
	b.write("/etc/ssh/sshd_config", "Port=2222\nListenAddress 0.0.0.0:2201\nListenAddress [::]:2202\nListenAddress ::1\n")
	for _, p := range b.prov.Profiles() {
		if p.Name == "ssh" && p.PortsSpec() != "2222/tcp|2201/tcp|2202/tcp" {
			t.Errorf("ssh : %s", p.PortsSpec())
		}
	}
}

func TestPortsSSHEcoutes(t *testing.T) {
	out := `LISTEN 0 128 0.0.0.0:2222 0.0.0.0:* users:(("sshd",pid=156,fd=6))
LISTEN 0 128 [::]:2222 [::]:* users:(("sshd",pid=156,fd=7))
LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("apache2",pid=9,fd=4))
ESTAB 0 0 10.0.0.1:22 10.0.0.9:5000 users:(("sshd-session",pid=300,fd=4))`
	if got := sshdPortsFromSS(out); len(got) != 1 || got[0] != "2222" {
		t.Errorf("ports de sshd : %v", got)
	}
}

// Sous ssh.socket, systemd partage le descripteur d'écoute avec sshd, et ss
// peut le nommer le premier.
func TestPortsSSHEcoutesSystemdNomméLePremier(t *testing.T) {
	out := `LISTEN 0 4096 0.0.0.0:22 0.0.0.0:* users:(("systemd",pid=1,fd=137),("sshd",pid=812,fd=3))`
	if got := sshdPortsFromSS(out); len(got) != 1 || got[0] != "22" {
		t.Errorf("ports de sshd : %v", got)
	}
}

// ssReel se déclare non simulé : sshdListening se tait sur un exécuteur simulé.
type ssReel struct{ *exec.Recorder }

func (s *ssReel) Simulated() bool { return false }

// Avant la première connexion, seul systemd tient le port d'une ssh.socket : ss
// n'y montre aucun sshd, et la garde ne voyait rien.
func TestPortsSSHEcoutesParSocketSystemd(t *testing.T) {
	b := nouveauBanc(t, "{}", "ssh")
	b.rec.Responses["ss -Htlnp"] = exec.Result{Stdout: `LISTEN 0 4096 0.0.0.0:2222 0.0.0.0:* users:(("systemd",pid=1,fd=137))
LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("apache2",pid=9,fd=4))`}
	b.rec.Responses["systemctl show"] = exec.Result{Stdout: "0.0.0.0:2222 (Stream)\n[::]:2222 (Stream)\n"}
	b.prov.Exec = &ssReel{b.rec}
	if got := b.prov.sshdListening(context.Background()); len(got) != 1 || got[0] != "2222" {
		t.Errorf("ports de ssh.socket : %v", got)
	}
}

// Une ssh.socket inactive n'écoute pas : ses ListenStream ne disent rien des
// ports d'un sshd qui tourne seul.
func TestPortsSSHSocketInactiveIgnorée(t *testing.T) {
	b := nouveauBanc(t, "{}", "ssh")
	b.rec.Responses["ss -Htlnp"] = exec.Result{Stdout: `LISTEN 0 128 0.0.0.0:2222 0.0.0.0:* users:(("sshd",pid=156,fd=6))`}
	b.rec.Errors["systemctl is-active"] = errors.New("inactive")
	b.rec.Responses["systemctl show"] = exec.Result{Stdout: "[::]:22 (Stream)\n"}
	b.prov.Exec = &ssReel{b.rec}
	if got := b.prov.sshdListening(context.Background()); len(got) != 1 || got[0] != "2222" {
		t.Errorf("seuls les ports de sshd comptent : %v", got)
	}
}

// reconcilier met le banc dans l'état d'un pare-feu réconcilié et actif :
// fichier de profils écrit, règles posées, ufw actif. Le plan qui suit est vide.
func (b *banc) reconcilier(projects ...*model.Project) {
	b.t.Helper()
	for _, s := range b.plan(projects...) {
		if s.Resource != "firewall/profiles" {
			continue
		}
		if _, err := engine.Execute(context.Background(), &engine.Plan{Steps: []*engine.Step{s}},
			engine.Options{}); err != nil {
			b.t.Fatal(err)
		}
	}
	desired, err := b.prov.Desired()
	if err != nil {
		b.t.Fatal(err)
	}
	b.write("/etc/ufw/user.rules", rulesFile(desired, 4))
	b.write("/etc/ufw/user6.rules", rulesFile(desired, 6))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	if res := resources(b.plan(projects...)); len(res) != 0 {
		b.t.Fatalf("état réconcilié non conforme : %v", res)
	}
}

func (b *banc) profil(name string) string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.root, "etc/ufw/applications.d/ogs"))
	if err != nil {
		b.t.Fatal(err)
	}
	return parseProfiles(data)[name]
}

// sshdSur2222 prépare un sshd déplacé sur 2222, qu'une session de confiance
// administre. ss le montre à l'écoute ; aucune ssh.socket.
func sshdSur2222(t *testing.T) *banc {
	b := nouveauBanc(t, "sources:\n  bureau:\n    owner: \"Bureau\"\n    addresses: [10.200.0.2]\n"+
		"trusted: [bureau]\npolicy:\n  incoming: deny\n", "ssh")
	b.write("/etc/ssh/sshd_config", "Include /etc/ssh/sshd_config.d/*.conf\n")
	b.write("/etc/ssh/sshd_config.d/essai.conf", "Port 2222\n")
	b.prov.Admin = session("10.200.0.2", "")
	b.rec.Responses["ss -Htlnp"] = exec.Result{Stdout: `LISTEN 0 128 0.0.0.0:2222 0.0.0.0:* users:(("sshd",pid=156,fd=6))
LISTEN 0 128 [::]:2222 [::]:* users:(("sshd",pid=156,fd=7))`}
	b.rec.Errors["systemctl is-active"] = errors.New("inactive")
	b.prov.Exec = &ssReel{b.rec}
	b.reconcilier()
	if got := b.profil("ogs-ssh"); got != "2222/tcp" {
		t.Fatalf("état initial : ogs-ssh = %s", got)
	}
	return b
}

// sshd admet un commentaire en fin de ligne : « Port 2222 # déplacé » relu tel
// quel faisait retomber le profil au port 22, et « ufw app update ogs-ssh »
// fermait 2222, où sshd écoutait toujours.
func TestPortSSHAnnotéGardeSonPort(t *testing.T) {
	b := sshdSur2222(t)
	b.write("/etc/ssh/sshd_config.d/essai.conf", "Port 2222 # déplacé le 12/09\n"+
		"ListenAddress 0.0.0.0:2201\t# bastion\nPort 2200#collé\n")
	for _, p := range b.prov.Profiles() {
		if p.Name == "ssh" && p.PortsSpec() != "2222/tcp|2201/tcp" {
			t.Errorf("ssh : %s — le commentaire de fin de ligne ne fait pas partie de la valeur, "+
				"un « # » collé au mot si", p.PortsSpec())
		}
	}
	b.write("/etc/ssh/sshd_config.d/essai.conf", "Port 2222 # déplacé le 12/09\n")
	if res := resources(b.plan()); len(res) != 0 {
		t.Errorf("une annotation ne change rien au pare-feu : %v", res)
	}
	if b.rec.Contains("ufw app update ogs-ssh") {
		t.Error("le profil ssh ne doit pas être relu par ufw")
	}
}

// Changer les ports d'un profil employé ferme l'ancien port : c'est un
// resserrement, que la garde des ports où sshd écoute doit voir comme un
// retrait. Elle ne jouait que sur un retrait de règle ou une activation.
func TestChangementDePortsDuProfilSSHGardé(t *testing.T) {
	b := sshdSur2222(t)
	b.prov.Cfg.Profiles = map[string]config.UFWProfile{"ssh": {Ports: []string{"22/tcp"}}}
	_, err := b.planErr()
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "sshd écoute sur le port 2222") {
		t.Fatalf("des ports ssh qui ferment le port où sshd écoute doivent être refusés : %v", err)
	}
	b.prov.AllowLockout = true
	steps, err := b.planErr()
	if err != nil {
		t.Fatalf("--allow-lockout passe outre : %v", err)
	}
	if res := resources(steps); len(res) != 1 || res[0] != "firewall/profiles" {
		t.Errorf("seul le fichier de profils change : %v", res)
	}
}

// Là où ss se tait — mode simulé, ss absent —, la session porte le port : un
// profil ssh qui ne l'ouvre plus couperait la suivante. La garde ne comparait
// que l'adresse.
func TestGardeSessionContrôleLePortDuServeur(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	b.write("/etc/ssh/sshd_config", "Port 2222\n")
	b.prov.Admin = session("203.0.113.10", "2222")
	b.reconcilier()

	b.prov.Cfg.Profiles["ssh"] = config.UFWProfile{Ports: []string{"22/tcp"}}
	_, err := b.planErr()
	if err == nil || !strings.Contains(err.Error(), "arrivée sur le port 2222") {
		t.Fatalf("un profil ssh qui n'ouvre plus le port de la session doit être refusé : %v", err)
	}
	b.prov.Cfg.Profiles["ssh"] = config.UFWProfile{Ports: []string{"22/tcp", "2200:2300/tcp"}}
	if _, err := b.planErr(); err != nil {
		t.Errorf("une plage qui couvre le port de la session le laisse passer : %v", err)
	}
}

func TestSessionSSHLitLePortDuServeur(t *testing.T) {
	s, ok := parseSSHConnection("::ffff:203.0.113.10 51234 ::ffff:10.0.0.1 2222")
	if !ok || s.Addr != netip.MustParseAddr("203.0.113.10") || s.Port != "2222" ||
		s.Local != netip.MustParseAddr("10.0.0.1") {
		t.Errorf("session : %+v", s)
	}
	if s, ok := parseSSHConnection("203.0.113.10"); !ok || s.Port != "" {
		t.Errorf("sans port, l'adresse suffit : %+v", s)
	}
}

func TestPortsOuvertsParUnProfil(t *testing.T) {
	for _, c := range []struct {
		specs []string
		port  string
		want  bool
	}{
		{[]string{"22/tcp"}, "22", true},
		{[]string{"2200/tcp", "22/tcp"}, "22", true},
		{[]string{"22,2222/tcp"}, "2222", true},
		{[]string{"2200:2300/tcp"}, "2222", true},
		{[]string{"2200:2300/tcp"}, "22", false},
		{[]string{"2222/udp"}, "2222", false},
		{[]string{"2222"}, "2222", true},
	} {
		if got := opens(c.specs, c.port); got != c.want {
			t.Errorf("opens(%v, %s) = %v", c.specs, c.port, got)
		}
	}
}

// Deux moteurs sans web.engine : une inconnue, non l'absence de serveur web.
// Le profil web tenu pour absent voyait ses règles retirées au premier
// « ogs firewall ban » — l'accès public de tous les sites, et les validations
// HTTP-01, avec lui.
func TestMoteurWebIndéterminéGardeLesRèglesDuWeb(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	proj := &model.Project{}
	proj.Spec.Certificates = []model.Certificate{{Name: "shop", Domains: []string{"shop.fr"}}}
	b.reconcilier(proj)

	b.prov.WebUnit, b.prov.WebUndecided = "", true
	b.prov.Cfg.Ban = append(b.prov.Cfg.Ban, config.ParseUFWEntry("198.51.100.9"))
	steps, err := b.planErr(proj)
	if err != nil {
		t.Fatalf("bannir une adresse ne dépend pas du moteur web : %v", err)
	}
	if d := diffs(t, steps); strings.Contains(d, "ogs-web") {
		t.Errorf("les règles du web ne doivent pas partir :\n%s", d)
	}
	for _, p := range b.prov.Profiles() {
		if p.Name == "web" && !p.Available {
			t.Errorf("un serveur web installé reste un serveur web : %+v", p)
		}
	}
}

// La garde HTTP-01 porte sur ce que le plan laisse ouvert, non sur la seule
// disponibilité détectée : un profil web indisponible retire ses règles.
func TestGardeHTTP01SansProfilWeb(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	proj := &model.Project{}
	proj.Spec.Certificates = []model.Certificate{{Name: "shop", Domains: []string{"shop.fr"}}}
	b.reconcilier(proj)

	// Écarté dans ufw.yaml, le refus nomme le forçage : « forcer
	// profiles.web.available » ne disait rien à qui l'avait déjà forcé à faux.
	faux, web := false, b.prov.Cfg.Profiles["web"]
	b.prov.Cfg.Profiles["web"] = config.UFWProfile{Available: &faux, Allow: web.Allow}
	if _, err := b.planErr(proj); err == nil || !strings.Contains(err.Error(), "« profiles.web.available: false »") {
		t.Errorf("un web écarté sous un projet en HTTP-01 doit être refusé en nommant le forçage : %v", err)
	}
	b.prov.Cfg.Profiles["web"] = web

	b.prov.WebUnit = ""
	if _, err := b.planErr(proj); err == nil || !strings.Contains(err.Error(), "HTTP-01") {
		t.Errorf("retirer l'accès public au web sous un projet en HTTP-01 doit être refusé : %v", err)
	}
	// Sans projet en HTTP-01, un web absent perd ses règles : rien n'y écoute.
	steps, err := b.planErr()
	if err != nil {
		t.Fatal(err)
	}
	if d := diffs(t, steps); !strings.Contains(d, "ufw delete allow from any to any app ogs-web") {
		t.Errorf("les règles d'un profil web indisponible partent :\n%s", d)
	}
}

// sshdÉcoute fait montrer par ss un sshd à l'écoute sur ces ports, sans
// ssh.socket ; sans port, ss se tait.
func (b *banc) sshdÉcoute(ports ...string) {
	var lines []string
	for _, p := range ports {
		lines = append(lines, fmt.Sprintf(`LISTEN 0 128 0.0.0.0:%s 0.0.0.0:* users:(("sshd",pid=156,fd=6))`, p))
	}
	b.rec.Responses["ss -Htlnp"] = exec.Result{Stdout: strings.Join(lines, "\n")}
	b.rec.Errors["systemctl is-active"] = errors.New("inactive")
	b.prov.Exec = &ssReel{b.rec}
}

// sansSession tient lieu de console : aucune session SSH à protéger — et
// DetectAdmin ne va pas lire celle du poste qui lance les tests.
func sansSession() (Session, bool) { return Session{}, false }

func (b *banc) lire(path string) string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.root, path))
	if err != nil {
		b.t.Fatal(err)
	}
	return string(data)
}

// sshd déplacé de 22 à 2200 et redémarré : la session en cours, arrivée sur 22,
// survit, mais ss ne montre plus sshd que sur 2200 — 22 comme 2200 sont déjà
// fermés, le profil valant encore 22. Le plan qui porte le profil sur 2200 rend
// l'accès ; la garde, qui comptait le port de la session alors que ss disait
// sshd parti, le refusait, et seul --allow-lockout réparait.
func TestGardeSessionIgnoreUnPortQueSshdAQuitté(t *testing.T) {
	b := nouveauBanc(t, "sources:\n  bureau:\n    addresses: [10.200.0.2]\ntrusted: [bureau]\n"+
		"policy:\n  incoming: deny\n", "ssh")
	b.write("/etc/ssh/sshd_config", "Port 22\n")
	b.prov.Admin = session("10.200.0.2", "22")
	b.sshdÉcoute("22")
	b.reconcilier()

	b.write("/etc/ssh/sshd_config", "Port 2200\n")
	// Là où ss se tait, le port de la session est le seul indice : sshd n'a
	// peut-être pas été redémarré, et fermer 22 couperait la suivante.
	b.sshdÉcoute()
	if _, err := b.planErr(); err == nil || !strings.Contains(err.Error(), "arrivée sur le port 22") {
		t.Fatalf("ss muet : le port de la session doit rester gardé : %v", err)
	}

	b.sshdÉcoute("2200")
	steps, err := b.planErr()
	if err != nil {
		t.Fatalf("le plan qui ouvre le port où sshd écoute désormais doit passer : %v", err)
	}
	if _, err := engine.Execute(context.Background(), &engine.Plan{Steps: steps}, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := b.profil("ogs-ssh"); got != "2200/tcp" || !b.rec.Contains("ufw app update ogs-ssh") {
		t.Errorf("le profil ssh doit suivre sshd sur 2200 : %s", got)
	}
}

// Un profil ssh écarté (profiles.ssh.available: false), ou qu'aucune unité ne
// signale, voit ses règles retirées. La garde des ports où sshd écoute exigeait
// un profil disponible : depuis la console, sans session à protéger, rien ne
// refusait de fermer 22 et 2200 à tous.
func TestGardePortsDeSshdSansProfilSSH(t *testing.T) {
	b := nouveauBanc(t, "sources:\n  bureau:\n    addresses: [10.200.0.2]\ntrusted: [bureau]\n"+
		"policy:\n  incoming: deny\n", "ssh")
	b.write("/etc/ssh/sshd_config", "Port 22\nPort 2200\n")
	b.prov.Admin = sansSession
	b.sshdÉcoute("22", "2200")
	b.reconcilier()
	desired, err := b.prov.Desired()
	if err != nil {
		t.Fatal(err)
	}

	faux := false
	b.prov.Cfg.Profiles = map[string]config.UFWProfile{"ssh": {Available: &faux}}
	_, err = b.planErr()
	if err == nil || !strings.Contains(err.Error(), "sshd écoute sur le port 22, que plus rien n'ouvrirait") ||
		!strings.Contains(err.Error(), "port 2200") || !strings.Contains(err.Error(), "écarté dans ufw.yaml") {
		t.Fatalf("un profil ssh écarté ne doit pas fermer les ports où sshd écoute : %v", err)
	}
	b.prov.AllowLockout = true
	if _, err := b.planErr(); err != nil {
		t.Errorf("--allow-lockout passe outre : %v", err)
	}
	b.prov.AllowLockout = false

	b.prov.Cfg.Profiles = nil
	if err := os.Remove(filepath.Join(b.root, "lib/systemd/system/ssh.service")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.planErr(); err == nil || !strings.Contains(err.Error(), "aucune unité ssh") {
		t.Errorf("un profil ssh que rien ne détecte ne doit pas fermer les ports où sshd écoute : %v", err)
	}

	// Qui ouvre ssh par ses propres règles n'est pas refusé : ogs ne les
	// touche pas, et elles laissent passer après le plan.
	b.write("/etc/ufw/user.rules", strings.Replace(rulesFile(desired, 4), "*filter\n", "*filter\n"+
		tuple("limit tcp 22 0.0.0.0/0 any 0.0.0.0/0 in", "")+
		tuple("allow tcp 2200 0.0.0.0/0 any 10.200.0.0/24 in", "poste de secours"), 1))
	steps, err := b.planErr()
	if err != nil {
		t.Fatalf("les règles de l'administrateur ouvrent les ports de sshd : %v", err)
	}
	if d := diffs(t, steps); !strings.Contains(d, "ufw delete allow from 10.200.0.2 to any app ogs-ssh") {
		t.Errorf("les règles d'ogs du profil indisponible partent :\n%s", d)
	}
}

// Les gardes ne voyaient que les règles voulues : un « ufw limit OpenSSH » posé
// à la main faisait refuser « ssh : aucune source » à chaque apply, et une
// session que seule une règle de l'administrateur admet était refusée au
// moindre changement — des ports de MariaDB, par exemple.
func TestGardesComptentLesRèglesDeLAdministrateur(t *testing.T) {
	b := nouveauBanc(t, "profiles:\n  web: {allow: [any]}\n", "ssh")
	for _, c := range []struct {
		regle string
		admis bool
	}{
		{"limit tcp 22 0.0.0.0/0 any 0.0.0.0/0 OpenSSH - in", true},
		{"allow_log tcp 22 0.0.0.0/0 any 192.0.2.0/24 in", true},
		{"allow udp 22 0.0.0.0/0 any 0.0.0.0/0 in", false},
		{"allow tcp 80 0.0.0.0/0 any 0.0.0.0/0 in", false},
		// SSH par une interface, ou vers une adresse du serveur : un accès.
		{"allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 in_eth0", true},
		{"allow tcp 22 10.8.0.1 any 0.0.0.0/0 in", true},
		{"deny tcp 22 0.0.0.0/0 any 0.0.0.0/0 in", false},
	} {
		b.write("/etc/ufw/user.rules", "*filter\n"+tuple(c.regle, "")+"COMMIT\n")
		_, err := b.planErr()
		if refusé := err != nil && strings.Contains(err.Error(), "ssh : aucune source"); refusé == c.admis {
			t.Errorf("« %s » : source ssh %v attendue, obtenu %v", c.regle, c.admis, err)
		}
	}

	b = nouveauBanc(t, declarationType, "ssh", "mariadb")
	b.prov.Admin = session("192.0.2.50", "22")
	b.write("/etc/ufw/user.rules", "*filter\n"+tuple("allow tcp 22 0.0.0.0/0 any 198.51.100.0/24 in", "")+"COMMIT\n")
	if _, err := b.planErr(); err == nil || !strings.Contains(err.Error(), "192.0.2.50, qu'aucune règle du profil ssh") {
		t.Fatalf("une règle de l'administrateur qui n'admet pas la session ne la couvre pas : %v", err)
	}
	b.write("/etc/ufw/user.rules", "*filter\n"+tuple("allow tcp 22 0.0.0.0/0 any 192.0.2.0/24 in", "")+"COMMIT\n")
	if _, err := b.planErr(); err != nil {
		t.Fatalf("une session admise par une règle de l'administrateur n'est pas coupée : %v", err)
	}

	// Pare-feu réconcilié, la session admise par l'administrateur seul : un
	// changement de port de MariaDB resserre le pare-feu, et la garde joue.
	b.prov.Admin = sansSession
	b.reconcilier()
	b.prov.Admin = session("192.0.2.50", "")
	b.write("/etc/ufw/user.rules", strings.Replace(b.lire("/etc/ufw/user.rules"), "*filter\n", "*filter\n"+
		tuple("allow tcp 22 0.0.0.0/0 any 192.0.2.0/24 in", ""), 1))
	b.prov.Cfg.Profiles["mysql"] = config.UFWProfile{Ports: []string{"3307/tcp"},
		Allow: b.prov.Cfg.Profiles["mysql"].Allow}
	steps, err := b.planErr()
	if err != nil {
		t.Fatalf("changer le port de MariaDB ne coupe pas une session admise par l'administrateur : %v", err)
	}
	if d := diffs(t, steps); !strings.Contains(d, "+ports=3307/tcp") {
		t.Errorf("le port de MariaDB doit changer :\n%s", d)
	}
}

// Ce qu'une règle laissée en place ouvre : celle de l'administrateur vaut par
// ses propres ports ; une règle d'ogs reconnue est voulue, ou retirée. Sans
// session, une règle liée à une interface ou à une adresse du serveur ouvre le
// port du dehors ; pour une session, l'adresse du serveur doit être celle
// qu'elle a jointe, et l'interface ne compte pas.
func TestRèglesLaisséesEnPlace(t *testing.T) {
	v4 := parseRules([]byte("*filter\n"+
		tuple("allow any 22 0.0.0.0/0 any 0.0.0.0/0 ogs-ssh - in", "ogs: accès public à ssh")+
		tuple("allow any any 0.0.0.0/0 any 192.0.2.0/24 in", "")+
		tuple("allow tcp 2200:2300 0.0.0.0/0 any 198.51.100.7 in", "")+
		tuple("allow tcp 22 10.0.0.1 any 0.0.0.0/0 in", "")+
		tuple("allow tcp 2022 0.0.0.0/0 any 0.0.0.0/0 in_wg0", "")+
		tuple("allow any 22 0.0.0.0/0 any 203.0.113.0/24 ogs-sftp - in", "à moi")), 4)
	v6 := parseRules([]byte("*filter\n"+tuple("allow tcp 22 ::/0 any ::/0 in", "")), 6)
	rules := append(v4, v6...)
	profiles := map[string]Profile{"ogs-sftp": {UFWName: "ogs-sftp", Ports: []string{"2223/tcp"}}}
	for _, c := range []struct {
		port, from, local string
		want              bool
	}{
		{"22", "203.0.113.99", "", false},        // ni la règle d'ogs, ni celle vers 10.0.0.1
		{"22", "203.0.113.99", "10.0.0.1", true}, // arrivée à l'adresse que la règle vise
		{"22", "203.0.113.99", "10.0.0.2", false},
		{"8080", "192.0.2.5", "", true},    // tous les ports
		{"2250", "198.51.100.7", "", true}, // une plage
		{"2250", "198.51.100.8", "", false},
		{"2223", "203.0.113.4", "", true}, // les ports du profil réécrit
		{"22", "203.0.113.4", "", false},
		{"22", "2001:db8::1", "", true},     // IPv6, depuis toute adresse de sa famille
		{"2022", "203.0.113.99", "", false}, // par quelle interface est-elle arrivée ?
		{"2250", "", "", true},              // depuis une source quelconque
		{"2022", "", "", true},              // par le VPN
	} {
		s := Session{}
		if c.from != "" {
			s.Addr = netip.MustParseAddr(c.from)
		}
		if c.local != "" {
			s.Local = netip.MustParseAddr(c.local)
		}
		if got := keptOpens(rules, profiles, tcpSpecs(c.port), s); got != c.want {
			t.Errorf("port %s depuis %q vers %q : %v, attendu %v", c.port, c.from, c.local, got, c.want)
		}
	}
	if keptOpens(v4, nil, tcpSpecs("22"), Session{Addr: netip.MustParseAddr("2001:db8::1")}) {
		t.Error("une règle IPv4 n'admet pas un client IPv6")
	}
	s := Session{Addr: netip.MustParseAddr("203.0.113.99")}
	if !viaInterface(rules, profiles, tcpSpecs("2022"), s) || viaInterface(rules, profiles, tcpSpecs("22"), s) {
		t.Error("seule la règle liée à wg0 admettrait la session sur 2022")
	}
	// La boucle locale n'ouvre rien du dehors ; une session qui arrive par elle
	// y passe.
	lo := parseRules([]byte("*filter\n"+tuple("allow tcp 2023 0.0.0.0/0 any 127.0.0.1 in", "")+
		tuple("allow tcp 2024 127.0.0.1 any 0.0.0.0/0 in", "")), 4)
	local := Session{Addr: netip.MustParseAddr("127.0.0.1"), Local: netip.MustParseAddr("127.0.0.1")}
	for _, port := range []string{"2023", "2024"} {
		if keptOpens(lo, nil, tcpSpecs(port), Session{}) || !keptOpens(lo, nil, tcpSpecs(port), local) {
			t.Errorf("boucle locale, port %s", port)
		}
	}
	if !overlaps([]string{"2200:2300/tcp"}, []string{"22/tcp", "2250/tcp"}) || overlaps([]string{"22/udp"}, []string{"22"}) {
		t.Error("recouvrement des ports")
	}
}

// Qui n'ouvre ssh que par un VPN (« ufw allow in on wg0 to any port 22 »), ou
// vers une adresse précise du serveur, garde un accès. La garde des ports de
// sshd ne comptait qu'une règle sans interface ni destination : depuis la
// console, elle refusait tout resserrement — un simple bannissement — en disant
// qu'aucune règle de l'administrateur n'ouvrait le port.
func TestGardePortsDeSshdCompteUnAccèsLiéÀUneInterface(t *testing.T) {
	b := nouveauBanc(t, "sources:\n  bureau:\n    addresses: [10.200.0.2]\ntrusted: [bureau]\n"+
		"policy:\n  incoming: deny\nprofiles:\n  ssh: {available: false}\n", "ssh")
	b.write("/etc/ssh/sshd_config", "Port 22\n")
	b.prov.Admin = sansSession
	b.sshdÉcoute("22")
	b.prov.AllowLockout = true
	b.reconcilier()
	b.prov.AllowLockout = false
	desired, err := b.prov.Desired()
	if err != nil {
		t.Fatal(err)
	}
	b.prov.Cfg.Ban = append(b.prov.Cfg.Ban, config.ParseUFWEntry("198.51.100.9"))
	for _, regle := range []string{
		"allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 in_wg0",
		"allow tcp 22 10.8.0.1 any 0.0.0.0/0 in",
		"limit_log tcp 22 10.8.0.0/24 any 0.0.0.0/0 in_wg0",
	} {
		b.write("/etc/ufw/user.rules", strings.Replace(rulesFile(desired, 4), "*filter\n", "*filter\n"+
			tuple(regle, "ssh par le VPN"), 1))
		if _, err := b.planErr(); err != nil {
			t.Errorf("« %s » ouvre le port 22 : bannir une adresse ne le ferme pas : %v", regle, err)
		}
	}
	// Ce qui n'ouvre rien du dehors ne compte pas : la boucle locale, qu'ufw
	// laisse passer de toute façon, une sortie, un routage, un port d'origine.
	for _, regle := range []string{
		"allow tcp 22 127.0.0.1 any 0.0.0.0/0 in",
		"allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 in_lo",
		"allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 out_eth0",
		"route:allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 in_wg0!out_eth0",
		"allow tcp 22 0.0.0.0/0 1234 0.0.0.0/0 in",
	} {
		b.write("/etc/ufw/user.rules", strings.Replace(rulesFile(desired, 4), "*filter\n", "*filter\n"+
			tuple(regle, ""), 1))
		_, err := b.planErr()
		if err == nil || !strings.Contains(err.Error(), "sshd écoute sur le port 22, que plus rien n'ouvrirait") {
			t.Errorf("« %s » n'ouvre pas le port 22 du dehors : %v", regle, err)
		}
	}
}

// Pour une session, la question n'est plus « le port reste-t-il ouvert ? » mais
// « la suivante passera-t-elle par le même chemin ? ». Une règle qui vise une
// adresse du serveur se compare à celle que la session a jointe ; une règle
// liée à une interface ne compte pas, rien ne disant laquelle la session a
// empruntée — et le refus le dit, pour qu'on ne le croie pas aveugle à la règle.
func TestGardeSessionParUnVPN(t *testing.T) {
	b := nouveauBanc(t, "sources:\n  bureau:\n    addresses: [10.200.0.2]\ntrusted: [bureau]\n"+
		"policy:\n  incoming: deny\nprofiles:\n  ssh: {available: false}\n", "ssh")
	b.write("/etc/ssh/sshd_config", "Port 22\n")
	b.prov.Admin = sansSession
	b.sshdÉcoute("22")
	b.prov.AllowLockout = true
	b.reconcilier()
	b.prov.AllowLockout = false
	desired, err := b.prov.Desired()
	if err != nil {
		t.Fatal(err)
	}
	règle := func(r string) {
		b.write("/etc/ufw/user.rules", strings.Replace(rulesFile(desired, 4), "*filter\n", "*filter\n"+
			tuple(r, "ssh par le VPN"), 1))
	}
	b.prov.Cfg.Ban = append(b.prov.Cfg.Ban, config.ParseUFWEntry("198.51.100.9"))
	b.prov.Admin = func() (Session, bool) {
		return Session{Addr: netip.MustParseAddr("10.8.0.2"), Local: netip.MustParseAddr("10.8.0.1"), Port: "22"}, true
	}

	règle("allow tcp 22 10.8.0.1 any 10.8.0.0/24 in")
	if _, err := b.planErr(); err != nil {
		t.Errorf("la session a joint l'adresse que la règle vise : %v", err)
	}
	règle("allow tcp 22 0.0.0.0/0 any 10.8.0.0/24 in_wg0")
	_, err = b.planErr()
	if err == nil || !strings.Contains(err.Error(), "10.8.0.2, et le profil ssh n'est pas disponible") ||
		!strings.Contains(err.Error(), "liée à une interface l'admettrait peut-être") {
		t.Errorf("une règle liée à une interface ne couvre pas la session, et le refus le dit : %v", err)
	}
	// Arrivée à une autre adresse du serveur, la session ne passe pas par la
	// règle qui vise celle du VPN.
	règle("allow tcp 22 10.8.0.1 any 10.8.0.0/24 in")
	b.prov.Admin = func() (Session, bool) {
		return Session{Addr: netip.MustParseAddr("10.8.0.2"), Local: netip.MustParseAddr("203.0.113.1"), Port: "22"}, true
	}
	if _, err := b.planErr(); err == nil || strings.Contains(err.Error(), "interface") {
		t.Errorf("la session a joint une autre adresse que celle de la règle : %v", err)
	}
	// Sans session, les deux ouvrent le port du dehors.
	b.prov.Admin = sansSession
	if _, err := b.planErr(); err != nil {
		t.Errorf("depuis la console : %v", err)
	}
}

// constat dit si doctor rend un constat dont le détail contient ce motif.
func constat(findings []Finding, motif string) bool {
	for _, f := range findings {
		if strings.Contains(f.Detail, motif) {
			return true
		}
	}
	return false
}

// L'unité transitoire du retour automatique vit sous /run : un redémarrage dans
// le délai l'efface, et ufw repart actif, sans filet. Rien ne le signalait —
// confirm répondait « aucune activation en attente », doctor se taisait. Le
// témoin posé sous /var/lib/ogs à l'armement survit au redémarrage.
func TestTémoinDUneActivationNonConfirmée(t *testing.T) {
	b := nouveauBanc(t, declarationType+"rollbackAfter: 30s\n", "ssh", "mariadb")
	b.write("/var/lib/ogs/.garde", "")
	b.prov.Exec = &ssReel{b.rec}
	b.prov.Admin = session("203.0.113.10", "")
	témoin := filepath.Join(b.root, "var/lib/ogs/ufw-unconfirmed")
	ctx := context.Background()
	activer := func() *engine.Step {
		t.Helper()
		b.write("/etc/ufw/ufw.conf", "ENABLED=no\nLOGLEVEL=low\n")
		b.rec.Responses["systemctl is-active"] = exec.Result{Stdout: "inactive\n"}
		var state *engine.Step
		for _, s := range b.plan() {
			if s.Resource == "firewall/state" {
				state = s
			}
		}
		if state == nil {
			t.Fatal("l'activation manque au plan")
		}
		if err := state.Apply(ctx); err != nil || b.prov.Armed == 0 {
			t.Fatalf("activation : %v (retour armé : %s)", err, b.prov.Armed)
		}
		if _, err := os.Stat(témoin); err != nil {
			t.Fatalf("le témoin doit être posé à l'armement : %v", err)
		}
		return state
	}
	observer := func(enabled, timer bool) Observed {
		t.Helper()
		conf, active := "ENABLED=no\n", "inactive\n"
		if enabled {
			conf = "ENABLED=yes\n"
		}
		if timer {
			active = "active\n"
		}
		b.write("/etc/ufw/ufw.conf", conf+"LOGLEVEL=low\n")
		b.rec.Responses["systemctl is-active"] = exec.Result{Stdout: active}
		obs, err := b.prov.Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return obs
	}

	activer()
	if obs := observer(true, true); !obs.RollbackPending || obs.RollbackLost() {
		t.Errorf("en attente : %+v", obs)
	}
	// Redémarrage dans le délai : le minuteur a disparu, ufw est actif.
	obs := observer(true, false)
	if !obs.RollbackLost() {
		t.Fatalf("le retour perdu doit se voir : %+v", obs)
	}
	if !constat(b.prov.Checks(obs, nil), "retour automatique a disparu") {
		t.Error("doctor doit signaler le retour perdu")
	}
	if confirmed, err := b.prov.Confirm(ctx, obs); err != nil || !confirmed {
		t.Errorf("la nouvelle session confirme l'activation au retour perdu : %v %v", confirmed, err)
	}
	if _, err := os.Stat(témoin); !os.IsNotExist(err) {
		t.Errorf("confirm retire le témoin : %v", err)
	}
	if obs := observer(true, false); obs.Unconfirmed || constat(b.prov.Checks(obs, nil), "confirm") {
		t.Errorf("confirmée, l'activation ne se signale plus : %+v", obs)
	}

	// Le retour a joué : ufw inactif, le témoin reste, et doctor dit pourquoi.
	activer()
	obs = observer(false, false)
	if !obs.RolledBack() || !constat(b.prov.Checks(obs, nil), "le retour automatique a désactivé ufw") {
		t.Errorf("le retour joué doit se voir : %+v", obs)
	}
	if confirmed, _ := b.prov.Confirm(ctx, obs); confirmed {
		t.Error("un retour joué n'a rien à confirmer")
	}
	if _, err := os.Stat(témoin); err != nil {
		t.Errorf("le témoin reste tant que rien n'a été confirmé : %v", err)
	}

	// L'activation défaite, ou ufw désactivé par ufw.yaml, retire le témoin.
	state := activer()
	if err := state.Compensate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(témoin); !os.IsNotExist(err) {
		t.Errorf("la compensation retire le témoin : %v", err)
	}
	activer()
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	b.prov.Cfg.Active = new(bool)
	for _, s := range b.plan() {
		if s.Resource == "firewall/state" {
			if err := s.Apply(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := os.Stat(témoin); !os.IsNotExist(err) {
		t.Errorf("désactivé par ufw.yaml, ufw n'a plus d'activation à confirmer : %v", err)
	}
}

// Une règle de l'administrateur qui admet l'adresse de la session sur un port
// sans rapport (80) faisait choisir le message du port : « arrivée sur le port
// 22, que le profil ssh n'ouvre pas (22/tcp) » — le profil l'ouvre, c'est
// l'adresse qu'aucune règle ssh n'admet. Le refus nomme ce qui manque, et
// seulement cela.
func TestGardeSessionNommeCeQuiManque(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	b.write("/etc/ufw/user.rules", "*filter\n"+tuple("allow tcp 80 0.0.0.0/0 any 192.0.2.0/24 in", "")+"COMMIT\n")
	b.prov.Admin = session("192.0.2.50", "22")
	_, err := b.planErr()
	if err == nil || strings.Contains(err.Error(), "arrivée sur le port") ||
		!strings.Contains(err.Error(), "192.0.2.50, qu'aucune règle du profil ssh n'autorise") {
		t.Errorf("l'adresse manque, non le port : %v", err)
	}
	// L'adresse et le port manquent ensemble : les deux remèdes.
	b.prov.Admin = session("192.0.2.50", "2222")
	_, err = b.planErr()
	if err == nil || !strings.Contains(err.Error(), "ajouter cette adresse à trusted") ||
		!strings.Contains(err.Error(), "le port 2222") {
		t.Errorf("l'adresse et le port manquent : %v", err)
	}
	// L'adresse admise par la confiance, seul le port manque.
	b.prov.Admin = session("203.0.113.10", "2222")
	_, err = b.planErr()
	if err == nil || !strings.Contains(err.Error(), "arrivée sur le port 2222") ||
		strings.Contains(err.Error(), "ajouter cette adresse") {
		t.Errorf("seul le port manque : %v", err)
	}
}

// Un profil personnalisé qui ouvre un port de sshd est un accès comme un autre :
// seul le profil « ssh » comptait, et un bannissement était refusé alors que
// rien ne fermait ce port — comme la session qui y était arrivée.
func TestGardesComptentLesAutresProfils(t *testing.T) {
	b := nouveauBanc(t, "sources:\n  bureau:\n    addresses: [10.200.0.2]\ntrusted: [bureau]\n"+
		"policy:\n  incoming: deny\nprofiles:\n  ssh: {ports: [22/tcp]}\n"+
		"  sshalt: {ports: [2222/tcp], units: [ssh]}\n", "ssh")
	b.write("/etc/ssh/sshd_config", "Port 22\nPort 2222\n")
	b.prov.Admin = sansSession
	b.sshdÉcoute("22", "2222")
	b.prov.AllowLockout = true
	b.reconcilier()
	b.prov.AllowLockout = false

	b.prov.Cfg.Ban = append(b.prov.Cfg.Ban, config.ParseUFWEntry("198.51.100.9"))
	if _, err := b.planErr(); err != nil {
		t.Errorf("sshalt ouvre 2222 : bannir une adresse ne le ferme pas : %v", err)
	}
	b.sshdÉcoute()
	b.prov.Admin = session("10.200.0.2", "2222")
	if _, err := b.planErr(); err != nil {
		t.Errorf("la session arrivée sur 2222 repasse par sshalt : %v", err)
	}
}

// « ufw app update » en échec après l'écriture du fichier de profils : le
// fichier restait aux nouveaux ports, le plan suivant le disait conforme, et la
// règle d'ogs gardait l'ancien port pour toujours — sshd déplacé sur 2200, plus
// aucune session neuve ne passait, sans un mot du plan ni de doctor (Q-06).
func TestProfilsRejouésAprèsUnAppUpdateEnÉchec(t *testing.T) {
	b := nouveauBanc(t, "sources:\n  bureau:\n    addresses: [10.200.0.2]\ntrusted: [bureau]\n"+
		"policy:\n  incoming: deny\n", "ssh")
	b.write("/etc/ssh/sshd_config", "Port 22\n")
	b.prov.Admin = session("10.200.0.2", "22")
	b.sshdÉcoute("22")
	b.reconcilier()

	b.write("/etc/ssh/sshd_config", "Port 2200\n")
	b.sshdÉcoute("2200")
	b.rec.Errors["ufw app"] = errors.New("ERROR: problem running ufw-init (verrou)")
	steps, err := b.planErr()
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Execute(context.Background(), &engine.Plan{Steps: steps}, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("l'échec d'« ufw app update » doit être signalé")
	}
	if got := b.profil("ogs-ssh"); got != "22/tcp" {
		t.Errorf("le fichier de profils doit revenir aux ports que portent les règles : ogs-ssh = %s", got)
	}
	// Le profil en échec est ramené au fichier rendu : ufw a pu réécrire
	// user.rules avant d'échouer.
	updates := 0
	for _, c := range b.rec.Calls() {
		if c.Name == "ufw" && strings.Join(c.Args, " ") == "app update ogs-ssh" {
			updates++
		}
	}
	if updates != 2 {
		t.Errorf("« ufw app update ogs-ssh » doit être retenté sur le fichier rendu : %d appel(s)", updates)
	}

	delete(b.rec.Errors, "ufw app")
	steps, err = b.planErr()
	if err != nil {
		t.Fatal(err)
	}
	if res := resources(steps); len(res) != 1 || res[0] != "firewall/profiles" {
		t.Fatalf("le plan suivant doit rejouer les profils : %v", res)
	}
	for _, s := range steps {
		if s.Resource == "firewall/profiles" && !strings.Contains(s.Summary, "ports modifiés : ogs-ssh") {
			t.Errorf("le profil dont les ports changent doit être nommé : %s", s.Summary)
		}
	}
	before := len(b.rec.Calls())
	if _, err := engine.Execute(context.Background(), &engine.Plan{Steps: steps}, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	replayed := false
	for _, c := range b.rec.Calls()[before:] {
		replayed = replayed || (c.Name == "ufw" && strings.Join(c.Args, " ") == "app update ogs-ssh")
	}
	if !replayed || b.profil("ogs-ssh") != "2200/tcp" {
		t.Errorf("« ufw app update ogs-ssh » doit être rejoué : rejoué=%v, profil=%s", replayed, b.profil("ogs-ssh"))
	}
}
