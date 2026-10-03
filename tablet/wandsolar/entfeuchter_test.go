package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type efProbe struct {
	e      *entfeuchter
	rufe   []bool
	t      time.Time
	scharf string
}

func neueProbe(t *testing.T, scharf bool, start time.Time) *efProbe {
	ort, _ = time.LoadLocation("Europe/Berlin")
	dir := t.TempDir()
	p := &efProbe{t: start, scharf: filepath.Join(dir, "scharf")}
	if scharf {
		os.WriteFile(p.scharf, nil, 0644)
	}
	p.e = neuerEntfeuchter(filepath.Join(dir, "ef.json"), p.scharf, "192.168.2.160", "", 0.4,
		func(string, ...any) {})
	p.e.schalte = func(ip string, an bool) (float64, error) {
		p.rufe = append(p.rufe, an)
		return 0.38, nil
	}
	return p
}

// laufe misst n Minuten lang alle 30 Sekunden mit diesen Werten.
func (p *efProbe) laufe(min int, netz, akku float64) {
	for i := 0; i < min*2; i++ {
		p.t = p.t.Add(30 * time.Second)
		p.e.pruefe(messwert{Zeit: p.t, Netz: netz, Akku: akku})
	}
}

func TestFenster(t *testing.T) {
	ort, _ = time.LoadLocation("Europe/Berlin")
	faelle := []struct {
		t  time.Time
		ok bool
	}{
		{time.Date(2026, 10, 5, 10, 59, 0, 0, ort), false}, // Montag
		{time.Date(2026, 10, 5, 11, 0, 0, 0, ort), true},
		{time.Date(2026, 10, 5, 17, 59, 0, 0, ort), true},
		{time.Date(2026, 10, 5, 18, 0, 0, 0, ort), false},
		{time.Date(2026, 10, 3, 12, 0, 0, 0, ort), false}, // Samstag
		{time.Date(2026, 10, 3, 13, 0, 0, 0, ort), true},
		{time.Date(2026, 10, 4, 17, 30, 0, 0, ort), true}, // Sonntag
	}
	for _, f := range faelle {
		if efFenster(f.t) != f.ok {
			t.Errorf("%v: erwartet %v", f.t, f.ok)
		}
	}
}

func TestEinErstNachFuenfMinuten(t *testing.T) {
	p := neueProbe(t, true, time.Date(2026, 10, 5, 12, 0, 0, 0, ort))
	p.laufe(4, 1.0, 0)
	if p.e.st.An {
		t.Fatal("nach vier Minuten schon an")
	}
	p.laufe(2, 1.0, 0)
	if !p.e.st.An {
		t.Fatal("nach sechs Minuten Ueberschuss nicht an")
	}
	// Zu wenig Einspeisung fuer 0,4 kW plus Reserve schaltet nicht ein.
	q := neueProbe(t, true, time.Date(2026, 10, 5, 12, 0, 0, 0, ort))
	q.laufe(20, 0.45, 0)
	if q.e.st.An {
		t.Fatal("bei 0,45 kW eingeschaltet")
	}
}

func TestAusBeiMangelErstNachMindestlaufzeit(t *testing.T) {
	p := neueProbe(t, true, time.Date(2026, 10, 5, 12, 0, 0, 0, ort))
	// Die Mindestlaufzeit ist heute schon erfuellt, sonst haelt sie ihn an.
	p.e.st.Woche, p.e.st.Tag = efWochenKey(p.t), p.t.Format("2006-01-02")
	p.e.st.Sekunden, p.e.st.TagSek = 6*3600, 3600
	p.laufe(6, 1.0, 0)
	if !p.e.st.An {
		t.Fatal("nicht an")
	}
	// Sofort Wolke: Akku entlaedt. Vor zehn Minuten Laufzeit bleibt er an.
	p.laufe(5, 0, -0.3)
	if !p.e.st.An {
		t.Fatal("vor der Mindestlaufzeit aus")
	}
	p.laufe(6, 0, -0.3)
	if p.e.st.An {
		t.Fatal("bei Akkuentladung nach Mindestlaufzeit noch an")
	}
	// Kurz danach wieder Sonne: erst nach der Pause wieder ein.
	p.laufe(6, 1.0, 0)
	if p.e.st.An {
		t.Fatal("Pause nach dem Abschalten nicht eingehalten")
	}
	p.laufe(6, 1.0, 0)
	if !p.e.st.An {
		t.Fatal("nach der Pause nicht wieder an")
	}
}

func TestFensterendeUndWochenbudget(t *testing.T) {
	p := neueProbe(t, true, time.Date(2026, 10, 5, 17, 40, 0, 0, ort))
	p.laufe(6, 1.0, 0)
	if !p.e.st.An {
		t.Fatal("nicht an")
	}
	p.laufe(15, 1.0, 0) // bis 18:01
	if p.e.st.An {
		t.Fatal("nach 18 Uhr noch an")
	}

	q := neueProbe(t, true, time.Date(2026, 10, 9, 12, 0, 0, 0, ort)) // Freitag
	q.e.st.Woche = efWochenKey(q.t)
	q.e.st.Sekunden = (30*time.Hour - 20*time.Minute).Seconds()
	q.laufe(6, 1.0, 0)
	if !q.e.st.An {
		t.Fatal("mit 20 Minuten Rest nicht an")
	}
	q.laufe(20, 1.0, 0)
	if q.e.st.An {
		t.Fatal("ueber 30 Stunden gelaufen")
	}
	if q.e.st.Sekunden > (30*time.Hour + time.Minute).Seconds() {
		t.Fatalf("Budget ueberzogen: %.0f s", q.e.st.Sekunden)
	}
	// Neue Woche, neues Budget.
	q.t = time.Date(2026, 10, 12, 12, 0, 0, 0, ort)
	q.laufe(6, 1.0, 0)
	if !q.e.st.An || q.e.st.Sekunden > 600 {
		t.Fatalf("neue Woche nicht frisch: an %v, %.0f s", q.e.st.An, q.e.st.Sekunden)
	}
}

