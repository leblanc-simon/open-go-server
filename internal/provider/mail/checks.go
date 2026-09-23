package mail

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"leblanc.io/open-go-server/internal/model"
)

// Finding est un constat de « doctor ».
type Finding struct {
	Name, Status, Detail, Fix string
}

// Statuts d'un constat, ceux de « doctor ».
const (
	statusOK      = "ok"
	statusWarning = "avertissement"
	statusFailure = "échec"
)

// Checks porte ce qu'aucune vérification de configuration ne voit : un
// opendkim.conf qui ne désigne pas les tables d'ogs ne signe rien et ne le dit
// pas, une clé trop ouverte est ignorée par OpenDKIM, et un enregistrement non
// publié rend toute signature invérifiable.
func (p *Provider) Checks(ctx context.Context, projects []*model.Project, obs Observed) []Finding {
	var out []Finding
	add := func(name, status, detail, fix string) {
		out = append(out, Finding{Name: name, Status: status, Detail: detail, Fix: fix})
	}
	wanted := 0
	for _, proj := range projects {
		wanted += len(proj.DKIMDomains())
	}
	if !obs.Present {
		if wanted > 0 {
			add("signature DKIM", statusWarning,
				fmt.Sprintf("%d domaine(s) demandent la signature, mais OpenDKIM est absent", wanted),
				"apt install opendkim — ou retirer spec.mail.dkim des projets concernés")
		}
		return out
	}
	// Un OpenDKIM que personne n'emploie par ogs n'a pas à désigner ses tables :
	// l'administrateur peut le tenir à la main, et le lui reprocher accuserait
	// toute machine où le paquet est installé.
	if wanted == 0 {
		return out
	}
	// En mode simulé, OpenDKIM est réputé présent sans l'être : ni son compte ni
	// sa configuration n'ont à exister, et les réclamer accuserait le mode d'essai.
	if !p.simulated() {
		if !obs.OwnerOK {
			add("compte d'OpenDKIM", statusFailure,
				"le compte "+p.Cfg.Mail.DKIMUser()+" est introuvable : les clés ne peuvent pas lui appartenir",
				"régler mail.dkim.user dans /etc/ogs/config.yaml")
		}
		if status, detail, fix := p.confCheck(); status != "" {
			add("configuration d'OpenDKIM", status, detail, fix)
		}
	}

	var permIssues, missing []string
	for _, proj := range projects {
		keys, err := p.loadKeys(proj.ID())
		if err != nil {
			add("clés DKIM de "+proj.ID(), statusFailure, err.Error(), "")
			continue
		}
		grouped := byDomain(keys)
		for _, d := range proj.DKIMDomains() {
			active, ok := find(grouped[d], StateActive)
			if !ok {
				missing = append(missing, d)
				continue
			}
			path := p.Paths.OpenDKIMKeyFile(proj.ID(), d, active.Selector)
			info, err := os.Stat(path)
			switch {
			case err != nil:
				permIssues = append(permIssues, path+" : absent")
			default:
				if drift := p.permDrift(info, obs); drift != "" {
					permIssues = append(permIssues, path+" : "+drift)
				}
			}
			p.dnsCheck(ctx, add, proj, active)
			if pending, ok := find(grouped[d], StatePending); ok {
				verdict, _ := p.Verify(ctx, pending)
				status, detail := statusWarning, "préparée, enregistrement "+verdict+" : "+pending.Name()
				fix := "publier l'enregistrement (ogs mail dkim records " + proj.ID() + ")"
				if verdict == DNSMatch {
					detail = "préparée et publiée : " + pending.Name()
					fix = "ogs mail dkim activate " + proj.ID()
				}
				add("rotation DKIM de "+d, status, detail, fix)
			}
		}
		for _, k := range keys {
			if k.State != StateRetired {
				continue
			}
			if verdict, _ := p.Verify(ctx, k); verdict == DNSMatch && p.expired(k) {
				add("enregistrement DKIM remplacé", statusWarning,
					k.Name()+" est encore publié alors que sa clé a été retirée",
					"le retirer de la zone de "+k.Domain)
			}
		}
	}
	if len(missing) > 0 {
		add("clés DKIM", statusWarning, "aucune clé active pour "+strings.Join(missing, ", "),
			"ogs apply")
	}
	if len(permIssues) > 0 {
		add("droits des clés DKIM", statusFailure, strings.Join(permIssues, " ; ")+
			" : OpenDKIM refuse la clé et rend une erreur temporaire — le courrier du domaine reste en file",
			"ogs apply — sous RequireSafeKeys, OpenDKIM refuse une clé lisible au-delà de son propriétaire")
	} else if len(missing) == 0 {
		add("droits des clés DKIM", statusOK, "0600, au compte "+p.Cfg.Mail.DKIMUser(), "")
	}
	return out
}

