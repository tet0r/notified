package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Discord only shows images it can download itself, so pictures in
// notifications must be public URLs or files uploaded with the message.
// App and container icons come from the dashboard-icons collection on a
// public CDN (the same artwork bundled for the web UI, as PNG).
const iconCDN = "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/"

// typeIcons maps an integration type to its dashboard-icons name.
var typeIcons = map[string]string{
	"sonarr": "sonarr", "radarr": "radarr", "lidarr": "lidarr", "readarr": "readarr", "prowlarr": "prowlarr",
	"plex": "plex", "jellyfin": "jellyfin", "emby": "emby", "tautulli": "tautulli",
	"seerr": "seerr", "overseerr": "overseerr", "jellyseerr": "jellyseerr",
	"qbittorrent": "qbittorrent", "sabnzbd": "sabnzbd", "nzbget": "nzbget", "nzbhydra2": "nzbhydra2",
	"docker": "docker", "dockerhost": "docker", "portainer": "portainer",
}

func appIconURL(typ string) string {
	if name := typeIcons[typ]; name != "" {
		return iconCDN + name + ".png"
	}
	return ""
}

// iconAliases covers well-known images whose name is not the app's name.
var iconAliases = map[string]string{
	"pms-docker": "plex",
	"pms":        "plex",
	"plexinc":    "plex",
}

var (
	iconNameRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	iconSuffixes = []string{"-server", "-docker", "-app", "-web", "-ce", "-ee", "-core", "-backend", "-frontend", "-machine-learning"}
)

// iconCandidates lists icon names worth trying for a container, most
// specific first: the image name, that name with a generic suffix removed,
// its first word, the publisher, then the container's own name.
func iconCandidates(image, container string) []string {
	repo := strings.ToLower(imageRepo(image))
	if i := strings.Index(repo, "@"); i >= 0 {
		repo = repo[:i]
	}
	parts := strings.Split(repo, "/")
	last := parts[len(parts)-1]
	org := ""
	if len(parts) >= 2 {
		org = parts[len(parts)-2]
	}
	name := strings.ReplaceAll(strings.ToLower(container), "_", "-")

	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if a, ok := iconAliases[s]; ok {
			s = a
		}
		if len(s) >= 2 && iconNameRE.MatchString(s) && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	variants := func(s string) {
		add(s)
		for _, suf := range iconSuffixes {
			if strings.HasSuffix(s, suf) {
				add(strings.TrimSuffix(s, suf))
			}
		}
	}
	variants(last)
	if first, _, ok := strings.Cut(last, "-"); ok {
		add(first)
	}
	// Skip generic publishers that host many unrelated apps.
	if org != "" && org != "library" && org != "linuxserver" && !strings.Contains(org, ".") {
		variants(org)
	}
	variants(name)
	if first, _, ok := strings.Cut(name, "-"); ok {
		add(first)
	}
	return out
}

// containerIcon finds an icon for a container, or "" if nothing matches.
// Lookups are cached for the life of the process.
func (a *App) containerIcon(ctx context.Context, image, container string) string {
	for _, cand := range iconCandidates(image, container) {
		if known, ok := a.iconCache.Load(cand); ok {
			if known.(bool) {
				return iconCDN + cand + ".png"
			}
			continue
		}
		exists, definite := a.iconExists(ctx, cand)
		if definite {
			a.iconCache.Store(cand, exists)
		}
		if exists {
			return iconCDN + cand + ".png"
		}
	}
	return ""
}

// iconExists reports whether the CDN has an icon. definite is false when the
// CDN could not be reached, so a temporary outage is not cached as "missing".
func (a *App) iconExists(ctx context.Context, name string) (exists, definite bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, iconCDN+name+".png", nil)
	if err != nil {
		return false, true
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return false, false
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, true
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden:
		return false, true
	}
	return false, false
}

// fetchImage downloads a picture so it can be uploaded to Discord. This is
// for artwork on servers Discord cannot reach, such as a Jellyfin or Emby
// instance on the local network.
func (a *App) fetchImage(ctx context.Context, url string) (data []byte, filename string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	// A redirect must not carry the request to a different host.
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) >= 3 || next.URL.Host != via[0].URL.Host {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const max = 4 << 20
	data, err = io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > max {
		return nil, "", fmt.Errorf("image is larger than 4 MB")
	}
	name := imageFilename(data)
	if name == "" {
		return nil, "", fmt.Errorf("not an image")
	}
	return data, name, nil
}

// imageFilename picks an upload filename from the file's own contents, so a
// mislabelled or non-image response is never sent on as a picture.
func imageFilename(data []byte) string {
	switch http.DetectContentType(data) {
	case "image/jpeg":
		return "thumb.jpg"
	case "image/png":
		return "thumb.png"
	case "image/webp":
		return "thumb.webp"
	case "image/gif":
		return "thumb.gif"
	}
	return ""
}
