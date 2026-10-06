package main

// Heizkoerperventile an der FRITZ!Box (FRITZ!DECT). Gelesen werden alle,
// geschrieben wird seit 06.10.2026 nur fuer die Lueftung im Spielekeller.
//
// Die Box gibt je Ventil Ist- und Solltemperatur, offenes Fenster, Boost,
// Sommer- und Urlaubsbetrieb und den Batteriestand aus. Wie weit das Ventil
// offen steht, gibt sie nicht heraus. "Fordert Waerme" wird deshalb aus
// ist < soll abgeleitet.
//
// Im Haus gibt es einen Kamin (Hinweis vom 03.10.2026): Ein Raum kann warm
// werden, ohne dass sein Ventil Waerme fordert. Steigt die Temperatur in zwei
// Stunden um mehr als ein Grad, ohne dass das Ventil fordert, heisst das auf
// der Seite "warm ohne Heizung".
//
// Zugang: Benutzer und Passwort in fritz.json neben der Wetterseite, nur auf
// dem Tablet. Anmeldung mit PBKDF2 (login_sid.lua?version=2). Abruf alle
// fuenf Minuten, Verlauf sieben Tage, auf Platte gesichert.

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ventil struct {
	AIN       string    `json:"ain"`
	Name      string    `json:"name"`
	Ist       float64   `json:"ist"`
	Soll      float64   `json:"soll"` // -1 aus, 99 voll auf
	Fordert   bool      `json:"fordert"`
	Fenster   bool      `json:"fenster_offen"`
	Boost     bool      `json:"boost"`
	Sommer    bool      `json:"sommer"`
	Urlaub    bool      `json:"urlaub"`
	Batterie  int       `json:"batterie"`
	BattLeer  bool      `json:"batterie_schwach"`
	Fehler    int       `json:"fehler"`
	Naechste  time.Time `json:"naechste_umschaltung,omitempty"`
	NaechsteT float64   `json:"naechste_soll,omitempty"`
	Fremd     bool      `json:"warm_ohne_heizung"`
}

type ventilPunkt struct {
	Z    int64   `json:"z"` // Unix
	Ist  float64 `json:"i"`
	Soll float64 `json:"s"`
	F    bool    `json:"f,omitempty"` // fordert
}

type fritz struct {
	sync.Mutex
	pfad, verlaufPfad string
	client            *http.Client
	sitzung           sync.Mutex // schuetzt sid, Lesen und Schreiben teilen sich die Sitzung
	sid               string
	ventile           []ventil
	zeit              time.Time
	verlauf           map[string][]ventilPunkt
	fehler            string
	sag               func(string, ...any)
	gesichert         time.Time
}

func neueFritz(pfad, verlaufPfad string, sag func(string, ...any)) *fritz {
	f := &fritz{pfad: pfad, verlaufPfad: verlaufPfad, client: &http.Client{Timeout: 10 * time.Second},
		verlauf: map[string][]ventilPunkt{}, sag: sag}
	if roh, err := os.ReadFile(verlaufPfad); err == nil {
		json.Unmarshal(roh, &f.verlauf)
	}
	return f
}

type fritzZugang struct {
	Adresse, Benutzer, Passwort string
}

func (f *fritz) zugang() (fritzZugang, error) {
	var z fritzZugang
	roh, err := os.ReadFile(f.pfad)
	if err != nil {
		return z, fmt.Errorf("keine Zugangsdaten")
	}
	if err := json.Unmarshal(roh, &z); err != nil || z.Benutzer == "" {
		return z, fmt.Errorf("Zugangsdaten unlesbar")
	}
	if z.Adresse == "" {
		z.Adresse = "http://192.168.2.1"
	}
	return z, nil
}

type sidAntwort struct {
	SID       string `xml:"SID"`
	Challenge string `xml:"Challenge"`
	BlockTime int    `xml:"BlockTime"`
}

func (f *fritz) holeXML(u string, form url.Values, ziel any) error {
	var r *http.Response
	var err error
	if form != nil {
		r, err = f.client.PostForm(u, form)
	} else {
		r, err = f.client.Get(u)
	}
	if err != nil {
		return err
	}
	defer r.Body.Close()
	roh, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if s, ok := ziel.(*string); ok {
		*s = string(roh)
		return nil
	}
	return xml.Unmarshal(roh, ziel)
}

