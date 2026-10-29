// Package cron génère les tâches planifiées d'un projet.
package cron

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/removal"
	"leblanc.io/open-go-server/internal/template"
	"leblanc.io/open-go-server/internal/validate"
)

// Provider réconcilie le fichier cron d'un projet.
type Provider struct {
	Paths     config.Paths
	Templates *template.Set
	// Owner résout le propriétaire des fichiers de journal.
	Owner func(proj *model.Project) (uid, gid int, err error)
	Now   func() time.Time
	// Force autorise l'écrasement d'un fichier qu'ogs n'a pas écrit, après
	// sauvegarde horodatée (§6.4). Sans lui, le refus n'aurait aucune porte de
	// sortie et l'apply du projet resterait bloqué.
	Force bool
	// Simulated suspend le contrôle d'installation d'une version PHP fixée par
	// une tâche : le mode préfixé ne pose aucun binaire.
	Simulated bool
	// PHPVersions sont les versions qu'une tâche peut fixer (« php.versions »,
	// D125) ; nulles, la liste livrée.
	PHPVersions []string
}

// refusStep produit l'étape qui refuse d'écraser un fichier qu'ogs n'a pas
// écrit. Le message nomme la porte de sortie — elle existe désormais — et
// distingue les deux causes : un fichier « non géré » n'a jamais porté la marque
// d'ogs, c'est ce que rencontre un site adopté à son premier apply ; un fichier
// « modifié hors ogs » a été retouché après avoir été généré.
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
func (p *Provider) Name() string { return "cron" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// FileName renvoie le nom du fichier cron d'un projet. Un point dans le nom
// ferait ignorer le fichier par cron, d'où le préfixe et l'identifiant seuls.
func FileName(id string) string { return "ogs-" + id }

// Path renvoie le chemin du fichier cron.
func (p *Provider) Path(id string) string {
	return filepath.Join(p.Paths.CronDir(), FileName(id))
}

// job est une tâche prête à être écrite.
type job struct {
	Name     string
	Schedule string
	Command  string
	Log      string
	Enabled  bool
	LockPath string
	// Legacy est le nom nu résolu dans le répertoire de travail par
	// compatibilité (D101), vide sinon.
	Legacy string
	// LegacyCause dit pourquoi le répertoire de travail n'a pas pu être vérifié,
	// vide quand le fichier homonyme y a été trouvé.
	LegacyCause string
}

// Build calcule le contenu attendu du fichier cron.
func (p *Provider) Build(proj *model.Project) ([]byte, []job, error) {
	jobs, err := p.jobs(proj)
	if err != nil {
		return nil, nil, err
	}
	if len(jobs) == 0 {
		return nil, nil, nil
	}
	// Les tâches sont échappées sur une copie, au seul usage du gabarit : « ogs
	// cron run » passe job.Command à /bin/sh sans intermédiaire, et le shell, lui,
	// ne défait pas la contre-oblique entre apostrophes — l'essai rendrait alors
	// un résultat différent de l'exécution réelle. Les chemins de journal
	// (créés par Plan) doivent eux aussi rester vrais.
	rendus := make([]job, len(jobs))
	for i, j := range jobs {
		rendus[i] = j
		rendus[i].Command = echapperPourcent(j.Command)
		rendus[i].Log = echapperPourcent(j.Log)
	}
	body, err := p.Templates.Render("cron/project", map[string]any{
		"User": proj.Spec.User.Name,
		"Jobs": rendus,
	})
	if err != nil {
		return nil, nil, err
	}
	content := fs.Render(fs.Header{
		Project:   proj.ID(),
		Resource:  "cron/" + proj.ID(),
		Generated: p.now(),
		Template:  p.Templates.OverridePath("cron/project"),
	}, body, "#")
	return content, jobs, nil
}

// echapperPourcent protège les « % » d'une ligne de cron. Dans un fichier de
// /etc/cron.d, « % » n'est pas un caractère ordinaire : cron le remplace par un
// saut de ligne et envoie sur l'entrée standard tout ce qui suit le premier. La
// commande serait tronquée et la redirection vers logs/, écrite après elle par
// le gabarit, perdue avec la coupure — la tâche échouerait indéfiniment sans
// laisser la moindre trace. C'est la même raison qui fait refuser un retour à la
// ligne dans « command ». Un « % » déjà échappé à la main (l'idiome connu
// « date +\%F ») est laissé tel quel : le redoubler écrirait « \\% », que cron
// rendrait comme une contre-oblique suivie d'un saut de ligne.
func echapperPourcent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			b.WriteByte('\\')
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '%':
			b.WriteString(`\%`)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// jobs traduit les déclarations en lignes de cron.
func (p *Provider) jobs(proj *model.Project) ([]job, error) {
	runDir := p.Paths.ProjectRunDir(proj.ID())
	logDir := proj.Spec.Layout.LogDir
	out := make([]job, 0, len(proj.Spec.Crons))
	for _, decl := range proj.Spec.Crons {
		if decl.Name == "" {
			return nil, fmt.Errorf("projet %s : tâche sans nom", proj.ID())
		}
		command, legacy, cause, err := p.command(proj, decl)
		if err != nil {
			return nil, err
		}
		logFile := decl.Log
		if logFile == "" {
			logFile = "cron-" + decl.Name + ".log"
		}
		// Toujours un nom, jamais un chemin (D71) : la validation l'impose, et le
		// fichier est pré-créé par root dans le répertoire des journaux du projet.
		logFile = filepath.Join(logDir, filepath.Base(logFile))
		enabled := decl.Enabled == nil || *decl.Enabled
		out = append(out, job{
			Name: decl.Name, Schedule: strings.TrimSpace(decl.Schedule),
			Command: command, Log: logFile, Enabled: enabled,
			LockPath:    filepath.Join(runDir, decl.Name+".lock"),
			Legacy:      legacy,
			LegacyCause: cause,
		})
	}
	return out, nil
}

// command construit la commande complète : répertoire de travail, verrou,
// interpréteur. Le verrou est posé par défaut — une tâche qui se recouvre est une
// cause fréquente d'incident, et l'oubli est plus probable que le besoin inverse.
//
// La ligne commence par un « cd » : cron lance la tâche dans le home, et une
// commande qui lit un chemin relatif — un fichier de configuration, un
// répertoire var/ — le chercherait là. Le répertoire de travail ne servait
// auparavant qu'à compléter une commande relative, ce qui le rendait trompeur.
// Le « cd » précède le verrou : flock exécute une commande, pas une ligne de
// shell. S'il échoue, la tâche ne part pas, et le journal dit pourquoi.
func (p *Provider) command(proj *model.Project, decl model.CronJob) (line, legacy, legacyCause string, err error) {
	if strings.ContainsAny(decl.Command, "\n\r") {
		return "", "", "", fmt.Errorf("tâche %s : la commande ne peut pas contenir de retour à la ligne", decl.Name)
	}
	// Les affectations de tête — « APP_ENV=prod bin/console … » — sont mises à
	// part : placées après flock, qui exécute une commande et non une ligne de
	// shell, elles étaient prises pour le nom du programme ; placées devant, elles
	// atteignent la commande à travers lui. Seules les affectations sans
	// guillemet ni échappement sont reconnues : au-delà, il faudrait analyser le
	// shell (§20).
	command := strings.TrimSpace(decl.Command)
	var assigns []string
	for {
		fields := strings.Fields(command)
		if len(fields) == 0 || !isAssignment(fields[0]) {
			break
		}
		assigns = append(assigns, fields[0])
		command = strings.TrimLeft(strings.TrimPrefix(command, fields[0]), " \t")
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", "", "", fmt.Errorf("tâche %s : commande vide", decl.Name)
	}
	workdir := decl.Workdir
	if workdir == "" {
		// Le code applicatif vit dans www/ : c'est là que se trouve bin/console.
		// Prendre la racine du projet produirait un chemin inexistant.
		workdir = filepath.Join(proj.Spec.Layout.Root, "www")
	}
	interp, err := p.interpreter(proj, decl)
	if err != nil {
		return "", "", "", err
	}
	// Seul le premier mot est réécrit : ses arguments sont rendus à l'identique.
	// Passer la ligne entière par filepath.Join réduisait « // » dans une URL et
	// résolvait « a/../b » jusqu'à emporter le nom de la commande (Q-68).
	first := fields[0]
	rest := strings.TrimPrefix(command, first)
	// Une commande relative est résolue depuis le répertoire de travail, et
	// « bin/console » comme « php … » passent par l'interpréteur de la tâche :
	// le lien géré par défaut, de sorte qu'une bascule de version PHP n'exige pas
	// de réécrire les crons (D30), ou la version que fixe « php: » (D98).
	switch {
	case first == "bin/console" || first == "./bin/console":
		command = interp + " " + filepath.Join(workdir, "bin", "console") + rest
	case first == "php":
		command = interp + rest
	case decl.PHP != "":
		// Ignorer la version demandée ferait tourner la tâche sous une autre
		// sans un mot : c'était le cas avant D98.
		return "", "", "", fmt.Errorf("tâche %s : « php: %s » ne s'applique qu'à « bin/console … » ou "+
			"« php … », pas à %q", decl.Name, decl.PHP, first)
	case filepath.IsAbs(first) || strings.ContainsAny(first, "~$`\"'"):
		// Absolu, ou désigné par le shell (« ~/x.sh », « $HOME/x.sh ») : rien à
		// résoudre ici.
	case strings.Contains(first, "/"):
		// Un chemin relatif — « scripts/x.sh », « ./x » — est résolu dans le
		// répertoire de travail.
		command = filepath.Join(workdir, first) + rest
	default:
		// Un nom nu qui désigne un fichier du répertoire de travail y était
		// résolu, et des déclarations en dépendent : la résolution est gardée,
		// signalée par une étape non critique (D101) — de même quand le
		// répertoire ne peut pas être vérifié. Sans homonyme établi, le nom nu
		// reste au PATH du fichier cron, comme sous un shell.
		if found, cause := p.lookupIn(proj, workdir, first); found {
			command = filepath.Join(workdir, first) + rest
			legacy = first
			legacyCause = cause
		}
	}

	lock := decl.Lock == nil || *decl.Lock
	if lock {
		lockPath := filepath.Join(p.Paths.ProjectRunDir(proj.ID()), decl.Name+".lock")
		command = fmt.Sprintf("/usr/bin/flock -n %s %s", lockPath, command)
	}
	if len(assigns) > 0 {
		command = strings.Join(assigns, " ") + " " + command
	}
	return "cd " + shellQuote(workdir) + " && " + command, legacy, legacyCause, nil
}

// isAssignment reconnaît une affectation de variable du shell sans guillemet ni
// échappement : « NOM=valeur ».
func isAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" || strings.ContainsAny(word, "'\"\\`$") {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// lookupIn dit si le répertoire de travail porte une entrée de ce nom. Il est
// relu sans suivre de lien, depuis la racine du projet quand il s'y trouve —
// le home appartient à l'utilisateur (D83) —, depuis « / » sinon. Seule
// l'absence établie — l'entrée, ou un répertoire du chemin, n'existe pas —
// envoie le nom au PATH. Toute autre issue — un parent lié, comme le
// « current » de Deployer ou de Capistrano, un accès refusé — ne prouve rien :
// la résolution d'avant D101 est gardée, et cause nomme pourquoi.
func (p *Provider) lookupIn(proj *model.Project, workdir, name string) (found bool, cause string) {
	root := filepath.Clean(proj.Spec.Layout.Root)
	target := filepath.Join(workdir, name)
	var err error
	if rel, rerr := filepath.Rel(root, target); rerr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		_, err = fs.LstatAt(root, rel)
	} else {
		_, err = fs.LstatAt("/", strings.TrimPrefix(target, "/"))
	}
	switch {
	case err == nil:
		return true, ""
	case errors.Is(err, os.ErrNotExist):
		// Seule l'absence de l'entrée dans un répertoire de travail qui existe
		// envoie au PATH. Un répertoire pas encore déployé — « www/futur/app » —
		// ne prouve rien : entre le déploiement et l'apply suivant, la tâche
		// partirait au PATH et échouerait en silence, là où elle marchait.
		info, werr := p.lstatWorkdir(root, workdir)
		switch {
		case werr == nil && info.IsDir():
			return false, ""
		case werr == nil || errors.Is(werr, fs.ErrNotDirectory):
			return true, "répertoire de travail non vérifiable : un de ses composants est un lien"
		case errors.Is(werr, os.ErrNotExist) && filepath.Clean(workdir) == filepath.Join(root, "www"):
			// www/ est créé vide par l'apply même (model.defaultDirs) : avant le
			// premier, son absence vaut celle de l'entrée.
			return false, ""
		case errors.Is(werr, os.ErrNotExist):
			return true, "répertoire de travail absent"
		}
		return true, "répertoire de travail non vérifiable : " + werr.Error()
	case errors.Is(err, fs.ErrNotDirectory):
		return true, "répertoire de travail non vérifiable : un de ses composants est un lien"
	}
	return true, "répertoire de travail non vérifiable : " + err.Error()
}

