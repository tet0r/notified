package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// updateURL is the GitHub "latest release" endpoint the update check reads.
// Forks can point it at their own repository.
var updateURL = env("NOTIFIED_UPDATE_URL", "https://api.github.com/repos/tet0r/notified/releases/latest")

const updateInterval = 6 * time.Hour

// parseSemver reads the leading MAJOR.MINOR.PATCH of a version string. It
// accepts a "v" prefix and ignores any suffix, so `git describe` output such
// as "0.2.0-3-gabc1234" compares as 0.2.0.
func parseSemver(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".", 3)
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		if end == 0 || (end < len(p) && i < 2) {
			return v, false
		}
		n, err := strconv.Atoi(p[:end])
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// newerVersion reports whether latest is a higher release than current.
// Builds without a recognisable version (e.g. "dev") never report an update.
func newerVersion(latest, current string) bool {
	l, ok1 := parseSemver(latest)
	c, ok2 := parseSemver(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

type updateInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	URL       string `json:"url"`
	Notes     string `json:"notes"`
	Available bool   `json:"update_available"`
	CheckedAt int64  `json:"checked_at"`
	Enabled   bool   `json:"enabled"`
	Error     string `json:"error,omitempty"`
}

func (a *App) updateInfo() updateInfo {
	checked, _ := strconv.ParseInt(a.setting("update_checked"), 10, 64)
	latest := a.setting("update_latest")
	return updateInfo{
		Current:   version,
		Latest:    latest,
		URL:       a.setting("update_url"),
		Notes:     a.setting("update_notes"),
		Available: newerVersion(latest, version),
		CheckedAt: checked,
		Enabled:   a.setting("update_check") != "false",
		Error:     a.setting("update_error"),
	}
}

// checkForUpdate asks GitHub for the latest release and stores the result
// for the web UI. Updates are only ever shown in the app.
func (a *App) checkForUpdate(ctx context.Context) error {
	err := a.fetchLatestRelease(ctx)
	a.setSetting("update_checked", strconv.FormatInt(time.Now().Unix(), 10))
	if err != nil {
		a.setSetting("update_error", err.Error())
		return err
	}
	a.setSetting("update_error", "")
	return nil
}

func (a *App) fetchLatestRelease(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Notified/"+version)
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %s", req.URL.Host, unwrapNetErr(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil // no releases published yet
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update check got HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return fmt.Errorf("unexpected update check response: %w", err)
	}
	latest := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	if _, ok := parseSemver(latest); !ok {
		return fmt.Errorf("latest release has an unrecognised version %q", rel.TagName)
	}
	// Only keep links that are plain web URLs; this value is rendered as a link.
	if !strings.HasPrefix(rel.HTMLURL, "https://") {
		rel.HTMLURL = ""
	}
	a.setSetting("update_latest", latest)
	a.setSetting("update_url", rel.HTMLURL)
	a.setSetting("update_notes", trunc(strings.TrimSpace(rel.Body), 1500))
	return nil
}

func (a *App) updateLoop(ctx context.Context) {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if a.setting("update_check") != "false" {
			a.checkForUpdate(ctx)
		}
		timer.Reset(updateInterval)
	}
}

func (a *App) handleUpdateGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.updateInfo())
}

func (a *App) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	a.checkForUpdate(r.Context())
	writeJSON(w, http.StatusOK, a.updateInfo())
}
