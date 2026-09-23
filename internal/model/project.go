// Package model porte les structures du domaine et leur normalisation.
package model

import (
	"fmt"
	"path"
	"path/filepath"

	"leblanc.io/open-go-server/internal/naming"
)

// APIVersion et Kind identifient le schéma du fichier projet.
const (
	APIVersion  = "ogs/v1"
	KindProject = "Project"
	// DefaultPHPVersion est la version retenue lorsqu'aucune n'est demandée. La
	// détection des versions réellement installées relève du provider PHP.
	DefaultPHPVersion = "8.3"
	// MainPoolName est le pool principal : il porte le nom nu du projet, les
	// autres reçoivent un suffixe de rôle.
	MainPoolName = "web"
	// DefaultShell est le shell des comptes de projet lorsque ni le projet ni la
	// configuration du parc n'en déclarent (D62). C'est un shell ouvert : la
	// connexion entrante reste décidée par sshd et par les clés autorisées (D61),
	// non par ogs. Le fermer se fait par « user.shell » dans la configuration, ou
	// projet par projet par « spec.user.shell ».
	DefaultShell = "/bin/bash"
)

// Project est l'état désiré d'un projet, tel qu'il est écrit dans
// /etc/ogs/projects/<id>.yaml.
type Project struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

// Metadata identifie le projet.
type Metadata struct {
	Name        string            `yaml:"name"`
	Host        string            `yaml:"host"`
	Env         string            `yaml:"env,omitempty"`
	Description string            `yaml:"description,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
}

// Spec regroupe l'ensemble des ressources déclarées.
type Spec struct {
	App          App               `yaml:"app"`
	User         User              `yaml:"user"`
	Layout       Layout            `yaml:"layout"`
	Domains      []Domain          `yaml:"domains,omitempty"`
	Databases    []Database        `yaml:"databases,omitempty"`
	AMQP         *AMQP             `yaml:"amqp,omitempty"`
	PHP          PHP               `yaml:"php,omitempty"`
	Web          Web               `yaml:"web,omitempty"`
	Certificates []Certificate     `yaml:"certificates,omitempty"`
	Crons        []CronJob         `yaml:"crons,omitempty"`
	Services     []Service         `yaml:"services,omitempty"`
	Sudo         *Sudo             `yaml:"sudo,omitempty"`
	Mail         *Mail             `yaml:"mail,omitempty"`
	Hooks        map[string]string `yaml:"hooks,omitempty"`
}

// App décrit le type d'application et, pour les runtimes non-PHP, le processus.
type App struct {
	Kind       AppKind           `yaml:"kind"`
	Version    string            `yaml:"version,omitempty"` // « auto » ou une version explicite
	Runtime    string            `yaml:"runtime,omitempty"` // version du runtime (node, ruby, python)
	Entrypoint string            `yaml:"entrypoint,omitempty"`
	Listen     string            `yaml:"listen,omitempty"` // socket | port
	Instances  int               `yaml:"instances,omitempty"`
	Env        map[string]string `yaml:"env,omitempty"`
	Health     *Health           `yaml:"health,omitempty"`
}

// Health paramètre l'attente de disponibilité d'une application.
type Health struct {
	Path    string `yaml:"path,omitempty"`
	Timeout string `yaml:"timeout,omitempty"`
}

// User décrit l'utilisateur système du projet.
type User struct {
	Name           string   `yaml:"name"`
	Group          string   `yaml:"group"`
	UID            string   `yaml:"uid,omitempty"` // « auto » ou un entier
	Shell          string   `yaml:"shell,omitempty"`
	Home           string   `yaml:"home"`
	WebAccess      string   `yaml:"webAccess,omitempty"` // group | acl | none
	AuthorizedKeys []string `yaml:"authorizedKeys,omitempty"`
}

// Layout décrit l'arborescence du projet.
type Layout struct {
	Root    string `yaml:"root"`
	Docroot string `yaml:"docroot"`
	LogDir  string `yaml:"logDir"`
	Mode    string `yaml:"mode,omitempty"`
	Dirs    []Dir  `yaml:"dirs,omitempty"`
}

// Dir est un répertoire du projet, avec son mode et son propriétaire.
type Dir struct {
	Path  string `yaml:"path"`
	Mode  string `yaml:"mode"`
	Owner string `yaml:"owner,omitempty"` // vide = utilisateur du projet ; « root » pour logs/ et .bin/
}

// Domain est un nom de domaine servi.
type Domain struct {
	Name   string `yaml:"name"`
	Role   string `yaml:"role,omitempty"` // primary | alias | redirect
	Target string `yaml:"target,omitempty"`
}

// Database est une base de données et son utilisateur principal.
type Database struct {
	Engine     string   `yaml:"engine"`
	Cluster    string   `yaml:"cluster,omitempty"`
	Name       string   `yaml:"name"`
	User       string   `yaml:"user"`
	Host       string   `yaml:"host,omitempty"`
	Charset    string   `yaml:"charset,omitempty"`
	Collation  string   `yaml:"collation,omitempty"`
	Privileges string   `yaml:"privileges,omitempty"`
	AuthPlugin string   `yaml:"authPlugin,omitempty"`
	Extensions []string `yaml:"extensions,omitempty"`
	ExtraUsers []DBUser `yaml:"extraUsers,omitempty"`
}

// DBUser est un utilisateur de base additionnel.
type DBUser struct {
	User       string `yaml:"user"`
	Privileges string `yaml:"privileges"`
}

// AMQP décrit le vhost RabbitMQ du projet.
type AMQP struct {
	Vhost       string            `yaml:"vhost"`
	User        string            `yaml:"user"`
	Permissions map[string]string `yaml:"permissions,omitempty"`
	Exchanges   []Exchange        `yaml:"exchanges,omitempty"`
	Queues      []Queue           `yaml:"queues,omitempty"`
	Bindings    []Binding         `yaml:"bindings,omitempty"`
	Policies    []Policy          `yaml:"policies,omitempty"`
}

type Exchange struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"`
	Durable bool   `yaml:"durable"`
}

