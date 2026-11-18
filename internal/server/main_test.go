package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aviandri/tautan-be/internal/config"
	"github.com/aviandri/tautan-be/internal/testutil"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	pool, _, stop, err := testutil.StartPostgres()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testPool = pool
	code := m.Run()
	stop()
	os.Exit(code)
}

func testConfig() config.Config {
	c := config.Load()
	c.JWTSecret = "test-secret"
	c.CORSOrigins = []string{"http://localhost:3000"}
	c.PaymentsEncryptionKey = "" // payment tests opt in with newPaymentsServer
	return c
}

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	testutil.Truncate(t, testPool)
	s := New(testConfig(), testPool)
	s.Quiet = true
	return s, s.Routes()
}

func TestHealth(t *testing.T) {
	_, h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/health", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestCORSPreflight(t *testing.T) {
	_, h := newTestServer(t)
	req := httptest.NewRequest("OPTIONS", "/api/v1/public/alice/events", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("allow-origin = %q", got)
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatal("credentials not allowed")
	}
}
