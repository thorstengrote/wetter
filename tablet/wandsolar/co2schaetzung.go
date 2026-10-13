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
)

type co2Punkt struct {
	Zeit    time.Time `json:"zeit"`
	Schaetz float64   `json:"schaetzung"`
	Echt    float64   `json:"echt,omitempty"` // 0 ohne Sensor
	Fenster string    `json:"fenster"`
	Schlaf  bool      `json:"schlaf,omitempty"`
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
// Schlaf gelten fuer die ganze Spanne, der Takt ist kurz genug.
func (c *co2Schaetzung) rechne(t time.Time, fenster string, schlaf func(time.Time) bool, echt float64) {
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
		q := 0.0
		if schlaf(c.st.Zeit) {
			q = co2Quelle
		}
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
	c.Lock()
	defer c.Unlock()
	if t.Before(c.st.Zeit) {
		c.st = co2Stand{}
	}
	c.rechne(t, f, c.schlaf, e)
	if n := len(c.st.Verlauf); n == 0 || t.Sub(c.st.Verlauf[n-1].Zeit) >= co2Takt-10*time.Second {
		c.st.Verlauf = append(c.st.Verlauf, co2Punkt{Zeit: t, Schaetz: math.Round(c.st.Wert), Echt: e,
			Fenster: f, Schlaf: c.schlaf(t)})
		grenze := t.Add(-co2Behalten)
		for len(c.st.Verlauf) > 0 && c.st.Verlauf[0].Zeit.Before(grenze) {
			c.st.Verlauf = c.st.Verlauf[1:]
		}
		c.berichte(t)
		schreibeJSON(c.pfad, c.st)
	}
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
