package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Jellyfin has no built-in webhooks; these payloads come from the official
// Webhook plugin's Generic destination with "Send All Properties" enabled,
// which posts the plugin's whole data object as one flat JSON object.

var jellyfinEvents = map[string]string{
	"playbackstart":               "play",
	"playbackstop":                "stop",
	"playbackprogress":            "progress",
	"itemadded":                   "new",
	"itemdeleted":                 "item_deleted",
	"authenticationfailure":       "auth_failure",
	"authenticationsuccess":       "auth_success",
	"sessionstart":                "session_start",
	"pendingrestart":              "pending_restart",
	"taskcompleted":               "task_completed",
	"subtitledownloadfailure":     "subtitle_failure",
	"plugininstalled":             "plugin",
	"pluginupdated":               "plugin",
	"pluginuninstalled":           "plugin",
	"plugininstalling":            "plugin",
	"plugininstallationcancelled": "plugin",
	"plugininstallationfailed":    "plugin_failed",
	"usercreated":                 "user_created",
	"userdeleted":                 "user_deleted",
	"userlockedout":               "user_locked_out",
	"userpasswordchanged":         "user_changed",
	"userupdated":                 "user_changed",
	"userdatasaved":               "user_data",
}

// mediaIDRE matches the item ids Jellyfin and Emby use. Ids are placed in
// request paths, so anything else (slashes, dots, query characters) is refused.
var mediaIDRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// jfStr reads a string the plugin has pre-escaped for templates (it turns
// every `"` into `\"`), undoing that so titles read normally.
func jfStr(p J, key string) string {
	return strings.ReplaceAll(p.Str(key), `\"`, `"`)
}

func jellyfinMediaTitle(p J) string {
	name := jfStr(p, "name")
	switch strings.ToLower(p.Str("itemtype")) {
	case "episode":
		t := jfStr(p, "seriesname")
		if p.Str("seasonnumber") != "" && p.Str("episodenumber") != "" {
			t += fmt.Sprintf(" — S%02dE%02d", int(p.Num("seasonnumber")), int(p.Num("episodenumber")))
		}
		if name != "" {
			t += " · " + name
		}
		return strings.TrimPrefix(t, " — ")
	case "season":
		return strings.TrimPrefix(jfStr(p, "seriesname")+" — "+name, " — ")
	case "audio", "musicalbum":
		return strings.TrimPrefix(jfStr(p, "artist")+" — "+name, " — ")
	}
	if y := p.Str("year"); name != "" && y != "" && y != "0" {
		name += " (" + y + ")"
	}
	return name
}

