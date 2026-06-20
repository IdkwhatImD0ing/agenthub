package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"time"

	"agenthub/internal/db"
	"agenthub/internal/server"
)

func main() {
	listenAddr := flag.String("listen", ":8080", "listen address")
	dataDir := flag.String("data", "./data", "data directory (SQLite DB + bare git repo)")
	adminKey := flag.String("admin-key", "", "admin API key (required, or set AGENTHUB_ADMIN_KEY)")
	maxBundleMB := flag.Int("max-bundle-mb", 50, "max bundle upload size in MB")
	maxPushesPerHour := flag.Int("max-pushes-per-hour", 100, "max git pushes per agent per hour")
	maxPostsPerHour := flag.Int("max-posts-per-hour", 100, "max posts per agent per hour")
	maxAgentsPerSession := flag.Int("max-agents-per-session", 0, "max agents per session (0 = unlimited)")
	noAuth := flag.Bool("no-auth", false, "local mode: skip admin-key checks and let the dashboard mutate freely; default listen becomes 127.0.0.1")
	flag.Parse()

	// Admin key from flag or env (not required in --no-auth mode).
	key := *adminKey
	if key == "" {
		key = os.Getenv("AGENTHUB_ADMIN_KEY")
	}
	if key == "" && !*noAuth {
		log.Fatal("--admin-key or AGENTHUB_ADMIN_KEY is required (or pass --no-auth for local-only mode)")
	}

	// Local mode binds to loopback unless the operator overrode the address.
	if *noAuth && *listenAddr == ":8080" {
		*listenAddr = "127.0.0.1:8080"
	}

	// Create data directory
	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	// Initialize database
	database, err := db.Open(filepath.Join(*dataDir, "agenthub.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()

	if err := database.Migrate(); err != nil {
		log.Fatalf("migrate database: %v", err)
	}

	// Per-project bare git repos are created lazily under
	// {dataDir}/projects/{slug}/repo.git, so there's no single repo to init here.

	// Start rate limit cleanup goroutine
	go func() {
		for {
			time.Sleep(30 * time.Minute)
			database.CleanupRateLimits()
		}
	}()

	// Start server
	srv := server.New(database, *dataDir, key, server.Config{
		MaxBundleSize:       int64(*maxBundleMB) * 1024 * 1024,
		MaxPushesPerHour:    *maxPushesPerHour,
		MaxPostsPerHour:     *maxPostsPerHour,
		MaxAgentsPerSession: *maxAgentsPerSession,
		NoAuth:              *noAuth,
		ListenAddr:          *listenAddr,
	})

	log.Fatal(srv.ListenAndServe())
}