// anmelden nach AVM "Session-IDs im FRITZ!OS", Verfahren PBKDF2.
func (f *fritz) anmelden(z fritzZugang) error {
	var a sidAntwort
	if err := f.holeXML(z.Adresse+"/login_sid.lua?version=2", nil, &a); err != nil {
		return err
	}
	teile := strings.Split(a.Challenge, "$")
	if len(teile) != 5 || teile[0] != "2" {
		return fmt.Errorf("unerwartete Challenge")
	}
	i1, _ := strconv.Atoi(teile[1])
	s1, _ := hex.DecodeString(teile[2])
	i2, _ := strconv.Atoi(teile[3])
	s2, _ := hex.DecodeString(teile[4])
	h1, err := pbkdf2.Key(sha256.New, z.Passwort, s1, i1, 32)
	if err != nil {
		return err
	}
	h2, err := pbkdf2.Key(sha256.New, string(h1), s2, i2, 32)
	if err != nil {
		return err
	}
	form := url.Values{"username": {z.Benutzer}, "response": {teile[4] + "$" + hex.EncodeToString(h2)}}
	if err := f.holeXML(z.Adresse+"/login_sid.lua?version=2", form, &a); err != nil {
		return err
	}
	if a.SID == "" || strings.Trim(a.SID, "0") == "" {
		return fmt.Errorf("Anmeldung abgelehnt (Sperre %d s)", a.BlockTime)
	}
	f.sid = a.SID
	return nil
}

type fritzListe struct {
	Devices []struct {
		AIN  string `xml:"identifier,attr"`
		Name string `xml:"name"`
		Temp *struct {
			Celsius int `xml:"celsius"`
		} `xml:"temperature"`
		HKR *struct {
			Tist       int `xml:"tist"`
			Tsoll      int `xml:"tsoll"`
			Battery    int `xml:"battery"`
			BatteryLow int `xml:"batterylow"`
			Window     int `xml:"windowopenactiv"`
			Boost      int `xml:"boostactive"`
			Summer     int `xml:"summeractive"`
			Holiday    int `xml:"holidayactive"`
			ErrorCode  int `xml:"errorcode"`
			NextChange *struct {
				EndPeriod int64 `xml:"endperiod"`
				Tchange   int   `xml:"tchange"`
			} `xml:"nextchange"`
		} `xml:"hkr"`
	} `xml:"device"`
}

// hkrTemp: Halbgrad-Werte der Box. 253 heisst aus, 254 heisst an.
func hkrTemp(v int) float64 {
	switch v {
	case 253:
		return -1
	case 254:
		return 99
	}
	return float64(v) / 2
}

func (f *fritz) lies() {
	z, err := f.zugang()
	if err != nil {
		f.merkeFehler(err)
		return
	}
	var txt string
	f.sitzung.Lock()
	for versuch := 0; versuch < 2; versuch++ {
		if f.sid == "" {
			if err := f.anmelden(z); err != nil {
				f.sitzung.Unlock()
				f.merkeFehler(err)
				return
			}
		}
		err = f.holeXML(z.Adresse+"/webservices/homeautoswitch.lua?switchcmd=getdevicelistinfos&sid="+f.sid, nil, &txt)
		if err == nil && strings.Contains(txt, "<devicelist") {
			break
		}
		f.sid = "" // abgelaufen, neu anmelden
	}
	f.sitzung.Unlock()
	var l fritzListe
	if err := xml.Unmarshal([]byte(txt), &l); err != nil {
		f.merkeFehler(fmt.Errorf("Geraeteliste: %v", err))
		return
	}
	jetzt := time.Now().In(ort)
	var vs []ventil
	f.Lock()
	defer f.Unlock()
	for _, d := range l.Devices {
		if d.HKR == nil {
			continue
		}
		h := d.HKR
		v := ventil{AIN: strings.ReplaceAll(d.AIN, " ", ""), Name: strings.TrimSpace(d.Name),
			Ist: hkrTemp(h.Tist), Soll: hkrTemp(h.Tsoll), Fenster: h.Window == 1, Boost: h.Boost == 1,
			Sommer: h.Summer == 1, Urlaub: h.Holiday == 1, Batterie: h.Battery, BattLeer: h.BatteryLow == 1,
			Fehler: h.ErrorCode}
		if d.Temp != nil {
			v.Ist = float64(d.Temp.Celsius) / 10 // mit Versatzkorrektur, genauer als tist
		}
		v.Fordert = !v.Fenster && !v.Sommer && (v.Soll >= 99 || (v.Soll > 0 && v.Ist < v.Soll-0.3))
		if n := h.NextChange; n != nil && n.EndPeriod > 0 {
			v.Naechste = time.Unix(n.EndPeriod, 0).In(ort)
			v.NaechsteT = hkrTemp(n.Tchange)
		}
		p := append(f.verlauf[v.AIN], ventilPunkt{Z: jetzt.Unix(), Ist: v.Ist, Soll: v.Soll, F: v.Fordert})
		grenze := jetzt.Add(-7 * 24 * time.Hour).Unix()
		for len(p) > 0 && p[0].Z < grenze {
			p = p[1:]
		}
		f.verlauf[v.AIN] = p
		// Warm ohne Heizung: in zwei Stunden mehr als ein Grad waermer, und
		// das Ventil hat in der Zeit nicht gefordert.
		vor := jetzt.Add(-2 * time.Hour).Unix()
		for _, q := range p {
			if q.Z >= vor {
				if v.Ist-q.Ist >= 1.0 {
					v.Fremd = true
					for _, r := range p {
						if r.Z >= q.Z && r.F {
							v.Fremd = false
						}
					}
				}
				break
			}
		}
		vs = append(vs, v)
	}
	f.ventile, f.zeit, f.fehler = vs, jetzt, ""
	if jetzt.Sub(f.gesichert) >= 30*time.Minute {
		schreibeJSON(f.verlaufPfad, f.verlauf)
		f.gesichert = jetzt
	}
}

