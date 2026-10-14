package main

// Wer ist im Spielkeller? Ohne Bewegungsmelder: die FRITZ!Box kennt ueber
// ihr Mesh, an welchem Repeater jedes Handy haengt (Idee vom 14.10.2026).
// Der Spielkeller-Repeater ist der, an dem der Shelly des Entfeuchters
// haengt; so stimmt es auch, wenn ein Repeater getauscht wird. Jedes Handy
// dort zaehlt als eine Person, Gaeste im Gastnetz eingeschlossen (Bens
// Freundin). Die CO2-Schaetzung nimmt das statt fester Schlafzeiten.
//
// Regeln von Thorsten (14.10.2026): ein Handy zaehlt erst, wenn es ein paar
// Minuten dort ist, sonst ist es nur eine Besorgung im Keller (anwBleiben).
// Kein Handy dort beweist nicht, dass niemand im Raum ist; dann gilt in der
// Schaetzung weiter die Schlafzeit. Kurze Aussetzer eines schlafenden
// Handys ueberbrueckt anwHalten.
//
// Abfrage per TR-064 (Port 49000, Digest-Anmeldung), Aktion
// X_AVM-DE_GetMeshListPath. Der Benutzer des Tablets braucht dafuer das
// Recht "FRITZ!Box Einstellungen".

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	anwTakt     = 2 * time.Minute
	anwFrisch   = 10 * time.Minute // aelter: unbekannt
	anwBleiben  = 10 * time.Minute // so lange dort, dann zaehlt es
	anwHalten   = 10 * time.Minute // so lange nach dem letzten Sehen
	anwBehalten = 14 * 24 * time.Hour
)

var anwHandy = regexp.MustCompile(`(?i)iphone|android|galaxy|pixel|handy|phone|redmi|xiaomi`)

type anwGeraet struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
	IP   string `json:"ip,omitempty"`
	Gast bool   `json:"gast,omitempty"`
	RCPI int    `json:"rcpi,omitempty"` // Empfang am Repeater, dBm
}

type anwSicht struct {
	Seit, Zuletzt time.Time
}

type anwPunkt struct {
	Zeit     time.Time `json:"zeit"`
	Personen int       `json:"personen"`
	MACs     []string  `json:"macs,omitempty"`
}

type anwesenheit struct {
	sync.Mutex
	pfad    string
	fr      *fritz
	shelly  func() string // IP des Shelly im Spielkeller
	sag     func(string, ...any)
	client  *http.Client
	zeit    time.Time
	handys  []anwGeraet
	gesehen map[string]*anwSicht // je MAC
	fehler  string
	verlauf []anwPunkt
}

func neueAnwesenheit(pfad string, fr *fritz, sag func(string, ...any)) *anwesenheit {
	a := &anwesenheit{pfad: pfad, fr: fr, sag: sag, client: &http.Client{Timeout: 20 * time.Second}}
	if roh, err := os.ReadFile(pfad); err == nil {
		json.Unmarshal(roh, &a.verlauf)
	}
	return a
}

// personen im Spielkeller; false, wenn die letzte Abfrage zu alt ist.
func (a *anwesenheit) personen(t time.Time) (int, bool) {
	a.Lock()
	defer a.Unlock()
	if a.zeit.IsZero() || t.Sub(a.zeit) > anwFrisch {
		return 0, false
	}
	return a.zaehle(t), true
}

func (a *anwesenheit) zaehle(t time.Time) int {
	n := 0
	for _, g := range a.gesehen {
		if g.Zuletzt.Sub(g.Seit) >= anwBleiben && t.Sub(g.Zuletzt) <= anwHalten {
			n++
		}
	}
	return n
}

// --- TR-064 mit Digest-Anmeldung (RFC 2617, qop auth) ---

