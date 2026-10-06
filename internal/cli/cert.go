package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/fs"
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
	dns.AddCommand(newCertDNSSetCommand(app), newCertDNSListCommand(app))
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
					// Une paire dépareillée n'est servie par aucun vhost (D110) :
					// l'avertissement qui suit la liste dit pourquoi et le remède.
					if c.Unpaired {
						issuer += " — CLÉ ET CERTIFICAT DÉPAREILLÉS"
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
			var issued []*model.Project
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
				issued = append(issued, p)
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
				// Un challenge imposé n'a plus de repli : l'exploitant a choisi.
				req.Challenge, req.AutoDNS = ch, false
				if ch != tlsprov.ChallengeDNS01 {
					req.DNSCredentials = ""
				} else if req.DNSProvider == "" && req.DNSCredentials == "" {
					req.DNSProvider = tlsprov.DNSProviderOVH
				}
			}
			if selfSign {
				req.Challenge, req.AutoDNS, req.DNSCredentials = tlsprov.ChallengeSelfSigned, false, ""
			}
			req.Staging = staging || app.Cfg.ACME.Staging
			if err := req.Validate(); err != nil {
				return exitf(CodeValidation, "%s", err)
			}
			if app.DryRun {
				app.printf("--dry-run : certificat %s pour %s (%s%s)\n",
					req.Name, strings.Join(req.Domains, ", "), req.Method(), stagingSuffix(req.Staging))
				return nil
			}
			app.printf("émission du certificat %s pour %s (%s%s)…\n",
				req.Name, strings.Join(req.Domains, ", "), req.Method(), stagingSuffix(req.Staging))
			// Un repli — la demande complète refusée, le certificat émis pour les
			// seuls noms du vhost principal — a écrit un matériel neuf : il se
			// recharge comme une émission, et se signale comme un succès partiel.
			issueErr := provs.TLS.Issue(cmd.Context(), req)
			partial := (*tlsprov.PartialError)(nil)
			fallback := (*tlsprov.DNSFallbackError)(nil)
			errors.As(issueErr, &fallback)
			if issueErr != nil && !errors.As(issueErr, &partial) && fallback == nil {
				_ = app.Audit.Record("", "tls/certificate/"+req.Name, "cert.issue", "échec", issueErr.Error())
				return wrap(CodeError, issueErr)
			}
			info, _, _ := provs.TLS.Store.Read(req.Name)
			if partial != nil {
				_ = app.Audit.Record("", "tls/certificate/"+req.Name, "cert.issue", "avertissement",
					issueErr.Error())
				app.warnf("⚠ %v\n", issueErr)
				app.printf("✓ certificat %s émis pour %s, valable jusqu'au %s\n",
					req.Name, strings.Join(partial.Covered, ", "), info.NotAfter.Format("02/01/2006"))
			} else if fallback != nil {
				_ = app.Audit.Record("", "tls/certificate/"+req.Name, "cert.issue", "avertissement",
					issueErr.Error())
				app.warnf("⚠ %v\n", issueErr)
				app.printf("✓ certificat %s émis, valable jusqu'au %s\n",
					req.Name, info.NotAfter.Format("02/01/2006"))
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
			reloadErr := provs.Web.Reload(cmd.Context())
			if reloadErr != nil {
				app.warnf("⚠ rechargement du serveur web : %v\n", reloadErr)
			}
			// Un certificat émis est un certificat changé : « postCertRenew » le
			// dit, une fois le serveur rechargé, comme après « cert renew » (D117).
			hookWarned := app.postCertRenewAfterReload(cmd.Context(), provs, reloadErr,
				[]string{req.Name}, issued)
			if reloadErr != nil {
				// Code 6 : le certificat est émis, mais le serveur présente toujours
				// l'ancien — un succès partiel, non un succès.
				return exitf(CodePartial, "certificat %s émis, mais le serveur web ne l'a pas relu", req.Name)
			}
			app.printf("  activer le TLS sur le vhost :\n    ogs apply\n")
			if partial != nil {
				return exitf(CodePartial, "certificat %s émis sans %s",
					req.Name, strings.Join(partial.Missing, ", "))
			}
			if fallback != nil {
				return exitf(CodePartial, "certificat %s émis par http-01 : le challenge dns-01 de sa zone a échoué",
					req.Name)
			}
			if hookWarned > 0 {
				return exitf(CodePartial, "certificat %s émis, mais le hook postCertRenew a échoué", req.Name)
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
			var examined []*model.Project
			deferred := 0
			if all || len(args) == 0 {
				requests = append(requests, provs.TLS.SharedRequests()...)
				projects, err := app.loadProjects()
				if err != nil {
					return err
				}
				examined = projects
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
					examined = append(examined, p)
					reqs, err := provs.TLS.Requests(p)
					if err != nil {
						return exitf(CodeValidation, "%s", err)
					}
					requests = append(requests, app.sansDevancerLApply(provs, p, reqs, &deferred)...)
					// Les partagés que désignent ses vhosts se renouvellent avec lui
					// (D102), comme « renew --all » les renouvelle.
					for _, r := range provs.TLS.DesignatedShared(p) {
						if !slices.ContainsFunc(requests, func(o tlsprov.Request) bool { return o.Name == r.Name }) {
							requests = append(requests, r)
						}
					}
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
			// Un certificat renouvelé en production malgré acme.staging (D110) est un
			// avertissement, non un échec : il compte dans le code 6, sous son nom.
			kept := 0
			for _, e := range errs {
				app.warnf("⚠ %v\n", e)
				var pk *tlsprov.ProductionKeptError
				if errors.As(e, &pk) {
					kept++
					_ = app.Audit.Record("", "tls/certificate/"+pk.Name, "cert.renew", "avertissement", e.Error())
					continue
				}
				_ = app.Audit.Record("", "tls/certificate", "cert.renew", "échec", e.Error())
			}
			// Le hook est déclenché même en cas d'échec partiel : les certificats
			// renouvelés le sont bel et bien, et une supervision doit l'apprendre.
			// Il l'est après le rechargement (D117), plus bas : lancé avant, il
			// annonçait un certificat que le serveur ne présentait pas encore.
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
					app.warnf("⚠ rechargement du serveur web : %v\n", reloadErr)
				}
			}
			hookWarned := 0
			if len(done) > 0 {
				hookWarned = app.postCertRenewAfterReload(cmd.Context(), provs, reloadErr, done, examined)
			}
			if len(errs) > 0 {
				// Code 6 : le timer systemd le tolère, un renouvellement en échec ne
				// doit pas masquer ceux qui ont abouti.
				if kept == len(errs) {
					return exitf(CodePartial, "%d certificat(s) de production renouvelé(s) en production malgré acme.staging", kept)
				}
				return exitf(CodePartial, "%d renouvellement(s) en échec", len(errs)-kept)
			}
			if reloadErr != nil {
				return exitf(CodePartial, "certificats renouvelés, mais le serveur web ne les a pas relus")
			}
			// Code 6, que le timer tolère : le hook constate, son échec ne défait
			// rien, mais il ne se tait pas (D117).
			if hookWarned > 0 {
				return exitf(CodePartial, "certificats renouvelés, mais le hook postCertRenew a échoué")
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
			// Une paire dépareillée se révoque comme une autre : la révocation porte
			// sur le certificat en place, signée par le compte ACME, non par sa clé
			// (D110). Elle est nommée, jamais dite absente.
			info, ok, paired, err := provs.TLS.Store.ReadPair(args[0])
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
			question := fmt.Sprintf("révoquer le certificat %s ?", args[0])
			if !paired {
				question = fmt.Sprintf("révoquer le certificat %s (%s, clé et certificat dépareillés : "+
					"le certificat en place est révoqué) ?", args[0], strings.Join(info.Domains, ", "))
			}
			if err := app.confirm(question); err != nil {
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
		Long: "Refuse de supprimer un certificat qu'une déclaration emploie ou qu'un vhost\n" +
			"écrit désigne encore — sites-available et sites-enabled du moteur, sites\n" +
			"hors d'ogs compris : le serveur web refuserait sa configuration au\n" +
			"rechargement suivant. Un certificat partagé se désigne par « @nom ». La\n" +
			"suppression est demandée en confirmation (--yes pour s'en passer).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			// Le nom devient un chemin que la suppression efface en entier : il est
			// contrôlé avant toute lecture (D110).
			if err := tlsprov.CheckRef(ref); err != nil {
				return exitf(CodeUsage, "%s", err)
			}
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			projects, err := app.loadProjects()
			if err != nil {
				return err
			}
			info, present, paired, err := provs.TLS.Store.ReadPair(ref)
			if err != nil {
				return wrap(CodeError, err)
			}
			if !present {
				return exitf(CodeUsage, "aucun certificat %q", ref)
			}
			if err := provs.TLS.DeleteRefusal(ref, projects); err != nil {
				return exitf(CodeConflict, "%s", err)
			}
			question := fmt.Sprintf("supprimer le certificat %s (%s, expire le %s) ?", ref,
				strings.Join(info.Domains, ", "), info.NotAfter.Format("02/01/2006"))
			if !paired {
				question = fmt.Sprintf("supprimer le certificat %s (%s, clé et certificat dépareillés) ?", ref,
					strings.Join(info.Domains, ", "))
			}
			if err := app.confirm(question); err != nil {
				return err
			}
			if err := provs.TLS.Store.Remove(ref); err != nil {
				return wrap(CodeError, err)
			}
			_ = app.Audit.Record("", "tls/certificate/"+ref, "cert.delete", "ok", "")
			app.printf("✓ certificat %s supprimé\n", ref)
			return nil
		},
	}
}

