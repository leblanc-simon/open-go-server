package cli

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/amqp"
	appprov "leblanc.io/open-go-server/internal/provider/app"
	"leblanc.io/open-go-server/internal/provider/db"
	"leblanc.io/open-go-server/internal/provider/hardening"
	"leblanc.io/open-go-server/internal/provider/monitoring"
	"leblanc.io/open-go-server/internal/provider/php"
	"leblanc.io/open-go-server/internal/provider/system"
	tlsprov "leblanc.io/open-go-server/internal/provider/tls"
	"leblanc.io/open-go-server/internal/provider/web"
	"leblanc.io/open-go-server/internal/validate"
)

// sudoersFile est la racine de la politique sudo. Elle n'est pas dérivée du
// préfixe : le contrôle qui la lit ne vaut que sur un système réel.
const sudoersFile = "/etc/sudoers"

// check est le résultat d'un contrôle de diagnostic.
type check struct {
	Name   string `json:"name" yaml:"name"`
	Status string `json:"status" yaml:"status"` // ok | avertissement | échec
	Detail string `json:"detail" yaml:"detail"`
	Fix    string `json:"fix,omitempty" yaml:"fix,omitempty"`
}

func newDoctorCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnostiquer l'installation et l'environnement",
		Long: "Vérifie les prérequis, les permissions et la cohérence de l'installation.\n" +
			"Chaque anomalie est accompagnée de la commande qui la corrige. Aucun paquet\n" +
			"n'est installé : ogs diagnostique, il n'administre pas la distribution.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := app.runChecks(cmd.Context())
			failed, warned := 0, 0
			for _, c := range checks {
				switch c.Status {
				case "échec":
					failed++
				case "avertissement":
					warned++
				}
			}
			err := app.emit("DoctorReport", checks, nil, func(tw *tabwriter.Writer) {
				fmt.Fprintln(tw, "ÉTAT\tCONTRÔLE\tDÉTAIL")
				for _, c := range checks {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", symbolFor(c.Status), c.Name, c.Detail)
				}
			})
			if err != nil {
				return wrap(CodeError, err)
			}
			for _, c := range checks {
				if c.Fix != "" && c.Status != "ok" {
					app.printf("  → %s : %s\n", c.Name, c.Fix)
				}
			}
			if failed > 0 {
				return exitf(CodeDependency, "%d contrôle(s) en échec, %d avertissement(s)", failed, warned)
			}
			if warned > 0 {
				return exitf(CodePartial, "%d avertissement(s)", warned)
			}
			return nil
		},
	}
}

// orphanCertificateChecks nomme les certificats de projet qu'aucun projet du parc
// ne déclare plus : ceux qu'une suppression antérieure à D100 laissait, clé
// privée comprise, et dont l'expiration faisait avertir doctor à vie.
func orphanCertificateChecks(provs *providers, projects []*model.Project, list []tlsprov.Info,
	add func(name, status, detail, fix string)) {
	used := map[string]bool{}
	for _, p := range projects {
		used[p.ID()] = true
		reqs, _ := provs.TLS.Requests(p)
		for _, r := range reqs {
			used[r.Name] = true
		}
	}
	for _, info := range list {
		if info.Shared || used[info.Name] {
			continue
		}
		add("certificats", "avertissement", "certificat "+info.Name+" sans projet qui le déclare "+
			"(laissé par une suppression antérieure à D100 ?)", "ogs cert delete "+info.Name)
	}
}

// certificateDeclarationChecks nomme les déclarations de certificat à corriger
// (D94) : un nom qui sort de l'espace du projet, ou deux certificats d'un projet
// sous un même nom, que plan et apply refusent au seul projet qui les porte. Le
// chargement du parc les tolère, pour qu'une seule déclaration n'arrête pas le
// renouvellement de tous les certificats.
func certificateDeclarationChecks(projects []*model.Project, add func(name, status, detail, fix string)) {
	for _, p := range projects {
		if err := validate.CertificateNames(p); err != nil {
			add("certificats déclarés", "échec", "projet "+p.ID()+" : "+strings.ReplaceAll(err.Error(), "\n", " ; "),
				"corriger spec.certificates dans "+p.ID()+".yaml ; plan et apply de ce projet le refusent")
		}
	}
}

