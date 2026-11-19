package canvas_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The org Config tab's picker lists the server connectors shared with the
// org, marks them, can bind through one, and the org's connector routes
// never reach a server connector.
func TestOrgConfigPickerServerConnector(t *testing.T) {
	r := newConfigRig(t)
	env := r.owner.env
	srv := env.ServerConnector(t, "main", "srvsec")
	const tab = "/acme/-/drawer/config/picker"

	if body := r.owner.do(t, "GET", tab, nil, true).Body.String(); strings.Contains(body, `value="`+srv+`"`) {
		t.Fatalf("an unshared server connector is in the picker:\n%s", body)
	}
	if _, err := env.Orch.ShareConnector(context.Background(), nil, srv, []string{r.owner.org}, false); err != nil {
		t.Fatal(err)
	}
	body := r.owner.do(t, "GET", tab, nil, true).Body.String()
	if !strings.Contains(body, `value="`+srv+`"`) || !strings.Contains(body, "main (github.com), server") ||
		!strings.Contains(body, `value="`+r.conn+`"`) {
		t.Fatalf("the picker lacks the shared connector or the org's own:\n%s", body)
	}

	r.file(t, regionFile)
	rec := r.owner.do(t, "POST", "/acme/-/drawer/config", url.Values{"connector": {srv}, "repo": {"acme/org"}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Config file saved and planned.") {
		t.Fatalf("bind through a shared server connector = %d\n%s", rec.Code, rec.Body)
	}

	// the card opens read-only; its writes reach nothing
	card := r.owner.do(t, "GET", "/acme/-/connectors/"+srv, nil, true)
	if body := card.Body.String(); card.Code != http.StatusOK || !strings.Contains(body, "shared from the server") ||
		strings.Contains(body, ">Rename</button>") || strings.Contains(body, "/delete") {
		t.Errorf("shared card = %d, want read-only:\n%s", card.Code, body)
	}
	for _, path := range []string{"/acme/-/connectors/" + srv + "/delete", "/acme/-/connectors/" + srv + "/rename"} {
		r.owner.do(t, "POST", path, url.Values{"name": {"mine"}}, true)
	}
	if cs, _ := env.Orch.ServerConnectors(context.Background()); len(cs) != 1 || cs[0].Name != "main" {
		t.Errorf("server connectors after org writes = %+v", cs)
	}
}

// A server admin sees "Make this a server connector" on the org's own
// connector, an owner who is no admin does not; the move leaves the card
// shared and the org's binding in place.
func TestMakeServerConnectorAction(t *testing.T) {
	r := newConfigRig(t)
	env := r.owner.env
	card := "/acme/-/connectors/" + r.conn
	if body := r.owner.do(t, "GET", card, nil, true).Body.String(); strings.Contains(body, "Make this a server connector") {
		t.Fatalf("an owner who is no admin is offered the move:\n%s", body)
	}
	a := env.User(t, "admin@acme.test", true)
	env.Member(t, r.owner.org, a, "owner")
	admin := browser{env, r.owner.h, r.owner.org, env.Session(t, a)}
	body := admin.do(t, "GET", card, nil, true).Body.String()
	if !strings.Contains(body, "Make this a server connector") || !strings.Contains(body, `hx-post="`+card+`/make-server"`) {
		t.Fatalf("an admin is not offered the move:\n%s", body)
	}
	if rec := r.owner.do(t, "POST", card+"/make-server", nil, true); rec.Code != http.StatusForbidden {
		t.Errorf("an owner's move = %d, want 403", rec.Code)
	}
	rec := admin.do(t, "POST", card+"/make-server", nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "It is a server connector now") || !strings.Contains(rec.Body.String(), "shared from the server") ||
		strings.Contains(rec.Body.String(), "Make this a server connector") {
		t.Fatalf("move = %d\n%s", rec.Code, rec.Body)
	}
	if cs, _ := env.Orch.ServerConnectors(context.Background()); len(cs) != 1 || cs[0].ID != r.conn {
		t.Errorf("server connectors = %+v", cs)
	}
}
