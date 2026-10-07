package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func testLueftung() *lueftung {
	l := &lueftung{cfg: standardLueftung(), sag: func(string, ...any) {}}
	l.st.Phase = "zu"
	return l
}

// Mittwoch, 7. Oktober 2026
func mittwoch(h, m int) time.Time { return time.Date(2026, 10, 7, h, m, 0, 0, ort) }

// luftTag: jede Stunde von zwei Tagen ab Tagesbeginn gleich, ausser den angegebenen.
func luftTag(t time.Time, normal luftWert, ausnahmen map[int]luftWert) map[int64]luftWert {
	m := map[int64]luftWert{}
	anf := tagesAnfang(t)
	for h := 0; h < 48; h++ {
		w := normal
		if a, ok := ausnahmen[h]; ok {
			w = a
		}
		m[anf.Add(time.Duration(h)*time.Hour).Unix()] = w
	}
	return m
}

func eingang(t time.Time, temp, rh float64, luft map[int64]luftWert) lueftEingang {
	return lueftEingang{Jetzt: t, Innen: &messFeuchte{Temp: temp, RH: rh, Zeit: t}, ZielRH: 53,
		Luft: luft, KF: "zu", KFDa: true}
}

// drinnen 20 Grad, 60 %: Taupunkt rund 12 Grad
const tpInnen = 12.0

func TestTaupunkt(t *testing.T) {
	// 20 Grad, 50 Prozent: Taupunkt rund 9,3 Grad, rund 8,6 g/m3
	if tp := taupunkt(20, 50); math.Abs(tp-9.3) > 0.1 {
		t.Errorf("Taupunkt %.2f", tp)
	}
	if w := wasser(20, 50); math.Abs(w-8.6) > 0.1 {
		t.Errorf("Wasser %.2f", w)
	}
	if tp := taupunkt(20, 60); math.Abs(tp-tpInnen) > 0.1 {
		t.Errorf("Taupunkt bei 60 %%: %.2f", tp)
	}
}

func TestLueftungOeffnetNachMesswertenRundUmDieUhr(t *testing.T) {
	for _, zeit := range []time.Time{mittwoch(14, 0), mittwoch(3, 0), mittwoch(23, 30)} {
		l := testLueftung()
		luft := luftTag(zeit, luftWert{Temp: 15, Taupunkt: 10}, nil) // 2 K: zu wenig
		if a, g, _ := l.entscheide(eingang(zeit, 20, 60, luft)); a != "" || !strings.Contains(g, "zu feucht") {
			t.Errorf("%v bei 2 K: %q %q", zeit, a, g)
		}
		luft = luftTag(zeit, luftWert{Temp: 15, Taupunkt: 8}, nil) // 4 K
		if a, g, _ := l.entscheide(eingang(zeit, 20, 60, luft)); a != "auf" {
			t.Errorf("%v bei 4 K: %q %q", zeit, a, g)
		}
	}
}

func TestLueftungSperren(t *testing.T) {
	t0 := mittwoch(14, 0)
	for _, f := range []struct {
		w    luftWert
		temp float64
		rh   float64
		soll string
	}{
		{luftWert{Temp: 15, Taupunkt: 2, Regen: 0.5}, 20, 60, "Regen"},
		{luftWert{Temp: 15, Taupunkt: 2, Boeen: 60}, 20, 60, "Böen"},
		{luftWert{Temp: -2, Taupunkt: -8}, 20, 60, "zu kalt"},
		{luftWert{Temp: 15, Taupunkt: 2}, 20, 52, "trocken genug"},
		{luftWert{Temp: 15, Taupunkt: 2}, 17.5, 70, "zu kühl"},
	} {
		l := testLueftung()
		if a, g, _ := l.entscheide(eingang(t0, f.temp, f.rh, luftTag(t0, f.w, nil))); a != "" || !strings.Contains(g, f.soll) {
			t.Errorf("%s: %q %q", f.soll, a, g)
		}
	}
	// Pause nach der letzten Lueftung und Tagesgrenze, wenn eine gesetzt ist
	luft := luftTag(t0, luftWert{Temp: 15, Taupunkt: 2}, nil)
	l := testLueftung()
	l.st.Laeufe = []lueftLauf{{Von: t0.Add(-time.Hour), Bis: t0.Add(-10 * time.Minute)}}
	if a, g, _ := l.entscheide(eingang(t0, 20, 60, luft)); a != "" || !strings.Contains(g, "Pause") {
		t.Errorf("Pause: %q %q", a, g)
	}
	if a, _, _ := l.entscheide(eingang(t0.Add(25*time.Minute), 20, 60, luft)); a != "auf" {
		t.Error("nach der Pause nicht auf")
	}
	l.cfg.MaxJeTag = 1
	if a, g, _ := l.entscheide(eingang(t0.Add(25*time.Minute), 20, 60, luft)); a != "" || !strings.Contains(g, "1-mal") {
		t.Errorf("Tagesgrenze: %q %q", a, g)
	}
	// Von Hand geoeffnet: Finger weg
	l = testLueftung()
	e := eingang(t0, 20, 60, luft)
	e.KF = "offen"
	if a, _, _ := l.entscheide(e); a != "" {
		t.Error("hat von Hand geoeffnete Fenster angefasst")
	}
}

