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
	if again := b.do(t, "GET", "/acme/-/drawer?tab=people", nil, true).Body.String(); strings.Contains(again, "not shown again") {
		t.Errorf("the key shows again:\n%s", again)
	}
	body = b.do(t, "POST", "/acme/-/drawer/invite", url.Values{"email": {"new@acme.test"}, "role": {"owner"}}, true).Body.String()
	if !strings.Contains(body, "Invite link: ") || !strings.Contains(body, `data-copy="/invite/`) {
		t.Errorf("invite:\n%s", body)
	}
	// the link sits right after the Add people form that posted it
	form := strings.Index(body, `hx-post="/acme/-/drawer/invite"`)
	end := form + strings.Index(body[form:], "</form>")
	if note := strings.Index(body, "Invite link: "); form < 0 || note < end || !strings.Contains(body[end:note], "data-acted") {
		t.Errorf("invite link at %d, form ends at %d\n%s", note, end, body)
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

// A literal SMB password is refused on the password field alone: one error
// span, no other field flagged.
func TestOrgShareLiteralPasswordErrorsOnce(t *testing.T) {
	b := newBrowser(t, "owner")
	body := b.do(t, "POST", "/acme/-/drawer/shares", url.Values{
		"slug": {"qasmb"}, "kind": {"smb"}, "source": {"//203.0.113.1/playground"},
		"user": {"stackr"}, "password": {"literal"},
	}, true).Body.String()
	if n := strings.Count(body, `id="error-password"`); n != 1 {
		t.Errorf("error-password spans = %d:\n%s", n, body)
	}
	if n := strings.Count(body, `data-has-error="true"`); n != 1 {
		t.Errorf("flagged fields = %d:\n%s", n, body)
	}
}
