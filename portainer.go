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
	"path"
	"regexp"
	"sort"
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

// pollPortainer compares every eligible running container's image with the
// registry and records the ones that are behind.
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

		docker := fmt.Sprintf("/api/endpoints/%d/docker", env.ID)
		var containers []portainerContainer
		if err := p.call(ctx, http.MethodGet, docker+"/containers/json", nil, &containers); err != nil {
			addErr("%s: could not list containers: %v", env.Name, err)
			continue
		}

		localDigests := map[string]map[string]bool{} // image id -> digests
		remoteDigest := map[string]string{}          // image ref -> digest ("" = lookup failed)
		for _, c := range containers {
			if len(c.Names) == 0 {
				continue
			}
			name := strings.TrimPrefix(c.Names[0], "/")
			project := c.Labels["com.docker.compose.project"]
			if project != "" && !managed[project] {
				report.Limited++
				continue
			}
			skip := isSelf(c) || c.Labels["com.docker.swarm.service.name"] != "" ||
				strings.Contains(c.Image, "@") || strings.HasPrefix(c.Image, "sha256:") || !imageRefRE.MatchString(c.Image)
			for _, pat := range ignore {
				if ok, _ := path.Match(pat, name); ok {
					skip = true
				}
			}
			if skip {
				report.Ignored++
				continue
			}

			local, seen := localDigests[c.ImageID]
			if !seen {
				var img struct {
					RepoDigests []string `json:"RepoDigests"`
				}
				local = map[string]bool{}
				if err := p.call(ctx, http.MethodGet, docker+"/images/"+c.ImageID+"/json", nil, &img); err != nil {
					addErr("%s: %v", name, err)
				}
				for _, d := range img.RepoDigests {
					local[digestOf(d)] = true
				}
				localDigests[c.ImageID] = local
			}
			if len(local) == 0 {
				report.Ignored++ // built locally or never pulled from a registry
				continue
			}

			remote, seen := remoteDigest[c.Image]
			if !seen {
				var dist struct {
					Descriptor struct {
						Digest string `json:"digest"`
					} `json:"Descriptor"`
				}
				if err := p.call(ctx, http.MethodGet, docker+"/distribution/"+c.Image+"/json", nil, &dist); err != nil {
					addErr("%s: could not check %s: %v", name, c.Image, err)
				}
				remote = dist.Descriptor.Digest
				remoteDigest[c.Image] = remote
			}
			if remote == "" {
				continue
			}
			report.Checked++
			if !local[remote] {
				report.Items = append(report.Items, pendingUpdate{
					EnvID: env.ID, EnvName: env.Name, ContainerID: c.ID, Name: name,
					Image: c.Image, Stack: project, Latest: remote,
				})
			}
		}
	}
	sort.Slice(report.Items, func(i, j int) bool {
		if report.Items[i].EnvName != report.Items[j].EnvName {
			return report.Items[i].EnvName < report.Items[j].EnvName
		}
		return report.Items[i].Name < report.Items[j].Name
	})
	a.savePortainerReport(in.ID, report)

	// Announce each new image build once.
	active := map[string]bool{}
	var fresh []pendingUpdate
	for _, it := range report.Items {
		k := fmt.Sprintf("u:%d:%s:%s", it.EnvID, it.Name, it.Latest)
		active[k] = true
		if _, done := a.stateGet(in.ID, k); !done {
			fresh = append(fresh, it)
			a.stateSet(in.ID, k, "1")
		}
	}
	a.statePrune(in.ID, "u:", active)
	if len(fresh) > 0 {
		e := Embed{Color: colorBlue, Author: &EmbedAuthor{Name: in.Name}}
		e.Title = fmt.Sprintf("%d container updates available", len(fresh))
		if len(fresh) == 1 {
			e.Title = "Container update available"
		}
		var lines []string
		for _, it := range fresh {
			lines = append(lines, "• **"+it.Name+"** — "+it.Image)
		}
		e.Description = joinLimited(lines, 15)
		if base := a.setting("base_url"); base != "" {
			e.URL = base + "/#updates"
			e.Add("Update", "Open Notified → Container updates to apply.", false)
		}
		a.Dispatch(in, "update_available", e, nil)
	}
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
		if in.Type == "portainer" && in.Enabled {
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

// handleContainerUpdatesCheck re-runs the check for every Portainer integration.
func (a *App) handleContainerUpdatesCheck(w http.ResponseWriter, r *http.Request) {
	ins, err := a.listIntegrations()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, in := range ins {
		if in.Type == "portainer" && in.Enabled {
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
	if err != nil || in.Type != "portainer" || !in.Enabled {
		httpError(w, http.StatusNotFound, "Portainer integration not found")
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

	p, err := newPortainer(in, 20*time.Minute)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Deliberately not tied to the browser request: closing the tab must not
	// abort Portainer halfway through swapping the container.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	apiPath := fmt.Sprintf("/api/docker/%d/containers/%s/recreate", item.EnvID, item.ContainerID)
	err = p.call(ctx, http.MethodPost, apiPath, map[string]bool{"PullImage": true}, nil)

	e := Embed{Author: &EmbedAuthor{Name: in.Name}, Description: "**" + item.Name + "** — " + item.Image}
	if err != nil {
		e.Title, e.Color = "Container update failed", colorRed
		e.Add("Error", trunc(err.Error(), 600), false)
		e.Add("What to check", "Open the container in Portainer. If the update stopped partway it may be stopped or renamed with an \"-old\" suffix.", false)
		a.Dispatch(in, "update_failed", e, nil)
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	e.Title, e.Color = "Container updated", colorGreen
	e.Add("Environment", item.EnvName, true)
	a.Dispatch(in, "updated", e, nil)

	report.Items = append(report.Items[:idx], report.Items[idx+1:]...)
	a.savePortainerReport(in.ID, report)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
