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

func TestGardeSessionAdministration(t *testing.T) {
	b := nouveauBanc(t, declarationType, "ssh", "mariadb")
	b.prov.Admin = func() (netip.Addr, bool) { return netip.MustParseAddr("192.0.2.50"), true }
	_, err := b.planErr()
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "192.0.2.50") {
		t.Fatalf("une activation qui couperait la session doit être refusée : %v", err)
	}
	b.prov.Force = true
	if _, err := b.planErr(); err != nil {
		t.Errorf("--force passe outre la garde : %v", err)
	}

	b.prov.Force = false
	b.prov.Admin = func() (netip.Addr, bool) { return netip.MustParseAddr("203.0.113.10"), true }
	if _, err := b.planErr(); err != nil {
		t.Errorf("une session couverte par la confiance est acceptée : %v", err)
	}
	b.prov.Admin = func() (netip.Addr, bool) { return netip.MustParseAddr("192.0.2.66"), true }
	if _, err := b.planErr(); err == nil || !strings.Contains(err.Error(), "bannirait") {
		t.Errorf("bannir l'adresse de la session doit être refusé : %v", err)
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
	if got := b.prov.IgnoreIP(); len(got) != 1 || got[0] != "203.0.113.10" {
		t.Errorf("adresses de confiance pour fail2ban : %v", got)
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
