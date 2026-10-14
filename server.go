package main

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Incoming webhooks: authenticated by the per-integration token in the URL.
	mux.HandleFunc("POST /api/v1/hook/{token}", a.handleHook)
	mux.HandleFunc("PUT /api/v1/hook/{token}", a.handleHook)

	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("POST /api/setup", a.handleSetup)
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.auth(a.handleLogout))

	mux.HandleFunc("GET /api/types", a.auth(a.handleTypes))
	mux.HandleFunc("GET /api/dashboard", a.auth(a.handleDashboard))

	mux.HandleFunc("GET /api/channels", a.auth(a.handleChannelList))
	mux.HandleFunc("POST /api/channels", a.auth(a.handleChannelSave))
	mux.HandleFunc("PUT /api/channels/{id}", a.auth(a.handleChannelSave))
	mux.HandleFunc("DELETE /api/channels/{id}", a.auth(a.handleChannelDelete))
	mux.HandleFunc("POST /api/channels/{id}/test", a.auth(a.handleChannelTest))

	mux.HandleFunc("GET /api/integrations", a.auth(a.handleIntegrationList))
	mux.HandleFunc("POST /api/integrations", a.auth(a.handleIntegrationSave))
	mux.HandleFunc("PUT /api/integrations/{id}", a.auth(a.handleIntegrationSave))
	mux.HandleFunc("DELETE /api/integrations/{id}", a.auth(a.handleIntegrationDelete))
	mux.HandleFunc("POST /api/integrations/{id}/test", a.auth(a.handleIntegrationTest))
	mux.HandleFunc("POST /api/integrations/{id}/token", a.auth(a.handleIntegrationToken))
	mux.HandleFunc("GET /api/integrations/{id}/containers", a.auth(a.handleIntegrationContainers))
	mux.HandleFunc("POST /api/integrations/{id}/ignore", a.auth(a.handleIntegrationIgnore))

	mux.HandleFunc("GET /api/notifications", a.auth(a.handleNotificationList))
	mux.HandleFunc("DELETE /api/notifications", a.auth(a.handleNotificationClear))
	mux.HandleFunc("GET /api/notifications/{id}", a.auth(a.handleNotificationGet))
	mux.HandleFunc("POST /api/notifications/{id}/resend", a.auth(a.handleNotificationResend))

	mux.HandleFunc("GET /api/settings", a.auth(a.handleSettingsGet))
	mux.HandleFunc("PUT /api/settings", a.auth(a.handleSettingsPut))
	mux.HandleFunc("POST /api/settings/password", a.auth(a.handlePassword))
	mux.HandleFunc("GET /api/backup", a.auth(a.handleBackup))
	mux.HandleFunc("GET /api/container-updates", a.auth(a.handleContainerUpdates))
	mux.HandleFunc("POST /api/container-updates/check", a.auth(a.handleContainerUpdatesCheck))
	mux.HandleFunc("POST /api/container-updates/apply", a.auth(a.handleContainerUpdateApply))
	mux.HandleFunc("POST /api/overview/send", a.auth(a.handleOverviewSend))
	mux.HandleFunc("GET /api/update", a.auth(a.handleUpdateGet))
	mux.HandleFunc("POST /api/update/check", a.auth(a.handleUpdateCheck))

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	static := http.FileServerFS(sub)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		csp := "default-src 'self'; img-src 'self' https: data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'"
		if strings.HasPrefix(r.URL.Path, "/icons/") {
			// Icon SVGs carry their own <style> blocks; allow those and nothing else.
			csp = "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'"
		}
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})
	return mux
}

// handleHook receives a webhook from an external app, turns it into an embed
// and queues it. Once the token is valid it always answers 200 so the sending
// app does not disable its webhook; problems are recorded in the history.
func (a *App) handleHook(w http.ResponseWriter, r *http.Request) {
	in, err := a.integrationByToken(r.PathValue("token"))
	if err != nil {
		httpError(w, http.StatusNotFound, "unknown webhook token")
		return
	}
	def := typeDefs[in.Type]
	if def == nil || def.Parse == nil {
		httpError(w, http.StatusBadRequest, "this integration does not accept webhooks")
		return
	}

	const maxBody = 16 << 20 // Plex attaches a thumbnail to some events
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var raw, thumb []byte
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(maxBody); err != nil {
			httpError(w, http.StatusBadRequest, "invalid multipart body")
			return
		}
		raw = []byte(firstNonEmpty(r.FormValue("payload"), r.FormValue("data")))
		thumb = formImage(r, "thumb")
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
		if err := r.ParseForm(); err != nil {
			httpError(w, http.StatusBadRequest, "invalid form body")
			return
		}
		raw = []byte(firstNonEmpty(r.FormValue("payload"), r.FormValue("data")))
	default:
		raw, err = io.ReadAll(r.Body)
		if err != nil {
			httpError(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
	}

	if !in.Enabled {
		writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
		return
	}

	// Some apps send plain text for their test button; treat it as an empty
	// payload rather than rejecting it.
	p := J{}
	if json.Unmarshal(raw, &p) != nil || p == nil {
		p = J{}
	}
	if len(raw) > 256<<10 {
		raw = raw[:256<<10]
	}

	event, e, ok := def.Parse(in, p)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if name := imageFilename(thumb); name != "" && e.Thumbnail == nil {
		// Plex uploads the item's artwork with the event; pass it on to Discord.
		e.attach, e.attachName = thumb, name
	}
	if def.Enrich != nil && e.Thumbnail == nil && len(e.attach) == 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		def.Enrich(ctx, a, in, p, &e)
		cancel()
	}
	status := "ok"
	if _, err := a.Dispatch(in, event, e, raw); err != nil {
		status = "error: " + err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status, "event": event})
}

// formImage reads a small uploaded file from a multipart webhook.
func formImage(r *http.Request, field string) []byte {
	if r.MultipartForm == nil || len(r.MultipartForm.File[field]) == 0 {
		return nil
	}
	fh := r.MultipartForm.File[field][0]
	if fh.Size > 4<<20 {
		return nil
	}
	f, err := fh.Open()
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return nil
	}
	return data
}
