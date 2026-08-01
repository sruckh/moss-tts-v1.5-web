package server

import (
	"errors"
	"net/http"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/web"
)

// handleLoginPage renders the login form. Already-authenticated users have no
// business here and go straight to the app.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth.UserID(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_ = web.Login("").Render(r.Context(), w)
}

// handleLogin verifies the submitted credentials. On success it responds 200
// with the session cookie set and a body that moves the browser to the app
// (HX-Redirect for HTMX, a script + link otherwise); on bad credentials it
// re-renders the form with a 401 and no cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	err := s.auth.Login(w, r, r.PostFormValue("username"), r.PostFormValue("password"))
	switch {
	case err == nil:
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/")
		}
		_ = web.LoginSuccess().Render(r.Context(), w)
	case errors.Is(err, auth.ErrInvalidCredentials):
		w.WriteHeader(http.StatusUnauthorized)
		_ = web.Login("Invalid username or password.").Render(r.Context(), w)
	default:
		w.WriteHeader(http.StatusInternalServerError)
		_ = web.Login("Something went wrong. Please try again.").Render(r.Context(), w)
	}
}

// handleLogout destroys the session and returns to the login page.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	_ = s.auth.Logout(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
