package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEngine answers Docker API calls with a delay, like a remote host.
func fakeCall(containers []portainerContainer, delay time.Duration, inflight, peak *atomic.Int32) dockerCall {
	return func(ctx context.Context, method, apiPath string, body, out any) error {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer inflight.Add(-1)
		var v any
		switch {
		case apiPath == "/containers/json":
			v = containers
		case strings.HasPrefix(apiPath, "/images/"):
			time.Sleep(delay)
			v = map[string]any{"RepoDigests": []string{"x@sha256:old"}}
		case strings.HasPrefix(apiPath, "/distribution/"):
			time.Sleep(delay)
			ref := strings.TrimSuffix(strings.TrimPrefix(apiPath, "/distribution/"), "/json")
			d := "sha256:old"
			if strings.Contains(ref, "new") {
				d = "sha256:new"
			}
			v = map[string]any{"Descriptor": map[string]string{"digest": d}}
		}
		data, _ := json.Marshal(v)
		return json.Unmarshal(data, out)
	}
}

func TestScanRunsInParallelAndKeepsOrder(t *testing.T) {
	registryGap = 5 * time.Millisecond
	var cs []portainerContainer
	for i := 0; i < 12; i++ {
		img := fmt.Sprintf("ghcr.io/org/app%d:latest", i)
		if i%3 == 0 {
			img = fmt.Sprintf("ghcr.io/org/new%d:latest", i) // these have an update
		}
		cs = append(cs, portainerContainer{ID: fmt.Sprintf("c%02d", i), Names: []string{fmt.Sprintf("/svc%02d", i)}, Image: img, ImageID: fmt.Sprintf("sha256:i%d", i)})
	}
	var inflight, peak atomic.Int32
	call := fakeCall(cs, 100*time.Millisecond, &inflight, &peak)
	report := portainerReport{}
	var errs []string
	start := time.Now()
	scanContainers(context.Background(), call, scanOpts{envName: "local"}, &report, func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) })
	took := time.Since(start)

	// One at a time this is 12 inspections + 12 lookups at 100ms each, ~2.4s.
	if took > 1200*time.Millisecond {
		t.Errorf("scan took %v; it should run requests in parallel", took)
	}
	if peak.Load() < 3 {
		t.Errorf("at most %d requests ran at once", peak.Load())
	}
	if len(errs) > 0 || report.Checked != 12 {
		t.Fatalf("checked %d, errors %v", report.Checked, errs)
	}
	var names []string
	for _, it := range report.Items {
		names = append(names, it.Name)
	}
	if strings.Join(names, ",") != "svc00,svc03,svc06,svc09" {
		t.Errorf("updates found, in order: %v", names)
	}
}

func TestRegistryPacingIsPerRegistry(t *testing.T) {
	registryGap = 100 * time.Millisecond
	defer func() { registryGap = 150 * time.Millisecond }()
	p := &pacer{next: map[string]time.Time{}}
	start := time.Now()
	for i := 0; i < 3; i++ {
		p.wait(context.Background(), "docker.io")
	}
	sameRegistry := time.Since(start)
	start = time.Now()
	for _, r := range []string{"ghcr.io", "lscr.io", "quay.io"} {
		p.wait(context.Background(), r)
	}
	if sameRegistry < 180*time.Millisecond {
		t.Errorf("three lookups to one registry took %v; they should be spaced out", sameRegistry)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Errorf("different registries should not wait for each other")
	}
	for in, want := range map[string]string{
		"nginx:1.27": "docker.io", "plexinc/pms-docker": "docker.io", "ghcr.io/a/b:1": "ghcr.io",
		"lscr.io/linuxserver/sonarr": "lscr.io", "registry.local:5000/x": "registry.local:5000", "localhost/app": "localhost",
	} {
		if got := registryOf(in); got != want {
			t.Errorf("registryOf(%q) = %q, want %q", in, got, want)
		}
	}
}
