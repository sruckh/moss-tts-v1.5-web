package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/jobs"
)

func adminAction(t *testing.T, srv *Server, cookie *http.Cookie, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func userIDByName(t *testing.T, srv *Server, username string) int64 {
	t.Helper()
	var id int64
	if err := srv.db.QueryRowContext(context.Background(),
		"SELECT id FROM users WHERE username = ?", username).Scan(&id); err != nil {
		t.Fatalf("user %s id: %v", username, err)
	}
	return id
}

func TestEveryAdminRouteRejectsNonAdmin(t *testing.T) {
	srv := newTestServer(t)
	cookie := signInAs(t, srv, "ordinary_user", auth.StatusApproved)

	tests := []struct {
		method string
		path   string
		form   url.Values
	}{
		{http.MethodGet, "/admin/", nil},
		{http.MethodPost, "/admin/users/1/status", url.Values{"status": {auth.StatusDisabled}}},
		{http.MethodPost, "/admin/users/1/role", url.Values{"role": {auth.RoleUser}}},
		{http.MethodDelete, "/admin/users/1", nil},
		{http.MethodPost, "/admin/requests/1/approve", nil},
		{http.MethodPost, "/admin/requests/1/deny", nil},
		{http.MethodDelete, "/admin/requests/1", nil},
		{http.MethodPost, "/admin/voices/1/global", url.Values{"is_global": {"false"}}},
		{http.MethodPost, "/admin/voices/1/owner", url.Values{"user_id": {"0"}}},
		{http.MethodPost, "/admin/voices/1/unassign", url.Values{"user_id": {"1"}}},
	}

	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			rec := adminAction(t, srv, cookie, test.method, test.path, test.form)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAdminPageRendersManagementData(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	_ = signInAs(t, srv, "listed_user", auth.StatusApproved)
	if _, err := srv.access.Create(context.Background(), "waiting_user", "waiting@example.com", applicantPassword); err != nil {
		t.Fatalf("Create access request: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"Manage access and ownership", "listed_user", "waiting_user", "Moss"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("admin page missing %q", want)
		}
	}
}

func TestLastApprovedAdminCannotBeChanged(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	adminID := userIDByName(t, srv, "admin")
	id := strconv.FormatInt(adminID, 10)

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"delete":  adminAction(t, srv, cookie, http.MethodDelete, "/admin/users/"+id, nil),
		"demote":  adminAction(t, srv, cookie, http.MethodPost, "/admin/users/"+id+"/role", url.Values{"role": {auth.RoleUser}}),
		"disable": adminAction(t, srv, cookie, http.MethodPost, "/admin/users/"+id+"/status", url.Values{"status": {auth.StatusDisabled}}),
	} {
		if rec.Code != http.StatusConflict {
			t.Errorf("%s status = %d, want 409 (body %q)", name, rec.Code, rec.Body.String())
		}
	}

	who, err := srv.auth.LiveIdentity(context.Background(), adminID)
	if err != nil {
		t.Fatalf("LiveIdentity: %v", err)
	}
	if who.Role != auth.RoleAdmin || who.Status != auth.StatusApproved {
		t.Fatalf("admin after guards = %+v, want approved admin", who)
	}
}

func TestAdminCanUpdateUserStatusAndRole(t *testing.T) {
	srv := newTestServer(t)
	adminCookie := login(t, srv)
	_ = signInAs(t, srv, "managed_user", auth.StatusApproved)
	userID := userIDByName(t, srv, "managed_user")
	id := strconv.FormatInt(userID, 10)

	steps := []struct {
		path string
		form url.Values
	}{
		{"/admin/users/" + id + "/status", url.Values{"status": {auth.StatusDisabled}}},
		{"/admin/users/" + id + "/status", url.Values{"status": {auth.StatusApproved}}},
		{"/admin/users/" + id + "/role", url.Values{"role": {auth.RoleAdmin}}},
		{"/admin/users/" + id + "/role", url.Values{"role": {auth.RoleUser}}},
	}
	for _, step := range steps {
		rec := adminAction(t, srv, adminCookie, http.MethodPost, step.path, step.form)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s status = %d, want 200 (body %q)", step.path, rec.Code, rec.Body.String())
		}
	}

	who, err := srv.auth.LiveIdentity(context.Background(), userID)
	if err != nil {
		t.Fatalf("LiveIdentity: %v", err)
	}
	if who.Role != auth.RoleUser || who.Status != auth.StatusApproved {
		t.Fatalf("managed user = %+v, want approved user", who)
	}
}

