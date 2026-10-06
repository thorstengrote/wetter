package main

// Schnittstelle der Oberflaeche /steuerung.
//
// Das Tablet selbst (127.0.0.1) braucht keine PIN, wer davor steht, ist im
// Haus. Ueber das WLAN (Port 8090) gilt die PIN aus der Datei pin neben der
// Wetterseite. Die Datei liegt nur auf dem Tablet, nie im Repo. Fehlt sie,
// ist der Zugang ueber das WLAN gesperrt. Nach der Anmeldung bleibt ein
// Geraet 30 Tage angemeldet. Fuenf falsche Versuche sperren eine Minute.
//
// Geraete, deren MAC in der Datei freigabe steht (eine je Zeile), brauchen
// keine PIN. Die MAC zur Absenderadresse kommt aus der ARP-Tabelle des
// Tablets. Seit 03.10.2026 steht dort Thorstens MacBook.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type zugang struct {
	sync.Mutex
	pinPfad, pfad, freiPfad string
	sitzungen               map[string]time.Time
	fehler                  int
	gesperrtBis             time.Time
}

func neuerZugang(dir string) *zugang {
	z := &zugang{pinPfad: filepath.Join(dir, "pin"), pfad: filepath.Join(dir, "sitzungen.json"),
		freiPfad:  filepath.Join(dir, "freigabe"),
		sitzungen: map[string]time.Time{}}
	if roh, err := os.ReadFile(z.pfad); err == nil {
		json.Unmarshal(roh, &z.sitzungen)
	}
	return z
}

func vomTablet(r *http.Request) bool {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && (h == "127.0.0.1" || h == "::1")
}

// macVon sucht die MAC zu einer IP in der ARP-Tabelle.
var arpPfad = "/proc/net/arp"

func macVon(ip string) string {
	roh, err := os.ReadFile(arpPfad)
	if err != nil {
		return ""
	}
	for _, z := range strings.Split(string(roh), "\n")[1:] {
		f := strings.Fields(z)
		if len(f) >= 4 && f[0] == ip {
			return strings.ToLower(f[3])
		}
	}
	return ""
}

func (z *zugang) freigegeben(r *http.Request) bool {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	roh, err := os.ReadFile(z.freiPfad)
	if err != nil {
		return false
	}
	mac := macVon(h)
	if mac == "" {
		return false
	}
	for _, l := range strings.Split(string(roh), "\n") {
		if strings.ToLower(strings.TrimSpace(l)) == mac {
			return true
		}
	}
	return false
}

func (z *zugang) erlaubt(r *http.Request) bool {
	if vomTablet(r) || z.freigegeben(r) {
		return true
	}
	c, err := r.Cookie("sitzung")
	if err != nil {
		return false
	}
	z.Lock()
	defer z.Unlock()
	bis, ok := z.sitzungen[c.Value]
	return ok && time.Now().Before(bis)
}

