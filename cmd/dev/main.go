// Command dev starts an embedded Postgres (data kept in ./.devdata) and runs
// the API against it. No Docker or system Postgres needed.
package main

import (
	"log"

	"github.com/aviandri/tautan-be/internal/config"
	"github.com/aviandri/tautan-be/internal/devdb"
	"github.com/aviandri/tautan-be/internal/logx"
	"github.com/aviandri/tautan-be/internal/server"
)

func main() {
	cfg := config.Load()
	logx.Setup(cfg.LogFormat, cfg.LogLevel)
	if cfg.DatabaseURL == "" {
		pg, err := devdb.Start()
		if err != nil {
			log.Fatalf("start embedded postgres: %v", err)
		}
		defer func() {
			if err := pg.Stop(); err != nil {
				log.Printf("stop embedded postgres: %v", err)
			}
		}()
		cfg.DatabaseURL = devdb.URL()
	}
	if err := server.Run(cfg); err != nil {
		log.Printf("server: %v", err)
	}
}
