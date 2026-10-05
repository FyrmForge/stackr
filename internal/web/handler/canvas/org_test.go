package canvas_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A minted key shows once in the keys tab with a Copy, not in a note; an
// invite answers with its link and the row can copy it.
func TestOrgMintAndInvite(t *testing.T) {
	b := newBrowser(t, "owner")
	body := b.do(t, "POST", "/acme/-/drawer/keys", url.Values{"key_name": {"laptop"}}, true).Body.String()
	if !strings.Contains(body, "It is not shown again.") || !strings.Contains(body, `data-copy="`) ||
		strings.Contains(body, `class="banner" role="status"`) {
		t.Errorf("mint:\n%s", body)
	}
	if again := b.do(t, "GET", "/acme/-/drawer?tab=keys", nil, true).Body.String(); strings.Contains(again, "not shown again") {
		t.Errorf("the key shows again:\n%s", again)
	}
	body = b.do(t, "POST", "/acme/-/drawer/invite", url.Values{"email": {"new@acme.test"}, "role": {"owner"}}, true).Body.String()
	if !strings.Contains(body, "Invite link: ") || !strings.Contains(body, `data-copy="/invite/`) {
		t.Errorf("invite:\n%s", body)
	}
}

// Revoking an invite drops its row and kills the link.
func TestOrgRevokeInvite(t *testing.T) {
	b := newBrowser(t, "owner")
	body := b.do(t, "POST", "/acme/-/drawer/invite", url.Values{"email": {"new@acme.test"}, "role": {"owner"}}, true).Body.String()
	_, rest, _ := strings.Cut(body, `data-copy="/invite/`)
	token, _, _ := strings.Cut(rest, `"`)
	body = b.do(t, "POST", "/acme/-/drawer/invites/"+token+"/revoke", nil, true).Body.String()
	if strings.Contains(body, "new@acme.test") || !strings.Contains(body, "Invite revoked.") {
		t.Errorf("revoke:\n%s", body)
	}
	if got := b.do(t, "GET", "/invite/"+token, nil, false).Code; got != http.StatusNotFound {
		t.Errorf("revoked link = %d, want 404", got)
	}
}