// setzeSoll stellt die Solltemperatur eines Ventils, fuer die Lueftung. Unter
// null heisst aus. Die Box haelt den Wert bis zum naechsten Schaltpunkt ihres
// Wochenplans. Danach wird der Stand neu gelesen, damit der Rest der
// Steuerung den neuen Sollwert sieht.
func (f *fritz) setzeSoll(ain string, grad float64) error {
	z, err := f.zugang()
	if err != nil {
		return err
	}
	param := "253"
	if grad >= 0 {
		v := int(math.Round(grad * 2))
		param = strconv.Itoa(min(56, max(16, v)))
	}
	var txt string
	f.sitzung.Lock()
	for versuch := 0; versuch < 2; versuch++ {
		if f.sid == "" {
			if err = f.anmelden(z); err != nil {
				break
			}
		}
		err = f.holeXML(z.Adresse+"/webservices/homeautoswitch.lua?switchcmd=sethkrtsoll&ain="+
			url.QueryEscape(ain)+"&param="+param+"&sid="+f.sid, nil, &txt)
		if err == nil && strings.TrimSpace(txt) == param {
			break
		}
		if err == nil {
			err = fmt.Errorf("Box antwortet %q", strings.TrimSpace(txt))
		}
		f.sid = ""
	}
	f.sitzung.Unlock()
	if err != nil {
		return err
	}
	f.sag("FRITZ!Box: Ventil %s auf %s gestellt", ain, param)
	f.lies()
	return nil
}

// ventilMit sucht ein Ventil, dessen Name den Text enthaelt.
func (f *fritz) ventilMit(teil string) (ventil, bool) {
	f.Lock()
	defer f.Unlock()
	for _, v := range f.ventile {
		if strings.Contains(strings.ToLower(v.Name), strings.ToLower(teil)) {
			return v, true
		}
	}
	return ventil{}, false
}

func (f *fritz) merkeFehler(err error) {
	f.Lock()
	defer f.Unlock()
	if f.fehler != err.Error() {
		f.sag("FRITZ!Box: %v", err)
	}
	f.fehler = err.Error()
}

func (f *fritz) laufe() {
	time.Sleep(time.Minute)
	for {
		f.lies()
		time.Sleep(5 * time.Minute)
	}
}

// stand: Ventile mit Bedarfsstunden aus dem Verlauf.
func (f *fritz) stand() map[string]any {
	f.Lock()
	defer f.Unlock()
	jetzt := time.Now().Unix()
	var raeume []map[string]any
	for _, v := range f.ventile {
		h24, h7 := 0.0, 0.0
		p := f.verlauf[v.AIN]
		for i := 1; i < len(p); i++ {
			if !p[i-1].F {
				continue
			}
			dt := float64(p[i].Z-p[i-1].Z) / 3600
			if dt > 0.5 {
				dt = 0.5 // Luecke, nicht als Bedarf zaehlen
			}
			h7 += dt
			if p[i].Z >= jetzt-86400 {
				h24 += dt
			}
		}
		raeume = append(raeume, map[string]any{"ventil": v, "bedarf_24h": h24, "bedarf_7t": h7})
	}
	return map[string]any{"zeit": f.zeit, "fehler": f.fehler, "raeume": raeume}
}

func (f *fritz) kurve(ain string, stunden int) []ventilPunkt {
	f.Lock()
	defer f.Unlock()
	grenze := time.Now().Add(-time.Duration(stunden) * time.Hour).Unix()
	var out []ventilPunkt
	for _, p := range f.verlauf[ain] {
		if p.Z >= grenze {
			out = append(out, p)
		}
	}
	return out
}

func (f *fritz) hinweise() []string {
	f.Lock()
	defer f.Unlock()
	var h []string
	for _, v := range f.ventile {
		if v.BattLeer {
			h = append(h, "Ventil "+v.Name+": Batterie schwach")
		}
		if v.Fehler != 0 {
			h = append(h, fmt.Sprintf("Ventil %s: Fehler %d", v.Name, v.Fehler))
		}
	}
	return h
}
