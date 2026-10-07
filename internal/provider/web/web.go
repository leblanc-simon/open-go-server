package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/app"
	"leblanc.io/open-go-server/internal/provider/php"
	"leblanc.io/open-go-server/internal/provider/system"
	tlsprov "leblanc.io/open-go-server/internal/provider/tls"
	"leblanc.io/open-go-server/internal/template"
)

// Provider réconcilie les vhosts d'un projet.
type Provider struct {
	// LogsRoot est la racine des journaux des projets (D71), seule entrée du
	// complément d'unité. Vide, les répertoires des projets sont énumérés.
	LogsRoot string

	Paths     config.Paths
	Exec      exec.Executor
	Templates *template.Set
	Engine    Engine
	PHP       *php.Provider
	// Certificate résout le certificat que sert un vhost — celui qu'il désigne,
	// sinon celui du projet (D102) : sa référence, ce que couvre le matériel
	// présent, ce que couvrira la demande. Le vhost n'est écrit en TLS que si le
	// certificat existe — le contraire empêcherait le serveur web de démarrer — et
	// couvre l'un de ses noms (vueTLSDe).
	Certificate func(*model.Project, model.VHost) tlsprov.Coverage
	// CertDir localise le matériel d'un certificat.
	CertDir func(ref string) string
	// CertificatePair vérifie la paire d'un répertoire de certificat que désigne
	// un vhost activé, et l'achève avec repair si une écriture interrompue le
	// permet (tls.Provider.CheckPair) : avant tout rechargement, toute
	// validation, tout redémarrage (D110).
	CertificatePair func(ctx context.Context, dir string, repair bool) (PairCheck, error)
	// Listeners énumère les écoutes TCP de la machine (app.ReadListeners), pour
	// le port du vhost de supervision (R2-37, D118). Nil : /proc/net, ou rien en
	// mode simulé.
	Listeners func() ([]app.Listener, error)
	// ProxyTarget résout la cible d'un vhost de proxy : socket ou adresse locale.
	ProxyTarget func(*model.Project) (string, bool)
	Now         func() time.Time
	// Force autorise l'écrasement d'un fichier qu'ogs n'a pas écrit, après
	// sauvegarde horodatée (§6.4). Sans lui, le refus n'aurait aucune porte de
	// sortie : un site adopté, dont le vhost ne porte pas la marque d'ogs, ne
	// pourrait jamais être appliqué une première fois.
	Force bool
}

// refusStep produit l'étape qui refuse d'écraser un fichier qu'ogs n'a pas
// écrit. Le message nomme la porte de sortie — elle existe désormais — et
// distingue les deux causes : un fichier « non géré » n'a jamais porté la marque
// d'ogs, c'est ce que rencontre un site adopté à son premier apply ; un fichier
// « modifié hors ogs » a été retouché après avoir été généré.
func refusStep(resource, summary, path, backups string, state fs.State) *engine.Step {
	libelle, motif := "modifié hors ogs", "a été modifié en dehors d'ogs"
	if state == fs.StateForeign {
		libelle, motif = "non géré par ogs", "existe déjà sans porter la marque d'ogs"
	}
	s := engine.NewStep(resource, engine.Update, summary+" — fichier "+libelle)
	s.Apply = func(context.Context) error {
		return fmt.Errorf("%s %s : relancer avec --force pour l'écraser "+
			"(l'original sera sauvegardé dans %s)", path, motif, backups)
	}
	return s
}