func TestAdminManagesAccessRequests(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	ctx := context.Background()

	approveID, err := srv.access.Create(ctx, "approve_me", "approve@example.com", applicantPassword)
	if err != nil {
		t.Fatalf("Create approve request: %v", err)
	}
	denyID, err := srv.access.Create(ctx, "deny_me", "", applicantPassword)
	if err != nil {
		t.Fatalf("Create deny request: %v", err)
	}
	deleteID, err := srv.access.Create(ctx, "delete_me", "", applicantPassword)
	if err != nil {
		t.Fatalf("Create delete request: %v", err)
	}

	actions := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/admin/requests/" + strconv.FormatInt(approveID, 10) + "/approve"},
		{http.MethodPost, "/admin/requests/" + strconv.FormatInt(denyID, 10) + "/deny"},
		{http.MethodDelete, "/admin/requests/" + strconv.FormatInt(deleteID, 10)},
	}
	for _, action := range actions {
		rec := adminAction(t, srv, cookie, action.method, action.path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s status = %d, want 200 (body %q)", action.method, action.path, rec.Code, rec.Body.String())
		}
	}

	approvedUserID := userIDByName(t, srv, "approve_me")
	who, err := srv.auth.LiveIdentity(ctx, approvedUserID)
	if err != nil {
		t.Fatalf("approved LiveIdentity: %v", err)
	}
	if who.Status != auth.StatusApproved || who.Role != auth.RoleUser {
		t.Fatalf("approved account = %+v, want approved user", who)
	}
	denied, err := srv.access.Get(ctx, denyID)
	if err != nil {
		t.Fatalf("Get denied request: %v", err)
	}
	if denied.Status != auth.RequestDenied {
		t.Errorf("denied request status = %q, want denied", denied.Status)
	}
	if _, err := srv.access.Get(ctx, deleteID); !errors.Is(err, auth.ErrNoSuchRequest) {
		t.Errorf("deleted request error = %v, want ErrNoSuchRequest", err)
	}
}

