package connector_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

const user = "u1"

type fakeApp struct {
	state        string
	notInstalled bool
}

func (f *fakeApp) Manifest(id, _, state string) (string, string, error) {
	f.state = state
	return "https://github.com/settings/apps/new?state=" + state, "{}", nil
}

func (f *fakeApp) ConvertManifest(context.Context, string) (githubapp.App, error) {
	return githubapp.App{
		ID:            7,
		Slug:          "stackr-x",
		PEM:           "pem",
		WebhookSecret: "whsec",
	}, nil
}

func (f *fakeApp) Token(_ context.Context, key string, app githubapp.App) (string, error) {
	if f.notInstalled {
		return "", githubapp.ErrNotInstalled
	}
	return "tok-" + app.Slug, nil
}

func (f *fakeApp) Repos(_ context.Context, token string) ([]githubapp.Repo, error) {
	return []githubapp.Repo{{FullName: "acme/api", DefaultBranch: "main"}}, nil
}

func seedOrg(t *testing.T, st *store.Store) string {
	t.Helper()
	id := uuid.NewString()
	if err := st.Orgs.Create(ctx, store.Org{
		ID:        id,
		Name:      id,
		Slug:      id[:8],
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func connect(t *testing.T, l *connector.Leaf, f *fakeApp, org string) store.Connector {
	t.Helper()
	if _, _, _, err := l.Begin(ctx, org, user, ""); err != nil {
		t.Fatal(err)
	}
	c, err := l.Complete(ctx, user, f.state, "code")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHandshake(t *testing.T) {
	st := servicetest.Store(t)
	f := &fakeApp{}
	l := connector.New(st.Connectors, f)
	org := seedOrg(t, st)

	c, action, _, err := l.Begin(ctx, org, user, "")
	if err != nil || !strings.Contains(action, c.ID+".") || connector.Connected(c) {
		t.Fatalf("begin = %+v %s %v", c, action, err)
	}
	if u, err := l.InstallURL(ctx, org, c.ID); err != nil || u != "" {
		t.Errorf("pending connector install url = %q %v", u, err)
	}
	if _, err := l.For(ctx, org, "https://github.com/acme/api"); !errors.As(err, &connector.NoConnector{}) {
		t.Errorf("pending connector resolved: %v", err)
	}
	if _, secret, _ := l.WebhookSecret(ctx, c.ID); secret != "" {
		t.Error("pending connector has a webhook secret")
	}
	if _, err := l.Complete(ctx, user, c.ID+".wrong", "code"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("wrong nonce = %v", err)
	}
	if _, err := l.Complete(ctx, "u2", f.state, "code"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("another user's callback = %v", err)
	}
	c, err = l.Complete(ctx, user, f.state, "code")
	if err != nil || !connector.Connected(c) || c.Name != "GitHub · stackr-x" {
		t.Fatalf("complete = %+v %v", c, err)
	}
	if u, err := l.InstallURL(ctx, org, c.ID); err != nil || u != "https://github.com/apps/stackr-x/installations/new" {
		t.Errorf("install url = %q %v", u, err)
	}
	if _, err := l.InstallURL(ctx, "other-org", c.ID); err == nil {
		t.Error("another org read the install url")
	}
	f.notInstalled = true
	if rs, err := l.Repos(ctx, org, c.ID); err != nil || len(rs) != 0 {
		t.Errorf("not installed repos = %v %v", rs, err)
	}
	f.notInstalled = false
	if rs, err := l.Repos(ctx, org, c.ID); err != nil || len(rs) != 1 || rs[0].FullName != "acme/api" {
		t.Errorf("installed repos = %v %v", rs, err)
	}
	if _, err := l.Complete(ctx, user, f.state, "code"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("replayed callback = %v", err)
	}
	if _, _, _, err := l.Begin(ctx, org, user, ""); err == nil {
		t.Error("second github connector in one org")
	}

	got, err := l.For(ctx, org, "https://github.com/acme/api")
	if err != nil || got.ID != c.ID {
		t.Fatalf("For = %+v %v", got, err)
	}
	env, err := l.CloneEnv(ctx, got, "https://github.com/acme/api")
	if err != nil || len(env) != 3 {
		t.Errorf("clone env = %v %v", env, err)
	}
	if _, secret, _ := l.WebhookSecret(ctx, c.ID); secret != "whsec" {
		t.Errorf("secret = %q", secret)
	}
	if _, err := l.For(ctx, org, "https://gitlab.com/acme/api"); !errors.As(err, &connector.NoConnector{}) {
		t.Errorf("gitlab = %v", err)
	}
	list, _ := l.List(ctx, org)
	if len(list) != 1 || list[0].Config != "" {
		t.Errorf("list = %+v", list)
	}
}

// A connector id from another org (a stack file can name one) is not found:
// it would clone that org's private repos with that org's token.
func TestConnectorScope(t *testing.T) {
	st := servicetest.Store(t)
	f := &fakeApp{}
	l := connector.New(st.Connectors, f)
	mine, theirs := seedOrg(t, st), seedOrg(t, st)
	c := connect(t, l, f, theirs)

	if _, err := l.Get(ctx, mine, c.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("cross-org get = %v", err)
	}
	if _, err := l.For(ctx, mine, "https://github.com/their/private"); !errors.As(err, &connector.NoConnector{}) {
		t.Errorf("cross-org resolve = %v", err)
	}
	if err := l.Delete(ctx, mine, c.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("cross-org delete = %v", err)
	}
	if _, err := l.Rename(ctx, mine, c.ID, "x"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("cross-org rename = %v", err)
	}
}

func connectServer(t *testing.T, l *connector.Leaf, f *fakeApp) store.Connector {
	t.Helper()
	if _, _, _, err := l.BeginServer(ctx, user, ""); err != nil {
		t.Fatal(err)
	}
	c, err := l.Complete(ctx, user, f.state, "code")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// An org reads the server connectors shared with it and nothing else, and
// never writes through one: delete, rename and Get stay org-only.
func TestServerConnectorReadsWidenWritesDoNot(t *testing.T) {
	st := servicetest.Store(t)
	f := &fakeApp{}
	l := connector.New(st.Connectors, f)
	acme, other := seedOrg(t, st), seedOrg(t, st)
	srv := connectServer(t, l, f)

	if cs, _ := l.ListConnected(ctx, acme); len(cs) != 0 {
		t.Fatalf("unshared connector listed: %+v", cs)
	}
	if _, err := l.Usable(ctx, acme, srv.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unshared usable = %v", err)
	}
	if _, err := l.SetShares(ctx, srv.ID, []string{acme}, false); err != nil {
		t.Fatal(err)
	}
	cs, err := l.ListConnected(ctx, acme)
	if err != nil || len(cs) != 1 || cs[0].ID != srv.ID || cs[0].Config != "" {
		t.Fatalf("shared ListConnected = %+v %v", cs, err)
	}
	if cs, _ := l.ListConnected(ctx, other); len(cs) != 0 {
		t.Errorf("an unnamed org sees it: %+v", cs)
	}
	if cs, _ := l.List(ctx, acme); len(cs) != 1 || !cs[0].Shared || cs[0].Config != "" {
		t.Errorf("List lacks the shared connector, marked and stripped: %+v", cs)
	}
	if _, err := l.Usable(ctx, acme, srv.ID); err != nil {
		t.Errorf("shared usable = %v", err)
	}
	if rs, err := l.Repos(ctx, acme, srv.ID); err != nil || len(rs) != 1 {
		t.Errorf("shared repos = %v %v", rs, err)
	}
	if _, err := l.Get(ctx, acme, srv.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("org Get reached a server connector: %v", err)
	}
	if _, err := l.Rename(ctx, acme, srv.ID, "mine"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("org rename of a server connector = %v", err)
	}
	if err := l.Delete(ctx, acme, srv.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("org delete of a server connector = %v", err)
	}
	if _, err := l.InstallURL(ctx, acme, srv.ID); err == nil {
		t.Error("org read a server connector's install url")
	}
	// all orgs
	if _, err := l.SetShares(ctx, srv.ID, []string{acme}, true); err != nil {
		t.Fatal(err)
	}
	if cs, _ := l.ListConnected(ctx, other); len(cs) != 1 {
		t.Errorf("share-all not seen by another org: %+v", cs)
	}
	if ids, _ := l.SharedOrgs(ctx, srv.ID); len(ids) != 0 {
		t.Errorf("share-all keeps named rows: %v", ids)
	}
	// none again
	if _, err := l.SetShares(ctx, srv.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if cs, _ := l.ListConnected(ctx, other); len(cs) != 0 {
		t.Errorf("revoked share still listed: %+v", cs)
	}
}

// For: the named connector wins, then the org's own for the host, then
// exactly one shared server connector; several shared is a refusal that
// says to name one.
func TestResolveHost(t *testing.T) {
	st := servicetest.Store(t)
	f := &fakeApp{}
	l := connector.New(st.Connectors, f)
	acme := seedOrg(t, st)
	url := "https://github.com/acme/api"

	s1 := connectServer(t, l, f)
	if _, err := l.For(ctx, acme, url); !errors.As(err, &connector.NoConnector{}) {
		t.Errorf("unshared = %v", err)
	}
	_, _ = l.SetShares(ctx, s1.ID, nil, true)
	if c, err := l.For(ctx, acme, url); err != nil || c.ID != s1.ID {
		t.Fatalf("one shared = %+v %v", c, err)
	} else if env, err := l.CloneEnv(ctx, c, url); err != nil || len(env) != 3 {
		t.Errorf("shared clone env = %v %v", env, err)
	}

	_, _ = l.RenameServer(ctx, s1.ID, "one") // the fake app names every App alike
	s2 := connectServer(t, l, f)
	_, _ = l.SetShares(ctx, s2.ID, []string{acme}, false)
	if _, err := l.For(ctx, acme, url); !errors.As(err, &connector.Ambiguous{}) ||
		!strings.Contains(err.Error(), "name the connector to use") {
		t.Errorf("two shared = %v", err)
	}
	cs, _ := l.ListConnected(ctx, acme)
	if c, err := connector.Resolve(cs, s2.ID, "github.com"); err != nil || c.ID != s2.ID {
		t.Errorf("named = %+v %v", c, err)
	}
	if _, err := connector.Resolve(cs, "nope", "github.com"); !errors.As(err, &connector.NoConnector{}) {
		t.Errorf("unknown named = %v", err)
	}

	own := connect(t, l, f, acme)
	if c, err := l.For(ctx, acme, url); err != nil || c.ID != own.ID {
		t.Errorf("own wins = %+v %v", c, err)
	}
	cs, _ = l.ListConnected(ctx, acme)
	if len(cs) != 3 || cs[0].ID != own.ID {
		t.Errorf("own first = %+v", cs)
	}
}

// Server connector names are unique (the server file names them); a pending
// one does not take the name a second pending one needs.
func TestServerConnectorNames(t *testing.T) {
	st := servicetest.Store(t)
	f := &fakeApp{}
	l := connector.New(st.Connectors, f)

	for range 2 {
		if _, _, _, err := l.BeginServer(ctx, user, ""); err != nil {
			t.Fatalf("second pending server connector: %v", err)
		}
	}
	a := connectServer(t, l, f)
	if _, err := l.RenameServer(ctx, a.ID, "  "); !errors.As(err, &errs.Invalid{}) {
		t.Errorf("blank name = %v", err)
	}
	b, _, _, _ := l.BeginServer(ctx, user, "")
	if _, err := l.RenameServer(ctx, b.ID, a.Name); !errors.As(err, &errs.Conflict{}) {
		t.Errorf("duplicate name = %v", err)
	}
	if c, err := l.RenameServer(ctx, a.ID, "main"); err != nil || c.Name != "main" || c.Config != "" {
		t.Errorf("rename = %+v %v", c, err)
	}
	// an org may reuse a server connector's name
	org := seedOrg(t, st)
	oc := connect(t, l, f, org)
	if c, err := l.Rename(ctx, org, oc.ID, "main"); err != nil || c.Name != "main" {
		t.Errorf("org reuses a server name = %+v %v", c, err)
	}
	if _, err := l.RenameServer(ctx, oc.ID, "x"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("server rename of an org's connector = %v", err)
	}
	if err := l.DeleteServer(ctx, oc.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("server delete of an org's connector = %v", err)
	}
	if err := l.DeleteServer(ctx, a.ID); err != nil {
		t.Errorf("delete server connector: %v", err)
	}
}

// The handshake of a server connector is bound to the admin who began it.
func TestServerHandshakeUser(t *testing.T) {
	st := servicetest.Store(t)
	f := &fakeApp{}
	l := connector.New(st.Connectors, f)
	c, _, _, err := l.BeginServer(ctx, user, "")
	if err != nil || c.OrgID != nil {
		t.Fatalf("begin = %+v %v", c, err)
	}
	if _, err := l.Complete(ctx, "u2", f.state, "code"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("another user completed it: %v", err)
	}
	if u, _ := l.ServerInstallURL(ctx, c.ID); u != "" {
		t.Errorf("pending install url = %q", u)
	}
	if _, err := l.Complete(ctx, user, f.state, "code"); err != nil {
		t.Fatal(err)
	}
	if u, _ := l.ServerInstallURL(ctx, c.ID); u != "https://github.com/apps/stackr-x/installations/new" {
		t.Errorf("install url = %q", u)
	}
}

// racy lets one write land between the leaf's read of a row and its write.
type racy struct {
	store.ConnectorStore
	after func()
}

func (r *racy) Get(ctx context.Context, id string) (store.Connector, error) {
	c, err := r.ConnectorStore.Get(ctx, id)
	if f := r.after; f != nil {
		r.after = nil
		f()
	}
	return c, err
}

// A share or rename that reads a pending row, then loses the race with
// Complete, must not write the pending config back over the App.
func TestShareRenameKeepCompletedConfig(t *testing.T) {
	for name, act := range map[string]func(*connector.Leaf, string) error{
		"share": func(l *connector.Leaf, id string) error {
			_, err := l.SetShares(ctx, id, nil, true)
			return err
		},
		"rename": func(l *connector.Leaf, id string) error {
			_, err := l.RenameServer(ctx, id, "main")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := servicetest.Store(t)
			f := &fakeApp{}
			plain := connector.New(st.Connectors, f)
			pending, _, _, err := plain.BeginServer(ctx, user, "")
			if err != nil {
				t.Fatal(err)
			}
			r := &racy{ConnectorStore: st.Connectors}
			r.after = func() {
				if _, err := plain.Complete(ctx, user, f.state, "code"); err != nil {
					t.Error(err)
				}
			}
			if err := act(connector.New(r, f), pending.ID); err != nil {
				t.Fatal(err)
			}
			got, err := plain.Server(ctx, pending.ID)
			if err != nil || !connector.Connected(got) {
				t.Fatalf("the App was lost: %+v %v", got, err)
			}
		})
	}
}

// Complete keeps what a share did while GitHub was converting the manifest.
func TestCompleteKeepsShares(t *testing.T) {
	st := servicetest.Store(t)
	f := &fakeApp{}
	plain := connector.New(st.Connectors, f)
	pending, _, _, err := plain.BeginServer(ctx, user, "")
	if err != nil {
		t.Fatal(err)
	}
	r := &racy{ConnectorStore: st.Connectors}
	r.after = func() {
		if _, err := plain.SetShares(ctx, pending.ID, nil, true); err != nil {
			t.Error(err)
		}
	}
	if _, err := connector.New(r, f).Complete(ctx, user, f.state, "code"); err != nil {
		t.Fatal(err)
	}
	if got, _ := plain.Server(ctx, pending.ID); !got.ShareAll || !connector.Connected(got) {
		t.Errorf("share-all or App lost: %+v", got)
	}
}
