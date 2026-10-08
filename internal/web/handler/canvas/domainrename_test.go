package canvas_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The org's domains tab renames the org's own row: an owner gets a form on
// it and none on the instance's row, the post moves the host, the instance
// row is out of reach, and a viewer is refused.
func TestOrgDomainRename(t *testing.T) {
	ctx := context.Background()
	b := newBrowser(t, "owner")
	inst, err := b.env.Orch.CreateDomainResource(ctx, "instance", "", "example.com", false, "")
	if err != nil {
		t.Fatal(err)
	}
	own, err := b.env.Orch.CreateDomainResource(ctx, "org", b.org, "acme.io", false, "")
	if err != nil {
		t.Fatal(err)
	}
	body := b.do(t, "GET", "/acme/-/drawer?tab=domains", nil, true).Body.String()
	rename := "/acme/-/drawer/domains/" + own.ID + "/rename"
	if !strings.Contains(body, rename) || strings.Contains(body, "/domains/"+inst.ID+"/rename") {
		t.Errorf("rename form missing on the org's row, or present on the instance's:\n%s", body)
	}
	// the rename asks first, as the CLI does; the yes posts the typed host
	for _, want := range []string{
		"<confirm-dialog", "Every hostname under it moves and the old one redirects. Point DNS at the new name first.",
		`hx-post="` + rename + `"`, `hx-trigger="confirmed"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rename form lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `<button type="submit" class="btn btn-sm">Rename</button>`) {
		t.Errorf("a submit button renames without asking:\n%s", body)
	}
	rec := b.do(t, "POST", rename, url.Values{"host": {"acme.dev"}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Domain renamed to acme.dev.") {
		t.Fatalf("rename = %d\n%s", rec.Code, rec.Body)
	}
	if rs, _ := b.env.Orch.DomainResources(ctx, b.org); len(rs) != 2 || rs[0].Host != "acme.dev" {
		t.Errorf("rows after rename = %+v, want acme.dev first", rs)
	}
	if rec := b.do(t, "POST", "/acme/-/drawer/domains/"+inst.ID+"/rename", url.Values{"host": {"x.dev"}}, true); rec.Code != http.StatusNotFound {
		t.Errorf("rename the instance's row through the org = %d, want 404", rec.Code)
	}

	v := newBrowser(t, "viewer")
	vr, err := v.env.Orch.CreateDomainResource(ctx, "org", v.org, "acme.io", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec := v.do(t, "POST", "/acme/-/drawer/domains/"+vr.ID+"/rename", url.Values{"host": {"x.dev"}}, true); rec.Code != http.StatusForbidden {
		t.Errorf("viewer rename = %d, want 403", rec.Code)
	}
}
