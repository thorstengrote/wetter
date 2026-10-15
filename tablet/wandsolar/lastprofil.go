package main

// Hauslast je Minute, zwei Wochen lang (Wunsch vom 15.10.2026). Daraus
// sollen sich Waschmaschine und Trockner ablesen lassen, ohne dass jemand
// ihre Zeiten notiert: die Waschmaschine heizt mit etwa 2 kW, der Trockner
// (Beko DPU 8306 GXE, Waermepumpe) zieht ueber Stunden einige hundert Watt.
//
// Die Hauslast allein reicht dafuer nicht. Die Waermepumpe der Heizung,
// Spuelmaschine, Backofen und Sandwichmaker ziehen aehnlich viel (Hinweis
// vom 15.10.2026). Darum steht zu jeder Minute dabei, was die Waschkueche
// misst: eine Laststufe zaehlt erst als Waesche, wenn Temperatur oder
// Feuchte dort darauf antworten. Die Entfeuchter stehen mit ihrer Leistung
// dabei, damit man sie abziehen kann.
//
// Je Tag eine Datei, jede Minute eine Zeile angehaengt: zwei Wochen am Stueck
// alle paar Minuten neu zu schreiben, waeren Hunderte MB am Tag auf dem Flash.

import (
	"bufio"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const lastTage = 14

type lastPunkt struct {
	Zeit time.Time `json:"zeit"` // Beginn der Minute
	Haus float64   `json:"haus"` // kW, Mittel der Proben
	Max  float64   `json:"max"`  // kW, hoechste Probe
	// Entfeuchter, kW, 0 aus
	EntfKeller float64 `json:"entf_keller,omitempty"`
	EntfWk     float64 `json:"entf_wk,omitempty"`
	// Waschkueche, letzter Sensorwert, 0 ohne
	WkRH      float64 `json:"wk_rh,omitempty"`
	WkTemp    float64 `json:"wk_temp,omitempty"`
	WkFenster bool    `json:"wk_fenster,omitempty"`
}

// lastExtra: was die Steuerung zur Minute weiss.
type lastExtra struct {
	EntfKeller, EntfWk, WkRH, WkTemp float64
	WkFenster                        bool
}

type lastprofil struct {
	sync.Mutex
	dir     string // lastprofil-JJJJ-MM-TT.jsonl
	punkte  []lastPunkt
	summe   float64
	n       int
	extra   func() lastExtra // nimmt das Schloss der Steuerung, nil in Tests
	offen   lastPunkt        // laufende Minute
	offenDa bool
}

func lastDatei(dir string, t time.Time) string {
	return filepath.Join(dir, "lastprofil-"+t.Format("2006-01-02")+".jsonl")
}

func neuesLastprofil(dir string) *lastprofil {
	l := &lastprofil{dir: dir}
	heute := time.Now().In(ort)
	for i := lastTage; i >= 0; i-- {
		f, err := os.Open(lastDatei(dir, heute.AddDate(0, 0, -i)))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var p lastPunkt
			if json.Unmarshal(sc.Bytes(), &p) == nil {
				l.punkte = append(l.punkte, p)
			}
		}
		f.Close()
	}
	l.aufraeumen(heute)
	return l
}

// aufraeumen: Tagesdateien aelter als lastTage loeschen.
func (l *lastprofil) aufraeumen(t time.Time) {
	alt := "lastprofil-" + t.AddDate(0, 0, -lastTage).Format("2006-01-02") + ".jsonl"
	namen, _ := filepath.Glob(filepath.Join(l.dir, "lastprofil-*.jsonl"))
	for _, n := range namen {
		if b := filepath.Base(n); strings.Compare(b, alt) < 0 {
			os.Remove(n)
		}
	}
}

func (l *lastprofil) anhaengen(p lastPunkt) {
	roh, err := json.Marshal(p)
	if err != nil {
		return
	}
	f, err := os.OpenFile(lastDatei(l.dir, p.Zeit), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	f.Write(append(roh, '\n'))
	f.Close()
}

// nimm: eine Probe des Wechselrichters. Eine Minute wird abgeschlossen,
// sobald die erste Probe der naechsten kommt.
func (l *lastprofil) nimm(m messwert) {
	// Nach einem Neustart steht die Uhr kurz im Jahr 2022.
	if m.Zeit.Year() < 2026 {
		return
	}
	var x lastExtra
	if l.extra != nil {
		x = l.extra() // vor dem eigenen Schloss
	}
	minute := m.Zeit.Truncate(time.Minute)
	l.Lock()
	defer l.Unlock()
	if l.offenDa && !minute.Equal(l.offen.Zeit) {
		l.offen.Haus = math.Round(l.summe/float64(l.n)*1000) / 1000
		l.punkte = append(l.punkte, l.offen)
		l.anhaengen(l.offen)
		l.offenDa = false
		if minute.Day() != l.offen.Zeit.Day() {
			l.aufraeumen(minute)
		}
		grenze := minute.AddDate(0, 0, -lastTage)
		i := 0
		for i < len(l.punkte) && l.punkte[i].Zeit.Before(grenze) {
			i++
		}
		l.punkte = l.punkte[i:]
	}
	if !l.offenDa {
		l.offen = lastPunkt{Zeit: minute}
		l.summe, l.n, l.offenDa = 0, 0, true
	}
	l.summe += m.Haus
	l.n++
	l.offen.Max = math.Max(l.offen.Max, math.Round(m.Haus*1000)/1000)
	l.offen.EntfKeller, l.offen.EntfWk = x.EntfKeller, x.EntfWk
	l.offen.WkRH, l.offen.WkTemp, l.offen.WkFenster = x.WkRH, x.WkTemp, x.WkFenster
}

// bediene: /api/lastprofil?h=48, Standard die letzten 24 Stunden.
func (l *lastprofil) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/api/lastprofil", func(w http.ResponseWriter, r *http.Request) {
		h, err := strconv.Atoi(r.URL.Query().Get("h"))
		if err != nil || h <= 0 {
			h = 24
		}
		ab := time.Now().Add(-time.Duration(h) * time.Hour)
		l.Lock()
		defer l.Unlock()
		aus := []lastPunkt{}
		for _, p := range l.punkte {
			if !p.Zeit.Before(ab) {
				aus = append(aus, p)
			}
		}
		jsonAntwort(w, map[string]any{"punkte": aus})
	})
}
