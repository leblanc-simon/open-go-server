package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/hook"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/store"
)

// Le chemin unique d'apply (D112).
//
// « ogs apply », « site create », « site clone », « project restore » et
// l'interface appliquaient chacun leur copie de la même boucle — projets, puis
// plan global —, et chaque copie avait perdu quelque chose : le plan global
// n'était ni affiché ni confirmé, et sauté sous --dry-run (R2-12) ; un projet en
// échec en privait tout le parc, y compris les projets appliqués avant lui
// (R1-38) ; preApply ne gardait pas le plan global (R3-24) ; l'interface
// appliquait sans plan global, sans hook et sans afficher un rechargement en
// échec (R1-39). Toutes passent désormais par applyFleet :
//
//  1. affichage : le plan de chaque projet visé, puis celui du parc, calculés
//     sans rien réserver ;
//  2. une seule confirmation pour le tout — sous --dry-run, l'affichage seul ;
//  3. les hooks preApply de chaque projet à changer, et celui du parc : un refus
//     arrête tout avant le premier geste ;
//  4. chaque projet, son plan recalculé juste avant son exécution : un plan qui
//     a grossi depuis l'affichage n'est appliqué qu'une fois ses ajouts affichés
//     et confirmés. Un projet en échec n'arrête que lui ;
//  5. le plan du parc, recalculé après les projets — c'est un ordre (fichiers de
//     parc, journaux, clés) — et exécuté même si un projet a échoué : un projet
//     en échec y figure par la déclaration du parc, jamais par la version qui
//     vient d'échouer, et une création défaite n'y figure plus ;
//  6. postCreate au premier apply réussi d'un projet déclaré par « project
//     create », « site create » ou l'interface.

// statusDeclared marque, dans la base d'état, un projet déclaré par une
// commande de création et jamais appliqué : son premier apply réussi lance
// postCreate (D112). Une base perdue ne porte aucune marque : le hook manque
// alors, il n'est jamais lancé à tort — un projet adopté, restauré ou cloné
// n'en reçoit pas non plus de cette façon.
const statusDeclared = "declared"

// applyRun paramètre une exécution du chemin d'apply.
type applyRun struct {
	// creation : la commande vient de déposer les déclarations qu'elle applique
	// (site create, site clone, project restore). Une création que la saga a
	// défaite entière retire sa déclaration : restée, elle entrait dans les
	// fichiers de parc — répertoire d'exécution d'un compte inexistant, et
	// chaque apply suivant rendait 6 (« Failed to resolve user »).
	creation bool
	// confirmed : la confirmation a été donnée par l'appelant sur le plan qu'il
	// a lui-même affiché (l'interface). Un plan qui a grossi depuis demande
	// toujours la sienne.
	confirmed bool
	// shown porte les plans que l'appelant a affichés, par projet, et sous « * »
	// celui du parc ; à défaut, ceux de l'affichage d'applyFleet font foi.
	shown map[string]planKeys
	// audit préfixe l'action enregistrée au journal d'audit.
	audit string
	// recap affiche les chaînes de connexion des projets appliqués.
	recap bool
}

// planKeys identifie les changements d'un plan, ressource et action.
type planKeys map[string]bool

// stepKey identifie un changement. Une purge d'archives ou une étape du pare-feu
// se comparent aussi par leur résumé, qui porte leur quantité : une purge d'une
// archive montrée n'en couvre pas cinq.
func stepKey(s *engine.Step) string {
	k := s.Resource + " " + s.Action.String()
	if retraitDuParc(s) {
		k += " " + s.Summary
	}
	return k
}

func keysOf(plan *engine.Plan) planKeys {
	k := planKeys{}
	if plan == nil {
		return k
	}
	for _, s := range plan.Mutating() {
		k[stepKey(s)] = true
	}
	return k
}

// without rend le plan privé des étapes données.
func without(plan *engine.Plan, drop []*engine.Step) *engine.Plan {
	gone := map[*engine.Step]bool{}
	for _, s := range drop {
		gone[s] = true
	}
	out := &engine.Plan{Project: plan.Project}
	for _, s := range plan.Steps {
		if !gone[s] {
			out.Add(s)
		}
	}
	return out
}

