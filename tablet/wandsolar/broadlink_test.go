package main

import (
	"net"
	"os"
	"testing"
	"time"
)

func TestBlPruefsumme(t *testing.T) {
	if blPruefsumme([]byte{1, 2, 3}) != 0xBEAF+6 {
		t.Fatal("Pruefsumme")
	}
	k := []byte("0123456789abcdef")
	if string(blCBC(k, blCBC(k, k, true), false)) != string(k) {
		t.Fatal("CBC")
	}
}

// Nur mit BL_ECHT=<eigene IP>: sucht, meldet an, liest den Zustand. Schaltet nichts.
func TestBroadlinkEchtLesen(t *testing.T) {
	ip := os.Getenv("BL_ECHT")
	if ip == "" {
		t.Skip()
	}
	gef, err := blSuche(net.ParseIP(ip), 4*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for mac, st := range gef {
		t.Logf("gefunden %s unter %s, Typ 0x%04x", mac, st.IP, st.Typ)
		if err := st.anmelden(); err != nil {
			t.Fatalf("Anmeldung: %v", err)
		}
		an, err := st.zustand()
		if err != nil {
			t.Fatalf("Zustand: %v", err)
		}
		t.Logf("angemeldet, Sitzung %08x, Steckdose an: %v", st.id, an)
	}
	if len(gef) == 0 {
		t.Fatal("nichts gefunden")
	}
}