type Queue struct {
	Name      string         `yaml:"name"`
	Type      string         `yaml:"type,omitempty"` // classic | quorum
	Durable   bool           `yaml:"durable"`
	Arguments map[string]any `yaml:"arguments,omitempty"`
}

type Binding struct {
	Exchange   string `yaml:"exchange"`
	Queue      string `yaml:"queue"`
	RoutingKey string `yaml:"routingKey,omitempty"`
}

type Policy struct {
	Name       string         `yaml:"name"`
	Pattern    string         `yaml:"pattern"`
	Definition map[string]any `yaml:"definition"`
}

// PHP regroupe les pools du projet.
type PHP struct {
	Pools []Pool `yaml:"pools,omitempty"`
}

// Pool est un pool PHP-FPM.
type Pool struct {
	Name     string            `yaml:"name"`
	Version  string            `yaml:"version"`
	Preset   string            `yaml:"preset,omitempty"`
	Listen   string            `yaml:"listen,omitempty"` // unix | tcp
	Number   string            `yaml:"number,omitempty"` // « auto » ou un numéro imposé
	Settings map[string]string `yaml:"settings,omitempty"`
	Env      map[string]string `yaml:"env,omitempty"`
}

// Web décrit le moteur et les vhosts.
type Web struct {
	Engine string  `yaml:"engine,omitempty"`
	VHosts []VHost `yaml:"vhosts,omitempty"`
	// TLS porte le réglage de portée projet. Il est nécessaire parce que le vhost
	// est le plus souvent implicite — déduit des domaines : sans lui, un projet
	// sans section « vhosts » n'aurait aucun endroit où dire qu'il reste en HTTP.
	TLS *TLS `yaml:"tls,omitempty"`
}

// TLSDisabled indique que le projet renonce explicitement au chiffrement. Le
// défaut reste l'inverse : un site public sans certificat n'est plus acceptable.
func (w Web) TLSDisabled() bool { return w.TLS != nil && !w.TLS.Enabled }

