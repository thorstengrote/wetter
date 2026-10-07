package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestNaechstesAuftreten(t *testing.T) {
	l := berlin()
	if z := naechstes(time.Date(2026, 10, 4, 21, 0, 0, 0, l), 2, 0); z != time.Date(2026, 10, 5, 2, 0, 0, 0, l) {
		t.Fatalf("21 Uhr fuer 2 Uhr: %v", z)
	}
	if z := naechstes(time.Date(2026, 10, 5, 0, 30, 0, 0, l), 2, 0); z != time.Date(2026, 10, 5, 2, 0, 0, 0, l) {
		t.Fatalf("0:30 fuer 2 Uhr: %v", z)
	}
}

func TestNachtFaehrtNurZu(t *testing.T) {
	l := berlin()
	pos := 100.0
	var gefahren []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/velux/stand":
			json.NewEncoder(w).Encode(map[string]any{"gruppen": map[string]any{"levi": map[string]any{"position": pos, "bekannt": 1}}})
		case "/velux/gruppen":
			w.Write([]byte(`{"positionen":{"lueftungsverdunklung":22}}`))
		case "/velux/fahre":
			var a map[string]string
			json.NewDecoder(r.Body).Decode(&a)
			gefahren = append(gefahren, a["gruppe"]+":"+a["richtung"])
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	jetzt := time.Date(2026, 10, 4, 21, 0, 0, 0, l)
	n := neueNacht(filepath.Join(t.TempDir(), "n.json"), srv.URL, func(string, ...any) {})
	n.jetzt = func() time.Time { return jetzt }
	if _, err := n.setze("levi", "nacht", "02:00", "zu"); err != nil {
		t.Fatal(err)
	}
	n.pruefe()
	if len(gefahren) != 0 {
		t.Fatal("zu frueh gefahren")
	}
	jetzt = time.Date(2026, 10, 5, 2, 0, 30, 0, l)
	n.pruefe()
	if len(gefahren) != 1 || gefahren[0] != "levi:zu" || len(n.liste()) != 0 {
		t.Fatalf("nicht gefahren oder nicht geloescht: %v %v", gefahren, n.liste())
	}
	// Schon unten: nichts tun.
	pos = 0
	n.setze("levi", "nacht", "03:00", "lueft")
	jetzt = time.Date(2026, 10, 5, 3, 1, 0, 0, l)
	n.pruefe()
	if len(gefahren) != 1 {
		t.Fatal("Rollladen war schon zu und wurde trotzdem gefahren")
	}
	// Ueberfaellig: verwerfen.
	pos = 100
	jetzt = time.Date(2026, 10, 5, 3, 30, 0, 0, l)
	n.setze("levi", "nacht", "04:00", "zu")
	jetzt = time.Date(2026, 10, 5, 9, 0, 0, 0, l)
	n.pruefe()
	if len(gefahren) != 1 || len(n.liste()) != 0 {
		t.Fatal("ueberfaelliger Auftrag mittags ausgefuehrt")
	}
}

func TestMorgenOeffnetNur(t *testing.T) {
	l := berlin()
	pos := 0.0
	var gefahren []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/velux/stand":
			json.NewEncoder(w).Encode(map[string]any{"gruppen": map[string]any{"ben": map[string]any{"position": pos, "bekannt": 2}}})
		case "/velux/gruppen":
			w.Write([]byte(`{"positionen":{"lueftungsverdunklung":22}}`))
		case "/velux/fahre":
			var a map[string]string
			json.NewDecoder(r.Body).Decode(&a)
			gefahren = append(gefahren, a["gruppe"]+":"+a["richtung"])
		}
	}))
	defer srv.Close()
	jetzt := time.Date(2026, 10, 4, 22, 0, 0, 0, l)
	n := neueNacht(filepath.Join(t.TempDir(), "n.json"), srv.URL, func(string, ...any) {})
	n.jetzt = func() time.Time { return jetzt }
	n.setze("ben", "nacht", "01:00", "zu")
	if _, err := n.setze("ben", "morgen", "07:00", "auf"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.setze("ben", "morgen", "07:00", "zu"); err == nil {
		t.Fatal("morgens zu angenommen")
	}
	if len(n.liste()) != 2 {
		t.Fatal("Nacht und Morgen nicht nebeneinander")
	}
	pos = 100 // jemand hat schon geoeffnet: Nacht faehrt zu, Morgen oeffnet
	jetzt = time.Date(2026, 10, 5, 1, 0, 30, 0, l)
	n.pruefe()
	pos = 0
	jetzt = time.Date(2026, 10, 5, 7, 0, 30, 0, l)
	n.pruefe()
	if len(gefahren) != 2 || gefahren[0] != "ben:zu" || gefahren[1] != "ben:auf" || len(n.liste()) != 0 {
		t.Fatalf("falsch gefahren: %v, offen %v", gefahren, n.liste())
	}
	// Morgens schon offen: nichts tun.
	pos = 100
	n.setze("ben", "morgen", "08:00", "lueft")
	jetzt = time.Date(2026, 10, 5, 8, 1, 0, 0, l)
	n.pruefe()
	if len(gefahren) != 2 {
		t.Fatal("offenen Rollladen morgens auf Lueftung zugefahren")
	}
}

func TestNachtEigeneGruppe(t *testing.T) {
	berlin()
	n := neueNacht(t.TempDir()+"/n.json", "http://127.0.0.1:1", func(string, ...any) {})
	jetzt := time.Date(2026, 10, 7, 21, 0, 0, 0, ort)
	n.jetzt = func() time.Time { return jetzt }
	var gefahren []string
	n.eigene = map[string]func(string) error{"buero": func(r string) error { gefahren = append(gefahren, r); return nil }}
	if _, err := n.setze("buero", "nacht", "23:00", "lueft"); err != nil {
		t.Fatal(err)
	}
	n.pruefe()
	if len(gefahren) != 0 {
		t.Fatal("zu frueh gefahren")
	}
	jetzt = jetzt.Add(2*time.Hour + time.Minute)
	n.pruefe()
	if len(gefahren) != 1 || gefahren[0] != "lueft" || len(n.liste()) != 0 {
		t.Fatalf("gefahren %v, offen %v", gefahren, n.liste())
	}
}
