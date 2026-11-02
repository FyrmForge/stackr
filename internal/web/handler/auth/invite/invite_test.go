package invite_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// Opening an invite signed in as someone else says whose it is, and
// accepting it says the same instead of "no longer valid".
func TestInviteForAnotherAccount(t *testing.T) {
	s := webtest.New(t)
	owner := s.User(t, "boss@acme.test", false)
	i, err := s.Orch.Invite(t.Context(), s.Org, "new@acme.test", "member", owner)
	if err != nil {
		t.Fatal(err)
	}
	rec := s.Do(t, "GET", "/invite/"+i.ID, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "This invite is for new@acme.test") || !strings.Contains(rec.Body.String(), "owner@acme.test") {
		t.Fatalf("page = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/invite/"+i.ID, nil)
	if !strings.Contains(rec.Body.String(), "This invite is for new@acme.test") || strings.Contains(rec.Body.String(), "no longer valid") {
		t.Errorf("accept = %d\n%s", rec.Code, rec.Body)
	}
}
