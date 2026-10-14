package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// restoreTables are copied from a backup. Sessions are left alone so the
// person restoring stays signed in.
var restoreTables = []string{"settings", "channels", "integrations", "notifications", "state"}

const maxBackupSize = 512 << 20

// validateBackup opens an uploaded file read-only and checks it is a healthy
// Notified database with an admin account.
func validateBackup(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return errors.New("the file could not be opened as a database")
	}
	defer db.Close()
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return errors.New("this is not a Notified backup (not a SQLite database)")
	}
	if check != "ok" {
		return fmt.Errorf("the backup file is damaged: %s", trunc(check, 200))
	}
	for _, t := range restoreTables {
		var n int
		db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, t).Scan(&n)
		if n == 0 {
			return fmt.Errorf("this is not a Notified backup (no %s table)", t)
		}
	}
	var hash string
	db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='admin_hash'`).Scan(&hash)
	if hash == "" {
		return errors.New("the backup has no admin account, so restoring it would lock everyone out")
	}
	return nil
}

func tableColumns(ctx context.Context, conn *sql.Conn, schema, table string) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(`PRAGMA %s.table_info(%s)`, schema, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// restoreFrom replaces the live data with the backup's, in one transaction,
// so a failure part-way leaves the current data untouched.
func (a *App) restoreFrom(ctx context.Context, path string) error {
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS src`, path); err != nil {
		return fmt.Errorf("could not open the backup: %w", err)
	}
	defer conn.ExecContext(context.Background(), `DETACH DATABASE src`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range restoreTables {
		mainCols, err := tableColumns(ctx, conn, "main", t)
		if err != nil {
			return err
		}
		srcCols, err := tableColumns(ctx, conn, "src", t)
		if err != nil {
			return err
		}
		// Copy only columns both sides have, so a backup from an older or newer
		// version still restores what it can.
		var cols []string
		for c := range mainCols {
			if srcCols[c] {
				cols = append(cols, `"`+c+`"`)
			}
		}
		if len(cols) == 0 {
			continue
		}
		list := strings.Join(cols, ",")
		if _, err := tx.ExecContext(ctx, `DELETE FROM main.`+t); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO main.`+t+` (`+list+`) SELECT `+list+` FROM src.`+t); err != nil {
			return fmt.Errorf("could not restore %s: %w", t, err)
		}
	}
	return tx.Commit()
}

// handleRestore replaces all settings, channels, integrations and history
// with an uploaded backup. The current data is saved next to the database
// first, so a mistaken restore can itself be undone.
func (a *App) handleRestore(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBackupSize+1<<20)
	file, _, err := r.FormFile("backup")
	if err != nil {
		httpError(w, http.StatusBadRequest, "choose a backup file to upload (at most 512 MB)")
		return
	}
	defer file.Close()

	tmp := filepath.Join(a.dataDir, fmt.Sprintf(".restore-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not store the upload")
		return
	}
	_, err = io.Copy(out, file)
	out.Close()
	if err != nil {
		httpError(w, http.StatusBadRequest, "the upload did not complete")
		return
	}
	if err := validateBackup(r.Context(), tmp); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	safety := filepath.Join(a.dataDir, "notified-before-restore-"+time.Now().Format("2006-01-02-150405")+".db")
	if _, err := a.db.ExecContext(r.Context(), `VACUUM INTO ?`, safety); err != nil {
		httpError(w, http.StatusInternalServerError, "could not save a copy of the current data first, so nothing was changed: "+err.Error())
		return
	}
	if err := a.restoreFrom(r.Context(), tmp); err != nil {
		httpError(w, http.StatusInternalServerError, "restore failed and nothing was changed: "+err.Error())
		return
	}
	// Restart every poller with the restored configuration.
	a.pollMu.Lock()
	clear(a.polls)
	a.pollMu.Unlock()
	log.Printf("restored data from an uploaded backup; the previous data was saved to %s", safety)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true", "previous": filepath.Base(safety)})
}
