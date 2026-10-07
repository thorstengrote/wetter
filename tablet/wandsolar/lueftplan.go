package main

// Wochenplan der Lueftung, aus der Vorhersage gerechnet. Er nimmt an, dass
// der Taupunkt drinnen bleibt, wo er gerade ist. Das stimmt nicht genau, der
// Keller aendert sich aber viel langsamer als das Wetter, und fuer die Frage
// "wann wird voraussichtlich gelueftet" reicht es.
//
// Seit 07.10.2026 lueftet die Automatik, sobald die Messwerte es hergeben,
// ohne Uhrzeiten und Budgets. Der Plan markiert deshalb jede Stunde, in der
// die Vorhersage genug Taupunktabstand und gutes Wetter erwarten laesst,
// sofern die Raumluft gerade feuchter ist als das Ziel. CO2 laesst sich
// nicht vorhersagen und fehlt im Plan.

import (
	"math"
	"time"
)

type lueftStunde struct {
	Zeit     time.Time `json:"zeit"`
	Ruhe     bool      `json:"ruhe,omitempty"` // im Raum wird geschlafen
	Daten    bool      `json:"daten"`
	Abstand  float64   `json:"abstand"` // Taupunkt drinnen minus draussen
	Punkte   float64   `json:"punkte"`
	Geeignet bool      `json:"geeignet"`
	Warum    string    `json:"warum,omitempty"`
	Geplant  bool      `json:"geplant,omitempty"`
	Temp     float64   `json:"temp"`
	Taupunkt float64   `json:"taupunkt"`
	Regen    float64   `json:"regen"`
}

type lueftPlan struct {
	Erstellt   time.Time     `json:"erstellt"`
	TaupunktIn float64       `json:"taupunkt_innen"`
	OhneInnen  bool          `json:"ohne_innen,omitempty"`
	Trocken    bool          `json:"trocken,omitempty"` // Raumluft unter dem Ziel, keine Feuchte-Lueftung erwartet
	Stunden    []lueftStunde `json:"stunden"`
	AbstandK   float64       `json:"abstand_k"`
}

// planeWoche: unter l.Lock aufrufen.
func (l *lueftung) planeWoche(e lueftEingang) lueftPlan {
	c, t := l.cfg, e.Jetzt
	p := lueftPlan{Erstellt: t, AbstandK: c.AbstandK}
	switch {
	case e.Innen != nil:
		p.TaupunktIn = taupunkt(e.Innen.Temp, e.Innen.RH)
		p.Trocken = e.Innen.RH <= e.ZielRH
	case len(l.st.Laeufe) > 0:
		p.TaupunktIn, p.OhneInnen = l.st.Laeufe[len(l.st.Laeufe)-1].TaupunktIn, true
	default:
		p.OhneInnen = true
		return p
	}
	start := tagesAnfang(t)
	for h := 0; h < 7*24; h++ {
		z := start.Add(time.Duration(h) * time.Hour)
		s := lueftStunde{Zeit: z}
		if l.wach != nil {
			s.Ruhe = !l.wach(z)
		}
		if w, ok := luftZu(e.Luft, z); ok {
			s.Daten, s.Temp, s.Taupunkt, s.Regen = true, w.Temp, w.Taupunkt, w.Regen
			s.Abstand = math.Round((p.TaupunktIn-w.Taupunkt)*10) / 10
			pkt, warum := l.eignung(p.TaupunktIn, w, c.AbstandK)
			s.Punkte, s.Warum, s.Geeignet = math.Round(pkt*10)/10, warum, warum == ""
		}
		s.Geplant = s.Geeignet && !p.Trocken && !z.Before(t.Truncate(time.Hour))
		p.Stunden = append(p.Stunden, s)
	}
	return p
}