// VHost est un hôte virtuel.
type VHost struct {
	Name     string         `yaml:"name"`
	Template string         `yaml:"template,omitempty"`
	Domains  []string       `yaml:"domains"`
	Docroot  string         `yaml:"docroot,omitempty"`
	Pool     string         `yaml:"pool,omitempty"`
	TLS      *TLS           `yaml:"tls,omitempty"`
	Options  map[string]any `yaml:"options,omitempty"`
	Blocks   []Block        `yaml:"blocks,omitempty"`
}

// TLS référence un certificat, de projet ou partagé (@nom).
type TLS struct {
	Enabled     bool   `yaml:"enabled"`
	Certificate string `yaml:"certificate,omitempty"`
}

// Block est un fragment de configuration brut injecté dans un vhost.
type Block struct {
	Name    string `yaml:"name"`
	Content string `yaml:"content"`
}

// Certificate est un certificat propre au projet.
type Certificate struct {
	Name        string   `yaml:"name"`
	Domains     []string `yaml:"domains"`
	Challenge   string   `yaml:"challenge,omitempty"`
	DNSProvider string   `yaml:"dnsProvider,omitempty"`
	KeyType     string   `yaml:"keyType,omitempty"`
	Staging     bool     `yaml:"staging,omitempty"`
}

// CronJob est une tâche planifiée.
type CronJob struct {
	Name     string `yaml:"name"`
	Schedule string `yaml:"schedule"`
	Command  string `yaml:"command"`
	PHP      string `yaml:"php,omitempty"`
	Workdir  string `yaml:"workdir,omitempty"`
	Lock     *bool  `yaml:"lock,omitempty"`
	Log      string `yaml:"log,omitempty"`
	Enabled  *bool  `yaml:"enabled,omitempty"`
}

// Service est un service systemd généré depuis un template.
type Service struct {
	Name      string            `yaml:"name"`
	Template  string            `yaml:"template"`
	Instances int               `yaml:"instances,omitempty"`
	Params    map[string]string `yaml:"params,omitempty"`
	Enabled   *bool             `yaml:"enabled,omitempty"`
}

// Sudo paramètre la délégation pour ce projet.
type Sudo struct {
	Style string   `yaml:"style,omitempty"`
	Verbs []string `yaml:"verbs,omitempty"`
}

// Mail regroupe ce qui touche au courrier sortant du projet. ogs n'en gère que
// la signature DKIM (D69) : le MTA, ses relais, SPF et DMARC restent dehors.
type Mail struct {
	DKIM *DKIM `yaml:"dkim,omitempty"`
}

// DKIM demande la signature du courrier du projet. Elle est opt-in : beaucoup de
// projets envoient par un relais tiers qui signe de sa propre clé, et signer par
// défaut créerait des clés dont aucun courrier ne se sert.
type DKIM struct {
	Enabled bool `yaml:"enabled"`
	// Domains énumère les domaines d'envoi. Vide, ce sont les domaines primaires
	// du projet — jamais ceux de redirection, qui n'émettent rien.
	Domains []string `yaml:"domains,omitempty"`
}

// DKIMDomains renvoie les domaines dont le courrier doit être signé, défaut
// appliqué ; nil si le projet ne demande pas de signature. Le défaut n'est pas
// matérialisé dans le fichier : un domaine primaire ajouté plus tard doit être
// signé sans qu'il faille y penser.
func (p *Project) DKIMDomains() []string {
	if p.Spec.Mail == nil || p.Spec.Mail.DKIM == nil || !p.Spec.Mail.DKIM.Enabled {
		return nil
	}
	if len(p.Spec.Mail.DKIM.Domains) > 0 {
		return append([]string(nil), p.Spec.Mail.DKIM.Domains...)
	}
	var out []string
	for _, d := range p.Spec.Domains {
		if d.Role == "" || d.Role == "primary" {
			out = append(out, d.Name)
		}
	}
	return out
}

// New construit un projet neuf à partir d'un host et d'un environnement.
func New(host string, env naming.Env, kind AppKind) *Project {
	return &Project{
		APIVersion: APIVersion,
		Kind:       KindProject,
		Metadata:   Metadata{Host: host, Env: string(env)},
		Spec:       Spec{App: App{Kind: kind, Version: "auto"}},
	}
}

