package main

// Heizung des Spielekellers (seit 07.10.2026). Thorsten hat den Zeitplan der
// FRITZ!Box fuer das Ventil abgeschaltet, die Solltemperatur kommt jetzt von
// hier:
//
//   - in der Wachzeit Komfort, in der Schlafzeit Nacht
//   - waehrend gelueftet wird Lueften (8 Grad), damit nicht zum Fenster
//     hinaus geheizt wird, danach sofort wieder der Wert davor
//
// Die Schlafzeiten sind die von Thorstens Sohn: werktags bis 9:30, am
// Wochenende bis 10:30, ab 22:30. Feiertage in NRW und Ferien zaehlen wie
// Sonntag. Das Ferienende stellt man hier ein, es gilt auch fuer den
// Entfeuchter.
//
// Gesetzt wird nur, wenn das Ventil etwas anderes will, und denselben Wert
// hoechstens alle 30 Minuten noch einmal, falls die Box ihn nicht uebernimmt.
// Ist die Zentralheizung aus, kostet das nichts und bewirkt nichts.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type heizCfg struct {
	Modus     string      `json:"modus"`  // aus, probe, scharf
	Ventil    string      `json:"ventil"` // Teil des Namens in der FRITZ!Box
	Wach      [7]zeitraum `json:"wach"`   // Mo bis So
	FerienBis string      `json:"ferien_bis"`
	Komfort   float64     `json:"komfort"`
	Nacht     float64     `json:"nacht"`
	Lueften   float64     `json:"lueften"`
}

func standardHeizung() heizCfg {
	w, we := zeitraum{9.5, 22.5}, zeitraum{10.5, 22.5}
	return heizCfg{Modus: "scharf", Ventil: "spielkeller", Wach: [7]zeitraum{w, w, w, w, w, we, we},
		Komfort: 20, Nacht: 18, Lueften: 8}
}

type heizRegel struct {
	sync.Mutex
	cfgPfad     string
	cfg         heizCfg
	sag         func(string, ...any)
	ziel        float64
	grund       string
	gesetzt     time.Time
	gesetztWert float64

	ventil    func(name string) *ventil
	setzeSoll func(ain string, grad float64) error
	lueftet   func() bool
}

func neueHeizRegel(cfgPfad string, sag func(string, ...any)) *heizRegel {
	h := &heizRegel{cfgPfad: cfgPfad, cfg: standardHeizung(), sag: sag}
	if roh, err := os.ReadFile(cfgPfad); err == nil {
		json.Unmarshal(roh, &h.cfg)
	} else {
		schreibeJSON(cfgPfad, h.cfg)
	}
	setzeFerien(h.cfg.FerienBis)
	return h
}

func (c heizCfg) wach(t time.Time) bool {
	z := c.Wach[tagIndex(t)]
	h := float64(t.Hour()) + float64(t.Minute())/60
	return h >= z.Von && h < z.Bis
}

// wach: fuer andere Module, etwa die Ruhezeiten in der Anzeige.
func (h *heizRegel) wach(t time.Time) bool {
	h.Lock()
	defer h.Unlock()
	return h.cfg.wach(t)
}

// schlafzeiten: die Schlafzeiten zwischen von und bis als Paare aus
// Unix-Millisekunden, in Viertelstunden gerechnet, fuer die Zeitleiste.
func (c heizCfg) schlafzeiten(von, bis time.Time) [][2]int64 {
	var aus [][2]int64
	t := von.Truncate(15 * time.Minute)
	for ; t.Before(bis); t = t.Add(15 * time.Minute) {
		if c.wach(t) {
			continue
		}
		ms := t.UnixMilli()
		if n := len(aus); n > 0 && aus[n-1][1] == ms {
			aus[n-1][1] = ms + 15*60*1000
		} else {
			aus = append(aus, [2]int64{ms, ms + 15*60*1000})
		}
	}
	return aus
}

// zielFuer: Solltemperatur und Grund.
func (c heizCfg) zielFuer(t time.Time, lueftet bool) (float64, string) {
	switch {
	case lueftet:
		return c.Lueften, "es wird gelüftet"
	case c.wach(t):
		return c.Komfort, "Wachzeit"
	}
	return c.Nacht, "Schlafzeit"
}

