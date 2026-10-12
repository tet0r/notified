package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

type pollStatus struct {
	lastRun time.Time
	lastErr string
	next    time.Time
	running bool
}

func pollInterval(in *Integration, def *TypeDef) time.Duration {
	secs := cfgInt(in.Config, "interval", def.DefaultInterval)
	if secs <= 0 {
		return 0
	}
	if secs < 10 {
		secs = 10
	}
	return time.Duration(secs) * time.Second
}

// scheduler wakes every few seconds and starts any poller that is due.
// Reading integrations fresh each tick means edits apply without a restart.
func (a *App) scheduler(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.pollDue(ctx)
		}
	}
}

func (a *App) pollDue(ctx context.Context) {
	ins, err := a.listIntegrations()
	if err != nil {
		log.Printf("scheduler: %v", err)
		return
	}
	now := time.Now()
	for _, in := range ins {
		def := typeDefs[in.Type]
		if def == nil || def.Poll == nil || !in.Enabled || pollInterval(in, def) == 0 {
			continue
		}
		a.pollMu.Lock()
		st := a.polls[in.ID]
		due := st == nil || (!st.running && !now.Before(st.next))
		a.pollMu.Unlock()
		if due {
			go a.runPoll(ctx, in)
		}
	}
}

// runPoll executes one poll unless one is already in flight for the
// integration, and records the outcome for the UI.
func (a *App) runPoll(ctx context.Context, in *Integration) error {
	def := typeDefs[in.Type]
	if def == nil || def.Poll == nil {
		return nil
	}
	a.pollMu.Lock()
	st := a.polls[in.ID]
	if st == nil {
		st = &pollStatus{}
		a.polls[in.ID] = st
	}
	if st.running {
		a.pollMu.Unlock()
		return nil
	}
	st.running = true
	a.pollMu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := safePoll(cctx, def.Poll, a, in)
	cancel()

	iv := pollInterval(in, def)
	if iv == 0 {
		iv = time.Minute
	}
	a.pollMu.Lock()
	st.running = false
	st.lastRun = time.Now()
	st.next = st.lastRun.Add(iv)
	st.lastErr = ""
	if err != nil {
		st.lastErr = err.Error()
	}
	a.pollMu.Unlock()
	return err
}

func safePoll(ctx context.Context, fn pollFunc, a *App, in *Integration) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("poller panicked: %v", r)
			log.Printf("poll %s (%s): %v", in.Name, in.Type, err)
		}
	}()
	return fn(ctx, a, in)
}
