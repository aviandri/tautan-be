// Package server wires HTTP routes to handlers.
package server

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aviandri/tautan-be/internal/config"
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
	fetcher        *safeFetcher
	images         imageWorker
	reval          *revalidator
	remote         remoteWorker
	google         *oidcClient // nil when Google sign-in is not configured
	// InlineRemote resolves oEmbed/RSS content during the save request (tests, seeding).
	InlineRemote bool
	// InlineImages encodes image variants during the upload request (tests, seeding).
	InlineImages bool
	// Quiet disables per-request access logs (used by tests).
	Quiet    bool
	draining atomic.Bool
	proxy    proxyResolver
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
		fetcher:        newSafeFetcher(false),
		reval:          newRevalidator(),
		proxy:          newProxyResolver(cfg),
	}
	if cfg.GoogleEnabled() {
		s.google = newOIDCClient(cfg.GoogleIssuer, cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRedirectURL,
			func() time.Time { return s.now() })
	}
	return s
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

		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Get("/me", s.handleMe)
			r.Patch("/me/password", s.handleSetPassword)
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