// Name identifie le provider.
func (p *Provider) Name() string { return "web" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Contextes de surcharge d'un vhost (D63). Les noms sont ceux du rôle, non ceux
// d'un moteur : « vhost » est le server de nginx et le VirtualHost d'Apache,
// « docroot » le location / de l'un et le Directory du docroot de l'autre.
const (
	IncludeVHost   = "vhost"
	IncludeDocroot = "docroot"
)

// VHostPlan est le rendu attendu d'un vhost.
type VHostPlan struct {
	Name        string
	Path        string
	EnabledPath string
	Content     []byte
	State       fs.State
	Enabled     bool
	ServerName  string
	Domains     []string
	// IncludeDirs énumère les répertoires de surcharge que le vhost référence.
	IncludeDirs []string
}

// Observed est l'état réel des vhosts.
type Observed struct {
	// Stale énumère les vhosts du projet devenus inutiles.
	Stale []string
	// Conflicts associe un domaine à la configuration étrangère qui le sert déjà.
	Conflicts map[string]string
}

// Observe inspecte les vhosts existants et cherche les conflits de domaine.
//
// Un fichier n'est obsolète que si son en-tête le dit écrit par ogs pour ce
// projet, et comme l'un de ses vhosts (fs.ManagedResource) — jamais d'après son
// nom. L'attribution par préfixe (« <id>_* ») faisait retirer au premier apply
// de « shop » le vhost de son voisin « shop_api », et à un projet adopté les
// fichiers d'origine qu'un nom tronqué lui rattachait, le site voisin compris.
// La ressource écarte le vhost de supervision, dont l'en-tête nomme « parc » —
// un identifiant valide. Un fichier que rien ne rattache au projet reste une
// configuration étrangère : elle n'est jamais retirée, et si elle sert l'un de
// ses domaines, le plan s'arrête sur le conflit.
func (p *Provider) Observe(_ context.Context, proj *model.Project) (Observed, error) {
	obs := Observed{Conflicts: map[string]string{}}
	wanted := map[string]bool{}
	for _, vh := range p.vhosts(proj) {
		wanted[filepath.Join(p.Engine.AvailableDir(), p.vhostFile(proj, vh))] = true
	}
	entries, err := os.ReadDir(p.Engine.AvailableDir())
	if err != nil && !os.IsNotExist(err) {
		return obs, err
	}
	// Les domaines servis sont ceux de spec.domains et ceux des vhosts, que rien
	// n'oblige à y figurer (R4-02, D106) ; les noms se comparent sans casse ni
	// point final, comme le moteur les compare (canonicalName).
	ownDomains := map[string]bool{}
	for _, d := range proj.Spec.Domains {
		ownDomains[canonicalName(d.Name)] = true
	}
	for _, vh := range p.vhosts(proj) {
		for _, d := range vh.Domains {
			ownDomains[canonicalName(d)] = true
		}
	}
	// Configuration étrangère : on cherche seulement si elle sert l'un de nos
	// domaines, pour refuser de créer un vhost qui ne serait jamais atteint.
	conflits := func(path string, data []byte) {
		for _, name := range servedNames(p.Engine.Name(), string(data)) {
			if name = canonicalName(name); ownDomains[name] {
				obs.Conflicts[name] = path
			}
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(p.Engine.AvailableDir(), e.Name())
		if wanted[path] {
			// Le fichier voulu relève de Build et des cinq états : c'est là qu'un
			// vhost adopté, qui ne porte pas encore la marque d'ogs, est refusé
			// sans --force puis repris avec sauvegarde.
			continue
		}
		// Un fichier illisible ne peut pas être attribué : il n'est pas retiré.
		owner, resource, err := fs.ManagedResource(path)
		if err == nil && owner == proj.ID() && strings.HasPrefix(resource, "web/vhost/") {
			obs.Stale = append(obs.Stale, path)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		conflits(path, data)
	}
	p.loadedConflicts(proj, wanted, conflits)
	sort.Strings(obs.Stale)
	return obs, nil
}

// OwnedVHostFiles énumère les fichiers de sites-available que leur en-tête
// attribue au projet comme vhost (fs.ManagedResource), que sa déclaration les
// nomme encore ou non. La suppression en a besoin (D119) : le nom d'un vhost de
// redirection dépend de sa cible, que l'exploitant édite, et recalculer les
// fichiers depuis la seule déclaration laissait actif, après « project
// delete », un vhost écrit pour une ancienne cible — il désignait des journaux
// et un certificat effacés, et le rechargement suivant faisait tomber le parc.
func (p *Provider) OwnedVHostFiles(id string) []string {
	entries, err := os.ReadDir(p.Engine.AvailableDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		owner, resource, err := fs.ManagedResource(filepath.Join(p.Engine.AvailableDir(), e.Name()))
		if err == nil && owner == id && strings.HasPrefix(resource, "web/vhost/") {
			out = append(out, e.Name())
		}
	}
	return out
}

// loadedConflicts étend la détection aux fichiers que le moteur charge hors de
// sites-available (Engine.LoadedConfigs) : conf.d, conf-enabled, et les fichiers
// ordinaires de sites-enabled (R2-44, D118). Un site servi de là n'était pas vu,
// et le vhost d'ogs, écrit et activé, n'était jamais atteint — le moteur retient
// la première déclaration d'un nom, et conf.d comme conf-enabled sont chargés
// avant sites-enabled.
//
// Lecture seule : rien de ce qui est trouvé ici n'est attribué au projet ni
// retiré (D76) — seul sites-available, où ogs écrit, porte des fichiers
// obsolètes. Le risque est le faux conflit, qui refuserait un site qui marchait :
// un lien d'activation vers sites-available est écarté (le fichier visé y est
// déjà jugé, le vhost du projet compris), comme un fichier caché, que glob(3)
// n'inclut pas, et un fichier qu'ogs a écrit pour ce projet.
func (p *Provider) loadedConflicts(proj *model.Project, wanted map[string]bool,
	conflits func(string, []byte)) {
	available, err := filepath.EvalSymlinks(p.Engine.AvailableDir())
	if err != nil {
		available = p.Engine.AvailableDir()
	}
	for _, pattern := range p.Engine.LoadedConfigs() {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, path := range matches {
			if strings.HasPrefix(filepath.Base(path), ".") {
				continue
			}
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				// Lien rompu : il ne sert aucun nom — nginx refuserait d'ailleurs
				// toute sa configuration, ce que sa validation dira.
				continue
			}
			if filepath.Dir(target) == available || wanted[target] {
				continue
			}
			info, err := os.Stat(target)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if owner, resource, err := fs.ManagedResource(target); err == nil &&
				owner == proj.ID() && strings.HasPrefix(resource, "web/vhost/") {
				continue
			}
			data, err := readBounded(target, maxConfigRead)
			if err != nil {
				continue
			}
			conflits(path, data)
		}
	}
}

// maxConfigRead borne la lecture d'une configuration étrangère : un fichier de
// configuration ne pèse jamais un mégaoctet, et la détection n'a pas à lire en
// mémoire ce qu'on aurait déposé là.
const maxConfigRead = 1 << 20

// readBounded lit au plus max octets d'un fichier ordinaire.
func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

// apacheHost retire d'un nom d'Apache le schéma et le port qu'il admet
// (« https://shop.fr:443 » → « shop.fr »), crochets d'une adresse IPv6 compris.
func apacheHost(name string) string {
	if i := strings.Index(name, "://"); i >= 0 {
		name = name[i+3:]
	}
	if strings.HasPrefix(name, "[") {
		if i := strings.Index(name, "]"); i >= 0 {
			return name[:i+1]
		}
		return name
	}
	if i := strings.LastIndex(name, ":"); i >= 0 {
		return name[:i]
	}
	return name
}

// canonicalName ramène un nom servi à la forme que le moteur compare : sans
// casse ni point final. Le préfixe « www. » est gardé, à la différence de la
// clé d'unicité du parc (naming.NormalizeHost) : une configuration étrangère
// qui sert www.shop.fr ne reçoit pas les requêtes de shop.fr, et la refuser
// bloquerait un plan sans détournement.
func canonicalName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// vhosts renvoie les vhosts déclarés, ou ceux déduits du projet
// (model.Project.VHosts).
func (p *Provider) vhosts(proj *model.Project) []model.VHost {
	return proj.VHosts()
}

// vhostName traduit le nom logique d'un vhost en nom de base de son fichier
// (model.Project.VHostName).
func (p *Provider) vhostName(proj *model.Project, vh model.VHost) string {
	return proj.VHostName(vh)
}

// vhostFile est le nom du fichier d'un vhost dans sites-available et
// sites-enabled (model.Project.VHostFile) : le nom réel d'un vhost adopté, que
// le nom suivi de l'extension ne désigne pas toujours.
func (p *Provider) vhostFile(proj *model.Project, vh model.VHost) string {
	return proj.VHostFile(vh)
}

// checkFile refuse le nom réel d'un vhost que le moteur ne chargerait pas.
// Apache n'inclut que sites-enabled/*.conf, là où nginx prend tout
// sites-enabled/* : un « file: shop.fr » sur Apache donnerait un vhost écrit,
// activé, et jamais servi, sans un mot. L'import renseigne toujours le nom réel,
// seule une déclaration éditée à la main est en cause ; et la validation du
// projet ne peut pas le voir, le moteur étant celui de la machine (D73).
func (p *Provider) checkFile(vh model.VHost) error {
	if vh.File == "" || p.Engine.Name() != "apache" || strings.HasSuffix(vh.File, model.VHostExt) {
		return nil
	}
	return &FileError{VHost: vh.Name, File: vh.File}
}

// conflictHint dit quoi faire d'une configuration étrangère qui sert déjà un
// domaine du projet. Renvoyer à « ogs import » un projet déjà adopté le ferait
// tourner en rond : c'est alors, le plus souvent, le fichier d'origine d'un
// vhost qu'une adoption antérieure n'a pas relevé par son nom exact — le scan
// réduisait « shop.fr » à « shop », et le projet cherche « shop.conf ». Le
// désigner suffit ; ogs ne le devine pas, « shop.fr » et « shop.de » ayant pu
// donner le même nom.
//
// Hors de sites-available (conf.d, conf-enabled, un fichier ordinaire de
// sites-enabled, D118), l'adoption ne reprendrait rien : l'import ne lit que
// sites-available. Le remède est alors de retirer le bloc qui sert ce nom.
func (p *Provider) conflictHint(proj *model.Project, path string) string {
	if filepath.Dir(path) != filepath.Clean(p.Engine.AvailableDir()) {
		return fmt.Sprintf("retirer ou désactiver le bloc qui sert ce nom dans %s — le moteur le charge avec "+
			"les vhosts et retient la première déclaration d'un nom ; l'adoption ne lit que %s",
			filepath.Base(path), p.Engine.AvailableDir())
	}
	if proj.Metadata.Labels["adopted"] == "true" {
		return fmt.Sprintf("si c'est le fichier d'origine d'un vhost adopté, le désigner par "+
			"« file: %s » dans spec.web.vhosts ; sinon, retirer cette configuration", filepath.Base(path))
	}
	return "retirer cette configuration ou adopter le site avec « ogs import »"
}

// classifyConflicts juge les conflits d'un projet (D118). Un conflit refuse la
// création d'un vhost neuf, qui ne serait jamais atteint. Pour un vhost qu'ogs a
// déjà écrit et activé pour ce projet, le site marche : le plan n'est plus
// refusé en dur. Reste à savoir laquelle des deux déclarations le moteur sert —
// la première chargée :
//   - l'étrangère vient avant (conf.d, conf-enabled, ou un nom qui précède dans
//     sites-enabled) : c'est elle qui est servie, avertissement non bloquant,
//     code 6 (warnings, par vhost) ;
//   - elle vient après, ou n'est pas chargée du tout (un sites-available sans
//     lien) : configuration morte, une note du plan, que doctor rappelle ;
//   - dans le doute, le code 6 reste.
func (p *Provider) classifyConflicts(proj *model.Project, obs Observed) (map[string][]string, []string, error) {
	warnings := map[string][]string{}
	var notes []string
	domains := make([]string, 0, len(obs.Conflicts))
	for d := range obs.Conflicts {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	for _, domain := range domains {
		path := obs.Conflicts[domain]
		vh, ok := p.vhostServing(proj, domain)
		if !ok || !p.vhostActive(proj, vh) {
			return nil, nil, fmt.Errorf("le domaine %s est déjà servi par %s : %s", domain, path,
				p.conflictHint(proj, path))
		}
		ours := filepath.Join(p.Engine.AvailableDir(), p.vhostFile(proj, vh))
		okey, oknown := p.loadKey(ours)
		fkey, fknown := p.loadKey(path)
		switch {
		case fknown && fkey == "":
			notes = append(notes, fmt.Sprintf("vhost %s : %s est aussi déclaré par %s, que le moteur ne charge "+
				"pas — configuration morte, à retirer (D118)", vh.Name, domain, path))
		case fknown && oknown && okey != "" && fkey > okey:
			notes = append(notes, fmt.Sprintf("vhost %s : %s est aussi déclaré par %s, chargé après le vhost "+
				"d'ogs, qui est celui servi — déclaration sans effet, à retirer (D118)", vh.Name, domain, path))
		default:
			warnings[vh.Name] = append(warnings[vh.Name], fmt.Sprintf("%s est servi par %s, chargé avant le "+
				"vhost d'ogs (%s)", domain, path, p.conflictHint(proj, path)))
		}
	}
	return warnings, notes, nil
}

// ConflictReport rend, pour doctor, les conflits d'un projet : déclarations
// servies avant le vhost d'ogs, déclarations mortes, et le refus de créer.
func (p *Provider) ConflictReport(proj *model.Project, obs Observed) (served, dead []string, refusal error) {
	warnings, notes, err := p.classifyConflicts(proj, obs)
	names := make([]string, 0, len(warnings))
	for n := range warnings {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, w := range warnings[n] {
			served = append(served, "vhost "+n+" : "+w)
		}
	}
	return served, notes, err
}

// ConflictNotes rend les informations du plan sur les déclarations mortes
// d'un domaine du projet (classifyConflicts), à porter dans engine.Plan.Notes.
func (p *Provider) ConflictNotes(proj *model.Project, obs Observed) []string {
	_, notes, _ := p.classifyConflicts(proj, obs)
	return notes
}

// HSTSNotes rend les informations du plan sur les options HSTS non reconnues
// des vhosts du projet, à porter dans engine.Plan.Notes (D118, D119). Ce ne sont
// ni des étapes ni des écarts : le vhost est servi tel que rendu, la politique
// complète, et le code de retour n'en dépend pas. Portées par une étape NoOp,
// elles n'étaient montrées nulle part — ni le plan ni le JSON ne retiennent une
// étape qui ne change rien.
func (p *Provider) HSTSNotes(proj *model.Project) []string {
	var notes []string
	for _, vh := range p.vhosts(proj) {
		if problems := HSTSOptionProblems(vh.Options); len(problems) > 0 {
			notes = append(notes, fmt.Sprintf("vhost %s : %s (D119)",
				p.vhostName(proj, vh), strings.Join(problems, " ; ")))
		}
	}
	return notes
}

// loadKey situe un fichier dans l'ordre de chargement du moteur : conf.d ou
// conf-enabled d'abord (« 0 »), puis sites-enabled par nom (« 1<nom> ») — un
// fichier de sites-available l'est par le premier lien chargé qui le désigne.
// Une clé vide dit un fichier que le moteur ne charge pas ; known faux, un
// emplacement dont l'ordre n'est pas connu.
func (p *Provider) loadKey(path string) (key string, known bool) {
	patterns := p.Engine.LoadedConfigs()
	dir := filepath.Dir(path)
	loaded := func(name string) bool {
		if strings.HasPrefix(name, ".") {
			return false
		}
		ok, _ := filepath.Match(filepath.Base(patterns[len(patterns)-1]), name)
		return ok
	}
	switch dir {
	case filepath.Dir(patterns[0]):
		return "0", true
	case filepath.Clean(p.Engine.EnabledDir()):
		return "1" + filepath.Base(path), true
	case filepath.Clean(p.Engine.AvailableDir()):
		first := ""
		for _, link := range p.linksTo(path) {
			if name := filepath.Base(link); loaded(name) && (first == "" || name < first) {
				first = name
			}
		}
		if first == "" {
			return "", true
		}
		return "1" + first, true
	}
	return "", false
}

// vhostServing rend le vhost du projet qui sert domain.
func (p *Provider) vhostServing(proj *model.Project, domain string) (model.VHost, bool) {
	for _, vh := range p.vhosts(proj) {
		for _, d := range vh.Domains {
			if canonicalName(d) == domain {
				return vh, true
			}
		}
	}
	return model.VHost{}, false
}

// vhostActive dit si ogs a déjà écrit le fichier d'un vhost pour ce projet — sa
// marque le dit (D76) — et si un lien de sites-enabled le désigne.
func (p *Provider) vhostActive(proj *model.Project, vh model.VHost) bool {
	path := filepath.Join(p.Engine.AvailableDir(), p.vhostFile(proj, vh))
	owner, _, err := fs.ManagedResource(path)
	return err == nil && owner == proj.ID() && len(p.linksTo(path)) > 0
}

// FileError signale un nom de fichier de vhost que le moteur de la machine ne
// chargerait pas. C'est une faute de déclaration, que l'appelant rend comme
// telle (code 3), et non un conflit.
type FileError struct {
	VHost, File string
}

func (e *FileError) Error() string {
	return fmt.Sprintf("vhost %s : le fichier %s ne serait jamais chargé — Apache n'inclut que "+
		"sites-enabled/*%s", e.VHost, e.File, model.VHostExt)
}

// VHostNames énumère les noms de vhost d'un projet, qui nomment leurs
// répertoires de surcharge (RemoveIncludes). Leurs fichiers se nomment par
// VHostFiles : le nom suivi de l'extension ne désigne pas celui d'un vhost
// adopté sans extension, que la suppression laisserait derrière elle.
func (p *Provider) VHostNames(proj *model.Project) []string {
	vhosts := p.vhosts(proj)
	out := make([]string, 0, len(vhosts))
	for _, vh := range vhosts {
		out = append(out, p.vhostName(proj, vh))
	}
	return out
}

// VHostFiles énumère les noms réels des fichiers de vhost d'un projet, dans
// sites-available comme dans sites-enabled. Il diffère de FileName(VHostNames)
// pour un vhost adopté dont le fichier ne porte pas l'extension du moteur.
func (p *Provider) VHostFiles(proj *model.Project) []string {
	vhosts := p.vhosts(proj)
	out := make([]string, 0, len(vhosts))
	for _, vh := range vhosts {
		out = append(out, p.vhostFile(proj, vh))
	}
	return out
}

// Build calcule le rendu attendu d'un vhost.
func (p *Provider) Build(proj *model.Project, vh model.VHost) (*VHostPlan, error) {
	at, err := proj.AppType()
	if err != nil {
		return nil, err
	}
	name := p.vhostName(proj, vh)
	tmpl := vh.Template
	if tmpl == "" {
		tmpl = at.VHostTemplate
	}
	if len(vh.Domains) == 0 {
		return nil, fmt.Errorf("vhost %s : aucun domaine", name)
	}

	docroot := vh.Docroot
	if docroot == "" {
		docroot = proj.Spec.Layout.Docroot
	}
	frontController := at.FrontController
	if frontController == "" {
		frontController = "index.php"
	}

	// Le TLS n'est activé dans le rendu que si le certificat présent couvre l'un
	// des noms du vhost. Sinon le vhost est produit en HTTP seul : c'est aussi ce
	// qui permet au challenge HTTP-01 d'aboutir avant la première émission.
	cov := p.coverage(proj, vh)
	vue := vueTLSDe(proj, vh, wantsTLS(proj, vh), cov.Serves)
	certDir := filepath.Join(p.Paths.ACMEDir(), "certificates", name)
	if cov.Ref != "" && p.CertDir != nil {
		certDir = p.CertDir(cov.Ref)
	}

	ctx := map[string]any{
		"Project":         proj.ID(),
		"Name":            name,
		"AppKind":         string(proj.Spec.App.Kind),
		"ServerName":      vue.serverName,
		"Aliases":         vue.aliases,
		"Docroot":         docroot,
		"LogDir":          proj.Spec.Layout.LogDir,
		"FrontController": frontController,
		// La racine du projet borne le suivi des liens : tout composant sous elle,
		// docroot compris, est à la main de l'utilisateur (D51).
		"Root": proj.Spec.Layout.Root,
		// Deux valeurs distinctes : Apache aliase le répertoire des jetons, nginx
		// prend le webroot et y accole l'URI.
		"ACMEChallenge":     p.Paths.ACMEChallenge(),
		"ACMEChallengeDir":  p.Paths.ACMEChallengeTokens(),
		"RobotsNoindexFile": p.Paths.RobotsNoindexFile(),
		"TLS":               vue.on,
		// HSTS reste vrai quand la politique est active : un template surchargé qui
		// écrit lui-même l'en-tête sous « if .HSTS » garde son sens. HSTSHeader
		// est la valeur que portent les templates d'ogs, retrait compris (D119).
		"HSTS":              hstsActive(vh.Options),
		"HSTSHeader":        hstsHeader(vh.Options),
		"CertDir":           certDir,
		"Noindex":           proj.IsPrep(),
		"ClientMaxBodySize": optionString(vh.Options, "clientMaxBodySize", "32M"),
		"Blocks":            vh.Blocks,
		// Répertoires de surcharge (D63) : le template décide lesquels il inclut
		// réellement, et c'est le rendu qui fait foi — les répertoires créés sont
		// ceux que le vhost référence, pas ceux qu'une table dirait.
		"IncludeVHost":   p.IncludeDir(name, IncludeVHost),
		"IncludeDocroot": p.IncludeDir(name, IncludeDocroot),
		"RedirectTarget": optionString(vh.Options, "target", vh.Domains[0]),
		"RedirectScheme": vue.redirectScheme,
		"ProxyTarget":    optionString(vh.Options, "proxyTarget", ""),
		"FPMHandler":     "",
		"FPMPass":        "",
	}

	if proj.Spec.App.Kind == model.KindPrestashop {
		// Les règles des .htaccess livrés suivent la branche en place (D99) : un
		// changement de version change le rendu, que le plan suivant réécrit.
		// Seule la branche entre dans le rendu : une version corrective ne réécrit
		// pas le vhost.
		branche := BranchePrestashop(VersionPrestashop(proj))
		ctx["PrestashopIndeterminee"] = branche == ""
		if branche == "" {
			branche = BrancheParDefaut
		}
		ctx["Prestashop"] = branche
		ctx["MarquePrestashop"] = MarquePrestashop(branche)
	}

	if at.NeedsPHP {
		pool, err := p.poolFor(proj, vh)
		if err != nil {
			return nil, err
		}
		ref := p.Engine.FPMReference(pool.Listen, pool.Unix)
		ctx["FPMHandler"] = ref
		ctx["FPMPass"] = ref
	}
	if tmpl == "proxy" {
		target := optionString(vh.Options, "proxyTarget", "")
		if target == "" && p.ProxyTarget != nil {
			resolved, unix := p.ProxyTarget(proj)
			if resolved == "" {
				return nil, fmt.Errorf("vhost %s : aucune cible de proxy résolue", name)
			}
			target = p.Engine.ProxyReference(resolved, unix)
		}
		if target == "" {
			return nil, fmt.Errorf("vhost %s : préciser la cible de proxy (options.proxyTarget)", name)
		}
		ctx["ProxyTarget"] = target
	}

	body, err := p.Templates.Render(p.Engine.TemplateName(tmpl), ctx)
	if err != nil {
		return nil, err
	}
	content := fs.Render(fs.Header{
		Project:   proj.ID(),
		Resource:  "web/vhost/" + name,
		Generated: p.now(),
		Template:  p.Templates.OverridePath(p.Engine.TemplateName(tmpl)),
	}, body, p.Engine.CommentPrefix())

	path := filepath.Join(p.Engine.AvailableDir(), p.vhostFile(proj, vh))
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	enabledPath := filepath.Join(p.Engine.EnabledDir(), p.vhostFile(proj, vh))
	_, enabledErr := os.Lstat(enabledPath)
	return &VHostPlan{
		Name: name, Path: path, EnabledPath: enabledPath, Content: content,
		State: state, Enabled: enabledErr == nil, ServerName: vue.serverName, Domains: vh.Domains,
		IncludeDirs: p.includeDirs(content),
	}, nil
}

// wantsTLS dit si un vhost demande le TLS : oui par défaut, sauf renoncement du
// projet ou du vhost.
func wantsTLS(proj *model.Project, vh model.VHost) bool {
	return !proj.Spec.Web.TLSDisabled() && (vh.TLS == nil || vh.TLS.Enabled)
}

// coverage résout le certificat d'un vhost, vide s'il n'en demande pas.
func (p *Provider) coverage(proj *model.Project, vh model.VHost) tlsprov.Coverage {
	if !wantsTLS(proj, vh) || p.Certificate == nil {
		return tlsprov.Coverage{}
	}
	return p.Certificate(proj, vh)
}

// vueTLS est ce que le certificat décide dans le rendu d'un vhost.
type vueTLS struct {
	// on rend le bloc 443.
	on bool
	// serverName est le nom canonique : celui du ServerName, vers lequel le port
	// 80 redirige ; aliases, les autres noms, dans l'ordre déclaré.
	serverName string
	aliases    []string
	// redirectScheme est le schéma de la cible d'un vhost de redirection.
	redirectScheme string
}

// vueTLSDe calcule le rendu TLS d'un vhost selon ce que couvre un certificat —
// le présent pour le rendu, le demandé pour savoir si une émission le changera.
//
// Le bloc 443 est rendu dès que le certificat couvre **l'un** des noms du vhost,
// et non tous : un nom ajouté attend sa réémission sans retirer le HTTPS des
// autres. Le critère n'est pas le seul domaine principal : quand celui-ci change
// en gardant l'ancien parmi les alias, c'est l'ancien que le certificat couvre,
// et il faut continuer de le servir. D'où le nom canonique, qui est le premier
// nom couvert — la redirection du port 80 vise ainsi toujours un nom que le
// certificat présente, jamais une erreur de nom. Les noms en attente restent
// déclarés dans les deux blocs : le port 80 les sert pour le challenge HTTP-01,
// avant toute redirection, et le 443 les présentera au rechargement qui suit
// l'émission.
//
// Une redirection ne vise https que si sa cible, quand c'est un nom du projet,
// est couverte : un domaine principal remplacé qui devient redirection doit
// continuer d'aboutir pendant l'émission du nouveau, fût-ce en HTTP. Une cible
// étrangère est visée en https dès que le TLS est voulu : ce que couvre notre
// certificat ne dit rien d'elle — une préproduction dont le wildcard ne couvre
// pas le nom redirigé l'enverrait sinon en http.
func vueTLSDe(proj *model.Project, vh model.VHost, want bool, covers func(string) bool) vueTLS {
	v := vueTLS{serverName: vh.Domains[0], aliases: vh.Domains[1:], redirectScheme: "http"}
	if want {
		for i, d := range vh.Domains {
			if !covers(d) {
				continue
			}
			v.on = true
			if i > 0 {
				v.serverName = d
				v.aliases = append(append([]string{}, vh.Domains[:i]...), vh.Domains[i+1:]...)
			}
			break
		}
	}
	target := optionString(vh.Options, "target", vh.Domains[0])
	switch {
	case !want:
	case ownDomain(proj, target):
		if covers(target) {
			v.redirectScheme = "https"
		}
	default:
		v.redirectScheme = "https"
	}
	return v
}

// equal compare deux rendus TLS.
func (v vueTLS) equal(o vueTLS) bool {
	return v.on == o.on && v.serverName == o.serverName &&
		v.redirectScheme == o.redirectScheme && strings.Join(v.aliases, " ") == strings.Join(o.aliases, " ")
}

// ownDomain dit si un nom est l'un des domaines du projet.
func ownDomain(proj *model.Project, name string) bool {
	for _, d := range proj.Spec.Domains {
		if strings.EqualFold(d.Name, name) {
			return true
		}
	}
	return false
}

// IncludeDir est le répertoire de surcharge d'un vhost pour un contexte donné.
func (p *Provider) IncludeDir(name, context string) string {
	return filepath.Join(p.Engine.IncludeRoot(), name, context)
}

// reIncludeDirective reconnaît les inclusions facultatives d'un répertoire de
// surcharge, dans la syntaxe de l'un ou l'autre moteur. Le masque est exigé :
// c'est lui qui rend l'inclusion facultative — nginx et Apache traitent
// l'absence de correspondance comme un silence, là où un chemin littéral absent
// est une erreur fatale. « include fastcgi_params » n'est pas concerné.
var reIncludeDirective = regexp.MustCompile(`(?m)^\s*(?:IncludeOptional|include)\s+(/\S+)/\*\.conf`)

// includeDirs énumère les répertoires de surcharge qu'un vhost référence.
//
// La liste est déduite du rendu, et non d'une table associant un template à ses
// contextes : les templates sont surchargeables, et une table finirait par
// mentir — ogs créerait un répertoire que le vhost n'inclut pas, où les fichiers
// déposés seraient ignorés en silence. Seuls les chemins sous la racine du
// moteur sont retenus : un include que l'exploitant ajoute ailleurs ne relève
// pas d'ogs.
func (p *Provider) includeDirs(content []byte) []string {
	root := p.Engine.IncludeRoot()
	var out []string
	seen := map[string]bool{}
	for _, m := range reIncludeDirective.FindAllStringSubmatch(string(content), -1) {
		dir := filepath.Clean(m[1])
		if dir != root && !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			continue
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// poolFor retrouve le pool associé à un vhost, afin d'en tirer le point d'écoute.
func (p *Provider) poolFor(proj *model.Project, vh model.VHost) (*php.PoolPlan, error) {
	if p.PHP == nil {
		return nil, fmt.Errorf("provider PHP indisponible : impossible de résoudre le pool du vhost")
	}
	want := vh.Pool
	if want == "" {
		want = model.MainPoolName
	}
	for _, pool := range proj.Spec.PHP.Pools {
		if pool.Name == want {
			return p.PHP.Build(proj, pool)
		}
	}
	return nil, fmt.Errorf("vhost %s : pool %q non déclaré", p.vhostName(proj, vh), want)
}

// Plan calcule les étapes relatives aux vhosts.
func (p *Provider) Plan(_ context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	actifs, _, err := p.classifyConflicts(proj, obs)
	if err != nil {
		return nil, err
	}
	var steps []*engine.Step
	for i, vh := range p.vhosts(proj) {
		if err := p.checkFile(vh); err != nil {
			return nil, err
		}
		bp, err := p.Build(proj, vh)
		if err != nil {
			return nil, err
		}
		// Le fichier d'un autre projet n'est jamais réécrit, --force compris : le
		// vhost « b2b » de shop s'écrirait dans shop_b2b.conf, le vhost principal
		// du projet shop_b2b, qui cesserait d'être servi.
		if err := fs.CheckOwner(bp.Path, proj.ID(),
			fmt.Sprintf("vhost « %s » de %s", vh.Name, proj.ID())); err != nil {
			return nil, err
		}
		steps = append(steps, p.vhostStep(proj, bp)...)
		if motifs := actifs[vh.Name]; len(motifs) > 0 {
			w := engine.NewStep("web/vhost/"+bp.Name+"/conflit", engine.Update,
				fmt.Sprintf("vhost %s : %d domaine(s) servi(s) par une autre déclaration", bp.Name, len(motifs)))
			w.Critical = false
			w.Apply = func(context.Context) error {
				return fmt.Errorf("le moteur retient la première déclaration d'un nom : %s", strings.Join(motifs, " ; "))
			}
			steps = append(steps, w)
		}
		if i == 0 {
			if s := p.htaccessStep(proj, bp); s != nil {
				steps = append(steps, s)
			}
		}
	}
	if s := redirectStep(proj); s != nil {
		steps = append(steps, s)
	}
	for _, stale := range obs.Stale {
		s, err := p.removeStep(stale, proj.ID())
		if err != nil {
			return nil, err
		}
		if s != nil {
			steps = append(steps, s)
		}
	}
	return steps, nil
}

// redirectStep annonce les cibles de redirection écartées (D119) : une cible
// qui n'est pas un nom d'hôte, ou une boucle de redirections, fait viser le
// premier domaine servi, comme avant que la cible soit honorée. L'étape est non
// critique — les vhosts sont écrits, l'apply rend le code 6 — et nomme la cible ;
// le refus n'est jamais au chargement, qui arrêterait tout le parc sur une
// déclaration jusque-là servie. Nil s'il n'y a rien à dire, ou si les vhosts sont
// déclarés : ils portent leur propre cible (options.target).
func redirectStep(proj *model.Project) *engine.Step {
	if len(proj.Spec.Web.VHosts) > 0 {
		return nil
	}
	problems := proj.RedirectProblems()
	if len(problems) == 0 {
		return nil
	}
	s := engine.NewStep("web/redirect/"+proj.ID(), engine.Update,
		"cible de redirection écartée : le premier domaine servi est visé (D119)")
	s.Critical = false
	s.Apply = func(context.Context) error {
		return fmt.Errorf("%s — corriger spec.domains[].target", strings.Join(problems, " ; "))
	}
	return s
}

// htaccessStep annonce, là où le vhost d'un Prestashop va être écrit, ce que le
// template ne reprend pas (D99) : sous Apache, ce que porte son .htaccess racine,
// qu'ogs ne lit plus (AllowOverride None) — une boutique en sous-répertoire ou à
// URI virtuelle tomberait en silence à l'application — ; sous les deux moteurs,
// une version de Prestashop indéterminée alors que le code est en place, servie
// avec les règles de la branche par défaut. L'étape est non critique — le vhost
// est écrit, l'apply rend le code 6 — et nomme la sortie. Nil s'il n'y a rien à
// dire, ou si le vhost reste tel quel : doctor rappelle alors le cas à chaque
// passage.
func (p *Provider) htaccessStep(proj *model.Project, bp *VHostPlan) *engine.Step {
	if proj.Spec.App.Kind != model.KindPrestashop {
		return nil
	}
	if bp.State != fs.StateAbsent && bp.State != fs.StateOutdated &&
		!((bp.State == fs.StateHandEdited || bp.State == fs.StateForeign) && p.Force) {
		return nil
	}
	var constats, remedes []string
	if p.Engine.Name() == "apache" {
		if c := ConstatsBoutiques(proj, false); len(c) > 0 {
			constats = append(constats, c...)
			remedes = append(remedes, fmt.Sprintf("les .htaccess du docroot %s ne sont plus lus sous Apache "+
				"(AllowOverride None, D99) : porter ces règles dans %s/*.conf (D63), sans quoi le site "+
				"ne sert plus ces URL", proj.Spec.Layout.Docroot, p.IncludeDir(bp.Name, IncludeDocroot)))
		}
	}
	if BranchePrestashop(VersionPrestashop(proj)) == "" && CodePrestashopPresent(proj) {
		constats = append(constats, "version de Prestashop indéterminée")
		remedes = append(remedes, fmt.Sprintf("ni src/Core/Version.php ni app/AppKernel.php — lus au "+
			"propriétaire du docroot, sans suivre de lien, 256 Kio au plus — n'en donnent une version 1.7, 8 ou 9 : le vhost "+
			"porte les règles de la %s (D99)", BrancheParDefaut))
	}
	if len(constats) == 0 {
		return nil
	}
	s := engine.NewStep("web/vhost/"+bp.Name+"/htaccess", engine.Update,
		fmt.Sprintf("vhost %s : %s (D99)", bp.Name, strings.Join(constats, ", ")))
	s.Critical = false
	s.Apply = func(context.Context) error {
		return errors.New(strings.Join(remedes, " ; "))
	}
	return s
}

func (p *Provider) vhostStep(proj *model.Project, bp *VHostPlan) []*engine.Step {
	resource := "web/vhost/" + bp.Name
	summary := fmt.Sprintf("vhost %s %s (%s)", p.Engine.Name(), bp.Name, strings.Join(bp.Domains, ", "))
	var steps []*engine.Step

	// Les répertoires de surcharge d'abord : le vhost les référence dès son
	// écriture. Leur absence n'empêcherait rien — le masque est facultatif — mais
	// un exploitant qui lit le vhost doit trouver le répertoire annoncé.
	if s := p.includeStep(bp); s != nil {
		steps = append(steps, s)
	}

	state, forced := bp.State, false
	if (state == fs.StateHandEdited || state == fs.StateForeign) && p.Force {
		// --force : l'étape redevient une mise à jour ordinaire, annoncée comme
		// telle par « plan » avant qu'« apply » ne l'exécute. La sauvegarde, elle,
		// n'a lieu qu'à l'application : « plan » et « diff » n'écrivent rien.
		state, forced = fs.StateOutdated, true
		summary += " — écrasement forcé (sauvegarde dans " + p.Paths.BackupsDir() + ")"
	}

	switch state {
	case fs.StateCurrent:
		steps = append(steps, engine.NewStep(resource, engine.NoOp, summary))
	case fs.StateHandEdited, fs.StateForeign:
		steps = append(steps, refusStep(resource, summary, bp.Path, p.Paths.BackupsDir(), state))
	default:
		action := engine.Create
		if state == fs.StateOutdated {
			action = engine.Update
		}
		s := engine.NewStep(resource, action, summary)
		s.Diff = func() (string, error) { return diffText(bp.Path, bp.Content) }
		// Le corps précédent est relevé à l'application et non au plan : l'état du
		// disque a pu changer entre les deux.
		var previous []byte
		var hadPrevious bool
		s.Apply = func(ctx context.Context) error {
			if data, err := os.ReadFile(bp.Path); err == nil {
				previous, hadPrevious = data, true
			}
			if forced {
				// Écraser le travail d'un exploitant sans en garder trace rend
				// l'opération irréversible : la sauvegarde précède l'écriture.
				if _, err := fs.BackupTo(bp.Path, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			return p.writeVHost(ctx, bp)
		}
		// La compensation rend le vhost qui était servi. Le supprimer laisserait le
		// lien de sites-enabled pointer dans le vide : le vhost était déjà actif,
		// donc l'étape d'activation est un NoOp, donc rien ne la compense et rien ne
		// retire le lien. Nginx tient l'inclusion d'un lien mort pour fatale — le
		// moteur ne redémarre plus, tout le parc avec lui — et Apache l'ignore en
		// silence, perdant le site. Le dégât se révèle au rechargement suivant,
		// d'où qu'il vienne, alors qu'ogs vient d'annoncer un retour à l'état initial.
		s.Compensate = func(context.Context) error {
			if hadPrevious {
				return fs.WriteAtomic(bp.Path, previous, 0o644, fs.KeepOwner)
			}
			// Une compensation en échec arrête tout le déroulage : un fichier déjà
			// absent n'est pas une erreur.
			if err := os.Remove(bp.Path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		s.Reload = []string{"web:" + p.Engine.Name()}
		steps = append(steps, s)
	}

	if !bp.Enabled {
		s := engine.NewStep(resource+"/enabled", engine.Create,
			fmt.Sprintf("activation du vhost %s", bp.Name))
		s.Apply = func(ctx context.Context) error { return p.enable(ctx, bp) }
		s.Compensate = func(context.Context) error { return os.Remove(bp.EnabledPath) }
		s.Reload = []string{"web:" + p.Engine.Name()}
		steps = append(steps, s)
	} else {
		steps = append(steps, engine.NewStep(resource+"/enabled", engine.NoOp,
			fmt.Sprintf("vhost %s actif", bp.Name)))
	}
	return steps
}

// includeStep crée les répertoires de surcharge d'un vhost (D63), ou constate
// qu'ils sont là. Nil si le vhost n'en référence aucun — un vhost de
// redirection n'a rien à personnaliser, et son unique server porte le challenge
// ACME.
//
// Les répertoires sont en 0750 root:root, plus fermés que le répertoire du
// moteur, qu'« ogs harden » laisse en root:www-data : leur contenu est lu par le
// processus maître, **en root**. Un fichier que le compte d'un projet pourrait
// écrire lui donnerait un « root / » ou un « alias » sur ce qu'il veut, donc la
// lecture de tout le système par HTTP. Corollaire : un fichier que les workers
// ouvrent à la requête, sous www-data — auth_basic_user_file, AuthUserFile —,
// n'a pas sa place ici, où ils ne le liraient pas.
func (p *Provider) includeStep(bp *VHostPlan) *engine.Step {
	if len(bp.IncludeDirs) == 0 {
		return nil
	}
	resource := "web/vhost/" + bp.Name + "/includes"
	missing := make([]string, 0, len(bp.IncludeDirs))
	for _, dir := range bp.IncludeDirs {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			missing = append(missing, dir)
		}
	}
	base := filepath.Join(p.Engine.IncludeRoot(), bp.Name)
	if len(missing) == 0 {
		return engine.NewStep(resource, engine.NoOp, "surcharges du vhost "+bp.Name+" ("+base+")")
	}
	s := engine.NewStep(resource, engine.Create,
		fmt.Sprintf("répertoires de surcharge du vhost %s (%s)", bp.Name, base))
	// Non critique : un vhost parfaitement servi ne doit pas être défait parce
	// qu'un répertoire d'agrément n'a pas pu être créé.
	s.Critical = false
	s.Apply = func(context.Context) error {
		for _, dir := range missing {
			if err := fs.EnsureDir(p.Engine.IncludeRoot(), 0o750, fs.KeepOwner); err != nil {
				return err
			}
			if err := fs.EnsureDir(filepath.Dir(dir), 0o750, fs.KeepOwner); err != nil {
				return err
			}
			if err := fs.EnsureDir(dir, 0o750, fs.KeepOwner); err != nil {
				return err
			}
		}
		return nil
	}
	s.Compensate = func(context.Context) error {
		// Seuls les répertoires vides sont retirés : un fichier déposé entre-temps
		// porte le travail de quelqu'un. os.Remove refuse un répertoire non vide,
		// ce qui suffit à s'en assurer.
		for _, dir := range missing {
			_ = os.Remove(dir)
		}
		_ = os.Remove(base)
		return nil
	}
	return s
}

// PlanTLS produit la bascule des vhosts dont le rendu dépend d'une émission à
// venir : l'activation du TLS d'un vhost que le certificat présent ne couvre
// pas, ou le retour au nom canonique déclaré d'un vhost servi sur le certificat
// en place. Ces étapes sont appliquées après l'émission : le rendu est donc
// recalculé au moment de l'exécution, pas au plan.
//
// Un vhost dont le rendu ne change pas — un alias ajouté, le reste couvert —
// n'a pas d'étape : son bloc 443 désigne déjà le certificat par son chemin, et
// c'est le rechargement porté par l'étape d'émission qui lui fait présenter le
// nouveau.
func (p *Provider) PlanTLS(_ context.Context, proj *model.Project) []*engine.Step {
	var steps []*engine.Step
	for _, vh := range p.vhosts(proj) {
		cov := p.coverage(proj, vh)
		// Un certificat désigné en attente (D102) : le vhost sert le certificat
		// présent jusqu'à son émission, et bascule sur lui dans cette étape.
		pending := cov.Pending != ""
		if cov.Ref == "" || (cov.Complete() && !pending) || len(vh.Domains) == 0 {
			continue
		}
		want := wantsTLS(proj, vh)
		avant, après := vueTLSDe(proj, vh, want, cov.Serves), vueTLSDe(proj, vh, want, cov.WillServe)
		if avant.equal(après) && !pending {
			continue
		}
		vhCopy := vh
		name := p.vhostName(proj, vh)
		summary := fmt.Sprintf("activation TLS du vhost %s", name)
		switch {
		case pending:
			summary = fmt.Sprintf("bascule du vhost %s sur le certificat %s", name, cov.Pending)
		case avant.on:
			summary = fmt.Sprintf("bascule du vhost %s sur le certificat réémis", name)
		case !après.on:
			// Seule la cible change de schéma : une redirection que le certificat
			// demandé ne couvre pas, vers un principal qu'il couvrira.
			summary = fmt.Sprintf("redirection du vhost %s vers https", name)
		}
		s := engine.NewStep("web/vhost/"+name+"/tls", engine.Update, summary)
		// Non critique : si l'émission a échoué, le site reste servi tel quel.
		s.Critical = false
		// Le fichier remplacé, relevé à l'application, pour la compensation.
		var (
			path        string
			previous    []byte
			hadPrevious bool
		)
		s.Apply = func(ctx context.Context) error {
			// Le vhost est rendu d'après le matériel présent, et comparé au fichier
			// en place, que l'émission ait abouti ou non : un repli a pu couvrir une
			// partie de ce qu'elle visait — la cible d'une redirection écrite en http
			// quand rien n'était couvert, que le visiteur rejoindrait sinon par un
			// saut de plus jusqu'au prochain apply.
			bp, err := p.Build(proj, vhCopy)
			if err != nil {
				return err
			}
			écrit := false
			path, previous, hadPrevious = "", nil, false
			switch bp.State {
			case fs.StateCurrent:
			case fs.StateHandEdited, fs.StateForeign:
				// Le plan a refusé ou sauvegardé tout fichier retouché : celui-ci l'a
				// été pendant l'exécution, et c'est le travail de quelqu'un, qu'une
				// bascule n'écrase pas.
				return fmt.Errorf("%s modifié hors ogs pendant l'exécution : bascule abandonnée", bp.Path)
			default:
				if data, err := os.ReadFile(bp.Path); err == nil {
					previous, hadPrevious = data, true
				}
				if err := p.writeVHost(ctx, bp); err != nil {
					return err
				}
				écrit, path = true, bp.Path
			}
			cov := p.coverage(proj, vhCopy)
			if cov.Pending != "" {
				// Le certificat désigné ne couvre toujours pas le vhost : il reste
				// servi avec le certificat en place.
				manque := fmt.Errorf("certificat %s indisponible : le vhost reste servi avec %s "+
					"(reprendre avec « ogs cert issue » puis « ogs apply %s »)", cov.Pending, cov.Ref, proj.ID())
				if écrit {
					return engine.Partial(manque)
				}
				return manque
			}
			vue := vueTLSDe(proj, vhCopy, want, cov.Serves)
			if vue.equal(vueTLSDe(proj, vhCopy, want, cov.WillServe)) {
				return nil
			}
			// L'émission a échoué, ou ne couvre pas tout : le rendu écrit ne
			// présente que ce que couvre le matériel sur le disque.
			reprise := fmt.Sprintf("reprendre avec « ogs cert issue %s » puis « ogs apply %s »",
				proj.ID(), proj.ID())
			var manquants []string
			for _, d := range append(append([]string{}, vhCopy.Domains...),
				optionString(vhCopy.Options, "target", "")) {
				if d != "" && cov.WillServe(d) && !cov.Serves(d) {
					manquants = append(manquants, d)
				}
			}
			var manque error
			switch {
			case !vue.on && len(cov.Present) == 0:
				manque = fmt.Errorf("certificat %s indisponible : le vhost reste en HTTP (%s)",
					cov.Ref, reprise)
			case !vue.on:
				// Le certificat existe, et sert le site, mais ne couvre pas ce vhost :
				// une redirection que la demande complète n'a pas obtenue. Le dire
				// « indisponible » ferait croire le site en HTTP.
				manque = fmt.Errorf("certificat %s sans %s : le vhost reste en HTTP (%s)",
					cov.Ref, strings.Join(manquants, ", "), reprise)
			default:
				// Émission échouée ou repliée sans le nouveau nom : le certificat en
				// place, réémis ou non, ne sert que l'ancien.
				manque = fmt.Errorf("certificat %s sans %s : le vhost reste servi en HTTPS "+
					"sous %s avec le certificat en place (%s)", cov.Ref, strings.Join(manquants, ", "),
					vue.serverName, reprise)
			}
			if écrit {
				// Un vhost réécrit doit être rechargé, bascule incomplète ou non.
				return engine.Partial(manque)
			}
			return manque
		}
		// La compensation rend le vhost d'avant la bascule (D110). Sans elle, une
		// étape critique en échec plus loin — ou une interruption — déroulait
		// l'émission, qui retire le certificat neuf, et laissait le bloc 443 qui
		// le désigne : le moteur refusait la configuration au rechargement
		// suivant, et le parc tombait avec ce site. Une bascule aboutie en partie
		// n'est pas compensée (engine.Partial) : c'est alors la compensation de
		// l'émission qui garde le certificat qu'un vhost écrit désigne.
		s.Compensate = func(context.Context) error {
			switch {
			case path == "":
				return nil
			case hadPrevious:
				return fs.WriteAtomic(path, previous, 0o644, fs.KeepOwner)
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		s.Reload = []string{"web:" + p.Engine.Name()}
		steps = append(steps, s)
	}
	return steps
}

// CertificateReferences énumère les vhosts écrits — sites-available et
// sites-enabled du moteur — qui désignent le répertoire d'un certificat. C'est
// ce que le serveur charge qui décide si un certificat peut quitter le disque
// (D110), non ce que déclarent les projets : un vhost resté après une
// déclaration retirée, celui d'un site hors d'ogs, une bascule qu'un déroulage
// n'a pas défaite le désignent encore, et le moteur refuserait sa
// configuration au rechargement suivant. Un fichier illisible ou démesuré est
// tenu pour désignant : dans le doute, le certificat reste.
func (p *Provider) CertificateReferences(dir string) []string {
	needle := []byte(strings.TrimSuffix(dir, "/") + "/")
	seen := map[string]bool{}
	var out []string
	for _, d := range []string{p.Engine.AvailableDir(), p.Engine.EnabledDir()} {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			path := filepath.Join(d, e.Name())
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if real, err := filepath.EvalSymlinks(path); err == nil {
				if seen[real] {
					continue
				}
				seen[real] = true
			}
			if info.Size() > 4<<20 {
				out = append(out, path)
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil || bytes.Contains(data, needle) {
				out = append(out, path)
			}
		}
	}
	sort.Strings(out)
	return out
}

// writeVHost écrit puis valide. La validation intervient avant l'activation :
// une configuration refusée par le moteur ne doit jamais devenir active.
func (p *Provider) writeVHost(ctx context.Context, bp *VHostPlan) error {
	// Refus avant écriture : un vhost dont le journal est hors d'atteinte du moteur
	// ne casse pas ce seul site, il arrête le serveur au rechargement suivant.
	for _, logPath := range logPaths(string(bp.Content)) {
		if err := p.reachable(ctx, logPath); err != nil {
			return fmt.Errorf("journal %s : %w", logPath, err)
		}
	}
	previous, hadPrevious := os.ReadFile(bp.Path)
	if err := fs.WriteAtomic(bp.Path, bp.Content, 0o644, fs.KeepOwner); err != nil {
		return err
	}
	if err := p.validateConfig(ctx); err != nil {
		if hadPrevious == nil {
			_ = fs.WriteAtomic(bp.Path, previous, 0o644, fs.KeepOwner)
		} else {
			_ = os.Remove(bp.Path)
		}
		return err
	}
	return nil
}

// enable crée le lien symbolique d'activation, comme le font a2ensite et le
// répertoire sites-enabled de Nginx, puis revalide.
func (p *Provider) enable(ctx context.Context, bp *VHostPlan) error {
	if err := os.MkdirAll(p.Engine.EnabledDir(), 0o755); err != nil {
		return err
	}
	rel, err := filepath.Rel(p.Engine.EnabledDir(), bp.Path)
	if err != nil {
		rel = bp.Path
	}
	if err := os.Symlink(rel, bp.EnabledPath); err != nil && !os.IsExist(err) {
		return err
	}
	if err := p.validateConfig(ctx); err != nil {
		_ = os.Remove(bp.EnabledPath)
		return err
	}
	return nil
}

// DropInPath est le chemin du complément d'unité que gère ogs.
func (p *Provider) DropInPath() string {
	return filepath.Join(p.Paths.SystemdDir(), p.Engine.Unit()+".service.d", "ogs.conf")
}

// PlanDropIn produit l'étape du complément d'unité du moteur web : il ouvre en
// écriture les répertoires de journaux des projets.
//
// Les unités des serveurs web sont durcies par leur distribution — Ubuntu livre
// « apache2.service » avec ProtectHome=read-only — et un service ainsi protégé voit
// /home en lecture seule **dans son seul espace de noms** : « mount » affiche rw,
// root écrit sans peine, et le maître meurt pourtant sur l'ouverture du journal d'un
// vhost, emportant tous les sites (« AH00019: Unable to open logs, exiting »). Sans
// ce complément, ogs est inutilisable sur une telle machine.
//
// ReadWritePaths remonte les chemins en écriture après application de ProtectHome
// et de ProtectSystem, ce qui laisse le reste du durcissement intact. Chaque
// répertoire est énuméré plutôt que la racine des homes : rien d'autre que les
// journaux n'a besoin de s'ouvrir. Le préfixe « - » est indispensable — un
// ReadWritePaths pointant sur un chemin absent fait échouer le service en
// 226/NAMESPACE avant toute exécution, et un projet supprimé laisse son répertoire
// dans la liste jusqu'au prochain apply.
func (p *Provider) PlanDropIn(ctx context.Context, projects []*model.Project) ([]*engine.Step, error) {
	var body []byte
	body = append(body, "[Service]\n"...)
	// Une seule entrée, la racine des journaux (D71) : ils ont quitté /home, que
	// ProtectHome montrait en lecture seule au seul service, et tout répertoire de
	// journaux vit désormais sous cette racine — la validation le refuse ailleurs.
	// L'entrée reste par prudence envers un ProtectSystem=strict, qui monterait
	// /var en lecture seule. Son contenu ne dépend plus des projets : en créer un
	// ne redémarre plus le serveur web.
	if p.LogsRoot != "" {
		body = append(body, "ReadWritePaths=-"+p.LogsRoot+"\n"...)
	} else {
		seen := map[string]bool{}
		dirs := make([]string, 0, len(projects))
		for _, proj := range projects {
			dir := proj.Spec.Layout.LogDir
			if dir == "" || seen[dir] {
				continue
			}
			seen[dir] = true
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		for _, dir := range dirs {
			body = append(body, "ReadWritePaths=-"+dir+"\n"...)
		}
	}
	content := fs.Render(fs.Header{
		Project: "parc", Resource: "web/dropin", Generated: p.now(),
	}, body, "#")

	path := p.DropInPath()
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	summary := "journaux ouverts à " + p.Engine.Unit() + " (" + filepath.Base(path) + ")"
	if state == fs.StateCurrent {
		return []*engine.Step{engine.NewStep("web/dropin", engine.NoOp, summary)}, nil
	}
	action := engine.Create
	if state != fs.StateAbsent {
		action = engine.Update
	}
	s := engine.NewStep("web/dropin", action, summary)
	// Le corps précédent est relevé à l'application et non au plan : l'état du
	// disque a pu changer entre les deux.
	var previous []byte
	var hadPrevious bool
	var restore func() error
	s.Apply = func(ctx context.Context) error {
		if data, err := os.ReadFile(path); err == nil {
			previous, hadPrevious = data, true
		}
		if err := fs.EnsureDir(filepath.Dir(path), 0o755, fs.KeepOwner); err != nil {
			return err
		}
		if err := fs.WriteAtomic(path, content, 0o644, fs.KeepOwner); err != nil {
			return err
		}
		if p.Exec.Simulated() {
			return nil
		}
		_, err := p.Exec.Run(ctx, exec.Command{
			Name: "systemctl", Args: []string{"daemon-reload"}})
		if err != nil {
			// Le moteur ne compense jamais l'étape qui échoue : le complément
			// laissé en place passait pour conforme au plan suivant, qui ne
			// rejouait ni le rechargement ni le redémarrage (Q-06). Rendu tel
			// qu'il était, il redit ce que systemd a chargé.
			if rerr := restore(); rerr != nil {
				return fmt.Errorf("%w ; le complément n'a pas pu être rendu (%v) : le plan suivant le dira "+
					"conforme sans que systemd l'ait relu", err, rerr)
			}
		}
		return err
	}
	// La compensation rend le complément qui était en place. Le supprimer
	// rendrait au moteur web l'unité durcie par sa distribution — ProtectHome
	// remonte /home en lecture seule dans le seul espace de noms du service —
	// donc un maître qui s'arrête en 226/NAMESPACE ou sur AH00019 au prochain
	// démarrage, tout le parc avec lui, alors qu'ogs vient d'annoncer un retour à
	// l'état initial. La suppression n'est juste que si le fichier était absent.
	restore = func() error {
		if hadPrevious {
			return fs.WriteAtomic(path, previous, 0o644, fs.KeepOwner)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			// Une compensation en échec arrête tout le déroulage : un fichier déjà
			// absent n'est pas une erreur.
			return err
		}
		return nil
	}
	s.Compensate = func(ctx context.Context) error {
		if err := restore(); err != nil {
			return err
		}
		if p.Exec.Simulated() {
			return nil
		}
		_, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"daemon-reload"}})
		return err
	}
	steps := []*engine.Step{s}

	// Le redémarrage est une étape distincte, et non critique : l'espace de noms
	// d'un service est établi à son démarrage — un « reload » ne le refait pas — mais
	// un moteur qui refuse de repartir pour une raison étrangère au projet (un port
	// déjà pris, une configuration tierce) ne doit pas faire échouer l'apply et
	// défaire par rollback un site par ailleurs correct.
	//
	// systemd distingue deux arrêts, et ogs doit les distinguer aussi : un moteur
	// « inactive » a été arrêté proprement — l'exploitant l'a peut-être voulu, et le
	// complément prendra effet à son prochain démarrage — tandis qu'un moteur
	// « failed » est mort sur une erreur dont la cause vient précisément d'être
	// levée. Le laisser mort alors qu'on peut le relever serait absurde.
	verb := ""
	switch p.unitState(ctx) {
	case "active", "reloading":
		verb = "restart"
	case "failed":
		verb = "start"
	}
	if verb != "" {
		summary := "redémarrage de " + p.Engine.Unit() + " (espace de noms des journaux)"
		if verb == "start" {
			summary = "relance de " + p.Engine.Unit() + ", arrêté sur erreur"
		}
		r := engine.NewStep("web/dropin/restart", engine.Update, summary)
		r.Critical = false
		r.Apply = func(ctx context.Context) error {
			if p.Exec.Simulated() {
				return nil
			}
			// Un redémarrage relit toute la configuration et rouvre tous les
			// journaux : il porte les contrôles du rechargement, et davantage, car
			// un « restart » refusé ne laisse pas tourner l'ancien maître (R1-36,
			// D118). Une paire dépareillée qu'un vhost désigne (D110), une
			// configuration devenue invalide depuis son écriture, un journal que le
			// maître ne pourra pas ouvrir (AH00019) : chacun arrêterait tous les
			// sites. Le refus rend le complément tel qu'il était — le moteur ne
			// compense jamais l'étape qui échoue, et un complément laissé en place
			// passerait pour conforme au plan suivant, qui ne rejouerait jamais le
			// redémarrage.
			for _, check := range []func(context.Context) error{
				func(ctx context.Context) error { return p.CheckCertificates(ctx, true) },
				p.validateConfig,
				p.CheckLogs,
			} {
				if err := check(ctx); err != nil {
					return p.refusRedemarrage(ctx, verb, restore, err)
				}
			}
			raison := ""
			if err := p.restartUnit(ctx, verb); err != nil {
				raison = err.Error()
			} else {
				// Comme après un rechargement : « restart » rend la main avant que
				// le maître n'ait ouvert ses journaux et ses clés, et sa mort suit.
				// L'observation part de l'état d'après le redémarrage, dont le
				// processus principal est neuf.
				raison = p.diedAfterReload(ctx, p.snapshot(ctx))
			}
			if raison == "" {
				return nil
			}
			return p.relever(ctx, "le redémarrage", raison)
		}
		steps = append(steps, r)
	}
	return steps, nil
}

// PlanSnippet produit l'étape du fragment de portée serveur. Elle est globale :
// le fichier ne dépend d'aucun projet, mais les vhosts de proxy en dépendent
// ($connection_upgrade), et ceux des sites PHP aussi ($ogs_fichier).
func (p *Provider) PlanSnippet() (*engine.Step, error) {
	path := p.Engine.SnippetPath()
	if path == "" {
		return nil, nil
	}
	body, err := p.Templates.Render(p.Engine.TemplateName("http-snippet"), map[string]any{})
	if err != nil {
		return nil, err
	}
	content := fs.Render(fs.Header{
		Project: "parc", Resource: "web/snippet", Generated: p.now(),
		Template: p.Templates.OverridePath(p.Engine.TemplateName("http-snippet")),
	}, body, p.Engine.CommentPrefix())

	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	if state == fs.StateCurrent {
		return engine.NewStep("web/snippet", engine.NoOp, "réglages "+p.Engine.Name()), nil
	}
	summary := "réglages " + p.Engine.Name() + " (" + filepath.Base(path) + ")"
	// Le fragment suit le régime des cinq états comme les vhosts (§6.4). C'est un
	// fichier de portée serveur, et l'exploitant peut légitimement y avoir ajouté
	// un réglage : l'écraser en silence lui retirerait sans un mot une
	// configuration qui vaut pour tout le parc.
	forced := false
	if (state == fs.StateHandEdited || state == fs.StateForeign) && p.Force {
		state, forced = fs.StateOutdated, true
		summary += " — écrasement forcé (sauvegarde dans " + p.Paths.BackupsDir() + ")"
	}
	if state == fs.StateHandEdited || state == fs.StateForeign {
		return refusStep("web/snippet", summary, path, p.Paths.BackupsDir(), state), nil
	}
	action := engine.Create
	if state != fs.StateAbsent {
		action = engine.Update
	}
	s := engine.NewStep("web/snippet", action, summary)
	// Le corps précédent est relevé à l'application et non au plan : l'état du
	// disque a pu changer entre les deux.
	var previous []byte
	var hadPrevious bool
	// restaurer rend le corps antérieur, ou retire le fichier s'il n'y en avait
	// pas. Un fichier déjà absent n'est pas une erreur : une compensation en échec
	// arrête tout le déroulage.
	restaurer := func() error {
		if hadPrevious {
			return fs.WriteAtomic(path, previous, 0o644, fs.KeepOwner)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	s.Apply = func(ctx context.Context) error {
		if data, err := os.ReadFile(path); err == nil {
			previous, hadPrevious = data, true
		}
		if forced {
			// La sauvegarde n'a lieu qu'ici : « plan » et « diff » n'écrivent rien.
			if _, err := fs.BackupTo(path, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := fs.EnsureDir(filepath.Dir(path), 0o755, fs.KeepOwner); err != nil {
			return err
		}
		if err := fs.WriteAtomic(path, content, 0o644, fs.KeepOwner); err != nil {
			return err
		}
		// La configuration est revalidée : un fragment fautif casserait tous les
		// sites, pas seulement celui qu'on installe. Et la revalidation porte sur
		// la configuration entière : elle peut échouer pour une cause étrangère au
		// fragment, d'où le retour au corps antérieur plutôt que la suppression.
		if err := p.validateConfig(ctx); err != nil {
			_ = restaurer()
			return err
		}
		return nil
	}
	// La compensation rend le fragment qui était en place. Le supprimer retirerait
	// la « map $http_upgrade $connection_upgrade » dont dépend tout vhost de proxy
	// du parc : nginx refuserait alors de démarrer, tout le parc avec lui, alors
	// qu'ogs vient d'annoncer un retour à l'état initial. La suppression n'est
	// juste que si le fichier était absent — l'étape figure désormais aussi dans le
	// plan de chaque projet, donc dans le déroulage de sa saga.
	s.Compensate = func(context.Context) error { return restaurer() }
	s.Reload = []string{"web:" + p.Engine.Name()}
	return s, nil
}

// Reload recharge le serveur web. Un rechargement suffit à prendre en compte une
// nouvelle appartenance de groupe : les workers rappellent initgroups() au fork.
//
// La configuration est revalidée juste avant, et non seulement à l'écriture :
// « apachectl graceful » ne teste rien de lui-même, et un maître Apache qui relit
// une configuration devenue invalide **meurt** — le serveur entier, pas le seul
// vhost fautif. Or l'écriture et le rechargement sont séparés dans le temps : les
// intentions sont groupées en fin d'application, et une suppression intervenue
// entre-temps peut avoir emporté un fichier qu'un vhost référence. Refuser le
// rechargement laisse le serveur tourner sur la configuration qu'il a déjà en
// mémoire, ce qui est toujours préférable à un serveur mort.
func (p *Provider) Reload(ctx context.Context) error {
	if p.Exec.Simulated() {
		return nil
	}
	// Une paire clé/certificat dépareillée qu'un vhost activé désigne : nginx
	// refuse la configuration, le maître Apache meurt en la chargeant alors
	// qu'« apachectl -t » ne voit rien (D110). Une écriture interrompue s'achève ;
	// sinon le rechargement est refusé, en le nommant.
	if err := p.CheckCertificates(ctx, true); err != nil {
		return fmt.Errorf("rechargement refusé : %s continue de servir sa configuration précédente — "+
			"les vhosts écrits par cette exécution ne sont pas encore servis ; %w", p.Engine.Unit(), err)
	}
	if err := p.validateConfig(ctx); err != nil {
		return fmt.Errorf("%w\n  rechargement refusé : %s continue de servir sa configuration "+
			"précédente", err, p.Engine.Unit())
	}
	if err := p.CheckLogs(ctx); err != nil {
		return fmt.Errorf("%w\n  rechargement refusé : %s continue de servir sa configuration "+
			"précédente", err, p.Engine.Unit())
	}
	before := p.snapshot(ctx)
	if _, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl",
		Args: []string{"reload", p.Engine.Unit()}}); err != nil {
		return err
	}
	// Un rechargement qui rend 0 ne dit pas que le serveur vit encore : systemd
	// journalise « Reloaded » puis, une ligne plus bas, la mort du maître. Aucune
	// vérification en amont ne couvre tous les cas — un service dont l'unité porte
	// ProtectHome voit /home en lecture seule dans son seul espace de noms, où ogs
	// ne peut pas se placer. Reste à constater le résultat, et à relever le serveur
	// plutôt qu'à laisser un parc entier hors ligne jusqu'à ce que quelqu'un s'en
	// aperçoive. Le fait est signalé dans tous les cas : un serveur qui meurt à
	// chaque rechargement est un incident, même relevé aussitôt.
	//
	// La mort peut suivre le retour de la commande : « apachectl graceful » rend
	// la main avant que le maître n'ait relu sa configuration. Le moteur est donc
	// observé quelques instants (diedAfterReload), non relu une fois.
	raison := p.diedAfterReload(ctx, before)
	if raison == "" {
		return nil
	}
	return p.relever(ctx, "le rechargement", raison)
}

// relever relance le moteur qu'une opération a arrêté, compteur d'échecs
// acquitté, et rend toujours une erreur : un serveur qui meurt à chaque
// rechargement ou redémarrage est un incident, même relevé aussitôt.
func (p *Provider) relever(ctx context.Context, operation, raison string) error {
	if !p.unitActive(ctx) {
		if err := p.restartUnit(ctx, "start"); err != nil || !p.unitActive(ctx) {
			if err == nil {
				err = errors.New("unité toujours inactive")
			}
			return fmt.Errorf("%s a arrêté %s (%s), qui refuse de redémarrer : %w\n"+
				"  les sites ne sont plus servis — voir « journalctl -u %s »",
				operation, p.Engine.Unit(), raison, err, p.Engine.Unit())
		}
	}
	return fmt.Errorf("%s a arrêté %s (%s) ; le service a été relevé, "+
		"mais la cause reste à traiter (voir « journalctl -u %s »)",
		operation, p.Engine.Unit(), raison, p.Engine.Unit())
}

// refusRedemarrage rend le complément d'unité tel qu'il était quand le
// redémarrage qui devait le prendre en compte est refusé, et le relit par
// systemd : le plan suivant le réécrira et rejouera le redémarrage, une fois la
// cause levée. Le moteur, lui, n'est pas touché.
func (p *Provider) refusRedemarrage(ctx context.Context, verb string, restore func() error, cause error) error {
	etat := p.Engine.Unit() + " continue de servir sa configuration précédente"
	quoi := "redémarrage refusé"
	if verb == "start" {
		etat, quoi = p.Engine.Unit()+" reste arrêté", "relance refusée"
	}
	rendu := "le complément d'unité est rendu, et le prochain apply rejouera son écriture et le redémarrage"
	if err := restore(); err != nil {
		rendu = fmt.Sprintf("le complément d'unité n'a pas pu être rendu (%v) : le plan suivant le dira "+
			"conforme sans que le service l'ait chargé", err)
	} else if _, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"daemon-reload"}}); err != nil {
		rendu = fmt.Sprintf("le complément d'unité est rendu, mais systemd ne l'a pas relu (%v)", err)
	}
	return fmt.Errorf("%s : %s ; %s — %w", quoi, etat, rendu, cause)
}

// restartUnit redémarre le moteur. Le compteur d'échecs est acquitté au préalable :
// systemd refuse de relancer un service qui a échoué plus de StartLimitBurst fois
// dans l'intervalle (cinq fois en dix secondes par défaut) et rend « start request
// repeated too quickly » — un serveur mort resterait mort, alors que la cause vient
// d'être levée. « reset-failed » est sans effet sur un service sain.
func (p *Provider) restartUnit(ctx context.Context, verb string) error {
	_, _ = p.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{"reset-failed", p.Engine.Unit()}})
	_, err := p.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{verb, p.Engine.Unit()}})
	return err
}

// unitActive dit si l'unité du moteur est active.
func (p *Provider) unitActive(ctx context.Context) bool {
	_, err := p.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{"is-active", "--quiet", p.Engine.Unit()},
	})
	return err == nil
}

// unitState renvoie l'état systemd de l'unité du moteur : « active », « inactive »,
// « failed »… En mode simulé, l'unité est réputée active — c'est ce qui permet aux
// tests d'observer les commandes qui auraient été passées.
func (p *Provider) unitState(ctx context.Context) string {
	if p.Exec.Simulated() {
		return "active"
	}
	res, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{
		"show", p.Engine.Unit(), "-p", "ActiveState", "--value"}})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// CheckLogs vérifie que les journaux des vhosts gérés sont ouvrables en écriture.
