// Command dev starts an embedded Postgres (data kept in ./.devdata) and runs
// the API against it. No Docker or system Postgres needed.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/aviandri/tautan-be/internal/config"
	"github.com/aviandri/tautan-be/internal/server"
)

func main() {
	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		port := 54329
		if v, err := strconv.Atoi(os.Getenv("DEV_PG_PORT")); err == nil {
			port = v
		}
		base, _ := filepath.Abs(".devdata")
		pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
			Port(uint32(port)).Username("tautan").Password("tautan").Database("tautan").
			DataPath(filepath.Join(base, "data")).
			RuntimePath(filepath.Join(base, "runtime")).
			BinariesPath(filepath.Join(base, "bin")).
			CachePath(filepath.Join(base, "cache")).
			Logger(os.Stderr))
		if err := pg.Start(); err != nil {
			log.Fatalf("start embedded postgres: %v", err)
		}
		defer func() {
			if err := pg.Stop(); err != nil {
				log.Printf("stop embedded postgres: %v", err)
			}
		}()
		cfg.DatabaseURL = fmt.Sprintf("postgres://tautan:tautan@localhost:%d/tautan?sslmode=disable", port)
	}
	if err := server.Run(cfg); err != nil {
		log.Printf("server: %v", err)
	}
}
