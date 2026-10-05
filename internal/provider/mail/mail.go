// Package mail tient la signature DKIM du courrier des projets (D69) : une clé
// par domaine d'envoi, scellée dans le keystore et écrite pour OpenDKIM, et les
// deux tables qui les désignent, dérivées du parc entier.
//
// ogs n'installe ni OpenDKIM ni le MTA, ne touche ni à opendkim.conf ni au
// branchement du milter, et ne publie rien dans le DNS : il émet
// l'enregistrement à publier, et vérifie ce qui l'a été.
package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/removal"
	"leblanc.io/open-go-server/internal/provider/system"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/store"
)

// ReloadTarget est l'intention de rechargement d'OpenDKIM.
const ReloadTarget = "mail:opendkim"

// Provider tient les clés DKIM et les tables d'OpenDKIM.
type Provider struct {
	Paths config.Paths
	Cfg   *config.Config
	Exec  exec.Executor
	Store *store.Store
	Keys  *secret.Keystore

	// Force autorise l'écrasement d'un fichier modifié à la main (§6.4).
	Force bool

	Now func() time.Time
	// LookupTXT résout un enregistrement TXT ; nil, le résolveur du système.
	LookupTXT func(ctx context.Context, name string) ([]string, error)
	// Generate engendre une clé privée en PEM ; nil, GenerateKey.
	Generate func(bits int) (string, error)

	// written compte les tables écrites par ce processus, et loadedAt vaut
	// written au dernier rechargement réussi d'OpenDKIM (reloaded) : un fichier
	// de clé ne part qu'une fois les tables qui la désignaient relues (D108).
	written  int
	loadedAt int
	reloaded bool
}

// Name identifie le provider.
func (p *Provider) Name() string { return "mail" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provider) generate() (string, error) {
	bits := p.Cfg.Mail.DKIMKeyBits()
	if p.Generate != nil {
		return p.Generate(bits)
	}
	return GenerateKey(bits)
}

func (p *Provider) simulated() bool { return p.Exec != nil && p.Exec.Simulated() }

// Observed est ce qu'ogs a trouvé sur la machine.
type Observed struct {
	// Present dit si ogs gère les clés : OpenDKIM est là, ou la configuration
	// l'impose.
	Present bool
	// UID et GID sont ceux du compte d'OpenDKIM, propriétaire des clés. OwnerOK
	// faux, le compte est introuvable : une clé écrite pour root serait refusée
	// par OpenDKIM, qui ne pourrait pas la lire.
	UID, GID int
	OwnerOK  bool
}

// Observe inspecte la machine. Le critère de présence est opendkim.conf, non un
// répertoire : c'est le fichier qu'OpenDKIM lit, et des tables que personne ne
// désigne ne signeraient rien. En mode simulé, OpenDKIM est réputé présent : le
// mode d'essai doit produire un parc complet sur une machine où rien n'est
// installé.
func (p *Provider) Observe(context.Context) (Observed, error) {
	obs := Observed{UID: -1, GID: -1}
	if v, set := p.Cfg.Mail.DKIMEnabled(); set {
		obs.Present = v
	} else if p.simulated() {
		obs.Present = true
	} else {
		info, err := os.Stat(p.Paths.OpenDKIMConf())
		obs.Present = err == nil && info.Mode().IsRegular()
	}
	// Le compte est cherché même quand ogs ne gère pas les clés : les tables
	// qu'opendkim.conf désigne encore, signature désactivée, sont tenues vides
	// (PlanTables, D108), et écrites pour root, OpenDKIM ne les lirait plus.
	if u, err := user.Lookup(p.Cfg.Mail.DKIMUser()); err == nil {
		uid, err1 := strconv.Atoi(u.Uid)
		gid, err2 := strconv.Atoi(u.Gid)
		if err1 == nil && err2 == nil {
			obs.UID, obs.GID, obs.OwnerOK = uid, gid, true
		}
	}
	return obs, nil
}

