package main

// Lueftung des Spielekellers. Der Raum hat rund 20 m2 und zwei alte
// Kellerfensterschaechte, der Luftwechsel ist langsam. Thorstens Sohn
// schlaeft dort.
//
// Vorgabe vom 07.10.2026: Wann und wie lange gelueftet wird, bestimmen allein
// die Messwerte und das Raumklima. Die Fenstermotoren stoeren nachts kaum,
// es gibt also keine Uhrzeiten, keine Tagesbudgets und keine festen Dauern.
// Gegen Hin und Her schuetzen die Hysterese (auf ab AbstandK, zu unter
// SchlussK) und eine kurze Pause zwischen zwei Lueftungen. Der Entfeuchter
// laeuft nachts trotzdem nie, das regelt er selbst. Die Heizung des Raums
// regelt heizregel.go, sie geht beim Lueften herunter.
//
// Zwei Gruende zu lueften:
//
//   - Feuchte. Massstab ist der Taupunkt, also das Wasser in der Luft. Die
//     relative Feuchte taugt dafuer nicht, warme Sommerluft mit 50 % bringt
//     mehr Wasser herein, als die kalte Kellerluft mit 65 % hat. Drinnen misst
//     der SwitchBot des Entfeuchters, beim Lueften alle 2 Minuten. Draussen
//     gilt die Vorhersage der laufenden Stunde, sie liegt beim Taupunkt 1,5
//     bis 2 K daneben, deshalb 3 K Abstand zum Oeffnen.
//   - Freier Sonnenstrom schlaegt knappes Lueften (07.10.2026): Kann der
//     Entfeuchter gerade kostenlos laufen und liegt der Taupunktabstand unter
//     SonneAbstand, holt er mehr Wasser heraus als das Fenster. Dann bleibt
//     es zu, oder es geht zu, und der Entfeuchter uebernimmt.
//   - CO2, solange der Velux-Sensor im Keller liegt. Dafuer geht das Fenster
//     nur einen Spalt auf (SpaltSek), fuer Feuchte ganz. Ab CO2Auf wird gelueftet,
//     wenn die Aussenluft nicht feuchter ist als drinnen, ab CO2Max auch dann.
//
// Der Raumfuehler hat das letzte Wort. Das Fenster geht zu, wenn
//   - der Taupunktabstand unter SchlussK faellt und kein CO2-Grund besteht
//   - der Taupunkt drinnen nach PruefMin nicht gefallen ist (dann 2 h Ruhe)
//   - die relative Feuchte um RHAnstieg Punkte steigt: der Raum kuehlt aus,
//     die Waende werden klamm (dann 2 h Ruhe)
//   - der Raum unter MinInnen abkuehlt, es stuermt oder friert
//
// Jede Lueftung schreibt ihre Kurve mit. Bei kalter Aussenluft bestaetigt das
// Ventil unter dem Fenster, dass es wirklich offen ist: seine Temperatur
// faellt. Faellt sie nicht, wird der Befehl einmal wiederholt.
//
// Ein Sicherheitsnetz im D1 mini gibt es bewusst nicht. Faellt das Tablet
// aus, bleiben die Fenster, wie sie sind. MaxStd beendet nur eine Lueftung,
// die aus irgendeinem Grund nie zu Ende kaeme.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
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
	Modus        string  `json:"modus"`               // aus, probe, scharf
	AbstandK     float64 `json:"abstand_k"`           // auf, wenn der Taupunkt draussen so weit unter drinnen liegt
	SchlussK     float64 `json:"schluss_k"`           // zu darunter
	ZielRH       float64 `json:"ziel_rh"`             // darunter nicht wegen Feuchte lueften, 0 heisst Untergrenze des Entfeuchters
	MinInnen     float64 `json:"min_innen"`           // Raum nicht kaelter als
	MinAussen    float64 `json:"min_aussen"`          // kein Lueften bei Frost darunter
	MaxBoeen     float64 `json:"max_boeen"`           // km/h
	PruefMin     float64 `json:"pruef_min"`           // dann muss der Taupunkt drinnen gefallen sein
	RHAnstieg    float64 `json:"rh_anstieg"`          // zu, wenn die relative Feuchte so weit steigt, 0 aus
	PauseMin     float64 `json:"pause_min"`           // zwischen zwei Lueftungen
	MaxStd       float64 `json:"max_std"`             // Notbremse fuer eine Lueftung
	MaxJeTag     int     `json:"max_je_tag"`          // 0 heisst unbegrenzt
	SonneVorrang float64 `json:"sonne_vorrang_unter"` // darunter nicht wegen Feuchte lueften, solange der Entfeuchter mit Sonne laeuft
	SonneAbstand float64 `json:"sonne_abstand_k"`     // unter diesem Abstand trocknet freier Sonnenstrom besser als Lueften, 0 aus
	CO2Auf       float64 `json:"co2_auf"`             // ppm
	CO2Zu        float64 `json:"co2_zu"`              // ppm
	CO2Max       float64 `json:"co2_max"`             // ab hier auch bei feuchterer Aussenluft
	VentilName   string  `json:"ventil_name"`         // zur Bestaetigung, Teil des Namens in der FRITZ!Box
	Kaeltestrafe float64 `json:"kaeltestrafe_k"`      // nur fuer die Farbe im Plan: je Grad unter 12 draussen
	SpaltSek     float64 `json:"spalt_sek"`           // CO2-Lueftung: so lange auffahren, dann stop; 0 heisst ganz auf
	NachTrocken  float64 `json:"nach_trocken"`        // nach "trocken genug" erst so viele Punkte ueber dem Ziel wieder
}

