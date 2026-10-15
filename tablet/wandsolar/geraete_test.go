package main

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

// Ein nachgebauter Tag: Grundlast mit Rauschen, darauf die Geraete nach
// ihren Signaturen. Jedes muss als das erkannt werden, was es ist.
func TestGeraeteErkennen(t *testing.T) {
	t0 := time.Date(2026, 10, 16, 6, 0, 0, 0, berlin())
	r := rand.New(rand.NewSource(1))
	last := map[int64]float64{} // zusaetzliche Watt je 10 s
	an := func(von time.Duration, dauer time.Duration, w float64) {
		for d := time.Duration(0); d < dauer; d += 10 * time.Second {
			last[t0.Add(von+d).Unix()] += w
		}
	}
	m := func(n float64) time.Duration { return time.Duration(n * float64(time.Minute)) }
	// 06:30 Wasserkocher
	an(m(30), m(3), 2200)
	// 07:00 Kaffee
	an(m(60), m(6), 1500)
	// 08:00 Spuelmaschine: zwei Heizphasen
	an(m(120), m(15), 2000)
	an(m(195), m(10), 2000)
	// 11:00 Backofen: 12 min durchheizen, dann 2 an, 3 aus
	an(m(300), m(12), 2500)
	for k := 0; k < 8; k++ {
		an(m(315+5*float64(k)), m(2), 2500)
	}
	// 13:00 OptiGrill: 6 min vor, dann 1 an, 1 aus
	an(m(420), m(6), 2000)
	for k := 0; k < 8; k++ {
		an(m(427+2*float64(k)), m(1), 2000)
	}
	// 14:00 Brunnenpumpe, 25 min
	an(m(480), m(25), 1300)
	// 15:00 Trockner: 2 h um 650 W, Waschkueche wird waermer
	an(m(540), m(120), 650)
	var p []lastProbe
	var pk []lastPunkt
	for d := time.Duration(0); d < 14*time.Hour; d += 10 * time.Second {
		ts := t0.Add(d)
		p = append(p, lastProbe{ts.Unix(), int(250 + r.Float64()*40 + last[ts.Unix()])})
		if d%time.Minute == 0 {
			temp := 18.6
			if d >= m(540) {
				temp += 0.5 * minF((d-m(540)).Minutes()/60, 1)
			}
			pk = append(pk, lastPunkt{Zeit: ts, WkTemp: temp, WkRH: 62})
		}
	}
	l := erkenne(p, pk)
	var namen []string
	for _, x := range l {
		namen = append(namen, x.Von.In(ort).Format("15:04")+" "+x.Geraet)
	}
	alle := strings.Join(namen, " | ")
	for _, soll := range []string{"06:30 Wasserkocher", "07:00 Kaffeemaschine", "08:00 Spülmaschine", "11:00 Backofen",
		"13:00 OptiGrill oder Heißluftfritteuse", "14:00 Brunnenpumpe", "15:00 Trockner"} {
		if !strings.Contains(alle, soll) {
			t.Errorf("fehlt %q in %s", soll, alle)
		}
	}
	if len(l) != 7 {
		t.Errorf("%d Laeufe statt 7: %s", len(l), alle)
	}
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
