package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func berlin() *time.Location {
	l, _ := time.LoadLocation("Europe/Berlin")
	ort = l
	return l
}

// pvWoche baut eine Prognose: je Tag die Spitzenleistung in kW, Glocke um
// 12 Uhr zwischen 7 und 18 Uhr.
func pvWoche(start time.Time, spitzen ...float64) map[int64]float64 {
	p := map[int64]float64{}
	for d, sp := range spitzen {
		tag := tagesAnfang(start).AddDate(0, 0, d)
		for h := 0; h < 24; h++ {
			v := 0.0
			if h >= 7 && h < 18 {
				x := (float64(h) + 0.5 - 12) / 3
				v = sp * math.Exp(-x*x)
			}
			p[tag.Add(time.Duration(h)*time.Hour).Unix()] = v
		}
	}
	return p
}

func planFuer(t time.Time, soc float64, pv map[int64]float64, cfg geraetCfg, laeufe []lauf, beginn time.Time) plan {
	in := planEingabe{Jetzt: t, SOC: soc, PV: pv, Faktor: 1, Leistung: cfg.LeistungKW, Cfg: cfg,
		Erlaubt: cfg.erlaubt, Laeufe: laeufe, Beginn: beginn}
	return neuerPlaner(in).rechne()
}

func minutenAm(p plan, tag time.Time, art string) float64 {
	m := 0.0
	for _, s := range p.Slots {
		if s.An && tagKey(s.Zeit) == tagKey(tag) && (art == "" || s.Art == art) {
			m += 30
		}
	}
	return m
}

func TestErlaubteZeiten(t *testing.T) {
	l := berlin()
	c := standardEntfeuchter()
	for _, f := range []struct {
		t  time.Time
		ok bool
	}{
		{time.Date(2026, 10, 5, 10, 59, 0, 0, l), false},
		{time.Date(2026, 10, 5, 11, 0, 0, 0, l), true},
		{time.Date(2026, 10, 5, 18, 0, 0, 0, l), false},
		{time.Date(2026, 10, 3, 12, 30, 0, 0, l), false},
		{time.Date(2026, 10, 3, 13, 0, 0, 0, l), true},
	} {
		if c.erlaubt(f.t) != f.ok {
			t.Errorf("%v: erwartet %v", f.t, f.ok)
		}
	}
}

// Sonniger Tag, Akku halb voll: er darf vor dem vollen Akku anlaufen.
func TestVorziehenVorVollemAkku(t *testing.T) {
	l := berlin()
	jetzt := time.Date(2026, 10, 5, 10, 0, 0, 0, l) // Montag
	pv := pvWoche(jetzt, 6, 6, 6, 6, 6, 6, 6)
	p := planFuer(jetzt, 8, pv, standardEntfeuchter(), nil, jetzt.AddDate(0, 0, -10))
	s := p.Slots[2] // 11:00
	if !s.An || s.Art != "frei" {
		t.Fatalf("11:00 an einem Sonnentag nicht frei geplant: %+v", s)
	}
	if s.SOC >= 99 {
		t.Fatalf("Test zeigt nichts, Akku schon voll: %.0f", s.SOC)
	}
	for _, x := range p.Slots {
		if x.An && x.Quelle == "netz" {
			t.Fatalf("Netzstrom an einem Sonnentag: %v", x.Zeit)
		}
	}
}

// Ohne Vorziehen nur bei echter Einspeisung.
func TestOhneVorziehen(t *testing.T) {
	l := berlin()
	jetzt := time.Date(2026, 10, 5, 10, 0, 0, 0, l)
	c := standardEntfeuchter()
	c.Vorziehen = false
	p := planFuer(jetzt, 10, pvWoche(jetzt, 8), c, nil, jetzt.AddDate(0, 0, -10))
	if p.Slots[2].An && p.Slots[2].Art == "frei" {
		t.Fatal("ohne Vorziehen bei leerem Akku um 11 Uhr frei geplant")
	}
}

