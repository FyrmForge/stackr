package canvas_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The org's Registry tab opens for a member and shows the repositories
// section; a registry that is not there is the section's text, not an error.
func TestOrgRegistryTab(t *testing.T) {
	b := newBrowser(t, "owner")
	rec := b.do(t, "GET", "/acme/-/drawer?tab=registry", nil, true)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `id="tab-registry"`) || !strings.Contains(body, "Repositories") {
		t.Fatalf("registry tab = %d\n%s", rec.Code, body)
	}
	if !strings.Contains(body, "The registry did not answer") && !strings.Contains(body, "the registry did not answer") && !strings.Contains(body, "No images yet") {
		t.Errorf("neither a list nor the registry's failure:\n%s", body)
	}
}

// A repository of another org does not open, and a version is deleted by an
// owner alone: a member and a viewer get a refusal, an owner reaches the
// service, which finds no such repo in this org.
func TestRegistryDeleteIsTheOwners(t *testing.T) {
	const path = "/acme/-/drawer/registry/delete?repo=other%2Fshop%2Fapi&tag=v1"
	for _, role := range []string{"viewer", "member"} {
		b := newBrowser(t, role)
		if rec := b.do(t, "POST", path, url.Values{}, true); rec.Code != http.StatusForbidden {
			t.Errorf("%s delete = %d, want 403", role, rec.Code)
		}
	}
	b := newBrowser(t, "owner")
	if rec := b.do(t, "POST", path, url.Values{}, true); rec.Code != http.StatusNotFound {
		t.Errorf("owner delete of another org's repo = %d, want 404", rec.Code)
	}
	if rec := b.do(t, "GET", "/acme/-/drawer?tab=registry&repo=other%2Fshop%2Fapi", nil, true); rec.Code != http.StatusNotFound {
		t.Errorf("another org's repo opened: %d", rec.Code)
	}
}
