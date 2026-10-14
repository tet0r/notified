package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func (a *App) handleTypes(w http.ResponseWriter, r *http.Request) {
	out := make([]*TypeDef, 0, len(typeOrder))
	for _, t := range typeOrder {
		out = append(out, typeDefs[t])
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- channels ----

func (a *App) handleChannelList(w http.ResponseWriter, r *http.Request) {
	list, err := a.listChannels()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *App) handleChannelSave(w http.ResponseWriter, r *http.Request) {
	var c Channel
	if !readJSON(w, r, &c) {
		return
	}
	if err := validateChannel(&c); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.Method == http.MethodPost {
		c.CreatedAt = time.Now().Unix()
		res, err := a.db.Exec(`INSERT INTO channels(name,webhook_url,created_at) VALUES(?,?,?)`, c.Name, c.WebhookURL, c.CreatedAt)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		c.ID, _ = res.LastInsertId()
	} else {
		c.ID = pathID(r)
		res, err := a.db.Exec(`UPDATE channels SET name=?, webhook_url=? WHERE id=?`, c.Name, c.WebhookURL, c.ID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			httpError(w, http.StatusNotFound, "channel not found")
			return
		}
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *App) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if _, err := a.db.Exec(`DELETE FROM channels WHERE id=?`, id); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.db.Exec(`UPDATE integrations SET channel_id=0 WHERE channel_id=?`, id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleChannelTest posts synchronously so the UI can show Discord's answer.
func (a *App) handleChannelTest(w http.ResponseWriter, r *http.Request) {
	ch, err := a.getChannel(pathID(r))
	if err != nil {
		httpError(w, http.StatusNotFound, "channel not found")
		return
	}
	e := Embed{
		Title:       "Test notification",
		Description: "Channel **" + ch.Name + "** is connected to " + a.setting("instance_name") + ".",
		Color:       colorBlurple,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := a.postDiscord(r.Context(), ch.WebhookURL, e); err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- integrations ----

type integrationStatus struct {
	LastRun   int64  `json:"last_run"`
	LastError string `json:"last_error"`
}

type integrationOut struct {
	*Integration
	Status    *integrationStatus `json:"status,omitempty"`
	LastEvent int64              `json:"last_event"`
}

func (a *App) handleIntegrationList(w http.ResponseWriter, r *http.Request) {
	list, err := a.listIntegrations()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	last := map[int64]int64{}
	if rows, err := a.db.Query(`SELECT integration_id, MAX(created_at) FROM notifications GROUP BY integration_id`); err == nil {
		for rows.Next() {
			var id, ts int64
			if rows.Scan(&id, &ts) == nil {
				last[id] = ts
			}
		}
		rows.Close()
	}
	out := make([]integrationOut, 0, len(list))
	a.pollMu.Lock()
	for _, in := range list {
		o := integrationOut{Integration: in, LastEvent: last[in.ID]}
		if st := a.polls[in.ID]; st != nil && !st.lastRun.IsZero() {
			o.Status = &integrationStatus{LastRun: st.lastRun.Unix(), LastError: st.lastErr}
		}
		out = append(out, o)
	}
	a.pollMu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleIntegrationSave(w http.ResponseWriter, r *http.Request) {
	var body Integration
	if !readJSON(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}

	var in *Integration
	if r.Method == http.MethodPost {
		if typeDefs[body.Type] == nil {
			httpError(w, http.StatusBadRequest, "unknown integration type")
			return
		}
		in = &Integration{Type: body.Type, Token: randomToken(20)}
	} else {
		var err error
		if in, err = a.getIntegration(pathID(r)); err != nil {
			httpError(w, http.StatusNotFound, "integration not found")
			return
		}
	}
	def := typeDefs[in.Type]
	if def == nil {
		httpError(w, http.StatusBadRequest, "unknown integration type")
		return
	}

	in.Name = body.Name
	in.Enabled = body.Enabled
	in.ChannelID = body.ChannelID
	// Keep only fields the type defines, filling in defaults.
	in.Config = map[string]string{}
	for _, f := range def.Fields {
		v, ok := body.Config[f.Key]
		if !ok {
			v = f.Default
		}
		in.Config[f.Key] = strings.TrimSpace(v)
	}
	in.Events = map[string]EventRule{}
	for _, e := range def.Events {
		if rule, ok := body.Events[e.Key]; ok {
			in.Events[e.Key] = rule
		} else {
			in.Events[e.Key] = EventRule{Enabled: e.Default}
		}
	}

	var err error
	if in.ID == 0 {
		err = a.insertIntegration(in)
	} else {
		err = a.updateIntegration(in)
		// Let the next scheduler tick pick up new settings immediately.
		a.pollMu.Lock()
		delete(a.polls, in.ID)
		a.pollMu.Unlock()
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (a *App) handleIntegrationDelete(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if err := a.deleteIntegration(id); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.pollMu.Lock()
	delete(a.polls, id)
	a.pollMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleIntegrationToken(w http.ResponseWriter, r *http.Request) {
	in, err := a.getIntegration(pathID(r))
	if err != nil {
		httpError(w, http.StatusNotFound, "integration not found")
		return
	}
	in.Token = randomToken(20)
	if err := a.updateIntegration(in); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, in)
}

// handleIntegrationTest sends a test embed and, for polling integrations,
// runs one poll so connection problems show up straight away.
func (a *App) handleIntegrationTest(w http.ResponseWriter, r *http.Request) {
	in, err := a.getIntegration(pathID(r))
	if err != nil {
		httpError(w, http.StatusNotFound, "integration not found")
		return
	}
	def := typeDefs[in.Type]
	label := in.Type
	if def != nil {
		label = def.Label
	}
	e := Embed{
		Title:       "Test notification",
		Description: fmt.Sprintf("**%s** (%s) is set up in %s.", in.Name, label, a.setting("instance_name")),
		Color:       colorBlurple,
	}
	out := map[string]any{"ok": true}
	if _, err := a.Dispatch(in, "test", e, nil); err != nil {
		out["ok"] = false
		out["send_error"] = err.Error()
	}
	if def != nil && def.Poll != nil {
		if err := a.runPoll(r.Context(), in); err != nil {
			out["ok"] = false
			out["poll_error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- container ignore lists (Docker and Portainer integrations) ----

type containerLists struct {
	Detected []string `json:"detected"`
	Ignored  []string `json:"ignored"`
}

// containerLists returns the containers an integration has seen and the ones
// the user has chosen to ignore.
func (a *App) containerLists(in *Integration) containerLists {
	out := containerLists{Detected: []string{}, Ignored: splitList(in.Config["ignore"])}
	if out.Ignored == nil {
		out.Ignored = []string{}
	}
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out.Detected = append(out.Detected, name)
		}
	}
	switch in.Type {
	case "portainer", "dockerhost":
		for _, n := range a.portainerReport(in.ID).Names {
			add(n)
		}
	case "docker":
		var known map[string]containerState
		if v, ok := a.stateGet(in.ID, "containers"); ok {
			json.Unmarshal([]byte(v), &known)
		}
		for n := range known {
			add(n)
		}
	}
	sort.Slice(out.Detected, func(i, j int) bool {
		return strings.ToLower(out.Detected[i]) < strings.ToLower(out.Detected[j])
	})
	return out
}

func (a *App) ignoreCapable(w http.ResponseWriter, r *http.Request) *Integration {
	in, err := a.getIntegration(pathID(r))
	if err != nil || (!isUpdateType(in.Type) && in.Type != "docker") {
		httpError(w, http.StatusNotFound, "integration not found")
		return nil
	}
	return in
}

func (a *App) handleIntegrationContainers(w http.ResponseWriter, r *http.Request) {
	if in := a.ignoreCapable(w, r); in != nil {
		writeJSON(w, http.StatusOK, a.containerLists(in))
	}
}

// handleIntegrationIgnore adds or removes one name on the ignore list and
// saves it straight away.
func (a *App) handleIntegrationIgnore(w http.ResponseWriter, r *http.Request) {
	in := a.ignoreCapable(w, r)
	if in == nil {
		return
	}
	var body struct {
		Name   string `json:"name"`
		Ignore bool   `json:"ignore"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 255 || strings.ContainsAny(name, ",\r\n") {
		httpError(w, http.StatusBadRequest, "invalid container name")
		return
	}
	var list []string
	for _, n := range splitList(in.Config["ignore"]) {
		if n != name {
			list = append(list, n)
		}
	}
	if body.Ignore {
		list = append(list, name)
	}
	in.Config["ignore"] = strings.Join(list, ", ")
	if err := a.updateIntegration(in); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if body.Ignore && isUpdateType(in.Type) {
		// Take it off the pending list now rather than at the next check.
		report := a.portainerReport(in.ID)
		kept := report.Items[:0]
		for _, it := range report.Items {
			if it.Name != name {
				kept = append(kept, it)
			}
		}
		report.Items = kept
		a.savePortainerReport(in.ID, report)
	}
	writeJSON(w, http.StatusOK, a.containerLists(in))
}

// ---- notification history ----

func (a *App) handleNotificationList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	where, args := []string{"1=1"}, []any{}
	if v, err := strconv.ParseInt(q.Get("integration_id"), 10, 64); err == nil && v > 0 {
		where, args = append(where, "integration_id=?"), append(args, v)
	}
	if v := q.Get("status"); v != "" {
		where, args = append(where, "status=?"), append(args, v)
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		// The embed holds the message text (show names, containers, fields), so searching it finds those too.
		where, args = append(where, "(title LIKE ? OR event LIKE ? OR integration_name LIKE ? OR embed LIKE ?)"), append(args, "%"+v+"%", "%"+v+"%", "%"+v+"%", "%"+v+"%")
	}
	cond := strings.Join(where, " AND ")

	var total int
	a.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE `+cond, args...).Scan(&total)

	rows, err := a.db.Query(`SELECT id,created_at,integration_id,integration_name,type,event,title,status,error,channel_id,embed
		FROM notifications WHERE `+cond+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	items := []Notification{}
	for rows.Next() {
		var n Notification
		var embed string
		if err := rows.Scan(&n.ID, &n.CreatedAt, &n.IntegrationID, &n.IntegrationName, &n.Type, &n.Event, &n.Title, &n.Status, &n.Error, &n.ChannelID, &embed); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		n.Summary, n.Details = embedDigest(embed)
		items = append(items, n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "items": items})
}

// embedDigest turns a stored embed into two short plain-text lines for the
// history list: what it is about (the description) and the key facts (the
// first few short fields), e.g. "Severance — S02E03 · Who Is Alive?" and
// "Quality: WEBDL-2160p · Size: 7.0 GB · Indexer: NZBgeek".
func embedDigest(raw string) (summary, details string) {
	if raw == "" {
		return "", ""
	}
	var e Embed
	if json.Unmarshal([]byte(raw), &e) != nil {
		return "", ""
	}
	plain := strings.NewReplacer("**", "", "__", "", "`", "").Replace
	var lines []string
	for _, l := range strings.Split(plain(e.Description), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	summary = trunc(strings.Join(lines, " — "), 200)
	var facts []string
	for _, f := range e.Fields {
		v := strings.TrimSpace(plain(f.Value))
		// Long free-text fields (release names, summaries, errors) stay in the detail view.
		if v == "" || strings.Contains(v, "\n") || len([]rune(v)) > 60 {
			continue
		}
		facts = append(facts, f.Name+": "+v)
		if len(facts) == 4 {
			break
		}
	}
	return summary, strings.Join(facts, " · ")
}

func (a *App) getNotification(id int64) (*Notification, error) {
	var n Notification
	err := a.db.QueryRow(`SELECT id,created_at,integration_id,integration_name,type,event,title,status,error,channel_id,embed,payload
		FROM notifications WHERE id=?`, id).
		Scan(&n.ID, &n.CreatedAt, &n.IntegrationID, &n.IntegrationName, &n.Type, &n.Event, &n.Title, &n.Status, &n.Error, &n.ChannelID, &n.Embed, &n.Payload)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (a *App) handleNotificationGet(w http.ResponseWriter, r *http.Request) {
	n, err := a.getNotification(pathID(r))
	if err != nil {
		httpError(w, http.StatusNotFound, "notification not found")
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (a *App) handleNotificationResend(w http.ResponseWriter, r *http.Request) {
	n, err := a.getNotification(pathID(r))
	if err != nil {
		httpError(w, http.StatusNotFound, "notification not found")
		return
	}
	var e Embed
	if n.Embed == "" || json.Unmarshal([]byte(n.Embed), &e) != nil {
		httpError(w, http.StatusBadRequest, "this entry has no stored message to resend")
		return
	}
	e.Timestamp, e.Footer = "", nil
	chID := n.ChannelID
	if in, err := a.getIntegration(n.IntegrationID); err == nil && chID == 0 {
		chID = in.ChannelID
	}
	id, err := a.deliver(n.IntegrationID, n.IntegrationName, n.Type, n.Event, chID, e, []byte(n.Payload))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

func (a *App) handleNotificationClear(w http.ResponseWriter, r *http.Request) {
	if _, err := a.db.Exec(`DELETE FROM notifications WHERE status != 'queued'`); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- dashboard ----

func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	type day struct {
		Date    string `json:"date"`
		Sent    int    `json:"sent"`
		Failed  int    `json:"failed"`
		Skipped int    `json:"skipped"`
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	days := make([]day, 7)
	index := map[string]int{}
	for i := range days {
		d := today.AddDate(0, 0, i-6).Format("2006-01-02")
		days[i].Date = d
		index[d] = i
	}
	counts := map[string]int{}
	cutoff := now.Add(-24 * time.Hour).Unix()

	rows, err := a.db.Query(`SELECT created_at, status FROM notifications WHERE created_at >= ?`, today.AddDate(0, 0, -6).Unix())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for rows.Next() {
		var ts int64
		var status string
		if rows.Scan(&ts, &status) != nil {
			continue
		}
		if ts >= cutoff {
			counts[status]++
		}
		if i, ok := index[time.Unix(ts, 0).In(now.Location()).Format("2006-01-02")]; ok {
			switch status {
			case "sent":
				days[i].Sent++
			case "failed":
				days[i].Failed++
			case "skipped":
				days[i].Skipped++
			}
		}
	}
	rows.Close()

	var integrations, active, channels int
	a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(enabled),0) FROM integrations`).Scan(&integrations, &active)
	a.db.QueryRow(`SELECT COUNT(*) FROM channels`).Scan(&channels)

	writeJSON(w, http.StatusOK, map[string]any{
		"sent_24h":            counts["sent"],
		"failed_24h":          counts["failed"],
		"skipped_24h":         counts["skipped"],
		"integrations":        integrations,
		"integrations_active": active,
		"channels":            channels,
		"days":                days,
		"version":             version,
	})
}

// ---- settings ----

var editableSettings = []string{"instance_name", "bot_name", "avatar_url", "base_url", "retention", "update_check", "overview_enabled", "overview_time", "overview_channel_id"}

func (a *App) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{"admin_user": a.setting("admin_user")}
	for _, k := range editableSettings {
		out[k] = a.setting(k)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if !readJSON(w, r, &body) {
		return
	}
	for _, k := range editableSettings {
		v, ok := body[k]
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "instance_name", "bot_name":
			if v == "" {
				v = settingDefaults[k]
			}
			v = trunc(v, 80)
		case "base_url":
			v = strings.TrimRight(v, "/")
		case "retention":
			n, err := strconv.Atoi(v)
			if err != nil || n < 100 || n > 100000 {
				httpError(w, http.StatusBadRequest, "history size must be between 100 and 100000")
				return
			}
		case "update_check", "overview_enabled":
			v = strconv.FormatBool(v == "true")
		case "overview_channel_id":
			n, _ := strconv.ParseInt(v, 10, 64)
			v = strconv.FormatInt(n, 10)
		case "overview_time":
			h, m, ok := parseClock(v)
			if !ok {
				httpError(w, http.StatusBadRequest, "the overview time must look like 00:00 (24-hour)")
				return
			}
			v = fmt.Sprintf("%02d:%02d", h, m)
		}
		if err := a.setSetting(k, v); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	a.handleSettingsGet(w, r)
}
