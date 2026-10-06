package main

import (
	"testing"
	"time"
)

func TestKfStellung(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	ev := func(sek int, r string) kfEreignis {
		return kfEreignis{Zeit: t0.Add(time.Duration(sek) * time.Second), Richtung: r}
	}
	faelle := []struct {
		name string
		e    []kfEreignis
		nach int
		soll string
	}{
		{"leer", nil, 0, "unbekannt"},
		{"nur stop", []kfEreignis{ev(0, "stop")}, 10, "unbekannt"},
		{"faehrt auf", []kfEreignis{ev(0, "auf")}, 10, "fährt auf"},
		{"offen", []kfEreignis{ev(0, "auf")}, 120, "offen"},
		{"stop waehrend der Fahrt", []kfEreignis{ev(0, "ab"), ev(20, "stop")}, 300, "angehalten"},
		{"stop nach der Fahrt", []kfEreignis{ev(0, "ab"), ev(200, "stop")}, 300, "zu"},
		{"letzter Befehl zaehlt", []kfEreignis{ev(0, "ab"), ev(30, "stop"), ev(100, "auf")}, 400, "offen"},
	}
	for _, f := range faelle {
		if ist := kfStellung(f.e, t0.Add(time.Duration(f.nach)*time.Second)); ist != f.soll {
			t.Errorf("%s: %q, erwartet %q", f.name, ist, f.soll)
		}
	}
}
