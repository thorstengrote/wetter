package main

// Der Luftentfeuchter haengt am Shelly Plug M Gen3 und soll nur von
// Ueberschuss laufen. Regeln, so von Thorsten am 03.10.2026 vorgegeben:
//
//   - werktags 11 bis 18 Uhr, Samstag und Sonntag 13 bis 18 Uhr
//   - hoechstens 30 Stunden je Woche, Montag 0 Uhr beginnt eine neue
//
// Ueberschuss heisst hier Einspeisung ins Netz. Solange der Hausakku Platz
// hat, nimmt er den Strom und es wird nichts eingespeist; der Entfeuchter
// soll ihm nichts wegnehmen.
//
// Eingeschaltet wird, wenn fuenf Minuten lang mehr eingespeist wird, als der
// Entfeuchter braucht. Aus geht er, wenn er drei Minuten lang Strom aus dem
// Netz oder aus dem Akku zieht. Zwischen Ein und Aus liegen mindestens zehn
// Minuten, damit der Kompressor nicht im Takt schaltet. Fenster zu, Woche
// voll oder Messung veraltet schalten sofort ab.
//
// Damit der Entfeuchter nicht weiterlaeuft, wenn das Tablet ausfaellt, wird
// jedes Einschalten mit toggle_after an den Shelly geschickt: er schaltet
// sich nach 15 Minuten selbst ab, wenn wandsolar ihn nicht vorher erneut
// einschaltet. Das passiert bei jeder Messung.
//
// Scharf ist die Regelung nur, wenn die Datei /data/local/tmp/entfeuchter.scharf
// existiert. Ohne sie laeuft ein Probebetrieb: er rechnet alles durch,
// protokolliert, was er taete, und spricht den Shelly nicht an. Die Datei
// gehoert dem Nutzer shell und laesst sich ohne root anlegen und loeschen.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	efWoche      = 30 * time.Hour
	efEinNach    = 5 * time.Minute
	efAusNach    = 3 * time.Minute
	efMinAn      = 10 * time.Minute
	efMinAus     = 10 * time.Minute
	efRest       = 15 * time.Minute // weniger Wochenrest lohnt kein Einschalten
	efReserve    = 0.10             // kW Abstand zur Einspeisung beim Einschalten
	efBezug      = 0.10             // kW Bezug oder Akkuentladung, die als Mangel zaehlt
	efTotmann    = 900              // Sekunden, nach denen der Shelly selbst abschaltet
	efMaxSchritt = 2 * time.Minute  // laengere Luecken zaehlen nicht als Laufzeit
	efVeraltet   = 3 * time.Minute
)

func efFenster(t time.Time) bool {
	h := t.Hour()
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return h >= 13 && h < 18
	default:
		return h >= 11 && h < 18
	}
}

func efWochenKey(t time.Time) string {
	j, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", j, w)
}

// Was auf Platte liegt. Probe und scharf zaehlen getrennt, damit der
// Probebetrieb keine echte Laufzeit verbraucht.
type efStand struct {
	Woche      string    `json:"woche"`
	Sekunden   float64   `json:"sekunden"`
	ProbeSek   float64   `json:"probe_sekunden"`
	An         bool      `json:"an"`
	Seit       time.Time `json:"seit"`
	Leistung   float64   `json:"leistung_kw"`
	ShellyIP   string    `json:"shelly_ip"`
	LetzteZeit time.Time `json:"letzte_zeit"`
}

type entfeuchter struct {
	sync.Mutex
	pfad       string
	scharfPfd  string
	mac        string
	netz       string // z. B. "192.168.2."
	sag        func(string, ...any)
	schalte    func(ip string, an bool) (float64, error) // Rueckgabe: Leistung in kW
	st         efStand
	ueberSeit  time.Time
	mangelSeit time.Time
	grund      string
	scharf     bool
}

func neuerEntfeuchter(pfad, scharfPfd, ip, mac string, leistung float64, sag func(string, ...any)) *entfeuchter {
	e := &entfeuchter{pfad: pfad, scharfPfd: scharfPfd, mac: strings.ToUpper(mac), sag: sag}
	if i := strings.LastIndex(ip, "."); i > 0 {
		e.netz = ip[:i+1]
	}
	e.schalte = e.shellySchalte
	if roh, err := os.ReadFile(pfad); err == nil {
		json.Unmarshal(roh, &e.st)
	}
	if e.st.ShellyIP == "" {
		e.st.ShellyIP = ip
	}
	if e.st.Leistung <= 0 {
		e.st.Leistung = leistung
	}
	// Ohne diese Zeile saehe der erste Durchlauf nach jedem Neustart einen
	// Wechsel zu scharf und wuerde einen laufenden Entfeuchter abschalten.
	e.scharf = e.istScharf()
	return e
}

