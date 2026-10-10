package main

// Broadlink-Steckdosen (seit 10.10.2026): eine NEO Pro von GSM-One, innen eine
// Broadlink SP2 (Typ 0x2717, MAC B4:43:0D...). Gesprochen wird direkt im
// Heimnetz auf UDP 80, wie in python-broadlink: erst eine Anmeldung mit einem
// festen Schluessel, die einen Sitzungsschluessel liefert, dann Befehle
// AES-128-CBC verschluesselt. Strom misst dieser Typ nicht.
//
// Eingerichtet ohne App: Im eigenen WLAN "NeoAP" nimmt das Geraet ein
// unverschluesseltes Paket mit SSID und Passwort an (wandtablet/broadlink).
// Die Adresse findet der Dienst ueber einen Suchruf auf UDP 80.

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	blSchluessel, _ = hex.DecodeString("097628343fe99e23765c1513accf8b02")
	blIV, _         = hex.DecodeString("562e17996d093d28ddb3ba695a2e6f58")
)

type blGeraet struct {
	Name string `json:"name"`
	MAC  string `json:"mac"` // b4:43:0d:91:94:63
}

type blStand struct {
	IP         string    `json:"ip,omitempty"`
	Typ        uint16    `json:"typ,omitempty"`
	An         *bool     `json:"an,omitempty"`
	Abgefragt  time.Time `json:"abgefragt,omitempty"`
	Fehler     string    `json:"fehler,omitempty"`
	FehlerSeit time.Time `json:"fehler_seit,omitempty"`
	macRoh     []byte    // wie im Suchruf, so geht sie auch in jedes Paket
	id         uint32
	key        []byte
	zaehler    uint16
}

type broadlink struct {
	sync.Mutex
	pfad    string
	sag     func(string, ...any)
	geraete []blGeraet
	stand   map[string]*blStand // nach MAC
}

func neuesBroadlink(pfad string, sag func(string, ...any)) *broadlink {
	b := &broadlink{pfad: pfad, sag: sag, stand: map[string]*blStand{}}
	if roh, err := os.ReadFile(pfad); err == nil {
		json.Unmarshal(roh, &b.geraete)
	}
	for _, g := range b.geraete {
		b.stand[strings.ToLower(g.MAC)] = &blStand{}
	}
	return b
}

func blPruefsumme(b []byte) uint16 {
	s := uint32(0xBEAF)
	for _, x := range b {
		s += uint32(x)
	}
	return uint16(s)
}

func blCBC(key, b []byte, ver bool) []byte {
	c, _ := aes.NewCipher(key)
	aus := make([]byte, len(b))
	if ver {
		cipher.NewCBCEncrypter(c, blIV).CryptBlocks(aus, b)
	} else {
		cipher.NewCBCDecrypter(c, blIV).CryptBlocks(aus, b)
	}
	return aus
}

// suche: Suchruf ins Heimnetz, Antworten nach MAC.
func blSuche(eigene net.IP, warte time.Duration) (map[string]*blStand, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: eigene})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	port := c.LocalAddr().(*net.UDPAddr).Port
	t := time.Now()
	p := make([]byte, 0x30)
	_, off := t.Zone()
	tz := off / 3600
	if tz < 0 {
		p[0x08] = byte(0xFF + tz - 1)
		p[0x09], p[0x0A], p[0x0B] = 0xFF, 0xFF, 0xFF
	} else {
		p[0x08] = byte(tz)
	}
	binary.LittleEndian.PutUint16(p[0x0C:], uint16(t.Year()))
	p[0x0E], p[0x0F], p[0x10] = byte(t.Minute()), byte(t.Hour()), byte(t.Year()%100)
	p[0x11], p[0x12], p[0x13] = byte((int(t.Weekday())+6)%7+1), byte(t.Day()), byte(t.Month())
	copy(p[0x18:], eigene.To4())
	binary.LittleEndian.PutUint16(p[0x1C:], uint16(port))
	p[0x26] = 6
	binary.LittleEndian.PutUint16(p[0x20:], blPruefsumme(p))
	ziel := &net.UDPAddr{IP: net.IPv4bcast, Port: 80}
	gef := map[string]*blStand{}
	ende := time.Now().Add(warte)
	puffer := make([]byte, 1024)
	for time.Now().Before(ende) {
		c.WriteToUDP(p, ziel)
		c.SetReadDeadline(time.Now().Add(time.Second))
		for {
			n, von, err := c.ReadFromUDP(puffer)
			if err != nil {
				break
			}
			if n < 0x40 {
				continue
			}
			a := puffer[:n]
			roh := append([]byte(nil), a[0x3A:0x40]...)
			var m []string
			for i := 5; i >= 0; i-- {
				m = append(m, fmt.Sprintf("%02x", roh[i]))
			}
			gef[strings.Join(m, ":")] = &blStand{IP: von.IP.String(), Typ: binary.LittleEndian.Uint16(a[0x34:]), macRoh: roh}
		}
	}
	return gef, nil
}

