package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The daily overview is one Discord message summarising the previous 24
// hours: how many notifications went out, which apps were busiest, anything
// that failed, and what needs attention now.

// parseClock reads "HH:MM" (24-hour) into hour and minute.
func parseClock(s string) (hour, minute int, ok bool) {
	h, m, found := strings.Cut(strings.TrimSpace(s), ":")
	if !found {
		return 0, 0, false
	}
	hour, err1 := strconv.Atoi(h)
	minute, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

// overviewDue reports whether the overview should be sent now, and the moment
// it was scheduled for. It is due once per day, within an hour of the chosen
// time, so a restart shortly after midnight still sends it but starting the
// app in the afternoon does not produce a late one.
func overviewDue(now time.Time, clock, lastSent string) (bool, time.Time) {
	hour, minute, ok := parseClock(clock)
	if !ok {
		hour, minute = 0, 0
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if now.Before(at) || now.Sub(at) >= time.Hour {
		return false, at
	}
	return lastSent != at.Format("2006-01-02"), at
}

func (a *App) overviewLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if a.setting("overview_enabled") == "false" {
			continue
		}
		due, at := overviewDue(time.Now(), a.setting("overview_time"), a.setting("overview_last"))
		if !due {
			continue
		}
		// Mark it first: a delivery problem should not cause a resend every 30 seconds.
		a.setSetting("overview_last", at.Format("2006-01-02"))
		a.sendOverview(at)
	}
}

// overviewChannel returns the chosen channel, or the first one if none is set.
func (a *App) overviewChannel() int64 {
	if id, _ := strconv.ParseInt(a.setting("overview_channel_id"), 10, 64); id != 0 {
		if _, err := a.getChannel(id); err == nil {
			return id
		}
	}
	var first int64
	a.db.QueryRow(`SELECT id FROM channels ORDER BY id LIMIT 1`).Scan(&first)
	return first
}

type countPair struct {
	name string
	n    int
}

func topCounts(m map[string]int, limit int) []countPair {
	out := make([]countPair, 0, len(m))
	for k, v := range m {
		out = append(out, countPair{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].name < out[j].name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// buildOverview summarises the 24 hours ending at `until`.
func (a *App) buildOverview(until time.Time) (Embed, error) {
	from := until.Add(-24 * time.Hour)
	rows, err := a.db.Query(`SELECT integration_name, event, status, title FROM notifications
		WHERE created_at >= ? AND created_at < ? AND type != 'notified' AND event != 'test'`, from.Unix(), until.Unix())
	if err != nil {
		return Embed{}, err
	}
	var sent, failed, skipped int
	byApp := map[string]int{}
	byEvent := map[string]int{}
	var failures []string
	for rows.Next() {
		var name, event, status, title string
		if rows.Scan(&name, &event, &status, &title) != nil {
			continue
		}
		switch status {
		case "sent":
			sent++
			byApp[name]++
			byEvent[strings.ReplaceAll(event, "_", " ")]++
		case "failed":
			failed++
			if len(failures) < 5 {
				failures = append(failures, "• "+name+": "+firstNonEmpty(title, event))
			}
		case "skipped":
			skipped++
		}
	}
	rows.Close()

	name := a.setting("instance_name")
	e := Embed{
		Title:  "Daily overview · " + from.Format("Mon 2 Jan"),
		Color:  colorBlurple,
		Footer: &EmbedFooter{Text: name},
	}
	if sent+failed == 0 {
		e.Description = "A quiet day: no notifications were sent."
	} else {
		e.Description = fmt.Sprintf("**%d** notifications sent", sent)
		if failed > 0 {
			e.Description += fmt.Sprintf(", **%d** failed", failed)
		}
		e.Description += "."
	}
	var lines []string
	for _, c := range topCounts(byApp, 8) {
		lines = append(lines, fmt.Sprintf("%s: %d", c.name, c.n))
	}
	e.Add("By integration", strings.Join(lines, "\n"), true)
	lines = nil
	for _, c := range topCounts(byEvent, 8) {
		lines = append(lines, fmt.Sprintf("%s: %d", c.name, c.n))
	}
	e.Add("By event", strings.Join(lines, "\n"), true)
	if failed > len(failures) {
		failures = append(failures, fmt.Sprintf("…and %d more", failed-len(failures)))
	}
	e.Add("Failed to deliver", strings.Join(failures, "\n"), false)
	if skipped > 0 {
		e.Add("Skipped (event switched off)", strconv.Itoa(skipped), true)
	}

	// What needs attention right now.
	if groups, err := a.containerUpdates(); err == nil {
		var pending []string
		for _, g := range groups {
			for _, it := range g.Items {
				pending = append(pending, it.Name)
			}
		}
		if len(pending) > 0 {
			e.Add(fmt.Sprintf("Container updates waiting (%d)", len(pending)), joinLimited(pending, 10), false)
			if base := a.setting("base_url"); base != "" {
				e.URL = base + "/#updates"
			}
		}
	}
	if ins, err := a.listIntegrations(); err == nil {
		var problems []string
		a.pollMu.Lock()
		for _, in := range ins {
			if st := a.polls[in.ID]; in.Enabled && st != nil && st.lastErr != "" {
				problems = append(problems, "• "+in.Name+": "+trunc(st.lastErr, 120))
			}
		}
		a.pollMu.Unlock()
		e.Add("Integrations with problems", joinLimited(problems, 6), false)
	}
	return e, nil
}

// sendOverview builds and queues the overview for the day ending at `until`.
func (a *App) sendOverview(until time.Time) error {
	ch := a.overviewChannel()
	if ch == 0 {
		return errors.New("add a Discord channel first")
	}
	e, err := a.buildOverview(until)
	if err != nil {
		return err
	}
	_, err = a.deliver(0, a.setting("instance_name"), "notified", "overview", ch, e, nil)
	return err
}

// handleOverviewSend sends an overview of the last 24 hours immediately.
func (a *App) handleOverviewSend(w http.ResponseWriter, r *http.Request) {
	if err := a.sendOverview(time.Now()); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
