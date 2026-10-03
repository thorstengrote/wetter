// wandsolar liest die Huawei-Solaranlage ueber Modbus TCP und liefert die
// Wetterseite samt Messwerten auf 127.0.0.1 aus.
//
// Warum ein eigenes Binary und kein Skript: eine Shell-Fassung braucht je
// Messzyklus rund fuenfzig Prozesse und haelt drei Viertel der Zeit wartende
// Pipelines offen. Hier ist es ein Prozess, eine Verbindung zum Dongle und
// ein Server ohne Prozess je Anfrage. Gebaut wie hapwatch: ein einzelnes
// statisches Binary ohne Laufzeitabhaengigkeiten.
//
// Warum die Seite lokal ausgeliefert wird: Firefox verweigert einem
// HTTPS-Dokument den Abruf eines HTTP-Ziels, auch auf 127.0.0.1. Die
// Ausnahme fuer localhost, die es in der Norm gibt, greift in Fenix nicht.
// Am 25.09.2026 gemessen: von einer HTTP-Seite kam die Anfrage an, von der
// Kiosk-Seite ueber HTTPS ueber zwei Abfragezyklen keine einzige. Liegt die
// Seite lokal, ist alles derselbe Ursprung. 127.0.0.1 gilt zugleich als
// sicherer Kontext, Vollbild und alles andere funktionieren also weiter.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	// Zeitzone fest einbauen. Auf Android findet ein reines Linux-Binary
	// sonst keine Zoneninfo und rechnet in UTC, womit die Stundenwerte um
	// zwei Stunden gegen die Wirklichkeit verrutschen.
	_ "time/tzdata"
)

var ort = time.UTC

// ---------------------------------------------------------------- Modbus

// Der SDongle spricht Modbus TCP, der Wechselrichter hat die Adresse 1.
// Zwei Eigenheiten, beide gemessen und beide teuer gelernt:
//
//   - Nach dem Verbindungsaufbau braucht er rund eine Sekunde Ruhe. Frueher
//     gestellte Anfragen beantwortet er gar nicht.
//   - Er verarbeitet nur eine Anfrage gleichzeitig. Schickt man mehrere
//     hintereinander, ohne die Antwort abzuwarten, kommt Ausnahme 6,
//     "Geraet beschaeftigt".
const (
	einheit      = 1
	ruheNachAuf  = 1100 * time.Millisecond
	pauseAnfrage = 20 * time.Millisecond
	frist        = 8 * time.Second
)

type dongle struct {
	adresse string
	verb    net.Conn
	tid     uint16
}

func (d *dongle) schliesse() {
	if d.verb != nil {
		d.verb.Close()
		d.verb = nil
	}
}

func (d *dongle) verbinde() error {
	d.schliesse()
	v, err := net.DialTimeout("tcp", d.adresse, frist)
	if err != nil {
		return err
	}
	d.verb = v
	time.Sleep(ruheNachAuf)
	return nil
}

// lies holt einen zusammenhaengenden Block Halteregister.
func (d *dongle) lies(start, anzahl uint16) ([]uint16, error) {
	if d.verb == nil {
		if err := d.verbinde(); err != nil {
			return nil, err
		}
	}
	d.tid++
	anfrage := make([]byte, 12)
	binary.BigEndian.PutUint16(anfrage[0:], d.tid)
	binary.BigEndian.PutUint16(anfrage[4:], 6)
	anfrage[6] = einheit
	anfrage[7] = 3
	binary.BigEndian.PutUint16(anfrage[8:], start)
	binary.BigEndian.PutUint16(anfrage[10:], anzahl)

	d.verb.SetDeadline(time.Now().Add(frist))
	if _, err := d.verb.Write(anfrage); err != nil {
		d.schliesse()
		return nil, err
	}

	kopf := make([]byte, 9)
	if _, err := io.ReadFull(d.verb, kopf); err != nil {
		d.schliesse()
		return nil, err
	}
	if kopf[7]&0x80 != 0 {
		// Nach einer Ausnahme ist die Verbindung aus dem Takt, die
		// Folgeantworten kaemen verkuerzt an. Also neu aufbauen.
		d.schliesse()
		return nil, fmt.Errorf("Modbus-Ausnahme %d", kopf[8])
	}
	nutz := make([]byte, int(kopf[8]))
	if _, err := io.ReadFull(d.verb, nutz); err != nil {
		d.schliesse()
		return nil, err
	}
	if len(nutz) != int(anzahl)*2 {
		d.schliesse()
		return nil, fmt.Errorf("erwartet %d Bytes, bekommen %d", anzahl*2, len(nutz))
	}
	aus := make([]uint16, anzahl)
	for i := range aus {
		aus[i] = binary.BigEndian.Uint16(nutz[i*2:])
	}
	time.Sleep(pauseAnfrage)
	return aus, nil
}

