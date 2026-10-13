package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// The Portainer integration finds running containers whose image tag has a
// newer build in the registry and lets the user recreate them from the web
// UI. It never touches Notified's own container, containers in stacks that
// Portainer did not create ("Limited" control in Portainer), swarm tasks, or
// images pinned to a digest.

// selfImageRepo identifies Notified's own image so it is never offered for update.
const selfImageRepo = "tet0r/notified"

type portainer struct {
	base string
	key  string
	c    *http.Client
}

// portainerBaseURL reduces whatever the user pasted to the address the API
// lives under. People often paste a page URL from the browser, such as
// https://host:9443/#!/2/docker/containers, so the fragment, query and a
// trailing /api are dropped. A sub-path (reverse proxy) is kept.
func portainerBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("URL and access token are required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("the Portainer URL must start with http:// or https://, for example https://portainer:9443")
	}
	u.Fragment, u.RawQuery, u.User = "", "", nil
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func newPortainer(in *Integration, timeout time.Duration) (*portainer, error) {
	base, err := portainerBaseURL(in.Config["url"])
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(in.Config["apikey"])
	if key == "" {
		return nil, errors.New("URL and access token are required")
	}
	return &portainer{base: base, key: key, c: &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: cfgBool(in.Config, "insecure_tls")},
		},
	}}, nil
}

// call performs one API request. out may be nil. A 204 leaves out untouched.
func (p *portainer) call(ctx context.Context, method, apiPath string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+apiPath, rd)
	if err != nil {
		return errors.New("invalid Portainer URL")
	}
	req.Header.Set("X-API-Key", p.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.c.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach Portainer at %s: %s", req.URL.Host, unwrapNetErr(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("Portainer rejected the access token (HTTP 401)")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		var e struct {
			Message string `json:"message"`
			Details string `json:"details"`
		}
		json.Unmarshal(raw, &e)
		msg := strings.TrimSpace(e.Message + " " + e.Details)
		if msg == "" {
			msg = trunc(strings.TrimSpace(string(raw)), 200)
		}
		return fmt.Errorf("Portainer returned HTTP %d: %s", resp.StatusCode, trunc(msg, 300))
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		what, _, _ := strings.Cut(apiPath, "?")
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("<")) || strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
			// A 200 with HTML means something other than the API answered:
			// a wrong URL, or a login page from a reverse proxy in front.
			return fmt.Errorf("%s returned a web page instead of API data for %s. Use Portainer's own address (for example https://portainer:9443) and make sure no login proxy sits in front of its API", req.URL.Host, what)
		}
		return fmt.Errorf("could not read Portainer's answer for %s: %v", what, err)
	}
	return nil
}

// registryLookup is one paced, retried registry lookup through Portainer.
func (p *portainer) registryLookup(ctx context.Context, apiPath string, out any) error {
	return retryThrottled(ctx, func() error { return p.call(ctx, http.MethodGet, apiPath, nil, out) })
}

type pendingUpdate struct {
	EnvID       int64  `json:"env_id"`
	EnvName     string `json:"env_name"`
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	Image       string `json:"image"`
	Stack       string `json:"stack,omitempty"`
	Latest      string `json:"latest"` // registry digest
}

type portainerReport struct {
	CheckedAt int64           `json:"checked_at"`
	Items     []pendingUpdate `json:"items"`
	Checked   int             `json:"checked"` // containers compared against the registry
	Limited   int             `json:"limited"` // skipped: stack not managed by Portainer
	Ignored   int             `json:"ignored"` // skipped: ignore list, Notified itself, pinned or local images
	Names     []string        `json:"names"`   // containers the user could choose to ignore
	Errors    []string        `json:"errors"`
}

func (a *App) portainerReport(id int64) portainerReport {
	var r portainerReport
	if v, ok := a.stateGet(id, "report"); ok {
		json.Unmarshal([]byte(v), &r)
	}
	if r.Items == nil {
		r.Items = []pendingUpdate{}
	}
	if r.Errors == nil {
		r.Errors = []string{}
	}
	return r
}

func (a *App) savePortainerReport(id int64, r portainerReport) {
	data, _ := json.Marshal(r)
	a.stateSet(id, "report", string(data))
}

type portainerContainer struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Labels  map[string]string `json:"Labels"`
}

var imageRefRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

