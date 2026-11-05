package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Stored secrets (API keys, passwords, access tokens and Discord webhook URLs)
// are encrypted with AES-256-GCM before they reach the database. The key comes
// from NOTIFIED_SECRET_KEY when set, otherwise from a key file Notified creates
// in its data folder. Notified has to decrypt the secrets to use them, so this
// keeps them out of the database file, not away from the running app.

const (
	secretPrefix  = "enc:v1:"
	secretKeyFile = "secret.key"
)

type secretBox struct {
	seal  cipher.AEAD   // encrypts with the current key
	opens []cipher.AEAD // tried in order when decrypting (current key first)
}

// box is the process-wide secret box. It is nil in tests that do not set one
// up, in which case values are stored as given.
var box *secretBox

// unreadableSecrets counts values that could not be decrypted (wrong or lost key).
var unreadableSecrets atomic.Int64

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// loadSecretKeys returns the key to encrypt with and any older key that may
// still be needed to read existing values, creating the key file on first run.
// Setting NOTIFIED_SECRET_KEY later is supported: the file key stays readable
// so existing secrets can be moved over to the new key.
func loadSecretKeys(dataDir string) (current []byte, older [][]byte, source string, err error) {
	path := filepath.Join(dataDir, secretKeyFile)
	var fileKey []byte
	if data, err := os.ReadFile(path); err == nil {
		fileKey, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(fileKey) != 32 {
			return nil, nil, "", fmt.Errorf("%s is not a valid key file", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, "", err
	}

	if env := os.Getenv("NOTIFIED_SECRET_KEY"); env != "" {
		k := sha256.Sum256([]byte(env))
		if fileKey != nil {
			older = append(older, fileKey)
		}
		return k[:], older, "the NOTIFIED_SECRET_KEY environment variable", nil
	}
	if fileKey != nil {
		return fileKey, nil, path, nil
	}
	fileKey = make([]byte, 32)
	if _, err := rand.Read(fileKey); err != nil {
		return nil, nil, "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, "", fmt.Errorf("could not create %s: %w", path, err)
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(fileKey) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, nil, "", werr
	}
	log.Printf("created a new secret key at %s", path)
	return fileKey, nil, path, nil
}

func newSecretBox(current []byte, older [][]byte) (*secretBox, error) {
	s, err := newAEAD(current)
	if err != nil {
		return nil, err
	}
	b := &secretBox{seal: s, opens: []cipher.AEAD{s}}
	for _, k := range older {
		o, err := newAEAD(k)
		if err != nil {
			return nil, err
		}
		b.opens = append(b.opens, o)
	}
	return b, nil
}

// sealSecret encrypts a value for storage. Empty and already-encrypted
// values are stored as they are.
func sealSecret(v string) string {
	if box == nil || v == "" || strings.HasPrefix(v, secretPrefix) {
		return v
	}
	nonce := make([]byte, box.seal.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	out := box.seal.Seal(nonce, nonce, []byte(v), nil)
	return secretPrefix + base64.RawStdEncoding.EncodeToString(out)
}

// openSecret decrypts a stored value. Values saved before encryption existed
// come back unchanged. A value that cannot be decrypted comes back empty, so
// it shows as "not set" and can be entered again.
func openSecret(v string) string {
	if !strings.HasPrefix(v, secretPrefix) {
		return v
	}
	plain, err := tryOpen(v)
	if err != nil {
		unreadableSecrets.Add(1)
		return ""
	}
	return plain
}

func tryOpen(v string) (string, error) {
	if box == nil {
		return "", errors.New("no secret key loaded")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(v, secretPrefix))
	if err != nil {
		return "", err
	}
	for _, a := range box.opens {
		n := a.NonceSize()
		if len(raw) < n {
			continue
		}
		if plain, err := a.Open(nil, raw[:n], raw[n:], nil); err == nil {
			return string(plain), nil
		}
	}
	return "", errors.New("no key decrypts this value")
}

// isSecretField reports whether an integration setting holds a secret.
func isSecretField(typ, key string) bool {
	if def := typeDefs[typ]; def != nil {
		for _, f := range def.Fields {
			if f.Key == key {
				return f.Kind == "password"
			}
		}
	}
	return false
}

// sealConfig returns a copy of an integration's settings with its secret
// fields encrypted, ready to store.
func sealConfig(typ string, cfg map[string]string) map[string]string {
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		if isSecretField(typ, k) {
			v = sealSecret(v)
		}
		out[k] = v
	}
	return out
}

// encryptExisting rewrites every stored secret with the current key. It
// encrypts values saved before encryption existed and moves values sealed
// with an older key onto the current one; values already on the current key
// are left alone.
func (a *App) encryptExisting() (int, error) {
	if box == nil {
		return 0, nil
	}
	changed := 0
	ins, err := a.listIntegrations()
	if err != nil {
		return 0, err
	}
	rows, err := a.db.Query(`SELECT id, config FROM integrations`)
	if err != nil {
		return 0, err
	}
	raw := map[int64]string{}
	for rows.Next() {
		var id int64
		var cfg string
		if rows.Scan(&id, &cfg) == nil {
			raw[id] = cfg
		}
	}
	rows.Close()
	for _, in := range ins {
		if needsResealing(in.Type, raw[in.ID]) {
			if err := a.updateIntegration(in); err != nil {
				return changed, err
			}
			changed++
		}
	}

	type ch struct {
		id  int64
		url string
	}
	var chans []ch
	rows, err = a.db.Query(`SELECT id, webhook_url FROM channels`)
	if err != nil {
		return changed, err
	}
	for rows.Next() {
		var c ch
		if rows.Scan(&c.id, &c.url) == nil {
			chans = append(chans, c)
		}
	}
	rows.Close()
	for _, c := range chans {
		if c.url == "" || (strings.HasPrefix(c.url, secretPrefix) && sealedWithCurrent(c.url)) {
			continue
		}
		plain := openSecret(c.url)
		if plain == "" {
			continue // unreadable: leave it for the admin to re-enter
		}
		if _, err := a.db.Exec(`UPDATE channels SET webhook_url=? WHERE id=?`, sealSecret(plain), c.id); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

// needsResealing reports whether any secret field in a stored config is plain
// text or sealed with an older key.
func needsResealing(typ, storedJSON string) bool {
	for _, f := range typeDefs[typ].Fields {
		if f.Kind != "password" {
			continue
		}
		key := `"` + f.Key + `":"`
		i := strings.Index(storedJSON, key)
		if i < 0 {
			continue
		}
		rest := storedJSON[i+len(key):]
		end := strings.Index(rest, `"`)
		if end <= 0 {
			continue // empty value
		}
		v := rest[:end]
		if !strings.HasPrefix(v, secretPrefix) || !sealedWithCurrent(v) {
			return true
		}
	}
	return false
}

func sealedWithCurrent(v string) bool {
	if box == nil {
		return true
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(v, secretPrefix))
	n := box.seal.NonceSize()
	if err != nil || len(raw) < n {
		return false
	}
	_, err = box.seal.Open(nil, raw[:n], raw[n:], nil)
	return err == nil
}

// initSecrets loads the key and encrypts anything stored in plain text.
func (a *App) initSecrets(dataDir string) error {
	current, older, source, err := loadSecretKeys(dataDir)
	if err != nil {
		return err
	}
	if box, err = newSecretBox(current, older); err != nil {
		return err
	}
	n, err := a.encryptExisting()
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("encrypted the stored secrets of %d integrations and channels (key from %s)", n, source)
	}
	if bad := unreadableSecrets.Load(); bad > 0 {
		a.secretWarn = fmt.Sprintf("%d saved secrets (API keys, passwords or webhook URLs) could not be decrypted with the current key, so they show as not set. If NOTIFIED_SECRET_KEY was changed or the %s file was lost, put the old one back, or re-enter those values.", bad, secretKeyFile)
		log.Printf("WARNING: %s", a.secretWarn)
	}
	return nil
}
