package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token   TEXT PRIMARY KEY,
	expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS channels (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL,
	webhook_url TEXT NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS integrations (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	type       TEXT NOT NULL,
	name       TEXT NOT NULL,
	enabled    INTEGER NOT NULL DEFAULT 1,
	token      TEXT NOT NULL UNIQUE,
	channel_id INTEGER NOT NULL DEFAULT 0,
	config     TEXT NOT NULL DEFAULT '{}',
	events     TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notifications (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at       INTEGER NOT NULL,
	integration_id   INTEGER NOT NULL,
	integration_name TEXT NOT NULL,
	type             TEXT NOT NULL,
	event            TEXT NOT NULL,
	title            TEXT NOT NULL,
	status           TEXT NOT NULL,
	error            TEXT NOT NULL DEFAULT '',
	channel_id       INTEGER NOT NULL DEFAULT 0,
	embed            TEXT NOT NULL DEFAULT '',
	payload          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_notifications_created ON notifications(created_at);
CREATE TABLE IF NOT EXISTS state (
	integration_id INTEGER NOT NULL,
	key            TEXT NOT NULL,
	value          TEXT NOT NULL,
	PRIMARY KEY (integration_id, key)
);
`

func openDB(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Anything still queued was lost when the previous process stopped.
	db.Exec(`UPDATE notifications SET status='failed', error='interrupted by restart' WHERE status='queued'`)
	return db, nil
}

// ---- settings ----

var settingDefaults = map[string]string{
	"instance_name": "Notified",
	"bot_name":      "Notified",
	"avatar_url":    "",
	"base_url":      "",
	"retention":     "2000",
}

func (a *App) setting(key string) string {
	var v string
	if err := a.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return settingDefaults[key]
	}
	return v
}

func (a *App) setSetting(key, value string) error {
	_, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---- channels ----

type Channel struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	WebhookURL string `json:"webhook_url"`
	CreatedAt  int64  `json:"created_at"`
}

func (a *App) listChannels() ([]Channel, error) {
	rows, err := a.db.Query(`SELECT id,name,webhook_url,created_at FROM channels ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.Name, &c.WebhookURL, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (a *App) getChannel(id int64) (*Channel, error) {
	var c Channel
	err := a.db.QueryRow(`SELECT id,name,webhook_url,created_at FROM channels WHERE id=?`, id).
		Scan(&c.ID, &c.Name, &c.WebhookURL, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func validateChannel(c *Channel) error {
	c.Name = strings.TrimSpace(c.Name)
	c.WebhookURL = strings.TrimSpace(c.WebhookURL)
	if c.Name == "" {
		return errors.New("name is required")
	}
	u, err := url.Parse(c.WebhookURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("webhook URL must be a valid http(s) URL")
	}
	return nil
}

// ---- integrations ----

type EventRule struct {
	Enabled   bool  `json:"enabled"`
	ChannelID int64 `json:"channel_id"`
}

type Integration struct {
	ID        int64                `json:"id"`
	Type      string               `json:"type"`
	Name      string               `json:"name"`
	Enabled   bool                 `json:"enabled"`
	Token     string               `json:"token"`
	ChannelID int64                `json:"channel_id"`
	Config    map[string]string    `json:"config"`
	Events    map[string]EventRule `json:"events"`
	CreatedAt int64                `json:"created_at"`
}

// rule returns the routing rule for an event, falling back to the type's
// default when the user has not configured it. Unknown events are enabled.
func (in *Integration) rule(event string) EventRule {
	if r, ok := in.Events[event]; ok {
		return r
	}
	if def := typeDefs[in.Type]; def != nil {
		for _, e := range def.Events {
			if e.Key == event {
				return EventRule{Enabled: e.Default}
			}
		}
	}
	return EventRule{Enabled: true}
}

const integrationCols = `id,type,name,enabled,token,channel_id,config,events,created_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanIntegration(r rowScanner) (*Integration, error) {
	var in Integration
	var cfg, ev string
	if err := r.Scan(&in.ID, &in.Type, &in.Name, &in.Enabled, &in.Token, &in.ChannelID, &cfg, &ev, &in.CreatedAt); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(cfg), &in.Config)
	json.Unmarshal([]byte(ev), &in.Events)
	if in.Config == nil {
		in.Config = map[string]string{}
	}
	if in.Events == nil {
		in.Events = map[string]EventRule{}
	}
	return &in, nil
}

func (a *App) listIntegrations() ([]*Integration, error) {
	rows, err := a.db.Query(`SELECT ` + integrationCols + ` FROM integrations ORDER BY type, name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Integration{}
	for rows.Next() {
		in, err := scanIntegration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (a *App) getIntegration(id int64) (*Integration, error) {
	return scanIntegration(a.db.QueryRow(`SELECT `+integrationCols+` FROM integrations WHERE id=?`, id))
}

func (a *App) integrationByToken(token string) (*Integration, error) {
	return scanIntegration(a.db.QueryRow(`SELECT `+integrationCols+` FROM integrations WHERE token=?`, token))
}

func (a *App) insertIntegration(in *Integration) error {
	cfg, _ := json.Marshal(in.Config)
	ev, _ := json.Marshal(in.Events)
	in.CreatedAt = time.Now().Unix()
	res, err := a.db.Exec(`INSERT INTO integrations(type,name,enabled,token,channel_id,config,events,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		in.Type, in.Name, in.Enabled, in.Token, in.ChannelID, string(cfg), string(ev), in.CreatedAt)
	if err != nil {
		return err
	}
	in.ID, err = res.LastInsertId()
	return err
}

func (a *App) updateIntegration(in *Integration) error {
	cfg, _ := json.Marshal(in.Config)
	ev, _ := json.Marshal(in.Events)
	_, err := a.db.Exec(`UPDATE integrations SET name=?,enabled=?,token=?,channel_id=?,config=?,events=? WHERE id=?`,
		in.Name, in.Enabled, in.Token, in.ChannelID, string(cfg), string(ev), in.ID)
	return err
}

func (a *App) deleteIntegration(id int64) error {
	if _, err := a.db.Exec(`DELETE FROM integrations WHERE id=?`, id); err != nil {
		return err
	}
	_, err := a.db.Exec(`DELETE FROM state WHERE integration_id=?`, id)
	return err
}

// ---- poller state ----

func (a *App) stateGet(id int64, key string) (string, bool) {
	var v string
	if err := a.db.QueryRow(`SELECT value FROM state WHERE integration_id=? AND key=?`, id, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

func (a *App) stateSet(id int64, key, value string) {
	a.db.Exec(`INSERT INTO state(integration_id,key,value) VALUES(?,?,?)
		ON CONFLICT(integration_id,key) DO UPDATE SET value=excluded.value`, id, key, value)
}

func (a *App) stateDel(id int64, key string) {
	a.db.Exec(`DELETE FROM state WHERE integration_id=? AND key=?`, id, key)
}

// statePrune removes keys with the given prefix that are no longer active,
// so a condition that clears and later returns is reported again.
func (a *App) statePrune(id int64, prefix string, active map[string]bool) {
	rows, err := a.db.Query(`SELECT key FROM state WHERE integration_id=?`, id)
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil && strings.HasPrefix(k, prefix) && !active[k] {
			stale = append(stale, k)
		}
	}
	rows.Close()
	for _, k := range stale {
		a.stateDel(id, k)
	}
}

// ---- notification history ----

type Notification struct {
	ID              int64  `json:"id"`
	CreatedAt       int64  `json:"created_at"`
	IntegrationID   int64  `json:"integration_id"`
	IntegrationName string `json:"integration_name"`
	Type            string `json:"type"`
	Event           string `json:"event"`
	Title           string `json:"title"`
	Status          string `json:"status"`
	Error           string `json:"error"`
	ChannelID       int64  `json:"channel_id"`
	Embed           string `json:"embed,omitempty"`
	Payload         string `json:"payload,omitempty"`
}

func (a *App) insertNotification(n *Notification) (int64, error) {
	n.CreatedAt = time.Now().Unix()
	res, err := a.db.Exec(`INSERT INTO notifications(created_at,integration_id,integration_name,type,event,title,status,error,channel_id,embed,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		n.CreatedAt, n.IntegrationID, n.IntegrationName, n.Type, n.Event, n.Title, n.Status, n.Error, n.ChannelID, n.Embed, n.Payload)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (a *App) setNotificationStatus(id int64, status, errMsg string) {
	a.db.Exec(`UPDATE notifications SET status=?, error=? WHERE id=?`, status, errMsg, id)
}
