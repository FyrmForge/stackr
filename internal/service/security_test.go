package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
)

const adminPW = "correct horse battery"

// secWorld is a world with an admin who can log in.
func secWorld(t *testing.T) (*world, *Principal) {
	t.Helper()
	w := newWorld(t)
	u, err := w.orch.users.CreateAdmin(context.Background(), "adm@x.test", adminPW, "Adm")
	must(t, err)
	return w, &Principal{User: u, KeyID: "k-1", Access: authz.User{ID: u.ID, Active: true, Admin: true}}
}

// events is the log for one action, newest first.
func events(t *testing.T, w *world, action string) []SecurityEvent {
	t.Helper()
	es, err := w.orch.SecurityLog(context.Background(), SecurityFilter{Action: action})
	must(t, err)
	return es
}

// one asserts the action left exactly one row and returns it.
func one(t *testing.T, w *world, action string) SecurityEvent {
	t.Helper()
	es := events(t, w, action)
	if len(es) != 1 {
		t.Fatalf("%s rows = %d, want 1: %+v", action, len(es), es)
	}
	return es[0]
}

// breakAudit drops the table, so the next audit insert fails.
func breakAudit(t *testing.T, w *world) {
	t.Helper()
	_, err := w.st.DB().Exec(`DROP TABLE security_events`)
	must(t, err)
}

func count(t *testing.T, w *world, query string, args ...any) int {
	t.Helper()
	var n int
	must(t, w.st.DB().Get(&n, query, args...))
	return n
}

func TestLoginWritesOneRow(t *testing.T) {
	w, adm := secWorld(t)
	ctx := context.Background()
	meta := RequestMeta{IP: "203.0.113.9", UserAgent: "curl/8"}

	if _, err := w.orch.Login(ctx, "adm@x.test", adminPW, meta); err != nil {
		t.Fatal(err)
	}
	e := one(t, w, securitylog.LoginOK)
	if e.ActorID != adm.User.ID || e.IP != meta.IP || e.UserAgent != meta.UserAgent {
		t.Errorf("login.ok = %+v", e)
	}

	// bad password and unknown email are each one row, and never hold the password
	for _, email := range []string{"adm@x.test", "nobody@x.test"} {
		if _, err := w.orch.Login(ctx, email, "wrong-password", meta); err == nil {
			t.Fatal("bad login passed")
		} else if _, bad := errs.IsInvalid(err); !bad {
			t.Fatalf("bad login = %v, want the usual invalid answer", err)
		}
	}
	bad := events(t, w, securitylog.LoginBad)
	if len(bad) != 2 || bad[0].ActorID != "" || bad[0].TargetID != "nobody@x.test" || bad[1].TargetID != "adm@x.test" {
		t.Errorf("login.bad = %+v", bad)
	}
	for _, e := range bad {
		if strings.Contains(e.Detail, "wrong-password") {
			t.Error("the password leaked into the log")
		}
	}

	must(t, w.orch.LoginRefused(ctx, " Adm@X.test ", meta))
	if e := one(t, w, securitylog.LoginLimited); e.TargetID != "adm@x.test" || e.IP != meta.IP {
		t.Errorf("login.limited = %+v", e)
	}
	if n := len(events(t, w, securitylog.LoginOK)); n != 1 {
		t.Errorf("login.ok rows = %d after the failures", n)
	}
}

func TestFailedAuditRollsLoginBack(t *testing.T) {
	w, _ := secWorld(t)
	breakAudit(t, w)
	if s, err := w.orch.Login(context.Background(), "adm@x.test", adminPW, RequestMeta{}); err == nil || s != nil {
		t.Fatalf("login = %v, %v, want it to fail", s, err)
	}
	if n := count(t, w, `SELECT COUNT(*) FROM sessions`); n != 0 {
		t.Errorf("sessions = %d, want none", n)
	}
}

func TestKeyMintAndRevokeWriteOneRow(t *testing.T) {
	w, adm := secWorld(t)
	ctx := context.Background()

	_, k, err := w.orch.MintKey(ctx, adm, "", "laptop", "", "")
	must(t, err)
	e := one(t, w, securitylog.KeyMint)
	if e.ActorID != adm.User.ID || e.KeyID != "k-1" || e.TargetID != k.ID || !strings.Contains(e.Detail, "name=laptop") {
		t.Errorf("key.mint = %+v", e)
	}
	if strings.Contains(e.Detail, k.TokenHash) {
		t.Error("the key hash is in the log")
	}

	// the CLI browser exchange mints through the same place
	code, err := w.orch.CLICode(ctx, adm, "cli")
	must(t, err)
	_, k2, err := w.orch.ExchangeCLICode(ctx, code)
	must(t, err)
	mints := events(t, w, securitylog.KeyMint)
	if len(mints) != 2 || mints[0].TargetID != k2.ID || mints[0].KeyID != "" {
		t.Errorf("exchange mint = %+v", mints)
	}

	must(t, w.orch.RevokeKey(ctx, adm, k.ID))
	if e := one(t, w, securitylog.KeyRevoke); e.TargetID != k.ID || e.ActorID != adm.User.ID {
		t.Errorf("key.revoke = %+v", e)
	}
}

