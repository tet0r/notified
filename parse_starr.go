package main

import (
	"context"
	"fmt"
	"strings"
)

type starrKind struct {
	label string // app name
	noun  string // what a release is called
}

var starrKinds = map[string]starrKind{
	"sonarr":   {"Sonarr", "Episode"},
	"radarr":   {"Radarr", "Movie"},
	"lidarr":   {"Lidarr", "Album"},
	"readarr":  {"Readarr", "Book"},
	"prowlarr": {"Prowlarr", "Release"},
}

var starrEvents = map[string]string{
	"test":                      "test",
	"grab":                      "grab",
	"download":                  "download",
	"albumdownload":             "download",
	"rename":                    "rename",
	"retag":                     "retag",
	"seriesadd":                 "added",
	"movieadded":                "added",
	"artistadd":                 "added",
	"artistadded":               "added",
	"authoradd":                 "added",
	"authoradded":               "added",
	"seriesdelete":              "deleted",
	"moviedelete":               "deleted",
	"artistdelete":              "deleted",
	"albumdelete":               "deleted",
	"authordelete":              "deleted",
	"bookdelete":                "deleted",
	"episodefiledelete":         "file_deleted",
	"moviefiledelete":           "file_deleted",
	"bookfiledelete":            "file_deleted",
	"health":                    "health",
	"healthrestored":            "health_restored",
	"applicationupdate":         "app_update",
	"manualinteractionrequired": "manual_interaction",
	"downloadfailure":           "failure",
	"importfailure":             "failure",
}

// starrPoster picks cover art from a Starr "images" array.
func starrPoster(obj J) string {
	var fallback string
	for _, img := range obj.Arr("images") {
		u := firstNonEmpty(img.Str("remoteUrl"), img.Str("url"))
		if !strings.HasPrefix(u, "http") {
			continue
		}
		switch strings.ToLower(img.Str("coverType")) {
		case "poster", "cover":
			return u
		}
		if fallback == "" {
			fallback = u
		}
	}
	return fallback
}

// starrSubject describes what the event is about: a heading, optional detail
// lines, an external link and artwork.
func starrSubject(kind string, p J) (title, detail, link, poster string) {
	switch kind {
	case "sonarr":
		s := p.Obj("series")
		title = s.Str("title")
		poster = starrPoster(s)
		if id := s.Str("tvdbId"); id != "" && id != "0" {
			link = "https://thetvdb.com/?tab=series&id=" + id
		}
		var lines []string
		for _, ep := range p.Arr("episodes") {
			line := fmt.Sprintf("S%02dE%02d", int(ep.Num("seasonNumber")), int(ep.Num("episodeNumber")))
			if t := ep.Str("title"); t != "" {
				line += " · " + t
			}
			lines = append(lines, line)
		}
		detail = joinLimited(lines, 10)
	case "radarr":
		m := p.Obj("movie")
		title = m.Str("title")
		if y := m.Str("year"); title != "" && y != "" && y != "0" {
			title += " (" + y + ")"
		}
		poster = starrPoster(m)
		if id := m.Str("tmdbId"); id != "" && id != "0" {
			link = "https://www.themoviedb.org/movie/" + id
		} else if id := m.Str("imdbId"); id != "" {
			link = "https://www.imdb.com/title/" + id
		}
	case "lidarr":
		ar := p.Obj("artist")
		title = ar.Str("name")
		poster = starrPoster(ar)
		if id := ar.Str("mbId"); id != "" {
			link = "https://musicbrainz.org/artist/" + id
		}
		albums := p.Arr("albums")
		if al := p.Obj("album"); len(al) > 0 {
			albums = append(albums, al)
		}
		var lines []string
		for _, al := range albums {
			lines = append(lines, al.Str("title"))
			if poster == "" {
				poster = starrPoster(al)
			}
		}
		detail = joinLimited(lines, 10)
	case "readarr":
		au := p.Obj("author")
		title = au.Str("name")
		poster = starrPoster(au)
		books := p.Arr("books")
		if b := p.Obj("book"); len(b) > 0 {
			books = append(books, b)
		}
		var lines []string
		for _, b := range books {
			lines = append(lines, b.Str("title"))
			if poster == "" {
				poster = starrPoster(b)
			}
		}
		detail = joinLimited(lines, 10)
	}
	return
}

