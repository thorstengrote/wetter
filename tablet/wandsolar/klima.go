package main

// Raumklima aus dem Velux-Sensor (Temperatur, Luftfeuchte, CO2), seit
// 07.10.2026. Er steht im Wohnzimmer und steuert dort die Velux-Lueftung.
// hapwatch hoert ohnehin auf das Gateway und gibt die letzten Werte unter
// /velux/klima aus, wandsolar holt sie alle 5 Minuten und schreibt einen
// Verlauf ueber 14 Tage.
//
// Der Sensor laesst sich ausleihen: Fuer eine Woche in den Spielekeller
// gelegt, zeigt er, wie hoch das CO2 dort nachts steigt, und eicht so die
// Schaetzung aus der Feuchte. Damit die Werte nicht durcheinandergeraten,
// stellt man den Standort in der Steuerung um, und jeder Messpunkt traegt
// ihn mit.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type klimaPunkt struct {
	Zeit time.Time `json:"zeit"`
	Temp float64   `json:"t"`
	RH   float64   `json:"rh"`
	CO2  float64   `json:"co2"`
	Ort  string    `json:"ort"`
}

type raumklima struct {
	sync.Mutex
	basis, cfgPfad, verlaufPfad string
	sag                         func(string, ...any)
	web                         *http.Client
	Standort                    string    `json:"standort"` // wohnzimmer, spielekeller
	Seit                        time.Time `json:"seit"`     // seit wann dort
	verlauf                     []klimaPunkt
	letzter                     *klimaPunkt
	gesichert                   time.Time
	fehler                      string
}

var klimaOrte = map[string]string{"wohnzimmer": "Wohnzimmer", "spielekeller": "Spielekeller"}

func neuesKlima(basis, cfgPfad, verlaufPfad string, sag func(string, ...any)) *raumklima {
	k := &raumklima{basis: basis, cfgPfad: cfgPfad, verlaufPfad: verlaufPfad, sag: sag,
		web: &http.Client{Timeout: 5 * time.Second}, Standort: "wohnzimmer"}
	if roh, err := os.ReadFile(cfgPfad); err == nil {
		json.Unmarshal(roh, k)
	}
	if roh, err := os.ReadFile(verlaufPfad); err == nil {
		json.Unmarshal(roh, &k.verlauf)
	}
	return k
}

func (k *raumklima) lies() {
	if k.basis == "" {
		return
	}
	var a struct {
		Werte map[string]struct {
			Wert float64   `json:"wert"`
			Zeit time.Time `json:"zeit"`
		} `json:"werte"`
	}
	r, err := k.web.Get(strings.TrimRight(k.basis, "/") + "/velux/klima")
	if err == nil {
		err = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&a)
		r.Body.Close()
	}
	k.Lock()
	defer k.Unlock()
	if err != nil {
		if k.fehler == "" {
			k.sag("Raumklima: %v", err)
		}
		k.fehler = err.Error()
		return
	}
	k.fehler = ""
	co2, ok := a.Werte["co2"]
	if !ok || co2.Wert <= 0 {
		return // hapwatch hat noch nichts vom Sensor gehoert
	}
	jetzt := time.Now().In(ort)
	p := klimaPunkt{Zeit: jetzt, Temp: a.Werte["temperatur"].Wert, RH: a.Werte["luftfeuchte"].Wert,
		CO2: co2.Wert, Ort: k.Standort}
	// Der Sensor meldet nur Aenderungen. Ist sein letzter Wert alt, steht er
	// still oder ist ausser Reichweite, dann wird nichts mitgeschrieben.
	neuester := co2.Zeit
	for _, w := range a.Werte {
		if w.Zeit.After(neuester) {
			neuester = w.Zeit
		}
	}
	k.letzter = &p
	if jetzt.Sub(neuester) > 2*time.Hour {
		return
	}
	k.verlauf = append(k.verlauf, p)
	grenze := jetzt.Add(-14 * 24 * time.Hour)
	for len(k.verlauf) > 0 && k.verlauf[0].Zeit.Before(grenze) {
		k.verlauf = k.verlauf[1:]
	}
	if jetzt.Sub(k.gesichert) >= 30*time.Minute {
		schreibeJSON(k.verlaufPfad, k.verlauf)
		k.gesichert = jetzt
	}
}

// co2Keller: CO2 im Spielekeller, wenn der Sensor dort liegt und der Wert
// frisch ist, sonst 0.
func (k *raumklima) co2Keller() float64 {
	k.Lock()
	defer k.Unlock()
	if k.Standort != "spielekeller" || k.letzter == nil || k.letzter.Ort != "spielekeller" ||
		time.Since(k.letzter.Zeit) > 20*time.Minute {
		return 0
	}
	return k.letzter.CO2
}

// hinweise fuer die Wand: Messwoche im Keller vorbei, oder der Sensor ist
// dort ausser Funkreichweite.
func (k *raumklima) hinweise() []string {
	k.Lock()
	defer k.Unlock()
	if k.Standort != "spielekeller" {
		return nil
	}
	var h []string
	if !k.Seit.IsZero() && time.Since(k.Seit) >= 7*24*time.Hour {
		h = append(h, "Messwoche vorbei: Velux-Sensor zurück ins Wohnzimmer, Velux-Automatik wieder einschalten")
	}
	letzte := k.Seit
	for i := len(k.verlauf) - 1; i >= 0; i-- {
		if k.verlauf[i].Ort == "spielekeller" {
			letzte = k.verlauf[i].Zeit
			break
		}
	}
	if !letzte.IsZero() && time.Since(letzte) >= time.Hour {
		h = append(h, "Velux-Sensor im Keller sendet nicht, seit "+letzte.In(ort).Format("15:04"))
	}
	return h
}

func (k *raumklima) laufe() {
	time.Sleep(90 * time.Second) // hapwatch braucht nach dem Start seine Sitzung
	for {
		k.lies()
		time.Sleep(5 * time.Minute)
	}
}

func (k *raumklima) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/api/klima", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var a struct {
				Standort string `json:"standort"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 256)).Decode(&a) != nil || klimaOrte[a.Standort] == "" {
				http.Error(w, "standort wohnzimmer oder spielekeller", 400)
				return
			}
			k.Lock()
			if k.Standort != a.Standort {
				k.Seit = time.Now().In(ort)
			}
			k.Standort = a.Standort
			schreibeJSON(k.cfgPfad, map[string]any{"standort": k.Standort, "seit": k.Seit})
			schreibeJSON(k.verlaufPfad, k.verlauf)
			k.Unlock()
			k.sag("Raumklima: Velux-Sensor steht jetzt im %s", klimaOrte[a.Standort])
			w.WriteHeader(204)
			return
		}
		k.Lock()
		defer k.Unlock()
		a := map[string]any{"standort": k.Standort, "orte": klimaOrte, "seit": k.Seit}
		if k.letzter != nil {
			a["aktuell"] = k.letzter
		}
		if k.fehler != "" {
			a["fehler"] = k.fehler
		}
		// Verlauf nur auf Nachfrage, er ist gross: ?tage=7&ort=spielekeller
		if tage, _ := strconv.Atoi(r.URL.Query().Get("tage")); tage > 0 {
			von := time.Now().AddDate(0, 0, -tage)
			o := r.URL.Query().Get("ort")
			var v []klimaPunkt
			for _, p := range k.verlauf {
				if !p.Zeit.Before(von) && (o == "" || p.Ort == o) {
					v = append(v, p)
				}
			}
			a["verlauf"] = v
		}
		jsonAntwort(w, a)
	})
}
