package mail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/removal"
)

// Entry est une ligne des tables : une clé active et le domaine qu'elle signe.
type Entry struct {
	Project  string
	Domain   string
	Selector string
	KeyFile  string
}

// Entries énumère les clés actives du parc, dans l'ordre des projets puis des
// domaines. Seules y figurent les clés dont le fichier existe : une table qui
// désigne une clé absente ne laisse pas partir le courrier sans signature —
// OpenDKIM rend une erreur temporaire, et le MTA garde en file tout le courrier
// du domaine, là où une absence de ligne l'aurait laissé partir.
func (p *Provider) Entries(projects []*model.Project) ([]Entry, error) {
	keysOf := map[string][]Key{}
	for _, proj := range projects {
		if len(proj.DKIMDomains()) == 0 || proj.IsPrep() {
			continue
		}
		keys, err := p.loadKeys(proj.ID())
		if err != nil {
			return nil, err
		}
		keysOf[proj.ID()] = keys
	}
	return p.entries(projects, keysOf), nil
}

func (p *Provider) entries(projects []*model.Project, keysOf map[string][]Key) []Entry {
	var out []Entry
	for _, proj := range projects {
		domains := proj.DKIMDomains()
		if len(domains) == 0 || proj.IsPrep() {
			continue
		}
		grouped := byDomain(keysOf[proj.ID()])
		for _, d := range domains {
			k, ok := find(grouped[d], StateActive)
			if !ok {
				// Aucune clé scellée : la ligne en place reste, si elle désigne un
				// fichier du projet (keptEntry, D108).
				if e, kept := p.keptEntry(proj.ID(), d); kept {
					out = append(out, e)
				}
				continue
			}
			// Le fichier est nommé par le domaine sous lequel la clé a été scellée,
			// la ligne par sa forme canonique (D108).
			path := p.Paths.OpenDKIMKeyFile(proj.ID(), k.Domain, k.Selector)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			out = append(out, Entry{Project: proj.ID(), Domain: d, Selector: k.Selector, KeyFile: path})
		}
	}
	return out
}

// renderKeyTable et renderSigningTable produisent le corps des deux tables.
//
// La SigningTable est lue en expressions (« refile: ») : c'est ce qui permet
// « *@domaine », qu'une table littérale ne saurait pas reconnaître — elle ne
// signerait alors rien, sans le moindre message.
func renderKeyTable(entries []Entry) []byte {
	var b strings.Builder
	b.WriteString("# Clés actives du parc : <nom> <domaine>:<sélecteur>:<clé privée>.\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "%s._domainkey.%s %s:%s:%s\n", e.Selector, e.Domain, e.Domain, e.Selector, e.KeyFile)
	}
	return []byte(b.String())
}

func renderSigningTable(entries []Entry) []byte {
	var b strings.Builder
	b.WriteString("# Domaines signés : lue en expressions (refile:), d'où « *@domaine ».\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "*@%s %s._domainkey.%s\n", e.Domain, e.Selector, e.Domain)
	}
	return []byte(b.String())
}

