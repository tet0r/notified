package main

import "context"

// FieldDef describes one configuration input shown in the web UI.
// Kind is one of: text, password, number, bool, textarea.
type FieldDef struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Kind    string `json:"kind"`
	Default string `json:"default"`
	Help    string `json:"help,omitempty"`
}

type EventDef struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
}

type parseFunc func(in *Integration, p J) (event string, e Embed, ok bool)
type pollFunc func(ctx context.Context, a *App, in *Integration) error

// TypeDef describes an integration type: how it is configured, which events
// it can emit, and how it receives (Parse) or collects (Poll) them.
type TypeDef struct {
	Type            string     `json:"type"`
	Label           string     `json:"label"`
	Group           string     `json:"group"`
	Webhook         bool       `json:"webhook"`
	Poller          bool       `json:"poller"`
	Fields          []FieldDef `json:"fields"`
	Events          []EventDef `json:"events"`
	Setup           string     `json:"setup"`
	DefaultInterval int        `json:"-"`
	Parse           parseFunc  `json:"-"`
	Poll            pollFunc   `json:"-"`
}

var typeOrder = []string{
	"sonarr", "radarr", "lidarr", "readarr", "prowlarr",
	"plex", "tautulli", "overseerr", "jellyseerr",
	"qbittorrent", "sabnzbd",
	"system", "docker", "healthcheck",
}

var typeDefs = map[string]*TypeDef{}

func init() {
	for _, d := range buildTypeDefs() {
		typeDefs[d.Type] = d
	}
}

func starrFields(app string) []FieldDef {
	return []FieldDef{
		{Key: "url", Label: app + " URL", Kind: "text", Help: "Optional. Needed only for stuck-queue checks; the address Notified can reach " + app + " at."},
		{Key: "apikey", Label: app + " API key", Kind: "password", Help: "Optional. Settings → General → API Key."},
		{Key: "interval", Label: "Queue check interval (seconds)", Kind: "number", Default: "600", Help: "0 disables queue checks."},
		{Key: "stuck_minutes", Label: "Report queue items stuck for (minutes)", Kind: "number", Default: "30"},
	}
}

func starrSetup(app string) string {
	return "In " + app + " open Settings → Connect → + → Webhook.\n" +
		"Set URL to {URL} and Method to POST, tick the triggers you want, then press Test and Save.\n" +
		"Optionally fill in the URL and API key below to also be told about downloads stuck in the queue."
}

