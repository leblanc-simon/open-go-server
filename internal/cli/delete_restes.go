package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/model"
	appprov "leblanc.io/open-go-server/internal/provider/app"
	tlsprov "leblanc.io/open-go-server/internal/provider/tls"
)

// Ce que la suppression d'un projet laissait derrière elle (D100) : certificats
// et clés privées, numéros de pool et ports, secrets du keystore. Rien de cela
// n'était dangereux sur l'instant, mais tout s'accumulait — la plage d'une
// version PHP finissait saturée, doctor avertissait à vie d'un certificat
// expiré, et un projet recréé sous le même identifiant héritait des secrets de
// l'ancien.

// certSecretPrefix nomme, dans l'archive, les fichiers d'un certificat de
// projet : ils y voyagent scellés comme les secrets du keystore — la clé privée
// n'a pas à y être en clair —, et la restauration les rend au magasin, jamais au
// keystore.
const certSecretPrefix = "tls/"

// certFiles sont les fichiers d'un certificat que l'archive recueille.
var certFiles = []string{tlsprov.FileCert, tlsprov.FileChain, tlsprov.FileFullchain, tlsprov.FileKey}

// keptCert est un certificat que la suppression laisse, et pourquoi.
type keptCert struct{ name, reason string }

// projectCertificates partage les certificats du projet entre ceux que la
// suppression retire et ceux qu'elle laisse. Un certificat de projet porte le
// nom de son répertoire, unique dans le parc (D94) : l'identifiant pour le
// certificat implicite, le nom déclaré sinon. Un parc antérieur à D94 peut
// pourtant porter deux déclarations du même nom — elles se partagent alors le
// répertoire : il reste, nommé, tant qu'un autre projet le déclare. Un
// certificat partagé (« @… ») n'est jamais au projet.
func (a *App) projectCertificates(provs *providers, p *model.Project) (remove []string, kept []keptCert) {
	reqs, err := provs.TLS.Requests(p)
	if err != nil {
		// Une déclaration de certificat fautive : son nom n'est pas sûr, rien
		// n'est retiré. Le certificat implicite reste éprouvé.
		reqs = nil
	}
	names := map[string]bool{}
	for _, r := range reqs {
		names[r.Name] = true
	}
	// Le certificat implicite porte l'identifiant même quand la déclaration en
	// nomme d'autres : il a pu être émis avant qu'elle ne change.
	names[p.ID()] = true
	others := map[string][]string{}
	if all, err := a.loadProjects(); err == nil {
		for _, other := range all {
			if other.ID() == p.ID() {
				continue
			}
			oreqs, _ := provs.TLS.Requests(other)
			for _, r := range oreqs {
				others[r.Name] = append(others[r.Name], other.ID())
			}
		}
	} else {
		// Le parc illisible ne dit pas qui d'autre se sert d'un certificat :
		// rien ne part.
		for name := range names {
			if _, ok, _ := provs.TLS.Store.Read(name); ok {
				kept = append(kept, keptCert{name, "parc illisible (" + err.Error() + ")"})
			}
		}
		return nil, kept
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		if tlsprov.IsShared(name) || tlsprov.CheckProjectName(name) != nil {
			continue
		}
		info, err := os.Lstat(provs.TLS.Store.Dir(name))
		if err != nil || !info.IsDir() {
			continue
		}
		if users := others[name]; len(users) > 0 {
			kept = append(kept, keptCert{name, "aussi déclaré par " + strings.Join(users, ", ")})
			continue
		}
		remove = append(remove, name)
	}
	return remove, kept
}