// Env renvoie l'environnement du projet.
func (p *Project) Env() naming.Env {
	e, err := naming.ParseEnv(p.Metadata.Env)
	if err != nil {
		return naming.EnvProd
	}
	return e
}

// ID renvoie l'identifiant du projet.
func (p *Project) ID() string { return p.Metadata.Name }

// PrimaryDomain renvoie le domaine servi en premier, celui que le vhost porte en
// ServerName. C'est le host effectif tel qu'il a été matérialisé à la
// normalisation : le relire vaut mieux que le recalculer, une convention de
// nommage pouvant changer après qu'un projet a été déclaré.
func (p *Project) PrimaryDomain() string {
	for _, d := range p.Spec.Domains {
		if d.Role == "" || d.Role == "primary" {
			return d.Name
		}
	}
	return ""
}

// IsPrep indique si le projet est une préproduction.
func (p *Project) IsPrep() bool { return p.Env() == naming.EnvPrep }

// AppType résout le type d'application du projet.
func (p *Project) AppType() (AppType, error) {
	return Resolve(p.Spec.App.Kind, p.Spec.App.Version)
}

// Normalize remplit les valeurs déductibles : identifiant, host effectif, home,
// utilisateur, arborescence et docroot. Les valeurs déjà renseignées sont
// respectées, ce qui permet de surcharger n'importe quel défaut.
func (p *Project) Normalize(homeRoot, prepDomain string) error {
	if p.APIVersion == "" {
		p.APIVersion = APIVersion
	}
	if p.Kind == "" {
		p.Kind = KindProject
	}
	env, err := naming.ParseEnv(p.Metadata.Env)
	if err != nil {
		return err
	}
	p.Metadata.Env = string(env)

	host, err := naming.NormalizeHost(p.Metadata.Host)
	if err != nil {
		return fmt.Errorf("metadata.host : %w", err)
	}
	p.Metadata.Host = host

	if p.Metadata.Name == "" {
		id, err := naming.ID(host, env, prepDomain)
		if err != nil {
			return err
		}
		p.Metadata.Name = id
	}
	if err := naming.ValidateIdentifier(p.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name : %w", err)
	}
	id := p.Metadata.Name

	effective, err := naming.EffectiveHost(host, env, prepDomain)
	if err != nil {
		return err
	}

	if p.Spec.App.Kind == "" {
		p.Spec.App.Kind = KindStatic
	}
	if p.Spec.App.Version == "" {
		p.Spec.App.Version = "auto"
	}
	at, err := p.AppType()
	if err != nil {
		return fmt.Errorf("spec.app : %w", err)
	}

	// Utilisateur : nom et groupe valent l'identifiant (D23).
	if p.Spec.User.Name == "" {
		p.Spec.User.Name = id
	}
	if p.Spec.User.Group == "" {
		p.Spec.User.Group = p.Spec.User.Name
	}
	// Le shell n'est pas déduit du parc ici : l'appelant a posé le défaut
	// configuré avant d'appeler, et ce repli ne sert qu'aux chemins qui n'ont pas
	// de configuration sous la main — l'importateur, les tests.
	if p.Spec.User.Shell == "" {
		p.Spec.User.Shell = DefaultShell
	}
	if !path.IsAbs(p.Spec.User.Shell) {
		return fmt.Errorf("spec.user.shell doit être un chemin absolu (valeur : %q)", p.Spec.User.Shell)
	}
	if p.Spec.User.UID == "" {
		p.Spec.User.UID = "auto"
	}
	if p.Spec.User.WebAccess == "" {
		p.Spec.User.WebAccess = "group"
	}

	root := filepath.Join(homeRoot, effective)
	if p.Spec.Layout.Root == "" {
		p.Spec.Layout.Root = root
	}
	if p.Spec.User.Home == "" {
		p.Spec.User.Home = p.Spec.Layout.Root
	}
	if p.Spec.Layout.LogDir == "" {
		p.Spec.Layout.LogDir = filepath.Join(p.Spec.Layout.Root, "logs")
	}
	if p.Spec.Layout.Mode == "" {
		// 0711 : seul le propriétaire peut énumérer son home ; les autres ne
		// peuvent que le traverser, ce dont le serveur web a besoin pour atteindre
		// le docroot.
		//
		// Le bit de groupe est là et ne peut pas en être ôté : Unix retient la
		// première classe applicable, sans repli. www-data étant membre du groupe du
		// projet, un home en 0701 lui appliquerait les droits de groupe — nuls — et
		// lui refuserait la traversée que les bits « autres » semblaient accorder.
		// Ajouter www-data au groupe lui retirerait donc l'accès, ce qui n'a rien
		// d'intuitif mais est bien le comportement du noyau.
		p.Spec.Layout.Mode = "0711"
	}
	if p.Spec.Layout.Docroot == "" && at.Docroot != "" {
		p.Spec.Layout.Docroot = filepath.Join(p.Spec.Layout.Root, at.Docroot)
	}
	p.Spec.Layout.Dirs = p.defaultDirs(at)

	if len(p.Spec.Domains) == 0 {
		p.Spec.Domains = []Domain{{Name: effective, Role: "primary"}}
	}

	// Un type d'application PHP implique un pool : le déclarer par défaut évite
	// d'exiger de l'administrateur une saisie qui n'a qu'une réponse sensée.
	if at.NeedsPHP && len(p.Spec.PHP.Pools) == 0 {
		preset := "medium"
		if p.IsPrep() {
			// La préproduction n'a pas à dimensionner comme la production.
			preset = "small"
		}
		p.Spec.PHP.Pools = []Pool{{Name: MainPoolName, Version: DefaultPHPVersion, Preset: preset}}
	}
	for i := range p.Spec.PHP.Pools {
		if p.Spec.PHP.Pools[i].Version == "" {
			p.Spec.PHP.Pools[i].Version = DefaultPHPVersion
		}
		if p.Spec.PHP.Pools[i].Preset == "" {
			p.Spec.PHP.Pools[i].Preset = "medium"
		}
		if p.Spec.PHP.Pools[i].Listen == "" {
			p.Spec.PHP.Pools[i].Listen = "unix"
		}
		if p.Spec.PHP.Pools[i].Number == "" {
			p.Spec.PHP.Pools[i].Number = "auto"
		}
	}
	return nil
}

