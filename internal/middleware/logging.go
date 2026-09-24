package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/ctx"
	"github.com/FyrmForge/hamr/pkg/logging"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// HeaderRequestID is the HTTP header used to propagate request IDs.
const HeaderRequestID = "X-Request-ID"

// Logging generates or propagates a request ID, attaches a structured logger
// to the request context, and logs request completion.
func Logging() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			reqID := c.Request().Header.Get(HeaderRequestID)
			if reqID == "" {
				reqID = uuid.New().String()
			}

			ctx.Set(c, ctx.RequestIDKey, reqID)
			c.Response().Header().Set(HeaderRequestID, reqID)

			logger := logging.FromContext(c.Request().Context()).With(
				slog.String("request_id", reqID),
				slog.String("client_ip", c.RealIP()),
				slog.String("user_agent", c.Request().UserAgent()),
			)
			reqCtx := logging.WithLogger(c.Request().Context(), logger)
			c.SetRequest(c.Request().WithContext(reqCtx))

			start := time.Now()
			err := next(c)

			if !strings.HasPrefix(c.Request().URL.Path, "/static") {
				status := c.Response().Status
				duration := time.Since(start)
				method := c.Request().Method
				path := c.Request().URL.Path

				attrs := []any{
					slog.String("method", method),
					slog.String("path", path),
					slog.Int("status", status),
					slog.Float64("duration_ms", float64(duration.Milliseconds())),
				}

				msg := "[HTTP:" + method + ":" + slog.IntValue(status).String() + "]"
				if status >= http.StatusInternalServerError {
					logger.Error(msg, attrs...)
				} else {
					logger.Info(msg, attrs...)
				}
			}

			return err
		}
	}
}
