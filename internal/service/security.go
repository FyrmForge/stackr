package service

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/FyrmForge/hamr/pkg/auth"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// SecurityEvent is one row of the security audit log.
type SecurityEvent = store.SecurityEvent

// SecurityFilter narrows SecurityLog; a zero field matches everything.
type SecurityFilter = store.SecurityFilter

// RequestMeta is what the HTTP layer knows about the caller of a login.
type RequestMeta struct{ IP, UserAgent string }

// agent is the user agent, clipped for the audit log.
func (m RequestMeta) agent() string { return strings.ToValidUTF8(clipBytes(m.UserAgent, 512), "") }

// audit is the log bound to a transaction: the act and its row commit or
// roll back together.
func audit(tx store.Tx) *securitylog.Leaf { return securitylog.New(tx.SecurityEvents) }

// noted records what a request just did to what, after it succeeded, and
// passes err through: a failed act writes nothing. The actor is the user and
// key in ctx ("" for the system).
// ponytail: appended after the act, not in its transaction, so a failed append
// is logged and the act stands; wrap the verb in store.Tx where the log must
// be as certain as the act.
func (o *Orchestrator) noted(ctx context.Context, err error, action, kind, id, detail string) error {
	if err != nil {
		return err
	}
	user, key := store.ActorFrom(ctx)
	if aerr := securitylog.New(o.store.SecurityEvents).Append(ctx, securitylog.Event{
		Action: action, ActorID: user, KeyID: key, TargetKind: kind, TargetID: id, Detail: detail,
	}); aerr != nil {
		slog.Error("audit", "action", action, "error", aerr.Error())
	}
	return nil
}

// SecurityLog (server admin) lists audit events, newest first.
func (o *Orchestrator) SecurityLog(ctx context.Context, f SecurityFilter) ([]SecurityEvent, error) {
	return securitylog.New(o.store.SecurityEvents).List(ctx, f)
}

// LoginRefused records a login the rate limit turned away; it counts as a
// failed login. The caller has not checked the password.
func (o *Orchestrator) LoginRefused(ctx context.Context, email string, meta RequestMeta) error {
	return o.auditLogin(ctx, securitylog.LoginLimited, email, meta)
}

func (o *Orchestrator) auditLogin(ctx context.Context, action, email string, meta RequestMeta) error {
	email = strings.ToValidUTF8(clipBytes(strings.ToLower(strings.TrimSpace(email)), 254), "")
	return o.store.Tx(ctx, func(tx store.Tx) error {
		return audit(tx).Append(ctx, securitylog.Event{
			Action: action, TargetKind: "email", TargetID: email,
			IP: meta.IP, UserAgent: meta.agent(),
		})
	})
}

// openSession opens a session on the transaction, so a failed audit row
// leaves no session behind.
func (o *Orchestrator) openSession(ctx context.Context, tx store.Tx, userID string) (*auth.Session, error) {
	return auth.NewSessionManager(tx.Sessions,
		auth.WithCookieSecure(o.cfg.CookieSecure),
		auth.WithCookieDomain(o.cfg.CookieDomain),
	).CreateSession(ctx, userID, nil)
}

// RevealSecrets lists a scope's params with secret values and records the
// reveal by the principal. The values are not returned when the audit row
// cannot be written.
func (o *Orchestrator) RevealSecrets(ctx context.Context, by *Principal, s ParamScope) ([]Param, error) {
	ps, err := o.Params(ctx, s, true)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range ps {
		if p.Kind == "secret" {
			names = append(names, p.Collection+"."+p.Name)
		}
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.SecretReveal, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: s.Kind, TargetID: s.ID, Detail: strings.Join(names, "\n"),
		})
	})
	if err != nil {
		return nil, err
	}
	return ps, nil
}

// clipBytes cuts s to at most n bytes on a rune boundary.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