// defaultDirs complète l'arborescence attendue sans écraser les entrées déjà
// déclarées. logs/ et .bin/ appartiennent à root : le projet ne doit pas pouvoir
// y créer d'entrée, ce qui interdit la substitution d'un fichier de log par un
// lien symbolique (§7.1.2).
//
// Les modes sont en 0710 plutôt qu'en 0750 : le groupe traverse, il n'énumère
// pas. Servir un fichier n'exige que la traversée du répertoire et la lecture du
// fichier ; pouvoir lister le contenu ne sert qu'à explorer, ce dont personne
// n'a besoin — ni le serveur web, ni un voisin membre du groupe.
func (p *Project) defaultDirs(at AppType) []Dir {
	want := []Dir{
		{Path: "www", Mode: "0710"},
		{Path: "var", Mode: "0700"},
		{Path: "var/tmp", Mode: "0700"},
		{Path: "var/sessions", Mode: "0700"},
		{Path: "logs", Mode: "0710", Owner: "root"},
		{Path: ".bin", Mode: "0710", Owner: "root"},
		{Path: ".ssh", Mode: "0700"},
	}
	if at.Docroot != "" && at.Docroot != "www" {
		want = append(want, Dir{Path: at.Docroot, Mode: "0710"})
	}
	existing := make(map[string]bool, len(p.Spec.Layout.Dirs))
	out := make([]Dir, 0, len(want)+len(p.Spec.Layout.Dirs))
	for _, d := range p.Spec.Layout.Dirs {
		existing[path.Clean(d.Path)] = true
		out = append(out, d)
	}
	for _, d := range want {
		if !existing[path.Clean(d.Path)] {
			out = append(out, d)
		}
	}
	return out
}