// PlanTables calcule les deux tables à partir du parc entier. Elles sont
// recalculées **en entier**, jamais depuis les projets visés : OpenDKIM n'en
// accepte qu'une de chaque, et recalculées depuis un sous-ensemble elles
// retireraient de la signature tous les autres projets.
//
// Viennent ensuite les retraits de fichiers de clés — domaines qui ne sont plus
// signés, clés remplacées dont la rétention est écoulée —, chacun soumis à ce que
// les tables en place ne désignent plus la clé et qu'OpenDKIM les ait relues
// (releasable, D108).
//
// OpenDKIM absent, les tables d'un apply précédent sont retirées : elles
// désigneraient des clés que plus rien ne réconcilie. Mais si opendkim.conf les
// désigne encore — signature désactivée par mail.dkim.enabled: false —, elles
// sont vidées et non retirées : OpenDKIM ne démarrerait plus sur une table
// introuvable, et une table vide ne désigne aucune clé, donc ne retient aucun
// courrier (D108). « doctor » le signale.
func (p *Provider) PlanTables(projects []*model.Project, obs Observed) ([]*engine.Step, error) {
	if !obs.Present {
		if p.ConfDesignatesTables() {
			return p.tableSteps(projects, nil, obs,
				" — signature désactivée, vidée : "+p.Paths.OpenDKIMConf()+" la désigne encore")
		}
		var steps []*engine.Step
		for _, t := range []struct{ resource, path string }{
			{"mail/dkim-keytable", p.Paths.OpenDKIMKeyTable()},
			{"mail/dkim-signingtable", p.Paths.OpenDKIMSigningTable()},
		} {
			s, err := p.removeStep(t.resource, t.path)
			if err != nil {
				return nil, err
			}
			if s != nil {
				steps = append(steps, s)
			}
		}
		return steps, nil
	}
	// Les clés du parc, lues une fois. Celles d'un projet qui ne s'ouvrent pas
	// — keystore indisponible, clé maître régénérée — ne font plus échouer le
	// plan global, donc tout apply (D108) : une étape en échec nomme la cause. Si
	// le projet signe, les tables restent en l'état — recalculées sans ses clés,
	// elles cesseraient de le signer —, et rien n'est retiré.
	keysOf := map[string][]Key{}
	var unreadable []*engine.Step
	frozen := false
	for _, proj := range projects {
		keys, err := p.loadKeys(proj.ID())
		if err != nil {
			signs := len(proj.DKIMDomains()) > 0 && !proj.IsPrep()
			consequence := "ses fichiers de clés ne sont pas retirés"
			if signs {
				consequence = "tables d'OpenDKIM laissées en l'état"
				frozen = true
			}
			unreadable = append(unreadable,
				p.unreadableStep("mail/dkim-keys/"+proj.ID(), proj.ID(), consequence, err))
			continue
		}
		keysOf[proj.ID()] = keys
	}
	if frozen {
		return unreadable, nil
	}
	// Aucun projet ne demande la signature et ogs n'a jamais écrit de tables : il
	// n'a rien à faire ici. OpenDKIM peut servir à l'administrateur par ailleurs,
	// et des tables vides posées d'office feraient recharger — donc valider — sa
	// configuration à chaque apply, pour rien.
	if !anyDKIM(projects) && !exists(p.Paths.OpenDKIMKeyTable()) && !exists(p.Paths.OpenDKIMSigningTable()) {
		return unreadable, nil
	}
	steps, err := p.tableSteps(projects, p.entries(projects, keysOf), obs, "")
	if err != nil {
		return nil, err
	}
	steps = append(steps, unreadable...)
	// Les fichiers des domaines qui ne sont plus signés, puis les clés remplacées
	// expirées, ferment la marche : après les tables qui cessent de les désigner.
	for _, proj := range projects {
		keys, ok := keysOf[proj.ID()]
		if !ok {
			continue
		}
		orphans, err := p.orphanSteps(proj, keys)
		if err != nil {
			return nil, err
		}
		steps = append(steps, orphans...)
		expiries, err := p.expirySteps(proj, keys)
		if err != nil {
			return nil, err
		}
		steps = append(steps, expiries...)
	}
	return steps, nil
}

