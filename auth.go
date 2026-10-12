package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "notified_session"
	sessionTTL    = 30 * 24 * time.Hour
	pbkdf2Iter    = 210000
)

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

func (a *App) setupRequired() bool {
	return a.setting("admin_hash") == ""
}

func (a *App) resetAdmin() {
	a.db.Exec(`DELETE FROM settings WHERE key IN ('admin_user','admin_hash')`)
	a.db.Exec(`DELETE FROM sessions`)
}

func (a *App) startSession(w http.ResponseWriter, r *http.Request) error {
	token := randomToken(32)
	exp := time.Now().Add(sessionTTL)
	if _, err := a.db.Exec(`INSERT INTO sessions(token,expires) VALUES(?,?)`, hashToken(token), exp.Unix()); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	})
	return nil
}

func (a *App) authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	var exp int64
	if err := a.db.QueryRow(`SELECT expires FROM sessions WHERE token=?`, hashToken(c.Value)).Scan(&exp); err != nil {
		return false
	}
	return exp > time.Now().Unix()
}

// csrfOK requires a custom header on state-changing requests. Browsers will
// not attach it to cross-site form posts, and the cookie is SameSite=Strict.
func csrfOK(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	return r.Header.Get("X-Notified") != ""
}

func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !csrfOK(r) {
			httpError(w, http.StatusForbidden, "missing X-Notified header")
			return
		}
		if !a.authenticated(r) {
			httpError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		next(w, r)
	}
}

// ---- login throttling ----

type loginFail struct {
	count int
	reset time.Time
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *App) loginBlocked(ip string) bool {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	f := a.loginFails[ip]
	if f == nil {
		return false
	}
	if time.Now().After(f.reset) {
		delete(a.loginFails, ip)
		return false
	}
	return f.count >= 10
}

func (a *App) loginFailed(ip string) {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	f := a.loginFails[ip]
	if f == nil || time.Now().After(f.reset) {
		f = &loginFail{reset: time.Now().Add(10 * time.Minute)}
		a.loginFails[ip] = f
	}
	f.count++
}

// ---- handlers ----

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"setup_required": a.setupRequired(),
		"authenticated":  a.authenticated(r),
		"version":        version,
		"instance_name":  a.setting("instance_name"),
	})
}

func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		httpError(w, http.StatusForbidden, "missing X-Notified header")
		return
	}
	if !a.setupRequired() {
		httpError(w, http.StatusConflict, "setup has already been completed")
		return
	}
	var c credentials
	if !readJSON(w, r, &c) {
		return
	}
	c.Username = strings.TrimSpace(c.Username)
	if c.Username == "" || len(c.Password) < 8 {
		httpError(w, http.StatusBadRequest, "username is required and the password must be at least 8 characters")
		return
	}
	hash, err := hashPassword(c.Password)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.setSetting("admin_user", c.Username)
	a.setSetting("admin_hash", hash)
	if err := a.startSession(w, r); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		httpError(w, http.StatusForbidden, "missing X-Notified header")
		return
	}
	ip := clientIP(r)
	if a.loginBlocked(ip) {
		httpError(w, http.StatusTooManyRequests, "too many failed attempts; try again in a few minutes")
		return
	}
	var c credentials
	if !readJSON(w, r, &c) {
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(c.Username)), []byte(a.setting("admin_user"))) == 1
	passOK := checkPassword(a.setting("admin_hash"), c.Password)
	if !userOK || !passOK {
		a.loginFailed(ip)
		httpError(w, http.StatusUnauthorized, "incorrect username or password")
		return
	}
	if err := a.startSession(w, r); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.db.Exec(`DELETE FROM sessions WHERE token=?`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handlePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !checkPassword(a.setting("admin_hash"), in.Current) {
		httpError(w, http.StatusBadRequest, "current password is incorrect")
		return
	}
	if len(in.New) < 8 {
		httpError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	hash, err := hashPassword(in.New)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.setSetting("admin_hash", hash)
	// Sign out every other session, then keep this browser signed in.
	a.db.Exec(`DELETE FROM sessions`)
	a.startSession(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
