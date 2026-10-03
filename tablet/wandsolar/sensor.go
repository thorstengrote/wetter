package main

// Raumluft fuer die Steuerung: Feuchte aus einem SwitchBot-Sensor ueber die
// SwitchBot-Cloud (API v1.1), weil der Hub Mini keine lokale Schnittstelle
// hat. Zugangsdaten liegen nur auf dem Tablet in switchbot.json neben der
// Wetterseite. Alle 5 Minuten ein Abruf je Sensor, erlaubt sind 10.000 am Tag.
//
// Fenster: hapwatch kennt die Stellung jedes Velux-Geraets und gibt sie unter
// /velux/stand aus, ohne das Gateway zu fragen.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type messFeuchte struct {
	RH   float64   `json:"rh"`
	Temp float64   `json:"temp"`
	Zeit time.Time `json:"zeit"`
}

type switchbot struct {
	sync.Mutex
	pfad      string
	client    *http.Client
	liste     []map[string]string
	listeZeit time.Time
	werte     map[string]messFeuchte
	verlauf   map[string][]messFeuchte // 48 Stunden je Sensor
	fehler    string
	sag       func(string, ...any)
}

// Auf Android gibt es kein resolv.conf, ein reines Linux-Binary fragt dann
// [::1]:53 und bekommt keine Antwort. Namen deshalb selbst bei der FritzBox
// aufloesen, ersatzweise bei Quad9.
var namensDienst = &net.Resolver{PreferGo: true,
	Dial: func(ctx context.Context, netz, adr string) (net.Conn, error) {
		d := net.Dialer{Timeout: 3 * time.Second}
		if c, err := d.DialContext(ctx, "udp", "192.168.2.1:53"); err == nil {
			return c, nil
		}
		return d.DialContext(ctx, "udp", "9.9.9.9:53")
	}}

func neuerSwitchbot(pfad string, sag func(string, ...any)) *switchbot {
	d := &net.Dialer{Timeout: 10 * time.Second, Resolver: namensDienst}
	tr := &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second}
	return &switchbot{pfad: pfad, client: &http.Client{Timeout: 15 * time.Second, Transport: tr},
		werte: map[string]messFeuchte{}, verlauf: map[string][]messFeuchte{}, sag: sag}
}

func (b *switchbot) rufe(pfad string, ziel any) error {
	roh, err := os.ReadFile(b.pfad)
	if err != nil {
		return fmt.Errorf("keine Zugangsdaten")
	}
	var z struct{ Token, Secret string }
	if json.Unmarshal(roh, &z) != nil || z.Token == "" {
		return fmt.Errorf("Zugangsdaten unlesbar")
	}
	t := fmt.Sprint(time.Now().UnixMilli())
	nb := make([]byte, 16)
	rand.Read(nb)
	nonce := hex.EncodeToString(nb)
	m := hmac.New(sha256.New, []byte(z.Secret))
	m.Write([]byte(z.Token + t + nonce))
	sign := strings.ToUpper(base64.StdEncoding.EncodeToString(m.Sum(nil)))
	req, _ := http.NewRequest("GET", "https://api.switch-bot.com/v1.1"+pfad, nil)
	req.Header.Set("Authorization", z.Token)
	req.Header.Set("sign", sign)
	req.Header.Set("t", t)
	req.Header.Set("nonce", nonce)
	r, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	var a struct {
		StatusCode int             `json:"statusCode"`
		Message    string          `json:"message"`
		Body       json.RawMessage `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		return err
	}
	if a.StatusCode != 100 {
		return fmt.Errorf("SwitchBot %d: %s", a.StatusCode, a.Message)
	}
	return json.Unmarshal(a.Body, ziel)
}

// sensoren: alle Geraete mit Feuchtemessung, zehn Minuten zwischengespeichert.
func (b *switchbot) sensoren() ([]map[string]string, error) {
	b.Lock()
	if time.Since(b.listeZeit) < 10*time.Minute && b.liste != nil {
		l := b.liste
		b.Unlock()
		return l, nil
	}
	b.Unlock()
	var body struct {
		DeviceList []struct {
			DeviceID   string `json:"deviceId"`
			DeviceName string `json:"deviceName"`
			DeviceType string `json:"deviceType"`
		} `json:"deviceList"`
	}
	if err := b.rufe("/devices", &body); err != nil {
		return nil, err
	}
	var l []map[string]string
	for _, d := range body.DeviceList {
		switch d.DeviceType {
		case "Meter", "MeterPlus", "Meter Plus", "WoIOSensor", "MeterPro", "MeterPro(CO2)", "Hub 2", "Hub 3":
			l = append(l, map[string]string{"id": d.DeviceID, "name": d.DeviceName, "typ": d.DeviceType})
		}
	}
	b.Lock()
	b.liste, b.listeZeit = l, time.Now()
	b.Unlock()
	return l, nil
}

func (b *switchbot) lies(id string) {
	var st struct {
		Humidity    float64 `json:"humidity"`
		Temperature float64 `json:"temperature"`
	}
	if err := b.rufe("/devices/"+id+"/status", &st); err != nil {
		b.Lock()
		if b.fehler != err.Error() {
			b.sag("SwitchBot: %v", err)
		}
		b.fehler = err.Error()
		b.Unlock()
		return
	}
	w := messFeuchte{RH: st.Humidity, Temp: st.Temperature, Zeit: time.Now().In(ort)}
	b.Lock()
	defer b.Unlock()
	b.fehler = ""
	b.werte[id] = w
	v := append(b.verlauf[id], w)
	grenze := w.Zeit.Add(-48 * time.Hour)
	for len(v) > 0 && v[0].Zeit.Before(grenze) {
		v = v[1:]
	}
	b.verlauf[id] = v
}

// wert: letzter Messwert, wenn juenger als 30 Minuten.
func (b *switchbot) wert(id string) (messFeuchte, bool) {
	b.Lock()
	defer b.Unlock()
	w, ok := b.werte[id]
	return w, ok && time.Since(w.Zeit) < 30*time.Minute
}

func (b *switchbot) kurve(id string) []messFeuchte {
	b.Lock()
	defer b.Unlock()
	return append([]messFeuchte(nil), b.verlauf[id]...)
}

// laufe fragt alle 5 Minuten die Sensoren ab, die eingestellt sind.
func (b *switchbot) laufe(ids func() []string) {
	time.Sleep(time.Minute) // erst das Netz und die Uhr nach dem Start
	for {
		for _, id := range ids() {
			b.lies(id)
		}
		time.Sleep(5 * time.Minute)
	}
}

// fensterStand liest die Stellung der Velux-Fenster aus hapwatch.
var fensterClient = &http.Client{Timeout: 3 * time.Second}

func fensterStand(basis string) (map[string]float64, error) {
	r, err := fensterClient.Get(strings.TrimRight(basis, "/") + "/velux/stand")
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	var a struct {
		Geraete map[string]float64 `json:"geraete"`
	}
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		return nil, err
	}
	return a.Geraete, nil
}
