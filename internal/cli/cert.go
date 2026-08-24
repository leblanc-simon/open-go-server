package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"github.com/leviathan/ogs/internal/fs"
	"github.com/leviathan/ogs/internal/hook"
	"github.com/leviathan/ogs/internal/model"
	tlsprov "github.com/leviathan/ogs/internal/provider/tls"
)

func newCertCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cert",
		Aliases: []string{"certificate"},
		Short:   "Gérer les certificats TLS",
	}
	dns := &cobra.Command{Use: "dns-provider", Short: "Gérer les identifiants DNS"}
	dns.AddCommand(newCertDNSSetCommand(app))
	cmd.AddCommand(
		newCertListCommand(app),
		newCertIssueCommand(app),
		newCertRenewCommand(app),
		newCertRevokeCommand(app),
		newCertDeleteCommand(app),
		dns,
	)
	return cmd
}

func newCertListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "Lister les certificats présents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			list, err := provs.TLS.Store.List()
			if err != nil {
				return wrap(CodeError, err)
			}
			warnings, err := provs.TLS.Warnings()
			if err != nil {
				return wrap(CodeError, err)
			}
			now := time.Now()
			return wrap(CodeError, app.emit("CertificateList", list, warnings, func(tw *tabwriter.Writer) {
				if len(list) == 0 {
					fmt.Fprintln(app.Out, "aucun certificat")
					return
				}
				fmt.Fprintln(tw, "NOM\tPORTÉE\tDOMAINES\tEXPIRE\tJOURS\tÉMETTEUR")
				for _, c := range list {
					scope := "projet"
					if c.Shared {
						scope = "serveur"
					}
					issuer := c.Issuer
					if c.SelfSign {
						issuer = "auto-signé"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
						c.Name, scope, strings.Join(c.Domains, " "),
						c.NotAfter.Format("02/01/2006"), c.DaysLeft(now), issuer)
				}
			}))
		},
	}
}

func newCertIssueCommand(app *App) *cobra.Command {
	var (
		challenge string
		staging   bool
		selfSign  bool
	)
	cmd := &cobra.Command{
		Use:   "issue <id|@partagé>",
		Short: "Émettre ou réémettre un certificat",
		Long: "Émet le certificat d'un projet, ou un certificat de portée serveur désigné par\n" +
			"@nom. Le vhost doit déjà répondre en HTTP pour qu'un challenge HTTP-01 aboutisse.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()

			var req tlsprov.Request
			if tlsprov.IsShared(args[0]) {
				r, ok := provs.TLS.SharedRequest(args[0])
				if !ok {
					return exitf(CodeUsage, "aucun certificat partagé %q déclaré dans %s",
						args[0], app.Paths.ConfigFile())
				}
				req = r
			} else {
				p, err := app.loadProject(args[0])
				if err != nil {
					return err
				}
				reqs, err := provs.TLS.Requests(p)
				if err != nil {
					return exitf(CodeValidation, "%s", err)
				}
				if len(reqs) == 0 {
					if ref := provs.TLS.SharedRef(p); ref != "" {
						return exitf(CodeUsage, "le projet %s s'appuie sur le certificat partagé %s : "+
							"l'émettre avec « ogs cert issue %s »", p.ID(), ref, ref)
					}
					return exitf(CodeUsage, "le projet %s ne demande aucun certificat", p.ID())
				}
				req = reqs[0]
			}
			if challenge != "" {
				ch, err := tlsprov.ParseChallenge(challenge)
				if err != nil {
					return exitf(CodeUsage, "%s", err)
				}
				req.Challenge = ch
			}
			if selfSign {
				req.Challenge = tlsprov.ChallengeSelfSigned
			}
			req.Staging = staging || app.Cfg.ACME.Staging
			if err := req.Validate(); err != nil {
				return exitf(CodeValidation, "%s", err)
			}
			if app.DryRun {
				app.printf("--dry-run : certificat %s pour %s (%s%s)\n",
					req.Name, strings.Join(req.Domains, ", "), req.Challenge, stagingSuffix(req.Staging))
				return nil
			}
			app.printf("émission du certificat %s pour %s (%s%s)…\n",
				req.Name, strings.Join(req.Domains, ", "), req.Challenge, stagingSuffix(req.Staging))
			if err := provs.TLS.Issue(cmd.Context(), req); err != nil {
				_ = app.Audit.Record("", "tls/certificate/"+req.Name, "cert.issue", "échec", err.Error())
				return wrap(CodeError, err)
			}
			_ = app.Audit.Record("", "tls/certificate/"+req.Name, "cert.issue", "ok",
				strings.Join(req.Domains, ","))
			info, _, _ := provs.TLS.Store.Read(req.Name)
			app.printf("✓ certificat %s émis, valable jusqu'au %s\n",
				req.Name, info.NotAfter.Format("02/01/2006"))
			app.printf("  activer le TLS sur le vhost :\n    ogs apply\n")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&challenge, "challenge", "", "forcer le challenge : http-01, dns-01 ou selfsigned")
	f.BoolVar(&staging, "staging", false, "utiliser l'environnement de test de l'autorité")
	f.BoolVar(&selfSign, "self-signed", false, "produire un certificat auto-signé (développement)")
	return cmd
}