//
// C'est le trou que la validation syntaxique laisse : « apachectl -t » répond
// « Syntax OK » sans jamais ouvrir un fichier de log, alors que le maître, lui,
// les ouvre tous au (re)démarrage et **s'arrête** si l'un d'eux résiste —
// « AH00019: Unable to open logs, exiting ». Un seul projet dont le journal est
// inaccessible emporte donc tous les sites du serveur. Le cas s'est produit sur un
// /home remonté en lecture seule après une erreur disque : la configuration était
// valide, l'écriture impossible.
//
// Tous les vhosts activés sont examinés, pas seulement ceux du projet en cours :
// un rechargement est global, et c'est un tiers qui en fait les frais.
func (p *Provider) CheckLogs(ctx context.Context) error {
	entries, err := os.ReadDir(p.Engine.EnabledDir())
	if err != nil {
		// Répertoire absent : il n'y a aucun vhost à servir, donc rien à vérifier.
		return nil
	}
	seen := map[string]bool{}
	for _, e := range entries {
		path := filepath.Join(p.Engine.EnabledDir(), e.Name())
		content, err := os.ReadFile(path)
		if err != nil || !strings.Contains(firstLines(content), fs.Marker) {
			// Un vhost qu'ogs n'a pas écrit ne relève pas de lui.
			continue
		}
		for _, logPath := range logPaths(string(content)) {
			if seen[logPath] {
				continue
			}
			seen[logPath] = true
			if err := writable(logPath); err != nil {
				return fmt.Errorf("journal %s inaccessible en écriture (%s) : %w",
					logPath, e.Name(), err)
			}
			if err := p.reachable(ctx, logPath); err != nil {
				return fmt.Errorf("journal %s (%s) : %w", logPath, e.Name(), err)
			}
		}
	}
	return nil
}

