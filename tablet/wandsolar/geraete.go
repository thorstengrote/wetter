package main

// Geraete in der Hauslast erkennen (Wunsch vom 15.10.2026), aus den Proben
// im 10-Sekunden-Takt (lastprofil.go). Fachlich heisst das Lastzerlegung
// (NILM). Fertige Modelle aus den oeffentlichen Messreihen (UK-DALE, REFIT,
// REDD) passen hier schlecht: sie lernen an Messungen alle 1 bis 6 Sekunden
// und an fremden Geraeten. Uebernommen ist das allgemeine Wissen:
//
//   - Heizende Geraete schalten in sauberen Stufen und sind auch bei
//     langsamer Messung gut zu erkennen. Wasserkocher 1,8 bis 3 kW fuer
//     1 bis 5 Minuten. Backofen heizt durch und taktet dann am Thermostat.
//   - Wasch- und Spuelmaschine heizen mit einem Heizstab um 2 kW, die
//     Waschmaschine einmal am Anfang 10 bis 25 Minuten, die Spuelmaschine
//     zweimal (Spuelen und Klarspuelen), dazwischen nur Pumpe und Motor.
//     Die Aktivierungsgrenzen aus der Neural-NILM-Arbeit (Kelly und
//     Knottenbelt 2015, UK-DALE) setzen fuer beide mindestens 30 Minuten
//     Laufzeit an.
//   - Geraete mit Verdichter (Waermepumpe, Waermepumpentrockner) haben
//     keine saubere Stufe. Ein Inverter faehrt langsam hoch und aendert die
//     Leistung im Lauf. Sie zeigen sich als laengere erhoehte Grundlast.
//
// Was im Haus steht (Hinweise vom 15.10.2026, Daten aus Handel und Tests):
//   - Herd Stoves Richmond S900DF: Gaskochfeld, nur die Backoefen sind
//     elektrisch. Ein Elektrokochfeld zum Verwechseln gibt es nicht.
//     Leistung der Backoefen nicht gefunden, Startwert 1,2 bis 3,8 kW.
//   - Kontaktgrill Tefal OptiGrill GC702D, 2.000 W: heizt einige Minuten
//     voll vor und regelt dann im Takt, ein Grillgang dauert selten ueber
//     eine halbe Stunde.
//   - Heissluftfritteuse Panasonic NF-BC1000 (9,6 l, zwei Zonen): Leistung
//     nicht gefunden, die kleineren Panasonic-Modelle haben 1,45 kW. Taktet
//     wie der Grill und ist von ihm kaum zu trennen.
//   - Wasserkocher Kenwood kMix SKM 034, in Europa um 2,2 kW.
//   - Filterkaffeemaschine Moccamaster Select: Heizelement um 1,5 kW, eine
//     volle Kanne bruehen dauert etwa 6 Minuten, danach haelt die
//     Warmhalteplatte mit weniger als 100 W (unter der kleinsten Stufe).
//   - Spuelmaschine Smeg ST2FABBL: 0,81 kWh je Spuelgang, Eco beim
//     Schwestermodell gut 4 Stunden.
//   - Trockner Beko DPU 8306 GXE, Waermepumpe, 8 kg, in der Waschkueche.
//   - Waschmaschine Bosch in der Waschkueche, Modell unbekannt. Bosch heizt
//     wie ueblich mit einem Heizstab um 2 kW.
//   - Waermepumpe der Heizung (Hybrid mit Gas), Warmwasser seit Oktober
//     gegen 12 Uhr. Modell unbekannt.
//   - Beamer ViewSonic PX700HD, 310 W, abends ueber Stunden; mit der PS5
//     (um 200 W beim Spielen) gut 500 W. Allein ist die PS5 unter der
//     kleinsten Stufe.
//   - Kuehlschrank Siemens: taktet um 100 W, bleibt in der Grundlast.
//   - Entfeuchter Spielekeller am Shelly und Pro Breeze 12 l in der
//     Waschkueche: kennt die Steuerung, ihre Leistung wird abgezogen.
//
// Die Grenzen unten sind Startwerte. Mit markierten Laeufen (POST
// /api/geraete/marke) werden sie auf die Geraete im Haus eingestellt.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	geKante      = 350.0            // W, kleinste Stufe, die zaehlt
	geRampe      = 25 * time.Second // zwei Schritte gleicher Richtung sind eine Stufe
	gePaar       = 0.3              // Ein- und Ausschalten duerfen so weit auseinander liegen
	geLaengst    = 4 * time.Hour    // laenger bleibt keine Stufe stehen
	geTaktLuecke = 10 * time.Minute
	geDauerMin   = 300.0 // W ueber der Grundlast fuer eine Dauerlast
)

