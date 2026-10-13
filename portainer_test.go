package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPortainerBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://portainer:9443":                           "https://portainer:9443",
		"https://portainer:9443/":                          "https://portainer:9443",
		" https://portainer:9443/#!/2/docker/containers ":  "https://portainer:9443",
		"https://portainer:9443/#!/home":                   "https://portainer:9443",
		"https://portainer:9443/api":                       "https://portainer:9443",
		"https://portainer:9443/api/":                      "https://portainer:9443",
		"http://192.168.1.10:9000/?x=1":                    "http://192.168.1.10:9000",
		"https://example.com/portainer/":                   "https://example.com/portainer",
		"https://example.com/portainer/#!/3/docker/stacks": "https://example.com/portainer",
	}
	for in, want := range cases {
		got, err := portainerBaseURL(in)
		if err != nil || got != want {
			t.Errorf("portainerBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "portainer:9443", "192.168.1.10:9000", "ftp://x"} {
		if got, err := portainerBaseURL(bad); err == nil {
			t.Errorf("portainerBaseURL(%q) = %q, want an error", bad, got)
		}
	}
}

func TestPortainerCallErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<!doctype html><html><body>Sign in</body></html>"))
		case "/api/shape":
			w.Write([]byte(`{"not":"an array"}`))
		case "/api/ok":
			w.Write([]byte(`[{"Id":1}]`))
		case "/api/empty":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	p := &portainer{base: srv.URL, key: "k", c: &http.Client{Timeout: 5 * time.Second}}
	var out []struct{ Id int }

	err := p.call(context.Background(), http.MethodGet, "/api/html?secret=1", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "web page instead of API data for /api/html.") {
		t.Errorf("HTML answer: got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks the query string: %v", err)
	}
	err = p.call(context.Background(), http.MethodGet, "/api/shape", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "could not read Portainer's answer for /api/shape") {
		t.Errorf("wrong shape: got %v", err)
	}
	if err := p.call(context.Background(), http.MethodGet, "/api/ok", nil, &out); err != nil || len(out) != 1 {
		t.Errorf("ok: got %v, %v", out, err)
	}
	if err := p.call(context.Background(), http.MethodGet, "/api/empty", nil, &out); err != nil {
		t.Errorf("204: got %v", err)
	}
}

func TestRegistryLookupRetriesWhenThrottled(t *testing.T) {
	registryGap, registryBackoff = time.Millisecond, 5*time.Millisecond
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"message":"toomanyrequests: retry-after: 785.35µs, allowed: 44000/minute"}`))
			return
		}
		w.Write([]byte(`{"Descriptor":{"digest":"sha256:abc"}}`))
	}))
	defer srv.Close()
	p := &portainer{base: srv.URL, key: "k", c: &http.Client{Timeout: 5 * time.Second}}
	var dist struct {
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"Descriptor"`
	}
	if err := p.registryLookup(context.Background(), "/api/x", &dist); err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if hits != 3 || dist.Descriptor.Digest != "sha256:abc" {
		t.Errorf("hits = %d, digest = %q", hits, dist.Descriptor.Digest)
	}
}
