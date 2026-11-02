package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// IdempotencyHeader names a request a client may retry: the first answer is
// kept a day and sent again, so a lost response never queues a second job.
const IdempotencyHeader = "Idempotency-Key"

// maxIdempotentBody caps what the middleware reads to hash; the endpoints it
// guards take a few small fields.
const maxIdempotentBody = 1 << 20

// Idempotent replays the first answer to a request with an Idempotency-Key,
// per principal and URL. A key reused with another body is a 400, one whose
// first request is still running a 409. Only a 2xx answer is kept: a failed
// request frees its key to be retried. Mount after the route's gate.
func (a *Access) Idempotent() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req, p := c.Request(), Principal(c)
			key := req.Header.Get(IdempotencyHeader)
			if key == "" || p == nil {
				return next(c)
			}
			if len(key) > 200 {
				return errs.Invalidf(IdempotencyHeader, "the key is over 200 characters")
			}
			body, err := io.ReadAll(http.MaxBytesReader(c.Response(), req.Body, maxIdempotentBody))
			if err != nil {
				return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "request body is too large")
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			id := hash(p.User.ID, req.Method, req.URL.Path, key)
			reqHash := hash(string(body))
			ans, err := a.orch.IdempotencyBegin(req.Context(), id, reqHash)
			if err != nil {
				return HTTPError(err)
			}
			if ans != nil {
				c.Response().Header().Set("Idempotent-Replay", "true")
				if len(ans.Body) == 0 {
					return c.NoContent(ans.Status)
				}
				return c.JSONBlob(ans.Status, ans.Body)
			}
			rec := &recorder{ResponseWriter: c.Response().Writer}
			c.Response().Writer = rec
			err = next(c)
			status := c.Response().Status
			ctx := req.Context()
			if err == nil && status >= 200 && status < 300 {
				if a.orch.IdempotencyFinish(ctx, id, reqHash, service.Answer{Status: status, Body: rec.buf.Bytes()}) == nil {
					return nil
				}
			}
			_ = a.orch.IdempotencyRelease(ctx, id) // a failed or unkept request frees its key
			return err
		}
	}
}

// hash is a field-separated digest, so ("ab","c") and ("a","bc") differ.
func hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// recorder copies the response body while it passes through.
type recorder struct {
	http.ResponseWriter
	buf bytes.Buffer
}

func (r *recorder) Write(b []byte) (int, error) {
	r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}
