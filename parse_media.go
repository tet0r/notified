package main

import (
	"fmt"
	"strings"
)

// userAllowed applies the optional "only these users" filter.
func userAllowed(in *Integration, user string) bool {
	allowed := splitList(in.Config["users"])
	if len(allowed) == 0 {
		return true
	}
	for _, u := range allowed {
		if strings.EqualFold(u, user) {
			return true
		}
	}
	return false
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- Plex ----

var plexEvents = map[string]string{
	"media.play":               "play",
	"media.pause":              "pause",
	"media.resume":             "resume",
	"media.stop":               "stop",
	"media.scrobble":           "watched",
	"media.rate":               "rate",
	"library.new":              "new",
	"library.on.deck":          "on_deck",
	"device.new":               "new_device",
	"playback.started":         "shared_play",
	"admin.database.backup":    "db_backup",
	"admin.database.corrupted": "db_corrupt",
}

func plexMediaTitle(m J) string {
	switch m.Str("type") {
	case "episode":
		t := m.Str("grandparentTitle")
		if m.Str("parentIndex") != "" && m.Str("index") != "" {
			t += fmt.Sprintf(" — S%02dE%02d", int(m.Num("parentIndex")), int(m.Num("index")))
		}
		if ep := m.Str("title"); ep != "" {
			t += " · " + ep
		}
		return t
	case "track":
		return firstNonEmpty(m.Str("grandparentTitle"), m.Str("parentTitle")) + " — " + m.Str("title")
	}
	t := m.Str("title")
	if y := m.Str("year"); t != "" && y != "" {
		t += " (" + y + ")"
	}
	return t
}

func parsePlex(in *Integration, p J) (string, Embed, bool) {
	raw := p.Str("event")
	event, known := plexEvents[raw]
	if !known {
		event = "other"
	}
	user := p.Str("Account", "title")
	if strings.HasPrefix(raw, "media.") && !userAllowed(in, user) {
		return "", Embed{}, false
	}
	m := p.Obj("Metadata")
	e := Embed{Author: &EmbedAuthor{Name: firstNonEmpty(p.Str("Server", "title"), in.Name)}}
	if media := plexMediaTitle(m); media != "" {
		e.Description = "**" + media + "**"
	}
	e.Add("User", user, true)
	e.Add("Player", p.Str("Player", "title"), true)
	e.Add("Library", m.Str("librarySectionTitle"), true)

	switch event {
	case "play":
		e.Title, e.Color = "Playback started", colorGreen
	case "pause":
		e.Title, e.Color = "Playback paused", colorAmber
	case "resume":
		e.Title, e.Color = "Playback resumed", colorGreen
	case "stop":
		e.Title, e.Color = "Playback stopped", colorGrey
	case "watched":
		e.Title, e.Color = "Watched", colorTeal
	case "rate":
		e.Title, e.Color = "Item rated", colorPurple
		e.Add("Rating", p.Str("rating"), true)
	case "new":
		e.Title, e.Color = "New in library", colorBlue
		e.Add("Summary", trunc(m.Str("summary"), 400), false)
	case "on_deck":
		e.Title, e.Color = "New on deck", colorBlue
	case "new_device":
		e.Title, e.Color = "New device connected", colorAmber
	case "shared_play":
		e.Title, e.Color = "Shared user started playback", colorGreen
	case "db_backup":
		e.Title, e.Color = "Plex database backup completed", colorGreen
	case "db_corrupt":
		e.Title, e.Color = "Plex database corruption detected", colorRed
	default:
		e.Title, e.Color = "Plex: "+firstNonEmpty(raw, "event"), colorGrey
	}
	return event, e, true
}

// ---- Tautulli ----

var tautulliTitles = map[string]struct {
	title string
	color int
}{
	"play":       {"Playback started", colorGreen},
	"stop":       {"Playback stopped", colorGrey},
	"pause":      {"Playback paused", colorAmber},
	"resume":     {"Playback resumed", colorGreen},
	"watched":    {"Watched", colorTeal},
	"change":     {"Transcode decision changed", colorAmber},
	"error":      {"Playback error", colorRed},
	"buffer":     {"Buffer warning", colorAmber},
	"concurrent": {"User concurrent streams", colorAmber},
	"newdevice":  {"User new device", colorAmber},
	"created":    {"Recently added", colorBlue},
	"intdown":    {"Plex server is down", colorRed},
	"intup":      {"Plex server is back up", colorGreen},
	"extdown":    {"Plex remote access is down", colorRed},
	"extup":      {"Plex remote access is back up", colorGreen},
	"pmsupdate":  {"Plex update available", colorBlue},
	"update":     {"Tautulli update available", colorBlue},
}

func parseTautulli(in *Integration, p J) (string, Embed, bool) {
	action := strings.ToLower(p.Str("action"))
	user := p.Str("user")
	if user != "" && !userAllowed(in, user) {
		return "", Embed{}, false
	}
	e := Embed{Author: &EmbedAuthor{Name: firstNonEmpty(p.Str("server"), in.Name)}}
	event := action
	if t, ok := tautulliTitles[action]; ok {
		e.Title, e.Color = t.title, t.color
	} else {
		// Tautulli's own "Test" button sends placeholder data with no action.
		event = "test"
		if action != "" {
			event = "other"
		}
		e.Title, e.Color = "Tautulli: "+firstNonEmpty(action, "test notification"), colorBlurple
	}
	if title := p.Str("title"); title != "" {
		e.Description = "**" + title + "**"
	}
	e.URL = p.Str("plex_url")
	e.Thumb(p.Str("poster"))
	e.Add("User", user, true)
	e.Add("Player", firstNonEmpty(p.Str("player"), p.Str("platform")), true)
	e.Add("Library", p.Str("library"), true)
	e.Add("Quality", p.Str("quality"), true)
	e.Add("Stream", titleCase(p.Str("transcode")), true)
	switch action {
	case "stop", "pause", "watched":
		if pr := p.Str("progress"); pr != "" {
			e.Add("Progress", pr+"%", true)
		}
	case "concurrent":
		e.Add("User streams", p.Str("user_streams"), true)
	case "newdevice":
		e.Add("IP address", p.Str("ip"), true)
	case "pmsupdate":
		e.Add("Current version", p.Str("server_version"), true)
		e.Add("New version", p.Str("update_version"), true)
	case "update":
		e.Add("New version", p.Str("tautulli_version"), true)
	}
	if action == "play" || action == "stop" {
		e.Add("Active streams", p.Str("streams"), true)
	}
	return event, e, true
}

// ---- Seerr / Overseerr / Jellyseerr ----
//
// Seerr is the merged successor of Overseerr and Jellyseerr and sends the same
// default webhook payload, so one parser serves all three.

var seerrEventMap = map[string]struct {
	event string
	color int
}{
	"MEDIA_PENDING":        {"pending", colorAmber},
	"MEDIA_APPROVED":       {"approved", colorBlue},
	"MEDIA_AUTO_APPROVED":  {"auto_approved", colorBlue},
	"MEDIA_AUTO_REQUESTED": {"auto_requested", colorBlue},
	"MEDIA_AVAILABLE":      {"available", colorGreen},
	"MEDIA_DECLINED":       {"declined", colorRed},
	"MEDIA_FAILED":         {"failed", colorRed},
	"ISSUE_CREATED":        {"issue_created", colorAmber},
	"ISSUE_COMMENT":        {"issue_comment", colorPurple},
	"ISSUE_RESOLVED":       {"issue_resolved", colorGreen},
	"ISSUE_REOPENED":       {"issue_reopened", colorAmber},
	"TEST_NOTIFICATION":    {"test", colorBlurple},
}

func parseSeerr(in *Integration, p J) (string, Embed, bool) {
	nt := strings.ToUpper(p.Str("notification_type"))
	event, color := "other", colorGrey
	if m, ok := seerrEventMap[nt]; ok {
		event, color = m.event, m.color
	}
	e := Embed{
		Author:      &EmbedAuthor{Name: in.Name},
		Title:       firstNonEmpty(p.Str("event"), nt, "Request notification"),
		Color:       color,
		Description: trunc(p.Str("message"), 600),
	}
	if subject := p.Str("subject"); subject != "" {
		e.Description = strings.TrimSpace("**" + subject + "**\n" + e.Description)
	}
	if event == "test" && p.Str("event") == "" {
		e.Title = in.Name + " test notification"
	}
	e.Thumb(p.Str("image"))

	media := p.Obj("media")
	if id, mt := media.Str("tmdbId"), media.Str("media_type"); id != "" && (mt == "movie" || mt == "tv") {
		if base := strings.TrimRight(in.Config["url"], "/"); base != "" {
			e.URL = base + "/" + mt + "/" + id
		} else {
			e.URL = "https://www.themoviedb.org/" + mt + "/" + id
		}
	}
	e.Add("Requested by", p.Str("request", "requestedBy_username"), true)
	e.Add("Status", titleCase(strings.ToLower(strings.ReplaceAll(media.Str("status"), "_", " "))), true)
	e.Add("Reported by", p.Str("issue", "reportedBy_username"), true)
	e.Add("Issue type", titleCase(strings.ToLower(p.Str("issue", "issue_type"))), true)
	if c := p.Str("comment", "comment_message"); c != "" {
		e.Add("Comment from "+firstNonEmpty(p.Str("comment", "commentedBy_username"), "user"), trunc(c, 600), false)
	}
	for _, x := range p.Arr("extra") {
		if name := x.Str("name"); name != "" {
			e.Add(name, x.Str("value"), true)
		}
	}
	return event, e, true
}
