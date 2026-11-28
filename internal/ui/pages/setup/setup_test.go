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

func TestApproveDestroysUsesPanelConfirm(t *testing.T) {
	var b strings.Builder
	if err := Plan(PlanView{Approve: "/x/approve", Destroys: []string{"a", "b"}}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, want := range []string{"<confirm-dialog", "This apply destroys: a, b.", `hx-post="/x/approve"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "hx-confirm") {
		t.Error("native hx-confirm left")
	}
}
