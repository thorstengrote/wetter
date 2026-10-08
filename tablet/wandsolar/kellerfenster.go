package main

// Kellerfenster im Spielekeller: zwei rohrmotor24-RMF-Antriebe ohne
// Rueckmeldung, gefahren ueber einen D1 mini mit 433-MHz-Sender
// (Quelle im Repo wandtablet unter funk433/kellerfenster).
//
// Der D1 mini hoert ausserdem die RMF-Funkzeitschaltuhr an der Wand mit.
// Seine Ereignisse tragen nur Millisekunden seit seinem Start, die Zeit
// rechnet wandsolar beim Abruf aus. Faellt der Zaehler, ist er neu
// gestartet, und alles danach ist neu.
//
// Eine echte Stellung gibt es nicht. Der Stand ist der letzte Befehl,
// egal ob von der Uhr, vom Tablet oder von Hand am D1 mini.
//
// Zugang: Adresse und Schluessel in kellerfenster.json neben der Wetterseite,
// nur auf dem Tablet. Fehlt die Datei, ist das Modul still.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

type kfEreignis struct {
	Zeit     time.Time `json:"zeit"`
	Quelle   string    `json:"quelle"` // uhr, netz, seriell
	Richtung string    `json:"richtung"`
	Kanal    int       `json:"kanal"`
}

type kfZugang struct {
	Adresse    string `json:"adresse"`
	Schluessel string `json:"schluessel"`
}

type kellerfenster struct {
	sync.Mutex
	pfad, verlaufPfad string
	sag               func(string, ...any)
	web               *http.Client

	ereignisse []kfEreignis
	letzterMs  uint32 // jetzt_ms beim letzten Abruf
	gesehenMs  uint32 // juengstes uebernommenes Ereignis seit dem Start des D1 mini
	erreicht   time.Time
	fehler     string
	rssi       int
}

const kfFahrzeit = 25 * time.Second // gestoppt am 08.10.2026: ganz auf in 23 s

func neueKellerfenster(pfad, verlaufPfad string, sag func(string, ...any)) *kellerfenster {
	k := &kellerfenster{pfad: pfad, verlaufPfad: verlaufPfad, sag: sag,
		web: &http.Client{Timeout: 8 * time.Second}}
	if roh, err := os.ReadFile(verlaufPfad); err == nil {
		json.Unmarshal(roh, &k.ereignisse)
	}
	return k
}

func (k *kellerfenster) zugang() (kfZugang, error) {
	var z kfZugang
	roh, err := os.ReadFile(k.pfad)
	if err != nil {
		return z, err
	}
	if err := json.Unmarshal(roh, &z); err != nil || z.Adresse == "" {
		return z, fmt.Errorf("kellerfenster.json unvollstaendig")
	}
	return z, nil
}

func (k *kellerfenster) lies() {
	z, err := k.zugang()
	if err != nil {
		return
	}
	var a struct {
		JetztMs    uint32 `json:"jetzt_ms"`
		RSSI       int    `json:"rssi"`
		Ereignisse []struct {
			Ms       uint32 `json:"ms"`
			Quelle   string `json:"quelle"`
			Richtung string `json:"richtung"`
			Kanal    int    `json:"kanal"`
		} `json:"ereignisse"`
	}
	t := time.Now()
	r, err := k.web.Get("http://" + z.Adresse + "/stand")
	if err == nil {
		err = json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&a)
		r.Body.Close()
	}
	k.Lock()
	defer k.Unlock()
	if err != nil {
		if k.fehler == "" {
			k.sag("Kellerfenster: nicht erreichbar: %v", err)
		}
		k.fehler = "nicht erreichbar"
		return
	}
	if k.fehler != "" {
		k.sag("Kellerfenster: wieder erreichbar")
	}
	k.fehler, k.erreicht, k.rssi = "", t, a.RSSI
	if a.JetztMs < k.letzterMs {
		k.gesehenMs = 0 // D1 mini neu gestartet
	}
	k.letzterMs = a.JetztMs
	neu := false
	for _, e := range a.Ereignisse {
		if e.Ms <= k.gesehenMs || e.Ms > a.JetztMs {
			continue
		}
		k.gesehenMs = e.Ms
		zeit := t.Add(-time.Duration(a.JetztMs-e.Ms) * time.Millisecond)
		// Nach einem Neustart von wandsolar liefert der D1 mini dieselben
		// Ereignisse noch einmal, die stehen dann schon im Verlauf.
		if k.schonDa(zeit, e.Richtung) {
			continue
		}
		k.ereignisse = append(k.ereignisse, kfEreignis{zeit, e.Quelle, e.Richtung, e.Kanal})
		if e.Quelle == "uhr" {
			k.sag("Kellerfenster: %s an der Zeitschaltuhr (Kanal %d)", e.Richtung, e.Kanal)
		}
		neu = true
	}
	if neu {
		if len(k.ereignisse) > 100 {
			k.ereignisse = k.ereignisse[len(k.ereignisse)-100:]
		}
		if roh, err := json.Marshal(k.ereignisse); err == nil {
			os.WriteFile(k.verlaufPfad+".neu", roh, 0644)
			os.Rename(k.verlaufPfad+".neu", k.verlaufPfad)
		}
	}
}

