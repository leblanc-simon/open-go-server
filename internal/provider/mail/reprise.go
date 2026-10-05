package mail

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/system"
)

// Ce fichier porte D108 : ce qu'ogs fait d'une clé que le keystore ne connaît
// pas, de clés qu'il ne peut pas ouvrir, et la condition à laquelle le fichier
// d'une clé peut partir.

// unreadableStep rend l'étape en échec qui remplace celles d'un projet dont les
// clés ne s'ouvrent pas : keystore indisponible, clé maître régénérée ou refusée
// pour son mode. Faire échouer le plan arrêtait tout apply — le plan global
// porte les tables du parc entier —, et la cause était avalée.
func (p *Provider) unreadableStep(resource, projectID, consequence string, cause error) *engine.Step {
	s := engine.NewStep(resource, engine.Update,
		"clés DKIM de "+projectID+" — illisibles, "+consequence)
	s.Critical = false
	s.Apply = func(context.Context) error {
		return fmt.Errorf("%v — vérifier %s et la base d'état", cause, p.Paths.MasterKey())
	}
	return s
}

// maxKeyFile borne la lecture d'un fichier de clé : une clé RSA-4096 en PEM fait
// à peine plus de 3 Kio.
const maxKeyFile = 64 << 10

// readKeyFile lit le fichier d'une clé en place, par un descripteur ouvert sans
// suivre de lien, et rend son contenu s'il peut être tenu pour une clé d'ogs :
// fichier ordinaire à un seul nom, 0600, au compte d'OpenDKIM, clé RSA lisible. C'est le
// régime sous lequel ogs les écrit, et celui sous lequel OpenDKIM les accepte.
func (p *Provider) readKeyFile(path string, obs Observed) (string, time.Time, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return "", time.Time{}, errors.New("lien symbolique, pas un fichier ordinaire")
	}
	if err != nil {
		return "", time.Time{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return "", time.Time{}, errors.New("pas un fichier ordinaire")
	}
	// Un lien physique ne se voit pas comme un lien : root a pu lier ici la clé
	// d'un autre projet, que le rescellement aurait donnée à celui-ci.
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return "", time.Time{}, fmt.Errorf("lien physique (%d noms) : la clé d'un autre domaine, peut-être", st.Nlink)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return "", time.Time{}, fmt.Errorf("mode %04o au lieu de 0600", perm)
	}
	if !p.simulated() && obs.OwnerOK {
		if uid, gid, ok := system.OwnerOf(info); !ok || uid != obs.UID || gid != obs.GID {
			return "", time.Time{}, fmt.Errorf("propriétaire %d:%d au lieu de %s", uid, gid, p.Cfg.Mail.DKIMUser())
		}
	}
	if info.Size() > maxKeyFile {
		return "", time.Time{}, fmt.Errorf("%d octets : trop grand pour une clé", info.Size())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return "", time.Time{}, err
	}
	if _, err := PublicKey(string(data)); err != nil {
		return "", time.Time{}, err
	}
	return string(data), info.ModTime().UTC(), nil
}

// selectorsOnDisk énumère les sélecteurs des fichiers de clés posés pour un
// domaine. Le répertoire est à root (0750 root:<groupe d'OpenDKIM>) : le lire par
// son nom ne laisse le choix à personne d'autre.
func (p *Provider) selectorsOnDisk(projectID, domain string) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(filepath.Dir(p.Paths.OpenDKIMKeyFile(projectID, domain, "x")))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if sel, ok := strings.CutSuffix(e.Name(), ".private"); ok && sel != "" && !strings.HasPrefix(sel, ".") {
			out[sel] = true
		}
	}
	return out
}

// diskKey est un fichier de clé en place : le répertoire de domaine qui le porte
// — sous la forme, brute ou canonique, où il a été écrit — et son sélecteur.
type diskKey struct{ dir, selector string }