func digestTeile(h string) map[string]string {
	m := map[string]string{}
	h = strings.TrimPrefix(h, "Digest ")
	for _, t := range regexp.MustCompile(`(\w+)=("[^"]*"|[^,\s]*)`).FindAllStringSubmatch(h, -1) {
		m[t[1]] = strings.Trim(t[2], `"`)
	}
	return m
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func (a *anwesenheit) soap(z fritzZugang, pfad, dienst, aktion string) (string, error) {
	basis, err := url.Parse(z.Adresse)
	if err != nil {
		return "", err
	}
	ziel := "http://" + basis.Hostname() + ":49000" + pfad
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:` +
		aktion + ` xmlns:u="` + dienst + `"/></s:Body></s:Envelope>`
	frage := func(auth string) (*http.Response, error) {
		r, _ := http.NewRequest("POST", ziel, strings.NewReader(body))
		r.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		r.Header.Set("SoapAction", dienst+"#"+aktion)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		return a.client.Do(r)
	}
	r, err := frage("")
	if err != nil {
		return "", err
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode == http.StatusUnauthorized {
		d := digestTeile(r.Header.Get("WWW-Authenticate"))
		cb := make([]byte, 8)
		rand.Read(cb)
		cnonce := hex.EncodeToString(cb)
		ha1 := md5hex(z.Benutzer + ":" + d["realm"] + ":" + z.Passwort)
		ha2 := md5hex("POST:" + pfad)
		antw := md5hex(ha1 + ":" + d["nonce"] + ":00000001:" + cnonce + ":auth:" + ha2)
		auth := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=auth, nc=00000001, cnonce="%s", response="%s"`,
			z.Benutzer, d["realm"], d["nonce"], pfad, cnonce, antw)
		if r, err = frage(auth); err != nil {
			return "", err
		}
	}
	defer r.Body.Close()
	roh, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		return "", err
	}
	if r.StatusCode != http.StatusOK {
		if m := regexp.MustCompile(`<errorDescription>(.*?)<`).FindStringSubmatch(string(roh)); m != nil {
			return "", fmt.Errorf("%s", m[1])
		}
		return "", fmt.Errorf("HTTP %d", r.StatusCode)
	}
	return string(roh), nil
}

// --- Mesh auswerten ---

type meshListe struct {
	Nodes []struct {
		UID        string                   `json:"uid"`
		Name       string                   `json:"device_name"`
		MAC        string                   `json:"device_mac_address"`
		Rolle      string                   `json:"mesh_role"`
		Adressen   []struct{ Value string } `json:"ip_addresses"`
		Interfaces []struct {
			Links []struct {
				State string `json:"state"`
				N1    string `json:"node_1_uid"`
				N2    string `json:"node_2_uid"`
				RCPI  int    `json:"rx_rcpi"`
			} `json:"node_links"`
		} `json:"node_interfaces"`
	} `json:"nodes"`
}

// handysAm: Handys, die per WLAN an demselben Zugangspunkt haengen wie das
// Geraet mit der IP anker.
func handysAm(m meshListe, anker string) ([]anwGeraet, error) {
	ip := func(i int) string {
		for _, a := range m.Nodes[i].Adressen {
			if v, _, _ := strings.Cut(a.Value, "/"); v != "" && strings.Contains(v, ".") {
				return v
			}
		}
		return ""
	}
	ap := map[string]bool{}
	for _, n := range m.Nodes {
		if n.Rolle == "master" || n.Rolle == "slave" {
			ap[n.UID] = true
		}
	}
	// Zugangspunkt eines Endgeraets
	zugang := func(i int) (string, int) {
		n := m.Nodes[i]
		for _, f := range n.Interfaces {
			for _, l := range f.Links {
				if l.State != "CONNECTED" {
					continue
				}
				o := l.N1
				if o == n.UID {
					o = l.N2
				}
				if ap[o] {
					return o, l.RCPI
				}
			}
		}
		return "", 0
	}
	ziel := ""
	for i := range m.Nodes {
		if ip(i) == anker {
			ziel, _ = zugang(i)
		}
	}
	if ziel == "" {
		return nil, fmt.Errorf("Shelly %s nicht im Mesh", anker)
	}
	var r []anwGeraet
	for i, n := range m.Nodes {
		if ap[n.UID] || !anwHandy.MatchString(n.Name) {
			continue
		}
		if z, rcpi := zugang(i); z == ziel {
			g := anwGeraet{Name: n.Name, MAC: n.MAC, IP: ip(i), RCPI: rcpi}
			g.Gast = strings.HasPrefix(g.IP, "192.168.179.")
			r = append(r, g)
		}
	}
	return r, nil
}

func (a *anwesenheit) lies() error {
	z, err := a.fr.zugang()
	if err != nil {
		return err
	}
	x, err := a.soap(z, "/upnp/control/hosts", "urn:dslforum-org:service:Hosts:1", "X_AVM-DE_GetMeshListPath")
	if err != nil {
		return err
	}
	m := regexp.MustCompile(`<NewX_AVM-DE_MeshListPath>(.*?)<`).FindStringSubmatch(x)
	if m == nil {
		return fmt.Errorf("kein Mesh-Pfad")
	}
	basis, _ := url.Parse(z.Adresse)
	r, err := a.client.Get("http://" + basis.Hostname() + ":49000" + m[1])
	if err != nil {
		return err
	}
	defer r.Body.Close()
	var liste meshListe
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&liste); err != nil {
		return err
	}
	h, err := handysAm(liste, a.shelly())
	if err != nil {
		return err
	}
	t := time.Now()
	a.Lock()
	defer a.Unlock()
	a.merke(h, t)
	p := anwPunkt{Zeit: t, Personen: a.zaehle(t)}
	for _, g := range h {
		p.MACs = append(p.MACs, g.MAC)
	}
	a.verlauf = append(a.verlauf, p)
	for len(a.verlauf) > 0 && t.Sub(a.verlauf[0].Zeit) > anwBehalten {
		a.verlauf = a.verlauf[1:]
	}
	schreibeJSON(a.pfad, a.verlauf)
	return nil
}

