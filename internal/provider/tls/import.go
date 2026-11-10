package tls

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// Import d'un certificat émis ailleurs (D130).
//
// Le cas d'usage est la migration d'un site : la cible reçoit le certificat que
// certbot tient sur l'ancien serveur, sert le site en HTTPS avant que le DNS ne
// bascule — ce qu'aucune émission HTTP-01 ne permet —, et ogs renouvelle
// ensuite par son propre compte ACME, au seuil ordinaire (RenewBefore). Rien ne
// distingue un certificat importé d'un certificat émis : le plan juge ce qui est
// sur le disque, et c'est voulu — le renouvellement, la bascule TLS du vhost
// (D79), le contrôle des paires (D110) et la suppression s'appliquent tels quels.

// MaxImportSize borne la lecture de chaque fichier importé : une chaîne complète
// tient en quelques kilo-octets.
const MaxImportSize = 1 << 20

// Imported est une paire lue, contrôlée et prête à écrire.
type Imported struct {
	Material Material
	Info     Info
	// Warnings nomme ce qui n'empêche pas l'import mais mérite d'être dit : une
	// chaîne non vérifiable, une échéance qui déclenchera le renouvellement.
	Warnings []string
}

// ParseImport lit la chaîne complète et la clé d'un certificat importé. Elle
// refuse ce que le serveur web refuserait, ou ce qu'ogs remplacerait aussitôt :
// une clé chiffrée ou dépareillée, un certificat échu, de l'environnement de
// test de l'autorité, auto-signé ou d'autorité. roots désigne les autorités
// de confiance — nil pour celles du système —, contre lesquelles la chaîne est
// vérifiée : un échec n'y est qu'un avertissement, le magasin du système
// pouvant manquer.
func ParseImport(chainPEM, keyPEM []byte, now time.Time, roots *x509.CertPool) (Imported, error) {
	var certs []*pem.Block
	for rest := chainPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch {
		case block.Type == "CERTIFICATE":
			certs = append(certs, block)
		case strings.Contains(block.Type, "PRIVATE KEY"):
			return Imported{}, fmt.Errorf("le fichier du certificat porte une clé privée : " +
				"désigner la chaîne (fullchain.pem) par --cert et la clé (privkey.pem) par --key")
		}
	}
	if len(certs) == 0 {
		return Imported{}, fmt.Errorf("aucun certificat PEM dans le fichier du certificat")
	}
	key, err := importKey(keyPEM)
	if err != nil {
		return Imported{}, err
	}
	leafPEM := pem.EncodeToMemory(certs[0])
	var chain []byte
	for _, b := range certs[1:] {
		chain = append(chain, pem.EncodeToMemory(b)...)
	}
	full := append(append([]byte{}, leafPEM...), chain...)
	leaf, err := x509.ParseCertificate(certs[0].Bytes)
	if err != nil {
		return Imported{}, fmt.Errorf("certificat illisible : %w", err)
	}
	// chain.pem de certbot désigné à la place de fullchain.pem : le dire avant
	// la clé, qui ne correspond pas à l'autorité.
	if leaf.IsCA {
		return Imported{}, fmt.Errorf("le premier certificat du fichier est celui d'une autorité (%s) : "+
			"la chaîne doit commencer par le certificat du site (fullchain.pem)", leaf.Subject.CommonName)
	}
	if !pairs(full, key) {
		return Imported{}, fmt.Errorf("la clé privée ne correspond pas au certificat")
	}
	info, err := Store{}.parseInfo("", full)
	if err != nil {
		return Imported{}, err
	}
	switch {
	case info.Expired(now):
		return Imported{}, fmt.Errorf("certificat échu depuis le %s", info.NotAfter.Format("02/01/2006"))
	case info.Staging:
		return Imported{}, fmt.Errorf("certificat de l'environnement de test de l'autorité (%s), qu'aucun "+
			"navigateur ne reconnaît", info.Issuer)
	case leaf.Issuer.String() == leaf.Subject.String() || leaf.CheckSignatureFrom(leaf) == nil:
		return Imported{}, fmt.Errorf("certificat auto-signé : « ogs cert issue --self-signed » en produit un")
	}
	imp := Imported{Material: Material{Certificate: leafPEM, Chain: chain, PrivateKey: key}, Info: info}
	if now.Before(info.NotBefore) {
		imp.Warnings = append(imp.Warnings, fmt.Sprintf("certificat valable seulement à partir du %s",
			info.NotBefore.Format("02/01/2006 15:04")))
	}
	inter := x509.NewCertPool()
	for _, b := range certs[1:] {
		if c, err := x509.ParseCertificate(b.Bytes); err == nil {
			inter.AddCert(c)
		}
	}
	verifyAt := now
	if verifyAt.Before(info.NotBefore) {
		verifyAt = info.NotBefore
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, CurrentTime: verifyAt,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		imp.Warnings = append(imp.Warnings, fmt.Sprintf("chaîne non vérifiée contre les autorités du système "+
			"(%v) : un navigateur pourrait la refuser — fullchain.pem de certbot porte l'intermédiaire, "+
			"cert.pem non", err))
	}
	if info.NeedsRenewal(now) {
		imp.Warnings = append(imp.Warnings, fmt.Sprintf("le certificat expire dans %d jours : le prochain apply "+
			"et le timer tenteront de le renouveler, ce qui échoue tant que le DNS désigne l'autre serveur — "+
			"il reste servi jusque-là ; renouveler d'abord à la source (certbot renew --force-renewal) "+
			"évite ces tentatives", info.DaysLeft(now)))
	}
	return imp, nil
}