func (h *heizRegel) schritt(t time.Time) {
	h.Lock()
	c := h.cfg
	h.Unlock()
	if c.Modus == "aus" {
		h.merke(0, "Heizregel aus")
		return
	}
	lueftet := h.lueftet != nil && h.lueftet()
	ziel, grund := c.zielFuer(t, lueftet)
	v := h.ventil(c.Ventil)
	if v == nil {
		h.merke(ziel, grund+", Ventil nicht gefunden")
		return
	}
	if math.Abs(v.Soll-ziel) < 0.3 {
		h.merke(ziel, grund)
		return
	}
	if c.Modus != "scharf" {
		h.merke(ziel, fmt.Sprintf("%s, Probe: würde %.1f statt %.1f °C setzen", grund, ziel, v.Soll))
		return
	}
	h.Lock()
	nochmal := h.gesetztWert == ziel && t.Sub(h.gesetzt) < 30*time.Minute
	h.Unlock()
	if nochmal {
		h.merke(ziel, grund+", wartet auf die FRITZ!Box")
		return
	}
	if err := h.setzeSoll(v.AIN, ziel); err != nil {
		h.merke(ziel, grund+", Ventil nicht gestellt: "+err.Error())
		return
	}
	h.sag("Heizung Spielkeller: %.1f °C, %s", ziel, grund)
	h.Lock()
	h.gesetzt, h.gesetztWert = t, ziel
	h.Unlock()
	h.merke(ziel, grund)
}

func (h *heizRegel) merke(ziel float64, grund string) {
	h.Lock()
	h.ziel, h.grund = ziel, grund
	h.Unlock()
}

func (h *heizRegel) laufe() {
	time.Sleep(3 * time.Minute) // erst Box und Lueftung
	for {
		h.schritt(time.Now().In(ort))
		time.Sleep(time.Minute)
	}
}

func pruefeHeizCfg(c heizCfg) string {
	if !strings.Contains(" aus probe scharf ", " "+c.Modus+" ") || c.Modus == "" {
		return "Modus aus, Probe oder scharf"
	}
	for i, z := range c.Wach {
		if z.Von < 0 || z.Bis > 24 || z.Von > z.Bis {
			return fmt.Sprintf("Wachzeit am %s unstimmig", []string{"Mo", "Di", "Mi", "Do", "Fr", "Sa", "So"}[i])
		}
	}
	for _, v := range []float64{c.Komfort, c.Nacht, c.Lueften} {
		if v < 8 || v > 28 {
			return "Temperaturen 8 bis 28 °C"
		}
	}
	if c.Ventil == "" {
		return "Ventilname fehlt"
	}
	if c.FerienBis != "" {
		if _, err := time.Parse("2006-01-02", c.FerienBis); err != nil {
			return "Ferienende als Datum, etwa 2026-10-24"
		}
	}
	return ""
}

func (h *heizRegel) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/api/heizregel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			jetzt := time.Now().In(ort)
			h.Lock()
			a := map[string]any{"cfg": h.cfg, "ziel": h.ziel, "grund": h.grund,
				"wach_jetzt": h.cfg.wach(jetzt), "schlaf": h.cfg.schlafzeiten(jetzt.AddDate(0, 0, -3), jetzt.AddDate(0, 0, 8))}
			h.Unlock()
			jsonAntwort(w, a)
			return
		}
		var c heizCfg
		if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&c) != nil {
			http.Error(w, "unbrauchbar", 400)
			return
		}
		if fehler := pruefeHeizCfg(c); fehler != "" {
			http.Error(w, fehler, 400)
			return
		}
		h.Lock()
		h.cfg = c
		h.gesetzt = time.Time{} // neue Werte sofort setzen
		schreibeJSON(h.cfgPfad, c)
		h.Unlock()
		setzeFerien(c.FerienBis)
		h.sag("Heizung Spielkeller: Einstellungen geändert, Modus %s", c.Modus)
		w.WriteHeader(204)
	})
}
