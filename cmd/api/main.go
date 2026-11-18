// Command api runs the Tautan HTTP API against DATABASE_URL.
//
//	api              serve (migrates first unless MIGRATE_ON_START=false)
//	api migrate      apply pending migrations and exit (release step / init job)
//	api healthcheck  GET /livez on the local listener; exit 0 when healthy
//	                 (for Docker HEALTHCHECK in the distroless image, which has no curl)
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aviandri/tautan-be/internal/config"
	"github.com/aviandri/tautan-be/internal/db"
	"github.com/aviandri/tautan-be/internal/logx"
	"github.com/aviandri/tautan-be/internal/server"
)

func main() {
	cfg := config.Load()
	logx.Setup(cfg.LogFormat, cfg.LogLevel)
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "", "serve":
		err = server.Run(cfg)
	case "migrate":
		if err = cfg.Validate(); err == nil {
			err = db.Migrate(cfg.DatabaseURL)
		}
		if err == nil {
			slog.Info("migrations applied")
		}
	case "healthcheck":
		err = healthcheck(cfg.Addr)
	default:
		err = fmt.Errorf("unknown command %q (want serve, migrate or healthcheck)", cmd)
	}
	if err != nil {
		slog.Error("fatal", "cmd", cmd, "err", err)
		os.Exit(1)
	}
}

func healthcheck(addr string) error {
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	c := &http.Client{Timeout: 3 * time.Second}
	res, err := c.Get("http://" + host + "/livez")
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("livez: status %d", res.StatusCode)
	}
	return nil
}