// importKey extrait la clé privée, seule : un fichier d'OpenSSL porte parfois
// un bloc « EC PARAMETERS » devant elle, que le magasin n'a pas à garder.
func importKey(data []byte) ([]byte, error) {
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, fmt.Errorf("aucune clé privée PEM dans le fichier de la clé")
		}
		switch block.Type {
		case "ENCRYPTED PRIVATE KEY":
			return nil, fmt.Errorf("clé privée chiffrée : le serveur web ne la chargerait pas sans " +
				"phrase de passe — la déchiffrer d'abord (openssl pkey)")
		case "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY":
			if _, ok := block.Headers["DEK-Info"]; ok {
				return nil, fmt.Errorf("clé privée chiffrée : le serveur web ne la chargerait pas sans " +
					"phrase de passe — la déchiffrer d'abord (openssl pkey)")
			}
			return pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: block.Bytes}), nil
		}
	}
}

// ImportTarget choisit la demande du projet à laquelle rattacher le certificat
// importé : celle que désigne name, sinon la seule du projet, sinon la seule
// dont le certificat couvre un nom. Il rend les noms de la demande que le
// certificat ne couvre pas : le plan tentera de les obtenir par une émission,
// qui échoue tant que le DNS désigne l'autre serveur — le certificat importé
// reste servi d'ici là (D79). Un certificat qui ne couvre aucun nom de la
// demande est refusé : rien ne le servirait.
func (p *Provider) ImportTarget(proj *model.Project, name string, info Info) (Request, []string, error) {
	reqs, err := p.Requests(proj)
	if err != nil {
		return Request{}, nil, err
	}
	if len(reqs) == 0 {
		if ref := p.SharedRef(proj); ref != "" {
			return Request{}, nil, fmt.Errorf("le projet %s s'appuie sur le certificat partagé %s : "+
				"l'importer sous « %s »", proj.ID(), ref, ref)
		}
		return Request{}, nil, fmt.Errorf("le projet %s ne demande aucun certificat (aucun vhost en TLS)",
			proj.ID())
	}
	var candidates []Request
	switch {
	case name != "":
		for _, r := range reqs {
			if r.Name == name {
				candidates = append(candidates, r)
			}
		}
		if len(candidates) == 0 {
			return Request{}, nil, fmt.Errorf("le projet %s ne déclare aucun certificat %q (certificats : %s)",
				proj.ID(), name, requestNames(reqs))
		}
	case len(reqs) == 1:
		candidates = reqs
	default:
		for _, r := range reqs {
			if len(r.Domains) > len(uncovered(info, r.Domains)) {
				candidates = append(candidates, r)
			}
		}
		if len(candidates) != 1 {
			return Request{}, nil, fmt.Errorf("le projet %s déclare plusieurs certificats (%s) : "+
				"désigner celui à remplacer par --name", proj.ID(), requestNames(reqs))
		}
	}
	req := candidates[0]
	missing, err := importCoverage(req, info)
	return req, missing, err
}

