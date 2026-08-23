package cli

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/leviathan/ogs/internal/exec"
	appprov "github.com/leviathan/ogs/internal/provider/app"
	"github.com/leviathan/ogs/internal/validate"
)

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
			checks := app.runChecks()
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

func (a *App) runChecks() []check {
	var out []check
	add := func(name, status, detail, fix string) {
		out = append(out, check{Name: name, Status: status, Detail: detail, Fix: fix})
	}

	if a.Prefix == "" && os.Getuid() != 0 {
		add("privilèges", "échec", "ogs n'est pas exécuté en tant que root", "utiliser sudo")
	} else {
		add("privilèges", "ok", fmt.Sprintf("uid %d", os.Getuid()), "")
	}

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
	// web ne peut atteindre aucun docroot.
	homeRoot := a.homeRoot()
	if info, err := os.Stat(homeRoot); err != nil {
		add("racine des homes", "échec", err.Error(), "")
	} else if info.Mode().Perm()&0o001 == 0 {
		add("racine des homes", "échec",
			fmt.Sprintf("%s (mode %04o) n'est pas traversable : aucun docroot ne sera servi", homeRoot, info.Mode().Perm()),
			"chmod 0755 "+homeRoot)
	} else {
		add("racine des homes", "ok", homeRoot, "")
	}

	// Binaires attendus par le provider système.
	for _, bin := range []string{"useradd", "groupadd", "gpasswd", "usermod"} {
		if path, err := osexec.LookPath(bin); err != nil {
			add("binaire "+bin, "échec", "introuvable dans le PATH", "apt install passwd")
		} else {
			add("binaire "+bin, "ok", path, "")
		}
	}

	// Versions PHP en fin de support : signalées, jamais bloquantes.
	projects, err := a.loadProjects()
	if err != nil {
		add("projets", "avertissement", err.Error(), "")
		return out
	}
	add("projets", "ok", fmt.Sprintf("%d déclaré(s)", len(projects)), "")

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
		provs.close()
	}
	// « ogs my » identifie l'appelant par SUDO_UID, que sudo ne positionne de
	// façon fiable que si env_reset est active.
	if a.Prefix == "" {
		if data, err := os.ReadFile("/etc/sudoers"); err == nil {
			if strings.Contains(string(data), "!env_reset") {
				add("sudo env_reset", "échec",
					"env_reset est désactivée : l'appelant contrôle son environnement et « ogs my » "+
						"ne peut plus l'identifier de façon sûre",
					"réactiver Defaults env_reset dans /etc/sudoers")
			} else {
				add("sudo env_reset", "ok", "active", "")
			}
		}
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
		}
	}
	for _, p := range projects {
		for _, pool := range p.Spec.PHP.Pools {
			if date, eol := validate.EOLDate(pool.Version); eol {
				add(fmt.Sprintf("php %s (%s)", pool.Version, p.ID()), "avertissement",
					"support terminé depuis le "+date, "envisager « ogs php switch »")
			}
		}
	}
	return out
}
