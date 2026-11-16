// Command seed creates demo data. `make seed-heavy` builds the perf test page.
//
// It uses DATABASE_URL, else the dev database from `make dev` (already running,
// or started here if not). Images are written to UPLOAD_DIR like the API does.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/aviandri/tautan-be/internal/config"
	"github.com/aviandri/tautan-be/internal/db"
	"github.com/aviandri/tautan-be/internal/devdb"
	"github.com/aviandri/tautan-be/internal/server"
)

func main() {
	full := flag.Bool("full", false, "seed heavy_all (every block type) instead of heavy")
	flag.Parse()
	cfg := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = devdb.URL()
		if pool, err := db.Connect(ctx, cfg.DatabaseURL); err == nil {
			pool.Close() // `make dev` is running
		} else {
			pg, err := devdb.Start()
			if err != nil {
				log.Fatalf("start embedded postgres: %v", err)
			}
			defer pg.Stop()
		}
	}
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	s := server.New(cfg, pool)
	s.Quiet = true
	start := time.Now()
	u, err := s.SeedHeavy(ctx, *full)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	fmt.Printf("seeded %q in %s (password %q)\n", u, time.Since(start).Round(time.Millisecond), server.HeavyPassword)
	fmt.Printf("page: %s/%s\n", cfg.AppBaseURL, u)
	fmt.Printf("api:  %s/api/v1/public/%s\n", cfg.PublicBaseURL, u)
}
