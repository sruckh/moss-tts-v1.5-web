package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/web"
)

// navContext returns the context a full-page render should use: the request's
// own, plus the chrome's view of who is asking — administrator or not, and how
// many access requests are still waiting.
//
// It is called by the four page handlers rather than mounted as middleware on
// purpose. The rail is only drawn by a whole-page render; putting the count
// query in middleware would run it on every HTMX fragment too, which for the
// queue means one extra COUNT every two seconds per open tab.
//
// The role comes from the database, not the session snapshot, for the same
// reason the admin gate reads it live: a demotion should take the link away on
// the next page load.
func (s *Server) navContext(r *http.Request) context.Context {
	ctx := r.Context()
	userID, ok := s.auth.UserID(r)
	if !ok {
		return ctx
	}
	who, err := s.auth.LiveIdentity(ctx, userID)
	if err != nil || who.Role != auth.RoleAdmin {
		// A missing or unreadable identity means no admin chrome. The request
		// itself is already past the session gate, so this is not the place to
		// fail it.
		return ctx
	}
	pending, err := s.pendingRequestCount(ctx)
	if err != nil {
		// The badge is decoration on a page the admin can still use. Log it and
		// draw the link without a count rather than 500 the whole page.
		slog.Warn("count pending access requests", "err", err)
	}
	return web.WithNav(ctx, web.NavState{IsAdmin: true, PendingRequests: pending})
}

// pendingRequestCount is the number on the Admin nav badge: applications that
// have been neither approved nor denied.
func (s *Server) pendingRequestCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM access_requests WHERE status = ?", auth.RequestPending).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}