// dnsCheck compare l'enregistrement publié à la clé active d'un domaine.
func (p *Provider) dnsCheck(ctx context.Context, add func(string, string, string, string),
	proj *model.Project, k Key) {
	name := "enregistrement DKIM de " + k.Domain
	verdict, detail := p.Verify(ctx, k)
	switch verdict {
	case DNSMatch:
		add(name, statusOK, "publié et conforme : "+k.Name(), "")
	case DNSAbsent:
		add(name, statusWarning, detail+" : la signature ne peut pas être vérifiée",
			"publier l'enregistrement (ogs mail dkim records "+proj.ID()+")")
	case DNSMismatch:
		add(name, statusFailure, detail+" : toute signature échoue à la vérification",
			"republier l'enregistrement (ogs mail dkim records "+proj.ID()+")")
	default:
		add(name, statusWarning, "vérification impossible : "+detail, "")
	}
}

// confCheck vérifie qu'opendkim.conf désigne les tables d'ogs. Sans elles, rien
// n'est signé et rien ne le dit : OpenDKIM démarre, vérifie le courrier entrant,
// et laisse partir le sortant tel quel.
func (p *Provider) confCheck() (status, detail, fix string) {
	data, err := os.ReadFile(p.Paths.OpenDKIMConf())
	if err != nil {
		return statusFailure, err.Error(), ""
	}
	keyTable, signingTable := confValue(data, "KeyTable"), confValue(data, "SigningTable")
	wantKey := p.Paths.OpenDKIMKeyTable()
	wantSigning := "refile:" + p.Paths.OpenDKIMSigningTable()
	fix = fmt.Sprintf("dans %s :\n  KeyTable      %s\n  SigningTable  %s",
		p.Paths.OpenDKIMConf(), wantKey, wantSigning)
	var problems []string
	if keyTable != wantKey && keyTable != "file:"+wantKey && keyTable != "refile:"+wantKey {
		problems = append(problems, "KeyTable vaut « "+orNone(keyTable)+" »")
	}
	if signingTable != wantSigning {
		if signingTable == wantSigning[len("refile:"):] || signingTable == "file:"+wantSigning[len("refile:"):] {
			problems = append(problems, "SigningTable est lue littéralement : « *@domaine » n'y "+
				"reconnaît aucun expéditeur — préfixer par refile:")
		} else {
			problems = append(problems, "SigningTable vaut « "+orNone(signingTable)+" »")
		}
	}
	if len(problems) > 0 {
		return statusFailure, strings.Join(problems, " ; ") + " : rien n'est signé", fix
	}
	return statusOK, "désigne les tables d'ogs", ""
}

// confValue lit une directive d'opendkim.conf : un mot-clé, des blancs, une
// valeur ; les commentaires commencent par « # ». Les mots-clés ne sont pas
// sensibles à la casse.
func confValue(data []byte, keyword string) string {
	value := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], keyword) {
			value = fields[1]
		}
	}
	return value
}

func orNone(s string) string {
	if s == "" {
		return "(absente)"
	}
	return s
}

// ForeignTables dit si opendkim.conf désigne des tables qui ne sont pas celles
// d'ogs : l'import les signale sans les adopter (D69), et un apply ne les
// remplacera pas.
func (p *Provider) ForeignTables() (string, bool) {
	data, err := os.ReadFile(p.Paths.OpenDKIMConf())
	if err != nil {
		return "", false
	}
	kt := confValue(data, "KeyTable")
	if kt == "" {
		return "", false
	}
	path := kt
	if i := strings.IndexByte(kt, ':'); i >= 0 {
		path = kt[i+1:]
	}
	if path == p.Paths.OpenDKIMKeyTable() {
		return "", false
	}
	return kt, true
}