func (k *kellerfenster) schonDa(zeit time.Time, richtung string) bool {
	for i := len(k.ereignisse) - 1; i >= 0 && i >= len(k.ereignisse)-20; i-- {
		e := k.ereignisse[i]
		d := e.Zeit.Sub(zeit)
		if e.Richtung == richtung && d > -3*time.Second && d < 3*time.Second {
			return true
		}
	}
	return false
}

func (k *kellerfenster) fahre(richtung string) error {
	z, err := k.zugang()
	if err != nil {
		return fmt.Errorf("kein Zugang zum D1 mini: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+z.Adresse+"/fahre?r="+richtung, nil)
	req.Header.Set("X-Schluessel", z.Schluessel)
	r, err := k.web.Do(req)
	if err != nil {
		return fmt.Errorf("D1 mini nicht erreichbar")
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		return fmt.Errorf("D1 mini antwortet %d", r.StatusCode)
	}
	k.sag("Kellerfenster: %s gesendet", richtung)
	k.lies()
	return nil
}

func (k *kellerfenster) laufe() {
	for {
		k.lies()
		time.Sleep(20 * time.Second)
	}
}

// stand liefert den letzten Befehl als Stellung: auf, zu oder angehalten,
// in der ersten Minute nach dem Befehl als Bewegung.
func (k *kellerfenster) stand() map[string]any {
	k.Lock()
	defer k.Unlock()
	if _, err := k.zugang(); err != nil {
		return map[string]any{"eingerichtet": false}
	}
	a := map[string]any{"eingerichtet": true, "erreichbar": k.fehler == "" && !k.erreicht.IsZero(),
		"rssi": k.rssi}
	if !k.erreicht.IsZero() {
		a["abgerufen"] = k.erreicht
	}
	if k.fehler != "" {
		a["fehler"] = k.fehler
	}
	if n := len(k.ereignisse); n > 0 {
		a["stellung"], a["letzter"] = kfStellung(k.ereignisse, time.Now()), k.ereignisse[n-1]
		von := n - 10
		if von < 0 {
			von = 0
		}
		a["ereignisse"] = k.ereignisse[von:]
	}
	return a
}

// kfStellung leitet die Stellung aus den Befehlen ab. Ein Stop zaehlt nur,
// solange die Fenster nach dem letzten Auf oder Ab noch fahren koennen,
// danach ist die Fahrt laengst zu Ende und die Stellung bleibt.
func kfStellung(e []kfEreignis, jetzt time.Time) string {
	for i := len(e) - 1; i >= 0; i-- {
		if e[i].Richtung == "stop" {
			continue
		}
		fahrtEnde := e[i].Zeit.Add(kfFahrzeit)
		for _, s := range e[i+1:] {
			if s.Richtung == "stop" && s.Zeit.Before(fahrtEnde) {
				return "angehalten"
			}
		}
		if jetzt.Before(fahrtEnde) {
			return map[string]string{"auf": "fährt auf", "ab": "fährt zu"}[e[i].Richtung]
		}
		return map[string]string{"auf": "offen", "ab": "zu"}[e[i].Richtung]
	}
	return "unbekannt"
}

func (k *kellerfenster) bediene(mux *http.ServeMux) {
	mux.HandleFunc("/api/kellerfenster", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			jsonAntwort(w, k.stand())
			return
		}
		var a struct {
			Richtung string `json:"richtung"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 256)).Decode(&a) != nil ||
			(a.Richtung != "auf" && a.Richtung != "ab" && a.Richtung != "stop") {
			http.Error(w, "richtung auf, ab oder stop", 400)
			return
		}
		if err := k.fahre(a.Richtung); err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		w.WriteHeader(204)
	})
}
