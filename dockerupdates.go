package main

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
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
// Lookups to the same registry are spaced registryGap apart (registries
// throttle bursts); different registries proceed independently.
var (
	registryGap     = 150 * time.Millisecond
	registryBackoff = 2 * time.Second
	registryWorkers = 6 // lookups in flight at once, across all registries
	inspectWorkers  = 8 // image inspections in flight at once
)

// registryOf returns the registry an image reference is pulled from.
func registryOf(image string) string {
	first, _, ok := strings.Cut(imageRepo(image), "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return strings.ToLower(first)
	}
	return "docker.io"
}

// pacer spaces out requests per key (here, per registry).
type pacer struct {
	mu   sync.Mutex
	next map[string]time.Time
}

var registryPacer = &pacer{next: map[string]time.Time{}}

func (p *pacer) wait(ctx context.Context, key string) error {
	p.mu.Lock()
	now := time.Now()
	at := p.next[key]
	if at.Before(now) {
		at = now
	}
	p.next[key] = at.Add(registryGap)
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Until(at)):
		return nil
	}
}

// forEachLimited runs fn for every key, at most n at a time, and waits.
func forEachLimited(n int, keys []string, fn func(string)) {
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func(k string) {
			defer func() { <-sem; wg.Done() }()
			fn(k)
		}(k)
	}
	wg.Wait()
}

// retryThrottled runs a registry lookup, retrying with backoff when the
// registry answers "too many requests".
func retryThrottled(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * registryBackoff):
			}
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
// registry and appends the ones that are behind to the report. Image details
// and registry lookups run in parallel; results are assembled in container
// order so reports and errors read the same from one check to the next.
func scanContainers(ctx context.Context, call dockerCall, o scanOpts, report *portainerReport, addErr func(string, ...any)) {
	var containers []portainerContainer
	if err := call(ctx, http.MethodGet, "/containers/json", nil, &containers); err != nil {
		addErr("%s: could not list containers: %v", o.envName, err)
		return
	}

	// 1. Decide which containers to check (cheap, no requests).
	type candidate struct {
		c       portainerContainer
		name    string
		project string
	}
	var cands []candidate
	var imageIDs, seenID = []string{}, map[string]bool{}
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
		skip := untouchable || strings.Contains(c.Image, "@") || strings.HasPrefix(c.Image, "sha256:") || !imageRefRE.MatchString(c.Image) || strings.Contains(c.Image, "..")
		for _, pat := range o.ignore {
			if ok, _ := path.Match(pat, name); ok {
				skip = true
			}
		}
		if skip {
			report.Ignored++
			continue
		}
		cands = append(cands, candidate{c, name, project})
		if !seenID[c.ImageID] {
			seenID[c.ImageID] = true
			imageIDs = append(imageIDs, c.ImageID)
		}
	}

	// 2. Inspect each distinct local image, several at a time.
	type imageInfo struct {
		digests map[string]bool
		labels  map[string]string
		err     error
	}
	var mu sync.Mutex
	images := map[string]imageInfo{}
	forEachLimited(inspectWorkers, imageIDs, func(id string) {
		var img struct {
			RepoDigests []string `json:"RepoDigests"`
			Config      struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
		}
		err := call(ctx, http.MethodGet, "/images/"+id+"/json", nil, &img)
		info := imageInfo{digests: map[string]bool{}, labels: img.Config.Labels, err: err}
		for _, d := range img.RepoDigests {
			info.digests[digestOf(d)] = true
		}
		mu.Lock()
		images[id] = info
		mu.Unlock()
	})

	// 3. Ask the registry for each distinct image reference, paced per registry.
	var refs, seenRef = []string{}, map[string]bool{}
	for _, cd := range cands {
		if len(images[cd.c.ImageID].digests) > 0 && !seenRef[cd.c.Image] {
			seenRef[cd.c.Image] = true
			refs = append(refs, cd.c.Image)
		}
	}
	type remoteInfo struct {
		digest string
		err    error
	}
	remotes := map[string]remoteInfo{}
	forEachLimited(registryWorkers, refs, func(ref string) {
		var dist struct {
			Descriptor struct {
				Digest string `json:"digest"`
			} `json:"Descriptor"`
		}
		err := retryThrottled(ctx, func() error {
			if err := registryPacer.wait(ctx, registryOf(ref)); err != nil {
				return err
			}
			return call(ctx, http.MethodGet, "/distribution/"+ref+"/json", nil, &dist)
		})
		mu.Lock()
		remotes[ref] = remoteInfo{dist.Descriptor.Digest, err}
		mu.Unlock()
	})

	// 4. Assemble the results in container order.
	reportedInspect, reportedRef := map[string]bool{}, map[string]bool{}
	for _, cd := range cands {
		img := images[cd.c.ImageID]
		if img.err != nil && !reportedInspect[cd.c.ImageID] {
			reportedInspect[cd.c.ImageID] = true
			addErr("%s: %v", cd.name, img.err)
		}
		if len(img.digests) == 0 {
			report.Ignored++ // built locally or never pulled from a registry
			continue
		}
		rem := remotes[cd.c.Image]
		if rem.err != nil && !reportedRef[cd.c.Image] {
			reportedRef[cd.c.Image] = true
			addErr("%s: could not check %s: %v", cd.name, cd.c.Image, rem.err)
		}
		if rem.digest == "" {
			continue
		}
		report.Checked++
		if img.digests[rem.digest] {
			continue
		}
		report.Items = append(report.Items, pendingUpdate{
			EnvID: o.envID, EnvName: o.envName, ContainerID: cd.c.ID, Name: cd.name,
			Image: cd.c.Image, Stack: cd.project, Latest: rem.digest,
		})
		// Container labels include the image's, so either source works.
		l := map[string]string{}
		for k, v := range img.labels {
			l[k] = v
		}
		for _, k := range []string{"org.opencontainers.image.source", "org.opencontainers.image.version"} {
			if v := cd.c.Labels[k]; v != "" && l[k] == "" {
				l[k] = v
			}
		}
		if report.labels == nil {
			report.labels = map[string]map[string]string{}
		}
		report.labels[cd.c.ID] = l
	}
}

// finishScan stores the report and announces each new image build once.
func (a *App) finishScan(ctx context.Context, in *Integration, report portainerReport) {
	a.addReleaseNotes(ctx, report.Items, report.labels)
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
			e.Add("Installed", it.InstalledVersion, true)
			e.Add("What's new", releaseNotesLine(it), false)
			e.Thumb(a.containerIcon(ctx, it.Image, it.Name))
			e.actions = updateActions(in.ID, it)
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

// releaseNotesLine is the Discord text linking to an update's release notes.
func releaseNotesLine(it pendingUpdate) string {
	if it.NotesURL == "" {
		return ""
	}
	label := "Release notes"
	if it.LatestVersion != "" {
		label = "Latest release " + strings.NewReplacer("[", "(", "]", ")").Replace(it.LatestVersion)
	}
	line := "[" + label + "](" + it.NotesURL + ")"
	if it.ReleasedAt > 0 {
		line += fmt.Sprintf(" · <t:%d:R>", it.ReleasedAt) // Discord shows this as "3 days ago"
	}
	return line
}
