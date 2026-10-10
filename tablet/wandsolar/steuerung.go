package main

// Steuerung der Verbraucher. Jedes Geraet ist ein Schalter plus Regeln,
// eingestellt ueber die Oberflaeche /steuerung und gespeichert in
// steuerung.json. Der erste Verbraucher ist der Luftentfeuchter am Shelly
// Plug M Gen3 (Vorgaben vom 03.10.2026):
//
//   - darf werktags 11 bis 18 Uhr laufen, am Wochenende 13 bis 18 Uhr
//   - in jedem gleitenden 7-Tage-Raum hoechstens 30 und mindestens 5 Stunden
//   - spaetestens alle 3 Tage ein Lauf von mindestens 30 Minuten
//   - Netzstrom nur, wenn es anders nicht geht
//
// Wann er laeuft, legt der Planer fest (planer.go). Die Regelung hier folgt
// dem Plan und prueft ihn gegen die Messung:
//
//   - Plan sagt "frei" und es kommt trotzdem Strom aus dem Netz: aus, die
//     Prognose war zu freundlich. Der naechste Plan rechnet mit der Messung.
//   - Plan sagt "pflicht": an, auch mit Netzstrom.
//   - Kein Plan, aber echte Einspeisung: an, wie bisher.
//
// Kompressor und Wassertank (Hinweis von Thorsten, 03.10.2026): Nach dem
// Einschalten laeuft erst nur der Luefter, der Kompressor folgt nach etwa
// zwei Minuten, und die Leistung springt. Nur dann wird entfeuchtet. Bleibt
// der Sprung nach der Anlaufzeit eine Viertelstunde aus, ist mit hoher
// Wahrscheinlichkeit der Wassertank voll. Dann wird abgeschaltet, auf der
// Wand und in der Oberflaeche erscheint ein Hinweis, und eine Stunde spaeter
// probiert die Regelung es erneut. Springt der Kompressor wieder an, ist der
// Hinweis von selbst weg. "Tank geleert" in der Oberflaeche beendet die
// Wartezeit sofort.
//
// Raumluft (Vorgaben vom 03.10.2026, im Raum schlaeft nachts jemand, morgens
// wird automatisch gelueftet): Mit einem Feuchtesensor gilt
//
//   - ueber der Obergrenze: laufen, notfalls mit Netzstrom, bis 3 Punkte
//     darunter
//   - unter der Untergrenze: nicht laufen, auch nicht mit freier Sonne, sonst
//     wird die Luft nur ausgetrocknet
//   - dazwischen: nur freie Laeufe
//   - Fenster des Raums offen: Pause, waehrend gelueftet wird
//
// Die festen Mindestregeln (5 h in 7 Tagen, alle 3 Tage) sind ein Ersatz
// fuer die fehlende Messung. Mit frischem Sensorwert ruhen sie, faellt der
// Sensor aus, gelten sie wieder.
//
// Mindestens zehn Minuten an und zehn aus, wegen des Kompressors. Jedes
// Einschalten geht mit toggle_after an den Shelly: faellt das Tablet aus,
// schaltet er sich nach der Totmannzeit selbst ab.

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type zeitraum struct {
	Von float64 `json:"von"` // Stunden, 11 heisst 11:00
	Bis float64 `json:"bis"`
}

type geraetCfg struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Typ           string      `json:"typ"` // schalter
	ShellyIP      string      `json:"shelly_ip"`
	ShellyMAC     string      `json:"shelly_mac"`
	Modus         string      `json:"modus"` // aus, probe, scharf
	LeistungKW    float64     `json:"leistung_kw"`
	LeistungFest  bool        `json:"leistung_fest"`
	Zeiten        [7]zeitraum `json:"zeiten"` // Mo bis So
	MaxH7         float64     `json:"max_h_7t"`
	MinH7         float64     `json:"min_h_7t"`
	MaxLueckeTage int         `json:"max_luecke_tage"`
	MinLaufMin    float64     `json:"min_lauf_min"`
	MinAnMin      float64     `json:"min_an_min"`
	MinAusMin     float64     `json:"min_aus_min"`
	EinReserveW   float64     `json:"ein_reserve_w"`
	AusBezugW     float64     `json:"aus_bezug_w"`
	Vorziehen     bool        `json:"vorziehen"`
	NetzErlaubt   bool        `json:"netz_erlaubt"`
	TotmannMin    float64     `json:"totmann_min"`
	SensorID      string      `json:"sensor_id"` // SwitchBot, leer heisst ohne
	SensorName    string      `json:"sensor_name"`
	FeuchteOben   float64     `json:"feuchte_oben"`
	FeuchteUnten  float64     `json:"feuchte_unten"`
	Fenster       []string    `json:"fenster"` // Velux-Geraete des Raums
}

