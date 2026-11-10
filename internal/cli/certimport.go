package cli

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	tlsprov "leblanc.io/open-go-server/internal/provider/tls"
)

// importRoots désigne les autorités contre lesquelles la chaîne d'un certificat
// importé est vérifiée : nil pour celles du système. Les essais y posent leur
// propre autorité.
var importRoots *x509.CertPool

// certImport est un import préparé : paire lue et contrôlée, demande choisie.
type certImport struct {
	req     tlsprov.Request
	imp     tlsprov.Imported
	missing []string
}

// warnings rend tout ce que l'import doit dire sans le refuser.
func (c certImport) warnings() []string {
	out := append([]string{}, c.imp.Warnings...)
	if len(c.missing) > 0 {
		out = append(out, fmt.Sprintf("le certificat ne couvre pas %s : le plan tentera de l'obtenir par une "+
			"émission, qui échoue tant que le DNS désigne l'autre serveur — le certificat importé reste servi "+
			"pour les autres noms", strings.Join(c.missing, ", ")))
	}
	return out
}

// prepareCertImport lit et contrôle la paire désignée, puis choisit la demande à
// laquelle la rattacher (D130) : pour le projet proj, ou pour le certificat
// partagé ref quand proj est nil. Rien n'est écrit.
func (a *App) prepareCertImport(provs *providers, proj *model.Project, ref, certPath, keyPath, name string) (certImport, error) {
	if certPath == "" || keyPath == "" {
		return certImport{}, exitf(CodeUsage, "--cert et --key sont requis ensemble")
	}
	chain, err := fs.ReadDesignated(certPath, tlsprov.MaxImportSize)
	if err != nil {
		return certImport{}, exitf(CodeUsage, "--cert : %v", err)
	}
	key, err := fs.ReadDesignated(keyPath, tlsprov.MaxImportSize)
	if err != nil {
		return certImport{}, exitf(CodeUsage, "--key : %v", err)
	}
	imp, err := tlsprov.ParseImport(chain, key, time.Now(), importRoots)
	if err != nil {
		return certImport{}, exitf(CodeValidation, "%s : %v", certPath, err)
	}
	var (
		req     tlsprov.Request
		missing []string
	)
	if proj == nil {
		if name != "" {
			return certImport{}, exitf(CodeUsage, "--name ne s'emploie qu'avec un projet")
		}
		req, missing, err = provs.TLS.SharedImportTarget(ref, imp.Info)
	} else {
		req, missing, err = provs.TLS.ImportTarget(proj, name, imp.Info)
	}
	if err != nil {
		return certImport{}, exitf(CodeValidation, "%s", err)
	}
	return certImport{req: req, imp: imp, missing: missing}, nil
}

// describe affiche le certificat importé et ses avertissements.
func (a *App) describeCertImport(c certImport) {
	info := c.imp.Info
	a.printf("certificat %s : %s, émis par %s, expire le %s (%d jours)\n", c.req.Name,
		strings.Join(info.Domains, ", "), info.Issuer, info.NotAfter.Format("02/01/2006"), info.DaysLeft(time.Now()))
	for _, w := range c.warnings() {
		a.warnf("⚠ %s\n", w)
	}
}

