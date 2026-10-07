package main

// Lueftung des Spielekellers (Vorgaben vom 06. und 07.10.2026). Der Raum hat
// rund 20 m2 und zwei alte Kellerfensterschaechte, der Luftwechsel ist also
// langsam. Im Raum schlaeft Thorstens Sohn, meist von 23 bis 9 Uhr, am
// Wochenende bis 10:30. Die Fenstermotoren sind hoerbar, aber nicht schlimm,
// gute Luft geht vor. Deshalb:
//
//   - tagsueber (Zeiten, werktags ab 9:30, am Wochenende ab 10:30, bis 22:30)
//     hoechstens MaxJeTag Lueftungen mit AbstandStd Pause
//   - nachts hoechstens NachtMaxJe Lueftungen, die dafuer lange offen bleiben
//     duerfen. Eine Lueftung vom Abend wird mit Beginn der Nacht zur
//     Nachtlueftung, wenn das Nachtbudget reicht, sonst geht sie vorher zu.
//     NachtMaxJe 0 heisst: nachts nie fahren, offene Fenster gehen zu.
//   - der Entfeuchter laeuft nachts nie, das regelt er selbst
//
// Gelueftet wird nur, wenn es den Keller trockener macht. Massstab ist der
// Taupunkt, also das Wasser in der Luft. Die relative Feuchte taugt dafuer
// nicht, warme Sommerluft mit 50 % bringt mehr Wasser herein, als die kalte
// Kellerluft mit 65 % hat. Drinnen misst der SwitchBot des Entfeuchters an
// der Wand gegenueber vom Fenster, waehrend gelueftet wird alle 2 Minuten.
// Draussen gilt die Vorhersage der laufenden Stunde von der Wetterseite. Sie
// liegt beim Taupunkt 1,5 bis 2 K daneben, deshalb 3 K Abstand, und der
// Raumfuehler hat das letzte Wort:
//
//   - nach PruefMin muss der Taupunkt drinnen gefallen sein
//   - steigt die relative Feuchte um RHAnstieg Punkte ueber den Startwert,
//     kuehlt der Raum aus und die Waende werden klamm, auch wenn der
//     Taupunkt faellt
//
// In beiden Faellen geht das Fenster zu, und zwei Stunden ist Ruhe. Jede
// Lueftung schreibt ihre Kurve mit, damit sich sehen laesst, wie der Raum
// reagiert.
//
// Von den erlaubten Stunden werden die besten genommen. Eine Stunde zaehlt
// umso mehr, je groesser der Taupunktabstand ist, Kaelte zieht ab, weil sie
// den Raum auskuehlt und die Heizung nachher nachschieben muss.
//
// Waehrend gelueftet wird:
//   - der Entfeuchter pausiert (lueftPause), er steht direkt am Fenster
//   - das Ventil des Spielkellers steht auf 8 Grad und bekommt danach seinen
//     alten Sollwert zurueck, sofern die Box ihn nicht inzwischen selbst nach
//     Wochenplan geaendert hat
//   - bei kalter Aussenluft bestaetigt das Ventil unter dem Fenster, dass es
//     wirklich offen ist: seine Temperatur faellt. Faellt sie nicht, wird der
//     Befehl einmal wiederholt.
//
// Ein Sicherheitsnetz im D1 mini gibt es bewusst nicht (Entscheidung vom
// 07.10.2026): Faellt das Tablet aus, bleiben die Fenster, wie sie sind. Die
// RMF-Zeitschaltuhr steht auf Handbetrieb und faehrt nichts von selbst.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type luftWert struct {
	Temp     float64 `json:"temp"`
	Taupunkt float64 `json:"taupunkt"`
	Regen    float64 `json:"regen"` // mm in der Stunde ab dem Zeitpunkt
	Boeen    float64 `json:"boeen"` // km/h
}

// taupunkt nach Magnus (Sonntag 1990).
func taupunkt(temp, rh float64) float64 {
	const a, b = 17.62, 243.12
	g := math.Log(math.Max(rh, 1)/100) + a*temp/(b+temp)
	return b * g / (a - g)
}

// wasser: absolute Feuchte in g/m3.
func wasser(temp, rh float64) float64 {
	e := 6.112 * math.Exp(17.62*temp/(243.12+temp)) * rh / 100
	return 216.7 * e / (273.15 + temp)
}

