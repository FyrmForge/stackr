package promote

import (
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
)

func TestGeneratedSecret(t *testing.T) {
	w := setup(t)
	w.files["c1"] = strings.Replace(shopFile, "key: {type: secret}", "key: {type: secret, generate: 24}", 1)
	r := w.release(t, "c1")
	scope := params.Scope{Kind: "env", ID: w.dev.ID}

	p, err := w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	if !strings.Contains(kinds(p), "param:app.key") {
		t.Errorf("plan = %s", kinds(p))
	}
	for _, c := range p.Changes {
		if c.Field == "app.key" && c.Note != "generated" {
			t.Errorf("note = %q", c.Note)
		}
	}
	vals, err := w.f.D.Params.Values(ctx, scope, true)
	must(t, err)
	first := vals["app.key"]
	if !first.Secret || len(first.V) != 24 {
		t.Fatalf("generated = %+v", first)
	}

	// A second promote of a new release keeps the value.
	w.files["c2"] = w.files["c1"] + "\n"
	r2 := w.release(t, "c2")
	p2, err := w.f.Apply(ctx, w.dev.ID, r2.ID, io.Discard, nil)
	must(t, err)
	for _, c := range p2.Changes {
		if c.Field == "app.key" {
			t.Errorf("second plan touches the secret: %s", kinds(p2))
		}
	}
	vals, err = w.f.D.Params.Values(ctx, scope, true)
	must(t, err)
	if vals["app.key"].V != first.V {
		t.Error("generated secret changed on a later promote")
	}
}

func TestGenerateGrammar(t *testing.T) {
	for name, decl := range map[string]string{
		"with value": "{type: secret, generate: 32, value: x}",
		"on a param": "{type: param, generate: 32}",
		"too short":  "{type: secret, generate: 8}",
		"too long":   "{type: secret, generate: 200}",
		"v0 default": "{type: secret, default: generated, length: 32}",
	} {
		f := "version: 1\nstack: s\nparams:\n  a:\n    b: " + decl + "\n"
		if _, err := Load([]byte(f), nil, "acme"); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}