// Heute Sonne, dann vier graue Tage: heute vorarbeiten, danach nur die
// Pflicht alle drei Tage.
func TestVorarbeitenFuerGraueTage(t *testing.T) {
	l := berlin()
	jetzt := time.Date(2026, 10, 5, 9, 0, 0, 0, l)
	pv := pvWoche(jetzt, 8, 0.3, 0.3, 0.3, 0.3, 0.3, 0.3)
	p := planFuer(jetzt, 30, pv, standardEntfeuchter(), nil, jetzt.AddDate(0, 0, -30))
	if m := minutenAm(p, jetzt, ""); m < 5*60 {
		t.Fatalf("heute nur %.0f min, sollte die Woche vorarbeiten", m)
	}
	for d := 1; d <= 2; d++ {
		if m := minutenAm(p, jetzt.AddDate(0, 0, d), ""); m > 0 {
			t.Fatalf("Tag +%d trotz Vorarbeit %.0f min geplant", d, m)
		}
	}
	if m := minutenAm(p, jetzt.AddDate(0, 0, 3), "pflicht"); m < 30 {
		t.Fatalf("Tag +3 ohne Pflichtlauf, Abstand ueber drei Tage")
	}
}

// Ganze Woche grau, letzter Lauf vor zwei Tagen: Pflicht heute oder bald,
// nie mehr als drei Tage Abstand, 5 Stunden in 7 Tagen.
func TestGraueWoche(t *testing.T) {
	l := berlin()
	jetzt := time.Date(2026, 10, 5, 9, 0, 0, 0, l)
	pv := pvWoche(jetzt, 0.4, 0.4, 0.4, 0.4, 0.4, 0.4, 0.4)
	alt := []lauf{{Von: jetzt.AddDate(0, 0, -2).Add(4 * time.Hour), Bis: jetzt.AddDate(0, 0, -2).Add(5 * time.Hour)}}
	p := planFuer(jetzt, 20, pv, standardEntfeuchter(), alt, jetzt.AddDate(0, 0, -30))
	if minutenAm(p, jetzt, "") < 30 {
		t.Fatal("heute kein Lauf, obwohl der letzte zwei Tage her ist")
	}
	gesamt := 0.0
	for d := 0; d < 7; d++ {
		gesamt += minutenAm(p, jetzt.AddDate(0, 0, d), "")
	}
	if gesamt < 4*60 {
		t.Fatalf("in sieben grauen Tagen nur %.0f min geplant", gesamt)
	}
}

// Netz verboten: kein Pflichtlauf mit Netzbezug, dafuer ein Hinweis.
func TestNetzVerboten(t *testing.T) {
	l := berlin()
	jetzt := time.Date(2026, 10, 5, 9, 0, 0, 0, l)
	c := standardEntfeuchter()
	c.NetzErlaubt = false
	p := planFuer(jetzt, 5, pvWoche(jetzt, 0, 0, 0, 0, 0, 0, 0), c, nil, jetzt.AddDate(0, 0, -30))
	for _, s := range p.Slots {
		if s.An {
			t.Fatalf("ohne Sonne und Akku trotz Verbot geplant: %v", s.Zeit)
		}
	}
	if len(p.Hinweise) == 0 {
		t.Fatal("kein Hinweis auf verfehlte Pflicht")
	}
}

