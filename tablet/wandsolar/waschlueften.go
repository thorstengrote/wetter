package main

// Lueften der Waschkueche von Hand (Wunsch vom 13.10.2026). Das Fenster dort
// hat keinen Motor, nur einen SwitchBot-Kontakt. Die Wand soll im
// Vorbeigehen sagen:
//
//   - "Waschküche lüften: jetzt bis 18 Uhr", wenn es angebracht ist: die
//     Luft ist nicht schon trocken, die letzte Lueftung liegt drei Tage
//     zurueck oder die Feuchte steht ueber der Obergrenze, und draussen ist die
//     Luft fuer mindestens drei Stunden deutlich trockener (Taupunkt 3 K
//     unter drinnen), bei Tag (7 bis 21 Uhr) und ohne Frost.
//   - "Waschküche offen seit 3 h", solange das Fenster offen ist und es
//     etwas bringt.
//   - "Waschküche: Fenster schließen", wenn draussen die Luft feuchter wird,
//     drinnen trocken genug ist, Frost kommt oder es einen Tag offen war.
//
// Auskuehlen ist in dem Raum egal, dort haelt sich niemand auf. Schimmel ist
// schlimmer. Waehrend das Fenster offen ist, steht das Heizventil auf aus,
// danach wieder auf dem alten Wert.
//
// Pruefung ueber die Raumluft: Der Kontakt allein ist nicht verlaesslich,
// die Cloud liefert bei verlorenem Kontakt den alten Stand weiter (13.10.).
// Ist es draussen deutlich kaelter, muss der Raum nach dem Oeffnen
// abkuehlen oder die Feuchte sich der Aussenluft naehern. Tut sich nach
// einer Stunde nichts, steht "Kontakt prüfen?" dabei, und nach drei Stunden
// glaubt der Entfeuchter dem Kontakt nicht mehr.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
	"time"
)

type wkHinweis struct {
	Art   string `json:"art"` // empfehlung, offen, schliessen
	Text  string `json:"text"`
	Unter string `json:"unter,omitempty"`
}

type wkStand struct {
	OffenSeit      time.Time `json:"offen_seit"`
	TempStart      float64   `json:"temp_start"`
	AbsStart       float64   `json:"abs_start"`
	Verifiziert    bool      `json:"verifiziert"`
	Zweifel        bool      `json:"zweifel"`
	LetztesLueften time.Time `json:"letztes_lueften"` // Ende der letzten Lueftung ueber eine Stunde
	VentilAIN      string    `json:"ventil_ain,omitempty"`
	VentilVorher   float64   `json:"ventil_vorher,omitempty"`
	VentilAus      bool      `json:"ventil_aus,omitempty"`
}

type wkLueften struct {
	sync.Mutex
	pfad    string
	st      *steuerung
	sag     func(string, ...any)
	ventil  func() (ventil, bool)
	setze   func(ain string, grad float64) error
	s       wkStand
	hinweis *wkHinweis
}

const (
	wkTage        = 3   // alle paar Tage
	wkAbstandAuf  = 3.0 // K Taupunkt draussen unter drinnen, um zu empfehlen
	wkAbstandZu   = 1.0 // darunter bringt es nichts mehr
	wkMinStunden  = 3   // so lange muss es draussen passen
	wkFrost       = 2.0 // °C, Wasserleitungen in der Waschkueche
	wkGlaubeOffen = 3 * time.Hour
)

func neueWkLueften(pfad string, st *steuerung, sag func(string, ...any)) *wkLueften {
	w := &wkLueften{pfad: pfad, st: st, sag: sag}
	if roh, err := os.ReadFile(pfad); err == nil {
		json.Unmarshal(roh, &w.s)
	}
	return w
}

// lage: was die Steuerung gerade ueber die Waschkueche weiss.
type wkLage struct {
	cfg     geraetCfg
	innen   *messFeuchte
	kontakt kontaktWert
	frisch  bool
	luft    map[int64]luftWert
}

func (w *wkLueften) lage() (wkLage, bool) {
	s := w.st
	s.Lock()
	var l wkLage
	ok := false
	for _, g := range s.geraete {
		if g.cfg.ID == "waschkueche" && g.cfg.KontaktID != "" {
			l.cfg, ok = g.cfg, true
		}
	}
	l.luft = s.luft
	sb := s.sb
	s.Unlock()
	if !ok || sb == nil {
		return l, false
	}
	if m, ok := sb.wert(l.cfg.SensorID); ok {
		l.innen = &m
	}
	l.kontakt, l.frisch = sb.kontakt(l.cfg.KontaktID)
	return l, true
}

