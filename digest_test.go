package main

import (
	"encoding/json"
	"testing"
)

func TestEmbedDigest(t *testing.T) {
	e := Embed{
		Title:       "Episode grabbed",
		Description: "**Severance**\nS02E03 · Who Is Alive?",
		Fields: []EmbedField{
			{Name: "Quality", Value: "WEBDL-2160p", Inline: true},
			{Name: "Size", Value: "7.0 GB", Inline: true},
			{Name: "Indexer", Value: "NZBgeek", Inline: true},
			{Name: "Release group", Value: "GRP", Inline: true},
			{Name: "Download client", Value: "SABnzbd", Inline: true},
			{Name: "Release", Value: "Severance.S02E03.Who.Is.Alive.2160p.ATVP.WEB-DL.DDP5.1.Atmos.DV.HDR.H.265-GRP", Inline: false},
		},
	}
	raw, _ := json.Marshal(e)
	summary, details := embedDigest(string(raw))
	if summary != "Severance — S02E03 · Who Is Alive?" {
		t.Errorf("summary = %q", summary)
	}
	if details != "Quality: WEBDL-2160p · Size: 7.0 GB · Indexer: NZBgeek · Release group: GRP" {
		t.Errorf("details = %q", details)
	}
	if s, d := embedDigest(""); s != "" || d != "" {
		t.Errorf("empty embed gave %q %q", s, d)
	}
	if s, d := embedDigest("not json"); s != "" || d != "" {
		t.Errorf("bad embed gave %q %q", s, d)
	}
}