func joinLimited(lines []string, max int) string {
	var out []string
	for _, l := range lines {
		if l != "" {
			out = append(out, l)
		}
	}
	if len(out) > max {
		extra := len(out) - max
		out = append(out[:max], fmt.Sprintf("…and %d more", extra))
	}
	return strings.Join(out, "\n")
}

func parseStarr(kind string) parseFunc {
	k := starrKinds[kind]
	return func(in *Integration, p J) (string, Embed, bool) {
		raw := strings.ToLower(p.Str("eventType"))
		event, known := starrEvents[raw]
		if !known {
			event = "other"
		}
		if event == "download" && p.Bool("isUpgrade") {
			event = "upgrade"
		}

		e := Embed{Author: &EmbedAuthor{Name: firstNonEmpty(p.Str("instanceName"), in.Name)}}
		subject, detail, link, poster := starrSubject(kind, p)
		e.URL = link
		e.Thumb(poster)
		if subject != "" {
			e.Description = "**" + subject + "**"
			if detail != "" {
				e.Description += "\n" + detail
			}
		}

		rel := p.Obj("release")
		file := J{}
		for _, key := range []string{"episodeFile", "movieFile", "bookFile", "trackFile"} {
			if f := p.Obj(key); len(f) > 0 {
				file = f
				break
			}
		}
		client := firstNonEmpty(p.Str("downloadClient"), p.Str("downloadClientName"))

		switch event {
		case "test":
			e.Title = k.label + " test notification"
			e.Description = "The webhook from **" + e.Author.Name + "** is reaching Notified."
			e.Color = colorBlurple
		case "grab":
			e.Title = k.noun + " grabbed"
			e.Color = colorAmber
			e.Add("Quality", rel.Str("quality"), true)
			e.Add("Size", humanBytes(rel.Num("size")), true)
			e.Add("Indexer", rel.Str("indexer"), true)
			e.Add("Release group", rel.Str("releaseGroup"), true)
			e.Add("Download client", client, true)
			if s := firstNonEmpty(rel.Str("customFormatScore"), p.Str("customFormatInfo", "customFormatScore")); s != "" && s != "0" {
				e.Add("Custom format score", s, true)
			}
			e.Add("Release", rel.Str("releaseTitle"), false)
		case "download", "upgrade":
			e.Title = k.noun + " imported"
			e.Color = colorGreen
			if event == "upgrade" {
				e.Title = k.noun + " upgraded"
				e.Color = colorTeal
			}
			e.Add("Quality", firstNonEmpty(file.Str("quality"), rel.Str("quality")), true)
			e.Add("Size", humanBytes(firstNum(file.Num("size"), rel.Num("size"))), true)
			e.Add("Release group", firstNonEmpty(file.Str("releaseGroup"), rel.Str("releaseGroup")), true)
			e.Add("Download client", client, true)
			e.Add("File", firstNonEmpty(file.Str("relativePath"), file.Str("path")), false)
		case "rename":
			e.Title = k.label + " renamed files"
			e.Color = colorGrey
			if n := len(p.Arr("renamedEpisodeFiles")) + len(p.Arr("renamedMovieFiles")) + len(p.Arr("renamedBookFiles")) + len(p.Arr("renamedTrackFiles")); n > 0 {
				e.Add("Files", fmt.Sprintf("%d", n), true)
			}
		case "retag":
			e.Title = k.label + " retagged files"
			e.Color = colorGrey
		case "added":
			e.Title = "Added to " + k.label
			e.Color = colorBlue
		case "deleted":
			e.Title = "Deleted from " + k.label
			e.Color = colorGrey
			if p.Bool("deletedFiles") {
				e.Add("Files deleted", "Yes", true)
			} else {
				e.Add("Files deleted", "No", true)
			}
		case "file_deleted":
			e.Title = k.noun + " file deleted"
			e.Color = colorGrey
			e.Add("Reason", p.Str("deleteReason"), true)
			e.Add("File", firstNonEmpty(file.Str("relativePath"), file.Str("path")), false)
		case "health":
			e.Title = k.label + " health issue"
			e.Color = colorRed
			if strings.EqualFold(p.Str("level"), "warning") {
				e.Color = colorAmber
			}
			e.Description = p.Str("message")
			e.Add("Level", p.Str("level"), true)
			e.Add("Check", p.Str("type"), true)
			e.URL = p.Str("wikiUrl")
		case "health_restored":
			e.Title = k.label + " health restored"
			e.Color = colorGreen
			e.Description = p.Str("message")
			e.Add("Check", p.Str("type"), true)
		case "app_update":
			e.Title = k.label + " updated"
			e.Color = colorBlue
			e.Description = p.Str("message")
			e.Add("Previous version", p.Str("previousVersion"), true)
			e.Add("New version", p.Str("newVersion"), true)
		case "manual_interaction":
			e.Title = k.label + " needs manual interaction"
			e.Color = colorRed
			e.Add("Download client", client, true)
			e.Add("Status", firstNonEmpty(p.Str("downloadStatus"), p.Str("downloadInfo", "status")), true)
			e.Add("Release", firstNonEmpty(p.Str("downloadInfo", "title"), rel.Str("releaseTitle")), false)
			e.Add("Details", starrStatusMessages(p.Arr("downloadStatusMessages")), false)
		case "failure":
			e.Title = k.label + " download or import failed"
			e.Color = colorRed
			e.Add("Quality", firstNonEmpty(p.Str("quality"), rel.Str("quality")), true)
			e.Add("Download client", client, true)
			e.Add("Release", firstNonEmpty(p.Str("releaseTitle"), rel.Str("releaseTitle")), false)
			e.Add("Message", p.Str("message"), false)
		default:
			e.Title = k.label + ": " + firstNonEmpty(p.Str("eventType"), "event")
			e.Color = colorGrey
			e.Add("Message", p.Str("message"), false)
		}
		return event, e, true
	}
}

