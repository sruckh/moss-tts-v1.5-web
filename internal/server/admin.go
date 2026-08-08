package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/voices"
	"github.com/sruckh/timbre/internal/web"
)

var (
	errAdminUserNotFound = errors.New("user not found")
	errAdminInput        = errors.New("invalid admin input")
	errLastAdmin         = errors.New("the last approved admin cannot be changed")
	errSelfDemotion      = errors.New("an admin cannot demote their own account")
)

// adminOnly protects the management surface with the user's live database
// identity. A role change therefore takes effect on the very next request even
// when the session cookie still carries the older role snapshot.
func (s *Server) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := s.auth.UserID(r)
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		who, err := s.auth.LiveIdentity(r.Context(), userID)
		switch {
		case errors.Is(err, auth.ErrNoSuchUser):
			s.adminForbidden(w, r)
			return
		case err != nil:
			serverError(w, r, err)
			return
		}
		if who.Role != auth.RoleAdmin {
			s.adminForbidden(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) adminForbidden(w http.ResponseWriter, r *http.Request) {
	if wantsJSON(r) || r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "admin access required"})
		return
	}
	http.Error(w, "admin access required", http.StatusForbidden)
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	data, err := s.adminData(r.Context())
	if err != nil {
		serverError(w, r, err)
		return
	}
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(data)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = web.AdminPage(data).Render(s.navContext(r), w)
}

func (s *Server) adminData(ctx context.Context) (web.AdminData, error) {
	users, err := s.adminUsers(ctx)
	if err != nil {
		return web.AdminData{}, err
	}
	requests, err := s.access.List(ctx, "")
	if err != nil {
		return web.AdminData{}, err
	}
	cards, err := s.adminVoices(ctx)
	if err != nil {
		return web.AdminData{}, err
	}
	return web.AdminData{Users: users, Requests: requests, Voices: cards}, nil
}

func (s *Server) adminUsers(ctx context.Context) ([]web.AdminUser, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, COALESCE(email, ''), role, status, created_at
		FROM users
		ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list admin users: %w", err)
	}
	defer rows.Close()

	var out []web.AdminUser
	for rows.Next() {
		var user web.AdminUser
		if err := rows.Scan(&user.ID, &user.Username, &user.Email, &user.Role, &user.Status, &user.CreatedAt); err != nil {
			return nil, fmt.Errorf("list admin users: scan: %w", err)
		}
		out = append(out, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list admin users: %w", err)
	}
	return out, nil
}