func TestLueftungSonneHatVorrangBeiKaelte(t *testing.T) {
	l := testLueftung()
	t0 := mittwoch(13, 0)
	e := eingang(t0, 20, 60, luftTag(t0, luftWert{Temp: 8, Taupunkt: 0}, nil))
	e.EntfeuchterSonne = true
	if a, g, _ := l.entscheide(e); a != "" || !strings.Contains(g, "mit Sonne") {
		t.Fatalf("kalt: %q %q", a, g)
	}
	e = eingang(t0, 20, 60, luftTag(t0, luftWert{Temp: 15, Taupunkt: 2}, nil))
	e.EntfeuchterSonne = true
	if a, g, _ := l.entscheide(e); a != "auf" {
		t.Fatalf("mild: %q %q", a, g)
	}
}

func offeneLueftung(t0 time.Time, anlass string) *lueftung {
	l := testLueftung()
	l.st.Phase, l.st.Seit = "offen", t0
	l.st.Laeufe = []lueftLauf{{Von: t0, TaupunktIn: tpInnen, RHIn: 60, Anlass: anlass}}
	return l
}

func TestLueftungSchliesstNachMesswerten(t *testing.T) {
	t0 := mittwoch(14, 0)
	gut := luftTag(t0, luftWert{Temp: 15, Taupunkt: 5}, nil)
	for _, f := range []struct {
		nach   time.Duration
		temp   float64
		rh     float64
		luft   map[int64]luftWert
		soll   string
		sperre bool
	}{
		{5 * time.Hour, 20, 55, gut, "", false}, // laeuft, solange es hilft
		{30 * time.Minute, 20, 58, luftTag(t0, luftWert{Temp: 15, Taupunkt: 11.5}, nil), "Abstand nur noch", false},
		{5 * time.Minute, 16.5, 60, gut, "abgekühlt", false},
		{21 * time.Minute, 20, 61, gut, "nicht gefallen", true},
		{10 * time.Minute, 18, 66, gut, "Feuchte steigt", true},
		{30 * time.Minute, 20, 50, gut, "trocken genug", false},
		{5 * time.Minute, 20, 60, luftTag(t0, luftWert{Temp: 15, Taupunkt: 5, Regen: 1}, nil), "Regen", false},
		{12*time.Hour + time.Minute, 20, 55, gut, "Notbremse", false},
	} {
		l := offeneLueftung(t0, "feuchte")
		a, g, sp := l.entscheide(eingang(t0.Add(f.nach), f.temp, f.rh, f.luft))
		if f.soll == "" {
			if a != "" {
				t.Errorf("sollte offen bleiben: %q %q", a, g)
			}
			continue
		}
		if a != "zu" || !strings.Contains(g, f.soll) || (sp > 0) != f.sperre {
			t.Errorf("%s: %q %q %v", f.soll, a, g, sp)
		}
	}
}