func (w *wkLueften) schritt(t time.Time) {
	l, ok := w.lage()
	w.Lock()
	defer w.Unlock()
	w.hinweis = nil
	if !ok {
		return
	}
	offen := l.frisch && l.kontakt.Offen
	if offen {
		w.offen(t, l)
	} else if l.frisch && !w.s.OffenSeit.IsZero() {
		w.geschlossen(t)
	}
	if offen {
		w.hinweis = w.offenHinweis(t, l)
	} else if l.frisch {
		w.hinweis = w.empfehlung(t, l)
	}
	w.st.wkVerifiziert.Store(w.s.Verifiziert)
	schreibeJSON(w.pfad, w.s)
}

func (w *wkLueften) offen(t time.Time, l wkLage) {
	if w.s.OffenSeit.IsZero() {
		w.s.OffenSeit, w.s.Verifiziert, w.s.Zweifel = l.kontakt.Seit, false, false
		if w.s.OffenSeit.IsZero() || w.s.OffenSeit.After(t) {
			w.s.OffenSeit = t
		}
		if l.innen != nil {
			w.s.TempStart = l.innen.Temp
			w.s.AbsStart = absFeuchte(l.innen.Temp, taupunkt(l.innen.Temp, l.innen.RH))
		}
		w.sag("Waschküche: Fenster offen")
		w.ventilAus()
	}
	// Pruefung ueber die Raumluft, sobald der Fuehler etwas sehen kann
	aus, ausOk := luftZu(l.luft, t)
	dauer := t.Sub(w.s.OffenSeit)
	if w.s.Verifiziert || l.innen == nil || !ausOk || dauer < 30*time.Minute {
		return
	}
	abs := absFeuchte(l.innen.Temp, taupunkt(l.innen.Temp, l.innen.RH))
	absAus := absFeuchte(aus.Temp, aus.Taupunkt)
	kuehler := aus.Temp < w.s.TempStart-2 && l.innen.Temp <= w.s.TempStart-0.4
	richtung := math.Abs(abs-absAus) <= math.Abs(w.s.AbsStart-absAus)-0.5
	if kuehler || richtung {
		w.s.Verifiziert, w.s.Zweifel = true, false
		return
	}
	// Ohne Unterschied zwischen drinnen und draussen laesst sich nichts sagen
	merklich := aus.Temp < w.s.TempStart-3 || math.Abs(w.s.AbsStart-absAus) > 2
	if dauer >= time.Hour && merklich && !w.s.Zweifel {
		w.s.Zweifel = true
		w.sag("Waschküche: Kontakt meldet offen, die Raumluft reagiert nicht")
	}
}

func (w *wkLueften) geschlossen(t time.Time) {
	dauer := t.Sub(w.s.OffenSeit)
	if dauer >= time.Hour {
		w.s.LetztesLueften = t
	}
	w.sag("Waschküche: Fenster zu nach %s", dauerKurz(dauer))
	w.s.OffenSeit, w.s.Verifiziert, w.s.Zweifel = time.Time{}, false, false
	w.ventilZurueck()
}

// ventilAus: Heizung aus, solange gelueftet wird. Steht das Ventil schon
// auf aus, bleibt nichts zurueckzustellen.
func (w *wkLueften) ventilAus() {
	if w.ventil == nil || w.setze == nil {
		return
	}
	v, ok := w.ventil()
	if !ok || v.Soll < 0 {
		return
	}
	if err := w.setze(v.AIN, -1); err != nil {
		w.sag("Waschküche: Heizventil nicht ausgeschaltet: %v", err)
		return
	}
	w.s.VentilAIN, w.s.VentilVorher, w.s.VentilAus = v.AIN, v.Soll, true
	w.sag("Waschküche: Heizventil aus beim Lüften (vorher %.1f °C)", v.Soll)
}

func (w *wkLueften) ventilZurueck() {
	if !w.s.VentilAus || w.setze == nil {
		return
	}
	soll := w.s.VentilVorher
	if soll >= 99 {
		soll = 28 // "voll auf" gibt es als Sollwert nicht
	}
	if err := w.setze(w.s.VentilAIN, soll); err != nil {
		w.sag("Waschküche: Heizventil nicht zurückgestellt: %v", err)
		return
	}
	w.s.VentilAus = false
	w.sag("Waschküche: Heizventil wieder %.1f °C", soll)
}

func (w *wkLueften) offenHinweis(t time.Time, l wkLage) *wkHinweis {
	h := &wkHinweis{Art: "offen", Text: "Waschküche offen seit " + wkDauer(t.Sub(w.s.OffenSeit))}
	aus, ausOk := luftZu(l.luft, t)
	grund := ""
	if l.innen != nil && ausOk {
		tp := taupunkt(l.innen.Temp, l.innen.RH)
		switch {
		case aus.Temp < wkFrost-1:
			grund = fmt.Sprintf("Frost, draußen %.0f °C", aus.Temp)
		case aus.Taupunkt >= tp-wkAbstandZu:
			grund = "draußen ist die Luft jetzt feuchter"
		case l.innen.RH <= l.cfg.FeuchteUnten:
			grund = fmt.Sprintf("trocken genug, %.0f %%", l.innen.RH)
		case t.Sub(w.s.OffenSeit) >= 24*time.Hour:
			grund = "einen Tag offen, das reicht"
		}
		if grund == "" {
			if ende := wkEnde(l.luft, t, tp); !ende.IsZero() {
				h.Unter = "lüftet gut bis ca. " + wkUhr(ende, t)
			} else {
				h.Unter = "lüftet gut"
			}
		}
	}
	if grund != "" {
		h.Art, h.Text, h.Unter = "schliessen", "Waschküche: Fenster schließen", "offen seit "+wkDauer(t.Sub(w.s.OffenSeit))+" · "+grund
	}
	if w.s.Zweifel {
		h.Unter = "Raumluft reagiert nicht, Kontakt prüfen?"
	}
	return h
}