func standardEntfeuchter() geraetCfg {
	w, we := zeitraum{11, 18}, zeitraum{13, 18}
	return geraetCfg{
		ID: "entfeuchter", Name: "Luftentfeuchter", Typ: "schalter",
		ShellyIP: "192.168.2.160", ShellyMAC: "089272568D2C", Modus: "probe",
		LeistungKW: 0.29, Zeiten: [7]zeitraum{w, w, w, w, w, we, we},
		MaxH7: 30, MinH7: 5, MaxLueckeTage: 3, MinLaufMin: 30,
		MinAnMin: 10, MinAusMin: 10, EinReserveW: 100, AusBezugW: 100,
		Vorziehen: true, NetzErlaubt: true, TotmannMin: 15,
		FeuchteOben: 65, FeuchteUnten: 50,
	}
}

func (c geraetCfg) erlaubt(t time.Time) bool {
	i := tagIndex(t) // Montag = 0, Feiertage und Ferien wie Sonntag
	h := float64(t.Hour()) + float64(t.Minute())/60
	z := c.Zeiten[i]
	return h >= z.Von && h < z.Bis
}

type lauf struct {
	Von   time.Time          `json:"von"`
	Bis   time.Time          `json:"bis,omitempty"`
	Probe bool               `json:"probe,omitempty"`
	Min   map[string]float64 `json:"min"` // Minuten je Quelle: sonne, akku, netz
	Grund string             `json:"grund"`
}

type geraetStand struct {
	An         bool      `json:"an"`
	Seit       time.Time `json:"seit"`
	LeistungKW float64   `json:"leistung_kw"`
	ShellyIP   string    `json:"shelly_ip"`
	Laeufe     []lauf    `json:"laeufe"`
	Hand       string    `json:"hand,omitempty"` // an, aus
	HandBis    time.Time `json:"hand_bis,omitempty"`
	KompKW     float64   `json:"kompressor_kw"`      // gelernte Leistung mit Kompressor
	Stoerung   string    `json:"stoerung,omitempty"` // z. B. Tank voll
	StoerSeit  time.Time `json:"stoerung_seit,omitempty"`
	Wiederholt time.Time `json:"wiederholt,omitempty"` // naechster Versuch nach Stoerung
	Beginn     time.Time `json:"beginn"`
	LetzteZeit time.Time `json:"letzte_zeit"`
}

type geraet struct {
	cfg          geraetCfg
	st           geraetStand
	plan         plan
	planZeit     time.Time
	grund        string
	ueberSeit    time.Time
	mangelSeit   time.Time
	freiSperre   time.Time // geplante freie Laeufe ausgesetzt bis
	modusAlt     string
	letzteKW     float64 // was der Shelly zuletzt gemessen hat
	komp         bool    // Kompressor lief bei der letzten Messung
	kompZuletzt  time.Time
	nass         bool // ueber der Obergrenze, bis 3 Punkte darunter
	trocken      bool // unter der Untergrenze, bis 2 Punkte darueber
	feuchte      *messFeuchte
	fensterAuf   bool
	shellyFehler time.Time // seit wann der Shelly nicht antwortet
}

type steuerung struct {
	sync.Mutex
	cfgPfad, standPfad string
	geraete            []*geraet
	sag                func(string, ...any)
	schalte            func(ip string, an bool, totmann int) (float64, error)
	prognose           map[int64]float64 // Stundenbeginn -> kW, von der Wetterseite
	prognoseZeit       time.Time
	letzte             *messwert
	stundenPV          func() map[int]float64 // gemessene Stundenmittel heute
	sb                 *switchbot
	veluxBasis         string
	fenster            map[string]float64
	fensterZeit        time.Time
	aussen             map[int64]float64  // Aussentemperatur je Stunde, von der Wetterseite
	luft               map[int64]luftWert // Taupunkt, Regen, Boeen je Stunde, von der Wetterseite
	lueftPause         atomic.Bool        // die Lueftung hat die Fenster offen
	lueftBald          atomic.Int64       // Unix-Zeit der naechsten Lueftung heute, 0 ohne
}

