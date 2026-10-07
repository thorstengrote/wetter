package main

// Tuya-Geraete aus der Smart-Life-App (seit 07.10.2026): zwei
// Rollladenschalter im Buero, die Steckdosen Vorgarten Beleuchtung und Pool
// Filter. Gesprochen wird direkt im Heimnetz, Protokoll 3.3 auf TCP 6668,
// AES-128-ECB mit dem lokalen Schluessel jedes Geraets. Die Schluessel kamen
// einmal aus der Tuya-Cloud und liegen nur auf dem Tablet in tuya.json. Wird
// ein Geraet in der App neu gekoppelt, aendert sich sein Schluessel, dann
// muss er neu geholt werden (wandtablet/tuya/tuya.py).
//
// Die Adressen im Heimnetz findet der Dienst selbst: Tuya-Geraete rufen alle
// paar Sekunden per UDP auf Port 6667 ihre ID aus, verschluesselt mit einem
// festen, bei Tuya bekannten Schluessel.
//
// Datenpunkte: Rollladen dp 1 control (lokal on, stop, off), dp 3 Fahrzeit;
// Steckdose dp 1 switch_1, dp 9 countdown_1. Die Rollladenschalter melden
// keine Position, nur den letzten Befehl.

import (
	"bytes"
	"crypto/aes"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

type tuyaGeraet struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	Key  string `json:"key"`
	Art  string `json:"art"` // clkg Rollladen, cz Steckdose
	// Lueftungsstellung (gestoppt am 07.10.2026 am rechten Rollladen):
	// von ganz offen 24 s runter, von ganz zu 6 s hoch.
	VonOben  float64 `json:"lueftung_von_oben,omitempty"`
	VonUnten float64 `json:"lueftung_von_unten,omitempty"`
}

func (g tuyaGeraet) lueftungsZeiten() (oben, unten time.Duration) {
	o, u := g.VonOben, g.VonUnten
	if o <= 0 {
		o = 24
	}
	if u <= 0 {
		u = 6
	}
	return time.Duration(o * float64(time.Second)), time.Duration(u * float64(time.Second))
}

type tuyaStand struct {
	IP         string         `json:"ip,omitempty"`
	Gesehen    time.Time      `json:"gesehen,omitempty"` // letzter Rundruf
	DPS        map[string]any `json:"dps,omitempty"`
	Abgefragt  time.Time      `json:"abgefragt,omitempty"`
	Fehler     string         `json:"fehler,omitempty"`
	Befehl     string         `json:"befehl,omitempty"` // Rollladen: letzter Befehl, auch "lueften"
	BefehlZeit time.Time      `json:"befehl_zeit,omitempty"`
	Folge      int            `json:"-"` // jeder Befehl von aussen bricht eine laufende Lueftungsfahrt ab
}

type tuya struct {
	sync.Mutex
	pfad    string
	sag     func(string, ...any)
	geraete []tuyaGeraet
	stand   map[string]*tuyaStand
	seq     uint32
}

func neuesTuya(pfad string, sag func(string, ...any)) *tuya {
	t := &tuya{pfad: pfad, sag: sag, stand: map[string]*tuyaStand{}}
	if roh, err := os.ReadFile(pfad); err == nil {
		json.Unmarshal(roh, &t.geraete)
	}
	for _, g := range t.geraete {
		t.stand[g.ID] = &tuyaStand{}
	}
	return t
}

// --- Protokoll 3.3 -------------------------------------------------------

func pkcs7(b []byte) []byte {
	n := 16 - len(b)%16
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}

func aesECB(key, b []byte, ver bool) ([]byte, error) {
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(b)%16 != 0 {
		return nil, errors.New("Laenge kein Vielfaches von 16")
	}
	aus := make([]byte, len(b))
	for i := 0; i < len(b); i += 16 {
		if ver {
			c.Encrypt(aus[i:i+16], b[i:i+16])
		} else {
			c.Decrypt(aus[i:i+16], b[i:i+16])
		}
	}
	return aus, nil
}