func firstNum(vals ...float64) float64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// starrStatusMessages flattens [{title, messages[]}] into readable lines.
func starrStatusMessages(msgs []J) string {
	var lines []string
	for _, m := range msgs {
		if t := m.Str("title"); t != "" {
			lines = append(lines, t)
		}
		if arr, ok := m["messages"].([]any); ok {
			for _, x := range arr {
				if s := toStr(x); s != "" {
					lines = append(lines, "• "+s)
				}
			}
		}
	}
	return joinLimited(lines, 8)
}

// starrArtwork looks up the poster for the series, movie, artist or author
// in an event when the webhook itself carried no usable image. It needs the
// integration's URL and API key; without them it does nothing.
func starrArtwork(kind string) enrichFunc {
	object, resource, apiVer := "series", "series", "v3"
	switch kind {
	case "radarr":
		object, resource = "movie", "movie"
	case "lidarr":
		object, resource, apiVer = "artist", "artist", "v1"
	case "readarr":
		object, resource, apiVer = "author", "author", "v1"
	}
	return func(ctx context.Context, a *App, in *Integration, p J, e *Embed) {
		base := strings.TrimRight(strings.TrimSpace(in.Config["url"]), "/")
		key := strings.TrimSpace(in.Config["apikey"])
		id := int64(p.Num(object, "id"))
		if base == "" || key == "" || id <= 0 {
			return
		}
		var item J
		url := fmt.Sprintf("%s/api/%s/%s/%d", base, apiVer, resource, id)
		if err := getJSON(ctx, a.http, url, map[string]string{"X-Api-Key": key}, &item); err == nil {
			e.Thumb(starrPoster(item))
		}
	}
}
