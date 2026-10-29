// Package model porte les structures du domaine et leur normalisation.
package model

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

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
	Kind       AppKind `yaml:"kind"`
	Version    string  `yaml:"version,omitempty"` // « auto » ou une version explicite
	Runtime    string  `yaml:"runtime,omitempty"` // version du runtime (node, ruby, python)
	Entrypoint string  `yaml:"entrypoint,omitempty"`
	// Command est la ligne ExecStart d'une application « custom » (D75), et
	// d'elle seule : les autres types la déduisent de leur runtime. Les variables
	// d'environnement y sont développées par systemd — ${PORT}, ${SOCKET_PATH}.
	Command   string            `yaml:"command,omitempty"`
	Listen    string            `yaml:"listen,omitempty"` // socket | port
	Instances int               `yaml:"instances,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	// EnvFile branche sur l'unité le fichier d'environnement du projet,
	// /etc/ogs/env/<id>.env (D75). Son emplacement n'est pas réglable : systemd
	// le lit en root, et un fichier que l'utilisateur pourrait remplacer par un
	// lien lui ferait lire les secrets d'un voisin.
	EnvFile bool    `yaml:"envFile,omitempty"`
	Health  *Health `yaml:"health,omitempty"`
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
	Owner string `yaml:"owner,omitempty"` // vide = utilisateur du projet ; « root » sinon
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
	// MaxConnections borne les connexions simultanées du compte principal
	// (D127) : absent, la limite est calculée depuis la déclaration du projet
	// (workers PHP, services, tâches, marge) ; -1 retire toute limite.
	MaxConnections int `yaml:"maxConnections,omitempty"`
}

// DBUser est un utilisateur de base additionnel.
type DBUser struct {
	User       string `yaml:"user"`
	Privileges string `yaml:"privileges"`
	// MaxConnections borne ses connexions simultanées (D127) : absent, 10 ; -1
	// retire toute limite.
	MaxConnections int `yaml:"maxConnections,omitempty"`
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
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	// Preset est le dimensionnement **déclaré**. Vide, le pool suit le preset
	// par défaut de la machine (« php.pool.defaultPreset » de config.yaml, D103),
	// descendu d'un cran en préproduction si l'environnement le demande. La
	// normalisation n'y écrit plus rien : un défaut recopié dans la déclaration
	// ne se distinguerait plus d'un choix, et un changement de config.yaml ne
	// toucherait jamais les projets créés avant lui. Les déclarations écrites
	// avant D103 portent « medium » en toutes lettres : elles le gardent.
	Preset   string       `yaml:"preset,omitempty"`
	Listen   string       `yaml:"listen,omitempty"` // unix | tcp
	Number   string       `yaml:"number,omitempty"` // « auto » ou un numéro imposé
	Settings PoolSettings `yaml:"settings,omitempty"`
	// AllowFunctions rouvre des fonctions de la liste disable_functions que le
	// pool hérite (D103), sans recopier la liste.
	AllowFunctions []string          `yaml:"allowFunctions,omitempty"`
	Env            map[string]string `yaml:"env,omitempty"`
}

// PresetLabel nomme le preset déclaré, ou dit que le pool suit celui de la
// machine.
func (p Pool) PresetLabel() string {
	if p.Preset == "" {
		return "preset de la machine"
	}
	return p.Preset
}

// PoolSettings porte des réglages FPM. Une valeur nulle (« ~ » en YAML) retire
// la clé : la valeur héritée des couches inférieures — socle d'ogs, défauts de
// la machine, preset, type d'application — disparaît du pool rendu (D103). Une
// chaîne vide, elle, est une valeur.
type PoolSettings map[string]*string

// Value rend l'adresse d'une valeur de réglage, pour écrire une table littérale.
func Value(v string) *string { return &v }

// Clone duplique la table : les valeurs pointées sont copiées, pas partagées.
func (s PoolSettings) Clone() PoolSettings {
	if s == nil {
		return nil
	}
	out := make(PoolSettings, len(s))
	for k, v := range s {
		if v != nil {
			v = Value(*v)
		}
		out[k] = v
	}
	return out
}

// Get rend la valeur d'une clé, et faux si elle est absente ou retirée.
func (s PoolSettings) Get(key string) (string, bool) {
	v, ok := s[key]
	if !ok || v == nil {
		return "", false
	}
	return *v, true
}

// Web décrit les vhosts.
type Web struct {
	// Engine n'est conservé que pour relire les déclarations antérieures : le
	// moteur web est un choix de machine (« web.engine » de config.yaml), et ce
	// champ n'a jamais été lu. Normalize l'efface, si bien qu'il disparaît du
	// fichier à sa prochaine écriture — le garder aurait laissé croire qu'un
	// projet peut être servi par un autre moteur que ses voisins.
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
	Name string `yaml:"name"`
	// File est le nom réel du fichier d'un vhost adopté, dans sites-available et
	// sites-enabled. Le nom suivi de l'extension du moteur ne le désigne pas
	// toujours : l'usage Debian pour nginx est un fichier sans extension
	// (« shop.fr »). Vide, le fichier se déduit du nom. L'adoption ne renomme
	// jamais (§9) : sans ce champ, le premier apply écrivait un second fichier à
	// côté de l'original.
	File     string         `yaml:"file,omitempty"`
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
	// DNSCredentials désigne le jeu d'identifiants DNS du challenge dns-01 ; vide,
	// chaque nom reçoit celui de sa zone (D97).
	DNSCredentials string `yaml:"dnsCredentials,omitempty"`
	KeyType        string `yaml:"keyType,omitempty"`
	Staging        bool   `yaml:"staging,omitempty"`
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
//
// Les domaines sont rendus canonisés (MailDomain), doublons écartés (D108) : la
// validation compare des formes normalisées, et ogs employait la chaîne brute —
// « Shop.FR. » nommait un répertoire de clés et des lignes de table que
// l'enregistrement publié ne reconnaissait pas. Canoniser à la lecture, jamais
// refuser au chargement : la forme brute a été acceptée jusque-là.
func (p *Project) DKIMDomains() []string {
	if p.Spec.Mail == nil || p.Spec.Mail.DKIM == nil || !p.Spec.Mail.DKIM.Enabled {
		return nil
	}
	var raw []string
	if len(p.Spec.Mail.DKIM.Domains) > 0 {
		raw = p.Spec.Mail.DKIM.Domains
	} else {
		for _, d := range p.Spec.Domains {
			if d.Role == "" || d.Role == "primary" {
				raw = append(raw, d.Name)
			}
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range raw {
		name := MailDomain(d)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// MailDomain canonise un domaine d'envoi : sans blancs autour, en minuscules,
// sans point final. C'est la forme sous laquelle ogs nomme les clés, les lignes
// des tables et l'enregistrement à publier.
func MailDomain(d string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
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

// VHostExt est l'extension des fichiers de vhost, la même pour les deux
// moteurs : Apache n'inclut que sites-enabled/*.conf.
const VHostExt = ".conf"

// VHosts renvoie les vhosts déclarés, ou ceux que le projet déduit. Un projet
// n'a pas à déclarer son vhost : le type d'application et les domaines
// suffisent, les domaines « redirect » faisant naître en plus un vhost de
// redirection par cible distincte (D119).
//
// La règle vit ici, avec celle des noms de fichiers, parce que deux lecteurs en
// dépendent : le provider web, qui écrit les fichiers, et le contrôle d'unicité
// du parc, qui refuse que deux projets visent le même. Recopiée de l'un à
// l'autre, elle aurait divergé sans que rien ne le signale, et le contrôle
// aurait approuvé des collisions que l'apply aurait trouvées.
func (p *Project) VHosts() []VHost {
	if len(p.Spec.Web.VHosts) > 0 {
		return p.Spec.Web.VHosts
	}
	at, err := p.AppType()
	if err != nil {
		return nil
	}
	var domains []string
	for _, d := range p.Spec.Domains {
		switch d.Role {
		case "", "primary", "alias":
			domains = append(domains, d.Name)
		}
	}
	if len(domains) == 0 {
		return nil
	}
	main := VHost{
		Name:     MainPoolName,
		Template: at.VHostTemplate,
		Domains:  domains,
		Docroot:  p.Spec.Layout.Docroot,
	}
	if at.NeedsPHP && len(p.Spec.PHP.Pools) > 0 {
		main.Pool = p.Spec.PHP.Pools[0].Name
	}
	out := []VHost{main}
	// Un vhost de redirection par cible distincte (D119), dans l'ordre où chaque
	// cible apparaît : celui du premier domaine servi garde le nom « redirect »,
	// donc le fichier qu'il avait avant que la cible déclarée soit honorée.
	targets, first := p.RedirectTargets()
	index := map[string]int{}
	for _, rt := range targets {
		key := canonicalTarget(rt.Target)
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, VHost{
				Name:     redirectVHostName(rt.Target, first),
				Template: "redirect",
				Options:  map[string]any{"target": rt.Target},
			})
		}
		out[i].Domains = append(out[i].Domains, rt.Domain)
	}
	return out
}

// VHostName traduit le nom logique d'un vhost en nom de base de son fichier :
// le vhost principal porte le nom nu du projet, les autres un suffixe de rôle.
// Un projet adopté conserve en revanche le nom réel de ses fichiers : les
// renommer reviendrait à recréer le vhost, et son domaine apparaîtrait en
// conflit avec lui-même.
func (p *Project) VHostName(vh VHost) string {
	if vh.Name == "" || vh.Name == MainPoolName {
		return p.ID()
	}
	if p.Metadata.Labels["adopted"] == "true" {
		return vh.Name
	}
	return p.ID() + "_" + vh.Name
}

// VHostFile est le nom du fichier d'un vhost dans sites-available et
// sites-enabled. Celui d'un vhost adopté est son nom réel, que l'import porte
// dans « file » : l'usage Debian pour nginx est un fichier sans extension
// (« shop.fr »), que le nom du vhost suivi de « .conf » ne désigne pas. Faute
// de quoi le premier apply écrivait un second fichier à côté de l'original,
// servant les mêmes domaines — et l'original, rattaché au projet par un nom
// tronqué, partait comme obsolète.
func (p *Project) VHostFile(vh VHost) string {
	if vh.File != "" {
		return vh.File
	}
	return p.VHostName(vh) + VHostExt
}

// Normalize remplit les valeurs déductibles : identifiant, host effectif, home,
// utilisateur, arborescence et docroot. Les valeurs déjà renseignées sont
// respectées, ce qui permet de surcharger n'importe quel défaut.
// DefaultLogsRoot est la racine des journaux des projets quand l'appelant n'en
// fournit pas (D71) : l'importateur, les tests. La configuration du parc la règle
// par « logs.root », que la CLI transmet par WithLogsRoot.
const DefaultLogsRoot = "/var/log/ogs-projects"

// NormalizeOption règle un défaut de la normalisation qui vient du parc.
type NormalizeOption func(*normalizeOptions)

type normalizeOptions struct{ logsRoot string }

// WithLogsRoot fixe la racine des journaux, préfixe éventuel compris.
func WithLogsRoot(root string) NormalizeOption {
	return func(o *normalizeOptions) {
		if root != "" {
			o.logsRoot = root
		}
	}
}

func (p *Project) Normalize(homeRoot, prepDomain string, opts ...NormalizeOption) error {
	options := normalizeOptions{logsRoot: DefaultLogsRoot}
	for _, o := range opts {
		o(&options)
	}
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

	p.Spec.Web.Engine = ""

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
	// Les journaux vivent hors du home (D71). Une déclaration antérieure porte
	// l'ancien défaut, <home>/logs : elle est ramenée au nouveau, et la migration
	// reprend le contenu de l'ancien répertoire. Un autre chemin a été choisi par
	// l'exploitant ; c'est la validation qui dira s'il est acceptable.
	if p.Spec.Layout.LogDir == "" || filepath.Clean(p.Spec.Layout.LogDir) == LegacyLogDir(p.Spec.Layout.Root) {
		p.Spec.Layout.LogDir = filepath.Join(options.logsRoot, id)
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
	//
	// Le preset n'y est pas écrit (D103) : le pool suit celui de la machine,
	// que la préproduction descend d'un cran (environments.prep.presetDowngrade).
	if at.NeedsPHP && len(p.Spec.PHP.Pools) == 0 {
		p.Spec.PHP.Pools = []Pool{{Name: MainPoolName, Version: DefaultPHPVersion}}
	}
	for i := range p.Spec.PHP.Pools {
		if p.Spec.PHP.Pools[i].Version == "" {
			p.Spec.PHP.Pools[i].Version = DefaultPHPVersion
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

// LinkDir est le répertoire des liens gérés — interpréteur PHP, runtimes,
// binaire applicatif —, relatif à la racine du projet (D70). C'est celui que la
// convention XDG réserve aux exécutables de l'utilisateur, et que le .profile de
// Debian ajoute au PATH : le projet y trouve la version que l'inventaire déclare
// sans rien configurer. Il appartient à l'utilisateur, comme la convention le
// veut — pip et pipx y écrivent.
const LinkDir = ".local/bin"

// LegacyLinkDir est l'ancien emplacement des liens, retiré à la migration.
const LegacyLinkDir = ".bin"

// BinDir renvoie le chemin absolu du répertoire des liens gérés d'un projet.
func BinDir(root string) string { return filepath.Join(root, filepath.FromSlash(LinkDir)) }

// legacyDirs sont les entrées que les déclarations antérieures à D70 et D71
// portent pour .bin/ et logs/. Elles en sont retirées à la normalisation, et
// seulement sous ces formes exactes : un .bin/ ou un logs/ déclaré autrement
// l'a été par l'exploitant. Deux modes, parce qu'ogs en a écrit deux : 0750
// jusqu'à D51, 0710 ensuite — et la normalisation n'écrase jamais une entrée
// déjà déclarée, si bien qu'un projet créé avant D51 a gardé la première. Ne
// reconnaître que 0710 faisait passer ce .bin pour celui de l'exploitant :
// l'arborescence le recréait à chaque apply, et la migration ne le retirait
// jamais (D116).
var legacyDirs = []Dir{
	{Path: LegacyLinkDir, Mode: "0710", Owner: "root"},
	{Path: LegacyLinkDir, Mode: "0750", Owner: "root"},
	{Path: "logs", Mode: "0710", Owner: "root"},
	{Path: "logs", Mode: "0750", Owner: "root"},
}

func isLegacyDir(d Dir) bool {
	for _, l := range legacyDirs {
		if d == l {
			return true
		}
	}
	return false
}

// DeclaresDir dit si la déclaration porte une entrée de spec.layout.dirs pour
// ce chemin, relatif à la racine. Après normalisation, une telle entrée pour
// .bin est celle de l'exploitant : les formes d'ogs en ont été retirées.
func (p *Project) DeclaresDir(rel string) bool {
	for _, d := range p.Spec.Layout.Dirs {
		if path.Clean(d.Path) == path.Clean(rel) {
			return true
		}
	}
	return false
}

// LegacyLogDir est l'ancien répertoire des journaux d'un projet, dans son home.
// Il ne sert plus qu'à la migration, et le nom y devient un lien (D71).
func LegacyLogDir(root string) string { return filepath.Join(root, "logs") }

// LogLink est le nom du lien de commodité posé dans le home vers les journaux.
const LogLink = "logs"

// defaultDirs complète l'arborescence attendue sans écraser les entrées déjà
// déclarées. Les journaux n'y figurent plus : ils vivent hors du home (D71), un
// répertoire root dans un home se renommant sans privilège. Les liens gérés vivent dans un répertoire de
// l'utilisateur (D70) : ils sont un état réconcilié à chaque apply, non une
// barrière — le home appartenant à l'utilisateur, un répertoire root en dessous
// se renomme et se remplace sans le moindre privilège.
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
		{Path: ".local", Mode: "0700"},
		{Path: LinkDir, Mode: "0700"},
		{Path: ".ssh", Mode: "0700"},
	}
	if at.Docroot != "" && at.Docroot != "www" {
		want = append(want, Dir{Path: at.Docroot, Mode: "0710"})
	}
	existing := make(map[string]bool, len(p.Spec.Layout.Dirs))
	out := make([]Dir, 0, len(want)+len(p.Spec.Layout.Dirs))
	for _, d := range p.Spec.Layout.Dirs {
		if isLegacyDir(d) {
			continue
		}
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