// fleetUniquenessChecks nomme les conflits d'unicité que porte déjà le parc
// (D94, D106) : plan et apply les refusent au projet visé, en code 4, mais le
// chargement les tolère — une déclaration écrite avant une règle neuve ne doit
// pas arrêter le renouvellement des certificats de tout le parc. Sans doctor,
// rien ne les dirait avant l'apply suivant.
func fleetUniquenessChecks(projects []*model.Project, prepDomain string, add func(name, status, detail, fix string)) {
	err := validate.Uniqueness(projects, prepDomain)
	if err == nil {
		add("unicité du parc", "ok", "aucun conflit entre projets", "")
		return
	}
	for _, line := range strings.Split(err.Error(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fix := "corriger la déclaration de l'un des deux projets ; plan et apply de chacun le refusent"
		if strings.HasPrefix(line, "répertoire de surcharge :") {
			fix = "renommer le vhost de l'un des deux projets (spec.web.vhosts[].name) et déplacer ses " +
				"surcharges sous le nouveau nom : l'ancien répertoire reste sinon orphelin, ses fichiers ignorés"
		}
		add("unicité du parc", "échec", line, fix)
	}
}

// databaseChecks rapporte ce que le plan des bases ne montre pas (D111) : une
// base PostgreSQL ouverte à PUBLIC — celle d'un projet adopté, qu'ogs laisse en
// l'état et dont le plan ne dit rien tant qu'il n'y a rien à faire ; celle
// d'ogs, que l'apply referme —, un joker MySQL gardé parce qu'il couvre une
// autre base, et une base illisible, sans quoi doctor se tairait sur elle.
func databaseChecks(ctx context.Context, provs *providers, projects []*model.Project,
	add func(name, status, detail, fix string)) {
	for _, p := range projects {
		for _, n := range provs.DB.Notices(ctx, p) {
			switch n.Kind {
			case db.NoticeUnavailable:
				add("bases", "avertissement", fmt.Sprintf("projet %s : base %s %s illisible — %v",
					p.ID(), n.Engine, n.Database, n.Err), "")
			case db.NoticeJokerKept:
				add("bases", "avertissement", fmt.Sprintf("projet %s : %s garde un GRANT à joker sur %s, qui couvre "+
					"aussi %s ; ogs ne le retire pas, il porte l'accès à ces bases (D111)", p.ID(), n.Account,
					n.Database, strings.Join(n.Covers, ", ")),
					"accorder ces bases nommément, puis retirer le joker à la main")
			case db.NoticePublicConnect:
				if n.Adopted {
					add("bases", "avertissement", fmt.Sprintf("projet %s : la base adoptée %s (propriétaire %s) "+
						"accorde CONNECT à PUBLIC — tout rôle du cluster peut s'y connecter et lire son catalogue ; "+
						"ogs ne referme pas une base qu'il n'a pas créée (D111)", p.ID(), n.Database, n.Owner),
						fmt.Sprintf("s'assurer qu'aucun autre rôle n'en dépend, puis « REVOKE CONNECT ON DATABASE %s "+
							"FROM PUBLIC » et « GRANT CONNECT » à chaque rôle qui l'emploie", n.Database))
					continue
				}
				add("bases", "avertissement", fmt.Sprintf("projet %s : la base %s accorde de nouveau CONNECT "+
					"à PUBLIC (D45)", p.ID(), n.Database), "ogs apply "+p.ID())
			}
		}
	}
}

// cronLegacyChecks nomme les tâches dont un nom nu n'est résolu dans le
// répertoire de travail que par compatibilité (D101) : l'apply le dit aussi, par
// une étape non critique, mais une tâche ne se relit pas à chaque apply.
func cronLegacyChecks(provs *providers, projects []*model.Project, add func(name, status, detail, fix string)) {
	for _, p := range projects {
		legacy, err := provs.Cron.Legacy(p)
		if err != nil {
			continue // le plan du projet le refuse et le dit
		}
		for _, l := range legacy {
			add("tâches planifiées", "avertissement", "projet "+p.ID()+" : "+l,
				"« ogs cron » ou spec.crons dans "+p.ID()+".yaml")
		}
	}
}

// appListenerChecks vérifie qu'aucun autre compte ne tient le port d'un projet :
// une écoute qui aboutit ne dit pas qui répond, et le vhost relaie tout à qui
// tient le port (D94). Le port est relu dans l'unité écrite — doctor n'attribue
// rien et n'écrit pas dans la base d'état —, et un projet sans unité, ou dont
// l'unité ne porte pas de port, n'est pas éprouvé.
func appListenerChecks(provs *providers, projects []*model.Project, add func(name, status, detail, fix string)) {
	foreign := 0
	for _, p := range projects {
		if !appprov.Applies(p) {
			continue
		}
		who, err := provs.App.ForeignListenerOnDisk(p)
		if err != nil {
			add("écoute applicative", "avertissement", "projet "+p.ID()+" : "+err.Error(), "")
			continue
		}
		if who != "" {
			foreign++
			add("écoute applicative", "échec", "projet "+p.ID()+" : "+who,
				"arrêter ce processus, puis « ogs app restart "+p.ID()+" »")
		}
	}
	if foreign == 0 {
		add("écoute applicative", "ok", "chaque port applicatif est tenu par son projet", "")
	}
}

// listeners énumère les points d'écoute TCP de la machine.
func (a *App) listeners() ([]string, error) {
	res, err := a.Exec.Run(context.Background(), exec.Command{
		Name: "ss", Args: []string{"-tlnH"},
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		// ss -tlnH : State Recv-Q Send-Q Local:Port Peer:Port
		if len(fields) >= 4 {
			out = append(out, fields[3])
		}
	}
	return out, nil
}

func symbolFor(status string) string {
	switch status {
	case "ok":
		return "✓"
	case "avertissement":
		return "⚠"
	}
	return "✗"
}

// checkDefaultAuthorizedKeys contrôle le trousseau de clés commun du parc (D61).
// Un statut vide signifie « pas de trousseau » : il n'y a alors rien à dire.
//
// Sous StrictModes — le défaut d'OpenSSH —, sshd ignore **en silence** un
// authorized_keys qui n'appartient pas à root ou à l'utilisateur, ou qui est
// inscriptible au-delà de son propriétaire : la connexion est refusée sans que
// rien, du côté d'ogs, ne paraisse anormal. Le lien étant résolu avant ce
// contrôle, ce sont les droits du trousseau qui décident, non ceux du lien. Et
// sshd le lit sous l'identité de l'utilisateur du projet : illisible par lui, il
// ne sert à rien non plus.
func (a *App) checkDefaultAuthorizedKeys() (status, detail, fix string) {
	keys := a.Paths.DefaultAuthorizedKeys()
	info, err := os.Stat(keys)
	if err != nil {
		return "", "", ""
	}
	if !info.Mode().IsRegular() {
		return "échec", fmt.Sprintf("%s n'est pas un fichier régulier", keys), ""
	}
	var problems []string
	perm := info.Mode().Perm()
	if perm&0o004 == 0 {
		problems = append(problems,
			fmt.Sprintf("mode %04o : illisible par les utilisateurs des projets", perm))
	}
	// Tout ce qui se juge par rapport à root ne vaut qu'en production : sous
	// préfixe, l'arborescence appartient à qui fait tourner l'essai, et les modes
	// y sont ceux de son umask.
	if a.Prefix == "" {
		if uid, _, ok := system.OwnerOf(info); ok && uid != 0 {
			problems = append(problems, fmt.Sprintf("propriétaire uid %d, attendu root", uid))
		}
		if perm&0o022 != 0 {
			problems = append(problems,
				fmt.Sprintf("mode %04o : inscriptible au-delà de root, sshd l'ignorera", perm))
		}
		// Le répertoire traversé compte autant que le fichier : sshd remonte la chaîne.
		parent := filepath.Dir(keys)
		if pinfo, perr := os.Stat(parent); perr == nil && pinfo.Mode().Perm()&0o022 != 0 {
			problems = append(problems, fmt.Sprintf("%s (mode %04o) est inscriptible au-delà de root",
				parent, pinfo.Mode().Perm()))
		}
	}
	if len(problems) > 0 {
		return "échec", strings.Join(problems, " ; "),
			"chown root:root " + keys + " && chmod 0644 " + keys
	}
	return "ok", fmt.Sprintf("%s (mode %04o), lié dans le .ssh de chaque projet", keys, perm), ""
}

// htaccessChecks signale, pour chaque projet servi par Apache, ce que ses
// .htaccess portent et que le vhost ne reprend pas. Depuis D99, ogs n'active plus
// les .htaccess sous Apache (AllowOverride None) — un .htaccess servi par
// www-data pouvait détourner le serveur vers le socket FPM d'un voisin ou vers
// server-status — et porte lui-même les règles des .htaccess que livre
// Prestashop. Seul est donc signalé ce que le template ne couvre pas : boutique
// en sous-répertoire, URI virtuelles du multi-boutique, directives hors du bloc
// ~~start~~/~~end~~ du .htaccess racine, .htaccess de sous-répertoire qui ne
// ressemble pas à ceux que Prestashop livre ; pour un autre type, tout .htaccess
// qui porte une directive. Un .htaccess vide ne dit rien, et rien n'est lu à
// travers un lien (D51) : un docroot lié est passé.
func (a *App) htaccessChecks(projects []*model.Project, add func(name, status, detail, fix string)) {
	for _, p := range projects {
		if p.Spec.Layout.Docroot == "" {
			continue // application relayée : aucun docroot servi
		}
		reps, err := web.ListerHtaccess(p)
		if err != nil || len(reps) == 0 {
			continue
		}
		surcharges := "les surcharges du docroot du vhost (…/" + p.ID() + "/docroot/*.conf, D63)"
		var constats, propres []string
		for _, rel := range reps {
			contenu, err := web.LireHtaccess(p, rel)
			if err != nil || !web.HtaccessSignificatif(contenu) {
				continue
			}
			switch {
			case p.Spec.App.Kind != model.KindPrestashop:
				propres = append(propres, rel)
			case rel == ".":
				constats = append(constats, web.ConstatsHtaccessRacine(contenu, true)...)
			case web.EngendreParPrestashop(contenu):
				// Boutique installée dans un sous-répertoire du docroot.
				for _, c := range web.ConstatsHtaccessRacine(contenu, true) {
					constats = append(constats, rel+"/ : "+c)
				}
			case !web.HtaccessSousRepertoireCouvert(contenu):
				propres = append(propres, rel)
			}
		}
		if len(propres) > 0 {
			constats = append(constats, "directives propres dans "+listeBornee(propres, 5))
		}
		if len(constats) == 0 {
			continue
		}
		detail := "le .htaccess n'est pas lu sous Apache (AllowOverride None, D99), et ses directives sont ignorées : "
		if p.Spec.App.Kind == model.KindPrestashop {
			detail = "le template reprend les .htaccess livrés par Prestashop, pas ceci (AllowOverride None, D99) : "
		}
		add("htaccess "+p.ID(), "avertissement", detail+strings.Join(constats, " ; "),
			"porter les règles nécessaires dans "+surcharges)
	}
}

// prestashopVersionChecks rapproche, pour chaque Prestashop déployé, la version
// en place des règles que son vhost porte (D99) : elles diffèrent d'une branche à
// l'autre, et le vhost n'est réécrit qu'au prochain apply. Une version
// indéterminée est servie avec les règles de la branche par défaut, ce qui se dit
// aussi.
func prestashopVersionChecks(projects []*model.Project, eng web.Engine, add func(name, status, detail, fix string)) {
	for _, p := range projects {
		if p.Spec.App.Kind != model.KindPrestashop || p.Spec.Layout.Docroot == "" || !web.CodePrestashopPresent(p) {
			continue
		}
		nom := "prestashop " + p.ID()
		version := web.VersionPrestashop(p)
		branche := web.BranchePrestashop(version)
		if branche == "" {
			add(nom, "avertissement", fmt.Sprintf("version de Prestashop indéterminée (%s) : le vhost porte les "+
				"règles de la %s (D99)", valeurOuAbsente(version), web.BrancheParDefaut),
				"src/Core/Version.php (8, 9) ou app/AppKernel.php (1.7), sous le docroot, doit appartenir à son "+
					"propriétaire, ne pas être un lien et ne pas dépasser 256 Kio")
			continue
		}
		vhosts := p.VHosts()
		if len(vhosts) == 0 {
			continue
		}
		chemin := filepath.Join(eng.AvailableDir(), p.VHostFile(vhosts[0]))
		contenu, err := os.ReadFile(chemin)
		if err != nil {
			continue // vhost pas encore écrit : le plan le dira
		}
		if !web.RenduPourBranche(contenu, branche) {
			add(nom, "avertissement", fmt.Sprintf("Prestashop %s en place, mais le vhost %s ne porte pas les "+
				"règles de la %s (D99)", version, chemin, branche), "ogs apply "+p.ID())
			continue
		}
		add(nom, "ok", fmt.Sprintf("Prestashop %s, vhost aux règles de la %s", version, branche), "")
	}
}

func valeurOuAbsente(v string) string {
	if v == "" {
		return "aucune version lisible"
	}
	return "version " + v
}

// listeBornee énumère au plus n éléments, puis en dit le reste.
func listeBornee(elements []string, n int) string {
	for i, e := range elements {
		if e == "." {
			elements[i] = "la racine"
		}
	}
	if len(elements) <= n {
		return strings.Join(elements, ", ")
	}
	return fmt.Sprintf("%s et %d autre(s)", strings.Join(elements[:n], ", "), len(elements)-n)
}

// addConfigKeyChecks nomme les clés inconnues de config.yaml et des fichiers
// d'identifiants (D101). Celles de config.yaml sont un échec : les commandes qui
// écrivent les refusent. Celles des identifiants, écrits par « credentials set »
// et non à la main, un avertissement : un jeu DNS reste servi tel que lu (D97),
// et amqp.yaml aussi, l'avertissement accompagnant chaque commande qui le lit.
func (a *App) addConfigKeyChecks(add func(name, status, detail, fix string)) {
	path := a.Paths.ConfigFile()
	switch {
	case len(a.cfgUnknown) > 0:
		add("configuration", "échec", config.UnknownKeysError(path, a.cfgUnknown).Error(),
			"corriger l'orthographe de la clé ou la retirer : apply et les autres commandes qui écrivent la refusent")
	case len(a.cfgRetired) > 0:
		add("configuration", "avertissement", fmt.Sprintf("%s : clé(s) retirée(s), sans effet : %s", path,
			strings.Join(a.cfgRetired, ", ")), "les retirer du fichier")
	default:
		if _, err := os.Stat(path); err == nil {
			add("configuration", "ok", path+" : aucune clé inconnue", "")
		}
	}
	amqpPath := filepath.Join(a.Paths.Etc(), "credentials", "amqp.yaml")
	if data, err := os.ReadFile(amqpPath); err == nil {
		var creds amqp.Credentials
		if err := yaml.UnmarshalWithOptions(data, &creds, yaml.Strict()); err != nil {
			add("identifiants RabbitMQ", "avertissement", amqpPath+" : "+strictError(err),
				"ogs amqp credentials set --user <compte>")
		}
	}
	entries, _ := os.ReadDir(a.Paths.DNSCredsDir())
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		file := filepath.Join(a.Paths.DNSCredsDir(), e.Name())
		data, err := os.ReadFile(file)
		if err != nil {
			continue // la lecture elle-même est jugée par les contrôles des certificats
		}
		var creds dnsCredentialsFile
		if err := yaml.UnmarshalWithOptions(data, &creds, yaml.Strict()); err != nil {
			add("identifiants DNS "+name, "avertissement", file+" : "+strictError(err),
				"ogs cert dns-provider set "+name)
		}
	}
}

// unknownFieldRe reconnaît le refus d'une clé par le décodeur strict.
var unknownFieldRe = regexp.MustCompile(`unknown field "([^"]*)"`)

// strictError rend en français la première ligne d'un refus du décodeur strict.
func strictError(err error) string {
	return unknownFieldRe.ReplaceAllString(firstLine(err.Error()), "clé inconnue « $1 »")
}

func (a *App) runChecks(ctx context.Context) []check {
	var out []check
	add := func(name, status, detail, fix string) {
		out = append(out, check{Name: name, Status: status, Detail: detail, Fix: fix})
	}

	if a.Prefix == "" && os.Getuid() != 0 {
		add("privilèges", "échec", "ogs n'est pas exécuté en tant que root", "utiliser sudo")
	} else {
		add("privilèges", "ok", fmt.Sprintf("uid %d", os.Getuid()), "")
	}

	a.addConfigKeyChecks(add)

	// Journaux de la base d'état : le mode est RELEVÉ avant toute ouverture —
	// store.Open les remet en 0600, et regarder après reviendrait à taire ce que
	// « doctor » a trouvé sur le disque —, mais le compte rendu vient APRÈS, une
	// fois su ce que cette exécution a corrigé. Annoncer un échec et un « chmod »
	// devenu sans effet ferait courir l'exploitant après une anomalie levée, et
	// donnerait deux verdicts différents à deux exécutions de suite.
	journaux := a.stateJournalModes()

	// Base d'état : les contrôles qui portent sur les secrets en dépendent, et
	// elle n'est ouverte nulle part sur le chemin de « doctor ». Elle ne l'est que
	// si elle existe déjà : store.Open créerait /var/lib/ogs en 0750, là où le
	// challenge ACME exige 0751 — le diagnostic fabriquerait l'anomalie que le
	// contrôle suivant rapporte. Son ouverture ratée reste un avertissement :
	// « doctor » est fait pour tourner sur une installation abîmée, l'absence des
	// répertoires étant par ailleurs déjà dite plus haut.
	if _, err := os.Stat(a.Paths.StateDB()); err == nil {
		if err := a.openStore(); err != nil {
			add("base d'état", "avertissement",
				"base d'état illisible : "+firstLine(err.Error())+
					" — les contrôles qui en dépendent sont omis", "ogs init")
		}
	} else if n := a.lostState(); n > 0 {
		// Disparue d'un parc qui compte des projets : les commandes qui en
		// dépendent refusent en code 5, et rien ne la recrée sans un mot (D120).
		add("base d'état", "échec", a.lostStateMessage(n),
			"restaurer "+a.Paths.StateDB()+" depuis sa sauvegarde ; à défaut, ogs init")
	}
	a.addStateJournalChecks(add, journaux)

	// Arborescence de l'outil.
	for _, d := range a.Paths.Dirs() {
		info, err := os.Stat(d.Path)
		switch {
		case os.IsNotExist(err):
			add("répertoire "+d.Path, "échec", "absent", "ogs init")
		case err != nil:
			add("répertoire "+d.Path, "échec", err.Error(), "")
		case info.Mode().Perm() != d.Mode:
			add("répertoire "+d.Path, "avertissement",
				fmt.Sprintf("mode %04o, attendu %04o", info.Mode().Perm(), d.Mode),
				fmt.Sprintf("chmod %04o %s", d.Mode, d.Path))
		default:
			add("répertoire "+d.Path, "ok", fmt.Sprintf("mode %04o", info.Mode().Perm()), "")
		}
	}

	// Clé maître : sa divulgation expose tous les secrets.
	if info, err := os.Stat(a.Paths.MasterKey()); err != nil {
		add("clé maître", "échec", "absente", "ogs init")
	} else if perm := info.Mode().Perm(); perm&0o077 != 0 {
		add("clé maître", "échec", fmt.Sprintf("mode %04o : lisible au-delà de son propriétaire", perm),
			"chmod 0400 "+a.Paths.MasterKey())
	} else {
		add("clé maître", "ok", fmt.Sprintf("mode %04o", perm), "")
	}

	// Racine des homes : elle doit rester traversable par tous, sinon le serveur
	// web ne peut atteindre aucun docroot. Traversable, non énumérable : la cible
	// est 0711 (D60) — le correctif proposé ne rouvre donc pas la lecture. Et
	// « harden » ne desserre jamais ce qu'un administrateur a fermé : un 0700 ici
	// resterait en place, sans qu'aucun contrôle de configuration ne le dise, la
	// configuration étant par ailleurs valide.
	homeRoot := a.homeRoot()
	if info, err := os.Stat(homeRoot); err != nil {
		add("racine des homes", "échec", err.Error(), "")
	} else if info.Mode().Perm()&0o001 == 0 {
		add("racine des homes", "échec",
			fmt.Sprintf("%s (mode %04o) n'est pas traversable : aucun docroot ne sera servi", homeRoot, info.Mode().Perm()),
			"chmod 0711 "+homeRoot)
	} else {
		add("racine des homes", "ok",
			fmt.Sprintf("%s (mode %04o)", homeRoot, info.Mode().Perm()), "")
	}

	// Trousseau de clés commun : ogs le lie dans le .ssh de chaque projet, mais
	// c'est sshd qui décide s'il le lit. Le contrôle ne s'affiche que si le fichier
	// existe — la fonction est facultative, et un parc qui ne s'en sert pas n'a pas
	// à lire une ligne de plus.
	if status, detail, fix := a.checkDefaultAuthorizedKeys(); status != "" {
		add("trousseau de clés commun", status, detail, fix)
	}

	// Shell des comptes de projet : useradd accepte un chemin qui n'existe pas, et
	// le compte devient alors inutilisable sans le moindre message. Un shell fermé
	// à dessein — nologin — n'est pas un défaut : c'est son absence du disque qui
	// en est un.
	shell := a.Cfg.Shell()
	if _, err := os.Stat(shell); err != nil {
		add("shell des projets", "échec",
			fmt.Sprintf("%s est déclaré (user.shell) mais absent du système", shell),
			"corriger user.shell dans "+a.Paths.ConfigFile())
	} else {
		add("shell des projets", "ok", shell, "")
	}

	// Binaires attendus par le provider système.
	for _, bin := range []string{"useradd", "groupadd", "gpasswd", "usermod"} {
		if path, err := osexec.LookPath(bin); err != nil {
			add("binaire "+bin, "échec", "introuvable dans le PATH", "apt install passwd")
		} else {
			add("binaire "+bin, "ok", path, "")
		}
	}

	// Changement d'identité : tout ce qu'ogs lance sous le compte d'un projet —
	// tâche, installation de runtime, restauration d'une base — passe par
	// setpriv, runuser à défaut, jamais par su (D122).
	if tool, err := exec.UserSwitcher(); err != nil {
		add("changement d'identité", "échec", err.Error(), "apt install util-linux")
	} else {
		add("changement d'identité", "ok", tool, "")
	}

	// Clusters PostgreSQL : l'accès d'administration passe par l'authentification
	// « peer », qui exige un rôle au nom de l'utilisateur système. Une installation
	// neuve n'en comporte pas, et le défaut ne se découvrirait qu'à la création
	// d'une base.
	if a.Prefix == "" && !a.Exec.Simulated() {
		clusters := db.DetectClusters("")
		role := db.PostgresAdminRole(os.Getenv("OGS_PG_ADMIN"))
		for _, cl := range clusters {
			client, err := db.NewPostgresClient(cl, role)
			if err != nil {
				add("cluster postgres "+cl.ID(), "échec", err.Error(), "")
				continue
			}
			if _, err := client.Endpoint(ctx); err != nil {
				add("cluster postgres "+cl.ID(), "échec", err.Error(),
					fmt.Sprintf("su - postgres -c 'createuser --superuser %s'", role))
			} else {
				add("cluster postgres "+cl.ID(), "ok",
					fmt.Sprintf("joignable en tant que %s", role), "")
			}
			client.Close()
		}
	}

	// Droits des répertoires sensibles — configuration et racine des homes : sur un
	// serveur mutualisé, les utilisateurs de projet ont un compte local et lisent
	// tout ce qui n'est pas fermé.
	if etats := a.hardeningProvider().Observe(); len(etats) > 0 {
		if n := hardening.Restrict(etats); n > 0 {
			add("droits des répertoires système", "avertissement",
				fmt.Sprintf("%d répertoire(s) énumérables au-delà de root", n),
				"ogs harden --apply")
		} else {
			add("droits des répertoires système", "ok",
				"configuration et racine des homes fermées", "")
		}
	}

	// Archives expirées : la purge a lieu à l'apply, mais un parc dont on
	// n'applique rien pendant des mois garderait des dumps indéfiniment.
	if retention := a.Cfg.Archives.RetentionDays; retention > 0 {
		expired, err := archive.Expired(a.Paths.ArchivesDir(),
			time.Duration(retention)*24*time.Hour, nowFunc())
		if err == nil && len(expired) > 0 {
			add("archives", "avertissement",
				fmt.Sprintf("%d archive(s) au-delà de %d jours attendent la purge",
					len(expired), retention),
				"ogs apply")
		} else if err == nil {
			add("archives", "ok", fmt.Sprintf("rétention %d jours", retention), "")
		}
	} else {
		add("archives", "ok", "conservées indéfiniment (rétention désactivée)", "")
	}

	// Modules Apache exigés par les vhosts produits. Leur absence ne se traduit
	// pas par un refus clair : Apache accepte la configuration puis rend une 500
	// avec un message obscur, ou ignore la directive en silence.
	if eng, err := a.webEngine(); err == nil && eng.Name() == "apache" {
		mods := filepath.Join(a.Paths.Prefix(), "/etc/apache2/mods-enabled")
		requis := []struct{ nom, pourquoi string }{
			{"ssl", "aucun site ne peut être servi en HTTPS"},
			{"proxy", "aucun relais vers PHP-FPM ni vers une application"},
			{"proxy_fcgi", "PHP ne peut pas être exécuté : le vhost rendra le source"},
			{"proxy_http", "les applications Go, Node, Ruby et Python ne peuvent pas être relayées"},
			{"rewrite", "les redirections vers HTTPS et les front controllers ne fonctionnent pas"},
			{"headers", "les en-têtes de sécurité (HSTS, X-Robots-Tag) sont ignorés"},
			{"status", "le vhost de supervision ne rend pas l'état du serveur : " +
				"SetHandler server-status n'a aucun effet sans ce module, et Apache " +
				"ne s'en plaint pas — il sert une page vide"},
		}
		var manquants []string
		for _, m := range requis {
			if _, err := os.Stat(filepath.Join(mods, m.nom+".load")); err != nil {
				add("mod_"+m.nom, "avertissement",
					"module Apache absent : "+m.pourquoi,
					"a2enmod "+m.nom+" && systemctl reload apache2")
				manquants = append(manquants, m.nom)
			}
		}
		if len(manquants) == 0 {
			add("modules apache", "ok", "tous les modules requis sont activés", "")
		}
	}

	// Journaux des vhosts : le serveur les ouvre tous à son (re)démarrage et
	// s'arrête si l'un résiste, emportant tous les sites. « apachectl -t » n'en dit
	// rien — il ne teste que la syntaxe — et l'échec ne se découvre qu'au
	// rechargement suivant, trop tard. Un /home remonté en lecture seule après une
	// erreur disque suffit à le provoquer.
	if provs, err := a.buildProviders(); err == nil {
		defer provs.close()
		if err := provs.Web.CheckLogs(context.Background()); err != nil {
			add("journaux des vhosts", "échec",
				"un rechargement du serveur web l'arrêterait : "+err.Error(),
				"rétablir l'accès en écriture (montage en lecture seule ? droits du répertoire ?)")
		} else {
			add("journaux des vhosts", "ok", "les journaux des vhosts gérés sont inscriptibles", "")
		}
		// Paires clé/certificat que désignent les vhosts activés (D110) : nginx
		// refuse toute sa configuration, Apache meurt au rechargement suivant —
		// « apachectl -t » n'en dit rien. Doctor n'écrit rien : une écriture
		// interrompue est nommée comme les autres, l'apply ou « cert renew »
		// l'achève.
		if err := provs.Web.CheckCertificates(context.Background(), false); err != nil {
			add("certificats des vhosts", "échec",
				"un rechargement de "+provs.Engine.Unit()+" serait refusé : "+err.Error(),
				"ogs cert renew <nom> (ou remettre la clé d'origine en place)")
		}
		// Surcharges de vhost (D63) : un fichier lu en root par le maître, un
		// fichier déposé hors de tout contexte donc jamais chargé, un répertoire
		// qu'aucun vhost ne référence plus. Rien de tout cela ne se voit dans une
		// vérification de configuration — le serveur démarre et sert.
		problems := provs.Web.AuditIncludes()
		for _, p := range problems {
			status := "avertissement"
			if p.Severe {
				status = "échec"
			}
			add("surcharges de vhost", status, p.Detail, p.Fix)
		}
		if len(problems) == 0 {
			add("surcharges de vhost", "ok",
				"répertoires de surcharge conformes ("+provs.Web.Engine.IncludeRoot()+")", "")
		}
	}

	// Versions PHP en fin de support : signalées, jamais bloquantes.
	// Une déclaration refusée est nommée, et les contrôles suivants tournent sur
	// le reste du parc : s'arrêter sur elle taisait tout ce qui suit (R1-41).
	projects, refused, err := a.loadFleet()
	// Les hooks de parc s'éprouvent même quand un projet ne se charge pas.
	a.hookChecks(projects, add)
	if err != nil {
		add("projets", "avertissement", err.Error(), "")
		return out
	}
	for _, err := range refused {
		add("projets", "échec", err.Error(), "corriger la déclaration : le plan et l'apply du parc la refusent, "+
			"le renouvellement des certificats la saute (code 6)")
	}
	add("projets", "ok", fmt.Sprintf("%d déclaré(s) et lisible(s)", len(projects)), "")
	// Les racines de config.yaml se comparent à celles que portent les
	// déclarations : un changement est refusé au plan et à l'apply (D120).
	for _, p := range a.placementProblems(projects) {
		add("racines du parc", "échec", p.what, p.fix)
	}
	certificateDeclarationChecks(projects, add)
	fleetUniquenessChecks(projects, a.Cfg.PrepDomain(), add)

	// .htaccess sous Apache : ogs ne les active plus (AllowOverride None, D99) et
	// porte lui-même les règles de base. Ce qu'un .htaccess ajoute est ignoré.
	if eng, err := a.webEngine(); err == nil {
		if eng.Name() == "apache" {
			a.htaccessChecks(projects, add)
		}
		prestashopVersionChecks(projects, eng, add)
	}

	// Délégation sudo : un fichier au préfixe ogs- non généré par l'outil, ou
	// élargi à la main, mérite d'être signalé.
	if provs, err := a.buildProviders(); err == nil {
		if problems, err := provs.Sudoers.Audit(); err == nil {
			for _, p := range problems {
				add("sudoers", "échec", p, "vérifier ce fichier à la main")
			}
			if len(problems) == 0 {
				add("sudoers", "ok", "fichiers de délégation conformes", "")
			}
		}
		cronLegacyChecks(provs, projects, add)
		if a.Prefix == "" && !a.Exec.Simulated() {
			databaseChecks(ctx, provs, projects, add)
		}
		provs.close()
	}
	// « ogs my » s'exécute dans l'environnement que sudo reconstruit : env_reset
	// est l'hypothèse sous laquelle la surface déléguée a été taillée (D21/D28).
	if a.Prefix == "" {
		status, detail, fix := envResetPolicy(sudoersFile)
		add("sudo env_reset", status, detail, fix)
	}

	// Applications : une écoute au-delà de la boucle locale contourne le vhost,
	// donc le TLS, les en-têtes de sécurité et l'authentification.
	if a.Prefix == "" && !a.Exec.Simulated() {
		if listeners, err := a.listeners(); err == nil {
			if exposed := appprov.ExposedPorts(listeners); len(exposed) > 0 {
				add("écoute applicative", "échec",
					"port(s) applicatif(s) exposé(s) hors de la boucle locale : "+strings.Join(exposed, ", "),
					"faire écouter l'application sur 127.0.0.1, ou employer un socket Unix")
			} else {
				add("écoute applicative", "ok", "aucun port exposé", "")
			}
		}
		if provs, err := a.buildProviders(); err == nil {
			appListenerChecks(provs, projects, add)
			provs.close()
		}
	}

	// Certificats : le seuil d'alerte d'un partagé est plus précoce, son
	// expiration cassant toutes les préproductions à la fois.
	if provs, err := a.buildProviders(); err == nil {
		defer provs.close()
		if warnings, err := provs.TLS.Warnings(); err == nil {
			for _, w := range warnings {
				add("tls", "avertissement", w, "ogs cert renew --all")
			}
		}
		if list, err := provs.TLS.Store.List(); err == nil && len(list) > 0 {
			add("certificats", "ok", fmt.Sprintf("%d présent(s)", len(list)), "")
			orphanCertificateChecks(provs, projects, list, add)
		}
		// Une référence de certificat que rien ne porte (D102) : le vhost sert le
		// certificat par défaut, et l'apply du projet rend le code 6.
		for _, p := range projects {
			for _, problem := range provs.TLS.ReferenceProblems(p) {
				add("certificats désignés", "avertissement", "projet "+p.ID()+" : "+problem,
					"corriger tls.certificate dans "+p.ID()+".yaml, ou déclarer ce certificat")
			}
		}
	}
	// Identifiants DNS (D97) : un jeu illisible, ou une zone que deux jeux
	// revendiquent, ne se voient qu'au challenge — au renouvellement, des semaines
	// plus tard. Un jeu écarté laisse ses certificats en http-01, sans un mot.
	dnsSets, dnsErrs := tlsprov.LoadDNSCredentials(a.Paths.DNSCredsDir())
	for _, e := range dnsErrs {
		add("identifiants dns", "échec", e.Error(),
			"corriger le fichier, ou le réenregistrer avec « ogs cert dns-provider set »")
	}
	conflicts := tlsprov.DNSZoneConflicts(dnsSets)
	for _, c := range conflicts {
		add("identifiants dns", "échec", c, "ne déclarer chaque zone que dans un seul jeu")
	}
	if len(dnsSets) > 0 && len(dnsErrs) == 0 && len(conflicts) == 0 {
		zones := 0
		for _, s := range dnsSets {
			zones += len(s.Zones)
		}
		add("identifiants dns", "ok", fmt.Sprintf("%d jeu(x), %d zone(s) déclarée(s)", len(dnsSets), zones), "")
	}
	for _, p := range projects {
		for _, pool := range p.Spec.PHP.Pools {
			if date, eol := validate.EOLDate(pool.Version); eol {
				add(fmt.Sprintf("php %s (%s)", pool.Version, p.ID()), "avertissement",
					"support terminé depuis le "+date, "envisager « ogs php switch »")
			}
		}
	}
	// Pools PHP-FPM (D103) : réglages écartés ou refusés, capacité mémoire.
	a.phpPoolChecks(projects, add)
	webAccessChecks(projects, add)
	a.monitoringChecks(add)
	a.mailChecks(ctx, projects, add)
	a.firewallChecks(ctx, add)
	// Restaurations restées en plan (D113).
	a.restoreChecks(add)
	// Journaux des projets, hors du home (D71).
	if provs, err := a.buildProviders(); err == nil {
		for _, f := range provs.System.LogChecks(projects, a.logsRoot()) {
			add(f.Name, f.Status, f.Detail, f.Fix)
		}
		if !a.Exec.Simulated() {
			fpmUnitChecks(ctx, provs.PHP, projects, add)
			renewTimerCheck(ctx, provs.TLS, add)
		}
		provs.close()
	}
	return out
}

// fpmUnitChecks porte, pour chaque version PHP qu'emploie le parc, l'état de
// son unité et « php-fpm -t » (D105) : un FPM mort ou dont la configuration est
// refusée ne se voit d'aucun plan tant qu'aucun pool ne change, et le prochain
// rechargement le refusera.
func fpmUnitChecks(ctx context.Context, prov *php.Provider, projects []*model.Project,
	add func(name, status, detail, fix string)) {
	versions := map[string]bool{}
	for _, p := range projects {
		for _, pool := range p.Spec.PHP.Pools {
			versions[pool.Version] = true
		}
	}
	list := make([]string, 0, len(versions))
	for v := range versions {
		list = append(list, v)
	}
	sort.Strings(list)
	for _, v := range list {
		unit := config.PHPUnit(v)
		name := "php-fpm " + v
		if _, err := os.Stat(prov.Paths.PHPFpmBin(v)); err != nil {
			add(name, "échec", prov.Paths.PHPFpmBin(v)+" introuvable : PHP "+v+" est déclaré mais absent",
				"apt install php"+v+"-fpm")
			continue
		}
		st := prov.Unit(ctx, v)
		if err := prov.Validate(ctx, v); err != nil {
			add(name, "échec", strings.ReplaceAll(err.Error(), "\n", " ; "),
				"corriger la configuration, puis « php-fpm"+v+" -t » : tout rechargement est refusé d'ici là")
			continue
		}
		switch {
		case !st.Active:
			add(name, "échec", unit+" arrêtée : les sites PHP "+v+" ne sont pas servis",
				"« journalctl -u "+unit+" », puis « ogs apply »")
		case !st.Enabled:
			add(name, "avertissement", unit+" désactivée : elle ne repartira pas au démarrage de la machine",
				"ogs apply")
		default:
			add(name, "ok", unit+" active et activée, configuration acceptée", "")
		}
	}
}

// renewTimerCheck dit qu'un timer de renouvellement installé ne tourne pas : plus
// aucun certificat ne se renouvelle, sans un mot (R1-49, D105).
func renewTimerCheck(ctx context.Context, prov *tlsprov.Provider, add func(name, status, detail, fix string)) {
	if etat := prov.RenewTimerProblem(ctx); etat != "" {
		add("timer de renouvellement", "échec", "ogs-renew.timer "+etat+" : aucun certificat ne se renouvelle",
			"ogs apply")
	}
}

// firewallChecks porte les contrôles du pare-feu (D72). Une déclaration
// invalide est un échec : elle arrêterait le prochain apply.
func (a *App) firewallChecks(ctx context.Context, add func(name, status, detail, fix string)) {
	fw, err := a.firewall()
	if err != nil {
		add("pare-feu", "échec", strings.ReplaceAll(err.Error(), "\n", " "), "corriger "+a.Paths.UFWConfig())
		return
	}
	if !fw.Managed() {
		return
	}
	obs, err := fw.Observe(ctx)
	if err != nil {
		add("pare-feu", "avertissement", err.Error(), "")
		return
	}
	var listeners []string
	if !a.Exec.Simulated() {
		if l, err := a.listeners(); err == nil {
			listeners = l
		}
	}
	for _, f := range fw.Checks(obs, listeners) {
		add(f.Name, f.Status, f.Detail, f.Fix)
	}
}

// mailChecks porte les contrôles de la signature DKIM (D69) : ce que le plan ne
// voit pas — un opendkim.conf qui ne désigne pas les tables d'ogs, une clé trop
// ouverte, un enregistrement non publié ou différent.
func (a *App) mailChecks(ctx context.Context, projects []*model.Project, add func(name, status, detail, fix string)) {
	if a.Store == nil || a.Keys == nil {
		return
	}
	provs, err := a.buildProviders()
	if err != nil {
		return
	}
	defer provs.close()
	obs, err := provs.Mail.Observe(ctx)
	if err != nil {
		add("signature DKIM", "avertissement", err.Error(), "")
		return
	}
	for _, f := range provs.Mail.Checks(ctx, projects, obs) {
		add(f.Name, f.Status, f.Detail, f.Fix)
	}
}

// journalEtat est le mode d'un journal de la base d'état tel qu'il a été trouvé
// sur le disque, avant qu'une ouverture de la base ne puisse le changer.
type journalEtat struct {
	path string
	perm os.FileMode
}

// stateJournalModes relève les journaux de la base d'état — « state.db-wal » et
// « state.db-shm ». Par eux passent l'inventaire du parc et le journal d'audit, et
// SQLite calque leur mode sur celui de la base au moment où il les crée : une base
// héritée d'une version antérieure, ou un init interrompu avant le chmod, laisse
// derrière lui un journal en 0644. Il ne se voit nulle part — /var/lib/ogs est
// traversable à dessein (0751, pour le challenge ACME), donc lisible par le
// serveur web comme par les comptes des projets — et SQLite ne recrée pas un
// journal abandonné : il le reprend tel quel, mode compris.
func (a *App) stateJournalModes() []journalEtat {
	base := a.Paths.StateDB()
	var out []journalEtat
	for _, suffixe := range []string{"-wal", "-shm"} {
		path := base + suffixe
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		out = append(out, journalEtat{path: path, perm: info.Mode().Perm()})
	}
	return out
}

// addStateJournalChecks rend compte des journaux relevés, une fois la base
// ouverte. Un journal trop ouvert que store.Open vient de refermer n'est plus une
// anomalie du système : c'est une exposition passée, que la réparation n'annule
// pas mais que « chmod 0600 » ne corrige plus. Le dire en avertissement, sans
// remède, laisse le code de retour utilisable en supervision — là où un échec
// réclamerait une intervention devenue sans objet. Un journal encore ouvert au
// moment du compte rendu — base absente, donc jamais ouverte — reste un échec,
// avec le remède qui vaut.
func (a *App) addStateJournalChecks(add func(name, status, detail, fix string), journaux []journalEtat) {
	for _, j := range journaux {
		if j.perm&0o077 == 0 {
			add("journal de la base d'état", "ok",
				fmt.Sprintf("%s (mode %04o)", j.path, j.perm), "")
			continue
		}
		portee := ", alors que l'inventaire du parc et le journal d'audit y transitent"
		if info, err := os.Stat(j.path); err == nil && info.Mode().Perm()&0o077 == 0 {
			add("journal de la base d'état", "avertissement",
				fmt.Sprintf("%s était lisible au-delà de son propriétaire (mode %04o)%s ; "+
					"l'ouverture de la base l'a remis en %04o pendant ce diagnostic — "+
					"reste à savoir qui a pu le lire",
					j.path, j.perm, portee, info.Mode().Perm()), "")
			continue
		}
		add("journal de la base d'état", "échec",
			fmt.Sprintf("%s (mode %04o) est lisible au-delà de son propriétaire%s",
				j.path, j.perm, portee),
			"chmod 0600 "+j.path)
	}
}

// monitoringChecks porte ce que ni fail2ban ni Telegraf ne disent d'eux-mêmes.
//
// Le premier de ces contrôles est le plus important du lot : **une prison dont
// le journal n'existe plus empêche fail2ban de démarrer**, et il s'arrête alors
// entièrement — les prisons valides comprises. Le fichier reste syntaxiquement
// correct, le démon reste « enabled », et rien ne le dit jusqu'au prochain
// redémarrage de la machine. C'est le cas d'un projet dont les journaux ont été
// effacés à la main entre deux apply.
func (a *App) monitoringChecks(add func(name, status, detail, fix string)) {
	mon := a.Cfg.Monitoring

	// Fichier laissé par un outil depuis désactivé. L'apply le retire, mais un
	// parc qu'on n'a pas appliqué depuis garde une supervision que la
	// configuration dit éteinte : le fichier continue de s'appliquer — prisons
	// comprises — et n'est plus jamais recalculé, pas même à la suppression d'un
	// projet. Seul le refus explicite est examiné : sans valeur déclarée, c'est la
	// présence du répertoire de fragments qui tranche, et un fichier présent
	// suppose un répertoire présent.
	for _, outil := range []struct {
		nom     string
		decide  func() (bool, bool)
		fichier string
	}{
		{"entrées telegraf", mon.TelegrafEnabled, a.Paths.TelegrafFile()},
		{"prisons fail2ban", mon.Fail2banEnabled, a.Paths.Fail2banJailFile()},
	} {
		actif, déclaré := outil.decide()
		if !déclaré || actif {
			continue
		}
		if _, err := os.Stat(outil.fichier); err == nil {
			add("supervision désactivée", "avertissement",
				outil.fichier+" est encore en place alors que la configuration désactive cet "+
					"outil : il s'applique toujours, et plus rien ne le recalcule",
				"ogs apply (l'étape de retrait supprime le fichier)")
		}
	}

	// fail2ban : journaux référencés. Le contrôle n'a de sens que sur un système
	// réel — en mode simulé, le fichier retient tous les chemins par construction,
	// et rien n'est jamais donné à lire à fail2ban.
	jailFile := a.Paths.Fail2banJailFile()
	data, err := os.ReadFile(jailFile)
	switch {
	case a.Exec.Simulated():
	case err == nil:
		// Un même journal est surveillé par plusieurs prisons : dédupliquer, sinon
		// le diagnostic répète le même chemin autant de fois qu'il est référencé.
		var manquants []string
		vus := map[string]bool{}
		for _, path := range jailLogPaths(string(data)) {
			if vus[path] {
				continue
			}
			vus[path] = true
			if _, err := os.Stat(path); err != nil {
				manquants = append(manquants, path)
			}
		}
		if len(manquants) > 0 {
			add("prisons fail2ban", "échec",
				fmt.Sprintf("%d journal(aux) référencé(s) et absent(s) : %s — fail2ban refusera de "+
					"démarrer, et toutes les prisons tomberont avec celle-ci",
					len(manquants), strings.Join(manquants, ", ")),
				"ogs apply (le fichier est recalculé depuis les journaux réellement présents)")
		} else {
			add("prisons fail2ban", "ok", "les journaux référencés existent tous", "")
		}
	case !os.IsNotExist(err):
		add("prisons fail2ban", "avertissement", err.Error(), "")
	}

	// Prison de récidive : activée en configuration, elle n'est écrite que si le
	// journal de fail2ban existe (D68). Sinon elle est simplement omise, et rien
	// ne le dit — ni le fichier, qui reste syntaxiquement correct, ni
	// « fail2ban-client -t », qui n'a aucune prison à reprocher. La protection est
	// alors annoncée dans la configuration et absente de la machine.
	if err == nil && mon.JailEnabled("recidive") {
		if status, detail, fix := recidiveJailStatus(string(data), a.Paths.Fail2banLog()); status != "" {
			add("prison de récidive", status, detail, fix)
		}
	}

	// Le contrôle de configuration de fail2ban, lorsqu'il est installé. Il couvre
	// ce qu'aucune inspection de fichier ne voit : un filtre absent, une expression
	// régulière refusée, un backend indisponible — la prison sshd en backend
	// systemd sans les liaisons python3-systemd, par exemple. Chacun de ces cas
	// empêche le démon de démarrer, toutes prisons confondues.
	if a.Prefix == "" && !a.Exec.Simulated() {
		if _, err := osexec.LookPath("fail2ban-client"); err == nil {
			res, err := a.Exec.Run(context.Background(), exec.Command{
				Name: "fail2ban-client", Args: []string{"-t"}})
			if err != nil {
				detail := strings.TrimSpace(res.Stderr)
				if detail == "" {
					detail = strings.TrimSpace(res.Stdout)
				}
				add("configuration fail2ban", "échec",
					"fail2ban refuse sa configuration : "+firstLine(detail)+
						" — le démon ne démarrera pas, et toutes les prisons tomberont ensemble",
					"fail2ban-client -t pour le détail")
			} else {
				add("configuration fail2ban", "ok", "acceptée par fail2ban-client -t", "")
			}
		}
	}

	// Telegraf : le fragment porte les mots de passe des comptes de supervision.
	// Il doit être lisible par l'agent, et par lui seul.
	fragment := a.Paths.TelegrafFile()
	if info, err := os.Stat(fragment); err == nil {
		perm := info.Mode().Perm()
		switch {
		case perm&0o007 != 0:
			add("entrées telegraf", "échec",
				fmt.Sprintf("mode %04o : lisible par tous, alors que le fichier porte les mots de "+
					"passe des comptes de supervision", perm),
				"chmod 0640 "+fragment+" && chgrp "+mon.TelegrafUser()+" "+fragment)
		case !readableBy(info, mon.TelegrafUser()):
			// Un fragment que l'agent ne peut pas lire ne produit aucune erreur
			// visible : Telegraf démarre, ne collecte rien, et se tait.
			add("entrées telegraf", "avertissement",
				"le fragment n'est pas lisible par "+mon.TelegrafUser()+
					" : l'agent démarrera sans collecter ces entrées, sans le dire",
				"chgrp "+mon.TelegrafUser()+" "+fragment+" && chmod 0640 "+fragment)
		default:
			add("entrées telegraf", "ok", fmt.Sprintf("mode %04o", perm), "")
		}
	}

	// Comptes de supervision : leur mot de passe vit dans le keystore, et le
	// fragment en porte la copie que l'agent présente au moteur. Un secret que le
	// fragment référence sans qu'il soit scellé — base d'état restaurée sans ses
	// secrets — signifie que l'agent s'authentifie avec un mot de passe que le
	// moteur n'a peut-être jamais reçu ; indéchiffrable, c'est la clé maître qui
	// manque. Dans les deux cas rien ne le dit : un Telegraf qui n'arrive pas à
	// collecter démarre quand même et se tait (D65). Le contrôle part du fragment
	// et non de la configuration : une machine sans MySQL n'a pas d'entrée mysql,
	// donc pas de secret attendu, donc pas d'échec perpétuel.
	if fragmentData, err := os.ReadFile(fragment); err == nil {
		for _, name := range telegrafSecretNames(string(fragmentData)) {
			a.monitoringSecretCheck(add, name)
		}
	}
}

// recidiveJailStatus confronte la prison de récidive écrite au fichier à celle
// que la configuration demande. Le chemin du journal est un paramètre pour que le
// contrôle soit éprouvable sans toucher à /var/log.
//
// Un fichier sans aucune prison d'ogs ne dit rien de la récidive : elle lit le
// journal que les autres alimentent, et n'aurait rien à surveiller.
func recidiveJailStatus(content, logPath string) (status, detail, fix string) {
	if !strings.Contains(content, "[ogs-") {
		return "", "", ""
	}
	if strings.Contains(content, "[ogs-recidive]") {
		return "ok", "écrite, adossée à " + logPath, ""
	}
	if _, err := os.Stat(logPath); err != nil {
		return "avertissement",
			"la prison de récidive est activée en configuration mais absente du fichier : " +
				logPath + " n'existe pas, et une prison sans journal ne surveille rien — " +
				"la protection est annoncée, pas rendue",
			"vérifier « logtarget » dans la configuration de fail2ban — SYSLOG et STDOUT n'écrivent " +
				"aucun fichier —, démarrer le démon, puis « ogs apply »"
	}
	return "avertissement",
		"la prison de récidive est activée en configuration mais absente du fichier, alors que " +
			logPath + " existe : le fichier date d'avant l'activation",
		"ogs apply (le fichier de prisons est recalculé en entier)"
}

// monitoringSecretCheck éprouve un secret de parc référencé par le fragment.
func (a *App) monitoringSecretCheck(add func(name, status, detail, fix string), name string) {
	if a.Store == nil {
		add("comptes de supervision", "avertissement",
			"base d'état indisponible : le scellement de "+name+" n'a pas pu être vérifié",
			"ogs init")
		return
	}
	nonce, ciphertext, err := a.Store.GetSecret(monitoring.SecretScope, name)
	if err != nil {
		add("comptes de supervision", "échec",
			"aucun mot de passe scellé pour "+name+" : le fragment Telegraf en porte un que le "+
				"moteur n'a peut-être jamais reçu — l'agent échouera à chaque collecte sans le dire",
			"ogs apply")
		return
	}
	if a.Keys == nil {
		add("comptes de supervision", "avertissement",
			name+" est scellé, mais la clé maître est indisponible : le contenu n'a pas été vérifié",
			"vérifier "+a.Paths.MasterKey())
		return
	}
	if _, err := a.Keys.Open(monitoring.SecretScope, name, nonce, ciphertext); err != nil {
		add("comptes de supervision", "échec",
			name+" : déchiffrement refusé — base d'état restaurée sans la clé maître qui l'a scellée",
			"ogs apply (l'étape du compte rescelle le mot de passe et le repose sur le moteur)")
		return
	}
	add("comptes de supervision", "ok", name+" scellé et déchiffrable", "")
}

// telegrafSecretNames énumère les secrets de parc que le fragment met en jeu.
// Deux entrées seulement en ont un : le compte PostgreSQL est authentifié
// « peer » et n'a pas de mot de passe (D67), et les autres n'ont pas de compte.
// Le nom du compte est lu dans le fragment plutôt que dans la configuration :
// c'est le mot de passe **écrit** que l'agent présente, et c'est celui-là qu'on
// veut retrouver dans le keystore.
func telegrafSecretNames(content string) []string {
	var out []string
	section := ""
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "[[inputs."):
			section = strings.TrimSuffix(strings.TrimPrefix(line, "[[inputs."), "]]")
		case section == "mysql" && strings.HasPrefix(line, "servers"):
			// servers = ["<compte>:<mot de passe>@unix(<socket>)/"]
			if _, value, ok := strings.Cut(line, "="); ok {
				dsn := strings.Trim(strings.TrimSpace(value), `[]" `)
				if user, _, ok := strings.Cut(dsn, ":"); ok && user != "" {
					out = append(out, "mysql/"+user)
				}
			}
		case section == "rabbitmq" && strings.HasPrefix(line, "username"):
			if _, value, ok := strings.Cut(line, "="); ok {
				if user := strings.Trim(strings.TrimSpace(value), `"`); user != "" {
					out = append(out, "rabbitmq/"+user)
				}
			}
		}
	}
	return out
}

