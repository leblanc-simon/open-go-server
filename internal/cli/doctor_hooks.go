package cli

import (
	"fmt"

	"leblanc.io/open-go-server/internal/hook"
	"leblanc.io/open-go-server/internal/model"
)

// hookChecks éprouve chaque hook comme son exécution l'éprouverait (D117) :
// un hook refusé ne l'est sinon qu'au moment où l'opération qu'il garde
// l'appelle — un « preDelete » refusé arrête la suppression. Un lien qui sort
// du répertoire des hooks reste admis tant que seul root contrôle sa cible et
// la chaîne qui y mène ; il est signalé, la copie du script sous le répertoire
// étant ce que HOOKS.md recommande.
func (a *App) hookChecks(projects []*model.Project, add func(name, status, detail, fix string)) {
	r := a.hookRunner()
	r.Disabled = false
	dir := a.Paths.HooksDir()
	n, refusés := 0, 0
	examiner := func(label string, p *model.Project, event string) {
		res, err := r.Inspect(p, event)
		switch {
		case err != nil:
			refusés++
			add("hook "+label, "échec", err.Error()+" — il sera refusé à l'exécution", "")
		case res.Real == "":
		case res.Outside(dir):
			n++
			add("hook "+label, "avertissement",
				fmt.Sprintf("%s est un lien vers %s, hors de %s : admis tant que seul root contrôle "+
					"la cible et ses répertoires", res.Path, res.Real, dir),
				fmt.Sprintf("install -o root -g root -m 0750 %s %s.copie && mv %s.copie %s",
					res.Real, res.Path, res.Path, res.Path))
		default:
			n++
		}
	}
	for _, event := range hook.Events() {
		examiner(event, nil, event)
	}
	for _, p := range projects {
		for _, event := range hook.Events() {
			if p.Spec.Hooks[event] == "" {
				continue
			}
			// Le hook déclaré par le projet, sans repli sur celui du parc : celui-ci
			// est déjà examiné.
			examiner(event+" ("+p.ID()+")", p, event)
		}
	}
	if n > 0 && refusés == 0 {
		add("hooks", "ok", fmt.Sprintf("%d hook(s) examiné(s) sous %s", n, dir), "")
	}
}
