package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
)

// NativePassword est le plugin d'authentification que les anciennes versions de
// PHP exigent de MySQL.
const NativePassword = "mysql_native_password"

// nativePasswordPHP énumère les versions de PHP dont mysqlnd n'emploie pas de
// façon fiable caching_sha2_password, le plugin par défaut de MySQL 8 (D126).
var nativePasswordPHP = map[string]bool{"7.1": true, "7.2": true}

// authPlugins lit les plugins d'authentification d'un serveur MySQL. Seul le
// client MySQL le porte : MariaDB authentifie par mysql_native_password dès
// « IDENTIFIED BY », et PostgreSQL n'a rien de tel.
type authPlugins interface {
	// AuthPlugin rend le plugin d'authentification d'un compte.
	AuthPlugin(ctx context.Context, user, host string) (string, error)
	// PluginActive dit si un plugin est chargé et actif sur le serveur.
	PluginActive(ctx context.Context, name string) (bool, error)
}

func (c *mysqlClient) AuthPlugin(ctx context.Context, user, host string) (string, error) {
	var plugin string
	err := c.db.QueryRowContext(ctx, "SELECT plugin FROM mysql.user WHERE User = ? AND Host = ?",
		user, host).Scan(&plugin)
	return plugin, err
}

func (c *mysqlClient) PluginActive(ctx context.Context, name string) (bool, error) {
	var status string
	err := c.db.QueryRowContext(ctx,
		"SELECT PLUGIN_STATUS FROM information_schema.PLUGINS WHERE PLUGIN_NAME = ?", name).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return strings.EqualFold(status, "ACTIVE"), err
}

