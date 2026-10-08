package promote

import (
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
)

// A resource rename leaves a generated redirect (auto, redirect_to) on each
// old host; the stack file never names one, and a re-plan must not remove
// it. A redirect the file once declared (not auto) still goes when the file
// stops naming it.
func TestGeneratedRedirectSurvivesPlan(t *testing.T) {
	w := setup(t)
	w.fake.Digests = map[string]string{"nginx:1": "sha256:one"}
	w.resource(t, domainres.Instance, "", "example.com")
	w.files["c1"] = autoFile("auto: true", "")
	r := w.release(t, "c1")
	_, err := w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	w.running(w.tileIn(t, w.dev, "api"))
	api := w.tileIn(t, w.dev, "api")

	_, err = w.f.D.Domains.Attach(ctx, api.ID, domain.Spec{
		Host: "old.example.com", RedirectTo: "api.dev.shop.acme.example.com", Auto: true,
	}, false, nil)
	must(t, err)
	_, err = w.f.D.Domains.Attach(ctx, api.ID, domain.Spec{
		Host: "filed.example.com", RedirectTo: "api.dev.shop.acme.example.com",
	}, false, nil)
	must(t, err)

	e, err := w.f.D.Envs.Get(ctx, w.dev.ID)
	must(t, err)
	p := planOK(t, w, e, w.release(t, "c1"))
	var removed []string
	for _, c := range p.Changes {
		if c.Kind == "domain" {
			removed = append(removed, c.Old)
		}
	}
	if got := strings.Join(removed, " "); got != "filed.example.com" {
		t.Errorf("domain removals = %q, want only the file's own redirect", got)
	}
}
