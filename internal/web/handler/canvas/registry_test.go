package canvas_test

import (
	"net/http"
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
	if !strings.Contains(body, "The registry did not answer") && !strings.Contains(body, "the registry did not answer") && !strings.Contains(body, "No images pushed yet") {
		t.Errorf("neither a list nor the registry's failure:\n%s", body)
	}
}