// beyond rend les changements du plan qui n'étaient pas affichés, restreints à
// ceux que retient keep (tous, sans filtre).
func beyond(plan *engine.Plan, shown planKeys, keep func(*engine.Step) bool) []*engine.Step {
	var out []*engine.Step
	for _, s := range plan.Mutating() {
		if shown[stepKey(s)] {
			continue
		}
		if keep == nil || keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// retraitDuParc dit si une étape du plan du parc retire ou ferme quelque chose
// que les déclarations confirmées ne commandent pas : la purge des archives
// (D52) et le pare-feu. Le reste du plan du parc se déduit des projets qu'on
// vient d'appliquer — leurs journaux, leurs clés, leurs certificats — et
// change naturellement entre l'affichage et l'exécution ; ces deux familles,
// elles, ne s'appliquent jamais sans avoir été montrées.
func retraitDuParc(s *engine.Step) bool {
	return strings.HasPrefix(s.Resource, "archive/") || strings.HasPrefix(s.Resource, "firewall/")
}

// fleetTarget suit un projet visé au fil du chemin.
type fleetTarget struct {
	p       *model.Project
	shown   planKeys
	changes int
	// failed est l'échec du projet : plan incalculable, refus, saga déroulée.
	failed error
	// created : projet déclaré par une commande de création et jamais appliqué
	// — postCreate lui est dû au premier apply qui aboutit, parc compris.
	created bool
	applied bool
}

// applyFleet est le chemin unique d'apply (D112).
func (a *App) applyFleet(ctx context.Context, projects []*model.Project, run applyRun) error {
	if run.audit == "" {
		run.audit = "apply."
	}
	// Verrou global : deux applications simultanées ne doivent jamais se croiser
	// sur la configuration du système.
	lock, err := engine.Acquire(a.Paths.GlobalLock())
	if err != nil {
		var locked engine.ErrLocked
		if errors.As(err, &locked) {
			return wrap(CodeLocked, err)
		}
		return wrap(CodeError, err)
	}
	defer lock.Release()
	// Chaque appelant éprouve déjà l'unicité ; la reprendre sous le verrou ferme
	// la fenêtre où un autre projet aurait été déclaré entre-temps (D94).
	if err := a.checkFleetUniqueness(projects...); err != nil {
		a.undoCreations(run, projects)
		return err
	}
	if err := a.openStore(); err != nil {
		return err
	}
	// L'affichage ne réserve aucun numéro (D100) : les plans exécutés sont
	// recalculés, eux, en réservant.
	a.Store.PreviewAllocations(true)
	provs, err := a.buildProviders()
	if err != nil {
		return err
	}
	defer provs.close()

	// 1. Affichage.
	targets := make([]*fleetTarget, 0, len(projects))
	for _, p := range projects {
		t := &fleetTarget{p: p, created: a.declaredOnly(p)}
		targets = append(targets, t)
		plan, err := a.buildPlan(ctx, provs, p)
		if err != nil {
			if ExitCode(err) == CodeCancelled || len(projects) == 1 {
				a.undoCreations(run, projects)
				return err
			}
			t.failed = err
			a.warnf("✗ %s : plan incalculable : %v\n", p.ID(), err)
			continue
		}
		t.shown = keysOf(plan)
		if k, ok := run.shown[p.ID()]; ok {
			t.shown = k
		}
		if plan.Empty() {
			a.printf("= %s : aucun changement\n", p.ID())
			continue
		}
		t.changes = len(plan.Mutating())
		a.printf("projet %s\n%s", p.ID(), plan.String())
	}
	preview, previewErr := a.buildGlobalPlan(ctx, provs, standing(targets))
	switch {
	case previewErr != nil && ExitCode(previewErr) == CodeCancelled:
		return previewErr
	case previewErr != nil:
		// Le plan du parc sera recalculé après les projets : son échec ici ne
		// prive pas les projets de leur apply, il sera rendu plus loin.
		a.warnf("⚠ plan du parc incalculable avant les projets : %v\n", previewErr)
	case !preview.Empty():
		a.printf("parc\n%s", preview.String())
	}
	shownGlobal := keysOf(preview)
	if k, ok := run.shown["*"]; ok {
		shownGlobal = k
	}

	pending := 0
	var names []string
	total := 0
	for _, t := range targets {
		if t.failed == nil && t.changes > 0 {
			pending++
			total += t.changes
			names = append(names, t.p.ID())
		}
	}
	globalPending := previewErr != nil || !preview.Empty()
	if globalPending {
		names = append(names, "le parc")
		if preview != nil {
			total += len(preview.Mutating())
		}
	}
	if a.DryRun {
		if pending > 0 || globalPending {
			a.printf("--dry-run : rien n'a été appliqué.\n")
		}
		return fleetOutcome(targets, nil, 0, false)
	}
	if pending == 0 && !globalPending {
		return fleetOutcome(targets, nil, 0, false)
	}

	// 2. Une seule confirmation, projets et parc ensemble.
	if !run.confirmed {
		if err := a.confirm(fmt.Sprintf("appliquer %d changement(s) sur %s ?", total, joinEt(names))); err != nil {
			// Refusée, une création n'a rien laissé : sa déclaration part avec elle.
			a.undoCreations(run, projects)
			return err
		}
	}

	// 3. Les hooks gardent la porte, avant le premier geste : preApply de chaque
	// projet à changer, puis preApplyFleet pour le plan du parc. preApply n'est
	// jamais appelé sans projet : un hook écrit pour un projet, appelé avec
	// OGS_ROOT vide, opérait sur la racine de la machine.
	for _, t := range targets {
		if t.failed != nil || t.changes == 0 {
			continue
		}
		if _, err := a.runHook(ctx, provs.Hooks, t.p, hook.PreApply, hook.Context{}); err != nil {
			a.undoCreations(run, projects)
			return err
		}
	}
	parcHook := false
	if globalPending {
		if _, err := a.runHook(ctx, provs.Hooks, nil, hook.PreApplyFleet, hook.Context{}); err != nil {
			a.undoCreations(run, projects)
			return err
		}
		parcHook = true
	}

	// 4. Les projets.
	a.Store.PreviewAllocations(false)
	reload := a.reloadFunc(provs)
	warnings := 0
	interrupted := false
	for _, t := range targets {
		if t.failed != nil || t.changes == 0 {
			continue
		}
		if ctx.Err() != nil {
			interrupted = true
			break
		}
		w, err := a.applyTarget(ctx, provs, t, run, reload)
		warnings += w
		if err != nil {
			t.failed = err
			if ExitCode(err) == CodeCancelled && ctx.Err() != nil {
				interrupted = true
				break
			}
		}
	}

	// 5. Le parc, une fois les projets appliqués, même si l'un a échoué.
	var globalErr error
	if !interrupted && ctx.Err() == nil {
		w, err := a.applyGlobal(ctx, provs, targets, shownGlobal, parcHook, run)
		warnings += w
		globalErr = err
	} else {
		interrupted = true
	}

	// 6. postCreate, au premier apply réussi d'un projet déclaré par une
	// commande de création, une fois le plan du parc réussi. Sinon la marque
	// reste, et le prochain apply qui aboutit le lancera.
	for _, t := range targets {
		if !t.created || t.failed != nil {
			continue
		}
		if globalErr != nil || interrupted {
			_ = a.Store.SetProjectStatus(t.p.ID(), statusDeclared)
			continue
		}
		if !t.applied {
			// Rien à changer au projet lui-même : son premier apply est celui-ci.
			_ = a.recordState(t.p)
		}
		if warned, _ := a.runHook(ctx, provs.Hooks, t.p, hook.PostCreate, hook.Context{}); warned {
			warnings++
		}
	}
	if run.recap {
		a.connectionRecap(ctx, provs, targets)
	}
	return fleetOutcome(targets, globalErr, warnings, interrupted)
}

// executeProjectPlan exécute le plan d'un projet ; les tests y substituent un
// déroulage incomplet, qu'aucun provider ne produit à la demande.
var executeProjectPlan = engine.Execute

// unwoundEntirely dit si une saga en échec n'a rien laissé derrière elle : ni
// compensation en échec, ni étape résiduelle (D104). Seule une création ainsi
// défaite retire sa déclaration ; sinon ce qu'elle a laissé reste nommé et
// reprenable — une restauration se reprend par --resume, et « ogs project
// delete » démonte le reste (R1-54).
func unwoundEntirely(report *engine.Report) bool {
	return report != nil && report.RollbackErr == nil && report.Residue() == nil
}

// applyTarget exécute le plan d'un projet, recalculé et réservé.
func (a *App) applyTarget(ctx context.Context, provs *providers, t *fleetTarget, run applyRun,
	reload func(context.Context, string) error) (int, error) {
	p := t.p
	mark := a.Store.AllocationMark()
	plan, err := a.buildPlan(ctx, provs, p)
	if err != nil {
		_ = a.Store.CancelAllocations(mark)
		a.warnf("✗ %s : %v\n", p.ID(), err)
		a.undoCreations(run, []*model.Project{p})
		return 0, err
	}
	if plan.Empty() {
		a.printf("= %s : plus aucun changement\n", p.ID())
		return 0, nil
	}
	// Ce qui n'a pas été montré n'a pas été confirmé.
	if extra := beyond(plan, t.shown, nil); len(extra) > 0 {
		a.warnf("le plan de %s a changé depuis son affichage :\n", p.ID())
		for _, s := range extra {
			a.warnf("  %s\n", s.String())
		}
		if err := a.confirmExtra(run, fmt.Sprintf("appliquer aussi ces %d changement(s) sur %s ?", len(extra), p.ID())); err != nil {
			_ = a.Store.CancelAllocations(mark)
			a.undoCreations(run, []*model.Project{p})
			return 0, err
		}
	}
	report, err := executeProjectPlan(ctx, plan, engine.Options{
		OnStep: func(s *engine.Step, stepErr error) {
			a.stepf("  ", s, stepErr)
		},
		OnWarning: func(s *engine.Step, stepErr error) {
			a.warnf("  ⚠ %s : %v\n", s.Summary, stepErr)
		},
		Audit: func(s *engine.Step, outcome, detail string) {
			_ = a.Audit.Record(p.ID(), s.Resource, run.audit+s.Action.String(), outcome, detail)
		},
		Reload:          reload,
		OnReloadWarning: a.reloadWarning,
	})
	warnings := len(report.Warnings)
	if err != nil {
		clean := unwoundEntirely(report)
		if report.RolledBack && report.RollbackErr == nil {
			// Déroulé, le plan n'a laissé aucun fichier qui serve ses numéros.
			_ = a.Store.CancelAllocations(mark)
		}
		a.reportRollback(report)
		if run.creation && clean {
			a.undoCreation(p)
		}
		return warnings, wrap(CodeError, err)
	}
	if err := a.recordState(p); err != nil {
		return warnings, err
	}
	t.applied = true
	a.printf("✓ %s : %d étape(s) appliquée(s)", p.ID(), report.Applied())
	if len(report.Warnings) > 0 {
		a.printf(", %d avertissement(s)", len(report.Warnings))
	}
	a.printf(" (audit %s)\n", a.Audit.RunID())
	// Les certificats émis par ce plan l'ont été avant son rechargement groupé,
	// achevé ici : « postCertRenew » le dit maintenant (D117) — pour « ogs
	// apply », site create, site clone, la restauration et l'interface. Un
	// certificat nommé par deux étapes ne compte qu'une fois : le hook d'un
	// projet ne reçoit pas deux fois le même certificat partagé.
	if changed := uniqueNames(certsChanged(report)); len(changed) > 0 {
		warnings += a.postCertRenew(ctx, provs.Hooks, changed,
			a.certUsersFleet(provs, changed, []*model.Project{p}))
	}
	// Le hook « postApply » constate : son échec est un avertissement.
	if warned, _ := a.runHook(ctx, provs.Hooks, p, hook.PostApply,
		hook.Context{Resources: resourcesOf(report)}); warned {
		warnings++
	}
	return warnings, nil
}

// applyGlobal recalcule et exécute le plan du parc après les projets. Les
// projets visés qui ont abouti, ou que rien ne changeait, y figurent par leur
// version en mémoire (« -f ») ; un projet en échec, par la déclaration du parc.
func (a *App) applyGlobal(ctx context.Context, provs *providers, targets []*fleetTarget, shown planKeys,
	parcHook bool, run applyRun) (int, error) {
	global, err := a.buildGlobalPlan(ctx, provs, standing(targets))
	if err != nil {
		return 0, err
	}
	if global.Empty() {
		return 0, nil
	}
	global, skipped := a.settleExtras(global, beyond(global, shown, retraitDuParc), run.confirmed)
	if global.Empty() {
		return skipped, nil
	}
	if !parcHook {
		if _, err := a.runHook(ctx, provs.Hooks, nil, hook.PreApplyFleet, hook.Context{}); err != nil {
			return skipped, err
		}
	}
	a.printf("parc\n")
	report, warnings, err := a.runGlobal(ctx, provs, global, run.audit)
	warnings += skipped
	if err != nil {
		return warnings, err
	}
	if warned, _ := a.runHook(ctx, provs.Hooks, nil, hook.PostApplyFleet,
		hook.Context{Resources: resourcesOf(report)}); warned {
		warnings++
	}
	return warnings, nil
}

// settleExtras traite ce qui s'est ajouté au plan du parc depuis son affichage :
// montré, puis confirmé. Refusé — ou impossible à demander, sous l'interface —,
// seuls ces ajouts sont écartés, le reste du plan du parc s'applique : ce que
// les projets viennent de créer en dépend. L'écart est un avertissement (code 6).
func (a *App) settleExtras(plan *engine.Plan, extra []*engine.Step, confirmed bool) (*engine.Plan, int) {
	if len(extra) == 0 {
		return plan, 0
	}
	a.warnf("le plan du parc a changé depuis son affichage :\n")
	for _, s := range extra {
		a.warnf("  %s\n", s.String())
	}
	if !confirmed {
		if err := a.confirm(fmt.Sprintf("appliquer aussi ces %d changement(s) sur le parc ?", len(extra))); err == nil {
			return plan, 0
		}
	}
	a.warnf("⚠ %d changement(s) du parc écarté(s), faute de confirmation : un prochain « ogs apply » les "+
		"proposera de nouveau\n", len(extra))
	return without(plan, extra), 1
}

// runGlobal exécute le plan du parc : affichage, audit, rechargements — sans
// eux, systemd-tmpfiles ne tournait pas et le timer de renouvellement n'était
// jamais activé —, rechargements en échec affichés, retour d'ufw annoncé.
func (a *App) runGlobal(ctx context.Context, provs *providers, plan *engine.Plan, audit string) (*engine.Report, int, error) {
	report, err := engine.Execute(ctx, plan, engine.Options{
		OnStep: func(s *engine.Step, stepErr error) {
			a.stepf("  ", s, stepErr)
		},
		OnWarning: func(s *engine.Step, stepErr error) {
			a.warnf("  ⚠ %s : %v\n", s.Summary, stepErr)
		},
		Audit: func(s *engine.Step, outcome, detail string) {
			_ = a.Audit.Record("", s.Resource, audit+s.Action.String(), outcome, detail)
		},
		Reload:          a.reloadFunc(provs),
		OnReloadWarning: a.reloadWarning,
	})
	if err != nil {
		a.reportRollback(report)
		return report, len(report.Warnings), wrap(CodeError, err)
	}
	a.announceRollback(provs.Firewall)
	return report, len(report.Warnings), nil
}

// confirmExtra demande la confirmation de ce qui s'est ajouté au plan depuis
// son affichage. Quand la confirmation venait de l'appelant (l'interface), elle
// ne peut pas être redemandée ici : le plan est à revoir.
func (a *App) confirmExtra(run applyRun, question string) error {
	if run.confirmed {
		return exitf(CodeCancelled, "le plan a changé depuis son affichage : le revoir avant d'appliquer")
	}
	return a.confirm(question)
}

// standing rend les projets visés qui figurent au plan du parc par leur version
// en mémoire : ceux qui ont abouti ou que rien ne changeait. Un projet en échec
// n'y est pas substitué — sa version refusée ne doit pas se propager au parc.
func standing(targets []*fleetTarget) []*model.Project {
	out := make([]*model.Project, 0, len(targets))
	for _, t := range targets {
		if t.failed == nil {
			out = append(out, t.p)
		}
	}
	return out
}

func resourcesOf(report *engine.Report) []string {
	if report == nil {
		return nil
	}
	out := make([]string, 0, len(report.Outcomes))
	for _, o := range report.Outcomes {
		out = append(out, o.Step.Resource)
	}
	return out
}

// fleetOutcome rend le code de l'exécution : l'échec d'un projet d'abord — son
// propre code s'il est seul —, puis celui du parc, l'interruption, et enfin les
// avertissements (code 6).
func fleetOutcome(targets []*fleetTarget, globalErr error, warnings int, interrupted bool) error {
	var failed []*fleetTarget
	for _, t := range targets {
		if t.failed != nil {
			failed = append(failed, t)
		}
	}
	switch {
	case len(failed) == 1 && globalErr == nil:
		return failed[0].failed
	case len(failed) > 0:
		parts := make([]string, 0, len(failed)+1)
		for _, t := range failed {
			parts = append(parts, fmt.Sprintf("%s (%v)", t.p.ID(), t.failed))
		}
		if globalErr != nil {
			parts = append(parts, fmt.Sprintf("parc (%v)", globalErr))
		}
		return exitf(ExitCode(failed[0].failed), "%d projet(s) en échec : %s", len(failed), strings.Join(parts, " ; "))
	case globalErr != nil:
		return globalErr
	case interrupted:
		return exitf(CodeCancelled, "interruption reçue : l'apply s'est arrêté")
	case warnings > 0:
		return exitf(CodePartial, "terminé avec %d avertissement(s)", warnings)
	}
	return nil
}

// joinEt énumère « a », « a et b », « a, b et c ».
func joinEt(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " et " + names[len(names)-1]
}

// connectionRecap affiche les chaînes de connexion des projets appliqués : ce
// que l'exploitant doit reporter dans la configuration applicative.
func (a *App) connectionRecap(ctx context.Context, provs *providers, targets []*fleetTarget) {
	for _, t := range targets {
		p := t.p
		if t.failed != nil || (len(p.Spec.Databases) == 0 && p.Spec.AMQP == nil) {
			continue
		}
		entries, err := provs.DB.Env(ctx, p)
		if err != nil || len(entries) == 0 {
			continue
		}
		a.printf("\nchaînes de connexion de %s :\n", p.ID())
		for _, e := range entries {
			a.printf("  %s=%q\n", e.Name, e.Value)
		}
	}
}

// markDeclared inscrit un projet déclaré par une commande de création et pas
// encore appliqué (statusDeclared). La ligne qu'il remplace — celle d'un projet
// supprimé du même identifiant — est gardée pour une création défaite.
func (a *App) markDeclared(p *model.Project) {
	if a.Store == nil {
		return
	}
	if a.declaredBefore == nil {
		a.declaredBefore = map[string]*store.ProjectRow{}
	}
	if _, seen := a.declaredBefore[p.ID()]; !seen {
		var prev *store.ProjectRow
		if row, err := a.Store.Project(p.ID()); err == nil {
			prev = &row
		}
		a.declaredBefore[p.ID()] = prev
	}
	_ = a.Store.UpsertProject(store.ProjectRow{
		ID: p.ID(), Host: p.Metadata.Host, Env: p.Metadata.Env,
		EffectiveHost: effectiveHostOf(p), Home: p.Spec.Layout.Root,
		AppKind: string(p.Spec.App.Kind), AppVersion: p.Spec.App.Version,
		Status: statusDeclared,
	})
}

// declaredOnly dit si le projet attend son premier apply réussi.
func (a *App) declaredOnly(p *model.Project) bool {
	if a.Store == nil {
		return false
	}
	row, err := a.Store.Project(p.ID())
	return err == nil && row.Status == statusDeclared
}

// undoCreations défait les créations d'une exécution qui n'a rien appliqué.
func (a *App) undoCreations(run applyRun, projects []*model.Project) {
	if !run.creation {
		return
	}
	for _, p := range projects {
		a.undoCreation(p)
	}
}

// undoCreation retire la déclaration d'une création que rien n'a laissée sur
// le système : la saga l'a défaite entière, ou rien n'a été appliqué. Restée,
// elle entrait dans les fichiers de parc — le répertoire d'exécution d'un compte
// qui n'existe pas — et l'apply suivant d'un voisin rendait 6. Les secrets
// qu'une restauration a réinjectés restent au keystore, comme sous --keep-data :
// ils prouvent peut-être des ressources restées en place (D100), et une nouvelle
// restauration les réécrit.
func (a *App) undoCreation(p *model.Project) {
	path := a.Paths.ProjectFile(p.ID())
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		a.warnf("⚠ création de %s défaite, mais sa déclaration reste : %v — la retirer : rm %s\n",
			p.ID(), err, path)
		return
	}
	if a.Store != nil && a.declaredOnly(p) {
		if prev := a.declaredBefore[p.ID()]; prev != nil {
			_ = a.Store.UpsertProject(*prev)
		} else {
			_ = a.Store.DeleteProject(p.ID())
		}
	}
	if a.undone == nil {
		a.undone = map[string]bool{}
	}
	a.undone[p.ID()] = true
	a.warnf("↶ création de %s défaite : sa déclaration %s est retirée\n", p.ID(), path)
}

// creationUndone dit que la création du projet a été défaite, sa déclaration
// retirée : une restauration retire alors aussi son témoin (D113).
func (a *App) creationUndone(id string) bool { return a.undone[id] }

// uniqueNames rend les noms sans doublon, dans leur premier ordre.
func uniqueNames(names []string) []string {
	seen := map[string]bool{}
	out := names[:0:0]
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
