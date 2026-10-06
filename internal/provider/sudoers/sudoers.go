// Package sudoers génère la délégation de pilotage à l'utilisateur d'un projet.
package sudoers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/removal"
	"leblanc.io/open-go-server/internal/provider/unit"
)

// Styles de délégation.
const (
	// StyleOGS n'autorise que « ogs my <verbe> » : aucune cible à détourner.
	StyleOGS = "ogs"
	// StyleSystemctl énumère les unités réelles, une ligne par verbe et par unité.
	StyleSystemctl = "systemctl"
	// StyleNone n'accorde rien.
	StyleNone = "none"
)

// DefaultVerbs sont les verbes délégués par défaut.
var DefaultVerbs = []string{"start", "stop", "restart", "status"}

// Binary est le chemin du binaire autorisé. Il doit être absolu : une commande
// relative dans un fichier sudoers serait résolue selon le PATH de l'appelant.
const Binary = "/usr/bin/ogs"

// SystemctlBinary est le chemin de systemctl sur Debian et Ubuntu.
const SystemctlBinary = "/usr/bin/systemctl"

// Provider génère /etc/sudoers.d/ogs-<id>.
type Provider struct {
	Paths config.Paths
	Exec  exec.Executor
	Now   func() time.Time
	// Force autorise l'écrasement d'un fichier qu'ogs n'a pas écrit, après
	// sauvegarde horodatée (§6.4). Sans lui, le refus n'aurait aucune porte de
	// sortie et l'apply du projet resterait bloqué.
	Force bool
}

// refusStep produit l'étape qui refuse d'écraser un fichier qu'ogs n'a pas
// écrit. Une règle sudoers retouchée à la main porte une délégation que
// quelqu'un a voulue : l'écraser en silence la retirerait sans un mot. Le
// message distingue les deux causes — un fichier « non géré » n'a jamais porté
// la marque d'ogs, un fichier « modifié hors ogs » a été retouché après coup.
func refusStep(resource, summary, path, backups string, state fs.State) *engine.Step {
	libelle, motif := "modifié hors ogs", "a été modifié en dehors d'ogs"
	if state == fs.StateForeign {
		libelle, motif = "non géré par ogs", "existe déjà sans porter la marque d'ogs"
	}
	s := engine.NewStep(resource, engine.Update, summary+" — fichier "+libelle)
	s.Apply = func(context.Context) error {
		return fmt.Errorf("%s %s : relancer avec --force pour l'écraser "+
			"(l'original sera sauvegardé dans %s)", path, motif, backups)
	}
	return s
}