// filesOnDisk énumère les fichiers de clés posés pour un domaine signé, sous
// toute forme de son nom : une clé scellée avant D108 sous « Shop.fr » vit dans
// keys/<id>/Shop.fr/, et c'est elle que le DNS publie. Ne chercher que sous la
// forme canonique faisait engendrer une clé neuve après la perte de la base.
func (p *Provider) filesOnDisk(projectID, domain string) []diskKey {
	dirs, err := os.ReadDir(filepath.Join(p.Paths.OpenDKIMKeysDir(), projectID))
	if err != nil {
		return nil
	}
	var out []diskKey
	for _, d := range dirs {
		if !d.IsDir() || model.MailDomain(d.Name()) != domain {
			continue
		}
		for sel := range p.selectorsOnDisk(projectID, d.Name()) {
			out = append(out, diskKey{dir: d.Name(), selector: sel})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].selector != out[j].selector {
			return out[i].selector < out[j].selector
		}
		return out[i].dir < out[j].dir
	})
	return out
}

// adoptOrCreateStep traite un domaine signé dont le keystore ne connaît aucune
// clé active ni préparée. Sans fichier en place, la clé est engendrée. Avec un
// fichier en place — la base d'état a été perdue, ou recréée —, ce fichier est
// la clé que le DNS publie : la régénérer l'écrasait sans copie, sous le même
// sélecteur du même mois, et les tables désignaient une clé que personne n'avait
// publiée. Il est donc **rescellé tel quel** s'il est au régime d'ogs, et refusé
// sinon — jamais remplacé par une clé neuve (D108).
//
// La clé rescellée garde le domaine du répertoire qui la porte : c'est lui qui
// nomme son fichier, la ligne des tables prenant la forme canonique.
func (p *Provider) adoptOrCreateStep(projectID, domain string, existing []Key, obs Observed) (*engine.Step, error) {
	known := map[string]bool{}
	for _, k := range existing {
		known[p.Paths.OpenDKIMKeyFile(projectID, k.Domain, k.Selector)] = true
	}
	var candidates []diskKey
	for _, f := range p.filesOnDisk(projectID, domain) {
		if !known[p.Paths.OpenDKIMKeyFile(projectID, f.dir, f.selector)] {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		selector := nextSelector(p.Cfg.Mail.DKIMSelectorPrefix(), p.now(), existing,
			p.selectorsOnDisk(projectID, domain))
		return p.createStep(projectID, domain, selector, StateActive, obs), nil
	}
	resource := "mail/dkim/" + domain
	var chosen *diskKey
	if designated := p.designatedFile(domain); designated != "" {
		for i, c := range candidates {
			if p.Paths.OpenDKIMKeyFile(projectID, c.dir, c.selector) == designated {
				chosen = &candidates[i]
			}
		}
	}
	if chosen == nil && len(candidates) == 1 {
		chosen = &candidates[0]
	}
	if chosen == nil {
		files := make([]string, 0, len(candidates))
		for _, c := range candidates {
			files = append(files, p.Paths.OpenDKIMKeyFile(projectID, c.dir, c.selector))
		}
		s := engine.NewStep(resource, engine.Create,
			"clé DKIM de "+domain+" — plusieurs fichiers en place, absents du keystore")
		s.Critical = false
		s.Apply = func(context.Context) error {
			return fmt.Errorf("%s : aucune table ne désigne l'un d'eux, et ogs ne choisit pas la clé que "+
				"le DNS publie — ne garder que celle dont l'enregistrement est publié "+
				"(dig TXT <sélecteur>._domainkey.%s), puis relancer : elle sera reprise ; "+
				"aucune clé neuve n'est engendrée qui en écraserait une", strings.Join(files, ", "), domain)
		}
		return s, nil
	}
	sel, dir := chosen.selector, chosen.dir
	path := p.Paths.OpenDKIMKeyFile(projectID, dir, sel)
	refus := func(cause error) error {
		return fmt.Errorf("%s est en place mais absent du keystore, et refusé (%v) : il est peut-être la "+
			"clé que le DNS publie — ogs ne le reprend pas, et n'en engendre pas d'autre qui l'écraserait ; "+
			"la ligne que la KeyTable en place lui consacre est gardée. "+
			"Le remettre au régime d'ogs (fichier ordinaire à un seul nom, 0600, au compte %s) pour qu'il "+
			"soit repris, ou le retirer pour qu'une clé neuve soit engendrée, à publier",
			path, cause, p.Cfg.Mail.DKIMUser())
	}
	if _, _, err := p.readKeyFile(path, obs); err != nil {
		s := engine.NewStep(resource+"/"+sel, engine.Create,
			fmt.Sprintf("clé DKIM de %s (sélecteur %s) — fichier en place refusé", domain, sel))
		s.Critical = false
		s.Apply = func(context.Context) error { return refus(err) }
		return s, nil
	}
	summary := fmt.Sprintf("clé DKIM de %s (sélecteur %s) — reprise du fichier en place, absente du "+
		"keystore : c'est celle que le DNS publie", domain, sel)
	if others := len(candidates) - 1; others > 0 {
		summary += fmt.Sprintf(" (%d autre(s) fichier(s) laissé(s) en place)", others)
	}
	s := engine.NewStep(resource+"/"+sel, engine.Create, summary)
	s.Critical = false
	s.Diff = func() (string, error) { return "clé rescellée depuis " + path, nil }
	s.Apply = func(context.Context) error {
		// Relue à l'application, par le même descripteur qui la juge : le fichier
		// a pu changer depuis le plan.
		pemData, mtime, err := p.readKeyFile(path, obs)
		if err != nil {
			return refus(err)
		}
		k := Key{Domain: dir, Selector: sel, State: StateActive,
			Created: mtime, Activated: mtime, PEM: pemData}
		return p.sealKey(projectID, k)
	}
	s.Compensate = func(context.Context) error {
		return p.Store.DeleteSecret(projectID, SecretName(dir, sel))
	}
	return s, nil
}

// keyTableLine est une ligne de la KeyTable en place.
type keyTableLine struct{ name, domain, selector, file string }

// keyTableLines lit les lignes de la KeyTable en place ; absente ou illisible,
// elle n'en a aucune.
func (p *Provider) keyTableLines() []keyTableLine {
	data, err := os.ReadFile(p.Paths.OpenDKIMKeyTable())
	if err != nil {
		return nil
	}
	var out []keyTableLine
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		parts := strings.SplitN(fields[1], ":", 3)
		if len(parts) != 3 {
			continue
		}
		out = append(out, keyTableLine{name: fields[0], domain: model.MailDomain(parts[0]),
			selector: parts[1], file: parts[2]})
	}
	return out
}

