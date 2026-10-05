package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/exec"
)

// PairCheck est l'état d'une paire clé/certificat que désigne un vhost activé.
type PairCheck struct {
	// Name est le nom du certificat (« shopfr », « @wildcard-prep »).
	Name string
	// Paired dit si la clé correspond au certificat — au retour, une écriture
	// interrompue ayant pu être achevée.
	Paired bool
	// Repaired dit qu'une écriture interrompue vient d'être achevée.
	Repaired bool
}

// UnpairedError nomme les certificats dépareillés que désignent les vhosts
// activés (D110) : le moteur refuserait sa configuration entière — nginx dès
// « nginx -t », Apache au rechargement seulement, dont son maître meurt alors
// qu'« apachectl -t » répond « Syntax OK ».
type UnpairedError struct {
	Names []string
	// Files associe chaque certificat aux vhosts activés qui le désignent.
	Files map[string][]string
}

func (e *UnpairedError) Error() string {
	var parts []string
	for _, n := range e.Names {
		parts = append(parts, fmt.Sprintf("certificat %s : clé et certificat dépareillés (désigné par %s) — "+
			"« ogs cert renew %s »", n, strings.Join(e.Files[n], ", "), n))
	}
	return strings.Join(parts, " ; ")
}

// CheckCertificates relit les certificats du magasin que désignent les vhosts
// activés — ceux d'ogs comme ceux posés à la main : un rechargement est global,
// et c'est le parc qui tombe — et vérifie leurs paires. Avec repair, une
// écriture interrompue est achevée (Store.Repair) ; une paire qu'aucun fichier
// en attente n'achève est nommée (UnpairedError). Sans repair — doctor —, rien
// n'est écrit.
//
// Sur le modèle de CheckLogs : c'est un trou que la validation laisse.
// « apachectl -t » ne charge aucune clé ; le maître, lui, les charge toutes au
// rechargement, et meurt sur la première qui ne correspond pas à son certificat
// (AH02565). Une clé recopiée à la main sur un site suffisait à faire tomber le
// parc au premier apply d'un projet sans rapport, rechargement annoncé « ↻ » et
// code 0.
func (p *Provider) CheckCertificates(ctx context.Context, repair bool) error {
	if p.CertificatePair == nil || p.CertDir == nil {
		return nil
	}
	refs := p.certificateDirs()
	if len(refs) == 0 {
		return nil
	}
	dirs := make([]string, 0, len(refs))
	for d := range refs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	unpaired := &UnpairedError{Files: map[string][]string{}}
	for _, dir := range dirs {
		check, err := p.CertificatePair(ctx, dir, repair)
		if err != nil {
			return err
		}
		if !check.Paired {
			unpaired.Names = append(unpaired.Names, check.Name)
			unpaired.Files[check.Name] = refs[dir]
		}
	}
	if len(unpaired.Names) > 0 {
		return unpaired
	}
	return nil
}

// certificateDirs énumère les répertoires du magasin de certificats que
// désignent les vhosts activés, avec les fichiers qui les désignent.
func (p *Provider) certificateDirs() map[string][]string {
	root := filepath.Dir(filepath.Clean(p.CertDir("ogs")))
	motif := regexp.MustCompile(regexp.QuoteMeta(root+string(filepath.Separator)) + `([^/\s"';]+)/`)
	out := map[string][]string{}
	entries, err := os.ReadDir(p.Engine.EnabledDir())
	if err != nil {
		return nil
	}
	for _, e := range entries {
		path := filepath.Join(p.Engine.EnabledDir(), e.Name())
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		for _, m := range motif.FindAllStringSubmatch(string(data), -1) {
			dir := filepath.Join(root, m[1])
			if m[1] == "." || m[1] == ".." || seen[dir] {
				continue
			}
			seen[dir] = true
			out[dir] = append(out[dir], e.Name())
		}
	}
	return out
}

// validateConfig valide la configuration entière du moteur, une écriture de
// certificat interrompue achevée d'abord : une paire dépareillée qu'un vhost
// désigne fait refuser toute la configuration par nginx, et le refus nomme alors
// le certificat plutôt que la seule sortie du moteur (D110).
func (p *Provider) validateConfig(ctx context.Context) error {
	pairs := p.CheckCertificates(ctx, true)
	err := validate(ctx, p.Engine, p.Exec)
	var unpaired *UnpairedError
	if err != nil && errors.As(pairs, &unpaired) {
		return fmt.Errorf("%w\n  %s", err, unpaired)
	}
	return err
}

// Observation d'un rechargement : un « reload » qui rend 0 ne dit pas que le
// serveur vit encore. « apachectl graceful » rend la main avant que le maître
// n'ait relu sa configuration, et un maître qui meurt en chargeant une clé le fait
// après — « is-active » relu aussitôt le voyait vivant.
var (
	reloadWatch = 2 * time.Second
	reloadPoll  = 200 * time.Millisecond
)

// unitSnapshot est l'état systemd du moteur.
type unitSnapshot struct {
	state     string
	mainPID   string
	nRestarts int
}

// snapshot relit l'état du moteur, vide si systemd ne le dit pas (substitut
// sans init).
func (p *Provider) snapshot(ctx context.Context) unitSnapshot {
	res, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{
		"show", p.Engine.Unit(), "-p", "ActiveState", "-p", "MainPID", "-p", "NRestarts"}})
	var s unitSnapshot
	if err != nil {
		return s
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			s.state = v
		case "MainPID":
			s.mainPID = v
		case "NRestarts":
			s.nRestarts, _ = strconv.Atoi(v)
		}
	}
	return s
}

// diedAfterReload observe le moteur pendant reloadWatch : il est mort si son
// unité cesse d'être active, si son processus principal change ou disparaît, ou
// si systemd l'a relancé entre-temps (NRestarts). Rend la raison, vide s'il a
// survécu.
func (p *Provider) diedAfterReload(ctx context.Context, before unitSnapshot) string {
	deadline := time.Now().Add(reloadWatch)
	for {
		if !p.unitActive(ctx) {
			return "unité inactive"
		}
		now := p.snapshot(ctx)
		switch {
		case now.state == "failed" || now.state == "inactive" || now.state == "deactivating":
			return "unité « " + now.state + " »"
		case before.mainPID != "" && before.mainPID != "0" && now.mainPID != "" && now.mainPID != before.mainPID:
			return fmt.Sprintf("processus principal %s remplacé par %s", before.mainPID, now.mainPID)
		case now.nRestarts > before.nRestarts:
			return "relancé par systemd"
		}
		if !time.Now().Before(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(reloadPoll):
		}
	}
}