// AuditIncludes contrôle les répertoires de surcharge des vhosts (D63) et rend
// la liste des anomalies, vide si tout est conforme.
//
// Trois choses qu'aucune vérification de configuration ne voit :
//
//   - un fichier de surcharge est lu par le processus maître, **en root**. S'il
//     est inscriptible au-delà de son propriétaire, ce compte obtient un
//     « root / » ou un « alias » vers ce qu'il veut : la lecture de tout le
//     système par HTTP. Même régime que les hooks, donc, et pour la même raison.
//   - un fichier .conf déposé à la racine du répertoire du vhost, hors des
//     sous-répertoires de contexte, n'est chargé par personne. Rien ne le dit :
//     le site fonctionne, la surcharge est simplement sans effet.
//   - un répertoire qu'aucun vhost ne référence plus — vhost renommé, projet
//     supprimé — garde des fichiers que leur auteur croit actifs.
func (p *Provider) AuditIncludes() []IncludeProblem {
	root := p.Engine.IncludeRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		// Aucun répertoire de surcharge : rien à contrôler.
		return nil
	}
	referenced := p.referencedIncludeDirs()
	var problems []IncludeProblem
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if !e.IsDir() {
			problems = append(problems, ignored("%s n'est pas un répertoire de vhost", dir))
			continue
		}
		var used bool
		for ref := range referenced {
			if ref == dir || strings.HasPrefix(ref, dir+string(filepath.Separator)) {
				used = true
				break
			}
		}
		if !used {
			// Le répertoire entier est orphelin : détailler chacun de ses contextes
			// répéterait la même chose autant de fois qu'il en contient.
			problems = append(problems, ignored(
				"%s ne correspond à aucun vhost géré : ses surcharges ne sont pas chargées", dir))
		}
		problems = append(problems, p.auditIncludeDir(dir, referenced, used)...)
	}
	sort.Slice(problems, func(i, j int) bool { return problems[i].Detail < problems[j].Detail })
	return problems
}