// keyOwner est le propriétaire des clés, et dirOwner celui des répertoires qui
// les portent — root, groupe d'OpenDKIM, pour que le démon traverse sans pouvoir
// rien y créer. En mode simulé, rien n'est changé : le mode d'essai tourne sans
// privilèges.
func (p *Provider) keyOwner(obs Observed) fs.Owner {
	if p.simulated() || !obs.OwnerOK {
		return fs.KeepOwner
	}
	return fs.Owner{UID: obs.UID, GID: obs.GID}
}

func (p *Provider) dirOwner(obs Observed) fs.Owner {
	if p.simulated() || !obs.OwnerOK {
		return fs.KeepOwner
	}
	return fs.Owner{UID: 0, GID: obs.GID}
}

// Plan calcule les étapes d'un projet : créer la clé d'un domaine qui n'en a
// pas — ou reprendre celle qui est en place —, réécrire un fichier disparu
// depuis le keystore.
//
// Toutes les étapes sont **non critiques** : une clé qui n'est pas posée ne doit
// pas défaire un site par ailleurs fonctionnel. Pour la même raison, des clés
// illisibles — keystore indisponible, clé maître régénérée — ne font pas échouer
// le plan : une étape en échec nomme la cause (D108).
//
// L'expiration d'une clé remplacée n'est pas ici : elle se fait au plan global,
// après les tables, et seulement quand elles ne la désignent plus (D108).
func (p *Provider) Plan(_ context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	if !obs.Present {
		return nil, nil
	}
	keys, err := p.loadKeys(proj.ID())
	if err != nil {
		return []*engine.Step{p.unreadableStep("mail/dkim/"+proj.ID(), proj.ID(),
			"aucune clé n'est posée ni réécrite", err)}, nil
	}
	domains := proj.DKIMDomains()
	if len(domains) > 0 && !obs.OwnerOK && !p.simulated() {
		// Écrire une clé pour root la rendrait illisible à OpenDKIM : mieux vaut
		// ne rien écrire, et le dire.
		s := engine.NewStep("mail/dkim/"+proj.ID(), engine.Create,
			"clés DKIM de "+proj.ID()+" — compte "+p.Cfg.Mail.DKIMUser()+" introuvable")
		s.Critical = false
		s.Apply = func(context.Context) error {
			return fmt.Errorf("le compte %s n'existe pas : OpenDKIM ne pourrait pas lire ses clés "+
				"(régler mail.dkim.user dans /etc/ogs/config.yaml)", p.Cfg.Mail.DKIMUser())
		}
		return []*engine.Step{s}, nil
	}
	grouped := byDomain(keys)
	var steps []*engine.Step
	for _, domain := range domains {
		existing := grouped[domain]
		_, hasActive := find(existing, StateActive)
		_, hasPending := find(existing, StatePending)
		if !hasActive && !hasPending {
			s, err := p.adoptOrCreateStep(proj.ID(), domain, existing, obs)
			if err != nil {
				return nil, err
			}
			steps = append(steps, s)
		}
		for _, k := range existing {
			if k.State == StateRetired && p.expired(k) {
				// Le plan global la retire, après les tables (D108).
				continue
			}
			s, err := p.keyFileStep(proj.ID(), k, obs)
			if err != nil {
				return nil, err
			}
			steps = append(steps, s)
		}
	}
	// Le fichier d'un domaine qui n'est plus signé n'est pas retiré ici : les
	// tables le désignent encore, et OpenDKIM retiendrait le courrier du domaine
	// jusqu'à leur rechargement. C'est le plan global qui le retire, une fois les
	// tables rechargées (PlanTables).
	return steps, nil
}

// expired dit si la rétention d'une clé retirée est écoulée.
func (p *Provider) expired(k Key) bool {
	return !k.Retired.IsZero() && p.now().Sub(k.Retired) >= p.Cfg.Mail.DKIMRetention()
}

