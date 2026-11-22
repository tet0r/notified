package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestGithubRepoFromSource(t *testing.T) {
	good := map[string]string{
		"https://github.com/linuxserver/docker-sonarr":    "linuxserver/docker-sonarr",
		"https://github.com/immich-app/immich.git":        "immich-app/immich",
		"https://github.com/home-assistant/core/tree/dev": "home-assistant/core",
		"git+https://github.com/Owner/Repo.Name":          "Owner/Repo.Name",
		"https://www.github.com/a/b#readme":               "a/b",
		"  https://github.com/jellyfin/jellyfin  ":        "jellyfin/jellyfin",
	}
	for in, want := range good {
		if got := githubRepoFromSource(in); got != want {
			t.Errorf("githubRepoFromSource(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "https://gitlab.com/a/b", "https://github.com/onlyowner", "https://github.com.evil.com/a/b",
		"javascript:alert(1)", "https://github.com/a/..", "http://example.com/https://github.com/a/b", "https://github.com/a b/c"} {
		if got := githubRepoFromSource(bad); got != "" {
			t.Errorf("githubRepoFromSource(%q) = %q, want nothing", bad, got)
		}
	}
}

func TestGuessedRepoAndDockerHub(t *testing.T) {
	if got := guessedRepo("lscr.io/linuxserver/radarr:latest"); got != "linuxserver/docker-radarr" {
		t.Errorf("lscr: %q", got)
	}
	if got := guessedRepo("nginx:1.27"); got != "" {
		t.Errorf("nginx should have no guess: %q", got)
	}
	cases := map[string]string{
		"nginx:1.27":                "https://hub.docker.com/_/nginx/tags",
		"plexinc/pms-docker:latest": "https://hub.docker.com/r/plexinc/pms-docker/tags",
		"docker.io/library/redis:7": "https://hub.docker.com/_/redis/tags",
		"ghcr.io/immich-app/immich": "",
		"registry.local:5000/a/b:1": "",
	}
	for in, want := range cases {
		if got := dockerHubTagsURL(in); got != want {
			t.Errorf("dockerHubTagsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAddReleaseNotes(t *testing.T) {
	// Lookups run in parallel, so the fake server's counter needs a lock.
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/repos/immich-app/immich/releases/latest":
			w.Write([]byte(`{"tag_name":"v2.3.1","name":"v2.3.1","published_at":"2026-11-18T10:00:00Z","html_url":"https://evil.example/x"}`))
		case "/repos/linuxserver/docker-sonarr/releases/latest":
			w.Write([]byte(`{"tag_name":"4.0.16.2944-ls296","published_at":"2026-11-19T08:00:00Z"}`))
		case "/repos/someone/no-releases/releases/latest":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusForbidden) // rate limited
		}
	}))
	defer srv.Close()
	prev := githubAPI
	githubAPI = srv.URL
	defer func() { githubAPI = prev }()

	a := newTestApp(t)
	a.http = srv.Client()
	items := []pendingUpdate{
		{ContainerID: "c1", Image: "ghcr.io/immich-app/immich-server:release"},
		{ContainerID: "c2", Image: "lscr.io/linuxserver/sonarr:latest"},
		{ContainerID: "c3", Image: "someone/no-releases:latest"},
		{ContainerID: "c4", Image: "nginx:1.27"},
		{ContainerID: "c5", Image: "ghcr.io/x/y:1"},
	}
	labels := map[string]map[string]string{
		"c1": {"org.opencontainers.image.source": "https://github.com/immich-app/immich", "org.opencontainers.image.version": "v2.3.0"},
		"c2": {"org.opencontainers.image.version": "4.0.15.2941-ls295"},
		"c3": {"org.opencontainers.image.source": "https://github.com/someone/no-releases"},
		"c5": {"org.opencontainers.image.source": "https://github.com/x/ratelimited"},
	}
	a.addReleaseNotes(context.Background(), items, labels)

	if it := items[0]; it.NotesURL != "https://github.com/immich-app/immich/releases/tag/v2.3.1" || it.LatestVersion != "v2.3.1" || it.InstalledVersion != "v2.3.0" || it.ReleasedAt == 0 {
		t.Errorf("immich: %+v", it)
	}
	if strings.Contains(items[0].NotesURL, "evil") {
		t.Errorf("a URL from the API response must not be used as-is")
	}
	if it := items[1]; it.NotesURL != "https://github.com/linuxserver/docker-sonarr/releases/tag/4.0.16.2944-ls296" || it.InstalledVersion != "4.0.15.2941-ls295" {
		t.Errorf("linuxserver guess: %+v", it)
	}
	if it := items[2]; it.NotesURL != "https://github.com/someone/no-releases/releases" || it.LatestVersion != "" {
		t.Errorf("no releases: %+v", it)
	}
	if it := items[3]; it.NotesURL != "https://hub.docker.com/_/nginx/tags" {
		t.Errorf("docker hub fallback: %+v", it)
	}
	if it := items[4]; it.NotesURL != "https://github.com/x/ratelimited/releases" {
		t.Errorf("rate limited fallback: %+v", it)
	}

	a.addReleaseNotes(context.Background(), items[:1], labels)
	mu.Lock()
	defer mu.Unlock()
	if hits["/repos/immich-app/immich/releases/latest"] != 1 {
		t.Errorf("the second lookup should come from the cache, GitHub was asked %d times", hits["/repos/immich-app/immich/releases/latest"])
	}
}