// IncludeProblem est une anomalie relevée dans les répertoires de surcharge.
type IncludeProblem struct {
	Detail string
	// Severe distingue ce qui ouvre une élévation de privilège — un fichier lu en
	// root qu'un tiers peut écrire — de ce qui n'est qu'inopérant : un fichier
	// jamais chargé ne casse rien, il ne fait rien.
	Severe bool
	// Fix est la commande qui rétablit, vide s'il n'y en a pas d'évidente.
	Fix string
}

func ignored(format string, args ...any) IncludeProblem {
	return IncludeProblem{Detail: fmt.Sprintf(format, args...)}
}

// auditIncludeDir contrôle un répertoire de vhost et ses contextes.
func (p *Provider) auditIncludeDir(dir string, referenced map[string]bool, used bool) []IncludeProblem {
	problems := p.auditIncludePerm(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return append(problems, ignored("%s illisible : %v", dir, err))
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if !e.IsDir() {
			if strings.HasSuffix(e.Name(), ".conf") {
				problems = append(problems, ignored(
					"%s est hors de tout contexte : le déplacer dans %s/ ou %s/, sinon il reste ignoré",
					path, IncludeVHost, IncludeDocroot))
			}
			continue
		}
		problems = append(problems, p.auditIncludePerm(path)...)
		if !referenced[path] {
			if used {
				problems = append(problems, ignored(
					"%s n'est inclus par aucun vhost : contexte inconnu du moteur %s",
					path, p.Engine.Name()))
			}
			continue
		}
		files, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".conf") {
				continue
			}
			problems = append(problems, p.auditIncludePerm(filepath.Join(path, f.Name()))...)
		}
	}
	return problems
}

