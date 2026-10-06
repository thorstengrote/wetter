package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Feuchte, die bei der Temperatur den gewuenschten Taupunkt ergibt.
func rhFuer(temp, tp float64) float64 {
	return 100 * math.Exp(17.62*tp/(243.12+tp)) / math.Exp(17.62*temp/(243.12+temp))
}

func testLueftung() *lueftung {
	l := &lueftung{cfg: standardLueftung(), sag: func(string, ...any) {}}
	l.st.Phase = "zu"
	return l
}

// Mittwoch, 7. Oktober 2026
func mittwoch(h, m int) time.Time { return time.Date(2026, 10, 7, h, m, 0, 0, ort) }

// luftTag: jede Stunde des Tages gleich, ausser den angegebenen.
func luftTag(t time.Time, normal luftWert, ausnahmen map[int]luftWert) map[int64]luftWert {
	m := map[int64]luftWert{}
	anf := tagesAnfang(t)
	for h := 0; h < 24; h++ {
		w := normal
		if a, ok := ausnahmen[h]; ok {
			w = a
		}
		m[anf.Add(time.Duration(h)*time.Hour).Unix()] = w
	}
	return m
}

func TestTaupunkt(t *testing.T) {
	// 20 Grad, 50 Prozent: Taupunkt rund 9,3 Grad, rund 8,6 g/m3
	if tp := taupunkt(20, 50); math.Abs(tp-9.3) > 0.1 {
		t.Errorf("Taupunkt %.2f", tp)
	}
	if w := wasser(20, 50); math.Abs(w-8.6) > 0.1 {
		t.Errorf("Wasser %.2f", w)
	}
	if rh := rhFuer(20, 9.26); math.Abs(rh-50) > 0.5 {
		t.Errorf("rhFuer %.2f", rh)
	}
}

func eingang(t time.Time, temp, rh float64, luft map[int64]luftWert) lueftEingang {
	return lueftEingang{Jetzt: t, Innen: &messFeuchte{Temp: temp, RH: rh, Zeit: t}, ZielRH: 53,
		Luft: luft, KF: "zu", KFDa: true}
}

func TestLueftungOeffnetNurMitAbstand(t *testing.T) {
	l := testLueftung()
	t0 := mittwoch(14, 0)
	// drinnen 20 Grad, 60 % -> Taupunkt 12; draussen Taupunkt 11, zu feucht
	luft := luftTag(t0, luftWert{Temp: 15, Taupunkt: 11}, nil)
	a, g, _ := l.entscheide(eingang(t0, 20, 60, luft))
	if a != "" || !strings.Contains(g, "zu feucht") {
		t.Fatalf("%q %q", a, g)
	}
	// draussen Taupunkt 5: los
	luft = luftTag(t0, luftWert{Temp: 15, Taupunkt: 5}, nil)
	if a, g, _ := l.entscheide(eingang(t0, 20, 60, luft)); a != "auf" {
		t.Fatalf("%q %q", a, g)
	}
}

func TestLueftungNachtruheUndSperren(t *testing.T) {
	l := testLueftung()
	luft := luftTag(mittwoch(0, 0), luftWert{Temp: 15, Taupunkt: 2}, nil)
	for _, f := range []struct {
		t    time.Time
		soll string
	}{
		{mittwoch(8, 59), "Nachtruhe"},
		{mittwoch(22, 31), "Nachtruhe"},
		{mittwoch(22, 10), "zu spät"},
		{time.Date(2026, 10, 10, 10, 0, 0, 0, ort), "Nachtruhe"}, // Samstag vor 10:30
	} {
		if a, g, _ := l.entscheide(eingang(f.t, 20, 60, luft)); a != "" || !strings.Contains(g, f.soll) {
			t.Errorf("%v: %q %q", f.t, a, g)
		}
	}
	// Samstag 10:30 geht
	sa := time.Date(2026, 10, 10, 10, 30, 0, 0, ort)
	if a, g, _ := l.entscheide(eingang(sa, 20, 60, luftTag(sa, luftWert{Temp: 15, Taupunkt: 2}, nil))); a != "auf" {
		t.Errorf("Samstag 10:30: %q %q", a, g)
	}
	// Regen, Boeen, Frost, trockene Raumluft, kuehler Raum
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
		{luftWert{Temp: 15, Taupunkt: 2}, 16.5, 70, "zu kühl"},
	} {
		if a, g, _ := l.entscheide(eingang(t0, f.temp, f.rh, luftTag(t0, f.w, nil))); a != "" || !strings.Contains(g, f.soll) {
			t.Errorf("%s: %q %q", f.soll, a, g)
		}
	}
}

func TestLueftungWartetAufBessereStunde(t *testing.T) {
	l := testLueftung()
	l.cfg.MaxJeTag = 1
	t0 := mittwoch(10, 0)
	// jetzt Abstand 4 K, um 15 Uhr 9 K
	luft := luftTag(t0, luftWert{Temp: 15, Taupunkt: 8}, map[int]luftWert{15: {Temp: 15, Taupunkt: 3}})
	a, g, _ := l.entscheide(eingang(t0, 20, 60, luft))
	if a != "" || !strings.Contains(g, "15 Uhr") {
		t.Fatalf("%q %q", a, g)
	}
	t1 := mittwoch(15, 5)
	if a, g, _ := l.entscheide(eingang(t1, 20, 60, luft)); a != "auf" {
		t.Fatalf("15 Uhr: %q %q", a, g)
	}
}

