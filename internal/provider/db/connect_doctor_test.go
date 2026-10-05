package db

import (
	"context"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

func TestDoctorSignaleLesBasesOuvertesAPublic(t *testing.T) {
	// La base adoptée qui accorde CONNECT à PUBLIC n'est jamais refermée (D111) :
	// doctor la nomme à chaque passage, et dit d'une base d'ogs rouverte que
	// l'apply la corrige.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "postgres")
	decl.Name, decl.User = "shop", "shop"
	proj.Spec.Databases = []model.Database{decl}
	adopter(proj)
	prov.clients = map[string]Client{"postgres": clusterAcces([]string{"shop"}, []string{"shop", "shop_owner"},
		map[string]string{"shop": "shop_owner"}, map[string]bool{"shop": true})}

	notes := prov.Notices(context.Background(), proj)
	if len(notes) != 1 || notes[0].Kind != NoticePublicConnect || !notes[0].Adopted || notes[0].Database != "shop" {
		t.Errorf("la base adoptée ouverte à PUBLIC doit être signalée : %+v", notes)
	}

	prov, proj = fixture(t)
	proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "postgres")}
	sceller(t, prov, proj, "postgres", "shopfr")
	prov.clients = map[string]Client{"postgres": clusterAcces([]string{"shopfr"}, []string{"shopfr"},
		map[string]string{"shopfr": "shopfr"}, map[string]bool{"shopfr": false})}
	if notes := prov.Notices(context.Background(), proj); len(notes) != 0 {
		t.Errorf("une base fermée n'est pas signalée : %+v", notes)
	}

	// Moteur injoignable : doctor le dit, au lieu de se taire.
	proj.Spec.Databases[0].Cluster = "99/absent"
	prov.clients = nil
	if notes := prov.Notices(context.Background(), proj); len(notes) != 1 || notes[0].Kind != NoticeUnavailable {
		t.Errorf("une base illisible doit être signalée : %+v", notes)
	}
}

func TestDoctorSignaleUnJokerGarde(t *testing.T) {
	// Le joker qui couvre une autre base reste (D111) : le plan ne le nomme que
	// si l'étape tourne, doctor à chaque passage.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.Name, decl.User, decl.Host = "shop_fr", "shop", "%"
	proj.Spec.Databases = []model.Database{decl}
	adopter(proj)
	client, _ := fauxMySQL(t, []string{"shop_fr", "shop-fr"}, []string{"shop@%"}, [][2]string{
		{`shop\_fr`, "SELECT,SHOW VIEW"},
		{"shop_fr", tousDroits},
	})
	prov.clients = map[string]Client{"mysql": client}
	notes := prov.Notices(context.Background(), proj)
	if len(notes) != 1 || notes[0].Kind != NoticeJokerKept || len(notes[0].Covers) != 1 || notes[0].Covers[0] != "shop-fr" {
		t.Errorf("le joker gardé doit être signalé : %+v", notes)
	}
}
