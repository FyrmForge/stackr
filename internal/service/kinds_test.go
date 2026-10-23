package service_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A redirect setup saves nothing until the provider sends the browser back
// to the one callback with the state it was given, signed in as the user
// who began; then the connection is saved, once.
func TestRedirectSetup(t *testing.T) {
	env := servicetest.NewWith(t, []service.Option{service.WithKinds(servicetest.FakeKind{})})
	ctx := context.Background()
	in := service.ConnectorAdd{Kind: "fake", Values: map[string]string{"account": "acme", "token": "t0k"}, User: "u1", Return: "/-/admin?tab=connectors"}

	for name, bad := range map[string]service.ConnectorAdd{
		"no user (the API)": {Kind: in.Kind, Values: in.Values, Return: in.Return},
		"offsite return":    {Kind: in.Kind, Values: in.Values, User: "u1", Return: "//evil.example/"},
	} {
		if _, err := env.Orch.AddConnector(ctx, bad); !errors.Is(err, errs.ErrRefused) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	a, err := env.Orch.AddConnector(ctx, in)
	if err != nil || a.Out == nil || a.Out.Method != "POST" || a.Out.URL != servicetest.FakeOutURL ||
		!strings.HasSuffix(a.Out.Values["redirect_uri"], service.ConnectorCallbackPath) {
		t.Fatalf("add = %+v %v", a, err)
	}
	state := a.Out.Values["state"]
	if cs, _ := env.Orch.ConnectorsAt(ctx, nil); len(cs) != 0 {
		t.Fatalf("saved before the provider answered: %v", cs)
	}
	back := func(user, state string) (string, error) {
		return env.Orch.ConnectorBack(ctx, user, url.Values{"state": {state}, "code": {"c0de"}})
	}
	id, _, _ := strings.Cut(state, ".")
	for name, err := range map[string]error{
		"another user": func() error { _, err := back("u2", state); return err }(),
		"wrong nonce":  func() error { _, err := back("u1", id+".00"); return err }(),
		"no state":     func() error { _, err := back("u1", ""); return err }(),
	} {
		if !errors.Is(err, errs.ErrRefused) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	ret, err := back("u1", state)
	if err != nil || ret != in.Return {
		t.Fatalf("back = %q %v", ret, err)
	}
	cs, _ := env.Orch.ConnectorsAt(ctx, nil)
	if len(cs) != 1 || cs[0].ID != id || cs[0].Name != "Fake · acme" || cs[0].Kind != "fake" || len(cs[0].Capabilities) != 0 {
		t.Fatalf("saved = %+v", cs)
	}
	if _, err := back("u1", state); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("a second callback with the same state: %v", err)
	}
	// a git list never shows it; removing it needs no capability's clean-up
	if srv, _ := env.Orch.ServerConnectors(ctx); len(srv) != 0 {
		t.Errorf("the fake kind is in the git list: %v", srv)
	}
	if err := env.Orch.DeleteServerConnector(ctx, id); err != nil {
		t.Fatal(err)
	}
	if cs, _ := env.Orch.ConnectorsAt(ctx, nil); len(cs) != 0 {
		t.Errorf("left after remove: %v", cs)
	}
}

// Kinds list their declared steps and capabilities; Namecheap offers DNS
// and its server IPv4 field asks for the prefill.
func TestConnectorKinds(t *testing.T) {
	env := servicetest.New(t)
	k, err := env.Orch.ConnectorKindOf("namecheap")
	if err != nil || len(k.Caps) != 1 || k.Caps[0] != service.CapDNS || k.Redirects || len(k.Steps) != 4 {
		t.Fatalf("namecheap = %+v %v", k, err)
	}
	if _, err := env.Orch.ConnectorKindOf("nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown kind: %v", err)
	}
}
