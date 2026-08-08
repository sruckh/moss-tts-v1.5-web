package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/auth"
)

// studioRoutes is every route that carries studio surface — pages and the HTMX
// fragments behind them. The gate has to cover all of them, so the audit test
// walks this list rather than spot-checking `/`.
var studioRoutes = []struct{ method, path string }{
	{http.MethodGet, "/"},
	{http.MethodGet, "/voices"},
	{http.MethodPost, "/voices/upload"},
	{http.MethodPost, "/voices/1/name"},
	{http.MethodGet, "/voices/1/reference"},
	{http.MethodGet, "/queue"},
	{http.MethodGet, "/jobs"},
	{http.MethodPost, "/jobs"},
	{http.MethodGet, "/jobs/1/audio"},
	{http.MethodGet, "/jobs/1/player"},
	{http.MethodDelete, "/jobs/1"},
	{http.MethodGet, "/health"},
}

// signInAs registers an applicant, moves them to status, and returns their
// session cookie. The account is real and the session is genuine — only the
// approval state varies, which is what the gate keys on.
func signInAs(t *testing.T, srv *Server, username, status string) *http.Cookie {
	t.Helper()

	if _, err := srv.auth.Register(context.Background(), username, "", applicantPassword); err != nil {
		t.Fatalf("Register(%s): %v", username, err)
	}
	setStatus(t, srv, username, status)

	form := url.Values{"username": {username}, "password": {applicantPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login as %s: status %d", username, rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionName {
			return c
		}
	}
	t.Fatalf("login as %s issued no session cookie", username)
	return nil
}

func setStatus(t *testing.T, srv *Server, username, status string) {
	t.Helper()
	if _, err := srv.db.ExecContext(context.Background(),
		"UPDATE users SET status = ? WHERE username = ?", status, username); err != nil {
		t.Fatalf("set %s status=%s: %v", username, status, err)
	}
}

func do(t *testing.T, srv *Server, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// The stage audit: no studio route returns data for a pending or disabled
// session. Every one answers 403 and the holding screen, never a fragment.
func TestNoStudioRouteServesAnUnapprovedSession(t *testing.T) {
	for _, status := range []string{auth.StatusPending, auth.StatusDisabled} {
		t.Run(status, func(t *testing.T) {
			srv := newTestServer(t)
			cookie := signInAs(t, srv, "applicant", status)

			for _, route := range studioRoutes {
				rec := do(t, srv, route.method, route.path, cookie)

				if rec.Code != http.StatusForbidden {
					t.Errorf("%s %s = %d, want 403", route.method, route.path, rec.Code)
					continue
				}
				body := rec.Body.String()
				// The holding screen is the only thing that may come back. If a
				// studio handler had run, its markup would be here instead.
				for _, leak := range []string{"queue", "Render Queue", "voice-grid", "playback-body"} {
					if strings.Contains(body, leak) {
						t.Errorf("%s %s leaked %q into a %s session's response",
							route.method, route.path, leak, status)
					}
				}
			}
		})
	}
}

func TestApprovedSessionReachesTheStudio(t *testing.T) {
	srv := newTestServer(t)
	cookie := signInAs(t, srv, "approved", auth.StatusApproved)

	rec := do(t, srv, http.MethodGet, "/", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Waiting for approval") {
		t.Error("an approved user was shown the holding screen")
	}
}

// The reason the gate reads the database rather than the session: an admin
// disabling an account must take effect on that account's next request, not
// whenever its week-old session expires.
func TestDisablingTakesEffectOnTheNextRequest(t *testing.T) {
	srv := newTestServer(t)
	cookie := signInAs(t, srv, "approved", auth.StatusApproved)

	if rec := do(t, srv, http.MethodGet, "/", cookie); rec.Code != http.StatusOK {
		t.Fatalf("GET / before disabling = %d, want 200", rec.Code)
	}

	// The session cookie is untouched and still says 'approved'.
	setStatus(t, srv, "approved", auth.StatusDisabled)

	rec := do(t, srv, http.MethodGet, "/", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET / after disabling = %d, want 403 — the gate read a stale session", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Access disabled") {
		t.Error("a disabled user was not told their account was switched off")
	}
}

// Approval works the same way round: an admin approving a waiting user lets
// them in without making them sign in again.
func TestApprovingTakesEffectOnTheNextRequest(t *testing.T) {
	srv := newTestServer(t)
	cookie := signInAs(t, srv, "applicant", auth.StatusPending)

	if rec := do(t, srv, http.MethodGet, "/", cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("GET / while pending = %d, want 403", rec.Code)
	}
	setStatus(t, srv, "applicant", auth.StatusApproved)

	if rec := do(t, srv, http.MethodGet, "/", cookie); rec.Code != http.StatusOK {
		t.Errorf("GET / after approval = %d, want 200", rec.Code)
	}
}

// The two holding states read differently, because "waiting" resolves on its
// own and "disabled" needs a conversation.
func TestHoldingScreenDistinguishesPendingFromDisabled(t *testing.T) {
	for _, tc := range []struct{ status, want, notWant string }{
		{auth.StatusPending, "Waiting for approval", "Access disabled"},
		{auth.StatusDisabled, "Access disabled", "Waiting for approval"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			srv := newTestServer(t)
			cookie := signInAs(t, srv, "applicant", tc.status)

			body := do(t, srv, http.MethodGet, "/", cookie).Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("holding screen does not say %q", tc.want)
			}
			if strings.Contains(body, tc.notWant) {
				t.Errorf("holding screen wrongly says %q", tc.notWant)
			}
		})
	}
}