func TestLueftungKaelteZiehtAb(t *testing.T) {
	l := testLueftung()
	l.cfg.MaxJeTag = 1
	t0 := mittwoch(9, 0)
	// morgens 0 Grad und Taupunkt -3, nachmittags 10 Grad und Taupunkt -2:
	// der Abstand ist morgens 1 K groesser, die Waerme wiegt mehr.
	luft := luftTag(t0, luftWert{Temp: 0, Taupunkt: -3}, map[int]luftWert{14: {Temp: 10, Taupunkt: -2}})
	a, g, _ := l.entscheide(eingang(t0, 18, 65, luft))
	if a != "" || !strings.Contains(g, "14 Uhr") {
		t.Fatalf("%q %q", a, g)
	}
}

func TestLueftungSchliesst(t *testing.T) {
	t0 := mittwoch(14, 0)
	luft := luftTag(t0, luftWert{Temp: 15, Taupunkt: 5}, nil)
	neu := func() *lueftung {
		l := testLueftung()
		l.st.Phase, l.st.Seit = "offen", t0
		l.st.Laeufe = []lueftLauf{{Von: t0, TaupunktIn: taupunkt(20, 60)}}
		return l
	}
	for _, f := range []struct {
		nach   time.Duration
		temp   float64
		rh     float64
		w      *luftWert
		soll   string
		sperre bool
	}{
		{31 * time.Minute, 20, 58, nil, "nach 30 Minuten", false},
		{5 * time.Minute, 15.5, 60, nil, "abgekühlt", false},
		{16 * time.Minute, 20, 61, nil, "nicht gefallen", true},
		{5 * time.Minute, 20, 50, nil, "trocken genug", false},
		{5 * time.Minute, 20, 60, &luftWert{Temp: 15, Taupunkt: 5, Regen: 1}, "Regen", false},
		{5 * time.Minute, 20, 60, &luftWert{Temp: 8, Taupunkt: 5}, "", false}, // kalt: 15 Minuten
		{16 * time.Minute, 20, 55, &luftWert{Temp: 8, Taupunkt: 5}, "nach 15 Minuten", false},
	} {
		l := neu()
		lf := luft
		if f.w != nil {
			lf = luftTag(t0, *f.w, nil)
		}
		a, g, sp := l.entscheide(eingang(t0.Add(f.nach), f.temp, f.rh, lf))
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
	// Nachtruhe schliesst auch Fenster, die jemand von Hand geoeffnet hat
	l := testLueftung()
	e := eingang(mittwoch(23, 0), 20, 60, luft)
	e.KF = "offen"
	if a, _, _ := l.entscheide(e); a != "zu" {
		t.Errorf("Nachtruhe mit offenem Fenster: %q", a)
	}
}

func TestLueftungSchritt(t *testing.T) {
	t0 := mittwoch(14, 0)
	luft := luftTag(t0, luftWert{Temp: 15, Taupunkt: 5}, nil)
	var befehle []string
	var pause bool
	soll := 18.0
	v := &ventil{AIN: "123", Name: "Spielkeller", Ist: 20, Soll: 18}
	l := testLueftung()
	jetzt := t0
	rh := 60.0
	l.eingang = func() lueftEingang { return eingang(jetzt, 20, rh, luft) }
	l.ventil = func(string) *ventil { w := *v; w.Soll = soll; return &w }
	l.fahre = func(r string) error { befehle = append(befehle, r); return nil }
	l.setzeSoll = func(_ string, g float64) error { soll = g; return nil }
	l.pause = func(b bool) { pause = b }
	l.standPfad = t.TempDir() + "/stand.json"

	l.schritt()
	if l.st.Phase != "offen" || soll != 8 || !pause || len(befehle) != 1 || befehle[0] != "auf" {
		t.Fatalf("Oeffnen: %v soll %v pause %v %v", l.st.Phase, soll, pause, befehle)
	}
	jetzt, rh = t0.Add(10*time.Minute), 50
	l.schritt()
	if l.st.Phase != "zu" || soll != 18 || pause || befehle[len(befehle)-1] != "ab" {
		t.Fatalf("Schliessen: %v soll %v pause %v %v", l.st.Phase, soll, pause, befehle)
	}
	if n := len(l.st.Laeufe); n != 1 || l.st.Laeufe[0].Bis.IsZero() {
		t.Fatalf("Lauf nicht gebucht: %+v", l.st.Laeufe)
	}
	// Probe faehrt nichts
	l = testLueftung()
	l.cfg.Modus = "probe"
	befehle, soll, pause = nil, 18, false
	jetzt, rh = t0, 60
	l.eingang = func() lueftEingang { return eingang(jetzt, 20, rh, luft) }
	l.ventil = func(string) *ventil { w := *v; w.Soll = soll; return &w }
	l.fahre = func(r string) error { befehle = append(befehle, r); return nil }
	l.setzeSoll = func(_ string, g float64) error { soll = g; return nil }
	l.pause = func(b bool) { pause = b }
	l.standPfad = t.TempDir() + "/stand.json"
	l.schritt()
	if l.st.Phase != "offen" || len(befehle) != 0 || soll != 18 || pause {
		t.Fatalf("Probe hat gehandelt: %v %v %v", befehle, soll, pause)
	}
}
