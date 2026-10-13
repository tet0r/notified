package main

import (
	"fmt"
	"strings"
	"time"
)

// Emby's webhook notification posts a JSON object with an "Event" name, a
// ready-made "Title"/"Description", and nested User, Item, Server, Session
// and PlaybackInfo objects. The parser leans on Emby's own Title for anything
// it does not recognise, so new event types still produce a sensible message.

var embyEvents = map[string]string{
	"playback.start":               "play",
	"playback.stop":                "stop",
	"playback.pause":               "pause",
	"playback.unpause":             "resume",
	"playback.progress":            "progress",
	"library.new":                  "new",
	"library.deleted":              "item_deleted",
	"item.markplayed":              "watched",
	"item.markunplayed":            "watched",
	"item.rate":                    "rate",
	"user.authenticated":           "auth_success",
	"user.authenticationfailed":    "auth_failure",
	"user.lockedout":               "auth_failure",
	"system.notificationtest":      "test",
	"system.webhooktest":           "test",
	"system.updateavailable":       "update_available",
	"system.serverrestartrequired": "restart_required",
}

func embyMediaTitle(item J) string {
	name := item.Str("Name")
	switch strings.ToLower(item.Str("Type")) {
	case "episode":
		t := item.Str("SeriesName")
		if item.Str("ParentIndexNumber") != "" && item.Str("IndexNumber") != "" {
			t += fmt.Sprintf(" — S%02dE%02d", int(item.Num("ParentIndexNumber")), int(item.Num("IndexNumber")))
		}
		if name != "" {
			t += " · " + name
		}
		return strings.TrimPrefix(t, " — ")
	case "season":
		return strings.TrimPrefix(item.Str("SeriesName")+" — "+name, " — ")
	case "audio", "musicalbum":
		artist := item.Str("AlbumArtist")
		if artist == "" {
			if arr, ok := item["Artists"].([]any); ok && len(arr) > 0 {
				artist = toStr(arr[0])
			}
		}
		return strings.TrimPrefix(artist+" — "+name, " — ")
	}
	if y := item.Str("ProductionYear"); name != "" && y != "" && y != "0" {
		name += " (" + y + ")"
	}
	return name
}

// ticksToClock converts .NET ticks (100ns units) to h:mm:ss.
func ticksToClock(ticks float64) string {
	if ticks <= 0 {
		return ""
	}
	d := time.Duration(ticks*100) * time.Nanosecond
	return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

func parseEmby(in *Integration, p J) (string, Embed, bool) {
	raw := strings.ToLower(p.Str("Event"))
	event, known := embyEvents[raw]
	switch {
	case known:
	case strings.HasPrefix(raw, "plugins."):
		event = "plugin"
	case raw == "":
		event = "test"
	default:
		event = "other"
	}
	// Progress fires constantly; drop it without a history entry unless enabled.
	if event == "progress" && !in.rule(event).Enabled {
		return "", Embed{}, false
	}
	user := p.Str("User", "Name")
	if strings.HasPrefix(raw, "playback.") && !userAllowed(in, user) {
		return "", Embed{}, false
	}

	item := p.Obj("Item")
	session := p.Obj("Session")
	play := p.Obj("PlaybackInfo")
	e := Embed{Author: &EmbedAuthor{Name: firstNonEmpty(p.Str("Server", "Name"), in.Name)}}
	media := embyMediaTitle(item)
	if media != "" {
		e.Description = "**" + media + "**"
	}
	if base, id := strings.TrimRight(strings.TrimSpace(in.Config["url"]), "/"), item.Str("Id"); base != "" && mediaIDRE.MatchString(id) {
		e.URL = base + "/web/index.html#!/item?id=" + id
		if sid := p.Str("Server", "Id"); mediaIDRE.MatchString(sid) {
			e.URL += "&serverId=" + sid
		}
		e.Thumb(base + "/emby/Items/" + id + "/Images/Primary?maxHeight=300")
	}
	addWho := func() {
		e.Add("User", user, true)
		e.Add("Device", session.Str("DeviceName"), true)
		e.Add("Client", session.Str("Client"), true)
	}
	// Emby writes its own summary line; use it when there is no media title.
	fallback := func() {
		if e.Description == "" {
			e.Description = trunc(firstNonEmpty(p.Str("Description"), p.Str("Title")), 600)
		}
	}

	switch event {
	case "test":
		e.Title, e.Color = "Emby test notification", colorBlurple
		e.Description = "The webhook from **" + e.Author.Name + "** is reaching Notified."
	case "play":
		e.Title, e.Color = "Playback started", colorGreen
		addWho()
	case "pause":
		e.Title, e.Color = "Playback paused", colorAmber
		addWho()
	case "resume":
		e.Title, e.Color = "Playback resumed", colorGreen
		addWho()
	case "progress":
		e.Title, e.Color = "Playback progress", colorGrey
		addWho()
		e.Add("Position", ticksToClock(play.Num("PositionTicks")), true)
	case "stop":
		e.Title, e.Color = "Playback stopped", colorGrey
		addWho()
		if play.Bool("PlayedToCompletion") {
			e.Add("Finished", "Yes", true)
		} else if pos := ticksToClock(play.Num("PositionTicks")); pos != "" {
			e.Add("Stopped at", pos+" of "+firstNonEmpty(ticksToClock(item.Num("RunTimeTicks")), "?"), true)
		}
	case "new":
		e.Title, e.Color = "New in library", colorBlue
		e.Add("Type", item.Str("Type"), true)
		e.Add("Summary", trunc(item.Str("Overview"), 400), false)
		fallback()
	case "item_deleted":
		e.Title, e.Color = "Removed from library", colorGrey
		e.Add("Type", item.Str("Type"), true)
		fallback()
	case "watched":
		e.Title, e.Color = "Marked as played", colorTeal
		if raw == "item.markunplayed" {
			e.Title, e.Color = "Marked as unplayed", colorGrey
		}
		e.Add("User", user, true)
	case "rate":
		e.Title, e.Color = "Item rated", colorPurple
		e.Add("User", user, true)
	case "auth_success":
		e.Title, e.Color = "User signed in", colorGreen
		addWho()
		e.Add("Address", session.Str("RemoteEndPoint"), true)
	case "auth_failure":
		e.Title, e.Color = "Failed sign-in", colorRed
		if raw == "user.lockedout" {
			e.Title = "User locked out"
		}
		e.Add("User", user, true)
		e.Add("Address", session.Str("RemoteEndPoint"), true)
		e.Add("Device", session.Str("DeviceName"), true)
		fallback()
	case "update_available":
		e.Title, e.Color = "Emby update available", colorBlue
		e.Add("Installed version", p.Str("Server", "Version"), true)
		fallback()
	case "restart_required":
		e.Title, e.Color = "Emby needs a restart", colorAmber
		fallback()
	case "plugin":
		e.Title, e.Color = firstNonEmpty(p.Str("Title"), "Plugin changed"), colorBlue
		e.Description = trunc(p.Str("Description"), 600)
	default:
		e.Title, e.Color = firstNonEmpty(p.Str("Title"), "Emby: "+raw), colorGrey
		e.Description = trunc(p.Str("Description"), 600)
		e.Add("User", user, true)
	}
	return event, e, true
}