// merke: Handys einer Abfrage eintragen. Wer laenger als anwHalten weg
// war, faengt neu an zu zaehlen.
func (a *anwesenheit) merke(h []anwGeraet, t time.Time) {
	if a.gesehen == nil {
		a.gesehen = map[string]*anwSicht{}
	}
	a.handys, a.zeit, a.fehler = h, t, ""
	for _, g := range h {
		s := a.gesehen[g.MAC]
		if s == nil || t.Sub(s.Zuletzt) > anwHalten {
			s = &anwSicht{Seit: t}
			a.gesehen[g.MAC] = s
		}
		s.Zuletzt = t
	}
	for m, s := range a.gesehen {
		if t.Sub(s.Zuletzt) > anwHalten {
			delete(a.gesehen, m)
		}
	}
}

func (a *anwesenheit) laufe() {
	time.Sleep(90 * time.Second)
	for {
		if err := a.lies(); err != nil {
			a.Lock()
			neu := a.fehler != err.Error()
			a.fehler = err.Error()
			a.Unlock()
			if neu {
				a.sag("Anwesenheit Spielkeller: %v", err)
			}
		}
		time.Sleep(anwTakt)
	}
}

func (a *anwesenheit) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/api/anwesenheit", func(w http.ResponseWriter, r *http.Request) {
		a.Lock()
		defer a.Unlock()
		jsonAntwort(w, map[string]any{"zeit": a.zeit, "handys": a.handys, "gesehen": a.gesehen, "personen": a.zaehle(time.Now()), "fehler": a.fehler, "verlauf": a.verlauf})
	})
}

// --- Atemfeuchte: der zweite Hinweis, wenn kein Handy dort ist ---
//
// Ein Schlaefer atmet rund 40 g Wasser je Stunde aus. In der Nacht zum
// 14.10. stieg die Raumluft ab Mitternacht von 59 auf 64 %, genau mit dem
// CO2, davor stand sie still. Der Sensor liefert ganze Prozent, 1 % sind bei
// 20 °C 0,17 g/m³; ein Anstieg ist nach etwa einer Stunde sicher.
//
// Der Entfeuchter verfaelscht das: solange er laeuft, faellt die Feuchte,
// und danach steigt sie wieder, weil Waende und Moebel nachliefern. Darum
// zaehlt die Feuchte erst, wenn er seit atemEntfPause aus ist.

const (
	atemFenster   = 75 * time.Minute // Anstieg ueber diese Spanne
	atemEntfPause = 2 * time.Hour    // so lange Entfeuchter aus
	atemSteigt    = 0.15             // g/m³ ueber atemFenster: jemand da
	atemStill     = 0.05             // darunter: niemand
	atemBehalten  = 3 * time.Hour
	atemNachholen = 0.75 // Stunden Quelle, die beim Erkennen schon vorbei sind
)

type atemPunkt struct {
	Zeit time.Time
	Abs  float64 // g/m³
	Entf bool
}

type atem struct {
	punkte []atemPunkt
}

func (a *atem) merke(t time.Time, rh, temp float64, entf bool) {
	a.punkte = append(a.punkte, atemPunkt{t, absFeuchte(temp, taupunkt(temp, rh)), entf})
	for len(a.punkte) > 0 && t.Sub(a.punkte[0].Zeit) > atemBehalten {
		a.punkte = a.punkte[1:]
	}
}

// zeigt: jemand da? sicher=false, wenn die Feuchte nichts sagt (zu wenig
// Werte, Entfeuchter lief, Anstieg zwischen den Schwellen).
func (a *atem) zeigt(t time.Time) (da, sicher bool) {
	if len(a.punkte) == 0 || t.Sub(a.punkte[0].Zeit) < atemFenster+15*time.Minute {
		return false, false
	}
	var alt, neu []float64
	for _, p := range a.punkte {
		if p.Entf && t.Sub(p.Zeit) <= atemEntfPause {
			return false, false
		}
		switch d := t.Sub(p.Zeit); {
		case d <= 15*time.Minute:
			neu = append(neu, p.Abs)
		case d >= atemFenster && d <= atemFenster+15*time.Minute:
			alt = append(alt, p.Abs)
		}
	}
	if len(alt) == 0 || len(neu) == 0 {
		return false, false
	}
	mittel := func(v []float64) float64 {
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s / float64(len(v))
	}
	switch d := mittel(neu) - mittel(alt); {
	case d >= atemSteigt:
		return true, true
	case d <= atemStill:
		return false, true
	}
	return false, false
}
