package form

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"
)

// GetError returns the error message for a field, or empty string if none.
func GetError(errors map[string]string, field string) string {
	if errors == nil {
		return ""
	}
	return errors[field]
}

// IsSelected returns "selected" if the value matches the current value.
func IsSelected(current, value string) string {
	if current == value {
		return "selected"
	}
	return ""
}

// OOBValidator renders per-field validation errors as OOB HTML swaps.
// Use with validate.WithOOBRenderer(form.OOBValidator).
func OOBValidator(c echo.Context, field, errMsg string) error {
	status := http.StatusOK
	if errMsg != "" {
		status = http.StatusUnprocessableEntity
	}
	return respond.HTML(c, status, FieldErrorOOB(field, errMsg))
}
