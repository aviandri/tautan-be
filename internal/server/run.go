package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/aviandri/tautan-be/internal/config"
	"github.com/aviandri/tautan-be/internal/db"
	"github.com/aviandri/tautan-be/internal/mail"
	"github.com/aviandri/tautan-be/internal/payments"
)

// Run serves HTTP until SIGINT/SIGTERM, migrating first when MIGRATE_ON_START
// is on.
//
// Shutdown order: /readyz starts returning 503, then after SHUTDOWN_DELAY the
// listener closes and in-flight requests get up to SHUTDOWN_TIMEOUT to finish.
// Pending revalidate webhooks are then sent and background workers stop.
// Images still being encoded stay ready=false and are re-queued on the next
// start (ResumeImages).
func Run(cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if _, err := config.ParseTrustedProxies(cfg.TrustedProxies); err != nil {
		return err
	}
	if _, err := payments.NewSealer(cfg.PaymentsEncryptionKey, cfg.PaymentsEncryptionKeysOld); err != nil {
		return err
	}
	if _, err := mail.New(MailConfig(cfg)); err != nil {
		return err
	}
	if cfg.Production() && (cfg.MailDriver == "" || cfg.MailDriver == "log") {
		slog.Warn("MAIL_DRIVER=log: emails (password resets) are logged, not sent; set MAIL_DRIVER=smtp")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.MigrateOnStart {
		if err := db.Migrate(cfg.DatabaseURL); err != nil {
			return err
		}
		slog.Info("migrations applied")
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	store, err := NewStore(cfg)
	if err != nil {
		return err
	}
	api := New(cfg, pool)
	api.UseStore(store)
	workers, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	go api.RunRemoteRefresher(workers)
	go api.RunMailer(workers)
	go func() {
		if err := api.ResumeImages(workers); err != nil {
			slog.Error("resume image variants", "err", err)
		}
	}()
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No Read/WriteTimeout: 25 MB file uploads and downloads on slow
		// mobile links can take minutes. The reverse proxy in front bounds them.
		IdleTimeout: 120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("tautan api listening", "addr", cfg.Addr, "env", cfg.Env)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	stop() // a second signal kills the process immediately
	slog.Info("shutting down", "delay", cfg.ShutdownDelay.String(), "timeout", cfg.ShutdownTimeout.String())
	api.SetDraining()
	time.Sleep(cfg.ShutdownDelay)
	shCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	err = srv.Shutdown(shCtx)
	stopWorkers()
	api.FlushPending()
	slog.Info("shutdown complete", "err", err)
	return err
}
