package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const oldID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeEngine is a minimal Docker Engine API that records what was asked of it.
type fakeEngine struct {
	ops        []string
	createBody map[string]any
	names      map[string]string // container id -> name
	running    map[string]bool
	failPull   bool
	failCreate bool
	failStart  bool // only for the new container
	failRemove bool
}

func (f *fakeEngine) handler(t *testing.T) http.Handler {
	fail := func(w http.ResponseWriter, code int, msg string) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"message": msg})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		parts := strings.Split(strings.Trim(p, "/"), "/")
		switch {
		case r.Method == http.MethodGet && p == "/containers/"+oldID+"/json":
			json.NewEncoder(w).Encode(map[string]any{
				"Id": oldID, "Name": "/sonarr",
				"State": map[string]any{"Running": true},
				"Config": map[string]any{
					"Image": "lscr.io/linuxserver/sonarr:latest", "Hostname": oldID[:12],
					"Env":    []string{"PUID=1000", "TZ=America/New_York"},
					"Labels": map[string]string{"com.docker.compose.project": "media", "com.docker.compose.service": "sonarr"},
				},
				"HostConfig": map[string]any{
					"NetworkMode": "media_default", "Binds": []string{"/srv/config/sonarr:/config"},
					"RestartPolicy": map[string]any{"Name": "unless-stopped"},
					"PortBindings":  map[string]any{"8989/tcp": []map[string]string{{"HostPort": "8989"}}},
				},
				"NetworkSettings": map[string]any{"Networks": map[string]any{
					"media_default": map[string]any{"Aliases": []string{"sonarr", oldID[:12]}, "IPAddress": "172.20.0.5", "NetworkID": "n1"},
					"proxy":         map[string]any{"Aliases": []string{"sonarr"}, "IPAMConfig": map[string]any{"IPv4Address": "172.30.0.9"}},
				}},
			})
		case r.Method == http.MethodPost && p == "/images/create":
			f.ops = append(f.ops, "pull "+r.URL.Query().Get("fromImage")+":"+r.URL.Query().Get("tag"))
			io.WriteString(w, `{"status":"Pulling from linuxserver/sonarr"}`+"\n")
			if f.failPull {
				io.WriteString(w, `{"error":"manifest unknown"}`+"\n")
				return
			}
			io.WriteString(w, `{"status":"Status: Downloaded newer image"}`+"\n")
		case r.Method == http.MethodPost && p == "/containers/create":
			if f.failCreate {
				fail(w, http.StatusConflict, "invalid mount config")
				return
			}
			json.NewDecoder(r.Body).Decode(&f.createBody)
			f.names["new1"] = r.URL.Query().Get("name")
			f.ops = append(f.ops, "create "+r.URL.Query().Get("name"))
			json.NewEncoder(w).Encode(map[string]string{"Id": "new1"})
		case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "containers" && parts[2] == "stop":
			f.running[parts[1]] = false
			f.ops = append(f.ops, "stop "+f.names[parts[1]])
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "containers" && parts[2] == "rename":
			f.ops = append(f.ops, "rename "+f.names[parts[1]]+" -> "+r.URL.Query().Get("name"))
			f.names[parts[1]] = r.URL.Query().Get("name")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "containers" && parts[2] == "start":
			if parts[1] == "new1" && f.failStart {
				fail(w, http.StatusInternalServerError, "driver failed programming external connectivity")
				return
			}
			f.running[parts[1]] = true
			f.ops = append(f.ops, "start "+f.names[parts[1]])
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "networks" && parts[2] == "connect":
			f.ops = append(f.ops, "connect "+parts[1])
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && len(parts) == 2 && parts[0] == "containers":
			if parts[1] == oldID && f.failRemove {
				fail(w, http.StatusConflict, "removal in progress")
				return
			}
			f.ops = append(f.ops, "remove "+f.names[parts[1]])
			delete(f.names, parts[1])
			delete(f.running, parts[1])
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newFake(t *testing.T) (*fakeEngine, *dockerHost) {
	f := &fakeEngine{names: map[string]string{oldID: "sonarr"}, running: map[string]bool{oldID: true}}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return f, &dockerHost{base: srv.URL, host: "fake", c: &http.Client{Timeout: 5 * time.Second}}
}

func TestRecreateSuccess(t *testing.T) {
	f, d := newFake(t)
	note, err := d.recreate(context.Background(), oldID)
	if err != nil || note != "" {
		t.Fatalf("recreate: note %q, err %v", note, err)
	}
	want := []string{
		"pull lscr.io/linuxserver/sonarr:latest", "stop sonarr", "rename sonarr -> sonarr-old",
		"create sonarr", "connect proxy", "start sonarr", "remove sonarr-old",
	}
	if !slices.Equal(f.ops, want) {
		t.Errorf("operations:\n got %v\nwant %v", f.ops, want)
	}
	if f.names["new1"] != "sonarr" || !f.running["new1"] || len(f.names) != 1 {
		t.Errorf("end state: names %v running %v", f.names, f.running)
	}

	b := f.createBody
	if b["Image"] != "lscr.io/linuxserver/sonarr:latest" {
		t.Errorf("image not carried over: %v", b["Image"])
	}
	if _, has := b["Hostname"]; has {
		t.Errorf("the old container id must not be reused as hostname: %v", b["Hostname"])
	}
	hc, _ := b["HostConfig"].(map[string]any)
	if hc == nil || hc["NetworkMode"] != "media_default" || hc["Binds"] == nil || hc["PortBindings"] == nil || hc["RestartPolicy"] == nil {
		t.Errorf("host settings not carried over: %v", hc)
	}
	if labels, _ := b["Labels"].(map[string]any); labels["com.docker.compose.project"] != "media" {
		t.Errorf("compose labels not carried over: %v", b["Labels"])
	}
	eps, _ := b["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	ep, _ := eps["media_default"].(map[string]any)
	if len(eps) != 1 || ep == nil {
		t.Fatalf("expected only the primary network at creation, got %v", eps)
	}
	if aliases, _ := ep["Aliases"].([]any); len(aliases) != 1 || aliases[0] != "sonarr" {
		t.Errorf("aliases should keep the service name and drop the old id: %v", ep["Aliases"])
	}
	if _, has := ep["IPAddress"]; has {
		t.Errorf("runtime network fields must not be sent: %v", ep)
	}
}

func TestRecreatePullFailureTouchesNothing(t *testing.T) {
	f, d := newFake(t)
	f.failPull = true
	_, err := d.recreate(context.Background(), oldID)
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("want pull error, got %v", err)
	}
	if len(f.ops) != 1 || f.names[oldID] != "sonarr" || !f.running[oldID] {
		t.Errorf("container was touched: ops %v names %v running %v", f.ops, f.names, f.running)
	}
}

func TestRecreateRestoresWhenCreateFails(t *testing.T) {
	f, d := newFake(t)
	f.failCreate = true
	_, err := d.recreate(context.Background(), oldID)
	if err == nil || !strings.Contains(err.Error(), "original container was restored") {
		t.Fatalf("want restored error, got %v", err)
	}
	if f.names[oldID] != "sonarr" || !f.running[oldID] || len(f.names) != 1 {
		t.Errorf("original not restored: names %v running %v ops %v", f.names, f.running, f.ops)
	}
}

func TestRecreateRestoresWhenStartFails(t *testing.T) {
	f, d := newFake(t)
	f.failStart = true
	_, err := d.recreate(context.Background(), oldID)
	if err == nil || !strings.Contains(err.Error(), "would not start") || !strings.Contains(err.Error(), "original container was restored") {
		t.Fatalf("want start failure with restore, got %v", err)
	}
	if f.names[oldID] != "sonarr" || !f.running[oldID] || len(f.names) != 1 {
		t.Errorf("original not restored or new one left behind: names %v running %v ops %v", f.names, f.running, f.ops)
	}
}

func TestRecreateLeftoverIsANoteNotAFailure(t *testing.T) {
	f, d := newFake(t)
	f.failRemove = true
	note, err := d.recreate(context.Background(), oldID)
	if err != nil || !strings.Contains(note, "sonarr-old") {
		t.Fatalf("want success with a note, got note %q err %v", note, err)
	}
	if !f.running["new1"] {
		t.Errorf("new container should be running: %v", f.running)
	}
}

func TestBuildCreateBodySharedNetwork(t *testing.T) {
	body, extra := buildCreateBody(J{
		"Id":              oldID,
		"Config":          map[string]any{"Image": "qbittorrent:latest", "Hostname": "abc", "ExposedPorts": map[string]any{"8080/tcp": map[string]any{}}},
		"HostConfig":      map[string]any{"NetworkMode": "container:gluetun"},
		"NetworkSettings": map[string]any{"Networks": map[string]any{}},
	})
	for _, k := range []string{"Hostname", "ExposedPorts", "NetworkingConfig"} {
		if _, has := body[k]; has {
			t.Errorf("%s must be omitted when sharing another container's network", k)
		}
	}
	if len(extra) != 0 {
		t.Errorf("no networks expected, got %v", extra)
	}
}

func TestNewDockerHostAddresses(t *testing.T) {
	for addr, want := range map[string]string{
		"http://omv:2375":             "http://omv:2375",
		"tcp://192.168.1.5:2375":      "http://192.168.1.5:2375",
		"http://omv:2375/":            "http://omv:2375",
		"unix:///var/run/docker.sock": "http://docker",
	} {
		d, err := newDockerHost(&Integration{Config: map[string]string{"url": addr}}, time.Second)
		if err != nil || d.base != want {
			t.Errorf("%q: got %v, %v; want %q", addr, d, err, want)
		}
	}
	for _, bad := range []string{"", "omv:2375", "unix://", "ftp://omv"} {
		if _, err := newDockerHost(&Integration{Config: map[string]string{"url": bad}}, time.Second); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestSocketProxyRefusalIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "<html><body><h1>403 Forbidden</h1>\nRequest forbidden by administrative rules.\n</body></html>")
	}))
	defer srv.Close()
	d := &dockerHost{base: srv.URL, host: "omv:2375", c: &http.Client{Timeout: 5 * time.Second}}
	err := d.call(context.Background(), http.MethodPost, "/containers/abc/stop", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "socket proxy") || !strings.Contains(err.Error(), "POST=1") {
		t.Errorf("got %v", err)
	}
}