func newCertDNSSetCommand(app *App) *cobra.Command {
	var (
		provider          string
		zones             []string
		endpoint          string
		applicationKey    string
		applicationSecret string
		consumerKey       string
		clientID          string
		clientSecret      string
	)
	cmd := &cobra.Command{
		Use:   "set <nom>",
		Short: "Enregistrer un jeu d'identifiants DNS",
		Long: "Les identifiants sont écrits en 0600 sous /etc/ogs/credentials/dns/<nom>.yaml. Ils\n" +
			"donnent un droit d'écriture sur des zones entières : les traiter comme un secret\n" +
			"de premier rang. OVH accepte deux modes, clés applicatives (--application-key)\n" +
			"ou OAuth2 (--client-id).\n\n" +
			"Plusieurs jeux d'un même provider coexistent, un par compte ou par jeton : --zone\n" +
			"déclare les zones dont un jeu répond (répétable). Le challenge dns-01 de chaque\n" +
			"nom emploie le jeu de sa zone, la plus longue l'emportant, et un certificat dont\n" +
			"tous les noms relèvent d'une zone déclarée passe de lui-même au challenge dns-01,\n" +
			"avec repli sur http-01 (D97). Un jeu sans zone — l'ancien « ovh » — ne sert\n" +
			"qu'un certificat qui demande dns-01 ou le désigne par « dnsCredentials ». Sans\n" +
			"--zone, les zones d'un jeu existant sont conservées : renouveler une clé ne\n" +
			"doit pas faire repasser ses certificats en http-01.\n\n" +
			"Les secrets sont demandés sans écho sur un terminal, et lus sur l'entrée\n" +
			"standard sinon, une ligne chacun : le secret applicatif puis la clé de\n" +
			"consommateur, ou le secret client. Les options qui les portent, déconseillées,\n" +
			"les laissent dans le journal de sudo et l'historique du shell.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := tlsprov.CheckDNSCredentialsName(name); err != nil {
				return exitf(CodeUsage, "%s", err)
			}
			if provider != tlsprov.DNSProviderOVH {
				return exitf(CodeUsage, "provider %q non géré : ogs ne gère qu'%q",
					provider, tlsprov.DNSProviderOVH)
			}
			dir := app.Paths.DNSCredsDir()
			var declared []string
			for _, z := range zones {
				n, err := tlsprov.NormalizeZone(z)
				if err != nil {
					return exitf(CodeUsage, "%s", err)
				}
				if !slices.Contains(declared, n) {
					declared = append(declared, n)
				}
			}
			sets, _ := tlsprov.LoadDNSCredentials(dir)
			if len(zones) == 0 {
				if previous, err := tlsprov.ReadDNSCredentials(dir, name); err == nil {
					declared = previous.Zones
				}
			}
			// Deux jeux d'une même zone rendraient le choix indéterminé : le premier
			// renouvellement de chaque certificat de la zone échouerait.
			for _, s := range sets {
				if s.Name == name {
					continue
				}
				for _, z := range declared {
					if slices.Contains(s.Zones, z) {
						return exitf(CodeConflict, "la zone %s est déjà déclarée par le jeu %s", z, s.Name)
					}
				}
			}
			creds := dnsCredentialsFile{Provider: provider, Zones: declared, Endpoint: endpoint}
			saisie := app.saisieSecrets(cmd)
			var err error
			switch {
			case clientID != "":
				if clientSecret, err = saisie.valeur("client-secret", clientSecret, "secret client OAuth2"); err != nil {
					return err
				}
				creds.ClientID, creds.ClientSecret = clientID, clientSecret
			case applicationKey != "":
				applicationSecret, err = saisie.valeur("application-secret", applicationSecret, "secret applicatif")
				if err != nil {
					return err
				}
				if consumerKey, err = saisie.valeur("consumer-key", consumerKey, "clé de consommateur"); err != nil {
					return err
				}
				creds.ApplicationKey, creds.ApplicationSecret, creds.ConsumerKey =
					applicationKey, applicationSecret, consumerKey
			default:
				return exitf(CodeUsage, "fournir soit --client-id (OAuth2), soit --application-key "+
					"(clés applicatives) ; les secrets sont ensuite demandés")
			}
			data, err := yaml.Marshal(creds)
			if err != nil {
				return wrap(CodeError, err)
			}
			path := tlsprov.DNSCredentialsFile(dir, name)
			if err := fs.EnsureDir(dir, 0o700, fs.KeepOwner); err != nil {
				return wrap(CodeError, err)
			}
			header := []byte("# Identifiants DNS pour le challenge dns-01. Droit d'écriture sur la zone.\n")
			if err := fs.WriteAtomic(path, append(header, data...), 0o600, fs.KeepOwner); err != nil {
				return wrap(CodeError, err)
			}
			if err := app.openStore(); err == nil {
				_ = app.Audit.Record("", "tls/dns-provider/"+name, "cert.dns-provider.set", "ok",
					strings.Join(declared, ","))
			}
			app.printf("✓ identifiants %s (%s) enregistrés : %s (mode 0600)\n", name, provider, path)
			if len(declared) > 0 {
				app.printf("  zones : %s — leurs certificats passent au challenge dns-01, "+
					"repli http-01, au prochain apply ou renouvellement.\n", strings.Join(declared, ", "))
			} else {
				app.printf("  aucune zone : employés par un certificat qui demande dns-01 "+
					"sans zone déclarée, ou les désigne par « dnsCredentials: %s ».\n", name)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&provider, "provider", tlsprov.DNSProviderOVH, "provider DNS du jeu")
	f.StringSliceVar(&zones, "zone", nil, "zone dont le jeu répond (répétable)")
	f.StringVar(&endpoint, "endpoint", "ovh-eu", "point d'accès de l'API (ovh-eu, ovh-ca…)")
	f.StringVar(&applicationKey, "application-key", "", "clé applicative")
	f.StringVar(&applicationSecret, "application-secret", "",
		"secret applicatif (déconseillé : visible dans le journal de sudo ; l'omettre pour le saisir)")
	f.StringVar(&consumerKey, "consumer-key", "",
		"clé de consommateur (déconseillé : visible dans le journal de sudo ; l'omettre pour la saisir)")
	f.StringVar(&clientID, "client-id", "", "identifiant client OAuth2")
	f.StringVar(&clientSecret, "client-secret", "",
		"secret client OAuth2 (déconseillé : visible dans le journal de sudo ; l'omettre pour le saisir)")
	return cmd
}