// aussenJetzt: Aussentemperatur der laufenden Stunde, nil ohne Prognose.
func (s *steuerung) aussenJetzt() any {
	s.Lock()
	defer s.Unlock()
	if v, ok := s.aussen[time.Now().Truncate(time.Hour).Unix()]; ok {
		return v
	}
	return nil
}

// sensorIDs fuer den Abruf.
func (s *steuerung) sensorIDs() []string {
	s.Lock()
	defer s.Unlock()
	var ids []string
	for _, g := range s.geraete {
		if g.cfg.SensorID != "" {
			ids = append(ids, g.cfg.SensorID)
		}
	}
	return ids
}

// raumluft setzt Feuchte- und Fensterzustand des Geraets. Ohne frischen
// Sensorwert bleibt feuchte nil.
func (s *steuerung) raumluft(g *geraet, t time.Time) {
	g.feuchte = nil
	if g.cfg.SensorID != "" && s.sb != nil {
		if w, ok := s.sb.wert(g.cfg.SensorID); ok {
			g.feuchte = &w
		}
	}
	if g.feuchte == nil {
		g.nass, g.trocken = false, false
	} else {
		rh := g.feuchte.RH
		if rh >= g.cfg.FeuchteOben {
			g.nass = true
		} else if rh <= g.cfg.FeuchteOben-3 {
			g.nass = false
		}
		if rh <= g.cfg.FeuchteUnten {
			g.trocken = true
		} else if rh >= g.cfg.FeuchteUnten+2 {
			g.trocken = false
		}
	}
	g.fensterAuf = false
	if len(g.cfg.Fenster) > 0 && s.veluxBasis != "" {
		if t.Sub(s.fensterZeit) >= time.Minute {
			if f, err := fensterStand(s.veluxBasis); err == nil {
				s.fenster = f
			}
			s.fensterZeit = t
		}
		for _, id := range g.cfg.Fenster {
			if s.fenster[id] > 0 {
				g.fensterAuf = true
			}
		}
	}
	// Die Kellerfenster melden keine Stellung, die Lueftung sagt es selbst.
	if g.cfg.SensorID != "" && s.lueftPause.Load() {
		g.fensterAuf = true
	}
}

func neueSteuerung(cfgPfad, standPfad string, sag func(string, ...any)) *steuerung {
	s := &steuerung{cfgPfad: cfgPfad, standPfad: standPfad, sag: sag, prognose: map[int64]float64{}}
	s.schalte = shellySchalte
	var cfgs []geraetCfg
	if roh, err := os.ReadFile(cfgPfad); err == nil {
		json.Unmarshal(roh, &cfgs)
	}
	if len(cfgs) == 0 {
		cfgs = []geraetCfg{standardEntfeuchter()}
	}
	staende := map[string]geraetStand{}
	if roh, err := os.ReadFile(standPfad); err == nil {
		json.Unmarshal(roh, &staende)
	}
	for _, c := range cfgs {
		g := &geraet{cfg: c, st: staende[c.ID], modusAlt: c.Modus}
		if g.st.LeistungKW <= 0 {
			g.st.LeistungKW = c.LeistungKW
		}
		if g.st.ShellyIP == "" {
			g.st.ShellyIP = c.ShellyIP
		}
		s.geraete = append(s.geraete, g)
	}
	return s
}

func schreibeJSON(pfad string, v any) {
	roh, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return
	}
	tmp := pfad + ".neu"
	if os.WriteFile(tmp, roh, 0644) == nil {
		os.Rename(tmp, pfad)
	}
}

func (s *steuerung) sichern() {
	st := map[string]geraetStand{}
	for _, g := range s.geraete {
		st[g.cfg.ID] = g.st
	}
	schreibeJSON(s.standPfad, st)
}

func (s *steuerung) sichereCfg() {
	var c []geraetCfg
	for _, g := range s.geraete {
		c = append(c, g.cfg)
	}
	schreibeJSON(s.cfgPfad, c)
}

func (g *geraet) leistung() float64 {
	if g.cfg.LeistungFest || g.st.LeistungKW <= 0 {
		return g.cfg.LeistungKW
	}
	return g.st.LeistungKW
}

// probe: laufen die Zahlen dieses Geraets im Probebetrieb?
func (g *geraet) probe() bool { return g.cfg.Modus != "scharf" }

func (g *geraet) laeufeFuer(probe bool) []lauf {
	var l []lauf
	for _, x := range g.st.Laeufe {
		if x.Probe == probe {
			l = append(l, x)
		}
	}
	return l
}

