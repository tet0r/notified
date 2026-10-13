package main

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

// Update detection shared by the Portainer and Docker host integrations.
// Both reach a Docker Engine API (Portainer proxies one per environment),
// so the comparison of running images against the registry is the same.

// dockerCall performs one Docker Engine API request, e.g. GET /containers/json.
type dockerCall func(ctx context.Context, method, apiPath string, body, out any) error

type scanOpts struct {
	envID   int64
	envName string
	// managed lists the compose projects that may be updated. nil allows all;
	// Portainer passes the stacks it controls so "Limited" ones are skipped.
	managed map[string]bool
	ignore  []string
}

// Pacing for registry lookups; variables so tests can shorten them.
var (
	registryGap     = 300 * time.Millisecond
	registryBackoff = 2 * time.Second
)

// retryThrottled runs a registry lookup. Registries throttle bursts of these
// with HTTP 429, so lookups are spaced out and a throttled one is retried.
func retryThrottled(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		// A short gap before every lookup, growing after each throttled attempt.
		wait := registryGap + time.Duration(attempt*attempt)*registryBackoff
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if err = fn(); err == nil || !isThrottled(err) {
			return err
		}
	}
	return err
}

func isThrottled(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 429") || strings.Contains(msg, "toomanyrequests") || strings.Contains(msg, "too many requests")
}

// isSocketProxy spots the proxy Notified itself connects through. Recreating
// it would cut the connection halfway and leave it stopped.
func isSocketProxy(c portainerContainer) bool {
	return strings.Contains(strings.ToLower(c.Image), "socket-proxy")
}

// scanContainers compares each eligible running container's image with the
// registry and appends the ones that are behind to the report.
func scanContainers(ctx context.Context, call dockerCall, o scanOpts, report *portainerReport, addErr func(string, ...any)) {
	var containers []portainerContainer
	if err := call(ctx, http.MethodGet, "/containers/json", nil, &containers); err != nil {
		addErr("%s: could not list containers: %v", o.envName, err)
		return
	}
	localDigests := map[string]map[string]bool{} // image id -> digests
	remoteDigest := map[string]string{}          // image ref -> digest ("" = lookup failed)
	for _, c := range containers {
		if len(c.Names) == 0 {
			continue
		}
		name := strings.TrimPrefix(c.Names[0], "/")
		project := c.Labels["com.docker.compose.project"]
		if project != "" && o.managed != nil && !o.managed[project] {
			report.Limited++
			continue
		}
		untouchable := isSelf(c) || isSocketProxy(c) || c.Labels["com.docker.swarm.service.name"] != ""
		if !untouchable {
			report.Names = append(report.Names, name)
		}
		skip := untouchable || strings.Contains(c.Image, "@") || strings.HasPrefix(c.Image, "sha256:") || !imageRefRE.MatchString(c.Image)
		for _, pat := range o.ignore {
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
			if err := call(ctx, http.MethodGet, "/images/"+c.ImageID+"/json", nil, &img); err != nil {
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
			err := retryThrottled(ctx, func() error {
				return call(ctx, http.MethodGet, "/distribution/"+c.Image+"/json", nil, &dist)
			})
			if err != nil {
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
				EnvID: o.envID, EnvName: o.envName, ContainerID: c.ID, Name: name,
				Image: c.Image, Stack: project, Latest: remote,
			})
		}
	}
}

// finishScan stores the report and announces each new image build once.
func (a *App) finishScan(ctx context.Context, in *Integration, report portainerReport) {
	sort.Slice(report.Items, func(i, j int) bool {
		if report.Items[i].EnvName != report.Items[j].EnvName {
			return report.Items[i].EnvName < report.Items[j].EnvName
		}
		return report.Items[i].Name < report.Items[j].Name
	})
	a.savePortainerReport(in.ID, report)

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

	link := ""
	if base := a.setting("base_url"); base != "" {
		link = base + "/#updates"
	}
	switch {
	case len(fresh) == 0:
	case len(fresh) <= 8:
		// One message per container, so each can show that container's icon.
		for _, it := range fresh {
			e := Embed{Title: "Container update available", Color: colorBlue, URL: link, Author: &EmbedAuthor{Name: in.Name}}
			e.Description = "**" + it.Name + "**\n" + it.Image
			if it.EnvName != in.Name {
				e.Add("Environment", it.EnvName, true)
			}
			e.Add("Stack", it.Stack, true)
			e.Thumb(a.containerIcon(ctx, it.Image, it.Name))
			a.Dispatch(in, "update_available", e, nil)
		}
	default:
		// Too many for individual messages: send one summary.
		e := Embed{Color: colorBlue, URL: link, Author: &EmbedAuthor{Name: in.Name}}
		e.Title = fmt.Sprintf("%d container updates available", len(fresh))
		var lines []string
		for _, it := range fresh {
			lines = append(lines, "• **"+it.Name+"** — "+it.Image)
		}
		e.Description = joinLimited(lines, 20)
		a.Dispatch(in, "update_available", e, nil)
	}
}
