package mail

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Record est l'enregistrement DNS d'une clé, tel qu'il faut le publier. ogs ne
// le publie pas (D69) : les zones des clients vivent chez d'autres, et écrire
// dans la zone d'un client n'engage pas la même confiance que dans la sienne.
type Record struct {
	Domain   string   `json:"domain" yaml:"domain"`
	Selector string   `json:"selector" yaml:"selector"`
	State    string   `json:"state" yaml:"state"`
	Name     string   `json:"name" yaml:"name"`
	Type     string   `json:"type" yaml:"type"`
	Value    string   `json:"value" yaml:"value"`
	Strings  []string `json:"strings" yaml:"strings"`
}

// maxTXTString est la longueur maximale d'une chaîne de caractères dans un
// enregistrement TXT. Une clé RSA-2048 la dépasse : l'enregistrement est donc
// découpé en plusieurs chaînes, que le résolveur recolle.
const maxTXTString = 255

// NewRecord construit l'enregistrement d'une clé.
func NewRecord(k Key) (Record, error) {
	pub, err := PublicKey(k.PEM)
	if err != nil {
		return Record{}, err
	}
	value := "v=DKIM1; k=rsa; p=" + pub
	return Record{
		Domain: k.Domain, Selector: k.Selector, State: k.State,
		Name: k.Name() + ".", Type: "TXT", Value: value, Strings: chunk(value, maxTXTString),
	}, nil
}

func chunk(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

// Zone rend l'enregistrement au format d'un fichier de zone. Les parenthèses
// permettent de le poser sur plusieurs lignes, chaque chaîne tenant dans la
// limite de 255 octets.
func (r Record) Zone() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s IN TXT (", r.Name)
	for i, s := range r.Strings {
		if i > 0 {
			b.WriteString("\n" + strings.Repeat(" ", len(r.Name)+10))
		} else {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%q", s)
	}
	b.WriteString(" )")
	return b.String()
}

// Verdicts d'une vérification DNS.
const (
	// DNSMatch : l'enregistrement publié porte la clé.
	DNSMatch = "conforme"
	// DNSAbsent : aucun enregistrement DKIM n'est publié sous ce nom.
	DNSAbsent = "absent"
	// DNSMismatch : un enregistrement est publié, mais pour une autre clé.
	DNSMismatch = "différent"
	// DNSUnreachable : le résolveur n'a pas répondu ; rien ne peut être conclu.
	DNSUnreachable = "injoignable"
)

// lookupTimeout borne une résolution : « doctor » et « activate » ne doivent pas
// rester suspendus à un résolveur muet.
const lookupTimeout = 5 * time.Second

// Verify compare l'enregistrement publié à la clé. Le résolveur rend chaque
// enregistrement TXT chaînes recollées ; elles le sont une seconde fois ici, par
// prudence, espaces compris — un exploitant qui a collé la valeur sur plusieurs
// lignes dans l'interface de son registrar y laisse souvent des blancs.
func (p *Provider) Verify(ctx context.Context, k Key) (verdict, detail string) {
	want, err := PublicKey(k.PEM)
	if err != nil {
		return DNSMismatch, err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	txts, err := p.lookup(ctx, k.Name())
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return DNSAbsent, "aucun enregistrement TXT sous " + k.Name()
		}
		return DNSUnreachable, err.Error()
	}
	found := false
	for _, txt := range txts {
		tags := parseTags(txt)
		if tags["v"] != "" && tags["v"] != "DKIM1" {
			continue
		}
		pub, ok := tags["p"]
		if !ok {
			continue
		}
		found = true
		if pub == want {
			return DNSMatch, k.Name()
		}
	}
	if !found {
		return DNSAbsent, "aucun enregistrement DKIM sous " + k.Name()
	}
	return DNSMismatch, "l'enregistrement publié sous " + k.Name() + " porte une autre clé"
}

func (p *Provider) lookup(ctx context.Context, name string) ([]string, error) {
	if p.LookupTXT != nil {
		return p.LookupTXT(ctx, name)
	}
	if p.simulated() {
		// Le mode d'essai n'atteint pas le système : il n'atteint pas davantage le
		// réseau, et un résolveur muet y ferait attendre chaque « doctor ».
		return nil, errors.New("résolution non effectuée en mode simulé")
	}
	return net.DefaultResolver.LookupTXT(ctx, name)
}

// parseTags lit la liste « balise=valeur ; … » d'un enregistrement DKIM. Les
// blancs sont retirés des valeurs : la RFC 6376 les autorise dans la clé
// publique, et un copier-coller en introduit.
func parseTags(txt string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(txt, ";") {
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.Join(strings.Fields(value), "")
		out[name] = value
	}
	return out
}