type lueftCfg struct {
	Modus        string      `json:"modus"` // aus, probe, scharf
	Zeiten       [7]zeitraum `json:"zeiten"`
	AbstandK     float64     `json:"abstand_k"`           // Taupunkt draussen so weit unter drinnen
	SchlussK     float64     `json:"schluss_k"`           // darunter wieder zu
	MinInnen     float64     `json:"min_innen"`           // Raum nicht kaelter als
	MinAussen    float64     `json:"min_aussen"`          // kein Lueften unter
	MaxMin       float64     `json:"max_min"`             // Hoechstdauer
	MaxMinKalt   float64     `json:"max_min_kalt"`        // Hoechstdauer unter 10 Grad draussen
	MaxJeTag     int         `json:"max_je_tag"`          // Lueftungen am Tag
	AbstandStd   float64     `json:"abstand_std"`         // Pause zwischen zwei Lueftungen
	PruefMin     float64     `json:"pruef_min"`           // dann muss der Taupunkt drinnen gefallen sein
	MaxBoeen     float64     `json:"max_boeen"`           // km/h
	ZielRH       float64     `json:"ziel_rh"`             // darunter nicht lueften, 0 heisst Untergrenze des Entfeuchters
	VentilSoll   float64     `json:"ventil_soll"`         // Grad waehrend des Lueftens
	VentilName   string      `json:"ventil_name"`         // Teil des Namens in der FRITZ!Box
	Kaeltestrafe float64     `json:"kaeltestrafe_k"`      // je Grad unter 12 draussen
	SonneVorrang float64     `json:"sonne_vorrang_unter"` // darunter nicht lueften, solange der Entfeuchter mit Sonne laeuft
	FerienBis    string      `json:"ferien_bis"`          // JJJJ-MM-TT, bis dahin Sonntagszeiten
	NachtMaxJe   int         `json:"nacht_max_je"`        // Lueftungen je Nacht, 0 heisst nachts nie fahren
	NachtMaxMin  float64     `json:"nacht_max_min"`       // Hoechstdauer einer Nachtlueftung
	RHAnstieg    float64     `json:"rh_anstieg"`          // zu, wenn die relative Feuchte so weit steigt, 0 aus
}

func standardLueftung() lueftCfg {
	w, we := zeitraum{9.5, 22.5}, zeitraum{10.5, 22.5}
	return lueftCfg{Modus: "scharf", Zeiten: [7]zeitraum{w, w, w, w, w, we, we},
		AbstandK: 3, SchlussK: 1, MinInnen: 16, MinAussen: 0, MaxMin: 90, MaxMinKalt: 40,
		MaxJeTag: 2, AbstandStd: 3, PruefMin: 20, MaxBoeen: 50, VentilSoll: 8,
		VentilName: "spielkeller", Kaeltestrafe: 0.15, SonneVorrang: 10,
		NachtMaxJe: 1, NachtMaxMin: 240, RHAnstieg: 5}
}

func (c lueftCfg) erlaubt(t time.Time) bool {
	z := c.Zeiten[tagIndex(t)]
	h := float64(t.Hour()) + float64(t.Minute())/60
	return h >= z.Von && h < z.Bis
}

// beginn der erlaubten Zeit an diesem Tag.
func (c lueftCfg) beginn(t time.Time) time.Time {
	z := c.Zeiten[tagIndex(t)]
	return tagesAnfang(t).Add(time.Duration(z.Von * float64(time.Hour)))
}

// nacht: die Nacht, in der t liegt, von ende bis zum naechsten beginn. Nur
// sinnvoll ausserhalb der erlaubten Zeit.
func (c lueftCfg) nacht(t time.Time) (von, bis time.Time) {
	if t.Before(c.beginn(t)) {
		return c.ende(t.AddDate(0, 0, -1)), c.beginn(t)
	}
	return c.ende(t), c.beginn(t.AddDate(0, 0, 1))
}

// ende der erlaubten Zeit an diesem Tag.
func (c lueftCfg) ende(t time.Time) time.Time {
	z := c.Zeiten[tagIndex(t)]
	return tagesAnfang(t).Add(time.Duration(z.Bis * float64(time.Hour)))
}