func (e *entfeuchter) sichern() {
	roh, err := json.Marshal(e.st)
	if err != nil {
		return
	}
	tmp := e.pfad + ".neu"
	if os.WriteFile(tmp, roh, 0644) == nil {
		os.Rename(tmp, e.pfad)
	}
}

func (e *entfeuchter) istScharf() bool {
	_, err := os.Stat(e.scharfPfd)
	return err == nil
}

// pruefe wird nach jeder Messung aufgerufen.
func (e *entfeuchter) pruefe(m messwert) {
	e.Lock()
	defer e.Unlock()
	t := m.Zeit

	// Nach einem Neustart laeuft die Uhr die erste Minute falsch (siehe
	// laden in main.go). Eine Zeit vor der letzten bekannten wird ignoriert.
	if !e.st.LetzteZeit.IsZero() && t.Before(e.st.LetzteZeit) {
		return
	}

	scharf := e.istScharf()
	if scharf != e.scharf {
		if scharf {
			e.sag("Entfeuchter: scharf geschaltet")
		} else {
			e.sag("Entfeuchter: Probebetrieb")
		}
		// Wechsel zwischen Probe und scharf beginnt aus, damit eine
		// gedachte Laufzeit nicht als echte weiterlaeuft.
		if e.st.An {
			e.st.An = false
			e.st.Seit = t
		}
		e.scharf = scharf
		if scharf {
			e.setze(false, "frisch scharf geschaltet")
		}
	}

	if w := efWochenKey(t); w != e.st.Woche {
		e.st.Woche, e.st.Sekunden, e.st.ProbeSek = w, 0, 0
	}

	// Laufzeit seit der letzten Messung verbuchen.
	if e.st.An && !e.st.LetzteZeit.IsZero() {
		dt := t.Sub(e.st.LetzteZeit)
		if dt > efMaxSchritt {
			dt = efMaxSchritt
		}
		if e.scharf {
			e.st.Sekunden += dt.Seconds()
		} else {
			e.st.ProbeSek += dt.Seconds()
		}
	}
	e.st.LetzteZeit = t
	genutzt := time.Duration(e.st.Sekunden * float64(time.Second))
	if !e.scharf {
		genutzt = time.Duration(e.st.ProbeSek * float64(time.Second))
	}

	// Ueberschuss und Mangel mit Dauer.
	ueber := m.Netz >= e.st.Leistung+efReserve
	mangel := m.Netz < -efBezug || m.Akku < -efBezug
	if ueber {
		if e.ueberSeit.IsZero() {
			e.ueberSeit = t
		}
	} else {
		e.ueberSeit = time.Time{}
	}
	if mangel {
		if e.mangelSeit.IsZero() {
			e.mangelSeit = t
		}
	} else {
		e.mangelSeit = time.Time{}
	}

	dauer := t.Sub(e.st.Seit)
	switch {
	case e.st.An && !efFenster(t):
		e.setze(false, "Zeitfenster vorbei")
	case e.st.An && genutzt >= efWoche:
		e.setze(false, "30 Stunden der Woche verbraucht")
	case e.st.An && !e.mangelSeit.IsZero() && t.Sub(e.mangelSeit) >= efAusNach && dauer >= efMinAn:
		e.setze(false, fmt.Sprintf("kein Ueberschuss mehr (Netz %+.2f kW, Akku %+.2f kW)", m.Netz, m.Akku))
	case e.st.An:
		e.setze(true, "") // Totmann erneuern
		e.grund = "laeuft"
	case !efFenster(t):
		e.grund = "ausserhalb des Zeitfensters"
	case efWoche-genutzt < efRest:
		e.grund = "Wochenbudget aufgebraucht"
	case e.ueberSeit.IsZero():
		e.grund = fmt.Sprintf("zu wenig Ueberschuss (Netz %+.2f kW, gebraucht %.2f)", m.Netz, e.st.Leistung+efReserve)
	case t.Sub(e.ueberSeit) < efEinNach:
		e.grund = "Ueberschuss, wartet auf fuenf Minuten"
	case !e.st.Seit.IsZero() && dauer < efMinAus:
		e.grund = "Pause nach dem Abschalten"
	default:
		e.setze(true, fmt.Sprintf("Ueberschuss %.2f kW seit %s", m.Netz, e.ueberSeit.Format("15:04")))
	}
	e.sichern()
}

