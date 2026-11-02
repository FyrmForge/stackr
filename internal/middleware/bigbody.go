package middleware

import (
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

const rawBodyKey = "stackr.raw-body"

// LiftBodyLimit takes the server-wide body limit (hamr's BodyLimit, 2M) off
// POSTs whose path ends in one of suffixes: a dump upload. It runs in Pre,
// ahead of BodyLimit, and keeps the raw body for Body; nothing reads it
// before the handler, so an unauthenticated caller never gets it read.
func LiftBodyLimit(suffixes ...string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			r := c.Request()
			if r.Method == http.MethodPost && hasSuffix(r.URL.Path, suffixes) {
				c.Set(rawBodyKey, r.Body)
				r.ContentLength = -1 // BodyLimit refuses a declared length up front
			}
			return next(c)
		}
	}
}

func hasSuffix(path string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
}

// Body is the request body without the server-wide limit when
// LiftBodyLimit took it off, else the (limited) body. A handler reads it
// only after the route's auth ran.
func Body(c echo.Context) io.Reader {
	if b, ok := c.Get(rawBodyKey).(io.ReadCloser); ok {
		return b
	}
	return c.Request().Body
}

// UnlimitBody puts the raw body back on the request, for a handler that
// parses a multipart form (c.FormFile) rather than reading Body, capped at
// max bytes (a longer one fails the form read with *http.MaxBytesError).
func UnlimitBody(c echo.Context, max int64) {
	if b, ok := c.Get(rawBodyKey).(io.ReadCloser); ok {
		c.Request().Body = http.MaxBytesReader(c.Response(), b, max)
	}
}