func (g *geraet) offenerLauf() *lauf {
	if n := len(g.st.Laeufe); n > 0 && g.st.Laeufe[n-1].Bis.IsZero() {
		return &g.st.Laeufe[n-1]
	}
	return nil
}

// setzePrognose nimmt die Stundenwerte der Wetterseite an.
func (s *steuerung) setzePrognose(p map[int64]float64) {
	s.Lock()
	defer s.Unlock()
	s.prognose = p
	s.prognoseZeit = time.Now()
	for _, g := range s.geraete {
		g.planZeit = time.Time{} // neu planen
	}
}

// faktorHeute: gemessene durch vorhergesagte Erzeugung der vergangenen
// Stunden von heute. 1, solange zu wenig Sonne war, um es zu sagen.
func (s *steuerung) faktorHeute(t time.Time) float64 {
	if s.stundenPV == nil {
		return 1
	}
	gem := s.stundenPV()
	var sm, sp float64
	tag := tagesAnfang(t)
	for h := 0; h < t.Hour(); h++ {
		v, ok := s.prognose[tag.Add(time.Duration(h)*time.Hour).Unix()]
		m, okm := gem[h]
		if ok && okm && v > 0.3 {
			sp += v
			sm += m
		}
	}
	// Mit einem Polster von 2 kWh auf beiden Seiten: zwei truebe
	// Morgenstunden sollen nicht gleich den ganzen Tag abwerten.
	if sp < 1 {
		return 1
	}
	return math.Max(0.3, math.Min(1.2, (sm+2)/(sp+2)))
}

func (s *steuerung) planeFuer(g *geraet, t time.Time, soc float64) {
	cfg := g.cfg
	hand := g.st.Hand == "aus" && t.Before(g.st.HandBis)
	if g.feuchte != nil {
		// Mit Messung entscheidet die Feuchte, nicht die Stundenregel.
		cfg.MinH7, cfg.MaxLueckeTage = 0, 0
	}
	in := planEingabe{
		Jetzt: t, SOC: soc, PV: s.prognose, Faktor: s.faktorHeute(t),
		Leistung: g.leistung(), Cfg: cfg,
		Erlaubt: func(x time.Time) bool {
			if hand && x.Before(g.st.HandBis) {
				return false
			}
			return cfg.erlaubt(x)
		},
		Laeufe: g.laeufeFuer(g.probe()), Beginn: g.st.Beginn,
	}
	g.plan = neuerPlaner(in).rechne()
	g.planZeit = t
}

// pruefe laeuft nach jeder Messung.
func (s *steuerung) pruefe(m messwert) {
	s.Lock()
	defer s.Unlock()
	s.letzte = &m
	t := m.Zeit
	for _, g := range s.geraete {
		s.pruefeGeraet(g, m, t)
	}
	s.sichern()
}