// dnsCredentialsFile est le fichier d'un jeu d'identifiants DNS, tel qu'écrit.
type dnsCredentialsFile struct {
	Provider          string   `yaml:"provider"`
	Zones             []string `yaml:"zones,omitempty"`
	Endpoint          string   `yaml:"endpoint,omitempty"`
	ApplicationKey    string   `yaml:"applicationKey,omitempty"`
	ApplicationSecret string   `yaml:"applicationSecret,omitempty"`
	ConsumerKey       string   `yaml:"consumerKey,omitempty"`
	ClientID          string   `yaml:"clientId,omitempty"`
	ClientSecret      string   `yaml:"clientSecret,omitempty"`
}

// dnsCredentialsView est ce que « dns-provider list » montre d'un jeu : jamais
// ses identifiants.
type dnsCredentialsView struct {
	Name     string   `json:"name" yaml:"name"`
	Provider string   `json:"provider" yaml:"provider"`
	Zones    []string `json:"zones" yaml:"zones"`
	File     string   `json:"file" yaml:"file"`
}

func newCertDNSListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "Lister les jeux d'identifiants DNS et leurs zones",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := app.Paths.DNSCredsDir()
			sets, errs := tlsprov.LoadDNSCredentials(dir)
			views := make([]dnsCredentialsView, 0, len(sets))
			for _, s := range sets {
				views = append(views, dnsCredentialsView{Name: s.Name, Provider: s.Provider,
					Zones: orEmpty(s.Zones), File: tlsprov.DNSCredentialsFile(dir, s.Name)})
			}
			var warnings []string
			for _, e := range errs {
				warnings = append(warnings, e.Error())
			}
			warnings = append(warnings, tlsprov.DNSZoneConflicts(sets)...)
			err := app.emit("DNSCredentialsList", views, warnings, func(tw *tabwriter.Writer) {
				if len(views) == 0 {
					fmt.Fprintln(app.Out, "aucun jeu d'identifiants DNS")
				} else {
					fmt.Fprintln(tw, "NOM\tPROVIDER\tZONES")
					for _, v := range views {
						zones := strings.Join(v.Zones, " ")
						if zones == "" {
							zones = "(aucune : dns-01 demandé seulement)"
						}
						fmt.Fprintf(tw, "%s\t%s\t%s\n", v.Name, v.Provider, zones)
					}
				}
			})
			if err != nil {
				return wrap(CodeError, err)
			}
			if len(warnings) > 0 {
				return exitf(CodePartial, "%d jeu(x) d'identifiants DNS à corriger", len(warnings))
			}
			return nil
		},
	}
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
	if !ok {
		// Une paire dépareillée n'est servie par aucun vhost, mais elle n'est pas
		// absente : la nommer dit le remède (D110).
		if raw, present, _, _ := provs.TLS.Store.ReadPair(ref); present && raw.Unpaired {
			return "dépareillé", "certificat " + ref + " : clé et certificat dépareillés — « ogs cert renew " + ref + " »"
		}
	}
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
