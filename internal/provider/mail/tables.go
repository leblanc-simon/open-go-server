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
	var out []Entry
	for _, proj := range projects {
		domains := proj.DKIMDomains()
		if len(domains) == 0 || proj.IsPrep() {
			continue
		}
		keys, err := p.loadKeys(proj.ID())
		if err != nil {
			return nil, err
		}
		grouped := byDomain(keys)
		for _, d := range domains {
			k, ok := find(grouped[d], StateActive)
			if !ok {
				continue
			}
			path := p.Paths.OpenDKIMKeyFile(proj.ID(), d, k.Selector)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			out = append(out, Entry{Project: proj.ID(), Domain: d, Selector: k.Selector, KeyFile: path})
		}
	}
	return out, nil
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
// OpenDKIM absent ou désactivé, les tables d'un apply précédent sont retirées :
// elles désigneraient des clés que plus rien ne réconcilie.
func (p *Provider) PlanTables(projects []*model.Project, obs Observed) ([]*engine.Step, error) {
	if !obs.Present {
		var steps []*engine.Step
		for _, t := range []struct{ resource, path string }{
			{"mail/dkim-keytable", p.Paths.OpenDKIMKeyTable()},
			{"mail/dkim-signingtable", p.Paths.OpenDKIMSigningTable()},
		} {
			if s := removeStep(t.resource, t.path); s != nil {
				steps = append(steps, s)
			}
		}
		return steps, nil
	}
	entries, err := p.Entries(projects)
	if err != nil {
		return nil, err
	}
	// Aucun projet ne demande la signature et ogs n'a jamais écrit de tables : il
	// n'a rien à faire ici. OpenDKIM peut servir à l'administrateur par ailleurs,
	// et des tables vides posées d'office feraient recharger — donc valider — sa
	// configuration à chaque apply, pour rien.
	if !anyDKIM(projects) && !exists(p.Paths.OpenDKIMKeyTable()) && !exists(p.Paths.OpenDKIMSigningTable()) {
		return nil, nil
	}
	var steps []*engine.Step
	for _, t := range []struct {
		resource, summary, path string
		body                    []byte
	}{
		{"mail/dkim-keytable", fmt.Sprintf("KeyTable d'OpenDKIM (%d clé(s))", len(entries)),
			p.Paths.OpenDKIMKeyTable(), renderKeyTable(entries)},
		{"mail/dkim-signingtable", fmt.Sprintf("SigningTable d'OpenDKIM (%d domaine(s))", len(entries)),
			p.Paths.OpenDKIMSigningTable(), renderSigningTable(entries)},
	} {
		header := fs.Header{
			Project:   fmt.Sprintf("%d projet(s)", len(projects)),
			Resource:  t.resource,
			Generated: p.now(),
		}
		s, err := p.tableStep(t.resource, t.summary, t.path, fs.Render(header, t.body, "#"), obs)
		if err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	// Les fichiers des domaines qui ne sont plus signés ferment la marche, après
	// les tables qui cessent de les désigner.
	for _, proj := range projects {
		orphans, err := p.orphanSteps(proj, obs)
		if err != nil {
			return nil, err
		}
		steps = append(steps, orphans...)
	}
	return steps, nil
}

// orphanSteps retire les fichiers des clés d'un projet dont le domaine n'est
// plus signé — ou dont le projet ne signe plus rien.
func (p *Provider) orphanSteps(proj *model.Project, obs Observed) ([]*engine.Step, error) {
	keys, err := p.loadKeys(proj.ID())
	if err != nil {
		return nil, err
	}
	signed := map[string]bool{}
	for _, d := range proj.DKIMDomains() {
		signed[d] = true
	}
	var steps []*engine.Step
	for _, k := range keys {
		if signed[k.Domain] {
			continue
		}
		if s := p.removeFileStep(proj.ID(), k, obs); s != nil {
			steps = append(steps, s)
		}
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
		return fs.WriteAtomic(path, content, TableMode, p.dirOwner(obs))
	}
	s.Compensate = func(context.Context) error {
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

// removeStep retire une table devenue sans objet.
func removeStep(resource, path string) *engine.Step {
	previous, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	s := engine.NewStep(resource, engine.Delete, "retrait de "+path+" (signature DKIM désactivée)")
	s.Critical = false
	s.Diff = func() (string, error) { return "suppression de " + path, nil }
	s.Apply = func(context.Context) error {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	s.Compensate = func(context.Context) error { return fs.WriteAtomic(path, previous, TableMode, fs.KeepOwner) }
	return s
}

// Reload valide la configuration d'OpenDKIM, le recharge, puis constate.
//
// La validation n'est pas une politesse : un démon qui relit une table illisible
// s'arrête, et tout le courrier du serveur passe alors par le comportement de
// repli du MTA — en attente, ou sans signature. On valide avant, on constate
// après, et on relève le démon s'il est tombé.
func (p *Provider) Reload(ctx context.Context) error {
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
