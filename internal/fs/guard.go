package fs

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// Guard retient d'une copie (CopyTreeGuardedAt) ce qui ne doit pas passer d'un
// projet à l'autre. Une copie de la production vers la préproduction recopie
// l'application avec sa configuration : les identifiants de la base et du
// transport de la production, ses clés d'API, sa clé de chiffrement. La
// préproduction s'y connectait avec eux et y écrivait, et son compte les recevait
// en clair.
//
// La garde agit de deux façons, parce qu'aucune ne suffit seule. Les chemins
// (Paths) désignent ce qu'un type d'application range, par convention, dans des
// fichiers connus — un .env.local de Symfony porte aussi bien DATABASE_URL
// qu'APP_SECRET ou une clé d'API, qu'ogs ne saurait ni reconnaître ni
// remplacer : ils ne sont pas recopiés. Les valeurs (Values) sont les secrets
// qu'ogs détient lui-même — les mots de passe qu'il a générés — et qu'il
// reconnaît où qu'ils soient : un wp-config.php, un cache compilé, un journal.
// Un fichier qui en porte un est recopié, le secret remplacé par un masque de
// même longueur. Le retenir en entier ferait pis : une application privée de son
// fichier de configuration peut offrir son assistant d'installation à qui visite
// la préproduction, publique (D57) — WordPress le fait —, là où un mot de passe
// faux ne rend qu'une erreur de connexion.
type Guard struct {
	Paths  []GuardPath
	Values []GuardValue
}

// GuardPath désigne des entrées de la source par leur chemin relatif à sa
// racine, composant par composant au sens de path.Match : « .env.*.local »,
// « config/secrets/*/*.decrypt.private.php ». Une entrée désignée est retenue
// quel que soit son type — un lien de ce nom mène à la même configuration —, un
// répertoire avec tout son contenu, et une entrée de même nom déjà présente dans
// la destination reste en place : c'est celle du projet de destination, qu'un
// hook postCreate a pu écrire.
type GuardPath struct {
	Pattern string
	// Reason dit pourquoi l'entrée n'est pas recopiée.
	Reason string
}

// GuardValue est une valeur qu'aucun fichier recopié ne doit porter.
type GuardValue struct {
	Value []byte
	// Reason nomme ce qu'elle est : « mot de passe de shopfr (base mysql shopfr) ».
	Reason string
}

// guardMask est le motif des masques : lisible dans le fichier recopié, et fait
// des seuls caractères ASCII, pour que la longueur en octets soit celle du secret.
const guardMask = "OGS-SECRET-RETIRE-"

// MaskOf rend le masque d'une valeur de n octets : le motif, complété ou coupé
// à n. Il est de même longueur que le secret qu'il remplace, si bien que rien ne
// se décale — ni les positions d'un fichier creux, ni la longueur annoncée d'une
// chaîne PHP sérialisée (s:32:"…").
func MaskOf(n int) []byte {
	if n <= len(guardMask) {
		return []byte(guardMask[:n])
	}
	return []byte(guardMask + strings.Repeat("x", n-len(guardMask)))
}

// guardPath est un GuardPath découpé en composants.
type guardPath struct {
	comps  []string
	reason string
}

// guard est la forme de travail d'une Guard.
type guard struct {
	paths  []guardPath
	values []GuardValue
	masks  [][]byte
	// keep est la longueur de la plus longue valeur, moins un : ce qu'un morceau
	// doit garder du précédent pour voir une occurrence à cheval sur les deux.
	keep int
}

// newGuard prépare g, ou rend nil si elle ne retient rien. Un motif invalide ou
// une valeur vide sont refusés : une garde qui ne retiendrait rien sans le dire
// laisserait passer ce qu'elle devait arrêter.
func newGuard(g Guard) (*guard, error) {
	if len(g.Paths) == 0 && len(g.Values) == 0 {
		return nil, nil
	}
	out := &guard{}
	for _, p := range g.Paths {
		comps := strings.Split(p.Pattern, "/")
		for _, c := range comps {
			if c == "" || c == "." || c == ".." {
				return nil, fmt.Errorf("garde de la copie : motif %q invalide", p.Pattern)
			}
			if _, err := path.Match(c, ""); err != nil {
				return nil, fmt.Errorf("garde de la copie : motif %q : %w", p.Pattern, err)
			}
		}
		out.paths = append(out.paths, guardPath{comps: comps, reason: p.Reason})
	}
	for _, v := range g.Values {
		if len(v.Value) == 0 {
			return nil, fmt.Errorf("garde de la copie : valeur vide (%s)", v.Reason)
		}
		out.values = append(out.values, v)
		out.masks = append(out.masks, MaskOf(len(v.Value)))
		out.keep = max(out.keep, len(v.Value)-1)
	}
	return out, nil
}

