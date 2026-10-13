package main

import (
	"strings"
	"testing"
	"time"
)

func wkLuft(t0 time.Time, f func(h int) luftWert) map[int64]luftWert {
	m := map[int64]luftWert{}
	for i := -12; i < 48; i++ {
		x := t0.Truncate(time.Hour).Add(time.Duration(i) * time.Hour)
		m[x.Unix()] = f(x.In(ort).Hour())
	}
	return m
}

func wkTest(t0 time.Time, rh float64, luft map[int64]luftWert, offen bool, seit time.Time) (*wkLueften, wkLage) {
	cfg := standardWaschkueche()
	cfg.FeuchteOben, cfg.FeuchteUnten = 60, 55
	w := &wkLueften{sag: func(string, ...any) {}}
	return w, wkLage{cfg: cfg, innen: &messFeuchte{Temp: 19, RH: rh, Zeit: t0}, frisch: true,
		kontakt: kontaktWert{Offen: offen, Seit: seit, Zeit: t0}, luft: luft}
}

func TestWkEmpfehlung(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 11, 0, 0, 0, ort)
	trocken := wkLuft(t0, func(h int) luftWert { return luftWert{Temp: 12, Taupunkt: 5} }) // drinnen 19 °C/60 % -> Taupunkt 11
	w, l := wkTest(t0, 60, trocken, false, time.Time{})
	h := w.empfehlung(t0, l)
	if h == nil || !strings.Contains(h.Text, "jetzt") {
		t.Fatalf("trockene Luft, lange nicht gelueftet: %+v", h)
	}
	// Vor zwei Tagen gelueftet und nicht sehr feucht: nichts
	w.s.LetztesLueften = t0.Add(-48 * time.Hour)
	l.innen.RH = 58
	if h := w.empfehlung(t0, l); h != nil {
		t.Fatalf("zu frueh wieder: %+v", h)
	}
	l.innen.RH = 61 // ueber der Obergrenze: auch frueher
	if h := w.empfehlung(t0, l); h == nil {
		t.Fatal("ueber der Obergrenze keine Empfehlung")
	}
	l.innen.RH = 60
	// Nachts nicht empfehlen
	w.s.LetztesLueften = time.Time{}
	nacht := time.Date(2026, 10, 14, 23, 0, 0, 0, ort)
	if h := w.empfehlung(nacht, l); h != nil {
		t.Fatalf("nachts: %+v", h)
	}
	// Draussen feucht: nichts
	feucht := wkLuft(t0, func(h int) luftWert { return luftWert{Temp: 12, Taupunkt: 10} })
	w, l = wkTest(t0, 60, feucht, false, time.Time{})
	if h := w.empfehlung(t0, l); h != nil {
		t.Fatalf("feuchte Aussenluft: %+v", h)
	}
}

func TestWkSchliessen(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 18, 0, 0, 0, ort)
	// bis 20 Uhr trocken, danach feucht
	luft := wkLuft(t0, func(h int) luftWert {
		if h >= 18 && h < 20 {
			return luftWert{Temp: 10, Taupunkt: 5}
		}
		return luftWert{Temp: 9, Taupunkt: 11}
	})
	w, l := wkTest(t0, 60, luft, true, t0.Add(-3*time.Hour))
	w.s.OffenSeit = t0.Add(-3 * time.Hour)
	h := w.offenHinweis(t0, l)
	if h.Art != "offen" || !strings.Contains(h.Unter, "20 Uhr") || h.Text != "Waschküche offen seit 3 h" {
		t.Fatalf("offen: %+v", h)
	}
	t1 := t0.Add(2*time.Hour + 5*time.Minute)
	h = w.offenHinweis(t1, l)
	if h.Art != "schliessen" || !strings.Contains(h.Unter, "feuchter") {
		t.Fatalf("schliessen: %+v", h)
	}
}

// Raumluft bestaetigt das offene Fenster, oder sie tut es nicht.
func TestWkPruefung(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 11, 0, 0, 0, ort)
	kalt := wkLuft(t0, func(h int) luftWert { return luftWert{Temp: 8, Taupunkt: 4} })
	w, l := wkTest(t0, 60, kalt, true, t0)
	w.offen(t0, l)
	l.innen = &messFeuchte{Temp: 18.4, RH: 58}
	w.offen(t0.Add(40*time.Minute), l)
	if !w.s.Verifiziert {
		t.Fatal("Abkuehlung nicht als offen erkannt")
	}
	w, l = wkTest(t0, 60, kalt, true, t0)
	w.offen(t0, l)
	w.offen(t0.Add(70*time.Minute), l)
	if w.s.Verifiziert || !w.s.Zweifel {
		t.Fatalf("Raum unveraendert, trotzdem geglaubt: %+v", w.s)
	}
	if h := w.offenHinweis(t0.Add(70*time.Minute), l); !strings.Contains(h.Unter, "Kontakt") {
		t.Fatalf("kein Zweifel im Hinweis: %+v", h)
	}
}
