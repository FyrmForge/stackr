package components

import (
	"net/url"
	"strings"
)

// BaseURL is the application's public origin (e.g. "https://example.com").
// Empty in dev; set from main via the BASE_URL env var.
var BaseURL string

// StaticBaseURL is the base URL prefix for static assets.
// Set from main before any templates render.
var StaticBaseURL = "/static"

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

// PanelURL is AbsoluteURL on the panel's own host (the panel_domain setting)
// with BaseURL's scheme and port, so a link made after the panel moved does
// not name the old host. An empty host, or BaseURL unset, is AbsoluteURL.
func PanelURL(host, path string) string {
	u, err := url.Parse(BaseURL)
	if host == "" || err != nil || u.Host == "" {
		return AbsoluteURL(path)
	}
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	u.Host = host
	return strings.TrimRight(u.String(), "/") + path
}