// sende: ein Paket an das Geraet, Antwort entschluesselt.
func (st *blStand) sende(befehl uint16, nutz []byte) ([]byte, error) {
	key := st.key
	if key == nil {
		key = blSchluessel
	}
	st.zaehler = (st.zaehler + 1) | 0x8000
	p := make([]byte, 0x38)
	copy(p, []byte{0x5a, 0xa5, 0xaa, 0x55, 0x5a, 0xa5, 0xaa, 0x55})
	binary.LittleEndian.PutUint16(p[0x24:], st.Typ)
	binary.LittleEndian.PutUint16(p[0x26:], befehl)
	binary.LittleEndian.PutUint16(p[0x28:], st.zaehler)
	copy(p[0x2A:], st.macRoh)
	binary.LittleEndian.PutUint32(p[0x30:], st.id)
	binary.LittleEndian.PutUint16(p[0x34:], blPruefsumme(nutz))
	if r := len(nutz) % 16; r != 0 {
		nutz = append(nutz, make([]byte, 16-r)...)
	}
	p = append(p, blCBC(key, nutz, true)...)
	binary.LittleEndian.PutUint16(p[0x20:], blPruefsumme(p))
	c, err := net.DialTimeout("udp4", net.JoinHostPort(st.IP, "80"), 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	puffer := make([]byte, 2048)
	for versuch := 0; versuch < 3; versuch++ {
		c.Write(p)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := c.Read(puffer)
		if err != nil {
			continue
		}
		a := puffer[:n]
		if n < 0x38 {
			return nil, errors.New("Antwort zu kurz")
		}
		if code := binary.LittleEndian.Uint16(a[0x22:]); code != 0 {
			return nil, fmt.Errorf("Geraet meldet Fehler 0x%04x", code)
		}
		rest := a[0x38:]
		if len(rest)%16 != 0 {
			return nil, errors.New("Antwort nicht blockweise")
		}
		return blCBC(key, rest, false), nil
	}
	return nil, errors.New("keine Antwort")
}

func (st *blStand) anmelden() error {
	n := make([]byte, 0x50)
	for i := 0x04; i < 0x14; i++ {
		n[i] = 0x31
	}
	n[0x1E], n[0x2D] = 0x01, 0x01
	copy(n[0x30:], "Test 1")
	// Eine Anmeldung beginnt immer ohne Sitzung. Nach einem Neustart der
	// Steckdose meldete sie mit der alten Nummer "Steuerschluessel abgelaufen"
	// (0xfff9, 10.10.2026).
	st.key, st.id = nil, 0
	a, err := st.sende(0x65, n)
	if err != nil {
		return err
	}
	if len(a) < 0x14 {
		return errors.New("Anmeldung ohne Schluessel")
	}
	st.id = binary.LittleEndian.Uint32(a[0:4])
	st.key = append([]byte(nil), a[0x04:0x14]...)
	return nil
}

// zustand fragt den Schaltzustand ab (SP2).
func (st *blStand) zustand() (bool, error) {
	n := make([]byte, 16)
	n[0] = 1
	a, err := st.sende(0x6A, n)
	if err != nil {
		return false, err
	}
	if len(a) < 5 {
		return false, errors.New("Antwort ohne Zustand")
	}
	return a[4] == 1 || a[4] == 3, nil
}

func (st *blStand) schalte(an bool) error {
	n := make([]byte, 16)
	n[0] = 2
	if an {
		n[4] = 1
	}
	_, err := st.sende(0x6A, n)
	return err
}

// --- Dienst --------------------------------------------------------------

func eigeneAdresse() net.IP {
	c, err := net.Dial("udp4", "192.168.2.1:80")
	if err != nil {
		return nil
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP
}

func (b *broadlink) finde() {
	ip := eigeneAdresse()
	if ip == nil {
		return
	}
	gef, err := blSuche(ip, 4*time.Second)
	if err != nil {
		b.sag("Broadlink: Suche gescheitert: %v", err)
		return
	}
	b.Lock()
	defer b.Unlock()
	for mac, neu := range gef {
		st := b.stand[mac]
		if st == nil {
			continue
		}
		if st.IP != neu.IP {
			if st.IP != "" {
				b.sag("Broadlink: %s jetzt unter %s", mac, neu.IP)
			}
			st.IP, st.key = neu.IP, nil
		}
		st.Typ, st.macRoh = neu.Typ, neu.macRoh
	}
}

func (b *broadlink) mitGeraet(mac string, f func(st *blStand) error) error {
	b.Lock()
	defer b.Unlock()
	st := b.stand[strings.ToLower(mac)]
	if st == nil {
		return errors.New("unbekanntes Geraet")
	}
	if st.IP == "" {
		return errors.New("im Heimnetz nicht zu sehen")
	}
	if st.key == nil {
		if err := st.anmelden(); err != nil {
			return fmt.Errorf("Anmeldung: %v", err)
		}
	}
	err := f(st)
	if err != nil {
		st.key = nil // beim naechsten Mal neu anmelden
	}
	return err
}

func (b *broadlink) frage(mac string) {
	var an bool
	err := b.mitGeraet(mac, func(st *blStand) error {
		var e error
		an, e = st.zustand()
		return e
	})
	b.Lock()
	defer b.Unlock()
	st := b.stand[strings.ToLower(mac)]
	if err != nil {
		if st.Fehler == "" {
			b.sag("Broadlink: %s: %v", mac, err)
			st.FehlerSeit = time.Now()
		}
		st.Fehler = err.Error()
		return
	}
	st.An, st.Abgefragt, st.Fehler = &an, time.Now(), ""
}

// hinweise fuer die Wand: seit 15 Minuten keine Verbindung.
func (b *broadlink) hinweise() []string {
	b.Lock()
	defer b.Unlock()
	var h []string
	for _, g := range b.geraete {
		st := b.stand[strings.ToLower(g.MAC)]
		if st != nil && st.Fehler != "" && time.Since(st.FehlerSeit) > 15*time.Minute {
			h = append(h, g.Name+": nicht erreichbar seit "+st.FehlerSeit.In(ort).Format("15:04"))
		}
	}
	return h
}

func (b *broadlink) laufe() {
	time.Sleep(20 * time.Second)
	runde := 0
	for {
		if runde%10 == 0 {
			b.finde()
		}
		runde++
		for _, g := range b.geraete {
			b.frage(g.MAC)
		}
		time.Sleep(time.Minute)
	}
}

func (b *broadlink) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/api/broadlink", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var a struct {
				MAC string `json:"mac"`
				Was string `json:"was"` // an, aus
			}
			if json.NewDecoder(io.LimitReader(r.Body, 512)).Decode(&a) != nil || (a.Was != "an" && a.Was != "aus") {
				http.Error(w, "mac und was (an, aus)", 400)
				return
			}
			if err := b.mitGeraet(a.MAC, func(st *blStand) error { return st.schalte(a.Was == "an") }); err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			b.sag("Broadlink: %s %s", a.MAC, a.Was)
			b.frage(a.MAC)
			w.WriteHeader(204)
			return
		}
		b.Lock()
		var aus []map[string]any
		for _, g := range b.geraete {
			aus = append(aus, map[string]any{"name": g.Name, "mac": g.MAC, "stand": b.stand[strings.ToLower(g.MAC)]})
		}
		b.Unlock()
		jsonAntwort(w, aus)
	})
}
