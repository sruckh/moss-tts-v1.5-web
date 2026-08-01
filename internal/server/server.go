// Package server wires the HTTP surface.
//
// Every browser request handled here is sub-second by design: the long RunPod
// render happens out-of-band in a background worker, so Cloudflare's 90s cap
// is never in play.
package server

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/config"
	"github.com/sruckh/timbre/internal/web"
)

// Server holds the dependencies shared by every handler.
type Server struct {
	cfg    config.Config
	db     *sql.DB
	auth   *auth.Manager
	router chi.Router
}

// New builds the router. Voices and the job queue land in later goals.
func New(cfg config.Config, database *sql.DB, authManager *auth.Manager) *Server {
	srv := &Server{cfg: cfg, db: database, auth: authManager, router: chi.NewRouter()}
	srv.routes()
	return srv
}

func (s *Server) routes() {
	s.router.Use(middleware.RequestID)
	// Behind NGINX Proxy Manager and Cloudflare, so the client IP only ever
	// arrives in a forwarded header.
	s.router.Use(middleware.RealIP)
	s.router.Use(middleware.Recoverer)
	// Everything not on auth's exempt list requires a session.
	s.router.Use(s.auth.Middleware)

	s.router.Get("/healthz", s.handleHealth)
	s.router.Get("/login", s.handleLoginPage)
	s.router.Post("/login", s.handleLogin)
	s.router.Post("/logout", s.handleLogout)
	s.router.Handle("/static/*", http.StripPrefix("/static/",
		http.FileServer(http.FS(web.StaticFS()))))
	s.router.Get("/", templ.Handler(web.Shell()).ServeHTTP)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// handleHealth reports liveness for the container healthcheck and for NPM's
// upstream probe. It deliberately says nothing about secrets or the database
// contents — it is reachable without auth.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
