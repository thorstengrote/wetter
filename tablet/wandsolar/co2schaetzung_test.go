package main

import (
	"testing"
	"time"
)

// Geschlossen und Schlaefer: steigt, ganz auf: faellt Richtung draussen.
func TestCO2SchaetzungRichtung(t *testing.T) {
	c := &co2Schaetzung{}
	t0 := time.Date(2026, 10, 12, 23, 0, 0, 0, berlin())
	schlaf := func(time.Time) bool { return true }
	c.rechne(t0, "zu", schlaf, 600)
	c.rechne(t0.Add(4*time.Hour), "zu", schlaf, 0)
	if c.st.Wert < 1150 || c.st.Wert > 1400 { // Gleichgewicht 2087, nach 4 h 1271
		t.Fatalf("4 h zu mit Schlaefer: %.0f", c.st.Wert)
	}
	c.rechne(t0.Add(6*time.Hour), "auf", schlaf, 0)
	if c.st.Wert > 600 {
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
