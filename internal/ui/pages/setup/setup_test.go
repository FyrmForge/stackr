package setup

import (
	"context"
	"strings"
	"testing"
)

// Discard is a typed confirm on the org's name; a draft types "discard".
func TestDiscardIsTyped(t *testing.T) {
	for word, v := range map[string]DoneView{
		"acme":    {Base: "/acme/-/setup", Name: "acme"},
		"discard": {Base: "/x/-/setup", Name: "Untitled organization", Draft: true},
	} {
		var b strings.Builder
		if err := Done(v).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		got := b.String()
		if !strings.Contains(got, `<confirm-dialog word="`+word+`"`) || !strings.Contains(got, `hx-post="`+v.Base+`/discard"`) ||
			strings.Contains(got, "hx-confirm") {
			t.Errorf("%s:\n%s", word, got)
		}
	}
}
