// Package testutil provides a shared embedded Postgres for tests.
package testutil

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aviandri/tautan-be/internal/db"
)

// StartPostgres boots an embedded Postgres on a free port, migrates it and
// returns a pool plus a stop func. Call it once from TestMain.
// Set TEST_DATABASE_URL to use an existing database instead.
func StartPostgres() (*pgxpool.Pool, string, func(), error) {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return connect(url, func() {})
	}
	port, err := freePort()
	if err != nil {
		return nil, "", nil, err
	}
	cache, _ := filepath.Abs(filepath.Join(repoRoot(), ".devdata", "cache"))
	tmp, err := os.MkdirTemp("", "tautan-pg-*")
	if err != nil {
		return nil, "", nil, err
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(port)).Database("tautan_test").
		RuntimePath(filepath.Join(tmp, "rt")).
		DataPath(filepath.Join(tmp, "data")).
		BinariesPath(filepath.Join(tmp, "bin")).
		CachePath(cache).
		Logger(nil))
	if err := pg.Start(); err != nil {
		return nil, "", nil, fmt.Errorf("start embedded postgres: %w", err)
	}
	url := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/tautan_test?sslmode=disable", port)
	return connect(url, func() { _ = pg.Stop(); _ = os.RemoveAll(tmp) })
}

func connect(url string, stop func()) (*pgxpool.Pool, string, func(), error) {
	if err := db.Migrate(url); err != nil {
		stop()
		return nil, "", nil, err
	}
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		stop()
		return nil, "", nil, err
	}
	return pool, url, func() { pool.Close(); stop() }, nil
}

// Truncate empties all application tables between tests.
func Truncate(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`TRUNCATE users, refresh_tokens, profiles, tabs, blocks, events, contact_submissions, contacts, form_submissions, username_redirects, images, remote_cache, poll_votes, questions, giveaway_entries, giveaway_winners, user_identities, payment_providers, orders, payment_events, email_outbox, password_resets, email_verifications RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func repoRoot() string {
	dir, _ := os.Getwd()
	for d := dir; d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	return dir
}
