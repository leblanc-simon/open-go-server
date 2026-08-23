// Package sudoers génère la délégation de pilotage à l'utilisateur d'un projet.
package sudoers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/fs"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/provider/unit"
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
	user := proj.Spec.User.Name
	var b strings.Builder
	fmt.Fprintf(&b, "# Délégation du pilotage des services à %s.\n", user)
	switch style {
	case StyleOGS:
		b.WriteString("# La commande ne prend aucun argument de projet : ogs déduit le projet de\n")
		b.WriteString("# l'appelant, donc aucune cible n'est détournable.\n")
		for _, verb := range verbs {
			if err := checkToken(verb); err != nil {
				return nil, err
			}
			fmt.Fprintf(&b, "%s ALL=(root) NOPASSWD: %s my %s\n", user, Binary, verb)
		}
	case StyleSystemctl:
		b.WriteString("# Unités énumérées nommément : régénéré à chaque changement d'échelle.\n")
		target := unit.TargetName(proj.ID())
		for _, verb := range verbs {
			if err := checkToken(verb); err != nil {
				return nil, err
			}
			fmt.Fprintf(&b, "%s ALL=(root) NOPASSWD: %s %s %s\n", user, SystemctlBinary, verb, target)
		}
		for _, svc := range proj.Spec.Services {
			count := instances[svc.Name]
			if count == 0 {
				count = svc.Instances
			}
			if count <= 0 {
				count = 1
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

// settings résout le style et les verbes, la déclaration du projet primant sur
// la configuration globale.
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

// checkToken refuse tout élément susceptible d'élargir la délégation.
func checkToken(v string) error {
	if v == "" {
		return fmt.Errorf("verbe vide")
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z') && r != '-' {
			return fmt.Errorf("verbe %q invalide : lettres minuscules et tiret seulement", v)
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
		if _, err := os.Stat(path); err == nil {
			s := engine.NewStep("sudoers/"+proj.ID(), engine.Delete, "délégation retirée")
			s.Apply = func(context.Context) error { return os.Remove(path) }
			return s, nil
		}
		return nil, nil
	}
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	style, _ := p.settings(proj)
	summary := fmt.Sprintf("délégation sudo à %s (style %s)", proj.Spec.User.Name, style)
	if state == fs.StateCurrent {
		return engine.NewStep("sudoers/"+proj.ID(), engine.NoOp, summary), nil
	}
	action := engine.Create
	if state != fs.StateAbsent {
		action = engine.Update
	}
	s := engine.NewStep("sudoers/"+proj.ID(), action, summary)
	s.Diff = func() (string, error) {
		current, err := os.ReadFile(path)
		if err != nil {
			return "nouveau fichier " + path, nil
		}
		return fs.UnifiedDiff(string(current), string(content), path), nil
	}
	s.Apply = func(ctx context.Context) error { return p.write(ctx, path, content) }
	s.Compensate = func(context.Context) error { return os.Remove(path) }
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