// adminVoices lists every voice card with every user currently granted access
// to it. Access is many-to-many through voice_assignments, so this is a
// one-to-many fetch (card -> assignees), not a single LEFT JOIN on the legacy
// owner_id mirror column — that column reflects only the most recent grant
// and would hide every earlier one still in force.
func (s *Server) adminVoices(ctx context.Context) ([]web.AdminVoice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.id, v.kind, v.name, COALESCE(v.model, ''), v.is_global, v.created_at
		FROM voices v
		ORDER BY v.created_at, v.id`)
	if err != nil {
		return nil, fmt.Errorf("list admin voices: %w", err)
	}
	defer rows.Close()

	var out []web.AdminVoice
	for rows.Next() {
		var card web.AdminVoice
		var global int
		if err := rows.Scan(&card.ID, &card.Kind, &card.Name, &card.Model, &global, &card.CreatedAt); err != nil {
			return nil, fmt.Errorf("list admin voices: scan: %w", err)
		}
		card.IsGlobal = global != 0
		out = append(out, card)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list admin voices: %w", err)
	}

	byVoiceID := make(map[int64]*web.AdminVoice, len(out))
	for i := range out {
		byVoiceID[out[i].ID] = &out[i]
	}

	assignRows, err := s.db.QueryContext(ctx, `
		SELECT va.voice_id, u.id, u.username
		FROM voice_assignments va
		JOIN users u ON u.id = va.user_id
		ORDER BY va.voice_id, u.username`)
	if err != nil {
		return nil, fmt.Errorf("list voice assignments: %w", err)
	}
	defer assignRows.Close()
	for assignRows.Next() {
		var voiceID int64
		var assignee web.AdminVoiceAssignee
		if err := assignRows.Scan(&voiceID, &assignee.UserID, &assignee.Username); err != nil {
			return nil, fmt.Errorf("list voice assignments: scan: %w", err)
		}
		if card, ok := byVoiceID[voiceID]; ok {
			card.Assignees = append(card.Assignees, assignee)
		}
	}
	if err := assignRows.Err(); err != nil {
		return nil, fmt.Errorf("list voice assignments: %w", err)
	}
	return out, nil
}

func (s *Server) adminActionDone(w http.ResponseWriter, r *http.Request) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		data, err := s.adminData(r.Context())
		if err != nil {
			serverError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = web.AdminPanel(data).Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func adminRouteID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func (s *Server) handleAdminUserStatus(w http.ResponseWriter, r *http.Request) {
	id, err := adminRouteID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}
	if err := s.setAdminUserStatus(r.Context(), id, strings.TrimSpace(r.PostFormValue("status"))); err != nil {
		s.adminError(w, r, err)
		return
	}
	s.adminActionDone(w, r)
}

func (s *Server) handleAdminUserRole(w http.ResponseWriter, r *http.Request) {
	id, err := adminRouteID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actorID, _ := s.auth.UserID(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}
	if err := s.setAdminUserRole(r.Context(), actorID, id, strings.TrimSpace(r.PostFormValue("role"))); err != nil {
		s.adminError(w, r, err)
		return
	}
	s.adminActionDone(w, r)
}

func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := adminRouteID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actorID, _ := s.auth.UserID(r)
	audioPaths, err := s.deleteAdminUser(r.Context(), id)
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	for _, path := range audioPaths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("remove deleted user's audio", "path", path, "err", err)
		}
	}
	if id == actorID {
		if err := s.auth.Logout(w, r); err != nil {
			serverError(w, r, err)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.adminActionDone(w, r)
}

func (s *Server) handleAdminApproveRequest(w http.ResponseWriter, r *http.Request) {
	id, err := adminRouteID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actorID, _ := s.auth.UserID(r)
	if _, err := s.access.Approve(r.Context(), id, actorID); err != nil {
		s.adminError(w, r, err)
		return
	}
	s.adminActionDone(w, r)
}

func (s *Server) handleAdminDenyRequest(w http.ResponseWriter, r *http.Request) {
	id, err := adminRouteID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actorID, _ := s.auth.UserID(r)
	if err := s.access.Deny(r.Context(), id, actorID); err != nil {
		s.adminError(w, r, err)
		return
	}
	s.adminActionDone(w, r)
}

func (s *Server) handleAdminDeleteRequest(w http.ResponseWriter, r *http.Request) {
	id, err := adminRouteID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := s.db.ExecContext(r.Context(), "DELETE FROM access_requests WHERE id = ?", id)
	if err != nil {
		serverError(w, r, fmt.Errorf("delete access request %d: %w", id, err))
		return
	}
	affected, err := res.RowsAffected()
	if err != nil {
		serverError(w, r, fmt.Errorf("delete access request %d: %w", id, err))
		return
	}
	if affected == 0 {
		s.adminError(w, r, auth.ErrNoSuchRequest)
		return
	}
	s.adminActionDone(w, r)
}

func (s *Server) handleAdminVoiceGlobal(w http.ResponseWriter, r *http.Request) {
	id, err := adminRouteID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}
	isGlobal, err := strconv.ParseBool(strings.TrimSpace(r.PostFormValue("is_global")))
	if err != nil {
		http.Error(w, "is_global must be true or false", http.StatusBadRequest)
		return
	}
	if err := s.voices.SetGlobal(r.Context(), id, isGlobal); err != nil {
		s.adminError(w, r, err)
		return
	}
	s.adminActionDone(w, r)
}

// handleAdminVoiceOwner grants a user access to a card. Access is
// many-to-many: this adds a grant alongside whatever the card already has, it
// never replaces one user with another. user_id=0 is the one exception — an
// admin-only reset that clears every existing grant at once, kept for the
// API but no longer offered by the UI now that individual revoke (see
// handleAdminVoiceUnassign) covers the normal case.
func (s *Server) handleAdminVoiceOwner(w http.ResponseWriter, r *http.Request) {
	id, err := adminRouteID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}
	ownerID, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("user_id")), 10, 64)
	if err != nil || ownerID < 0 {
		http.Error(w, "user_id must be zero or a user id", http.StatusBadRequest)
		return
	}
	if ownerID > 0 {
		var exists int
		if err := s.db.QueryRowContext(r.Context(), "SELECT 1 FROM users WHERE id = ?", ownerID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				s.adminError(w, r, errAdminUserNotFound)
				return
			}
			serverError(w, r, fmt.Errorf("validate voice owner: %w", err))
			return
		}
	}
	if err := s.voices.Assign(r.Context(), id, ownerID); err != nil {
		s.adminError(w, r, err)
		return
	}
	s.adminActionDone(w, r)
}

// handleAdminVoiceUnassign revokes one user's access to a card without
// touching anyone else's. The owner route can only ever describe a single
// assignment, and access is a junction table — taking a card away from one
// person is not the same request as handing it to another.
func (s *Server) handleAdminVoiceUnassign(w http.ResponseWriter, r *http.Request) {
	id, err := adminRouteID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}
	userID, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("user_id")), 10, 64)
	if err != nil || userID <= 0 {
		http.Error(w, "user_id must be a user id", http.StatusBadRequest)
		return
	}
	if err := s.voices.Unassign(r.Context(), id, userID); err != nil {
		s.adminError(w, r, err)
		return
	}
	s.adminActionDone(w, r)
}

func (s *Server) adminError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errAdminUserNotFound), errors.Is(err, auth.ErrNoSuchRequest), errors.Is(err, voices.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errAdminInput):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errLastAdmin), errors.Is(err, errSelfDemotion),
		errors.Is(err, auth.ErrRequestNotPending), errors.Is(err, auth.ErrUsernameTaken):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		serverError(w, r, err)
	}
}

type adminUserState struct {
	Role   string
	Status string
}

func adminUserStateFor(ctx context.Context, tx *sql.Tx, userID int64) (adminUserState, error) {
	var state adminUserState
	if err := tx.QueryRowContext(ctx,
		"SELECT role, status FROM users WHERE id = ?", userID).Scan(&state.Role, &state.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return adminUserState{}, errAdminUserNotFound
		}
		return adminUserState{}, fmt.Errorf("read user %d: %w", userID, err)
	}
	return state, nil
}

func approvedAdminCount(ctx context.Context, tx *sql.Tx) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM users WHERE role = ? AND status = ?",
		auth.RoleAdmin, auth.StatusApproved).Scan(&count); err != nil {
		return 0, fmt.Errorf("count approved admins: %w", err)
	}
	return count, nil
}

func (s *Server) setAdminUserStatus(ctx context.Context, userID int64, status string) error {
	if status != auth.StatusApproved && status != auth.StatusDisabled {
		return fmt.Errorf("%w: status must be approved or disabled", errAdminInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set user status: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	state, err := adminUserStateFor(ctx, tx, userID)
	if err != nil {
		return err
	}
	if state.Role == auth.RoleAdmin && state.Status == auth.StatusApproved && status != auth.StatusApproved {
		count, err := approvedAdminCount(ctx, tx)
		if err != nil {
			return err
		}
		if count <= 1 {
			return errLastAdmin
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET status = ? WHERE id = ?", status, userID); err != nil {
		return fmt.Errorf("set user %d status: %w", userID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set user status: commit: %w", err)
	}
	return nil
}

func (s *Server) setAdminUserRole(ctx context.Context, actorID, userID int64, role string) error {
	if role != auth.RoleAdmin && role != auth.RoleUser {
		return fmt.Errorf("%w: role must be admin or user", errAdminInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set user role: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	state, err := adminUserStateFor(ctx, tx, userID)
	if err != nil {
		return err
	}
	if state.Role == auth.RoleAdmin && role == auth.RoleUser {
		if actorID == userID {
			return errSelfDemotion
		}
		if state.Status == auth.StatusApproved {
			count, err := approvedAdminCount(ctx, tx)
			if err != nil {
				return err
			}
			if count <= 1 {
				return errLastAdmin
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET role = ? WHERE id = ?", role, userID); err != nil {
		return fmt.Errorf("set user %d role: %w", userID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set user role: commit: %w", err)
	}
	return nil
}

func (s *Server) deleteAdminUser(ctx context.Context, userID int64) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("delete user: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	state, err := adminUserStateFor(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if state.Role == auth.RoleAdmin && state.Status == auth.StatusApproved {
		count, err := approvedAdminCount(ctx, tx)
		if err != nil {
			return nil, err
		}
		if count <= 1 {
			return nil, errLastAdmin
		}
	}

	rows, err := tx.QueryContext(ctx,
		"SELECT audio_path FROM jobs WHERE user_id = ? AND audio_path IS NOT NULL AND audio_path <> ''", userID)
	if err != nil {
		return nil, fmt.Errorf("delete user: list audio: %w", err)
	}
	var audioPaths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("delete user: scan audio: %w", err)
		}
		audioPaths = append(audioPaths, path)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("delete user: list audio: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("delete user: close audio rows: %w", err)
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE user_id = ?", userID); err != nil {
		return nil, fmt.Errorf("delete user: jobs: %w", err)
	}
	// The cards themselves outlive the account — a clone someone else may still
	// need is not the deleted user's to take with them — so ownership is cleared
	// rather than cascaded. Their grants go, though: an assignment row naming a
	// user who no longer exists is access nobody can audit. The FK would cascade
	// this anyway; doing it here means the invariant does not depend on the
	// foreign_keys pragma being on.
	if _, err := tx.ExecContext(ctx, "UPDATE voices SET owner_id = NULL WHERE owner_id = ?", userID); err != nil {
		return nil, fmt.Errorf("delete user: voices: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM voice_assignments WHERE user_id = ?", userID); err != nil {
		return nil, fmt.Errorf("delete user: voice assignments: %w", err)
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", userID)
	if err != nil {
		return nil, fmt.Errorf("delete user: account: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("delete user: account: %w", err)
	}
	if affected == 0 {
		return nil, errAdminUserNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("delete user: commit: %w", err)
	}
	return audioPaths, nil
}