// SharedImportTarget est ImportTarget pour un certificat de portée serveur.
func (p *Provider) SharedImportTarget(ref string, info Info) (Request, []string, error) {
	req, ok := p.SharedRequest(ref)
	if !ok {
		return Request{}, nil, fmt.Errorf("aucun certificat partagé %q déclaré", ref)
	}
	missing, err := importCoverage(req, info)
	return req, missing, err
}

func importCoverage(req Request, info Info) ([]string, error) {
	missing := uncovered(info, req.Domains)
	if len(missing) == len(req.Domains) {
		return nil, fmt.Errorf("le certificat (%s) ne couvre aucun nom du certificat %s (%s)",
			strings.Join(info.Domains, ", "), req.Name, strings.Join(req.Domains, ", "))
	}
	return missing, nil
}

func uncovered(info Info, domains []string) []string {
	var out []string
	for _, d := range domains {
		if !coveredBy(info.Domains, d) {
			out = append(out, d)
		}
	}
	return out
}

func requestNames(reqs []Request) string {
	names := make([]string, 0, len(reqs))
	for _, r := range reqs {
		names = append(names, r.Name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// ErrImportExists signale un certificat déjà présent sous ce nom : l'import ne
// le remplace que sous --force, après sauvegarde.
var ErrImportExists = errors.New("certificat déjà présent")

// Import écrit le certificat importé sous le nom de la demande. Un certificat en
// place n'est remplacé que sous force, sa paire sauvegardée d'abord dans le
// répertoire des sauvegardes d'ogs (§6.4) — jamais à côté : le magasin est relu
// par le renouvellement. Il rend les sauvegardes écrites et dit si un
// certificat était en place.
func (p *Provider) Import(ctx context.Context, name string, imp Imported, force bool) (backups []string, replaced bool, err error) {
	_, present, _, err := p.Store.ReadPair(name)
	if err != nil {
		return nil, false, err
	}
	if present && !force {
		return nil, true, fmt.Errorf("%w : %s — --force le remplace, après sauvegarde dans %s",
			ErrImportExists, p.Store.Dir(name), p.Paths.BackupsDir())
	}
	if present {
		now := p.now()
		base := DirName(name)
		for _, f := range []struct {
			file string
			mode os.FileMode
		}{{FileFullchain, 0o644}, {FileKey, 0o600}} {
			data, rerr := readIfExists(p.Store.Path(name, f.file))
			if rerr != nil {
				return backups, true, rerr
			}
			if data == nil {
				continue
			}
			dst, werr := fs.BackupDataTo(base+"-"+f.file, data, f.mode, p.Paths.BackupsDir(), now)
			if werr != nil {
				return backups, true, fmt.Errorf("sauvegarde de %s : %w", filepath.Join(p.Store.Dir(name), f.file), werr)
			}
			backups = append(backups, dst)
		}
	}
	if err := p.Store.WriteContext(ctx, name, imp.Material); err != nil {
		return backups, present, err
	}
	return backups, present, nil
}

// Served dit quels vhosts écrits désignent le certificat : ceux-là ne le
// présentent qu'une fois le serveur web rechargé.
func (p *Provider) Served(name string) []string { return p.servedBy(name) }

// RemoveUnserved retire un certificat qu'aucun vhost écrit ne désigne : la
// création défaite d'un site qui l'avait importé ne le laisse pas derrière elle.
func (p *Provider) RemoveUnserved(name string) error {
	if len(p.servedBy(name)) > 0 {
		return nil
	}
	return p.Store.Remove(name)
}

func readIfExists(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}
