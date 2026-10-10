package main

// Was kostet ein Liter Wasser aus der Kellerluft, gelueftet oder entfeuchtet?
// (Vorgabe vom 10.10.2026: Lueften kann teuer sein, wenn der Raum danach
// wieder aufgeheizt werden muss.)
//
// Standardmodell fuer ein modernisiertes Haus aus den 1970ern mit
// Hybridheizung, Gas und Waermepumpe. Echte Preise kennt niemand im Haus,
// deshalb Richtwerte:
//
//   - Gas 11 ct/kWh bei 92 % Nutzungsgrad, also rund 12 ct je kWh Waerme
//   - Waermepumpe mit Heizkoerpern, Jahresmittel um 3, kalt weniger; Strom
//     aus Einspeisung 7,82 ct, vom Netz 24,55 ct. Die Hybridregelung nimmt
//     das Guenstigere.
//   - Heizgrenze 14 °C im Tagesmittel draussen, darueber heizt niemand
//   - Spielekeller 46 m³, zwei Kellerfenster ganz auf tauschen etwa 60 m³
//     in der Stunde, im Spalt etwa 20. Auf die Luftwaerme kommen 30 % fuer
//     Waende und Moebel, die beim Lueften mit auskuehlen.
//   - Entfeuchter: 12 l am Tag bei 30 °C und 80 %, in kuehler Kellerluft
//     entsprechend weniger. Seinen Strom gibt er ganz als Waerme an den
//     Raum ab, dazu die Kondensationswaerme, 0,68 kWh je Liter. In der
//     Heizzeit spart das Heizwaerme.
//
// Ausserhalb der Heizzeit kostet Lueften nichts.

import (
	"math"
	"time"
)

const (
	preisEinspeisung = 7.82  // ct/kWh, entgangene Verguetung
	preisNetz        = 24.55 // ct/kWh
	preisGasWaerme   = 12.0  // ct je kWh Waerme
	heizgrenze       = 14.0  // °C Tagesmittel draussen
	luftstromVoll    = 60.0  // m³/h
	luftstromSpalt   = 20.0
	speicherZuschlag = 1.3
	entfeuchterLpH   = 0.5 // Liter je Stunde bei 30 °C und 80 %
	kondensWaerme    = 0.68
)

// absFeuchte in g/m³ aus Temperatur und Taupunkt.
func absFeuchte(temp, tp float64) float64 {
	e := 6.112 * math.Exp(17.62*tp/(243.12+tp))
	return 216.7 * e / (temp + 273.15)
}

func copWP(aussen float64) float64 {
	return math.Max(2, math.Min(4.5, 2.6+0.06*aussen))
}

// waermePreis in ct je kWh Waerme, die Hybridheizung nimmt das Guenstigere.
func waermePreis(aussen float64, einspeisung bool) float64 {
	strom := preisNetz
	if einspeisung {
		strom = preisEinspeisung
	}
	return math.Min(preisGasWaerme, strom/copWP(aussen))
}

// heizzeit: Tagesmittel draussen, 12 Stunden zurueck und voraus, so weit die
// Vorhersage reicht, unter der Heizgrenze.
func heizzeit(luft map[int64]luftWert, t time.Time) bool {
	s, n := 0.0, 0
	for h := -12; h <= 12; h++ {
		if w, ok := luft[t.Truncate(time.Hour).Add(time.Duration(h)*time.Hour).Unix()]; ok {
			s += w.Temp
			n++
		}
	}
	return n >= 6 && s/float64(n) < heizgrenze
}

type feuchteKosten struct {
	Heizzeit   bool    `json:"heizzeit"`
	Lueften    float64 `json:"lueften_ct_l"`     // ct je Liter
	Entfeucht  float64 `json:"entfeuchter_ct_l"` // ct je Liter, negativ heisst: spart Heizung
	WaermeCt   float64 `json:"waerme_ct_kwh"`
	LueftenLpH float64 `json:"lueften_l_h"`
}

// kostenJeLiter fuer die jetzige Stunde. entfStrom ist der Preis, zu dem der
// Entfeuchter statt der Lueftung laufen wuerde.
func kostenJeLiter(innen messFeuchte, tpIn float64, aus luftWert, heiz, einspeisung bool, entfKW, entfStrom float64) feuchteKosten {
	k := feuchteKosten{Heizzeit: heiz}
	k.LueftenLpH = luftstromVoll * (absFeuchte(innen.Temp, tpIn) - absFeuchte(aus.Temp, aus.Taupunkt)) / 1000
	if !heiz {
		return k
	}
	k.WaermeCt = waermePreis(aus.Temp, einspeisung)
	kwh := luftstromVoll * 0.34 * math.Max(0, innen.Temp-aus.Temp) / 1000 * speicherZuschlag
	if k.LueftenLpH > 0.005 {
		k.Lueften = kwh * k.WaermeCt / k.LueftenLpH
	} else {
		k.Lueften = math.Inf(1)
	}
	lph := entfeuchterLpH * math.Max(0.05, math.Min(1, (tpIn-3)/23))
	k.Entfeucht = (entfKW*entfStrom - (entfKW+kondensWaerme*lph)*k.WaermeCt) / lph
	return k
}
