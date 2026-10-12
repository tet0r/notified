package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// J is a loosely typed JSON object. Webhook payloads differ between app
// versions, so parsers read them defensively instead of using fixed structs.
type J map[string]any

func asMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case J:
		return m, true
	}
	return nil, false
}

func dig(v any, path ...string) any {
	for _, k := range path {
		m, ok := asMap(v)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

func (j J) Str(path ...string) string {
	return strings.TrimSpace(toStr(dig(map[string]any(j), path...)))
}

func (j J) Num(path ...string) float64 {
	switch t := dig(map[string]any(j), path...).(type) {
	case float64:
		return t
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	}
	return 0
}

func (j J) Bool(path ...string) bool {
	switch t := dig(map[string]any(j), path...).(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "true")
	case float64:
		return t != 0
	}
	return false
}

func (j J) Obj(path ...string) J {
	m, _ := asMap(dig(map[string]any(j), path...))
	return J(m)
}

func (j J) Arr(path ...string) []J {
	a, _ := dig(map[string]any(j), path...).([]any)
	out := make([]J, 0, len(a))
	for _, x := range a {
		if m, ok := asMap(x); ok {
			out = append(out, J(m))
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func humanBytes(b float64) string {
	if b <= 0 {
		return ""
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", b, units[i])
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

func cfgInt(cfg map[string]string, key string, def int) int {
	v := strings.TrimSpace(cfg[key])
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func cfgFloat(cfg map[string]string, key string, def float64) float64 {
	v := strings.TrimSpace(cfg[key])
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func cfgBool(cfg map[string]string, key string) bool {
	v := strings.ToLower(strings.TrimSpace(cfg[key]))
	return v == "true" || v == "1" || v == "on" || v == "yes"
}

// splitList splits on commas and newlines, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// getJSON performs a GET and decodes a JSON response.
func getJSON(ctx context.Context, c *http.Client, url string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		// Report the host only: the full URL can carry an API key.
		return fmt.Errorf("cannot reach %s: %s", req.URL.Host, unwrapNetErr(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("unexpected response from %s: %w", req.URL.Host, err)
	}
	return nil
}
