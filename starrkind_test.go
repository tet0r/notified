package main

import "testing"

func TestRadarrPostedToSonarrIntegration(t *testing.T) {
	sonarr := &Integration{Type: "sonarr", Name: "Sonarr", Config: map[string]string{}, Events: map[string]EventRule{}}
	grab := J{
		"eventType":    "Grab",
		"instanceName": "Radarr",
		"movie":        map[string]any{"title": "Dune: Part Two", "year": float64(2024), "tmdbId": float64(693134)},
		"release":      map[string]any{"quality": "Bluray-2160p"},
	}
	event, e, ok := parseStarr("sonarr")(sonarr, grab)
	if !ok || event != "grab" {
		t.Fatalf("event %q ok %v", event, ok)
	}
	if e.Title != "Movie grabbed" {
		t.Errorf("title = %q, want Movie grabbed", e.Title)
	}
	if e.Description != "**Dune: Part Two (2024)**" {
		t.Errorf("description = %q", e.Description)
	}
	if e.Author.IconURL != appIconURL("radarr") || e.Thumbnail == nil || e.Thumbnail.URL != appIconURL("radarr") {
		t.Errorf("should carry Radarr's icon, got author %q thumb %+v", e.Author.IconURL, e.Thumbnail)
	}
}

func TestDetectStarrKind(t *testing.T) {
	cases := []struct {
		p    J
		want string
	}{
		{J{"series": map[string]any{"title": "x"}}, "sonarr"},
		{J{"movie": map[string]any{"title": "x"}}, "radarr"},
		{J{"artist": map[string]any{"name": "x"}}, "lidarr"},
		{J{"author": map[string]any{"name": "x"}}, "readarr"},
		{J{"eventType": "Health", "instanceName": "Radarr 4K"}, "radarr"},
		{J{"eventType": "Health", "instanceName": "My Server"}, "sonarr"},
		{J{"eventType": "Test"}, "sonarr"},
	}
	for _, c := range cases {
		if got := detectStarrKind(c.p, "sonarr"); got != c.want {
			t.Errorf("detectStarrKind(%v) = %q, want %q", c.p, got, c.want)
		}
	}
}