// createStep engendre une clé, la scelle et l'écrit. La génération appartient à
// l'Apply : « plan » n'écrit rien, et une clé tirée au plan serait perdue si
// l'exploitant renonçait à l'apply.
func (p *Provider) createStep(projectID, domain, selector, state string, obs Observed) *engine.Step {
	what := "clé DKIM"
	if state == StatePending {
		what = "clé DKIM préparée"
	}
	s := engine.NewStep("mail/dkim/"+domain+"/"+selector, engine.Create,
		fmt.Sprintf("%s de %s (sélecteur %s)", what, domain, selector))
	s.Critical = false
	path := p.Paths.OpenDKIMKeyFile(projectID, domain, selector)
	s.Diff = func() (string, error) {
		return fmt.Sprintf("nouvelle clé RSA-%d écrite en %s", p.Cfg.Mail.DKIMKeyBits(), path), nil
	}
	s.Apply = func(context.Context) error {
		pemData, err := p.generate()
		if err != nil {
			return err
		}
		k := Key{Domain: domain, Selector: selector, State: state, Created: p.now().UTC(), PEM: pemData}
		if state == StateActive {
			k.Activated = k.Created
		}
		// Scellée d'abord : le keystore est la source de vérité, et un fichier
		// écrit sans clé scellée serait une clé que rien ne saurait réécrire.
		if err := p.sealKey(projectID, k); err != nil {
			return err
		}
		return p.writeKey(path, k, obs)
	}
	s.Compensate = func(context.Context) error {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return p.Store.DeleteSecret(projectID, SecretName(domain, selector))
	}
	return s
}

// writeKey écrit le fichier d'une clé et les répertoires qui le portent.
func (p *Provider) writeKey(path string, k Key, obs Observed) error {
	if err := p.ensureKeyDirs(filepath.Dir(path), obs); err != nil {
		return err
	}
	// 0600 : sous RequireSafeKeys, défaut d'OpenDKIM, une clé lisible au-delà de
	// son propriétaire est refusée — le régime 0640 d'un fragment Telegraf ne
	// convient pas ici.
	return fs.WriteAtomic(path, []byte(k.PEM), 0o600, p.keyOwner(obs))
}

// ensureKeyDirs pose la racine des clés, le répertoire du projet et celui du
// domaine, en 0750 root:<groupe d'OpenDKIM> : le démon traverse, personne
// d'autre n'énumère.
func (p *Provider) ensureKeyDirs(domainDir string, obs Observed) error {
	root := p.Paths.OpenDKIMKeysDir()
	if err := p.ensureOpenDKIMDir(obs); err != nil {
		return err
	}
	for _, dir := range []string{root, filepath.Dir(domainDir), domainDir} {
		if err := fs.EnsureDir(dir, 0o750, p.dirOwner(obs)); err != nil {
			return err
		}
	}
	return nil
}

// Aucune étape de clé ne demande de rechargement : OpenDKIM relit le fichier
// d'une clé à chaque message qu'il signe, et seules les tables disent quelle clé
// signer. Recharger ici ferait même valider par « opendkim -n » une
// configuration qui désigne des tables pas encore écrites — le plan global les
// pose après les projets — et avertir à tort au premier apply.

// OpenDKIMDirMode est le mode de /etc/opendkim et des répertoires qu'ogs y pose.
// C'est celui qu'« ogs harden » exige (hardening.Defaults) : EnsureDir repose le
// mode d'un répertoire existant, et deux juges en désaccord feraient osciller
// l'un et l'autre à chaque passage — l'apply rouvrant ce que harden ferme.
const OpenDKIMDirMode = 0o750

// ensureOpenDKIMDir pose /etc/opendkim en 0750 root:<groupe d'OpenDKIM>.
func (p *Provider) ensureOpenDKIMDir(obs Observed) error {
	return fs.EnsureDir(filepath.Dir(p.Paths.OpenDKIMKeysDir()), OpenDKIMDirMode, p.dirOwner(obs))
}

