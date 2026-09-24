// Package devemail provides a dev-only sample handler that sends a test
// message through the configured email.Sender. Useful for smoke-testing the
// hamr dev mail inbox at /__hamr/mail without wiring your own handler first.
package devemail

import (
	"fmt"
	"net/http"

	"github.com/FyrmForge/hamr/pkg/email"
	"github.com/labstack/echo/v4"
)

// Handler renders a sample email through deps.EmailSender and redirects to
// the dev inbox. Only registered when EMAIL_MOCK=true and DEV_MODE=true.
type Handler struct {
	sender  email.Sender
	devMode bool
}

// NewHandler constructs a Handler. sender may be nil; SendTest then returns
// 503 so the route is safe to register unconditionally.
func NewHandler(sender email.Sender, devMode bool) *Handler {
	return &Handler{sender: sender, devMode: devMode}
}

// SendTest dispatches a canned email. Refuses in production to prevent an
// accidental route leak. Redirects to /__hamr/mail on success so the user
// lands in the dev inbox.
func (h *Handler) SendTest(c echo.Context) error {
	if !h.devMode {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if h.sender == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "email sender not configured (EMAIL_MOCK=true required)")
	}

	msg := email.Message{
		From:    email.Addr("hamr dev", "no-reply@hamr.local"),
		To:      []email.Address{email.Addr("Test User", "test@example.com")},
		Subject: "Hello from hamr dev",
		Text:    "This is a sample message from /dev/send-test-email.\n",
		HTML: `<!DOCTYPE html>
<html><body style="font-family:sans-serif;color:#222">
<h1 style="color:#f97316">Hello from hamr dev</h1>
<p>This is a sample message from <code>/dev/send-test-email</code>.</p>
<p>Your production code calls <code>email.Sender.Send</code>, which in dev
ships to the hamr inbox at <a href="/__hamr/mail">/__hamr/mail</a>.</p>
</body></html>`,
		Headers: map[string]string{"X-App": "hamr-dev-demo"},
		Tags:    map[string]string{"source": "dev-send-test-email"},
	}

	if _, err := h.sender.Send(c.Request().Context(), msg); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("send: %v", err))
	}
	return c.Redirect(http.StatusSeeOther, "/__hamr/mail")
}