type lueftLauf struct {
	Von          time.Time     `json:"von"`
	Bis          time.Time     `json:"bis,omitempty"`
	Probe        bool          `json:"probe,omitempty"`
	Grund        string        `json:"grund"`
	Ende         string        `json:"ende,omitempty"`
	TaupunktIn   float64       `json:"taupunkt_innen"`
	TaupunktAus  float64       `json:"taupunkt_aussen"`
	TempIn       float64       `json:"temp_innen"`
	TempAus      float64       `json:"temp_aussen"`
	RHIn         float64       `json:"rh_innen"`
	TaupunktEnde float64       `json:"taupunkt_innen_ende,omitempty"`
	TempEnde     float64       `json:"temp_innen_ende,omitempty"`
	RHEnde       float64       `json:"rh_innen_ende,omitempty"`
	Bestaetigt   bool          `json:"bestaetigt,omitempty"`
	Nacht        bool          `json:"nacht,omitempty"`
	Kurve        []kurvenPunkt `json:"kurve,omitempty"`
}

// kurvenPunkt: Raumluft waehrend einer Lueftung, Minuten seit dem Oeffnen.
type kurvenPunkt struct {
	M  float64 `json:"m"`
	T  float64 `json:"t"`
	RH float64 `json:"rh"`
	TP float64 `json:"tp"`
}

func punktAus(von time.Time, w *messFeuchte) kurvenPunkt {
	return kurvenPunkt{M: math.Round(w.Zeit.Sub(von).Minutes()*10) / 10, T: w.Temp, RH: w.RH,
		TP: math.Round(taupunkt(w.Temp, w.RH)*100) / 100}
}

type lueftStand struct {
	Phase       string      `json:"phase"` // zu, offen, schliesst
	Seit        time.Time   `json:"seit"`
	SperreBis   time.Time   `json:"sperre_bis,omitempty"`
	VentilAIN   string      `json:"ventil_ain,omitempty"`
	VentilAlt   float64     `json:"ventil_alt,omitempty"`
	VentilStart float64     `json:"ventil_start,omitempty"` // Temperatur am Ventil beim Oeffnen
	Wiederholt  bool        `json:"wiederholt,omitempty"`
	Laeufe      []lueftLauf `json:"laeufe"`
}

// lueftEingang: alles, was eine Entscheidung braucht, eingesammelt ohne Sperren.
type lueftEingang struct {
	Jetzt            time.Time
	Innen            *messFeuchte
	ZielRH           float64
	Luft             map[int64]luftWert
	KF               string // Stellung der Kellerfenster
	EntfeuchterSonne bool   // Entfeuchter laeuft gerade ohne Netz und Akku
	KFDa             bool   // D1 mini eingerichtet
	Ventil           *ventil
}

type lueftung struct {
	sync.Mutex
	cfgPfad, standPfad string
	cfg                lueftCfg
	st                 lueftStand
	grund              string
	naechste           time.Time // naechste geplante Lueftung heute, null ohne
	sag                func(string, ...any)

	// Verbindungen nach aussen, in Tests ersetzt
	eingang   func() lueftEingang // ohne Ventil, das sucht ventil nach dem Namen
	ventil    func(name string) *ventil
	fahre     func(richtung string) error
	setzeSoll func(ain string, grad float64) error
	pause     func(bool)
	bald      func(time.Time) // naechste Lueftung an den Entfeuchter
}

// ergaenze: was nur mit den eigenen Einstellungen geht, unter l.Lock.
func (l *lueftung) ergaenze(e *lueftEingang) {
	if l.cfg.ZielRH > 0 {
		e.ZielRH = l.cfg.ZielRH
	}
	if l.ventil != nil {
		e.Ventil = l.ventil(l.cfg.VentilName)
	}
}

func neueLueftung(cfgPfad, standPfad string, sag func(string, ...any)) *lueftung {
	l := &lueftung{cfgPfad: cfgPfad, standPfad: standPfad, cfg: standardLueftung(), sag: sag}
	if roh, err := os.ReadFile(cfgPfad); err == nil {
		json.Unmarshal(roh, &l.cfg)
	} else {
		schreibeJSON(cfgPfad, l.cfg)
	}
	if roh, err := os.ReadFile(standPfad); err == nil {
		json.Unmarshal(roh, &l.st)
	}
	if l.st.Phase == "" {
		l.st.Phase = "zu"
	}
	setzeFerien(l.cfg.FerienBis)
	return l
}

func (l *lueftung) laufzeit() *lueftLauf {
	if n := len(l.st.Laeufe); n > 0 && l.st.Laeufe[n-1].Bis.IsZero() {
		return &l.st.Laeufe[n-1]
	}
	return nil
}

// heute: Lueftungen am Tag, nachts begonnene zaehlen nicht.
func (l *lueftung) heute(t time.Time) int {
	n, anf := 0, tagesAnfang(t)
	for _, x := range l.st.Laeufe {
		if !x.Von.Before(anf) && !x.Nacht && x.Probe == (l.cfg.Modus != "scharf") {
			n++
		}
	}
	return n
}