// auditIncludePerm contrôle propriétaire et mode d'un chemin de surcharge. Tout
// ce qui se juge par rapport à root ne vaut qu'en production : sous préfixe,
// l'arborescence appartient à qui fait tourner l'essai, et les modes y sont ceux
// de son umask.
func (p *Provider) auditIncludePerm(path string) []IncludeProblem {
	if p.Paths.Prefix() != "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	var problems []IncludeProblem
	if info.Mode().Perm()&0o022 != 0 {
		problems = append(problems, IncludeProblem{
			Detail: fmt.Sprintf("%s est inscriptible au-delà de son propriétaire (mode %04o) : "+
				"ce compte dicterait la configuration que %s lit en root",
				path, info.Mode().Perm(), p.Engine.Unit()),
			Severe: true,
			Fix:    "chmod go-w " + path,
		})
	}
	if uid, _, ok := system.OwnerOf(info); ok && uid != 0 {
		problems = append(problems, IncludeProblem{
			Detail: fmt.Sprintf("%s appartient à l'uid %d, attendu root", path, uid),
			Severe: true,
			Fix:    "chown root:root " + path,
		})
	}
	return problems
}

// referencedIncludeDirs énumère les répertoires de surcharge que les vhosts
// activés référencent réellement. C'est le moteur qui décide, non ogs : un vhost
// écrit depuis un template surchargé peut n'inclure aucun répertoire, ou en
// inclure d'autres.
func (p *Provider) referencedIncludeDirs() map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(p.Engine.EnabledDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		content, err := os.ReadFile(filepath.Join(p.Engine.EnabledDir(), e.Name()))
		if err != nil || !strings.Contains(firstLines(content), fs.Marker) {
			continue
		}
		for _, dir := range p.includeDirs(content) {
			out[dir] = true
		}
	}
	return out
}

