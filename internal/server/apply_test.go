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

func postApply(t *testing.T, srv *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/apply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// apply sends a well-formed application and insists it was stored, so a test
// about what happens next does not have to re-check that first.
func apply(t *testing.T, srv *Server, username string) {
	t.Helper()
	if rec := postApply(t, srv, applicantForm(username)); rec.Code != http.StatusCreated {
		t.Fatalf("apply as %s: status %d (body %q)", username, rec.Code, rec.Body.String())
	}
}

// checkStatus performs the public lookup and returns the rendered page.
func checkStatus(t *testing.T, srv *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, srv, http.MethodGet, "/apply/status?applicant="+url.QueryEscape(query), nil)
}

// latestRequest is the id of the newest access_requests row for a username.
func latestRequest(t *testing.T, srv *Server, username string) int64 {
	t.Helper()

	requests, err := srv.access.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, req := range requests { // newest first
		if req.Username == username {
			return req.ID
		}
	}
	t.Fatalf("no access request for %s", username)
	return 0
}

// adminID is the bootstrapped admin, who decides requests in these tests.
func adminID(t *testing.T, srv *Server) int64 {
	t.Helper()

	var id int64
	if err := srv.db.QueryRow("SELECT id FROM users WHERE username = 'admin'").Scan(&id); err != nil {
		t.Fatalf("admin id: %v", err)
	}
	return id
}

// Applying for access cannot require access. Both routes answer an anonymous
// caller, and neither hands one anything back that resembles a session.
func TestApplyRoutesArePublicAndIssueNoSession(t *testing.T) {
	srv := newTestServer(t)

	cases := []struct {
		name string
		rec  *httptest.ResponseRecorder
		want int
	}{
		{"GET /apply", do(t, srv, http.MethodGet, "/apply", nil), http.StatusOK},
		{"POST /apply", postApply(t, srv, applicantForm("applicant")), http.StatusCreated},
		{"GET /apply/status", do(t, srv, http.MethodGet, "/apply/status", nil), http.StatusOK},
		{"GET /apply/status?applicant", checkStatus(t, srv, "applicant"), http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Reaching the handler without a cookie is itself the proof that
			// the route is on auth's exempt list.
			if c.rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body %q)", c.rec.Code, c.want, c.rec.Body.String())
			}
			for _, cookie := range c.rec.Result().Cookies() {
				if cookie.Name == auth.SessionName {
					t.Error("a public apply route issued a session cookie")
				}
			}
		})
	}
}

// An application is a row in access_requests and nothing else. In particular it
// is not a user, which is what makes the studio unreachable rather than merely
// guarded.
func TestApplyCreatesARequestAndNoUser(t *testing.T) {
	srv := newTestServer(t)
	apply(t, srv, "applicant")

	requests, err := srv.access.List(context.Background(), auth.RequestPending)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(requests) != 1 || requests[0].Username != "applicant" {
		t.Fatalf("pending requests = %+v, want one for applicant", requests)
	}

	var users int
	if err := srv.db.QueryRow("SELECT COUNT(*) FROM users WHERE username = 'applicant'").Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 0 {
		t.Errorf("applying created %d user rows, want 0", users)
	}
}

// The audit this stage exists to satisfy: whatever state their request is in,
// an applicant reaches no studio route. They cannot even obtain a session,
// because approval — not application — is what creates the account.
func TestApplicantReachesNoStudioRoute(t *testing.T) {
	srv := newTestServer(t)
	apply(t, srv, "applicant")

	form := url.Values{"username": {"applicant"}, "password": {applicantPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login as an applicant = %d, want 401", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionName {
			t.Fatal("a failed applicant login issued a session cookie")
		}
	}

	for _, route := range studioRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := do(t, srv, route.method, route.path, nil)
			if rec.Code != http.StatusFound {
				t.Errorf("status = %d, want 302 to /login", rec.Code)
			}
		})
	}
}

// A refused application comes back as the form, not as a status code nobody
// sees, and it keeps what was typed — except the password.
func TestApplyRejectsBadInputWith400(t *testing.T) {
	srv := newTestServer(t)

	cases := map[string]url.Values{
		"no username":     {"password": {applicantPassword}},
		"short username":  {"username": {"ab"}, "password": {applicantPassword}},
		"spaced username": {"username": {"two words"}, "password": {applicantPassword}},
		"no password":     {"username": {"applicant"}},
		"short password":  {"username": {"applicant"}, "password": {"short"}},
		"bad email": {"username": {"applicant"}, "password": {applicantPassword},
			"email": {"not-an-address"}},
	}
	for name, form := range cases {
		t.Run(name, func(t *testing.T) {
			rec := postApply(t, srv, form)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "Application not sent") {
				t.Error("the rejection did not re-render the form with a reason")
			}
			if strings.Contains(rec.Body.String(), applicantPassword) {
				t.Error("the rejected form echoed the submitted password back")
			}
		})
	}
}