// Sonnenwoche: Laeufe mit freiem Sonnenstrom zaehlen seit 07.10.2026 nicht
// gegen die 30 Stunden, der Planer darf die Sonne voll nutzen.
func TestSonnenlaeufeZaehlenNichtGegenGrenze(t *testing.T) {
	l := berlin()
	jetzt := time.Date(2026, 10, 5, 9, 0, 0, 0, l)
	p := planFuer(jetzt, 100, pvWoche(jetzt, 9, 9, 9, 9, 9, 9, 9), standardEntfeuchter(), nil, jetzt.AddDate(0, 0, -30))
	gesamt := 0.0
	for _, s := range p.Slots {
		if s.An {
			gesamt += 30
			if s.Art != "frei" {
				t.Fatalf("Pflichtlauf in einer Sonnenwoche: %v", s.Zeit)
			}
		}
	}
	if gesamt <= 30*60 {
		t.Fatalf("Sonnenwoche nicht ueber die Grenze genutzt: %.1f h", gesamt/60)
	}
	// Auch mit voller Vorgeschichte aus Netzstrom bleibt die Sonne frei
	var vor []lauf
	for d := 1; d <= 6; d++ {
		v := jetzt.AddDate(0, 0, -d)
		vor = append(vor, lauf{Von: v, Bis: v.Add(5 * time.Hour), Min: map[string]float64{"netz": 300}})
	}
	p = planFuer(jetzt, 100, pvWoche(jetzt, 9, 9, 9, 9, 9, 9, 9), standardEntfeuchter(), vor, jetzt.AddDate(0, 0, -30))
	if m := minutenAm(p, jetzt, "frei"); m < 60 {
		t.Fatalf("Sonne trotz voller Netzwoche nicht genutzt: %.0f min", m)
	}
}

func TestNurNetzUndAkkuZaehlen(t *testing.T) {
	if z := (lauf{Min: map[string]float64{"sonne": 60, "netz": 30, "akku": 30}}).zaehlt(); z != 0.5 {
		t.Fatalf("Anteil %.2f", z)
	}
	if z := (lauf{}).zaehlt(); z != 1 {
		t.Fatalf("ohne Aufteilung %.2f", z)
	}
	l := berlin()
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, l)
	g := &geraet{cfg: standardEntfeuchter()}
	g.cfg.Modus = "scharf"
	g.st.Laeufe = []lauf{{Von: t0.Add(-4 * time.Hour), Bis: t0.Add(-2 * time.Hour), Min: map[string]float64{"sonne": 90, "netz": 30}}}
	if m := g.minuten7Grenze(t0); m != 30 {
		t.Fatalf("zaehlende Minuten %.0f", m)
	}
	if m := g.minuten7(t0); m != 120 {
		t.Fatalf("Laufzeit %.0f", m)
	}
}

// --- Regelung ----------------------------------------------------------

type probeSt struct {
	s    *steuerung
	rufe []bool
	t    time.Time
}

func neueSt(t *testing.T, modus string, start time.Time) *probeSt {
	berlin()
	dir := t.TempDir()
	p := &probeSt{t: start}
	p.s = neueSteuerung(filepath.Join(dir, "c.json"), filepath.Join(dir, "s.json"), func(string, ...any) {})
	g := p.s.geraete[0]
	g.cfg.Modus, g.modusAlt = modus, modus
	g.st.Beginn = start.AddDate(0, 0, -30)
	p.s.schalte = func(ip string, an bool, tm int) (float64, error) {
		p.rufe = append(p.rufe, an)
		return 0.38, nil
	}
	return p
}

func (p *probeSt) laufe(min int, netz, akku, soc float64) {
	for i := 0; i < min*2; i++ {
		p.t = p.t.Add(30 * time.Second)
		p.s.pruefe(messwert{Zeit: p.t, Netz: netz, Akku: akku, SOC: soc})
	}
}

func TestFreierLaufGehtAusBeiNetzbezug(t *testing.T) {
	l := berlin()
	p := neueSt(t, "scharf", time.Date(2026, 10, 5, 10, 55, 0, 0, l))
	p.s.setzePrognose(pvWoche(p.t, 8, 8, 8, 8, 8, 8, 8))
	p.laufe(10, 0, 2.0, 40) // Akku laedt, Plan sagt frei ab 11
	g := p.s.geraete[0]
	if !g.st.An {
		t.Fatalf("frei geplanter Lauf nicht gestartet: %s", g.grund)
	}
	p.laufe(12, -0.6, -0.2, 40) // doch Wolken: Netzbezug
	if g.st.An {
		t.Fatalf("bei Netzbezug trotz freiem Plan noch an: %s", g.grund)
	}
}

