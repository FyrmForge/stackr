package components

import (
	"net/http"
	"strconv"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"
)

// VarsSummary is a Variables row closed, worded as the canvas card:
// "3 params · 1 secret", or "none set". set counts every name, secrets too.
func VarsSummary(set, secret int) string {
	if set == 0 {
		return "none set"
	}
	return count(set-secret, "param") + " · " + count(secret, "secret")
}

func count(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + one + "s"
}

// GetError returns the error message for a field, or empty string if none.
func GetError(errors map[string]string, field string) string {
	if errors == nil {
		return ""
	}
	return errors[field]
}

// OOBValidator renders per-field validation errors as OOB HTML swaps.
// Use with validate.WithOOBRenderer(components.OOBValidator).
func OOBValidator(c echo.Context, field, errMsg string) error {
	status := http.StatusOK
	if errMsg != "" {
		status = http.StatusUnprocessableEntity
	}
	return respond.HTML(c, status, FieldErrorOOB(field, errMsg))
}