func (s *steuerung) pruefeGeraet(g *geraet, m messwert, t time.Time) {
	// Nach einem Neustart laeuft die Uhr die erste Minute falsch.
	if !g.st.LetzteZeit.IsZero() && t.Before(g.st.LetzteZeit) {
		return
	}
	if g.st.Beginn.IsZero() {
		g.st.Beginn = t
	}
	cfg := g.cfg

	// Moduswechsel: aus und neu beginnen, damit gedachte Laufzeit nicht
	// als echte weiterlaeuft.
	if cfg.Modus != g.modusAlt {
		s.sag("%s: Modus %s", cfg.Name, cfg.Modus)
		if g.st.An {
			g.st.An = false
			s.beende(g, t, "Moduswechsel")
		}
		g.st.Seit = t
		if cfg.Modus == "scharf" || g.modusAlt == "scharf" {
			s.befehl(g, false)
		}
		g.modusAlt = cfg.Modus
		g.planZeit = time.Time{}
	}

	// Laufzeit verbuchen, nach Quelle.
	if g.st.An && !g.st.LetzteZeit.IsZero() {
		dt := math.Min(t.Sub(g.st.LetzteZeit).Minutes(), 2)
		if l := g.offenerLauf(); l != nil && dt > 0 {
			q := "sonne"
			switch {
			case m.Netz < -0.05:
				q = "netz"
			case m.Akku < -0.05:
				q = "akku"
			}
			if l.Min == nil {
				l.Min = map[string]float64{}
			}
			l.Min[q] += dt
			if g.komp {
				l.Min["kompressor"] += dt
			}
		}
	}
	g.st.LetzteZeit = t

	if cfg.Modus == "aus" {
		g.grund = "abgeschaltet"
		return
	}
	hatteSensor := g.feuchte != nil
	s.raumluft(g, t)
	if g.planZeit.IsZero() || t.Sub(g.planZeit) >= 5*time.Minute || hatteSensor != (g.feuchte != nil) {
		s.planeFuer(g, t, m.SOC)
	}

	// Was sagt der Plan fuer jetzt?
	art := ""
	if len(g.plan.Slots) > 0 && g.plan.Slots[0].An && !t.Before(g.plan.Slots[0].Zeit) &&
		t.Before(g.plan.Slots[0].Zeit.Add(slotDauer)) {
		art = g.plan.Slots[0].Art
	}
	if art == "frei" && t.Before(g.freiSperre) {
		art = ""
	}

	// Ueberschuss und Mangel mit Dauer.
	p := g.leistung()
	ueber := m.Netz >= p+cfg.EinReserveW/1000
	bezug := m.Netz < -cfg.AusBezugW/1000
	mangel := bezug || m.Akku < -cfg.AusBezugW/1000
	if ueber {
		if g.ueberSeit.IsZero() {
			g.ueberSeit = t
		}
	} else {
		g.ueberSeit = time.Time{}
	}
	// Laeuft er nach Plan frei, ist Akkustrom gewollt; dann zaehlt nur Netz.
	schlecht := mangel
	if g.st.An && art == "frei" {
		schlecht = bezug
	}
	if schlecht {
		if g.mangelSeit.IsZero() {
			g.mangelSeit = t
		}
	} else {
		g.mangelSeit = time.Time{}
	}

	dauer := t.Sub(g.st.Seit)
	minAn := time.Duration(cfg.MinAnMin * float64(time.Minute))
	minAus := time.Duration(cfg.MinAusMin * float64(time.Minute))
	woche := g.minuten7Grenze(t) // nur Netz- und Akkuminuten
	handAn := g.st.Hand == "an" && t.Before(g.st.HandBis)
	handAus := g.st.Hand == "aus" && t.Before(g.st.HandBis)
	mangelLang := !g.mangelSeit.IsZero() && t.Sub(g.mangelSeit) >= 3*time.Minute

	stoerung := g.st.Stoerung != "" && t.Before(g.st.Wiederholt)
	rh := ""
	if g.feuchte != nil {
		rh = fmt.Sprintf("%.0f %%", g.feuchte.RH)
	}
	// Steht heute eine Lueftung an, trocknet der Entfeuchter vorher nicht mit
	// Netzstrom, was die Lueftung umsonst erledigt. Mit Sonne laeuft er
	// weiter, und ist die Luft sehr feucht, wartet er nicht.
	wartet := ""
	if b := s.lueftBald.Load(); b > 0 && g.feuchte != nil && g.feuchte.RH < cfg.FeuchteOben+5 {
		if bt := time.Unix(b, 0); bt.Sub(t) < 8*time.Hour {
			wartet = "wartet auf die Lüftung um " + bt.In(ort).Format("15:04")
			if !bt.After(t) {
				wartet = "die Lüftung beginnt gleich"
			}
		}
	}

	switch {
	case g.st.An && stoerung:
		s.setze(g, t, false, g.st.Stoerung)
	case handAn:
		s.setze(g, t, true, "von Hand bis "+g.st.HandBis.Format("15:04"))
	case handAus:
		s.setze(g, t, false, "von Hand aus bis "+g.st.HandBis.Format("02.01. 15:04"))
	case g.st.An && g.fensterAuf:
		s.setze(g, t, false, "Fenster offen, Pause beim Lueften")
	case g.st.An && !cfg.erlaubt(t):
		s.setze(g, t, false, "erlaubte Laufzeit vorbei")
	case g.st.An && g.trocken && dauer >= minAn:
		s.setze(g, t, false, "Raumluft trocken genug ("+rh+")")
	case g.st.An && cfg.MaxH7 > 0 && woche >= cfg.MaxH7*60:
		s.setze(g, t, false, fmt.Sprintf("%.0f Stunden in 7 Tagen erreicht", cfg.MaxH7))
	case g.st.An && art == "pflicht":
		s.setze(g, t, true, "Pflichtlauf nach Plan")
	case g.st.An && g.nass && wartet == "":
		s.setze(g, t, true, "Raumluft zu feucht ("+rh+")")
	case g.st.An && mangelLang && dauer >= minAn:
		if art == "frei" {
			g.freiSperre = g.plan.Slots[0].Zeit.Add(slotDauer)
			g.planZeit = time.Time{}
			s.setze(g, t, false, fmt.Sprintf("Netzbezug trotz Plan (%+.2f kW), Prognose zu freundlich", m.Netz))
		} else {
			s.setze(g, t, false, fmt.Sprintf("kein Ueberschuss mehr (Netz %+.2f kW, Akku %+.2f kW)", m.Netz, m.Akku))
		}
	case g.st.An:
		s.setze(g, t, true, "")
	case stoerung:
		g.grund = g.st.Stoerung + ", neuer Versuch " + g.st.Wiederholt.Format("15:04")
	case g.fensterAuf:
		g.grund = "Fenster offen, Pause beim Lueften"
	case !cfg.erlaubt(t):
		g.grund = "ausserhalb der erlaubten Laufzeit"
	case cfg.MaxH7 > 0 && cfg.MaxH7*60-woche < 15:
		g.grund = fmt.Sprintf("%.0f Stunden in 7 Tagen erreicht", cfg.MaxH7)
	case !g.st.Seit.IsZero() && dauer < minAus:
		g.grund = "Pause nach dem Abschalten"
	case g.trocken:
		g.grund = "Raumluft trocken genug (" + rh + "), kein Lauf"
	case g.nass && wartet == "":
		s.setze(g, t, true, "Raumluft zu feucht ("+rh+"), laeuft notfalls mit Netzstrom")
	case art == "pflicht":
		s.setze(g, t, true, "Pflichtlauf nach Plan")
	case art == "frei":
		s.setze(g, t, true, "Sonne nach Plan")
	case !g.ueberSeit.IsZero() && t.Sub(g.ueberSeit) >= 5*time.Minute:
		s.setze(g, t, true, fmt.Sprintf("Einspeisung %.2f kW seit %s", m.Netz, g.ueberSeit.Format("15:04")))
	case !g.ueberSeit.IsZero():
		g.grund = "Einspeisung, wartet auf fuenf Minuten"
	case g.nass:
		g.grund = "Raumluft zu feucht (" + rh + "), " + wartet
	default:
		g.grund = "wartet"
		if n := g.naechster(t); !n.IsZero() {
			g.grund = "naechster Lauf geplant " + n.Format("Mon 15:04")
		}
	}
}