// tableSteps rend les deux étapes d'écriture des tables pour ces lignes.
//
// Les deux tables s'écrivent ensemble ou pas du tout (D108) : la SigningTable
// désigne les noms de la KeyTable, et une table retouchée refusée sans --force
// pendant que l'autre était réécrite laissait la SigningTable désigner un nom
// que la KeyTable ne porte plus — OpenDKIM rend alors une erreur temporaire, et
// le MTA garde en file tout le courrier du domaine. Si l'une est refusée,
// l'autre reste en l'état, et son étape le dit.
func (p *Provider) tableSteps(projects []*model.Project, entries []Entry, obs Observed, note string) ([]*engine.Step, error) {
	type table struct {
		resource, summary, path string
		content                 []byte
		state                   fs.State
	}
	tables := []*table{
		{resource: "mail/dkim-keytable", summary: fmt.Sprintf("KeyTable d'OpenDKIM (%d clé(s))", len(entries)),
			path: p.Paths.OpenDKIMKeyTable(), content: renderKeyTable(entries)},
		{resource: "mail/dkim-signingtable", summary: fmt.Sprintf("SigningTable d'OpenDKIM (%d domaine(s))", len(entries)),
			path: p.Paths.OpenDKIMSigningTable(), content: renderSigningTable(entries)},
	}
	refused := ""
	for _, t := range tables {
		header := fs.Header{
			Project:   fmt.Sprintf("%d projet(s)", len(projects)),
			Resource:  t.resource,
			Generated: p.now(),
		}
		t.content = fs.Render(header, t.content, "#")
		state, err := fs.Inspect(t.path, t.content)
		if err != nil {
			return nil, err
		}
		t.state = state
		if !p.Force && (state == fs.StateHandEdited || state == fs.StateForeign) {
			refused = t.path
		}
	}
	var steps []*engine.Step
	for _, t := range tables {
		if refused != "" && t.path != refused && t.state != fs.StateCurrent {
			s := engine.NewStep(t.resource, engine.Update, t.summary+note+" — laissée en l'état")
			s.Critical = false
			other := refused
			s.Apply = func(context.Context) error {
				return fmt.Errorf("%s n'est pas réécrite : %s est refusée, et les deux tables ne s'écrivent "+
					"qu'ensemble — l'une désignerait un nom que l'autre ne porte plus, et OpenDKIM "+
					"retiendrait le courrier du domaine", t.path, other)
			}
			steps = append(steps, s)
			continue
		}
		s, err := p.tableStep(t.resource, t.summary+note, t.path, t.content, obs)
		if err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	return steps, nil
}

// orphanSteps retire les fichiers des clés d'un projet dont le domaine n'est
// plus signé — ou dont le projet ne signe plus rien.
func (p *Provider) orphanSteps(proj *model.Project, keys []Key) ([]*engine.Step, error) {
	signed := map[string]bool{}
	for _, d := range proj.DKIMDomains() {
		signed[d] = true
	}
	var steps []*engine.Step
	for _, k := range keys {
		if signed[model.MailDomain(k.Domain)] {
			continue
		}
		s, err := p.removeFileStep(proj.ID(), k)
		if err != nil {
			return nil, err
		}
		if s != nil {
			steps = append(steps, s)
		}
	}
	return steps, nil
}

// expirySteps retire les clés remplacées d'un domaine signé dont la rétention
// est écoulée. Elles étaient retirées au plan du projet, avant les tables : une
// bascule non suivie du recalcul des tables laissait celles-ci désigner une clé
// qui disparaissait (D108).
func (p *Provider) expirySteps(proj *model.Project, keys []Key) ([]*engine.Step, error) {
	signed := map[string]bool{}
	for _, d := range proj.DKIMDomains() {
		signed[d] = true
	}
	var steps []*engine.Step
	for _, k := range keys {
		if k.State != StateRetired || !p.expired(k) || !signed[model.MailDomain(k.Domain)] {
			continue
		}
		s, err := p.expireStep(proj.ID(), k)
		if err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	return steps, nil
}

func anyDKIM(projects []*model.Project) bool {
	for _, proj := range projects {
		if len(proj.DKIMDomains()) > 0 {
			return true
		}
	}
	return false
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// tableStep écrit une table gérée : inspection par empreinte, refus d'écraser
// ce qu'un exploitant a retouché (§6.4), compensation par l'ancien contenu.
func (p *Provider) tableStep(resource, summary, path string, content []byte, obs Observed) (*engine.Step, error) {
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	forced := false
	switch state {
	case fs.StateCurrent:
		return engine.NewStep(resource, engine.NoOp, summary), nil
	case fs.StateHandEdited, fs.StateForeign:
		if !p.Force {
			libelle, motif := "modifié hors ogs", "a été modifié en dehors d'ogs"
			if state == fs.StateForeign {
				libelle, motif = "non géré par ogs", "existe déjà sans porter la marque d'ogs"
			}
			s := engine.NewStep(resource, engine.Update, summary+" — fichier "+libelle)
			s.Critical = false
			s.Apply = func(context.Context) error {
				return fmt.Errorf("%s %s : relancer avec --force pour l'écraser "+
					"(l'original sera sauvegardé en %s)", path, motif, p.backupPattern(path))
			}
			return s, nil
		}
		forced = true
		state = fs.StateOutdated
		summary += " — écrasement forcé (sauvegarde " + p.backupPattern(path) + ")"
	}
	action := engine.Create
	if state != fs.StateAbsent {
		action = engine.Update
	}
	s := engine.NewStep(resource, action, summary)
	// Non critique, comme les clés : une table qu'on n'arrive pas à écrire ne doit
	// pas faire compenser le déploiement d'un site.
	s.Critical = false
	s.Diff = func() (string, error) {
		current, err := os.ReadFile(path)
		if err != nil {
			return "nouveau fichier " + path, nil
		}
		return fs.UnifiedDiff(string(current), string(content), path), nil
	}
	var previous []byte
	s.Apply = func(context.Context) error {
		if data, err := os.ReadFile(path); err == nil {
			previous = data
		}
		if forced {
			if _, err := fs.BackupTo(path, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := p.ensureOpenDKIMDir(obs); err != nil {
			return err
		}
		if err := fs.EnsureDir(filepath.Dir(path), OpenDKIMDirMode, p.dirOwner(obs)); err != nil {
			return err
		}
		// 0640 root:<groupe d'OpenDKIM> : les tables ne portent aucun secret, mais
		// c'est le régime qu'« ogs harden » impose à /etc/opendkim — les écrire en
		// 0644 ferait rouvrir à chaque apply ce que harden referme. OpenDKIM les
		// relit sous son propre compte, par son groupe.
		if err := fs.WriteAtomic(path, content, TableMode, p.dirOwner(obs)); err != nil {
			return err
		}
		p.written++
		return nil
	}
	s.Compensate = func(context.Context) error {
		p.written++
		if previous == nil {
			return os.Remove(path)
		}
		return fs.WriteAtomic(path, previous, TableMode, p.dirOwner(obs))
	}
	s.Reload = []string{ReloadTarget}
	return s, nil
}

// TableMode est le mode des tables d'OpenDKIM (voir tableStep).
const TableMode = 0o640

// removeStep retire une table devenue sans objet, sous le régime de son
// écriture : retouchée, elle ne part que sous --force et sauvegardée ; sans la
// marque d'ogs, c'est celle de l'administrateur, qu'OpenDKIM n'accepte qu'en un
// exemplaire — ogs ne la retire pas.
func (p *Provider) removeStep(resource, path string) (*engine.Step, error) {
	return removal.File{
		Resource:    resource,
		Summary:     "retrait de " + path + " (signature DKIM désactivée)",
		Path:        path,
		Force:       p.Force,
		Backups:     p.Paths.BackupsDir(),
		Now:         p.now,
		NonCritical: true,
	}.Step()
}

// Reload valide la configuration d'OpenDKIM, le recharge, puis constate.
//
// La validation n'est pas une politesse : un démon qui relit une table illisible
// s'arrête, et tout le courrier du serveur passe alors par le comportement de
// repli du MTA — en attente, ou sans signature. On valide avant, on constate
// après, et on relève le démon s'il est tombé.
func (p *Provider) Reload(ctx context.Context) error {
	written := p.written
	err := p.reload(ctx)
	if err == nil {
		p.reloaded, p.loadedAt = true, written
	}
	return err
}

func (p *Provider) reload(ctx context.Context) error {
	if p.Exec == nil || p.Exec.Simulated() {
		return nil
	}
	if res, err := p.Exec.Run(ctx, exec.Command{
		Name: "opendkim", Args: []string{"-n", "-x", p.Paths.OpenDKIMConf()}}); err != nil {
		return fmt.Errorf("configuration d'OpenDKIM refusée : %s", firstError(res, err))
	}
	if !p.unitActive(ctx) {
		// Le démon est arrêté : le démarrer serait une décision que
		// l'administrateur n'a pas prise.
		return nil
	}
	if res, err := p.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{"reload-or-restart", "opendkim"}}); err != nil {
		return fmt.Errorf("rechargement d'OpenDKIM : %s", firstError(res, err))
	}
	if !p.unitActive(ctx) {
		_, _ = p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"reset-failed", "opendkim"}})
		if _, err := p.Exec.Run(ctx, exec.Command{
			Name: "systemctl", Args: []string{"restart", "opendkim"}}); err != nil {
			return fmt.Errorf("OpenDKIM s'est arrêté au rechargement et n'a pas pu être relancé : %w", err)
		}
		return fmt.Errorf("OpenDKIM s'est arrêté au rechargement ; il a été relancé — " +
			"vérifier « journalctl -u opendkim »")
	}
	return nil
}

func (p *Provider) unitActive(ctx context.Context) bool {
	res, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"is-active", "opendkim"}})
	if err != nil && res.Stdout == "" {
		return false
	}
	return strings.TrimSpace(res.Stdout) == "active"
}

func firstError(res exec.Result, err error) string {
	if s := strings.TrimSpace(res.Stderr); s != "" {
		return s
	}
	if s := strings.TrimSpace(res.Stdout); s != "" {
		return s
	}
	return err.Error()
}
