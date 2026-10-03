package main

// Der Planer verteilt die erwartete Sonne der naechsten sieben Tage auf die
// Verbraucher. Er rechnet in halben Stunden und mit demselben Akkumodell wie
// die Wetterseite (schritt in index.html).
//
// Grundgedanke: Ein Lauf ist "frei", wenn er ueber den ganzen Zeitraum keinen
// zusaetzlichen Netzbezug verursacht. Das schliesst Einspeisung ein, aber
// auch Akkustrom, den die Sonne spaeter am Tag ohnehin wieder auffuellt. So
// darf ein Geraet anlaufen, bevor der Akku voll ist, wenn die Prognose
// genug Einspeisung fuer den Rest des Tages erwartet. Gegen eine zu
// freundliche Prognose rechnet der Planer dabei mit nur zwei Dritteln der
// vorhergesagten Erzeugung, und fuer heute zusaetzlich mit dem Verhaeltnis
// von gemessener zu vorhergesagter Erzeugung der bisherigen Stunden.
//
// Danach werden die Pflichten erfuellt: Mindestlaufzeit in jedem gleitenden
// 7-Tage-Raum und spaetestens alle n Tage ein Lauf. Dafuer nimmt der Planer
// die Halbstunden, die am wenigsten Netzbezug kosten. Das sind meist die
// sonnigsten oder die, in denen der Akku noch genug hat.

import (
	"math"
	"sort"
	"strconv"
	"time"
)

const (
	slotDauer    = 30 * time.Minute
	slotH        = 0.5 // Stunden je Halbstunde
	akkuKWh      = 10.0
	akkuMaxKW    = 5.0
	akkuUnten    = 5.0  // Prozent, darunter gibt der Akku nichts ab
	vorsicht     = 0.67 // Anteil der Prognose, mit dem "frei" geprueft wird
	freiSchwelle = 0.005
)

// Tagesgang des Hausverbrauchs in kW, wie in index.html.
var hausProfil = [24]float64{.30, .28, .27, .27, .28, .36, .72, 1.45, 1.15, .82, .74, .80,
	1.10, .92, .78, .76, .84, 1.25, 2.10, 2.30, 1.55, 1.02, .58, .38}

type planSlot struct {
	Zeit   time.Time `json:"zeit"`
	PV     float64   `json:"pv"`
	Haus   float64   `json:"haus"`
	An     bool      `json:"an"`
	Art    string    `json:"art,omitempty"`    // frei, pflicht
	Quelle string    `json:"quelle,omitempty"` // sonne, akku, netz
	SOC    float64   `json:"soc"`
	Ein    float64   `json:"ein"`
	Bezug  float64   `json:"bezug"`
	Erlaub bool      `json:"erlaubt"`
}

type plan struct {
	Erstellt time.Time  `json:"erstellt"`
	Faktor   float64    `json:"faktor_heute"`
	Slots    []planSlot `json:"slots"`
	Hinweise []string   `json:"hinweise"`
}

type planEingabe struct {
	Jetzt    time.Time
	SOC      float64
	PV       map[int64]float64 // Stundenbeginn (Unix) -> kW
	Faktor   float64           // gemessen / vorhergesagt, heute
	Leistung float64           // kW des Geraets
	Cfg      geraetCfg
	Erlaubt  func(time.Time) bool
	Laeufe   []lauf // Vergangenheit
	Beginn   time.Time
}

func slotBeginn(t time.Time) time.Time {
	return t.Truncate(slotDauer)
}

func tagKey(t time.Time) string { return t.Format("2006-01-02") }

// Ein Simulationsschritt, wie schritt() in der Seite.
func simSchritt(pv, last, soc float64) (socNeu, ein, bez float64) {
	rest := pv - last
	if rest > 0 {
		laden := math.Min(rest, math.Min(akkuMaxKW, math.Max(0, (100-soc)/100*akkuKWh/slotH)))
		return soc + laden*slotH/akkuKWh*100, rest - laden, 0
	}
	ab := math.Min(-rest, math.Min(akkuMaxKW, math.Max(0, (soc-akkuUnten)/100*akkuKWh/slotH)))
	return soc - ab*slotH/akkuKWh*100, 0, -rest - ab
}

type planer struct {
	in    planEingabe
	slots []planSlot
	pvV   []float64 // vorsichtige Erzeugung
	on    []bool
	art   []string
	hist  []float64 // Minuten je Halbstunde in den 7 Tagen vor dem Plan
}

