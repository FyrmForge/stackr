package components

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/FyrmForge/hamr/pkg/ctx"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The class on <html> is the whole theme switch: a wrong value here silently
// serves the wrong palette to everyone.
func TestThemeClass(t *testing.T) {
	if got := themeClass(context.Background(), nil); got != "" {
		t.Fatalf("no context: got %q, want the OS default", got)
	}

	e := echo.New()
	newCtx := func() echo.Context {
		return e.NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
	}

	if got := themeClass(context.Background(), newCtx()); got != "" {
		t.Fatalf("signed out: got %q, want the OS default", got)
	}

	for theme, want := range map[string]string{
		"light":  "light",
		"dark":   "dark",
		"system": "",
		"":       "",
		"puce":   "",
	} {
		c := newCtx()
		ctx.Set(c, ctx.SubjectKey, any(&repo.User{Theme: theme}))
		if got := themeClass(context.Background(), c); got != want {
			t.Errorf("theme %q: got %q, want %q", theme, got, want)
		}
		// The error page is rendered with no echo.Context at all, so the same
		// answer has to come off the request context ThemeContext filled.
		var got string
		err := middleware.ThemeContext()(func(c echo.Context) error {
			got = themeClass(c.Request().Context(), nil)
			return nil
		})(c)
		if err != nil {
			t.Fatalf("theme middleware: %v", err)
		}
		if got != want {
			t.Errorf("theme %q off the request context: got %q, want %q", theme, got, want)
		}
	}
}