// empfehlung: lohnt es sich, bald zu lueften?
func (w *wkLueften) empfehlung(t time.Time, l wkLage) *wkHinweis {
	if l.innen == nil || l.cfg.FeuchteUnten <= 0 {
		return nil
	}
	rh := l.innen.RH
	if rh < l.cfg.FeuchteUnten+1 {
		return nil // schon trocken
	}
	// Alle paar Tage, oder wenn sonst der Entfeuchter laufen muesste
	tage := t.Sub(w.s.LetztesLueften).Hours() / 24
	if tage < wkTage && rh < l.cfg.FeuchteOben {
		return nil
	}
	tp := taupunkt(l.innen.Temp, rh)
	von, bis := wkFenster(l.luft, t, tp)
	if von.IsZero() {
		return nil
	}
	h := &wkHinweis{Art: "empfehlung"}
	if !von.After(t) {
		h.Text = "Waschküche lüften: jetzt bis " + wkUhr(bis, t)
	} else {
		h.Text = "Waschküche lüften: ab " + wkUhr(von, t) + " bis " + wkUhr(bis, t)
	}
	if w.s.LetztesLueften.IsZero() || tage >= 30 {
		h.Unter = fmt.Sprintf("Raumluft %.0f %%", rh)
	} else {
		h.Unter = fmt.Sprintf("Raumluft %.0f %% · zuletzt vor %.0f Tagen", rh, math.Floor(tage))
	}
	return h
}

// wkFenster: der naechste Zeitraum binnen 6 Stunden, der bei Tag beginnt und
// mindestens drei Stunden lang trockene Aussenluft ohne Frost hat. Der
// Taupunkt drinnen gilt als gleichbleibend.
func wkFenster(luft map[int64]luftWert, t time.Time, tpIn float64) (time.Time, time.Time) {
	gut := func(x time.Time, abstand float64) bool {
		w, ok := luft[x.Truncate(time.Hour).Unix()]
		return ok && w.Taupunkt <= tpIn-abstand && w.Temp >= wkFrost
	}
	for i := 0; i <= 6; i++ {
		von := t.Truncate(time.Hour).Add(time.Duration(i) * time.Hour)
		if i == 0 {
			von = t
		}
		h := von.In(ort).Hour()
		if h < 7 || h >= 21 || !gut(von, wkAbstandAuf) {
			continue
		}
		bis := von.Truncate(time.Hour)
		for n := 0; n < 24 && gut(bis, wkAbstandZu); n++ {
			bis = bis.Add(time.Hour)
		}
		if bis.Sub(von) >= wkMinStunden*time.Hour {
			return von, bis
		}
	}
	return time.Time{}, time.Time{}
}

// wkEnde: bis wann die Aussenluft noch trockener ist, null ohne Vorhersage.
func wkEnde(luft map[int64]luftWert, t time.Time, tpIn float64) time.Time {
	x := t.Truncate(time.Hour)
	for n := 0; n < 48; n++ {
		w, ok := luft[x.Unix()]
		if !ok {
			return time.Time{}
		}
		if w.Taupunkt > tpIn-wkAbstandZu || w.Temp < wkFrost-1 {
			return x
		}
		x = x.Add(time.Hour)
	}
	return x
}

// wkDauer: "25 min", "3 h", fuer die Pille an der Wand.
func wkDauer(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%d h", int(d.Hours()))
}

// wkUhr: "18 Uhr", am Folgetag "morgen 7 Uhr".
func wkUhr(x, t time.Time) string {
	x, t = x.In(ort), t.In(ort)
	s := fmt.Sprintf("%d Uhr", x.Hour())
	if x.YearDay() != t.YearDay() {
		s = "morgen " + s
	}
	return s
}

func (w *wkLueften) aktuell() *wkHinweis {
	w.Lock()
	defer w.Unlock()
	return w.hinweis
}

func (w *wkLueften) laufe() {
	time.Sleep(90 * time.Second) // erst Uhr, Sensor und Kontakt
	for {
		if t := time.Now().In(ort); t.Year() >= 2026 {
			w.schritt(t)
		}
		time.Sleep(time.Minute)
	}
}
