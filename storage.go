package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Everything Notified knows lives in one SQLite file. If the container's data
// directory is not a real, named place on the host, recreating the container
// (which every update does) silently starts again from nothing. These checks
// make that situation loud instead of silent.

var anonymousVolumeRE = regexp.MustCompile(`/volumes/[0-9a-f]{64}/_data$`)

// storageWarning inspects how the data directory is mounted and returns a
// warning for the admin, or "" if it looks durable. mountinfo is the content
// of /proc/self/mountinfo.
func storageWarning(dataDir string, mountinfo io.Reader) string {
	dataDir = filepath.ToSlash(filepath.Clean(dataDir))
	found := false
	root, fstype := "", ""
	sc := bufio.NewScanner(mountinfo)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// Fields: id parent major:minor root mountpoint options ...
		f := strings.Fields(sc.Text())
		if len(f) >= 5 && f[4] == dataDir {
			found, root, fstype = true, f[3], "" // keep the last match: later mounts shadow earlier ones
			// The filesystem type is the first field after the "-" separator.
			for i := 5; i+1 < len(f); i++ {
				if f[i] == "-" {
					fstype = f[i+1]
					break
				}
			}
		}
	}
	switch {
	case !found:
		return fmt.Sprintf("Nothing is mounted at %s, so all settings live inside the container and will be lost the next time it is recreated or updated. Add a volume for %s (for example /srv/notified/data:%s) and download a backup first.", dataDir, dataDir, dataDir)
	case fstype == "tmpfs" || fstype == "ramfs":
		// Seen with Docker Desktop: a host path such as /data/compose/NN/data
		// (what "./data" becomes in a Portainer stack) lives in the VM's
		// memory-backed filesystem and is emptied whenever Docker restarts.
		return fmt.Sprintf("%s is on a temporary in-memory filesystem, so all settings are erased whenever Docker or the machine restarts. Use a named Docker volume (notified_data:%s) instead, and download a backup first.", dataDir, dataDir)
	case anonymousVolumeRE.MatchString(root):
		return fmt.Sprintf("%s is an unnamed Docker volume. Updating with docker compose usually keeps it, but recreating the container any other way (Portainer, a new stack, a different folder) starts with empty settings. Mount a host folder at %s instead, and download a backup first.", dataDir, dataDir)
	}
	return ""
}

// checkStorage runs storageWarning against the live system. It only knows how
// to do this on Linux; elsewhere it reports nothing.
func checkStorage(dataDir string) string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	return storageWarning(dataDir, f)
}

// handleBackup sends a consistent copy of the whole database: every setting,
// channel, integration and the history. It contains secrets (API keys,
// Discord webhook URLs), so it is only available to the signed-in admin.
func (a *App) handleBackup(w http.ResponseWriter, r *http.Request) {
	tmp := filepath.Join(a.dataDir, fmt.Sprintf(".backup-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	// VACUUM INTO writes a complete, consistent snapshot even while the app is running.
	if _, err := a.db.ExecContext(r.Context(), `VACUUM INTO ?`, tmp); err != nil {
		httpError(w, http.StatusInternalServerError, "could not create the backup: "+err.Error())
		return
	}
	f, err := os.Open(tmp)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not read the backup")
		return
	}
	defer f.Close()
	name := "notified-backup-" + time.Now().Format("2006-01-02-1504") + ".db"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	io.Copy(w, f)
}
