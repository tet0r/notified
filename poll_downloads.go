package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---- Starr queue ----

// pollStarrQueue reports queue items that have sat in a warning or error
// state for longer than the configured window.
func pollStarrQueue(kind string) pollFunc {
	apiVer := "v3"
	if kind == "lidarr" || kind == "readarr" {
		apiVer = "v1"
	}
	label := starrKinds[kind].label
	return func(ctx context.Context, a *App, in *Integration) error {
		base := strings.TrimRight(strings.TrimSpace(in.Config["url"]), "/")
		key := strings.TrimSpace(in.Config["apikey"])
		if base == "" || key == "" {
			return nil // queue checks are optional
		}
		var out struct {
			Records []J `json:"records"`
		}
		err := getJSON(ctx, a.http, base+"/api/"+apiVer+"/queue?page=1&pageSize=250", map[string]string{"X-Api-Key": key}, &out)
		if err != nil {
			return err
		}
		after := time.Duration(cfgInt(in.Config, "stuck_minutes", 30)) * time.Minute
		now := time.Now()
		active := map[string]bool{}
		for _, r := range out.Records {
			tracked := strings.ToLower(r.Str("trackedDownloadStatus"))
			status := strings.ToLower(r.Str("status"))
			if tracked != "warning" && tracked != "error" && status != "warning" && status != "failed" {
				continue
			}
			k := "q:" + r.Str("id")
			active[k] = true
			val, ok := a.stateGet(in.ID, k)
			if !ok {
				val = strconv.FormatInt(now.Unix(), 10)
				a.stateSet(in.ID, k, val)
			}
			if val == "notified" {
				continue
			}
			first, _ := strconv.ParseInt(val, 10, 64)
			if now.Sub(time.Unix(first, 0)) < after {
				continue
			}
			e := Embed{Title: label + " queue item is stuck", Color: colorRed, Author: &EmbedAuthor{Name: in.Name}}
			e.Description = "**" + firstNonEmpty(r.Str("title"), "Unknown release") + "**"
			e.Add("Status", firstNonEmpty(r.Str("trackedDownloadState"), r.Str("status")), true)
			e.Add("Download client", r.Str("downloadClient"), true)
			e.Add("Indexer", r.Str("indexer"), true)
			e.Add("Details", firstNonEmpty(starrStatusMessages(r.Arr("statusMessages")), r.Str("errorMessage")), false)
			a.Dispatch(in, "stuck", e, nil)
			a.stateSet(in.ID, k, "notified")
		}
		a.statePrune(in.ID, "q:", active)
		return nil
	}
}

// ---- qBittorrent ----