func stagingSuffix(staging bool) string {
	if staging {
		return ", environnement de test"
	}
	return ""
}

func newCertRenewCommand(app *App) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "renew [id|@partagé...]",
		Short: "Renouveler les certificats proches de l'échéance",
		Long: "Renouvelle les certificats expirant sous 30 jours. Avec --force, renouvelle\n" +
			"quoi qu'il en soit. C'est la commande appelée par le timer ogs-renew.",
		RunE: func(cmd *cobra.Command, args []string) error {
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()

			var requests []tlsprov.Request
			if all || len(args) == 0 {
				requests = append(requests, provs.TLS.SharedRequests()...)
				projects, err := app.loadProjects()
				if err != nil {
					return err
				}
				for _, p := range projects {
					reqs, err := provs.TLS.Requests(p)
					if err != nil {
						continue
					}
					requests = append(requests, reqs...)
				}
			} else {
				for _, arg := range args {
					if tlsprov.IsShared(arg) {
						if r, ok := provs.TLS.SharedRequest(arg); ok {
							requests = append(requests, r)
						}
						continue
					}
					p, err := app.loadProject(arg)
					if err != nil {
						return err
					}
					reqs, err := provs.TLS.Requests(p)
					if err != nil {
						return exitf(CodeValidation, "%s", err)
					}
					requests = append(requests, reqs...)
				}
			}
			if len(requests) == 0 {
				app.printf("aucun certificat à examiner\n")
				return nil
			}
			done, errs := provs.TLS.Renew(cmd.Context(), requests, app.Force)
			for _, name := range done {
				app.printf("✓ %s renouvelé\n", name)
				_ = app.Audit.Record("", "tls/certificate/"+name, "cert.renew", "ok", "")
			}
			for _, e := range errs {
				app.printf("⚠ %v\n", e)
				_ = app.Audit.Record("", "tls/certificate", "cert.renew", "échec", e.Error())
			}
			// Le hook est déclenché même en cas d'échec partiel : les certificats
			// renouvelés le sont bel et bien, et une supervision doit l'apprendre.
			if len(done) > 0 {
				_, _ = app.runHook(cmd.Context(), provs.Hooks, nil, hook.PostCertRenew,
					hook.Context{Resources: done, Extra: map[string]string{"cert": strings.Join(done, ",")}})
			}
			if len(done) == 0 && len(errs) == 0 {
				app.printf("= aucun certificat n'arrive à échéance (%d examiné(s))\n", len(requests))
				return nil
			}
			if len(errs) > 0 {
				// Code 6 : le timer systemd le tolère, un renouvellement en échec ne
				// doit pas masquer ceux qui ont abouti.
				return exitf(CodePartial, "%d renouvellement(s) en échec", len(errs))
			}
			// Le serveur web doit relire les nouveaux fichiers.
			if err := provs.Web.Reload(cmd.Context()); err != nil {
				app.printf("⚠ rechargement du serveur web : %v\n", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "examiner tous les certificats du parc")
	return cmd
}

func newCertRevokeCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id|@partagé>",
		Short: "Révoquer un certificat auprès de l'autorité",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			info, ok, err := provs.TLS.Store.Read(args[0])
			if err != nil {
				return wrap(CodeError, err)
			}
			if !ok {
				return exitf(CodeUsage, "aucun certificat %q", args[0])
			}
			if info.SelfSign {
				return exitf(CodeUsage, "le certificat %s est auto-signé : il n'y a rien à révoquer "+
					"(le supprimer avec « ogs cert delete »)", args[0])
			}
			if err := app.confirm(fmt.Sprintf("révoquer le certificat %s ?", args[0])); err != nil {
				return err
			}
			cert, err := os.ReadFile(provs.TLS.Store.Path(args[0], tlsprov.FileFullchain))
			if err != nil {
				return wrap(CodeError, err)
			}
			req := tlsprov.Request{Name: args[0], Domains: info.Domains, Challenge: tlsprov.ChallengeHTTP01}
			if err := provs.TLS.Issuer.Revoke(cmd.Context(), req, cert); err != nil {
				return wrap(CodeError, err)
			}
			_ = app.Audit.Record("", "tls/certificate/"+args[0], "cert.revoke", "ok", "")
			app.printf("✓ certificat %s révoqué\n", args[0])
			return nil
		},
	}
}

func newCertDeleteCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id|@partagé>",
		Short: "Supprimer un certificat du disque",
		Long: "Refuse de supprimer un certificat encore référencé par un vhost : cela\n" +
			"empêcherait le serveur web de démarrer.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			projects, err := app.loadProjects()
			if err != nil {
				return err
			}
			if err := provs.TLS.Delete(args[0], projects); err != nil {
				return exitf(CodeConflict, "%s", err)
			}
			_ = app.Audit.Record("", "tls/certificate/"+args[0], "cert.delete", "ok", "")
			app.printf("✓ certificat %s supprimé\n", args[0])
			return nil
		},
	}
}

func newCertDNSSetCommand(app *App) *cobra.Command {
	var (
		endpoint          string
		applicationKey    string
		applicationSecret string
		consumerKey       string
		clientID          string
		clientSecret      string
	)
	cmd := &cobra.Command{
		Use:   "set <provider>",
		Short: "Enregistrer les identifiants d'un provider DNS",
		Long: "Les identifiants sont écrits en 0600 sous /etc/ogs/credentials/dns/. Ils\n" +
			"donnent un droit d'écriture sur la zone entière : les traiter comme un secret\n" +
			"de premier rang. OVH accepte deux modes, clés applicatives ou OAuth2.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := args[0]
			if provider != "ovh" {
				return exitf(CodeUsage, "provider %q non géré en v1 (seul « ovh » est testé)", provider)
			}
			creds := map[string]string{}
			if endpoint != "" {
				creds["endpoint"] = endpoint
			}
			switch {
			case clientID != "":
				if clientSecret == "" {
					return exitf(CodeUsage, "--client-secret est requis avec --client-id")
				}
				creds["clientId"] = clientID
				creds["clientSecret"] = clientSecret
			case applicationKey != "":
				if applicationSecret == "" || consumerKey == "" {
					return exitf(CodeUsage,
						"--application-secret et --consumer-key sont requis avec --application-key")
				}
				creds["applicationKey"] = applicationKey
				creds["applicationSecret"] = applicationSecret
				creds["consumerKey"] = consumerKey
			default:
				return exitf(CodeUsage, "fournir soit --client-id et --client-secret, "+
					"soit --application-key, --application-secret et --consumer-key")
			}
			data, err := yaml.Marshal(creds)
			if err != nil {
				return wrap(CodeError, err)
			}
			path := filepath.Join(app.Paths.DNSCredsDir(), provider+".yaml")
			if err := fs.EnsureDir(filepath.Dir(path), 0o700, fs.KeepOwner); err != nil {
				return wrap(CodeError, err)
			}
			header := []byte("# Identifiants DNS pour le challenge dns-01. Droit d'écriture sur la zone.\n")
			if err := fs.WriteAtomic(path, append(header, data...), 0o600, fs.KeepOwner); err != nil {
				return wrap(CodeError, err)
			}
			if err := app.openStore(); err == nil {
				_ = app.Audit.Record("", "tls/dns-provider/"+provider, "cert.dns-provider.set", "ok", "")
			}
			app.printf("✓ identifiants %s enregistrés : %s (mode 0600)\n", provider, path)
			app.printf("  ils seront employés pour les challenges dns-01, donc pour les wildcards.\n")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&endpoint, "endpoint", "ovh-eu", "point d'accès de l'API (ovh-eu, ovh-ca…)")
	f.StringVar(&applicationKey, "application-key", "", "clé applicative")
	f.StringVar(&applicationSecret, "application-secret", "", "secret applicatif")
	f.StringVar(&consumerKey, "consumer-key", "", "clé de consommateur")
	f.StringVar(&clientID, "client-id", "", "identifiant client OAuth2")
	f.StringVar(&clientSecret, "client-secret", "", "secret client OAuth2")
	return cmd
}

// certProviders ouvre la base d'état puis construit les providers : les
// commandes de certificats ont besoin du suivi des échecs.
func (a *App) certProviders() (*providers, error) {
	if err := a.openStore(); err != nil {
		return nil, err
	}
	return a.buildProviders()
}

// certificateDomains est utilisé par doctor pour signaler les échéances.
func certificateDomains(p *model.Project) []string {
	out := make([]string, 0, len(p.Spec.Domains))
	for _, d := range p.Spec.Domains {
		out = append(out, d.Name)
	}
	return out
}
