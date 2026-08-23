package cli

import (
	"fmt"
	"os"
	"os/exec"
	"text/tabwriter"

	"github.com/spf13/cobra"

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
		if path, err := exec.LookPath(bin); err != nil {
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
