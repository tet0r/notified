package main

import (
	"slices"
	"testing"
)

func TestIconCandidates(t *testing.T) {
	cases := []struct {
		image, container string
		want             string   // must be among the candidates
		first            string   // expected first candidate, if set
		absent           []string // must not be suggested
	}{
		{image: "lscr.io/linuxserver/sonarr:latest", container: "sonarr", want: "sonarr", first: "sonarr", absent: []string{"linuxserver", "lscr.io"}},
		{image: "ghcr.io/immich-app/immich-server:release", container: "immich_server", want: "immich", first: "immich-server"},
		{image: "plexinc/pms-docker:latest", container: "plex", want: "plex", first: "plex"},
		{image: "ghcr.io/home-assistant/home-assistant:stable", container: "hass", want: "home-assistant", first: "home-assistant"},
		{image: "nginx:1.27", container: "web", want: "nginx", first: "nginx", absent: []string{"library"}},
		{image: "registry.example.com:5000/team/grafana:11", container: "metrics", want: "grafana", first: "grafana"},
		{image: "postgres@sha256:abc", container: "db", want: "postgres", first: "postgres"},
		{image: "portainer/portainer-ce:latest", container: "portainer", want: "portainer", first: "portainer-ce"},
	}
	for _, c := range cases {
		got := iconCandidates(c.image, c.container)
		if !slices.Contains(got, c.want) {
			t.Errorf("iconCandidates(%q, %q) = %v, missing %q", c.image, c.container, got, c.want)
		}
		if c.first != "" && (len(got) == 0 || got[0] != c.first) {
			t.Errorf("iconCandidates(%q, %q) = %v, want first %q", c.image, c.container, got, c.first)
		}
		for _, bad := range c.absent {
			if slices.Contains(got, bad) {
				t.Errorf("iconCandidates(%q, %q) = %v, should not contain %q", c.image, c.container, got, bad)
			}
		}
	}
}

func TestImageFilename(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}
	if got := imageFilename(jpeg); got != "thumb.jpg" {
		t.Errorf("jpeg: got %q", got)
	}
	if got := imageFilename(png); got != "thumb.png" {
		t.Errorf("png: got %q", got)
	}
	for _, notImage := range [][]byte{nil, []byte("<html><body>login</body></html>"), []byte(`{"error":"nope"}`), []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")} {
		if got := imageFilename(notImage); got != "" {
			t.Errorf("non-image %q: got %q", notImage, got)
		}
	}
}