func TestAdminManagesVoiceVisibilityAndOwner(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	_ = signInAs(t, srv, "voice_owner", auth.StatusApproved)
	ownerID := userIDByName(t, srv, "voice_owner")
	voiceID := firstVoiceID(t, srv, cookie)
	id := strconv.FormatInt(voiceID, 10)

	for _, action := range []struct {
		path string
		form url.Values
	}{
		{"/admin/voices/" + id + "/global", url.Values{"is_global": {"false"}}},
		{"/admin/voices/" + id + "/owner", url.Values{"user_id": {strconv.FormatInt(ownerID, 10)}}},
	} {
		rec := adminAction(t, srv, cookie, http.MethodPost, action.path, action.form)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s status = %d, want 200 (body %q)", action.path, rec.Code, rec.Body.String())
		}
	}

	card, err := srv.voices.Get(context.Background(), voiceID)
	if err != nil {
		t.Fatalf("Get voice: %v", err)
	}
	if card.IsGlobal || !card.OwnerID.Valid || card.OwnerID.V != ownerID {
		t.Fatalf("voice after admin actions = %+v, want private owner %d", card, ownerID)
	}

	rec := adminAction(t, srv, cookie, http.MethodPost, "/admin/voices/"+id+"/owner", url.Values{"user_id": {"0"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("unassign status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	card, err = srv.voices.Get(context.Background(), voiceID)
	if err != nil {
		t.Fatalf("Get unassigned voice: %v", err)
	}
	if card.OwnerID.Valid {
		t.Fatalf("OwnerID = %+v, want NULL", card.OwnerID)
	}
}

// The admin page must render every assignee a card has, not just the most
// recently added one — a single "Owner" field on the read model would hide
// every grant but the last, which is the bug this test guards against.
func TestAdminPageShowsAllAssigneesOnAPrivateCard(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	_ = signInAs(t, srv, "first_holder", auth.StatusApproved)
	_ = signInAs(t, srv, "second_holder", auth.StatusApproved)
	firstID := userIDByName(t, srv, "first_holder")
	secondID := userIDByName(t, srv, "second_holder")
	voiceID := firstVoiceID(t, srv, cookie)
	id := strconv.FormatInt(voiceID, 10)

	for _, action := range []struct {
		path string
		form url.Values
	}{
		{"/admin/voices/" + id + "/global", url.Values{"is_global": {"false"}}},
		{"/admin/voices/" + id + "/owner", url.Values{"user_id": {strconv.FormatInt(firstID, 10)}}},
		{"/admin/voices/" + id + "/owner", url.Values{"user_id": {strconv.FormatInt(secondID, 10)}}},
	} {
		rec := adminAction(t, srv, cookie, http.MethodPost, action.path, action.form)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s status = %d, want 200 (body %q)", action.path, rec.Code, rec.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/ status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	// Both accounts appear regardless (the Users table lists every account), so
	// the revoke control's aria-label is what proves the *card* — not just the
	// page — lists both: it names the assignee it would remove.
	body := rec.Body.String()
	for _, want := range []string{"Revoke first_holder", "Revoke second_holder"} {
		if !strings.Contains(body, want) {
			t.Errorf("admin page after two grants on one card is missing %q — only the last assignee rendered", want)
		}
	}

	// Revoking one must remove only that grant, leaving the other assignee.
	rec = adminAction(t, srv, cookie, http.MethodPost, "/admin/voices/"+id+"/unassign",
		url.Values{"user_id": {strconv.FormatInt(firstID, 10)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("unassign status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	body = rec.Body.String()
	if strings.Contains(body, "Revoke first_holder") {
		t.Error("admin page still offers to revoke first_holder after their access was already revoked")
	}
	if !strings.Contains(body, "Revoke second_holder") {
		t.Error("admin page lost second_holder's grant after revoking a different user's access")
	}
}

// Access is a junction table, so revoking one person's grant must leave every
// other grant on the same card standing.
func TestAdminUnassignRevokesOnlyThatUsersAccess(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	_ = signInAs(t, srv, "keeps_access", auth.StatusApproved)
	_ = signInAs(t, srv, "loses_access", auth.StatusApproved)
	keepsID := userIDByName(t, srv, "keeps_access")
	losesID := userIDByName(t, srv, "loses_access")
	voiceID := firstVoiceID(t, srv, cookie)
	id := strconv.FormatInt(voiceID, 10)

	// Stock cards seed global; private is what makes the assignments decisive.
	for _, action := range []struct {
		path string
		form url.Values
	}{
		{"/admin/voices/" + id + "/global", url.Values{"is_global": {"false"}}},
		{"/admin/voices/" + id + "/owner", url.Values{"user_id": {strconv.FormatInt(keepsID, 10)}}},
		{"/admin/voices/" + id + "/owner", url.Values{"user_id": {strconv.FormatInt(losesID, 10)}}},
	} {
		rec := adminAction(t, srv, cookie, http.MethodPost, action.path, action.form)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s status = %d, want 200 (body %q)", action.path, rec.Code, rec.Body.String())
		}
	}

	rec := adminAction(t, srv, cookie, http.MethodPost, "/admin/voices/"+id+"/unassign",
		url.Values{"user_id": {strconv.FormatInt(losesID, 10)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("unassign status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	ctx := context.Background()
	for userID, want := range map[int64]bool{keepsID: true, losesID: false} {
		got, err := srv.voices.IsAccessibleToUser(ctx, voiceID, userID)
		if err != nil {
			t.Fatalf("IsAccessibleToUser(%d): %v", userID, err)
		}
		if got != want {
			t.Errorf("user %d access = %v, want %v", userID, got, want)
		}
	}
}

func TestAdminDeleteUserCleansJobsAudioAndVoiceOwner(t *testing.T) {
	srv := newTestServer(t)
	adminCookie := login(t, srv)
	_ = signInAs(t, srv, "delete_target", auth.StatusApproved)
	targetID := userIDByName(t, srv, "delete_target")
	ctx := context.Background()

	voiceID, err := srv.voices.CreateCloned(ctx, targetID, "Owned clone", ".wav", []byte("reference"))
	if err != nil {
		t.Fatalf("CreateCloned: %v", err)
	}
	jobID, err := srv.jobs.Enqueue(ctx, jobs.NewJob{UserID: targetID, VoiceID: voiceID, Text: "delete with account"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	audioPath := filepath.Join(t.TempDir(), "owned.wav")
	if err := os.WriteFile(audioPath, []byte("RIFFownedWAVE"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := srv.jobs.MarkReady(ctx, jobID, audioPath, "wav", 24000, 0, 0, ""); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}

	rec := adminAction(t, srv, adminCookie, http.MethodDelete,
		"/admin/users/"+strconv.FormatInt(targetID, 10), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete user status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	var count int
	if err := srv.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE id = ?", targetID).Scan(&count); err != nil {
		t.Fatalf("count user: %v", err)
	}
	if count != 0 {
		t.Errorf("user count = %d, want 0", count)
	}
	if err := srv.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE user_id = ?", targetID).Scan(&count); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 0 {
		t.Errorf("job count = %d, want 0", count)
	}
	if err := srv.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM voice_assignments WHERE user_id = ?", targetID).Scan(&count); err != nil {
		t.Fatalf("count voice assignments: %v", err)
	}
	if count != 0 {
		t.Errorf("voice assignment count = %d, want 0", count)
	}
	if _, err := os.Stat(audioPath); !os.IsNotExist(err) {
		t.Errorf("audio file still exists or stat failed unexpectedly: %v", err)
	}
	card, err := srv.voices.Get(ctx, voiceID)
	if err != nil {
		t.Fatalf("voice was deleted: %v", err)
	}
	if card.OwnerID.Valid {
		t.Fatalf("voice owner = %+v, want NULL", card.OwnerID)
	}
}
