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
	const tab = "/acme/-/drawer?tab=config"

	if body := r.owner.do(t, "GET", tab, nil, true).Body.String(); strings.Contains(body, `value="`+srv+`"`) {
		t.Fatalf("an unshared server connector is in the picker:\n%s", body)
	}
	if _, err := env.Orch.ShareConnector(context.Background(), srv, []string{r.owner.org}, false); err != nil {
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