// inNacht: Nachtlueftungen in der Nacht, in der t liegt.
func (l *lueftung) inNacht(t time.Time) int {
	von, bis := l.cfg.nacht(t)
	n := 0
	for _, x := range l.st.Laeufe {
		if x.Nacht && !x.Von.Before(von) && x.Von.Before(bis) && x.Probe == (l.cfg.Modus != "scharf") {
			n++
		}
	}
	return n
}

func (l *lueftung) letztesEnde() time.Time {
	for i := len(l.st.Laeufe) - 1; i >= 0; i-- {
		if x := l.st.Laeufe[i]; x.Probe == (l.cfg.Modus != "scharf") {
			if x.Bis.IsZero() {
				return x.Von
			}
			return x.Bis
		}
	}
	return time.Time{}
}

// eignung einer Stunde: Punktzahl in Kelvin und, wenn sie nicht taugt, warum.
func (l *lueftung) eignung(tpIn float64, w luftWert, abstand float64) (float64, string) {
	c := l.cfg
	d := tpIn - w.Taupunkt
	switch {
	case w.Temp < c.MinAussen:
		return 0, fmt.Sprintf("draußen zu kalt (%.0f °C)", w.Temp)
	case w.Regen >= 0.2:
		return 0, "Regen angesagt"
	case w.Boeen >= c.MaxBoeen:
		return 0, fmt.Sprintf("Böen bis %.0f km/h", w.Boeen)
	case d < abstand:
		return 0, fmt.Sprintf("Außenluft zu feucht (Taupunkt %.1f gegen %.1f °C drinnen)", w.Taupunkt, tpIn)
	}
	return d - c.Kaeltestrafe*math.Max(0, 12-w.Temp), ""
}

func luftZu(luft map[int64]luftWert, t time.Time) (luftWert, bool) {
	w, ok := luft[t.Truncate(time.Hour).Unix()]
	return w, ok
}

