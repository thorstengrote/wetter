package main

import (
	"encoding/json"
	"testing"
	"time"
)

// Box n-1, Repeater n-2 im Keller. Shelly und zwei Handys am Repeater, eins
// davon im Gastnetz, ein Handy und ein iPad woanders.
func TestHandysAmSpielkellerRepeater(t *testing.T) {
	roh := `{"nodes":[
 {"uid":"n-1","device_name":"fritz.box","mesh_role":"master"},
 {"uid":"n-2","device_name":"fritz.repeater","mesh_role":"slave"},
 {"uid":"n-3","device_name":"shellyplug","ip_addresses":[{"value":"192.168.2.160/24"}],
  "node_interfaces":[{"node_links":[{"state":"CONNECTED","node_1_uid":"n-2","node_2_uid":"n-3"}]}]},
 {"uid":"n-4","device_name":"iPhone","device_mac_address":"A","ip_addresses":[{"value":"192.168.2.173/24"}],
  "node_interfaces":[{"node_links":[{"state":"CONNECTED","node_1_uid":"n-2","node_2_uid":"n-4","rx_rcpi":-60}]}]},
 {"uid":"n-5","device_name":"iPhone","device_mac_address":"B","ip_addresses":[{"value":"192.168.179.11/24"}],
  "node_interfaces":[{"node_links":[{"state":"CONNECTED","node_1_uid":"n-2","node_2_uid":"n-5"}]}]},
 {"uid":"n-6","device_name":"iPhone","device_mac_address":"C","ip_addresses":[{"value":"192.168.2.130/24"}],
  "node_interfaces":[{"node_links":[{"state":"CONNECTED","node_1_uid":"n-1","node_2_uid":"n-6"}]}]},
 {"uid":"n-7","device_name":"iPad","ip_addresses":[{"value":"192.168.2.157/24"}],
  "node_interfaces":[{"node_links":[{"state":"CONNECTED","node_1_uid":"n-2","node_2_uid":"n-7"}]}]},
 {"uid":"n-8","device_name":"iPhone","device_mac_address":"D",
  "node_interfaces":[{"node_links":[{"state":"DISCONNECTED","node_1_uid":"n-2","node_2_uid":"n-8"}]}]}
]}`
	var m meshListe
	if err := json.Unmarshal([]byte(roh), &m); err != nil {
		t.Fatal(err)
	}
	h, err := handysAm(m, "192.168.2.160")
	if err != nil || len(h) != 2 || h[0].MAC != "A" || h[0].RCPI != -60 || !h[1].Gast {
		t.Fatalf("%v %+v", err, h)
	}
	if _, err := handysAm(m, "192.168.2.99"); err == nil {
		t.Fatal("unbekannter Shelly ohne Fehler")
	}
}

// Erst nach 10 Minuten zaehlt ein Handy; kurz im Keller zaehlt nicht;
// ein Aussetzer von 4 Minuten bricht nicht ab.
func TestAnwesenheitBleiben(t *testing.T) {
	a := &anwesenheit{}
	t0 := time.Date(2026, 10, 14, 23, 0, 0, 0, berlin())
	ben := []anwGeraet{{MAC: "A"}}
	for i := 0; i <= 4; i++ { // 8 Minuten
		a.merke(ben, t0.Add(time.Duration(i)*2*time.Minute))
	}
	if n := a.zaehle(t0.Add(8 * time.Minute)); n != 0 {
		t.Fatalf("nach 8 min: %d", n)
	}
	a.merke(ben, t0.Add(10*time.Minute))
	if n := a.zaehle(t0.Add(10 * time.Minute)); n != 1 {
		t.Fatalf("nach 10 min: %d", n)
	}
	a.merke(nil, t0.Add(12*time.Minute))
	a.merke(ben, t0.Add(14*time.Minute))
	if n := a.zaehle(t0.Add(14 * time.Minute)); n != 1 {
		t.Fatalf("Aussetzer: %d", n)
	}
	a.merke(nil, t0.Add(30*time.Minute))
	if n := a.zaehle(t0.Add(30 * time.Minute)); n != 0 {
		t.Fatalf("weg: %d", n)
	}
}

// Nacht zum 14.10.: 59 % flach, dann Anstieg; Entfeuchter sperrt.
func TestAtemfeuchte(t *testing.T) {
	var a atem
	t0 := time.Date(2026, 10, 13, 22, 0, 0, 0, berlin())
	for m := 0; m <= 120; m += 3 {
		a.merke(t0.Add(time.Duration(m)*time.Minute), 59, 20.1, false)
	}
	if da, sicher := a.zeigt(t0.Add(120 * time.Minute)); da || !sicher {
		t.Fatalf("flach: %v %v", da, sicher)
	}
	t1 := t0.Add(120 * time.Minute)
	for m := 3; m <= 90; m += 3 {
		rh := 59.0
		if m > 30 {
			rh = 60
		}
		if m > 60 {
			rh = 61
		}
		a.merke(t1.Add(time.Duration(m)*time.Minute), rh, 20.1, false)
	}
	if da, sicher := a.zeigt(t1.Add(90 * time.Minute)); !da || !sicher {
		t.Fatalf("Anstieg: %v %v", da, sicher)
	}
	a.merke(t1.Add(93*time.Minute), 61, 20.1, true)
	if _, sicher := a.zeigt(t1.Add(93 * time.Minute)); sicher {
		t.Fatal("Entfeuchter laeuft, trotzdem sicher")
	}
}
