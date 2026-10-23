package servicetest

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
)

// FakeKind is a connector kind for tests of the core: a form step (an
// account and a secret token), then the redirect out to FakeOutURL and back
// through the core callback with ?code=. Register it with
// service.WithKinds(servicetest.FakeKind{}).
type FakeKind struct{}

// FakeOutURL is where a FakeKind setup sends the browser.
const FakeOutURL = "https://fake.example/authorize"

func (FakeKind) Name() string         { return "fake" }
func (FakeKind) Label() string        { return "Fake" }
func (FakeKind) Hint() string         { return "A kind for tests." }
func (FakeKind) New() connectors.Conn { return &FakeConn{} }

func (FakeKind) Steps() []connectors.Step {
	return []connectors.Step{
		{Title: "Account", Fields: []connectors.Field{
			{Name: "account", Label: "Account"},
			{Name: "token", Label: "Token", Secret: true},
		}},
		{Title: "Sign in", Intro: "Fake signs you in, then sends you back.", Out: true},
	}
}

// FakeConn is a FakeKind connection: its settings blob.
type FakeConn struct {
	Account string `json:"account"`
	Token   string `json:"token"`
	Code    string `json:"code"`
}

func (f *FakeConn) Name() string { return "Fake · " + f.Account }

// Setup refuses an empty field, and a token that starts with "bad" on the
// token field (never echoing it).
func (f *FakeConn) Setup(v map[string]string) error {
	f.Account, f.Token = strings.TrimSpace(v["account"]), v["token"]
	var bad []error
	if f.Account == "" {
		bad = append(bad, errs.Invalidf("account", "Enter the account"))
	}
	switch {
	case f.Token == "":
		bad = append(bad, errs.Invalidf("token", "Enter the token"))
	case strings.HasPrefix(f.Token, "bad"):
		bad = append(bad, errs.Invalidf("token", "Fake refused the token"))
	}
	return errors.Join(bad...)
}

func (f *FakeConn) Out(_ context.Context, id, back, state string) (connectors.Out, error) {
	return connectors.Out{Method: "POST", URL: FakeOutURL, Values: map[string]string{"state": state, "redirect_uri": back, "id": id}}, nil
}

func (f *FakeConn) Back(_ context.Context, q url.Values) error {
	if f.Code = q.Get("code"); f.Code == "" {
		return errs.Refusedf("Fake sent no code")
	}
	return nil
}