// archiveCertificates scelle dans l'archive les fichiers des certificats que la
// suppression retire.
func (a *App) archiveCertificates(w *archive.Writer, provs *providers, p *model.Project, names []string) error {
	for _, name := range names {
		for _, file := range certFiles {
			data, err := os.ReadFile(provs.TLS.Store.Path(name, file))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if a.Keys == nil {
				return fmt.Errorf("clé maître illisible : le certificat %s ne peut pas être archivé chiffré", name)
			}
			secret := certSecretPrefix + name + "/" + file
			nonce, ciphertext, err := a.Keys.Seal(p.ID(), secret, data)
			if err != nil {
				return err
			}
			if err := w.AddSecret(secret, nonce, ciphertext); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeCertificates retire les certificats du projet, une fois le serveur web
// rechargé sans les vhosts qui les désignaient : retirés avant, le rechargement
// échouait sur un fichier introuvable.
func (a *App) removeCertificates(provs *providers, names []string, removed *[]string,
	warn func(format string, args ...any)) {
	for _, name := range names {
		if err := provs.TLS.Store.Remove(name); err != nil {
			warn("certificat %s : %v", name, err)
			continue
		}
		*removed = append(*removed, "certificat "+name)
	}
}

// restoreCertificates rend au magasin les certificats que l'archive porte, avant
// l'apply : le vhost les présente d'emblée, et aucune émission n'est demandée
// pour un certificat encore valable. Un certificat déjà présent — émis depuis —
// n'est jamais remplacé.
func (a *App) restoreCertificates(p *model.Project, archivePath string, secrets map[string][2][]byte) []restoreGap {
	var missing []restoreGap
	files := map[string]map[string][]byte{}
	unreadable := map[string]bool{}
	for name, pair := range secrets {
		rest, ok := strings.CutPrefix(name, certSecretPrefix)
		if !ok {
			continue
		}
		cut := strings.LastIndex(rest, "/")
		if cut <= 0 {
			continue
		}
		cert, file := rest[:cut], rest[cut+1:]
		if tlsprov.CheckProjectName(cert) != nil {
			continue
		}
		if a.Keys == nil {
			a.warnf("⚠ clé maître illisible : le certificat %s n'est pas restauré\n", cert)
			return append(missing, restoreGap{"certificats non restaurés (clé maître illisible)",
				fmt.Sprintf("l'apply en émet de nouveaux ; ceux de l'archive %s demandent la clé maître "+
					"d'origine", archivePath)})
		}
		data, err := a.Keys.Open(p.ID(), name, pair[0], pair[1])
		if err != nil && unreadable[cert] {
			continue
		}
		if err != nil {
			// Une fois par certificat, non par fichier : ses trois fichiers sont
			// scellés sous la même clé.
			unreadable[cert] = true
			a.warnf("⚠ certificat %s illisible avec la clé maître actuelle : non restauré\n", cert)
			missing = append(missing, restoreGap{
				fmt.Sprintf("certificat %s non restauré (illisible avec la clé maître actuelle)", cert),
				"l'apply en émet un nouveau, sinon : ogs cert renew " + p.ID()})
			continue
		}
		if files[cert] == nil {
			files[cert] = map[string][]byte{}
		}
		files[cert][file] = data
	}
	for cert := range unreadable {
		delete(files, cert)
	}
	store := tlsprov.Store{Paths: a.Paths}
	for cert, content := range files {
		if _, err := os.Lstat(store.Dir(cert)); err == nil {
			a.printf("= certificat %s déjà présent : celui de l'archive n'est pas restauré\n", cert)
			continue
		}
		m := tlsprov.Material{Certificate: content[tlsprov.FileCert], Chain: content[tlsprov.FileChain],
			PrivateKey: content[tlsprov.FileKey]}
		if len(m.Certificate) == 0 || len(m.PrivateKey) == 0 {
			continue
		}
		if err := store.Write(cert, m); err != nil {
			a.warnf("⚠ restauration du certificat %s : %v\n", cert, err)
			missing = append(missing, restoreGap{fmt.Sprintf("certificat %s non restauré (%v)", cert, err),
				"l'apply en émet un nouveau, sinon : ogs cert renew " + p.ID()})
			continue
		}
		a.printf("✓ certificat restauré : %s\n", cert)
	}
	return missing
}

// keystoreSecret dit si un secret de l'archive va au keystore à la
// restauration : le fichier d'environnement et les certificats ont leur
// emplacement propre.
func keystoreSecret(name string) bool {
	return name != appprov.EnvFileSecret && name != envFileKeptSecret && !strings.HasPrefix(name, certSecretPrefix)
}

// restorableSecret dit si un secret de l'archive retourne au keystore du projet
// restauré. L'archive est faite avant les retraits : elle porte les preuves de
// propriété (D91, D93) de ressources que la suppression a ensuite détruites — un
// ancien compte, un vhost remplacé. Réinjectées, elles faisaient d'un homonyme
// créé depuis par l'administrateur une ressource d'ogs, que l'apply de la
// restauration retirait (D100). Seules reviennent donc les preuves de ce que la
// déclaration restaurée nomme : l'apply recrée ces ressources avec leur mot de
// passe d'origine, et les reconnaît siennes. Ce que la suppression a laissé en
// place, faute de pouvoir le retirer, a gardé sa preuve au keystore : rien n'est
// perdu. La marque d'un compte déplacé (« -moved/ ») ne revient jamais : elle
// désignerait, sous l'ancien hôte, un compte que la suppression a retiré.
func restorableSecret(p *model.Project, name string) bool {
	if !keystoreSecret(name) {
		return false
	}
	if strings.Contains(name, "-moved/") {
		return false
	}
	if !isProof(name) {
		return true
	}
	declared := map[string]bool{}
	for _, d := range p.Spec.Databases {
		engines := []string{d.Engine}
		if d.Engine == "mysql" || d.Engine == "mariadb" {
			engines = []string{"mysql", "mariadb"}
		}
		for _, e := range engines {
			declared[e+"/"+d.User] = true
			for _, u := range d.ExtraUsers {
				declared[e+"/"+u.User] = true
			}
		}
	}
	if a := p.Spec.AMQP; a != nil {
		declared["amqp/"+a.User] = true
		declared["amqp-vhost/"+a.Vhost] = true
		declared["amqp-declared/"+a.Vhost] = true
	}
	return declared[name]
}

// releaseAllocations libère les numéros du projet dont la ressource a quitté le
// disque : pool et unité applicative retirés (D100). Un fichier resté en place —
// laissé parce qu'ogs ne l'a pas écrit, ou d'un retrait en échec — sert encore
// son numéro : le libérer le ferait attribuer à un autre projet, dont le vhost
// relaierait alors vers ce qui tourne encore (D94).
func (a *App) releaseAllocations(provs *providers, p *model.Project, warn func(format string, args ...any)) {
	if a.Store == nil {
		return
	}
	list, err := a.Store.ProjectAllocations(p.ID())
	if err != nil {
		warn("numéros attribués : %v", err)
		return
	}
	for _, alloc := range list {
		var path string
		switch {
		case strings.HasPrefix(alloc.Scope, "php-"):
			path = a.Paths.PHPPoolFile(strings.TrimPrefix(alloc.Scope, "php-"), alloc.Name)
		case strings.HasPrefix(alloc.Scope, "app-") && alloc.Name == appprov.ServiceName:
			path = provs.App.UnitPath(p)
		default:
			// Une plage inconnue : rien ne dit ce qui sert le numéro.
			continue
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			continue
		}
		if err := a.Store.Release(alloc.Scope, alloc.Project, alloc.Name); err != nil {
			warn("numéro %d (%s) : %v", alloc.Number, alloc.Scope, err)
		}
	}
}

// proofPrefixes sont les secrets qui prouvent qu'une ressource d'un moteur est à
// ogs (D91, D93) : leur sort revient au provider qui juge la ressource — il les
// retire avec elle, et les garde tant qu'elle reste.
var proofPrefixes = []string{"mysql/", "mariadb/", "postgres/", "amqp/", "amqp-vhost/", "amqp-declared/"}

func isProof(name string) bool {
	for _, prefix := range proofPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// purgeSecrets retire du keystore les secrets du projet qui ne prouvent plus
// rien (D100). L'archive en garde la copie ; --purge, aucune. Restés, ils
// passaient à tout projet recréé sous le même identifiant — la clé DKIM, que
// celui-ci aurait publiée sans le savoir, par exemple.
//
// Les preuves de propriété ne sont pas jugées ici : chacune part avec sa
// ressource, retirée par son provider, et reste si la ressource reste — retirée
// avant, la ressource deviendrait étrangère, et ogs ne la reprendrait plus
// jamais. Sous --keep-data, rien ne part : bases et files restent, avec ce qui
// les prouve, et la clé DKIM avec le contenu. keepDKIM garde les clés quand
// leurs fichiers n'ont pas pu être retirés.
func (a *App) purgeSecrets(p *model.Project, keepDKIM bool, removed *[]string,
	warn func(format string, args ...any)) {
	if a.Store == nil {
		return
	}
	names, err := a.Store.SecretNames(p.ID())
	if err != nil {
		warn("secrets : %v", err)
		return
	}
	count := 0
	for _, name := range names {
		if isProof(name) || (keepDKIM && strings.HasPrefix(name, "dkim/")) {
			continue
		}
		if err := a.Store.DeleteSecret(p.ID(), name); err != nil {
			warn("secret %s : %v", name, err)
			continue
		}
		count++
	}
	if count > 0 {
		*removed = append(*removed, fmt.Sprintf("%d secret(s) du keystore", count))
	}
	// Ce qui reste prouve une ressource encore en place : le dire, sans quoi
	// l'exploitant croirait le keystore vidé.
	if rest, err := a.Store.SecretNames(p.ID()); err == nil && len(rest) > 0 {
		a.printf("= secrets conservés au keystore, avec la ressource qu'ils prouvent : %s — le projet "+
			"n'existant plus, ils y restent jusqu'au retrait de leur ressource ; un projet recréé sous "+
			"l'identifiant %s les reprendrait\n", strings.Join(rest, ", "), p.ID())
	}
}