const fenster7 = 7 * 48 // Halbstunden in 7 Tagen

func neuerPlaner(in planEingabe) *planer {
	p := &planer{in: in}
	start := slotBeginn(in.Jetzt)
	heute := tagKey(in.Jetzt)
	for i := 0; i < 7*48; i++ {
		t := start.Add(time.Duration(i) * slotDauer)
		std := t.Truncate(time.Hour).Unix()
		pv, ok := in.PV[std]
		if !ok {
			if len(in.PV) > 0 {
				break // Prognose reicht nicht weiter
			}
			if tagKey(t) != heute {
				break // ohne Prognose nur heute planen
			}
		}
		if tagKey(t) == heute && in.Faktor > 0 {
			pv *= in.Faktor
		}
		p.slots = append(p.slots, planSlot{Zeit: t, PV: pv, Haus: hausProfil[t.Hour()],
			Erlaub: in.Erlaubt(t)})
		p.pvV = append(p.pvV, pv*vorsicht)
	}
	p.on = make([]bool, len(p.slots))
	p.art = make([]string, len(p.slots))
	p.hist = make([]float64, fenster7)
	anfang := start.Add(-fenster7 * slotDauer)
	for _, l := range in.Laeufe {
		a, b := l.Von, l.Bis
		if b.IsZero() || b.After(start) {
			b = start
		}
		if a.Before(anfang) {
			a = anfang
		}
		for a.Before(b) {
			k := int(a.Sub(anfang) / slotDauer)
			ende := anfang.Add(time.Duration(k+1) * slotDauer)
			if ende.After(b) {
				ende = b
			}
			if k >= 0 && k < fenster7 {
				p.hist[k] += ende.Sub(a).Minutes()
			}
			a = ende
		}
	}
	return p
}

// netzAb rechnet ab Slot j bis zum Ende und gibt den Netzbezug in kWh.
// soc ist der Stand vor Slot j. Mit kipp >= 0 wird dieser Slot umgedreht,
// mit Laenge n die folgenden n Slots.
func (p *planer) netzAb(j int, soc float64, kipp, n int) float64 {
	sum := 0.0
	for i := j; i < len(p.slots); i++ {
		an := p.on[i]
		if kipp >= 0 && i >= kipp && i < kipp+n {
			an = !an
		}
		last := p.slots[i].Haus
		if an {
			last += p.in.Leistung
		}
		var bez float64
		soc, _, bez = simSchritt(p.pvV[i], last, soc)
		sum += bez * slotH
	}
	return sum
}

// socVor gibt den Stand vor jedem Slot unter dem jetzigen Plan.
func (p *planer) socVor() []float64 {
	s := make([]float64, len(p.slots)+1)
	s[0] = p.in.SOC
	for i := range p.slots {
		last := p.slots[i].Haus
		if p.on[i] {
			last += p.in.Leistung
		}
		s[i+1], _, _ = simSchritt(p.pvV[i], last, s[i])
	}
	return s
}

// kosten: zusaetzlicher Netzbezug, wenn Slots j..j+n-1 eingeschaltet werden.
func (p *planer) kosten(j, n int, soc []float64) float64 {
	return p.netzAb(j, soc[j], j, n) - p.netzAb(j, soc[j], -1, 0)
}

// Laufzeit in Minuten zwischen von und bis, Vergangenheit plus Plan.
func (p *planer) minuten(von, bis time.Time) float64 {
	m := 0.0
	jetzt := p.in.Jetzt
	for _, l := range p.in.Laeufe {
		a, b := l.Von, l.Bis
		if b.IsZero() || b.After(jetzt) {
			b = jetzt
		}
		if a.Before(von) {
			a = von
		}
		if b.After(bis) {
			b = bis
		}
		if b.After(a) {
			m += b.Sub(a).Minutes()
		}
	}
	for i, s := range p.slots {
		if !p.on[i] {
			continue
		}
		a, b := s.Zeit, s.Zeit.Add(slotDauer)
		if i == 0 && jetzt.After(a) {
			a = jetzt // der laufende Slot zaehlt nur ab jetzt
		}
		if a.Before(von) {
			a = von
		}
		if b.After(bis) {
			b = bis
		}
		if b.After(a) {
			m += b.Sub(a).Minutes()
		}
	}
	return m
}