// An unbound key is minted directly only by an admin on a session or an
// unbound key; anyone else gets one through stackr login, which an org's
// key may not start (B36).
func TestUnboundKeyGates(t *testing.T) {
	w, adm := secWorld(t)
	ctx := context.Background()
	plain := *adm
	plain.Access.Admin = false
	bound := *adm
	bound.Access.Key, bound.Access.KeyOrg = true, "o1"
	for name, p := range map[string]*Principal{"non-admin": &plain, "org-bound admin key": &bound} {
		if _, _, err := w.orch.MintKey(ctx, p, "", "x", "", ""); !errors.Is(err, errs.ErrRefused) {
			t.Errorf("%s mints an unbound key: %v", name, err)
		}
	}
	if _, err := w.orch.CLICode(ctx, &bound, "x"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("org key approves a CLI login: %v", err)
	}
	code, err := w.orch.CLICode(ctx, &plain, "x")
	must(t, err)
	if _, k, err := w.orch.ExchangeCLICode(ctx, code); err != nil || k.OrgID != nil {
		t.Errorf("CLI login key = %+v %v, want no org", k, err)
	}
}

func TestFailedAuditRollsKeysBack(t *testing.T) {
	w, adm := secWorld(t)
	ctx := context.Background()
	_, k, err := w.orch.MintKey(ctx, adm, "", "keep", "", "")
	must(t, err)
	breakAudit(t, w)

	if _, _, err := w.orch.MintKey(ctx, adm, "", "ghost", "", ""); err == nil {
		t.Fatal("mint passed with a broken log")
	}
	if n := count(t, w, `SELECT COUNT(*) FROM api_keys`); n != 1 {
		t.Errorf("keys = %d, want only the first", n)
	}
	if err := w.orch.RevokeKey(ctx, adm, k.ID); err == nil {
		t.Fatal("revoke passed with a broken log")
	}
	if n := count(t, w, `SELECT COUNT(*) FROM api_keys WHERE id = ?`, k.ID); n != 1 {
		t.Error("the key was revoked although its audit row failed")
	}
}

// parked deploys a tile that wants a host mount, so a grant is waiting.
func parked(t *testing.T, w *world) Job {
	t.Helper()
	ctx := context.Background()
	w.fake.RunID = "c1"
	w.fake.Details = map[string]docker.Detail{"c1": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}}}
	tl := w.tile(t, "mon", false)
	_, _, err := w.orch.UpdateTile(ctx, tl.ID, func(t *Tile) error {
		t.Volumes = "host:/var/run/docker.sock:/var/run/docker.sock:ro"
		return nil
	})
	must(t, err)
	j, err := w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	if j = w.waiting(t, j.ID); j.State != job.Waiting {
		t.Fatalf("deploy = %s %q, want parked", j.State, j.Error)
	}
	return j
}

func TestGrantApproveAndRevokeWriteOneRow(t *testing.T) {
	w, adm := secWorld(t)
	ctx := context.Background()
	parked(t, w)
	g, err := w.orch.HostGrant(ctx, w.stack)
	must(t, err)

	_, err = w.orch.ApproveHostGrant(ctx, w.stack, adm.User.ID, g.Pending, nil)
	must(t, err)
	e := one(t, w, securitylog.GrantApprove)
	if e.ActorID != adm.User.ID || e.TargetKind != "stack" || e.TargetID != w.stack || !strings.Contains(e.Detail, "mon host:/var/run/docker.sock") {
		t.Errorf("grant.approve = %+v", e)
	}

	must(t, w.orch.RevokeHostGrant(ctx, w.stack, "", adm.User.ID))
	if e := one(t, w, securitylog.GrantRevoke); e.ActorID != adm.User.ID || e.TargetID != w.stack {
		t.Errorf("grant.revoke = %+v", e)
	}
}