func newCertImportCommand(app *App) *cobra.Command {
	var certPath, keyPath, name string
	cmd := &cobra.Command{
		Use:   "import <id|@partagé> --cert <fullchain.pem> --key <privkey.pem>",
		Short: "Importer un certificat émis ailleurs",
		Long: "Pose dans le magasin d'ogs un certificat émis ailleurs — typiquement celui que\n" +
			"certbot tient sur le serveur d'où un site migre (fullchain.pem et privkey.pem de\n" +
			"/etc/letsencrypt/live/<domaine>) —, pour servir le site en HTTPS avant que le DNS\n" +
			"ne désigne cette machine. ogs le traite ensuite comme les siens : il le renouvelle\n" +
			"par son propre compte ACME trente jours avant l'échéance, ce qui suppose que le\n" +
			"DNS ait basculé d'ici là.\n\n" +
			"La clé doit correspondre au certificat, en clair ; le certificat doit être en\n" +
			"cours de validité, d'une autorité reconnue, et couvrir au moins un nom du projet.\n" +
			"Un certificat déjà présent n'est remplacé que sous --force, après sauvegarde dans\n" +
			"/var/lib/ogs/backups. Rien n'est appliqué : « ogs apply <id> » passe ensuite le\n" +
			"vhost en HTTPS ; un vhost qui sert déjà ce certificat est rechargé aussitôt.\n\n" +
			"Pour un site à créer, « ogs site create … --cert … --key … » importe avant le\n" +
			"premier apply, qui n'a alors rien à émettre.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			var proj *model.Project
			if !tlsprov.IsShared(ref) {
				if proj, err = app.loadProject(ref); err != nil {
					return err
				}
			}
			c, err := app.prepareCertImport(provs, proj, ref, certPath, keyPath, name)
			if err != nil {
				return err
			}
			app.describeCertImport(c)
			if app.DryRun {
				app.printf("--dry-run : le certificat serait écrit dans %s ; rien n'a été écrit\n", provs.TLS.Store.Dir(c.req.Name))
				return nil
			}
			// Le verrou global écarte un apply qui émettrait le même certificat entre
			// la lecture de l'existant et l'écriture ; « cert renew », qui ne le prend
			// pas (D110), est tenu par le verrou du certificat.
			lock, err := engine.Acquire(app.Paths.GlobalLock())
			if err != nil {
				var locked engine.ErrLocked
				if errors.As(err, &locked) {
					return wrap(CodeLocked, err)
				}
				return wrap(CodeError, err)
			}
			defer lock.Release()
			return app.writeCertImport(cmd.Context(), provs, proj, c, app.Force, true)
		},
	}
	f := cmd.Flags()
	f.StringVar(&certPath, "cert", "", "chaîne complète du certificat, PEM (fullchain.pem)")
	f.StringVar(&keyPath, "key", "", "clé privée, PEM et en clair (privkey.pem)")
	f.StringVar(&name, "name", "", "certificat du projet à remplacer, s'il en déclare plusieurs")
	return cmd
}

// writeCertImport écrit le certificat préparé, recharge le serveur web si un
// vhost le sert déjà, et rend le code de l'import. hint fait nommer l'apply qui
// passera le vhost en HTTPS — « site create » l'enchaîne lui-même.
func (a *App) writeCertImport(ctx context.Context, provs *providers, proj *model.Project, c certImport,
	replace, hint bool) error {
	resource := "tls/certificate/" + c.req.Name
	id := ""
	if proj != nil {
		id = proj.ID()
	}
	backups, replaced, err := provs.TLS.Import(ctx, c.req.Name, c.imp, replace)
	for _, b := range backups {
		a.printf("  sauvegarde : %s\n", b)
	}
	if err != nil {
		_ = a.Audit.Record(id, resource, "cert.import", "échec", err.Error())
		if errors.Is(err, tlsprov.ErrImportExists) {
			return exitf(CodeConflict, "%s", err)
		}
		return wrap(CodeError, err)
	}
	warnings := c.warnings()
	result := "ok"
	if len(warnings) > 0 {
		result = "avertissement"
	}
	_ = a.Audit.Record(id, resource, "cert.import", result, strings.Join(c.imp.Info.Domains, ","))
	verb := "importé"
	if replaced {
		verb = "remplacé"
	}
	a.printf("✓ certificat %s %s dans %s\n", c.req.Name, verb, provs.TLS.Store.Dir(c.req.Name))
	if len(provs.TLS.Served(c.req.Name)) > 0 {
		// Un vhost désigne déjà ce répertoire : son rendu ne change pas, « ogs
		// apply » ne rechargerait rien, et le serveur présenterait l'ancien
		// matériel qu'il garde en mémoire.
		a.printf("  ↻ rechargement de %s\n", provs.Engine.Unit())
		reloadErr := provs.Web.Reload(ctx)
		if reloadErr != nil {
			a.warnf("⚠ rechargement du serveur web : %v\n", reloadErr)
		}
		var known []*model.Project
		if proj != nil {
			known = []*model.Project{proj}
		}
		hookWarned := a.postCertRenewAfterReload(ctx, provs, reloadErr, []string{c.req.Name}, known)
		if reloadErr != nil {
			return exitf(CodePartial, "certificat %s %s, mais le serveur web ne l'a pas relu", c.req.Name, verb)
		}
		if hookWarned > 0 {
			return exitf(CodePartial, "certificat %s %s, mais le hook postCertRenew a échoué", c.req.Name, verb)
		}
	} else if hint && proj != nil {
		a.printf("  servir le site en HTTPS :\n    ogs apply %s\n", proj.ID())
	} else if hint {
		a.printf("  servir les préproductions en HTTPS :\n    ogs apply\n")
	}
	if len(warnings) > 0 {
		return exitf(CodePartial, "certificat %s %s avec %d avertissement(s)", c.req.Name, verb, len(warnings))
	}
	return nil
}