func entpacke(key, b []byte) ([]byte, error) {
	k, err := aesECB(key, b, false)
	if err != nil {
		return nil, err
	}
	if n := len(k); n > 0 && int(k[n-1]) <= 16 && int(k[n-1]) <= n {
		k = k[:n-int(k[n-1])]
	}
	return k, nil
}

// rahmen baut ein Paket: Praefix, Folge, Befehl, Laenge, Nutzlast, CRC, Suffix.
func rahmen(seq, cmd uint32, nutz []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(0x000055AA))
	binary.Write(&b, binary.BigEndian, seq)
	binary.Write(&b, binary.BigEndian, cmd)
	binary.Write(&b, binary.BigEndian, uint32(len(nutz)+8))
	b.Write(nutz)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
	binary.Write(&b, binary.BigEndian, uint32(0x0000AA55))
	return b.Bytes()
}

// leseRahmen liest ein Antwortpaket und gibt Befehl und Nutzlast ohne
// Rueckgabecode zurueck.
func leseRahmen(r io.Reader) (uint32, []byte, error) {
	kopf := make([]byte, 16)
	if _, err := io.ReadFull(r, kopf); err != nil {
		return 0, nil, err
	}
	if binary.BigEndian.Uint32(kopf[0:]) != 0x000055AA {
		return 0, nil, errors.New("falsches Praefix")
	}
	cmd := binary.BigEndian.Uint32(kopf[8:])
	n := binary.BigEndian.Uint32(kopf[12:])
	if n < 8 || n > 8192 {
		return 0, nil, fmt.Errorf("unsinnige Laenge %d", n)
	}
	rest := make([]byte, n)
	if _, err := io.ReadFull(r, rest); err != nil {
		return 0, nil, err
	}
	nutz := rest[:n-8]
	if len(nutz) >= 4 {
		nutz = nutz[4:] // Rueckgabecode
	}
	return cmd, nutz, nil
}

const (
	tuyaSteuern   = 7
	tuyaAbfrage   = 10
	tuyaAbfrage22 = 13 // Geraete mit 22-stelliger ID verstehen 10 nicht
)

// anfrage schickt einen Befehl und liest die Antwort mit Datenpunkten.
func (t *tuya) anfrage(g tuyaGeraet, ip string, cmd uint32, dps map[string]any) (map[string]any, error) {
	key := []byte(g.Key)
	jetzt := strconv.FormatInt(time.Now().Unix(), 10)
	var nutz []byte
	if cmd == tuyaAbfrage && len(g.ID) == 22 {
		// Abfrage als Steuerbefehl mit leeren Datenpunkten, wie es diese
		// Geraete erwarten. Die Antwort enthaelt dann den Stand.
		cmd, dps = tuyaAbfrage22, map[string]any{"1": nil, "2": nil, "3": nil}
	}
	if cmd == tuyaSteuern || cmd == tuyaAbfrage22 {
		roh, _ := json.Marshal(map[string]any{"devId": g.ID, "uid": g.ID, "t": jetzt, "dps": dps})
		v, err := aesECB(key, pkcs7(roh), true)
		if err != nil {
			return nil, err
		}
		nutz = append(append([]byte("3.3"), make([]byte, 12)...), v...)
	} else {
		roh, _ := json.Marshal(map[string]any{"gwId": g.ID, "devId": g.ID, "uid": g.ID, "t": jetzt})
		v, err := aesECB(key, pkcs7(roh), true)
		if err != nil {
			return nil, err
		}
		nutz = v
	}
	t.Lock()
	t.seq++
	seq := t.seq
	t.Unlock()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "6668"), 4*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(6 * time.Second))
	if _, err := c.Write(rahmen(seq, cmd, nutz)); err != nil {
		return nil, err
	}
	// Das Geraet antwortet auf Steuerbefehle oft erst leer und schickt den
	// neuen Stand hinterher. Bis zu drei Pakete lesen.
	for i := 0; i < 3; i++ {
		_, antw, err := leseRahmen(c)
		if err != nil {
			if cmd != tuyaAbfrage && i > 0 {
				return nil, nil // Befehl kam an, nur kein Stand hinterher
			}
			return nil, err
		}
		if len(antw) == 0 {
			continue
		}
		if bytes.HasPrefix(antw, []byte("3.3")) {
			antw = antw[15:]
		}
		klar, err := entpacke(key, antw)
		if err != nil {
			return nil, err
		}
		var a struct {
			DPS map[string]any `json:"dps"`
		}
		if json.Unmarshal(klar, &a) == nil && a.DPS != nil {
			return a.DPS, nil
		}
	}
	return nil, nil
}