// entscheide: "auf", "zu" oder nichts, dazu der Grund. Rechnet nur, setzt
// aber naechste und macht aus einer Abendlueftung eine Nachtlueftung.
func (l *lueftung) entscheide(e lueftEingang) (aktion, grund string, sperre time.Duration) {
	c, t := l.cfg, e.Jetzt
	l.naechste = time.Time{}
	if c.Modus == "aus" {
		if l.st.Phase != "zu" {
			return "zu", "Automatik ausgeschaltet", 0
		}
		return "", "Automatik aus", 0
	}
	aus, ausDa := luftZu(e.Luft, t)
	nacht := !c.erlaubt(t)

	if l.st.Phase != "zu" {
		lz := l.laufzeit()
		dauer := t.Sub(l.st.Seit)
		if lz != nil && nacht && !lz.Nacht {
			if l.inNacht(t) >= c.NachtMaxJe {
				return "zu", "Nachtruhe", 0
			}
			lz.Nacht = true // laeuft als Nachtlueftung weiter
		}
		max := c.MaxMin
		switch {
		case lz != nil && lz.Nacht:
			max = c.NachtMaxMin
		case ausDa && aus.Temp < 10:
			max = c.MaxMinKalt
		}
		switch {
		case l.st.Phase == "schliesst":
			return "zu", "Schließen wiederholen", 0
		case nacht && lz == nil:
			return "zu", "Nachtruhe", 0
		case dauer >= time.Duration(max*float64(time.Minute)):
			return "zu", fmt.Sprintf("nach %.0f Minuten", max), 0
		case e.Innen == nil && dauer >= 15*time.Minute:
			return "zu", "kein Messwert drinnen", 0
		case e.Innen != nil && e.Innen.Temp < c.MinInnen:
			return "zu", fmt.Sprintf("Raum auf %.1f °C abgekühlt", e.Innen.Temp), 0
		case !ausDa:
			return "zu", "keine Vorhersage mehr", 0
		}
		if e.Innen != nil {
			tp := taupunkt(e.Innen.Temp, e.Innen.RH)
			if _, warum := l.eignung(tp, aus, c.SchlussK); warum != "" {
				return "zu", warum, 0
			}
			frisch := e.Innen.Zeit.After(l.st.Seit.Add(3 * time.Minute))
			if lz != nil && frisch && c.RHAnstieg > 0 && e.Innen.RH >= lz.RHIn+c.RHAnstieg {
				return "zu", fmt.Sprintf("relative Feuchte steigt (%.0f auf %.0f %%)", lz.RHIn, e.Innen.RH), 2 * time.Hour
			}
			if lz != nil && frisch && dauer >= time.Duration(c.PruefMin*float64(time.Minute)) && tp >= lz.TaupunktIn {
				return "zu", fmt.Sprintf("Taupunkt drinnen nicht gefallen (%.1f °C)", tp), 2 * time.Hour
			}
			if e.Innen.RH <= e.ZielRH-2 {
				return "zu", fmt.Sprintf("Raumluft trocken genug (%.0f %%)", e.Innen.RH), 0
			}
		}
		art := "lüftet"
		if lz != nil && lz.Nacht {
			art = "Nachtlüftung"
		}
		return "", fmt.Sprintf("%s seit %.0f Minuten", art, dauer.Minutes()), 0
	}

	// Fenster sind zu, oder sollten es sein.
	if nacht && c.NachtMaxJe == 0 {
		if e.KF == "offen" || e.KF == "fährt auf" || e.KF == "angehalten" {
			return "zu", "Nachtruhe, Kellerfenster standen offen", 0
		}
		return "", "Nachtruhe", 0
	}
	switch {
	case !e.KFDa:
		return "", "Kellerfenster nicht eingerichtet", 0
	case e.KF == "offen" || e.KF == "fährt auf":
		return "", "Fenster von Hand geöffnet", 0
	case e.Innen == nil:
		return "", "kein frischer Messwert drinnen", 0
	case !ausDa:
		return "", "keine Vorhersage", 0
	case !nacht && c.NachtMaxJe == 0 && c.ende(t).Sub(t) < time.Duration(c.MaxMin*float64(time.Minute)):
		return "", "zu spät für heute", 0
	case !nacht && l.heute(t) >= c.MaxJeTag:
		return "", fmt.Sprintf("heute schon %d-mal gelüftet", c.MaxJeTag), 0
	case nacht && l.inNacht(t) >= c.NachtMaxJe:
		return "", "diese Nacht schon gelüftet", 0
	case e.Innen.RH <= e.ZielRH:
		return "", fmt.Sprintf("Raumluft trocken genug (%.0f %%)", e.Innen.RH), 0
	case e.Innen.Temp < c.MinInnen+1:
		return "", fmt.Sprintf("Raum zu kühl (%.1f °C)", e.Innen.Temp), 0
	}
	tp := taupunkt(e.Innen.Temp, e.Innen.RH)
	frei := l.st.SperreBis
	if p := l.letztesEnde().Add(time.Duration(c.AbstandStd * float64(time.Hour))); p.After(frei) {
		frei = p
	}

	// Die besten restlichen Stunden dieses Tages oder dieser Nacht, so viele,
	// wie Lueftungen uebrig sind. Die jetzige muss mithalten koennen, sonst
	// wird gewartet. Die frueheste davon erfaehrt der Entfeuchter, damit er
	// nicht vorher mit Netzstrom trocknet, was die Lueftung umsonst erledigt.
	var bis time.Time
	rest := c.MaxJeTag - l.heute(t)
	if nacht {
		_, bis = c.nacht(t)
		rest = c.NachtMaxJe - l.inNacht(t)
	} else {
		bis = c.ende(t)
		if c.NachtMaxJe == 0 {
			bis = bis.Add(-time.Duration(c.MaxMin * float64(time.Minute)))
		}
	}
	k := l.kandidaten(tp, e.Luft, t.Truncate(time.Hour).Add(time.Hour), bis, frei)
	jetzt, warum := l.eignung(tp, aus, c.AbstandK)
	jetztGut := warum == "" && !t.Before(frei) && (len(k) < rest || jetzt >= k[rest-1].p-0.3)
	if jetztGut {
		l.naechste = t
	} else {
		for i := 0; i < rest && i < len(k); i++ {
			if l.naechste.IsZero() || k[i].t.Before(l.naechste) {
				l.naechste = k[i].t
			}
		}
	}
	switch {
	case t.Before(l.st.SperreBis):
		return "", "gesperrt bis " + l.st.SperreBis.In(ort).Format("15:04"), 0
	case t.Before(frei):
		return "", "Pause nach der letzten Lüftung", 0
	case warum != "":
		return "", warum, 0
	case e.EntfeuchterSonne && aus.Temp < c.SonneVorrang:
		return "", fmt.Sprintf("Entfeuchter läuft mit Sonne, Lüften kostet bei %.0f °C Heizwärme", aus.Temp), 0
	case !jetztGut:
		return "", fmt.Sprintf("um %s besser (%.1f gegen %.1f K)", k[0].t.In(ort).Format("15 Uhr"), k[0].p, jetzt), 0
	}
	vor := "Taupunkt"
	if nacht {
		vor = "Nachtlüftung, Taupunkt"
	}
	return "auf", fmt.Sprintf("%s draußen %.1f °C, drinnen %.1f °C", vor, aus.Taupunkt, tp), 0
}

