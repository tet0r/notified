package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// postJSON sends a JSON body and decodes a JSON response. Like getJSON it
// reports only the host on failure, never the URL or credentials.
func postJSON(ctx context.Context, c *http.Client, url, user, pass string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid URL")
	}
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %s", req.URL.Host, unwrapNetErr(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%s rejected the credentials (HTTP %d)", req.URL.Host, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unexpected response from %s", req.URL.Host)
	}
	return nil
}

// ---- NZBGet ----

func pollNzbget(ctx context.Context, a *App, in *Integration) error {
	base := strings.TrimRight(strings.TrimSpace(in.Config["url"]), "/")
	if base == "" {
		return errors.New("no URL configured")
	}
	rpc := func(method string, params []any, result any) error {
		var out struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
		if err := postJSON(ctx, a.http, base+"/jsonrpc", in.Config["username"], in.Config["password"], body, &out); err != nil {
			return err
		}
		if out.Error != nil {
			return fmt.Errorf("NZBGet %s failed: %s", method, out.Error.Message)
		}
		return json.Unmarshal(out.Result, result)
	}

	// history(false) excludes hidden duplicate-tracking records.
	var history []J
	if err := rpc("history", []any{false}, &history); err != nil {
		return err
	}
	_, baselined := a.stateGet(in.ID, "baseline")
	active := map[string]bool{}
	for _, item := range history {
		status := item.Str("Status")
		if !strings.HasPrefix(status, "FAILURE") {
			continue
		}
		k := "h:" + firstNonEmpty(item.Str("NZBID"), item.Str("ID"))
		active[k] = true
		if _, done := a.stateGet(in.ID, k); done {
			continue
		}
		a.stateSet(in.ID, k, "1")
		if !baselined {
			continue // don't replay old failures on first run
		}
		e := Embed{Title: "Download failed", Color: colorRed, Author: &EmbedAuthor{Name: in.Name}}
		e.Description = "**" + firstNonEmpty(item.Str("Name"), item.Str("NZBName"), "Unknown download") + "**"
		e.Add("Reason", nzbgetReason(status), true)
		e.Add("Category", item.Str("Category"), true)
		e.Add("Size", humanBytes(item.Num("FileSizeMB")*1024*1024), true)
		if h := item.Num("Health"); h > 0 && h < 1000 {
			e.Add("Health", fmt.Sprintf("%.1f%%", h/10), true)
		}
		a.Dispatch(in, "failed", e, nil)
	}
	a.statePrune(in.ID, "h:", active)
	a.stateSet(in.ID, "baseline", "1")

	var status J
	if err := rpc("status", []any{}, &status); err != nil {
		return err
	}
	paused := status.Bool("DownloadPaused")
	was, known := a.stateGet(in.ID, "paused")
	a.stateSet(in.ID, "paused", strconv.FormatBool(paused))
	if known && was != strconv.FormatBool(paused) {
		if paused {
			e := Embed{Title: "NZBGet downloads paused", Color: colorAmber, Author: &EmbedAuthor{Name: in.Name}}
			e.Add("Remaining", humanBytes(status.Num("RemainingSizeMB")*1024*1024), true)
			a.Dispatch(in, "paused", e, nil)
		} else {
			e := Embed{Title: "NZBGet downloads resumed", Color: colorGreen, Author: &EmbedAuthor{Name: in.Name}}
			a.Dispatch(in, "resumed", e, nil)
		}
	}
	return nil
}

// nzbgetReason turns a status such as "FAILURE/UNPACK" into readable text.
func nzbgetReason(status string) string {
	_, detail, _ := strings.Cut(status, "/")
	switch strings.ToUpper(detail) {
	case "PAR":
		return "Par check or repair failed"
	case "UNPACK":
		return "Unpack failed"
	case "MOVE":
		return "Could not move files to the destination"
	case "SCRIPT":
		return "A post-processing script failed"
	case "DISK":
		return "Disk error (out of space?)"
	case "HEALTH":
		return "Too many missing articles"
	case "CONNECTION":
		return "News server connection failed"
	case "FETCH":
		return "Could not fetch the NZB"
	case "BAD":
		return "Marked as bad"
	case "SCAN":
		return "The NZB file could not be read"
	case "PASSWORD":
		return "Password required"
	}
	return status
}

// ---- NZBHydra 2 ----

// hydraTime formats NZBHydra's timestamps, which arrive either as epoch
// seconds or as ISO-8601 text depending on version.
func hydraTime(v any) string {
	switch t := v.(type) {
	case float64:
		if t > 0 {
			return time.Unix(int64(t), 0).Format("2006-01-02 15:04 MST")
		}
	case string:
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			return ts.Local().Format("2006-01-02 15:04 MST")
		}
		return t
	}
	return ""
}

// pollHydra watches indexer states through NZBHydra's stats API and reports
// indexers that NZBHydra has disabled after errors, and when they return.
func pollHydra(ctx context.Context, a *App, in *Integration) error {
	base := strings.TrimRight(strings.TrimSpace(in.Config["url"]), "/")
	key := strings.TrimSpace(in.Config["apikey"])
	if base == "" || key == "" {
		return errors.New("URL and API key are required")
	}
	var indexers []J
	if err := postJSON(ctx, a.http, base+"/api/stats/indexers", "", "", map[string]string{"apikey": key}, &indexers); err != nil {
		return fmt.Errorf("%w (check the API key and that \"Allow stats access via API\" is enabled in NZBHydra)", err)
	}
	minLevel := cfgInt(in.Config, "min_level", 1)
	_, baselined := a.stateGet(in.ID, "baseline")
	active := map[string]bool{}
	for _, ix := range indexers {
		name := ix.Str("indexer")
		if name == "" {
			continue
		}
		state := ix.Str("state")
		// Only system-initiated disables are problems; DISABLED_USER is a choice.
		down := state == "DISABLED_SYSTEM" || (state == "DISABLED_SYSTEM_TEMPORARY" && int(ix.Num("level")) >= minLevel)
		k := "ix:" + name
		_, wasDown := a.stateGet(in.ID, k)
		if down {
			active[k] = true
		}
		switch {
		case down && !wasDown:
			a.stateSet(in.ID, k, state)
			if !baselined {
				continue
			}
			e := Embed{Title: "Indexer disabled", Color: colorRed, Author: &EmbedAuthor{Name: in.Name}}
			e.Description = "**" + name + "**"
			if state == "DISABLED_SYSTEM" {
				e.Add("Status", "Disabled until re-enabled manually", true)
			} else {
				e.Add("Status", "Temporarily disabled", true)
				e.Add("Retry after", hydraTime(ix["disabledUntil"]), true)
			}
			e.Add("Last error", trunc(ix.Str("lastError"), 600), false)
			a.Dispatch(in, "indexer_disabled", e, nil)
		case !down && wasDown && state == "ENABLED":
			e := Embed{Title: "Indexer re-enabled", Color: colorGreen, Author: &EmbedAuthor{Name: in.Name}}
			e.Description = "**" + name + "**"
			a.Dispatch(in, "indexer_enabled", e, nil)
		}
	}
	a.statePrune(in.ID, "ix:", active)
	a.stateSet(in.ID, "baseline", "1")
	return nil
}