// An HTML page is useless to a polling fragment, so HTMX and JSON callers get
// JSON — and a 403, not a 200 that would tell them everything is fine.
func TestGateAnswersHTMXAndJSONCallersInKind(t *testing.T) {
	srv := newTestServer(t)
	cookie := signInAs(t, srv, "applicant", auth.StatusPending)

	for name, header := range map[string][2]string{
		"htmx": {"HX-Request", "true"},
		"json": {"Accept", "application/json"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
			req.AddCookie(cookie)
			req.Header.Set(header[0], header[1])
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if strings.Contains(rec.Body.String(), "<html") {
				t.Error("a fragment caller was sent an HTML page")
			}
		})
	}
}

// Being held must not be a trap: signing out is the one thing an unapproved
// user can still do.
func TestUnapprovedUserCanStillSignOut(t *testing.T) {
	srv := newTestServer(t)
	cookie := signInAs(t, srv, "applicant", auth.StatusPending)

	rec := do(t, srv, http.MethodPost, "/logout", cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /logout = %d, want 303", rec.Code)
	}
}

// The gate sits after the session gate and must not change what the public
// surface does.
func TestGateLeavesThePublicSurfaceAlone(t *testing.T) {
	srv := newTestServer(t)

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/login", http.StatusOK},
		{http.MethodGet, "/static/app.css", http.StatusOK},
		// Still a redirect to /login, not a holding screen: no session at all is
		// a different answer from a session that is not approved.
		{http.MethodGet, "/", http.StatusFound},
	} {
		if rec := do(t, srv, tc.method, tc.path, nil); rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}

// A session outliving its user is a dangling credential, not an approved one.
func TestDeletedUserIsHeldNotAdmitted(t *testing.T) {
	srv := newTestServer(t)
	cookie := signInAs(t, srv, "approved", auth.StatusApproved)

	if _, err := srv.db.ExecContext(context.Background(),
		"DELETE FROM users WHERE username = 'approved'"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if rec := do(t, srv, http.MethodGet, "/", cookie); rec.Code != http.StatusForbidden {
		t.Errorf("GET / for a deleted user = %d, want 403", rec.Code)
	}
}