func TestProbebetriebRuehrtShellyNichtAn(t *testing.T) {
	p := neueProbe(t, false, time.Date(2026, 10, 5, 12, 0, 0, 0, ort))
	p.laufe(60, 1.0, 0)
	if !p.e.st.An {
		t.Fatal("Probe haette eingeschaltet")
	}
	if len(p.rufe) != 0 {
		t.Fatalf("Shelly im Probebetrieb angesprochen: %v", p.rufe)
	}
	if p.e.st.Sekunden != 0 || p.e.st.ProbeSek < 3000 {
		t.Fatalf("Probe zaehlt falsch: echt %.0f, Probe %.0f", p.e.st.Sekunden, p.e.st.ProbeSek)
	}
}

func TestFalscheUhrNachNeustart(t *testing.T) {
	p := neueProbe(t, true, time.Date(2026, 10, 5, 12, 0, 0, 0, ort))
	p.laufe(6, 1.0, 0)
	vorher := p.e.st.Sekunden
	p.e.pruefe(messwert{Zeit: time.Date(2026, 5, 23, 15, 20, 0, 0, ort), Netz: -2})
	if !p.e.st.An || p.e.st.Sekunden != vorher {
		t.Fatal("Messung mit falscher Uhr verarbeitet")
	}
}

func prognose(t time.Time, pv, ein []float64) efPrognose {
	return efPrognose{Tag: t.Format("2006-01-02"), PV: pv, Ein: ein}
}

func stunden(werte map[int]float64) []float64 {
	a := make([]float64, 24)
	for h, v := range werte {
		a[h] = v
	}
	return a
}

// Grauer Tag ohne Prognose: Pflichtlauf startet so, dass er um 18 Uhr fertig ist.
func TestMindestlaufOhnePrognose(t *testing.T) {
	p := neueProbe(t, true, time.Date(2026, 10, 5, 11, 0, 0, 0, ort)) // Montag
	p.laufe(6*60, -0.3, 0)                                            // bis 17:00
	if p.e.st.An {
		t.Fatal("ohne Prognose zu frueh gestartet")
	}
	p.laufe(60, -0.3, 0) // bis 18:00
	// Montag: Wochenrest 5 h auf 7 Tage = 43 min, mehr als 30.
	if m := p.e.st.TagSek / 60; m < 40 || m > 50 {
		t.Fatalf("Montag %.0f min gelaufen, erwartet rund 43", m)
	}
	if p.e.st.An {
		t.Fatal("nach 18 Uhr noch an")
	}
}

// Grauer Tag mit Prognose: der Block liegt in der sonnigsten Stunde.
func TestMindestlaufInDerSonne(t *testing.T) {
	start := time.Date(2026, 10, 5, 11, 0, 0, 0, ort)
	p := neueProbe(t, true, start)
	p.e.setzePrognose(prognose(start, stunden(map[int]float64{11: 0.5, 12: 0.8, 13: 1.6, 14: 1.2, 15: 0.6}), make([]float64, 24)))
	p.laufe(119, -0.3, 0) // bis 12:59
	if p.e.st.An {
		t.Fatal("vor 13 Uhr gestartet")
	}
	p.laufe(2, -0.3, 0)
	if !p.e.st.An {
		t.Fatal("um 13 Uhr nicht gestartet")
	}
	p.laufe(60, -0.3, 0)
	if p.e.st.An {
		t.Fatal("nach dem Pflichtlauf nicht wieder aus")
	}
}

// Sonne erwartet: erst warten, dann aus Ueberschuss laufen, kein Pflichtlauf.
func TestMindestlaufWartetAufSonne(t *testing.T) {
	start := time.Date(2026, 10, 5, 11, 0, 0, 0, ort)
	p := neueProbe(t, true, start)
	p.e.setzePrognose(prognose(start, stunden(map[int]float64{13: 5, 14: 5}), stunden(map[int]float64{13: 2, 14: 2})))
	p.laufe(120, -0.3, 0) // bis 13:00 grau
	if p.e.st.An {
		t.Fatal("trotz erwarteter Sonne Pflichtlauf gestartet")
	}
	p.laufe(70, 1.5, 0)
	if !p.e.st.An {
		t.Fatal("bei Ueberschuss nicht an")
	}
}

// Pflichtlauf schaltet bei Bezug nicht vorzeitig ab.
func TestPflichtlaufIgnoriertMangel(t *testing.T) {
	p := neueProbe(t, true, time.Date(2026, 10, 5, 17, 0, 0, 0, ort))
	p.laufe(20, -0.5, -0.3)
	if !p.e.st.An {
		t.Fatal("spaetester Start nicht eingehalten")
	}
	if m := p.e.st.TagSek / 60; m < 1 {
		t.Fatal("keine Laufzeit")
	}
}
