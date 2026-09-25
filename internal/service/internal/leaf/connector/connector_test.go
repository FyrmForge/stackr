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

type fakeApp struct{ state string }

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
	return "tok-" + app.Slug, nil
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
	if _, _, _, err := l.Begin(ctx, org, ""); err != nil {
		t.Fatal(err)
	}
	c, err := l.Complete(ctx, f.state, "code")
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

	c, action, _, err := l.Begin(ctx, org, "")
	if err != nil || !strings.Contains(action, c.ID+".") || connector.Connected(c) {
		t.Fatalf("begin = %+v %s %v", c, action, err)
	}
	if _, err := l.For(ctx, org, "https://github.com/acme/api"); !errors.As(err, &connector.NoConnector{}) {
		t.Errorf("pending connector resolved: %v", err)
	}
	if _, secret, _ := l.WebhookSecret(ctx, c.ID); secret != "" {
		t.Error("pending connector has a webhook secret")
	}
	if _, err := l.Complete(ctx, c.ID+".wrong", "code"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("wrong nonce = %v", err)
	}
	c, err = l.Complete(ctx, f.state, "code")
	if err != nil || !connector.Connected(c) || c.Name != "GitHub · stackr-x" {
		t.Fatalf("complete = %+v %v", c, err)
	}
	if _, err := l.Complete(ctx, f.state, "code"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("replayed callback = %v", err)
	}
	if _, _, _, err := l.Begin(ctx, org, ""); err == nil {
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
