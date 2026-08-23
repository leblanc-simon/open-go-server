// Package importer découvre les ressources d'un serveur déjà en production et
// propose de les adopter. Le scan est en lecture seule : rien n'est modifié.
package importer

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/provider/db"
	"github.com/leviathan/ogs/internal/provider/php"
	"github.com/leviathan/ogs/internal/provider/web"
)

// Kind est la nature d'une ressource découverte.
type Kind string

const (
	KindVHost       Kind = "vhost"
	KindPool        Kind = "pool"
	KindUser        Kind = "user"
	KindDatabase    Kind = "database"
	KindCron        Kind = "cron"
	KindUnit        Kind = "unit"
	KindCertificate Kind = "certificate"
)

// Resource est une ressource observée sur le système.
type Resource struct {
	Kind Kind   `json:"kind" yaml:"kind"`
	Name string `json:"name" yaml:"name"`
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	// Attributes porte les informations exploitables pour la corrélation.
	Attributes map[string]string `json:"attributes,omitempty" yaml:"attributes,omitempty"`
}

// Attr lit un attribut.
func (r Resource) Attr(key string) string {
	if r.Attributes == nil {
		return ""
	}
	return r.Attributes[key]
}

// Scanner inspecte le système.
type Scanner struct {
	Paths  config.Paths
	Engine web.Engine
	// DBClients associe un moteur à son client, pour découvrir les bases.
	DBClients map[string]db.Client
	// HomeRoots énumère les racines où chercher des comptes de projet.
	HomeRoots []string
	// SystemUIDMin est l'UID minimal d'un compte non système.
	SystemUIDMin int
}

// Scan découvre l'ensemble des ressources. Aucune écriture n'a lieu : une
// inspection doit pouvoir être lancée sur un serveur de production sans risque.
func (s *Scanner) Scan(ctx context.Context) ([]Resource, error) {
	var out []Resource
	out = append(out, s.scanVHosts()...)
	out = append(out, s.scanPools()...)
	users, err := s.scanUsers()
	if err != nil {
		return nil, err
	}
	out = append(out, users...)
	out = append(out, s.scanCrons()...)
	out = append(out, s.scanUnits()...)
	out = append(out, s.scanCertificates()...)
	dbs, err := s.scanDatabases(ctx)
	if err == nil {
		out = append(out, dbs...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

var (
	reServerName  = regexp.MustCompile(`(?im)^\s*ServerName\s+(\S+)`)
	reServerAlias = regexp.MustCompile(`(?im)^\s*ServerAlias\s+(.+)$`)
	reDocRoot     = regexp.MustCompile(`(?im)^\s*DocumentRoot\s+(\S+)`)
	reNginxNames  = regexp.MustCompile(`(?im)^\s*server_name\s+([^;]+);`)
	reNginxRoot   = regexp.MustCompile(`(?im)^\s*root\s+([^;]+);`)
	reFPMSocket   = regexp.MustCompile(`(?:unix:|fastcgi_pass\s+unix:)([^|;\s]+\.sock)`)
	reFPMTCP      = regexp.MustCompile(`(?:fcgi://|fastcgi_pass\s+)(127\.0\.0\.1:\d+)`)
)

// scanVHosts lit les configurations du serveur web.
func (s *Scanner) scanVHosts() []Resource {
	if s.Engine == nil {
		return nil
	}
	dir := s.Engine.AvailableDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	enabled := map[string]bool{}
	if links, err := os.ReadDir(s.Engine.EnabledDir()); err == nil {
		for _, l := range links {
			enabled[l.Name()] = true
		}
	}
	var out []Resource
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)
		attrs := map[string]string{
			"enabled": strconv.FormatBool(enabled[e.Name()]),
			"managed": strconv.FormatBool(strings.Contains(content, "Généré par ogs")),
		}
		var domains []string
		if m := reServerName.FindStringSubmatch(content); m != nil {
			domains = append(domains, m[1])
		}
		for _, m := range reServerAlias.FindAllStringSubmatch(content, -1) {
			domains = append(domains, strings.Fields(m[1])...)
		}
		for _, m := range reNginxNames.FindAllStringSubmatch(content, -1) {
			for _, name := range strings.Fields(m[1]) {
				if name != "_" {
					domains = append(domains, name)
				}
			}
		}
		if m := reDocRoot.FindStringSubmatch(content); m != nil {
			attrs["docroot"] = m[1]
		} else if m := reNginxRoot.FindStringSubmatch(content); m != nil {
			attrs["docroot"] = strings.TrimSpace(m[1])
		}
		if m := reFPMSocket.FindStringSubmatch(content); m != nil {
			attrs["fpm"] = m[1]
		} else if m := reFPMTCP.FindStringSubmatch(content); m != nil {
			attrs["fpm"] = m[1]
		}
		if len(domains) > 0 {
			attrs["domains"] = strings.Join(dedupe(domains), ",")
		}
		out = append(out, Resource{
			Kind: KindVHost, Name: strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())),
			Path: path, Attributes: attrs,
		})
	}
	return out
}

