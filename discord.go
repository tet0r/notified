package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	colorGreen   = 0x2ECC71
	colorRed     = 0xE74C3C
	colorAmber   = 0xF5A623
	colorBlue    = 0x3498DB
	colorTeal    = 0x1ABC9C
	colorGrey    = 0x95A5A6
	colorPurple  = 0x9B59B6
	colorBlurple = 0x5865F2
)

type EmbedAuthor struct {
	Name    string `json:"name,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
}

type EmbedImage struct {
	URL string `json:"url"`
}

type EmbedFooter struct {
	Text string `json:"text"`
}

type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// Embed is a Discord rich embed.
type Embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	URL         string       `json:"url,omitempty"`
	Color       int          `json:"color,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"`
	Author      *EmbedAuthor `json:"author,omitempty"`
	Thumbnail   *EmbedImage  `json:"thumbnail,omitempty"`
	Footer      *EmbedFooter `json:"footer,omitempty"`
	Fields      []EmbedField `json:"fields,omitempty"`

	// Not part of the Discord payload:
	// attach is a picture uploaded with the message and shown as the thumbnail.
	attach     []byte
	attachName string
	// fetchThumb is a picture to download and upload at send time, for
	// artwork on servers Discord cannot reach; fallbackThumb is used if that fails.
	fetchThumb    string
	fallbackThumb string
}

// Add appends a field, skipping empty values.
func (e *Embed) Add(name, value string, inline bool) {
	if value == "" {
		return
	}
	e.Fields = append(e.Fields, EmbedField{Name: name, Value: value, Inline: inline})
}

func (e *Embed) Thumb(url string) {
	if len(url) > 7 && (url[:7] == "http://" || url[:8] == "https://") {
		e.Thumbnail = &EmbedImage{URL: url}
	}
}

// clamp enforces Discord's embed size limits so a long payload is truncated
// instead of rejected.
func (e *Embed) clamp() {
	e.Title = trunc(e.Title, 256)
	e.Description = trunc(e.Description, 2048)
	if e.Author != nil {
		e.Author.Name = trunc(e.Author.Name, 256)
	}
	if len(e.Fields) > 25 {
		e.Fields = e.Fields[:25]
	}
	for i := range e.Fields {
		e.Fields[i].Name = trunc(e.Fields[i].Name, 256)
		e.Fields[i].Value = trunc(e.Fields[i].Value, 1024)
	}
}

type sendJob struct {
	logID      int64
	webhookURL string
	embed      Embed
}

// Dispatch applies the integration's event rules and queues the embed.
// It returns the history row id.
func (a *App) Dispatch(in *Integration, event string, e Embed, raw []byte) (int64, error) {
	rule := in.rule(event)
	if event == "test" {
		rule.Enabled = true
	}
	if !rule.Enabled {
		return a.insertNotification(&Notification{
			IntegrationID: in.ID, IntegrationName: in.Name, Type: in.Type, Event: event,
			Title: e.Title, Status: "skipped", Error: "event disabled", Payload: string(raw),
		})
	}
	chID := rule.ChannelID
	if chID != 0 {
		if _, err := a.getChannel(chID); err != nil {
			chID = 0
		}
	}
	if chID == 0 {
		chID = in.ChannelID
	}
	return a.deliver(in.ID, in.Name, in.Type, event, chID, e, raw)
}

// deliver records the notification and hands it to the sender.
func (a *App) deliver(integID int64, integName, typ, event string, chID int64, e Embed, raw []byte) (int64, error) {
	if e.Timestamp == "" {
		e.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if e.Footer == nil {
		e.Footer = &EmbedFooter{Text: a.setting("instance_name") + " • " + integName}
	}
	a.decorate(typ, integName, &e)
	e.clamp()
	embedJSON, _ := json.Marshal(e)
	n := &Notification{
		IntegrationID: integID, IntegrationName: integName, Type: typ, Event: event,
		Title: e.Title, Status: "queued", ChannelID: chID, Embed: string(embedJSON), Payload: string(raw),
	}
	ch, err := a.getChannel(chID)
	if err != nil {
		n.Status, n.Error = "failed", "no Discord channel is assigned to this integration"
		id, _ := a.insertNotification(n)
		return id, errors.New(n.Error)
	}
	id, err := a.insertNotification(n)
	if err != nil {
		return 0, err
	}
	select {
	case a.sendQ <- sendJob{logID: id, webhookURL: ch.WebhookURL, embed: e}:
	default:
		a.setNotificationStatus(id, "failed", "send queue is full")
		return id, errors.New("send queue is full")
	}
	return id, nil
}

// decorate gives every notification a picture: the app's icon beside the
// instance name, and as the thumbnail when the event has no artwork of its own.
func (a *App) decorate(typ, integName string, e *Embed) {
	icon := appIconURL(typ)
	if icon != "" {
		if e.Author == nil {
			e.Author = &EmbedAuthor{Name: integName}
		}
		if e.Author.IconURL == "" {
			e.Author.IconURL = icon
		}
	}
	// A thumbnail stored for an uploaded picture cannot be reused later (resend).
	if e.Thumbnail != nil && strings.HasPrefix(e.Thumbnail.URL, "attachment://") && len(e.attach) == 0 {
		e.Thumbnail = nil
	}
	switch {
	case len(e.attach) > 0:
		// The uploaded picture becomes the thumbnail when the message is sent.
	case e.Thumbnail == nil:
		e.Thumb(icon)
	case typ == "jellyfin" || typ == "emby":
		// Artwork lives on the media server, usually on the local network:
		// download it ourselves and upload it rather than hand Discord the URL.
		e.fetchThumb, e.fallbackThumb = e.Thumbnail.URL, icon
	}
}

func (a *App) sender(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-a.sendQ:
			if err := a.postDiscord(ctx, job.webhookURL, job.embed); err != nil {
				a.setNotificationStatus(job.logID, "failed", trunc(err.Error(), 500))
			} else {
				a.setNotificationStatus(job.logID, "sent", "")
			}
		}
	}
}

// postDiscord sends one embed, retrying on rate limits and server errors.
func (a *App) postDiscord(ctx context.Context, webhookURL string, e Embed) error {
	if e.fetchThumb != "" && len(e.attach) == 0 {
		if data, name, err := a.fetchImage(ctx, e.fetchThumb); err == nil {
			e.attach, e.attachName = data, name
		} else {
			e.Thumbnail = nil
			e.Thumb(e.fallbackThumb)
		}
	}
	payload := map[string]any{
		"username":   firstNonEmpty(a.setting("bot_name"), "Notified"),
		"avatar_url": a.setting("avatar_url"),
		// Text arriving in webhooks must never be able to ping anyone.
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	var body []byte
	contentType := "application/json"
	if len(e.attach) > 0 {
		name := firstNonEmpty(e.attachName, "thumb.jpg")
		e.Thumbnail = &EmbedImage{URL: "attachment://" + name}
		payload["embeds"] = []Embed{e}
		meta, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("payload_json", string(meta))
		part, err := mw.CreateFormFile("files[0]", name)
		if err != nil {
			return err
		}
		part.Write(e.attach)
		mw.Close()
		body, contentType = buf.Bytes(), mw.FormDataContentType()
	} else {
		payload["embeds"] = []Embed{e}
		var err error
		if body, err = json.Marshal(payload); err != nil {
			return err
		}
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		wait, err := a.postOnce(ctx, webhookURL, contentType, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if wait < 0 {
			return err // not retryable
		}
		if wait == 0 {
			wait = time.Duration(attempt+1) * 2 * time.Second
		}
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return lastErr
}

// postOnce returns a retry delay with the error: negative means give up,
// zero means use the default backoff.
func (a *App) postOnce(ctx context.Context, webhookURL, contentType string, body []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return -1, errors.New("invalid webhook URL")
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := a.http.Do(req)
	if err != nil {
		// Avoid echoing the webhook URL (a secret) into the history.
		return 0, errors.New("could not reach Discord: " + unwrapNetErr(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return 0, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		var rl struct {
			RetryAfter float64 `json:"retry_after"`
		}
		json.Unmarshal(data, &rl)
		if rl.RetryAfter == 0 {
			rl.RetryAfter, _ = strconv.ParseFloat(resp.Header.Get("Retry-After"), 64)
		}
		return time.Duration(rl.RetryAfter*1000)*time.Millisecond + 250*time.Millisecond, errors.New("rate limited by Discord")
	case resp.StatusCode >= 500:
		return 0, fmt.Errorf("Discord returned HTTP %d", resp.StatusCode)
	}
	return -1, fmt.Errorf("Discord rejected the message (HTTP %d): %s", resp.StatusCode, trunc(string(data), 300))
}

func unwrapNetErr(err error) string {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err.Error()
		}
		err = next
	}
}