func TestProbeSchaltetNicht(t *testing.T) {
	l := berlin()
	p := neueSt(t, "probe", time.Date(2026, 10, 5, 10, 55, 0, 0, l))
	p.s.setzePrognose(pvWoche(p.t, 8, 8, 8, 8, 8, 8, 8))
	p.laufe(60, 1.5, 0, 100)
	if !p.s.geraete[0].st.An {
		t.Fatal("Probe haette eingeschaltet")
	}
	if len(p.rufe) != 0 {
		t.Fatalf("Shelly im Probebetrieb angesprochen: %v", p.rufe)
	}
}

func TestEinspeisungOhnePrognose(t *testing.T) {
	l := berlin()
	p := neueSt(t, "scharf", time.Date(2026, 10, 5, 12, 0, 0, 0, l))
	p.laufe(4, 1.0, 0, 100)
	if p.s.geraete[0].st.An {
		t.Fatal("vor fuenf Minuten an")
	}
	p.laufe(2, 1.0, 0, 100)
	if !p.s.geraete[0].st.An {
		t.Fatal("bei Einspeisung nicht an")
	}
}

func TestFalscheUhr(t *testing.T) {
	l := berlin()
	p := neueSt(t, "scharf", time.Date(2026, 10, 5, 12, 0, 0, 0, l))
	p.laufe(6, 1.0, 0, 100)
	n := len(p.rufe)
	p.s.pruefe(messwert{Zeit: time.Date(2026, 5, 23, 15, 20, 0, 0, l), Netz: -2})
	if len(p.rufe) != n || !p.s.geraete[0].st.An {
		t.Fatal("Messung mit falscher Uhr verarbeitet")
	}
}

func TestHandAus(t *testing.T) {
	l := berlin()
	p := neueSt(t, "scharf", time.Date(2026, 10, 5, 12, 0, 0, 0, l))
	g := p.s.geraete[0]
	g.st.Hand, g.st.HandBis = "aus", tagesAnfang(p.t).AddDate(0, 0, 1)
	p.laufe(30, 2.0, 0, 100)
	if g.st.An {
		t.Fatal("trotz Hand aus eingeschaltet")
	}
}

// Kompressor springt nicht an: nach Anlauf plus Viertelstunde aus, Hinweis,
// eine Stunde spaeter neuer Versuch, mit Kompressor ist der Hinweis weg.
func TestTankVoll(t *testing.T) {
	l := berlin()
	p := neueSt(t, "scharf", time.Date(2026, 10, 5, 12, 0, 0, 0, l))
	kw := 0.05 // nur Luefter
	p.s.schalte = func(ip string, an bool, tm int) (float64, error) {
		if !an {
			return 0, nil
		}
		return kw, nil
	}
	g := p.s.geraete[0]
	p.laufe(6, 1.0, 0, 100) // an nach 5 min Einspeisung
	if !g.st.An {
		t.Fatal("nicht an")
	}
	p.laufe(14, 1.0, 0, 100)
	if !g.st.An || g.st.Stoerung != "" {
		t.Fatal("zu frueh als Stoerung erkannt")
	}
	p.laufe(12, 1.0, 0, 100)
	if g.st.An || g.st.Stoerung == "" {
		t.Fatalf("Tank voll nicht erkannt: an %v, %q", g.st.An, g.st.Stoerung)
	}
	if h := p.s.hinweise(); len(h) != 1 {
		t.Fatalf("kein Hinweis fuer die Wand: %v", h)
	}
	p.laufe(30, 1.0, 0, 100)
	if g.st.An {
		t.Fatal("vor dem neuen Versuch wieder an")
	}
	kw = 0.38 // Tank geleert, Kompressor laeuft
	p.laufe(45, 1.0, 0, 100)
	if !g.st.An || g.st.Stoerung != "" {
		t.Fatalf("nach dem Leeren nicht wieder normal: an %v, %q", g.st.An, g.st.Stoerung)
	}
	if g.st.KompKW < 0.2 {
		t.Fatalf("Kompressorleistung nicht gelernt: %.2f", g.st.KompKW)
	}
}

