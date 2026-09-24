// Package v1 is the /api/v1 handlers: bind and shape-check the input, call
// one orchestrator verb, render. The route table (internal/api/routes.go)
// mounts them behind the one auth middleware; nothing here decides access.
package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// H holds the one orchestrator every handler calls.
type H struct{ S *service.Orchestrator }

// list renders a nil slice as [], never null.
func list[T any](xs []T, err error) ([]T, error) {
	if xs == nil {
		xs = []T{}
	}
	return xs, err
}

// Endpoint is one handler plus the shapes the OpenAPI dump reads.
type Endpoint struct {
	Handle  echo.HandlerFunc
	In, Out reflect.Type // nil = no body
	Status  int          // the success code
	Stream  string       // non-empty: the response media type (SSE, raw)
}

// None is the input or output of an endpoint without a body.
type None struct{}

// JSON makes an endpoint that decodes In strictly (an unknown field is a
// 400, so a typo never silently sets nothing), calls f and renders Out.
func JSON[In, Out any](status int, f func(c echo.Context, in In) (Out, error)) Endpoint {
	e := Endpoint{Status: status, Handle: func(c echo.Context) error {
		var in In
		if err := decode(c, &in); err != nil {
			return err
		}
		out, err := f(c, in)
		if err != nil {
			return err
		}
		if _, empty := any(out).(None); empty {
			return c.NoContent(status)
		}
		return c.JSON(status, out)
	}}
	if t := reflect.TypeFor[In](); t != reflect.TypeFor[None]() {
		e.In = t
	}
	if t := reflect.TypeFor[Out](); t != reflect.TypeFor[None]() {
		e.Out = t
	}
	return e
}

// Get is JSON for a read without a body.
func Get[Out any](f func(c echo.Context) (Out, error)) Endpoint {
	return JSON(http.StatusOK, func(c echo.Context, _ None) (Out, error) { return f(c) })
}

// Streamed is an endpoint whose response is a stream of mediaType, not
// one JSON body.
func Streamed(mediaType string, f echo.HandlerFunc) Endpoint {
	return Endpoint{Handle: f, Status: http.StatusOK, Stream: mediaType}
}

// Job answers 202 with the queued job: the caller follows it with PollJob.
func Job[In any](f func(c echo.Context, in In) (service.Job, error)) Endpoint {
	return JSON(http.StatusAccepted, f)
}

// Done answers 204 after a verb that returns only an error.
func Done[In any](f func(c echo.Context, in In) error) Endpoint {
	return JSON(http.StatusNoContent, func(c echo.Context, in In) (None, error) { return None{}, f(c, in) })
}

func decode(c echo.Context, v any) error {
	if _, none := v.(*None); none {
		return nil
	}
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	err = d.Decode(v)
	if errors.Is(err, io.EOF) {
		return errs.Invalidf("", "request body is required")
	}
	if err != nil {
		return errs.Invalidf("", "%s", err.Error())
	}
	return nil
}

// scope and who read what the middleware resolved.
func scope(c echo.Context) service.Scope { return middleware.ScopeOf(c) }
func who(c echo.Context) string          { return middleware.Principal(c).User.ID }
func rc(c echo.Context) context.Context  { return c.Request().Context() }
