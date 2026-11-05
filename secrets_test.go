package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withBox installs a secret box for one test and removes it afterwards.
func withBox(t *testing.T, current []byte, older ...[]byte) {
	t.Helper()
	b, err := newSecretBox(current, older)
	if err != nil {
		t.Fatal(err)
	}
	prev := box
	box = b
	t.Cleanup(func() { box = prev })
}

func key(b byte) []byte { return []byte(strings.Repeat(string(rune(b)), 32)) }

func rawConfig(t *testing.T, a *App, id int64) string {
	var cfg string
	if err := a.db.QueryRow(`SELECT config FROM integrations WHERE id=?`, id).Scan(&cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSecretsAreEncryptedAtRest(t *testing.T) {
	withBox(t, key('a'))
	a := newTestApp(t)
	in := &Integration{Type: "sonarr", Name: "Sonarr", Enabled: true, Token: "tok",
		Config: map[string]string{"url": "http://sonarr:8989", "apikey": "SUPERSECRETKEY"}, Events: map[string]EventRule{}}
	if err := a.insertIntegration(in); err != nil {
		t.Fatal(err)
	}
	raw := rawConfig(t, a, in.ID)
	if strings.Contains(raw, "SUPERSECRETKEY") || !strings.Contains(raw, secretPrefix) {
		t.Errorf("API key stored in plain text: %s", raw)
	}
	if !strings.Contains(raw, "http://sonarr:8989") {
		t.Errorf("non-secret settings should stay readable: %s", raw)
	}
	got, _ := a.getIntegration(in.ID)
	if got.Config["apikey"] != "SUPERSECRETKEY" {
		t.Errorf("decrypted key = %q", got.Config["apikey"])
	}
	// The caller's copy must not be replaced by ciphertext.
	if in.Config["apikey"] != "SUPERSECRETKEY" {
		t.Errorf("insert changed the caller's config: %q", in.Config["apikey"])
	}
}

func TestExistingPlainSecretsAreEncryptedOnStartup(t *testing.T) {
	a := newTestApp(t)
	// Written by an older version: no encryption.
	a.db.Exec(`INSERT INTO integrations(type,name,enabled,token,channel_id,config,events,created_at)
		VALUES('sabnzbd','SAB',1,'t',0,'{"url":"http://sab","apikey":"PLAINKEY","interval":"300"}','{}',1)`)
	a.db.Exec(`INSERT INTO channels(name,webhook_url,created_at) VALUES('media','https://discord.com/api/webhooks/1/PLAINHOOK',1)`)

	withBox(t, key('b'))
	n, err := a.encryptExisting()
	if err != nil || n != 2 {
		t.Fatalf("encryptExisting = %d, %v; want 2 changes", n, err)
	}
	var url string
	a.db.QueryRow(`SELECT webhook_url FROM channels`).Scan(&url)
	if strings.Contains(rawConfig(t, a, 1), "PLAINKEY") || strings.Contains(url, "PLAINHOOK") {
		t.Errorf("secrets still in plain text after migration")
	}
	in, _ := a.getIntegration(1)
	ch, _ := a.getChannel(1)
	if in.Config["apikey"] != "PLAINKEY" || ch.WebhookURL != "https://discord.com/api/webhooks/1/PLAINHOOK" {
		t.Errorf("values changed by migration: %q %q", in.Config["apikey"], ch.WebhookURL)
	}
	if n, _ := a.encryptExisting(); n != 0 {
		t.Errorf("a second run should change nothing, changed %d", n)
	}
}

func TestSwitchingToAnEnvironmentKeyMovesSecrets(t *testing.T) {
	a := newTestApp(t)
	withBox(t, key('f')) // the key file
	a.insertIntegration(&Integration{Type: "sonarr", Name: "S", Token: "t", Config: map[string]string{"apikey": "K1"}, Events: map[string]EventRule{}})
	a.db.Exec(`INSERT INTO channels(name,webhook_url,created_at) VALUES('c',?,1)`, sealSecret("https://discord.com/api/webhooks/1/H"))

	withBox(t, key('e'), key('f')) // env key now current, file key still readable
	if n, _ := a.encryptExisting(); n != 2 {
		t.Errorf("expected both secrets moved to the new key, moved %d", n)
	}
	withBox(t, key('e')) // the file key alone is no longer needed
	in, _ := a.getIntegration(1)
	ch, _ := a.getChannel(1)
	if in.Config["apikey"] != "K1" || ch.WebhookURL != "https://discord.com/api/webhooks/1/H" {
		t.Errorf("after moving keys: %q %q", in.Config["apikey"], ch.WebhookURL)
	}
}

func TestWrongKeyShowsAsNotSet(t *testing.T) {
	a := newTestApp(t)
	withBox(t, key('x'))
	a.insertIntegration(&Integration{Type: "sonarr", Name: "S", Token: "t", Config: map[string]string{"apikey": "K"}, Events: map[string]EventRule{}})
	withBox(t, key('y'))
	before := unreadableSecrets.Load()
	in, _ := a.getIntegration(1)
	if in.Config["apikey"] != "" || unreadableSecrets.Load() != before+1 {
		t.Errorf("unreadable secret should come back empty and be counted, got %q", in.Config["apikey"])
	}
}

func TestKeyFileIsCreatedOnceAndReused(t *testing.T) {
	t.Setenv("NOTIFIED_SECRET_KEY", "")
	dir := t.TempDir()
	k1, _, src, err := loadSecretKeys(dir)
	if err != nil || len(k1) != 32 || !strings.HasSuffix(src, secretKeyFile) {
		t.Fatalf("first load: %v %d %q", err, len(k1), src)
	}
	info, err := os.Stat(filepath.Join(dir, secretKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	_ = info
	k2, _, _, _ := loadSecretKeys(dir)
	if string(k1) != string(k2) {
		t.Errorf("key changed between starts")
	}
	t.Setenv("NOTIFIED_SECRET_KEY", "my passphrase")
	k3, older, src, _ := loadSecretKeys(dir)
	if string(k3) == string(k1) || len(older) != 1 || string(older[0]) != string(k1) || !strings.Contains(src, "NOTIFIED_SECRET_KEY") {
		t.Errorf("environment key should take over and keep the file key readable")
	}
}
