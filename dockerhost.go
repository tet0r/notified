package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The Docker host integration talks straight to a Docker Engine API, usually
// through a socket proxy on another machine, for hosts whose containers are
// not managed by Portainer (for example OpenMediaVault's Compose plugin).
// It finds outdated containers the same way the Portainer integration does
// and performs the pull-and-recreate itself.

type dockerHost struct {
	base string
	host string // for error messages
	c    *http.Client
}

func newDockerHost(in *Integration, timeout time.Duration) (*dockerHost, error) {
	raw := strings.TrimSpace(in.Config["url"])
	if raw == "" {
		return nil, errors.New("the Docker address is required")
	}
	if sock, ok := strings.CutPrefix(raw, "unix://"); ok {
		if sock == "" {
			return nil, errors.New("the unix:// address needs a socket path, for example unix:///var/run/docker.sock")
		}
		return &dockerHost{base: "http://docker", host: sock, c: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			}},
		}}, nil
	}
	if rest, ok := strings.CutPrefix(raw, "tcp://"); ok {
		raw = "http://" + rest
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("the Docker address must look like http://host:2375 or unix:///var/run/docker.sock")
	}
	return &dockerHost{base: u.Scheme + "://" + u.Host, host: u.Host, c: &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: cfgBool(in.Config, "insecure_tls")},
		},
	}}, nil
}

// do sends one request and returns the response for the caller to read.
func (d *dockerHost) do(ctx context.Context, method, apiPath string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+apiPath, rd)
	if err != nil {
		return nil, errors.New("invalid Docker address")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Docker at %s: %s", d.host, unwrapNetErr(err))
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 || resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	what, _, _ := strings.Cut(apiPath, "?")
	if resp.StatusCode == http.StatusForbidden && bytes.Contains(raw, []byte("<html")) {
		// A socket proxy answers requests it does not permit with an HTML 403.
		return nil, fmt.Errorf("the Docker socket proxy at %s refused %s %s. It needs CONTAINERS=1, IMAGES=1, NETWORKS=1, DISTRIBUTION=1 and POST=1", d.host, method, what)
	}
	var e struct {
		Message string `json:"message"`
	}
	json.Unmarshal(raw, &e)
	msg := firstNonEmpty(e.Message, trunc(strings.TrimSpace(string(raw)), 200))
	return nil, fmt.Errorf("Docker returned HTTP %d for %s: %s", resp.StatusCode, what, trunc(msg, 300))
}

// call performs one request and decodes a JSON answer. out may be nil.
func (d *dockerHost) call(ctx context.Context, method, apiPath string, body, out any) error {
	resp, err := d.do(ctx, method, apiPath, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		what, _, _ := strings.Cut(apiPath, "?")
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("<")) {
			return fmt.Errorf("%s returned a web page instead of Docker API data for %s; check the address and port", d.host, what)
		}
		return fmt.Errorf("could not read Docker's answer for %s: %v", what, err)
	}
	return nil
}

func pollDockerHost(ctx context.Context, a *App, in *Integration) error {
	d, err := newDockerHost(in, 60*time.Second)
	if err != nil {
		return err
	}
	defer d.c.CloseIdleConnections()
	report := portainerReport{CheckedAt: time.Now().Unix(), Items: []pendingUpdate{}, Errors: []string{}}
	addErr := func(format string, args ...any) {
		if len(report.Errors) < 10 {
			report.Errors = append(report.Errors, fmt.Sprintf(format, args...))
		}
	}
	scanContainers(ctx, d.call, scanOpts{envName: in.Name, ignore: splitList(in.Config["ignore"])}, &report, addErr)
	a.finishScan(ctx, in, report)
	if len(report.Errors) > 0 {
		return errors.New(report.Errors[0])
	}
	return nil
}

// pull downloads the current image for a tag. Docker streams progress as JSON
// lines and reports a failed pull inside that stream, not in the status code.
func (d *dockerHost) pull(ctx context.Context, image string) error {
	repo := imageRepo(image)
	tag := strings.TrimPrefix(strings.TrimPrefix(image, repo), ":")
	if tag == "" {
		tag = "latest"
	}
	resp, err := d.do(ctx, http.MethodPost, "/images/create?fromImage="+url.QueryEscape(repo)+"&tag="+url.QueryEscape(tag), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var line struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&line); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("the pull of %s was interrupted: %s", image, unwrapNetErr(err))
		}
		if line.Error != "" {
			return fmt.Errorf("could not pull %s: %s", image, trunc(line.Error, 300))
		}
	}
}

// cleanEndpoint keeps only the user-chosen settings of a network attachment.
// The short id of the old container is one of its automatic aliases and must
// not be carried over to the new one.
func cleanEndpoint(ep J, oldShortID string) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"IPAMConfig", "Links", "DriverOpts"} {
		if v := ep[k]; v != nil {
			out[k] = v
		}
	}
	if arr, ok := ep["Aliases"].([]any); ok {
		var keep []any
		for _, x := range arr {
			if s, _ := x.(string); s != "" && s != oldShortID {
				keep = append(keep, s)
			}
		}
		if len(keep) > 0 {
			out["Aliases"] = keep
		}
	}
	return out
}