// designatedFile rend le fichier de clé que la KeyTable en place désigne pour un
// domaine (forme canonique) : c'est la clé qu'OpenDKIM emploie, donc celle que
// le DNS publie.
func (p *Provider) designatedFile(domain string) string {
	for _, l := range p.keyTableLines() {
		if l.domain == domain {
			return l.file
		}
	}
	return ""
}

// keptEntry rend la ligne que la KeyTable en place consacre à un domaine signé
// dont le keystore ne connaît aucune clé active — fichier en place refusé au
// rescellement, ou plusieurs fichiers qu'ogs ne départage pas. Recalculer les
// tables sans elle faisait cesser de signer le domaine sans un mot ; la garder
// est la règle de D108 (2) : ce qu'ogs ne sait pas ouvrir reste en l'état. Elle
// n'est gardée que si son fichier est l'un de ceux du projet pour ce domaine,
// et présent.
func (p *Provider) keptEntry(projectID, domain string) (Entry, bool) {
	files := map[string]bool{}
	for _, f := range p.filesOnDisk(projectID, domain) {
		files[p.Paths.OpenDKIMKeyFile(projectID, f.dir, f.selector)] = true
	}
	for _, l := range p.keyTableLines() {
		if l.domain != domain || !files[l.file] {
			continue
		}
		return Entry{Project: projectID, Domain: domain, Selector: l.selector, KeyFile: l.file}, true
	}
	return Entry{}, false
}