func TestLueftungCO2(t *testing.T) {
	t0 := mittwoch(2, 0)
	mit := func(e lueftEingang, co2 float64) lueftEingang { e.CO2 = co2; return e }
	// Abstand nur 1 K, fuer Feuchte zu wenig, aber die Luft ist verbraucht
	knapp := luftTag(t0, luftWert{Temp: 12, Taupunkt: tpInnen - 1}, nil)
	l := testLueftung()
	if a, g, _ := l.entscheide(mit(eingang(t0, 20, 60, knapp), 1100)); a != "auf-co2" {
		t.Fatalf("CO2 1100: %q %q", a, g)
	}
	// Aussenluft 2 K feuchter: erst ab CO2Max
	feucht := luftTag(t0, luftWert{Temp: 14, Taupunkt: tpInnen + 2}, nil)
	if a, _, _ := l.entscheide(mit(eingang(t0, 20, 60, feucht), 1100)); a != "" {
		t.Fatal("CO2 1100 bei feuchterer Aussenluft geoeffnet")
	}
	if a, _, _ := l.entscheide(mit(eingang(t0, 20, 60, feucht), 1500)); a != "auf-co2" {
		t.Fatal("CO2 1500 nicht geoeffnet")
	}
	// Eine CO2-Lueftung schliesst bei 700 ppm, nicht wegen des Taupunkts
	l = offeneLueftung(t0, "co2")
	if a, g, _ := l.entscheide(mit(eingang(t0.Add(25*time.Minute), 20, 60, knapp), 850)); a != "" {
		t.Fatalf("CO2-Lueftung zu frueh zu: %q %q", a, g)
	}
	if a, g, _ := l.entscheide(mit(eingang(t0.Add(40*time.Minute), 20, 60, knapp), 690)); a != "zu" || !strings.Contains(g, "CO₂ wieder") {
		t.Fatalf("CO2-Lueftung bei 690: %q %q", a, g)
	}
	// Ohne Sensor im Keller gibt es keinen CO2-Grund
	l = testLueftung()
	if a, _, _ := l.entscheide(eingang(t0, 20, 60, knapp)); a != "" {
		t.Fatal("ohne CO2-Wert geoeffnet")
	}
}

func TestLueftungSchritt(t *testing.T) {
	t0 := mittwoch(14, 0)
	luft := luftTag(t0, luftWert{Temp: 15, Taupunkt: 5}, nil)
	var befehle []string
	var pause bool
	l := testLueftung()
	jetzt, rh := t0, 60.0
	l.eingang = func() lueftEingang { return eingang(jetzt, 20, rh, luft) }
	l.fahre = func(r string) error { befehle = append(befehle, r); return nil }
	l.pause = func(b bool) { pause = b }
	l.standPfad = t.TempDir() + "/stand.json"

	l.schritt()
	if l.st.Phase != "offen" || !pause || len(befehle) != 1 || befehle[0] != "auf" || !l.lueftet() {
		t.Fatalf("Oeffnen: %v pause %v %v", l.st.Phase, pause, befehle)
	}
	jetzt, rh = t0.Add(4*time.Minute), 58
	l.schritt()
	if k := l.laufzeit().Kurve; len(k) != 2 || k[1].RH != 58 || k[1].M != 4 {
		t.Fatalf("Kurve: %+v", k)
	}
	jetzt, rh = t0.Add(30*time.Minute), 50
	l.schritt()
	if l.st.Phase != "zu" || pause || befehle[len(befehle)-1] != "ab" || l.lueftet() {
		t.Fatalf("Schliessen: %v pause %v %v", l.st.Phase, pause, befehle)
	}
	if n := len(l.st.Laeufe); n != 1 || l.st.Laeufe[0].Bis.IsZero() || l.st.Laeufe[0].Anlass != "feuchte" {
		t.Fatalf("Lauf nicht gebucht: %+v", l.st.Laeufe)
	}

	// Probe faehrt nichts
	l = testLueftung()
	l.cfg.Modus = "probe"
	befehle, pause = nil, false
	jetzt, rh = t0, 60
	l.eingang = func() lueftEingang { return eingang(jetzt, 20, rh, luft) }
	l.fahre = func(r string) error { befehle = append(befehle, r); return nil }
	l.pause = func(b bool) { pause = b }
	l.standPfad = t.TempDir() + "/stand.json"
	l.schritt()
	if l.st.Phase != "offen" || len(befehle) != 0 || pause || l.lueftet() {
		t.Fatalf("Probe hat gehandelt: %v %v", befehle, pause)
	}
}

func TestLueftungMeldetNaechste(t *testing.T) {
	l := testLueftung()
	t0 := mittwoch(10, 0)
	luft := luftTag(t0, luftWert{Temp: 15, Taupunkt: 10}, map[int]luftWert{15: {Temp: 15, Taupunkt: 3}})
	l.entscheide(eingang(t0, 20, 60, luft))
	if !l.naechste.Equal(mittwoch(15, 0)) {
		t.Fatalf("naechste %v", l.naechste)
	}
	l.entscheide(eingang(t0, 20, 60, luftTag(t0, luftWert{Temp: 15, Taupunkt: 13}, nil)))
	if !l.naechste.IsZero() {
		t.Fatalf("naechste trotz feuchter Luft: %v", l.naechste)
	}
}