// veraltet: keine frische Messung. Laeuft er, sofort aus.
func (e *entfeuchter) veraltet(jetzt time.Time) {
	e.Lock()
	defer e.Unlock()
	if e.st.An && !e.st.LetzteZeit.IsZero() && jetzt.Sub(e.st.LetzteZeit) > efVeraltet {
		e.setze(false, "keine Messung vom Wechselrichter")
		e.sichern()
	}
}

// setze schaltet, im Probebetrieb nur auf dem Papier. Ein leerer Grund heisst
// Erneuern ohne Protokollzeile.
func (e *entfeuchter) setze(an bool, grund string) {
	wechsel := an != e.st.An
	if wechsel {
		e.st.An = an
		e.st.Seit = e.st.LetzteZeit
		if e.st.Seit.IsZero() {
			e.st.Seit = time.Now().In(ort)
		}
	}
	if grund != "" {
		e.grund = grund
	}
	wort := map[bool]string{true: "ein", false: "aus"}[an]
	if !e.scharf {
		if wechsel {
			e.sag("Entfeuchter Probe: wuerde %sschalten, %s (Woche %.1f h)", wort, grund, e.st.ProbeSek/3600)
		}
		return
	}
	kw, err := e.schalte(e.st.ShellyIP, an)
	if err != nil && e.mac != "" && e.netz != "" {
		if ip := sucheShelly(e.netz, e.mac); ip != "" && ip != e.st.ShellyIP {
			e.sag("Entfeuchter: Shelly jetzt unter %s", ip)
			e.st.ShellyIP = ip
			kw, err = e.schalte(ip, an)
		}
	}
	if err != nil {
		e.sag("Entfeuchter: Shelly nicht erreichbar (%s): %v", wort, err)
		return
	}
	// Die tatsaechliche Leistung lernen, sobald er laeuft. Nicht direkt nach
	// dem Einschalten, da zieht der Kompressor noch nicht voll.
	if an && !wechsel && kw > 0.05 {
		e.st.Leistung = 0.8*e.st.Leistung + 0.2*kw
	}
	if wechsel {
		e.sag("Entfeuchter %s: %s (Woche %.1f h)", wort, grund, e.st.Sekunden/3600)
	}
}

var efClient = &http.Client{Timeout: 4 * time.Second}

func (e *entfeuchter) shellySchalte(ip string, an bool) (float64, error) {
	u := fmt.Sprintf("http://%s/rpc/Switch.Set?id=0&on=false", ip)
	if an {
		u = fmt.Sprintf("http://%s/rpc/Switch.Set?id=0&on=true&toggle_after=%d", ip, efTotmann)
	}
	r, err := efClient.Get(u)
	if err != nil {
		return 0, err
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		return 0, fmt.Errorf("Antwort %d", r.StatusCode)
	}
	r, err = efClient.Get(fmt.Sprintf("http://%s/rpc/Switch.GetStatus?id=0", ip))
	if err != nil {
		return 0, nil
	}
	defer r.Body.Close()
	var s struct {
		Apower float64 `json:"apower"`
	}
	json.NewDecoder(r.Body).Decode(&s)
	return s.Apower / 1000, nil
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
			if conn, err := net.DialTimeout("tcp", ip+":80", 700*time.Millisecond); err != nil {
				return
			} else {
				conn.Close()
			}
			r, err := c.Get("http://" + ip + "/shelly")
			if err != nil {
				return
			}
			defer r.Body.Close()
			var s struct {
				Mac string `json:"mac"`
			}
			if json.NewDecoder(r.Body).Decode(&s) == nil && strings.ToUpper(s.Mac) == mac {
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

// status fuer solar.json
func (e *entfeuchter) status() map[string]any {
	e.Lock()
	defer e.Unlock()
	h := e.st.Sekunden / 3600
	if !e.scharf {
		h = e.st.ProbeSek / 3600
	}
	return map[string]any{
		"scharf": e.scharf, "an": e.st.An, "woche_h": rund(h, 2),
		"leistung_kw": rund(e.st.Leistung, 3), "grund": e.grund,
	}
}
