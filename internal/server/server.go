// Package server wires HTTP routes to handlers.
package server

import (
	"context"
	"net/http"
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
	// Quiet disables per-request access logs (used by tests).
	Quiet bool
	cfg   config.Config
	db    *pgxpool.Pool
	now   func() time.Time
}

func New(cfg config.Config, pool *pgxpool.Pool) *Server {
	return &Server{
		cfg: cfg, db: pool, now: time.Now,
		eventLimiter:   newRateLimiter(120, time.Minute),
		contactLimiter: newRateLimiter(5, 10*time.Minute),
		formLimiter:    newRateLimiter(10, 10*time.Minute),
	}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	if s.cfg.TrustProxy {
		r.Use(middleware.RealIP)
	}
	if !s.Quiet {
		r.Use(middleware.Logger)
	}
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   s.cfg.CORSOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           600,
	}))

	r.Get("/healthz", s.handleHealth)
	r.Handle("/uploads/*", s.serveUploads())
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.handleHealth)

		r.Post("/auth/signup", s.handleSignup)
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/refresh", s.handleRefresh)
		r.Post("/auth/logout", s.handleLogout)
		r.Get("/auth/username-available", s.handleUsernameAvailable)

		r.Get("/public/{username}", s.handlePublicPage)
		r.Post("/public/{username}/events", s.handlePublicEvent)
		r.Post("/public/{username}/contact", s.handlePublicContact)
		r.Post("/public/{username}/form", s.handlePublicForm)
		r.Post("/public/{username}/subscribe", s.handlePublicSubscribe)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Get("/me", s.handleMe)
			r.Group(func(r chi.Router) {
				r.Use(s.withProfile)
				r.Patch("/profile", s.handlePatchProfile)
				r.Get("/profile/editor", s.handleEditor)
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

				r.Post("/tabs", s.handleCreateTab)
				r.Put("/tabs/order", s.handleOrderTabs)
				r.Patch("/tabs/{id}", s.handlePatchTab)
				r.Delete("/tabs/{id}", s.handleDeleteTab)

				r.Post("/blocks", s.handleCreateBlock)
				r.Put("/blocks/order", s.handleOrderBlocks)
				r.Get("/blocks/deleted", s.handleListDeleted)
				r.Post("/blocks/{id}/restore", s.handleRestoreBlock)
				r.Patch("/blocks/{id}", s.handlePatchBlock)
				r.Delete("/blocks/{id}", s.handleDeleteBlock)
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