// Name identifie le provider.
func (p *Provider) Name() string { return "sudoers" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Path renvoie le chemin du fichier sudoers d'un projet.
func (p *Provider) Path(id string) string { return p.Paths.SudoersFile(id) }

// Build calcule le contenu attendu. Les commandes sont énumérées sans le moindre
// métacaractère : un « * » dans une spécification sudoers laisserait passer des
// arguments imprévus, et « systemctl » sans unité ouvrirait link, isolate, mask
// ou l'arrêt de la machine.
func (p *Provider) Build(proj *model.Project, instances map[string]int) ([]byte, error) {
	style, verbs := p.settings(proj)
	if style == StyleNone {
		return nil, nil
	}
	if style == StyleSystemctl && len(proj.Spec.Services) == 0 && !supervised(proj) {
		// Rien à piloter : comme pour le style ogs d'un projet sans service, aucun
		// fichier. Recopié de config.yaml à la création (D121), le style aurait
		// sinon délégué une cible vide à chaque projet neuf.
		return nil, nil
	}
	user := proj.Spec.User.Name
	var b strings.Builder
	fmt.Fprintf(&b, "# Délégation du pilotage des services à %s.\n", user)
	switch style {
	case StyleOGS:
		b.WriteString("# La commande ne prend aucun argument de projet : ogs déduit le projet de\n")
		b.WriteString("# l'appelant, donc aucune cible n'est détournable.\n")
		for _, verb := range verbs {
			if err := checkToken("verbe", verb); err != nil {
				return nil, err
			}
			fmt.Fprintf(&b, "%s ALL=(root) NOPASSWD: %s my %s\n", user, Binary, verb)
		}
	case StyleSystemctl:
		b.WriteString("# Unités énumérées nommément : régénéré à chaque changement d'échelle.\n")
		target := unit.TargetName(proj.ID())
		for _, verb := range verbs {
			if err := checkToken("verbe", verb); err != nil {
				return nil, err
			}
			fmt.Fprintf(&b, "%s ALL=(root) NOPASSWD: %s %s %s\n", user, SystemctlBinary, verb, target)
		}
		for _, svc := range proj.Spec.Services {
			// Le nom du service est recopié dans une spécification de commande : il
			// subit le même régime que le verbe. Une virgule y sépare deux commandes
			// et un « # » commente la fin de ligne — deux élargissements qu'une
			// grammaire sudoers irréprochable ne trahit pas, donc que visudo laisse
			// passer.
			if err := checkToken("service", svc.Name); err != nil {
				return nil, err
			}
			// La spécification fait foi. La carte observée décrit l'état d'AVANT le
			// plan d'unités, qui vient de ramener le parc au nombre déclaré : s'y
			// fier laissait la délégation d'un apply en retard — sans droit sur les
			// instances qui viennent d'être démarrées à la montée en charge. L'observé
			// n'est retenu qu'en union, tant que la désactivation n'a pas eu lieu :
			// une instance encore active doit rester pilotable par son propriétaire.
			count := svc.Instances
			if count <= 0 {
				count = 1
			}
			if svc.Enabled != nil && !*svc.Enabled {
				// Un service explicitement désactivé ne se pilote plus, même s'il
				// tourne encore : l'unité étant un modèle, « systemctl start » ne
				// relancerait pas l'instance, il la recréerait.
				count = 0
			} else if observed := instances[svc.Name]; observed > count {
				count = observed
			}
			for i := 1; i <= count; i++ {
				name := unit.InstanceName(proj.ID(), svc.Name, i)
				for _, verb := range verbs {
					fmt.Fprintf(&b, "%s ALL=(root) NOPASSWD: %s %s %s\n",
						user, SystemctlBinary, verb, name)
				}
			}
		}
	default:
		return nil, fmt.Errorf("style de délégation %q inconnu (ogs, systemctl ou none)", style)
	}
	content := fs.Render(fs.Header{
		Project:   proj.ID(),
		Resource:  "sudoers/" + proj.ID(),
		Generated: p.now(),
	}, []byte(b.String()), "#")
	if err := Validate(content); err != nil {
		return nil, err
	}
	return content, nil
}

// CreationSettings rend la délégation que sudo.style et sudo.verbs de
// config.yaml demandent pour un projet neuf (D121), ou nil quand elle est celle
// que le provider applique à une déclaration sans réglage — ogs, et les verbes
// start, stop, restart et status. Ces deux clés sont un défaut de création, comme
// user.shell (D62) : copiées dans la déclaration d'un projet créé, elles ne
// valent rien pour un projet existant. Sans quoi changer config.yaml retirerait
// ou ajouterait des délégations à tout le parc au prochain apply — « style:
// none » retirant sans un mot les verbes dont un projet se sert.
func CreationSettings(style string, verbs []string) *model.Sudo {
	if style == "" {
		style = StyleOGS
	}
	if len(verbs) == 0 {
		verbs = DefaultVerbs
	}
	if style == StyleOGS && slices.Equal(verbs, DefaultVerbs) {
		return nil
	}
	out := &model.Sudo{Style: style}
	if style != StyleNone {
		out.Verbs = slices.Clone(verbs)
	}
	return out
}

// settings résout le style et les verbes : ceux de la déclaration, et à défaut
// la délégation historique. config.yaml n'est jamais lu ici (D121) : une
// déclaration sans réglage — tout projet créé avant D121, ou quand la
// configuration demandait ce défaut — garde la délégation qu'elle a toujours eue.
func (p *Provider) settings(proj *model.Project) (string, []string) {
	style, verbs := StyleOGS, DefaultVerbs
	if proj.Spec.Sudo != nil {
		if proj.Spec.Sudo.Style != "" {
			style = proj.Spec.Sudo.Style
		}
		if len(proj.Spec.Sudo.Verbs) > 0 {
			verbs = proj.Spec.Sudo.Verbs
		}
	}
	return style, verbs
}

// supervised dit si le projet a une application supervisée, dont l'unité
// rejoint la cible.
func supervised(proj *model.Project) bool {
	at, err := proj.AppType()
	return err == nil && at.Runtime != ""
}

// checkToken refuse tout élément susceptible d'élargir la délégation. Il vaut
// pour le verbe comme pour le nom de service : tous deux sont recopiés tels
// quels dans une spécification de commande.
func checkToken(kind, v string) error {
	if v == "" {
		return fmt.Errorf("%s vide", kind)
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("%s %q invalide : minuscules, chiffres, tiret et souligné seulement",
				kind, v)
		}
	}
	return nil
}

