package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestZugangUndAPI(t *testing.T) {
	berlin()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pin"), []byte("1234\n"), 0600)
	os.WriteFile(filepath.Join(dir, "steuerung.html"), []byte("<p>ok</p>"), 0644)
	st := neueSteuerung(filepath.Join(dir, "c.json"), filepath.Join(dir, "s.json"), func(string, ...any) {})
	st.schalte = func(string, bool, int) (float64, error) { return 0, nil }
	zu := neuerZugang(dir)
	mux := http.NewServeMux()
	st.bediene(mux, dir)
	mux.HandleFunc("/api/anmelden", zu.anmelden)
	h := zu.schuetze(mux)

	ruf := func(meth, pfad, body, addr string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(meth, pfad, strings.NewReader(body))
		r.RemoteAddr = addr
		if c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	lan := "192.168.2.50:5000"
	if w := ruf("GET", "/api/status", "", lan, nil); w.Code != 401 {
		t.Fatalf("WLAN ohne PIN: %d", w.Code)
	}
	if w := ruf("GET", "/steuerung", "", lan, nil); w.Code != 200 {
		t.Fatalf("Oberflaeche ohne PIN nicht erreichbar: %d", w.Code)
	}
	if w := ruf("GET", "/api/status", "", "127.0.0.1:4000", nil); w.Code != 200 {
		t.Fatalf("Tablet selbst braucht keine PIN: %d", w.Code)
	}
	if w := ruf("POST", "/api/anmelden", `{"pin":"9999"}`, lan, nil); w.Code != 403 {
		t.Fatalf("falsche PIN angenommen: %d", w.Code)
	}
	w := ruf("POST", "/api/anmelden", `{"pin":"1234"}`, lan, nil)
	if w.Code != 204 || len(w.Result().Cookies()) == 0 {
		t.Fatalf("Anmeldung: %d", w.Code)
	}
	k := w.Result().Cookies()[0]
	if w := ruf("GET", "/api/status", "", lan, k); w.Code != 200 {
		t.Fatalf("mit Sitzung: %d", w.Code)
	}
	// Prognose, Einstellung, Plan
	start := tagesAnfang(time.Now().In(ort))
	body := `{"stunden":[[` + itoa(int(start.Add(12*time.Hour).Unix())) + `,5.0]]}`
	if w := ruf("POST", "/prognose", body, "127.0.0.1:1", nil); w.Code != 204 {
		t.Fatalf("Prognose: %d", w.Code)
	}
	if len(st.prognose) != 1 {
		t.Fatal("Prognose nicht uebernommen")
	}
	c := standardEntfeuchter()
	c.Modus = "kaputt"
	if w := ruf("POST", "/api/einstellungen", `{"id":"entfeuchter","modus":"kaputt"}`, lan, k); w.Code != 400 {
		t.Fatalf("unsinnige Einstellung angenommen: %d", w.Code)
	}
	if w := ruf("POST", "/api/tablet", `{"hell_tag":5}`, lan, k); w.Code != 400 {
		t.Fatalf("Helligkeit 5 angenommen: %d", w.Code)
	}
}
