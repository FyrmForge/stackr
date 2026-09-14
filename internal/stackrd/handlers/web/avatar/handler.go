package avatar

import (
	"net/http"
	"path"
	"strings"

	"github.com/FyrmForge/hamr/pkg/storage"
	"github.com/labstack/echo/v4"
)

// Serve streams a stored avatar. GET /avatars/*
//
// Behind RequireAuth like every other page: these are small and not secret,
// but an unauthenticated endpoint that reads from a storage path is a liability
// nobody asked for.
func Serve(fs storage.FileStorage) echo.HandlerFunc {
	return func(c echo.Context) error {
		p := clean(c.Param("*"))
		if p == "" {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		r, err := fs.Open(c.Request().Context(), p)
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		defer func() { _ = r.Close() }()
		// Immutable: every save writes a new random filename, so a cached URL
		// can never be stale.
		c.Response().Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return c.Stream(http.StatusOK, contentType(p), r)
	}
}

// clean rejects anything that isn't a plain file under one of our two
// prefixes. The storage backend may be a filesystem, so "../" in a URL must
// never reach it.
func clean(raw string) string {
	p := path.Clean("/" + strings.TrimPrefix(raw, "/"))[1:]
	if p == "" || strings.Contains(p, "..") {
		return ""
	}
	if dir, _ := path.Split(p); dir != "users/" && dir != "orgs/" {
		return ""
	}
	return p
}

func contentType(p string) string {
	switch path.Ext(p) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}
