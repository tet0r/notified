package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Release notes for pending container updates. Most images record the
// project they are built from in the org.opencontainers.image.source label;
// for GitHub projects Notified looks up the latest release so the notes can be
// read before updating. Everything shown is built from strictly parsed parts
// (owner and repository names), never from a URL taken from an image as-is.

// githubAPI is the GitHub API base; tests point it at a fake server.
var githubAPI = "https://api.github.com"

var (
	githubRepoRE = regexp.MustCompile(`(?i)^(?:git\+)?https?://(?:www\.)?github\.com/([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100}?)(?:\.git)?(?:[/#?].*)?$`)
	hubNameRE    = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
)

// githubRepoFromSource extracts "owner/repo" from an image source label, or "".
func githubRepoFromSource(src string) string {
	m := githubRepoRE.FindStringSubmatch(strings.TrimSpace(src))
	if m == nil || m[2] == "." || m[2] == ".." {
		return ""
	}
	return m[1] + "/" + m[2]
}

// guessedRepo covers well-known images that don't carry a source label.
func guessedRepo(image string) string {
	repo := strings.ToLower(imageRepo(image))
	for _, prefix := range []string{"lscr.io/linuxserver/", "ghcr.io/linuxserver/", "linuxserver/"} {
		if name, ok := strings.CutPrefix(repo, prefix); ok && hubNameRE.MatchString(name) {
			return "linuxserver/docker-" + name
		}
	}
	return ""
}

// dockerHubTagsURL links to an image's tag list on Docker Hub, or "" for
// images from other registries.
func dockerHubTagsURL(image string) string {
	repo := strings.ToLower(imageRepo(image))
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "index.docker.io/")
	parts := strings.Split(repo, "/")
	if strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost" {
		return "" // another registry
	}
	for _, p := range parts {
		if !hubNameRE.MatchString(p) {
			return ""
		}
	}
	switch {
	case len(parts) == 1:
		return "https://hub.docker.com/_/" + parts[0] + "/tags"
	case len(parts) == 2 && parts[0] == "library": // Docker's official images
		return "https://hub.docker.com/_/" + parts[1] + "/tags"
	case len(parts) == 2:
		return "https://hub.docker.com/r/" + parts[0] + "/" + parts[1] + "/tags"
	}
	return ""
}

type releaseInfo struct {
	NotesURL  string
	Version   string
	Published int64
}

type cachedRelease struct {
	info releaseInfo
	ok   bool // false: the repository has no releases
	at   time.Time
}

// latestRelease asks GitHub for a repository's latest release. Results are
// cached for six hours to stay well inside GitHub's anonymous rate limit.
func (a *App) latestRelease(ctx context.Context, repo string) (releaseInfo, bool, error) {
	if v, ok := a.releaseCache.Load(repo); ok {
		c := v.(cachedRelease)
		if time.Since(c.at) < 6*time.Hour {
			return c.info, c.ok, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return releaseInfo{}, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Notified/"+version)
	resp, err := a.http.Do(req)
	if err != nil {
		return releaseInfo{}, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		a.releaseCache.Store(repo, cachedRelease{ok: false, at: time.Now()})
		return releaseInfo{}, false, nil
	default:
		return releaseInfo{}, false, fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	var rel struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return releaseInfo{}, false, err
	}
	tag := strings.TrimSpace(rel.TagName)
	info := releaseInfo{
		// Built here rather than taken from the response, so it can only ever
		// point at this repository's release page.
		NotesURL: "https://github.com/" + repo + "/releases",
		Version:  trunc(firstNonEmpty(tag, rel.Name), 60),
	}
	if tag != "" && !strings.ContainsAny(tag, "?#\\ ") {
		info.NotesURL += "/tag/" + escapeTag(tag)
	}
	if !rel.PublishedAt.IsZero() {
		info.Published = rel.PublishedAt.Unix()
	}
	a.releaseCache.Store(repo, cachedRelease{info: info, ok: true, at: time.Now()})
	return info, true, nil
}

func escapeTag(tag string) string {
	var b strings.Builder
	for _, r := range tag {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("-._~+", r):
			b.WriteRune(r)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", r))
		}
	}
	return b.String()
}

// addReleaseNotes fills in where to read about each pending update.
func (a *App) addReleaseNotes(ctx context.Context, items []pendingUpdate, labels map[string]map[string]string) {
	for i := range items {
		it := &items[i]
		l := labels[it.ContainerID]
		it.InstalledVersion = trunc(strings.TrimSpace(l["org.opencontainers.image.version"]), 60)

		repo, fromLabel := githubRepoFromSource(l["org.opencontainers.image.source"]), true
		if repo == "" {
			repo, fromLabel = guessedRepo(it.Image), false
		}
		if repo != "" {
			info, found, err := a.latestRelease(ctx, repo)
			switch {
			case err == nil && found:
				it.NotesURL, it.LatestVersion, it.ReleasedAt = info.NotesURL, info.Version, info.Published
				continue
			case err != nil || fromLabel:
				// GitHub unreachable or rate limited, or a confirmed project
				// that publishes no releases: its releases page is still the
				// right place to look.
				it.NotesURL = "https://github.com/" + repo + "/releases"
				continue
			}
		}
		it.NotesURL = dockerHubTagsURL(it.Image)
	}
}
