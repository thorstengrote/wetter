package main

// Wochenplan der Lueftung, aus der Vorhersage gerechnet (Wunsch vom
// 07.10.2026, wie beim Entfeuchter). Er nimmt an, dass der Taupunkt drinnen
// bleibt, wo er gerade ist. Das stimmt nicht genau, der Keller aendert sich
// aber viel langsamer als das Wetter, und fuer die Frage "an welchem Tag
// lohnt es sich" reicht es. Gewaehlt wird mit denselben Regeln wie in
// entscheide: erlaubte Zeit samt Feiertagen und Ferien, Taupunktabstand,
// Regen, Boeen, Frost, Kaeltestrafe, hoechstens MaxJeTag am Tag mit
// AbstandStd Pause dazwischen, nachts NachtMaxJe. Heute zaehlt erst ab der
// laufenden Stunde, und schon gelaufene Lueftungen gehen vom Budget ab.

import (
	"math"
	"sort"
	"time"
)

type lueftStunde struct {
	Zeit     time.Time `json:"zeit"`
	Erlaubt  bool      `json:"erlaubt"`
	Ruhe     bool      `json:"ruhe,omitempty"` // ausserhalb der erlaubten Zeit, im Raum wird geschlafen
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
	Erstellt      time.Time     `json:"erstellt"`
	TaupunktIn    float64       `json:"taupunkt_innen"`
	OhneInnen     bool          `json:"ohne_innen,omitempty"`
	Stunden       []lueftStunde `json:"stunden"`
	AbstandK      float64       `json:"abstand_k"`
	DauerMin      float64       `json:"dauer_min"`
	DauerKaltMin  float64       `json:"dauer_kalt_min"`
	DauerNachtMin float64       `json:"dauer_nacht_min"`
}

// planeWoche: unter l.Lock aufrufen. Jeder Tag und jede Nacht ist ein
// Fenster mit eigenem Budget, die Pause zwischen zwei Lueftungen gilt ueber
// die Grenzen hinweg.
func (l *lueftung) planeWoche(e lueftEingang) lueftPlan {
	c, t := l.cfg, e.Jetzt
	p := lueftPlan{Erstellt: t, AbstandK: c.AbstandK, DauerMin: c.MaxMin, DauerKaltMin: c.MaxMinKalt,
		DauerNachtMin: c.NachtMaxMin}
	if e.Innen != nil {
		p.TaupunktIn = taupunkt(e.Innen.Temp, e.Innen.RH)
	} else if n := len(l.st.Laeufe); n > 0 {
		p.TaupunktIn, p.OhneInnen = l.st.Laeufe[n-1].TaupunktIn, true
	} else {
		p.OhneInnen = true
		return p
	}
	type fenster struct {
		erste  int
		budget int
		kand   []int
	}
	var reihe []string
	fen := map[string]*fenster{}
	start := tagesAnfang(t)
	for h := 0; h < 7*24; h++ {
		z := start.Add(time.Duration(h) * time.Hour)
		tag := c.erlaubt(z)
		s := lueftStunde{Zeit: z, Ruhe: !tag}
		var id string
		if tag {
			s.Erlaubt = c.NachtMaxJe > 0 || !z.After(c.ende(z).Add(-time.Duration(c.MaxMin*float64(time.Minute))))
			id = "t" + z.Format("2006-01-02")
		} else {
			s.Erlaubt = c.NachtMaxJe > 0
			von, _ := c.nacht(z)
			id = "n" + von.Format("2006-01-02T15:04")
		}
		if w, ok := luftZu(e.Luft, z); ok {
			s.Daten, s.Temp, s.Taupunkt, s.Regen = true, w.Temp, w.Taupunkt, w.Regen
			s.Abstand = math.Round((p.TaupunktIn-w.Taupunkt)*10) / 10
			pkt, warum := l.eignung(p.TaupunktIn, w, c.AbstandK)
			s.Punkte, s.Warum, s.Geeignet = math.Round(pkt*10)/10, warum, warum == ""
		}
		f := fen[id]
		if f == nil {
			f = &fenster{erste: h, budget: c.MaxJeTag}
			if !tag {
				f.budget = c.NachtMaxJe
			}
			// Was im laufenden Tag oder in der laufenden Nacht schon gelueftet
			// wurde, geht ab.
			if z.Before(t.Add(time.Hour)) {
				if tag && tagesAnfang(z).Equal(tagesAnfang(t)) {
					f.budget -= l.heute(t)
				} else if !tag && !c.erlaubt(t) {
					if v1, _ := c.nacht(t); v1.Format("2006-01-02T15:04") == id[1:] {
						f.budget -= l.inNacht(t)
					}
				}
			}
			fen[id] = f
			reihe = append(reihe, id)
		}
		if s.Erlaubt && s.Geeignet && !z.Before(t.Truncate(time.Hour)) && !z.Before(l.st.SperreBis.Truncate(time.Hour)) {
			f.kand = append(f.kand, len(p.Stunden))
		}
		p.Stunden = append(p.Stunden, s)
	}
	var gewaehlt []time.Time
	if le := l.letztesEnde(); !le.IsZero() {
		gewaehlt = append(gewaehlt, le)
	}
	pause := time.Duration(c.AbstandStd * float64(time.Hour))
	for _, id := range reihe {
		f := fen[id]
		sort.SliceStable(f.kand, func(i, j int) bool { return p.Stunden[f.kand[i]].Punkte > p.Stunden[f.kand[j]].Punkte })
		for _, i := range f.kand {
			if f.budget <= 0 {
				break
			}
			z, frei := p.Stunden[i].Zeit, true
			for _, g := range gewaehlt {
				if ab := z.Sub(g); ab < pause && ab > -pause {
					frei = false
				}
			}
			if frei {
				p.Stunden[i].Geplant = true
				gewaehlt = append(gewaehlt, z)
				f.budget--
			}
		}
	}
	return p
}
