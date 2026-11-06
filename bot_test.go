package main

import (
	"strings"
	"testing"
)

func TestUpdateActions(t *testing.T) {
	it := pendingUpdate{EnvID: 3, Name: "sonarr", ContainerID: strings.Repeat("a", 64)}
	acts := updateActions(12, it)
	if len(acts) != 2 || acts[0].ID != "u|12|3|"+strings.Repeat("a", 64) || acts[1].ID != "i|12|sonarr" {
		t.Fatalf("got %+v", acts)
	}
	for _, a := range acts {
		if len(a.ID) > 100 {
			t.Errorf("button id longer than Discord allows: %d", len(a.ID))
		}
	}
	// A name that can't be encoded safely gets no Ignore button.
	if acts := updateActions(12, pendingUpdate{Name: "weird|name", ContainerID: "abc"}); len(acts) != 1 || acts[0].Label != "Update" {
		t.Errorf("got %+v", acts)
	}
}

func TestBotAllowlist(t *testing.T) {
	a := newTestApp(t)
	a.setSetting("bot_allowed_users", "111111111111111111, 222222222222222222")
	if !a.botAllowed("222222222222222222") || a.botAllowed("333333333333333333") || a.botAllowed("") {
		t.Error("allowlist check is wrong")
	}
	a.setSetting("bot_allowed_users", "")
	if a.botAllowed("111111111111111111") {
		t.Error("an empty allowlist must allow no one")
	}
}
