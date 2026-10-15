package main

// CO2 im Spielekeller ohne Sensor schaetzen. Der Velux-Sensor ist nur
// geliehen und geht zurueck ins Wohnzimmer. Bis dahin laeuft die Schaetzung
// im Hintergrund mit, die Lueftung steuert weiter nach dem echten Wert, und
// jeden Morgen steht im Protokoll, wie weit beide in der Nacht auseinander
// lagen (Wunsch vom 12.10.2026).
//
// Modell: ein Raum, gut durchmischt. In der Schlafzeit gibt ein Schlaefer
// CO2 ab, die Fenster tauschen Luft mit draussen (420 ppm):
//
//   dC/dt = Quelle * schlaeft - Luftwechsel(Fenster) * (C - 420)
//
// Angepasst an sechs Naechte vom 07. bis 13.10.2026 (Daten und Rechnung im
// Repo wandtablet unter kalibrierung/): Quelle 200 ppm je Stunde, Luftwechsel
// zu 0,1, Spalt 0,2, ganz auf 0,7 je Stunde. Die Kellerfenster in ihren
// abgedeckten Schaechten tauschen auch ganz offen wenig Luft. Mittlere
// Abweichung 110 bis 280 ppm je Nacht, wenn die Nacht beim echten Wert
// beginnt. Frei laufend fehlt der Abend: wer vorher im Raum gespielt hat,
// bringt CO2 in die Nacht mit, das die Schaetzung nicht kennt. Der groesste
// Fehler ist die Schlafzeit: am 08.10. ging Ben erst gegen 1 Uhr ins Bett,
// die Schaetzung nimmt die Zeiten der Heizregel.
//
// Seit 14.10.2026 zaehlt die Quelle Personen statt Schlafzeiten: jedes Handy
// am Spielkeller-Repeater ist eine (anwesenheit.go). In der Nacht davor ging
// es erst nach Mitternacht los, und zu zweit: 500 ppm je Stunde statt 200.
// Kein Handy dort beweist nicht, dass niemand da ist. Dann entscheidet die
// Atemfeuchte (anwesenheit.go): steigt sie bei ruhendem Entfeuchter, ist
// eine Person da, und die Zeit bis zum Erkennen wird nachgeholt; steht sie
// still, niemand. Sagt auch die Feuchte nichts, gilt die Schlafzeit mit
// einer Person (Wunsch vom 14.10.2026).
//
// Die Kellerfenster melden ihre Stellung nur aus den Fahrbefehlen. In der
// Nacht zum 15.10.2026 galten sie nach einem Neustart als offen und waren zu:
// CO2 stieg auf 2.440 ppm, die Feuchte im selben Takt (Korrelation 0,96),
// obwohl die Luft draussen 2,6 g/m³ trockener war. Offen haette der Raum
// knapp 2 g/m³ je Stunde verlieren muessen. Darum prueft die Schaetzung die
// gemeldete Stellung an der Feuchte: steigt sie, wo sie bei offenem Fenster
// deutlich fallen muesste, rechnet das Modell mit zu und das Protokoll
// meldet es einmal (Wunsch vom 15.10.2026). Nachgerechnet haette das die
// Abweichung der Nacht von 924 auf 289 ppm gesenkt.
//
// Die Schaetzung laeuft frei, ohne sich am echten Wert festzuhalten. Nur so
// zeigt der Vergleich, wie sie ohne Sensor taugen wuerde.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	co2Aussen   = 420.0
	co2Quelle   = 200.0 // ppm je Stunde, ein Schlaefer
	co2LwZu     = 0.1   // Luftwechsel je Stunde
	co2LwSpalt  = 0.2
	co2LwAuf    = 0.7             // 13.10.: ganze Nacht offen und trotzdem um 1.000 ppm
	co2Takt     = 5 * time.Minute // Vergleichspunkte
	co2Behalten = 14 * 24 * time.Hour
	// Fensterpruefung: verliert der Raum bei der gemeldeten Stellung
	// mindestens co2FensterZweifel g/m³ je Stunde an die trockenere Luft
	// draussen, muesste die Feuchte ueber co2FensterSpanne deutlich fallen.
	// Ein Schlaefer bringt netto nur 0,1 bis 0,2 g/m³ je Stunde. Faellt sie
	// nicht (hoechstens co2FensterFaellt), ist das Fenster zu. Zwei Stunden,
	// weil der Sensor ganze Prozent liefert, 0,17 g/m³ bei 20 °C.
	co2FensterZweifel = 0.6
	co2FensterSpanne  = 2 * time.Hour
	co2FensterFaellt  = -0.1
)

