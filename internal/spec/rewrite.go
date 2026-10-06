package spec

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// Edit désigne une valeur scalaire d'une déclaration par son chemin — une chaîne
// pour une clé, un entier pour un rang de liste — et la valeur qu'elle doit
// prendre.
type Edit struct {
	Path     []any
	Old, New string
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// ErrManualEdit signale une valeur qu'ogs ne sait pas réécrire sans toucher au
// reste du fichier : l'exploitant doit la reprendre lui-même.
var ErrManualEdit = errors.New("réécriture à faire à la main")

// RewriteValues réécrit, dans le texte d'une déclaration, les seules valeurs que
// désignent edits, octet pour octet ailleurs : commentaires, ordre des clés,
// style des guillemets et lignes vides restent ce que l'exploitant a écrit —
// Save, qui resérialise le projet, les perdrait. Une valeur n'est réécrite qu'en
// place, sur sa ligne, en remplaçant la plus courte portion qui diffère ; le
// résultat est relu et doit être le document d'origine aux seules valeurs
// désignées près, sans quoi rien n'est rendu.
//
// Une valeur qui n'a plus la forme attendue (Old) est une erreur : le fichier a
// changé depuis le plan.
func RewriteValues(data []byte, edits []Edit) ([]byte, error) {
	// Une marque d'ordre des octets en tête est gardée telle quelle : l'analyse
	// porte sur la suite, et la marque est rendue devant le texte réécrit.
	if rest, ok := bytes.CutPrefix(data, utf8BOM); ok {
		out, err := RewriteValues(rest, edits)
		if err != nil {
			return nil, err
		}
		return append(append([]byte(nil), utf8BOM...), out...), nil
	}
	file, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("YAML illisible : %w", err)
	}
	if len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return nil, fmt.Errorf("%w : la déclaration doit porter un seul document", ErrManualEdit)
	}
	type change struct {
		line, col int
		old, new  string
		field     string
	}
	var changes []change
	for _, e := range edits {
		field := pathString(e.Path)
		node, err := lookup(file.Docs[0].Body, e.Path)
		if err != nil {
			return nil, fmt.Errorf("%s : %w", field, err)
		}
		str, ok := node.(*ast.StringNode)
		if !ok {
			return nil, fmt.Errorf("%w : %s n'est pas une chaîne sur une ligne", ErrManualEdit, field)
		}
		if str.Value != e.Old {
			return nil, fmt.Errorf("%s a changé depuis le plan (%q, attendu %q)", field, str.Value, e.Old)
		}
		old, repl := minimalDiff(e.Old, e.New)
		if old == "" {
			continue
		}
		pos := str.GetToken().Position
		changes = append(changes, change{line: pos.Line, col: pos.Column, old: old, new: repl, field: field})
	}
	// Du bas vers le haut, et de droite à gauche sur une même ligne : une
	// réécriture ne décale jamais la position de celles qui restent à faire.
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].line != changes[j].line {
			return changes[i].line > changes[j].line
		}
		return changes[i].col > changes[j].col
	})
	out := append([]byte(nil), data...)
	for _, c := range changes {
		start, end, ok := lineBounds(out, c.line)
		if !ok || c.col < 1 || start+c.col-1 > end {
			return nil, fmt.Errorf("%w : position de %s introuvable", ErrManualEdit, c.field)
		}
		from := start + c.col - 1
		i := bytes.Index(out[from:end], []byte(c.old))
		if i < 0 {
			// Valeur repliée sur plusieurs lignes, ou écrite avec des
			// échappements : la retrouver par le texte serait deviner.
			return nil, fmt.Errorf("%w : %s n'est pas écrite telle quelle sur sa ligne", ErrManualEdit, c.field)
		}
		at := from + i
		out = append(out[:at], append([]byte(c.new), out[at+len(c.old):]...)...)
	}
	if err := sameExcept(data, out, edits); err != nil {
		return nil, err
	}
	return out, nil
}

// sameExcept relit les deux textes et exige qu'ils ne diffèrent que par les
// valeurs désignées, chacune devenue ce qui était demandé.
func sameExcept(before, after []byte, edits []Edit) error {
	var want, got any
	if err := yaml.Unmarshal(before, &want); err != nil {
		return err
	}
	if err := yaml.Unmarshal(after, &got); err != nil {
		return fmt.Errorf("%w : le texte réécrit ne se relit plus (%v)", ErrManualEdit, err)
	}
	for _, e := range edits {
		if !setGeneric(want, e.Path, e.New) {
			return fmt.Errorf("%w : %s introuvable à la relecture", ErrManualEdit, pathString(e.Path))
		}
	}
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("%w : la réécriture toucherait autre chose que les valeurs visées", ErrManualEdit)
	}
	return nil
}