func TestLueftungWochenplan(t *testing.T) {
	l := testLueftung()
	h := neueHeizRegel(t.TempDir()+"/h.json", func(string, ...any) {})
	l.wach = h.wach
	t0 := mittwoch(14, 20)
	luft := map[int64]luftWert{}
	for x := 0; x < 7*24; x++ {
		z := tagesAnfang(t0).Add(time.Duration(x) * time.Hour)
		w := luftWert{Temp: 14, Taupunkt: 10} // 2 K, zu wenig
		if z.Hour() >= 16 && z.Hour() < 19 {
			w.Taupunkt = 5
		}
		if z.Day() == 9 {
			w.Regen = 1 // Freitag regnet es
		}
		luft[z.Unix()] = w
	}
	p := l.planeWoche(eingang(t0, 20, 60, luft))
	if len(p.Stunden) != 7*24 {
		t.Fatalf("%d Stunden", len(p.Stunden))
	}
	je := map[int]int{}
	for _, s := range p.Stunden {
		if s.Geplant {
			je[s.Zeit.Day()]++
			if s.Zeit.Hour() < 16 || s.Zeit.Hour() >= 19 {
				t.Errorf("ungeeignete Stunde geplant: %v", s.Zeit)
			}
		}
		if s.Zeit.Hour() == 3 && !s.Ruhe {
			t.Errorf("3 Uhr nicht als Schlafzeit markiert: %v", s.Zeit)
		}
	}
	if je[7] != 3 || je[8] != 3 || je[9] != 0 || je[10] != 3 {
		t.Errorf("Stunden je Tag: %v", je)
	}
	// Trockene Raumluft: nichts geplant
	p = l.planeWoche(eingang(t0, 20, 50, luft))
	for _, s := range p.Stunden {
		if s.Geplant {
			t.Fatal("trotz trockener Raumluft geplant")
		}
	}
}

func TestFeiertageNRW(t *testing.T) {
	if o := ostern(2026); o.Month() != 4 || o.Day() != 5 {
		t.Fatalf("Ostern 2026: %v", o)
	}
	if o := ostern(2027); o.Month() != 3 || o.Day() != 28 {
		t.Fatalf("Ostern 2027: %v", o)
	}
	for _, f := range []struct {
		m, d int
		ja   bool
	}{{1, 1, true}, {4, 3, true}, {4, 6, true}, {5, 14, true}, {5, 25, true}, {6, 4, true},
		{10, 3, true}, {11, 1, true}, {12, 24, false}, {12, 26, true}, {10, 7, false}} {
		if feiertag(time.Date(2026, time.Month(f.m), f.d, 12, 0, 0, 0, ort)) != f.ja {
			t.Errorf("%d.%d.: erwartet %v", f.d, f.m, f.ja)
		}
	}
}

func TestHeizRegel(t *testing.T) {
	c := standardHeizung()
	for _, f := range []struct {
		t       time.Time
		lueftet bool
		soll    float64
	}{
		{mittwoch(14, 0), false, 20},
		{mittwoch(9, 15), false, 18}, // werktags erst ab 9:30 wach
		{mittwoch(23, 0), false, 18},
		{mittwoch(14, 0), true, 8},
		{time.Date(2026, 10, 10, 10, 0, 0, 0, ort), false, 18}, // Samstag bis 10:30
		{time.Date(2026, 12, 25, 10, 0, 0, 0, ort), false, 18}, // Feiertag wie Sonntag
		{time.Date(2026, 12, 28, 10, 0, 0, 0, ort), false, 20}, // Montag danach
	} {
		if z, _ := c.zielFuer(f.t, f.lueftet); z != f.soll {
			t.Errorf("%v lueftet %v: %.1f statt %.1f", f.t, f.lueftet, z, f.soll)
		}
	}
	// Ferien: Sonntagszeiten, auch fuer den Entfeuchter
	setzeFerien("2026-10-09")
	defer setzeFerien("")
	if z, _ := c.zielFuer(mittwoch(10, 0), false); z != 18 {
		t.Errorf("Ferien 10 Uhr: %.1f", z)
	}
	if standardEntfeuchter().erlaubt(time.Date(2026, 10, 8, 12, 0, 0, 0, ort)) {
		t.Error("Entfeuchter in den Ferien um 12 Uhr erlaubt, Sonntag gilt erst ab 13")
	}
}