type co2Punkt struct {
	Zeit    time.Time `json:"zeit"`
	Schaetz float64   `json:"schaetzung"`
	Echt    float64   `json:"echt,omitempty"` // 0 ohne Sensor
	Fenster string    `json:"fenster"`
	Schlaf  bool      `json:"schlaf,omitempty"`
	// Handys am Spielkeller-Repeater, fehlt ohne Mesh-Daten
	Personen *int   `json:"personen,omitempty"`
	Quelle   string `json:"quelle,omitempty"` // handy, feuchte, schlafzeit
	// Stellung, mit der das Modell rechnet, wenn die Feuchte der Meldung widerspricht
	FensterLuft string `json:"fenster_luft,omitempty"`
}

type co2Stand struct {
	Wert    float64    `json:"wert"`
	Zeit    time.Time  `json:"zeit"`
	Verlauf []co2Punkt `json:"verlauf"`
	Bericht time.Time  `json:"bericht"` // Ende der zuletzt berichteten Nacht
}

type co2Schaetzung struct {
	sync.Mutex
	pfad string
	sag  func(string, ...any)
	st   co2Stand

	// Verbindungen, in Tests ersetzt
	echt    func() float64         // echter Wert, 0 ohne
	fenster func() string          // zu, spalt, auf
	schlaf  func(t time.Time) bool // schlaeft jemand im Raum?
	// Personen im Raum, false ohne frische Daten. nil: nur Schlafzeit.
	personen func(t time.Time) (int, bool)
	// Raumluft und Entfeuchter, false ohne Messung. nil: keine Atemfeuchte.
	raum   func() (rh, temp float64, entf, ok bool)
	atem   atem
	atemDa bool // letzte Entscheidung der Feuchte, fuers Nachholen
	// Absolute Feuchte draussen in g/m³, false ohne Wetter. nil: keine Pruefung.
	// Nimmt das Schloss der Steuerung, darum vor dem eigenen holen.
	aussen      func(t time.Time) (float64, bool)
	fensterWar  string
	fensterSeit time.Time
	zweifel     bool // Feuchte widerspricht der gemeldeten Stellung
}

func neueCO2Schaetzung(pfad string, sag func(string, ...any)) *co2Schaetzung {
	c := &co2Schaetzung{pfad: pfad, sag: sag}
	if roh, err := os.ReadFile(pfad); err == nil {
		json.Unmarshal(roh, &c.st)
	}
	return c
}

func co2Luftwechsel(fenster string) float64 {
	switch fenster {
	case "auf":
		return co2LwAuf
	case "spalt":
		return co2LwSpalt
	}
	return co2LwZu
}

// kfFenster: Stellung der Kellerfenster fuer das Modell. "angehalten" ist
// der Spalt, so faehrt ihn die Lueftung.
func kfFenster(stellung string) string {
	switch stellung {
	case "offen", "fährt auf":
		return "auf"
	case "angehalten":
		return "spalt"
	}
	return "zu"
}