// imageRepo strips the tag from an image reference, keeping any registry port.
func imageRepo(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// isSelf reports whether a container is this Notified instance.
func isSelf(c portainerContainer) bool {
	// Inside Docker the hostname defaults to the short container id.
	if host, _ := os.Hostname(); len(host) >= 12 && strings.HasPrefix(c.ID, host) {
		return true
	}
	if strings.HasSuffix(strings.ToLower(imageRepo(c.Image)), selfImageRepo) {
		return true
	}
	return strings.Contains(strings.ToLower(c.Labels["org.opencontainers.image.source"]), selfImageRepo)
}

func digestOf(repoDigest string) string {
	if _, d, ok := strings.Cut(repoDigest, "@"); ok {
		return d
	}
	return repoDigest
}

// pollPortainer checks every Docker environment Portainer knows about.
func pollPortainer(ctx context.Context, a *App, in *Integration) error {
	p, err := newPortainer(in, 60*time.Second)
	if err != nil {
		return err
	}
	var envs []struct {
		ID     int64  `json:"Id"`
		Name   string `json:"Name"`
		Type   int    `json:"Type"`
		Status int    `json:"Status"`
	}
	if err := p.call(ctx, http.MethodGet, "/api/endpoints?excludeSnapshots=true", nil, &envs); err != nil {
		return err
	}

	ignore := splitList(in.Config["ignore"])
	report := portainerReport{CheckedAt: time.Now().Unix(), Items: []pendingUpdate{}, Errors: []string{}}
	addErr := func(format string, args ...any) {
		if len(report.Errors) < 10 {
			report.Errors = append(report.Errors, fmt.Sprintf(format, args...))
		}
	}

	for _, env := range envs {
		// Docker environments only (1 socket/API, 2 agent, 4 edge agent), and not marked down.
		if (env.Type != 1 && env.Type != 2 && env.Type != 4) || env.Status == 2 {
			continue
		}
		// Stacks Portainer manages in this environment. A compose project that
		// is not in this list was created outside Portainer: "Limited" control.
		var stacks []struct {
			Name string `json:"Name"`
			Type int    `json:"Type"`
		}
		filter := url.QueryEscape(fmt.Sprintf(`{"EndpointID":%d}`, env.ID))
		if err := p.call(ctx, http.MethodGet, "/api/stacks?filters="+filter, nil, &stacks); err != nil {
			addErr("%s: could not list stacks: %v", env.Name, err)
			continue
		}
		managed := map[string]bool{}
		for _, s := range stacks {
			managed[s.Name] = true
		}
		prefix := fmt.Sprintf("/api/endpoints/%d/docker", env.ID)
		call := func(ctx context.Context, method, apiPath string, body, out any) error {
			return p.call(ctx, method, prefix+apiPath, body, out)
		}
		scanContainers(ctx, call, scanOpts{envID: env.ID, envName: env.Name, managed: managed, ignore: ignore}, &report, addErr)
	}
	a.finishScan(ctx, in, report)
	if len(report.Errors) > 0 {
		return errors.New(report.Errors[0])
	}
	return nil
}

// ---- HTTP handlers ----

type containerUpdatesOut struct {
	IntegrationID   int64  `json:"integration_id"`
	IntegrationName string `json:"integration_name"`
	portainerReport
}

func (a *App) containerUpdates() ([]containerUpdatesOut, error) {
	ins, err := a.listIntegrations()
	if err != nil {
		return nil, err
	}
	out := []containerUpdatesOut{}
	for _, in := range ins {
		if isUpdateType(in.Type) && in.Enabled {
			out = append(out, containerUpdatesOut{in.ID, in.Name, a.portainerReport(in.ID)})
		}
	}
	return out, nil
}

func (a *App) handleContainerUpdates(w http.ResponseWriter, r *http.Request) {
	out, err := a.containerUpdates()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// isUpdateType reports whether an integration type offers container updates.
func isUpdateType(typ string) bool { return typ == "portainer" || typ == "dockerhost" }

// handleContainerUpdatesCheck re-runs the check for every update integration.
func (a *App) handleContainerUpdatesCheck(w http.ResponseWriter, r *http.Request) {
	ins, err := a.listIntegrations()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, in := range ins {
		if isUpdateType(in.Type) && in.Enabled {
			a.runPoll(r.Context(), in)
		}
	}
	a.handleContainerUpdates(w, r)
}

// handleContainerUpdateApply recreates one container with a fresh image pull.
// Only containers on the stored pending list can be updated, so the rules in
// pollPortainer (not Notified itself, not limited stacks) also bound this.
func (a *App) handleContainerUpdateApply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IntegrationID int64  `json:"integration_id"`
		EnvID         int64  `json:"env_id"`
		ContainerID   string `json:"container_id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	in, err := a.getIntegration(body.IntegrationID)
	if err != nil || !isUpdateType(in.Type) || !in.Enabled {
		httpError(w, http.StatusNotFound, "integration not found")
		return
	}

	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	report := a.portainerReport(in.ID)
	idx := -1
	for i, it := range report.Items {
		if it.EnvID == body.EnvID && it.ContainerID == body.ContainerID {
			idx = i
		}
	}
	if idx < 0 {
		httpError(w, http.StatusNotFound, "that container is not on the pending update list; run the check again")
		return
	}
	item := report.Items[idx]

	// Deliberately not tied to the browser request: closing the tab must not
	// abort the swap halfway.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	note := ""
	if in.Type == "dockerhost" {
		var d *dockerHost
		if d, err = newDockerHost(in, 20*time.Minute); err == nil {
			note, err = d.recreate(ctx, item.ContainerID)
			d.c.CloseIdleConnections()
		}
	} else {
		var p *portainer
		if p, err = newPortainer(in, 20*time.Minute); err == nil {
			apiPath := fmt.Sprintf("/api/docker/%d/containers/%s/recreate", item.EnvID, item.ContainerID)
			err = p.call(ctx, http.MethodPost, apiPath, map[string]bool{"PullImage": true}, nil)
		}
	}

	e := Embed{Author: &EmbedAuthor{Name: in.Name}, Description: "**" + item.Name + "** — " + item.Image}
	e.Thumb(a.containerIcon(ctx, item.Image, item.Name))
	if err != nil {
		e.Title, e.Color = "Container update failed", colorRed
		e.Add("Error", trunc(err.Error(), 600), false)
		e.Add("What to check", "Look at the container on its host. If the update stopped partway it may be stopped or renamed with an \"-old\" suffix.", false)
		a.Dispatch(in, "update_failed", e, nil)
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	e.Title, e.Color = "Container updated", colorGreen
	if item.EnvName != in.Name {
		e.Add("Environment", item.EnvName, true)
	}
	e.Add("Note", note, false)
	a.Dispatch(in, "updated", e, nil)

	report.Items = append(report.Items[:idx], report.Items[idx+1:]...)
	a.savePortainerReport(in.ID, report)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