type geKanteT struct {
	T time.Time
	D float64 // W, positiv beim Einschalten
}

type geBlock struct {
	Von, Bis time.Time
	H        float64 // W
}

func (b geBlock) dauer() time.Duration { return b.Bis.Sub(b.Von) }

// geLauf: ein erkannter Lauf eines Geraets.
type geLauf struct {
	Geraet   string    `json:"geraet"`
	Von      time.Time `json:"von"`
	Bis      time.Time `json:"bis"`
	KW       float64   `json:"kw"`     // typische Leistung
	KWh      float64   `json:"kwh"`    // Energie des Laufs
	Sicher   string    `json:"sicher"` // sicher, wahrscheinlich, moeglich
	Merkmale string    `json:"merkmale"`
	Bloecke  int       `json:"bloecke,omitempty"`
}

// geKanten: Stufen in den Proben. Eine Stufe, die ueber zwei Proben steigt,
// zaehlt als eine.
func geKanten(p []lastProbe) []geKanteT {
	var k []geKanteT
	for i := 1; i < len(p); i++ {
		dt := p[i].T - p[i-1].T
		if dt <= 0 || dt > 120 {
			continue // Luecke, kein Vergleich
		}
		d := float64(p[i].W - p[i-1].W)
		t := time.Unix(p[i].T, 0)
		if n := len(k); n > 0 && t.Sub(k[n-1].T) <= geRampe && (d > 0) == (k[n-1].D > 0) && math.Abs(d) >= 100 {
			k[n-1].D += d
			k[n-1].T = t
			continue
		}
		k = append(k, geKanteT{t, d})
	}
	aus := k[:0]
	for _, x := range k {
		if math.Abs(x.D) >= geKante {
			aus = append(aus, x)
		}
	}
	return aus
}

// geBloecke: jedes Einschalten mit dem passenden Ausschalten. Offen sind
// Stufen, deren Ende noch fehlt; die passendste, juengste wird geschlossen.
func geBloecke(k []geKanteT) []geBlock {
	type offen struct {
		T time.Time
		H float64
	}
	var auf []offen
	var b []geBlock
	for _, x := range k {
		if x.D > 0 {
			auf = append(auf, offen{x.T, x.D})
			continue
		}
		best, bf := -1, gePaar
		for i := len(auf) - 1; i >= 0; i-- {
			if x.T.Sub(auf[i].T) > geLaengst {
				continue
			}
			if f := math.Abs(-x.D-auf[i].H) / auf[i].H; f < bf {
				best, bf = i, f
			}
		}
		if best < 0 {
			continue
		}
		o := auf[best]
		b = append(b, geBlock{o.T, x.T, (o.H - x.D) / 2})
		auf = append(auf[:best], auf[best+1:]...)
	}
	sort.Slice(b, func(i, j int) bool { return b[i].Von.Before(b[j].Von) })
	return b
}

// geGruppen: Bloecke aehnlicher Hoehe mit kurzen Pausen, das Takten eines
// Thermostats.
func geGruppen(b []geBlock) [][]geBlock {
	var g [][]geBlock
	frei := make([]bool, len(b))
	for i := range b {
		if frei[i] {
			continue
		}
		grp := []geBlock{b[i]}
		frei[i] = true
		for j := i + 1; j < len(b); j++ {
			letzt := grp[len(grp)-1]
			if b[j].Von.Sub(letzt.Bis) > geTaktLuecke {
				break
			}
			if !frei[j] && math.Abs(b[j].H-b[i].H)/b[i].H < 0.25 && !b[j].Von.Before(letzt.Bis) {
				grp = append(grp, b[j])
				frei[j] = true
			}
		}
		g = append(g, grp)
	}
	return g
}

func geKWh(g []geBlock) float64 {
	e := 0.0
	for _, b := range g {
		e += b.H / 1000 * b.dauer().Hours()
	}
	return e
}

