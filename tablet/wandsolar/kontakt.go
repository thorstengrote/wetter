package main

// Fensterkontakte von SwitchBot (Contact Sensor), ueber denselben Hub und
// dieselbe Cloud wie die Feuchtefuehler. Der erste haengt seit 13.10.2026 am
// Fenster der Waschkueche, das von Hand geoeffnet wird.

import (
	"fmt"
	"time"
)

type kontaktWert struct {
	Offen bool      `json:"offen"`
	Seit  time.Time `json:"seit"` // seit wann in diesem Zustand, soweit gesehen
	Batt  int       `json:"batterie,omitempty"`
	Zeit  time.Time `json:"zeit"` // letzte Abfrage
}

func (b *switchbot) merkeKontakt(id string, offen bool, batt int) {
	jetzt := time.Now().In(ort)
	b.Lock()
	defer b.Unlock()
	b.fehler = ""
	k, alt := b.kontakte[id]
	if !alt || k.Offen != offen {
		if alt {
			wort := map[bool]string{true: "offen", false: "zu"}[offen]
			b.sag("Fensterkontakt %s: %s", id, wort)
		}
		k.Seit = jetzt
	}
	k.Offen, k.Batt, k.Zeit = offen, batt, jetzt
	b.kontakte[id] = k
}

// kontakt: letzter Stand, wenn juenger als 10 Minuten.
func (b *switchbot) kontakt(id string) (kontaktWert, bool) {
	b.Lock()
	defer b.Unlock()
	k, ok := b.kontakte[id]
	return k, ok && time.Since(k.Zeit) < 10*time.Minute
}

func (s *steuerung) kontaktIDs() []string {
	s.Lock()
	defer s.Unlock()
	var ids []string
	for _, g := range s.geraete {
		if g.cfg.KontaktID != "" {
			ids = append(ids, g.cfg.KontaktID)
		}
	}
	return ids
}

// kontaktHinweise: offenes Fenster, das nichts mehr bringt oder Waerme kostet.
func (s *steuerung) kontaktHinweise(g *geraet, jetzt time.Time) []string {
	if g.cfg.KontaktID == "" || s.sb == nil {
		return nil
	}
	k, ok := s.sb.kontakt(g.cfg.KontaktID)
	if !ok {
		return nil
	}
	var h []string
	raum := g.cfg.Raum()
	if k.Batt > 0 && k.Batt <= sensorBattGrenze {
		h = append(h, fmt.Sprintf("Fensterkontakt %s: Batterie wechseln (%d %%)", raum, k.Batt))
	}
	if !k.Offen || jetzt.Sub(k.Seit) < 30*time.Minute {
		return h
	}
	aus, ausOk := luftZu(s.luft, jetzt)
	if g.feuchte != nil && ausOk && aus.Taupunkt >= taupunkt(g.feuchte.Temp, g.feuchte.RH)-1 {
		return append(h, fmt.Sprintf("%s: Fenster zu, draußen ist die Luft feuchter (Taupunkt %.0f °C)", raum, aus.Taupunkt))
	}
	if ausOk && aus.Temp < 12 && jetzt.Sub(k.Seit) >= 2*time.Hour {
		return append(h, fmt.Sprintf("%s: Fenster offen seit %s, draußen %.0f °C", raum, k.Seit.In(ort).Format("15:04"), aus.Temp))
	}
	return h
}
