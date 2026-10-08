package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// A server param is not any tile's: a change redeploys nothing, the server
// file's items resolve it, a missing one is unset by name, and no other ref
// form works there.
func TestServerParams(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	up := w.tile(t, "api", true)

	rs, err := w.orch.SetParams(ctx, ServerParamScope, []ParamEntry{
		{Collection: "s3", Name: "region", Kind: "param", Value: "eu"},
		{Collection: "s3", Name: "secret_key", Kind: "secret", Value: "sk"},
	})
	must(t, err)
	if len(rs) != 0 {
		t.Errorf("SetParams named %+v; a server param redeploys no tile", rs)
	}
	if js, err := w.orch.TileJobs(ctx, []string{up.ID}, 10); err != nil || len(js) != 0 {
		t.Errorf("jobs = %+v, %v; a running tile must be left alone", js, err)
	}

	masked, err := w.orch.MaskedParams(ctx, ServerParamScope)
	if err != nil || len(masked) != 2 {
		t.Fatalf("masked = %+v, %v", masked, err)
	}
	for _, p := range masked {
		if p.Kind == "secret" && p.Value != "" {
			t.Errorf("masked secret carries %q", p.Value)
		}
	}

	got, err := w.orch.ExpandServerRefs(ctx, "{{x}} ${{ server.params.s3.secret_key }}@${{server.params.s3.region}}")
	if err != nil || got != "{{x}} sk@eu" {
		t.Errorf("expand = %q, %v", got, err)
	}
	if got, err = w.orch.ExpandServerRefs(ctx, "plain"); err != nil || got != "plain" {
		t.Errorf("plain = %q, %v", got, err)
	}
	_, err = w.orch.ExpandServerRefs(ctx, "${{ server.params.s3.nope }}")
	if u, ok := errs.IsUnset(err); !ok || u.Param != "server.params.s3.nope" {
		t.Errorf("missing = %v, want unset naming server.params.s3.nope", err)
	}
	for _, bad := range []string{"${{ params.s3.region }}", "${{ org.params.s3.region }}", "${{ tile.api.url }}"} {
		if got, err := w.orch.ExpandServerRefs(ctx, bad); err == nil || got != "" || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%s = %q, %v; want a refusal", bad, got, err)
		}
	}

	// an org's view never holds the rows
	if ps, err := w.orch.Params(ctx, ParamScope{Kind: "stack", ID: w.stack}, true); err != nil || len(ps) != 0 {
		t.Errorf("stack params = %+v, %v; a server row reached a stack", ps, err)
	}
	_, err = w.orch.DeleteParam(ctx, ServerParamScope, "s3", "region")
	must(t, err)
	if _, err := w.orch.DeleteParam(ctx, ServerParamScope, "s3", "region"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("delete again = %v, want not found", err)
	}
}
