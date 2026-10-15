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

// Nach einem Neustart mit falscher Uhr: die Dateien kommen mit der ersten
// gueltigen Probe.
func TestLastprofilSpaetLaden(t *testing.T) {
	dir := t.TempDir()
	l := neuesLastprofil(dir)
	t0 := time.Now().In(ort).Truncate(time.Minute).Add(-10 * time.Minute)
	for i := 0; i < 5; i++ {
		l.nimm(messwert{Zeit: t0.Add(time.Duration(i) * 30 * time.Second), Haus: 0.3})
	}
	neu := &lastprofil{dir: dir} // wie mit Uhr im Jahr 2022 gestartet
	neu.nimm(messwert{Zeit: time.Date(2022, 1, 1, 0, 0, 0, 0, ort), Haus: 0.3})
	if len(neu.roh) != 0 {
		t.Fatal("mit falscher Uhr geladen")
	}
	neu.nimm(messwert{Zeit: t0.Add(5 * time.Minute), Haus: 0.3})
	if len(neu.roh) != 6 || len(neu.punkte) != 2 {
		t.Fatalf("%d Proben, %d Minuten", len(neu.roh), len(neu.punkte))
	}
}