func standardLueftung() lueftCfg {
	return lueftCfg{Modus: "scharf", AbstandK: 3, SchlussK: 1, MinInnen: 17, MinAussen: 0, MaxBoeen: 50,
		PruefMin: 20, RHAnstieg: 5, PauseMin: 30, MaxStd: 12, SonneVorrang: 10, SonneAbstand: 3,
		CO2Auf: 1000, CO2Zu: 700, CO2Max: 1400, VentilName: "spielkeller", Kaeltestrafe: 0.15,
		SpaltSek: 7, NachTrocken: 3}
}

type lueftLauf struct {
	Von          time.Time     `json:"von"`
	Bis          time.Time     `json:"bis,omitempty"`
	Probe        bool          `json:"probe,omitempty"`
	Anlass       string        `json:"anlass,omitempty"` // feuchte, co2
	Grund        string        `json:"grund"`
	Ende         string        `json:"ende,omitempty"`
	TaupunktIn   float64       `json:"taupunkt_innen"`
	TaupunktAus  float64       `json:"taupunkt_aussen"`
	TempIn       float64       `json:"temp_innen"`
	TempAus      float64       `json:"temp_aussen"`
	RHIn         float64       `json:"rh_innen"`
	CO2In        float64       `json:"co2_innen,omitempty"`
	TaupunktEnde float64       `json:"taupunkt_innen_ende,omitempty"`
	TempEnde     float64       `json:"temp_innen_ende,omitempty"`
	RHEnde       float64       `json:"rh_innen_ende,omitempty"`
	CO2Ende      float64       `json:"co2_innen_ende,omitempty"`
	Bestaetigt   bool          `json:"bestaetigt,omitempty"`
	Spalt        bool          `json:"spalt,omitempty"`        // nur einen Spalt geoeffnet
	TrockenEnde  bool          `json:"trocken_ende,omitempty"` // endete, weil die Luft trocken genug war
	Nacht        bool          `json:"nacht,omitempty"`        // aus der Zeit mit Nachtregeln, nur noch alte Laeufe
	Kurve        []kurvenPunkt `json:"kurve,omitempty"`
}

// kurvenPunkt: Raumluft waehrend einer Lueftung, Minuten seit dem Oeffnen.
type kurvenPunkt struct {
	M   float64 `json:"m"`
	T   float64 `json:"t"`
	RH  float64 `json:"rh"`
	TP  float64 `json:"tp"`
	CO2 float64 `json:"co2,omitempty"`
}

