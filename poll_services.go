package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// ---- Docker containers ----

type dockerContainer struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

type containerState struct {
	Running bool   `json:"running"`
	Health  string `json:"health"` // "", "healthy", "unhealthy"
	Image   string `json:"image"`
}

func pollDocker(ctx context.Context, a *App, in *Integration) error {
	socket := firstNonEmpty(in.Config["socket"], "/var/run/docker.sock")
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
	defer client.CloseIdleConnections()

	var list []dockerContainer
	if err := getJSON(ctx, client, "http://docker/containers/json?all=1", nil, &list); err != nil {
		return fmt.Errorf("docker socket %s: %w", socket, err)
	}

	ignore := splitList(in.Config["ignore"])
	current := map[string]containerState{}
	for _, c := range list {
		if len(c.Names) == 0 {
			continue
		}
		name := strings.TrimPrefix(c.Names[0], "/")
		skip := false
		for _, pat := range ignore {
			if ok, _ := path.Match(pat, name); ok {
				skip = true
			}
		}
		if skip {
			continue
		}
		cs := containerState{Running: c.State == "running", Image: c.Image}
		switch {
		case strings.Contains(c.Status, "(unhealthy)"):
			cs.Health = "unhealthy"
		case strings.Contains(c.Status, "(healthy)"):
			cs.Health = "healthy"
		}
		current[name] = cs
	}

	prevJSON, seen := a.stateGet(in.ID, "containers")
	prev := map[string]containerState{}
	json.Unmarshal([]byte(prevJSON), &prev)
	curJSON, _ := json.Marshal(current)
	a.stateSet(in.ID, "containers", string(curJSON))
	if !seen {
		return nil // first run only records a baseline
	}

	notify := func(event, title string, color int, name string, cs containerState) {
		e := Embed{Title: title, Color: color, Author: &EmbedAuthor{Name: in.Name}}
		e.Description = "**" + name + "**"
		e.Add("Image", cs.Image, false)
		e.Thumb(a.containerIcon(ctx, cs.Image, name))
		a.Dispatch(in, event, e, nil)
	}
	for name, cs := range current {
		old, existed := prev[name]
		switch {
		case cs.Running && (!existed || !old.Running):
			notify("container_started", "Container started", colorGreen, name, cs)
		case !cs.Running && existed && old.Running:
			notify("container_stopped", "Container stopped", colorRed, name, cs)
		case cs.Running && cs.Health == "unhealthy" && old.Health != "unhealthy":
			notify("container_unhealthy", "Container unhealthy", colorRed, name, cs)
		case cs.Running && cs.Health == "healthy" && old.Health == "unhealthy":
			notify("container_healthy", "Container healthy again", colorGreen, name, cs)
		}
	}
	for name, old := range prev {
		if _, ok := current[name]; !ok && old.Running {
			notify("container_stopped", "Container removed", colorRed, name, old)
		}
	}
	return nil
}

// ---- Service checks ----

func checkTarget(ctx context.Context, cfg map[string]string) error {
	target := strings.TrimSpace(cfg["target"])
	if target == "" {
		return errors.New("no target configured")
	}
	timeout := time.Duration(cfgInt(cfg, "timeout", 10)) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", strings.TrimPrefix(target, "tcp://"))
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	}

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: cfgBool(cfg, "insecure_tls")},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Notified/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if want := cfgInt(cfg, "expected_status", 0); want > 0 {
		if resp.StatusCode != want {
			return fmt.Errorf("HTTP %d (expected %d)", resp.StatusCode, want)
		}
	} else if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// pollHealth returns nil even when the target is down: an outage is a result
// to report, not a poller malfunction.
func pollHealth(ctx context.Context, a *App, in *Integration) error {
	if strings.TrimSpace(in.Config["target"]) == "" {
		return errors.New("no target configured")
	}
	need := cfgInt(in.Config, "failures", 3)
	if need < 1 {
		need = 1
	}
	prev, _ := a.stateGet(in.ID, "fails")
	fails, _ := strconv.Atoi(prev)
	target := in.Config["target"]

	if err := checkTarget(ctx, in.Config); err != nil {
		fails++
		if fails == need {
			e := Embed{Title: in.Name + " is down", Color: colorRed}
			e.Add("Target", target, false)
			e.Add("Error", trunc(err.Error(), 500), false)
			a.Dispatch(in, "down", e, nil)
			a.stateSet(in.ID, "down_since", strconv.FormatInt(time.Now().Unix(), 10))
		}
		if fails > need {
			fails = need + 1
		}
		a.stateSet(in.ID, "fails", strconv.Itoa(fails))
		return nil
	}
	if fails >= need {
		e := Embed{Title: in.Name + " is back up", Color: colorGreen}
		e.Add("Target", target, false)
		if since, ok := a.stateGet(in.ID, "down_since"); ok {
			if unix, err := strconv.ParseInt(since, 10, 64); err == nil {
				e.Add("Downtime", humanDuration(time.Since(time.Unix(unix, 0))), true)
			}
		}
		a.Dispatch(in, "up", e, nil)
	}
	if fails > 0 {
		a.stateDel(in.ID, "fails")
		a.stateDel(in.ID, "down_since")
	}
	return nil
}
