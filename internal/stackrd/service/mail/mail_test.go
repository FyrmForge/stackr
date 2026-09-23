package mail

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The link is the whole point of the service: it used to be built from the
// request (scheme + Host), which is why no non-HTTP caller could send an
// invite at all. It comes from BASE_URL now, and a trailing slash there must
// not produce a double slash in a link people paste.
func TestInviteLink(t *testing.T) {
	for _, base := range []string{"https://panel.example.com", "https://panel.example.com/"} {
		got := New(nil, base).InviteLink("tok123")
		if want := "https://panel.example.com/invite/tok123"; got != want {
			t.Errorf("base %q: got %q, want %q", base, got, want)
		}
	}
}

// No provider configured must be a quiet skip, not a failure and not a
// reported bounce: the row exists and the link is in the response, which is
// the fallback every self-hosted box has.
func TestSendInviteWithoutProvider(t *testing.T) {
	s := New(nil, "https://panel.example.com")
	if s.Enabled() {
		t.Fatal("no mailer should not report enabled")
	}
	inv := &repo.Invite{ID: "tok", Email: "someone@example.com", Role: "member",
		ExpiresAt: time.Now().Add(time.Hour)}
	if failed := s.SendInvite(context.Background(), &repo.Org{Name: "QA"}, inv); failed {
		t.Error("a server that sends no mail must not report a bounce")
	}

	// And a nil service, which is what tests and the API router run with.
	var none *Service
	if failed := none.SendInvite(context.Background(), &repo.Org{Name: "QA"}, inv); failed {
		t.Error("nil service must be a no-op")
	}
}