func buildTypeDefs() []*TypeDef {
	ev := func(key, label string, def bool) EventDef { return EventDef{Key: key, Label: label, Default: def} }
	starrCommon := []EventDef{
		ev("health", "Health issue", true),
		ev("health_restored", "Health restored", true),
		ev("app_update", "Application updated", true),
		ev("manual_interaction", "Manual interaction required", true),
	}
	withCommon := func(evs ...EventDef) []EventDef { return append(evs, starrCommon...) }

	seerrEvents := []EventDef{
		ev("pending", "Request pending approval", true),
		ev("approved", "Request approved", true),
		ev("auto_approved", "Request auto-approved", true),
		ev("available", "Media available", true),
		ev("declined", "Request declined", true),
		ev("failed", "Request failed", true),
		ev("issue_created", "Issue reported", true),
		ev("issue_comment", "Issue comment", true),
		ev("issue_resolved", "Issue resolved", true),
		ev("issue_reopened", "Issue reopened", true),
	}
	seerrFields := []FieldDef{
		{Key: "url", Label: "Public URL", Kind: "text", Help: "Optional. Used to link notifications back to the request page."},
	}
	seerrSetup := func(app string) string {
		return "In " + app + " open Settings → Notifications → Webhook.\n" +
			"Enable the agent, set Webhook URL to {URL}, leave the JSON payload at its default, pick notification types, then Test and Save."
	}

	return []*TypeDef{
		{
			Type: "sonarr", Label: "Sonarr", Group: "Starr apps", Webhook: true, Poller: true,
			Fields: starrFields("Sonarr"), Setup: starrSetup("Sonarr"),
			Events: withCommon(
				ev("grab", "Episode grabbed", true),
				ev("download", "Episode imported", true),
				ev("upgrade", "Episode upgraded", true),
				ev("rename", "Files renamed", false),
				ev("added", "Series added", true),
				ev("deleted", "Series deleted", true),
				ev("file_deleted", "Episode file deleted", false),
				ev("stuck", "Stuck queue item", true),
			),
			DefaultInterval: 600, Parse: parseStarr("sonarr"), Poll: pollStarrQueue("sonarr"),
		},
		{
			Type: "radarr", Label: "Radarr", Group: "Starr apps", Webhook: true, Poller: true,
			Fields: starrFields("Radarr"), Setup: starrSetup("Radarr"),
			Events: withCommon(
				ev("grab", "Movie grabbed", true),
				ev("download", "Movie imported", true),
				ev("upgrade", "Movie upgraded", true),
				ev("rename", "Files renamed", false),
				ev("added", "Movie added", true),
				ev("deleted", "Movie deleted", true),
				ev("file_deleted", "Movie file deleted", false),
				ev("stuck", "Stuck queue item", true),
			),
			DefaultInterval: 600, Parse: parseStarr("radarr"), Poll: pollStarrQueue("radarr"),
		},
		{
			Type: "lidarr", Label: "Lidarr", Group: "Starr apps", Webhook: true, Poller: true,
			Fields: starrFields("Lidarr"), Setup: starrSetup("Lidarr"),
			Events: withCommon(
				ev("grab", "Album grabbed", true),
				ev("download", "Album imported", true),
				ev("upgrade", "Album upgraded", true),
				ev("failure", "Download or import failure", true),
				ev("rename", "Files renamed", false),
				ev("retag", "Tracks retagged", false),
				ev("added", "Artist added", true),
				ev("deleted", "Artist or album deleted", true),
				ev("stuck", "Stuck queue item", true),
			),
			DefaultInterval: 600, Parse: parseStarr("lidarr"), Poll: pollStarrQueue("lidarr"),
		},
		{
			Type: "readarr", Label: "Readarr", Group: "Starr apps", Webhook: true, Poller: true,
			Fields: starrFields("Readarr"), Setup: starrSetup("Readarr"),
			Events: withCommon(
				ev("grab", "Book grabbed", true),
				ev("download", "Book imported", true),
				ev("upgrade", "Book upgraded", true),
				ev("failure", "Download or import failure", true),
				ev("rename", "Files renamed", false),
				ev("retag", "Files retagged", false),
				ev("added", "Author added", true),
				ev("deleted", "Author or book deleted", true),
				ev("file_deleted", "Book file deleted", false),
				ev("stuck", "Stuck queue item", true),
			),
			DefaultInterval: 600, Parse: parseStarr("readarr"), Poll: pollStarrQueue("readarr"),
		},
		{
			Type: "prowlarr", Label: "Prowlarr", Group: "Starr apps", Webhook: true,
			Fields: []FieldDef{}, Setup: starrSetup("Prowlarr"),
			Events: []EventDef{
				ev("grab", "Release grabbed", false),
				ev("health", "Health issue", true),
				ev("health_restored", "Health restored", true),
				ev("app_update", "Application updated", true),
			},
			Parse: parseStarr("prowlarr"),
		},
		{
			Type: "plex", Label: "Plex", Group: "Media servers", Webhook: true,
			Fields: []FieldDef{
				{Key: "users", Label: "Only these users", Kind: "text", Help: "Optional comma-separated Plex usernames. Blank means everyone."},
			},
			Setup: "Plex webhooks need an active Plex Pass.\n" +
				"In Plex Web open Settings → Webhooks → Add Webhook and paste {URL}.",
			Events: []EventDef{
				ev("play", "Playback started", true),
				ev("pause", "Playback paused", false),
				ev("resume", "Playback resumed", false),
				ev("stop", "Playback stopped", true),
				ev("watched", "Item watched (scrobble)", false),
				ev("rate", "Item rated", false),
				ev("new", "New item added to library", true),
				ev("on_deck", "New item on deck", false),
				ev("new_device", "New device connected", true),
				ev("shared_play", "Shared user started playback", false),
				ev("db_backup", "Database backup completed", false),
				ev("db_corrupt", "Database corruption detected", true),
			},
			Parse: parsePlex,
		},
		{
			Type: "tautulli", Label: "Tautulli", Group: "Media servers", Webhook: true,
			Fields: []FieldDef{
				{Key: "users", Label: "Only these users", Kind: "text", Help: "Optional comma-separated usernames. Blank means everyone."},
			},
			Setup: "In Tautulli open Settings → Notification Agents → Add → Webhook.\n" +
				"Set Webhook URL to {URL} and Method to POST, then tick the triggers you want.\n" +
				"On the Data tab paste this into JSON Data for each ticked trigger:\n" + tautulliTemplate,
			Events: []EventDef{
				ev("play", "Playback started", true),
				ev("stop", "Playback stopped", true),
				ev("pause", "Playback paused", false),
				ev("resume", "Playback resumed", false),
				ev("watched", "Item watched", false),
				ev("change", "Transcode decision changed", false),
				ev("error", "Playback error", true),
				ev("buffer", "Buffer warning", true),
				ev("concurrent", "User concurrent streams", true),
				ev("newdevice", "User new device", true),
				ev("created", "Recently added", true),
				ev("intdown", "Plex server down", true),
				ev("intup", "Plex server back up", true),
				ev("extdown", "Plex remote access down", true),
				ev("extup", "Plex remote access back up", true),
				ev("pmsupdate", "Plex update available", true),
				ev("update", "Tautulli update available", true),
			},
			Parse: parseTautulli,
		},
		{
			Type: "overseerr", Label: "Overseerr", Group: "Requests", Webhook: true,
			Fields: seerrFields, Setup: seerrSetup("Overseerr"), Events: seerrEvents,
			Parse: parseSeerr,
		},
		{
			Type: "jellyseerr", Label: "Jellyseerr", Group: "Requests", Webhook: true,
			Fields: seerrFields, Setup: seerrSetup("Jellyseerr"), Events: seerrEvents,
			Parse: parseSeerr,
		},
		{
			Type: "qbittorrent", Label: "qBittorrent", Group: "Downloaders", Poller: true,
			Fields: []FieldDef{
				{Key: "url", Label: "Web UI URL", Kind: "text", Help: "e.g. http://qbittorrent:8080"},
				{Key: "username", Label: "Username", Kind: "text", Help: "Leave blank if authentication is bypassed for this host."},
				{Key: "password", Label: "Password", Kind: "password"},
				{Key: "interval", Label: "Check interval (seconds)", Kind: "number", Default: "300"},
				{Key: "stalled_minutes", Label: "Report downloads stalled for (minutes)", Kind: "number", Default: "60"},
			},
			Setup: "Notified signs in to the qBittorrent Web UI and reports downloads that stall or error.",
			Events: []EventDef{
				ev("stalled", "Download stalled", true),
				ev("errored", "Torrent error or missing files", true),
			},
			DefaultInterval: 300, Poll: pollQbit,
		},
		{
			Type: "sabnzbd", Label: "SABnzbd", Group: "Downloaders", Poller: true,
			Fields: []FieldDef{
				{Key: "url", Label: "SABnzbd URL", Kind: "text", Help: "e.g. http://sabnzbd:8080"},
				{Key: "apikey", Label: "API key", Kind: "password", Help: "Config → General → API Key."},
				{Key: "interval", Label: "Check interval (seconds)", Kind: "number", Default: "300"},
			},
			Setup: "Notified polls the SABnzbd API and reports failed downloads and queue pauses.",
			Events: []EventDef{
				ev("failed", "Download failed", true),
				ev("paused", "Queue paused", true),
				ev("resumed", "Queue resumed", false),
			},
			DefaultInterval: 300, Poll: pollSab,
		},
		{
			Type: "system", Label: "System monitor", Group: "System", Poller: true,
			Fields: []FieldDef{
				{Key: "interval", Label: "Check interval (seconds)", Kind: "number", Default: "60"},
				{Key: "cpu_threshold", Label: "CPU alert threshold (%)", Kind: "number", Default: "90", Help: "Must hold for 3 checks in a row. 0 disables."},
				{Key: "mem_threshold", Label: "Memory alert threshold (%)", Kind: "number", Default: "90", Help: "0 disables."},
				{Key: "disk_threshold", Label: "Disk alert threshold (%)", Kind: "number", Default: "90", Help: "0 disables."},
				{Key: "disk_paths", Label: "Disk paths to watch", Kind: "textarea", Default: "/host", Help: "One path per line, as seen inside the container."},
				{Key: "snapshot_hours", Label: "Send a snapshot every (hours)", Kind: "number", Default: "0", Help: "0 disables the periodic summary."},
			},
			Setup: "Reports host CPU, memory and disk usage. Mount the disks you want watched into the container " +
				"(the sample compose file mounts the host root read-only at /host).",
			Events: []EventDef{
				ev("cpu_high", "CPU usage high", true),
				ev("memory_high", "Memory usage high", true),
				ev("disk_high", "Disk usage high", true),
				ev("recovered", "Back to normal", true),
				ev("snapshot", "Periodic snapshot", true),
			},
			DefaultInterval: 60, Poll: pollSystem,
		},
		{
			Type: "docker", Label: "Docker containers", Group: "System", Poller: true,
			Fields: []FieldDef{
				{Key: "socket", Label: "Docker socket", Kind: "text", Default: "/var/run/docker.sock"},
				{Key: "interval", Label: "Check interval (seconds)", Kind: "number", Default: "30"},
				{Key: "ignore", Label: "Ignore containers", Kind: "text", Help: "Comma-separated names; * wildcards allowed."},
			},
			Setup: "Reports containers that stop, start or change health. Requires the Docker socket to be mounted " +
				"(/var/run/docker.sock:/var/run/docker.sock:ro in the compose file).",
			Events: []EventDef{
				ev("container_stopped", "Container stopped", true),
				ev("container_started", "Container started", true),
				ev("container_unhealthy", "Container unhealthy", true),
				ev("container_healthy", "Container healthy again", true),
			},
			DefaultInterval: 30, Poll: pollDocker,
		},
		{
			Type: "healthcheck", Label: "Service check", Group: "System", Poller: true,
			Fields: []FieldDef{
				{Key: "target", Label: "Target", Kind: "text", Help: "An http(s):// URL, or host:port for a TCP check."},
				{Key: "interval", Label: "Check interval (seconds)", Kind: "number", Default: "60"},
				{Key: "timeout", Label: "Timeout (seconds)", Kind: "number", Default: "10"},
				{Key: "failures", Label: "Failures before alerting", Kind: "number", Default: "3"},
				{Key: "expected_status", Label: "Expected HTTP status", Kind: "number", Help: "Blank accepts any status below 400."},
				{Key: "insecure_tls", Label: "Skip TLS certificate verification", Kind: "bool"},
			},
			Setup: "Checks that a URL or TCP port responds and reports when it goes down and comes back.",
			Events: []EventDef{
				ev("down", "Service down", true),
				ev("up", "Service recovered", true),
			},
			DefaultInterval: 60, Poll: pollHealth,
		},
	}
}

const tautulliTemplate = `{"action":"{action}","user":"{user}","title":"{title}","media_type":"{media_type}","library":"{library_name}","player":"{player}","platform":"{platform}","quality":"{quality_profile}","transcode":"{transcode_decision}","progress":"{progress_percent}","streams":"{streams}","user_streams":"{user_streams}","ip":"{ip_address}","poster":"{poster_url}","server":"{server_name}","server_version":"{server_version}","update_version":"{update_version}","tautulli_version":"{tautulli_update_version}","plex_url":"{plex_url}"}`
