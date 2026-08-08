package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/web"
)

// holdingAllowed lists what a signed-in but unapproved user may still reach:
// the public surface, plus the one route that lets them leave. Without /logout
// here an unapproved user would be stuck on the holding screen with no way to
// sign out and try another account.
//
// Everything else is studio surface — pages and HTMX fragments alike — which is
// why the gate is a middleware over the whole router rather than a check bolted
// onto each handler. A new route is gated the moment it is added, instead of
// the moment someone remembers to gate it.
func holdingAllowed(path string) bool {
	return auth.Exempt(path) || path == "/logout"
}

// approvalGate stops anyone whose account is not 'approved' before a studio
// handler runs, so no queue, voice or job data is ever assembled for them.
//
// It must be mounted after auth.Middleware, which is what guarantees a session
// exists by the time this runs.
//
// The status is read live from the database on every gated request, not taken
// from the session snapshot Login stored. That is the whole point: an admin who
// disables an account has it take effect on that user's next request, rather
// than whenever their week-old session happens to expire. The cost is one
// lookup by primary key per gated request; the alternative is making every
// place that can change a user's status also hunt down and invalidate their
// sessions, and forgetting to do that once is a security hole rather than a
// stale page.
func (s *Server) approvalGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if holdingAllowed(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// No session: auth.Middleware has already dealt with the request, and
		// this gate has nothing to add.
		id, ok := s.auth.UserID(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		who, err := s.auth.LiveIdentity(r.Context(), id)
		switch {
		case errors.Is(err, auth.ErrNoSuchUser):
			// The account was deleted while they were signed in. Treat it as
			// switched off rather than letting a dangling session through.
			s.holdingScreen(w, r, true)
			return
		case err != nil:
			http.Error(w, "could not verify your access", http.StatusInternalServerError)
			return
		}
		if who.Status == auth.StatusApproved {
			next.ServeHTTP(w, r)
			return
		}
		s.holdingScreen(w, r, who.Status == auth.StatusDisabled)
	})
}

// holdingScreen answers a gated request. It mirrors auth.Middleware's split:
// HTMX and JSON callers get JSON, because an HTML page is useless to them, and
// browsers get the holding page rendered in place of whatever they asked for.
//
// 403 either way — the request was authenticated and refused, which is exactly
// what Forbidden means. Answering 200 with a holding page would tell a polling
// fragment that everything is fine.
func (s *Server) holdingScreen(w http.ResponseWriter, r *http.Request, disabled bool) {
	if r.Header.Get("HX-Request") == "true" || wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": holdingReason(disabled),
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = web.Holding(disabled).Render(r.Context(), w)
}

func holdingReason(disabled bool) string {
	if disabled {
		return "this account has been disabled"
	}
	return "this account is waiting for approval"
}