// designation dit quelles tables en place désignent une clé : la KeyTable par
// son nom ou par son fichier, la SigningTable par son nom.
func (p *Provider) designation(k Key, path string) (keyTable, signingTable bool, err error) {
	name := k.Name()
	scan := func(table string, match func(fields []string) bool) (bool, error) {
		data, err := os.ReadFile(table)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && !strings.HasPrefix(fields[0], "#") && match(fields) {
				return true, nil
			}
		}
		return false, nil
	}
	keyTable, err = scan(p.Paths.OpenDKIMKeyTable(), func(f []string) bool {
		return strings.EqualFold(f[0], name) || strings.HasSuffix(f[1], ":"+path)
	})
	if err != nil {
		return false, false, err
	}
	signingTable, err = scan(p.Paths.OpenDKIMSigningTable(), func(f []string) bool {
		return strings.EqualFold(f[1], name)
	})
	return keyTable, signingTable, err
}

// designatedBy nomme la table en place qui désigne encore une clé, ou rend une
// chaîne vide.
func (p *Provider) designatedBy(k Key, path string) (string, error) {
	kt, st, err := p.designation(k, path)
	switch {
	case err != nil:
		return "", err
	case kt:
		return "la KeyTable (" + p.Paths.OpenDKIMKeyTable() + ")", nil
	case st:
		return "la SigningTable (" + p.Paths.OpenDKIMSigningTable() + ")", nil
	}
	return "", nil
}

// Undesignated rend les clés actives d'un projet — restreintes à un domaine —
// que les tables en place ne désignent pas encore, fichier présent. C'est ce
// que laisse une bascule scellée dont le recalcul des tables a échoué :
// « ogs mail dkim activate » la rejoue au lieu de répondre qu'aucune clé n'est
// préparée (D108).
//
// OpenDKIM absent ou désactivé, les tables ne désignent rien par construction :
// il n'y a rien à rejouer.
func (p *Provider) Undesignated(proj *model.Project, only string, obs Observed) ([]Key, error) {
	if !obs.Present {
		return nil, nil
	}
	domains, err := selectDomains(proj, only)
	if err != nil {
		return nil, err
	}
	keys, err := p.loadKeys(proj.ID())
	if err != nil {
		return nil, err
	}
	grouped := byDomain(keys)
	var out []Key
	for _, d := range domains {
		k, ok := find(grouped[d], StateActive)
		if !ok {
			continue
		}
		path := p.Paths.OpenDKIMKeyFile(proj.ID(), k.Domain, k.Selector)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		kt, st, err := p.designation(k, path)
		if err != nil {
			return nil, err
		}
		if !kt || !st {
			out = append(out, k)
		}
	}
	return out, nil
}

// releasable dit si le fichier d'une clé peut partir : les tables en place ne la
// désignent plus, et OpenDKIM les a relues depuis leur dernière écriture. Une
// clé encore désignée qui disparaît ne laisse pas partir le courrier sans
// signature : OpenDKIM rend une erreur temporaire, et le MTA garde en file tout
// le courrier du domaine (D108).
//
// Le rechargement se constate, il ne se suppose pas : l'étape vide les
// intentions en attente (FlushReloads), mais rien ne lui dit qu'il a réussi, ni
// qu'un apply précédent n'a pas laissé OpenDKIM sur des tables plus anciennes.
// Sans rechargement réussi depuis la dernière écriture d'une table dans ce
// processus, il est donc refait ici.
func (p *Provider) releasable(ctx context.Context, k Key, path string) error {
	where, err := p.designatedBy(k, path)
	if err != nil {
		return fmt.Errorf("%s reste en place : tables illisibles (%v)", path, err)
	}
	if where != "" {
		return fmt.Errorf("%s désigne encore la clé %s : son fichier reste en place jusqu'à ce qu'elle "+
			"ne la désigne plus — OpenDKIM rendrait une erreur temporaire et le MTA garderait en file "+
			"tout le courrier de %s", where, k.Name(), model.MailDomain(k.Domain))
	}
	if p.reloaded && p.loadedAt == p.written {
		return nil
	}
	if err := p.Reload(ctx); err != nil {
		return fmt.Errorf("%s reste en place : OpenDKIM n'a pas relu les tables (%v), et garde en mémoire "+
			"celles qui désignent la clé", path, err)
	}
	return nil
}
