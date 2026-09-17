package components

import (
	"context"
	"time"

	"github.com/FyrmForge/hamr/pkg/middleware"
)

// PageCtx bounds a call a page waits on. A miss has to render as unknown,
// never as a zero, and never as a request timeout 500.
func PageCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}

// BaseURL is the application's public origin (e.g. "https://example.com").
// Empty in dev; set from main via the BASE_URL env var.
var BaseURL string

// StaticBaseURL is the base URL prefix for static assets.
// Set from main before any templates render.
var StaticBaseURL = "/static"

// FlashTailwindClass returns Tailwind CSS classes for a flash type.
func FlashTailwindClass(t middleware.FlashType) string {
	switch t {
	case middleware.FlashSuccess:
		return "bg-rw-success/10 border-rw-success/40 text-rw-success"
	case middleware.FlashError:
		return "bg-rw-danger/10 border-rw-danger/40 text-rw-danger"
	case middleware.FlashWarning:
		return "bg-rw-warn/10 border-rw-warn/40 text-rw-warn"
	default:
		return "bg-rw-accent/10 border-rw-accent/40 text-rw-accentHi"
	}
}

// StaticURL returns the full URL for a static asset, using the fingerprinted
// path from the manifest when available (production), or the plain path (dev).
func StaticURL(path string) string {
	if StaticManifest != nil {
		if fp, ok := StaticManifest[path]; ok {
			return StaticBaseURL + "/" + fp
		}
	}
	return StaticBaseURL + "/" + path
}

// shortSHA trims a commit to the length everything on screen shows it at.
func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
