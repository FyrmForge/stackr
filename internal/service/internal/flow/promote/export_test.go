package promote

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Export then plan reads clean: an image tile, a built tile, a volume, a
// secret and a plain param, a domain.
func TestExportPlansClean(t *testing.T) {
	w := setup(t)
	scope := params.Scope{Kind: "env", ID: w.dev.ID}
	must(t, w.f.D.Params.Set(ctx, scope, params.Entry{Collection: "app", Name: "mode", Kind: params.Param, Value: "fast"}))
	must(t, w.f.D.Params.Set(ctx, scope, params.Entry{Collection: "app", Name: "key", Kind: params.Secret, Value: "s3cret-value"}))
	_, _, err := w.f.D.Volumes.Declare(ctx, volume.Scope{Kind: "env", ID: w.dev.ID}, "data", 100, nil)
	must(t, err)

	api := w.mk(t, w.dev, "api", store.Tile{
		Kind:          tile.Image,
		ImageRef:      "nginx:1",
		ContainerPort: 80,
		Volumes:       "data:/srv",
		EnvJSON:       `{"KEY":"${{ params.app.key }}","MODE":"${{ params.app.mode }}"}`,
		MemLimitMB:    256,
	})
	_, err = w.f.D.Domains.Attach(ctx, api.ID, domain.Spec{Host: "api.example.com", Port: 80}, false, nil)
	must(t, err)
	web := w.mk(t, w.dev, "web", store.Tile{
		Kind:      tile.Service,
		GitURL:    w.st.ConfigRepo,
		GitBranch: "main",
		DependsOn: "api",
	})
	w.running(api)
	w.running(web)

	out, _, err := w.f.Export(ctx, w.st.ID, w.dev.ID)
	must(t, err)
	got := string(out)
	if strings.Contains(got, "s3cret-value") || !strings.Contains(got, "type: secret") {
		t.Errorf("secret leaked or missing:\n%s", got)
	}
	if strings.Contains(got, "git_url") || strings.Contains(got, "restart") || strings.Contains(got, "replicas") {
		t.Errorf("defaults written:\n%s", got)
	}
	if _, err := Load(out, nil, "acme"); err != nil {
		t.Fatalf("strict decode: %v\n%s", err, got)
	}

	im, err := w.f.D.Images.Built(ctx, "shop/web:c1", "sha256:c1")
	must(t, err)
	w.files["c1"] = got
	r := w.release(t, "c1", release.Pin{Slug: "web", ImageID: &im.ID})
	_, err = w.f.D.Envs.SetRelease(ctx, w.dev, r.ID)
	must(t, err)
	p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	if len(p.Changes) != 0 || p.Blocked() {
		t.Errorf("plan of the export = %s %v\n%s", kinds(p), p.Blockers, got)
	}
}

// A UI-made domain with a literal basic-auth password stays in the export,
// minus the password, with a warning; planning the file does not remove the
// domain.
func TestExportKeepsDomainWithLiteralBasicAuth(t *testing.T) {
	w := setup(t)
	api := w.mk(t, w.dev, "api", store.Tile{Kind: tile.Image, ImageRef: "nginx:1", ContainerPort: 80})
	_, err := w.f.D.Domains.Attach(ctx, api.ID, domain.Spec{
		Host: "api.example.com", Port: 80,
		Extras: domain.Extras{BasicAuth: &domain.BasicAuth{User: "bob", Password: "hunter2"}},
	}, false, nil)
	must(t, err)
	w.running(api)

	out, warns, err := w.f.Export(ctx, w.st.ID, w.dev.ID)
	must(t, err)
	got := string(out)
	if !strings.Contains(got, "api.example.com") || strings.Contains(got, "hunter2") || strings.Contains(got, "basic_auth") {
		t.Errorf("export:\n%s", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "api.example.com") || !strings.Contains(warns[0], "basic") {
		t.Errorf("warnings = %v", warns)
	}

	w.files["c1"] = got
	r := w.release(t, "c1")
	_, err = w.f.D.Envs.SetRelease(ctx, w.dev, r.ID)
	must(t, err)
	p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	for _, c := range p.Changes {
		if c.Kind == "domain" && c.New == "" {
			t.Errorf("domain removed: %s", kinds(p))
		}
	}
}

// A literal protect password is never written, and no warning claims the plan
// clears it; a ref is written.
func TestExportDefaultsLeaveLiteralProtectPassword(t *testing.T) {
	m := defaultsMap(`{"protect":true,"protect_user":"bob","protect_password":"hunter2"}`)
	if b, _ := json.Marshal(m); strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "protect_user") {
		t.Errorf("m = %s", b)
	}
	m = defaultsMap(`{"protect_user":"bob","protect_password":"${{ params.a.b }}"}`)
	if m["protect_password"] == nil {
		t.Errorf("ref dropped: %v", m)
	}
}

// The whole ladder round-trips: dev (branch), staging and production
// (promote), a stack param, an env param per env. Every env plans clean.
func TestExportLadderPlansCleanPerEnv(t *testing.T) {
	w := setup(t)
	now := time.Now()
	mkEnv := func(name string, pos int) store.Environment {
		return store.Environment{
			ID: uuid.NewString(), StackID: w.st.ID, Name: name, Slug: name, Type: "static",
			Settings: "{}", Network: "n", Position: pos, FromKind: "promote", CreatedAt: now,
		}
	}
	stg, prod := w.prd, mkEnv("production", 2) // dev, prd (staging), production
	must(t, w.s.Environments.Create(ctx, prod))
	envs := []store.Environment{w.dev, stg, prod}
	must(t, w.f.D.Params.Set(ctx, params.Scope{Kind: "stack", ID: w.st.ID}, params.Entry{Collection: "app", Name: "region", Kind: params.Param, Value: "eu"}))
	for _, e := range envs {
		sc := params.Scope{Kind: "env", ID: e.ID}
		must(t, w.f.D.Params.Set(ctx, sc, params.Entry{Collection: "app", Name: "name_" + e.Slug, Kind: params.Param, Value: "v-" + e.Slug}))
		must(t, w.f.D.Params.Set(ctx, sc, params.Entry{Collection: "app", Name: "mode", Kind: params.Param, Value: "m-" + e.Slug}))
		must(t, w.f.D.Params.Set(ctx, sc, params.Entry{Collection: "app", Name: "key", Kind: params.Secret, Value: "s3cret-" + e.Slug}))
	}
	out, _, err := w.f.Export(ctx, w.st.ID, "")
	must(t, err)
	got := string(out)
	if strings.Contains(got, "s3cret") || strings.Count(got, "from: promote") != 2 || !strings.Contains(got, "branch: main") ||
		!strings.Contains(got, "region") {
		t.Fatalf("export:\n%s", got)
	}
	r, err := Load(out, nil, "acme")
	must(t, err)
	if r.Envs["prd"].FromKind != "promote" || r.Envs["production"].FromKind != "promote" || r.Envs["dev"].FromKind != "branch" {
		t.Errorf("ladder lost: %+v", r.Envs)
	}
	w.files["c1"] = got
	rel := w.release(t, "c1")
	for _, e := range envs {
		_, err = w.f.D.Envs.SetRelease(ctx, e, rel.ID)
		must(t, err)
		p, err := w.f.Plan(ctx, e.ID, rel.ID, io.Discard)
		must(t, err)
		if len(p.Changes) != 0 || p.Blocked() {
			t.Errorf("plan of %s = %s %v\n%s", e.Slug, kinds(p), p.Blockers, got)
		}
	}
}
