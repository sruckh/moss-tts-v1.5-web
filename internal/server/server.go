// Package server wires the HTTP surface.
//
// Every browser request handled here is sub-second by design: the long RunPod
// render happens out-of-band in a background worker, so Cloudflare's 90s cap
// is never in play.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/config"
	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/runpod"
	"github.com/sruckh/timbre/internal/voices"
	"github.com/sruckh/timbre/internal/web"
)

// Server holds the dependencies shared by every handler.
type Server struct {
	cfg    config.Config
	db     *sql.DB
	auth   *auth.Manager
	access *auth.AccessRequests
	voices *voices.Store
	jobs   *jobs.Store
	runpod *runpod.Client
	router chi.Router
}

// New builds the router. runpodClient is used only to probe /health here — job
// submission itself belongs to the background worker, never to a request.
func New(cfg config.Config, database *sql.DB, authManager *auth.Manager,
	voiceStore *voices.Store, jobStore *jobs.Store, runpodClient *runpod.Client) *Server {

	srv := &Server{
		cfg:    cfg,
		db:     database,
		auth:   authManager,
		access: auth.NewAccessRequests(database),
		voices: voiceStore,
		jobs:   jobStore,
		runpod: runpodClient,
		router: chi.NewRouter(),
	}
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
	// ...and a session alone is not enough: only an approved account reaches the
	// studio. Ordered after the session gate, which is what guarantees there is
	// a user to check.
	s.router.Use(s.approvalGate)

	s.router.Get("/healthz", s.handleHealth)
	s.router.Get("/login", s.handleLoginPage)
	s.router.Post("/login", s.handleLogin)
	s.router.Post("/logout", s.handleLogout)
	// Public: applying for an account cannot require one. Creates a 'pending'
	// user and issues no session.
	s.router.Post("/register", s.handleRegister)
	// ...and the browser-facing version of the same idea: the application form,
	// and the lookup that tells an applicant where their request stands. Both
	// write or read access_requests only and neither issues a session.
	s.router.Get("/apply", s.handleApplyPage)
	s.router.Post("/apply", s.handleApply)
	s.router.Get("/apply/status", s.handleApplyStatus)
	s.router.Handle("/static/*", http.StripPrefix("/static/",
		http.FileServer(http.FS(web.StaticFS()))))
	s.router.Get("/health", s.handleRunPodHealth)
	s.router.Route("/admin", func(r chi.Router) {
		r.Use(s.adminOnly)
		r.Get("/", s.handleAdmin)
		r.Post("/users/{id}/status", s.handleAdminUserStatus)
		r.Post("/users/{id}/role", s.handleAdminUserRole)
		r.Delete("/users/{id}", s.handleAdminDeleteUser)
		r.Post("/requests/{id}/approve", s.handleAdminApproveRequest)
		r.Post("/requests/{id}/deny", s.handleAdminDenyRequest)
		r.Delete("/requests/{id}", s.handleAdminDeleteRequest)
		r.Post("/voices/{id}/global", s.handleAdminVoiceGlobal)
		r.Post("/voices/{id}/owner", s.handleAdminVoiceOwner)
		r.Post("/voices/{id}/unassign", s.handleAdminVoiceUnassign)
	})
	s.router.Get("/voices", s.handleVoiceLibrary)
	s.router.Post("/voices/upload", s.handleVoiceUpload)
	s.router.Post("/voices/{id}/name", s.handleVoiceRename)
	// Session-gated preview of a stored reference clip. It is not a public URL
	// and RunPod never sees it: submission still carries the bytes base64-inline.
	s.router.Get("/voices/{id}/reference", s.handleVoiceReference)
	s.router.Get("/queue", s.handleQueuePage)
	s.router.Get("/jobs/queue", s.handleQueue)
	s.router.Get("/jobs", s.handleQueue)
	s.router.Post("/jobs", s.handleCreateJob)
	s.router.Get("/jobs/{id}/audio", s.handleDownloadAudio)
	s.router.Get("/jobs/{id}/player", s.handleJobPlayer)
	s.router.Delete("/jobs/{id}", s.handleDeleteJob)
	s.router.Get("/", s.handleStudio)
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

// runPodHealthTimeout bounds the upstream probe so /health answers promptly
// even when the endpoint is wedged.
const runPodHealthTimeout = 5 * time.Second

// handleRunPodHealth answers GET /health: the app plus a probe of the RunPod
// endpoint's worker pool and queue depth.
//
// It sits behind the session gate, unlike /healthz. /healthz is what the
// container HEALTHCHECK and NPM's upstream probe call, and container liveness
// must not depend on a third party — a RunPod outage would otherwise restart a
// perfectly healthy app. This route is the operator-facing view, and it costs
// an upstream call, so it needs a login.
func (s *Server) handleRunPodHealth(w http.ResponseWriter, r *http.Request) {
	type runpodStatus struct {
		Configured bool           `json:"configured"`
		Reachable  bool           `json:"reachable"`
		Error      string         `json:"error,omitempty"`
		Detail     *runpod.Health `json:"detail,omitempty"`
	}
	body := struct {
		OK     bool         `json:"ok"`
		RunPod runpodStatus `json:"runpod"`
	}{OK: true}

	body.RunPod.Configured = s.runpod != nil && s.runpod.Configured()
	switch {
	case !body.RunPod.Configured:
		body.RunPod.Error = "RUNPOD_ENDPOINT or RUNPOD_API_KEY is not configured"
	default:
		ctx, cancel := context.WithTimeout(r.Context(), runPodHealthTimeout)
		defer cancel()

		health, err := s.runpod.Health(ctx)
		if err != nil {
			body.RunPod.Error = err.Error()
		} else {
			body.RunPod.Reachable = true
			body.RunPod.Detail = &health
		}
	}

	w.Header().Set("Content-Type", "application/json")
	// Still 200: the app is up. The runpod object carries the upstream verdict,
	// so a monitor watching this route can distinguish the two failures.
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}