// RemoveIncludes retire les répertoires de surcharge d'un vhost supprimé, et
// rend ceux qu'il a fallu laisser en place. Un répertoire vide ne vaut rien ;
// un répertoire garni porte le travail de quelqu'un et n'est jamais détruit —
// même règle que l'authorized_keys d'un projet.
func (p *Provider) RemoveIncludes(name string) []string {
	base := filepath.Join(p.Engine.IncludeRoot(), name)
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var kept []string
	for _, e := range entries {
		path := filepath.Join(base, e.Name())
		// Seuls les répertoires vides sont candidats : un fichier régulier a été
		// déposé à la main — même mal placé, il est le travail de quelqu'un, et
		// os.Remove l'emporterait sans broncher.
		if !e.IsDir() {
			kept = append(kept, path)
			continue
		}
		// os.Remove refuse un répertoire non vide : c'est le test lui-même.
		if err := os.Remove(path); err != nil {
			kept = append(kept, path)
		}
	}
	if len(kept) == 0 {
		if err := os.Remove(base); err != nil {
			kept = append(kept, base)
		}
	}
	sort.Strings(kept)
	return kept
}

// reachable dit si le moteur web peut atteindre un chemin en écriture depuis son
// propre espace de noms. Écrire depuis ogs ne prouve rien : root écrit sans peine là
// où ProtectHome=read-only interdit au service de le faire, et c'est le service qui
// ouvre les journaux.
func (p *Provider) reachable(ctx context.Context, path string) error {
	props, err := p.unitProtection(ctx)
	if err != nil || props == nil {
		// Sans réponse de systemd, rien ne peut être affirmé : mieux vaut ne rien
		// dire que refuser un vhost sur une supposition.
		return nil
	}
	if !props.protectsHome || !underHome(path) {
		return nil
	}
	for _, rw := range props.readWrite {
		if path == rw || strings.HasPrefix(path, strings.TrimSuffix(rw, "/")+"/") {
			return nil
		}
	}
	return fmt.Errorf("hors d'atteinte de %s : son unité porte ProtectHome=%s, "+
		"qui lui montre /home en lecture seule — le maître s'arrêterait en ouvrant "+
		"ce journal (« Unable to open logs, exiting »). Appliquer le projet écrit le "+
		"complément d'unité %s, qui ouvre ce répertoire",
		p.Engine.Unit(), props.protectHome, p.DropInPath())
}

