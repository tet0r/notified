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

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

type App struct {
	db *sql.DB
	// dataDir holds the database; storageWarn is set at startup when that
	// directory would not survive the container being recreated.
	dataDir     string
	storageWarn string
	http        *http.Client
	sendQ       chan sendJob

	// iconCache remembers which container icon names exist on the CDN.
	iconCache sync.Map

	// applyMu serialises container updates so two clicks never overlap.
	applyMu sync.Mutex

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
	dbPath := filepath.Join(dataDir, "notified.db")
	_, statErr := os.Stat(dbPath)
	isNew := os.IsNotExist(statErr)
	db, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	if isNew {
		log.Printf("NOTICE: no database found at %s, so a new empty one was created. If you have used Notified before, it is looking at a different data folder than last time.", dbPath)
	}
	storageWarn := checkStorage(dataDir)
	if storageWarn != "" {
		log.Printf("WARNING: %s", storageWarn)
	}

	app := &App{
		db:          db,
		dataDir:     dataDir,
		storageWarn: storageWarn,
		http:        &http.Client{Timeout: 30 * time.Second},
		sendQ:       make(chan sendJob, 1000),
		polls:       map[int64]*pollStatus{},
		loginFails:  map[string]*loginFail{},
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
	go app.updateLoop(ctx)
	go app.overviewLoop(ctx)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Bounds slow uploads. There is deliberately no write timeout: a
		// container update can legitimately take many minutes to answer.
		ReadTimeout: 2 * time.Minute,
		IdleTimeout: 2 * time.Minute,
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
		a.pruneLoginFails()
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