// Validate relit le contenu produit et refuse tout élargissement : c'est une
// vérification de dernier recours, appliquée au texte final plutôt qu'aux
// intentions, de sorte qu'une évolution du générateur ne puisse pas la contourner.
func Validate(content []byte) error {
	for i, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.ContainsAny(trimmed, "*?[]") {
			return fmt.Errorf("ligne %d : métacaractère interdit dans une règle sudoers (%q)", i+1, trimmed)
		}
		if strings.Contains(trimmed, "ALL=(ALL)") || strings.Contains(trimmed, "=(ALL:ALL)") {
			return fmt.Errorf("ligne %d : délégation trop large (%q)", i+1, trimmed)
		}
		colon := strings.Index(trimmed, "NOPASSWD:")
		if colon < 0 {
			return fmt.Errorf("ligne %d : règle inattendue (%q)", i+1, trimmed)
		}
		command := strings.TrimSpace(trimmed[colon+len("NOPASSWD:"):])
		// Une virgule sépare deux commandes dans une spécification sudoers, le tag
		// NOPASSWD se propageant à la suivante ; un « # » commente la fin de la
		// ligne. Ni l'un ni l'autre ne rend la règle invalide — visudo l'accepte —
		// et tous deux suffisent à glisser un shell dans la délégation.
		if strings.ContainsAny(command, ",#") {
			return fmt.Errorf("ligne %d : ni virgule ni dièse dans une spécification de commande (%q)",
				i+1, command)
		}
		fields := strings.Fields(command)
		if len(fields) == 0 || !filepath.IsAbs(fields[0]) {
			return fmt.Errorf("ligne %d : la commande doit être un chemin absolu (%q)", i+1, command)
		}
		// systemctl sans unité autorise link, isolate, mask, poweroff.
		if strings.HasSuffix(fields[0], "systemctl") && len(fields) < 3 {
			return fmt.Errorf("ligne %d : systemctl doit être suivi d'un verbe et d'une unité (%q)", i+1, command)
		}
	}
	return nil
}