func (z *zugang) anmelden(w http.ResponseWriter, r *http.Request) {
	var a struct {
		PIN string `json:"pin"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 256)).Decode(&a)
	z.Lock()
	defer z.Unlock()
	if time.Now().Before(z.gesperrtBis) {
		http.Error(w, "gesperrt", 429)
		return
	}
	roh, err := os.ReadFile(z.pinPfad)
	pin := strings.TrimSpace(string(roh))
	if err != nil || pin == "" || subtle.ConstantTimeCompare([]byte(pin), []byte(a.PIN)) != 1 {
		z.fehler++
		if z.fehler >= 5 {
			z.fehler = 0
			z.gesperrtBis = time.Now().Add(time.Minute)
		}
		http.Error(w, "falsch", 403)
		return
	}
	z.fehler = 0
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	jetzt := time.Now()
	for k, v := range z.sitzungen {
		if jetzt.After(v) {
			delete(z.sitzungen, k)
		}
	}
	z.sitzungen[tok] = jetzt.Add(30 * 24 * time.Hour)
	schreibeJSON(z.pfad, z.sitzungen)
	http.SetCookie(w, &http.Cookie{Name: "sitzung", Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 30 * 24 * 3600})
	w.WriteHeader(204)
}

// schuetze laesst nur Angemeldete durch. Die Oberflaeche und die Anmeldung
// selbst sind frei, sonst kaeme niemand zur PIN.
func (z *zugang) schuetze(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/steuerung" || r.URL.Path == "/api/anmelden" || z.erlaubt(r) {
			h.ServeHTTP(w, r)
			return
		}
		http.Error(w, "PIN noetig", 401)
	})
}

func jsonAntwort(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func leseZahl(pfad string) (int, bool) {
	roh, err := os.ReadFile(pfad)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(roh)))
	return v, err == nil
}

// Wandtablet: Akku aus sysfs (wandsolar laeuft als root), Helligkeit aus der
// Datei, die wandwacht liest.
const (
	hellTagPfad  = "/data/local/tmp/hell_tag"
	hellTagStand = 120
)

func tabletStatus() map[string]any {
	st := map[string]any{}
	if v, ok := leseZahl("/sys/class/power_supply/Battery/capacity"); ok {
		st["akku"] = v
	}
	if v, ok := leseZahl("/sys/class/power_supply/Battery/current_now"); ok {
		st["strom_ma"] = v
	}
	if v, ok := leseZahl(hellTagPfad); ok {
		st["hell_tag"] = v
	} else {
		st["hell_tag"] = hellTagStand
	}
	return st
}

func (s *steuerung) bediene(mux *http.ServeMux, seitenDir string) {
	mux.HandleFunc("/steuerung", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(seitenDir, "steuerung.html"))
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		s.Lock()
		defer s.Unlock()
		t := time.Now().In(ort)
		var gs []map[string]any
		for _, g := range s.geraete {
			n := g.naechster(t)
			e := map[string]any{"cfg": g.cfg, "an": g.st.An, "seit": g.st.Seit, "grund": g.grund,
				"leistung_kw": g.leistung(), "gemessen_kw": g.letzteKW,
				"minuten_7t": g.minuten7(t), "beginn": g.st.Beginn,
				"hand": g.st.Hand, "hand_bis": g.st.HandBis,
				"stoerung": g.st.Stoerung, "stoerung_seit": g.st.StoerSeit, "wiederholt": g.st.Wiederholt,
				"kompressor": g.komp, "kompressor_kw": g.st.KompKW,
				"feuchte": g.feuchte, "nass": g.nass, "trocken": g.trocken, "fenster_auf": g.fensterAuf}
			if !n.IsZero() {
				e["naechster"] = n
			}
			gs = append(gs, e)
		}
		a := map[string]any{"zeit": t, "geraete": gs, "tablet": tabletStatus(),
			"prognose_zeit": s.prognoseZeit, "prognose_stunden": len(s.prognose)}
		if s.letzte != nil {
			m := s.letzte
			a["jetzt"] = map[string]any{"zeit": m.Zeit, "pv": m.PV, "haus": m.Haus, "akku": m.Akku,
				"netz": m.Netz, "soc": m.SOC, "heute_kwh": m.Heute}
		}
		jsonAntwort(w, a)
	})

	finde := func(id string) *geraet {
		for _, g := range s.geraete {
			if g.cfg.ID == id {
				return g
			}
		}
		if len(s.geraete) > 0 && id == "" {
			return s.geraete[0]
		}
		return nil
	}

	mux.HandleFunc("/api/sensoren", func(w http.ResponseWriter, r *http.Request) {
		if s.sb == nil {
			jsonAntwort(w, []any{})
			return
		}
		l, err := s.sb.sensoren()
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		jsonAntwort(w, l)
	})

	mux.HandleFunc("/api/feuchte", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("sensor")
		if s.sb == nil || id == "" {
			jsonAntwort(w, []any{})
			return
		}
		jsonAntwort(w, s.sb.kurve(id))
	})

	mux.HandleFunc("/api/plan", func(w http.ResponseWriter, r *http.Request) {
		s.Lock()
		defer s.Unlock()
		g := finde(r.URL.Query().Get("id"))
		if g == nil {
			http.NotFound(w, r)
			return
		}
		jsonAntwort(w, map[string]any{"plan": g.plan, "leistung_kw": g.leistung()})
	})

	mux.HandleFunc("/api/verlauf", func(w http.ResponseWriter, r *http.Request) {
		s.Lock()
		defer s.Unlock()
		g := finde(r.URL.Query().Get("id"))
		if g == nil {
			http.NotFound(w, r)
			return
		}
		jsonAntwort(w, map[string]any{"laeufe": g.st.Laeufe, "leistung_kw": g.leistung()})
	})

	mux.HandleFunc("/api/einstellungen", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "nein", 405)
			return
		}
		var c geraetCfg
		if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&c); err != nil {
			http.Error(w, "unbrauchbar", 400)
			return
		}
		if msg := pruefeCfg(c); msg != "" {
			http.Error(w, msg, 400)
			return
		}
		s.Lock()
		defer s.Unlock()
		g := finde(c.ID)
		if g == nil {
			http.NotFound(w, r)
			return
		}
		if c.ShellyIP != g.cfg.ShellyIP {
			g.st.ShellyIP = c.ShellyIP
		}
		// Neue Leistung von Hand ersetzt den gelernten Wert, gelernt wird
		// danach von dort aus weiter.
		if c.LeistungKW != g.cfg.LeistungKW {
			g.st.LeistungKW = c.LeistungKW
		}
		if c.SensorID != "" && c.SensorID != g.cfg.SensorID && s.sb != nil {
			go s.sb.lies(c.SensorID) // nicht bis zum naechsten 5-Minuten-Abruf warten
		}
		g.cfg = c
		g.planZeit = time.Time{}
		s.sichereCfg()
		s.sag("%s: Einstellungen geaendert", c.Name)
		w.WriteHeader(204)
	})

	mux.HandleFunc("/api/hand", func(w http.ResponseWriter, r *http.Request) {
		var a struct {
			ID     string `json:"id"`
			Aktion string `json:"aktion"` // an1h, heuteaus, normal
		}
		if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 512)).Decode(&a) != nil {
			http.Error(w, "nein", 400)
			return
		}
		s.Lock()
		defer s.Unlock()
		g := finde(a.ID)
		if g == nil {
			http.NotFound(w, r)
			return
		}
		t := time.Now().In(ort)
		switch a.Aktion {
		case "an1h":
			g.st.Hand, g.st.HandBis = "an", t.Add(time.Hour)
		case "heuteaus":
			g.st.Hand, g.st.HandBis = "aus", tagesAnfang(t).AddDate(0, 0, 1)
		case "geleert":
			g.st.Stoerung, g.st.StoerSeit, g.st.Wiederholt = "", time.Time{}, time.Time{}
			g.st.Seit = time.Time{} // ohne Pause gleich wieder erlaubt
		default:
			g.st.Hand, g.st.HandBis = "", time.Time{}
		}
		g.planZeit = time.Time{}
		s.sag("%s: von Hand %s", g.cfg.Name, a.Aktion)
		if s.letzte != nil {
			m := *s.letzte
			m.Zeit = t
			s.pruefeGeraet(g, m, t)
		}
		s.sichern()
		w.WriteHeader(204)
	})

	mux.HandleFunc("/api/tablet", func(w http.ResponseWriter, r *http.Request) {
		var a struct {
			HellTag int `json:"hell_tag"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 256)).Decode(&a) != nil ||
			a.HellTag < 20 || a.HellTag > 255 {
			http.Error(w, "Helligkeit 20 bis 255", 400)
			return
		}
		os.WriteFile(hellTagPfad, []byte(strconv.Itoa(a.HellTag)+"\n"), 0666)
		s.sag("Wandtablet: Helligkeit tagsueber %d", a.HellTag)
		w.WriteHeader(204)
	})

	// Die Wetterseite schickt ihre Prognose: je Stunde Beginn (Unix) und kW.
	// Die alte Form {tag, pv[24]} wird weiter angenommen.
	mux.HandleFunc("/prognose", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "nein", 405)
			return
		}
		var a struct {
			Stunden [][2]float64 `json:"stunden"`
			Temp    [][2]float64 `json:"temp"` // Aussentemperatur je Stunde
			Luft    [][5]float64 `json:"luft"` // Beginn, Temperatur, Taupunkt, Regen mm, Boeen km/h
			Tag     string       `json:"tag"`
			PV      []float64    `json:"pv"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&a) != nil {
			http.Error(w, "unbrauchbar", 400)
			return
		}
		p := map[int64]float64{}
		for _, x := range a.Stunden {
			p[int64(x[0])] = x[1]
		}
		if len(p) == 0 && len(a.PV) == 24 {
			if d, err := time.ParseInLocation("2006-01-02", a.Tag, ort); err == nil {
				for h, v := range a.PV {
					p[d.Add(time.Duration(h)*time.Hour).Unix()] = v
				}
			}
		}
		if len(p) == 0 {
			http.Error(w, "leer", 400)
			return
		}
		if len(a.Temp) > 0 {
			tp := map[int64]float64{}
			for _, x := range a.Temp {
				tp[int64(x[0])] = x[1]
			}
			s.Lock()
			s.aussen = tp
			s.Unlock()
		}
		if len(a.Luft) > 0 {
			lp := map[int64]luftWert{}
			for _, x := range a.Luft {
				lp[int64(x[0])] = luftWert{Temp: x[1], Taupunkt: x[2], Regen: x[3], Boeen: x[4]}
			}
			s.Lock()
			s.luft = lp
			s.Unlock()
		}
		s.setzePrognose(p)
		w.WriteHeader(204)
	})
}

func pruefeCfg(c geraetCfg) string {
	switch {
	case c.ID == "":
		return "ID fehlt"
	case c.Modus != "aus" && c.Modus != "probe" && c.Modus != "scharf":
		return "Modus aus, probe oder scharf"
	case c.LeistungKW <= 0 || c.LeistungKW > 25:
		return "Leistung zwischen 0 und 25 kW"
	case c.MaxH7 < 0 || c.MaxH7 > 168 || c.MinH7 < 0 || c.MinH7 > c.MaxH7:
		return "Stunden je 7 Tage unplausibel"
	case c.MinAnMin < 0 || c.MinAusMin < 0 || c.MinLaufMin < 0 || c.MaxLueckeTage < 0:
		return "negative Zeiten"
	case net.ParseIP(c.ShellyIP) == nil:
		return "Shelly-Adresse ungueltig"
	}
	if c.SensorID != "" && (c.FeuchteUnten <= 0 || c.FeuchteOben > 100 || c.FeuchteUnten+5 > c.FeuchteOben) {
		return "Feuchtegrenzen: unten mindestens 5 Punkte unter oben"
	}
	for _, z := range c.Zeiten {
		if z.Von < 0 || z.Bis > 24 || z.Bis < z.Von {
			return "Zeitraum ungueltig"
		}
	}
	return ""
}