type kand struct {
	t time.Time
	p float64
}

// kandidaten: geeignete volle Stunden von ab bis vor bis, nicht vor frei,
// die besten zuerst.
func (l *lueftung) kandidaten(tp float64, luft map[int64]luftWert, ab, bis, frei time.Time) []kand {
	var k []kand
	for h := ab; h.Before(bis); h = h.Add(time.Hour) {
		if h.Before(frei) {
			continue
		}
		if w, ok := luftZu(luft, h); ok {
			if p, warum := l.eignung(tp, w, l.cfg.AbstandK); warum == "" {
				k = append(k, kand{h, p})
			}
		}
	}
	sort.SliceStable(k, func(i, j int) bool { return k[i].p > k[j].p })
	return k
}

// schritt: einmal entscheiden und handeln.
func (l *lueftung) schritt() {
	e := l.eingang()
	l.Lock()
	defer l.Unlock()
	l.ergaenze(&e)
	aktion, grund, sperre := l.entscheide(e)
	l.grund = grund
	scharf := l.cfg.Modus == "scharf"
	if l.bald != nil {
		if scharf {
			l.bald(l.naechste)
		} else {
			l.bald(time.Time{}) // Probe haelt den Entfeuchter nicht auf
		}
	}
	t := e.Jetzt

	switch aktion {
	case "auf":
		lz := lueftLauf{Von: t, Probe: !scharf, Grund: grund, TempIn: e.Innen.Temp, RHIn: e.Innen.RH,
			TaupunktIn: taupunkt(e.Innen.Temp, e.Innen.RH), Nacht: !l.cfg.erlaubt(t)}
		start := punktAus(t, e.Innen)
		start.M = 0
		lz.Kurve = []kurvenPunkt{start}
		if w, ok := luftZu(e.Luft, t); ok {
			lz.TaupunktAus, lz.TempAus = w.Taupunkt, w.Temp
		}
		if scharf {
			l.st.VentilAIN, l.st.VentilAlt, l.st.VentilStart = "", 0, 0
			if v := e.Ventil; v != nil {
				l.st.VentilStart = v.Ist
				if v.Soll > l.cfg.VentilSoll && v.Soll < 99 {
					if err := l.setzeSoll(v.AIN, l.cfg.VentilSoll); err != nil {
						l.sag("Lüftung: Ventil nicht verstellt: %v", err)
					} else {
						l.st.VentilAIN, l.st.VentilAlt = v.AIN, v.Soll
					}
				}
			}
			l.pause(true)
			if err := l.fahre("auf"); err != nil {
				l.sag("Lüftung: Öffnen gescheitert: %v", err)
				l.zurueck()
				l.st.SperreBis = t.Add(15 * time.Minute)
				l.grund = "Öffnen gescheitert, neuer Versuch " + l.st.SperreBis.In(ort).Format("15:04")
				l.sichern()
				return
			}
		}
		l.st.Phase, l.st.Seit, l.st.Wiederholt = "offen", t, false
		l.st.Laeufe = append(l.st.Laeufe, lz)
		if len(l.st.Laeufe) > 100 {
			l.st.Laeufe = l.st.Laeufe[len(l.st.Laeufe)-100:]
		}
		l.sag("Lüftung: auf%s, %s", map[bool]string{true: "", false: " (Probe)"}[scharf], grund)
		l.sichern()

	case "zu":
		lz := l.laufzeit()
		// Ein Probelauf wird nur gebucht. Ohne eigenen Lauf geht es um fremd
		// geoeffnete Fenster, die faehrt nur der scharfe Modus zu.
		if lz == nil && !scharf {
			return
		}
		if lz == nil || !lz.Probe {
			if err := l.fahre("ab"); err != nil {
				if l.st.Phase != "schliesst" {
					l.sag("Lüftung: Schließen gescheitert, versuche es weiter: %v", err)
				}
				if lz != nil {
					l.st.Phase = "schliesst"
				}
				l.grund = "Schließen gescheitert, neuer Versuch in einer Minute"
				l.sichern()
				return
			}
			l.zurueck()
		}
		if lz != nil {
			lz.Bis, lz.Ende = t, grund
			if e.Innen != nil {
				lz.TempEnde, lz.RHEnde = e.Innen.Temp, e.Innen.RH
				lz.TaupunktEnde = taupunkt(e.Innen.Temp, e.Innen.RH)
				l.merkePunkt(lz, e.Innen)
			}
		}
		if sperre > 0 {
			l.st.SperreBis = t.Add(sperre)
		}
		l.st.Phase, l.st.Seit = "zu", t
		l.sag("Lüftung: zu, %s", grund)
		l.sichern()

	default:
		lz := l.laufzeit()
		if lz != nil && e.Innen != nil && l.merkePunkt(lz, e.Innen) {
			l.sichern()
		}
		// Offen: bei kalter Aussenluft am Ventil unter dem Fenster pruefen,
		// ob es wirklich offen ist, und den Befehl einmal wiederholen.
		if l.st.Phase == "offen" && scharf && lz != nil && !lz.Probe && !lz.Bestaetigt && e.Ventil != nil &&
			lz.TempAus < l.st.VentilStart-3 && t.Sub(l.st.Seit) >= 10*time.Minute {
			if e.Ventil.Fenster || e.Ventil.Ist <= l.st.VentilStart-0.5 {
				lz.Bestaetigt = true
				l.sichern()
			} else if !l.st.Wiederholt {
				l.st.Wiederholt = true
				l.sag("Lüftung: Ventil merkt nichts vom offenen Fenster, Befehl wiederholt")
				l.fahre("auf")
				l.sichern()
			}
		}
	}
}