// LegacyPHP rend les versions de PHP du projet qui exigent mysql_native_password :
// celles de ses pools, et celles que fixent ses tâches planifiées — elles se
// connectent à la même base.
func LegacyPHP(proj *model.Project) []string {
	seen := map[string]bool{}
	for _, pool := range proj.Spec.PHP.Pools {
		if nativePasswordPHP[pool.Version] {
			seen[pool.Version] = true
		}
	}
	for _, job := range proj.Spec.Crons {
		if nativePasswordPHP[job.PHP] {
			seen[job.PHP] = true
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// declaredPlugin dit si la déclaration fixe elle-même son plugin.
func declaredPlugin(decl model.Database) bool {
	return decl.AuthPlugin != "" && decl.AuthPlugin != "auto"
}

// observePlugin relève, sur un vrai MySQL, ce dont dépend le plugin du compte
// principal : le plugin que porte le compte, et si mysql_native_password est
// actif. Rien n'est lu quand la déclaration n'appelle aucun plugin.
func (s *DBState) observePlugin(ctx context.Context, client Client, proj *model.Project, host string) {
	decl := s.Declaration
	if decl.Engine == "postgres" || s.Endpoint.Engine != "mysql" {
		return
	}
	legacy := LegacyPHP(proj)
	if !declaredPlugin(decl) && len(legacy) == 0 {
		return
	}
	reader, ok := client.(authPlugins)
	if !ok {
		return
	}
	s.legacyPHP = legacy
	if !declaredPlugin(decl) {
		if active, err := reader.PluginActive(ctx, NativePassword); err == nil {
			s.nativeRead, s.NativeActive = true, active
		}
	}
	if s.UserExists {
		if plugin, err := reader.AuthPlugin(ctx, decl.User, host); err == nil {
			s.UserPlugin = plugin
		}
	}
}

// wantedPlugin rend le plugin que les comptes de la base doivent porter, vide
// pour celui du serveur (D126). Une déclaration qui fixe le sien l'emporte ;
// « auto » demande mysql_native_password sur un vrai MySQL dès qu'une version
// de PHP du projet l'exige — MariaDB l'emploie déjà par défaut. Un serveur où le
// plugin est inactif (MySQL 8.4 le livre désactivé, MySQL 9 l'a retiré) garde son
// plugin par défaut, et missing nomme ce qui ne pourra pas se connecter.
func wantedPlugin(state DBState) (plugin, missing string) {
	decl := state.Declaration
	if declaredPlugin(decl) {
		return decl.AuthPlugin, ""
	}
	if len(state.legacyPHP) == 0 {
		return "", ""
	}
	if state.nativeRead && !state.NativeActive {
		return "", "PHP " + strings.Join(state.legacyPHP, ", ")
	}
	return NativePassword, ""
}

// pluginDrift dit si le compte principal existant porte un autre plugin que
// celui voulu. « CREATE USER IF NOT EXISTS » ne touche pas un compte en place :
// sans ce constat, un projet passé de PHP 8 à 7.1 gardait caching_sha2_password.
//
// Seul un plugin à mot de passe est reposé sur un compte en place : un
// authPlugin déclaré accepte tout identifiant (S2R-08), et auth_socket ouvrirait
// le compte au compte système homonyme — la création seule le pose, comme avant.
func pluginDrift(state DBState, plugin string) bool {
	return passwordPlugins[strings.ToLower(plugin)] && state.UserExists && state.UserPlugin != "" &&
		!strings.EqualFold(state.UserPlugin, plugin)
}

// passwordPlugins sont les plugins d'authentification par mot de passe de MySQL.
var passwordPlugins = map[string]bool{NativePassword: true, "caching_sha2_password": true, "sha256_password": true}

// pluginNotice produit l'étape non critique qui nomme un plugin qu'ogs ne peut
// pas poser : inactif sur le serveur, ou sur un compte dont il ne tient pas le
// mot de passe — le changer exige de le reposer.
func pluginNotice(resource string, decl model.Database, state DBState, plugin, missing, host string) *engine.Step {
	var summary string
	var reason error
	switch {
	case missing != "":
		summary = fmt.Sprintf("base %s %s — %s inactif sur le serveur", decl.Engine, decl.Name, NativePassword)
		reason = fmt.Errorf("%s ne s'authentifie pas par caching_sha2_password, et %s est inactif sur ce serveur : le compte "+
			"'%s'@'%s' garde le plugin par défaut, et l'application ne pourra pas se connecter. Activer le "+
			"plugin (MySQL 8.4 : « mysql_native_password=ON » dans la section [mysqld], puis redémarrer ; "+
			"MySQL 9 l'a retiré), ou passer le projet en PHP 7.3 ou plus (« ogs php switch ») (D126)",
			missing, NativePassword, decl.User, host)
	case pluginDrift(state, plugin) && !state.UserOwned:
		summary = fmt.Sprintf("base %s %s — compte %s en %s, %s attendu", decl.Engine, decl.Name,
			decl.User, state.UserPlugin, plugin)
		reason = fmt.Errorf("le compte '%s'@'%s' s'authentifie par %s, et %s est attendu ; ogs n'en tient "+
			"pas le mot de passe, et le changer exige de le reposer. Sur le serveur : ALTER USER '%s'@'%s' "+
			"IDENTIFIED WITH %s BY '<mot de passe de l'application>' (D126)",
			decl.User, host, state.UserPlugin, plugin, decl.User, host, plugin)
	default:
		return nil
	}
	s := engine.NewStep(resource+"/auth", engine.Update, summary)
	s.Critical = false
	s.Apply = func(context.Context) error { return reason }
	return s
}

// alterPluginStatement rend le changement de plugin du compte principal, que
// l'observation a demandé ; vide sinon. Il repose le mot de passe scellé :
// MySQL n'accepte pas de changer de plugin sans lui.
func alterPluginStatement(decl model.Database, state DBState, host, password string) (string, error) {
	if !state.alterPlugin || password == "" {
		return "", nil
	}
	return SetMySQLPassword(decl.User, host, password, decl.AuthPlugin)
}

// pluginNote complète le résumé de l'étape d'un changement de plugin.
func pluginNote(state DBState, plugin string) string {
	if !state.alterPlugin {
		return ""
	}
	why := ""
	if state.pluginAuto {
		why = " pour PHP " + strings.Join(state.legacyPHP, ", ")
	}
	return fmt.Sprintf(" — plugin %s → %s%s (D126)", state.UserPlugin, plugin, why)
}
