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