func (g *geraet) naechster(t time.Time) time.Time {
	for _, sl := range g.plan.Slots {
		if sl.An && sl.Zeit.After(t) {
			return sl.Zeit
		}
	}
	return time.Time{}
}

// zaehlt: Anteil eines Laufs, der gegen MaxH7 zaehlt. Minuten mit reinem
// Sonnenstrom zaehlen nicht (Vorgabe vom 07.10.2026): an einem Sonnentag soll
// die Wochengrenze den Entfeuchter nicht ausbremsen. Ohne Aufteilung zaehlt
// der ganze Lauf.
func (l lauf) zaehlt() float64 {
	ges := l.Min["sonne"] + l.Min["akku"] + l.Min["netz"]
	if ges <= 0 {
		return 1
	}
	return (l.Min["akku"] + l.Min["netz"]) / ges
}

// minuten7Grenze: was davon gegen MaxH7 zaehlt.
func (g *geraet) minuten7Grenze(t time.Time) float64 {
	m := 0.0
	von := t.Add(-7 * 24 * time.Hour)
	for _, l := range g.laeufeFuer(g.probe()) {
		a, b := l.Von, l.Bis
		if b.IsZero() {
			b = t
		}
		if a.Before(von) {
			a = von
		}
		if b.After(a) {
			m += b.Sub(a).Minutes() * l.zaehlt()
		}
	}
	return m
}

// minuten7: Laufzeit der letzten 7 Tage im aktuellen Modus.
func (g *geraet) minuten7(t time.Time) float64 {
	m := 0.0
	von := t.Add(-7 * 24 * time.Hour)
	for _, l := range g.laeufeFuer(g.probe()) {
		a, b := l.Von, l.Bis
		if b.IsZero() {
			b = t
		}
		if a.Before(von) {
			a = von
		}
		if b.After(a) {
			m += b.Sub(a).Minutes()
		}
	}
	return m
}

func (s *steuerung) beende(g *geraet, t time.Time, grund string) {
	if l := g.offenerLauf(); l != nil {
		l.Bis = t
	}
	// Protokoll auf 28 Tage kuerzen.
	grenze := t.Add(-28 * 24 * time.Hour)
	var neu []lauf
	for _, l := range g.st.Laeufe {
		if l.Bis.IsZero() || l.Bis.After(grenze) {
			neu = append(neu, l)
		}
	}
	g.st.Laeufe = neu
}

