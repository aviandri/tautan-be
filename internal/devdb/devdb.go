// Package devdb starts the embedded development Postgres (data in ./.devdata).
package devdb

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// Port is DEV_PG_PORT or 54329.
func Port() int {
	if v, err := strconv.Atoi(os.Getenv("DEV_PG_PORT")); err == nil {
		return v
	}
	return 54329
}

// URL is the connection string for the dev database.
func URL() string {
	return fmt.Sprintf("postgres://tautan:tautan@localhost:%d/tautan?sslmode=disable", Port())
}

// Start launches the embedded Postgres; call Stop on the result when done.
func Start() (*embeddedpostgres.EmbeddedPostgres, error) {
	base, _ := filepath.Abs(".devdata")
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(Port())).Username("tautan").Password("tautan").Database("tautan").
		DataPath(filepath.Join(base, "data")).
		RuntimePath(filepath.Join(base, "runtime")).
		BinariesPath(filepath.Join(base, "bin")).
		CachePath(filepath.Join(base, "cache")).
		Logger(os.Stderr))
	return pg, pg.Start()
}