func tagesAnfang(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// hatLauf: gibt es zwischen von und bis einen Lauf von mindestens minLauf
// Minuten am Stueck, vergangen oder geplant?
func (p *planer) hatLauf(von, bis time.Time, minLauf float64) bool {
	for _, l := range p.in.Laeufe {
		b := l.Bis
		if b.IsZero() {
			b = p.in.Jetzt
		}
		if b.After(von) && l.Von.Before(bis) && b.Sub(l.Von).Minutes() >= minLauf {
			return true
		}
	}
	// Geplante Bloecke, ein laufender Lauf am Anfang zaehlt mit.
	blockStart := time.Time{}
	for i, s := range p.slots {
		if p.on[i] {
			if blockStart.IsZero() {
				blockStart = s.Zeit
				if i == 0 {
					for _, l := range p.in.Laeufe {
						if l.Bis.IsZero() {
							blockStart = l.Von
						}
					}
				}
			}
			ende := s.Zeit.Add(slotDauer)
			if ende.Sub(blockStart).Minutes() >= minLauf && ende.After(von) && blockStart.Before(bis) {
				return true
			}
		} else {
			blockStart = time.Time{}
		}
	}
	return false
}

// passtMax: bleibt jeder 7-Tage-Raum, der die Slots i..i+n-1 enthaelt,
// unter dem Hoechstwert? Gezaehlt auf einer Zeitleiste aus Halbstunden, die
// 7 Tage vor dem Plan beginnt.
func (p *planer) passtMax(i, n int) bool {
	maxM := p.in.Cfg.MaxH7 * 60
	if maxM <= 0 {
		return true
	}
	wert := func(k int) float64 {
		if k < fenster7 {
			if k < 0 {
				return 0
			}
			return p.hist[k]
		}
		if j := k - fenster7; j < len(p.on) && p.on[j] {
			return 30
		}
		return 0
	}
	ki := fenster7 + i
	lo := ki + n - fenster7
	if lo < 0 {
		lo = 0
	}
	sum := 0.0
	for k := lo; k < lo+fenster7; k++ {
		sum += wert(k)
	}
	for st := lo; st <= ki; st++ {
		if st > lo {
			sum += wert(st+fenster7-1) - wert(st-1)
		}
		// Anteil des neuen Blocks in diesem Raum
		a, b := ki, ki+n
		if a < st {
			a = st
		}
		if b > st+fenster7 {
			b = st + fenster7
		}
		if sum+float64(b-a)*30 > maxM {
			return false
		}
	}
	return true
}

func (p *planer) frei(i int, soc []float64) bool {
	if !p.in.Cfg.Vorziehen {
		// Nur echte Einspeisung: ohne Geraet muss mindestens seine
		// Leistung ins Netz gehen.
		last := p.slots[i].Haus
		_, ein, _ := simSchritt(p.pvV[i], last, soc[i])
		return ein >= p.in.Leistung
	}
	return p.kosten(i, 1, soc) <= freiSchwelle
}

func (p *planer) rechne() plan {
	cfg := p.in.Cfg
	var hinweise []string

	// 1. Freie Laeufe, gleichmaessig ueber die Tage: die naechste freie
	// Halbstunde bekommt der Tag mit der bisher kleinsten Laufzeit, in ihm
	// die mit der meisten Einspeisung. Vorher ging es nur nach Einspeisung,
	// und bei knapper Hoechstgrenze ballte sich alles auf die sonnigsten
	// Tage (03.10.2026: Mo und Di je 5 h, Do 0, obwohl Do auch frei war).
	tagMin := map[string]float64{}
	for i := range p.slots {
		if p.on[i] {
			tagMin[tagKey(p.slots[i].Zeit)] += 30
		}
	}
	for runde := 0; runde < len(p.slots); runde++ {
		soc := p.socVor()
		best, bestEin, bestTag := -1, -1.0, math.Inf(1)
		for i := range p.slots {
			if p.on[i] || !p.slots[i].Erlaub || !p.passtMax(i, 1) || !p.frei(i, soc) {
				continue
			}
			_, ein, _ := simSchritt(p.pvV[i], p.slots[i].Haus, soc[i])
			tm := tagMin[tagKey(p.slots[i].Zeit)]
			if tm < bestTag-1e-9 || (tm < bestTag+1e-9 && ein > bestEin+1e-9) {
				best, bestEin, bestTag = i, ein, tm
			}
		}
		if best < 0 {
			break
		}
		p.on[best], p.art[best] = true, "frei"
		tagMin[tagKey(p.slots[best].Zeit)] += 30
	}

	// 2. Pflichten, Tag fuer Tag.
	if len(p.slots) > 0 {
		minLauf := math.Max(cfg.MinLaufMin, 1)
		nLauf := int(math.Ceil(minLauf / 30))
		erster := tagesAnfang(p.slots[0].Zeit)
		letzter := tagesAnfang(p.slots[len(p.slots)-1].Zeit)
		beginn := tagesAnfang(p.in.Beginn)
		for d := erster; !d.After(letzter); d = d.AddDate(0, 0, 1) {
			ende := d.AddDate(0, 0, 1)
			// Luecke: in den letzten n Tagen bis einschliesslich d ein Lauf.
			if n := cfg.MaxLueckeTage; n > 0 {
				von := d.AddDate(0, 0, -(n - 1))
				if !von.Before(beginn) && !p.hatLauf(von, ende, minLauf) {
					if !p.erfuelle(von, ende, nLauf, "Lauf alle "+itoa(n)+" Tage") {
						hinweise = append(hinweise, d.Format("02.01.")+": kein Lauf moeglich, Abstand ueber "+itoa(n)+" Tage")
					}
				}
			}
			// Mindestlaufzeit im 7-Tage-Raum, anteilig ab Beginn.
			if cfg.MinH7 > 0 {
				von := d.AddDate(0, 0, -6)
				tage := 7.0
				if von.Before(beginn) {
					tage = math.Max(0, ende.Sub(beginn).Hours()/24)
					von = beginn
				}
				soll := cfg.MinH7 * 60 * tage / 7
				for p.minuten(von, ende) < soll-1 {
					if !p.erfuelle(von, ende, 1, "Mindestlaufzeit") {
						hinweise = append(hinweise, d.Format("02.01.")+": Mindestlaufzeit nicht erreichbar")
						break
					}
				}
			}
		}
	}

	// 3. Ergebnis mit der unverminderten Prognose, so wie es wohl kommt.
	soc := p.in.SOC
	for i := range p.slots {
		s := &p.slots[i]
		s.An, s.Art = p.on[i], p.art[i]
		last := s.Haus
		if s.An {
			last += p.in.Leistung
		}
		soc, s.Ein, s.Bezug = simSchritt(s.PV, last, soc)
		s.SOC = soc
		if s.An {
			switch {
			case s.PV >= last:
				s.Quelle = "sonne"
			case s.Bezug > 0.01:
				s.Quelle = "netz"
			default:
				s.Quelle = "akku"
			}
		}
	}
	if hinweise == nil {
		hinweise = []string{}
	}
	return plan{Erstellt: p.in.Jetzt, Faktor: p.in.Faktor, Slots: p.slots, Hinweise: hinweise}
}

// erfuelle schaltet den guenstigsten Block aus n Halbstunden zwischen von und
// bis ein. false, wenn keiner geht.
func (p *planer) erfuelle(von, bis time.Time, n int, warum string) bool {
	soc := p.socVor()
	type kand struct {
		i     int
		k, pv float64
	}
	var ks []kand
	for i := 0; i+n <= len(p.slots); i++ {
		ok := true
		pv := 0.0
		for j := i; j < i+n; j++ {
			s := p.slots[j]
			if p.on[j] || !s.Erlaub || s.Zeit.Before(von) || !s.Zeit.Before(bis) {
				ok = false
				break
			}
			pv += s.PV
		}
		if !ok || !p.passtMax(i, n) {
			continue
		}
		k := p.kosten(i, n, soc)
		if !p.in.Cfg.NetzErlaubt && k > freiSchwelle {
			continue
		}
		ks = append(ks, kand{i, k, pv})
	}
	if len(ks) == 0 {
		return false
	}
	sort.Slice(ks, func(a, b int) bool {
		if math.Abs(ks[a].k-ks[b].k) > 0.01 {
			return ks[a].k < ks[b].k
		}
		if math.Abs(ks[a].pv-ks[b].pv) > 0.05 {
			return ks[a].pv > ks[b].pv
		}
		return ks[a].i > ks[b].i // bei Gleichstand spaeter, die Sonne kann noch kommen
	})
	for j := ks[0].i; j < ks[0].i+n; j++ {
		p.on[j], p.art[j] = true, "pflicht"
	}
	return true
}

func itoa(n int) string { return strconv.Itoa(n) }
