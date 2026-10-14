package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "notified.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &App{db: db, dataDir: dir, polls: map[int64]*pollStatus{}}
}

func upload(t *testing.T, a *App, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("backup", "backup.db")
	part.Write(data)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/restore", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.handleRestore(rec, req)
	return rec
}

func backupBytes(t *testing.T, a *App) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "b.db")
	if _, err := a.db.Exec(`VACUUM INTO ?`, path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func count(t *testing.T, a *App, q string) int {
	var n int
	if err := a.db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRestoreReplacesDataAndKeepsSessions(t *testing.T) {
	// The backup: an admin, one channel, two integrations.
	src := newTestApp(t)
	src.setSetting("admin_user", "admin")
	src.setSetting("admin_hash", "pbkdf2-sha256$1$x$y")
	src.setSetting("instance_name", "Restored")
	src.db.Exec(`INSERT INTO channels(name,webhook_url,created_at) VALUES('media','https://discord.com/api/webhooks/1/a',1)`)
	src.insertIntegration(&Integration{Type: "sonarr", Name: "Sonarr", Enabled: true, Token: "t1", Config: map[string]string{}, Events: map[string]EventRule{}})
	src.insertIntegration(&Integration{Type: "radarr", Name: "Radarr", Enabled: true, Token: "t2", Config: map[string]string{}, Events: map[string]EventRule{}})
	data := backupBytes(t, src)

	// The live instance: different data, and a signed-in session.
	dst := newTestApp(t)
	dst.setSetting("admin_hash", "current")
	dst.setSetting("instance_name", "Fresh")
	dst.insertIntegration(&Integration{Type: "plex", Name: "Plex", Enabled: true, Token: "t9", Config: map[string]string{}, Events: map[string]EventRule{}})
	dst.db.Exec(`INSERT INTO sessions(token,expires) VALUES('me', 9999999999)`)
	dst.polls[99] = &pollStatus{}

	rec := upload(t, dst, data)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if got := dst.setting("instance_name"); got != "Restored" {
		t.Errorf("settings not restored: %q", got)
	}
	if n := count(t, dst, `SELECT COUNT(*) FROM integrations`); n != 2 {
		t.Errorf("integrations = %d, want 2 (the backup's, not the old ones)", n)
	}
	if n := count(t, dst, `SELECT COUNT(*) FROM channels`); n != 1 {
		t.Errorf("channels = %d, want 1", n)
	}
	if n := count(t, dst, `SELECT COUNT(*) FROM sessions WHERE token='me'`); n != 1 {
		t.Errorf("the current session should survive a restore")
	}
	if len(dst.polls) != 0 {
		t.Errorf("pollers should restart after a restore")
	}
	matches, _ := filepath.Glob(filepath.Join(dst.dataDir, "notified-before-restore-*.db"))
	if len(matches) != 1 {
		t.Fatalf("expected a safety copy of the previous data, found %v", matches)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dst.dataDir, ".restore-*"))
	if len(leftovers) != 0 {
		t.Errorf("temporary upload left behind: %v", leftovers)
	}
	// The safety copy really holds the old data.
	old, err := openDB(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var name string
	old.QueryRow(`SELECT name FROM integrations`).Scan(&name)
	if name != "Plex" {
		t.Errorf("safety copy holds %q, want the previous Plex integration", name)
	}
}

func TestRestoreRefusesBadFiles(t *testing.T) {
	noAdmin := newTestApp(t)
	cases := map[string][]byte{
		"not a database":  []byte("hello, this is not sqlite"),
		"empty":           {},
		"no admin":        backupBytes(t, noAdmin),
		"html login page": []byte("<!doctype html><html><body>Sign in</body></html>"),
	}
	for name, data := range cases {
		a := newTestApp(t)
		a.setSetting("admin_hash", "current")
		a.setSetting("instance_name", "Keep me")
		rec := upload(t, a, data)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", name, rec.Code, rec.Body)
		}
		if a.setting("instance_name") != "Keep me" {
			t.Errorf("%s: live data changed by a refused restore", name)
		}
	}
	if !strings.Contains(upload(t, newTestApp(t), backupBytes(t, noAdmin)).Body.String(), "admin") {
		t.Errorf("a backup without an admin should say so")
	}
}