// Two ways an application can be pointless, both refused at the form rather
// than at an administrator's desk.
func TestApplyRefusesDuplicates(t *testing.T) {
	srv := newTestServer(t)
	apply(t, srv, "applicant")

	for name, who := range map[string]string{
		"already waiting": "applicant",
		"already a user":  "admin",
	} {
		t.Run(name, func(t *testing.T) {
			rec := postApply(t, srv, applicantForm(who))
			if rec.Code != http.StatusConflict {
				t.Errorf("status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}

	// The refusal left the queue as it was: one request, not two.
	requests, err := srv.access.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(requests) != 1 {
		t.Errorf("access requests = %d, want 1", len(requests))
	}
}

// The checkpoint decision: a denial is not a ban. Re-applying opens a fresh
// request, and the lookup answers with the new one.
func TestDeniedApplicantMayApplyAgain(t *testing.T) {
	srv := newTestServer(t)
	apply(t, srv, "applicant")
	if err := srv.access.Deny(context.Background(),
		latestRequest(t, srv, "applicant"), adminID(t, srv)); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	if body := checkStatus(t, srv, "applicant").Body.String(); !strings.Contains(body, "Not approved") {
		t.Fatal("the lookup did not report the denial")
	}

	apply(t, srv, "applicant")
	if body := checkStatus(t, srv, "applicant").Body.String(); !strings.Contains(body, "Waiting for a decision") {
		t.Error("the lookup did not report the re-application")
	}
}

// The status page discloses one thing: which of the three states the request is
// in. Each is reachable by the identifier the applicant supplied, by username
// and by email alike.
func TestApplyStatusReportsOnlyTheState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		decide func(t *testing.T, srv *Server, id, admin int64)
		want   string
	}{
		{"pending", func(*testing.T, *Server, int64, int64) {}, "Waiting for a decision"},
		{"approved", func(t *testing.T, srv *Server, id, admin int64) {
			if _, err := srv.access.Approve(context.Background(), id, admin); err != nil {
				t.Fatalf("Approve: %v", err)
			}
		}, "Approved"},
		{"denied", func(t *testing.T, srv *Server, id, admin int64) {
			if err := srv.access.Deny(context.Background(), id, admin); err != nil {
				t.Fatalf("Deny: %v", err)
			}
		}, "Not approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			apply(t, srv, "applicant")
			tc.decide(t, srv, latestRequest(t, srv, "applicant"), adminID(t, srv))

			for _, query := range []string{"applicant", "applicant@example.com"} {
				rec := checkStatus(t, srv, query)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
				if !strings.Contains(rec.Body.String(), tc.want) {
					t.Errorf("lookup by %q did not report %q", query, tc.want)
				}
			}
		})
	}
}

// Nothing about anyone else, and nothing about the studio, reaches the page —
// not the other applicant's name, not their email, not a voice or a job.
func TestApplyStatusNamesNoOneElse(t *testing.T) {
	srv := newTestServer(t)
	apply(t, srv, "applicant")
	apply(t, srv, "someoneelse")

	body := checkStatus(t, srv, "applicant").Body.String()
	// "admin" is not on this list: the pending blurb says "administrator", and
	// a substring test cannot tell that apart from a leaked username.
	for _, leak := range []string{"someoneelse", "someoneelse@example.com", "Moss"} {
		if strings.Contains(body, leak) {
			t.Errorf("the status page disclosed %q", leak)
		}
	}
}

// An identifier with no application behind it gets an answer, not an error: the
// lookup found nothing, which is a result.
func TestApplyStatusFindsNothingForAStranger(t *testing.T) {
	srv := newTestServer(t)
	apply(t, srv, "applicant")

	rec := checkStatus(t, srv, "stranger")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No application found") {
		t.Error("the lookup did not say it found nothing")
	}
}

// Approval is the only path from application to account, and it is stage 06's.
// Once taken, the applicant signs in like anyone else.
func TestApprovedApplicantCanSignInAndReachTheStudio(t *testing.T) {
	srv := newTestServer(t)
	apply(t, srv, "applicant")
	if _, err := srv.access.Approve(context.Background(),
		latestRequest(t, srv, "applicant"), adminID(t, srv)); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	form := url.Values{"username": {"applicant"}, "password": {applicantPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login after approval = %d, want 200", rec.Code)
	}

	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login after approval issued no session cookie")
	}
	if got := do(t, srv, http.MethodGet, "/", cookie); got.Code != http.StatusOK {
		t.Errorf("GET / as the approved applicant = %d, want 200", got.Code)
	}
}

// Someone with no account has to be able to find the form. The sign-in page is
// where they will look.
func TestLoginPageOffersTheApplyFlow(t *testing.T) {
	srv := newTestServer(t)

	body := do(t, srv, http.MethodGet, "/login", nil).Body.String()
	for _, href := range []string{`href="/apply"`, `href="/apply/status"`} {
		if !strings.Contains(body, href) {
			t.Errorf("the login page does not link to %s", href)
		}
	}
}
