package main

import "testing"

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"0.3.0", "0.2.0", true},
		{"v0.3.0", "0.2.9", true},
		{"1.0.0", "0.9.9", true},
		{"0.2.10", "0.2.9", true},
		{"0.2.0", "0.2.0", false},
		{"0.2.0", "0.3.0", false},
		// `git describe` output for a build a few commits past a tag.
		{"0.2.0", "0.2.0-3-gabc1234", false},
		{"0.3.0", "0.2.0-3-gabc1234", true},
		// Builds without a real version never claim an update.
		{"0.3.0", "dev", false},
		{"0.3.0", "abc1234", false},
		{"", "0.2.0", false},
		{"nightly", "0.2.0", false},
	}
	for _, c := range cases {
		if got := newerVersion(c.latest, c.current); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}