// rechne fuehrt die Schaetzung bis t fort, in Minutenschritten. Fenster und
// Quelle (ppm je Stunde) gelten fuer die ganze Spanne, der Takt ist kurz genug.
func (c *co2Schaetzung) rechne(t time.Time, fenster string, quelle func(time.Time) float64, echt float64) {
	if c.st.Zeit.IsZero() || t.Sub(c.st.Zeit) > 6*time.Hour {
		// Anfang oder lange Pause: mit dem echten Wert beginnen, sonst
		// mit dem, was nach Nacht oder Tag zu erwarten ist.
		c.st.Wert, c.st.Zeit = 600, t
		if echt > 0 {
			c.st.Wert = echt
		}
		return
	}
	lw := co2Luftwechsel(fenster)
	for c.st.Zeit.Before(t) {
		dt := math.Min(1, t.Sub(c.st.Zeit).Minutes())
		q := quelle(c.st.Zeit)
		c.st.Wert += (q - lw*(c.st.Wert-co2Aussen)) * dt / 60
		c.st.Zeit = c.st.Zeit.Add(time.Duration(dt * float64(time.Minute)))
	}
}

func (c *co2Schaetzung) schritt(t time.Time) {
	// Nach einem Neustart steht die Uhr die erste Minute im Jahr 2022.
	if t.Year() < 2026 {
		return
	}
	f, e := c.fenster(), c.echt()
	var pers *int
	if c.personen != nil {
		if n, ok := c.personen(t); ok {
			pers = &n
		}
	}
	// Raumluft vor dem eigenen Schloss holen, raum() nimmt das der Steuerung
	var rh, temp float64
	var entf, mess bool
	if c.raum != nil {
		rh, temp, entf, mess = c.raum()
	}
	aus, ausOk := 0.0, false
	if c.aussen != nil {
		aus, ausOk = c.aussen(t)
	}
	c.Lock()
	defer c.Unlock()
	if t.Before(c.st.Zeit) {
		c.st = co2Stand{}
	}
	if mess {
		c.atem.merke(t, rh, temp, entf)
	}
	if f != c.fensterWar {
		c.fensterWar, c.fensterSeit = f, t
	}
	fLuft := c.pruefeFenster(t, f, aus, ausOk)
	atemDa, atemSicher := c.atem.zeigt(t)
	art := "schlafzeit"
	switch {
	case pers != nil && *pers > 0:
		art = "handy"
	case atemSicher:
		art = "feuchte"
		if atemDa && !c.atemDa {
			c.st.Wert += co2Quelle * atemNachholen // seit dem Hinlegen schon vergangen
		}
		c.atemDa = atemDa
	}
	if art != "feuchte" {
		c.atemDa = art == "handy" || c.schlaf(t)
	}
	quelle := func(z time.Time) float64 {
		switch art {
		case "handy":
			return co2Quelle * float64(*pers)
		case "feuchte":
			if atemDa {
				return co2Quelle
			}
			return 0
		}
		if c.schlaf(z) {
			return co2Quelle
		}
		return 0
	}
	fModell := f
	if fLuft != "" {
		fModell = fLuft
	}
	c.rechne(t, fModell, quelle, e)
	if n := len(c.st.Verlauf); n == 0 || t.Sub(c.st.Verlauf[n-1].Zeit) >= co2Takt-10*time.Second {
		c.st.Verlauf = append(c.st.Verlauf, co2Punkt{Zeit: t, Schaetz: math.Round(c.st.Wert), Echt: e,
			Fenster: f, Schlaf: c.schlaf(t), Personen: pers, Quelle: art, FensterLuft: fLuft})
		grenze := t.Add(-co2Behalten)
		for len(c.st.Verlauf) > 0 && c.st.Verlauf[0].Zeit.Before(grenze) {
			c.st.Verlauf = c.st.Verlauf[1:]
		}
		c.berichte(t)
		schreibeJSON(c.pfad, c.st)
	}
}

