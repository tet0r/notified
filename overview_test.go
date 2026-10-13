package main

import (
	"testing"
	"time"
)

func TestOverviewDue(t *testing.T) {
	loc := time.FixedZone("test", -4*3600)
	at := func(day, hour, min int) time.Time { return time.Date(2026, 10, day, hour, min, 0, 0, loc) }
	cases := []struct {
		name     string
		now      time.Time
		clock    string
		lastSent string
		want     bool
	}{
		{"just after midnight, not sent yet", at(13, 0, 0), "00:00", "2026-10-12", true},
		{"restart 40 minutes after midnight still sends", at(13, 0, 40), "00:00", "2026-10-12", true},
		{"already sent today", at(13, 0, 5), "00:00", "2026-10-13", false},
		{"first start in the afternoon does not send a late one", at(13, 15, 0), "00:00", "", false},
		{"a minute before midnight", at(12, 23, 59), "00:00", "2026-10-12", false},
		{"custom time, before it", at(13, 7, 59), "08:00", "2026-10-12", false},
		{"custom time, at it", at(13, 8, 0), "08:00", "2026-10-12", true},
		{"unreadable time falls back to midnight", at(13, 0, 1), "noon", "", true},
	}
	for _, c := range cases {
		if got, _ := overviewDue(c.now, c.clock, c.lastSent); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseClock(t *testing.T) {
	for in, want := range map[string][2]int{"00:00": {0, 0}, "8:05": {8, 5}, "23:59": {23, 59}, " 12:30 ": {12, 30}} {
		h, m, ok := parseClock(in)
		if !ok || h != want[0] || m != want[1] {
			t.Errorf("parseClock(%q) = %d, %d, %v", in, h, m, ok)
		}
	}
	for _, bad := range []string{"", "24:00", "12:60", "12", "ab:cd", "-1:00"} {
		if _, _, ok := parseClock(bad); ok {
			t.Errorf("parseClock(%q) should fail", bad)
		}
	}
}
