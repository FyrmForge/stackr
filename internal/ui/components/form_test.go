package components

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// The form skips the browser's bubbles; a password field gets the show
// toggle and its autocomplete token.
func TestFormNovalidatePasswordToggle(t *testing.T) {
	var b strings.Builder
	body := Field(FieldView{Name: "password", Label: "Password", Type: "password", Autocomplete: "new-password"})
	if err := Form("f", "/x").Render(templ.WithChildren(context.Background(), body), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{" novalidate", "<password-toggle", `autocomplete="new-password"`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in %s", want, b.String())
		}
	}
}

// A field with an error is marked invalid and described by the error; a
// clean one is not. Two refused fields both show their reason.
func TestFieldInvalidAria(t *testing.T) {
	r := &Refused{Field: "cpu", Msg: "too many", More: map[string]string{"cpu": "too many", "mem": "too much"}}
	ctx := WithRefused(context.Background(), r)
	for name, want := range map[string]string{"cpu": "too many", "mem": "too much"} {
		var b strings.Builder
		if err := Field(FieldView{Name: name, Label: name}).Render(ctx, &b); err != nil {
			t.Fatal(err)
		}
		for _, w := range []string{`aria-invalid="true"`, `aria-describedby="error-` + name + `"`, want} {
			if !strings.Contains(b.String(), w) {
				t.Errorf("%s: missing %q in %s", name, w, b.String())
			}
		}
	}
	var b strings.Builder
	if err := Field(FieldView{Name: "ok", Label: "ok"}).Render(ctx, &b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "aria-invalid") {
		t.Errorf("clean field marked invalid: %s", b.String())
	}
}