func TestFailedAuditRollsGrantsBack(t *testing.T) {
	w, adm := secWorld(t)
	ctx := context.Background()
	parked(t, w)
	g, err := w.orch.HostGrant(ctx, w.stack)
	must(t, err)
	breakAudit(t, w)

	if _, err := w.orch.ApproveHostGrant(ctx, w.stack, adm.User.ID, g.Pending, nil); err == nil {
		t.Fatal("approve passed with a broken log")
	}
	if g, _ := w.orch.HostGrant(ctx, w.stack); g.Granted {
		t.Error("the grant stayed although its audit row failed")
	}

	// a grant that exists survives a revoke whose audit fails
	_, err = w.st.DB().Exec(`CREATE TABLE security_events (id TEXT PRIMARY KEY, at DATETIME NOT NULL, action TEXT NOT NULL,
		actor_id TEXT NOT NULL DEFAULT '', key_id TEXT NOT NULL DEFAULT '', target_kind TEXT NOT NULL DEFAULT '',
		target_id TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '', user_agent TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '')`)
	must(t, err)
	_, err = w.orch.ApproveHostGrant(ctx, w.stack, adm.User.ID, g.Pending, nil)
	must(t, err)
	breakAudit(t, w)
	if err := w.orch.RevokeHostGrant(ctx, w.stack, "", adm.User.ID); err == nil {
		t.Fatal("revoke passed with a broken log")
	}
	if g, _ := w.orch.HostGrant(ctx, w.stack); !g.Granted {
		t.Error("the grant was revoked although its audit row failed")
	}
}

func TestRevealSecretsWritesOneRow(t *testing.T) {
	w, adm := secWorld(t)
	ctx := context.Background()
	scope := ParamScope{Kind: "org", ID: w.org}
	_, err := w.orch.SetParams(ctx, scope, []ParamEntry{
		{Collection: "app", Name: "token", Kind: "secret", Value: "s3cret"},
		{Collection: "app", Name: "mode", Kind: "param", Value: "fast"},
	})
	must(t, err)
	if len(events(t, w, securitylog.SecretReveal)) != 0 {
		t.Fatal("setting a secret counted as a reveal")
	}

	ps, err := w.orch.RevealSecrets(ctx, adm, scope)
	must(t, err)
	if len(ps) != 2 {
		t.Fatalf("params = %d", len(ps))
	}
	e := one(t, w, securitylog.SecretReveal)
	if e.ActorID != adm.User.ID || e.KeyID != "k-1" || e.TargetKind != "org" || e.TargetID != w.org || e.Detail != "app.token" {
		t.Errorf("secrets.reveal = %+v", e)
	}
	if strings.Contains(e.Detail, "s3cret") {
		t.Error("the secret value is in the log")
	}

	breakAudit(t, w)
	if ps, err := w.orch.RevealSecrets(ctx, adm, scope); err == nil || ps != nil {
		t.Errorf("reveal with a broken log = %v, %v, want no values", ps, err)
	}
}

func TestSecurityLogFiltersAndAppendOnly(t *testing.T) {
	w, adm := secWorld(t)
	ctx := context.Background()
	before := time.Now().Add(-time.Minute)
	_, err := w.orch.Login(ctx, "adm@x.test", adminPW, RequestMeta{})
	must(t, err)
	_, _, err = w.orch.MintKey(ctx, adm, "", "laptop", "", "")
	must(t, err)
	_, _ = w.orch.Login(ctx, "adm@x.test", "nope-nope", RequestMeta{})

	list := func(f SecurityFilter) []SecurityEvent {
		es, err := w.orch.SecurityLog(ctx, f)
		must(t, err)
		return es
	}
	// 5: node.bootstrap and the controller cert.issue rows from boot, plus the three above.
	if n := len(list(SecurityFilter{})); n != 5 {
		t.Errorf("all = %d, want 5", n)
	}
	if es := list(SecurityFilter{}); es[0].Action != securitylog.LoginBad {
		t.Errorf("newest = %s, want login.bad", es[0].Action)
	}
	if n := len(list(SecurityFilter{ActorID: adm.User.ID})); n != 2 {
		t.Errorf("by actor = %d, want 2 (the bad login has none)", n)
	}
	if n := len(list(SecurityFilter{TargetKind: "email", TargetID: "adm@x.test"})); n != 1 {
		t.Errorf("by target = %d, want 1", n)
	}
	if n := len(list(SecurityFilter{Since: before, Until: time.Now().Add(time.Minute)})); n != 5 {
		t.Errorf("in window = %d, want 5", n)
	}
	if n := len(list(SecurityFilter{Since: time.Now().Add(time.Minute)})); n != 0 {
		t.Errorf("future window = %d, want 0", n)
	}
	if n := len(list(SecurityFilter{Limit: 1})); n != 1 {
		t.Errorf("limit = %d, want 1", n)
	}

	if _, err := w.st.DB().Exec(`UPDATE security_events SET detail = 'x'`); err == nil {
		t.Error("an event was updated")
	}
	if _, err := w.st.DB().Exec(`DELETE FROM security_events`); err == nil {
		t.Error("an event was deleted")
	}
}