// lstatWorkdir décrit le répertoire de travail lui-même, sans le suivre.
func (p *Provider) lstatWorkdir(root, workdir string) (os.FileInfo, error) {
	workdir = filepath.Clean(workdir)
	if workdir == root {
		return os.Lstat(root)
	}
	if rel, err := filepath.Rel(root, workdir); err == nil && !strings.HasPrefix(rel, "..") {
		return fs.LstatAt(root, rel)
	}
	if workdir == "/" {
		return os.Lstat("/")
	}
	return fs.LstatAt("/", strings.TrimPrefix(workdir, "/"))
}

// Legacy nomme les tâches dont la commande commence par un nom nu résolu dans
// le répertoire de travail par compatibilité (D101), avec la forme à écrire.
func (p *Provider) Legacy(proj *model.Project) ([]string, error) {
	jobs, err := p.jobs(proj)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, j := range jobs {
		if j.Legacy != "" {
			out = append(out, legacyMessage(j))
		}
	}
	return out, nil
}

func legacyMessage(j job) string {
	if j.LegacyCause != "" {
		return fmt.Sprintf("tâche %s : « %s » est résolu dans le répertoire de travail par compatibilité — %s "+
			"(lien, accès refusé ou répertoire pas encore déployé) : écrire « ./%s » ou un chemin absolu", j.Name, j.Legacy, j.LegacyCause, j.Legacy)
	}
	return fmt.Sprintf("tâche %s : « %s » est résolu dans le répertoire de travail par compatibilité, "+
		"un nom nu désignant sinon une commande du PATH — écrire « ./%s »", j.Name, j.Legacy, j.Legacy)
}

