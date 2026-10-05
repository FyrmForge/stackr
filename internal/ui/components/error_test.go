package components

import (
	"context"
	"strings"
	"testing"
)

// A signed-in viewer (render.Theme left a theme) keeps the rail on an
// error page; anonymous gets none.
func TestErrorPageKeepsRail(t *testing.T) {
	for ctx, want := range map[context.Context]bool{
		WithTheme(context.Background(), "dark"): true,
		context.Background():                    false,
	} {
		var b strings.Builder
		if err := ErrorPage(404, "Not Found").Render(ctx, &b); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(b.String(), `id="shell-rail"`); got != want {
			t.Errorf("rail = %v, want %v", got, want)
		}
	}
}

// The error page carries the request id and a CSRF token, so Log out works.
func TestErrorPageCarriesRequestAndCSRF(t *testing.T) {
	ctx := WithRequest(WithTheme(context.Background(), "dark"), "req-123", "tok-456")
	var b strings.Builder
	if err := ErrorPage(500, "boom").Render(ctx, &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"req-123", "tok-456"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
