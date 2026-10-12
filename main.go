package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var version = "0.1.0"

type App struct {
	db    *sql.DB
	http  *http.Client
	sendQ chan sendJob

	pollMu sync.Mutex
	polls  map[int64]*pollStatus

	loginMu    sync.Mutex
	loginFails map[string]*loginFail
}

func main() {
	port := env("NOTIFIED_PORT", "8080")
	dataDir := env("NOTIFIED_DATA_DIR", "/data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	db, err := openDB(filepath.Join(dataDir, "notified.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	app := &App{
		db:         db,
		http:       &http.Client{Timeout: 30 * time.Second},
		sendQ:      make(chan sendJob, 1000),
		polls:      map[int64]*pollStatus{},
		loginFails: map[string]*loginFail{},
	}
	if os.Getenv("NOTIFIED_RESET_ADMIN") == "1" {
		app.resetAdmin()
		log.Print("admin account cleared; open the web UI to create a new one, then remove NOTIFIED_RESET_ADMIN")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go app.sender(ctx)
	go app.scheduler(ctx)
	go app.janitor(ctx)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	log.Printf("Notified %s listening on :%s (data: %s)", version, port, dataDir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// janitor expires sessions and trims the notification history.
func (a *App) janitor(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		a.db.Exec(`DELETE FROM sessions WHERE expires < ?`, time.Now().Unix())
		keep := cfgInt(map[string]string{"n": a.setting("retention")}, "n", 2000)
		if keep < 100 {
			keep = 100
		}
		a.db.Exec(`DELETE FROM notifications WHERE id <= (SELECT id FROM notifications ORDER BY id DESC LIMIT 1 OFFSET ?)`, keep)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
