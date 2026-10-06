package main

// Welche Zeiten an einem Tag gelten. Montag ist 0, Sonntag 6. An Feiertagen
// in NRW und in den Ferien gelten die Sonntagszeiten, fuer Lueftung und
// Entfeuchter gleichermassen: im Spielekeller schlaeft dann jemand laenger.
// Das Ferienende stellt man in der Lueftung ein (Vorgabe vom 06.10.2026).

import (
	"sync/atomic"
	"time"
)

// ferienBis: Unix-Zeit vom Ende des letzten Ferientags, 0 heisst keine Ferien.
var ferienBis atomic.Int64

func setzeFerien(datum string) {
	if d, err := time.ParseInLocation("2006-01-02", datum, ort); err == nil {
		ferienBis.Store(d.AddDate(0, 0, 1).Unix())
		return
	}
	ferienBis.Store(0)
}

func ferien(t time.Time) bool { return t.Unix() < ferienBis.Load() }

// ostern nach Gauss in der Fassung von Lichtenberg, gregorianisch.
func ostern(jahr int) time.Time {
	a := jahr % 19
	b, c := jahr/100, jahr%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	monat := (h + l - 7*m + 114) / 31
	tag := (h+l-7*m+114)%31 + 1
	return time.Date(jahr, time.Month(monat), tag, 0, 0, 0, 0, ort)
}

// feiertag: gesetzliche Feiertage in Nordrhein-Westfalen.
func feiertag(t time.Time) bool {
	t = t.In(ort)
	j, m, d := t.Date()
	switch {
	case m == 1 && d == 1, m == 5 && d == 1, m == 10 && d == 3, m == 11 && d == 1,
		m == 12 && (d == 25 || d == 26):
		return true
	}
	o := ostern(j)
	heute := time.Date(j, m, d, 0, 0, 0, 0, ort)
	for _, n := range []int{-2, 1, 39, 50, 60} { // Karfreitag bis Fronleichnam
		if o.AddDate(0, 0, n).Equal(heute) {
			return true
		}
	}
	return false
}

func tagIndex(t time.Time) int {
	if feiertag(t) || ferien(t) {
		return 6
	}
	return (int(t.Weekday()) + 6) % 7
}