// Plan renvoie l'étape d'écriture du fichier sudoers.
func (p *Provider) Plan(_ context.Context, proj *model.Project, instances map[string]int) (*engine.Step, error) {
	content, err := p.Build(proj, instances)
	if err != nil {
		return nil, err
	}
	path := p.Path(proj.ID())
	if content == nil {
		// Même régime qu'à l'écriture : une règle ajoutée à la main porte une
		// délégation que quelqu'un a voulue, et ne part que sous --force, copie
		// faite ; un fichier sans la marque d'ogs n'est pas à lui — doctor le
		// signale déjà sous ce préfixe.
		return removal.File{
			Resource: "sudoers/" + proj.ID(),
			Summary:  "délégation retirée",
			Path:     path,
			Project:  proj.ID(),
			Force:    p.Force,
			Backups:  p.Paths.BackupsDir(),
			Now:      p.now,
		}.Step()
	}
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	style, _ := p.settings(proj)
	summary := fmt.Sprintf("délégation sudo à %s (style %s)", proj.Spec.User.Name, style)
	resource := "sudoers/" + proj.ID()
	forced := false
	switch state {
	case fs.StateCurrent:
		return engine.NewStep(resource, engine.NoOp, summary), nil
	case fs.StateHandEdited, fs.StateForeign:
		if !p.Force {
			return refusStep(resource, summary, path, p.Paths.BackupsDir(), state), nil
		}
		// --force : l'étape redevient une mise à jour ordinaire, annoncée comme
		// telle par « plan » avant qu'« apply » ne l'exécute. La sauvegarde, elle,
		// n'a lieu qu'à l'application : « plan » et « diff » n'écrivent rien.
		forced = true
		state = fs.StateOutdated
		summary += " — écrasement forcé (sauvegarde dans " + p.Paths.BackupsDir() + ")"
	}
	action := engine.Create
	if state != fs.StateAbsent {
		action = engine.Update
	}
	s := engine.NewStep(resource, action, summary)
	s.Diff = func() (string, error) {
		current, err := os.ReadFile(path)
		if err != nil {
			return "nouveau fichier " + path, nil
		}
		return fs.UnifiedDiff(string(current), string(content), path), nil
	}
	var previous []byte
	var hadPrevious bool
	s.Apply = func(ctx context.Context) error {
		if data, err := os.ReadFile(path); err == nil {
			previous, hadPrevious = data, true
		}
		if forced {
			// Écraser le travail d'un exploitant sans en garder trace rend
			// l'opération irréversible : la sauvegarde précède l'écriture.
			if _, err := fs.BackupTo(path, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return p.write(ctx, path, content)
	}
	// La compensation rend la délégation qui était en place. La supprimer serait
	// juste pour une création, mais retirerait à l'utilisateur du projet un accès
	// qu'il avait déjà s'il s'agissait d'une mise à jour.
	s.Compensate = func(context.Context) error {
		if hadPrevious {
			return fs.WriteAtomic(path, previous, 0o440, fs.KeepOwner)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return s, nil
}

// write écrit le fichier sudoers après validation par visudo. Un fichier
// syntaxiquement invalide sous /etc/sudoers.d rend sudo inutilisable pour
// l'ensemble du système : la vérification a donc lieu sur un temporaire, et le
// renommage n'intervient qu'en cas de succès.
func (p *Provider) write(ctx context.Context, path string, content []byte) error {
	if err := fs.EnsureDir(filepath.Dir(path), 0o750, fs.KeepOwner); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ogs-sudoers-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 0440 root:root : sudo refuse un fichier de sudoers.d inscriptible.
	if err := os.Chmod(tmpName, 0o440); err != nil {
		return err
	}
	if err := p.visudo(ctx, tmpName); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// visudo valide un fichier sudoers. Son absence n'est pas bloquante — la
// validation interne a déjà eu lieu — mais elle est signalée.
func (p *Provider) visudo(ctx context.Context, path string) error {
	if p.Exec == nil || p.Exec.Simulated() {
		return nil
	}
	bin := filepath.Join(p.Paths.Prefix(), "/usr/sbin/visudo")
	if _, err := os.Stat(bin); err != nil {
		return nil
	}
	if _, err := p.Exec.Run(ctx, exec.Command{Name: bin, Args: []string{"-cf", path}}); err != nil {
		return fmt.Errorf("fichier sudoers refusé par visudo : %w", err)
	}
	return nil
}

// Audit relit tous les fichiers de délégation et signale les anomalies : un
// fichier ogs-* non généré par ogs, ou un contenu élargi à la main.
func (p *Provider) Audit() ([]string, error) {
	entries, err := os.ReadDir(p.Paths.SudoersDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var problems []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "ogs-") {
			continue
		}
		path := filepath.Join(p.Paths.SudoersDir(), e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s illisible : %v", path, err))
			continue
		}
		if !strings.Contains(string(data), fs.Marker) {
			problems = append(problems,
				fmt.Sprintf("%s porte le préfixe ogs- mais n'a pas été généré par ogs", path))
			continue
		}
		if err := Validate(data); err != nil {
			problems = append(problems, fmt.Sprintf("%s : %v", path, err))
		}
		if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o440 {
			problems = append(problems,
				fmt.Sprintf("%s : mode %04o, attendu 0440", path, info.Mode().Perm()))
		}
	}
	return problems, nil
}