func setGeneric(v any, path []any, value string) bool {
	for i, k := range path {
		last := i == len(path)-1
		switch key := k.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return false
			}
			if last {
				m[key] = value
				return true
			}
			v = m[key]
		case int:
			l, ok := v.([]any)
			if !ok || key >= len(l) {
				return false
			}
			if last {
				l[key] = value
				return true
			}
			v = l[key]
		}
	}
	return false
}

// minimalDiff réduit une réécriture à la portion qui change, préfixe et suffixe
// communs ôtés : c'est elle seule que l'on cherche puis remplace dans le texte,
// où guillemets et échappements entourent la valeur sans la toucher.
func minimalDiff(old, repl string) (string, string) {
	p := 0
	for p < len(old) && p < len(repl) && old[p] == repl[p] {
		p++
	}
	s := 0
	for s < len(old)-p && s < len(repl)-p && old[len(old)-1-s] == repl[len(repl)-1-s] {
		s++
	}
	// La portion cherchée doit rester reconnaissable : on la prolonge jusqu'au
	// début de la valeur, qui n'apparaît qu'une fois sur la ligne de la clé.
	return old[:len(old)-s], repl[:len(repl)-s]
}

func lineBounds(data []byte, line int) (start, end int, ok bool) {
	start = 0
	for l := 1; l < line; l++ {
		i := bytes.IndexByte(data[start:], '\n')
		if i < 0 {
			return 0, 0, false
		}
		start += i + 1
	}
	end = len(data)
	if i := bytes.IndexByte(data[start:], '\n'); i >= 0 {
		end = start + i
	}
	return start, end, true
}

func lookup(node ast.Node, path []any) (ast.Node, error) {
	for _, k := range path {
		node = unwrap(node)
		switch key := k.(type) {
		case string:
			var values []*ast.MappingValueNode
			switch n := node.(type) {
			case *ast.MappingNode:
				values = n.Values
			case *ast.MappingValueNode:
				values = []*ast.MappingValueNode{n}
			default:
				return nil, fmt.Errorf("%w : %q n'est pas sous une table", ErrManualEdit, key)
			}
			var found ast.Node
			merged := false
			for _, v := range values {
				if v.Key == nil || v.Key.GetToken() == nil {
					continue
				}
				if _, ok := v.Key.(*ast.MergeKeyNode); ok || v.Key.GetToken().Value == "<<" {
					merged = true
					continue
				}
				if v.Key.GetToken().Value == key {
					found = v.Value
				}
			}
			if found == nil && merged {
				// La valeur vient d'une fusion (« <<: *ancre ») : la réécrire là
				// où elle est écrite changerait aussi tout ce qui fusionne la même
				// ancre.
				return nil, fmt.Errorf("%w : %q vient d'une fusion YAML « << », à reprendre là où l'ancre "+
					"est définie", ErrManualEdit, key)
			}
			if found == nil {
				return nil, fmt.Errorf("clé %q introuvable", key)
			}
			node = found
		case int:
			seq, ok := node.(*ast.SequenceNode)
			if !ok || key >= len(seq.Values) {
				return nil, fmt.Errorf("rang %d introuvable", key)
			}
			node = seq.Values[key]
		}
	}
	node = unwrap(node)
	if _, alias := node.(*ast.AliasNode); alias {
		return nil, fmt.Errorf("%w : la valeur est un alias YAML", ErrManualEdit)
	}
	return node, nil
}

func unwrap(n ast.Node) ast.Node {
	for {
		switch v := n.(type) {
		case *ast.TagNode:
			n = v.Value
		case *ast.AnchorNode:
			n = v.Value
		default:
			return n
		}
	}
}

func pathString(path []any) string {
	var b strings.Builder
	for _, k := range path {
		switch key := k.(type) {
		case string:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(key)
		case int:
			fmt.Fprintf(&b, "[%d]", key)
		}
	}
	return b.String()
}

// WriteFileAtomic remplace un fichier de déclaration par data, atomiquement :
// fichier temporaire créé en exclusif dans le même répertoire, mode et
// propriétaire de l'original reportés, données synchronisées, puis renommage.
// Un lien à la place du fichier est refusé — /etc/ogs appartient à root.
func WriteFileAtomic(path string, data []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s n'est pas un fichier ordinaire : refus de le réécrire", path)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".ogs-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return fail(err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && (int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid()) {
		if err := tmp.Chown(int(st.Uid), int(st.Gid)); err != nil {
			return fail(err)
		}
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
