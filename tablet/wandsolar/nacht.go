package main

// Einmalige Nachtschliessung eines Rollladens (Wunsch von Levi, 04.10.2026):
// abends festlegen, dass der Rollladen in der kommenden Nacht einmal zu einer
// Uhrzeit zufaehrt, etwa um 2 Uhr, weil er bis dahin lueften will.
//
//   - Gespeichert wird ein absoluter Zeitpunkt: das naechste Auftreten der
//     Uhrzeit innerhalb von 24 Stunden. Abends um 21 Uhr fuer 2 Uhr ist das
//     morgen frueh, um 0:30 in anderthalb Stunden.
//   - Nur in Schliessrichtung. Steht der Rollladen schon tiefer, passiert
//     nichts. Geoeffnet wird nie.
//   - Ziel: zu oder Lueftungsstellung.
//   - Nach dem Ausloesen ist der Auftrag weg. Ist er mehr als vier Stunden
//     ueberfaellig (Tablet war aus), wird er verworfen statt mittags
//     zuzufahren.
//   - Der Dienst plant, nicht die Seite: nachts ist der Browser gedrosselt.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type nachtAuftrag struct {
	Gruppe   string    `json:"gruppe"`
	Richtung string    `json:"richtung"` // zu, lueft
	Zeit     time.Time `json:"zeit"`
	Angelegt time.Time `json:"angelegt"`
}

type nacht struct {
	sync.Mutex
	pfad      string
	basis     string // hapwatch
	auftraege map[string]nachtAuftrag
	sag       func(string, ...any)
	client    *http.Client
	jetzt     func() time.Time
}

func neueNacht(pfad, basis string, sag func(string, ...any)) *nacht {
	n := &nacht{pfad: pfad, basis: strings.TrimRight(basis, "/"), auftraege: map[string]nachtAuftrag{},
		sag: sag, client: &http.Client{Timeout: 20 * time.Second},
		jetzt: func() time.Time { return time.Now().In(ort) }}
	if roh, err := os.ReadFile(pfad); err == nil {
		json.Unmarshal(roh, &n.auftraege)
	}
	return n
}

// naechstes: das naechste Auftreten von hh:mm nach t, hoechstens 24 h spaeter.
func naechstes(t time.Time, h, m int) time.Time {
	z := time.Date(t.Year(), t.Month(), t.Day(), h, m, 0, 0, t.Location())
	if !z.After(t) {
		z = z.AddDate(0, 0, 1)
	}
	return z
}

func (n *nacht) setze(gruppe, uhrzeit, richtung string) (nachtAuftrag, error) {
	var h, m int
	if _, err := fmt.Sscanf(uhrzeit, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return nachtAuftrag{}, fmt.Errorf("Uhrzeit wie 02:00")
	}
	if richtung != "zu" && richtung != "lueft" {
		return nachtAuftrag{}, fmt.Errorf("Richtung zu oder lueft")
	}
	t := n.jetzt()
	a := nachtAuftrag{Gruppe: gruppe, Richtung: richtung, Zeit: naechstes(t, h, m), Angelegt: t}
	n.Lock()
	n.auftraege[gruppe] = a
	schreibeJSON(n.pfad, n.auftraege)
	n.Unlock()
	n.sag("Nacht: %s um %s %s", gruppe, a.Zeit.Format("02.01. 15:04"), richtung)
	return a, nil
}

func (n *nacht) loesche(gruppe string) {
	n.Lock()
	defer n.Unlock()
	if _, ok := n.auftraege[gruppe]; ok {
		delete(n.auftraege, gruppe)
		schreibeJSON(n.pfad, n.auftraege)
		n.sag("Nacht: %s abgebrochen", gruppe)
	}
}

func (n *nacht) liste() map[string]nachtAuftrag {
	n.Lock()
	defer n.Unlock()
	out := map[string]nachtAuftrag{}
	for k, v := range n.auftraege {
		out[k] = v
	}
	return out
}

// position fragt hapwatch nach dem Stand der Gruppe.
func (n *nacht) position(gruppe string) (float64, float64, error) {
	r, err := n.client.Get(n.basis + "/velux/stand")
	if err != nil {
		return 0, 0, err
	}
	defer r.Body.Close()
	var a struct {
		Gruppen map[string]struct {
			Position float64 `json:"position"`
			Bekannt  int     `json:"bekannt"`
		} `json:"gruppen"`
	}
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		return 0, 0, err
	}
	g, ok := a.Gruppen[gruppe]
	if !ok || g.Bekannt == 0 {
		return 0, 0, fmt.Errorf("Stand von %s unbekannt", gruppe)
	}
	lueft := 22.0
	if r2, err := n.client.Get(n.basis + "/velux/gruppen"); err == nil {
		var b struct {
			Positionen map[string]float64 `json:"positionen"`
		}
		if json.NewDecoder(r2.Body).Decode(&b) == nil {
			if v, ok := b.Positionen["lueftungsverdunklung"]; ok {
				lueft = v
			}
		}
		r2.Body.Close()
	}
	return g.Position, lueft, nil
}

func (n *nacht) fahre(gruppe, richtung string) error {
	body, _ := json.Marshal(map[string]string{"gruppe": gruppe, "richtung": richtung})
	r, err := n.client.Post(n.basis+"/velux/fahre", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		roh, _ := io.ReadAll(io.LimitReader(r.Body, 300))
		return fmt.Errorf("hapwatch %d: %s", r.StatusCode, strings.TrimSpace(string(roh)))
	}
	return nil
}

// pruefe laeuft jede Minute.
func (n *nacht) pruefe() {
	t := n.jetzt()
	for g, a := range n.liste() {
		if t.Before(a.Zeit) {
			continue
		}
		if t.Sub(a.Zeit) > 4*time.Hour {
			n.sag("Nacht: %s verworfen, %s verpasst", g, a.Zeit.Format("15:04"))
			n.loesche(g)
			continue
		}
		pos, lueft, err := n.position(g)
		if err != nil {
			n.sag("Nacht: %s: %v, neuer Versuch in einer Minute", g, err)
			continue
		}
		ziel := 0.0
		if a.Richtung == "lueft" {
			ziel = lueft
		}
		if pos <= ziel+3 {
			n.sag("Nacht: %s steht schon bei %.0f, nichts zu tun", g, pos)
		} else if err := n.fahre(g, a.Richtung); err != nil {
			n.sag("Nacht: %s: %v, neuer Versuch in einer Minute", g, err)
			continue
		} else {
			n.sag("Nacht: %s von %.0f auf %s gefahren", g, pos, a.Richtung)
		}
		n.Lock()
		delete(n.auftraege, g)
		schreibeJSON(n.pfad, n.auftraege)
		n.Unlock()
	}
}

func (n *nacht) laufe() {
	for {
		time.Sleep(time.Minute)
		n.pruefe()
	}
}

func (n *nacht) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/api/nacht", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			jsonAntwort(w, n.liste())
			return
		}
		var a struct {
			Gruppe, Uhrzeit, Richtung string
			Abbrechen                 bool
		}
		if json.NewDecoder(io.LimitReader(r.Body, 512)).Decode(&a) != nil || a.Gruppe == "" {
			http.Error(w, "Gruppe fehlt", 400)
			return
		}
		if a.Abbrechen {
			n.loesche(a.Gruppe)
			jsonAntwort(w, n.liste())
			return
		}
		if _, err := n.setze(a.Gruppe, a.Uhrzeit, a.Richtung); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		jsonAntwort(w, n.liste())
	})
}