// pruefeFenster: "zu", wenn die Feuchte nicht faellt, obwohl sie bei der
// gemeldeten Stellung deutlich fallen muesste, sonst "". Die Stellung muss
// schon die ganze Messspanne gelten, sonst stammt der Anstieg von vorher.
func (c *co2Schaetzung) pruefeFenster(t time.Time, f string, aus float64, ausOk bool) string {
	urteil, verlust := "", 0.0
	d, innen, ok := c.atem.anstieg(t, co2FensterSpanne)
	if f != "zu" && ausOk && ok && t.Sub(c.fensterSeit) >= co2FensterSpanne+15*time.Minute {
		verlust = co2Luftwechsel(f) * (innen - aus)
		if d >= co2FensterFaellt && verlust >= co2FensterZweifel {
			urteil = "zu"
		}
	}
	if urteil != "" && !c.zweifel {
		c.sag("Kellerfenster gemeldet %s, die Raumluft spricht für zu: Feuchte in %.0f Stunden %+.2f g/m³, offen müsste sie um %.1f g/m³ je Stunde fallen",
			map[string]string{"auf": "offen", "spalt": "im Spalt"}[f], co2FensterSpanne.Hours(), d, verlust)
	}
	c.zweifel = urteil != ""
	return urteil
}

// berichte: einmal am Vormittag, wenn die Schlafzeit vorbei ist, die Nacht
// zusammenfassen.
func (c *co2Schaetzung) berichte(t time.Time) {
	if c.schlaf(t) {
		return
	}
	v := nachtVergleich(c.st.Verlauf, t)
	// Jede Nacht nur einmal (12.10.: abends noch einmal dieselbe Nacht)
	if v.N < 12 || !v.Bis.After(c.st.Bericht) {
		return
	}
	c.st.Bericht = v.Bis
	c.sag("CO₂-Schätzung letzte Nacht: mittlere Abweichung %.0f ppm, höchster Wert gemessen %.0f, geschätzt %.0f",
		v.Abw, v.MaxEcht, v.MaxSchaetz)
}

type co2Vergleich struct {
	Von, Bis            time.Time
	N                   int
	Abw                 float64 `json:"abweichung"`
	MaxEcht, MaxSchaetz float64
	UeberEcht, UeberSch int // Punkte ueber 1000 ppm
}

// nachtVergleich: die letzte zusammenhaengende Schlafzeit vor t.
func nachtVergleich(v []co2Punkt, t time.Time) co2Vergleich {
	var r co2Vergleich
	ende := -1
	for i := len(v) - 1; i >= 0; i-- {
		if v[i].Schlaf && !v[i].Zeit.After(t) {
			ende = i
			break
		}
	}
	if ende < 0 {
		return r
	}
	start := ende
	for start > 0 && v[start-1].Schlaf && v[start].Zeit.Sub(v[start-1].Zeit) < time.Hour {
		start--
	}
	r.Von, r.Bis = v[start].Zeit, v[ende].Zeit
	sum := 0.0
	for _, p := range v[start : ende+1] {
		if p.Echt <= 0 {
			continue
		}
		r.N++
		sum += math.Abs(p.Schaetz - p.Echt)
		r.MaxEcht = math.Max(r.MaxEcht, p.Echt)
		r.MaxSchaetz = math.Max(r.MaxSchaetz, p.Schaetz)
		if p.Echt > 1000 {
			r.UeberEcht++
		}
		if p.Schaetz > 1000 {
			r.UeberSch++
		}
	}
	if r.N > 0 {
		r.Abw = sum / float64(r.N)
	}
	return r
}

func (c *co2Schaetzung) wert() float64 {
	c.Lock()
	defer c.Unlock()
	return c.st.Wert
}

func (c *co2Schaetzung) laufe() {
	for {
		c.schritt(time.Now().In(ort))
		time.Sleep(time.Minute)
	}
}

func (c *co2Schaetzung) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/api/co2schaetzung", func(w http.ResponseWriter, r *http.Request) {
		c.Lock()
		defer c.Unlock()
		jetzt := time.Now().In(ort)
		jsonAntwort(w, map[string]any{"wert": math.Round(c.st.Wert), "zeit": c.st.Zeit,
			"verlauf": c.st.Verlauf, "letzte_nacht": nachtVergleich(c.st.Verlauf, jetzt),
			"modell": fmt.Sprintf("Quelle %.0f ppm/h, Luftwechsel zu %.2f, Spalt %.2f, auf %.1f je Stunde",
				co2Quelle, co2LwZu, co2LwSpalt, co2LwAuf)})
	})
}
