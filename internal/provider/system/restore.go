package system

import (
	"path"
	"path/filepath"

	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// RestoredOwner rend le propriétaire de chaque entrée qu'une restauration remet
// dans le home, d'après son chemin relatif à la racine du projet et le
// propriétaire que son en-tête d'archive porte.
//
// Une archive qui porte les propriétaires d'origine est suivie. origin est celui
// de la racine archivée — le home, qui appartient à l'utilisateur (D51) — : ce qui
// était à l'utilisateur lui revient, dans le groupe du projet ; tout le reste
// revient à root, dans le groupe du projet si c'était le sien, avec son mode
// d'origine. Rien n'est ainsi rendu lisible à qui ne le lisait pas : un lien dur
// vers un fichier du système, qu'un fs.protected_hardlinks désactivé laisse poser
// dans le home, revient à root avec son mode — le rendre à l'utilisateur lui
// aurait donné /etc/shadow —, et l'ancien ~/logs, root, d'un projet antérieur à
// D71 reste à root, ce que sa migration exige pour le reprendre. Les identifiants
// sont remappés sur ceux du compte recréé, qui ne sont pas forcément les mêmes.
//
// Une archive antérieure ne porte aucun propriétaire. La déclaration archivée
// (archived, telle que l'archive la porte, avant toute normalisation) décide
// alors : root et le groupe du projet sous un répertoire qu'elle donne à root,
// l'utilisateur ailleurs — le répertoire déclaré le plus proche l'emporte, comme
// model.defaultDirs décide des répertoires eux-mêmes. Elle doit être prise avant
// la normalisation, qui retire les anciennes entrées logs/ et .bin/ : sans elles,
// l'ancien ~/logs revenait à l'utilisateur, et la migration de D71 le refusait
// ensuite à chaque apply, comme un répertoire substitué qu'il n'était pas. Une
// racine archivée au compte de root ne dit pas davantage qui, de ses entrées,
// était au projet : la déclaration décide de même.
//
// Il fallait tout le home : rendre le seul www/ laissait à root un authorized_keys
// que sshd lit sous l'identité de l'utilisateur — la connexion par clé était
// refusée —, les sessions PHP, les caches, et tout le site d'un projet adopté
// servi hors de www/.
//
// Le compte doit exister : la restauration n'extrait qu'une fois le projet
// appliqué. En mode simulé, rien n'est changé.
func (p *Provider) RestoredOwner(proj *model.Project, archived []model.Dir, origin *fs.Owner) (func(rel string, orig fs.Owner) fs.Owner, error) {
	if p.Exec.Simulated() {
		return func(string, fs.Owner) fs.Owner { return fs.KeepOwner }, nil
	}
	uid, gid, err := p.ResolveOwner(proj, "")
	if err != nil {
		return nil, err
	}
	user, root := fs.Owner{UID: uid, GID: gid}, fs.Owner{UID: 0, GID: gid}
	if origin != nil && origin.UID != 0 {
		return func(_ string, orig fs.Owner) fs.Owner {
			if orig.UID == origin.UID {
				return user
			}
			o := fs.Owner{UID: 0, GID: 0}
			if orig.GID == origin.GID {
				o.GID = gid
			}
			return o
		}, nil
	}
	declared := make(map[string]fs.Owner, len(archived))
	for _, d := range archived {
		o := user
		if d.Owner == "root" {
			o = root
		}
		declared[path.Clean(filepath.ToSlash(d.Path))] = o
	}
	return func(rel string, _ fs.Owner) fs.Owner {
		for dir := path.Clean(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if o, ok := declared[dir]; ok {
				return o
			}
		}
		return user
	}, nil
}
