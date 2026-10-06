package cli

import (
	"context"
	"errors"
	"strings"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/hook"
	"leblanc.io/open-go-server/internal/model"
)

// certsDuProjet porte les certificats changés que sert un projet.
type certsDuProjet struct {
	p     *model.Project
	certs []string
}

// postCertRenew déclenche « postCertRenew » une fois le serveur web rechargé
// (D117) — par « cert renew », « cert issue » et « apply », dès qu'un certificat
// a changé. Le hook de parc est appelé une fois, pour tous les certificats
// changés, sans projet : c'est l'événement tel qu'il existait. Le hook que
// déclare un projet (spec.hooks) l'est en plus, pour les siens, avec son
// contexte : il ne remplace pas celui du parc, qui n'est pas un hook de projet.
// Il rend le nombre d'avertissements émis.
func (a *App) postCertRenew(ctx context.Context, r *hook.Runner, all []string, parProjet []certsDuProjet) int {
	if len(all) == 0 {
		return 0
	}
	warned := 0
	if w, _ := a.runHook(ctx, r, nil, hook.PostCertRenew, certHookContext(all)); w {
		warned++
	}
	for _, pc := range parProjet {
		if len(pc.certs) == 0 || pc.p.Spec.Hooks[hook.PostCertRenew] == "" {
			continue
		}
		if err := r.RunDeclared(ctx, pc.p, hook.PostCertRenew, certHookContext(pc.certs)); err != nil {
			a.warnf("⚠ %v\n", err)
			warned++
		}
	}
	return warned
}

func certHookContext(certs []string) hook.Context {
	return hook.Context{Resources: certs, Extra: map[string]string{"cert": strings.Join(certs, ",")}}
}

// certUsers range les certificats changés par projet qui les sert. Un
// certificat partagé se range sous chacun des projets qui le désignent.
func certUsers(provs *providers, names []string, projects []*model.Project) []certsDuProjet {
	var out []certsDuProjet
	for _, p := range projects {
		var certs []string
		for _, name := range names {
			for _, id := range provs.TLS.Users(name, []*model.Project{p}) {
				if id == p.ID() {
					certs = append(certs, name)
				}
			}
		}
		if len(certs) > 0 {
			out = append(out, certsDuProjet{p: p, certs: certs})
		}
	}
	return out
}

// certsChanged énumère les certificats qu'un apply a émis ou réémis : l'étape
// de certificat appliquée, ou aboutie en partie (repli, D80), et non compensée.
func certsChanged(report *engine.Report) []string {
	var out []string
	for _, o := range report.Outcomes {
		name, ok := strings.CutPrefix(o.Step.Resource, "tls/certificate/")
		if !ok || o.Skipped || o.Compensated {
			continue
		}
		if partial := (*engine.PartialError)(nil); o.Err != nil && !errors.As(o.Err, &partial) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// certUsersFleet range les certificats changés sous les projets qui les
// servent, dans tout le parc : un certificat partagé renouvelé change pour
// chacun d'eux. Le parc qui ne se charge pas — un voisin refusé — se replie sur
// les projets déjà connus de la commande, sans l'arrêter.
//
// Un projet que la commande connaît l'emporte sur sa déclaration du parc :
// « apply -f » applique une version qui n'est pas encore celle du disque, et
// c'est celle-là dont les certificats viennent de changer.
func (a *App) certUsersFleet(provs *providers, names []string, known []*model.Project) []certsDuProjet {
	fleet, err := a.loadProjects()
	if err != nil {
		return certUsers(provs, names, known)
	}
	merged := make([]*model.Project, 0, len(fleet)+len(known))
	seen := map[string]bool{}
	for _, p := range known {
		seen[p.ID()] = true
	}
	for _, p := range fleet {
		if !seen[p.ID()] {
			merged = append(merged, p)
		}
	}
	merged = append(merged, known...)
	return certUsers(provs, names, merged)
}

// postCertRenewAfterReload lance « postCertRenew » si le serveur web a relu les
// certificats, et dit sinon pourquoi il ne l'est pas : le hook annoncerait un
// certificat que le serveur ne présente pas. Il rend le nombre
// d'avertissements, le refus compris.
func (a *App) postCertRenewAfterReload(ctx context.Context, provs *providers, reloadErr error,
	changed []string, known []*model.Project) int {
	if len(changed) == 0 {
		return 0
	}
	if reloadErr != nil {
		a.warnf("⚠ hook postCertRenew non lancé : le serveur web n'a pas relu %s\n", strings.Join(changed, ", "))
		return 1
	}
	return a.postCertRenew(ctx, provs.Hooks, changed, a.certUsersFleet(provs, changed, known))
}