// --- Adressen im Heimnetz -------------------------------------------------

var tuyaUDPSchluessel = func() []byte { h := md5.Sum([]byte("yGAdlopoPVldABfn")); return h[:] }()

func (t *tuya) horche() {
	adr, _ := net.ResolveUDPAddr("udp4", ":6667")
	c, err := net.ListenUDP("udp4", adr)
	if err != nil {
		t.sag("Tuya: kann nicht auf UDP 6667 horchen: %v", err)
		return
	}
	puffer := make([]byte, 2048)
	for {
		n, von, err := c.ReadFromUDP(puffer)
		if err != nil || n < 28 {
			continue
		}
		nutz := puffer[20 : n-8]
		klar, err := entpacke(tuyaUDPSchluessel, nutz[:len(nutz)/16*16])
		if err != nil {
			continue
		}
		var a struct {
			GwID string `json:"gwId"`
			IP   string `json:"ip"`
		}
		if json.Unmarshal(klar, &a) != nil {
			continue
		}
		t.Lock()
		if st := t.stand[a.GwID]; st != nil {
			ip := von.IP.String()
			if st.IP != ip && st.IP != "" {
				t.sag("Tuya: %s jetzt unter %s", t.name(a.GwID), ip)
			}
			st.IP, st.Gesehen = ip, time.Now()
		}
		t.Unlock()
	}
}

func (t *tuya) name(id string) string {
	for _, g := range t.geraete {
		if g.ID == id {
			return g.Name
		}
	}
	return id
}

// --- Abfragen und Steuern --------------------------------------------------

func (t *tuya) geraet(id string) (tuyaGeraet, string, bool) {
	t.Lock()
	defer t.Unlock()
	for _, g := range t.geraete {
		if g.ID == id {
			return g, t.stand[id].IP, true
		}
	}
	return tuyaGeraet{}, "", false
}

func (t *tuya) frage(id string) {
	g, ip, ok := t.geraet(id)
	if !ok || ip == "" {
		return
	}
	dps, err := t.anfrage(g, ip, tuyaAbfrage, nil)
	t.Lock()
	defer t.Unlock()
	st := t.stand[id]
	if err != nil {
		if st.Fehler == "" {
			t.sag("Tuya: %s antwortet nicht: %v", g.Name, err)
		}
		st.Fehler = err.Error()
		return
	}
	st.Fehler, st.Abgefragt = "", time.Now()
	if dps != nil {
		st.DPS = dps
	}
}

// schalte: Rollladen "open", "stop", "close", "lueften"; Steckdose "an",
// "aus". Ein Befehl von aussen bricht eine laufende Lueftungsfahrt ab.
func (t *tuya) schalte(id, was string) error {
	g, _, ok := t.geraet(id)
	if !ok {
		return errors.New("unbekanntes Geraet")
	}
	if g.Art == "clkg" {
		t.Lock()
		t.stand[id].Folge++
		folge := t.stand[id].Folge
		t.Unlock()
		if was == "lueften" {
			go t.lueftungsfahrt(g, folge)
			return nil
		}
	}
	return t.befehl(id, was)
}