func pollQbit(ctx context.Context, a *App, in *Integration) error {
	base := strings.TrimRight(strings.TrimSpace(in.Config["url"]), "/")
	if base == "" {
		return errors.New("no URL configured")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()

	if user := in.Config["username"]; user != "" {
		form := url.Values{"username": {user}, "password": {in.Config["password"]}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v2/auth/login", strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Referer", base)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		if resp.StatusCode >= 300 || strings.HasPrefix(strings.TrimSpace(string(body)), "Fails") {
			return fmt.Errorf("qBittorrent login failed (HTTP %d)", resp.StatusCode)
		}
	}

	var torrents []J
	if err := getJSON(ctx, client, base+"/api/v2/torrents/info", map[string]string{"Referer": base}, &torrents); err != nil {
		return err
	}
	stallAfter := time.Duration(cfgInt(in.Config, "stalled_minutes", 60)) * time.Minute
	now := time.Now()
	active := map[string]bool{}
	for _, t := range torrents {
		state := t.Str("state")
		var event, title string
		switch state {
		case "error", "missingFiles":
			event, title = "errored", "Torrent error"
			if state == "missingFiles" {
				title = "Torrent is missing files"
			}
		case "stalledDL", "metaDL":
			// Stalled since whichever is later: last activity or when it was added.
			since := t.Num("last_activity")
			if added := t.Num("added_on"); added > since {
				since = added
			}
			if since <= 0 || now.Sub(time.Unix(int64(since), 0)) < stallAfter {
				continue
			}
			event, title = "stalled", "Download stalled"
		default:
			continue
		}
		k := "t:" + event + ":" + t.Str("hash")
		active[k] = true
		if _, done := a.stateGet(in.ID, k); done {
			continue
		}
		e := Embed{Title: title, Color: colorRed, Author: &EmbedAuthor{Name: in.Name}}
		if event == "stalled" {
			e.Color = colorAmber
		}
		e.Description = "**" + t.Str("name") + "**"
		e.Add("Progress", fmt.Sprintf("%.1f%%", t.Num("progress")*100), true)
		e.Add("Size", humanBytes(t.Num("size")), true)
		e.Add("Category", t.Str("category"), true)
		e.Add("Seeds", t.Str("num_seeds"), true)
		e.Add("State", state, true)
		a.Dispatch(in, event, e, nil)
		a.stateSet(in.ID, k, "1")
	}
	a.statePrune(in.ID, "t:", active)
	return nil
}

// ---- SABnzbd ----

func pollSab(ctx context.Context, a *App, in *Integration) error {
	base := strings.TrimRight(strings.TrimSpace(in.Config["url"]), "/")
	key := strings.TrimSpace(in.Config["apikey"])
	if base == "" || key == "" {
		return errors.New("URL and API key are required")
	}
	api := func(mode, extra string, out any) error {
		u := base + "/api?output=json&mode=" + mode + "&apikey=" + url.QueryEscape(key) + extra
		if err := getJSON(ctx, a.http, u, nil, out); err != nil {
			// Never surface the request URL: it carries the API key.
			return errors.New("SABnzbd " + mode + " request failed: " + redact(err.Error(), key))
		}
		return nil
	}

	var hist struct {
		History struct {
			Slots []J `json:"slots"`
		} `json:"history"`
	}
	if err := api("history", "&limit=50&failed_only=1", &hist); err != nil {
		return err
	}
	_, baselined := a.stateGet(in.ID, "baseline")
	active := map[string]bool{}
	for _, s := range hist.History.Slots {
		if !strings.EqualFold(s.Str("status"), "Failed") {
			continue
		}
		k := "h:" + s.Str("nzo_id")
		active[k] = true
		if _, done := a.stateGet(in.ID, k); done {
			continue
		}
		a.stateSet(in.ID, k, "1")
		if !baselined {
			continue // don't replay old failures on first run
		}
		e := Embed{Title: "Download failed", Color: colorRed, Author: &EmbedAuthor{Name: in.Name}}
		e.Description = "**" + s.Str("name") + "**"
		e.Add("Category", s.Str("category"), true)
		e.Add("Size", firstNonEmpty(s.Str("size"), humanBytes(s.Num("bytes"))), true)
		e.Add("Reason", trunc(s.Str("fail_message"), 600), false)
		a.Dispatch(in, "failed", e, nil)
	}
	a.statePrune(in.ID, "h:", active)
	a.stateSet(in.ID, "baseline", "1")

	var queue struct {
		Queue J `json:"queue"`
	}
	if err := api("queue", "&limit=1", &queue); err != nil {
		return err
	}
	paused := queue.Queue.Bool("paused")
	was, known := a.stateGet(in.ID, "paused")
	a.stateSet(in.ID, "paused", strconv.FormatBool(paused))
	if known && was != strconv.FormatBool(paused) {
		if paused {
			e := Embed{Title: "SABnzbd queue paused", Color: colorAmber, Author: &EmbedAuthor{Name: in.Name}}
			e.Add("Items in queue", queue.Queue.Str("noofslots_total"), true)
			e.Add("Remaining", queue.Queue.Str("sizeleft"), true)
			a.Dispatch(in, "paused", e, nil)
		} else {
			e := Embed{Title: "SABnzbd queue resumed", Color: colorGreen, Author: &EmbedAuthor{Name: in.Name}}
			a.Dispatch(in, "resumed", e, nil)
		}
	}
	return nil
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}