// Hilfen, um aus einem Block einzelne Werte zu ziehen.
func i32(b []uint16, start, adr uint16) int32 {
	i := adr - start
	return int32(uint32(b[i])<<16 | uint32(b[i+1]))
}
func u32(b []uint16, start, adr uint16) uint32 {
	i := adr - start
	return uint32(b[i])<<16 | uint32(b[i+1])
}

// ---------------------------------------------------------------- Messung

// Alle gebrauchten Register liegen in vier Bloecken. Einzeln gelesen waeren
// es acht Verbindungen, so sind es vier Anfragen ueber eine.
//
//	32064 PV-Eingang (DC)   32080 Ausgang (AC)   32106 gesamt   32114 heute
//	37015 geladen heute     37017 entladen heute   37113 Zaehler
//	37760 Ladezustand       37765 Akkuleistung, positiv = laden
//
// Drei Anfragen reichen: 37015 bis 37114 sind 100 Register und damit noch
// innerhalb der erlaubten 125 je Anfrage. Jede Anfrage kostet am Dongle
// rund 0,7 Sekunden, das Zusammenlegen lohnt sich also.
type messwert struct {
	Zeit  time.Time
	PV    float64 // kW, Erzeugung
	Haus  float64 // kW, Hausverbrauch
	Akku  float64 // kW, positiv = laden
	Netz  float64 // kW, positiv = Einspeisung
	SOC   float64 // Prozent
	Heute float64 // kWh
}

func (d *dongle) messe() (messwert, error) {
	var m messwert

	a, err := d.lies(32064, 52) // 32064 bis 32115
	if err != nil {
		return m, err
	}
	b, err := d.lies(37015, 100) // 37015 bis 37114
	if err != nil {
		return m, err
	}
	e, err := d.lies(37760, 7)
	if err != nil {
		return m, err
	}

	pv := float64(i32(a, 32064, 32064)) / 1000
	aus := float64(i32(a, 32064, 32080)) / 1000
	ertrag := float64(u32(a, 32064, 32114)) / 100
	geladen := float64(u32(b, 37015, 37015)) / 100
	entladen := float64(u32(b, 37015, 37017)) / 100
	netz := float64(i32(b, 37015, 37113)) / 1000
	soc := float64(e[0]) / 10
	akku := float64(i32(e, 37760, 37765)) / 1000

	m = messwert{
		Zeit: time.Now().In(ort),
		PV:   pv,
		// Der Hausverbrauch ist das, was den Wechselrichter verlaesst, plus
		// was zusaetzlich aus dem Netz kommt. Ueber die Solarleistung laesst
		// er sich nicht rechnen, weil abends der Akku speist.
		Haus: aus - netz,
		Akku: akku,
		Netz: netz,
		SOC:  soc,
		// 32114 zaehlt nur den Ausgang. Was im Akku liegt, fehlt darin,
		// deshalb der Zuschlag. Gegengeprueft gegen die FusionSolar-App.
		Heute: ertrag + geladen - entladen,
	}
	return m, nil
}

// ---------------------------------------------------------------- Stunden

// Je Stunde die Summe der Proben, damit die Seite die Vergangenheit als
// Messung zeichnen kann und nicht als Hochrechnung. Auf Platte, damit ein
// Neustart den Tag nicht verliert.
type stunde struct {
	PV, Haus, Akku, Netz float64
	N                    int
}

type tagesspeicher struct {
	Tag     string            `json:"tag"`
	Stunden map[string]stunde `json:"stunden"`
}

type zustand struct {
	sync.Mutex
	letzte  *messwert
	tag     tagesspeicher
	pfad    string
	gesamt  float64
	fehler  string
	seitPfd string
	velux   *url.URL // Steuerdienst in hapwatch, nil heisst abgeschaltet
	st      *steuerung
	fr      *fritz
}