// lueftungsfahrt: Lueftungsstellung anfahren. Ist der Rollladen sicher oben
// oder unten (letzter Befehl ganz durchgelaufen), reicht eine kurze Fahrt.
// Sonst erst ganz zu und dann hoch.
func (t *tuya) lueftungsfahrt(g tuyaGeraet, folge int) {
	oben, unten := g.lueftungsZeiten()
	t.Lock()
	st := t.stand[g.ID]
	durch := time.Since(st.BefehlZeit) >= 40*time.Second
	stand := st.Befehl
	t.Unlock()
	noch := func() bool {
		t.Lock()
		defer t.Unlock()
		return t.stand[g.ID].Folge == folge
	}
	schritt := func(was string, warte time.Duration) bool {
		if !noch() {
			return false
		}
		if err := t.befehl(g.ID, was); err != nil {
			t.sag("Tuya: %s Lueftungsfahrt abgebrochen: %v", g.Name, err)
			return false
		}
		time.Sleep(warte)
		return true
	}
	switch {
	case stand == "open" && durch:
		if !schritt("close", oben) {
			return
		}
	case stand == "close" && durch:
		if !schritt("open", unten) {
			return
		}
	default:
		if !schritt("close", 42*time.Second) || !schritt("open", unten) {
			return
		}
	}
	if noch() && t.befehl(g.ID, "stop") == nil {
		t.Lock()
		t.stand[g.ID].Befehl = "lueften"
		t.Unlock()
	}
}

// befehl schickt einen einzelnen Befehl ans Geraet.
func (t *tuya) befehl(id, was string) error {
	g, ip, ok := t.geraet(id)
	if !ok {
		return errors.New("unbekanntes Geraet")
	}
	if ip == "" {
		return fmt.Errorf("%s ist im Heimnetz nicht zu sehen", g.Name)
	}
	var dps map[string]any
	switch {
	case g.Art == "clkg" && (was == "open" || was == "stop" || was == "close"):
		// Im Heimnetz heissen die Werte on, stop, off. Die Cloud zeigt sie
		// als open, stop, close und uebersetzt selbst (gemessen 07.10.2026).
		dps = map[string]any{"1": map[string]string{"open": "on", "stop": "stop", "close": "off"}[was]}
	case g.Art == "cz" && (was == "an" || was == "aus"):
		dps = map[string]any{"1": was == "an"}
	default:
		return fmt.Errorf("%s kann %q nicht", g.Name, was)
	}
	neu, err := t.anfrage(g, ip, tuyaSteuern, dps)
	if err != nil {
		return err
	}
	t.Lock()
	st := t.stand[id]
	if neu != nil {
		if st.DPS == nil {
			st.DPS = map[string]any{}
		}
		for k, v := range neu {
			st.DPS[k] = v
		}
	}
	if g.Art == "clkg" {
		st.Befehl, st.BefehlZeit = was, time.Now()
	}
	t.Unlock()
	t.sag("Tuya: %s %s", g.Name, was)
	if g.Art == "cz" {
		t.frage(id)
	}
	return nil
}

// rolllaeden: die IDs aller Rollladenschalter.
func (t *tuya) rolllaeden() []string {
	t.Lock()
	defer t.Unlock()
	var ids []string
	for _, g := range t.geraete {
		if g.Art == "clkg" {
			ids = append(ids, g.ID)
		}
	}
	return ids
}

func (t *tuya) laufe() {
	go t.horche()
	time.Sleep(30 * time.Second) // erst die Rundrufe abwarten
	for {
		t.Lock()
		var ids []string
		for _, g := range t.geraete {
			ids = append(ids, g.ID)
		}
		t.Unlock()
		for _, id := range ids {
			t.frage(id)
		}
		time.Sleep(time.Minute)
	}
}

func (t *tuya) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/api/tuya", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var a struct {
				ID  string `json:"id"`
				Was string `json:"was"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 512)).Decode(&a) != nil {
				http.Error(w, "unbrauchbar", 400)
				return
			}
			if err := t.schalte(a.ID, a.Was); err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			w.WriteHeader(204)
			return
		}
		t.Lock()
		var aus []map[string]any
		for _, g := range t.geraete {
			st := t.stand[g.ID]
			online := !st.Gesehen.IsZero() && time.Since(st.Gesehen) < 2*time.Minute
			aus = append(aus, map[string]any{"id": g.ID, "name": g.Name, "art": g.Art, "online": online,
				"stand": st})
		}
		t.Unlock()
		jsonAntwort(w, aus)
	})
}
