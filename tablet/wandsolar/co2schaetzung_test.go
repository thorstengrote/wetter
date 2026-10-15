package main

import (
	"testing"
	"time"
)

// Geschlossen und Schlaefer: steigt, ganz auf: faellt Richtung draussen.
func TestCO2SchaetzungRichtung(t *testing.T) {
	c := &co2Schaetzung{}
	t0 := time.Date(2026, 10, 12, 23, 0, 0, 0, berlin())
	schlaf := func(time.Time) float64 { return co2Quelle }
	c.rechne(t0, "zu", schlaf, 600)
	c.rechne(t0.Add(4*time.Hour), "zu", schlaf, 0)
	if c.st.Wert < 1100 || c.st.Wert > 1500 { // steigt deutlich
		t.Fatalf("4 h zu mit Schlaefer: %.0f", c.st.Wert)
	}
	vorher := c.st.Wert
	c.rechne(t0.Add(6*time.Hour), "auf", schlaf, 0)
	if c.st.Wert > vorher-200 || c.st.Wert > 900 { // Gleichgewicht offen mit Schlaefer 706
		t.Fatalf("2 h ganz auf: %.0f", c.st.Wert)
	}
}

func TestNachtVergleich(t *testing.T) {
	t0 := time.Date(2026, 10, 12, 23, 0, 0, 0, berlin())
	var v []co2Punkt
	for i := 0; i < 24; i++ {
		v = append(v, co2Punkt{Zeit: t0.Add(time.Duration(i) * 5 * time.Minute), Schaetz: 900, Echt: 1000, Schlaf: true})
	}
	v = append(v, co2Punkt{Zeit: t0.Add(3 * time.Hour), Schaetz: 500, Echt: 500})
	r := nachtVergleich(v, t0.Add(3*time.Hour))
	if r.N != 24 || r.Abw != 100 || r.MaxEcht != 1000 {
		t.Fatalf("%+v", r)
	}
}

// Nacht zum 15.10.2026: Fenster gemeldet offen, Feuchte steigt mit dem CO2,
// draussen trockener. Das Modell muss mit zu rechnen und es einmal melden.
func TestFensterPruefungFeuchte(t *testing.T) {
	for _, fall := range []struct {
		name   string
		rhJe2h float64 // Prozentpunkte
		zu     bool
	}{{"steigt", 1, true}, {"faellt", -1, false}} {
		var meldungen int
		c := &co2Schaetzung{pfad: t.TempDir() + "/cs.json", sag: func(string, ...any) { meldungen++ }}
		t0 := time.Date(2026, 10, 15, 0, 0, 0, 0, berlin())
		jetzt := t0
		c.echt = func() float64 { return 0 }
		c.fenster = func() string { return "auf" }
		c.schlaf = func(time.Time) bool { return true }
		c.raum = func() (float64, float64, bool, bool) {
			return 59 + fall.rhJe2h*jetzt.Sub(t0).Hours()/2, 20.3, false, true
		}
		c.aussen = func(time.Time) (float64, bool) { return 7.7, true }
		for ; jetzt.Before(t0.Add(4 * time.Hour)); jetzt = jetzt.Add(time.Minute) {
			c.schritt(jetzt)
		}
		p := c.st.Verlauf[len(c.st.Verlauf)-1]
		if (p.FensterLuft == "zu") != fall.zu || meldungen != map[bool]int{true: 1, false: 0}[fall.zu] {
			t.Fatalf("%s: fenster_luft %q, %d Meldungen", fall.name, p.FensterLuft, meldungen)
		}
	}
}