// merkePunkt haengt einen neuen Messwert an die Kurve, true wenn neu.
func (l *lueftung) merkePunkt(lz *lueftLauf, w *messFeuchte) bool {
	if !w.Zeit.After(lz.Von) {
		return false
	}
	p := punktAus(lz.Von, w)
	if n := len(lz.Kurve); n > 0 && p.M <= lz.Kurve[n-1].M+0.5 {
		return false
	}
	lz.Kurve = append(lz.Kurve, p)
	return true
}

// zurueck: Ventil und Entfeuchter wie vor der Lueftung.
func (l *lueftung) zurueck() {
	l.pause(false)
	if l.st.VentilAIN == "" {
		return
	}
	e := l.eingang()
	l.ergaenze(&e)
	// Hat die Box den Sollwert inzwischen nach Wochenplan selbst geaendert,
	// gilt ihrer.
	if e.Ventil != nil && e.Ventil.AIN == l.st.VentilAIN && math.Abs(e.Ventil.Soll-l.cfg.VentilSoll) < 0.3 {
		if err := l.setzeSoll(l.st.VentilAIN, l.st.VentilAlt); err != nil {
			l.sag("Lüftung: Ventil nicht zurückgestellt: %v", err)
			return
		}
	}
	l.st.VentilAIN = ""
}

func (l *lueftung) sichern() { schreibeJSON(l.standPfad, l.st) }

func (l *lueftung) laufe() {
	time.Sleep(2 * time.Minute) // erst Sensor, Box und Prognose
	// Nach einem Neustart mitten in einer Lueftung: Pause wieder setzen.
	l.Lock()
	if l.st.Phase != "zu" {
		l.pause(true)
	}
	l.Unlock()
	for {
		l.schritt()
		time.Sleep(time.Minute)
	}
}

func (l *lueftung) stand(e lueftEingang) map[string]any {
	l.Lock()
	defer l.Unlock()
	l.ergaenze(&e)
	a := map[string]any{"cfg": l.cfg, "modus": l.cfg.Modus, "phase": l.st.Phase, "seit": l.st.Seit, "grund": l.grund,
		"heute": l.heute(e.Jetzt), "max_je_tag": l.cfg.MaxJeTag, "erlaubt_jetzt": l.cfg.erlaubt(e.Jetzt),
		"feiertag": feiertag(e.Jetzt), "ferien": ferien(e.Jetzt)}
	if !l.naechste.IsZero() {
		a["naechste"] = l.naechste
	}
	if e.Innen != nil {
		a["innen"] = map[string]float64{"temp": e.Innen.Temp, "rh": e.Innen.RH,
			"taupunkt": math.Round(taupunkt(e.Innen.Temp, e.Innen.RH)*10) / 10,
			"wasser":   math.Round(wasser(e.Innen.Temp, e.Innen.RH)*10) / 10}
	}
	if w, ok := luftZu(e.Luft, e.Jetzt); ok {
		a["aussen"] = w
	}
	von := len(l.st.Laeufe) - 10
	if von < 0 {
		von = 0
	}
	a["laeufe"] = l.st.Laeufe[von:]
	return a
}