// keyFileStep réconcilie le fichier d'une clé scellée. Le fichier ne porte pas
// d'en-tête : un PEM n'a pas de syntaxe de commentaire sur laquelle compter. La
// dérive se mesure donc contre la clé scellée, qui fait foi — un fichier absent
// est réécrit, jamais régénéré : une clé neuve exigerait de republier
// l'enregistrement chez le client.
func (p *Provider) keyFileStep(projectID string, k Key, obs Observed) (*engine.Step, error) {
	path := p.Paths.OpenDKIMKeyFile(projectID, k.Domain, k.Selector)
	resource := "mail/dkim/" + k.Domain + "/" + k.Selector
	label := fmt.Sprintf("clé DKIM de %s (sélecteur %s, %s)", k.Domain, k.Selector, stateLabel(k.State))
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if os.IsNotExist(err) {
		s := engine.NewStep(resource, engine.Create, label+" — réécrite depuis le keystore")
		s.Critical = false
		s.Diff = func() (string, error) { return "fichier absent : " + path, nil }
		s.Apply = func(context.Context) error { return p.writeKey(path, k, obs) }
		s.Compensate = func(context.Context) error { return os.Remove(path) }
		return s, nil
	}
	if string(current) != k.PEM {
		if !p.Force {
			s := engine.NewStep(resource, engine.Update, label+" — fichier modifié hors ogs")
			s.Critical = false
			s.Apply = func(context.Context) error {
				return fmt.Errorf("%s ne correspond plus à la clé scellée : relancer avec --force pour "+
					"le réécrire (l'original sera sauvegardé en %s)", path, p.backupPattern(path))
			}
			return s, nil
		}
		s := engine.NewStep(resource, engine.Update,
			label+" — écrasement forcé (sauvegarde "+p.backupPattern(path)+")")
		s.Critical = false
		s.Diff = func() (string, error) { return "contenu différent de la clé scellée : " + path, nil }
		s.Apply = func(context.Context) error {
			if _, err := fs.BackupTo(path, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
				return err
			}
			return p.writeKey(path, k, obs)
		}
		s.Compensate = func(context.Context) error {
			return fs.WriteAtomic(path, current, 0o600, p.keyOwner(obs))
		}
		return s, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if drift := p.permDrift(info, obs); drift != "" {
		s := engine.NewStep(resource, engine.Update, label+" — "+drift)
		s.Critical = false
		mode := info.Mode().Perm()
		s.Apply = func(context.Context) error { return p.writeKey(path, k, obs) }
		s.Compensate = func(context.Context) error { return os.Chmod(path, mode) }
		return s, nil
	}
	return engine.NewStep(resource, engine.NoOp, label), nil
}

// permDrift décrit l'écart de droits d'une clé, ou rend une chaîne vide.
func (p *Provider) permDrift(info os.FileInfo, obs Observed) string {
	if perm := info.Mode().Perm(); perm != 0o600 {
		return fmt.Sprintf("mode %04o au lieu de 0600", perm)
	}
	if p.simulated() || !obs.OwnerOK {
		return ""
	}
	if uid, gid, ok := system.OwnerOf(info); ok && (uid != obs.UID || gid != obs.GID) {
		return fmt.Sprintf("propriétaire %d:%d au lieu de %s", uid, gid, p.Cfg.Mail.DKIMUser())
	}
	return ""
}

// expireStep retire une clé remplacée dont la rétention est écoulée : son
// fichier et son secret. Son enregistrement DNS, lui, est à retirer à la main —
// « doctor » le signale tant qu'il est publié.
//
// Le fichier suit le régime de son écriture : s'il ne correspond plus à la clé
// scellée, il porte une clé que quelqu'un y a mise — une clé privée, que rien ne
// permettrait de retrouver —, et l'expiration attend --force, copie faite. La clé
// scellée reste alors en place : le refus ne défait rien à moitié.
func (p *Provider) expireStep(projectID string, k Key) (*engine.Step, error) {
	path := p.Paths.OpenDKIMKeyFile(projectID, k.Domain, k.Selector)
	resource := "mail/dkim/" + k.Domain + "/" + k.Selector
	summary := fmt.Sprintf("clé DKIM remplacée de %s (sélecteur %s) — rétention écoulée, "+
		"enregistrement %s à retirer du DNS", k.Domain, k.Selector, k.Name())
	// refus rend le refus que vaut l'état du fichier, ou nil s'il peut partir.
	refus := func(state fs.State) error {
		switch {
		case state == fs.StateForeign:
			return fmt.Errorf("%s n'est pas un fichier ordinaire : ogs ne le retire pas, "+
				"et la clé reste scellée — le retirer à la main", path)
		case state == fs.StateHandEdited && !p.Force:
			return fmt.Errorf("%s ne correspond plus à la clé scellée : relancer avec --force pour le "+
				"retirer (l'original sera sauvegardé en %s) ; la clé reste scellée d'ici là",
				path, p.backupPattern(path))
		}
		return nil
	}
	state, err := keyFileState(path, k)
	if err != nil {
		return nil, err
	}
	if err := refus(state); err != nil {
		label := "fichier modifié hors ogs"
		if state == fs.StateForeign {
			label = "pas un fichier ordinaire"
		}
		s := engine.NewStep(resource, engine.Delete, summary+" — "+label)
		s.Critical = false
		s.Apply = func(context.Context) error { return err }
		return s, nil
	}
	if state == fs.StateHandEdited {
		summary += " — retrait forcé (sauvegarde " + p.backupPattern(path) + ")"
	}
	s := engine.NewStep(resource, engine.Delete, summary)
	s.Critical = false
	// Comme le retrait d'un domaine qui n'est plus signé : après le rechargement
	// des tables, et seulement si elles ne désignent plus la clé (D108).
	s.FlushReloads = true
	var previous []byte
	var saved *sealed
	s.Apply = func(ctx context.Context) error {
		// L'état est relu, comme pour tout retrait (removal.File) : une clé intacte
		// au plan, remplacée avant la confirmation, partirait sans copie.
		state, err := keyFileState(path, k)
		if err != nil {
			return err
		}
		if err := refus(state); err != nil {
			return err
		}
		if err := p.releasable(ctx, k, path); err != nil {
			return err
		}
		if saved, err = p.snapshot(projectID, k); err != nil {
			return err
		}
		if data, err := os.ReadFile(path); err == nil {
			previous = data
		}
		if state == fs.StateHandEdited {
			if _, err := fs.BackupTo(path, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return p.Store.DeleteSecret(projectID, SecretName(k.Domain, k.Selector))
	}
	s.Compensate = func(context.Context) error {
		if err := p.restore(projectID, k, saved); err != nil {
			return err
		}
		if previous == nil {
			return nil
		}
		return fs.WriteAtomic(path, previous, 0o600, fs.KeepOwner)
	}
	return s, nil
}

// removeFileStep retire le fichier d'une clé dont le domaine n'est plus signé.
// La clé reste scellée : réactiver la signature doit retrouver l'enregistrement
// déjà publié chez le client, non en exiger un nouveau. Un fichier qui ne
// correspond plus à la clé scellée ne part que sous --force, copie faite : c'est
// la clé privée de quelqu'un, et le keystore ne la connaît pas.
//
// L'étape vide les rechargements en attente avant de s'exécuter : les tables
// qui désignaient la clé ont été réécrites juste avant, mais OpenDKIM garde en
// mémoire celles qu'il a chargées, et retirer le fichier d'abord lui ferait
// retenir le courrier du domaine jusqu'au rechargement. Et elle ne s'exécute
// que si ces tables ne la désignent plus et qu'OpenDKIM les a relues
// (releasable, D108) : vider la file ne dit pas que le rechargement a réussi, ni
// qu'une table retouchée a bien été réécrite.
func (p *Provider) removeFileStep(projectID string, k Key) (*engine.Step, error) {
	path := p.Paths.OpenDKIMKeyFile(projectID, k.Domain, k.Selector)
	s, err := removal.File{
		Resource: "mail/dkim/" + k.Domain + "/" + k.Selector,
		Summary: fmt.Sprintf("clé DKIM de %s (sélecteur %s) — domaine qui n'est plus signé, clé conservée "+
			"dans le keystore", k.Domain, k.Selector),
		Path:         path,
		Inspect:      func() (fs.State, error) { return keyFileState(path, k) },
		Force:        p.Force,
		Backups:      p.Paths.BackupsDir(),
		Now:          p.now,
		FlushReloads: true,
		NonCritical:  true,
	}.Step()
	if err != nil || s == nil || !s.FlushReloads {
		// Un refus ne retire rien : il n'a rien à attendre.
		return s, err
	}
	remove := s.Apply
	s.Apply = func(ctx context.Context) error {
		if err := p.releasable(ctx, k, path); err != nil {
			return err
		}
		return remove(ctx)
	}
	return s, nil
}

// keyFileState juge le fichier d'une clé contre la clé scellée, qui fait foi :
// un PEM ne peut pas porter l'en-tête d'ogs. Un fichier conforme se retire ; un
// fichier qui en diffère a été retouché ; ce qui n'est pas un fichier ordinaire
// n'est pas à ogs.
func keyFileState(path string, k Key) (fs.State, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return fs.StateAbsent, nil
	}
	if err != nil {
		return fs.StateAbsent, err
	}
	if !info.Mode().IsRegular() {
		return fs.StateForeign, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fs.StateAbsent, err
	}
	if string(data) == k.PEM {
		return fs.StateCurrent, nil
	}
	return fs.StateHandEdited, nil
}

// backupPattern nomme l'emplacement de la copie d'un écrasement forcé.
func (p *Provider) backupPattern(path string) string {
	return filepath.Join(p.Paths.BackupsDir(), filepath.Base(path)) + ".ogs-bak-<horodatage>"
}

func stateLabel(state string) string {
	switch state {
	case StateActive:
		return "active"
	case StatePending:
		return "préparée"
	case StateRetired:
		return "remplacée"
	}
	return state
}

// Records rend les enregistrements DNS des clés d'un projet : l'active, la
// préparée s'il y en a une, et les remplacées encore conservées — celles-là sont
// à retirer une fois leur rétention écoulée.
func (p *Provider) Records(proj *model.Project) ([]Record, error) {
	keys, err := p.loadKeys(proj.ID())
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, k := range keys {
		r, err := NewRecord(k)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// ProjectKeys expose les clés scellées d'un projet.
func (p *Provider) ProjectKeys(proj *model.Project) ([]Key, error) { return p.loadKeys(proj.ID()) }

// RemoveProject retire les fichiers de clés d'un projet supprimé. Les clés
// restent scellées, et partent dans l'archive avec les autres secrets du projet.
//
// Comme tout retrait de clé (D108), il n'a lieu que si les tables en place ne
// désignent plus aucune d'elles et qu'OpenDKIM les a relues : la suppression
// recalcule les tables juste avant, mais un recalcul en échec n'y est qu'un
// avertissement, et retirer alors les fichiers ferait garder en file tout le
// courrier des domaines du projet.
func (p *Provider) RemoveProject(id string) error {
	dir := filepath.Join(p.Paths.OpenDKIMKeysDir(), id)
	domains, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, d := range domains {
		if !d.IsDir() {
			continue
		}
		for sel := range p.selectorsOnDisk(id, d.Name()) {
			k := Key{Domain: d.Name(), Selector: sel}
			path := p.Paths.OpenDKIMKeyFile(id, d.Name(), sel)
			where, err := p.designatedBy(k, path)
			if err != nil {
				return fmt.Errorf("%s reste en place : tables illisibles (%v)", dir, err)
			}
			if where != "" {
				return fmt.Errorf("%s désigne encore la clé %s : les clés de %s restent en place (%s) — "+
					"OpenDKIM garderait en file le courrier de %s ; relancer « ogs apply » une fois la "+
					"table recalculée, puis retirer le répertoire", where, k.Name(), id, dir, d.Name())
			}
		}
	}
	if !p.reloaded || p.loadedAt != p.written {
		// La suppression a franchi son point de non-retour : le rechargement se
		// fait jusqu'au bout, interruption comprise.
		if err := p.Reload(context.Background()); err != nil {
			return fmt.Errorf("les clés de %s restent en place (%s) : OpenDKIM n'a pas relu les tables (%v)",
				id, dir, err)
		}
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ErrNothingPending signale qu'aucune clé n'attend d'activation.
var ErrNothingPending = errors.New("aucune clé DKIM préparée")

// ErrAlreadyPending signale qu'une rotation est déjà en cours.
var ErrAlreadyPending = errors.New("une clé DKIM est déjà préparée")

// selectDomains restreint les domaines d'un projet à celui demandé.
func selectDomains(proj *model.Project, only string) ([]string, error) {
	domains := proj.DKIMDomains()
	if len(domains) == 0 {
		return nil, fmt.Errorf("le projet %s ne demande pas de signature DKIM "+
			"(ogs mail dkim enable %s)", proj.ID(), proj.ID())
	}
	if only == "" {
		return domains, nil
	}
	for _, d := range domains {
		if d == model.MailDomain(only) {
			return []string{d}, nil
		}
	}
	return nil, fmt.Errorf("le domaine %s n'est pas signé par le projet %s (domaines signés : %s)",
		only, proj.ID(), strings.Join(domains, ", "))
}

// Rotate prépare une clé sous un nouveau sélecteur, sans rien changer à la
// signature : la bascule est l'affaire d'Activate, une fois l'enregistrement
// publié.
func (p *Provider) Rotate(ctx context.Context, proj *model.Project, only string, obs Observed) ([]*engine.Step, error) {
	if !obs.Present {
		return nil, errors.New("OpenDKIM n'est pas installé (opendkim.conf absent) — " +
			"forcer par mail.dkim.enabled dans /etc/ogs/config.yaml")
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
	var steps []*engine.Step
	for _, d := range domains {
		if k, ok := find(grouped[d], StatePending); ok {
			return nil, fmt.Errorf("%w pour %s (sélecteur %s) : la publier puis « ogs mail dkim activate %s »",
				ErrAlreadyPending, d, k.Selector, proj.ID())
		}
		if _, ok := find(grouped[d], StateActive); !ok {
			return nil, fmt.Errorf("%s n'a pas encore de clé active : « ogs apply %s » la crée", d, proj.ID())
		}
		selector := nextSelector(p.Cfg.Mail.DKIMSelectorPrefix(), p.now(), grouped[d],
			p.selectorsOnDisk(proj.ID(), d))
		steps = append(steps, p.createStep(proj.ID(), d, selector, StatePending, obs))
	}
	return steps, nil
}

// Pending énumère les clés préparées d'un projet, restreintes à un domaine.
func (p *Provider) Pending(proj *model.Project, only string) ([]Key, error) {
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
		if k, ok := find(grouped[d], StatePending); ok {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w pour %s : « ogs mail dkim rotate %s » en prépare une",
			ErrNothingPending, proj.ID(), proj.ID())
	}
	return out, nil
}

// Activate bascule chaque clé préparée en clé active, et retire celle qu'elle
// remplace. La vérification de l'enregistrement publié est l'affaire de
// l'appelant, qui décide de passer outre : basculer vers une clé non publiée
// casserait la signature de tout envoi.
func (p *Provider) Activate(proj *model.Project, pending []Key) *engine.Step {
	names := make([]string, 0, len(pending))
	for _, k := range pending {
		names = append(names, k.Domain+" → "+k.Selector)
	}
	s := engine.NewStep("mail/dkim/"+proj.ID()+"/activation", engine.Update,
		"activation des clés DKIM préparées ("+strings.Join(names, ", ")+")")
	type change struct {
		key   Key
		saved *sealed
	}
	var done []change
	s.Apply = func(context.Context) error {
		keys, err := p.loadKeys(proj.ID())
		if err != nil {
			return err
		}
		grouped := byDomain(keys)
		now := p.now().UTC()
		for _, next := range pending {
			for _, k := range grouped[next.Domain] {
				var updated Key
				switch {
				case k.Selector == next.Selector:
					updated = k
					updated.State, updated.Activated = StateActive, now
				case k.State == StateActive:
					updated = k
					updated.State, updated.Retired = StateRetired, now
				default:
					continue
				}
				saved, err := p.snapshot(proj.ID(), k)
				if err != nil {
					return err
				}
				if err := p.sealKey(proj.ID(), updated); err != nil {
					return err
				}
				done = append(done, change{key: k, saved: saved})
			}
		}
		return nil
	}
	s.Compensate = func(context.Context) error {
		var errs []error
		for i := len(done) - 1; i >= 0; i-- {
			errs = append(errs, p.restore(proj.ID(), done[i].key, done[i].saved))
		}
		return errors.Join(errs...)
	}
	return s
}