// wkAntwort: wie stark die Waschkueche im Zeitraum reagiert, groesster
// Anstieg von Temperatur (K) und Feuchte (Prozentpunkte) gegen den Anfang.
func wkAntwort(pk []lastPunkt, von, bis time.Time) (dT, dRH float64, ok bool) {
	var t0, rh0 float64
	for _, p := range pk {
		if p.Zeit.Before(von) || p.Zeit.After(bis) || p.WkTemp == 0 || p.WkFenster {
			continue
		}
		if !ok {
			t0, rh0, ok = p.WkTemp, p.WkRH, true
			continue
		}
		dT = math.Max(dT, p.WkTemp-t0)
		dRH = math.Max(dRH, p.WkRH-rh0)
	}
	return
}

// erkenne: alle Laeufe in den Proben. pk sind die Minutenwerte mit
// Entfeuchtern und Waschkueche.
func erkenne(p []lastProbe, pk []lastPunkt) []geLauf {
	var l []geLauf
	// Lange niedrige Stufen sind Verdichter (Trockner, Waermepumpe), auch
	// wenn sie sauber schalten: die bewertet dauerlasten.
	var bl []geBlock
	for _, b := range geBloecke(geKanten(p)) {
		if !(b.dauer() >= 45*time.Minute && b.H < 1600) {
			bl = append(bl, b)
		}
	}
	gr := geGruppen(bl)
	mm := func(d time.Duration) float64 { return d.Minutes() }
	benutzt := map[int]bool{}
	for i, g := range gr {
		if benutzt[i] {
			continue
		}
		a, z := g[0].Von, g[len(g)-1].Bis
		h := g[0].H
		erst := mm(g[0].dauer())
		lauf := geLauf{Von: a, Bis: z, KW: math.Round(h) / 1000, KWh: geKWh(g), Bloecke: len(g)}
		switch {
		// Kontaktgrill oder Fritteuse: kurz vorheizen, kurzer Gang im Takt
		case len(g) >= 3 && h >= 1300 && h <= 2300 && erst <= 10 && mm(z.Sub(a)) <= 45:
			lauf.Geraet, lauf.Sicher = "OptiGrill oder Heißluftfritteuse", "moeglich"
			lauf.Merkmale = fmt.Sprintf("heizt %.0f min vor, %d Takte in %.0f min", erst, len(g)-1, mm(z.Sub(a)))
		// Backofen: Durchheizen, dann mindestens zwei Takte
		case len(g) >= 3 && h >= 1200 && h <= 3800 && erst >= 4 && mm(z.Sub(a)) >= 15:
			lauf.Geraet, lauf.Sicher = "Backofen", "wahrscheinlich"
			lauf.Merkmale = fmt.Sprintf("heizt %.0f min durch, dann %d Takte", erst, len(g)-1)
		// Kaffeemaschine: ein Block um 1,5 kW, einige Minuten Bruehen
		case len(g) == 1 && h >= 1300 && h < 1800 && erst >= 3 && erst <= 10:
			lauf.Geraet, lauf.Sicher = "Kaffeemaschine", "moeglich"
			lauf.Merkmale = fmt.Sprintf("%.1f min Brühen", erst)
		// Wasserkocher: ein kurzer hoher Block
		case len(g) == 1 && h >= 1800 && h <= 3200 && erst >= 0.5 && erst <= 6:
			lauf.Geraet, lauf.Sicher = "Wasserkocher", "moeglich"
			lauf.Merkmale = fmt.Sprintf("%.1f min", erst)
		// Heizstab um 2 kW: Wasch- oder Spuelmaschine
		case len(g) <= 2 && h >= 1600 && h <= 2700 && erst >= 6 && erst <= 40:
			lauf.Geraet, lauf.Sicher = "Waschmaschine oder Spülmaschine", "moeglich"
			lauf.Merkmale = fmt.Sprintf("Heizstab %.0f min", erst)
			// zweiter Heizblock 15 bis 120 Minuten spaeter: Spuelmaschine
			for j := i + 1; j < len(gr); j++ {
				x := gr[j][0]
				d := x.Von.Sub(z)
				if d > 2*time.Hour {
					break
				}
				if d >= 15*time.Minute && x.H >= 1600 && x.H <= 2700 && mm(x.dauer()) >= 4 && mm(x.dauer()) <= 40 {
					lauf.Geraet, lauf.Sicher = "Spülmaschine", "wahrscheinlich"
					lauf.Merkmale = fmt.Sprintf("zwei Heizphasen, %.0f und %.0f min, %.0f min auseinander", erst, mm(x.dauer()), mm(d))
					lauf.Bis = gr[j][len(gr[j])-1].Bis
					lauf.KWh += geKWh(gr[j])
					benutzt[j] = true
					break
				}
			}
			if lauf.Geraet != "Spülmaschine" {
				if dT, dRH, ok := wkAntwort(pk, a, a.Add(3*time.Hour)); ok && (dRH >= 2 || dT >= 0.4) {
					lauf.Geraet, lauf.Sicher = "Waschmaschine", "wahrscheinlich"
					lauf.Merkmale += fmt.Sprintf(", Waschküche danach +%.1f K, +%.0f %%", dT, dRH)
				}
			}
		// Kurze Last um 1 kW: Mikrowelle, Toaster, Haartrockner
		case len(g) <= 2 && h >= 600 && h < 1600 && erst <= 10:
			lauf.Geraet, lauf.Sicher = "Kleingerät um 1 kW", "moeglich"
			lauf.Merkmale = fmt.Sprintf("%.1f min, Mikrowelle, Toaster oder Haartrockner", erst)
		default:
			lauf.Geraet, lauf.Sicher = fmt.Sprintf("unbekannt, %.1f kW", h/1000), "moeglich"
			lauf.Merkmale = fmt.Sprintf("%d Blöcke, erster %.0f min", len(g), erst)
		}
		l = append(l, lauf)
	}
	l = append(l, dauerlasten(p, bl, pk)...)
	sort.Slice(l, func(i, j int) bool { return l[i].Von.Before(l[j].Von) })
	return l
}

