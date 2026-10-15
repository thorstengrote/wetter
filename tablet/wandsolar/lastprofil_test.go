package main

import (
	"testing"
	"time"
)

// Proben alle 30 Sekunden ergeben Minutenmittel, angehaengt an die
// Tagesdatei und nach einem Neustart wieder gelesen.
func TestLastprofilMinuten(t *testing.T) {
	dir := t.TempDir()
	l := neuesLastprofil(dir)
	t0 := time.Now().In(ort).Truncate(time.Minute).Add(-10 * time.Minute)
	for i := 0; i < 7; i++ { // 3,5 Minuten
		haus := 0.4
		if i%2 == 1 {
			haus = 2.4 // Heizstab
		}
		l.nimm(messwert{Zeit: t0.Add(time.Duration(i) * 30 * time.Second), Haus: haus})
	}
	if len(l.punkte) != 3 || l.punkte[0].Haus != 1.4 || l.punkte[0].Max != 2.4 {
		t.Fatalf("Punkte: %+v", l.punkte)
	}
	if n := len(l.roh); n != 7 || l.roh[1].W != 2400 {
		t.Fatalf("Rohdaten: %+v", l.roh)
	}
	neu := neuesLastprofil(dir)
	if len(neu.punkte) != 3 || len(neu.roh) != 7 {
		t.Fatalf("nach Neustart %d Punkte, %d Proben", len(neu.punkte), len(neu.roh))
	}
}
