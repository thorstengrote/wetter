package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestTuyaVerschluesselung(t *testing.T) {
	key := []byte("0123456789abcdef")
	klar := []byte(`{"dps":{"1":true}}`)
	v, err := aesECB(key, pkcs7(klar), true)
	if err != nil {
		t.Fatal(err)
	}
	z, err := entpacke(key, v)
	if err != nil || !bytes.Equal(z, klar) {
		t.Fatalf("%q %v", z, err)
	}
}

func TestTuyaRahmen(t *testing.T) {
	// Antwort eines Geraets: Rueckgabecode 0, dann Nutzlast
	nutz := append([]byte{0, 0, 0, 0}, []byte("hallo")...)
	r := rahmen(7, 10, nutz)
	if binary.BigEndian.Uint32(r[len(r)-4:]) != 0x0000AA55 {
		t.Fatal("Suffix fehlt")
	}
	cmd, n, err := leseRahmen(bytes.NewReader(r))
	if err != nil || cmd != 10 || string(n) != "hallo" {
		t.Fatalf("%d %q %v", cmd, n, err)
	}
}

// Liest den Zustand der Vorgartenbeleuchtung im echten Heimnetz, schaltet
// nichts. Nur mit TUYA_ECHT=<ip> und ~/.tuya-geraete.json.
func TestTuyaEchtLesen(t *testing.T) {
	ip := os.Getenv("TUYA_ECHT")
	if ip == "" {
		t.Skip("TUYA_ECHT nicht gesetzt")
	}
	h, _ := os.UserHomeDir()
	roh, err := os.ReadFile(filepath.Join(h, ".tuya-geraete.json"))
	if err != nil {
		t.Skip(err)
	}
	var gs []tuyaGeraet
	json.Unmarshal(roh, &gs)
	tu := &tuya{sag: t.Logf}
	name := os.Getenv("TUYA_NAME")
	if name == "" {
		name = "Vorgarten Beleuchtung"
	}
	for _, g := range gs {
		if g.Name == name {
			dps, err := tu.anfrage(g, ip, tuyaAbfrage, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %v", name, dps)
			if _, ok := dps["1"]; !ok {
				t.Fatalf("kein Datenpunkt 1: %v", dps)
			}
			return
		}
	}
	t.Skip("Geraet nicht in der Liste")
}
