package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/hook"
	"leblanc.io/open-go-server/internal/model"
	tlsprov "leblanc.io/open-go-server/internal/provider/tls"
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
			// Un repli — la demande complète refusée, le certificat émis pour les
			// seuls noms du vhost principal — a écrit un matériel neuf : il se
			// recharge comme une émission, et se signale comme un succès partiel.
			issueErr := provs.TLS.Issue(cmd.Context(), req)
			partial := (*tlsprov.PartialError)(nil)
			if issueErr != nil && !errors.As(issueErr, &partial) {
				_ = app.Audit.Record("", "tls/certificate/"+req.Name, "cert.issue", "échec", issueErr.Error())
				return wrap(CodeError, issueErr)
			}
			info, _, _ := provs.TLS.Store.Read(req.Name)
			if partial != nil {
				_ = app.Audit.Record("", "tls/certificate/"+req.Name, "cert.issue", "avertissement",
					issueErr.Error())
				app.printf("⚠ %v\n", issueErr)
				app.printf("✓ certificat %s émis pour %s, valable jusqu'au %s\n",
					req.Name, strings.Join(partial.Covered, ", "), info.NotAfter.Format("02/01/2006"))
			} else {
				_ = app.Audit.Record("", "tls/certificate/"+req.Name, "cert.issue", "ok",
					strings.Join(req.Domains, ","))
				app.printf("✓ certificat %s émis, valable jusqu'au %s\n",
					req.Name, info.NotAfter.Format("02/01/2006"))
			}
			// Un vhost déjà servi en HTTPS désigne ce certificat par son chemin : son
			// rendu ne change pas quand l'émission ajoute un nom, donc « ogs apply »
			// ne rechargerait rien, et le serveur présenterait indéfiniment le
			// matériel qu'il garde en mémoire — celui qui ne couvre pas l'alias.
			app.printf("  ↻ rechargement de %s\n", provs.Engine.Unit())
			if err := provs.Web.Reload(cmd.Context()); err != nil {
				// Code 6 : le certificat est émis, mais le serveur présente toujours
				// l'ancien — un succès partiel, non un succès.
				app.printf("⚠ rechargement du serveur web : %v\n", err)
				return exitf(CodePartial, "certificat %s émis, mais le serveur web ne l'a pas relu", req.Name)
			}
			app.printf("  activer le TLS sur le vhost :\n    ogs apply\n")
			if partial != nil {
				return exitf(CodePartial, "certificat %s émis sans %s",
					req.Name, strings.Join(partial.Missing, ", "))
			}
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
		Long: "Renouvelle les certificats expirant sous 30 jours, et ceux qui ne couvrent plus\n" +
			"les domaines demandés — l'émission d'un alias qui a échoué à l'apply est ainsi\n" +
			"reprise, une fois la déclaration appliquée. Un nom refusé par l'autorité ne\n" +
			"coûte ni le certificat du site ni les noms qui peuvent aboutir : l'émission se\n" +
			"replie sur eux, les redirections écartées les premières (code 6). Loin de\n" +
			"l'échéance, le repli n'a lieu que s'il ajoute un nom sans retirer aucun de ceux\n" +
			"que le certificat en place sert. Avec --force, renouvelle quoi qu'il en soit.\n" +
			"C'est la commande appelée par le timer ogs-renew.",
		RunE: func(cmd *cobra.Command, args []string) error {
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()

			var requests []tlsprov.Request
			var refusés []error
			deferred := 0
			if all || len(args) == 0 {
				requests = append(requests, provs.TLS.SharedRequests()...)
				projects, err := app.loadProjects()
				if err != nil {
					return err
				}
				for _, p := range projects {
					reqs, err := provs.TLS.Requests(p)
					if err != nil {
						// Un projet dont la demande est refusée n'est plus renouvelé : le
						// passer en silence laissait le timer rendre 0 pendant que son
						// certificat allait à l'expiration. Les autres projets n'en
						// dépendent pas, d'où un échec compté plutôt qu'un arrêt.
						refusés = append(refusés, fmt.Errorf("%s : certificat non examiné, déclaration refusée : %w",
							p.ID(), err))
						continue
					}
					requests = append(requests, app.sansDevancerLApply(provs, p, reqs, &deferred)...)
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
					requests = append(requests, app.sansDevancerLApply(provs, p, reqs, &deferred)...)
				}
			}
			if len(requests) == 0 && deferred == 0 && len(refusés) == 0 {
				app.printf("aucun certificat à examiner\n")
				return nil
			}
			done, errs := provs.TLS.Renew(cmd.Context(), requests, app.Force)
			errs = append(refusés, errs...)
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
				app.printf("= aucun certificat n'arrive à échéance (%d examiné(s))\n", len(requests)+deferred)
				return nil
			}
			// Le serveur web doit relire les nouveaux fichiers, dès qu'il y en a : un
			// échec ailleurs dans le parc — une redirection dont le DNS pointe
			// ailleurs, et qui échoue à chaque passage — ne doit pas priver les
			// certificats renouvelés de leur rechargement, sans quoi le serveur
			// présenterait l'ancien matériel jusqu'à son expiration.
			var reloadErr error
			if len(done) > 0 {
				app.printf("↻ rechargement de %s\n", provs.Engine.Unit())
				if reloadErr = provs.Web.Reload(cmd.Context()); reloadErr != nil {
					app.printf("⚠ rechargement du serveur web : %v\n", reloadErr)
				}
			}
			if len(errs) > 0 {
				// Code 6 : le timer systemd le tolère, un renouvellement en échec ne
				// doit pas masquer ceux qui ont abouti.
				return exitf(CodePartial, "%d renouvellement(s) en échec", len(errs))
			}
			if reloadErr != nil {
				return exitf(CodePartial, "certificats renouvelés, mais le serveur web ne les a pas relus")
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
			if provider != tlsprov.DNSProviderOVH {
				return exitf(CodeUsage, "provider %q non géré : ogs ne gère qu'%q",
					provider, tlsprov.DNSProviderOVH)
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

// étatDuCertificat qualifie, pour « status » et l'interface, un certificat qui
// n'est pas complet : « absent », « expiré » ou « incomplet », et le problème à
// signaler. Un certificat incomplet — une réémission en attente, ou repliée —
// reste servi pour les noms qu'il couvre : l'annoncer absent dirait le
// contraire de ce que voit le visiteur. Le message nomme ces noms, et eux
// seuls : quand le domaine principal change et que l'ancien devient une
// redirection, c'est elle qui est servie en HTTPS, non le site.
func étatDuCertificat(provs *providers, p *model.Project, ref string) (état, problème string) {
	info, ok, _ := provs.TLS.Store.Read(ref)
	switch {
	case !ok:
		return "absent", "certificat " + ref + " indisponible"
	case info.Expired(nowFunc()):
		return "expiré", fmt.Sprintf("certificat %s expiré depuis le %s", ref, info.NotAfter.Format("02/01/2006"))
	}
	cov := provs.TLS.Coverage(p)
	var servis, manquants []string
	for _, d := range cov.Requested {
		if cov.Serves(d) {
			servis = append(servis, d)
		} else {
			manquants = append(manquants, d)
		}
	}
	if len(servis) == 0 {
		return "absent", fmt.Sprintf("certificat %s : aucun des domaines demandés n'est couvert (%s)",
			ref, strings.Join(manquants, ", "))
	}
	return "incomplet", fmt.Sprintf("certificat %s : %s pas encore couvert(s), HTTPS servi pour %s "+
		"sur le certificat en place", ref, strings.Join(manquants, ", "), strings.Join(servis, ", "))
}

// sansDevancerLApply écarte d'un renouvellement non forcé les réémissions que seule
// la couverture motive — un certificat frais qui ne couvre pas toute sa demande —
// tant que la déclaration du projet n'a pas été appliquée telle quelle. Un alias
// ajouté au fichier mais pas encore appliqué n'est servi par aucun vhost : son
// challenge échouerait à coup sûr, et chaque passage du timer compterait un échec
// auprès de l'autorité, jusqu'à suspendre les tentatives — celles de l'apply qui
// suit comprises. L'apply s'en chargera. Une échéance, elle, n'attend pas.
func (a *App) sansDevancerLApply(provs *providers, p *model.Project, reqs []tlsprov.Request,
	deferred *int) []tlsprov.Request {
	if a.Force || a.applied(p) {
		return reqs
	}
	var out []tlsprov.Request
	for _, req := range reqs {
		if provs.TLS.AwaitsCoverage(req) {
			*deferred++
			a.printf("= %s : déclaration modifiée depuis le dernier apply, réémission laissée à « ogs apply %s »\n",
				req.Name, p.ID())
			continue
		}
		out = append(out, req)
	}
	return out
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
