package exec

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
)

// UserPath est le PATH d'une commande lancée sous l'identité d'un compte :
// celui des tâches planifiées d'ogs (template cron/project.tmpl), pour que
// « ogs cron run » rende ce que cron rendra.
const UserPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// account est ce qu'exec retient d'un compte pour lancer une commande sous son
// identité.
type account struct {
	name, uid, gid, home string
}

// lookupAccount résout un compte ; les tests le remplacent.
var lookupAccount = func(name string) (account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return account{}, err
	}
	return account{name: u.Username, uid: u.Uid, gid: u.Gid, home: u.HomeDir}, nil
}

// Les outils de changement d'identité sont cherchés à leur place, jamais dans le
// PATH : root ne lance pas sous ce nom ce qu'un répertoire de son PATH lui
// présenterait. Tous deux viennent d'util-linux, paquet essentiel de Debian 12
// et 13 comme d'Ubuntu 22.04 et 24.04. Les tests remplacent ces listes.
var (
	setprivPaths = []string{"/usr/bin/setpriv", "/bin/setpriv"}
	runuserPaths = []string{"/usr/sbin/runuser", "/sbin/runuser"}
)

// errNoSwitcher dit qu'aucun outil de changement d'identité n'est présent.
var errNoSwitcher = errors.New("ni setpriv ni runuser (util-linux) n'est présent : ogs ne lance plus de " +
	"commande sous l'identité d'un projet par su, qui l'ouvre dans une session hors de son contrôle (D122) — " +
	"installer util-linux")

// UserSwitcher rend l'outil par lequel une commande passe sous l'identité d'un
// compte — setpriv, runuser à défaut —, ou l'erreur qui dit qu'il n'y en a pas.
// doctor le contrôle.
func UserSwitcher() (string, error) {
	if tool := firstTool(setprivPaths); tool != "" {
		return tool, nil
	}
	if tool := firstTool(runuserPaths); tool != "" {
		return tool, nil
	}
	return "", errNoSwitcher
}

// asUser rend la commande réellement lancée pour cmd.AsUser, et son
// environnement.
//
// setpriv d'abord : il pose l'uid, le gid et les groupes supplémentaires du
// compte (--init-groups, d'après /etc/group) puis exécute la commande à sa
// place — même processus, et aucune session PAM. runuser à défaut, avec -u : il
// exécute la commande sans shell et n'ouvre qu'une session PAM (pam_limits,
// sans logind). Dans les deux cas, Local.Run a ouvert une nouvelle session Unix
// dont la commande est le chef (procAttr) : son groupe de processus est celui
// que l'annulation tue, et elle n'a aucun terminal de contrôle.
//
// su n'est jamais employé : « su -c » ouvre lui-même la nouvelle session — c'est
// la protection d'util-linux contre l'injection par TIOCSTI dans le terminal de
// root —, mais dans un processus fils, hors du groupe que le SIGKILL d'ogs
// atteint (R4-27). Le setsid est donc fait ici, par ogs, sur le processus qu'il
// tue.
//
// Sans session PAM, pam_limits ne s'applique plus : la commande hérite des
// limites (rlimits) d'ogs, celles de root (§20).
func (l *Local) asUser(cmd Command) (string, []string, []string, error) {
	acct, err := lookupAccount(cmd.AsUser)
	if err != nil {
		return "", nil, nil, fmt.Errorf("compte %s introuvable : %w", cmd.AsUser, err)
	}
	argv := append([]string{"--", cmd.Name}, cmd.Args...)
	env := userEnv(acct, cmd.Env)
	if tool := firstTool(setprivPaths); tool != "" {
		return tool, append([]string{"--reuid=" + acct.uid, "--regid=" + acct.gid, "--init-groups"}, argv...),
			env, nil
	}
	tool, err := UserSwitcher()
	if err != nil {
		return "", nil, nil, err
	}
	return tool, append([]string{"-u", acct.name}, argv...), env, nil
}

// firstTool rend le premier exécutable présent de la liste.
func firstTool(paths []string) string {
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// inheritedVar dit si une variable d'ogs passe à toute commande lancée sous un
// compte : la langue et le fuseau, dont dépendent les messages et l'encodage
// des outils, et qui ne portent rien de root.
func inheritedVar(kv string) bool {
	k, _, _ := strings.Cut(kv, "=")
	return k == "LANG" || k == "LANGUAGE" || k == "TZ" || strings.HasPrefix(k, "LC_")
}

// ProxyEnv rend les variables de mandataire HTTP d'ogs (http_proxy,
// https_proxy, no_proxy, all_proxy, toutes casses). Elles ne passent pas à toute
// commande lancée sous un compte : un mandataire authentifié porte ses
// identifiants dans l'URL, que le projet lirait dans son environnement
// (/proc/<pid>/environ). Seul l'appelant qui télécharge — l'installation d'un
// runtime — les demande, par Command.Env (D122).
func ProxyEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToLower(k) {
		case "http_proxy", "https_proxy", "no_proxy", "all_proxy":
			out = append(out, kv)
		}
	}
	return out
}

// userEnv construit l'environnement d'une commande lancée sous un compte, de
// toutes pièces (D92, D122) : HOME, USER, LOGNAME, SHELL et PATH du compte, la
// langue et le fuseau d'ogs, puis ce que l'appelant fournit, qui l'emporte. Rien d'autre
// n'est hérité de root : sous « su » sans « -l », une variable de
// l'environnement d'ogs — un MYSQL_PWD, un PGPASSWORD, SSH_AUTH_SOCK, un
// OGS_PG_ADMIN — passait à l'utilisateur du projet. SHELL vaut /bin/sh, comme
// sous cron : le shell du compte peut être fermé (D62), et aucune commande
// lancée par ogs n'en dépend.
func userEnv(acct account, extra []string) []string {
	env := []string{
		"HOME=" + acct.home,
		"USER=" + acct.name,
		"LOGNAME=" + acct.name,
		"SHELL=/bin/sh",
		"PATH=" + UserPath,
	}
	for _, kv := range os.Environ() {
		if inheritedVar(kv) {
			env = append(env, kv)
		}
	}
	// os/exec retient la dernière valeur d'une variable répétée.
	return append(env, extra...)
}