func punktAus(von time.Time, w *messFeuchte, co2 float64) kurvenPunkt {
	return kurvenPunkt{M: math.Round(w.Zeit.Sub(von).Minutes()*10) / 10, T: w.Temp, RH: w.RH,
		TP: math.Round(taupunkt(w.Temp, w.RH)*100) / 100, CO2: co2}
}

type lueftStand struct {
	Phase       string      `json:"phase"` // zu, offen, schliesst
	Seit        time.Time   `json:"seit"`
	SperreBis   time.Time   `json:"sperre_bis,omitempty"`
	VentilStart float64     `json:"ventil_start,omitempty"` // Temperatur am Ventil beim Oeffnen
	Laeufe      []lueftLauf `json:"laeufe"`
}

// lueftEingang: alles, was eine Entscheidung braucht, eingesammelt ohne Sperren.
type lueftEingang struct {
	Jetzt            time.Time
	Innen            *messFeuchte
	ZielRH           float64
	CO2              float64 // ppm im Keller, 0 heisst unbekannt
	Luft             map[int64]luftWert
	KF               string  // Stellung der Kellerfenster
	EntfeuchterSonne bool    // Entfeuchter laeuft gerade ohne Netz und Akku
	SonneFrei        bool    // Entfeuchter darf jetzt laufen und haette freien Sonnenstrom
	KFDa             bool    // D1 mini eingerichtet
	ObenRH           float64 // Obergrenze des Entfeuchters, darueber zaehlen keine Kosten
	Einspeisung      bool    // das Haus speist gerade ein
	EntfKW           float64 // Leistung des Entfeuchters
	EntfStrom        float64 // ct/kWh, zu denen der Entfeuchter statt der Lueftung liefe
	Ventil           *ventil
}

type lueftung struct {
	sync.Mutex
	cfgPfad, standPfad string
	cfg                lueftCfg
	st                 lueftStand
	grund              string
	naechste           time.Time // naechste zu erwartende Lueftung, null ohne
	kosten             *feuchteKosten
	sag                func(string, ...any)

	// Verbindungen nach aussen, in Tests ersetzt
	eingang func() lueftEingang // ohne Ventil, das sucht ventil nach dem Namen
	ventil  func(name string) *ventil
	fahre   func(richtung string) error
	pause   func(bool)
	warte   func(time.Duration)  // fuer die Spaltoeffnung, in Tests ohne Warten
	bald    func(time.Time)      // naechste Lueftung an den Entfeuchter
	wach    func(time.Time) bool // Wachzeit im Raum, nur fuer die Anzeige
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
	return l
}

func (l *lueftung) laufzeit() *lueftLauf {
	if n := len(l.st.Laeufe); n > 0 && l.st.Laeufe[n-1].Bis.IsZero() {
		return &l.st.Laeufe[n-1]
	}
	return nil
}