// dauerlasten: was nach Abzug der Bloecke und Entfeuchter laenger ueber der
// Grundlast liegt, je Minute. Trockner und Waermepumpe.
func dauerlasten(p []lastProbe, bl []geBlock, pk []lastPunkt) []geLauf {
	if len(p) == 0 {
		return nil
	}
	// Minutenmittel der Proben
	type min struct {
		summe float64
		n     int
	}
	mi := map[int64]*min{}
	for _, x := range p {
		m := x.T / 60 * 60
		if mi[m] == nil {
			mi[m] = &min{}
		}
		mi[m].summe += float64(x.W)
		mi[m].n++
	}
	entf := map[int64]float64{}
	for _, x := range pk {
		entf[x.Zeit.Unix()] = (x.EntfKeller + x.EntfWk) * 1000
	}
	var zeiten []int64
	for t := range mi {
		zeiten = append(zeiten, t)
	}
	sort.Slice(zeiten, func(i, j int) bool { return zeiten[i] < zeiten[j] })
	rest := make([]float64, len(zeiten))
	for i, t := range zeiten {
		w := mi[t].summe/float64(mi[t].n) - entf[t]
		tt := time.Unix(t+30, 0)
		for _, b := range bl {
			if !tt.Before(b.Von) && tt.Before(b.Bis) {
				w -= b.H
			}
		}
		rest[i] = w
	}
	// Grundlast: das untere Zehntel des Tages
	s := append([]float64(nil), rest...)
	sort.Float64s(s)
	grund := s[len(s)/10]
	var l []geLauf
	start := -1
	schliesse := func(e int) {
		von, bis := time.Unix(zeiten[start], 0), time.Unix(zeiten[e], 0).Add(time.Minute)
		d := bis.Sub(von)
		if d < 20*time.Minute {
			return
		}
		e2, sum := 0.0, 0.0
		for i := start; i <= e; i++ {
			e2 += (rest[i] - grund) / 1000 / 60
			sum += rest[i] - grund
		}
		kw := sum / float64(e-start+1) / 1000
		lauf := geLauf{Von: von, Bis: bis, KW: math.Round(kw*100) / 100, KWh: e2, Sicher: "moeglich"}
		lt := von.In(ort)
		dT, _, ok := wkAntwort(pk, von, von.Add(minDur(d, time.Hour)))
		switch {
		case lt.Hour() == 11 && lt.Minute() >= 30 || lt.Hour() == 12 && lt.Minute() <= 45:
			lauf.Geraet, lauf.Sicher = "Wärmepumpe, Warmwasser", "wahrscheinlich"
			lauf.Merkmale = fmt.Sprintf("%.0f min um die Warmwasserzeit", d.Minutes())
		case kw >= 0.3 && kw <= 1.1 && d >= time.Hour && d <= 5*time.Hour && ok && dT >= 0.3:
			lauf.Geraet, lauf.Sicher = "Trockner", "wahrscheinlich"
			lauf.Merkmale = fmt.Sprintf("%.0f min, Waschküche +%.1f K", d.Minutes(), dT)
		case kw >= 0.25 && kw <= 0.6 && d >= 45*time.Minute && d <= 5*time.Hour && lt.Hour() >= 17:
			lauf.Geraet = "Beamer, vielleicht mit PS5"
			lauf.Merkmale = fmt.Sprintf("%.0f min am Abend ohne Wärme in der Waschküche", d.Minutes())
		case kw >= 0.3 && kw <= 1.1 && d >= time.Hour && d <= 5*time.Hour:
			lauf.Geraet = "Trockner oder Wärmepumpe"
			lauf.Merkmale = fmt.Sprintf("%.0f min ohne Wärme in der Waschküche", d.Minutes())
		default:
			lauf.Geraet = "Wärmepumpe oder Dauerlast"
			lauf.Merkmale = fmt.Sprintf("%.0f min", d.Minutes())
		}
		l = append(l, lauf)
	}
	for i := range rest {
		hoch := rest[i]-grund >= geDauerMin
		luecke := i > 0 && zeiten[i]-zeiten[i-1] > 300
		if start >= 0 && (!hoch || luecke) {
			schliesse(i - 1)
			start = -1
		}
		if hoch && start < 0 {
			start = i
		}
	}
	if start >= 0 {
		schliesse(len(rest) - 1)
	}
	return l
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------- Marken

// geMarke: ein Lauf, den jemand im Haus benannt hat.
type geMarke struct {
	Geraet string    `json:"geraet"`
	Von    time.Time `json:"von"`
	Bis    time.Time `json:"bis,omitempty"` // leer, solange es laeuft
	Notiz  string    `json:"notiz,omitempty"`
}

type geraete struct {
	sync.Mutex
	pfad   string
	marken []geMarke
	lp     *lastprofil
}

func neueGeraete(pfad string, lp *lastprofil) *geraete {
	g := &geraete{pfad: pfad, lp: lp}
	if roh, err := os.ReadFile(pfad); err == nil {
		json.Unmarshal(roh, &g.marken)
	}
	return g
}

// bediene: GET /api/geraete?h=24 erkennt die Laeufe der letzten Stunden;
// POST /api/geraete/marke {geraet, von, bis} merkt einen benannten Lauf,
// ohne bis beginnt er jetzt, {geraet, ende:true} beendet den offenen.
func (g *geraete) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/api/geraete", func(w http.ResponseWriter, r *http.Request) {
		h, err := strconv.Atoi(r.URL.Query().Get("h"))
		if err != nil || h <= 0 {
			h = 24
		}
		ab := time.Now().Add(-time.Duration(h) * time.Hour)
		g.lp.Lock()
		var p []lastProbe
		for _, x := range g.lp.roh {
			if x.T >= ab.Unix() {
				p = append(p, x)
			}
		}
		var pk []lastPunkt
		for _, x := range g.lp.punkte {
			if !x.Zeit.Before(ab) {
				pk = append(pk, x)
			}
		}
		g.lp.Unlock()
		l := erkenne(p, pk)
		for i := range l {
			l[i].Von, l[i].Bis = l[i].Von.In(ort), l[i].Bis.In(ort)
			l[i].KWh = math.Round(l[i].KWh*100) / 100
		}
		g.Lock()
		defer g.Unlock()
		jsonAntwort(w, map[string]any{"laeufe": l, "marken": g.marken})
	})
	mux.HandleFunc("/api/geraete/marke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "nur POST", http.StatusMethodNotAllowed)
			return
		}
		var m struct {
			geMarke
			Ende bool `json:"ende"`
		}
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil || m.Geraet == "" {
			http.Error(w, "geraet fehlt", http.StatusBadRequest)
			return
		}
		jetzt := time.Now().In(ort)
		g.Lock()
		defer g.Unlock()
		if m.Ende {
			for i := len(g.marken) - 1; i >= 0; i-- {
				if g.marken[i].Geraet == m.Geraet && g.marken[i].Bis.IsZero() {
					g.marken[i].Bis = jetzt
					schreibeJSON(g.pfad, g.marken)
					jsonAntwort(w, g.marken[i])
					return
				}
			}
			http.Error(w, "kein offener Lauf", http.StatusNotFound)
			return
		}
		if m.Von.IsZero() {
			m.Von = jetzt
		}
		g.marken = append(g.marken, m.geMarke)
		schreibeJSON(g.pfad, g.marken)
		jsonAntwort(w, m.geMarke)
	})
}