var rePoolUser = regexp.MustCompile(`(?im)^\s*user\s*=\s*(\S+)`)
var rePoolListen = regexp.MustCompile(`(?im)^\s*listen\s*=\s*(\S+)`)
var rePoolName = regexp.MustCompile(`(?m)^\[([^\]]+)\]`)

// scanPools lit les pools PHP-FPM de toutes les versions installées.
func (s *Scanner) scanPools() []Resource {
	var out []Resource
	for _, version := range php.Detect(s.Paths) {
		entries, err := os.ReadDir(version.PoolDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".conf" {
				continue
			}
			path := filepath.Join(version.PoolDir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			content := string(data)
			attrs := map[string]string{
				"version": version.Version,
				"managed": strconv.FormatBool(strings.Contains(content, "Généré par ogs")),
			}
			if m := rePoolName.FindStringSubmatch(content); m != nil {
				attrs["pool"] = m[1]
			}
			if m := rePoolUser.FindStringSubmatch(content); m != nil {
				attrs["user"] = m[1]
			}
			if m := rePoolListen.FindStringSubmatch(content); m != nil {
				attrs["listen"] = m[1]
			}
			out = append(out, Resource{
				Kind: KindPool, Name: strings.TrimSuffix(e.Name(), ".conf"),
				Path: path, Attributes: attrs,
			})
		}
	}
	return out
}

// scanUsers lit les comptes susceptibles de porter un site.
func (s *Scanner) scanUsers() ([]Resource, error) {
	path := filepath.Join(s.Paths.Prefix(), "/etc/passwd")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	minUID := s.SystemUIDMin
	if minUID == 0 {
		minUID = 1000
	}
	roots := s.HomeRoots
	if len(roots) == 0 {
		roots = []string{"/home", "/var/www", "/srv"}
	}
	var out []Resource
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 7 {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil || uid < minUID {
			continue
		}
		home := fields[5]
		relevant := false
		for _, root := range roots {
			if strings.HasPrefix(home, filepath.Join(s.Paths.Prefix(), root)) ||
				strings.HasPrefix(home, root) {
				relevant = true
			}
		}
		if !relevant {
			continue
		}
		out = append(out, Resource{
			Kind: KindUser, Name: fields[0],
			Attributes: map[string]string{
				"uid": fields[2], "gid": fields[3], "home": home, "shell": fields[6],
				// Le marqueur distingue un compte créé par ogs d'un compte à adopter.
				"managed": strconv.FormatBool(strings.HasPrefix(fields[4], "ogs=")),
			},
		})
	}
	return out, sc.Err()
}

// scanCrons lit /etc/cron.d et les crontabs des utilisateurs.
func (s *Scanner) scanCrons() []Resource {
	var out []Resource
	dir := s.Paths.CronDir()
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			path := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			out = append(out, Resource{
				Kind: KindCron, Name: e.Name(), Path: path,
				Attributes: map[string]string{
					"lines":   strconv.Itoa(countJobs(string(data))),
					"managed": strconv.FormatBool(strings.Contains(string(data), "Généré par ogs")),
				},
			})
		}
	}
	// Les crontabs personnelles sont aussi des tâches à reprendre.
	spool := filepath.Join(s.Paths.Prefix(), "/var/spool/cron/crontabs")
	if entries, err := os.ReadDir(spool); err == nil {
		for _, e := range entries {
			path := filepath.Join(spool, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			out = append(out, Resource{
				Kind: KindCron, Name: "crontab:" + e.Name(), Path: path,
				Attributes: map[string]string{
					"user":  e.Name(),
					"lines": strconv.Itoa(countJobs(string(data))),
				},
			})
		}
	}
	return out
}

