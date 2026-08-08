package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sruckh/timbre/internal/auth"
)

// shellPages is every route that renders the AppShell, and therefore the rail
// the Admin link and its badge live in.
var shellPages = []string{"/", "/voices", "/queue"}

// adminNavLink returns the rendered <a> for the Admin rail entry, so an
// assertion about the badge cannot accidentally match a badge belonging to a
// job row or a voice card elsewhere on the page.
func adminNavLink(t *testing.T, body string) (string, bool) {
	t.Helper()
	start := strings.Index(body, `href="/admin/"`)
	if start < 0 {
		return "", false
	}
	end := strings.Index(body[start:], "</a>")
	if end < 0 {
		t.Fatalf("Admin nav link is never closed: %q", body[start:])
	}
	return body[start : start+end], true
}

func TestAdminNavBadgeCountsPendingRequests(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)
	ctx := context.Background()

	for _, applicant := range []string{"first_applicant", "second_applicant", "third_applicant"} {
		if _, err := srv.access.Create(ctx, applicant, "", applicantPassword); err != nil {
			t.Fatalf("Create request for %s: %v", applicant, err)
		}
	}
	// A decided request is not work waiting, so it must not reach the badge.
	decidedID, err := srv.access.Create(ctx, "decided_applicant", "", applicantPassword)
	if err != nil {
		t.Fatalf("Create decided request: %v", err)
	}
	if err := srv.access.Deny(ctx, decidedID, userIDByName(t, srv, "admin")); err != nil {
		t.Fatalf("Deny request: %v", err)
	}

	for _, path := range append(shellPages, "/admin/") {
		rec := do(t, srv, http.MethodGet, path, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		link, ok := adminNavLink(t, rec.Body.String())
		if !ok {
			t.Fatalf("GET %s: admin sees no Admin nav link", path)
		}
		if !strings.Contains(link, "(3)") {
			t.Errorf("GET %s: Admin link %q has no pending count of 3", path, link)
		}
		if !strings.Contains(link, "badge") {
			t.Errorf("GET %s: Admin link %q renders the count without a badge", path, link)
		}
	}
}

func TestAdminNavBadgeHiddenWithoutPendingRequests(t *testing.T) {
	srv := newTestServer(t)
	cookie := login(t, srv)

	for _, path := range append(shellPages, "/admin/") {
		rec := do(t, srv, http.MethodGet, path, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		link, ok := adminNavLink(t, rec.Body.String())
		if !ok {
			t.Fatalf("GET %s: admin sees no Admin nav link", path)
		}
		if strings.Contains(link, "badge") {
			t.Errorf("GET %s: Admin link %q shows a badge with nothing pending", path, link)
		}
	}
}

// The badge is only ever half the question: a link to a surface that answers
// 403 is not navigation, so a non-admin sees no Admin entry at all — pending
// requests or not.
func TestAdminNavHiddenFromNonAdmin(t *testing.T) {
	srv := newTestServer(t)
	cookie := signInAs(t, srv, "ordinary_user", auth.StatusApproved)
	if _, err := srv.access.Create(context.Background(), "waiting_applicant", "", applicantPassword); err != nil {
		t.Fatalf("Create access request: %v", err)
	}

	for _, path := range shellPages {
		rec := do(t, srv, http.MethodGet, path, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		if link, ok := adminNavLink(t, rec.Body.String()); ok {
			t.Errorf("GET %s: non-admin sees the Admin nav link %q", path, link)
		}
	}
}