// envResetPolicy dit si la directive env_reset est en vigueur, en parcourant la
// politique sudo comme sudo la lit lui-même. Le chemin de la racine est un
// paramètre pour que le contrôle soit éprouvable sur une arborescence de test.
//
// Trois pièges, tous rencontrés par la version qui se contentait d'un
// strings.Contains sur /etc/sudoers. La politique ne tient pas dans ce seul
// fichier : Debian et Ubuntu le terminent par « @includedir /etc/sudoers.d », et
// c'est là qu'un administrateur pose ses Defaults — ogs y écrit lui-même ses
// règles de délégation. Dans un sudoers, « #includedir » n'est pas un
// commentaire mais la forme historique de la même directive : écarter toute
// ligne commençant par « # » supprimerait précisément l'inclusion qu'on veut
// suivre. Enfin c'est la dernière affectation qui l'emporte, un drop-in annulant
// ce que la racine a posé : on conclut sur l'état final, jamais sur la première
// occurrence — et sur une vraie ligne « Defaults », une sous-chaîne trouvée dans
// un commentaire n'étant pas une directive.
//
// Une source illisible ne vaut pas un « ok » : le contrôle dit alors qu'il n'a
// pas conclu. Affirmer « active » sans avoir tout lu est le défaut qu'on corrige.
func envResetPolicy(root string) (status, detail, fix string) {
	// env_reset est le défaut de sudo : sans directive contraire, elle est active.
	active, decided := true, ""
	var unread []string
	vus := map[string]bool{}

	var lire func(path string)
	lire = func(path string) {
		if vus[path] {
			return
		}
		vus[path] = true
		data, err := os.ReadFile(path)
		if err != nil {
			unread = append(unread, path)
			return
		}
		for _, line := range sudoersLines(string(data)) {
			if dir, ok := sudoersInclude(line, "includedir"); ok {
				entries, err := os.ReadDir(dir)
				if err != nil {
					unread = append(unread, dir)
					continue
				}
				var noms []string
				for _, e := range entries {
					// sudo ignore les entrées contenant un point ou terminées par « ~ » :
					// c'est ainsi qu'il écarte les .dpkg-dist et les sauvegardes d'éditeur.
					if e.IsDir() || strings.Contains(e.Name(), ".") || strings.HasSuffix(e.Name(), "~") {
						continue
					}
					noms = append(noms, e.Name())
				}
				// L'ordre des noms décide de la dernière valeur qui l'emporte.
				sort.Strings(noms)
				for _, n := range noms {
					lire(filepath.Join(dir, n))
				}
				continue
			}
			if f, ok := sudoersInclude(line, "include"); ok {
				lire(f)
				continue
			}
			if strings.HasPrefix(line, "#") {
				continue
			}
			if v, ok := envResetIn(line); ok {
				active, decided = v, path
			}
		}
	}
	lire(root)

	if !active {
		return "échec",
			"env_reset est désactivée par " + decided + " : la politique sudo n'est plus celle sous " +
				"laquelle la surface déléguée « ogs my » a été taillée (D28)",
			"retirer « !env_reset » de " + decided
	}
	if len(unread) > 0 {
		return "avertissement",
			"contrôle non concluant, source(s) illisible(s) : " + strings.Join(unread, ", "), ""
	}
	return "ok", "active", ""
}