// laden holt den gespeicherten Tag zurueck, und zwar ohne Blick auf die Uhr.
//
// Nach einem Neustart laeuft das Tablet die erste Minute mit einer falschen
// Zeit, bis das Netz sie stellt. Am 26.09.2026 um 03:00 kam es mit dem 23.05.
// hoch. Der alte Vergleich mit heute() hat den gespeicherten Tag deshalb fuer
// veraltet gehalten und verworfen, und die Stunden 0 bis 2 waren weg. Ob der
// Tag wirklich vorbei ist, entscheidet jetzt die erste Messung in nimm(), mit
// ihrem eigenen Datum.
func (z *zustand) laden() {
	roh, err := os.ReadFile(z.pfad)
	if err == nil {
		var t tagesspeicher
		if json.Unmarshal(roh, &t) == nil && t.Tag != "" && t.Stunden != nil {
			z.tag = t
		}
	}
	if z.tag.Stunden == nil {
		z.tag = tagesspeicher{Tag: heute(), Stunden: map[string]stunde{}}
	}
}

func (z *zustand) sichern() {
	roh, err := json.Marshal(z.tag)
	if err != nil {
		return
	}
	tmp := z.pfad + ".neu"
	if os.WriteFile(tmp, roh, 0644) == nil {
		os.Rename(tmp, z.pfad)
	}
}

// stundenPV gibt die gemessenen Stundenmittel der Erzeugung von heute.
func (z *zustand) stundenPV() map[int]float64 {
	z.Lock()
	defer z.Unlock()
	m := map[int]float64{}
	if z.tag.Tag != heute() {
		return m
	}
	for k, s := range z.tag.Stunden {
		if h, err := strconv.Atoi(k); err == nil && s.N > 0 {
			m[h] = s.PV / float64(s.N)
		}
	}
	return m
}

func heute() string { return time.Now().In(ort).Format("2006-01-02") }

func (z *zustand) nimm(m messwert) {
	z.Lock()
	defer z.Unlock()
	// Das Datum der Messung entscheidet. Liegt es vor dem gespeicherten Tag,
	// ist die Uhr noch nicht gestellt: die Leistung stimmt, aber die Stunde
	// waere falsch, also wird sie angezeigt und nicht verbucht. Liegt es
	// danach, hat ein neuer Tag begonnen. Das Datumsformat laesst sich als
	// Zeichenkette vergleichen.
	tag := m.Zeit.Format("2006-01-02")
	if tag < z.tag.Tag {
		z.letzte = &m
		z.fehler = ""
		return
	}
	if tag > z.tag.Tag {
		z.tag = tagesspeicher{Tag: tag, Stunden: map[string]stunde{}}
	}
	k := fmt.Sprintf("%d", m.Zeit.Hour())
	s := z.tag.Stunden[k]
	s.PV += m.PV
	s.Haus += m.Haus
	s.Akku += m.Akku
	s.Netz += m.Netz
	s.N++
	z.tag.Stunden[k] = s
	z.letzte = &m
	z.fehler = ""
	z.sichern()
}

// ---------------------------------------------------------------- Ausgabe

type ausgabe struct {
	Zeit     string                        `json:"zeit"`
	Jetzt    map[string]float64            `json:"jetzt"`
	HeuteKwh float64                       `json:"heute_kwh"`
	Stunden  map[string]map[string]float64 `json:"stunden"`
	Hinweise []string                      `json:"hinweise,omitempty"`
}

func rund(v float64, stellen int) float64 {
	p := 1.0
	for i := 0; i < stellen; i++ {
		p *= 10
	}
	return float64(int64(v*p+copysign(0.5, v))) / p
}
func copysign(v, z float64) float64 {
	if z < 0 {
		return -v
	}
	return v
}

func (z *zustand) json(hinweise []string) []byte {
	z.Lock()
	defer z.Unlock()
	if z.letzte == nil {
		return []byte(`{}`)
	}
	m := *z.letzte
	a := ausgabe{
		Zeit: m.Zeit.Format("2006-01-02T15:04:05"),
		Jetzt: map[string]float64{
			"pv": rund(m.PV, 3), "haus": rund(m.Haus, 3),
			"akku": rund(m.Akku, 3), "netz": rund(m.Netz, 3),
			"soc": rund(m.SOC, 1),
		},
		HeuteKwh: rund(m.Heute, 2),
		Stunden:  map[string]map[string]float64{},
		Hinweise: hinweise,
	}
	for k, s := range z.tag.Stunden {
		if s.N == 0 {
			continue
		}
		n := float64(s.N)
		a.Stunden[k] = map[string]float64{
			"pv": rund(s.PV/n, 3), "haus": rund(s.Haus/n, 3),
			"akku": rund(s.Akku/n, 3), "netz": rund(s.Netz/n, 3),
		}
	}
	roh, _ := json.Marshal(a)
	return roh
}

