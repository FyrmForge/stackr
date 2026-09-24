package components

import (
	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/repo"
)

// BaseURL is the application's public origin (e.g. "https://example.com").
// Empty in dev; set from main via the BASE_URL env var.
var BaseURL string

// StaticBaseURL is the base URL prefix for static assets.
// Set from main before any templates render.
var StaticBaseURL = "/static"

// FlashAlertClass returns the CSS alert class for a flash type (plain CSS).
func FlashAlertClass(t middleware.FlashType) string {
	switch t {
	case middleware.FlashSuccess:
		return "alert-success"
	case middleware.FlashError:
		return "alert-error"
	case middleware.FlashWarning:
		return "alert-warning"
	default:
		return "alert-info"
	}
}

// FlashTailwindClass returns Tailwind CSS classes for a flash type.
func FlashTailwindClass(t middleware.FlashType) string {
	switch t {
	case middleware.FlashSuccess:
		return "bg-green-900/50 border-green-700 text-green-300"
	case middleware.FlashError:
		return "bg-red-900/50 border-red-700 text-red-300"
	case middleware.FlashWarning:
		return "bg-yellow-900/50 border-yellow-700 text-yellow-300"
	default:
		return "bg-blue-900/50 border-blue-700 text-blue-300"
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

// AbsoluteURL returns an absolute URL for the given path by prepending BaseURL.
// When BaseURL is empty (local dev), the path is returned as-is.
func AbsoluteURL(path string) string {
	if BaseURL == "" {
		return path
	}
	return BaseURL + path
}

// GetUser returns the authenticated user from the Echo context, or nil
// if no user is loaded (e.g. guest pages).
func GetUser(c echo.Context) *repo.User {
	u, _ := middleware.GetSubject(c).(*repo.User)
	return u
}