// setze schaltet, im Probebetrieb nur auf dem Papier. Ein leerer Grund heisst
// erneuern.
func (s *steuerung) setze(g *geraet, t time.Time, an bool, grund string) {
	wechsel := an != g.st.An
	if grund != "" {
		g.grund = grund
	}
	if wechsel {
		g.st.An = an
		g.st.Seit = t
		if an {
			g.st.Laeufe = append(g.st.Laeufe, lauf{Von: t, Probe: g.probe(), Grund: grund,
				Min: map[string]float64{}})
		} else {
			s.beende(g, t, grund)
		}
		wort := map[bool]string{true: "ein", false: "aus"}[an]
		if g.probe() {
			s.sag("%s Probe: %s, %s (7 Tage %.1f h)", g.cfg.Name, wort, grund, g.minuten7(t)/60)
		} else {
			s.sag("%s %s: %s (7 Tage %.1f h)", g.cfg.Name, wort, grund, g.minuten7(t)/60)
		}
	}
	if g.probe() {
		return
	}
	kw, ok := s.befehl(g, an)
	if ok && an && !wechsel {
		s.kompressor(g, t, kw)
	}
}

// Schwelle zwischen nur Luefter und Kompressor: die Haelfte der gelernten
// Kompressorleistung, bis dahin 100 W.
func (g *geraet) kompSchwelle() float64 {
	if g.st.KompKW > 0.15 {
		return g.st.KompKW / 2
	}
	return 0.1
}

const (
	// Eingemessen am 03.10.2026: nach dem Einschalten 3 Minuten bei 1,2 W
	// (Bereitschaft, das Geraet misst), dann Kompressor mit 265 W, nach
	// 6 Minuten warm bei 290 W. Ein Luefter allein war nicht zu sehen.
	kompAnlauf = 6 * time.Minute
	kompFehlt  = 15 * time.Minute // danach so lange ohne Kompressor: Stoerung
)

func (s *steuerung) kompressor(g *geraet, t time.Time, kw float64) {
	g.komp = kw >= g.kompSchwelle()
	if g.komp {
		g.kompZuletzt = t
		if g.st.KompKW <= 0 {
			g.st.KompKW = kw
		}
		g.st.KompKW = 0.9*g.st.KompKW + 0.1*kw
		// Die Leistung fuer den Plan nur aus Messungen mit Kompressor.
		if !g.cfg.LeistungFest {
			g.st.LeistungKW = 0.8*g.st.LeistungKW + 0.2*kw
		}
		if g.st.Stoerung != "" {
			s.sag("%s: Kompressor laeuft wieder, Hinweis erledigt", g.cfg.Name)
			g.st.Stoerung, g.st.StoerSeit, g.st.Wiederholt = "", time.Time{}, time.Time{}
		}
		return
	}
	seit := g.st.Seit.Add(kompAnlauf)
	if g.kompZuletzt.After(seit) {
		seit = g.kompZuletzt
	}
	if t.Sub(g.st.Seit) < kompAnlauf || t.Sub(seit) < kompFehlt {
		return
	}
	text := "Wassertank vermutlich voll, nur Luefter"
	if kw < 0.02 {
		text = "Wassertank vermutlich voll, Entfeuchter steht"
	}
	if g.st.Stoerung == "" {
		g.st.StoerSeit = t
	}
	g.st.Stoerung = text
	g.st.Wiederholt = t.Add(time.Hour)
	s.setze(g, t, false, text)
}

// Ab diesem Batteriestand warnt die Wand.
const sensorBattGrenze = 15

// hinweise fuer die Wetterseite.
func (s *steuerung) hinweise() []string {
	s.Lock()
	defer s.Unlock()
	var h []string
	for _, g := range s.geraete {
		if g.st.Stoerung != "" && g.cfg.Modus == "scharf" {
			h = append(h, g.cfg.Name+": "+g.st.Stoerung)
		}
		if !g.shellyFehler.IsZero() && time.Since(g.shellyFehler) > 10*time.Minute {
			h = append(h, g.cfg.Name+": Shelly nicht erreichbar seit "+g.shellyFehler.In(ort).Format("15:04"))
		}
		// Feuchtesensor: Batterie und Funkstille. Der Meter Plus laeuft mit
		// zwei AAA-Zellen; SwitchBot meldet den Stand in Prozent.
		if g.cfg.SensorID != "" && s.sb != nil {
			name := strings.TrimSpace(g.cfg.SensorName)
			if w, ok := s.sb.letzter(g.cfg.SensorID); ok {
				if w.Batt > 0 && w.Batt <= sensorBattGrenze {
					h = append(h, fmt.Sprintf("%s: Batterie wechseln (%d %%)", name, w.Batt))
				}
				if time.Since(w.Zeit) > 2*time.Hour {
					h = append(h, name+": seit "+w.Zeit.Format("15:04")+" keine Werte, Batterie oder Hub pruefen")
				}
			}
		}
	}
	return h
}