// buildCreateBody turns an inspected container into the request that creates
// its replacement, and returns the extra networks to attach afterwards
// (Docker accepts only one network at creation on older engines).
func buildCreateBody(inspect J) (body map[string]any, extraNets map[string]any) {
	cfg := inspect.Obj("Config")
	hostCfg := inspect.Obj("HostConfig")
	id := inspect.Str("Id")
	shortID := id
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}

	body = map[string]any{}
	for k, v := range cfg {
		body[k] = v
	}
	// Docker sets the hostname to the container's own id unless told otherwise;
	// copying that would pin the new container to the old id.
	if cfg.Str("Hostname") == shortID {
		delete(body, "Hostname")
	}
	mode := hostCfg.Str("NetworkMode")
	if strings.HasPrefix(mode, "container:") {
		// Sharing another container's network forbids these settings.
		for _, k := range []string{"Hostname", "Domainname", "ExposedPorts", "MacAddress"} {
			delete(body, k)
		}
	}
	body["HostConfig"] = map[string]any(hostCfg)

	extraNets = map[string]any{}
	if mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:") {
		return body, extraNets
	}
	nets := inspect.Obj("NetworkSettings", "Networks")
	names := make([]string, 0, len(nets))
	for n := range nets {
		names = append(names, n)
	}
	sort.Strings(names)
	// Prefer the network named by NetworkMode as the one given at creation.
	first := ""
	for _, n := range names {
		if n == mode {
			first = n
		}
	}
	if first == "" && len(names) > 0 {
		first = names[0]
	}
	for _, n := range names {
		ep := cleanEndpoint(nets.Obj(n), shortID)
		if n == first {
			body["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{n: ep}}
		} else {
			extraNets[n] = ep
		}
	}
	return body, extraNets
}

// recreate replaces a container with one built from the newest image for its
// tag, keeping its settings. The old container is kept (renamed) until the
// new one has started, so a failure can put things back.
func (d *dockerHost) recreate(ctx context.Context, id string) (note string, err error) {
	var inspect J
	if err := d.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, &inspect); err != nil {
		return "", err
	}
	name := strings.TrimPrefix(inspect.Str("Name"), "/")
	image := inspect.Str("Config", "Image")
	if name == "" || image == "" {
		return "", errors.New("could not read the container's settings")
	}
	if inspect.Bool("HostConfig", "AutoRemove") {
		return "", errors.New("this container removes itself when stopped (--rm), so it cannot be recreated safely")
	}
	wasRunning := inspect.Bool("State", "Running")

	// 1. Pull first: if it fails, nothing has been touched.
	if err := d.pull(ctx, image); err != nil {
		return "", err
	}

	// 2. Stop and set the old container aside.
	oldName := name + "-old"
	if err := d.call(ctx, http.MethodPost, "/containers/"+id+"/stop", nil, nil); err != nil {
		return "", fmt.Errorf("could not stop %s: %w", name, err)
	}
	restore := func(cause error) error {
		// Put the original back under its own name and start it again.
		rerr := d.call(ctx, http.MethodPost, "/containers/"+id+"/rename?name="+url.QueryEscape(name), nil, nil)
		if rerr == nil && wasRunning {
			rerr = d.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil)
		}
		if rerr != nil {
			return fmt.Errorf("%w. Restoring the original also failed (%v): look for a container named %s", cause, rerr, oldName)
		}
		return fmt.Errorf("%w. The original container was restored", cause)
	}
	if err := d.call(ctx, http.MethodPost, "/containers/"+id+"/rename?name="+url.QueryEscape(oldName), nil, nil); err != nil {
		cause := fmt.Errorf("could not rename %s: %w", name, err)
		if wasRunning {
			if serr := d.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil); serr != nil {
				return "", fmt.Errorf("%w. It is now stopped and could not be restarted (%v)", cause, serr)
			}
		}
		return "", fmt.Errorf("%w. The original container was started again", cause)
	}

	// 3. Create the replacement with the same settings.
	body, extraNets := buildCreateBody(inspect)
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.call(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body, &created); err != nil || created.ID == "" {
		if err == nil {
			err = errors.New("Docker did not return the new container's id")
		}
		return "", restore(fmt.Errorf("could not create the new %s: %w", name, err))
	}
	discard := func(cause error) error {
		d.call(ctx, http.MethodDelete, "/containers/"+created.ID+"?force=1", nil, nil)
		return restore(cause)
	}
	for net, ep := range extraNets {
		req := map[string]any{"Container": created.ID, "EndpointConfig": ep}
		if err := d.call(ctx, http.MethodPost, "/networks/"+url.PathEscape(net)+"/connect", req, nil); err != nil {
			return "", discard(fmt.Errorf("could not attach the new %s to network %s: %w", name, net, err))
		}
	}

	// 4. Start it, then drop the old one. Volumes are never removed.
	if wasRunning {
		if err := d.call(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil); err != nil {
			return "", discard(fmt.Errorf("the new %s would not start: %w", name, err))
		}
	}
	if err := d.call(ctx, http.MethodDelete, "/containers/"+id, nil, nil); err != nil {
		// The update itself worked; only the leftover needs attention.
		return fmt.Sprintf("The old copy could not be removed and is still there as %s: %v", oldName, err), nil
	}
	return "", nil
}