// ---------------------------------------------------------------- Server

const wartet = `<!doctype html><meta charset=utf-8><meta http-equiv=refresh content=15>
<body style="background:#0d131b;color:#8b97a6;font:20px -apple-system,sans-serif;
display:flex;align-items:center;justify-content:center;height:90vh">Wetterseite fehlt noch</body>`

func (z *zustand) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/solar.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		// Hinweise vor dem eigenen Schloss holen: die Steuerung fragt
		// ihrerseits zustand ab, andersherum gaebe es eine Verklemmung.
		var h []string
		if z.st != nil {
			h = z.st.hinweise()
		}
		if z.fr != nil {
			h = append(h, z.fr.hinweise()...)
		}
		w.Write(z.json(h))
	})
	// Die Velux-Steuerung laeuft als eigener Dienst in hapwatch auf 8098.
	// Sie hier durchzureichen kostet nichts und spart der Seite den zweiten
	// Ursprung: ein Abruf auf einen anderen Port waere fuer den Browser eine
	// fremde Herkunft, mit Vorabfrage und CORS. So ist alles dieselbe Seite.
	if z.velux != nil {
		p := httputil.NewSingleHostReverseProxy(z.velux)
		p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(503)
			io.WriteString(w, `{"fehler":"hapwatch antwortet nicht"}`)
		}
		mux.Handle("/velux/", p)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(z.seitPfd)
		if err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, wartet)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			http.Error(w, "kaputt", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Ohne diese Zeile haelt Firefox die Seite nach eigener Schaetzung
		// fuer frisch und zeigt nach dem Ausrollen die alte Fassung, so am
		// 02.10.2026 nach einem Neustart des Browsers. no-cache heisst: vor
		// jeder Nutzung nachfragen. Unveraendert kostet das nur eine 304.
		w.Header().Set("Cache-Control", "no-cache")
		// ServeContent setzt Last-Modified, damit die Seite ihren eigenen
		// Stand anzeigen kann, und beantwortet bedingte Anfragen.
		http.ServeContent(w, r, "index.html", st.ModTime(), f)
	})
}

// ---------------------------------------------------------------- Start

