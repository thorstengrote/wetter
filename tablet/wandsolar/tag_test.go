package main

import (
	"path/filepath"
	"testing"
	"time"
)

// Der Fall vom 26.09.2026: gespeichert sind die Stunden 0 bis 2, dann startet
// das Tablet neu und misst die erste Minute mit dem 23.05.
func TestNeustartMitFalscherUhr(t *testing.T) {
	ort, _ = time.LoadLocation("Europe/Berlin")
	z := &zustand{pfad: filepath.Join(t.TempDir(), "tag.json")}
	z.tag = tagesspeicher{Tag: "2026-09-26", Stunden: map[string]stunde{
		"0": {PV: 0, Haus: 0.3, N: 120}, "1": {Haus: 0.3, N: 120}, "2": {Haus: 0.3, N: 120},
	}}
	z.sichern()

	neu := &zustand{pfad: z.pfad}
	neu.laden()
	if neu.tag.Tag != "2026-09-26" || len(neu.tag.Stunden) != 3 {
		t.Fatalf("gespeicherter Tag verworfen: %+v", neu.tag)
	}

	falsch := time.Date(2026, 5, 23, 15, 20, 0, 0, ort)
	neu.nimm(messwert{Zeit: falsch, PV: 1, Haus: 1})
	if len(neu.tag.Stunden) != 3 || neu.tag.Stunden["15"].N != 0 {
		t.Fatalf("Messung mit falscher Uhr verbucht: %+v", neu.tag.Stunden)
	}
	if neu.letzte == nil {
		t.Fatalf("Augenblickswert fehlt, obwohl die Leistung stimmt")
	}

	richtig := time.Date(2026, 9, 26, 3, 1, 0, 0, ort)
	neu.nimm(messwert{Zeit: richtig, Haus: 1.2})
	if neu.tag.Stunden["3"].N != 1 || len(neu.tag.Stunden) != 4 {
		t.Fatalf("Stunde 3 nicht verbucht: %+v", neu.tag.Stunden)
	}

	morgen := time.Date(2026, 9, 27, 0, 1, 0, 0, ort)
	neu.nimm(messwert{Zeit: morgen, Haus: 0.4})
	if neu.tag.Tag != "2026-09-27" || len(neu.tag.Stunden) != 1 {
		t.Fatalf("neuer Tag nicht begonnen: %+v", neu.tag)
	}
}