func countJobs(content string) int {
	count := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.Contains(line, "=") &&
			!strings.Contains(line, " ") {
			continue
		}
		if len(strings.Fields(line)) >= 6 {
			count++
		}
	}
	return count
}

var reUnitUser = regexp.MustCompile(`(?im)^\s*User\s*=\s*(\S+)`)
var reUnitExec = regexp.MustCompile(`(?im)^\s*ExecStart\s*=\s*(.+)$`)

// scanUnits lit les unités systemd qui ne proviennent pas d'un paquet.
func (s *Scanner) scanUnits() []Resource {
	entries, err := os.ReadDir(s.Paths.SystemdDir())
	if err != nil {
		return nil
	}
	var out []Resource
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".service") {
			continue
		}
		path := filepath.Join(s.Paths.SystemdDir(), e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)
		attrs := map[string]string{
			"managed": strconv.FormatBool(strings.Contains(content, "Généré par ogs")),
		}
		if m := reUnitUser.FindStringSubmatch(content); m != nil {
			attrs["user"] = m[1]
		}
		if m := reUnitExec.FindStringSubmatch(content); m != nil {
			attrs["exec"] = strings.TrimSpace(m[1])
		}
		out = append(out, Resource{Kind: KindUnit, Name: e.Name(), Path: path, Attributes: attrs})
	}
	return out
}

// scanCertificates cherche les certificats existants, y compris ceux de certbot.
func (s *Scanner) scanCertificates() []Resource {
	var out []Resource
	roots := []struct {
		dir    string
		source string
	}{
		{filepath.Join(s.Paths.Prefix(), "/etc/letsencrypt/live"), "certbot"},
		{filepath.Join(s.Paths.ACMEDir(), "certificates"), "ogs"},
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			out = append(out, Resource{
				Kind: KindCertificate, Name: e.Name(),
				Path: filepath.Join(root.dir, e.Name()),
				Attributes: map[string]string{
					"source":  root.source,
					"managed": strconv.FormatBool(root.source == "ogs"),
				},
			})
		}
	}
	return out
}

// scanDatabases interroge les moteurs disponibles.
func (s *Scanner) scanDatabases(ctx context.Context) ([]Resource, error) {
	var out []Resource
	systemSchemas := map[string]bool{
		"information_schema": true, "mysql": true, "performance_schema": true, "sys": true,
		"postgres": true, "template0": true, "template1": true,
	}
	for engine, client := range s.DBClients {
		if client == nil {
			continue
		}
		databases, err := client.Databases(ctx)
		if err != nil {
			continue
		}
		users, _ := client.Users(ctx)
		for name := range databases {
			if systemSchemas[name] {
				continue
			}
			attrs := map[string]string{"engine": engine}
			// La propriété d'une base n'est pas déductible : on rapproche par nom,
			// et l'incertitude est signalée plutôt que tranchée.
			var candidates []string
			for user := range users {
				account := strings.SplitN(user, "@", 2)[0]
				if account == name || strings.HasPrefix(account, name) ||
					strings.HasPrefix(name, account) {
					candidates = append(candidates, user)
				}
			}
			sort.Strings(candidates)
			if len(candidates) > 0 {
				attrs["users"] = strings.Join(candidates, ",")
			}
			out = append(out, Resource{Kind: KindDatabase, Name: name, Attributes: attrs})
		}
	}
	return out, nil
}

func dedupe(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, s := range items {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// Managed indique si une ressource est déjà gérée par ogs.
func Managed(r Resource) bool { return r.Attr("managed") == "true" }

// Summary compte les ressources par nature.
func Summary(resources []Resource) map[Kind]int {
	out := map[Kind]int{}
	for _, r := range resources {
		out[r.Kind]++
	}
	return out
}

// String rend une ressource lisible.
func (r Resource) String() string {
	if r.Path != "" {
		return fmt.Sprintf("%s %s (%s)", r.Kind, r.Name, r.Path)
	}
	return fmt.Sprintf("%s %s", r.Kind, r.Name)
}
