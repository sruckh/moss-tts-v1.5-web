package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/web"
)

// The public application flow. Nothing in this file reads or writes a session:
// /apply and /apply/status behave identically with and without a cookie, which
// is what makes "these routes grant nothing" a property you can test rather
// than a promise you have to audit by reading handlers.
//
// An application is an access_requests row and nothing else. It creates no
// user, so an applicant cannot log in and therefore cannot reach a studio route
// no matter what state their request is in. Stage 06's approval is the only
// thing that turns a request into an account.

// handleApplyPage serves the empty form.
func (s *Server) handleApplyPage(w http.ResponseWriter, r *http.Request) {
	_ = web.Apply(web.ApplyForm{}, "").Render(r.Context(), w)
}

// handleApply records an application. It answers HTML rather than the JSON
// POST /register uses, because this one is reached from a browser form: a
// rejected submission has to come back as the same page with the fields still
// filled, not as a status code the applicant never sees.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		applyError(w, r, web.ApplyForm{}, http.StatusBadRequest,
			"could not read the submitted form")
		return
	}

	// Handed back on rejection. The password deliberately is not.
	form := web.ApplyForm{
		Username: strings.TrimSpace(r.PostFormValue("username")),
		Email:    strings.TrimSpace(r.PostFormValue("email")),
	}

	_, err := s.access.Create(r.Context(), form.Username, form.Email, r.PostFormValue("password"))
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = web.ApplySubmitted(form.Username).Render(r.Context(), w)
	case errors.Is(err, auth.ErrUsernameTaken):
		applyError(w, r, form, http.StatusConflict,
			"that username belongs to an account already — sign in instead if it is yours")
	case errors.Is(err, auth.ErrRequestPending):
		// Dedup by username: the earlier application stands rather than being
		// overwritten, so a second submission cannot quietly replace the
		// password an administrator is about to approve.
		applyError(w, r, form, http.StatusConflict,
			"an application for that username is already waiting for a decision")
	case errors.Is(err, auth.ErrInvalidUsername),
		errors.Is(err, auth.ErrWeakPassword),
		errors.Is(err, auth.ErrInvalidEmail):
		applyError(w, r, form, http.StatusBadRequest, err.Error())
	default:
		// Whatever broke is ours, and an anonymous caller learns nothing about
		// the database from it.
		applyError(w, r, form, http.StatusInternalServerError,
			"could not record the application")
	}
}

// handleApplyStatus answers "where does my application stand" with one word.
//
// The lookup is a single row resolved by identifier rather than a filter over
// auth.AccessRequests.List: a public endpoint should not load another
// applicant's record just to discard it. The query lives here because the
// access_requests store belongs to stage 03 and is not this stage's to change;
// moving it to an AccessRequests.ByApplicant method would be the tidier home.
func (s *Server) handleApplyStatus(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("applicant"))
	if query == "" {
		_ = web.ApplyStatus("", "").Render(r.Context(), w)
		return
	}

	// Newest first: a denied applicant may apply again, which leaves them with
	// a decided row and a waiting one. The waiting one is the answer.
	var state string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT status FROM access_requests
		  WHERE username = ? OR (email IS NOT NULL AND email = ?)
		  ORDER BY id DESC LIMIT 1`, query, query).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		state = web.ApplyStateNone
	case err != nil:
		http.Error(w, "could not look up that application", http.StatusInternalServerError)
		return
	}

	// Only the state travels to the template. No email, no decision timestamp,
	// no deciding administrator, and no second row.
	_ = web.ApplyStatus(query, state).Render(r.Context(), w)
}

// applyError re-renders the form with a reason. The status code is set before
// the body because templ writes straight to the ResponseWriter.
func applyError(w http.ResponseWriter, r *http.Request, form web.ApplyForm, code int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = web.Apply(form, message).Render(r.Context(), w)
}
