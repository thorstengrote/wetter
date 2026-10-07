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
	l.cfg.NachtMaxJe = 0 // strenge Nachtruhe wie bis 07.10.2026
	luft := luftTag(mittwoch(0, 0), luftWert{Temp: 15, Taupunkt: 2}, nil)
	for _, f := range []struct {
		t    time.Time
		soll string
	}{
		{mittwoch(8, 59), "Nachtruhe"},
		{mittwoch(9, 15), "Nachtruhe"}, // werktags erst ab 9:30
		{mittwoch(22, 31), "Nachtruhe"},
		{mittwoch(21, 10), "zu spät"},                            // 90 Minuten passen nicht mehr bis 22:30
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
	t0 := mittwoch(9, 30)
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
		l.st.Laeufe = []lueftLauf{{Von: t0, TaupunktIn: taupunkt(20, 60), RHIn: 60}}
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
		{91 * time.Minute, 20, 58, nil, "nach 90 Minuten", false},
		{5 * time.Minute, 15.5, 60, nil, "abgekühlt", false},
		{21 * time.Minute, 20, 61, nil, "nicht gefallen", true},
		{10 * time.Minute, 18, 66, nil, "Feuchte steigt", true},
		{5 * time.Minute, 20, 50, nil, "trocken genug", false},
		{5 * time.Minute, 20, 60, &luftWert{Temp: 15, Taupunkt: 5, Regen: 1}, "Regen", false},
		{30 * time.Minute, 20, 58, &luftWert{Temp: 8, Taupunkt: 5}, "", false}, // kalt: 40 Minuten
		{41 * time.Minute, 20, 55, &luftWert{Temp: 8, Taupunkt: 5}, "nach 40 Minuten", false},
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
	// Strenge Nachtruhe schliesst auch Fenster, die jemand von Hand geoeffnet hat
	l := testLueftung()
	l.cfg.NachtMaxJe = 0
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
	jetzt, rh = t0.Add(4*time.Minute), 58
	l.schritt()
	if k := l.laufzeit().Kurve; len(k) != 2 || k[1].RH != 58 || k[1].M != 4 {
		t.Fatalf("Kurve: %+v", k)
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

func TestLueftungFeiertagUndFerien(t *testing.T) {
	l := testLueftung()
	l.cfg.NachtMaxJe = 0
	luftAm := func(t time.Time) map[int64]luftWert { return luftTag(t, luftWert{Temp: 15, Taupunkt: 2}, nil) }
	weihnacht := time.Date(2026, 12, 25, 10, 0, 0, 0, ort) // Freitag, Feiertag
	if a, g, _ := l.entscheide(eingang(weihnacht, 20, 60, luftAm(weihnacht))); a != "" || g != "Nachtruhe" {
		t.Errorf("Feiertag 10 Uhr: %q %q", a, g)
	}
	w2 := weihnacht.Add(30 * time.Minute)
	if a, g, _ := l.entscheide(eingang(w2, 20, 60, luftAm(w2))); a != "auf" {
		t.Errorf("Feiertag 10:30: %q %q", a, g)
	}
	setzeFerien("2026-10-09")
	defer setzeFerien("")
	t0 := mittwoch(10, 0)
	if a, g, _ := l.entscheide(eingang(t0, 20, 60, luftAm(t0))); a != "" || g != "Nachtruhe" {
		t.Errorf("Ferien 10 Uhr: %q %q", a, g)
	}
	nach := time.Date(2026, 10, 12, 10, 0, 0, 0, ort) // Montag nach den Ferien
	if a, g, _ := l.entscheide(eingang(nach, 20, 60, luftAm(nach))); a != "auf" {
		t.Errorf("nach den Ferien: %q %q", a, g)
	}
	// Der Entfeuchter haelt sich ebenso daran
	if standardEntfeuchter().erlaubt(time.Date(2026, 10, 8, 12, 0, 0, 0, ort)) {
		t.Error("Entfeuchter in den Ferien um 12 Uhr erlaubt, Sonntag gilt erst ab 13")
	}
}

func TestLueftungMeldetNaechste(t *testing.T) {
	l := testLueftung()
	l.cfg.MaxJeTag = 1
	t0 := mittwoch(10, 0)
	luft := luftTag(t0, luftWert{Temp: 15, Taupunkt: 8}, map[int]luftWert{15: {Temp: 15, Taupunkt: 3}})
	l.entscheide(eingang(t0, 20, 60, luft))
	if !l.naechste.Equal(mittwoch(15, 0)) {
		t.Fatalf("naechste %v", l.naechste)
	}
	// Ohne geeignete Stunde keine
	l.entscheide(eingang(t0, 20, 60, luftTag(t0, luftWert{Temp: 15, Taupunkt: 13}, nil)))
	if !l.naechste.IsZero() {
		t.Fatalf("naechste trotz feuchter Luft: %v", l.naechste)
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

func TestLueftungWochenplan(t *testing.T) {
	l := testLueftung()
	l.cfg.NachtMaxJe = 0
	t0 := mittwoch(14, 20)
	luft := map[int64]luftWert{}
	for d := 0; d < 7; d++ {
		tag := tagesAnfang(t0).AddDate(0, 0, d)
		for h := 0; h < 24; h++ {
			w := luftWert{Temp: 14, Taupunkt: 6}
			if h == 16 {
				w.Taupunkt = 2 // die beste Stunde jedes Tages
			}
			if d == 2 {
				w.Regen = 1 // Freitag regnet es den ganzen Tag
			}
			luft[tag.Add(time.Duration(h)*time.Hour).Unix()] = w
		}
	}
	p := l.planeWoche(eingang(t0, 20, 60, luft))
	if len(p.Stunden) != 7*24 {
		t.Fatalf("%d Stunden", len(p.Stunden))
	}
	je := map[int][]time.Time{}
	for _, s := range p.Stunden {
		if !s.Geplant {
			continue
		}
		d := int(tagesAnfang(s.Zeit).Sub(tagesAnfang(t0)).Hours() / 24)
		je[d] = append(je[d], s.Zeit)
		if !l.cfg.erlaubt(s.Zeit) {
			t.Errorf("in der Nachtruhe geplant: %v", s.Zeit)
		}
		if d == 0 && s.Zeit.Before(t0.Truncate(time.Hour)) {
			t.Errorf("in der Vergangenheit geplant: %v", s.Zeit)
		}
	}
	if len(je[2]) != 0 {
		t.Errorf("am Regentag geplant: %v", je[2])
	}
	for _, d := range []int{0, 1, 3, 4, 5, 6} {
		z := je[d]
		if len(z) != 2 {
			t.Errorf("Tag %d: %d Lueftungen %v", d, len(z), z)
			continue
		}
		if z[0].Hour() != 16 && z[1].Hour() != 16 {
			t.Errorf("Tag %d: beste Stunde 16 Uhr fehlt: %v", d, z)
		}
		if ab := z[1].Sub(z[0]); ab < 3*time.Hour && ab > -3*time.Hour {
			t.Errorf("Tag %d: zu dicht: %v", d, z)
		}
	}
	// Samstag nicht vor 10:30
	for _, z := range append(je[3], je[4]...) {
		if float64(z.Hour())+float64(z.Minute())/60 < 10.5 {
			t.Errorf("Wochenende zu frueh: %v", z)
		}
	}
}

func TestLueftungNachts(t *testing.T) {
	l := testLueftung()
	nacht := time.Date(2026, 10, 8, 1, 0, 0, 0, ort) // Donnerstag 1 Uhr
	luft := luftTag(nacht, luftWert{Temp: 12, Taupunkt: 3}, nil)
	a, g, _ := l.entscheide(eingang(nacht, 20, 60, luft))
	if a != "auf" || !strings.Contains(g, "Nachtlüftung") {
		t.Fatalf("Nachtlueftung: %q %q", a, g)
	}
	// Eine pro Nacht: die Nacht begann Mittwoch 22:30
	l.st.Laeufe = []lueftLauf{{Von: mittwoch(23, 30), Bis: mittwoch(23, 50), Nacht: true}}
	if a, g, _ := l.entscheide(eingang(nacht.Add(4*time.Hour), 20, 60, luft)); a != "" || !strings.Contains(g, "diese Nacht") {
		t.Fatalf("zweite in derselben Nacht: %q %q", a, g)
	}
	// Die Lueftung vom Abend laeuft als Nachtlueftung weiter
	l = testLueftung()
	ab := mittwoch(22, 0)
	l.st.Phase, l.st.Seit = "offen", ab
	l.st.Laeufe = []lueftLauf{{Von: ab, TaupunktIn: taupunkt(20, 60), RHIn: 60}}
	luft = luftTag(ab, luftWert{Temp: 12, Taupunkt: 3}, nil)
	if a, g, _ := l.entscheide(eingang(mittwoch(22, 40), 20, 59, luft)); a != "" || !strings.Contains(g, "Nachtlüftung") || !l.st.Laeufe[0].Nacht {
		t.Fatalf("Abend in die Nacht: %q %q", a, g)
	}
	if a, _, _ := l.entscheide(eingang(mittwoch(22, 0).Add(241*time.Minute), 20, 59, luftTag(mittwoch(23, 0), luftWert{Temp: 12, Taupunkt: 3}, nil))); a != "zu" {
		t.Fatal("Nachtlueftung nach 4 Stunden nicht zu")
	}
	// Ist das Nachtbudget verbraucht, geht die Abendlueftung vor der Nacht zu
	l = testLueftung()
	l.st.Phase, l.st.Seit = "offen", ab
	l.st.Laeufe = []lueftLauf{{Von: ab, TaupunktIn: taupunkt(20, 60), RHIn: 60}}
	l.cfg.NachtMaxJe = 0
	if a, g, _ := l.entscheide(eingang(mittwoch(22, 31), 20, 59, luft)); a != "zu" || g != "Nachtruhe" {
		t.Fatalf("ohne Nachtbudget: %q %q", a, g)
	}
}

func TestLueftungWochenplanNachts(t *testing.T) {
	l := testLueftung() // eine Nachtlueftung je Nacht
	t0 := mittwoch(14, 20)
	luft := map[int64]luftWert{}
	for h := 0; h < 8*24; h++ {
		luft[tagesAnfang(t0).Add(time.Duration(h)*time.Hour).Unix()] = luftWert{Temp: 14, Taupunkt: 6}
	}
	p := l.planeWoche(eingang(t0, 20, 60, luft))
	je := map[string]int{}
	for _, s := range p.Stunden {
		if s.Geplant && s.Ruhe {
			von, _ := l.cfg.nacht(s.Zeit)
			je[von.Format("02.01.")]++
		}
	}
	for n, k := range je {
		if k != 1 {
			t.Errorf("Nacht ab %s: %d Lueftungen", n, k)
		}
	}
	if len(je) < 6 {
		t.Errorf("nur %d Naechte geplant: %v", len(je), je)
	}
}
