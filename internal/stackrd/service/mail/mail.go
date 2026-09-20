// Package mail owns the messages stackr sends. Today that is one: the org
// invite. It lives here rather than in the web handler because the API's
// invite path had no mailer at all — an invite created over the API or the CLI
// created the row and silently never told anyone.
package mail

import (
	"context"
	"html"
	"log/slog"
	"strings"

	hamremail "github.com/FyrmForge/hamr/pkg/email"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/mail"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Service sends stackr's outgoing mail.
//
// baseOrigin is the panel's own public origin, taken at construction. The web
// handler used to build the invite link from the request (scheme + Host),
// which is why the link could not be built anywhere without an echo.Context —
// and therefore not from the API. BASE_URL is already parsed at boot and is
// the address the invite has to be openable at anyway.
type Service struct {
	mailer     *mail.Mailer
	baseOrigin string
}

// New builds the service. A nil mailer means no provider is configured: every
// send is skipped and the caller falls back to copy-the-link, which is the
// only channel a self-hosted box always has.
func New(m *mail.Mailer, baseOrigin string) *Service {
	return &Service{mailer: m, baseOrigin: strings.TrimRight(baseOrigin, "/")}
}

// Enabled reports whether this install can send mail at all. The panel words
// its flash differently when it cannot.
func (s *Service) Enabled() bool { return s != nil && s.mailer.Enabled() }

// InviteLink is the URL an invite is accepted at.
func (s *Service) InviteLink(token string) string {
	if s == nil {
		return "/invite/" + token
	}
	return s.baseOrigin + "/invite/" + token
}

// SendInvite mails the invite link to the address it is bound to and reports
// whether the send failed.
//
// A failure is not fatal and is deliberately not persisted: the row exists and
// the link is on the page, so the invite still works by hand. The caller shows
// the bounce on that one request (repo.Invite.MailFailed is `db:"-"` for
// exactly this).
func (s *Service) SendInvite(ctx context.Context, org *repo.Org, inv *repo.Invite) (failed bool) {
	if s == nil || inv == nil || org == nil || inv.Email == "" || !s.Enabled() {
		return false
	}
	link := s.InviteLink(inv.ID)
	expires := inv.ExpiresAt.Format("Jan 2 2006")
	err := s.mailer.Send(ctx, hamremail.Addr("", inv.Email),
		"You have been invited to "+org.Name+" on stackr",
		"You have been invited to join "+org.Name+" as "+inv.Role+
			".\n\nOpen this link to accept:\n"+link+
			"\n\nThe link works once and expires on "+expires+".",
		`<p>You have been invited to join <strong>`+html.EscapeString(org.Name)+`</strong> as `+inv.Role+`.</p>`+
			`<p><a href="`+html.EscapeString(link)+`">Accept the invitation</a></p>`+
			`<p>The link works once and expires on `+expires+`.</p>`,
	)
	if err != nil {
		slog.Warn("invite mail not sent", "to", inv.Email, "org", org.ID, "error", err)
		return true
	}
	return false
}
