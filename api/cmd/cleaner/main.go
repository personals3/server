// Cleaner — periodic garbage collection sidecar. Same image as the API
// server, just a different ENTRYPOINT. Runs the cleaner tick loop in a
// single goroutine on a tiny DB pool.
//
// See api/internal/cleaner/cleaner.go for the full design + invariants.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/personals3/api/internal/cleaner"
	"github.com/personals3/api/internal/config"
	"github.com/personals3/api/internal/db"
	"github.com/personals3/api/internal/sharding"
	"github.com/personals3/api/internal/storage"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	fs, err := storage.New(cfg.StorageRoot)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}

	// Cleaner reads its own env knobs; set STORAGE_ROOT for the NDJSON log path.
	_ = os.Setenv("STORAGE_ROOT", cfg.StorageRoot)

	// Run the shard-index backfill once at startup. Idempotent — on a system
	// already backfilled, this is a few cheap SELECTs and exits quickly.
	if err := sharding.Backfill(ctx, pool); err != nil {
		log.Printf("sharding backfill failed (non-fatal, will retry on next tick): %v", err)
	}

	// Disk-write watcher (Option B in docs/come-up-designs/cleaner.md V4
	// extension). Boots an inotify watcher that marks shards dirty whenever
	// anything appears on disk under /storage/buckets/, so bot-dropped files
	// are reaped within ~30 seconds instead of waiting for the 24h bloom scan.
	//
	// Opt-out by setting WATCH_DISK=0 (e.g. on networked storage where
	// inotify isn't available).
	if os.Getenv("WATCH_DISK") != "0" {
		watcher, err := cleaner.StartWatcher(ctx, pool, cfg.StorageRoot)
		if err != nil {
			log.Printf("watcher: failed to start (%v) — falling back to scheduled sweeps only", err)
		} else {
			go func() {
				<-ctx.Done()
				watcher.Stop()
			}()
		}
	}

	c, err := cleaner.New(cleaner.LoadConfig(), pool, fs)
	if err != nil {
		log.Fatalf("cleaner: %v", err)
	}

	go func() {
		<-ctx.Done()
		c.Stop()
	}()

	c.Run(ctx)
	log.Printf("cleaner exited")
}