// unitProtection porte ce que systemd applique à l'unité du moteur.
type unitProtection struct {
	protectHome  string
	protectsHome bool
	readWrite    []string
}

// unitProtection lit les propriétés effectives de l'unité — celles que systemd
// applique après l'unité et tous ses compléments, et non celles du seul fichier.
func (p *Provider) unitProtection(ctx context.Context) (*unitProtection, error) {
	if p.Exec.Simulated() {
		return nil, nil
	}
	res, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{
		"show", p.Engine.Unit(), "-p", "ProtectHome", "-p", "ReadWritePaths"}})
	if err != nil {
		return nil, err
	}
	out := &unitProtection{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ProtectHome":
			out.protectHome = value
			// « tmpfs » et « yes » masquent /home, « read-only » l'interdit en
			// écriture : les trois empêchent d'y écrire un journal.
			out.protectsHome = value == "yes" || value == "read-only" || value == "tmpfs"
		case "ReadWritePaths":
			// systemd rend la liste sur une ligne, séparée par des espaces, chaque
			// chemin pouvant porter le préfixe « - » qui le rend facultatif.
			for _, f := range strings.Fields(value) {
				out.readWrite = append(out.readWrite, strings.TrimPrefix(f, "-"))
			}
		}
	}
	if out.protectHome == "" {
		return nil, nil
	}
	return out, nil
}

// underHome dit si un chemin relève d'un répertoire personnel, seul territoire que
// ProtectHome couvre : /home, /root et /run/user.
func underHome(path string) bool {
	for _, root := range []string{"/home", "/root", "/run/user"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

// reLogDirective reconnaît les directives de journalisation des deux moteurs. Le
// chemin doit être absolu : les formats de log nommés d'Apache (« CustomLog …
// combined ») ne sont pas des chemins, et nginx accepte « access_log off ».
var reLogDirective = regexp.MustCompile(`(?m)^\s*(?:ErrorLog|CustomLog|error_log|access_log)\s+(/\S+)`)

// logPaths extrait les journaux référencés par un vhost.
func logPaths(content string) []string {
	var out []string
	for _, m := range reLogDirective.FindAllStringSubmatch(content, -1) {
		out = append(out, strings.TrimSuffix(m[1], ";"))
	}
	return out
}

// writable dit si un journal peut être ouvert en écriture, sans rien laisser
// derrière : un fichier existant est ouvert en ajout — le test le plus fidèle à ce
// que fera le serveur — et pour un journal encore absent c'est son répertoire qui
// est éprouvé, par un temporaire aussitôt retiré. Créer le journal serait plus
// simple mais le laisserait à root, alors que son propriétaire est le projet.
func writable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o640)
	if err == nil {
		return f.Close()
	}
	if !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(path)
	probe, err := os.CreateTemp(dir, ".ogs-log-probe-*")
	if err != nil {
		return err
	}
	_ = probe.Close()
	return os.Remove(probe.Name())
}

// firstLines borne la recherche de la marque à l'en-tête : un vhost entier peut
// être volumineux, et la marque est sur ses trois premières lignes.
func firstLines(data []byte) string {
	if len(data) > 512 {
		data = data[:512]
	}
	return string(data)
}

// HSTSMaxAge est la durée, en secondes, de la politique HSTS que posent les
// vhosts d'ogs : un an. C'est aussi la durée pendant laquelle un navigateur
// passé avant un retrait garde la politique — d'où celle de l'en-tête de retrait
// (D119).
const HSTSMaxAge = 31536000

// hstsActive dit si un vhost pose une politique HSTS : oui par défaut, non sous
// « hsts: false » (retrait) et « hsts: none » (aucun en-tête).
func hstsActive(options map[string]any) bool {
	v, ok := options["hsts"]
	if !ok {
		return true
	}
	switch v := v.(type) {
	case bool:
		return v
	case string:
		return !strings.EqualFold(v, "none")
	}
	return true
}

// hstsHeader rend la valeur de l'en-tête Strict-Transport-Security d'un vhost en
// TLS, vide s'il n'en porte aucun (D119) :
//
//   - par défaut, « max-age=31536000; includeSubDomains » ; « hstsSubdomains:
//     false » retire includeSubDomains — un sous-domaine servi en HTTP seul,
//     ici ou ailleurs, cesse d'être injoignable pour qui a visité le domaine ;
//   - « hsts: false » pose « max-age=0 » : un navigateur qui revient efface la
//     politique qu'il tenait, sous-domaines compris. Ne rien envoyer ne
//     l'effacerait pas, et le site resterait imposé en HTTPS jusqu'à un an
//     après la dernière visite ;
//   - « hsts: none » n'envoie aucun en-tête : à poser un an (HSTSMaxAge) au
//     moins après le dernier apply qui servait la politique, quand plus aucun
//     navigateur ne peut la tenir de ce site.
func hstsHeader(options map[string]any) string {
	if v, ok := options["hsts"]; ok {
		switch v := v.(type) {
		case bool:
			if !v {
				return "max-age=0"
			}
		case string:
			if strings.EqualFold(v, "none") {
				return ""
			}
		}
	}
	value := fmt.Sprintf("max-age=%d", HSTSMaxAge)
	if optionBool(options, "hstsSubdomains", true) {
		value += "; includeSubDomains"
	}
	return value
}

// HSTSOptionProblems nomme les valeurs de options.hsts et
// options.hstsSubdomains qu'ogs ne reconnaît pas (D119) : « off », « no », ou
// « false » entre guillemets laissent la politique complète en place, en
// silence. Le plan l'annonce en information et doctor le rappelle ; jamais le
// chargement, qui arrêterait le parc sur une déclaration servie jusque-là.
func HSTSOptionProblems(options map[string]any) []string {
	var out []string
	if v, ok := options["hsts"]; ok {
		switch v := v.(type) {
		case bool:
		case string:
			if !strings.EqualFold(v, "none") {
				out = append(out, fmt.Sprintf("options.hsts %q non reconnue (true, false ou none) : "+
					"la politique complète est servie", v))
			}
		default:
			out = append(out, fmt.Sprintf("options.hsts %v non reconnue (true, false ou none) : "+
				"la politique complète est servie", v))
		}
	}
	if v, ok := options["hstsSubdomains"]; ok {
		if _, isBool := v.(bool); !isBool {
			out = append(out, fmt.Sprintf("options.hstsSubdomains %v non reconnue (true ou false) : "+
				"includeSubDomains reste posé", v))
		}
	}
	return out
}

func optionString(options map[string]any, key, fallback string) string {
	if options == nil {
		return fallback
	}
	if v, ok := options[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return fallback
}

func optionBool(options map[string]any, key string, fallback bool) bool {
	if options == nil {
		return fallback
	}
	if v, ok := options[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return fallback
}

func diffText(path string, expected []byte) (string, error) {
	current, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "nouveau fichier " + path, nil
		}
		return "", err
	}
	return fs.UnifiedDiff(string(current), string(expected), path), nil
}