func (l *lueftung) heute(t time.Time) int {
	n, anf := 0, tagesAnfang(t)
	for _, x := range l.st.Laeufe {
		if !x.Von.Before(anf) && x.Probe == (l.cfg.Modus != "scharf") {
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

// wetter: was ein Lueften verbietet, egal aus welchem Grund. Regen gehoert
// seit 08.10.2026 nicht mehr dazu: die Schaechte sind schraeg mit einer
// Platte abgedeckt, es regnet nicht herein. Ob feuchte Regenluft schadet,
// entscheidet der Taupunkt.
func (l *lueftung) wetter(w luftWert) string {
	c := l.cfg
	switch {
	case w.Temp < c.MinAussen:
		return fmt.Sprintf("draußen zu kalt (%.0f °C)", w.Temp)
	case w.Boeen >= c.MaxBoeen:
		return fmt.Sprintf("Böen bis %.0f km/h", w.Boeen)
	}
	return ""
}

// eignung einer Stunde zum Lueften wegen Feuchte: Punktzahl in Kelvin fuer
// die Farbe im Plan und, wenn sie nicht taugt, warum.
func (l *lueftung) eignung(tpIn float64, w luftWert, abstand float64) (float64, string) {
	if warum := l.wetter(w); warum != "" {
		return 0, warum
	}
	d := tpIn - w.Taupunkt
	if d < abstand {
		return 0, fmt.Sprintf("Außenluft zu feucht (Taupunkt %.1f gegen %.1f °C drinnen)", w.Taupunkt, tpIn)
	}
	return d - l.cfg.Kaeltestrafe*math.Max(0, 12-w.Temp), ""
}

func luftZu(luft map[int64]luftWert, t time.Time) (luftWert, bool) {
	w, ok := luft[t.Truncate(time.Hour).Unix()]
	return w, ok
}

// vorschau: die erste volle Stunde der naechsten acht, in der die Vorhersage
// eine Lueftung wegen Feuchte erwarten laesst.
func (l *lueftung) vorschau(tp float64, luft map[int64]luftWert, t time.Time) time.Time {
	for h := t.Truncate(time.Hour).Add(time.Hour); h.Before(t.Add(8 * time.Hour)); h = h.Add(time.Hour) {
		if w, ok := luftZu(luft, h); ok {
			if _, warum := l.eignung(tp, w, l.cfg.AbstandK); warum == "" {
				return h
			}
		}
	}
	return time.Time{}
}

// co2Grund: verlangt die Luft drinnen nach Lueften, unabhaengig von der Feuchte?
func (l *lueftung) co2Grund(e lueftEingang, tp float64, aus luftWert, schwelle float64) bool {
	c := l.cfg
	return e.CO2 > 0 && e.CO2 >= schwelle && (aus.Taupunkt <= tp+0.5 || e.CO2 >= c.CO2Max)
}

// entscheide: "auf" (Feuchte), "auf-co2", "zu" oder nichts, dazu der Grund.
// Rechnet nur, setzt aber naechste.
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
	tp := 0.0
	if e.Innen != nil {
		tp = taupunkt(e.Innen.Temp, e.Innen.RH)
	}
	// Heizkosten gegen Entfeuchter, nur unter seiner Obergrenze. Darueber
	// geht Trocknen vor, Schimmel darf nicht entstehen. 10 % Abstand auf
	// beiden Seiten, damit es nicht hin und her geht.
	l.kosten = nil
	teuer := func(faktor float64) bool { return false }
	if e.Innen != nil && ausDa && e.EntfKW > 0 {
		k := kostenJeLiter(*e.Innen, tp, aus, heizzeit(e.Luft, t), e.Einspeisung, e.EntfKW, e.EntfStrom)
		l.kosten = &k
		teuer = func(faktor float64) bool {
			return k.Heizzeit && (e.ObenRH <= 0 || e.Innen.RH < e.ObenRH) && k.Lueften > k.Entfeucht*faktor+0.5
		}
	}
	kostenText := func() string {
		return fmt.Sprintf("Lüften kostet %.0f ct/l Heizwärme, Entfeuchter %.0f ct/l", l.kosten.Lueften, l.kosten.Entfeucht)
	}

	if l.st.Phase != "zu" {
		lz := l.laufzeit()
		dauer := t.Sub(l.st.Seit)
		switch {
		case l.st.Phase == "schliesst":
			return "zu", "Schließen wiederholen", 0
		case dauer >= time.Duration(c.MaxStd*float64(time.Hour)):
			return "zu", fmt.Sprintf("nach %.0f Stunden, Notbremse", c.MaxStd), 0
		case e.Innen == nil && dauer >= 15*time.Minute:
			return "zu", "kein Messwert drinnen", 0
		case e.Innen != nil && e.Innen.Temp < c.MinInnen:
			return "zu", fmt.Sprintf("Raum auf %.1f °C abgekühlt", e.Innen.Temp), 0
		case !ausDa:
			return "zu", "keine Vorhersage mehr", 0
		}
		if warum := l.wetter(aus); warum != "" {
			return "zu", warum, 0
		}
		l.naechste = t
		if e.Innen == nil {
			return "", fmt.Sprintf("lüftet seit %.0f Minuten, wartet auf Messwert", dauer.Minutes()), 0
		}
		frisch := e.Innen.Zeit.After(l.st.Seit.Add(3 * time.Minute))
		if lz != nil && frisch && c.RHAnstieg > 0 && e.Innen.RH >= lz.RHIn+c.RHAnstieg {
			return "zu", fmt.Sprintf("relative Feuchte steigt (%.0f auf %.0f %%)", lz.RHIn, e.Innen.RH), 2 * time.Hour
		}
		co2 := l.co2Grund(e, tp, aus, c.CO2Zu)
		feuchte := tp-aus.Taupunkt >= c.SchlussK && e.Innen.RH > e.ZielRH-2
		if lz != nil && lz.Anlass != "co2" && !co2 && e.SonneFrei && c.SonneAbstand > 0 && tp-aus.Taupunkt < c.SonneAbstand {
			return "zu", fmt.Sprintf("Entfeuchter übernimmt mit freiem Sonnenstrom, Abstand nur %.1f K", tp-aus.Taupunkt), 0
		}
		if lz != nil && lz.Anlass != "co2" && !co2 && teuer(1.1) {
			return "zu", kostenText(), 0
		}
		if lz != nil && lz.Anlass != "co2" && !co2 && frisch &&
			dauer >= time.Duration(c.PruefMin*float64(time.Minute)) && tp >= lz.TaupunktIn {
			return "zu", fmt.Sprintf("Taupunkt drinnen nicht gefallen (%.1f °C)", tp), 2 * time.Hour
		}
		if !feuchte && !co2 {
			switch {
			case lz != nil && lz.Anlass == "co2" && e.CO2 > 0 && e.CO2 <= c.CO2Zu:
				return "zu", fmt.Sprintf("CO₂ wieder bei %.0f ppm", e.CO2), 0
			case e.Innen.RH <= e.ZielRH-2:
				return "zu", fmt.Sprintf("Raumluft trocken genug (%.0f %%)", e.Innen.RH), 0
			}
			return "zu", fmt.Sprintf("Abstand nur noch %.1f K", tp-aus.Taupunkt), 0
		}
		// Aus einer CO2-Lueftung im Spalt wird eine Feuchte-Lueftung, sobald
		// die Feuchte allein fuers Oeffnen reichen wuerde. Dann ganz auf, das
		// trocknet staerker (Wunsch vom 10.10.2026: 9 Stunden nur im Spalt).
		if lz != nil && lz.Spalt && tp-aus.Taupunkt >= c.AbstandK && e.Innen.RH > e.ZielRH &&
			!(e.SonneFrei && c.SonneAbstand > 0 && tp-aus.Taupunkt < c.SonneAbstand) &&
			!(e.EntfeuchterSonne && aus.Temp < c.SonneVorrang) {
			return "voll", fmt.Sprintf("jetzt auch wegen Feuchte, Taupunkt draußen %.1f °C, drinnen %.1f °C", aus.Taupunkt, tp), 0
		}
		return "", fmt.Sprintf("lüftet seit %s", dauerKurz(dauer)), 0
	}

	// Fenster sind zu.
	switch {
	case !e.KFDa:
		return "", "Kellerfenster nicht eingerichtet", 0
	case e.KF == "offen" || e.KF == "fährt auf":
		return "", "Fenster von Hand geöffnet", 0
	case e.Innen == nil:
		return "", "kein frischer Messwert drinnen", 0
	case !ausDa:
		return "", "keine Vorhersage", 0
	}
	l.naechste = l.vorschau(tp, e.Luft, t)
	pause := l.letztesEnde().Add(time.Duration(c.PauseMin * float64(time.Minute)))
	switch {
	case t.Before(l.st.SperreBis):
		return "", "gesperrt bis " + l.st.SperreBis.In(ort).Format("15:04"), 0
	case t.Before(pause):
		return "", "kurze Pause nach der letzten Lüftung", 0
	case c.MaxJeTag > 0 && l.heute(t) >= c.MaxJeTag:
		return "", fmt.Sprintf("heute schon %d-mal gelüftet", c.MaxJeTag), 0
	case e.Innen.Temp < c.MinInnen+1:
		return "", fmt.Sprintf("Raum zu kühl (%.1f °C)", e.Innen.Temp), 0
	}
	if warum := l.wetter(aus); warum != "" {
		return "", warum, 0
	}
	if l.co2Grund(e, tp, aus, c.CO2Auf) {
		l.naechste = t
		return "auf-co2", fmt.Sprintf("CO₂ %.0f ppm, Taupunkt draußen %.1f °C, drinnen %.1f °C", e.CO2, aus.Taupunkt, tp), 0
	}
	// Nach einer Lueftung, die wegen trockener Luft endete, erst wieder ab
	// NachTrocken Punkten ueber dem Ziel (08.10.2026: zu bei 51 %, 40 Minuten
	// spaeter bei 55 % wieder auf und nach 20 Minuten wirkungslos zu).
	ziel := e.ZielRH
	if n := len(l.st.Laeufe); n > 0 && l.st.Laeufe[n-1].TrockenEnde {
		ziel += c.NachTrocken
	}
	switch {
	case e.Innen.RH <= ziel:
		return "", fmt.Sprintf("Raumluft trocken genug (%.0f %%, wieder ab %.0f %%)", e.Innen.RH, ziel+1), 0
	case tp-aus.Taupunkt < c.AbstandK:
		return "", fmt.Sprintf("Außenluft zu feucht (Taupunkt %.1f gegen %.1f °C drinnen)", aus.Taupunkt, tp), 0
	case e.SonneFrei && c.SonneAbstand > 0 && tp-aus.Taupunkt < c.SonneAbstand:
		return "", fmt.Sprintf("Entfeuchter trocknet mit freiem Sonnenstrom, Abstand nur %.1f K", tp-aus.Taupunkt), 0
	case e.EntfeuchterSonne && aus.Temp < c.SonneVorrang:
		return "", fmt.Sprintf("Entfeuchter läuft mit Sonne, Lüften kostet bei %.0f °C Heizwärme", aus.Temp), 0
	case teuer(0.9):
		return "", kostenText(), 0
	}
	l.naechste = t
	return "auf", fmt.Sprintf("Taupunkt draußen %.1f °C, drinnen %.1f °C", aus.Taupunkt, tp), 0
}

func dauerKurz(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%.0f Minuten", d.Minutes())
	}
	return fmt.Sprintf("%d:%02d Stunden", int(d.Hours()), int(d.Minutes())%60)
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
	case "auf", "auf-co2":
		anlass := "feuchte"
		if aktion == "auf-co2" {
			anlass = "co2"
		}
		lz := lueftLauf{Von: t, Probe: !scharf, Anlass: anlass, Grund: grund, TempIn: e.Innen.Temp,
			RHIn: e.Innen.RH, TaupunktIn: taupunkt(e.Innen.Temp, e.Innen.RH), CO2In: e.CO2}
		start := punktAus(t, e.Innen, e.CO2)
		start.M = 0
		lz.Kurve = []kurvenPunkt{start}
		if w, ok := luftZu(e.Luft, t); ok {
			lz.TaupunktAus, lz.TempAus = w.Taupunkt, w.Temp
		}
		if scharf {
			l.st.VentilStart = 0
			if v := e.Ventil; v != nil {
				l.st.VentilStart = v.Ist
			}
			l.pause(true)
			err := l.fahre("auf")
			// Fuer CO2 reicht ein Spalt (gestoppt am 08.10.2026: 7 s von
			// ganz zu, ganz auf dauert 23 s). Er kuehlt weniger aus.
			if err == nil && anlass == "co2" && l.cfg.SpaltSek > 0 {
				w := l.warte
				if w == nil {
					w = time.Sleep
				}
				w(time.Duration(l.cfg.SpaltSek * float64(time.Second)))
				err = l.fahre("stop")
				lz.Spalt = err == nil
			}
			if err != nil {
				l.sag("Lüftung: Öffnen gescheitert: %v", err)
				l.pause(false)
				l.st.SperreBis = t.Add(15 * time.Minute)
				l.grund = "Öffnen gescheitert, neuer Versuch " + l.st.SperreBis.In(ort).Format("15:04")
				l.sichern()
				return
			}
		}
		l.st.Phase, l.st.Seit = "offen", t
		l.st.Laeufe = append(l.st.Laeufe, lz)
		if len(l.st.Laeufe) > 100 {
			l.st.Laeufe = l.st.Laeufe[len(l.st.Laeufe)-100:]
		}
		l.sag("Lüftung: auf%s, %s", map[bool]string{true: "", false: " (Probe)"}[scharf], grund)
		l.sichern()

	case "voll":
		lz := l.laufzeit()
		if lz != nil && !lz.Probe {
			if err := l.fahre("auf"); err != nil {
				l.sag("Lüftung: ganz öffnen gescheitert: %v", err)
				return
			}
		}
		if lz != nil {
			lz.Spalt, lz.Anlass = false, "feuchte"
		}
		l.sag("Lüftung: ganz auf, %s", grund)
		l.sichern()

	case "zu":
		lz := l.laufzeit()
		if lz == nil || !lz.Probe {
			if err := l.fahre("ab"); err != nil {
				if l.st.Phase != "schliesst" {
					l.sag("Lüftung: Schließen gescheitert, versuche es weiter: %v", err)
				}
				l.st.Phase = "schliesst"
				l.grund = "Schließen gescheitert, neuer Versuch in einer Minute"
				l.sichern()
				return
			}
			l.pause(false)
		}
		if lz != nil {
			lz.Bis, lz.Ende = t, grund
			lz.TrockenEnde = strings.HasPrefix(grund, "Raumluft trocken genug")
			if e.Innen != nil {
				lz.TempEnde, lz.RHEnde, lz.CO2Ende = e.Innen.Temp, e.Innen.RH, e.CO2
				lz.TaupunktEnde = taupunkt(e.Innen.Temp, e.Innen.RH)
				l.merkePunkt(lz, e.Innen, e.CO2)
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
		if lz != nil && e.Innen != nil && l.merkePunkt(lz, e.Innen, e.CO2) {
			l.sichern()
		}
		// Offen: bei kalter Aussenluft zeigt das Ventil unter dem Fenster, ob
		// es wirklich offen ist. Nur noch zur Anzeige: es reagiert zu traege,
		// die Wiederholung des Befehls kam am 08.10.2026 bei jeder Lueftung,
		// obwohl das Fenster offen war.
		if l.st.Phase == "offen" && scharf && lz != nil && !lz.Probe && !lz.Bestaetigt && e.Ventil != nil &&
			l.st.VentilStart > 0 && lz.TempAus < l.st.VentilStart-3 &&
			(e.Ventil.Fenster || e.Ventil.Ist <= l.st.VentilStart-0.5) {
			lz.Bestaetigt = true
			l.sichern()
		}
	}
}

// merkePunkt haengt einen neuen Messwert an die Kurve, true wenn neu.
func (l *lueftung) merkePunkt(lz *lueftLauf, w *messFeuchte, co2 float64) bool {
	if !w.Zeit.After(lz.Von) {
		return false
	}
	p := punktAus(lz.Von, w, co2)
	if n := len(lz.Kurve); n > 0 && p.M <= lz.Kurve[n-1].M+0.5 {
		return false
	}
	lz.Kurve = append(lz.Kurve, p)
	return true
}

func (l *lueftung) sichern() { schreibeJSON(l.standPfad, l.st) }

// hinweise fuer die Wand: Fenster liessen sich nicht fahren.
func (l *lueftung) hinweise() []string {
	l.Lock()
	defer l.Unlock()
	if l.cfg.Modus == "scharf" && strings.Contains(l.grund, "gescheitert") {
		return []string{"Kellerfenster: " + l.grund}
	}
	return nil
}

// lueftet: ob gerade gelueftet wird, fuer Heizung und Entfeuchter.
func (l *lueftung) lueftet() bool {
	l.Lock()
	defer l.Unlock()
	return l.st.Phase != "zu" && l.cfg.Modus == "scharf"
}

func (l *lueftung) laufe() {
	// Nach einem Neustart mitten in einer Lueftung: Pause sofort setzen, sonst
	// laeuft der Entfeuchter an, bevor die Lueftung wieder entschieden hat.
	l.Lock()
	if l.st.Phase != "zu" && l.cfg.Modus == "scharf" {
		l.pause(true)
	}
	l.Unlock()
	time.Sleep(2 * time.Minute) // erst Sensor, Box und Prognose
	for {
		l.schritt()
		time.Sleep(time.Minute)
	}
}

func (l *lueftung) stand(e lueftEingang) map[string]any {
	l.Lock()
	defer l.Unlock()
	l.ergaenze(&e)
	a := map[string]any{"cfg": l.cfg, "modus": l.cfg.Modus, "phase": l.st.Phase, "seit": l.st.Seit,
		"grund": l.grund, "heute": l.heute(e.Jetzt)}
	if l.kosten != nil && !math.IsInf(l.kosten.Lueften, 0) {
		a["kosten"] = l.kosten
	}
	if !l.naechste.IsZero() {
		a["naechste"] = l.naechste
	}
	if e.Innen != nil {
		a["innen"] = map[string]float64{"temp": e.Innen.Temp, "rh": e.Innen.RH,
			"taupunkt": math.Round(taupunkt(e.Innen.Temp, e.Innen.RH)*10) / 10,
			"wasser":   math.Round(wasser(e.Innen.Temp, e.Innen.RH)*10) / 10}
	}
	if e.CO2 > 0 {
		a["co2"] = e.CO2
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
	switch {
	case c.AbstandK < 0.5 || c.AbstandK > 10:
		return "Taupunktabstand 0,5 bis 10 K"
	case c.SchlussK < 0 || c.SchlussK >= c.AbstandK:
		return "Schließen ab einem Abstand unter dem zum Öffnen"
	case c.MinInnen < 5 || c.MinInnen > 25:
		return "Raum mindestens 5 bis 25 °C"
	case c.MinAussen < -20 || c.MinAussen > 25:
		return "Außen mindestens -20 bis 25 °C"
	case c.PruefMin < 5 || c.PruefMin > 120:
		return "Prüfung nach 5 bis 120 Minuten"
	case c.RHAnstieg < 0 || c.RHAnstieg > 30:
		return "Feuchteanstieg 0 bis 30 Punkte"
	case c.PauseMin < 0 || c.PauseMin > 600:
		return "Pause 0 bis 600 Minuten"
	case c.MaxStd < 1 || c.MaxStd > 48:
		return "Notbremse 1 bis 48 Stunden"
	case c.MaxJeTag < 0 || c.MaxJeTag > 24:
		return "0 bis 24 Lüftungen am Tag, 0 heißt unbegrenzt"
	case c.MaxBoeen < 10 || c.MaxBoeen > 150:
		return "Böen 10 bis 150 km/h"
	case c.ZielRH < 0 || c.ZielRH > 90:
		return "Zielfeuchte 0 bis 90 %"
	case c.Kaeltestrafe < 0 || c.Kaeltestrafe > 1:
		return "Kältestrafe 0 bis 1 K je Grad"
	case c.SonneVorrang < -30 || c.SonneVorrang > 30:
		return "Vorrang des Entfeuchters -30 bis 30 °C"
	case c.SonneAbstand < 0 || c.SonneAbstand > 10:
		return "Sonnenvorrang 0 bis 10 K"
	case c.NachTrocken < 0 || c.NachTrocken > 15:
		return "Abstand nach trockener Luft 0 bis 15 Punkte"
	case c.SpaltSek < 0 || c.SpaltSek > 60:
		return "Spalt 0 bis 60 Sekunden"
	case c.CO2Zu < 400 || c.CO2Auf <= c.CO2Zu || c.CO2Max < c.CO2Auf || c.CO2Max > 5000:
		return "CO₂: zu unter auf unter Höchstwert, ab 400 ppm"
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
		l.Unlock()
		l.sag("Lüftung: Einstellungen geändert, Modus %s", c.Modus)
		w.WriteHeader(204)
	})
}