// withheld dit si l'entrée name du répertoire at est désignée par un chemin de la
// garde, et pourquoi. Seuls les motifs qui ont autant de composants que l'entrée
// a de niveaux sont essayés : la profondeur des motifs, non celle de la copie,
// borne le travail.
func (g *guard) withheld(at *dirNode, name string) (string, bool) {
	for _, p := range g.paths {
		if len(p.comps) != at.depth+1 {
			continue
		}
		if ok, _ := path.Match(p.comps[len(p.comps)-1], name); !ok {
			continue
		}
		n, match := at, true
		for i := len(p.comps) - 2; i >= 0 && match; i-- {
			match, _ = path.Match(p.comps[i], n.name)
			n = n.parent
		}
		if match {
			return p.reason, true
		}
	}
	return "", false
}

// masker remplace, dans les morceaux d'un fichier en cours de copie, les
// valeurs de la garde par leur masque, avant qu'ils ne soient écrits. Il garde
// la fin du morceau précédent : une occurrence à cheval sur deux morceaux est
// masquée des deux côtés, la part déjà écrite l'étant par une réécriture à sa
// position.
type masker struct {
	g      *guard
	tail   []byte // fin du morceau précédent, déjà écrite, g.keep octets au plus
	tailAt int64  // position de tail dans le fichier
	win    []byte
	hits   []bool
}

func newMasker(g *guard) *masker {
	return &masker{g: g, hits: make([]bool, len(g.values))}
}

// reset prépare le masquage d'un nouveau fichier.
func (m *masker) reset() {
	m.tail, m.tailAt = m.tail[:0], 0
	clear(m.hits)
}

// reasons rend ce qui a été masqué dans le fichier, sans doublon.
func (m *masker) reasons() []string {
	var out []string
	for i, hit := range m.hits {
		if hit && !slices.Contains(out, m.g.values[i].Reason) {
			out = append(out, m.g.values[i].Reason)
		}
	}
	return out
}

// mask masque en place, dans p — le morceau lu à la position at et que
// l'appelant va écrire dans out —, toute occurrence d'une valeur de la garde.
// Un morceau qui ne suit pas le précédent (une étendue de données après un trou)
// ne prolonge rien : le trou ne porte que des zéros.
func (m *masker) mask(out int, p []byte, at int64) error {
	if m.tailAt+int64(len(m.tail)) != at {
		m.tail = m.tail[:0]
	}
	if len(m.tail) > 0 {
		m.win = append(append(m.win[:0], m.tail...), p[:min(len(p), m.g.keep)]...)
		for i, v := range m.g.values {
			for from := 0; from < len(m.tail); {
				j := bytes.Index(m.win[from:], v.Value)
				if j < 0 {
					break
				}
				j += from
				// Seules comptent ici les occurrences qui commencent dans la part
				// déjà écrite et finissent dans le morceau : les autres sont
				// entières dans l'un ou l'autre.
				if j >= len(m.tail) {
					break
				}
				if j+len(v.Value) > len(m.tail) {
					before := len(m.tail) - j
					if err := pwriteAll(out, m.g.masks[i][:before], m.tailAt+int64(j)); err != nil {
						return err
					}
					copy(p, m.g.masks[i][before:])
					copy(m.win[j:], m.g.masks[i])
					m.hits[i] = true
				}
				from = j + 1
			}
		}
		// tail reste ce qui est écrit, masques compris.
		copy(m.tail, m.win)
	}
	for i, v := range m.g.values {
		for from := 0; ; {
			j := bytes.Index(p[from:], v.Value)
			if j < 0 {
				break
			}
			j += from
			copy(p[j:], m.g.masks[i])
			m.hits[i] = true
			from = j + len(v.Value)
		}
	}
	// La fin de ce morceau, masquée comme elle va être écrite, pour le suivant.
	if len(p) >= m.g.keep {
		m.tail = append(m.tail[:0], p[len(p)-m.g.keep:]...)
	} else {
		m.tail = append(m.tail, p...)
		if extra := len(m.tail) - m.g.keep; extra > 0 {
			m.tail = append(m.tail[:0], m.tail[extra:]...)
		}
	}
	m.tailAt = at + int64(len(p)) - int64(len(m.tail))
	return nil
}

// pwriteAll écrit p en entier dans fd, à la position off.
func pwriteAll(fd int, p []byte, off int64) error {
	for len(p) > 0 {
		w, err := unix.Pwrite(fd, p, off)
		if err != nil {
			return err
		}
		p, off = p[w:], off+int64(w)
	}
	return nil
}