// befehl schickt den Schaltbefehl, sucht den Shelly notfalls neu.
func (s *steuerung) befehl(g *geraet, an bool) (float64, bool) {
	totmann := int(g.cfg.TotmannMin * 60)
	if totmann <= 0 {
		totmann = 900
	}
	kw, err := s.schalte(g.st.ShellyIP, an, totmann)
	if err != nil && g.cfg.ShellyMAC != "" {
		netz := g.st.ShellyIP
		if i := strings.LastIndex(netz, "."); i > 0 {
			netz = netz[:i+1]
			if ip := sucheShelly(netz, strings.ToUpper(g.cfg.ShellyMAC)); ip != "" && ip != g.st.ShellyIP {
				s.sag("%s: Shelly jetzt unter %s", g.cfg.Name, ip)
				g.st.ShellyIP = ip
				kw, err = s.schalte(ip, an, totmann)
			}
		}
	}
	if err != nil {
		s.sag("%s: Shelly nicht erreichbar: %v", g.cfg.Name, err)
		if g.shellyFehler.IsZero() {
			g.shellyFehler = time.Now()
		}
		return 0, false
	}
	g.shellyFehler = time.Time{}
	g.letzteKW = kw
	return kw, true
}

// veraltet: keine frische Messung. Laeuft etwas, sofort aus.
func (s *steuerung) veraltet(t time.Time) {
	s.Lock()
	defer s.Unlock()
	for _, g := range s.geraete {
		if g.st.An && !g.st.LetzteZeit.IsZero() && t.Sub(g.st.LetzteZeit) > 3*time.Minute {
			s.setze(g, t, false, "keine Messung vom Wechselrichter")
		}
	}
	s.sichern()
}

var shellyClient = &http.Client{Timeout: 4 * time.Second}

func shellySchalte(ip string, an bool, totmann int) (float64, error) {
	u := fmt.Sprintf("http://%s/rpc/Switch.Set?id=0&on=false", ip)
	if an {
		u = fmt.Sprintf("http://%s/rpc/Switch.Set?id=0&on=true&toggle_after=%d", ip, totmann)
	}
	r, err := shellyClient.Get(u)
	if err != nil {
		return 0, err
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		return 0, fmt.Errorf("Antwort %d", r.StatusCode)
	}
	r, err = shellyClient.Get(fmt.Sprintf("http://%s/rpc/Switch.GetStatus?id=0", ip))
	if err != nil {
		return 0, nil
	}
	defer r.Body.Close()
	var st struct {
		Apower float64 `json:"apower"`
	}
	json.NewDecoder(r.Body).Decode(&st)
	return st.Apower / 1000, nil
}

// sucheShelly fragt das ganze Heimnetz nach dem Geraet mit dieser MAC. Auf
// Android gibt es kein resolv.conf, ein Name wie shellyplugmg3-...fritz.box
// laesst sich aus einem reinen Linux-Binary also nicht aufloesen.
func sucheShelly(netz, mac string) string {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	treffer := make(chan string, 1)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 48)
	for i := 1; i < 255; i++ {
		ip := fmt.Sprintf("%s%d", netz, i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			conn, err := net.DialTimeout("tcp", ip+":80", 700*time.Millisecond)
			if err != nil {
				return
			}
			conn.Close()
			r, err := c.Get("http://" + ip + "/shelly")
			if err != nil {
				return
			}
			defer r.Body.Close()
			var st struct {
				Mac string `json:"mac"`
			}
			if json.NewDecoder(r.Body).Decode(&st) == nil && strings.ToUpper(st.Mac) == mac {
				select {
				case treffer <- ip:
				default:
				}
			}
		}()
	}
	wg.Wait()
	select {
	case ip := <-treffer:
		return ip
	default:
		return ""
	}
}
