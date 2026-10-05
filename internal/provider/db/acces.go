package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
)

// createMySQL est CreateMySQL, sans création de compte quand aucun mot de passe
// n'est donné : c'est le compte existant d'un projet adopté, dont ogs ne tient
// pas le mot de passe (D78). Un « CREATE USER IF NOT EXISTS » sans mot de passe
// créerait un compte ouvert si le compte venait à disparaître entre
// l'observation et l'application ; sans lui, le GRANT échoue (MySQL 8 et MariaDB
// refusent de créer un compte par GRANT, NO_AUTO_CREATE_USER), et l'étape avec.
func createMySQL(spec MySQLSpec) (MySQLStatements, error) {
	stmts, err := CreateMySQL(spec)
	if err == nil && spec.Password == "" {
		stmts.User = nil
	}
	return stmts, err
}

// jokerRevokes rend le retrait des motifs à joker que l'observation a relevés
// sur la base (D111), compte par compte. Il suit les GRANT échappés : le compte
// garde ses droits sur la base pendant toute l'étape.
func jokerRevokes(decl model.Database, state DBState, host string) ([]string, error) {
	users := make([]string, 0, len(state.Jokers))
	for user := range state.Jokers {
		users = append(users, user)
	}
	sort.Strings(users)
	var out []string
	for _, user := range users {
		stmt, err := RevokeMySQLJoker(state.Jokers[user], state.JokerPrivs[user], user, host)
		if err != nil {
			return nil, err
		}
		out = append(out, stmt)
	}
	return out, nil
}

