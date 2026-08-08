package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/auth"
)

const applicantPassword = "a perfectly fine passphrase"

func postRegister(t *testing.T, srv *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func applicantForm(username string) url.Values {
	return url.Values{
		"username": {username},
		"email":    {username + "@example.com"},
		"password": {applicantPassword},
	}
}

// Applying for an account cannot require one, and the reply must not look like
// a login: the applicant is 'pending' and has been granted nothing.
func TestRegisterIsPublicAndIssuesNoSession(t *testing.T) {
	srv := newTestServer(t)

	// No cookie is sent, so reaching the handler at all proves /register is on
	// the exempt list.
	rec := postRegister(t, srv, applicantForm("applicant"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionName {
			t.Fatal("registering issued a session cookie")
		}
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if body["status"] != auth.StatusPending {
		t.Errorf("status = %q, want %q", body["status"], auth.StatusPending)
	}
}

// The applicant can sign in — stage 03's holding screen needs a session to know
// who is waiting — but the session says 'pending', which is what that gate will
// turn them away on.
func TestRegisteredApplicantLogsInAsPending(t *testing.T) {
	srv := newTestServer(t)
	if rec := postRegister(t, srv, applicantForm("applicant")); rec.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}

	form := url.Values{"username": {"applicant"}, "password": {applicantPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", rec.Code)
	}

	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("logging in as the applicant issued no session cookie")
	}

	next := httptest.NewRequest(http.MethodGet, "/", nil)
	next.AddCookie(cookie)
	who, ok := srv.auth.Current(next)
	if !ok {
		t.Fatal("the applicant's session is not readable")
	}
	if who.Role != auth.RoleUser || who.Status != auth.StatusPending {
		t.Errorf("applicant session = (%q, %q), want (%q, %q)",
			who.Role, who.Status, auth.RoleUser, auth.StatusPending)
	}
}

// The admin's session, by contrast, carries the credentials a gate needs to let
// it through.
func TestAdminSessionCarriesAdminAndApproved(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(login(t, srv))
	who, ok := srv.auth.Current(req)
	if !ok {
		t.Fatal("the admin's session is not readable")
	}
	if who.Role != auth.RoleAdmin || who.Status != auth.StatusApproved {
		t.Errorf("admin session = (%q, %q), want (%q, %q)",
			who.Role, who.Status, auth.RoleAdmin, auth.StatusApproved)
	}
}

func TestRegisterRejectsATakenUsername(t *testing.T) {
	srv := newTestServer(t)
	if rec := postRegister(t, srv, applicantForm("applicant")); rec.Code != http.StatusCreated {
		t.Fatalf("first register status = %d, want 201", rec.Code)
	}

	for _, name := range []string{"applicant", "admin"} {
		t.Run(name, func(t *testing.T) {
			if rec := postRegister(t, srv, applicantForm(name)); rec.Code != http.StatusConflict {
				t.Errorf("status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRegisterRejectsBadInputWith400(t *testing.T) {
	srv := newTestServer(t)

	cases := map[string]url.Values{
		"no username":    {"password": {applicantPassword}},
		"short username": {"username": {"ab"}, "password": {applicantPassword}},
		"no password":    {"username": {"applicant"}},
		"short password": {"username": {"applicant"}, "password": {"short"}},
		"bad email": {"username": {"applicant"}, "password": {applicantPassword},
			"email": {"not-an-address"}},
	}
	for name, form := range cases {
		t.Run(name, func(t *testing.T) {
			rec := postRegister(t, srv, form)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			if body["error"] == "" {
				t.Error("a rejection carried no reason")
			}
		})
	}
}

// Being exempt from the session gate is not the same as being a page. GET
// /register reaches the router and is refused on the method, not the session.
func TestRegisterIsPostOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/register", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /register status = %d, want 405", rec.Code)
	}
}