// Normaler Anlauf: zwei Minuten Luefter, dann Kompressor, keine Stoerung.
func TestKompressorAnlauf(t *testing.T) {
	l := berlin()
	p := neueSt(t, "scharf", time.Date(2026, 10, 5, 12, 0, 0, 0, l))
	n := 0
	p.s.schalte = func(ip string, an bool, tm int) (float64, error) {
		if !an {
			return 0, nil
		}
		n++
		if n < 7 { // drei Minuten Bereitschaft wie am Geraet gemessen
			return 0.0012, nil
		}
		return 0.28, nil
	}
	g := p.s.geraete[0]
	p.laufe(60, 1.0, 0, 100)
	if !g.st.An || g.st.Stoerung != "" {
		t.Fatalf("normaler Lauf gestoert: %q", g.st.Stoerung)
	}
	if l := g.offenerLauf(); l == nil || l.Min["kompressor"] < 40 {
		t.Fatal("Kompressorminuten nicht gezaehlt")
	}
}

func mitSensor(p *probeSt, rh float64) {
	g := p.s.geraete[0]
	g.cfg.SensorID = "S1"
	if p.s.sb == nil {
		p.s.sb = neuerSwitchbot("/nicht/da", func(string, ...any) {})
	}
	p.s.sb.werte["S1"] = messFeuchte{RH: rh, Zeit: time.Now()}
}

func TestZuFeuchtLaeuftMitNetz(t *testing.T) {
	l := berlin()
	p := neueSt(t, "scharf", time.Date(2026, 10, 5, 12, 0, 0, 0, l))
	mitSensor(p, 70)
	p.laufe(20, -0.4, 0, 5) // kein Ueberschuss, Akku leer
	g := p.s.geraete[0]
	if !g.st.An {
		t.Fatalf("bei 70 %% nicht an: %s", g.grund)
	}
	mitSensor(p, 63) // noch nicht 3 Punkte unter 65
	p.laufe(5, -0.4, 0, 5)
	if !g.st.An {
		t.Fatal("zu frueh aus, Abstand von 3 Punkten nicht eingehalten")
	}
	mitSensor(p, 61)
	p.laufe(5, -0.4, 0, 5)
	if g.st.An {
		t.Fatalf("bei 61 %% ohne Sonne noch an: %s", g.grund)
	}
}

func TestTrockenTrotzSonne(t *testing.T) {
	l := berlin()
	p := neueSt(t, "scharf", time.Date(2026, 10, 5, 12, 0, 0, 0, l))
	mitSensor(p, 45)
	p.laufe(30, 2.0, 0, 100)
	if p.s.geraete[0].st.An {
		t.Fatal("bei 45 % trotz Sonne gelaufen")
	}
}

func TestOhneSensorWiederMindestregeln(t *testing.T) {
	l := berlin()
	p := neueSt(t, "probe", time.Date(2026, 10, 5, 9, 0, 0, 0, l))
	p.s.setzePrognose(pvWoche(p.t, 0.3, 0.3, 0.3, 0.3, 0.3, 0.3, 0.3))
	mitSensor(p, 55)
	p.laufe(1, 0, 0, 30)
	g := p.s.geraete[0]
	pflicht := func() int {
		n := 0
		for _, s := range g.plan.Slots {
			if s.Art == "pflicht" {
				n++
			}
		}
		return n
	}
	if pflicht() != 0 {
		t.Fatal("mit Sensor trotzdem Pflichtlaeufe geplant")
	}
	p.s.sb.werte["S1"] = messFeuchte{RH: 55, Zeit: time.Now().Add(-time.Hour)} // veraltet
	p.laufe(1, 0, 0, 30)
	if pflicht() == 0 {
		t.Fatal("ohne frischen Sensorwert keine Mindestregeln")
	}
}

