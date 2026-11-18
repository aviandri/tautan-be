// Package server wires HTTP routes to handlers.
package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aviandri/tautan-be/internal/config"
	"github.com/aviandri/tautan-be/internal/mail"
	"github.com/aviandri/tautan-be/internal/payments"
	"github.com/aviandri/tautan-be/internal/storage"
)

type Server struct {
	eventLimiter   *rateLimiter
	contactLimiter *rateLimiter
	formLimiter    *rateLimiter
	salts          saltCache
	unfurlLimiter  *rateLimiter
	engageLimiter  *rateLimiter
	askLimiter     *rateLimiter
	unlockLimiter  *rateLimiter
	// Payments: nil sealer = PAYMENTS_ENCRYPTION_KEY unset, payments off.
	sealer              *payments.Sealer
	providerTestLimiter *rateLimiter
	orderLimiter        *rateLimiter
	lookupLimiter       *rateLimiter
	// PaymentOptions configures provider adapters (tests point them at fakes).
	PaymentOptions payments.Options
	fetcher        *safeFetcher
	images         imageWorker
	reval          *revalidator
	remote         remoteWorker
	google         *oidcClient // nil when Google sign-in is not configured
	mailer         mail.Sender
	mailWake       chan struct{}
	resetLimiter   *rateLimiter
	// InlineRemote resolves oEmbed/RSS content during the save request (tests, seeding).
	InlineRemote bool
	// InlineImages encodes image variants during the upload request (tests, seeding).
	InlineImages bool
	// Quiet disables per-request access logs (used by tests).
	Quiet    bool
	draining atomic.Bool
	proxy    proxyResolver
	store    storage.Store // nil: local disk at cfg.UploadDir (see uploads())
	cfg      config.Config
	db       *pgxpool.Pool
	now      func() time.Time
}

func New(cfg config.Config, pool *pgxpool.Pool) *Server {
	s := &Server{
		cfg: cfg, db: pool, now: time.Now,
		eventLimiter:   newRateLimiter(120, time.Minute),
		contactLimiter: newRateLimiter(5, 10*time.Minute),
		formLimiter:    newRateLimiter(10, 10*time.Minute),
		unfurlLimiter:  newRateLimiter(30, time.Minute),
		engageLimiter:  newRateLimiter(30, 10*time.Minute),
		askLimiter:     newRateLimiter(5, 10*time.Minute),
		unlockLimiter:  newRateLimiter(10, 10*time.Minute),
		resetLimiter:   newRateLimiter(10, 15*time.Minute),
		mailWake:       make(chan struct{}, 1),
		fetcher:        newSafeFetcher(false),
		reval:          newRevalidator(),
		proxy:          newProxyResolver(cfg),

		providerTestLimiter: newRateLimiter(20, 10*time.Minute),
		orderLimiter:        newRateLimiter(10, 10*time.Minute),
		lookupLimiter:       newRateLimiter(10, 10*time.Minute),
	}
	sealer, err := payments.NewSealer(cfg.PaymentsEncryptionKey, cfg.PaymentsEncryptionKeysOld)
	if err != nil {
		// Run refuses to start with a bad key; tests and tools just run without payments.
		log.Printf("payments disabled: %v", err)
	}
	s.sealer = sealer
	// Run rejects a bad mail config first (MailConfig); fall back to the log driver here.
	if m, err := mail.New(MailConfig(cfg)); err == nil {
		s.mailer = m
	} else {
		s.mailer = mail.Log{}
	}
	if cfg.GoogleEnabled() {
		s.google = newOIDCClient(cfg.GoogleIssuer, cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRedirectURL,
			func() time.Time { return s.now() })
	}
	return s
}

// MailConfig maps the env config onto the mail package's.
func MailConfig(cfg config.Config) mail.Config {
	return mail.Config{
		Driver: cfg.MailDriver, From: cfg.MailFrom,
		Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword,
		ShowBodies: !cfg.Production(),
	}
}