// stateNotes complète le résumé de l'étape d'une base de ce que l'observation a
// relevé et que le plan doit dire : CONNECT rendu à PUBLIC sur une base d'ogs
// (corrigé) ou laissé sur une base adoptée (signalé), un joker retiré ou gardé.
func stateNotes(state DBState, reopened bool) string {
	var notes []string
	switch {
	case reopened:
		notes = append(notes, "CONNECT rendu à PUBLIC : révoqué (D45)")
	case state.Declaration.Engine == "postgres" && state.PublicConnect:
		notes = append(notes, "CONNECT accordé à PUBLIC laissé en l'état : base adoptée (D111)")
	}
	if state.Declaration.Engine == "postgres" && !state.CreatedByOGS() && state.accessRead &&
		state.UserExists && !state.RoleConnect {
		notes = append(notes, fmt.Sprintf("CONNECT accordé au rôle %s (D111)", state.Declaration.User))
	}
	for _, user := range sortedKeys(state.Jokers) {
		notes = append(notes, fmt.Sprintf("joker « %s » de '%s' retiré", state.Jokers[user], user))
	}
	for _, user := range sortedListKeys(state.JokersKept) {
		notes = append(notes, fmt.Sprintf("joker de '%s' gardé : il couvre aussi %s", user,
			strings.Join(state.JokersKept[user], ", ")))
	}
	if len(notes) == 0 {
		return ""
	}
	return " — " + strings.Join(notes, " ; ")
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// recreatedNotice annonce, pour un projet adopté, les comptes que l'étape de la
// base va créer (R1-60, D111) : ogs n'a jamais tenu, à coup sûr, le mot de passe
// que l'application emploie. Un compte sans secret reçoit un mot de passe neuf ;
// un compte dont un secret est scellé est recréé avec lui — mais un apply
// antérieur à D111 scellait pour un compte adopté existant un mot de passe qu'il
// ne posait jamais (R2-21), que l'archive porte et que la restauration
// réinjecte. Dans les deux cas l'application peut perdre sa base : l'étape est
// non critique et échoue une fois l'étape de la base passée — code 6 —, en
// nommant le secret à reprendre et ce qui porte encore l'ancien mot de passe.
func (p *Provider) recreatedNotice(resource string, proj *model.Project, decl model.Database,
	state DBState, sealed *[]string) *engine.Step {
	if !state.Adopted {
		return nil
	}
	var users []string
	if !state.UserExists {
		users = append(users, decl.User)
	}
	for _, u := range decl.ExtraUsers {
		if !state.ExtraUsers[u.User] {
			users = append(users, u.User)
		}
	}
	if len(users) == 0 {
		return nil
	}
	s := engine.NewStep(resource+"/mot-de-passe", engine.Update, fmt.Sprintf(
		"base %s %s — projet adopté : %s recréé, avec un mot de passe neuf ou scellé sans preuve qu'il "+
			"soit celui de l'application ; l'application devra être reconfigurée",
		decl.Engine, decl.Name, strings.Join(users, ", ")))
	s.Critical = false
	s.Apply = func(context.Context) error {
		var fresh, reused, shows []string
		for _, user := range users {
			if slices.Contains(*sealed, user) {
				fresh = append(fresh, user)
			} else {
				reused = append(reused, user)
			}
			shows = append(shows, fmt.Sprintf("« ogs secrets show %s %s »", proj.ID(), secretName(decl.Engine, user)))
		}
		var what []string
		if len(fresh) > 0 {
			what = append(what, strings.Join(fresh, ", ")+" recréé avec un mot de passe neuf, scellé par ogs")
		}
		if len(reused) > 0 {
			what = append(what, strings.Join(reused, ", ")+" recréé avec le mot de passe scellé, qui n'a peut-être "+
				"jamais été celui de l'application : un apply antérieur à D111 scellait pour un compte adopté un "+
				"mot de passe qu'il ne posait pas")
		}
		return fmt.Errorf("base %s %s : le projet est adopté, et ogs n'a jamais tenu à coup sûr le mot de passe "+
			"que l'application emploie — %s. Reconfigurer l'application avec le mot de passe scellé : %s. "+
			"Le fichier d'environnement du projet (D75) et les variables d'environnement de ses pools FPM, "+
			"restaurés tels quels, portent encore l'ancien", decl.Engine, decl.Name, strings.Join(what, " ; "),
			strings.Join(shows, ", "))
	}
	return s
}

// Notice est un constat sur une base d'un projet que doctor rapporte (D111) :
// ce que le plan ne montre pas, faute d'y avoir rien à faire, ou ce qu'il n'a
// pas pu lire.
type Notice struct {
	Engine   string
	Database string
	// Kind vaut NoticePublicConnect, NoticeJokerKept ou NoticeUnavailable.
	Kind string
	// Adopted dit, pour NoticePublicConnect, que la base est adoptée : ogs la
	// laisse en l'état. Sinon, l'apply du projet la referme.
	Adopted bool
	// Owner est le propriétaire de la base PostgreSQL.
	Owner string
	// Account et Covers disent, pour NoticeJokerKept, le compte qui porte le
	// joker et les autres bases qu'il couvre.
	Account string
	Covers  []string
	// Err est le motif d'une base illisible.
	Err error
}

const (
	NoticePublicConnect = "public-connect"
	NoticeJokerKept     = "joker"
	NoticeUnavailable   = "indisponible"
)

// Notices relit, pour chaque base du projet, ce que doctor rapporte : une base
// PostgreSQL ouverte à PUBLIC, un joker MySQL gardé parce qu'il couvre une autre
// base, un moteur injoignable — sans quoi doctor se tairait sur une base qu'il
// n'a pas pu lire. Une requête par base, non l'observation entière du plan.
func (p *Provider) Notices(ctx context.Context, proj *model.Project) []Notice {
	adopted := proj.Metadata.Labels["adopted"] == "true"
	var out []Notice
	for _, decl := range proj.Spec.Databases {
		base := Notice{Engine: decl.Engine, Database: decl.Name}
		unavailable := func(err error) {
			n := base
			n.Kind, n.Err = NoticeUnavailable, err
			out = append(out, n)
		}
		client, err := p.clientFor(decl)
		if err != nil {
			unavailable(err)
			continue
		}
		switch decl.Engine {
		case "postgres":
			acl, ok := client.(databaseAccess)
			if !ok {
				continue
			}
			owner, public, _, err := acl.DatabaseAccess(ctx, decl.Name, decl.User)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				unavailable(err)
			case public:
				n := base
				n.Kind, n.Adopted, n.Owner = NoticePublicConnect, adopted, owner
				out = append(out, n)
			}
		default:
			databases, err := client.Databases(ctx)
			if err != nil {
				unavailable(err)
				continue
			}
			if !databases[decl.Name] {
				continue
			}
			host := decl.Host
			if host == "" {
				host = "localhost"
			}
			accounts := []string{decl.User}
			for _, u := range decl.ExtraUsers {
				accounts = append(accounts, u.User)
			}
			for _, user := range accounts {
				grants, err := client.Grants(ctx, user, host)
				if err != nil {
					unavailable(err)
					break
				}
				if pattern, others := mysqlJoker(grants, decl.Name, databases); pattern != "" && len(others) > 0 {
					n := base
					n.Kind, n.Account, n.Covers = NoticeJokerKept, fmt.Sprintf("'%s'@'%s'", user, host), others
					out = append(out, n)
				}
			}
		}
	}
	return out
}