// interpreter renvoie l'interpréteur PHP d'une tâche : le lien géré, ou le
// binaire de la version que la tâche fixe. Une version absente de la machine
// n'est relevée qu'en exécution réelle — un plan doit rester calculable sur une
// machine qui ne l'a pas, comme pour les pools.
func (p *Provider) interpreter(proj *model.Project, decl model.CronJob) (string, error) {
	if decl.PHP == "" {
		return filepath.Join(model.BinDir(proj.Spec.Layout.Root), "php"), nil
	}
	managed := p.PHPVersions
	if managed == nil {
		managed = config.DefaultPHPVersions
	}
	if err := validate.PHPVersion(decl.PHP, managed); err != nil {
		return "", fmt.Errorf("tâche %s : php : %w", decl.Name, err)
	}
	bin := p.Paths.PHPBin(decl.PHP)
	if !p.Simulated {
		if _, err := os.Stat(bin); err != nil {
			return "", fmt.Errorf("tâche %s : PHP %s n'est pas installé (%s introuvable)",
				decl.Name, decl.PHP, bin)
		}
	}
	return bin, nil
}

// shellQuote cite un argument pour /bin/sh. Un guillemet simple présent dans la
// valeur est fermé, échappé, puis réouvert.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Plan calcule les étapes du fichier cron et des fichiers de journal.
func (p *Provider) Plan(_ context.Context, proj *model.Project) ([]*engine.Step, error) {
	content, jobs, err := p.Build(proj)
	if err != nil {
		return nil, err
	}
	path := p.Path(proj.ID())
	if len(jobs) == 0 {
		// Aucune tâche : le fichier ne doit pas subsister — sauf s'il porte le
		// travail de quelqu'un. Une ligne ajoutée à la main par l'exploitant y
		// était refusée à l'écriture et partait pourtant au retrait de la
		// dernière tâche, sans --force ni copie.
		s, err := removal.File{
			Resource: "cron/" + proj.ID(),
			Summary:  "fichier cron devenu inutile",
			Path:     path,
			Project:  proj.ID(),
			Force:    p.Force,
			Backups:  p.Paths.BackupsDir(),
			Now:      p.now,
		}.Step()
		if err != nil || s == nil {
			return nil, err
		}
		return []*engine.Step{s}, nil
	}

	var steps []*engine.Step
	// Les fichiers de journal sont créés par ogs, avec le propriétaire attendu :
	// le répertoire des journaux appartient à root (D71), donc la tâche ne
	// pourrait pas les créer elle-même.
	for _, j := range jobs {
		j := j
		if _, err := os.Stat(j.Log); err == nil {
			continue
		}
		s := engine.NewStep("cron/log/"+j.Name, engine.Create,
			fmt.Sprintf("journal de la tâche %s (%s)", j.Name, filepath.Base(j.Log)))
		s.Apply = func(context.Context) error { return p.createLog(proj, j.Log) }
		s.Compensate = func(context.Context) error { return os.Remove(j.Log) }
		steps = append(steps, s)
	}

	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	resource := "cron/" + proj.ID()
	summary := fmt.Sprintf("tâches planifiées (%s)", describeJobs(jobs))
	forced := false
	switch state {
	case fs.StateCurrent:
		steps = append(steps, engine.NewStep(resource, engine.NoOp, summary))
		return append(steps, legacySteps(jobs)...), nil
	case fs.StateHandEdited, fs.StateForeign:
		if !p.Force {
			steps = append(steps, refusStep(resource, summary, path, p.Paths.BackupsDir(), state))
			return append(steps, legacySteps(jobs)...), nil
		}
		// --force : l'étape redevient une mise à jour ordinaire, annoncée comme
		// telle par « plan » avant qu'« apply » ne l'exécute. La sauvegarde, elle,
		// n'a lieu qu'à l'application : « plan » et « diff » n'écrivent rien.
		forced = true
		state = fs.StateOutdated
		summary += " — écrasement forcé (sauvegarde dans " + p.Paths.BackupsDir() + ")"
	}
	action := engine.Create
	if state == fs.StateOutdated {
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
	s.Apply = func(context.Context) error {
		if data, err := os.ReadFile(path); err == nil {
			previous = data
		}
		if forced {
			// Écraser le travail d'un exploitant sans en garder trace rend
			// l'opération irréversible : la sauvegarde précède l'écriture.
			if _, err := fs.BackupTo(path, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		// /etc/cron.d est à la machine, et « ogs harden » le ferme en 0750 (D56) :
		// le créer s'il manque, jamais le rouvrir (D116).
		if err := fs.EnsureSharedDir(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		// 0644 root:root : cron refuse un fichier de /etc/cron.d inscriptible par
		// un autre que root.
		return fs.WriteAtomic(path, content, 0o644, fs.KeepOwner)
	}
	s.Compensate = func(context.Context) error {
		if previous == nil {
			return os.Remove(path)
		}
		return fs.WriteAtomic(path, previous, 0o644, fs.KeepOwner)
	}
	steps = append(steps, s)
	return append(steps, legacySteps(jobs)...), nil
}

// legacySteps signale, par une étape non critique (code 6), chaque tâche dont un
// nom nu n'est résolu dans le répertoire de travail que par compatibilité.
func legacySteps(jobs []job) []*engine.Step {
	var out []*engine.Step
	for _, j := range jobs {
		if j.Legacy == "" {
			continue
		}
		msg := legacyMessage(j)
		s := engine.NewStep("cron/legacy/"+j.Name, engine.Update,
			fmt.Sprintf("tâche %s : nom nu « %s » résolu par compatibilité", j.Name, j.Legacy))
		s.Critical = false
		s.Apply = func(context.Context) error { return errors.New(msg) }
		out = append(out, s)
	}
	return out
}

// createLog crée un fichier de journal appartenant à l'utilisateur du projet, de
// sorte que la tâche puisse y écrire sans pouvoir remplacer le fichier.
func (p *Provider) createLog(proj *model.Project, path string) error {
	// MkdirAll plutôt qu'EnsureDir : le répertoire des journaux est posé par le
	// provider système (D71), seul juge de son mode — l'imposer ici aussi ferait
	// osciller le plan entre deux valeurs.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	owner := fs.KeepOwner
	if p.Owner != nil {
		uid, gid, err := p.Owner(proj)
		if err == nil {
			owner = fs.Owner{UID: uid, GID: gid}
		}
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return fs.WriteAtomic(path, nil, 0o640, owner)
}

func describeJobs(jobs []job) string {
	active, disabled := 0, 0
	for _, j := range jobs {
		if j.Enabled {
			active++
		} else {
			disabled++
		}
	}
	if disabled == 0 {
		return fmt.Sprintf("%d active(s)", active)
	}
	return fmt.Sprintf("%d active(s), %d désactivée(s)", active, disabled)
}

// NextOccurrences renvoie les prochaines dates d'exécution d'une expression, pour
// que l'administrateur confirme qu'il a écrit ce qu'il voulait.
func NextOccurrences(expr string, from time.Time, count int) ([]time.Time, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cinq champs attendus, %d obtenu(s)", len(fields))
	}
	var out []time.Time
	t := from.Truncate(time.Minute).Add(time.Minute)
	// Une année de recherche suffit : au-delà, l'expression ne correspond à rien.
	limit := from.AddDate(1, 0, 0)
	for len(out) < count && t.Before(limit) {
		if matches(fields, t) {
			out = append(out, t)
		}
		t = t.Add(time.Minute)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("aucune occurrence dans l'année à venir : l'expression est-elle correcte ?")
	}
	return out, nil
}

// matches évalue une expression cron sur un instant donné.
func matches(fields []string, t time.Time) bool {
	values := []int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
	for i, f := range fields {
		if !fieldMatches(f, values[i], i) {
			return false
		}
	}
	return true
}

func fieldMatches(field string, value, position int) bool {
	for _, part := range strings.Split(field, ",") {
		step := 1
		if slash := strings.Index(part, "/"); slash >= 0 {
			fmt.Sscanf(part[slash+1:], "%d", &step)
			part = part[:slash]
		}
		if part == "*" || part == "" {
			if step <= 1 {
				return true
			}
			base := 0
			if position == 2 || position == 3 {
				base = 1
			}
			if (value-base)%step == 0 {
				return true
			}
			continue
		}
		lo, hi := 0, 0
		if strings.Contains(part, "-") {
			fmt.Sscanf(part, "%d-%d", &lo, &hi)
		} else {
			fmt.Sscanf(part, "%d", &lo)
			hi = lo
		}
		// Dimanche s'écrit 0 ou 7 : les deux doivent correspondre.
		if position == 4 && value == 0 && (lo == 7 || hi == 7) {
			return true
		}
		for v := lo; v <= hi; v += step {
			if v == value {
				return true
			}
		}
	}
	return false
}
