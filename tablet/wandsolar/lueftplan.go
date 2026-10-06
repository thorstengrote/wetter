package main

// Wochenplan der Lueftung, aus der Vorhersage gerechnet (Wunsch vom
// 07.10.2026, wie beim Entfeuchter). Er nimmt an, dass der Taupunkt drinnen
// bleibt, wo er gerade ist. Das stimmt nicht genau, der Keller aendert sich
// aber viel langsamer als das Wetter, und fuer die Frage "an welchem Tag
// lohnt es sich" reicht es. Gewaehlt wird mit denselben Regeln wie in
// entscheide: erlaubte Zeit samt Feiertagen und Ferien, Taupunktabstand,
// Regen, Boeen, Frost, Kaeltestrafe, hoechstens MaxJeTag am Tag mit
// AbstandStd Pause dazwischen. Heute zaehlt erst ab der laufenden Stunde,
// und schon gelaufene Lueftungen gehen vom Tagesbudget ab.

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
	Erstellt     time.Time     `json:"erstellt"`
	TaupunktIn   float64       `json:"taupunkt_innen"`
	OhneInnen    bool          `json:"ohne_innen,omitempty"`
	Stunden      []lueftStunde `json:"stunden"`
	AbstandK     float64       `json:"abstand_k"`
	DauerMin     float64       `json:"dauer_min"`
	DauerKaltMin float64       `json:"dauer_kalt_min"`
}

// planeWoche: unter l.Lock aufrufen.
func (l *lueftung) planeWoche(e lueftEingang) lueftPlan {
	c, t := l.cfg, e.Jetzt
	p := lueftPlan{Erstellt: t, AbstandK: c.AbstandK, DauerMin: c.MaxMin, DauerKaltMin: c.MaxMinKalt}
	if e.Innen != nil {
		p.TaupunktIn = taupunkt(e.Innen.Temp, e.Innen.RH)
	} else if n := len(l.st.Laeufe); n > 0 {
		p.TaupunktIn, p.OhneInnen = l.st.Laeufe[n-1].TaupunktIn, true
	} else {
		p.OhneInnen = true
		return p
	}
	start := tagesAnfang(t)
	for d := 0; d < 7; d++ {
		tag := start.AddDate(0, 0, d)
		ende := c.ende(tag.Add(12 * time.Hour))
		letzte := ende.Add(-time.Duration(c.MaxMin * float64(time.Minute)))
		var kand []int
		for h := 0; h < 24; h++ {
			z := tag.Add(time.Duration(h) * time.Hour)
			s := lueftStunde{Zeit: z, Erlaubt: c.erlaubt(z) && !z.After(letzte), Ruhe: !c.erlaubt(z)}
			if w, ok := luftZu(e.Luft, z); ok {
				s.Daten, s.Temp, s.Taupunkt, s.Regen = true, w.Temp, w.Taupunkt, w.Regen
				s.Abstand = math.Round((p.TaupunktIn-w.Taupunkt)*10) / 10
				pkt, warum := l.eignung(p.TaupunktIn, w, c.AbstandK)
				s.Punkte, s.Warum, s.Geeignet = math.Round(pkt*10)/10, warum, warum == ""
			}
			vorbei := z.Add(time.Hour).Before(t) || (d == 0 && z.Before(t.Truncate(time.Hour)))
			if s.Erlaubt && s.Geeignet && !vorbei {
				kand = append(kand, len(p.Stunden))
			}
			p.Stunden = append(p.Stunden, s)
		}
		budget := c.MaxJeTag
		var gewaehlt []time.Time
		if d == 0 {
			budget -= l.heute(t)
			if le := l.letztesEnde(); !le.IsZero() {
				gewaehlt = append(gewaehlt, le)
			}
		}
		sort.SliceStable(kand, func(i, j int) bool { return p.Stunden[kand[i]].Punkte > p.Stunden[kand[j]].Punkte })
		pause := time.Duration(c.AbstandStd * float64(time.Hour))
		for _, i := range kand {
			if budget <= 0 {
				break
			}
			z, frei := p.Stunden[i].Zeit, true
			for _, g := range gewaehlt {
				if ab := z.Sub(g); ab < pause && ab > -pause {
					frei = false
				}
			}
			if frei && !z.Before(l.st.SperreBis.Truncate(time.Hour)) {
				p.Stunden[i].Geplant = true
				gewaehlt = append(gewaehlt, z)
				budget--
			}
		}
	}
	return p
}