func parseJellyfin(in *Integration, raw J) (string, Embed, bool) {
	// The plugin's key casing is an implementation detail; match keys
	// case-insensitively, as the plugin itself does.
	p := J{}
	for k, v := range raw {
		p[strings.ToLower(k)] = v
	}
	nt := p.Str("notificationtype")
	event, known := jellyfinEvents[strings.ToLower(nt)]
	switch {
	case nt == "":
		event = "test"
	case !known:
		event = "other"
	}
	if event == "task_completed" && !strings.EqualFold(p.Str("resultstatus"), "Completed") {
		event = "task_failed"
	}
	// These fire constantly; drop them without a history entry unless enabled.
	if (event == "progress" || event == "user_data") && !in.rule(event).Enabled {
		return "", Embed{}, false
	}
	user := jfStr(p, "notificationusername")
	isPlayback := event == "play" || event == "stop" || event == "progress"
	if isPlayback && !userAllowed(in, user) {
		return "", Embed{}, false
	}

	e := Embed{Author: &EmbedAuthor{Name: firstNonEmpty(jfStr(p, "servername"), in.Name)}}
	media := jellyfinMediaTitle(p)
	if media != "" {
		e.Description = "**" + media + "**"
	}
	// Artwork is downloaded by Notified, so it must only ever come from the
	// address the admin configured, never from one supplied in the webhook:
	// otherwise anyone holding the webhook URL could make Notified fetch
	// arbitrary internal addresses.
	id := p.Str("itemid")
	if !mediaIDRE.MatchString(id) {
		id = ""
	}
	configured := strings.TrimRight(strings.TrimSpace(in.Config["url"]), "/")
	linkBase := configured
	if linkBase == "" {
		// A link is only text in the Discord message; the payload's address is fine for that.
		if u := strings.TrimRight(p.Str("serverurl"), "/"); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			linkBase = u
		}
	}
	if id != "" && linkBase != "" {
		e.URL = linkBase + "/web/index.html#!/details?id=" + id
	}
	if id != "" && configured != "" {
		e.Thumb(configured + "/Items/" + id + "/Images/Primary?maxHeight=300")
	}
	addWho := func() {
		e.Add("User", user, true)
		e.Add("Device", jfStr(p, "devicename"), true)
		e.Add("Client", firstNonEmpty(jfStr(p, "clientname"), jfStr(p, "client")), true)
	}

	switch event {
	case "test":
		e.Title, e.Color = "Jellyfin test notification", colorBlurple
		e.Description = "The webhook from **" + e.Author.Name + "** is reaching Notified."
	case "play":
		e.Title, e.Color = "Playback started", colorGreen
		addWho()
	case "stop":
		e.Title, e.Color = "Playback stopped", colorGrey
		addWho()
		if p.Bool("playedtocompletion") {
			e.Add("Finished", "Yes", true)
		} else if pos := p.Str("playbackposition"); pos != "" {
			e.Add("Stopped at", pos+" of "+firstNonEmpty(p.Str("runtime"), "?"), true)
		}
	case "progress":
		e.Title, e.Color = "Playback progress", colorGrey
		if p.Bool("ispaused") {
			e.Title, e.Color = "Playback paused", colorAmber
		}
		addWho()
		e.Add("Position", p.Str("playbackposition"), true)
	case "new":
		e.Title, e.Color = "New in library", colorBlue
		e.Add("Type", p.Str("itemtype"), true)
		e.Add("Genres", jfStr(p, "genres"), true)
		e.Add("Summary", trunc(jfStr(p, "overview"), 400), false)
	case "item_deleted":
		e.Title, e.Color = "Removed from library", colorGrey
		e.Add("Type", p.Str("itemtype"), true)
	case "auth_failure":
		e.Title, e.Color = "Failed sign-in", colorRed
		e.Add("Username", firstNonEmpty(jfStr(p, "username"), user), true)
		e.Add("Address", p.Str("remoteendpoint"), true)
		e.Add("Device", jfStr(p, "devicename"), true)
		e.Add("App", firstNonEmpty(jfStr(p, "app"), jfStr(p, "clientname")), true)
	case "auth_success":
		e.Title, e.Color = "User signed in", colorGreen
		addWho()
		e.Add("Address", p.Str("remoteendpoint"), true)
	case "session_start":
		e.Title, e.Color = "Session started", colorBlue
		addWho()
		e.Add("Address", p.Str("remoteendpoint"), true)
	case "pending_restart":
		e.Title, e.Color = "Jellyfin needs a restart", colorAmber
	case "task_completed", "task_failed":
		e.Title, e.Color = "Scheduled task completed", colorGreen
		if event == "task_failed" {
			e.Title, e.Color = "Scheduled task did not complete", colorRed
		}
		e.Description = "**" + firstNonEmpty(jfStr(p, "taskname"), "Unknown task") + "**"
		e.Add("Result", p.Str("resultstatus"), true)
		e.Add("Category", jfStr(p, "taskcategory"), true)
		e.Add("Error", trunc(firstNonEmpty(jfStr(p, "resulterrormessage"), jfStr(p, "resultlongerrormessage")), 600), false)
	case "subtitle_failure":
		e.Title, e.Color = "Subtitle download failed", colorAmber
	case "plugin", "plugin_failed":
		e.Title, e.Color = "Plugin "+jellyfinPluginVerb(nt), colorBlue
		if event == "plugin_failed" {
			e.Color = colorRed
		}
		e.Description = "**" + firstNonEmpty(jfStr(p, "pluginname"), "Unknown plugin") + "**"
		e.Add("Version", p.Str("pluginversion"), true)
		e.Add("Error", trunc(jfStr(p, "exceptionmessage"), 600), false)
	case "user_created":
		e.Title, e.Color = "User created", colorBlue
		e.Add("User", user, true)
	case "user_deleted":
		e.Title, e.Color = "User deleted", colorGrey
		e.Add("User", user, true)
	case "user_locked_out":
		e.Title, e.Color = "User locked out", colorRed
		e.Add("User", user, true)
	case "user_changed":
		e.Title, e.Color = "User updated", colorGrey
		if strings.EqualFold(nt, "UserPasswordChanged") {
			e.Title = "User password changed"
		}
		e.Add("User", user, true)
	case "user_data":
		e.Title, e.Color = "Watch state changed", colorGrey
		e.Add("User", user, true)
		e.Add("Reason", p.Str("savereason"), true)
	default:
		e.Title, e.Color = "Jellyfin: "+nt, colorGrey
		e.Add("User", user, true)
	}
	return event, e, true
}

func jellyfinPluginVerb(nt string) string {
	switch strings.ToLower(nt) {
	case "plugininstalled":
		return "installed"
	case "pluginupdated":
		return "updated"
	case "pluginuninstalled":
		return "uninstalled"
	case "plugininstalling":
		return "installing"
	case "plugininstallationcancelled":
		return "installation cancelled"
	case "plugininstallationfailed":
		return "installation failed"
	}
	return "changed"
}