// newProxyResolver builds the resolver; Run has already rejected bad
// TRUSTED_PROXIES entries, so an error here only drops the bad entries.
func newProxyResolver(cfg config.Config) proxyResolver {
	trusted, _ := config.ParseTrustedProxies(cfg.TrustedProxies)
	return proxyResolver{trusted: trusted, header: cfg.ClientIPHeader}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(s.proxy.middleware)
	if !s.Quiet {
		r.Use(s.accessLog)
	}
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   s.cfg.CORSOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           600,
	}))

	r.Get("/livez", s.handleLive)
	r.Get("/readyz", s.handleReady)
	r.Get("/healthz", s.handleHealth)
	r.Handle("/uploads/*", s.serveUploads())
	// Payments (b): provider notifications, outside /api/v1 (no CORS, no auth;
	// each provider's own signature check authenticates them).
	r.Post("/webhooks/{provider}/{profile_id}", s.handlePaymentWebhook)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.handleHealth)

		r.Post("/auth/signup", s.handleSignup)
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/refresh", s.handleRefresh)
		r.Post("/auth/logout", s.handleLogout)
		r.Get("/auth/username-available", s.handleUsernameAvailable)
		r.Get("/auth/providers", s.handleAuthProviders)
		r.Get("/auth/google/start", s.handleGoogleStart)
		r.Post("/auth/google/callback", s.handleGoogleCallback)
		r.Post("/auth/password/forgot", s.handleForgotPassword)
		r.Post("/auth/password/reset", s.handleResetPassword)
		r.Post("/auth/verify-email", s.handleVerifyEmail)

		r.Get("/public/{username}", s.handlePublicPage)
		r.Post("/public/{username}/events", s.handlePublicEvent)
		r.Post("/public/{username}/contact", s.handlePublicContact)
		r.Post("/public/{username}/form", s.handlePublicForm)
		r.Post("/public/{username}/subscribe", s.handlePublicSubscribe)
		r.Get("/public/{username}/vcard", s.handleVCard)
		r.Post("/public/{username}/blocks/{id}/unlock", s.handleUnlock)
		r.Post("/public/{username}/blocks/{id}/vote", s.handlePollVote)
		r.Post("/public/{username}/blocks/{id}/ask", s.handleAsk)
		r.Post("/public/{username}/blocks/{id}/lead", s.handleLead)
		r.Post("/public/{username}/blocks/{id}/enter", s.handleGiveawayEnter)
		r.Get("/public/{username}/download/{token}", s.handleLeadDownload)
		r.Post("/public/{username}/orders", s.handleCreateOrder)
		r.Post("/public/{username}/orders/lookup", s.handleOrderLookup)
		r.Get("/public/{username}/orders/download/{token}", s.handleOrderDownload)
		r.Get("/public/{username}/orders/{id}", s.handlePublicOrder)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Get("/me", s.handleMe)
			r.Patch("/me/password", s.handleSetPassword)
			r.Post("/me/verification-email", s.handleResendVerification)
			r.Patch("/me/notifications", s.handlePatchNotifications)
			r.Group(func(r chi.Router) {
				r.Use(s.withProfile)
				r.With(s.revalidatesPage).Patch("/profile", s.handlePatchProfile)
				r.Get("/profile/editor", s.handleEditor)
				r.Post("/unfurl", s.handleUnfurl)
				r.With(s.revalidatesPage).Post("/import", s.handleImport)
				r.Post("/uploads", s.handleUpload)
				r.Get("/contact-submissions", s.handleListContacts)
				r.Get("/contact-submissions.csv", s.handleContactSubmissionsCSV)
				r.Get("/forms", s.handleListForms)
				r.Get("/forms/{id}/submissions", s.handleFormSubmissions)
				r.Get("/forms/{id}/submissions.csv", s.handleFormSubmissionsCSV)
				r.Get("/audience", s.handleAudience)
				r.Get("/audience.csv", s.handleAudienceCSV)
				r.Get("/audience/{id}", s.handleContactDetail)
				r.Get("/analytics/summary", s.handleAnalyticsSummary)
				r.Get("/analytics/overview", s.handleAnalyticsOverview)
				r.Get("/analytics/breakdown", s.handleAnalyticsBreakdown)
				r.Get("/analytics/blocks", s.handleAnalyticsBlocks)
				r.Get("/analytics/blocks/{id}/series", s.handleAnalyticsBlockSeries)
				r.Get("/analytics/export.csv", s.handleAnalyticsExport)

				r.With(s.revalidatesPage).Post("/tabs", s.handleCreateTab)
				r.With(s.revalidatesPage).Put("/tabs/order", s.handleOrderTabs)
				r.With(s.revalidatesPage).Patch("/tabs/{id}", s.handlePatchTab)
				r.With(s.revalidatesPage).Delete("/tabs/{id}", s.handleDeleteTab)

				r.With(s.revalidatesPage).Post("/blocks", s.handleCreateBlock)
				r.With(s.revalidatesPage).Put("/blocks/order", s.handleOrderBlocks)
				r.Get("/blocks/deleted", s.handleListDeleted)
				r.With(s.revalidatesPage).Post("/blocks/{id}/restore", s.handleRestoreBlock)
				r.With(s.revalidatesPage).Patch("/blocks/{id}", s.handlePatchBlock)
				r.With(s.revalidatesPage).Delete("/blocks/{id}", s.handleDeleteBlock)

				r.Get("/polls/{id}/results", s.handlePollResults)
				r.Get("/questions", s.handleListQuestions)
				r.With(s.revalidatesPage).Patch("/questions/{id}", s.handlePatchQuestion)
				r.With(s.revalidatesPage).Delete("/questions/{id}", s.handleDeleteQuestion)
				r.Get("/orders", s.handleListOrders)
				r.Get("/orders.csv", s.handleOrdersCSV)
				r.Get("/orders/{id}", s.handleGetOrder)
				r.Post("/orders/{id}/refresh", s.handleRefreshOrder)
				r.Route("/payments/providers", func(r chi.Router) {
					r.Get("/", s.handleListProviders)
					r.Group(func(r chi.Router) {
						r.Use(s.requirePayments)
						// enabled providers are part of page reads (buy buttons)
						r.With(s.revalidatesPage).Put("/{provider}", s.handlePutProvider)
						r.Post("/{provider}/test", s.handleTestProvider)
						r.With(s.revalidatesPage).Delete("/{provider}", s.handleDeleteProvider)
					})
				})

				r.Get("/giveaways/{id}", s.handleGiveaway)
				r.With(s.revalidatesPage).Post("/giveaways/{id}/draw", s.handleGiveawayDraw)
			})
		})
	})
	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "db_unavailable", "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// UseStore sets the upload store (Run picks it from STORAGE).
func (s *Server) UseStore(st storage.Store) { s.store = st }

// uploads is the upload store: the configured one, else local disk at
// cfg.UploadDir (read at call time, so tests can point it at a temp dir).
func (s *Server) uploads() storage.Store {
	if s.store != nil {
		return s.store
	}
	return storage.Local{Dir: s.cfg.UploadDir}
}

// NewStore builds the store STORAGE selects.
func NewStore(cfg config.Config) (storage.Store, error) {
	switch cfg.Storage {
	case "", "local":
		return storage.Local{Dir: cfg.UploadDir}, nil
	case "s3", "r2":
		return storage.NewS3(storage.S3Config{
			Bucket: cfg.S3Bucket, Region: cfg.S3Region, Endpoint: cfg.S3Endpoint,
			AccessKeyID: cfg.S3AccessKeyID, SecretAccessKey: cfg.S3SecretAccessKey,
			Prefix: cfg.S3Prefix, PathStyle: cfg.S3PathStyle,
		})
	default:
		return nil, fmt.Errorf("STORAGE must be local or s3 (got %q)", cfg.Storage)
	}
}
