// Package v1 is the /api/v1 handlers: bind and shape-check the input, call
// one orchestrator verb, render. The route table (internal/api/routes.go)
// mounts them behind the one auth middleware; nothing here decides access.
package v1

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// H holds the one orchestrator every handler calls.
type H struct{ Orch *service.Orchestrator }

// list renders a nil slice as [], never null.
func list[T any](xs []T, err error) ([]T, error) {
	if xs == nil {
		xs = []T{}
	}
	return xs, err
}

// paged is list for a collection that grows: ?limit= caps the page and
// ?cursor= (the X-Next-Cursor of the page before) picks up after it. The
// next cursor is an offset, so a row added between pages can shift one;
// good enough for lists a person reads. No limit is the whole list.
func paged[T any](c echo.Context) func([]T, error) ([]T, error) {
	return func(xs []T, err error) ([]T, error) { return pageOf(c, xs, err) }
}

func pageOf[T any](c echo.Context, xs []T, err error) ([]T, error) {
	xs, err = list(xs, err)
	if err != nil {
		return xs, err
	}
	var limit int
	if err := echo.QueryParamsBinder(c).Int("limit", &limit).BindError(); err != nil || limit < 0 {
		return nil, errs.Invalidf("limit", "limit is a whole number of rows, 0 or more")
	}
	start := 0
	if cur := c.QueryParam("cursor"); cur != "" {
		raw, derr := base64.RawURLEncoding.DecodeString(cur)
		n, aerr := strconv.Atoi(string(raw))
		if derr != nil || aerr != nil || n < 0 {
			return nil, errs.Invalidf("cursor", "cursor is not one this list gave")
		}
		start = min(n, len(xs))
	}
	xs = xs[start:]
	if limit > 0 && len(xs) > limit {
		next := base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(start + limit)))
		c.Response().Header().Set("X-Next-Cursor", next)
		xs = xs[:limit]
	}
	return xs, nil
}

// Paged names ?limit= and ?cursor= next to the other query params the
// handler reads, for the OpenAPI dump.
func (e Endpoint) Paged() Endpoint {
	e.Query = append(e.Query, "limit", "cursor")
	return e
}

// Endpoint is one handler plus the shapes the OpenAPI dump reads.
type Endpoint struct {
	Handle  echo.HandlerFunc
	In, Out reflect.Type // nil = no body
	Status  int          // the success code
	Stream  string       // non-empty: the response media type (SSE, raw)
	InRaw   string       // non-empty: the request body's media type, read raw
	Query   []string     // the query params the handler reads
}

// Q names the query params the handler reads, for the OpenAPI dump.
func (e Endpoint) Q(names ...string) Endpoint {
	e.Query = names
	return e
}

// Limit caps the request body at n bytes; a longer one is a 413.
func (e Endpoint) Limit(n int64) Endpoint {
	next := e.Handle
	e.Handle = func(c echo.Context) error {
		c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, n)
		return next(c)
	}
	return e
}

// None is the input or output of an endpoint without a body.
type None struct{}

// JSON makes an endpoint that decodes In strictly (an unknown field is a
// 400, so a typo never silently sets nothing), calls f and renders Out.
func JSON[In, Out any](status int, f func(c echo.Context, in In) (Out, error)) Endpoint {
	e := Endpoint{
		Status: status,
		Handle: func(c echo.Context) error {
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
		},
	}
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
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("request body is over %d bytes", tooBig.Limit))
	}
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	err = d.Decode(v)
	if _, optional := v.(interface{ BodyOptional() }); optional && errors.Is(err, io.EOF) {
		return nil // an input that marks its body optional: no body is the zero value
	}
	if errors.Is(err, io.EOF) {
		return errs.Invalidf("", "request body is required")
	}
	if err != nil {
		return badBody(err)
	}
	return nil
}

// badBody says what is wrong with a body that did not decode, in the API's
// words and with the field, never encoding/json's.
func badBody(err error) error {
	var typ *json.UnmarshalTypeError
	var syn *json.SyntaxError
	switch {
	case errors.As(err, &typ):
		return errs.Invalidf(typ.Field, "must be %s", jsonKind(typ.Type))
	case errors.As(err, &syn), errors.Is(err, io.ErrUnexpectedEOF):
		return errs.Invalidf("", "the request body is not valid JSON")
	}
	// encoding/json has no typed error for an unknown field
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		name = strings.Trim(name, `"`)
		return errs.Invalidf(name, "unknown field")
	}
	return errs.Invalidf("", "the request body could not be read")
}

func jsonKind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Slice, reflect.Array:
		return "a list"
	case reflect.Map, reflect.Struct:
		return "an object"
	}
	return "a number"
}

// scope and who read what the middleware resolved.
func scope(c echo.Context) service.Scope {
	return middleware.ScopeOf(c)
}

func who(c echo.Context) string {
	return middleware.Principal(c).User.ID
}

func rc(c echo.Context) context.Context {
	return c.Request().Context()
}