// sudoersInclude reconnaît une directive d'inclusion et rend son chemin. Les
// deux préfixes sont acceptés : « @include » est la forme moderne, « #include »
// la forme historique, toujours valide et celle que livre Debian.
func sudoersInclude(line, keyword string) (string, bool) {
	if line == "" || (line[0] != '@' && line[0] != '#') {
		return "", false
	}
	rest, ok := strings.CutPrefix(line[1:], keyword)
	if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(rest), `"`), true
}

// envResetIn lit l'état d'env_reset posé par une ligne « Defaults », s'il y en a
// un. Les formes qualifiées — Defaults:utilisateur, Defaults@hôte,
// Defaults>runas, Defaults!commande — décident autant que la forme nue.
func envResetIn(line string) (value, found bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "Defaults") {
		return false, false
	}
	if q := fields[0][len("Defaults"):]; q != "" && !strings.ContainsRune(":@>!", rune(q[0])) {
		return false, false
	}
	// Les paramètres sont séparés par des virgules, avec des espaces libres.
	params := strings.FieldsFunc(strings.Join(fields[1:], " "), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	for _, p := range params {
		switch p {
		case "env_reset":
			value, found = true, true
		case "!env_reset":
			value, found = false, true
		}
	}
	return value, found
}

// sudoersLines rend les lignes utiles d'un sudoers, les continuations recollées :
// une liste de paramètres « Defaults » est souvent repliée sur plusieurs lignes
// par une barre oblique inverse, et la couper en deux ferait perdre la directive.
func sudoersLines(content string) []string {
	var out []string
	var pending string
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if pending != "" {
			line = pending + " " + line
			pending = ""
		}
		if strings.HasSuffix(line, `\`) {
			pending = strings.TrimSpace(strings.TrimSuffix(line, `\`))
			continue
		}
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	if pending != "" {
		out = append(out, pending)
	}
	return out
}

// jailLogPaths extrait les chemins de journaux d'un fichier de prisons. La
// directive « logpath » accepte plusieurs chemins, un par ligne, les suivants
// étant indentés — d'où la reprise des lignes qui ne portent pas de directive.
func jailLogPaths(content string) []string {
	var out []string
	inLogpath := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "logpath"):
			inLogpath = true
			if _, value, ok := strings.Cut(trimmed, "="); ok {
				if v := strings.TrimSpace(value); v != "" {
					out = append(out, v)
				}
			}
		case inLogpath && trimmed != "" && !strings.Contains(trimmed, "=") &&
			!strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "#"):
			out = append(out, trimmed)
		default:
			inLogpath = false
		}
	}
	return out
}

// readableBy indique si un fichier est lisible par un compte, en ne considérant
// que le groupe propriétaire : c'est le seul canal qu'ogs emploie pour l'agent.
func readableBy(info os.FileInfo, username string) bool {
	if info.Mode().Perm()&0o040 == 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	u, err := user.Lookup(username)
	if err != nil {
		// L'agent n'est pas installé : le contrôle n'a pas d'objet.
		return true
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return true
	}
	return uint32(gid) == stat.Gid
}

// firstLine borne un message d'erreur multiligne à sa première ligne utile : le
// tableau de « doctor » est fait pour être lu d'un coup d'œil.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return s
}