func pruefeLueftCfg(c lueftCfg) string {
	if !strings.Contains(" aus probe scharf ", " "+c.Modus+" ") || c.Modus == "" {
		return "Modus aus, Probe oder scharf"
	}
	for i, z := range c.Zeiten {
		if z.Von < 0 || z.Bis > 24 || z.Von > z.Bis {
			return fmt.Sprintf("Lüftungszeit am %s unstimmig", []string{"Mo", "Di", "Mi", "Do", "Fr", "Sa", "So"}[i])
		}
	}
	switch {
	case c.AbstandK < 0.5 || c.AbstandK > 10:
		return "Taupunktabstand 0,5 bis 10 K"
	case c.SchlussK < 0 || c.SchlussK >= c.AbstandK:
		return "Schließen ab einem Abstand unter dem zum Öffnen"
	case c.MinInnen < 5 || c.MinInnen > 25:
		return "Raum mindestens 5 bis 25 °C"
	case c.MinAussen < -20 || c.MinAussen > 25:
		return "Außen mindestens -20 bis 25 °C"
	case c.MaxMin < 5 || c.MaxMin > 240 || c.MaxMinKalt < 5 || c.MaxMinKalt > c.MaxMin:
		return "Dauer 5 bis 240 Minuten, bei Kälte nicht länger als sonst"
	case c.NachtMaxJe < 0 || c.NachtMaxJe > 3:
		return "0 bis 3 Lüftungen je Nacht"
	case c.NachtMaxMin < 15 || c.NachtMaxMin > 720:
		return "Nachtlüftung 15 bis 720 Minuten"
	case c.RHAnstieg < 0 || c.RHAnstieg > 30:
		return "Feuchteanstieg 0 bis 30 Punkte"
	case c.MaxJeTag < 0 || c.MaxJeTag > 6:
		return "0 bis 6 Lüftungen am Tag"
	case c.AbstandStd < 0 || c.AbstandStd > 12:
		return "Pause zwischen Lüftungen 0 bis 12 Stunden"
	case c.PruefMin < 5 || c.PruefMin > c.MaxMin:
		return "Prüfung nach 5 Minuten bis zur Höchstdauer"
	case c.MaxBoeen < 10 || c.MaxBoeen > 150:
		return "Böen 10 bis 150 km/h"
	case c.ZielRH < 0 || c.ZielRH > 90:
		return "Zielfeuchte 0 bis 90 %"
	case c.VentilSoll < 8 || c.VentilSoll > 28:
		return "Ventil 8 bis 28 °C"
	case c.Kaeltestrafe < 0 || c.Kaeltestrafe > 1:
		return "Kältestrafe 0 bis 1 K je Grad"
	case c.SonneVorrang < -30 || c.SonneVorrang > 30:
		return "Vorrang des Entfeuchters -30 bis 30 °C"
	}
	if c.FerienBis != "" {
		if _, err := time.Parse("2006-01-02", c.FerienBis); err != nil {
			return "Ferienende als Datum, etwa 2026-10-24"
		}
	}
	return ""
}

func (l *lueftung) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/api/lueftung/plan", func(w http.ResponseWriter, r *http.Request) {
		e := l.eingang()
		l.Lock()
		l.ergaenze(&e)
		p := l.planeWoche(e)
		l.Unlock()
		jsonAntwort(w, p)
	})
	mux.HandleFunc("/api/lueftung", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			jsonAntwort(w, l.stand(l.eingang()))
			return
		}
		var c lueftCfg
		if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&c) != nil {
			http.Error(w, "unbrauchbar", 400)
			return
		}
		if fehler := pruefeLueftCfg(c); fehler != "" {
			http.Error(w, fehler, 400)
			return
		}
		l.Lock()
		l.cfg = c
		schreibeJSON(l.cfgPfad, l.cfg)
		setzeFerien(c.FerienBis)
		l.Unlock()
		l.sag("Lüftung: Einstellungen geändert, Modus %s", c.Modus)
		w.WriteHeader(204)
	})
}