func main() {
	var (
		wr    = flag.String("wr", "192.168.2.136:502", "Adresse des SDongle")
		hoere = flag.String("hoere", "127.0.0.1:8099", "wo der Dienst lauscht")
		seite = flag.String("seite", "/data/local/tmp/wand/index.html", "die Wetterseite")
		daten = flag.String("daten", "/data/local/tmp/wand/tag.json", "Stundenwerte")
		takt  = flag.Duration("takt", 30*time.Second, "Abstand zwischen zwei Messungen")
		prot  = flag.String("log", "", "Protokolldatei, leer heisst Standardfehler")
		vlx   = flag.String("velux", "http://127.0.0.1:8098", "Steuerdienst in hapwatch, leer schaltet ihn ab")
		einm  = flag.Bool("einmal", false, "einmal messen und beenden")
		lan   = flag.String("lan", ":8090", "Zugang aus dem WLAN fuer die Steuerung, leer schaltet ihn ab")
	)
	flag.Parse()

	// Ein reines Linux-Binary sucht die Zertifikate der Zertifizierungsstellen
	// unter /etc/ssl und findet auf Android nichts. Ohne diese Zeile scheitert
	// jeder HTTPS-Abruf, zuerst gesehen bei der SwitchBot-Cloud am 03.10.2026.
	if os.Getenv("SSL_CERT_DIR") == "" {
		if _, err := os.Stat("/system/etc/security/cacerts"); err == nil {
			os.Setenv("SSL_CERT_DIR", "/system/etc/security/cacerts")
		}
	}

	if l, err := time.LoadLocation("Europe/Berlin"); err == nil {
		ort = l
	}
	if *prot != "" {
		os.MkdirAll(filepath.Dir(*prot), 0755)
		if f, err := os.OpenFile(*prot, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			log.SetOutput(f)
		}
	}
	log.SetFlags(0)
	sag := func(f string, a ...any) {
		log.Printf("%s %s", time.Now().In(ort).Format("01-02 15:04"), fmt.Sprintf(f, a...))
	}

	d := &dongle{adresse: *wr}

	if *einm {
		m, err := d.messe()
		if err != nil {
			fmt.Println("Fehler:", err)
			os.Exit(1)
		}
		fmt.Printf("PV %.3f kW  Haus %.3f kW  Netz %+.3f kW  Akku %.0f %%  heute %.2f kWh\n",
			m.PV, m.Haus, m.Netz, m.SOC, m.Heute)
		return
	}

	os.MkdirAll(filepath.Dir(*daten), 0755)
	z := &zustand{pfad: *daten, seitPfd: *seite}
	if *vlx != "" {
		if u, err := url.Parse(*vlx); err == nil {
			z.velux = u
		} else {
			sag("Velux-Adresse %q unbrauchbar: %v", *vlx, err)
		}
	}
	z.laden()
	dir := filepath.Dir(*seite)
	z.st = neueSteuerung(filepath.Join(dir, "steuerung.json"), filepath.Join(dir, "steuerung-stand.json"), sag)
	z.st.stundenPV = z.stundenPV
	// Uebergang vom 03.10.2026: vorher schaltete die Datei entfeuchter.scharf.
	if _, err := os.Stat(filepath.Join(dir, "steuerung.json")); err != nil {
		if _, err := os.Stat("/data/local/tmp/entfeuchter.scharf"); err == nil {
			for _, g := range z.st.geraete {
				g.cfg.Modus, g.modusAlt = "scharf", "scharf"
			}
		}
		z.st.sichereCfg()
	}
	zu := neuerZugang(dir)
	z.st.sb = neuerSwitchbot(filepath.Join(dir, "switchbot.json"), sag)
	z.st.veluxBasis = *vlx
	go z.st.sb.laufe(z.st.sensorIDs)
	z.fr = neueFritz(filepath.Join(dir, "fritz.json"), filepath.Join(dir, "heizung-verlauf.json"), sag)
	go z.fr.laufe()

	mux := http.NewServeMux()
	z.bediene(mux)
	z.st.bediene(mux, filepath.Dir(*seite))
	mux.HandleFunc("/api/heizung", func(w http.ResponseWriter, r *http.Request) {
		a := z.fr.stand()
		a["aussen"] = z.st.aussenJetzt()
		jsonAntwort(w, a)
	})
	mux.HandleFunc("/api/heizung/verlauf", func(w http.ResponseWriter, r *http.Request) {
		h, _ := strconv.Atoi(r.URL.Query().Get("h"))
		if h <= 0 || h > 168 {
			h = 48
		}
		jsonAntwort(w, z.fr.kurve(r.URL.Query().Get("ain"), h))
	})
	srv := &http.Server{
		Addr:         *hoere,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	go func() {
		sag("wandsolar hoert auf %s", *hoere)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			sag("Server beendet: %v", err)
			os.Exit(1)
		}
	}()

	if *lan != "" {
		mux.HandleFunc("/api/anmelden", zu.anmelden)
		lanSrv := &http.Server{Addr: *lan, Handler: zu.schuetze(mux),
			ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
		go func() {
			sag("Steuerung im WLAN auf %s", *lan)
			if err := lanSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				sag("WLAN-Zugang beendet: %v", err)
			}
		}()
	}

	schluss := make(chan os.Signal, 1)
	signal.Notify(schluss, syscall.SIGINT, syscall.SIGTERM)

	fehler := 0
	uhr := time.NewTicker(*takt)
	defer uhr.Stop()

	messen := func() {
		m, err := d.messe()
		if err != nil {
			fehler++
			if fehler == 1 || fehler%40 == 0 {
				sag("keine Antwort vom Wechselrichter (%d): %v", fehler, err)
			}
			z.st.veraltet(time.Now().In(ort))
			return
		}
		if fehler > 0 {
			sag("wieder Antwort nach %d Fehlversuchen", fehler)
			fehler = 0
		}
		z.nimm(m)
		z.st.pruefe(m)
	}
	messen()

	for {
		select {
		case <-uhr.C:
			messen()
		case <-schluss:
			sag("wandsolar endet")
			d.schliesse()
			return
		}
	}
}