func TestFensterOffenPause(t *testing.T) {
	l := berlin()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"geraete":{"3":40,"5":0}}`))
	}))
	defer srv.Close()
	p := neueSt(t, "scharf", time.Date(2026, 10, 5, 12, 0, 0, 0, l))
	p.s.veluxBasis = srv.URL
	g := p.s.geraete[0]
	g.cfg.Fenster = []string{"3"}
	mitSensor(p, 70)
	p.laufe(20, 2.0, 0, 100)
	if g.st.An || !g.fensterAuf {
		t.Fatalf("bei offenem Fenster gelaufen: %s", g.grund)
	}
}

func TestSensorBatterieWarnung(t *testing.T) {
	l := berlin()
	p := neueSt(t, "scharf", time.Date(2026, 10, 5, 12, 0, 0, 0, l))
	mitSensor(p, 55)
	g := p.s.geraete[0]
	g.cfg.SensorName = "Meter Plus Spielekeller"
	p.s.sb.werte["S1"] = messFeuchte{RH: 55, Batt: 80, Zeit: time.Now()}
	if len(p.s.hinweise()) != 0 {
		t.Fatal("Warnung bei voller Batterie")
	}
	p.s.sb.werte["S1"] = messFeuchte{RH: 55, Batt: 12, Zeit: time.Now()}
	if h := p.s.hinweise(); len(h) != 1 || !strings.Contains(h[0], "Batterie wechseln") {
		t.Fatalf("keine Batteriewarnung: %v", h)
	}
	p.s.sb.werte["S1"] = messFeuchte{RH: 55, Batt: 50, Zeit: time.Now().Add(-3 * time.Hour)}
	if h := p.s.hinweise(); len(h) != 1 || !strings.Contains(h[0], "keine Werte") {
		t.Fatalf("Funkstille nicht gemeldet: %v", h)
	}
}

func TestWartetAufLueftung(t *testing.T) {
	l := berlin()
	p := neueSt(t, "scharf", time.Date(2026, 10, 5, 12, 0, 0, 0, l))
	mitSensor(p, 68)
	p.s.lueftBald.Store(p.t.Add(3 * time.Hour).Unix())
	p.laufe(20, -0.4, 0, 5)
	g := p.s.geraete[0]
	if g.st.An || !strings.Contains(g.grund, "wartet auf die Lüftung") {
		t.Fatalf("lief mit Netz trotz geplanter Lueftung: %v %s", g.st.An, g.grund)
	}
	p.laufe(20, 2.0, 0, 100) // Sonne: darf
	if !g.st.An {
		t.Fatalf("mit Sonne nicht gelaufen: %s", g.grund)
	}
	p2 := neueSt(t, "scharf", time.Date(2026, 10, 5, 12, 0, 0, 0, l))
	mitSensor(p2, 72) // sehr feucht: wartet nicht
	p2.s.lueftBald.Store(p2.t.Add(3 * time.Hour).Unix())
	p2.laufe(20, -0.4, 0, 5)
	if !p2.s.geraete[0].st.An {
		t.Fatalf("bei 72 %% gewartet: %s", p2.s.geraete[0].grund)
	}
}

func TestWaschkuecheUeberBroadlink(t *testing.T) {
	l := berlin()
	p := neueSt(t, "scharf", time.Date(2026, 10, 10, 13, 0, 0, 0, l))
	var wk *geraet
	for _, g := range p.s.geraete {
		if g.cfg.ID == "waschkueche" {
			wk = g
		}
	}
	if wk == nil {
		t.Fatal("Waschkueche fehlt in der Standardliste")
	}
	wk.cfg.Modus, wk.modusAlt = "scharf", "scharf"
	var geschaltet []bool
	p.s.blSchalte = func(mac string, an bool) error { geschaltet = append(geschaltet, an); return nil }
	if p.s.sb == nil {
		p.s.sb = neuerSwitchbot("/nicht/da", func(string, ...any) {})
	}
	p.s.sb.werte[wk.cfg.SensorID] = messFeuchte{RH: 70, Zeit: time.Now()}
	p.s.lueftPause.Store(true) // Spielekeller lueftet, die Waschkueche geht das nichts an
	p.laufe(40, -0.4, 0, 5)
	if !wk.st.An || len(geschaltet) == 0 || !geschaltet[0] {
		t.Fatalf("bei 70 %% nicht an: %s %v", wk.grund, geschaltet)
	}
	if wk.st.Stoerung != "" {
		t.Fatalf("Stoerung ohne Messung: %s", wk.st.Stoerung)
	}
}