func TestHeizRegelSetzt(t *testing.T) {
	h := neueHeizRegel(t.TempDir()+"/h.json", func(string, ...any) {})
	soll := 14.0
	var gesetzt []float64
	h.ventil = func(string) *ventil { return &ventil{AIN: "1", Name: "Spielkeller", Ist: 20, Soll: soll} }
	h.setzeSoll = func(_ string, g float64) error { gesetzt = append(gesetzt, g); return nil }
	lueftet := false
	h.lueftet = func() bool { return lueftet }
	t0 := mittwoch(14, 0)
	h.schritt(t0)
	if len(gesetzt) != 1 || gesetzt[0] != 20 {
		t.Fatalf("Komfort nicht gesetzt: %v", gesetzt)
	}
	h.schritt(t0.Add(time.Minute)) // Box hat noch nicht uebernommen: nicht gleich nochmal
	if len(gesetzt) != 1 {
		t.Fatalf("zu oft gesetzt: %v", gesetzt)
	}
	soll = 20
	lueftet = true
	h.schritt(t0.Add(2 * time.Minute))
	if gesetzt[len(gesetzt)-1] != 8 {
		t.Fatalf("beim Lueften nicht auf 8: %v", gesetzt)
	}
	soll, lueftet = 8, false
	h.schritt(t0.Add(40 * time.Minute))
	if gesetzt[len(gesetzt)-1] != 20 {
		t.Fatalf("nach dem Lueften nicht zurueck: %v", gesetzt)
	}
	// Probe setzt nichts
	h.cfg.Modus = "probe"
	n := len(gesetzt)
	soll = 14
	h.schritt(t0.Add(2 * time.Hour))
	if len(gesetzt) != n || !strings.Contains(h.grund, "Probe") {
		t.Fatalf("Probe hat gesetzt: %v %s", gesetzt, h.grund)
	}
}

func TestFreieSonneSchlaegtKnappesLueften(t *testing.T) {
	t0 := mittwoch(13, 0)
	sonne := func(e lueftEingang) lueftEingang { e.SonneFrei = true; return e }
	knapp := luftTag(t0, luftWert{Temp: 18, Taupunkt: tpInnen - 1.5}, nil) // 1,5 K
	weit := luftTag(t0, luftWert{Temp: 18, Taupunkt: tpInnen - 5}, nil)    // 5 K
	l := offeneLueftung(t0, "feuchte")
	if a, g, _ := l.entscheide(sonne(eingang(t0.Add(30*time.Minute), 20, 60, knapp))); a != "zu" || !strings.Contains(g, "Entfeuchter übernimmt") {
		t.Fatalf("knapp mit Sonne: %q %q", a, g)
	}
	l = offeneLueftung(t0, "feuchte")
	if a, g, _ := l.entscheide(sonne(eingang(t0.Add(30*time.Minute), 20, 58, weit))); a != "" {
		t.Fatalf("weiter Abstand mit Sonne sollte lueften: %q %q", a, g)
	}
	l = offeneLueftung(t0, "feuchte")
	if a, _, _ := l.entscheide(eingang(t0.Add(15*time.Minute), 20, 60, knapp)); a != "" {
		t.Fatal("ohne Sonne bei 1,5 K zu, obwohl ueber der Schliessgrenze")
	}
	// CO2 bleibt unberuehrt
	l = offeneLueftung(t0, "co2")
	e := sonne(eingang(t0.Add(30*time.Minute), 20, 60, knapp))
	e.CO2 = 1200
	if a, g, _ := l.entscheide(e); a != "" {
		t.Fatalf("CO2-Lueftung wegen Sonne zu: %q %q", a, g)
	}
	// Geschlossen und Sonne frei: mit weitem Abstand trotzdem lueften
	l = testLueftung()
	if a, _, _ := l.entscheide(sonne(eingang(t0, 20, 60, weit))); a != "auf" {
		t.Fatal("bei 5 K mit Sonne nicht gelueftet")
	}
}

func TestSchlafzeiten(t *testing.T) {
	c := standardHeizung()
	s := c.schlafzeiten(mittwoch(0, 0), mittwoch(23, 59))
	// Mittwoch: 0:00 bis 9:30 und 22:30 bis Mitternacht
	if len(s) != 2 || time.UnixMilli(s[0][1]).In(ort).Format("15:04") != "09:30" ||
		time.UnixMilli(s[1][0]).In(ort).Format("15:04") != "22:30" {
		t.Fatalf("Schlafzeiten %v", s)
	}
}
