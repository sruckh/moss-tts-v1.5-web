package web

import (
	"context"
	"strconv"
)

// NavState is what the rail in AppShell needs to know about the person looking
// at it: whether the Admin section is theirs at all, and how many access
// requests are still waiting for a decision.
//
// It travels in the request context rather than as a parameter on every page
// component. The nav belongs to the chrome, not to any one page, and threading
// it through Studio, VoiceLibrary, QueuePage and AdminPage would make four
// signatures carry a value none of them use.
type NavState struct {
	IsAdmin         bool
	PendingRequests int
}

type navContextKey struct{}

// WithNav returns ctx carrying nav, for a handler about to render a full page.
func WithNav(ctx context.Context, nav NavState) context.Context {
	return context.WithValue(ctx, navContextKey{}, nav)
}

// NavFromContext reads the nav state a handler stashed with WithNav. The zero
// value — no admin link, no badge — is the answer for every context without
// one, so a fragment rendered outside a page render is never a special case.
func NavFromContext(ctx context.Context) NavState {
	nav, _ := ctx.Value(navContextKey{}).(NavState)
	return nav
}

// pendingRequestsLabel spells out what the bare number in the nav badge counts,
// for the tooltip and for a screen reader.
func pendingRequestsLabel(count int) string {
	if count == 1 {
		return "1 access request awaiting review"
	}
	return strconv.Itoa(count) + " access requests awaiting review"
}
